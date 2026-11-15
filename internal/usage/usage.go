// Package usage turns each provider call into one usage record, writes the
// records to the store in batches through one writer, answers usage show
// from the store, and holds the OPS-8 counters until the metrics endpoint
// renders them (phase-3 design sections 5, 6 and 13).
package usage

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/plan"
)

// Call is one provider call as finish saw it. targets verify (phase 4) fills
// it with Key "_verify" and Model "".
type Call struct {
	RequestID, TraceID, Key, Model, Version string
	Endpoint                                domain.Endpoint
	Attempt, Status                         int // Status: the provider's; 0 without a response
	TargetID                                string
	Target                                  *domain.Target
	Provider                                *domain.Provider
	Out                                     *plan.Outgoing
	Stream, Complete                        bool   // Complete: the answer was read to its end
	ErrorCode                               string // the kick-off section 15 code, "client_gone", or ""
	Obs                                     domain.Observed
	Started, FirstEvent, Ended              time.Time // the service clock
	Revision                                int64
}

// Record is one row of the usage table, one field per column in the order of
// annex A, under the column's name: an empty string or a nil pointer is NULL.
// It holds no content, so the critical log line of a record the writer could
// not store carries every column (USE-10).
type Record struct {
	ID                 string   `json:"id"`
	RequestID          string   `json:"request_id"`
	TraceID            string   `json:"trace_id"`
	Attempt            int      `json:"attempt"`
	Endpoint           string   `json:"endpoint"`
	KeyName            string   `json:"key_name"`
	Model              string   `json:"model"`
	Target             string   `json:"target"`
	Billing            string   `json:"billing"`
	ResponseModel      string   `json:"response_model"`
	ModelMismatch      bool     `json:"model_mismatch"`
	StatusCode         int      `json:"status_code"`
	ErrorType          string   `json:"error_type"`
	FinishReason       string   `json:"finish_reason"`
	Stream             bool     `json:"stream"`
	InputTokens        int64    `json:"input_tokens"`
	CacheReadTokens    int64    `json:"cache_read_tokens"`
	CacheWriteTokens   int64    `json:"cache_write_tokens"`
	OutputTokens       int64    `json:"output_tokens"`
	ReasoningTokens    *int64   `json:"reasoning_tokens"`
	TokensSource       string   `json:"tokens_source"`
	CostUSD            *float64 `json:"cost_usd"`
	CostSource         string   `json:"cost_source"`
	EffortRequested    *string  `json:"effort_requested"`
	EffortAlias        *string  `json:"effort_alias"`
	EffortSent         *string  `json:"effort_sent"`
	EffortSource       string   `json:"effort_source"`
	EffortAdjusted     bool     `json:"effort_adjusted"`
	MaxOutputRequested *int64   `json:"max_output_requested"`
	MaxOutputAlias     *int64   `json:"max_output_alias"`
	MaxOutputSent      *int64   `json:"max_output_sent"`
	ParamsChanged      string   `json:"params_changed"`
	ContentEmpty       bool     `json:"content_empty"`
	TTFTMS             *int64   `json:"ttft_ms"`
	StartedAt          int64    `json:"started_at"`   // Unix milliseconds
	CompletedAt        int64    `json:"completed_at"` // Unix milliseconds
	ConfigRevision     int64    `json:"config_revision"`
	ProxyVersion       string   `json:"proxy_version"`
}

// NewRecord is the record of c (design section 5). The upstream model id is
// the target id after its first "/"; a returned model that starts with
// neither it nor a response_models entry is a mismatch (USE-4). Tokens follow
// design section 6: after a 2xx, the provider's usage when it arrived and the
// answer was read to its end, else an estimate that keeps what was reported
// (input from the bytes sent, output from the visible bytes, each divided by
// four and rounded up, the larger output winning); without a 2xx, zero
// tokens with source provider. Cost: none for subscription and none billing;
// else the provider's figure, else the price table over the recorded
// tokens, else none (USE-6, USE-7). content_empty is read on chat and
// messages only: an embeddings answer has no visible bytes, and its estimate
// has output 0 (phase-4 design section 4).
func NewRecord(c Call) Record {
	r := Record{
		ID: fmt.Sprintf("%s-%d", c.RequestID, c.Attempt), RequestID: c.RequestID, TraceID: c.TraceID, Attempt: c.Attempt,
		Endpoint: string(c.Endpoint), KeyName: c.Key, Model: c.Model, Target: c.TargetID, Billing: c.Provider.Billing,
		ResponseModel: c.Obs.ResponseModel, StatusCode: c.Status, ErrorType: c.ErrorCode, FinishReason: c.Obs.FinishReason,
		Stream: c.Stream, TokensSource: "provider", CostSource: "none",
		EffortRequested: c.Out.Effort.Requested, EffortAlias: c.Out.Effort.Alias, EffortSent: c.Out.Effort.Sent,
		EffortSource: c.Out.EffortSource, EffortAdjusted: c.Out.EffortAdjusted,
		MaxOutputRequested: c.Out.MaxOutput.Requested, MaxOutputAlias: c.Out.MaxOutput.Alias, MaxOutputSent: c.Out.MaxOutput.Sent,
		ParamsChanged: plan.Header(c.Out.Changes), StartedAt: c.Started.UnixMilli(), CompletedAt: c.Ended.UnixMilli(),
		ConfigRevision: c.Revision, ProxyVersion: c.Version,
	}
	_, upstream, _ := strings.Cut(c.TargetID, "/")
	m := c.Obs.ResponseModel
	r.ModelMismatch = m != "" && !strings.HasPrefix(m, upstream) &&
		!slices.ContainsFunc(c.Target.ResponseModels, func(p string) bool { return strings.HasPrefix(m, p) })
	if c.Stream && !c.FirstEvent.IsZero() {
		ttft := c.FirstEvent.Sub(c.Started).Milliseconds()
		r.TTFTMS = &ttft
	}

	var u domain.Usage
	if c.Status/100 == 2 {
		if c.Obs.Usage != nil {
			u = *c.Obs.Usage
		}
		if c.Obs.Usage == nil || !c.Complete {
			r.TokensSource = "estimated"
			if c.Obs.Usage == nil {
				u.Input = quarter(int64(len(c.Out.Body)))
			}
			u.Output = max(u.Output, quarter(c.Obs.VisibleBytes))
		}
		chatLike := c.Endpoint == domain.EndpointChat || c.Endpoint == domain.EndpointMessages
		r.ContentEmpty = chatLike && c.Obs.VisibleBytes == 0 && c.Obs.ToolCalls == 0
	}
	r.InputTokens, r.CacheReadTokens, r.CacheWriteTokens, r.OutputTokens, r.ReasoningTokens = u.Input, u.CacheRead, u.CacheWrite, u.Output, u.Reasoning

	switch p := c.Target.Pricing; {
	case r.Billing != domain.BillingPayPerUse:
	case c.Obs.Cost != nil:
		r.CostUSD, r.CostSource = c.Obs.Cost, "provider"
	case p != nil && p.InputPerMillion != nil && p.OutputPerMillion != nil:
		cost := PriceCost(p, u)
		r.CostUSD, r.CostSource = &cost, "price_table"
	}
	return r
}

// quarter is n divided by four, rounded up: the USE-8 estimate of bytes.
func quarter(n int64) int64 { return (n + 3) / 4 }

// PriceCost is USD for u at p: each part at its own price, a missing cache
// price at the input price, reasoning at the output price (it is part of it).
// p must declare both the input and the output price.
func PriceCost(p *domain.Pricing, u domain.Usage) float64 {
	in, out := *p.InputPerMillion, *p.OutputPerMillion
	read, write := cmp.Or(p.CacheReadPerMillion, &in), cmp.Or(p.CacheWritePerMillion, &in)
	plain := max(u.Input-u.CacheRead-u.CacheWrite, 0)
	return (float64(plain)*in + float64(u.CacheRead)**read + float64(u.CacheWrite)**write + float64(u.Output)*out) / 1e6
}

// Length reports whether a provider's own finish reason says the answer hit
// an output or context limit: length, max_tokens or
// model_context_window_exceeded (USE-9, OPS-8).
func Length(reason string) bool {
	return reason == "length" || reason == "max_tokens" || reason == "model_context_window_exceeded"
}

// Counter names one OPS-8 counter: by key and alias, with a label such as a
// status, a token type or an action (design section 13, point 3).
type Counter struct{ Name, Key, Model, Label string }

// Counters are the OPS-8 counters, which the metrics endpoint of phase 4
// renders. The zero Counters is ready; it is safe for concurrent use.
type Counters struct {
	mu sync.Mutex
	m  map[Counter]float64
}

// Add adds v to the counter k.
func (c *Counters) Add(k Counter, v float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[Counter]float64{}
	}
	c.m[k] += v
}

// Each calls fn for every counter, sorted by name, key, alias and label,
// outside the lock.
func (c *Counters) Each(fn func(Counter, float64)) {
	c.mu.Lock()
	m := maps.Clone(c.m)
	c.mu.Unlock()
	for _, k := range slices.SortedFunc(maps.Keys(m), func(a, b Counter) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Key, b.Key), cmp.Compare(a.Model, b.Model), cmp.Compare(a.Label, b.Label))
	}) {
		fn(k, m[k])
	}
}
