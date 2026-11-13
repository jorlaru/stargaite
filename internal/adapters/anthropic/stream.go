package anthropic

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/params"
	"github.com/jorlaru/stargaite/internal/reasoning"
)

// Comment is the event name ChatStream emits for an SSE comment line, whose
// text is the data: the relay writes ":" and the text, not an event.
const Comment = ":"

// ChatStream translates one Messages stream into chat completion chunks
// (design section 9, derived mapping). Use NewChatStream; it is not safe for
// concurrent use.
type ChatStream struct {
	think, usage bool
	now          func() time.Time
	frame        string              // the id, object, created and model members of every chunk; "" before message_start
	tools        map[string]int      // the tool call index of each tool_use block, by content block index
	text         *reasoning.Stripper // of the open text block toward a target that declares inline_think_tags; nil otherwise
	argless      bool                // the latest tool_use block is open and every partial_json of it so far is empty
	counts       counts
	obs          domain.Observed
}

// NewChatStream is the ChatStream of one attempt toward target t of provider
// p. includeUsage is the client's stream_options.include_usage, which asks
// for the usage chunk; now is the relay's clock, read once for created.
// Phase 2 does not read p (design section 11, point 1).
func NewChatStream(t *domain.Target, _ *domain.Provider, includeUsage bool, now func() time.Time) *ChatStream {
	return &ChatStream{think: t.InlineThinkTags, usage: includeUsage, now: now, tools: map[string]int{}}
}

// statuses are the HTTP statuses Anthropic documents for its error types,
// which classify an error event read before the commit (section 15 point 15).
var statuses = map[string]int{
	"invalid_request_error": 400, "authentication_error": 401, "billing_error": 402, "permission_error": 403, "not_found_error": 404,
	"conflict_error": 409, "request_too_large": 413, "rate_limit_error": 429, "api_error": 500, "timeout_error": 504, "overloaded_error": 529,
}

// ErrorEvent is an error event of a Messages stream: an event named error,
// or data with a top-level error member that is not null. ChatStream does
// not emit it: before the commit it is not relayed, after it the relay
// writes the terminal event of the inbound format. Message is the
// provider's text, which may name its account or echo the credential.
type ErrorEvent struct {
	Type, Message string
	Status        int // the status Anthropic documents for Type; 0 for a type it does not list
}

func (e *ErrorEvent) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("the provider's stream reported %q: %s", e.Type, e.Message)
	}
	return fmt.Sprintf("the provider's stream reported %q, status %d: %s", e.Type, e.Status, e.Message)
}

// Event translates one event, named by its event field (else by the data's
// type, which Anthropic sends with both), and emits the chat
// chunks it gives: message_start the first chunk, with the role; the text of
// a text block, through its own think-tag stripper toward a target that
// declares inline_think_tags, each piece whose visible text is not empty; a
// tool_use block's opening delta, its index counting tool calls only, then
// each input_json_delta as an arguments fragment; message_stop the finish
// chunk with the mapped finish_reason, the usage chunk when the client asked
// for it, and [DONE], after which it returns io.EOF; ping the comment ": ping"
// (named Comment). Thinking, signature and redacted_thinking content,
// content_block_stop, message_delta and an unknown event emit nothing, but a
// stripper's held bytes go out when its block stops, and a tool_use block
// whose partial_json fragments were all empty gets the arguments "{}" when it
// stops, as ToChat gives (a missing stop is taken at the next
// content_block_start or message_stop). It returns *ErrorEvent;
// an error for data that is not a JSON object, repeats a key in an object it
// reads, lacks a member it needs, or comes before message_start; or emit's
// error. Observed: the response model, the provider's own stop_reason, the
// usage of message_start updated by each message_delta, the visible bytes,
// the tool calls and whether a think block was removed.
func (s *ChatStream) Event(name string, data []byte, emit func(name string, data []byte) error) error {
	b, e := params.Scan(data, params.MessagesReply)
	if e != nil {
		return errors.New("an event of the stream: " + e.Message)
	}
	name = cmp.Or(name, str(b, "type")) // the event field, else the data's type, which Anthropic sends with both
	if v, ok := b.Get("error"); name == "error" || ok && string(v) != "null" {
		t := str(b, "error", "type")
		return &ErrorEvent{Type: t, Message: str(b, "error", "message") + str(b, "error"), Status: statuses[t]}
	}
	index := string(get(b, "index"))
	switch name {
	case "ping":
		return emit(Comment, []byte(" ping"))
	case "message_start":
		id, model := get(b, "message", "id"), get(b, "message", "model")
		if len(id) == 0 || id[0] != '"' || len(model) == 0 || model[0] != '"' {
			return errors.New("an event of the stream: message_start without a message id and model")
		}
		s.obs.ResponseModel = str(b, "message", "model")
		s.counts.read(b, "message", "usage")
		s.frame = fmt.Sprintf(`"id":%s,"object":"chat.completion.chunk","created":%d,"model":%s`, id, s.now().Unix(), model)
		return s.chunk(emit, `{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}`)
	case "content_block_start":
		if err := s.release(emit); err != nil {
			return err
		}
		switch str(b, "content_block", "type") {
		case "text":
			if s.think {
				s.text = &reasoning.Stripper{}
			}
			return s.content(emit, get(b, "content_block", "text"))
		case "tool_use":
			id, tool := get(b, "content_block", "id"), get(b, "content_block", "name")
			if len(id) == 0 || id[0] != '"' || len(tool) == 0 || tool[0] != '"' {
				return errors.New("an event of the stream: a tool_use block without an id and name")
			}
			k := s.obs.ToolCalls
			s.tools[index] = k
			s.obs.ToolCalls++
			s.argless = true
			return s.chunk(emit, fmt.Sprintf(`{"index":0,"delta":{"tool_calls":[{"index":%d,"id":%s,"type":"function","function":{"name":%s,"arguments":""}}]},"finish_reason":null}`, k, id, tool))
		}
	case "content_block_delta":
		switch str(b, "delta", "type") {
		case "text_delta":
			return s.content(emit, get(b, "delta", "text"))
		case "input_json_delta":
			k, ok := s.tools[index]
			args := get(b, "delta", "partial_json")
			if !ok || len(args) == 0 || args[0] != '"' {
				return errors.New("an event of the stream: an input_json_delta without its tool_use block or partial_json")
			}
			s.argless = s.argless && (k != s.obs.ToolCalls-1 || string(args) == `""`)
			return s.chunk(emit, fmt.Sprintf(`{"index":0,"delta":{"tool_calls":[{"index":%d,"function":{"arguments":%s}}]},"finish_reason":null}`, k, args))
		}
	case "content_block_stop":
		return s.release(emit)
	case "message_delta":
		s.obs.FinishReason = cmp.Or(str(b, "delta", "stop_reason"), s.obs.FinishReason)
		s.counts.read(b, "usage")
	case "message_stop":
		if err := s.release(emit); err != nil {
			return err
		}
		if err := s.chunk(emit, fmt.Sprintf(`{"index":0,"delta":{},"finish_reason":%q}`, cmp.Or(finish[s.obs.FinishReason], "stop"))); err != nil {
			return err
		}
		if u, chat := s.counts.usage(); s.usage && u != nil {
			if err := emit("", fmt.Appendf(nil, `{%s,"choices":[],"usage":%s}`, s.frame, chat)); err != nil {
				return err
			}
		}
		if err := emit("", []byte("[DONE]")); err != nil {
			return err
		}
		return io.EOF
	}
	return nil
}

// content emits the text of a text block, the raw JSON string raw, as a
// delta; through the block's stripper when there is one, re-encoded only
// when the stripper changed it. Nothing is emitted for empty visible text.
func (s *ChatStream) content(emit func(name string, data []byte) error, raw []byte) error {
	text, ok := unquote(raw)
	if !ok {
		return errors.New("an event of the stream: a text without a string")
	}
	if s.text != nil {
		if visible := s.text.Feed(text); visible != text {
			text, raw = visible, quote(visible)
		}
		s.obs.Stripped = s.obs.Stripped || s.text.Stripped
	}
	return s.write(emit, text, raw)
}

// release ends the open block: a text block's stripper's held bytes, which
// no tag followed, are emitted; a tool_use block whose arguments are all empty
// gets the arguments "{}", as ToChat gives for its empty input.
func (s *ChatStream) release(emit func(name string, data []byte) error) error {
	if s.argless {
		s.argless = false
		return s.chunk(emit, fmt.Sprintf(`{"index":0,"delta":{"tool_calls":[{"index":%d,"function":{"arguments":"{}"}}]},"finish_reason":null}`, s.obs.ToolCalls-1))
	}
	if s.text == nil {
		return nil
	}
	held := s.text.Flush()
	s.text = nil
	return s.write(emit, held, quote(held))
}

// write emits the visible text, raw as its JSON string, unless it is empty.
func (s *ChatStream) write(emit func(name string, data []byte) error, text string, raw []byte) error {
	if text == "" {
		return nil
	}
	s.obs.VisibleBytes += int64(len(text))
	return s.chunk(emit, `{"index":0,"delta":{"content":`+string(raw)+`},"finish_reason":null}`)
}

// chunk emits one chat.completion.chunk holding the choice.
func (s *ChatStream) chunk(emit func(name string, data []byte) error, choice string) error {
	if s.frame == "" {
		return errors.New("an event of the stream: content before message_start")
	}
	return emit("", []byte("{"+s.frame+`,"choices":[`+choice+"]}"))
}

// Observed is what the stream showed so far, read once it ended, whatever
// ended it.
func (s *ChatStream) Observed() domain.Observed {
	o := s.obs
	o.Usage, _ = s.counts.usage()
	return o
}
