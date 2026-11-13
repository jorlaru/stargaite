package anthropic

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/params"
	"github.com/jorlaru/stargaite/internal/reasoning"
)

// Observe reads a synchronous Messages answer from target t of provider p on
// the native path (/v1/messages to an anthropic target, design section 8) and
// returns the body to relay: the provider's bytes, except that for a target
// that declares inline_think_tags the text of every text block that held a
// think block is re-encoded without it (RSN-12). Thinking and
// redacted_thinking blocks, their signatures included, are never touched.
// Observed: the response model, the provider's own stop_reason, the usage
// (USE-3), the visible bytes of the text blocks, the tool_use blocks and
// whether a block was removed. The error says the body is not a JSON object,
// repeats a key in an object it reads, or holds a text block whose text is
// not a string: a 2xx that is not relayed. Phase 2 does not read p (design
// section 11, point 1).
func Observe(t *domain.Target, _ *domain.Provider, body []byte) ([]byte, domain.Observed, error) {
	b, e := params.Scan(body, params.MessagesReply)
	if e != nil {
		return nil, domain.Observed{}, errors.New(e.Message)
	}
	o := domain.Observed{ResponseModel: str(b, "model"), FinishReason: str(b, "stop_reason")}
	var ops []params.Op
	for j := 0; ; j++ {
		at := strconv.Itoa(j)
		if _, ok := b.Get("content", at); !ok {
			break
		}
		switch str(b, "content", at, "type") {
		case "text":
			text, ok := unquote(get(b, "content", at, "text"))
			if !ok {
				return nil, domain.Observed{}, fmt.Errorf("content[%d] is a text block without text", j)
			}
			if t.InlineThinkTags {
				var s reasoning.Stripper
				if visible := s.Feed(text) + s.Flush(); s.Stripped {
					ops = append(ops, params.Op{Path: []string{"content", at, "text"}, Value: quote(visible)})
					text, o.Stripped = visible, true
				}
			}
			o.VisibleBytes += int64(len(text))
		case "tool_use":
			o.ToolCalls++
		}
	}
	var c counts
	c.read(b, "usage")
	o.Usage, _ = c.usage()
	if ops != nil {
		var err error
		if body, err = b.Edit(ops...); err != nil {
			return nil, domain.Observed{}, err
		}
	}
	return body, o, nil
}

// Stream relays one Messages stream to a /v1/messages client (design section
// 9, Anthropic native). Use NewStream; it is not safe for concurrent use.
type Stream struct {
	think  bool
	text   *reasoning.Stripper // of the open text block toward a target that declares inline_think_tags; nil otherwise
	index  []byte              // that block's index, as sent
	counts counts
	obs    domain.Observed
}

// NewStream is the Stream of one attempt toward target t of provider p.
// Phase 2 does not read p (design section 11, point 1).
func NewStream(t *domain.Target, _ *domain.Provider) *Stream {
	return &Stream{think: t.InlineThinkTags}
}

// Event re-emits one event under its own name with its data unchanged, ping
// and unknown types included, dispatched by its event field, else by the
// data's type. Toward a target that declares inline_think_tags, the text of
// a text block, at its content_block_start and in each text_delta, goes
// through the block's own stripper and is re-encoded only when the stripper
// changed it; an event whose text becomes empty is still sent, with "", and
// the held bytes go out in one text_delta of the block's index when it stops
// (or at the next content_block_start or message_stop, if the stop is
// missing). message_stop is emitted, then Event returns io.EOF. It returns
// *ErrorEvent for an error event, which it does not emit; an error for data
// that is not a JSON object or repeats a key in an object it reads, a text
// that is not a string, an event named ":", the name the relay keeps for
// comments, or an event whose name holds a CR, which a client that ends
// lines at a bare CR would read as further lines; or emit's error. Observed: the response model, the provider's own
// stop_reason, the usage of message_start updated by each message_delta, the
// visible text, the tool_use blocks and whether a think block was removed.
func (s *Stream) Event(name string, data []byte, emit func(name string, data []byte) error) error {
	if name == Comment || strings.ContainsRune(name, '\r') {
		return errors.New(`an event of the stream: an event named ":" or with a CR in its name`)
	}
	b, e := params.Scan(data, params.MessagesReply)
	if e != nil {
		return errors.New("an event of the stream: " + e.Message)
	}
	kind := cmp.Or(name, str(b, "type"))
	if v, ok := b.Get("error"); kind == "error" || ok && string(v) != "null" {
		t := str(b, "error", "type")
		return &ErrorEvent{Type: t, Message: str(b, "error", "message") + str(b, "error"), Status: statuses[t]}
	}
	switch kind {
	case "message_start":
		s.obs.ResponseModel = str(b, "message", "model")
		s.counts.read(b, "message", "usage")
	case "content_block_start":
		if err := s.release(emit); err != nil {
			return err
		}
		switch str(b, "content_block", "type") {
		case "text":
			if s.think {
				s.text, s.index = &reasoning.Stripper{}, get(b, "index")
			}
			return s.content(name, b, data, "content_block", emit)
		case "tool_use":
			s.obs.ToolCalls++
		}
	case "content_block_delta":
		if str(b, "delta", "type") == "text_delta" {
			return s.content(name, b, data, "delta", emit)
		}
	case "content_block_stop":
		if err := s.release(emit); err != nil {
			return err
		}
	case "message_delta":
		s.obs.FinishReason = cmp.Or(str(b, "delta", "stop_reason"), s.obs.FinishReason)
		s.counts.read(b, "usage")
	case "message_stop":
		if err := s.release(emit); err != nil {
			return err
		}
		if err := emit(name, data); err != nil {
			return err
		}
		return io.EOF
	}
	return emit(name, data)
}

// content emits an event that carries text at parent.text, through the open
// block's stripper when there is one.
func (s *Stream) content(name string, b *params.Body, data []byte, parent string, emit func(name string, data []byte) error) error {
	text, ok := unquote(get(b, parent, "text"))
	if !ok {
		return errors.New("an event of the stream: a text without a string")
	}
	if s.text != nil {
		visible := s.text.Feed(text)
		s.obs.Stripped = s.obs.Stripped || s.text.Stripped
		if visible != text {
			var err error
			if data, err = b.Edit(params.Op{Path: []string{parent, "text"}, Value: quote(visible)}); err != nil {
				return err
			}
			text = visible
		}
	}
	s.obs.VisibleBytes += int64(len(text))
	return emit(name, data)
}

// release ends the open text block: its stripper's held bytes, which no tag
// followed, go out as one text_delta of the block's index.
func (s *Stream) release(emit func(name string, data []byte) error) error {
	if s.text == nil {
		return nil
	}
	held := s.text.Flush()
	s.text = nil
	if held == "" {
		return nil
	}
	s.obs.VisibleBytes += int64(len(held))
	return emit("content_block_delta", fmt.Appendf(nil, `{"type":"content_block_delta","index":%s,"delta":{"type":"text_delta","text":%s}}`,
		cmp.Or(string(s.index), "0"), quote(held)))
}

// Observed is what the stream showed so far, read once it ended, whatever
// ended it.
func (s *Stream) Observed() domain.Observed {
	o := s.obs
	o.Usage, _ = s.counts.usage()
	return o
}
