package usage

import (
	"math"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/plan"
)

func ptr[T any](v T) *T { return &v }

var t0 = time.UnixMilli(1_790_000_000_000)

// base is a synchronous chat call to a priced pay_per_use target that
// answered 200 with its usage: 100 input tokens, 40 of them cache reads, and
// 50 output tokens, 20 of them reasoning.
func base() Call {
	return Call{
		RequestID: "req", TraceID: "0af7651916cd43dd8448eb211c80319c", Key: "k", Model: "fast", Version: "v1.2.3",
		Endpoint: domain.EndpointChat, Attempt: 2, Status: 200, TargetID: "p/up-1",
		Target:   &domain.Target{Pricing: &domain.Pricing{InputPerMillion: ptr(1.0), OutputPerMillion: ptr(2.0)}, ResponseModels: []string{"alias-"}},
		Provider: &domain.Provider{Billing: domain.BillingPayPerUse},
		Out: &plan.Outgoing{Body: []byte(`{"model":"up-1","messages":[],"max_tokens":9}`), // 45 bytes
			Changes:      []plan.Change{{Name: "max_tokens", Action: "added"}, {Name: "reasoning_effort", Action: "translated"}},
			Effort:       plan.Layers[string]{Requested: ptr("high"), Alias: ptr("low"), Sent: ptr("medium")},
			EffortSource: "client", EffortAdjusted: true,
			MaxOutput: plan.Layers[int64]{Alias: ptr(int64(9)), Sent: ptr(int64(9))}},
		Complete: true,
		Obs: domain.Observed{ResponseModel: "up-1-2026-11-01", FinishReason: "stop", VisibleBytes: 30,
			Usage: &domain.Usage{Input: 100, CacheRead: 40, Output: 50, Reasoning: ptr(int64(20))}},
		Started: t0, Ended: t0.Add(2 * time.Second), Revision: 7,
	}
}

// TestNewRecordColumns checks every column of one successful call.
func TestNewRecordColumns(t *testing.T) {
	want := Record{
		ID: "req-2", RequestID: "req", TraceID: "0af7651916cd43dd8448eb211c80319c", Attempt: 2, Endpoint: "chat", KeyName: "k",
		Model: "fast", Target: "p/up-1", Billing: "pay_per_use", ResponseModel: "up-1-2026-11-01", StatusCode: 200,
		FinishReason: "stop", InputTokens: 100, CacheReadTokens: 40, OutputTokens: 50, ReasoningTokens: ptr(int64(20)),
		TokensSource: "provider", CostUSD: ptr((60*1.0 + 40*1.0 + 50*2.0) / 1e6), CostSource: "price_table",
		EffortRequested: ptr("high"), EffortAlias: ptr("low"), EffortSent: ptr("medium"), EffortSource: "client", EffortAdjusted: true,
		MaxOutputAlias: ptr(int64(9)), MaxOutputSent: ptr(int64(9)), ParamsChanged: "max_tokens:added,reasoning_effort:translated",
		StartedAt: t0.UnixMilli(), CompletedAt: t0.UnixMilli() + 2000, ConfigRevision: 7, ProxyVersion: "v1.2.3",
	}
	if got := NewRecord(base()); !reflect.DeepEqual(got, want) {
		t.Errorf("record\n%+v\nwant\n%+v", got, want)
	}
}

// TestNewRecord is NewRecord per outcome and billing (design sections 5 and
// 6, USE-3 to USE-8): tokens and their source, cost and its source, the
// mismatch, an empty answer and the time to the first event.
func TestNewRecord(t *testing.T) {
	type want struct {
		in, read, out int64
		reasoning     *int64
		source        string
		cost          *float64
		costSource    string
		mismatch      bool
		empty         bool
		ttft          *int64
		errorType     string
	}
	priced := ptr(200.0 / 1e6)
	for _, tc := range []struct {
		name string
		edit func(c *Call)
		want want
	}{
		{"success", func(*Call) {}, want{100, 40, 50, ptr(int64(20)), "provider", priced, "price_table", false, false, nil, ""}},
		{"the provider's cost wins", func(c *Call) { c.Obs.Cost = ptr(0.5) },
			want{100, 40, 50, ptr(int64(20)), "provider", ptr(0.5), "provider", false, false, nil, ""}},
		{"provider cost without prices", func(c *Call) { c.Obs.Cost, c.Target.Pricing = ptr(0.0), nil },
			want{100, 40, 50, ptr(int64(20)), "provider", ptr(0.0), "provider", false, false, nil, ""}},
		{"unpriced", func(c *Call) { c.Target.Pricing = nil }, want{100, 40, 50, ptr(int64(20)), "provider", nil, "none", false, false, nil, ""}},
		{"subscription", func(c *Call) { c.Provider.Billing, c.Obs.Cost = domain.BillingSubscription, ptr(0.5) },
			want{100, 40, 50, ptr(int64(20)), "provider", nil, "none", false, false, nil, ""}},
		{"billing none", func(c *Call) { c.Provider.Billing = domain.BillingNone }, want{100, 40, 50, ptr(int64(20)), "provider", nil, "none", false, false, nil, ""}},
		{"429", func(c *Call) {
			c.Status, c.ErrorCode, c.Obs, c.Complete = 429, "upstream_error", domain.Observed{}, false
		},
			want{0, 0, 0, nil, "provider", ptr(0.0), "price_table", false, false, nil, "upstream_error"}},
		{"5xx", func(c *Call) {
			c.Status, c.ErrorCode, c.Obs, c.Complete = 503, "upstream_error", domain.Observed{}, false
		},
			want{0, 0, 0, nil, "provider", ptr(0.0), "price_table", false, false, nil, "upstream_error"}},
		{"timer before a response", func(c *Call) {
			c.Status, c.ErrorCode, c.Obs, c.Complete = 0, "upstream_timeout", domain.Observed{}, false
		},
			want{0, 0, 0, nil, "provider", ptr(0.0), "price_table", false, false, nil, "upstream_timeout"}},
		{"client gone before the answer", func(c *Call) { c.Status, c.ErrorCode, c.Obs, c.Complete = 0, "client_gone", domain.Observed{}, false },
			want{0, 0, 0, nil, "provider", ptr(0.0), "price_table", false, false, nil, "client_gone"}},
		{"a sync answer whole, then the client gone", func(c *Call) { c.ErrorCode = "client_gone" },
			want{100, 40, 50, ptr(int64(20)), "provider", priced, "price_table", false, false, nil, "client_gone"}},
		{"cut stream without usage", func(c *Call) {
			c.Stream, c.Complete, c.ErrorCode, c.FirstEvent = true, false, "upstream_error", t0.Add(300*time.Millisecond)
			c.Obs = domain.Observed{ResponseModel: "up-1", VisibleBytes: 10}
		}, want{12, 0, 3, nil, "estimated", ptr((12*1.0 + 3*2.0) / 1e6), "price_table", false, false, ptr(int64(300)), "upstream_error"}},
		{"client gone mid-stream, partial usage kept", func(c *Call) {
			c.Stream, c.Complete, c.ErrorCode, c.FirstEvent = true, false, "client_gone", t0.Add(time.Second)
			c.Obs = domain.Observed{VisibleBytes: 30, Usage: &domain.Usage{Input: 100, CacheWrite: 10, Output: 1}}
		}, want{100, 0, 8, nil, "estimated", ptr((90*1.0 + 10*1.0 + 8*2.0) / 1e6), "price_table", false, false, ptr(int64(1000)), "client_gone"}},
		{"stream complete without usage", func(c *Call) {
			c.Stream, c.FirstEvent = true, t0
			c.Obs.Usage = nil
		}, want{12, 0, 8, nil, "estimated", ptr((12*1.0 + 8*2.0) / 1e6), "price_table", false, false, ptr(int64(0)), ""}},
		{"oversized answer", func(c *Call) { c.Obs, c.Complete, c.ErrorCode = domain.Observed{}, false, "upstream_error" },
			want{12, 0, 0, nil, "estimated", ptr(12.0 / 1e6), "price_table", false, true, nil, "upstream_error"}},
		{"empty answer", func(c *Call) { c.Obs.VisibleBytes = 0 }, want{100, 40, 50, ptr(int64(20)), "provider", priced, "price_table", false, true, nil, ""}},
		{"tool calls are not empty", func(c *Call) { c.Obs.VisibleBytes, c.Obs.ToolCalls = 0, 1 },
			want{100, 40, 50, ptr(int64(20)), "provider", priced, "price_table", false, false, nil, ""}},
		{"mismatch", func(c *Call) { c.Obs.ResponseModel = "other-model" }, want{100, 40, 50, ptr(int64(20)), "provider", priced, "price_table", true, false, nil, ""}},
		{"a response_models prefix", func(c *Call) { c.Obs.ResponseModel = "alias-7" },
			want{100, 40, 50, ptr(int64(20)), "provider", priced, "price_table", false, false, nil, ""}},
		{"no response model", func(c *Call) { c.Obs.ResponseModel = "" }, want{100, 40, 50, ptr(int64(20)), "provider", priced, "price_table", false, false, nil, ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.edit(&c)
			r := NewRecord(c)
			got := want{r.InputTokens, r.CacheReadTokens, r.OutputTokens, r.ReasoningTokens, r.TokensSource, r.CostUSD, r.CostSource,
				r.ModelMismatch, r.ContentEmpty, r.TTFTMS, r.ErrorType}
			if !reflect.DeepEqual(got.reasoning, tc.want.reasoning) || !reflect.DeepEqual(got.ttft, tc.want.ttft) || !near(got.cost, tc.want.cost) {
				t.Errorf("reasoning %v, ttft %v, cost %v; want %v, %v, %v", deref(got.reasoning), deref(got.ttft), deref(got.cost),
					deref(tc.want.reasoning), deref(tc.want.ttft), deref(tc.want.cost))
			}
			got.reasoning, got.ttft, got.cost = nil, nil, nil
			tc.want.reasoning, tc.want.ttft, tc.want.cost = nil, nil, nil
			if got != tc.want {
				t.Errorf("got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// TestNewRecordEndpoints is NewRecord per endpoint (phase-4 design section
// 4): content_empty only on chat and messages; an embeddings record with
// every column as the design lists it, its input reported, output 0,
// reasoning NULL, provider_default and every other effort and cap column
// NULL, priced by USE-6; a 2xx without usage estimated by USE-8 from the
// bytes sent (49, so 13 tokens), output 0, still priced; a failed call with zero tokens.
func TestNewRecordEndpoints(t *testing.T) {
	for _, tc := range []struct {
		ep    domain.Endpoint
		empty bool
	}{{domain.EndpointChat, true}, {domain.EndpointMessages, true}, {domain.EndpointEmbeddings, false}} {
		c := base()
		c.Endpoint, c.Obs.VisibleBytes = tc.ep, 0
		if r := NewRecord(c); r.ContentEmpty != tc.empty || r.Endpoint != string(tc.ep) {
			t.Errorf("%s: endpoint %q content_empty %v", tc.ep, r.Endpoint, r.ContentEmpty)
		}
	}

	embeddings := func() Call {
		return Call{
			RequestID: "req", TraceID: "0af7651916cd43dd8448eb211c80319c", Key: "k", Model: "embed", Version: "v1.2.3",
			Endpoint: domain.EndpointEmbeddings, Attempt: 1, Status: 200, TargetID: "p/emb-1",
			Target:   &domain.Target{Kind: domain.KindEmbeddings, Pricing: &domain.Pricing{InputPerMillion: ptr(0.02), OutputPerMillion: ptr(0.0)}},
			Provider: &domain.Provider{Billing: domain.BillingPayPerUse},
			Out:      &plan.Outgoing{Body: []byte(`{"model":"emb-1","input":"hello world, embedded"}`), EffortSource: "provider_default"}, // 49 bytes
			Complete: true,
			Obs:      domain.Observed{ResponseModel: "emb-1", Usage: &domain.Usage{Input: 5}},
			Started:  t0, Ended: t0.Add(80 * time.Millisecond), Revision: 3,
		}
	}
	want := Record{
		ID: "req-1", RequestID: "req", TraceID: "0af7651916cd43dd8448eb211c80319c", Attempt: 1, Endpoint: "embeddings", KeyName: "k",
		Model: "embed", Target: "p/emb-1", Billing: "pay_per_use", ResponseModel: "emb-1", StatusCode: 200, InputTokens: 5,
		TokensSource: "provider", CostUSD: ptr(5 * 0.02 / 1e6), CostSource: "price_table", EffortSource: "provider_default",
		StartedAt: t0.UnixMilli(), CompletedAt: t0.UnixMilli() + 80, ConfigRevision: 3, ProxyVersion: "v1.2.3",
	}
	got := NewRecord(embeddings())
	if !near(got.CostUSD, want.CostUSD) {
		t.Errorf("cost %v, want %v", deref(got.CostUSD), deref(want.CostUSD))
	}
	if got.CostUSD, want.CostUSD = nil, nil; !reflect.DeepEqual(got, want) {
		t.Errorf("record\n%+v\nwant\n%+v", got, want)
	}

	c := embeddings()
	c.Obs.Usage = nil
	if r := NewRecord(c); r.InputTokens != 13 || r.OutputTokens != 0 || r.ReasoningTokens != nil || r.TokensSource != "estimated" ||
		!near(r.CostUSD, ptr(13*0.02/1e6)) || r.CostSource != "price_table" || r.ContentEmpty {
		t.Errorf("a 2xx without usage: %+v", r)
	}
	c = embeddings()
	c.Status, c.ErrorCode, c.Obs, c.Complete = 503, "upstream_error", domain.Observed{}, false
	if r := NewRecord(c); r.InputTokens != 0 || r.OutputTokens != 0 || r.TokensSource != "provider" || r.ErrorType != "upstream_error" || r.ContentEmpty {
		t.Errorf("a 503: %+v", r)
	}
}

func near(a, b *float64) bool {
	return a == nil && b == nil || a != nil && b != nil && math.Abs(*a-*b) < 1e-15
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestPriceCost: each part at its own price, a missing cache price at the
// input price, reasoning inside the output.
func TestPriceCost(t *testing.T) {
	u := domain.Usage{Input: 1_000_000, CacheRead: 200_000, CacheWrite: 100_000, Output: 500_000, Reasoning: ptr(int64(400_000))}
	for _, tc := range []struct {
		p    domain.Pricing
		want float64
	}{
		{domain.Pricing{InputPerMillion: ptr(3.0), OutputPerMillion: ptr(15.0)}, 3 + 7.5},
		{domain.Pricing{InputPerMillion: ptr(3.0), OutputPerMillion: ptr(15.0), CacheReadPerMillion: ptr(0.3), CacheWritePerMillion: ptr(3.75)},
			0.7*3 + 0.2*0.3 + 0.1*3.75 + 7.5},
		{domain.Pricing{InputPerMillion: ptr(0.0), OutputPerMillion: ptr(0.0)}, 0},
	} {
		if got := PriceCost(&tc.p, u); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%+v: %v, want %v", tc.p, got, tc.want)
		}
	}
	// Cache parts above the input never price a negative plain input.
	if got := PriceCost(&domain.Pricing{InputPerMillion: ptr(1.0), OutputPerMillion: ptr(1.0)}, domain.Usage{Input: 1, CacheRead: 5}); got != 5e-6 {
		t.Errorf("cache above input: %v", got)
	}
}

// TestCounters: Add accumulates, Each visits in order of name, key, alias and
// label.
func TestCounters(t *testing.T) {
	var c Counters
	c.Add(Counter{Name: "tokens", Key: "b", Label: "output"}, 2)
	c.Add(Counter{Name: "requests", Key: "b", Model: "fast", Label: "200"}, 1)
	c.Add(Counter{Name: "tokens", Key: "a", Label: "output"}, 3)
	c.Add(Counter{Name: "tokens", Key: "b", Label: "output"}, 5)
	var got []string
	c.Each(func(k Counter, v float64) {
		got = append(got, k.Name+"/"+k.Key+"/"+k.Model+"/"+k.Label+"="+strconv.FormatFloat(v, 'g', -1, 64))
	})
	if want := []string{"requests/b/fast/200=1", "tokens/a//output=3", "tokens/b//output=7"}; !reflect.DeepEqual(got, want) {
		t.Errorf("%q", got)
	}
}

func TestLength(t *testing.T) {
	for reason, want := range map[string]bool{"length": true, "max_tokens": true, "model_context_window_exceeded": true, "stop": false, "end_turn": false, "": false} {
		if Length(reason) != want {
			t.Errorf("Length(%q) = %v", reason, !want)
		}
	}
}
