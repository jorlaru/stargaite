// Package mockprovider is a scriptable stand-in for an inference provider. It
// records the exact bytes of every request it receives, so tests can assert
// what the proxy sends, and answers each with the next scripted reply or, when
// none is queued, a well-formed default. It uses the standard library only.
package mockprovider

import (
	"cmp"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Step is one scripted reply. A zero Status means 200.
type Step struct {
	Status int
	Header http.Header
	Body   []byte
	Chunks [][]byte      // raw SSE bytes, each written and flushed after the headers; Body is ignored
	Delay  time.Duration // before the status line: a late first byte
	Gap    time.Duration // between chunks: silence
	Hang   bool          // after the body or the chunks, block until the request's context ends
	Cut    bool          // after the body or the chunks, abort the connection
}

// Call is one request as received: the body holds its exact bytes.
type Call struct {
	Method, Path string
	Header       http.Header
	Body         []byte
}

// Server is an http.Handler; it is safe for concurrent use.
type Server struct {
	mu    sync.Mutex
	queue []Step
	calls []Call
}

// defaults are the replies of an empty queue, by route: OpenAI chat
// completions and embeddings, and Anthropic messages.
var defaults = map[string]string{
	"/chat/completions": `{"id":"chatcmpl-mock","object":"chat.completion","created":0,"model":"mock",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"mock reply"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
	"/embeddings": `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0,0.5,1]}],"model":"mock",` +
		`"usage":{"prompt_tokens":1,"total_tokens":1}}`,
	"/v1/messages": `{"id":"msg_mock","type":"message","role":"assistant","model":"mock",` +
		`"content":[{"type":"text","text":"mock reply"}],"stop_reason":"end_turn","stop_sequence":null,` +
		`"usage":{"input_tokens":1,"output_tokens":2}}`,
}

// streams are the replies of an empty queue to a body with "stream": true,
// by route: OpenAI chat chunks with a usage chunk, sent unasked, and [DONE];
// the Anthropic Messages events.
var streams = map[string][]string{
	"/chat/completions": {
		`{"id":"chatcmpl-mock","object":"chat.completion.chunk","created":0,"model":"mock","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"id":"chatcmpl-mock","object":"chat.completion.chunk","created":0,"model":"mock","choices":[{"index":0,"delta":{"content":"mock "},"finish_reason":null}]}`,
		`{"id":"chatcmpl-mock","object":"chat.completion.chunk","created":0,"model":"mock","choices":[{"index":0,"delta":{"content":"reply"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-mock","object":"chat.completion.chunk","created":0,"model":"mock","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"chatcmpl-mock","object":"chat.completion.chunk","created":0,"model":"mock","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
		`[DONE]`,
	},
	"/v1/messages": {
		"event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_mock","type":"message","role":"assistant","model":"mock","content":[],` +
			`"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}`,
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"event: ping\ndata: " + `{"type":"ping"}`,
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"mock reply"}}`,
		"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}`,
		"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`,
		"event: message_stop\ndata: " + `{"type":"message_stop"}`,
	},
}

// New returns a server with an empty script.
func New() *Server { return &Server{} }

// ServeHTTP records the request, then answers POST /chat/completions,
// /embeddings and /v1/messages; any other route gets 404 or 405 and leaves the
// script untouched. With an empty queue, a body with "stream": true gets the
// route's default stream, if it has one.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	s.mu.Lock()
	s.calls = append(s.calls, Call{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	def, known := defaults[r.URL.Path]
	var step *Step
	if err == nil && known && r.Method == http.MethodPost && len(s.queue) > 0 {
		step, s.queue = &s.queue[0], s.queue[1:]
	}
	s.mu.Unlock()

	switch {
	case err != nil:
		http.Error(w, "request body could not be read", http.StatusBadRequest)
	case !known:
		http.NotFound(w, r)
	case r.Method != http.MethodPost:
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	case step != nil:
		reply(w, r, step)
	default:
		var req struct{ Stream bool }
		if events := streams[r.URL.Path]; json.Unmarshal(body, &req) == nil && req.Stream && events != nil {
			step = &Step{}
			for _, e := range events {
				if !strings.HasPrefix(e, "event:") {
					e = "data: " + e
				}
				step.Chunks = append(step.Chunks, []byte(e+"\n\n"))
			}
			reply(w, r, step)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, def)
	}
}

// reply answers with step. A step with Chunks defaults its Content-Type to
// text/event-stream. A wait ends early, and the reply with it, when the
// request's context ends.
func reply(w http.ResponseWriter, r *http.Request, step *Step) {
	maps.Copy(w.Header(), step.Header)
	if step.Chunks != nil && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	rc := http.NewResponseController(w)
	wait := func(d time.Duration) bool {
		if d <= 0 {
			return true
		}
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			return true
		case <-r.Context().Done():
			return false
		}
	}
	if !wait(step.Delay) {
		return
	}
	w.WriteHeader(cmp.Or(step.Status, http.StatusOK))
	if step.Chunks == nil {
		_, _ = w.Write(step.Body)
	}
	for i, c := range step.Chunks {
		if i > 0 && !wait(step.Gap) {
			return
		}
		_, _ = w.Write(c)
		_ = rc.Flush()
	}
	if step.Hang || step.Cut {
		_ = rc.Flush() // what was written reaches the client first
	}
	if step.Hang {
		<-r.Context().Done()
	}
	if step.Cut {
		panic(http.ErrAbortHandler)
	}
}

// Script queues replies after those already queued.
func (s *Server) Script(steps ...Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, steps...)
}

// Calls returns the requests received so far, in order.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}
