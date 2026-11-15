package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/usage"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// traced is yaml with the exporter published toward f's receiver.
func (f *fixture) traced(yaml string) string {
	tel := "telemetry: { otlp_endpoint: " + f.receiverURL + ", allow_private_network: true }"
	if strings.Contains(yaml, "\nsettings: { ") {
		return strings.Replace(yaml, "\nsettings: { ", "\nsettings: { "+tel+", ", 1)
	}
	return strings.Replace(yaml, "version: 1\n", "version: 1\nsettings: { "+tel+" }\n", 1)
}

// otlpSpan is one exported span as a backend reads its OTLP JSON.
type otlpSpan struct {
	TraceID      string `json:"traceId"`
	SpanID       string `json:"spanId"`
	ParentSpanID string `json:"parentSpanId"`
	Flags        uint32 `json:"flags"`
	Name         string `json:"name"`
	Kind         int    `json:"kind"`
	Start        string `json:"startTimeUnixNano"`
	End          string `json:"endTimeUnixNano"`
	Attributes   []struct {
		Key   string `json:"key"`
		Value struct {
			String *string  `json:"stringValue"`
			Bool   *bool    `json:"boolValue"`
			Int    *string  `json:"intValue"`
			Double *float64 `json:"doubleValue"`
			Array  *struct {
				Values []struct{ StringValue string }
			} `json:"arrayValue"`
		} `json:"value"`
	} `json:"attributes"`
	Status *struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"status"`
	attrs map[string]any // string, bool, int64, float64 or []string
}

// exported stops f's exporter, which sends what is queued, and returns the
// spans the receiver got, in the order they were sent, each attribute once.
func (f *fixture) exported(t *testing.T) []otlpSpan {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if n := f.spans.Close(ctx); n != 0 {
		t.Fatalf("%d spans dropped; status %+v", n, f.spans.Status())
	}
	var out []otlpSpan
	for _, c := range f.receiver.Calls() {
		var req struct {
			ResourceSpans []struct{ ScopeSpans []struct{ Spans []otlpSpan } }
		}
		if c.Path != "/v1/traces" || json.Unmarshal(c.Body, &req) != nil {
			t.Fatalf("the receiver got %s %s", c.Path, c.Body)
		}
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					s.attrs = map[string]any{}
					for _, a := range s.Attributes {
						var v any
						switch x := a.Value; {
						case x.String != nil:
							v = *x.String
						case x.Bool != nil:
							v = *x.Bool
						case x.Int != nil:
							v, _ = strconv.ParseInt(*x.Int, 10, 64)
						case x.Double != nil:
							v = *x.Double
						case x.Array != nil:
							var vs []string
							for _, e := range x.Array.Values {
								vs = append(vs, e.StringValue)
							}
							v = vs
						}
						if _, dup := s.attrs[a.Key]; dup || v == nil {
							t.Errorf("span %s: attribute %s twice or without a value", s.Name, a.Key)
						}
						s.attrs[a.Key] = v
					}
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// has checks that s carries each attribute of want with its value, and none
// of the names in absent.
func (s otlpSpan) has(t *testing.T, want map[string]any, absent ...string) {
	t.Helper()
	for k, v := range want {
		if got, ok := s.attrs[k]; !ok || !reflect.DeepEqual(got, v) {
			t.Errorf("span %q: %s = %#v, want %#v", s.Name, k, got, v)
		}
	}
	for _, k := range absent {
		if v, ok := s.attrs[k]; ok {
			t.Errorf("span %q: %s = %#v, want none", s.Name, k, v)
		}
	}
}

// failed checks the error status of s: ERROR with message, error.type code
// and the HTTP status.
func (s otlpSpan) failed(t *testing.T, code string, status int, message string) {
	t.Helper()
	if s.Status == nil || s.Status.Code != 2 || s.Status.Message != message {
		t.Errorf("span %q: status %+v, want ERROR %q", s.Name, s.Status, message)
	}
	s.has(t, map[string]any{"error.type": code, "http.response.status_code": int64(status)})
}

// traffic sends one request to the data plane with "Name", "value" pairs of
// headers, each added once.
func (f *fixture) traffic(path, token, body string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Add(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	f.data.ServeHTTP(w, r)
	return w
}

// spanConfig: pay/up-1 reasons with reasoning_effort and is priced; fb falls
// back from it to pay/up-2; anth/claude is an anthropic subscription with a
// quota; far falls back over two providers whose base_url names no port
// and no name resolves at dial time, https then http (which needs
// allow_private_network), to pay/up-2; and an alias for each refusal after
// authentication: a capability (notools), a budget of 0 (capped), an rpm
// limit of 1 (slow), a provider without credential (offline), an alias the
// key may not use (secret).
const spanConfig = `version: 1
settings: { max_body_bytes: 4096 }
providers:
  pay: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true, gen_ai_provider: openai }
  anth: { protocol: anthropic, base_url: MOCK, api_key_env: MOCK_KEY, billing: subscription, allow_private_network: true, gen_ai_provider: anthropic, quota: [{ window: 5h, requests: 100 }] }
  off: { protocol: openai, base_url: MOCK, api_key_env: MISSING_KEY, billing: none, allow_private_network: true }
  tls: { protocol: openai, base_url: "https://unresolvable.test", billing: none }
  plain: { protocol: openai, base_url: "http://unresolvable.test", billing: none, allow_private_network: true }
targets:
  pay/up-1: { kind: chat, reasoning: { dialect: reasoning_effort, levels: [low, medium, high], can_disable: true }, pricing: { input_per_million: 1, output_per_million: 2, as_of: "2026-11-01" } }
  pay/up-2: { kind: chat, pricing: { input_per_million: 1, output_per_million: 2, as_of: "2026-11-01" } }
  pay/notools: { kind: chat, capabilities: { tools: false }, pricing: { input_per_million: 1, output_per_million: 2, as_of: "2026-11-01" } }
  anth/claude: { kind: chat }
  off/m: { kind: chat }
  tls/m: { kind: chat }
  plain/m: { kind: chat }
models:
  fast: { targets: [pay/up-1], reasoning: { effort: high, mode: default }, max_output_tokens: 100 FAST }
  fb: { targets: [pay/up-1, pay/up-2] }
  claude: { targets: [anth/claude], max_output_tokens: 100 }
  notools: { targets: [pay/notools] }
  capped: { targets: [pay/up-1], budget: { limit_usd: 0, reset_interval: daily } }
  slow: { targets: [pay/up-2], rpm_limit: 1 }
  offline: { targets: [off/m] }
  secret: { targets: [pay/up-2] }
  far: { targets: [tls/m, plain/m, pay/up-2] }
keys:
  agent: { models: [fast, fb, claude, notools, capped, slow, offline, far], budget: { limit_usd: 10, reset_interval: daily } KEY }
`

const (
	clientTrace  = "0af7651916cd43dd8448eb211c80319c"
	clientParent = "b7ad6b7169203331"
	traceparent  = "00-" + clientTrace + "-" + clientParent + "-01"
	prompt       = "a prompt that must stay out of every span"
)

// newSpans is the data plane with spanConfig, its rates in place of FAST
// and KEY, toward one mock provider and with the exporter published.
func newSpans(t *testing.T, fast, key string) (*fixture, *mockprovider.Server, string) {
	t.Helper()
	f := newFixture(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	yaml := strings.NewReplacer("base_url: MOCK", "base_url: "+srv.URL, " FAST", fast, " KEY", key).Replace(spanConfig)
	return f, mock, f.apply(t, f.traced(yaml))["agent"]
}

// TestSpans is AC-25 to AC-27, AC-21 and AC-40 (span) through the data
// plane: each request a sampled key sends gives one request span (SERVER)
// under its trace, its x-stargaite-trace-id, with the attributes of annex D
// v1 from the record and the compatibility attributes Langfuse filters by;
// a fallback adds one CLIENT child for the failed attempt, with its error;
// every refusal after authentication, whatever the step, gives exactly one
// error span with the key, the alias it named and the session; a failed
// authentication and GET /v1/models give none (TEL-5). A valid traceparent
// is joined under a new span id (flags 0x301); a malformed one, two, or any
// one under trust_traceparent false starts a new trace. x-session-id sets the
// session when valid. No prompt, answer, header value other than the session
// or credential reaches the receiver.
func TestSpans(t *testing.T) {
	f, mock, token := newSpans(t, "", "")
	sid := []string{"X-Session-Id", "conv-42"}
	unsent := []string{"X-Custom", planted + "HEADER", "X-Api-Key", planted + "XAPI"}
	body := func(alias, extra string) string {
		return `{"model":"` + alias + `","messages":[{"role":"user","content":"` + prompt + `"}]` + extra + `}`
	}
	checks := map[string]func(t *testing.T, spans []otlpSpan){} // by trace id
	expect := func(name string, w *httptest.ResponseRecorder, status int, check func(t *testing.T, spans []otlpSpan)) {
		t.Helper()
		trace := w.Header().Get(traceHeader)
		if w.Code != status || len(trace) != 32 || checks[trace] != nil {
			t.Fatalf("%s: %d %s, trace %q", name, w.Code, w.Body, trace)
		}
		checks[trace] = func(t *testing.T, spans []otlpSpan) {
			t.Helper()
			t.Run(name, func(t *testing.T) { check(t, spans) })
		}
	}
	key := map[string]any{"stargaite.key": "agent", "langfuse.observation.metadata.stargaite_key": "agent", "stargaite.schema.version": int64(1),
		"stargaite.version": "test-version", "stargaite.config.revision": f.cfg.Snapshot().Revision, "gen_ai.operation.name": "chat"}
	with := func(m map[string]any, more map[string]any) map[string]any {
		out := map[string]any{}
		for _, src := range []map[string]any{m, more} {
			for k, v := range src {
				out[k] = v
			}
		}
		return out
	}
	alias := func(a string) map[string]any {
		return with(key, map[string]any{"stargaite.model": a, "gen_ai.request.model": a, "langfuse.observation.metadata.stargaite_model": a})
	}
	target := func(a, tg string) map[string]any {
		return with(alias(a), map[string]any{"stargaite.target": tg, "langfuse.observation.metadata.stargaite_target": tg})
	}
	session := map[string]any{"session.id": "conv-42", "gen_ai.conversation.id": "conv-42"}
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(mock2URL(f), "http://"))
	server := func() map[string]any {
		p, _ := strconv.ParseInt(port, 10, 64)
		return map[string]any{"server.address": "127.0.0.1", "server.port": p}
	}
	one := func(t *testing.T, spans []otlpSpan) otlpSpan {
		t.Helper()
		if len(spans) != 1 || spans[0].Kind != 2 {
			t.Fatalf("%d spans: %+v", len(spans), spans)
		}
		return spans[0]
	}
	root := func(t *testing.T, s otlpSpan) {
		t.Helper()
		if s.ParentSpanID != "" || s.Flags != 0x101 || len(s.SpanID) != 16 {
			t.Errorf("span %q: parent %q flags %#x id %q", s.Name, s.ParentSpanID, s.Flags, s.SpanID)
		}
	}

	// A synchronous success, joined to the client's trace, with the session.
	w := f.traffic("/v1/chat/completions", token, body("fast", ""), slices.Concat([]string{"Traceparent", traceparent}, sid, unsent)...)
	if w.Header().Get(traceHeader) != clientTrace {
		t.Fatalf("x-stargaite-trace-id %q, want the client's", w.Header().Get(traceHeader))
	}
	expect("success", w, 200, func(t *testing.T, spans []otlpSpan) {
		s := one(t, spans)
		if s.Name != "chat fast" || s.ParentSpanID != clientParent || s.Flags != 0x301 || s.SpanID == clientParent || s.Status != nil {
			t.Errorf("span %+v", s)
		}
		s.has(t, with(with(target("fast", "pay/up-1"), session), with(server(), map[string]any{
			"stargaite.endpoint": "chat", "gen_ai.provider.name": "openai", "stargaite.upstream.model": "up-1", "stargaite.billing": "pay_per_use",
			"gen_ai.response.model": "mock", "stargaite.model_mismatch": true, "stargaite.response.empty": false,
			"gen_ai.response.finish_reasons": []string{"stop"}, "gen_ai.usage.input_tokens": int64(1), "gen_ai.usage.output_tokens": int64(2),
			"stargaite.cost.usd": 5e-6, "gen_ai.usage.cost": 5e-6, "stargaite.cost.source": "price_table",
			"stargaite.reasoning.effort.alias": "high", "stargaite.reasoning.effort.sent": "high", "gen_ai.request.reasoning.level": "high",
			"stargaite.reasoning.effort.source": "alias_default", "stargaite.reasoning.effort.adjusted": false, "stargaite.reasoning.dialect": "reasoning_effort",
			"stargaite.max_output_tokens.alias": int64(100), "stargaite.max_output_tokens.sent": int64(100), "gen_ai.request.max_tokens": int64(100),
			"stargaite.params.changed": "max_tokens:added,reasoning_effort:added", "stargaite.attempts": int64(1), "http.response.status_code": int64(200),
		})), "error.type", "gen_ai.request.stream", "gen_ai.response.time_to_first_chunk", "stargaite.usage.estimated", "gen_ai.usage.cache_read.input_tokens",
			"stargaite.reasoning.effort.requested", "stargaite.max_output_tokens.requested")
	})

	// A stream, the client's traceparent in uppercase: a new trace.
	w = f.traffic("/v1/chat/completions", token, body("fast", `,"stream":true`), "Traceparent", strings.ToUpper(traceparent))
	expect("stream", w, 200, func(t *testing.T, spans []otlpSpan) {
		s := one(t, spans)
		root(t, s)
		s.has(t, with(target("fast", "pay/up-1"), map[string]any{"gen_ai.request.stream": true, "gen_ai.response.time_to_first_chunk": 0.0,
			"gen_ai.usage.output_tokens": int64(2), "stargaite.attempts": int64(1)}), "session.id", "gen_ai.conversation.id")
	})

	// /v1/messages toward the anthropic subscription: no cost attribute.
	w = f.traffic("/v1/messages", "", `{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":"`+prompt+`"}]}`,
		slices.Concat([]string{"X-Api-Key", token, "Traceparent", "ff-" + clientTrace + "-" + clientParent + "-01"}, sid)...)
	expect("messages", w, 200, func(t *testing.T, spans []otlpSpan) {
		s := one(t, spans)
		root(t, s)
		s.has(t, with(with(target("claude", "anth/claude"), session), map[string]any{"stargaite.endpoint": "messages", "gen_ai.provider.name": "anthropic",
			"stargaite.billing": "subscription", "stargaite.cost.source": "none", "gen_ai.response.finish_reasons": []string{"end_turn"},
			"stargaite.max_output_tokens.sent": int64(10), "stargaite.reasoning.dialect": "none"}), "stargaite.cost.usd", "gen_ai.usage.cost")
	})

	// A fallback after a 500: the failed attempt is a child span with its error
	// and no usage; the request span carries the serving attempt's (TEL-10).
	mock.Script(mockprovider.Step{Status: 500})
	w = f.traffic("/v1/chat/completions", token, body("fb", ""), sid...)
	expect("fallback", w, 200, func(t *testing.T, spans []otlpSpan) {
		if len(spans) != 2 || spans[0].Kind != 2 || spans[1].Kind != 3 {
			t.Fatalf("spans %+v", spans)
		}
		req, a := spans[0], spans[1]
		root(t, req)
		if a.Name != "attempt pay/up-1" || a.ParentSpanID != req.SpanID || a.SpanID == req.SpanID || a.TraceID != req.TraceID || a.Flags != 0x101 {
			t.Errorf("attempt span %+v under %+v", a, req)
		}
		req.has(t, with(target("fb", "pay/up-2"), map[string]any{"stargaite.attempts": int64(2), "gen_ai.usage.input_tokens": int64(1),
			"stargaite.cost.usd": 5e-6, "http.response.status_code": int64(200)}), "error.type")
		a.failed(t, "upstream_error", 500, "the provider answered 500")
		a.has(t, with(with(target("fb", "pay/up-1"), session), map[string]any{"stargaite.upstream.model": "up-1", "stargaite.reasoning.dialect": "reasoning_effort"}),
			"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "stargaite.cost.usd", "gen_ai.usage.cost", "stargaite.attempts")
	})

	// Base URLs without a port: server.port is the scheme's, 443 or 80.
	w = f.traffic("/v1/chat/completions", token, body("far", ""))
	expect("default ports", w, 200, func(t *testing.T, spans []otlpSpan) {
		if len(spans) != 3 {
			t.Fatalf("spans %+v", spans)
		}
		spans[1].has(t, map[string]any{"stargaite.target": "tls/m", "server.address": "unresolvable.test", "server.port": int64(443)})
		spans[2].has(t, map[string]any{"stargaite.target": "plain/m", "server.address": "unresolvable.test", "server.port": int64(80)})
	})

	// A provider's 400 whose text quotes the prompt: the client gets the text,
	// the span the status alone (TEL-8).
	mock.Script(mockprovider.Step{Status: 400, Body: []byte(`{"error":{"message":"input_value='` + prompt + `'"}}`)})
	w = f.traffic("/v1/chat/completions", token, body("fast", ""))
	if !strings.Contains(w.Body.String(), prompt) {
		t.Errorf("echoing 400: %d %s", w.Code, w.Body)
	}
	expect("echoing 400", w, 400, func(t *testing.T, spans []otlpSpan) {
		one(t, spans).failed(t, "upstream_error", 400, "the provider answered 400")
	})

	// Each refusal after authentication: one error span, with the key, the
	// alias it named and the session (AC-26).
	for _, tc := range []struct {
		name, body string
		status     int
		code       string
		alias      string
	}{
		{"malformed body", `{"model":`, 400, "invalid_request", ""},
		{"unknown parameter", body("fast", `,"enable_search":true`), 400, "unknown_parameter", "fast"},
		{"too large", body("fast", `,"user":"`+strings.Repeat("x", 5000)+`"`), 413, "request_too_large", ""},
		{"unknown alias", body("nope", ""), 404, "model_not_found", "nope"},
		{"alias not allowed", body("secret", ""), 403, "model_not_allowed", "secret"},
		{"capability", body("notools", `,"tools":[{"type":"function","function":{"name":"f"}}]`), 422, "capability_mismatch", "notools"},
		{"budget", body("capped", ""), 402, "budget_exceeded", "capped"},
		{"rate limit", body("slow", ""), 429, "rate_limit_exceeded", "slow"},
		{"no target", body("offline", ""), 503, "no_target_available", "offline"},
	} {
		if tc.name == "rate limit" {
			expect("rate limit admitted", f.traffic("/v1/chat/completions", token, tc.body, sid...), 200, func(t *testing.T, spans []otlpSpan) { one(t, spans) })
		}
		w := f.traffic("/v1/chat/completions", token, tc.body, sid...)
		var env struct{ Error domain.Error }
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Error.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body)
		}
		expect(tc.name, w, tc.status, func(t *testing.T, spans []otlpSpan) {
			s := one(t, spans)
			root(t, s)
			s.failed(t, tc.code, tc.status, env.Error.Message)
			want, name := key, "chat"
			if tc.alias != "" {
				want, name = alias(tc.alias), "chat "+tc.alias
			}
			if s.Name != name {
				t.Errorf("name %q, want %q", s.Name, name)
			}
			s.has(t, with(with(want, session), map[string]any{"stargaite.attempts": int64(0), "stargaite.endpoint": "chat"}),
				"stargaite.target", "langfuse.observation.metadata.stargaite_target", "gen_ai.provider.name", "server.address", "gen_ai.usage.input_tokens")
			if tc.alias == "" {
				s.has(t, nil, "stargaite.model", "gen_ai.request.model", "langfuse.observation.metadata.stargaite_model")
			}
		})
	}

	// Sessions that are not used, and traceparents that start a new trace.
	for _, tc := range []struct {
		name   string
		header []string
	}{
		{"a session of 129 bytes", []string{"X-Session-Id", strings.Repeat("s", 129)}},
		{"a session with a control byte", []string{"X-Session-Id", "conv\x01"}},
		{"a session with DEL", []string{"X-Session-Id", "conv\x7f"}},
		{"two sessions", []string{"X-Session-Id", "a", "X-Session-Id", "b"}},
		{"two traceparents", []string{"Traceparent", traceparent, "Traceparent", traceparent}},
		{"a traceparent of version 00 with more", []string{"Traceparent", traceparent + "-x"}},
		{"an all-zero parent", []string{"Traceparent", "00-" + clientTrace + "-0000000000000000-01"}},
	} {
		w := f.traffic("/v1/chat/completions", token, body("fast", ""), tc.header...)
		if w.Header().Get(traceHeader) == clientTrace {
			t.Errorf("%s: the client's trace was joined", tc.name)
		}
		expect(tc.name, w, 200, func(t *testing.T, spans []otlpSpan) {
			s := one(t, spans)
			root(t, s)
			s.has(t, nil, "session.id", "gen_ai.conversation.id")
		})
	}

	// No span: a failed authentication, GET /v1/models.
	if w := f.traffic("/v1/chat/completions", "sk-stargaite-wrong", body("fast", ""), "Traceparent", traceparent); w.Code != 401 || w.Header().Get(traceHeader) != "" {
		t.Errorf("401: %d %v", w.Code, w.Header())
	}
	if w := call(f.data, "Bearer "+token, http.MethodGet, "/v1/models", nil); w.Code != 200 || len(w.Header().Get(traceHeader)) != 32 {
		t.Errorf("/v1/models: %d %v", w.Code, w.Header())
	}

	// trust_traceparent false: a valid traceparent starts a new trace.
	yaml := strings.NewReplacer("base_url: MOCK", "base_url: "+mock2URL(f), " FAST", "", " KEY", "").Replace(spanConfig)
	f.apply(t, strings.Replace(f.traced(yaml), "otlp_endpoint: ", "trust_traceparent: false, otlp_endpoint: ", 1))
	w = f.traffic("/v1/chat/completions", token, body("fast", ""), "Traceparent", traceparent)
	if w.Header().Get(traceHeader) == clientTrace {
		t.Error("trust_traceparent false: the client's trace was joined")
	}
	expect("trust off", w, 200, func(t *testing.T, spans []otlpSpan) {
		s := one(t, spans)
		root(t, s)
		s.has(t, map[string]any{"stargaite.config.revision": f.cfg.Snapshot().Revision})
	})

	spans := f.exported(t)
	byTrace := map[string][]otlpSpan{}
	for _, s := range spans {
		if s.Start == "" || s.End < s.Start && len(s.End) <= len(s.Start) {
			t.Errorf("span %q: start %s end %s", s.Name, s.Start, s.End)
		}
		byTrace[s.TraceID] = append(byTrace[s.TraceID], s)
	}
	if len(byTrace) != len(checks) {
		t.Errorf("%d traces exported for %d requests", len(byTrace), len(checks))
	}
	for trace, check := range checks {
		check(t, byTrace[trace])
	}
	for _, c := range f.receiver.Calls() {
		for _, leak := range []string{planted, prompt, "mock reply", token, "client-header"} {
			if strings.Contains(string(c.Body), leak) {
				t.Errorf("an export carries %q", leak)
			}
		}
	}
}

// mock2URL is the base URL of the mock the published pay provider names.
func mock2URL(f *fixture) string {
	return f.cfg.Snapshot().Providers["pay"].Spec.BaseURL
}

// ledger is what accounting holds after a script: the records without their
// random ids, usage_daily, the budget figures, the quota windows and the
// counters.
func ledger(t *testing.T, f *fixture, n int) string {
	t.Helper()
	var b strings.Builder
	for _, r := range f.stored(t, n) {
		r.ID, r.RequestID, r.TraceID = "", "", ""
		b.WriteString("record " + toJSON(r) + "\n")
	}
	for _, row := range rows(t, f, `SELECT day || ' ' || key_name || ' ' || model || ' ' || target || ' ' || billing || ' ' || requests || ' ' ||
		errors || ' ' || input_tokens || ' ' || output_tokens || ' ' || printf('%.9f', cost_usd) FROM usage_daily ORDER BY 1`) {
		b.WriteString("daily " + row + "\n")
	}
	for _, scope := range []string{"global", "key:agent", "model:fast", "model:fb", "model:capped"} {
		for _, r := range []string{domain.ResetDaily, domain.ResetMonthly} {
			fmt.Fprintf(&b, "budget %s %s %.9f\n", scope, r, f.limits.Spent(admission.Budget{Scope: scope, Spec: domain.Budget{ResetInterval: r}}))
		}
	}
	b.WriteString("windows " + toJSON(f.limits.Windows()) + "\n")
	f.counters.Each(func(c usage.Counter, v float64) { fmt.Fprintf(&b, "counter %+v %v\n", c, v) })
	return b.String()
}

// TestSamplingInvariant is AC-28: one script, traced fully, at a key's rate
// of 0, 0.5 and 1 and at an alias's rate of 0 and 0.5, leaves the same
// records, usage_daily, budgets, quota windows and counters; only the spans
// differ: none at a key's 0, none for the alias at its 0. The draw is the
// request's own, never the trace id a client may set: at 0.5, requests under
// one traceparent are traced some, not all.
func TestSamplingInvariant(t *testing.T) {
	script := func(f *fixture, mock *mockprovider.Server, token string) int {
		chat := func(alias, extra string) { f.traffic("/v1/chat/completions", token, chatBody(alias, extra)) }
		for range 4 {
			chat("fast", "")
			chat("fast", `,"stream":true`)
			mock.Script(mockprovider.Step{Status: 500})
			chat("fb", "")
			f.traffic("/v1/messages", "", messagesBody("claude", ""), "X-Api-Key", token)
			chat("capped", "")
			chat("slow", "")
			chat("offline", "")
			chat("nope", "")
			chat("notools", `,"tools":[{"type":"function","function":{"name":"f"}}]`)
		}
		return 4 * 9
	}
	const calls = 4*5 + 1 // per round fast twice, fb twice and claude; slow once, its rpm limit is 1
	full, mock, token := newSpans(t, "", "")
	requests := script(full, mock, token)
	want := ledger(t, full, calls)
	if spans := full.exported(t); len(spans) != requests+4 {
		t.Errorf("full tracing: %d spans for %d requests and 4 failed attempts", len(spans), requests)
	}
	for _, tc := range []struct{ name, fast, key string }{
		{"key 0", "", ", trace_sample_rate: 0"},
		{"key 0.5", "", ", trace_sample_rate: 0.5"},
		{"key 1", "", ", trace_sample_rate: 1"},
		{"alias 0", ", trace_sample_rate: 0", ""},
		{"alias 0.5", ", trace_sample_rate: 0.5", ", trace_sample_rate: 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, mock, token := newSpans(t, tc.fast, tc.key)
			script(f, mock, token)
			if got := ledger(t, f, calls); got != want {
				t.Errorf("accounting differs from full tracing:\n%s\nfull:\n%s", got, want)
			}
			spans := f.exported(t)
			fast := 0
			for _, s := range spans {
				if s.attrs["stargaite.model"] == "fast" {
					fast++
				}
			}
			switch tc.name {
			case "key 0":
				if len(spans) != 0 {
					t.Errorf("%d spans", len(spans))
				}
			case "key 1":
				if len(spans) != requests+4 {
					t.Errorf("%d spans", len(spans))
				}
			case "alias 0":
				if fast != 0 || len(spans) != requests+4-8 {
					t.Errorf("%d spans, %d of fast", len(spans), fast)
				}
			default:
				if len(spans) > requests+4 {
					t.Errorf("%d spans", len(spans))
				}
			}
		})
	}
	t.Run("one traceparent at 0.5", func(t *testing.T) {
		f, _, token := newSpans(t, "", ", trace_sample_rate: 0.5")
		for range 64 {
			f.traffic("/v1/chat/completions", token, chatBody("fast", ""), "Traceparent", traceparent)
		}
		if n := len(f.exported(t)); n == 0 || n == 64 { // all on one side by chance: 2^-63
			t.Errorf("%d of 64 requests traced", n)
		}
	})
}

// TestExporterDown is AC-31 with traffic: with the receiver down, hanging
// or answering 500, responses sent while a batch is on its way take no
// longer than an upper bound of the baseline without an exporter, and the
// spans are counted in telemetry status, dropped or rejected by status, with
// the last error. The first 200 requests queue a batch (the telemetry
// package's batchSpans), which starts a send at once.
func TestExporterDown(t *testing.T) {
	const batch, measured = 200, 10
	run := func(t *testing.T, f *fixture, token string, n int) time.Duration {
		var slowest time.Duration
		for i := range n {
			start := time.Now()
			extra := ""
			if i%2 == 1 {
				extra = `,"stream":true`
			}
			if w := f.traffic("/v1/chat/completions", token, chatBody("fast", extra)); w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			slowest = max(slowest, time.Since(start))
		}
		return slowest
	}
	f := newFixture(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	token := f.apply(t, strings.NewReplacer("base_url: MOCK", "base_url: "+srv.URL, " FAST", "", " KEY", "").Replace(spanConfig))["agent"]
	baseline := run(t, f, token, measured)
	bound := 5*baseline + 500*time.Millisecond

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	down := "http://" + closed.Addr().String()
	_ = closed.Close()
	for _, tc := range []struct {
		name    string
		step    *mockprovider.Step
		url     string
		dropped bool
		last    string
	}{
		{"receiver down", nil, down, true, ""},
		{"receiver hanging", &mockprovider.Step{Hang: true}, "", true, ""},
		// The first 500 comes late, so the measured requests run while a send waits.
		{"receiver answering 500", &mockprovider.Step{Status: 500, Body: []byte("backend down"), Delay: time.Second}, "", false, "500 Internal Server Error: backend down"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.step != nil {
				for i := range 10 {
					step := *tc.step
					if i > 0 {
						step.Delay = 0
					}
					f.receiver.Script(step)
				}
			}
			yaml := f.traced(strings.NewReplacer("base_url: MOCK", "base_url: "+srv.URL, " FAST", "", " KEY", "").Replace(spanConfig))
			if tc.url != "" {
				yaml = strings.Replace(yaml, f.receiverURL, tc.url, 1)
			}
			token := f.apply(t, yaml)["agent"]
			run(t, f, token, batch)
			if tc.step != nil { // the batch reached the receiver, which hangs or answers 500
				deadline := time.Now().Add(5 * time.Second)
				for len(f.receiver.Calls()) == 0 && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				if len(f.receiver.Calls()) == 0 {
					t.Fatal("no batch was sent during the traffic")
				}
			}
			if slowest := run(t, f, token, measured); slowest > bound {
				t.Errorf("slowest response %v with the exporter, %v without (bound %v)", slowest, baseline, bound)
			}
			if tc.step != nil && tc.step.Delay > 0 { // let the late 500 end, so the stop does not cut it
				deadline := time.Now().Add(5 * time.Second)
				for f.receiver.Calls()[0].Ended.IsZero() && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
			}
			// The stop cuts a send in progress short: its spans are dropped.
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			f.spans.Close(ctx)
			var status struct {
				Exporter struct {
					Sent             int64            `json:"sent"`
					RejectedByStatus map[string]int64 `json:"rejected_by_status"`
					Dropped          int64            `json:"dropped"`
					LastError        string           `json:"last_error"`
				}
			}
			w := call(f.admin, "Bearer "+adminKey, http.MethodGet, "/admin/v1/telemetry", nil)
			if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
				t.Fatalf("telemetry status: %s", w.Body)
			}
			e := status.Exporter
			counted := e.RejectedByStatus["500"]
			if tc.dropped {
				counted = e.Dropped
			}
			if e.Sent != 0 || counted != batch+measured || !strings.Contains(e.LastError, tc.last) {
				t.Errorf("telemetry status %s", w.Body)
			}
		})
	}
}
