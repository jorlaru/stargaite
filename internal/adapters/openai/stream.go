package openai

import (
	"encoding/json/jsontext"
	"errors"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/params"
	"github.com/jorlaru/stargaite/internal/reasoning"
)

// Stream observes one streamed chat completion on the native path (design
// section 9) while the relay sends each chunk on unchanged, except the
// think-tag edits of delta content toward a target that declares
// inline_think_tags (RSN-12). The zero Stream is ready and edits nothing; it
// is not safe for concurrent use.
type Stream struct {
	obs   domain.Observed
	tools map[string]bool                // "choice/tool call" indexes of the tool calls seen
	think map[string]*reasoning.Stripper // by choice index; nil when the target declares no inline_think_tags
	last  *params.Body                   // the last chunk read while think is set: the frame of the chunk released at [DONE]
}

// NewStream is the Stream of one attempt toward target t of provider p.
// Phase 2 does not read p (design section 11, point 1).
func NewStream(t *domain.Target, _ *domain.Provider) *Stream {
	s := &Stream{}
	if t.InlineThinkTags {
		s.think = map[string]*reasoning.Stripper{}
	}
	return s
}

// ErrorChunk is a chunk with a top-level error member, the in-stream error
// the OpenAI SDKs read. Event does not emit it: before the commit it is not
// relayed, after it the relay sends it as the terminal event. Message is the
// provider's text, which may name its account or echo the credential.
type ErrorChunk struct{ Message string }

func (e *ErrorChunk) Error() string { return "the provider's stream reported an error: " + e.Message }

// Event observes the data of one event and emits it; the event name, which
// OpenAI does not send, is ignored. It returns io.EOF once [DONE] is emitted,
// the end of the stream; *ErrorChunk; an error for data that is not a JSON
// object or repeats a key in an object it reads; or emit's error. Observed:
// the first response model, the finish reason of choice 0, the visible bytes
// of every delta's content, each tool call once, whether a think block was
// removed, and the latest usage that is not null, which arrives when the
// client asked for it (stream_options.include_usage) or the provider sends it
// unasked.
//
// Toward a target that declares inline_think_tags, each choice's delta
// content goes through its own reasoning.Stripper and is re-encoded only when
// the visible text differs; every other byte of the chunk is kept. A chunk
// whose content becomes empty is still emitted with "content":"". The text a
// stripper holds back goes out with the choice's next content, or in the
// chunk that carries the choice's finish_reason (as a content member added to
// its delta when it has none), or, for a choice that never had one, in one
// chunk emitted before [DONE]: the last chunk read, with its choices replaced
// by the released text and its usage removed.
func (s *Stream) Event(_ string, data []byte, emit func(name string, data []byte) error) error {
	if string(data) == "[DONE]" {
		if err := s.release(emit); err != nil {
			return err
		}
		if err := emit("", data); err != nil {
			return err
		}
		return io.EOF
	}
	b, e := params.Scan(data, params.ChatReply)
	if e != nil {
		return errors.New("a chunk of the stream: " + e.Message)
	}
	if v, ok := b.Get("error"); ok && string(v) != "null" {
		return &ErrorChunk{Message: str(b, "error", "message") + str(b, "error")}
	}
	if s.obs.ResponseModel == "" {
		s.obs.ResponseModel = str(b, "model")
	}
	var ops []params.Op
	for i := 0; ; i++ {
		c := strconv.Itoa(i)
		if _, ok := b.Get("choices", c); !ok {
			break
		}
		index, ok := b.Get("choices", c, "index")
		if !ok {
			index = []byte("0")
		}
		finish := str(b, "choices", c, "finish_reason")
		if finish != "" && string(index) == "0" {
			s.obs.FinishReason = finish
		}
		content := []string{"choices", c, "delta", "content"}
		text := str(b, content...)
		if s.think != nil {
			st := s.think[string(index)]
			if st == nil {
				st = &reasoning.Stripper{}
				s.think[string(index)] = st
			}
			visible := st.Feed(text)
			if finish != "" {
				visible += st.Flush()
			}
			if visible != text {
				ops, text = append(ops, quote(content, visible)), visible
			}
			s.obs.Stripped = s.obs.Stripped || st.Stripped
		}
		s.obs.VisibleBytes += int64(len(text))
		for j := 0; ; j++ {
			call := []string{"choices", c, "delta", "tool_calls", strconv.Itoa(j)}
			if _, ok := b.Get(call...); !ok {
				break
			}
			k, _ := b.Get(append(call, "index")...)
			if id := string(index) + "/" + string(k); !s.tools[id] {
				if s.tools == nil {
					s.tools = map[string]bool{}
				}
				s.tools[id] = true
				s.obs.ToolCalls++
			}
		}
	}
	if u := usage(b); u != nil {
		s.obs.Usage = u
	}
	if s.think != nil {
		s.last = b
	}
	if ops != nil {
		var err error
		if data, err = b.Edit(ops...); err != nil {
			return errors.New("a chunk of the stream: " + err.Error())
		}
	}
	return emit("", data)
}

// release emits, before [DONE], the text still held for the choices that
// never carried a finish_reason, all in one chunk framed as the last one.
func (s *Stream) release(emit func(name string, data []byte) error) error {
	var choices []string
	for _, i := range slices.Sorted(maps.Keys(s.think)) {
		if v := s.think[i].Flush(); v != "" {
			q, _ := jsontext.AppendQuote(nil, v)
			choices = append(choices, `{"index":`+i+`,"delta":{"content":`+string(q)+`},"finish_reason":null}`)
			s.obs.VisibleBytes += int64(len(v))
		}
	}
	if choices == nil {
		return nil
	}
	data, err := s.last.Edit(params.Op{Path: []string{"choices"}, Value: []byte("[" + strings.Join(choices, ",") + "]")},
		params.Op{Path: []string{"usage"}})
	if err != nil {
		return errors.New("a chunk of the stream: " + err.Error())
	}
	return emit("", data)
}

// Observed is what the stream showed so far, read once it ended, whatever
// ended it.
func (s *Stream) Observed() domain.Observed { return s.obs }
