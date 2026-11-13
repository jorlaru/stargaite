package openai

import (
	"reflect"
	"testing"

	"github.com/jorlaru/stargaite/internal/domain"
)

// TestObserve is the usage table of design section 7 and the rest of what
// a synchronous response is observed for.
func TestObserve(t *testing.T) {
	r := int64(5)
	for _, tt := range []struct {
		name, body string
		want       domain.Observed
	}{
		{"full", `{"id":"x","model":"up-2","choices":[` +
			`{"index":0,"message":{"role":"assistant","content":"hé","tool_calls":[{"id":"a"},{"id":"b"}]},"finish_reason":"tool_calls"},` +
			`{"index":1,"message":{"role":"assistant","content":"abc"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":7,"total_tokens":17,"prompt_tokens_details":{"cached_tokens":4,"cache_write_tokens":2},` +
			`"completion_tokens_details":{"reasoning_tokens":5}}}`,
			domain.Observed{ResponseModel: "up-2", FinishReason: "tool_calls", VisibleBytes: 6, ToolCalls: 2,
				Usage: &domain.Usage{Input: 10, CacheRead: 4, CacheWrite: 2, Output: 7, Reasoning: &r}}},
		{"no reasoning tokens", `{"model":"m","choices":[{"message":{"content":null},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
			domain.Observed{ResponseModel: "m", FinishReason: "length", Usage: &domain.Usage{Input: 1, Output: 2}}},
		{"usage null", `{"model":"m","choices":[],"usage":null}`, domain.Observed{ResponseModel: "m"}},
		{"no usage, no choices", `{}`, domain.Observed{}},
	} {
		body, got, err := Observe(&domain.Target{}, &domain.Provider{}, []byte(tt.body))
		if err != nil || !reflect.DeepEqual(got, tt.want) || string(body) != tt.body {
			t.Errorf("%s: %+v (usage %+v), error %v", tt.name, got, got.Usage, err)
		}
	}
	for _, body := range []string{``, `[]`, `{"model":"m"`, `{"choices":[{"message":{"content":"a","content":"b"}}]}`, `{"usage":{"prompt_tokens":1,"prompt_tokens":2}}`} {
		if _, _, err := Observe(&domain.Target{InlineThinkTags: true}, &domain.Provider{}, []byte(body)); err == nil {
			t.Errorf("%q: no error", body)
		}
	}
}

// TestObserveThink is AC-19 on a synchronous body: toward a target that
// declares inline_think_tags, every block of every message content is
// removed, an escaped spelling and an unclosed block included; only the
// strings that changed are re-encoded, so every other byte stays the
// provider's. A body without blocks, and any body toward a target that does
// not declare them, is returned as it came.
func TestObserveThink(t *testing.T) {
	think := &domain.Target{InlineThinkTags: true}
	for _, tt := range []struct {
		name, body, want string
		visible          int64
		stripped         bool
	}{
		{"escaped tags", `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"\u003cthink\u003eplan\u003c/think\u003e\n\nHi \u00e9"},"finish_reason":"stop"}],"usage":null}`,
			`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"\n\nHi é"},"finish_reason":"stop"}],"usage":null}`, 7, true},
		{"several blocks, two choices", `{"choices":[{"message":{"content":"<think>a</think>A<think>b</think>B"}},{"message":{"content":"\u0041 <think>x</think>"}},{"message":{"content":"\u0043"}}]}`,
			`{"choices":[{"message":{"content":"AB"}},{"message":{"content":"A "}},{"message":{"content":"\u0043"}}]}`, 5, true},
		{"unclosed", `{"choices":[{"message":{"content":"answer<think>cut off","tool_calls":[{"id":"a","function":{"arguments":"<think>"}}]}}]}`,
			`{"choices":[{"message":{"content":"answer","tool_calls":[{"id":"a","function":{"arguments":"<think>"}}]}}]}`, 6, true},
		{"held prefix after a block", `{"choices":[{"message":{"content":"<think>a</think>3 <"}}]}`, `{"choices":[{"message":{"content":"3 <"}}]}`, 3, true},
		{"only a block", `{"choices":[{"message":{"content":"<think>all</think>"}}]}`, `{"choices":[{"message":{"content":""}}]}`, 0, true},
		{"no block", `{"choices":[{"message":{"content":"a <thing> \u003c/think>"}},{"message":{"content":null}}]}`,
			`{"choices":[{"message":{"content":"a <thing> \u003c/think>"}},{"message":{"content":null}}]}`, 18, false},
	} {
		body, o, err := Observe(think, &domain.Provider{}, []byte(tt.body))
		if err != nil || string(body) != tt.want || o.VisibleBytes != tt.visible || o.Stripped != tt.stripped {
			t.Errorf("%s: %s, %+v, %v", tt.name, body, o, err)
		}
		body, o, err = Observe(&domain.Target{}, &domain.Provider{}, []byte(tt.body))
		if err != nil || string(body) != tt.body || o.Stripped {
			t.Errorf("%s, no inline_think_tags: %s, %+v, %v", tt.name, body, o, err)
		}
	}
}
