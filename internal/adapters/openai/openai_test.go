package openai

import (
	"math"
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

// TestUsageRule is USE-3 on the openai protocol (phase-3 design section 6):
// the output is the larger of completion_tokens and total_tokens minus
// prompt_tokens, reasoning only an integer the provider reports, and cache
// reads fall back to Mistral's prompt_token_details.
func TestUsageRule(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	for _, tt := range []struct {
		name, usage string
		want        domain.Usage
	}{
		{"xAI: reasoning outside completion_tokens", `{"prompt_tokens":32,"completion_tokens":9,"total_tokens":135,"completion_tokens_details":{"reasoning_tokens":94}}`,
			domain.Usage{Input: 32, Output: 103, Reasoning: n(94)}},
		{"a plain provider", `{"prompt_tokens":10,"completion_tokens":7,"total_tokens":17,"completion_tokens_details":{"reasoning_tokens":5}}`,
			domain.Usage{Input: 10, Output: 7, Reasoning: n(5)}},
		{"no total", `{"prompt_tokens":10,"completion_tokens":7}`, domain.Usage{Input: 10, Output: 7}},
		{"a total below the parts", `{"prompt_tokens":10,"completion_tokens":7,"total_tokens":12}`, domain.Usage{Input: 10, Output: 7}},
		{"null reasoning", `{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"completion_tokens_details":{"reasoning_tokens":null}}`,
			domain.Usage{Input: 1, Output: 2}},
		{"reasoning zero", `{"prompt_tokens":1,"completion_tokens":2,"completion_tokens_details":{"reasoning_tokens":0}}`,
			domain.Usage{Input: 1, Output: 2, Reasoning: n(0)}},
		{"Mistral's cache reads", `{"prompt_tokens":100,"completion_tokens":2,"total_tokens":102,"prompt_token_details":{"cached_tokens":60}}`,
			domain.Usage{Input: 100, CacheRead: 60, Output: 2}},
		{"OpenAI's wins over Mistral's", `{"prompt_tokens":100,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":0},"prompt_token_details":{"cached_tokens":60}}`,
			domain.Usage{Input: 100, Output: 2}},
	} {
		_, got, err := Observe(&domain.Target{}, &domain.Provider{}, []byte(`{"choices":[],"usage":`+tt.usage+`}`))
		if err != nil || got.Usage == nil || !reflect.DeepEqual(*got.Usage, tt.want) {
			t.Errorf("%s: %+v, error %v", tt.name, got.Usage, err)
		}
		s := NewStream(&domain.Target{}, &domain.Provider{}, false)
		if err := s.Event("", []byte(`{"choices":[],"usage":`+tt.usage+`}`), func(string, []byte) error { return nil }); err != nil ||
			!reflect.DeepEqual(*s.Observed().Usage, tt.want) {
			t.Errorf("%s, streamed: %+v, error %v", tt.name, s.Observed().Usage, err)
		}
	}
}

// TestCost is the provider's cost (USE-6): the number at cost_report.path
// times usd_per_unit, nil when absent, null, not a number or negative; in a
// stream the latest one that is not null wins.
func TestCost(t *testing.T) {
	openrouter := &domain.Provider{CostReport: &domain.CostReport{Path: "usage.cost", USDPerUnit: 1}}
	xai := &domain.Provider{CostReport: &domain.CostReport{Path: "usage.cost_in_usd_ticks", USDPerUnit: 1e-10}}
	deep := &domain.Provider{CostReport: &domain.CostReport{Path: "x_billing.charge.usd", USDPerUnit: 1}}
	for _, tt := range []struct {
		name string
		p    *domain.Provider
		body string
		want *float64
	}{
		{"usage.cost", openrouter, `{"usage":{"prompt_tokens":1,"cost":0.0012}}`, ptr(0.0012)},
		{"ticks", xai, `{"usage":{"cost_in_usd_ticks":125000000}}`, ptr(0.0125)},
		{"a nested path", deep, `{"x_billing":{"charge":{"usd":2.5}}}`, ptr(2.5)},
		{"zero", openrouter, `{"usage":{"cost":0}}`, ptr(0.0)},
		{"absent", openrouter, `{"usage":{"prompt_tokens":1}}`, nil},
		{"null", openrouter, `{"usage":{"cost":null}}`, nil},
		{"a string", openrouter, `{"usage":{"cost":"0.1"}}`, nil},
		{"negative", openrouter, `{"usage":{"cost":-1}}`, nil},
		{"no cost_report", &domain.Provider{}, `{"usage":{"cost":0.5}}`, nil},
	} {
		_, got, err := Observe(&domain.Target{}, tt.p, []byte(tt.body))
		if err != nil || (got.Cost == nil) != (tt.want == nil) || got.Cost != nil && math.Abs(*got.Cost-*tt.want) > 1e-15 {
			t.Errorf("%s: %v, error %v", tt.name, got.Cost, err)
		}
	}
	// A repeated key on the way to the cost refuses the body, as anywhere the
	// proxy reads.
	if _, _, err := Observe(&domain.Target{}, deep, []byte(`{"x_billing":{"charge":{},"charge":{}}}`)); err == nil {
		t.Error("a repeated key on the cost path was read")
	}
	s := NewStream(&domain.Target{}, openrouter, false)
	for _, c := range []string{`{"choices":[],"usage":{"cost":0.5}}`, `{"choices":[],"usage":{"cost":null}}`, `{"choices":[],"usage":null}`,
		`{"choices":[],"usage":{"cost":0.75}}`, `{"choices":[]}`} {
		if err := s.Event("", []byte(c), func(string, []byte) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if c := s.Observed().Cost; c == nil || *c != 0.75 {
		t.Errorf("streamed cost %v", c)
	}
}

func ptr[T any](v T) *T { return &v }

// TestObserveUsage is an embeddings answer as each provider shapes it, base64
// and float alike (phase-4 design section 4): the body comes back unchanged;
// input is usage.prompt_tokens, else usage.total_tokens, output 0, reasoning
// nil, no usage without either integer; the cost is read at cost_report.path,
// also outside usage; the vectors are never read, so a repeat inside them is
// no error, while a body that is not an object or repeats a key in an object
// read is.
func TestObserveUsage(t *testing.T) {
	const vector = `[{"object":"embedding","index":0,"embedding":[0.0023,-0.0093,1e-5]}]`
	const b64 = `[{"object":"embedding","index":0,"embedding":"AAAAAAAAAD8AAIA/"}]`
	cost := func(v float64) *float64 { return &v }
	in := func(n int64) *domain.Usage { return &domain.Usage{Input: n} }
	openrouter := &domain.Provider{CostReport: &domain.CostReport{Path: "usage.cost", USDPerUnit: 1}}
	nested := &domain.Provider{CostReport: &domain.CostReport{Path: "billing.detail.cost", USDPerUnit: 0.5}}
	for _, tt := range []struct {
		name string
		p    *domain.Provider
		body string
		want domain.Observed
	}{
		{"OpenAI, float", &domain.Provider{}, `{"object":"list","data":` + vector + `,"model":"text-embedding-3-small","usage":{"prompt_tokens":8,"total_tokens":8}}`,
			domain.Observed{ResponseModel: "text-embedding-3-small", Usage: in(8)}},
		{"OpenAI, base64", &domain.Provider{}, `{"object":"list","data":` + b64 + `,"model":"text-embedding-3-small","usage":{"prompt_tokens":8,"total_tokens":8}}`,
			domain.Observed{ResponseModel: "text-embedding-3-small", Usage: in(8)}},
		{"Mistral", &domain.Provider{}, `{"id":"e1","object":"list","data":` + vector + `,"model":"mistral-embed","usage":{"prompt_tokens":15,"total_tokens":15,"completion_tokens":0}}`,
			domain.Observed{ResponseModel: "mistral-embed", Usage: in(15)}},
		{"Voyage, total only", &domain.Provider{}, `{"object":"list","data":` + vector + `,"model":"voyage-3.5","usage":{"total_tokens":10}}`,
			domain.Observed{ResponseModel: "voyage-3.5", Usage: in(10)}},
		{"Jina", &domain.Provider{}, `{"model":"jina-embeddings-v3","object":"list","usage":{"total_tokens":12,"prompt_tokens":11},"data":` + b64 + `}`,
			domain.Observed{ResponseModel: "jina-embeddings-v3", Usage: in(11)}},
		{"Jina, total only", &domain.Provider{}, `{"model":"jina-clip-v2","object":"list","usage":{"total_tokens":12},"data":` + vector + `}`,
			domain.Observed{ResponseModel: "jina-clip-v2", Usage: in(12)}},
		{"vLLM", &domain.Provider{}, `{"id":"embd-1","object":"list","created":1790000000,"model":"BAAI/bge-m3","data":` + vector +
			`,"usage":{"prompt_tokens":5,"total_tokens":5,"completion_tokens":0,"prompt_tokens_details":null}}`,
			domain.Observed{ResponseModel: "BAAI/bge-m3", Usage: in(5)}},
		{"OpenRouter, with its cost", openrouter, `{"object":"list","data":` + b64 + `,"model":"openai/text-embedding-3-small","usage":{"prompt_tokens":4,"total_tokens":4,"cost":0.00000008}}`,
			domain.Observed{ResponseModel: "openai/text-embedding-3-small", Usage: in(4), Cost: cost(0.00000008)}},
		{"a cost path outside usage", nested, `{"data":` + vector + `,"billing":{"detail":{"cost":2}},"usage":{"total_tokens":3}}`,
			domain.Observed{Usage: in(3), Cost: cost(1)}},
		{"prompt_tokens null", &domain.Provider{}, `{"usage":{"prompt_tokens":null,"total_tokens":7}}`, domain.Observed{Usage: in(7)}},
		{"a usage object without tokens", &domain.Provider{}, `{"usage":{"prompt_tokens":"8","completion_tokens":0}}`, domain.Observed{}},
		{"usage not an object", &domain.Provider{}, `{"usage":"8"}`, domain.Observed{}},
		{"zero reported", &domain.Provider{}, `{"usage":{"prompt_tokens":0,"total_tokens":0}}`, domain.Observed{Usage: in(0)}},
		{"usage null", &domain.Provider{}, `{"data":` + vector + `,"usage":null}`, domain.Observed{}},
		{"no usage", openrouter, `{"model":"m","data":` + vector + `}`, domain.Observed{ResponseModel: "m"}},
		{"a repeat inside the vectors", &domain.Provider{}, `{"data":[{"index":0,"index":1}],"usage":{"total_tokens":1}}`, domain.Observed{Usage: in(1)}},
	} {
		body, got, err := ObserveUsage(tt.p, []byte(tt.body))
		if err != nil || !reflect.DeepEqual(got, tt.want) || string(body) != tt.body {
			t.Errorf("%s: %+v (usage %+v, cost %v), error %v", tt.name, got, got.Usage, got.Cost, err)
		}
	}
	for _, body := range []string{``, `[]`, `[{"object":"embedding"}]`, `{"usage":`, `not json`, `{"model":"a","model":"b"}`,
		`{"usage":{"total_tokens":1,"total_tokens":2}}`, `{"billing":{"detail":{"cost":1,"cost":2}}}`} {
		if _, _, err := ObserveUsage(nested, []byte(body)); err == nil {
			t.Errorf("%q: no error", body)
		}
	}
}
