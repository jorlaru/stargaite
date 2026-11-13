package openai

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/domain"
)

// TestStream observes chunks as design section 7 lists and emits each one
// unchanged: [DONE] ends the stream; an error chunk, data that is not a JSON
// object and a failed emit are errors, and the first two emit nothing.
func TestStream(t *testing.T) {
	var emitted []string
	emit := func(name string, data []byte) error {
		emitted = append(emitted, name+string(data))
		return nil
	}
	chunks := []string{
		`{"model":"up-2","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}`,
		`{"model":"other","choices":[{"index":0,"delta":{"content":"hé"}},{"index":1,"delta":{"content":"abc"},"finish_reason":"length"}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":""}},{"index":1,"id":"b"}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}},{"index":1,"delta":{"tool_calls":[{"index":0,"id":"c"}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":5}}}`,
	}
	var s Stream
	for _, c := range chunks {
		if err := s.Event("", []byte(c), emit); err != nil {
			t.Fatalf("%s: %v", c, err)
		}
	}
	if err := s.Event("", []byte("[DONE]"), emit); err != io.EOF {
		t.Errorf("[DONE]: %v", err)
	}
	if want := append(chunks, "[DONE]"); !reflect.DeepEqual(emitted, want) {
		t.Errorf("emitted %q", emitted)
	}
	r := int64(5)
	want := domain.Observed{ResponseModel: "up-2", FinishReason: "tool_calls", VisibleBytes: 6, ToolCalls: 3,
		Usage: &domain.Usage{Input: 10, CacheRead: 4, Output: 7, Reasoning: &r}}
	if got := s.Observed(); !reflect.DeepEqual(got, want) {
		t.Errorf("observed %+v, usage %+v", got, got.Usage)
	}

	emitted = nil
	for _, tc := range []struct{ data, message string }{
		{`{"error":{"message":"overloaded","type":"server_error"}}`, "overloaded"},
		{`{"error":"plain text"}`, "plain text"},
	} {
		var chunk *ErrorChunk
		if err := s.Event("", []byte(tc.data), emit); !errors.As(err, &chunk) || chunk.Message != tc.message {
			t.Errorf("%s: %v", tc.data, err)
		}
	}
	for _, data := range []string{``, `[]`, `not json`, `{"model":"m"`, `{"choices":[{"delta":{"content":"a","content":"b"}}]}`} {
		if err := s.Event("", []byte(data), emit); err == nil || !strings.HasPrefix(err.Error(), "a chunk of the stream") {
			t.Errorf("%q: %v", data, err)
		}
	}
	if emitted != nil {
		t.Errorf("emitted %q", emitted)
	}
	boom := errors.New("boom")
	fails := func(string, []byte) error { return boom }
	for _, data := range []string{`{"choices":[]}`, `[DONE]`} {
		if err := s.Event("", []byte(data), fails); !errors.Is(err, boom) {
			t.Errorf("%s with a failed emit: %v", data, err)
		}
	}
	if err := s.Event("", []byte(`{"error":null,"choices":[]}`), emit); err != nil {
		t.Errorf("a null error member: %v", err)
	}
}
