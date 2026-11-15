package telemetry

import (
	"bytes"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/usage"
)

var update = flag.Bool("update", false, "rewrite testdata/spans.json from Build")

// Parse is W3C Trace Context Level 1, read strictly (TEL-6).
func TestParse(t *testing.T) {
	const (
		trace  = "0af7651916cd43dd8448eb211c80319c"
		parent = "b7ad6b7169203331"
	)
	valid := "00-" + trace + "-" + parent + "-01"
	for _, tt := range []struct {
		name   string
		values []string
		ok     bool
	}{
		{"valid", []string{valid}, true},
		{"flags are read for nothing", []string{"00-" + trace + "-" + parent + "-00"}, true},
		{"flags of any value", []string{"00-" + trace + "-" + parent + "-fe"}, true},
		{"a higher version, exactly 55", []string{"01-" + trace + "-" + parent + "-01"}, true},
		{"a higher version with more after a dash", []string{"cc-" + trace + "-" + parent + "-01-what-the-future-holds"}, true},
		{"no header", nil, false},
		{"two headers", []string{valid, valid}, false},
		{"empty", []string{""}, false},
		{"version ff", []string{"ff-" + trace + "-" + parent + "-01"}, false},
		{"version 00 with more", []string{valid + "-x"}, false},
		{"a higher version with more and no dash", []string{"01-" + trace + "-" + parent + "-01x"}, false},
		{"uppercase trace id", []string{"00-" + strings.ToUpper(trace) + "-" + parent + "-01"}, false},
		{"uppercase parent id", []string{"00-" + trace + "-" + strings.ToUpper(parent) + "-01"}, false},
		{"uppercase version", []string{"0A-" + trace + "-" + parent + "-01"}, false},
		{"uppercase flags", []string{"00-" + trace + "-" + parent + "-0A"}, false},
		{"all-zero trace id", []string{"00-" + strings.Repeat("0", 32) + "-" + parent + "-01"}, false},
		{"all-zero parent id", []string{"00-" + trace + "-" + strings.Repeat("0", 16) + "-01"}, false},
		{"short", []string{valid[:54]}, false},
		{"a field out of place", []string{"00-" + trace[:31] + "-" + "0" + parent + "-01"}, false},
		{"not hex", []string{"00-" + trace[:31] + "g-" + parent + "-01"}, false},
		{"a space in a field", []string{"00-" + trace + "-" + parent[:15] + " -01"}, false},
		{"comma-joined values", []string{valid + "," + valid}, false},
	} {
		c, ok := Parse(tt.values)
		if ok != tt.ok {
			t.Errorf("%s: ok %v", tt.name, ok)
			continue
		}
		if !ok && c != (Context{}) {
			t.Errorf("%s: context %+v", tt.name, c)
		}
		if ok && (!c.Remote || c.Trace[0] != 0x0a || c.Parent[7] != 0x31) {
			t.Errorf("%s: context %+v", tt.name, c)
		}
	}
	c := NewContext()
	if c.Trace == [16]byte{} || c.Parent != [8]byte{} || c.Remote || NewContext() == c {
		t.Errorf("NewContext %+v", c)
	}
}

// Sampled keeps none at 0, all at 1, and about half at 0.5 of uniform
// draws, below rate × 2^64 (TEL-7).
func TestSampled(t *testing.T) {
	for _, tt := range []struct {
		draw uint64
		rate float64
		want bool
	}{
		{0, 0, false}, {1<<64 - 1, 1, true}, {0, 1, true}, {0, 0.5, true}, {1<<63 - 1, 0.5, true}, {1 << 63, 0.5, false},
		{1<<64 - 1, 0.999, false}, {0, 1e-9, true}, {1 << 40, 1e-9, false},
	} {
		if got := Sampled(tt.draw, tt.rate); got != tt.want {
			t.Errorf("Sampled(%d, %v) = %v", tt.draw, tt.rate, got)
		}
	}
	for _, rate := range []float64{0, 0.5, 1} {
		kept := 0
		for range 10_000 {
			var b [8]byte
			random(b[:])
			if Sampled(binary.LittleEndian.Uint64(b[:]), rate) {
				kept++
			}
		}
		if lo, hi := int(rate*10_000)-300, int(rate*10_000)+300; kept < max(lo, 0) || kept > min(hi, 10_000) {
			t.Errorf("rate %v kept %d of 10,000", rate, kept)
		}
	}
}

// goldenCases are one request per span shape (AC-25): a success, a stream,
// an error, a refusal before any provider, a fallback whose failed attempts
// are child spans, a success under the content opt-in (TEL-8), an
// embeddings success and a rerank success.
func goldenCases() []struct {
	name string
	req  Request
} {
	at := time.UnixMilli(1_790_000_000_000).UTC()
	ms := at.UnixMilli()
	p := func(s string) *string { return &s }
	n := func(v int64) *int64 { return &v }
	usd := func(v float64) *float64 { return &v }
	remote := Context{Trace: [16]byte{0x0a, 0xf7, 15: 0x9c}, Parent: [8]byte{0xb7, 7: 0x31}, Remote: true}
	root := Context{Trace: [16]byte{0x11, 15: 0x22}}
	ok := usage.Record{Target: "pay/up-1", Billing: domain.BillingPayPerUse, ResponseModel: "up-1-2026", StatusCode: 200, FinishReason: "stop",
		InputTokens: 10, OutputTokens: 20, TokensSource: "provider", CostUSD: usd(0.00005), CostSource: "price_table",
		ReasoningTokens: n(0), EffortRequested: p("high"), EffortSent: p("high"), EffortSource: "client", MaxOutputRequested: n(100),
		MaxOutputAlias: n(500), MaxOutputSent: n(100), ParamsChanged: "max_tokens:translated", StartedAt: ms + 2, CompletedAt: ms + 250}
	stream := usage.Record{Target: "sub/claude", Billing: domain.BillingSubscription, ResponseModel: "claude", ModelMismatch: true, StatusCode: 200,
		FinishReason: "end_turn", Stream: true, InputTokens: 100, CacheReadTokens: 40, CacheWriteTokens: 10, OutputTokens: 30,
		ReasoningTokens: n(12), TokensSource: "provider", CostSource: "none", EffortAlias: p("medium"), EffortSent: p("budget:2048"),
		EffortSource: "alias_default", EffortAdjusted: true, MaxOutputSent: n(4096), ParamsChanged: "thinking:added",
		TTFTMS: n(120), StartedAt: ms + 5, CompletedAt: ms + 900}
	failed := usage.Record{Target: "pay/up-1", Billing: domain.BillingPayPerUse, StatusCode: 500, ErrorType: "upstream_error",
		TokensSource: "provider", CostUSD: usd(0), CostSource: "price_table", EffortSource: "provider_default", StartedAt: ms + 3, CompletedAt: ms + 40}
	cut := usage.Record{Target: "pay/free", Billing: domain.BillingPayPerUse, StatusCode: 200, ErrorType: "upstream_error", Stream: true,
		InputTokens: 7, OutputTokens: 3, TokensSource: "estimated", CostSource: "none", EffortSent: p("none"), EffortSource: "client",
		TTFTMS: n(15), StartedAt: ms + 41, CompletedAt: ms + 80}
	served := usage.Record{Target: "anth/claude", Billing: domain.BillingNone, ResponseModel: "claude-x", StatusCode: 200, FinishReason: "end_turn",
		Stream: true, InputTokens: 9, OutputTokens: 4, TokensSource: "provider", CostSource: "none", EffortSent: p("high"), EffortSource: "client",
		ParamsChanged: "protocol:translated,output_config.effort:added", TTFTMS: n(30), StartedAt: ms + 81, CompletedAt: ms + 300}
	emb := usage.Record{Target: "pay/emb-1", Billing: domain.BillingPayPerUse, ResponseModel: "emb-1", StatusCode: 200, InputTokens: 5,
		TokensSource: "provider", CostUSD: usd(0.0000001), CostSource: "price_table", EffortSource: "provider_default", StartedAt: ms + 2, CompletedAt: ms + 80}
	rr := usage.Record{Target: "pay/rerank-1", Billing: domain.BillingPayPerUse, StatusCode: 200, InputTokens: 12,
		TokensSource: "estimated", CostUSD: usd(0.0000012), CostSource: "price_table", EffortSource: "provider_default", StartedAt: ms + 2, CompletedAt: ms + 60}
	base := Request{Key: "agent", Alias: "fast", Endpoint: domain.EndpointChat, Version: "v1.2.3", Revision: 7, Status: 200, Started: at, Ended: at.Add(time.Second)}
	with := func(f func(r *Request)) Request {
		r := base
		f(&r)
		return r
	}
	return []struct {
		name string
		req  Request
	}{
		{"success", with(func(r *Request) {
			r.Context, r.Session = remote, "conv-42"
			r.Attempts = []Attempt{{Record: ok, Provider: "openai", Host: "api.example.com", Port: 443, Dialect: "reasoning_effort", Level: "high"}}
		})},
		{"stream", with(func(r *Request) {
			r.Context, r.Endpoint, r.Stream, r.Alias = root, domain.EndpointMessages, true, "claude"
			r.Attempts = []Attempt{{Record: stream, Provider: "anthropic", Host: "127.0.0.1", Port: 8080, Dialect: "thinking_budget", Stripped: true}}
		})},
		{"error", with(func(r *Request) {
			r.Context, r.Status, r.Code, r.Message = root, 502, "upstream_error", "the provider answered 500"
			r.Attempts = []Attempt{{Record: failed, Provider: "pay", Host: "api.example.com", Port: 443, Dialect: "none", Message: "the provider answered 500"}}
		})},
		{"refusal", with(func(r *Request) {
			r.Context, r.Session, r.Status, r.Code, r.Message = root, "conv-42", 429, "rate_limit_exceeded", "the rpm limit of key agent is reached"
			r.Stream = true
		})},
		{"refusal without an alias", with(func(r *Request) {
			r.Context, r.Alias, r.Status, r.Code, r.Message = root, "", 400, "invalid_request", "the request body is not valid JSON"
		})},
		{"attempt", with(func(r *Request) {
			// The first attempt starts in the request's millisecond, after it.
			r.Context, r.Stream, r.Started = remote, true, at.Add(3200*time.Microsecond)
			r.Attempts = []Attempt{
				{Record: failed, Provider: "pay", Host: "api.example.com", Port: 443, Dialect: "none", Message: "the provider answered 500",
					Started: at.Add(3400 * time.Microsecond), Ended: at.Add(40600 * time.Microsecond)},
				{Record: cut, Provider: "pay", Host: "api.example.com", Port: 443, Dialect: "reasoning_effort", Level: "none", Message: "the provider's stream failed",
					Started: at.Add(41300 * time.Microsecond), Ended: at.Add(80500 * time.Microsecond)},
				{Record: served, Provider: "anthropic", Host: "api.anthropic.com", Port: 443, Dialect: "adaptive_effort", Level: "high"},
			}
		})},
		{"content", with(func(r *Request) {
			// The limit of 29 bytes falls inside the "é" of the input's 29th and
			// 30th bytes, so 28 are kept; the output fits, its invalid byte
			// replaced.
			r.Context, r.ContentMax = root, 29
			r.Attempts = []Attempt{{Record: ok, Provider: "openai", Host: "api.example.com", Port: 443, Dialect: "reasoning_effort", Level: "high"}}
			r.Input, r.Output = []byte(`[{"role":"user","content":"héllo"}]`), []byte("a\xffb")
		})},
		{"embeddings", with(func(r *Request) {
			// Only the operation name differs: output tokens are the recorded 0,
			// and the effort source, adjusted and dialect stay (phase-4 design
			// section 4).
			r.Context, r.Endpoint, r.Alias = root, domain.EndpointEmbeddings, "embed"
			r.Attempts = []Attempt{{Record: emb, Provider: "openai", Host: "api.example.com", Port: 443, Dialect: "none"}}
		})},
		{"rerank", with(func(r *Request) {
			// The custom operation rerank; server.address and server.port of the
			// rerank_url called, an estimated record with output 0 (phase-4
			// design section 5).
			r.Context, r.Endpoint, r.Alias = root, domain.EndpointRerank, "rerank"
			r.Attempts = []Attempt{{Record: rr, Provider: "cohere", Host: "rerank.example.net", Port: 8443, Dialect: "none"}}
		})},
	}
}

// content cuts at a UTF-8 boundary to the limit in bytes, says when it cut,
// and replaces invalid UTF-8, which the JSON encoding cannot carry (TEL-8).
func TestContent(t *testing.T) {
	for _, tt := range []struct {
		in    string
		limit int64
		want  string
		cut   bool
	}{
		{"abc", 3, "abc", false}, {"abcd", 3, "abc", true}, {"ab€", 4, "ab", true}, {"ab€", 5, "ab€", false},
		{"€", 2, "", true}, {"a\xff", 4, "a\uFFFD", false}, {"a\xff", 2, "a", true}, {"abc", 0, "", true},
	} {
		if got, cut := content([]byte(tt.in), tt.limit); got != tt.want || cut != tt.cut {
			t.Errorf("content(%q, %d) = %q, %v", tt.in, tt.limit, got, cut)
		}
	}
}

// TestBuild is the golden of the span schema (AC-25, annex D v1): per span
// shape, the name, kind, parent, flags, status and every attribute, encoded
// as OTLP JSON, with the ids Build drew replaced once their relations are
// checked: the request span in the request's trace, under the remote parent
// when there is one, each attempt span its child with an id of its own,
// within its parent's times.
func TestBuild(t *testing.T) {
	type entry struct {
		Case  string           `json:"case"`
		Spans []jsontext.Value `json:"spans"`
	}
	golden := struct {
		Conventions   string  `json:"conventions"`
		SchemaVersion int     `json:"schema_version"`
		Cases         []entry `json:"cases"`
	}{Conventions: "semantic-conventions-genai@e6658f14c9d9ea0760825f029cb82e7673f48fab", SchemaVersion: SchemaVersion}
	for _, tc := range goldenCases() {
		spans := Build(tc.req)
		if want := max(len(tc.req.Attempts), 1); len(spans) != want {
			t.Fatalf("%s: %d spans, want %d", tc.name, len(spans), want)
		}
		ids, reqID := map[[8]byte]bool{}, spans[0].ID
		for i, s := range spans {
			if s.Trace != tc.req.Context.Trace || s.ID == [8]byte{} || ids[s.ID] {
				t.Errorf("%s: span %d trace %x id %x", tc.name, i, s.Trace, s.ID)
			}
			ids[s.ID] = true
			want := reqID
			if i == 0 {
				want = tc.req.Context.Parent
			}
			if s.Parent != want || s.Remote != (i == 0 && tc.req.Context.Remote) {
				t.Errorf("%s: span %d parent %x remote %v", tc.name, i, s.Parent, s.Remote)
			}
			if s.Start.Before(spans[0].Start) || s.End.After(spans[0].End) {
				t.Errorf("%s: span %d runs %v to %v, outside its parent's %v to %v", tc.name, i, s.Start, s.End, spans[0].Start, spans[0].End)
			}
			spans[i].ID = [8]byte{0: 0xee, 7: byte(i + 1)}
			if i > 0 {
				spans[i].Parent = spans[0].ID
			}
		}
		e := entry{Case: tc.name}
		for _, s := range spans {
			data, err := encode(s)
			if err != nil {
				t.Fatal(err)
			}
			e.Spans = append(e.Spans, data)
		}
		golden.Cases = append(golden.Cases, e)
	}
	got, err := json.Marshal(golden, jsontext.WithIndent("  "))
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *update {
		if err := os.WriteFile("testdata/spans.json", got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile("testdata/spans.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Build differs from testdata/spans.json (go test -update rewrites it after a reviewed change):\n%s", got)
	}
}
