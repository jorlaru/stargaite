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
		got, err := Observe([]byte(tt.body))
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: %+v (usage %+v), error %v", tt.name, got, got.Usage, err)
		}
	}
	for _, body := range []string{``, `[]`, `{"model":"m"`, `{"choices":[{"message":{"content":"a","content":"b"}}]}`, `{"usage":{"prompt_tokens":1,"prompt_tokens":2}}`} {
		if _, err := Observe([]byte(body)); err == nil {
			t.Errorf("%q: no error", body)
		}
	}
}
