package api

import (
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/usage"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// rerankConfig: pay's rerank_url is the mock's /rerank, on another host than
// its base_url, which nothing calls; rerank is a rerank alias whose cap,
// forced effort and drop rule passthrough never applies; fb falls back from
// pay/rr-1 to pay/rr-2; reported reads its provider's usage.cost; nourl
// reaches a rerank target whose provider has no rerank_url, embonly an
// embeddings target, anthonly an anthropic target (422); secret is an alias
// the key may not use. The responses are capped at 1 KiB.
const rerankConfig = `version: 1
settings: { max_body_bytes: 4096, max_response_bytes: 1024 }
providers:
  pay: { protocol: openai, base_url: "http://127.0.0.2:9/v1", rerank_url: MOCKURL/rerank, api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true, gen_ai_provider: cohere }
  rep: { protocol: openai, base_url: MOCKURL, rerank_url: MOCKURL/rerank, api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true, cost_report: { path: usage.cost, usd_per_unit: 1 } }
  bare: { protocol: openai, base_url: MOCKURL, api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true }
  anth: { protocol: anthropic, base_url: MOCKURL, rerank_url: MOCKURL/rerank, api_key_env: MOCK_KEY, billing: none, allow_private_network: true }
targets:
  pay/rr-1: { kind: rerank, pricing: { input_per_million: 1, output_per_million: 0, as_of: "2026-11-01" } }
  pay/rr-2: { kind: rerank, pricing: { input_per_million: 1, output_per_million: 0, as_of: "2026-11-01" } }
  rep/rr-3: { kind: rerank }
  bare/rr: { kind: rerank, pricing: { input_per_million: 1, output_per_million: 0, as_of: "2026-11-01" } }
  pay/emb: { kind: embeddings, pricing: { input_per_million: 1, output_per_million: 0, as_of: "2026-11-01" } }
  anth/rr: { kind: rerank }
models:
  rerank: { targets: [pay/rr-1], reasoning: { effort: high, mode: forced }, max_output_tokens: 100, params: { top_n: drop } }
  fb: { targets: [pay/rr-1, pay/rr-2] }
  reported: { targets: [rep/rr-3] }
  nourl: { targets: [bare/rr] }
  embonly: { targets: [pay/emb] }
  anthonly: { targets: [anth/rr] }
  secret: { targets: [pay/rr-1] }
keys:
  agent: { models: [rerank, fb, reported, nourl, embonly, anthonly] }
`

// mockRerank is the default rerank answer of the mock, which the proxy must
// relay byte for byte.
const mockRerank = `{"object":"list","model":"mock","results":[{"index":0,"relevance_score":0.5}],"usage":{"total_tokens":3}}`

func newRerank(t *testing.T, traced bool) (*fixture, *mockprovider.Server, *httptest.Server, string) {
	t.Helper()
	f := newFixture(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	yaml := strings.ReplaceAll(rerankConfig, "MOCKURL", srv.URL)
	if traced {
		yaml = f.traced(yaml)
	}
	return f, mock, srv, f.apply(t, yaml)["agent"]
}

// TestRerank is AC-46 for rerank through the data plane (phase-4 design
// section 5): served toward the provider's rerank_url with the answer relayed
// byte for byte, Cohere- and Jina-shaped, the body sent with only model
// replaced, every other byte and the key order kept, a stream member
// forwarded and no alias rule, cap or effort applied, the operator's
// credential and no client header; refused before any provider with the
// protections of chat, a repeated top-level key and invalid UTF-8 anywhere
// included (kick-off section 7.1), leaving no record; a target without
// rerank_url, of another kind or protocol skipped (422); TEI's bare array, a
// body that is not JSON and a malformed object relayed unchanged and
// estimated; a usage-free
// object estimated and priced by the table; total_tokens as input; a
// provider cost winning over the table; fallen back after a 503 with one
// record per attempt under one request id; an object repeating a key in
// usage is 502 and not tried again; counted per key and alias (OPS-8).
func TestRerank(t *testing.T) {
	f, mock, _, token := newRerank(t, false)
	post := func(body string, header ...string) *httptest.ResponseRecorder {
		t.Helper()
		return f.traffic("/v1/rerank", token, body, header...)
	}
	sent := func(n int) mockprovider.Call {
		t.Helper()
		calls := mock.Calls()
		if len(calls) != n {
			t.Fatalf("%d provider calls, want %d", len(calls), n)
		}
		return calls[n-1]
	}

	for _, tt := range []struct {
		name, token, body string
		status            int
		code, param       string
	}{
		{"no key", "", `{"model":"rerank","query":"q"}`, 401, "invalid_api_key", ""},
		{"a body over max_body_bytes", token, `{"model":"rerank","query":"` + strings.Repeat("a", 4096) + `"}`, 413, "request_too_large", ""},
		{"not JSON", token, `{"model":"rerank",`, 400, "invalid_request", ""},
		{"invalid UTF-8 inside a member", token, "{\"model\":\"rerank\",\"documents\":[{\"text\":\"\xff\"}]}", 400, "invalid_request", ""},
		{"a repeated top-level key", token, `{"model":"rerank","query":"a","query":"b"}`, 400, "invalid_request", "query"},
		{"not an object", token, `["rerank"]`, 400, "invalid_request", ""},
		{"no model", token, `{"query":"q"}`, 400, "invalid_request", "model"},
		{"model not a string", token, `{"model":1,"query":"q"}`, 400, "invalid_request", "model"},
		{"an unknown alias", token, `{"model":"nope","query":"q"}`, 404, "model_not_found", ""},
		{"an alias the key may not use", token, `{"model":"secret","query":"q"}`, 403, "model_not_allowed", ""},
		{"a provider without rerank_url", token, `{"model":"nourl","query":"q"}`, 422, "endpoint_not_supported", ""},
		{"an embeddings target only", token, `{"model":"embonly","query":"q"}`, 422, "endpoint_not_supported", ""},
		{"an anthropic target only", token, `{"model":"anthonly","query":"q"}`, 422, "endpoint_not_supported", ""},
	} {
		w := f.traffic("/v1/rerank", tt.token, tt.body)
		e := envelopeOf(t, w)
		if w.Code != tt.status || e.Code != tt.code || e.Param != tt.param || w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: %d %s", tt.name, w.Code, w.Body)
		}
	}
	if n := len(mock.Calls()); n != 0 {
		t.Fatalf("%d provider calls for refused requests", n)
	}

	// Cohere-shaped, with a stream member, a key the alias drops on other
	// endpoints and a repeat inside reasoning, which chat's walk would refuse
	// (only the top level is indexed): the mock's bytes, unchanged; the
	// provider gets the body with only model replaced at the rerank_url, the
	// operator's credential and no client header (PAR-14).
	const cohere = `{ "query":"what is it?", "model" :"rerank","documents":["a",{"text":"b"}],"top_n":1,"stream":true,"return_documents":false,"reasoning":{"k":1,"k":2} }`
	w := post(cohere, "X-Custom", "client-header")
	if w.Code != http.StatusOK || w.Body.String() != mockRerank || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("cohere: %d %s", w.Code, w.Body)
	}
	if h := w.Header(); h.Get(targetHeader) != "pay/rr-1" || h.Values(effortHeader) != nil || h.Values(changedHeader) != nil || h.Get(traceHeader) == "" {
		t.Errorf("cohere headers: %v", h)
	}
	c := sent(1)
	if c.Path != "/rerank" || string(c.Body) != strings.Replace(cohere, `"model" :"rerank"`, `"model" :"rr-1"`, 1) ||
		c.Header.Get("Authorization") != "Bearer "+planted+"MOCK_KEY" || c.Header.Get("X-Custom") != "" || c.Header.Get("Content-Type") != "application/json" {
		t.Errorf("cohere sent: %s %s %v", c.Path, c.Body, c.Header)
	}

	// Jina-shaped, with prompt_tokens and total_tokens: prompt_tokens is the input.
	const jina = `{"model":"rerank","query":"q","documents":[{"text":"a"}],"top_n":2,"return_documents":true}`
	jinaAnswer := `{"model":"jina-reranker-v2","usage":{"prompt_tokens":2,"total_tokens":5},"results":[{"index":0,"relevance_score":0.8,"document":{"text":"a"}}]}`
	mock.Script(mockprovider.Step{Body: []byte(jinaAnswer)})
	if w := post(jina); w.Code != http.StatusOK || w.Body.String() != jinaAnswer {
		t.Errorf("jina: %d %s", w.Code, w.Body)
	}
	if c := sent(2); string(c.Body) != `{"model":"rr-1","query":"q","documents":[{"text":"a"}],"top_n":2,"return_documents":true}` {
		t.Errorf("jina sent: %s", c.Body)
	}

	// TEI's bare array, with its tokens only in a header, a well-formed object
	// holding invalid UTF-8, one that starts as an object but is cut short and
	// plain text:
	// not usable JSON objects, so relayed unchanged and unobserved, each
	// estimated from the bytes sent.
	tei := `[{"index":0,"score":0.99},{"index":1,"score":0.01}]`
	mock.Script(mockprovider.Step{Header: http.Header{"X-Compute-Tokens": {"9"}}, Body: []byte(tei)}, mockprovider.Step{Body: []byte("{\"results\":[],\"x\":\"\xff\"}")},
		mockprovider.Step{Body: []byte(`{"results":[{"index":0}`)}, mockprovider.Step{Body: []byte("ok\xff")})
	if w := post(`{"model":"rerank","query":"q","texts":["a","b"]}`); w.Code != http.StatusOK || w.Body.String() != tei {
		t.Errorf("TEI: %d %s", w.Code, w.Body)
	}
	if w := post(`{"model":"rerank","query":"q","texts":["a","b"]}`); w.Code != http.StatusOK || w.Body.String() != "{\"results\":[],\"x\":\"\xff\"}" {
		t.Errorf("invalid UTF-8 in an object: %d %q", w.Code, w.Body)
	}
	if w := post(`{"model":"rerank","query":"q","texts":["a","b"]}`); w.Code != http.StatusOK || w.Body.String() != `{"results":[{"index":0}` {
		t.Errorf("a malformed object: %d %s", w.Code, w.Body)
	}
	if w := post(`{"model":"rerank","query":"q","texts":["a","b"]}`); w.Code != http.StatusOK || w.Body.String() != "ok\xff" {
		t.Errorf("not JSON: %d %q", w.Code, w.Body)
	}

	// An object without usage (Cohere's meta): estimated, priced by the table.
	cohereAnswer := `{"id":"r1","results":[{"index":0,"relevance_score":0.9}],"meta":{"billed_units":{"search_units":1}}}`
	mock.Script(mockprovider.Step{Body: []byte(cohereAnswer)})
	if w := post(`{"model":"rerank","query":"hello","documents":["a"]}`); w.Code != http.StatusOK || w.Body.String() != cohereAnswer {
		t.Errorf("without usage: %d %s", w.Code, w.Body)
	}

	// The provider's cost at its cost_report path wins over any table.
	mock.Script(mockprovider.Step{Body: []byte(`{"model":"rr-3","results":[],"usage":{"search_units":1,"total_tokens":4,"cost":0.25}}`)})
	if w := post(`{"model":"reported","query":"q"}`); w.Code != http.StatusOK {
		t.Errorf("reported: %d %s", w.Code, w.Body)
	}

	// Fallback: a 503 of the first target, then the second; two records
	// under one request id (RES-1, USE-1).
	mock.Script(mockprovider.Step{Status: http.StatusServiceUnavailable, Body: []byte(`{"error":{"message":"busy"}}`)})
	fb := post(`{"model":"fb","query":"q"}`)
	if fb.Code != http.StatusOK || fb.Body.String() != mockRerank || fb.Header().Get(targetHeader) != "pay/rr-2" {
		t.Errorf("fallback: %d %v %s", fb.Code, fb.Header(), fb.Body)
	}
	if c := mock.Calls(); len(c) != 10 || string(c[8].Body) != `{"model":"rr-1","query":"q"}` || string(c[9].Body) != `{"model":"rr-2","query":"q"}` {
		t.Errorf("fallback sent: %d calls", len(c))
	}

	// An object that repeats a key in usage: 502, not relayed and not tried
	// on the next target, recorded with the estimate of its 28 bytes sent.
	mock.Script(mockprovider.Step{Body: []byte(`{"usage":{"total_tokens":1,"total_tokens":2}}`)})
	if w := post(`{"model":"fb","query":"q"}`); w.Code != http.StatusBadGateway || envelopeOf(t, w).Code != "upstream_error" {
		t.Errorf("unusable: %d %s", w.Code, w.Body)
	}
	if n := len(mock.Calls()); n != 11 {
		t.Errorf("%d provider calls, want 11", n)
	}

	rows := f.stored(t, 11)
	p := func(v float64) *float64 { return &v }
	for i, want := range []struct {
		model, target  string
		status         int
		errorType      string
		in             int64
		source         string
		cost           *float64
		costSource     string
		attempt        int
		responseModel  string
		sameAsPrevious bool
	}{
		{"rerank", "pay/rr-1", 200, "", 3, "provider", p(3e-6), "price_table", 1, "mock", false},
		{"rerank", "pay/rr-1", 200, "", 2, "provider", p(2e-6), "price_table", 1, "jina-reranker-v2", false},
		{"rerank", "pay/rr-1", 200, "", 12, "estimated", p(12e-6), "price_table", 1, "", false}, // 46 bytes sent
		{"rerank", "pay/rr-1", 200, "", 12, "estimated", p(12e-6), "price_table", 1, "", false},
		{"rerank", "pay/rr-1", 200, "", 12, "estimated", p(12e-6), "price_table", 1, "", false},
		{"rerank", "pay/rr-1", 200, "", 12, "estimated", p(12e-6), "price_table", 1, "", false},
		{"rerank", "pay/rr-1", 200, "", 13, "estimated", p(13e-6), "price_table", 1, "", false}, // 50 bytes sent
		{"reported", "rep/rr-3", 200, "", 4, "provider", p(0.25), "provider", 1, "rr-3", false},
		{"fb", "pay/rr-1", 503, "upstream_error", 0, "provider", p(0), "price_table", 1, "", false},
		{"fb", "pay/rr-2", 200, "", 3, "provider", p(3e-6), "price_table", 2, "mock", true},
		{"fb", "pay/rr-1", 200, "upstream_error", 7, "estimated", p(7e-6), "price_table", 1, "", false},
	} {
		r := rows[i]
		if r.Endpoint != "rerank" || r.Key != "agent" || r.Model != want.model || r.Target != want.target || r.Status != want.status ||
			str(r.ErrorType) != want.errorType || r.Stream || str(r.FinishReason) != "" || r.Input != want.in || r.Output != 0 || r.Reasoning != nil ||
			r.TokensSource != want.source || r.Cost == nil || math.Abs(*r.Cost-*want.cost) > 1e-15 || r.CostSource != want.costSource ||
			r.EffortSent != nil || r.MaxOutputSent != nil || str(r.ParamsChanged) != "" || r.Empty || r.TTFT != nil ||
			r.Attempt != want.attempt || str(r.ResponseModel) != want.responseModel || (r.RequestID == rows[max(i-1, 0)].RequestID) != (want.sameAsPrevious || i == 0) {
			t.Errorf("row %d: %+v", i, r)
		}
	}
	if fb.Header().Get(domain.RequestIDHeader) != rows[9].RequestID {
		t.Errorf("the fallback's request id %q, records %q", fb.Header().Get(domain.RequestIDHeader), rows[9].RequestID)
	}

	counted := map[usage.Counter]float64{}
	f.counters.Each(func(k usage.Counter, v float64) { counted[k] = v })
	for k, want := range map[usage.Counter]float64{
		{Name: "requests", Key: "agent", Model: "rerank", Label: "200"}:   7,
		{Name: "requests", Key: "agent", Model: "fb", Label: "200"}:       1,
		{Name: "requests", Key: "agent", Model: "fb", Label: "502"}:       1,
		{Name: "requests", Key: "agent", Model: "nourl", Label: "422"}:    1,
		{Name: "tokens", Key: "agent", Model: "rerank", Label: "input"}:   66,
		{Name: "tokens", Key: "agent", Model: "reported", Label: "input"}: 4,
	} {
		if counted[k] != want {
			t.Errorf("counter %+v = %v, want %v", k, counted[k], want)
		}
	}
	for k := range counted {
		if k.Name == "empty" || k.Label == "output" {
			t.Errorf("counter %+v on rerank", k)
		}
	}
}

// TestRerankSpans is the rerank span through the data plane (TEL-4, annex
// D): named "rerank {alias}" with the custom gen_ai.operation.name rerank,
// server.address and server.port of the rerank_url called, not of the
// provider's base_url on another host, output tokens the recorded 0, and no
// content although the key opted in (TEL-8 off chat); a fallback adds one
// attempt child.
func TestRerankSpans(t *testing.T) {
	f, mock, srv, token := newRerank(t, true)
	if w := call(f.admin, "Bearer "+adminKey, http.MethodPatch, "/admin/v1/keys/agent", []byte(`{"content":"1h"}`)); w.Code != http.StatusOK {
		t.Fatalf("content opt-in: %d %s", w.Code, w.Body)
	}
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.ParseInt(u.Port(), 10, 64)
	ok := f.traffic("/v1/rerank", token, `{"model":"rerank","query":"a query that must stay out of every span","documents":["a"]}`)
	mock.Script(mockprovider.Step{Status: http.StatusServiceUnavailable})
	fb := f.traffic("/v1/rerank", token, `{"model":"fb","query":"q"}`)
	if ok.Code != http.StatusOK || fb.Code != http.StatusOK {
		t.Fatalf("%d %d", ok.Code, fb.Code)
	}
	f.stored(t, 3)

	byTrace := map[string][]otlpSpan{}
	for _, s := range f.exported(t) {
		byTrace[s.TraceID] = append(byTrace[s.TraceID], s)
		for _, a := range s.Attributes {
			if a.Value.String != nil && (strings.Contains(*a.Value.String, "a query") || strings.Contains(*a.Value.String, "relevance")) {
				t.Errorf("span %q carries content in %s", s.Name, a.Key)
			}
		}
	}
	served := map[string]any{"stargaite.endpoint": "rerank", "gen_ai.operation.name": "rerank", "gen_ai.provider.name": "cohere",
		"server.address": "127.0.0.1", "server.port": port, "gen_ai.usage.output_tokens": int64(0), "gen_ai.usage.input_tokens": int64(3),
		"stargaite.reasoning.effort.source": "provider_default", "stargaite.response.empty": false, "http.response.status_code": int64(200)}
	content := []string{"gen_ai.input.messages", "gen_ai.output.messages", "stargaite.content.truncated", "gen_ai.request.max_tokens",
		"stargaite.reasoning.effort.sent", "gen_ai.request.stream"}

	s := byTrace[ok.Header().Get(traceHeader)]
	if len(s) != 1 || s[0].Name != "rerank rerank" || s[0].Kind != 2 {
		t.Fatalf("success: %+v", s)
	}
	s[0].has(t, map[string]any{"stargaite.model": "rerank", "stargaite.target": "pay/rr-1", "stargaite.attempts": int64(1)}, content...)
	s[0].has(t, served)

	s = byTrace[fb.Header().Get(traceHeader)]
	if len(s) != 2 || s[0].Name != "rerank fb" || s[1].Name != "attempt pay/rr-1" || s[1].Kind != 3 || s[1].ParentSpanID != s[0].SpanID {
		t.Fatalf("fallback: %+v", s)
	}
	s[0].has(t, map[string]any{"stargaite.target": "pay/rr-2", "stargaite.attempts": int64(2)})
	s[0].has(t, served)
	s[1].has(t, map[string]any{"stargaite.target": "pay/rr-1", "error.type": "upstream_error", "http.response.status_code": int64(503),
		"gen_ai.operation.name": "rerank", "server.address": "127.0.0.1", "server.port": port}, "gen_ai.usage.input_tokens")
}
