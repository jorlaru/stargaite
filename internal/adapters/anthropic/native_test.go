package anthropic

import (
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jorlaru/stargaite/internal/domain"
)

// TestObserve: the native answer is relayed unchanged unless a text block
// held a think block, and a text block without a string text is not usable.
func TestObserve(t *testing.T) {
	const body = `{"id":"m","model":"claude-x","content":[{"type":"thinking","thinking":"<think>keep</think>","signature":"s"},` +
		`{"type":"text","text":"<think>x</think>Hi"},{"type":"tool_use","id":"t","name":"f","input":{}}],"stop_reason":"tool_use",` +
		`"usage":{"input_tokens":2,"cache_read_input_tokens":3,"output_tokens":4}}`
	for _, think := range []bool{false, true} {
		got, o, err := Observe(&domain.Target{InlineThinkTags: think}, &domain.Provider{}, []byte(body))
		want, visible := body, int64(len("<think>x</think>Hi"))
		if think {
			want, visible = strings.Replace(body, `"<think>x</think>Hi"`, `"Hi"`, 1), 2
		}
		if err != nil || string(got) != want || o.ResponseModel != "claude-x" || o.FinishReason != "tool_use" || o.ToolCalls != 1 ||
			o.VisibleBytes != visible || o.Stripped != think || o.Usage == nil || o.Usage.Input != 5 || o.Usage.Output != 4 {
			t.Errorf("think %v: %s %+v %v", think, got, o, err)
		}
	}
	for _, bad := range []string{`{"content":[{"type":"text","text":1}]}`, `{"model":"a","model":"b"}`, `[]`} {
		if _, _, err := Observe(&domain.Target{}, &domain.Provider{}, []byte(bad)); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

// FuzzStream: the native relay under random re-chunking of a text block's
// deltas, some in the escaped spelling of the tags. Every event keeps its
// name and is valid JSON; every event but the text deltas keeps its bytes;
// the text the client reads is the text without its think blocks toward a
// target that declares inline_think_tags, else the text itself; nothing
// follows message_stop.
func FuzzStream(f *testing.F) {
	f.Add("<think>plan</think>\n\nanswer <b>", []byte{3, 9, 1}, true)
	f.Add("a<think>never closed</thi", []byte{1, 1, 1, 1, 1, 1, 1, 1}, true)
	f.Add("x<", []byte{1}, false)
	f.Add("x<", []byte{1}, true)
	block := regexp.MustCompile(`(?s)<think>.*?(?:</think>|\z)`)
	f.Fuzz(func(t *testing.T, content string, cuts []byte, think bool) {
		if !utf8.ValidString(content) {
			return // decoded JSON strings are valid UTF-8
		}
		var pieces []string
		s := content
		for _, c := range cuts {
			n := min(int(c)%10, len(s))
			for n < len(s) && !utf8.RuneStart(s[n]) {
				n++
			}
			pieces, s = append(pieces, s[:n]), s[n:]
		}
		pieces = append(pieces, s)
		quote := func(s string, escape bool) string {
			q, err := jsontext.AppendQuote(nil, s)
			if err != nil {
				t.Fatal(err)
			}
			if escape {
				return strings.NewReplacer("<", `\u003c`, ">", `\u003e`, "/", `\/`).Replace(string(q))
			}
			return string(q)
		}
		type ev struct{ name, data string }
		in := []ev{{"message_start", `{"type":"message_start","message":{"id":"msg_f","model":"m","usage":{"input_tokens":5,"output_tokens":1}}}`},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":` + quote(content, false) + `}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`}}
		for i, p := range pieces {
			in = append(in, ev{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":` + quote(p, i%2 == 1) + `}}`},
				ev{"ping", `{"type":"ping"}`})
		}
		in = append(in, ev{"content_block_stop", `{"type":"content_block_stop","index":1}`},
			ev{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`},
			ev{"message_stop", `{"type":"message_stop"}`})

		var visible strings.Builder
		var out []ev
		emit := func(name string, data []byte) error {
			var d struct {
				Type  string
				Delta struct{ Type, Text string }
			}
			if err := json.Unmarshal(data, &d); err != nil || d.Type != name {
				t.Fatalf("emitted %s %q: %v", name, data, err)
			}
			if d.Delta.Type == "text_delta" {
				visible.WriteString(d.Delta.Text)
			}
			out = append(out, ev{name, string(data)})
			return nil
		}
		st := NewStream(&domain.Target{InlineThinkTags: think}, &domain.Provider{})
		for i, e := range in {
			if err := st.Event(e.name, []byte(e.data), emit); err != nil && (err != io.EOF || i != len(in)-1) {
				t.Fatalf("%s: %v", e.data, err)
			}
		}
		want := content
		if think {
			want = block.ReplaceAllString(content, "")
		}
		var kept, wantKept []ev
		for _, e := range out {
			if !strings.Contains(e.data, `"text_delta"`) {
				kept = append(kept, e)
			}
		}
		for _, e := range in {
			if !strings.Contains(e.data, `"text_delta"`) {
				wantKept = append(wantKept, e)
			}
		}
		o := st.Observed()
		if visible.String() != want || o.VisibleBytes != int64(len(want)) || len(kept) != len(wantKept) || out[len(out)-1].name != "message_stop" ||
			o.FinishReason != "end_turn" || o.Usage == nil || o.Usage.Output != 9 {
			t.Fatalf("visible %q (%d bytes), want %q; observed %+v; %d events", visible.String(), o.VisibleBytes, want, o, len(out))
		}
		for i := range kept {
			if kept[i] != wantKept[i] {
				t.Fatalf("event %d: %v, sent %v", i, kept[i], wantKept[i])
			}
		}
		if !think && len(out) != len(in) {
			t.Fatalf("%d events relayed for %d", len(out), len(in))
		}
	})
}

// TestCost: a provider of this protocol that declares a cost_report has its
// cost read by every observer, synchronous and streamed, the latest number in
// a stream winning (USE-6); none declared reads none.
func TestCost(t *testing.T) {
	p := &domain.Provider{CostReport: &domain.CostReport{Path: "usage.cost", USDPerUnit: 2}}
	tg := &domain.Target{}
	answer := `{"id":"msg_1","type":"message","model":"m","content":[{"type":"text","text":"a"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1,"cost":0.25}}`
	if _, o, err := Observe(tg, p, []byte(answer)); err != nil || o.Cost == nil || *o.Cost != 0.5 {
		t.Errorf("Observe: %v, error %v", o.Cost, err)
	}
	if _, o, err := ToChat(tg, p, []byte(answer), time.Unix(0, 0)); err != nil || o.Cost == nil || *o.Cost != 0.5 {
		t.Errorf("ToChat: %v, error %v", o.Cost, err)
	}
	if _, o, err := Observe(tg, &domain.Provider{}, []byte(answer)); err != nil || o.Cost != nil {
		t.Errorf("no cost_report: %v, error %v", o.Cost, err)
	}
	events := [][2]string{
		{"message_start", `{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1,"cost":0.1}}}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2,"cost":0.25}}`},
		{"message_delta", `{"type":"message_delta","delta":{},"usage":{"cost":null}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}
	native, chat := NewStream(tg, p), NewChatStream(tg, p, false, func() time.Time { return time.Unix(0, 0) })
	for _, e := range events {
		for _, ev := range []func(string, []byte, func(string, []byte) error) error{native.Event, chat.Event} {
			if err := ev(e[0], []byte(e[1]), func(string, []byte) error { return nil }); err != nil && err != io.EOF {
				t.Fatalf("%s: %v", e[0], err)
			}
		}
	}
	for name, o := range map[string]domain.Observed{"native": native.Observed(), "chat": chat.Observed()} {
		if o.Cost == nil || *o.Cost != 0.5 {
			t.Errorf("%s stream: %v", name, o.Cost)
		}
	}
}

// TestObserveInBand is RES-2 on the anthropic protocol (phase-5 design C45):
// an error in place of an answer, a body or event of type error or with a
// top-level error, is a *domain.ProviderError for Observe, ToChat and both
// streams; a type Anthropic documents gives its status and is the one word
// kept as Type, any other type stays inside the message, and an integer
// error.code is not read. An answer that also carries error is an answer.
func TestObserveInBand(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       domain.ProviderError
	}{
		{"overloaded_error", `{"type":"error","error":{"type":"overloaded_error","message":"words"}}`,
			domain.ProviderError{Status: 529, Type: "overloaded_error", Message: "words"}},
		{"invalid_request_error without a top-level type", `{"error":{"type":"invalid_request_error","message":"words"}}`,
			domain.ProviderError{Status: 400, Type: "invalid_request_error", Message: "words"}},
		{"billing_error", `{"type":"error","error":{"type":"billing_error","message":"words"}}`,
			domain.ProviderError{Status: 402, Type: "billing_error", Message: "words"}},
		{"an undocumented type", `{"type":"error","error":{"type":"a_new_error","message":"words"}}`, domain.ProviderError{Message: "a_new_error: words"}},
		{"a string error", `{"error":"words"}`, domain.ProviderError{Message: "words"}},
		{"type error alone", `{"type":"error"}`, domain.ProviderError{}},
		{"an integer code", `{"error":{"code":502,"message":"words"}}`, domain.ProviderError{Message: "words"}},
	} {
		check := func(where string, err error) {
			t.Helper()
			var pe *domain.ProviderError
			if !errors.As(err, &pe) || *pe != tc.want || strings.Contains(err.Error(), "words") {
				t.Errorf("%s, %s: %v", tc.name, where, err)
			}
		}
		_, _, err := Observe(&domain.Target{}, &domain.Provider{}, []byte(tc.body))
		check("Observe", err)
		_, _, err = ToChat(&domain.Target{}, &domain.Provider{}, []byte(tc.body), time.Unix(1, 0))
		check("ToChat", err)
		emit := func(string, []byte) error { t.Fatal("emitted"); return nil }
		check("Stream", NewStream(&domain.Target{}, &domain.Provider{}).Event("", []byte(tc.body), emit))
		check("ChatStream", NewChatStream(&domain.Target{}, &domain.Provider{}, false, time.Now).Event("", []byte(tc.body), emit))
	}
	answer := `{"id":"msg_1","model":"claude-x","content":[{"type":"text","text":"hi"}],"error":{"type":"api_error","message":"words"}}`
	if got, _, err := Observe(&domain.Target{}, &domain.Provider{}, []byte(answer)); err != nil || string(got) != answer {
		t.Errorf("Observe of an answer with an error: %s %v", got, err)
	}
	if got, _, err := ToChat(&domain.Target{}, &domain.Provider{}, []byte(answer), time.Unix(1, 0)); err != nil || !strings.Contains(string(got), `"content":"hi"`) {
		t.Errorf("ToChat of an answer with an error: %s %v", got, err)
	}
	if _, _, err := Observe(&domain.Target{}, &domain.Provider{}, []byte(`{"error":null}`)); err != nil {
		t.Errorf("a null error: %v", err)
	}
}
