package openai

import (
	"errors"
	"io"
	"strconv"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/params"
)

// Stream observes one streamed chat completion on the native path (design
// section 9) while the relay sends each chunk on unchanged. The zero Stream
// is ready; it is not safe for concurrent use.
type Stream struct {
	obs   domain.Observed
	tools map[string]bool // "choice/tool call" indexes of the tool calls seen
}

// ErrorChunk is a chunk with a top-level error member, the in-stream error
// the OpenAI SDKs read. Event does not emit it: before the commit it is not
// relayed, after it the relay sends it as the terminal event. Message is the
// provider's text, which may name its account or echo the credential.
type ErrorChunk struct{ Message string }

func (e *ErrorChunk) Error() string { return "the provider's stream reported an error: " + e.Message }

// Event observes the data of one event and emits it unchanged; the event
// name, which OpenAI does not send, is ignored. It returns io.EOF once
// [DONE] is emitted, the end of the stream; *ErrorChunk; an error for data
// that is not a JSON object or repeats a key in an object it reads; or
// emit's error. Observed: the first response model, the finish reason of
// choice 0, the visible bytes of every delta's content, each tool call once,
// and the latest usage that is not null, which arrives when the client asked
// for it (stream_options.include_usage) or the provider sends it unasked.
func (s *Stream) Event(_ string, data []byte, emit func(name string, data []byte) error) error {
	if string(data) == "[DONE]" {
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
	for i := 0; ; i++ {
		c := strconv.Itoa(i)
		if _, ok := b.Get("choices", c); !ok {
			break
		}
		index, ok := b.Get("choices", c, "index")
		if !ok {
			index = []byte("0")
		}
		if f := str(b, "choices", c, "finish_reason"); f != "" && string(index) == "0" {
			s.obs.FinishReason = f
		}
		s.obs.VisibleBytes += int64(len(str(b, "choices", c, "delta", "content")))
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
	return emit("", data)
}

// Observed is what the stream showed so far, read once it ended, whatever
// ended it.
func (s *Stream) Observed() domain.Observed { return s.obs }
