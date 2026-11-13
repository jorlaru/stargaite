package anthropic

import (
	"cmp"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"io"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jorlaru/stargaite/internal/domain"
)

func clock() time.Time { return now }

// event is one upstream event and what the stream emits for it, each
// emitted event as its name, if any, then its data.
type event struct {
	name, data string
	want       []string
}

func frame(choice string) string {
	return `{"id":"msg_1","object":"chat.completion.chunk","created":1790000000,"model":"claude-x","choices":[` + choice + `]}`
}

func delta(d string) string { return frame(`{"index":0,"delta":` + d + `,"finish_reason":null}`) }

// TestChatStream is the Anthropic-to-chat table of design section 9 toward a
// target that declares inline_think_tags, with the usage chunk asked for:
// the role first; thinking, signatures, block stops, message_delta and an
// unknown event emit nothing; ping is a comment; text through one stripper
// per block, its held bytes released when the block stops; tool calls
// numbered over the tool_use blocks only, id and name first, then the
// arguments fragments in order, "{}" at the stop of a block whose fragments
// were all empty (a tool without parameters); message_stop gives the finish
// chunk, the usage chunk and [DONE]. Events without their event field are
// dispatched by the data's type. Without inline_think_tags or include_usage
// the text is the provider's and no usage chunk is sent.
func TestChatStream(t *testing.T) {
	events := []event{
		{"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,` +
			`"usage":{"input_tokens":10,"cache_creation_input_tokens":3,"cache_read_input_tokens":20,"output_tokens":1}}}`,
			[]string{delta(`{"role":"assistant","content":""}`)}},
		{"ping", `{"type": "ping"}`, []string{": ping"}},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`, nil},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"<think>plan"}}`, nil},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"c2ln"}}`, nil},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`, nil},
		{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`, nil},
		{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"\u003cthi"}}`, nil},
		{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"nk>x</think>Hi \u00e9 <"}}`,
			[]string{delta(`{"content":"Hi é "}`)}},
		{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"b> \"q\""}}`, []string{delta(`{"content":"<b> \"q\""}`)}},
		{"content_block_stop", `{"type":"content_block_stop","index":1}`, nil},
		{"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`,
			[]string{delta(`{"tool_calls":[{"index":0,"id":"toolu_1","type":"function","function":{"name":"get_weather","arguments":""}}]}`)}},
		{"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":""}}`,
			[]string{delta(`{"tool_calls":[{"index":0,"function":{"arguments":""}}]}`)}},
		{"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\": "}}`,
			[]string{delta(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\": "}}]}`)}},
		{"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"<think>\"}"}}`,
			[]string{delta(`{"tool_calls":[{"index":0,"function":{"arguments":"\"<think>\"}"}}]}`)}},
		{"content_block_stop", `{"type":"content_block_stop","index":2}`, nil},
		{"content_block_start", `{"type":"content_block_start","index":3,"content_block":{"type":"text","text":"Done<"}}`, []string{delta(`{"content":"Done"}`)}},
		{"content_block_stop", `{"type":"content_block_stop","index":3}`, []string{delta(`{"content":"<"}`)}},
		{"content_block_start", `{"type":"content_block_start","index":4,"content_block":{"type":"redacted_thinking","data":"x"}}`, nil},
		{"content_block_stop", `{"type":"content_block_stop","index":4}`, nil},
		{"content_block_start", `{"type":"content_block_start","index":5,"content_block":{"type":"tool_use","id":"toolu_2","name":"now","input":{}}}`,
			[]string{delta(`{"tool_calls":[{"index":1,"id":"toolu_2","type":"function","function":{"name":"now","arguments":""}}]}`)}},
		{"content_block_delta", `{"type":"content_block_delta","index":5,"delta":{"type":"input_json_delta","partial_json":""}}`,
			[]string{delta(`{"tool_calls":[{"index":1,"function":{"arguments":""}}]}`)}},
		{"content_block_stop", `{"type":"content_block_stop","index":5}`, []string{delta(`{"tool_calls":[{"index":1,"function":{"arguments":"{}"}}]}`)}},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":null,"output_tokens":40,` +
			`"output_tokens_details":{"thinking_tokens":12}}}`, nil},
		{"a_later_event", `{"type":"a_later_event","index":9}`, nil},
		{"message_stop", `{"type":"message_stop"}`, []string{frame(`{"index":0,"delta":{},"finish_reason":"tool_calls"}`),
			`{"id":"msg_1","object":"chat.completion.chunk","created":1790000000,"model":"claude-x","choices":[],"usage":{"prompt_tokens":33,"completion_tokens":40,` +
				`"total_tokens":73,"prompt_tokens_details":{"cached_tokens":20,"cache_write_tokens":3},"completion_tokens_details":{"reasoning_tokens":12}}}`, "[DONE]"}},
	}
	var emitted []string
	emit := func(name string, data []byte) error {
		emitted = append(emitted, name+string(data))
		return nil
	}
	r := int64(12)
	want := domain.Observed{ResponseModel: "claude-x", FinishReason: "tool_use", VisibleBytes: 18, ToolCalls: 2, Stripped: true,
		Usage: &domain.Usage{Input: 33, CacheRead: 20, CacheWrite: 3, Output: 40, Reasoning: &r}}
	for _, named := range []bool{true, false} {
		s := NewChatStream(&domain.Target{InlineThinkTags: true}, &domain.Provider{}, true, clock)
		for i, e := range events {
			emitted = nil
			if !named {
				e.name = ""
			}
			if err := s.Event(e.name, []byte(e.data), emit); (err == io.EOF) != (i == len(events)-1) || err != nil && err != io.EOF {
				t.Fatalf("%s: %v", e.data, err)
			}
			if !reflect.DeepEqual(emitted, e.want) {
				t.Errorf("event field %t, %s:\nemitted %q\nwant    %q", named, e.data, emitted, e.want)
			}
			for _, d := range emitted {
				if d != "[DONE]" && !strings.HasPrefix(d, ":") && !json.Valid([]byte(d)) {
					t.Errorf("not JSON: %s", d)
				}
			}
		}
		if got := s.Observed(); !reflect.DeepEqual(got, want) {
			t.Errorf("event field %t: observed %+v, usage %+v", named, got, got.Usage)
		}
	}

	emitted = nil
	s := NewChatStream(&domain.Target{}, &domain.Provider{}, false, clock)
	for _, e := range events {
		_ = s.Event(e.name, []byte(e.data), emit)
	}
	var text strings.Builder
	for _, d := range emitted[:len(emitted)-1] {
		var c struct {
			Choices []struct{ Delta struct{ Content string } }
			Usage   any
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(d, ": ")), &c); err == nil {
			if c.Usage != nil {
				t.Errorf("a usage chunk without include_usage: %s", d)
			}
			for _, ch := range c.Choices {
				text.WriteString(ch.Delta.Content)
			}
		}
	}
	if text.String() != `<think>x</think>Hi é <b> "q"Done<` || emitted[len(emitted)-1] != "[DONE]" || s.Observed().Stripped || s.Observed().VisibleBytes != 34 {
		t.Errorf("without inline_think_tags: %q, observed %+v", text.String(), s.Observed())
	}
}

// TestChatStreamMissingStop: a block whose content_block_stop is missing ends
// at the next content_block_start or at message_stop, toward a target that
// declares inline_think_tags: the stripper's held bytes, and the "{}" of a
// tool_use block whose fragments were all empty, come before the next block's
// chunk or the finish chunk.
func TestChatStreamMissingStop(t *testing.T) {
	text := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"a<"}}`
	tool := `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"now","input":{}}}`
	empty := `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":""}}`
	held, open := delta(`{"content":"<"}`), delta(`{"tool_calls":[{"index":0,"id":"toolu_1","type":"function","function":{"name":"now","arguments":""}}]}`)
	args, stop := delta(`{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}`), frame(`{"index":0,"delta":{},"finish_reason":"stop"}`)
	next := `{"type":"content_block_start","index":2,"content_block":{"type":"text","text":"b"}}`
	for _, tc := range []struct {
		events []string
		want   []string
	}{
		{[]string{text, tool}, []string{delta(`{"content":"a"}`), held, open}},
		{[]string{text, `{"type":"message_stop"}`}, []string{delta(`{"content":"a"}`), held, stop, "[DONE]"}},
		{[]string{tool, empty, next}, []string{open, delta(`{"tool_calls":[{"index":0,"function":{"arguments":""}}]}`), args, delta(`{"content":"b"}`)}},
		{[]string{tool, `{"type":"message_stop"}`}, []string{open, args, stop, "[DONE]"}},
	} {
		var emitted []string
		emit := func(_ string, data []byte) error { emitted = append(emitted, string(data)); return nil }
		s := NewChatStream(&domain.Target{InlineThinkTags: true}, &domain.Provider{}, false, clock)
		_ = s.Event("message_start", []byte(`{"type":"message_start","message":{"id":"msg_1","model":"claude-x"}}`), emit)
		emitted = nil
		for _, e := range tc.events {
			if err := s.Event("", []byte(e), emit); err != nil && err != io.EOF {
				t.Fatalf("%s: %v", e, err)
			}
		}
		if !reflect.DeepEqual(emitted, tc.want) {
			t.Errorf("%q:\nemitted %q\nwant    %q", tc.events, emitted, tc.want)
		}
	}
}

// TestChatStreamErrors: an error event emits nothing and carries the status
// Anthropic documents for its type; data that is not a JSON object, a
// repeated key, content before message_start, a block without the members
// the chunk needs, and a failed emit are errors.
func TestChatStreamErrors(t *testing.T) {
	start := `{"type":"message_start","message":{"id":"msg_1","model":"claude-x","usage":{"input_tokens":1}}}`
	for _, tc := range []struct {
		name, data   string
		typ, message string
		status       int
	}{
		{"error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, "overloaded_error", "Overloaded", 529},
		{"error", `{"type":"error","error":{"type":"api_error","message":"Internal"}}`, "api_error", "Internal", 500},
		{"error", `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, "rate_limit_error", "slow down", 429},
		{"error", `{"type":"error","error":{"type":"timeout_error","message":"late"}}`, "timeout_error", "late", 504},
		{"error", `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`, "invalid_request_error", "bad", 400},
		{"error", `{"type":"error","error":{"type":"a_new_error","message":"new"}}`, "a_new_error", "new", 0},
		{"message_delta", `{"type":"message_delta","error":"plain text"}`, "", "plain text", 0},
		{"", `{"type":"error","error":{"type":"overloaded_error","message":"no event field"}}`, "overloaded_error", "no event field", 529},
	} {
		s := NewChatStream(&domain.Target{}, &domain.Provider{}, true, clock)
		var e *ErrorEvent
		if err := s.Event(tc.name, []byte(tc.data), func(string, []byte) error { t.Fatal("emitted"); return nil }); !errors.As(err, &e) ||
			*e != (ErrorEvent{Type: tc.typ, Message: tc.message, Status: tc.status}) || !strings.Contains(err.Error(), tc.message) {
			t.Errorf("%s: %v", tc.data, err)
		}
	}

	for _, tc := range []struct {
		name, data string
		started    bool
	}{
		{"message_start", ``, false},
		{"message_start", `[]`, false},
		{"message_start", `{"type":"message_start","message":{"id":"a","id":"b","model":"m"}}`, false},
		{"message_start", `{"type":"message_start","message":{"model":"m"}}`, false},
		{"message_start", `{"type":"message_start","message":{"id":1,"model":"m"}}`, false},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`, false},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f"}}`, false},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t"}}`, true},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":null}}`, true},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":1}}`, true},
		{"content_block_delta", `{"type":"content_block_delta","index":7,"delta":{"type":"input_json_delta","partial_json":"{"}}`, true},
		{"message_stop", `{"type":"message_stop"}`, false},
	} {
		s := NewChatStream(&domain.Target{}, &domain.Provider{}, true, clock)
		emit := func(string, []byte) error { return nil }
		if tc.started {
			if err := s.Event("message_start", []byte(start), emit); err != nil {
				t.Fatal(err)
			}
		}
		emit = func(string, []byte) error { t.Errorf("%s: emitted", tc.data); return nil }
		if err := s.Event(tc.name, []byte(tc.data), emit); err == nil || !strings.HasPrefix(err.Error(), "an event of the stream") {
			t.Errorf("%q: %v", tc.data, err)
		}
	}

	boom := errors.New("boom")
	fails := func(string, []byte) error { return boom }
	for _, name := range []string{"message_start", "ping"} {
		s := NewChatStream(&domain.Target{}, &domain.Provider{}, true, clock)
		if err := s.Event(name, []byte(start), fails); !errors.Is(err, boom) {
			t.Errorf("%s with a failed emit: %v", name, err)
		}
	}
	s := NewChatStream(&domain.Target{}, &domain.Provider{}, true, clock)
	_ = s.Event("message_start", []byte(start), func(string, []byte) error { return nil })
	if err := s.Event("message_stop", []byte(`{"type":"message_stop"}`), fails); !errors.Is(err, boom) {
		t.Errorf("message_stop with a failed emit: %v", err)
	}
}

// FuzzChatStream re-chunks a text, escaping the tags of every other piece,
// and a tool input, into deltas around thinking deltas, pings and a second
// tool call: every emitted event is a chunk with the stream's id, valid
// JSON, or a comment or [DONE]; each tool call opens once, in order, before
// its arguments, which join to the input, or to "{}" when every fragment is
// empty (a tool without parameters); the visible text is the reference's,
// each text block stripped on its own when inline_think_tags is declared;
// the usage chunk comes once, when asked.
func FuzzChatStream(f *testing.F) {
	f.Add("<think>plan</think>\n\nanswer <b>", `{"city":"Paris","n":[1,2]}`, []byte{3, 9, 1}, true, true)
	f.Add("a<think>never closed</thi", `{}`, []byte{1, 1, 1, 1, 1, 1, 1, 1}, true, false)
	f.Add("x<", `{"q":"<think>"}`, []byte{1}, false, true)
	f.Add("y", "", []byte{0}, true, true)
	block := regexp.MustCompile(`(?s)<think>.*?(?:</think>|\z)`)
	f.Fuzz(func(t *testing.T, content, input string, cuts []byte, think, usage bool) {
		if !utf8.ValidString(content) || !utf8.ValidString(input) {
			return // decoded JSON strings are valid UTF-8
		}
		cut := func(s string) []string {
			var pieces []string
			for _, c := range cuts {
				n := min(int(c)%10, len(s))
				for n < len(s) && !utf8.RuneStart(s[n]) {
					n++
				}
				pieces, s = append(pieces, s[:n]), s[n:]
			}
			return append(pieces, s)
		}
		quote := func(s string, escape bool) string {
			q, err := jsontext.AppendQuote(nil, s)
			if err != nil {
				t.Fatal(err)
			}
			if escape { // the escaped spelling of the tags
				return strings.NewReplacer("<", `\u003c`, ">", `\u003e`, "/", `\/`).Replace(string(q))
			}
			return string(q)
		}
		type ev struct{ name, data string }
		in := []ev{{"message_start", `{"type":"message_start","message":{"id":"msg_f","model":"m","usage":{"input_tokens":5,"output_tokens":1}}}`},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`}}
		for _, p := range cut(content) {
			in = append(in, ev{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":` + quote(p, false) + `}}`},
				ev{"ping", `{"type":"ping"}`})
		}
		in = append(in, ev{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			ev{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`})
		for i, p := range cut(content) {
			in = append(in, ev{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":` + quote(p, i%2 == 1) + `}}`})
		}
		in = append(in, ev{"content_block_stop", `{"type":"content_block_stop","index":1}`},
			ev{"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t0","name":"f","input":{}}}`})
		for _, p := range cut(input) {
			in = append(in, ev{"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":` + quote(p, false) + `}}`})
		}
		in = append(in, ev{"content_block_stop", `{"type":"content_block_stop","index":2}`},
			ev{"content_block_start", `{"type":"content_block_start","index":3,"content_block":{"type":"text","text":` + quote(content, false) + `}}`},
			ev{"content_block_start", `{"type":"content_block_start","index":4,"content_block":{"type":"tool_use","id":"t1","name":"g","input":{}}}`},
			ev{"content_block_delta", `{"type":"content_block_delta","index":4,"delta":{"type":"input_json_delta","partial_json":""}}`},
			ev{"content_block_stop", `{"type":"content_block_stop","index":4}`},
			ev{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`},
			ev{"message_stop", `{"type":"message_stop"}`})

		var visible strings.Builder
		args := map[int]string{}
		var opened []int
		usages, done := 0, false
		emit := func(name string, data []byte) error {
			switch {
			case done:
				t.Fatalf("%q after [DONE]", data)
			case name == Comment:
				return nil
			case string(data) == "[DONE]":
				done = true
				return nil
			}
			var c struct {
				ID, Object string
				Choices    []struct {
					Delta struct {
						Content   string
						ToolCalls []struct {
							Index    int
							ID       *string
							Function struct{ Arguments string }
						} `json:"tool_calls"`
					}
				}
				Usage *struct {
					Total int `json:"total_tokens"`
				}
			}
			if err := json.Unmarshal(data, &c); err != nil || c.ID != "msg_f" || c.Object != "chat.completion.chunk" {
				t.Fatalf("emitted %q: %v", data, err)
			}
			if c.Usage != nil {
				usages++
			}
			for _, ch := range c.Choices {
				visible.WriteString(ch.Delta.Content)
				for _, tc := range ch.Delta.ToolCalls {
					if tc.ID != nil {
						if len(opened) != tc.Index {
							t.Fatalf("tool call %d opened after %v", tc.Index, opened)
						}
						opened = append(opened, tc.Index)
					} else if len(opened) <= tc.Index {
						t.Fatalf("arguments of tool call %d before it opened", tc.Index)
					}
					args[tc.Index] += tc.Function.Arguments
				}
			}
			return nil
		}
		s := NewChatStream(&domain.Target{InlineThinkTags: think}, &domain.Provider{}, usage, clock)
		for i, e := range in {
			if err := s.Event(e.name, []byte(e.data), emit); err != nil && (err != io.EOF || i != len(in)-1) {
				t.Fatalf("%s: %v", e.data, err)
			}
		}
		want := content + content
		if think {
			want = block.ReplaceAllString(content, "") + block.ReplaceAllString(content, "")
		}
		wantUsages := 0
		if usage {
			wantUsages = 1
		}
		o := s.Observed()
		if !done || visible.String() != want || o.VisibleBytes != int64(len(want)) || !reflect.DeepEqual(opened, []int{0, 1}) ||
			!reflect.DeepEqual(args, map[int]string{0: cmp.Or(input, "{}"), 1: "{}"}) || o.ToolCalls != 2 || usages != wantUsages || o.FinishReason != "end_turn" {
			t.Fatalf("visible %q (%d bytes), want %q; opened %v, args %v, usage chunks %d; observed %+v", visible.String(), o.VisibleBytes, want,
				opened, args, usages, o)
		}
		if o.Usage == nil || o.Usage.Output != 9 || o.Usage.Input != 5 {
			t.Fatalf("usage %+v", o.Usage)
		}
	})
}
