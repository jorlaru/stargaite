package api

import (
	"encoding/json"
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

// embeddingsConfig: embed is an embeddings alias whose cap and forced effort
// passthrough never writes; rules forwards input_type and drops task and
// the encoding_format its target refuses (PAR-2, PAR-12); strict reaches a
// target that refuses dimensions with no drop rule; fb falls back from
// pay/emb-1 to pay/emb-2; reported reads its provider's usage.cost; chatonly
// and anthonly reach no target that can serve embeddings (422); secret is
// an alias the key may not use. The responses are capped at 1 KiB.
const embeddingsConfig = `version: 1
settings: { max_body_bytes: 4096, max_response_bytes: 1024 }
providers:
  pay: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true, gen_ai_provider: openai }
  rep: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true, cost_report: { path: usage.cost, usd_per_unit: 1 } }
  anth: { protocol: anthropic, base_url: MOCK, api_key_env: MOCK_KEY, billing: none, allow_private_network: true }
targets:
  pay/emb-1: { kind: embeddings, pricing: { input_per_million: 1, output_per_million: 0, as_of: "2026-11-01" } }
  pay/emb-2: { kind: embeddings, pricing: { input_per_million: 1, output_per_million: 0, as_of: "2026-11-01" } }
  pay/strict: { kind: embeddings, unsupported_params: [encoding_format, dimensions], pricing: { input_per_million: 1, output_per_million: 0, as_of: "2026-11-01" } }
  rep/emb-3: { kind: embeddings }
  pay/chat: { kind: chat, pricing: { input_per_million: 1, output_per_million: 2, as_of: "2026-11-01" } }
  anth/emb: { kind: embeddings }
models:
  embed: { targets: [pay/emb-1], reasoning: { effort: high, mode: forced }, max_output_tokens: 100 }
  rules: { targets: [pay/strict], params: { input_type: forward, encoding_format: drop, task: drop } }
  strict: { targets: [pay/strict] }
  fb: { targets: [pay/emb-1, pay/emb-2] }
  reported: { targets: [rep/emb-3] }
  chatonly: { targets: [pay/chat] }
  anthonly: { targets: [anth/emb] }
  secret: { targets: [pay/emb-1] }
keys:
  agent: { models: [embed, rules, strict, fb, reported, chatonly, anthonly] }
`

// The default embeddings answer of the mock, which the proxy must relay
// byte for byte, and a base64 answer with total-only usage, as Voyage and
// some Jina models report it.
const (
	mockEmbeddings = `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0,0.5,1]}],"model":"mock",` +
		`"usage":{"prompt_tokens":1,"total_tokens":1}}`
	totalOnlyBase64 = `{"object":"list","data":[{"object":"embedding","index":0,"embedding":"AAAAAAAAAD8AAIA/"}],"model":"emb-1",` +
		`"usage":{"total_tokens":7}}`
)

func newEmbeddings(t *testing.T, traced bool) (*fixture, *mockprovider.Server, *httptest.Server, string) {
	t.Helper()
	f := newFixture(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	yaml := strings.ReplaceAll(embeddingsConfig, "base_url: MOCK", "base_url: "+srv.URL)
	if traced {
		yaml = f.traced(yaml)
	}
	return f, mock, srv, f.apply(t, yaml)["agent"]
}

// TestEmbeddings is AC-46 for embeddings through the data plane (phase-4
// design section 4): served with the vectors relayed byte for byte, float
// and base64, the body sent with only model replaced and the alias's rules
// applied (PAR-2, PAR-3), the operator's credential and no client header;
// refused before any provider with the protections of chat (SEC-2, LIM-6,
// PAR-1, PAR-2, RES-10), leaving no record; fallen back with one record per
// attempt under one request id (USE-1); an unusable or oversized 2xx is 502
// (RES-4); recorded per key and alias with input tokens, output 0, no
// reasoning, provider_default and no cap, estimated when no usage arrived
// (USE-3, USE-8), and counted (OPS-8).
func TestEmbeddings(t *testing.T) {
	f, mock, _, token := newEmbeddings(t, false)
	post := func(body string, header ...string) *httptest.ResponseRecorder {
		t.Helper()
		return f.traffic("/v1/embeddings", token, body, header...)
	}
	sent := func(n int) mockprovider.Call {
		t.Helper()
		calls := mock.Calls()
		if len(calls) != n {
			t.Fatalf("%d provider calls, want %d", len(calls), n)
		}
		return calls[n-1]
	}

	// Refused before any provider, in the order of the steps; none leaves a
	// record (USE-1) or a provider call.
	for _, tt := range []struct {
		name, token, body string
		status            int
		code, param       string
	}{
		{"no key", "", `{"model":"embed","input":"hi"}`, 401, "invalid_api_key", ""},
		{"a body over max_body_bytes", token, `{"model":"embed","input":"` + strings.Repeat("a", 4096) + `"}`, 413, "request_too_large", ""},
		{"not JSON", token, `{"model":"embed",`, 400, "invalid_request", ""},
		{"invalid UTF-8", token, "{\"model\":\"embed\",\"input\":\"\xff\"}", 400, "invalid_request", ""},
		{"a repeated top-level key", token, `{"model":"embed","input":"a","input":"b"}`, 400, "invalid_request", "input"},
		{"model not a string", token, `{"model":1,"input":"hi"}`, 400, "invalid_request", "model"},
		{"an unknown alias", token, `{"model":"nope","input":"hi"}`, 404, "model_not_found", ""},
		{"an alias the key may not use", token, `{"model":"secret","input":"hi"}`, 403, "model_not_allowed", ""},
		{"an unknown key", token, `{"model":"embed","input":"hi","input_type":"query"}`, 400, "unknown_parameter", "input_type"},
		{"stream", token, `{"model":"embed","input":"hi","stream":true}`, 400, "unknown_parameter", "stream"},
		{"an unsupported key with no drop rule", token, `{"model":"strict","input":"hi","dimensions":8}`, 422, "unsupported_parameter", "dimensions"},
		{"a chat target only", token, `{"model":"chatonly","input":"hi"}`, 422, "endpoint_not_supported", ""},
		{"an anthropic embeddings target only", token, `{"model":"anthonly","input":"hi"}`, 422, "endpoint_not_supported", ""},
	} {
		w := f.traffic("/v1/embeddings", tt.token, tt.body)
		e := envelopeOf(t, w)
		if w.Code != tt.status || e.Code != tt.code || e.Param != tt.param || w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: %d %s", tt.name, w.Code, w.Body)
		}
	}
	if n := len(mock.Calls()); n != 0 {
		t.Fatalf("%d provider calls for refused requests", n)
	}

	// Float: the mock's bytes, unchanged, and the PAR-9 headers; the
	// provider gets the body with only model replaced, the operator's
	// credential and no client header (PAR-14); the alias's cap and effort
	// are never written.
	w := post(`{"model":"embed", "input":["hi", "there"],"encoding_format":"float","user":"u-1"}`, "X-Custom", "client-header")
	if w.Code != http.StatusOK || w.Body.String() != mockEmbeddings || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("float: %d %s", w.Code, w.Body)
	}
	if h := w.Header(); h.Get(targetHeader) != "pay/emb-1" || h.Values(effortHeader) != nil || h.Values(changedHeader) != nil || h.Get(traceHeader) == "" {
		t.Errorf("float headers: %v", h)
	}
	c := sent(1)
	if c.Path != "/embeddings" || string(c.Body) != `{"model":"emb-1", "input":["hi", "there"],"encoding_format":"float","user":"u-1"}` ||
		c.Header.Get("Authorization") != "Bearer "+planted+"MOCK_KEY" || c.Header.Get("X-Custom") != "" || c.Header.Get("Content-Type") != "application/json" {
		t.Errorf("float sent: %s %s %v", c.Path, c.Body, c.Header)
	}

	// Base64 with total-only usage: relayed byte for byte, the total as input.
	mock.Script(mockprovider.Step{Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(totalOnlyBase64)})
	if w := post(`{"model":"embed","input":"hi","encoding_format":"base64"}`); w.Code != http.StatusOK || w.Body.String() != totalOnlyBase64 {
		t.Errorf("base64: %d %s", w.Code, w.Body)
	}
	if c := sent(2); string(c.Body) != `{"model":"emb-1","input":"hi","encoding_format":"base64"}` {
		t.Errorf("base64 sent: %s", c.Body)
	}

	// The alias's rules: input_type forwarded, task and encoding_format
	// dropped, each listed in body order (PAR-2, PAR-3).
	if w := post(`{"model":"rules","input_type":"query","input":"hi","task":"t","encoding_format":"base64"}`); w.Code != http.StatusOK ||
		w.Header().Get(changedHeader) != "input_type:forwarded,task:dropped,encoding_format:dropped" {
		t.Errorf("rules: %d %v %s", w.Code, w.Header(), w.Body)
	}
	if c := sent(3); string(c.Body) != `{"model":"strict","input_type":"query","input":"hi"}` {
		t.Errorf("rules sent: %s", c.Body)
	}

	// The provider's cost at its cost_report path.
	mock.Script(mockprovider.Step{Body: []byte(`{"object":"list","data":[],"model":"emb-3","usage":{"prompt_tokens":4,"total_tokens":4,"cost":0.5}}`)})
	if w := post(`{"model":"reported","input":"hi"}`); w.Code != http.StatusOK {
		t.Errorf("reported: %d %s", w.Code, w.Body)
	}

	// A 2xx without usage: estimated from the 33 bytes sent, output 0.
	mock.Script(mockprovider.Step{Body: []byte(`{"object":"list","data":[],"model":"emb-1"}`)})
	if w := post(`{"model":"embed","input":"hello"}`); w.Code != http.StatusOK || w.Body.String() != `{"object":"list","data":[],"model":"emb-1"}` {
		t.Errorf("without usage: %d %s", w.Code, w.Body)
	}

	// Fallback: a 503 of the first target, then the second, built afresh;
	// two records under one request id (RES-1, USE-1).
	mock.Script(mockprovider.Step{Status: http.StatusServiceUnavailable, Body: []byte(`{"error":{"message":"busy"}}`)})
	fb := post(`{"model":"fb","input":"hi"}`)
	if fb.Code != http.StatusOK || fb.Body.String() != mockEmbeddings || fb.Header().Get(targetHeader) != "pay/emb-2" {
		t.Errorf("fallback: %d %v %s", fb.Code, fb.Header(), fb.Body)
	}
	if c := mock.Calls(); len(c) != 7 || string(c[5].Body) != `{"model":"emb-1","input":"hi"}` || string(c[6].Body) != `{"model":"emb-2","input":"hi"}` {
		t.Errorf("fallback sent: %d calls", len(c))
	}

	// A 2xx that is not a JSON object, and one over max_response_bytes:
	// 502, not relayed, and not tried on the next target (phase-2 design
	// section 10, RES-4); each recorded with the estimate of its 30 bytes
	// sent.
	mock.Script(mockprovider.Step{Body: []byte(`[0.1,0.2]`)}, mockprovider.Step{Body: []byte(`{"data":"` + strings.Repeat("A", 1024) + `"}`)})
	for range 2 {
		w := post(`{"model":"fb","input":"hi"}`)
		if e := envelopeOf(t, w); w.Code != http.StatusBadGateway || e.Code != "upstream_error" || e.Message != "the provider's answer is not usable" {
			t.Errorf("unusable: %d %s", w.Code, w.Body)
		}
	}
	if n := len(mock.Calls()); n != 9 {
		t.Errorf("%d provider calls, want 9: an unusable 2xx is not tried on the next target", n)
	}

	rows := f.stored(t, 9)
	p := func(v float64) *float64 { return &v }
	for i, want := range []struct {
		model, target  string
		status         int
		errorType      string
		in             int64
		source         string
		cost           *float64
		costSource     string
		changed        string
		attempt        int
		responseModel  string
		sameAsPrevious bool // the same request id as the row before
	}{
		{"embed", "pay/emb-1", 200, "", 1, "provider", p(1e-6), "price_table", "", 1, "mock", false},
		{"embed", "pay/emb-1", 200, "", 7, "provider", p(7e-6), "price_table", "", 1, "emb-1", false},
		{"rules", "pay/strict", 200, "", 1, "provider", p(1e-6), "price_table", "input_type:forwarded,task:dropped,encoding_format:dropped", 1, "mock", false},
		{"reported", "rep/emb-3", 200, "", 4, "provider", p(0.5), "provider", "", 1, "emb-3", false},
		{"embed", "pay/emb-1", 200, "", 9, "estimated", p(9e-6), "price_table", "", 1, "emb-1", false},
		{"fb", "pay/emb-1", 503, "upstream_error", 0, "provider", p(0), "price_table", "", 1, "", false},
		{"fb", "pay/emb-2", 200, "", 1, "provider", p(1e-6), "price_table", "", 2, "mock", true},
		{"fb", "pay/emb-1", 200, "upstream_error", 8, "estimated", p(8e-6), "price_table", "", 1, "", false},
		{"fb", "pay/emb-1", 200, "upstream_error", 8, "estimated", p(8e-6), "price_table", "", 1, "", false},
	} {
		r := rows[i]
		if r.Endpoint != "embeddings" || r.Key != "agent" || r.Model != want.model || r.Target != want.target || r.Status != want.status ||
			str(r.ErrorType) != want.errorType || r.Stream || str(r.FinishReason) != "" || r.Input != want.in || r.Output != 0 || r.Reasoning != nil ||
			r.CacheRead != 0 || r.CacheWrite != 0 || r.TokensSource != want.source || r.Cost == nil || math.Abs(*r.Cost-*want.cost) > 1e-15 || r.CostSource != want.costSource ||
			r.EffortSent != nil || r.MaxOutputSent != nil || str(r.ParamsChanged) != want.changed || r.Empty || r.TTFT != nil ||
			r.Attempt != want.attempt || str(r.ResponseModel) != want.responseModel || (r.RequestID == rows[max(i-1, 0)].RequestID) != (want.sameAsPrevious || i == 0) {
			t.Errorf("row %d: %+v", i, r)
		}
	}
	var effort, source, alias string
	if err := f.st.Read().QueryRow(`SELECT COALESCE(effort_requested, '') || COALESCE(effort_alias, '') || COALESCE(max_output_requested, '') || COALESCE(max_output_alias, ''),
		effort_source, model FROM usage WHERE rowid = 1`).Scan(&effort, &source, &alias); err != nil || effort != "" || source != "provider_default" || alias != "embed" {
		t.Errorf("effort and cap columns %q, source %q, alias %q, error %v", effort, source, alias, err)
	}
	if fb.Header().Get(domain.RequestIDHeader) != rows[6].RequestID {
		t.Errorf("the fallback's request id %q, records %q", fb.Header().Get(domain.RequestIDHeader), rows[6].RequestID)
	}

	// OPS-8: every request by key and alias and status, refused ones under
	// their status, the input tokens, and no empty answer.
	counted := map[usage.Counter]float64{}
	f.counters.Each(func(k usage.Counter, v float64) { counted[k] = v })
	for k, want := range map[usage.Counter]float64{
		{Name: "requests", Key: "agent", Model: "embed", Label: "200"}:    3,
		{Name: "requests", Key: "agent", Model: "embed", Label: "400"}:    2,
		{Name: "requests", Key: "agent", Model: "fb", Label: "200"}:       1,
		{Name: "requests", Key: "agent", Model: "fb", Label: "502"}:       2,
		{Name: "requests", Key: "agent", Model: "chatonly", Label: "422"}: 1,
		{Name: "tokens", Key: "agent", Model: "embed", Label: "input"}:    17,
		{Name: "tokens", Key: "agent", Model: "rules", Label: "input"}:    1,
	} {
		if counted[k] != want {
			t.Errorf("counter %+v = %v, want %v", k, counted[k], want)
		}
	}
	for k := range counted {
		if k.Name == "empty" || k.Label == "output" {
			t.Errorf("counter %+v on embeddings", k)
		}
	}
}

// envelopeOf is the OpenAI error envelope of w; the zero error for a body
// that is not one.
func envelopeOf(t *testing.T, w *httptest.ResponseRecorder) (e struct{ Code, Param, Message string }) {
	t.Helper()
	var env struct {
		Error struct{ Code, Param, Message string }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return env.Error
}

// TestEmbeddingsSpans is the embeddings span through the data plane (TEL-4,
// TEL-5, annex D): named "embeddings {alias}" with gen_ai.operation.name
// embeddings, server.address and server.port of the URL called, output
// tokens the recorded 0, the effort source provider_default, and no content
// although the key opted in (TEL-8 off chat); a fallback adds one attempt
// child; an unknown key is one error span with stargaite.attempts 0 and no
// record.
func TestEmbeddingsSpans(t *testing.T) {
	f, mock, srv, token := newEmbeddings(t, true)
	if w := call(f.admin, "Bearer "+adminKey, http.MethodPatch, "/admin/v1/keys/agent", []byte(`{"content":"1h"}`)); w.Code != http.StatusOK {
		t.Fatalf("content opt-in: %d %s", w.Code, w.Body)
	}
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.ParseInt(u.Port(), 10, 64)
	ok := f.traffic("/v1/embeddings", token, `{"model":"embed","input":"a prompt that must stay out of every span"}`)
	refused := f.traffic("/v1/embeddings", token, `{"model":"embed","input":"hi","dimensions":8,"task":"x"}`)
	mock.Script(mockprovider.Step{Status: http.StatusServiceUnavailable})
	fb := f.traffic("/v1/embeddings", token, `{"model":"fb","input":"hi"}`)
	if ok.Code != http.StatusOK || refused.Code != http.StatusBadRequest || fb.Code != http.StatusOK {
		t.Fatalf("%d %d %d", ok.Code, refused.Code, fb.Code)
	}
	f.stored(t, 3) // the success and the two attempts of the fallback: the refusal left none

	byTrace := map[string][]otlpSpan{}
	for _, s := range f.exported(t) {
		byTrace[s.TraceID] = append(byTrace[s.TraceID], s)
		for _, a := range s.Attributes {
			if a.Value.String != nil && strings.Contains(*a.Value.String, "a prompt") {
				t.Errorf("span %q carries the input in %s", s.Name, a.Key)
			}
		}
	}
	served := map[string]any{"stargaite.endpoint": "embeddings", "gen_ai.operation.name": "embeddings", "stargaite.key": "agent",
		"server.address": "127.0.0.1", "server.port": port, "gen_ai.usage.output_tokens": int64(0), "gen_ai.usage.input_tokens": int64(1),
		"stargaite.reasoning.effort.source": "provider_default", "stargaite.reasoning.effort.adjusted": false, "stargaite.reasoning.dialect": "none",
		"stargaite.response.empty": false, "http.response.status_code": int64(200)}
	content := []string{"gen_ai.input.messages", "gen_ai.output.messages", "stargaite.content.truncated", "gen_ai.request.max_tokens",
		"stargaite.max_output_tokens.alias", "stargaite.reasoning.effort.alias", "stargaite.reasoning.effort.sent", "gen_ai.request.stream"}

	s := byTrace[ok.Header().Get(traceHeader)]
	if len(s) != 1 || s[0].Name != "embeddings embed" || s[0].Kind != 2 {
		t.Fatalf("success: %+v", s)
	}
	s[0].has(t, map[string]any{"stargaite.model": "embed", "stargaite.target": "pay/emb-1", "stargaite.attempts": int64(1)}, content...)
	s[0].has(t, served)

	s = byTrace[refused.Header().Get(traceHeader)]
	if len(s) != 1 || s[0].Name != "embeddings embed" {
		t.Fatalf("refused: %+v", s)
	}
	s[0].failed(t, "unknown_parameter", 400, "Unknown parameter: 'task'.")
	s[0].has(t, map[string]any{"stargaite.attempts": int64(0), "gen_ai.operation.name": "embeddings", "stargaite.model": "embed"},
		"stargaite.target", "server.address", "gen_ai.usage.input_tokens")

	s = byTrace[fb.Header().Get(traceHeader)]
	if len(s) != 2 || s[0].Name != "embeddings fb" || s[1].Name != "attempt pay/emb-1" || s[1].Kind != 3 || s[1].ParentSpanID != s[0].SpanID {
		t.Fatalf("fallback: %+v", s)
	}
	s[0].has(t, map[string]any{"stargaite.target": "pay/emb-2", "stargaite.attempts": int64(2)})
	s[0].has(t, served)
	s[1].has(t, map[string]any{"stargaite.target": "pay/emb-1", "error.type": "upstream_error", "http.response.status_code": int64(503),
		"gen_ai.operation.name": "embeddings"}, "gen_ai.usage.input_tokens")
}
