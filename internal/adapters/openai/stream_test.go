package openai

import (
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"io"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

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

// TestStreamThink is AC-19 on a stream: toward a target that declares
// inline_think_tags, a tag split across chunks, escaped or not, is removed;
// a chunk whose content becomes empty is still emitted; the bytes held at
// the end of a chunk go out with the next content or in the finish chunk,
// and for a choice without one in a chunk before [DONE]. Tool calls, usage
// and [DONE] pass untouched. Toward a target that does not declare them,
// every chunk is emitted as it came.
func TestStreamThink(t *testing.T) {
	type pair struct{ in, out string }
	for _, tt := range []struct {
		name    string
		chunks  []pair
		visible int64
	}{
		{"split, escaped, flushed by the finish chunk", []pair{
			{`{"id":"c","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
				`{"id":"c","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`},
			{`{"id":"c","choices":[{"index":0,"delta":{"content":"<thi"},"finish_reason":null}]}`,
				`{"id":"c","choices":[{"index":0,"delta":{"content":""},"finish_reason":null}]}`},
			{`{"id":"c","choices":[{"index":0,"delta":{"content":"nk\u003esecret\u003c\/thi"},"finish_reason":null}]}`,
				`{"id":"c","choices":[{"index":0,"delta":{"content":""},"finish_reason":null}]}`},
			{`{"id":"c","choices":[{"index":0,"delta":{"content":"nk>Hello <"},"finish_reason":null}]}`,
				`{"id":"c","choices":[{"index":0,"delta":{"content":"Hello "},"finish_reason":null}]}`},
			{`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"t","function":{"name":"f","arguments":"<think>"}}]},"finish_reason":null}]}`,
				`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"t","function":{"name":"f","arguments":"<think>"}}]},"finish_reason":null}]}`},
			{`{"id":"c","choices":[{"index":0,"delta":{"content":"b> x<th"},"finish_reason":null}]}`,
				`{"id":"c","choices":[{"index":0,"delta":{"content":"<b> x"},"finish_reason":null}]}`},
			{`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`{"id":"c","choices":[{"index":0,"delta":{"content":"<th"},"finish_reason":"stop"}]}`},
			{`{"id":"c","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
				`{"id":"c","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`},
			{`[DONE]`, `[DONE]`},
		}, 14},
		{"several blocks, unclosed, released before [DONE]", []pair{
			{`{"id":"d","model":"m","choices":[{"index":0,"delta":{"content":"<think>1</think>a<think>2</think>b<"}},{"index":1,"delta":{"content":"z<think>x"}},{"index":2,"delta":{"content":"y<"}}]}`,
				`{"id":"d","model":"m","choices":[{"index":0,"delta":{"content":"ab"}},{"index":1,"delta":{"content":"z"}},{"index":2,"delta":{"content":"y"}}]}`},
			{`{"id":"d","model":"m","choices":[{"index":1,"delta":{"content":"</think"}}]}`,
				`{"id":"d","model":"m","choices":[{"index":1,"delta":{"content":""}}]}`},
			{`{"id":"d","model":"m","choices":[],"usage":{"prompt_tokens":1}}`,
				`{"id":"d","model":"m","choices":[],"usage":{"prompt_tokens":1}}`},
			{`[DONE]`, `{"id":"d","model":"m","choices":[{"index":0,"delta":{"content":"<"},"finish_reason":null},{"index":2,"delta":{"content":"<"},"finish_reason":null}]}`},
			{``, `[DONE]`},
		}, 6},
	} {
		var in, want, emitted []string
		for _, p := range tt.chunks {
			if p.in != "" {
				in = append(in, p.in)
			}
			want = append(want, p.out)
		}
		emit := func(_ string, data []byte) error {
			emitted = append(emitted, string(data))
			return nil
		}
		s := NewStream(&domain.Target{InlineThinkTags: true}, &domain.Provider{})
		for i, c := range in {
			if err := s.Event("", []byte(c), emit); err != nil && (err != io.EOF || i != len(in)-1) {
				t.Fatalf("%s: %s: %v", tt.name, c, err)
			}
		}
		if o := s.Observed(); !reflect.DeepEqual(emitted, want) || o.VisibleBytes != tt.visible || !o.Stripped {
			t.Errorf("%s: emitted %q, observed %+v", tt.name, emitted, o)
		}

		emitted = nil
		s = NewStream(&domain.Target{}, &domain.Provider{})
		for _, c := range in {
			_ = s.Event("", []byte(c), emit)
		}
		if !reflect.DeepEqual(emitted, in) || s.Observed().Stripped {
			t.Errorf("%s, no inline_think_tags: emitted %q", tt.name, emitted)
		}
	}
}

// FuzzStreamThink cuts a content into chunks at rune boundaries, escaping each one's tags or
// not, behind a tool-call chunk, a finish chunk and a usage chunk: every
// emitted event is valid JSON or [DONE], the tool-call and usage chunks are
// emitted once, in order and unchanged, the visible content is the
// reference's, and VisibleBytes counts it.
func FuzzStreamThink(f *testing.F) {
	f.Add("<think>plan</think>\n\nanswer <b>", []byte{3, 9, 1}, true)
	f.Add("a<think>never closed</thi", []byte{1, 1, 1, 1, 1, 1, 1, 1}, false)
	f.Add("x<", []byte{1}, true)
	block := regexp.MustCompile(`(?s)<think>.*?(?:</think>|\z)`)
	f.Fuzz(func(t *testing.T, content string, cuts []byte, finish bool) {
		if !utf8.ValidString(content) {
			return // decoded JSON strings are valid UTF-8
		}
		var pieces []string
		rest := content
		for _, c := range cuts {
			n := min(int(c)%10, len(rest))
			for n < len(rest) && !utf8.RuneStart(rest[n]) { // each chunk's string is valid UTF-8
				n++
			}
			pieces, rest = append(pieces, rest[:n]), rest[n:]
		}
		pieces = append(pieces, rest)
		tool := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"t","function":{"arguments":"<think>"}}]}}]}`
		usage := `{"choices":[],"usage":{"prompt_tokens":1}}`
		var in []string
		for i, p := range pieces {
			q, err := jsontext.AppendQuote(nil, p)
			if err != nil {
				t.Fatal(err)
			}
			if i%2 == 1 { // the escaped spelling of the tags
				q = []byte(strings.NewReplacer("<", `\u003c`, ">", `\u003e`, "/", `\/`).Replace(string(q)))
			}
			in = append(in, `{"choices":[{"index":0,"delta":{"content":`+string(q)+`}}]}`)
			if i == 0 {
				in = append(in, tool)
			}
		}
		if finish {
			in = append(in, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		}
		in = append(in, usage, "[DONE]")

		var visible strings.Builder
		var others []string
		emit := func(_ string, data []byte) error {
			if string(data) == "[DONE]" {
				return nil
			}
			var c struct {
				Choices []struct {
					Delta struct{ Content string }
				}
			}
			if err := json.Unmarshal(data, &c); err != nil {
				t.Fatalf("emitted %q: %v", data, err)
			}
			if s := string(data); s == tool || s == usage {
				others = append(others, s)
			}
			for _, ch := range c.Choices {
				visible.WriteString(ch.Delta.Content)
			}
			return nil
		}
		s := NewStream(&domain.Target{InlineThinkTags: true}, &domain.Provider{})
		for _, c := range in {
			if err := s.Event("", []byte(c), emit); err != nil && err != io.EOF {
				t.Fatalf("%q: %v", c, err)
			}
		}
		want := block.ReplaceAllString(content, "")
		if visible.String() != want || s.Observed().VisibleBytes != int64(len(want)) || !reflect.DeepEqual(others, []string{tool, usage}) {
			t.Fatalf("%q: visible %q (%d bytes), want %q; others %q", in, visible.String(), s.Observed().VisibleBytes, want, others)
		}
	})
}
