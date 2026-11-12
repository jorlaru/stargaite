// Package mockprovider is a scriptable stand-in for an inference provider. It
// records the exact bytes of every request it receives, so tests can assert
// what the proxy sends, and answers each with the next scripted reply or, when
// none is queued, a well-formed default. It uses the standard library only.
package mockprovider

import (
	"io"
	"maps"
	"net/http"
	"slices"
	"sync"
)

// Step is one scripted reply. A zero Status means 200.
type Step struct {
	Status int
	Header http.Header
	Body   []byte
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

// New returns a server with an empty script.
func New() *Server { return &Server{} }

// ServeHTTP records the request, then answers POST /chat/completions,
// /embeddings and /v1/messages; any other route gets 404 or 405 and leaves the
// script untouched.
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
		maps.Copy(w.Header(), step.Header)
		if step.Status != 0 {
			w.WriteHeader(step.Status)
		}
		_, _ = w.Write(step.Body)
	default:
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, def)
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
