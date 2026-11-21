package api

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/routing"
	"github.com/jorlaru/stargaite/internal/transport"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// resilienceConfig: the failing targets (bad and badanth on the BAD mock,
// refused on a closed port, rebound on a name the dial finds on loopback,
// which the SEC-4 guard refuses), each alone and followed by good/m, an
// anthropic target on the GOOD mock, so a fallback from an openai target is
// translated again; off has no credential; bad/m2 is a second target on
// provider bad; goodoai/m is an openai target on the GOOD mock; paid/m, behind
// an alias whose budget is spent, and sub/m, whose quota allows one request,
// are passed over dynamically. Timers are milliseconds.
const resilienceConfig = `version: 1
settings: { timeouts: { connect_ms: 200, total_ms: 300, first_byte_ms: 300, stream_idle_ms: 300 }, max_response_bytes: 4096 }
providers:
  bad: { protocol: openai, base_url: BAD, api_key_env: BAD_KEY, billing: none, allow_private_network: true }
  badanth: { protocol: anthropic, base_url: BAD, api_key_env: BAD_KEY, billing: none, allow_private_network: true }
  refused: { protocol: openai, base_url: REFUSED, billing: none, allow_private_network: true }
  rebound: { protocol: openai, base_url: REBOUND, billing: none }
  good: { protocol: anthropic, base_url: GOOD, api_key_env: GOOD_KEY, billing: none, allow_private_network: true }
  off: { protocol: openai, base_url: GOOD, api_key_env: MISSING_KEY, billing: none, allow_private_network: true }
  goodoai: { protocol: openai, base_url: GOOD, api_key_env: GOOD_KEY, billing: none, allow_private_network: true }
  paid: { protocol: openai, base_url: GOOD, api_key_env: GOOD_KEY, billing: pay_per_use, allow_private_network: true }
  sub: { protocol: openai, base_url: GOOD, api_key_env: GOOD_KEY, billing: subscription, allow_private_network: true, quota: [{ window: 1h, requests: 1 }] }
targets:
  bad/m: { kind: chat, reasoning: { dialect: reasoning_effort, levels: [low, medium, high], can_disable: true } }
  bad/m2: { kind: chat }
  badanth/m: { kind: chat }
  refused/m: { kind: chat }
  rebound/m: { kind: chat }
  good/m: { kind: chat, reasoning: { dialect: adaptive_effort, levels: [low, medium, high], can_disable: true } }
  good/notools: { kind: chat, capabilities: { tools: false } }
  off/m: { kind: chat }
  goodoai/m: { kind: chat }
  paid/m: { kind: chat, pricing: { input_per_million: 1, output_per_million: 1, as_of: 2026-11-01 } }
  sub/m: { kind: chat }
models:
  bad-one: { targets: [bad/m], reasoning: { effort: high, mode: default }, max_output_tokens: 500 }
  bad-two: { targets: [bad/m, good/m], reasoning: { effort: high, mode: default }, max_output_tokens: 500 }
  badanth-one: { targets: [badanth/m], max_output_tokens: 500 }
  badanth-two: { targets: [badanth/m, good/m], reasoning: { effort: high, mode: default }, max_output_tokens: 500 }
  refused-one: { targets: [refused/m] }
  refused-two: { targets: [refused/m, good/m], reasoning: { effort: high, mode: default }, max_output_tokens: 500 }
  rebound-one: { targets: [rebound/m] }
  rebound-two: { targets: [rebound/m, good/m], reasoning: { effort: high, mode: default }, max_output_tokens: 500 }
  bad-off: { targets: [bad/m, off/m] }
  bad-notools: { targets: [bad/m, good/notools], max_output_tokens: 500 }
  bad-m2: { targets: [bad/m2] }
  notools-bad: { targets: [good/notools, bad/m], max_output_tokens: 500 }
  badanth-oai: { targets: [badanth/m, goodoai/m], max_output_tokens: 500 }
  bad-paid: { targets: [bad/m, paid/m], budget: { limit_usd: 0, reset_interval: never } }
  bad-sub: { targets: [bad/m, sub/m] }
keys:
  agent: { models: ["*"] }
`

// newResilience is the data plane on a real listener with the two mocks
// of resilienceConfig behind it, and the configuration applied; s.mock is
// BAD.
func newResilience(t *testing.T) (s *streamFixture, good *mockprovider.Server, yaml string) {
	t.Helper()
	f := newFixture(t)
	bad, good := mockprovider.New(), mockprovider.New()
	badSrv, goodSrv := httptest.NewServer(bad), httptest.NewServer(good)
	t.Cleanup(badSrv.Close)
	t.Cleanup(goodSrv.Close)
	// Nothing listens on port 1, and no test running alongside is ever handed
	// it, as it could be a port closed a moment ago.
	const refused = "127.0.0.1:1"
	yaml = strings.NewReplacer("base_url: BAD,", "base_url: "+badSrv.URL+",", "base_url: GOOD,", "base_url: "+goodSrv.URL+",",
		"base_url: REFUSED,", "base_url: http://"+refused+",",
		"base_url: REBOUND,", "base_url: https://rebound.test:"+badSrv.URL[strings.LastIndex(badSrv.URL, ":")+1:]+",").Replace(resilienceConfig)
	yaml = f.traced(yaml)
	tokens := f.apply(t, yaml)
	data := httptest.NewServer(f.data)
	t.Cleanup(data.Close)
	return &streamFixture{fixture: f, mock: bad, url: data.URL, token: tokens["agent"]}, good, yaml
}

// lines counts the attempt and request lines of the log.
func lines(log string) (attempts, requests int) {
	for line := range strings.SplitSeq(strings.TrimSpace(log), "\n") {
		attempts += strings.Count(line, `"msg":"attempt"`)
		requests += strings.Count(line, `"msg":"request"`)
	}
	return attempts, requests
}

// TestFaultMatrix is AC-11 (fallback) and AC-21 for every fault of design
// section 12 but cancellation and shutdown (TestClientGone and TestShutdown,
// whose verdict is never eligible), synchronous and streamed,
// with one target and with a fallback. Alone, the target's failure is the
// section 15 envelope, or once a byte was written the terminal event. With
// a fallback, a failure eligible under RES-1 before the commit moves to
// good/m, which gets its own Build (a Messages body with the effort
// translated for it, where bad/m got reasoning_effort) and whose attempt the
// PAR-9 headers describe; any other failure is answered as alone and good/m
// is never called. Each call has one attempt line, each request one request
// line. The clock moves past the RES-2 window before each case, so no
// circuit opens here (TestBreakerBehaviour).
func TestFaultMatrix(t *testing.T) {
	s, good, _ := newResilience(t)
	first := "data: " + chunk("a") + "\n\n"
	big := `{"x":"` + strings.Repeat("x", 5000) + `"}`
	anthError := func(typ string) [][]byte {
		return [][]byte{[]byte(`event: error` + "\n" + `data: {"type":"error","error":{"type":"` + typ + `","message":"boom"}}` + "\n\n")}
	}
	retry := http.Header{"Retry-After": {"3"}}
	type fault struct {
		name      string
		stream    bool
		first     string // the provider of the failing target
		step      *mockprovider.Step
		status    int
		code      string
		eligible  bool
		committed bool // the client got a byte: a terminal event, never a fallback
	}
	var faults []fault
	for _, stream := range []bool{false, true} {
		for _, f := range []fault{
			{name: "refused dial", first: "refused", status: 502, code: "upstream_error", eligible: true},
			{name: "private dial", first: "rebound", status: 502, code: "upstream_error", eligible: true},
			{name: "429", first: "bad", step: &mockprovider.Step{Status: 429, Header: retry}, status: 429, code: "upstream_error", eligible: true},
			{name: "500", first: "bad", step: &mockprovider.Step{Status: 500}, status: 502, code: "upstream_error", eligible: true},
			{name: "529", first: "bad", step: &mockprovider.Step{Status: 529}, status: 502, code: "upstream_error", eligible: true},
			{name: "400", first: "bad", step: &mockprovider.Step{Status: 400, Body: []byte(`{"error":{"message":"no"}}`)}, status: 400, code: "upstream_error"},
			{name: "401", first: "bad", step: &mockprovider.Step{Status: 401}, status: 502, code: "upstream_auth_error", eligible: true},
			{name: "402", first: "bad", step: &mockprovider.Step{Status: 402}, status: 502, code: "upstream_error", eligible: true},
			{name: "403", first: "bad", step: &mockprovider.Step{Status: 403}, status: 502, code: "upstream_auth_error", eligible: true},
			{name: "404", first: "bad", step: &mockprovider.Step{Status: 404, Body: []byte(`{"error":{"message":"no such model"}}`)}, status: 502, code: "upstream_error", eligible: true},
			{name: "302", first: "bad", step: &mockprovider.Step{Status: 302, Header: http.Header{"Location": {"http://127.0.0.1:1/"}}}, status: 502, code: "upstream_error"},
		} {
			f.stream = stream
			faults = append(faults, f)
		}
	}
	faults = append(faults,
		fault{name: "delay past the total", first: "bad", step: &mockprovider.Step{Delay: 600 * time.Millisecond}, status: 504, code: "upstream_timeout", eligible: true},
		fault{name: "hang in the body", first: "bad", step: &mockprovider.Step{Body: []byte(`{"choices":[`), Hang: true}, status: 504, code: "upstream_timeout", eligible: true},
		fault{name: "cut in the body", first: "bad", step: &mockprovider.Step{Body: []byte(`{"choices":[`), Cut: true}, status: 502, code: "upstream_error", eligible: true},
		fault{name: "oversized body", first: "bad", step: &mockprovider.Step{Body: []byte(big)}, status: 502, code: "upstream_error"},
		fault{name: "a 200 with code 502", first: "bad", step: &mockprovider.Step{Body: []byte(`{"error":{"code":502,"message":"no provider"}}`)},
			status: 502, code: "upstream_error", eligible: true},
		fault{name: "a 200 with code 400", first: "bad", step: &mockprovider.Step{Body: []byte(`{"error":{"code":400,"message":"bad input"}}`)},
			status: 400, code: "upstream_error"},
		fault{name: "a 200 with an error and no code", first: "bad", step: &mockprovider.Step{Body: []byte(`{"error":{"message":"something"}}`)},
			status: 502, code: "upstream_error"},

		fault{name: "delay past the first byte", stream: true, first: "bad", step: &mockprovider.Step{Chunks: sse(chunk("a"), "[DONE]"), Delay: 600 * time.Millisecond},
			status: 504, code: "upstream_timeout", eligible: true},
		fault{name: "hang before the first event", stream: true, first: "bad", step: &mockprovider.Step{Chunks: [][]byte{[]byte(": c\n\n")}, Hang: true},
			status: 504, code: "upstream_timeout", eligible: true},
		fault{name: "gap past idle before the first event", stream: true, first: "bad", step: &mockprovider.Step{Chunks: [][]byte{[]byte(": c\n\n"), []byte(first)}, Gap: 600 * time.Millisecond},
			status: 504, code: "upstream_timeout", eligible: true},
		fault{name: "cut before the commit", stream: true, first: "bad", step: &mockprovider.Step{Chunks: [][]byte{[]byte(": c\n\n")}, Cut: true},
			status: 502, code: "upstream_error", eligible: true},
		fault{name: "an anthropic overloaded_error event first", stream: true, first: "badanth", step: &mockprovider.Step{Chunks: anthError("overloaded_error")},
			status: 502, code: "upstream_error", eligible: true},
		fault{name: "an anthropic api_error event first", stream: true, first: "badanth", step: &mockprovider.Step{Chunks: anthError("api_error")},
			status: 502, code: "upstream_error", eligible: true},
		fault{name: "an anthropic rate_limit_error event first", stream: true, first: "badanth", step: &mockprovider.Step{Chunks: anthError("rate_limit_error")},
			status: 429, code: "upstream_error", eligible: true},
		fault{name: "an anthropic invalid_request_error event first", stream: true, first: "badanth", step: &mockprovider.Step{Chunks: anthError("invalid_request_error")},
			status: 400, code: "upstream_error"},
		fault{name: "an openai error chunk first", stream: true, first: "bad", step: &mockprovider.Step{Chunks: sse(`{"error":{"message":"boom","type":"server_error"}}`)},
			status: 502, code: "upstream_error"},
		fault{name: "an openai error chunk with code 502 first", stream: true, first: "bad", step: &mockprovider.Step{Chunks: sse(`{"error":{"message":"boom","code":502}}`)},
			status: 502, code: "upstream_error", eligible: true},
		fault{name: "an oversized event first", stream: true, first: "bad", step: &mockprovider.Step{Chunks: sse(big)}, status: 502, code: "upstream_error"},
		fault{name: "cut after the commit", stream: true, first: "bad", step: &mockprovider.Step{Chunks: [][]byte{[]byte(first)}, Cut: true},
			status: 200, code: "upstream_error", eligible: true, committed: true},
		fault{name: "gap past idle after the commit", stream: true, first: "bad", step: &mockprovider.Step{Chunks: [][]byte{[]byte(first), []byte("data: [DONE]\n\n")}, Gap: 600 * time.Millisecond},
			status: 200, code: "upstream_timeout", eligible: true, committed: true},
		fault{name: "an error chunk after the commit", stream: true, first: "bad", step: &mockprovider.Step{Chunks: sse(chunk("a"), `{"error":{"message":"boom"}}`)},
			status: 200, code: "upstream_error", committed: true},
	)

	type spanWant struct {
		fellBack    bool
		first, code string
		status      int
	}
	wants := map[string]spanWant{} // by trace id
	wantAttempts, requests := 0, 0
	for _, f := range faults {
		for _, alias := range []string{f.first + "-one", f.first + "-two"} {
			mode := map[bool]string{false: "sync", true: "stream"}[f.stream]
			t.Run(mode+"/"+f.name+"/"+alias, func(t *testing.T) {
				s.pastWindow()
				if f.step != nil {
					s.mock.Script(*f.step)
				}
				badBefore, goodBefore := len(s.mock.Calls()), len(good.Calls())
				extra := ""
				if f.stream {
					extra = `,"stream":true`
				}
				resp, got := s.post(t, chatBody(alias, extra))
				requests++
				fellBack := strings.HasSuffix(alias, "-two") && f.eligible && !f.committed
				h := resp.Header
				// The request line's status: once committed, the terminal event's.
				wants[h.Get(traceHeader)] = spanWant{fellBack, f.first + "/m", f.code, map[bool]int{false: resp.StatusCode, true: domain.NewError(f.code, "").Status}[f.committed]}
				badCalls, goodCalls := len(s.mock.Calls())-badBefore, len(good.Calls())-goodBefore
				if want := map[bool]int{true: 1}[f.step != nil]; badCalls != want {
					t.Errorf("%d calls to the failing provider", badCalls)
				}
				wantAttempts++
				switch {
				case fellBack:
					wantAttempts++
					c := good.Calls()[len(good.Calls())-1]
					if resp.StatusCode != 200 || goodCalls != 1 || c.Path != "/v1/messages" ||
						!strings.Contains(string(c.Body), `"max_tokens":500,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}`) ||
						strings.Contains(string(c.Body), "reasoning_effort") || !strings.Contains(got, "mock reply") {
						t.Fatalf("fallback: %d %q; good/m got %d calls, the last %s", resp.StatusCode, got, goodCalls, c.Body)
					}
					if h.Get(targetHeader) != "good/m" || h.Get(effortHeader) != "high" || h.Get(changedHeader) != "protocol:translated,max_tokens:added,output_config.effort:added" ||
						h.Get("Retry-After") != "" {
						t.Errorf("headers of the fallback %v", h)
					}
					if f.stream != strings.HasSuffix(got, "data: [DONE]\n\n") {
						t.Errorf("body %q", got)
					}
					if f.first == "bad" && !strings.Contains(string(s.mock.Calls()[len(s.mock.Calls())-1].Body), `"reasoning_effort":"high"`) {
						t.Errorf("bad/m got %s", s.mock.Calls()[len(s.mock.Calls())-1].Body)
					}
				case f.committed:
					if resp.StatusCode != 200 || goodCalls != 0 || !strings.HasPrefix(got, first) || strings.Contains(got, "[DONE]") ||
						!strings.Contains(got, `"error":{`) || !strings.Contains(got, `"code":"`+f.code+`"`) {
						t.Errorf("committed: %d %q, %d calls to good/m", resp.StatusCode, got, goodCalls)
					}
				default:
					var env struct{ Error domain.Error }
					if err := json.Unmarshal([]byte(got), &env); err != nil || resp.StatusCode != f.status || env.Error.Code != f.code || goodCalls != 0 ||
						h.Get(targetHeader) != f.first+"/m" {
						t.Errorf("%d %s %v, %d calls to good/m", resp.StatusCode, got, h, goodCalls)
					}
					// A 429 keeps the provider's Retry-After, when it sent one.
					want := ""
					if f.status == 429 {
						want = f.step.Header.Get("Retry-After")
					}
					if h.Get("Retry-After") != want {
						t.Errorf("Retry-After %q", h.Get("Retry-After"))
					}
				}
			})
		}
	}
	log := s.log.String()
	if attempts, got := lines(log); attempts != wantAttempts || got != requests {
		t.Errorf("%d attempt lines for %d calls, %d request lines for %d requests", attempts, wantAttempts, got, requests)
	}
	// The private dial is the SEC-4 guard's refusal, not a failed lookup.
	if !strings.Contains(log, `"target":"rebound/m","status":0`) || !strings.Contains(log, "rebound.test: ") || strings.Contains(log, planted) {
		t.Errorf("log %s", log)
	}
	// AC-21: a failed attempt before a fallback is a child span of the
	// request's trace with its own target and error; a request that did not
	// fall back has its request span alone, with the error the client got.
	byTrace := map[string][]otlpSpan{}
	for _, sp := range s.exported(t) {
		byTrace[sp.TraceID] = append(byTrace[sp.TraceID], sp)
	}
	if len(byTrace) != len(wants) {
		t.Errorf("%d traces for %d requests", len(byTrace), len(wants))
	}
	for trace, want := range wants {
		spans := byTrace[trace]
		switch {
		case len(spans) != 1+map[bool]int{true: 1}[want.fellBack] || spans[0].Kind != 2:
			t.Errorf("%+v: spans %+v", want, spans)
		case want.fellBack:
			req, a := spans[0], spans[1]
			if req.Status != nil || req.attrs["stargaite.target"] != "good/m" || req.attrs["stargaite.attempts"] != int64(2) ||
				a.Kind != 3 || a.Name != "attempt "+want.first || a.ParentSpanID != req.SpanID || a.attrs["stargaite.target"] != want.first ||
				a.Status == nil || a.Status.Code != 2 || a.Status.Message == "" || a.attrs["error.type"] != want.code {
				t.Errorf("%+v: request span %+v, attempt span %+v", want, req, a)
			}
			// AC-18: each span's sent effort, level and cap are those its
			// provider got (the bodies checked above), translated again for
			// good/m.
			req.has(t, map[string]any{"stargaite.reasoning.effort.sent": "high", "gen_ai.request.reasoning.level": "high",
				"stargaite.reasoning.dialect": "adaptive_effort", "stargaite.max_output_tokens.sent": int64(500)})
			if want.first == "bad/m" {
				a.has(t, map[string]any{"stargaite.reasoning.effort.sent": "high", "gen_ai.request.reasoning.level": "high",
					"stargaite.reasoning.dialect": "reasoning_effort", "stargaite.max_output_tokens.sent": int64(500),
					"stargaite.params.changed": "max_tokens:added,reasoning_effort:added"})
			}
		default:
			s := spans[0]
			if s.attrs["stargaite.target"] != want.first || s.Status == nil || s.Status.Code != 2 || s.attrs["error.type"] != want.code ||
				s.attrs["http.response.status_code"] != int64(want.status) {
				t.Errorf("%+v: span %+v", want, s)
			}
		}
	}
}

// TestNoTargetLeft is RES-10 and the section 15 rule after a call (AC-21):
// once a provider was called and failed, the client gets that call's error,
// whatever excluded or skipped the targets after it; when both targets fail,
// the last call's error, with its Retry-After; the PAR-9 headers are the
// last attempt's, also when a fallback sends no effort or no change.
func TestNoTargetLeft(t *testing.T) {
	s, good, _ := newResilience(t)
	tools := `,"tools":[{"type":"function","function":{"name":"f"}}]`
	for _, tc := range []struct {
		name, alias, extra string
		bad, good          *mockprovider.Step
		status             int
		code, retry        string
		target, effort     string
		changed            string
		goodCalls          int
	}{
		{"the next target has no credential", "bad-off", "", &mockprovider.Step{Status: 500}, nil, 502, "upstream_error", "", "bad/m", "", "", 0},
		{"the next target is skipped", "bad-notools", tools, &mockprovider.Step{Status: 503}, nil, 502, "upstream_error", "", "bad/m", "", "max_tokens:added", 0},
		{"both fail", "bad-two", "", &mockprovider.Step{Status: 500}, &mockprovider.Step{Status: 429, Header: http.Header{"Retry-After": {"7"}}},
			429, "upstream_error", "7", "good/m", "high", "protocol:translated,max_tokens:added,output_config.effort:added", 1},
		// bad/m sends the client's effort, good/notools none: no stale header.
		{"the fallback sends no effort", "bad-notools", `,"reasoning_effort":"high"`, &mockprovider.Step{Status: 500}, nil, 200, "", "", "good/notools", "", "protocol:translated,max_tokens:added,reasoning_effort:dropped", 1},
		// badanth/m translates the body, goodoai/m sends it unchanged: no
		// stale change list.
		{"the fallback changes nothing", "badanth-oai", `,"max_tokens":100`, &mockprovider.Step{Status: 500}, nil, 200, "", "", "goodoai/m", "", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.pastWindow()
			s.mock.Script(*tc.bad)
			if tc.good != nil {
				good.Script(*tc.good)
			}
			goodBefore := len(good.Calls())
			resp, got := s.post(t, chatBody(tc.alias, tc.extra))
			var env struct{ Error domain.Error }
			if err := json.Unmarshal([]byte(got), &env); err != nil || resp.StatusCode != tc.status || env.Error.Code != tc.code ||
				resp.Header.Get("Retry-After") != tc.retry || resp.Header.Get(targetHeader) != tc.target || len(good.Calls())-goodBefore != tc.goodCalls ||
				resp.Header.Get(effortHeader) != tc.effort || resp.Header.Get(changedHeader) != tc.changed {
				t.Errorf("%d %s %v", resp.StatusCode, got, resp.Header)
			}
		})
	}
}

// TestBreakerBehaviour is AC-22 for the circuit (RES-2), on the service
// clock: five failures that count within 30 s open bad/m, so the next request
// to an alias with only bad/m answers 503 no_target_available without a call
// (RES-10), also after a static skip of the target before it, and an alias
// with a fallback goes straight to good/m; the circuit is keyed by target id,
// not provider, and outlives an apply (RES-7). After 20 s two trials
// pass and close it; five more failures open it again, and one failing trial
// reopens it for another 20 s. Failures that do not count never open it.
func TestBreakerBehaviour(t *testing.T) {
	s, good, yaml := newResilience(t)
	ask := func(alias string) (int, string) {
		resp, got := s.post(t, chatBody(alias, ""))
		var env struct{ Error domain.Error }
		_ = json.Unmarshal([]byte(got), &env)
		return resp.StatusCode, env.Error.Code
	}
	fail := func(n int, status int) {
		t.Helper()
		for range n {
			s.mock.Script(mockprovider.Step{Status: status})
			if status, _ := ask("bad-one"); status == 503 {
				t.Fatal("the circuit opened early")
			}
		}
	}
	open := func(why string) {
		t.Helper()
		before := len(s.mock.Calls())
		if status, code := ask("bad-one"); status != 503 || code != "no_target_available" || len(s.mock.Calls()) != before {
			t.Fatalf("%s: %d %s, %d calls", why, status, code, len(s.mock.Calls())-before)
		}
	}
	closed := func(why string) {
		t.Helper()
		before := len(s.mock.Calls())
		if status, _ := ask("bad-one"); status != 200 || len(s.mock.Calls()) != before+1 {
			t.Fatalf("%s: %d, %d calls", why, status, len(s.mock.Calls())-before)
		}
	}

	fail(10, 400) // never counts
	fail(4, 500)
	s.now.Add(29_000)
	fail(1, 429) // the fifth within 30 s
	open("five failures")
	goodBefore, badBefore := len(good.Calls()), len(s.mock.Calls())
	if status, _ := ask("bad-two"); status != 200 || len(good.Calls()) != goodBefore+1 || len(s.mock.Calls()) != badBefore {
		t.Fatalf("an open circuit is not passed over: %d", status)
	}
	// Another target on the same provider, and one on the same mock, each
	// have their own circuit.
	for _, alias := range []string{"bad-m2", "badanth-one"} {
		s.mock.Script(mockprovider.Step{Status: 500})
		before := len(s.mock.Calls())
		if status, _ := ask(alias); status != 502 || len(s.mock.Calls()) != before+1 {
			t.Fatalf("%s: %d", alias, status)
		}
	}
	// The first target skipped statically (tools), the open circuit is the
	// first dynamic reason: RES-10 answers it, not the skip.
	goodBefore, badBefore = len(good.Calls()), len(s.mock.Calls())
	resp, got := s.post(t, chatBody("notools-bad", `,"tools":[{"type":"function","function":{"name":"f"}}]`))
	if resp.StatusCode != 503 || !strings.Contains(got, `"code":"no_target_available"`) || len(good.Calls()) != goodBefore || len(s.mock.Calls()) != badBefore {
		t.Fatalf("a skip then an open circuit: %d %s", resp.StatusCode, got)
	}
	// An apply keeps the circuit.
	s.apply(t, strings.Replace(yaml, "keys:", "  extra: { targets: [refused/m] }\nkeys:", 1))
	open("after an apply")

	s.now.Add(20_000 - 1)
	open("before 20 s")
	s.now.Add(1)
	closed("first trial")
	closed("second trial")
	closed("closed")
	fail(5, 500)
	open("five more failures")
	s.now.Add(20_000)
	s.mock.Script(mockprovider.Step{Status: 529})
	if status, _ := ask("bad-one"); status != 502 {
		t.Fatalf("a failing trial: %d", status)
	}
	open("a failing trial")
	s.now.Add(20_000)
	closed("half-open again")
}

// TestSyncSlowClient: a synchronous answer whose write a client stalls past
// the total timer, then drops, is the client gone: no 504 and no breaker
// failure for the target, which answered (design section 10, Client gone).
func TestSyncSlowClient(t *testing.T) {
	s, _, yaml := newResilience(t)
	s.apply(t, strings.NewReplacer("total_ms: 300", "total_ms: 600", "max_response_bytes: 4096", "max_response_bytes: 33554432").Replace(yaml))
	// More than the socket buffers of both ends hold, so the write blocks.
	s.mock.Script(mockprovider.Step{Body: []byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"` + strings.Repeat("x", 16<<20) + `"},"finish_reason":"stop"}]}`)})
	conn, err := net.Dial("tcp", strings.TrimPrefix(s.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	tcp := conn.(*net.TCPConn)
	_ = tcp.SetReadBuffer(4096)
	body := chatBody("bad-one", "")
	sent := time.Now()
	if _, err := io.WriteString(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: proxy\r\nAuthorization: Bearer "+s.token+
		"\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body); err != nil {
		t.Fatal(err)
	}
	// The first byte of the answer, then nothing read past the total timer.
	if _, err := conn.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(sent.Add(800 * time.Millisecond)))
	_ = tcp.SetLinger(0) // a reset: the blocked write fails at once
	_ = conn.Close()
	for !strings.Contains(s.log.String(), `"msg":"request"`) {
		if time.Since(sent) > 3*time.Second {
			t.Fatalf("no request line: %s", s.log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	log := s.log.String()
	if !strings.Contains(log, `"cause":"client gone`) || strings.Contains(log, "total timeout") || strings.Contains(log, "upstream_timeout") {
		t.Errorf("log %s", log)
	}
}

// TestClassify is classify, through settle, per source of a status and per
// class of phase-5 design section 5.1: the status line, an error object with
// a status in place of an answer (OpenAI's code, Anthropic's documented type)
// and one without. Each row asserts the attempt's effective status, the
// verdict (eligible, breaker, operator, final) and the error answered when no
// target is left: its code, status and message. The client gone, the
// shutdown, the timers and a 2xx relayed are the controls.
func TestClassify(t *testing.T) {
	refused := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	body := []byte(`{"error":{"message":"bad thing sk-planted"}}`)
	retry := http.Header{"Retry-After": {"9"}}
	coded := func(status int) error { return &domain.ProviderError{Status: status, Message: "bad thing sk-planted"} }
	typed := func(typ string, status int) error {
		return &domain.ProviderError{Type: typ, Status: status, Message: "bad thing sk-planted"}
	}
	for _, tc := range []struct {
		name            string
		stream, written bool // written: after the commit
		status          int  // the status line
		cause, relay    error
		effective       int
		eligible        bool
		breaker         routing.Verdict
		operator, final bool
		code            string // "" for no error
		errStatus       int
		message         string
	}{
		{name: "2xx relayed", status: 200, effective: 200, breaker: routing.Pass},
		{name: "dial refused", cause: refused, eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 502, message: "the provider call failed"},
		{name: "connect timer", cause: errConnectTimeout, eligible: true, breaker: routing.Fail, code: "upstream_timeout", errStatus: 504, message: "connect timeout"},
		{name: "idle timer in a stream", stream: true, status: 200, effective: 200, cause: errIdleTimeout, eligible: true, breaker: routing.Fail, code: "upstream_timeout", errStatus: 504},
		{name: "sync body cut", status: 200, effective: 200, cause: io.ErrUnexpectedEOF, eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 502, message: "the provider call failed"},
		{name: "stream cut", stream: true, status: 200, effective: 200, cause: errStreamEnded, eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 502, message: "the provider's stream failed"},

		// Transient.
		{name: "429", status: 429, effective: 429, eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 429, message: "the provider answered 429"},
		{name: "500", status: 500, effective: 500, eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 502},
		{name: "529", status: 529, effective: 529, eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 502, message: "the provider answered 529"},
		{name: "code 502 inside a 200", status: 200, relay: coded(502), effective: 502, eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 502, message: "the provider answered 502"},
		{name: "code 429 inside a 200", status: 200, relay: coded(429), effective: 429, eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 429, message: "the provider answered 429"},
		{name: "openai chunk with code 503", stream: true, status: 200, relay: coded(503), effective: 503, eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 502},
		{name: "anthropic overloaded_error event", stream: true, status: 200, relay: typed("overloaded_error", 529), effective: 529,
			eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 502, message: "the provider answered 529"},
		{name: "anthropic timeout_error event", stream: true, status: 200, relay: typed("timeout_error", 504), effective: 504,
			eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 502},

		// The operator's.
		{name: "401", status: 401, effective: 401, eligible: true, breaker: routing.Pass, operator: true, final: true, code: "upstream_auth_error", errStatus: 502,
			message: "the provider refused the operator's credential"},
		{name: "403", status: 403, effective: 403, eligible: true, breaker: routing.Pass, operator: true, final: true, code: "upstream_auth_error", errStatus: 502},
		{name: "402", status: 402, effective: 402, eligible: true, breaker: routing.Pass, operator: true, final: true, code: "upstream_error", errStatus: 502,
			message: "the provider answered 402"},
		{name: "404", status: 404, effective: 404, eligible: true, breaker: routing.Pass, operator: true, final: true, code: "upstream_error", errStatus: 502,
			message: "the provider answered 404"},
		{name: "code 401 inside a 200", status: 200, relay: coded(401), effective: 401, eligible: true, breaker: routing.Pass, operator: true, final: true,
			code: "upstream_auth_error", errStatus: 502},
		{name: "anthropic billing_error event", stream: true, status: 200, relay: typed("billing_error", 402), effective: 402, eligible: true, breaker: routing.Pass,
			operator: true, final: true, code: "upstream_error", errStatus: 502, message: "the provider answered 402"},

		// The client's: the provider's text, redacted.
		{name: "400", status: 400, effective: 400, breaker: routing.Pass, final: true, code: "upstream_error", errStatus: 400, message: "bad thing [redacted]"},
		{name: "413", status: 413, effective: 413, breaker: routing.Pass, final: true, code: "upstream_error", errStatus: 413},
		{name: "422", status: 422, effective: 422, breaker: routing.Pass, final: true, code: "upstream_error", errStatus: 422},
		{name: "code 400 inside a 200", status: 200, relay: coded(400), effective: 400, breaker: routing.Pass, final: true, code: "upstream_error", errStatus: 400,
			message: "bad thing [redacted]"},
		{name: "anthropic invalid_request_error event", stream: true, status: 200, relay: typed("invalid_request_error", 400), effective: 400,
			breaker: routing.Pass, final: true, code: "upstream_error", errStatus: 400, message: "bad thing [redacted]"},

		// Nobody can tell.
		{name: "302", status: 302, effective: 302, breaker: routing.Pass, code: "upstream_error", errStatus: 502, message: "the provider answered 302"},
		{name: "409", status: 409, effective: 409, breaker: routing.Pass, code: "upstream_error", errStatus: 502},
		{name: "408", status: 408, effective: 408, breaker: routing.Pass, code: "upstream_error", errStatus: 502},
		{name: "an error object without a status", status: 200, relay: coded(0), effective: 200, breaker: routing.Pass, code: "upstream_error", errStatus: 502,
			message: "the provider reported an error in place of an answer"},
		{name: "an anthropic event of an unknown type", stream: true, status: 200, relay: &domain.ProviderError{Message: "new_error: x"}, effective: 200,
			breaker: routing.Pass, code: "upstream_error", errStatus: 502, message: "the provider's stream reported an error"},
		{name: "2xx not usable", status: 200, effective: 200, relay: errors.New("not JSON"), breaker: routing.Pass, code: "upstream_error", errStatus: 502, message: "not usable"},
		{name: "2xx not a stream", stream: true, status: 200, effective: 200, relay: errNotStream, breaker: routing.Pass, code: "upstream_error", errStatus: 502, message: "not an event stream"},
		{name: "oversized event", stream: true, status: 200, effective: 200, relay: transport.ErrEventTooLarge, breaker: routing.Pass, code: "upstream_error", errStatus: 502, message: "stream failed"},

		// After the commit: the breaker still hears the class, the answer is the
		// terminal event's (phase-5 design C45). A regression guard (phase-5
		// design C52): passes on bc85529, fails when settle turns a post-commit
		// Fail into Pass.
		{name: "openai chunk with code 502 after the commit", stream: true, written: true, status: 200, relay: coded(502), effective: 200, eligible: true,
			breaker: routing.Fail, code: "upstream_error", errStatus: 502, message: "the provider's stream reported an error"},
		{name: "stream cut after the commit", stream: true, written: true, status: 200, effective: 200, cause: errStreamEnded, eligible: true, breaker: routing.Fail,
			code: "upstream_error", errStatus: 502, message: "the provider's stream failed"},
		{name: "idle timer after the commit", stream: true, written: true, status: 200, effective: 200, cause: errIdleTimeout, eligible: true, breaker: routing.Fail,
			code: "upstream_timeout", errStatus: 504, message: "idle timeout"},

		{name: "client gone", stream: true, status: 200, effective: 200, cause: errClientGone, breaker: routing.None},
		{name: "shutdown", stream: true, status: 200, effective: 200, cause: ErrStopping, breaker: routing.None, code: "internal_error", errStatus: 500, message: "server stopping"},
		{name: "shutdown before an answer", cause: ErrStopping, breaker: routing.None, code: "internal_error", errStatus: 500, message: "server stopping"},
	} {
		a := &attempt{status: tc.status, written: tc.written, target: &config.Target{Provider: &config.Provider{Credential: transport.NewSecret("sk-planted")}}}
		settle(context.Background(), a, tc.stream, retry, body, tc.cause, tc.relay)
		v := a.end
		if a.status != tc.effective || v.eligible != tc.eligible || v.breaker != tc.breaker || v.operator != tc.operator || v.final != tc.final ||
			(v.err == nil) != (tc.code == "") {
			t.Errorf("%s: status %d, %+v", tc.name, a.status, v)
			continue
		}
		if v.err != nil && (v.err.Code != tc.code || v.err.Status != tc.errStatus || !strings.Contains(v.err.Message, tc.message) ||
			strings.Contains(v.err.Message, "sk-planted") || (v.err.RetryAfter == "9") != (tc.effective == 429)) {
			t.Errorf("%s: %+v", tc.name, v.err)
		}
		// The provider's text reaches the client for the client's class only.
		if v.err != nil && strings.Contains(v.err.Message, "bad thing") != (tc.final && !tc.operator) {
			t.Errorf("%s: message %q", tc.name, v.err.Message)
		}
	}
}

// TestBreakerClasses is RES-2 by class (phase-5 design section 5.1): a
// transient fault counts, so five 200s whose error object states 502 open
// bad/m's circuit; the operator's faults (401, 402, 403, 404), the client's
// (400) and a 2xx the relay cannot use do not, ten of each within the window
// leaving the circuit closed.
func TestBreakerClasses(t *testing.T) {
	s, _, _ := newResilience(t)
	ask := func() int {
		resp, _ := s.post(t, chatBody("bad-one", ""))
		return resp.StatusCode
	}
	for _, step := range []mockprovider.Step{{Status: 401}, {Status: 402}, {Status: 403}, {Status: 404}, {Status: 400}, {Body: []byte("oops")}} {
		s.pastWindow()
		for n := range 10 {
			s.mock.Script(step)
			if status := ask(); status == 503 {
				t.Fatalf("%d %s, failure %d: the circuit opened", step.Status, step.Body, n+1)
			}
		}
		before := len(s.mock.Calls())
		if status := ask(); status != 200 || len(s.mock.Calls()) != before+1 {
			t.Fatalf("%d %s: then %d", step.Status, step.Body, status)
		}
	}
	s.pastWindow()
	for n := range 5 {
		s.mock.Script(mockprovider.Step{Body: []byte(`{"error":{"code":502,"message":"no provider"}}`)})
		if status := ask(); status != 502 {
			t.Fatalf("coded 502, failure %d: %d", n+1, status)
		}
	}
	before := len(s.mock.Calls())
	if status := ask(); status != 503 || len(s.mock.Calls()) != before {
		t.Fatalf("after five coded 502s: %d, %d calls", status, len(s.mock.Calls())-before)
	}
}

// TestBreakerAfterCommit is RES-2 after the first chunk (phase-5 design C45):
// the answer is the terminal event, but the breaker still hears a transient
// fault, so five streams that each send a chunk and then end without their
// terminator, carry an OpenAI error chunk with code 502, or fall silent past
// stream_idle_ms open bad/m's circuit, and the next request is 503
// no_target_available without a call. A regression guard (phase-5 design
// C52): passes on bc85529, fails when settle turns a post-commit Fail into
// Pass.
func TestBreakerAfterCommit(t *testing.T) {
	for name, step := range map[string]mockprovider.Step{
		"no terminator":         {Chunks: sse(chunk("a"))},
		"an error chunk":        {Chunks: sse(chunk("a"), `{"error":{"code":502,"message":"no provider"}}`)},
		"silent past idle time": {Chunks: sse(chunk("a")), Hang: true},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := newResilience(t)
			for n := range 5 {
				s.mock.Script(step)
				if resp, got := s.post(t, chatBody("bad-one", `,"stream":true`)); resp.StatusCode != 200 || !strings.Contains(got, `"error"`) {
					t.Fatalf("stream %d: %d %s", n+1, resp.StatusCode, got)
				}
			}
			before := len(s.mock.Calls())
			if resp, got := s.post(t, chatBody("bad-one", `,"stream":true`)); resp.StatusCode != 503 || !strings.Contains(got, "no_target_available") ||
				len(s.mock.Calls()) != before {
				t.Fatalf("after five: %d %s, %d calls", resp.StatusCode, got, len(s.mock.Calls())-before)
			}
		})
	}
}

// TestShouldRetry is the request's retry hint (kick-off section 15, phase-5
// design section 5.1): x-should-retry: false when every target the request
// reached refused it for the client's or the operator's reason and none was
// passed over dynamically; absent whenever a new request may meet another
// answer, and on the proxy's own refusals, which never carry it.
func TestShouldRetry(t *testing.T) {
	s, good, _ := newResilience(t)
	tools := `,"tools":[{"type":"function","function":{"name":"f"}}]`
	for _, tc := range []struct {
		name, alias, extra string
		bad, good          *mockprovider.Step
		status             int
		final              bool
	}{
		{"a single 401", "bad-one", "", &mockprovider.Step{Status: 401}, nil, 502, true},
		{"a single 400", "bad-one", "", &mockprovider.Step{Status: 400}, nil, 400, true},
		{"a 404 then a statically skipped target", "bad-notools", tools, &mockprovider.Step{Status: 404}, nil, 502, true},
		{"a 200 with code 401", "bad-one", "", &mockprovider.Step{Body: []byte(`{"error":{"code":401}}`)}, nil, 502, true},

		{"429", "bad-one", "", &mockprovider.Step{Status: 429}, nil, 429, false},
		{"500", "bad-one", "", &mockprovider.Step{Status: 500}, nil, 502, false},
		{"a timer", "bad-one", "", &mockprovider.Step{Delay: 600 * time.Millisecond}, nil, 504, false},
		{"408", "bad-one", "", &mockprovider.Step{Status: 408}, nil, 502, false},
		{"an unusable 2xx", "bad-one", "", &mockprovider.Step{Body: []byte("oops")}, nil, 502, false},
		{"the proxy's own refusal", "bad-one", `,"enable_search":true`, nil, nil, 400, false},
		{"a served fallback", "bad-two", "", &mockprovider.Step{Status: 500}, nil, 200, false},
		{"a committed stream", "bad-one", `,"stream":true`, &mockprovider.Step{Chunks: sse(chunk("a"), `{"error":{"code":400,"message":"no"}}`)}, nil, 200, false},
		{"503 then 401", "bad-two", "", &mockprovider.Step{Status: 503}, &mockprovider.Step{Status: 401}, 502, false},
		// A regression guard (phase-5 design C53), with the quota pair below:
		// passes on a4ccd1f, fails when only an open circuit counts as a
		// target passed over.
		{"401 then a target over its budget", "bad-paid", "", &mockprovider.Step{Status: 401}, nil, 502, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.pastWindow()
			if tc.bad != nil {
				s.mock.Script(*tc.bad)
			}
			if tc.good != nil {
				good.Script(*tc.good)
			}
			resp, got := s.post(t, chatBody(tc.alias, tc.extra))
			if resp.StatusCode != tc.status || (resp.Header.Get("X-Should-Retry") == "false") != tc.final || len(resp.Header.Values("X-Should-Retry")) > 1 {
				t.Errorf("%d %v: %s", resp.StatusCode, resp.Header, got)
			}
		})
	}
	// A 401 then a full quota window: the first request takes sub/m's one
	// request of the hour, the second passes it over.
	for i, want := range []int{200, 502} {
		s.pastWindow()
		s.mock.Script(mockprovider.Step{Status: 401})
		if resp, got := s.post(t, chatBody("bad-sub", "")); resp.StatusCode != want || resp.Header.Get("X-Should-Retry") != "" {
			t.Errorf("a 401 then sub/m, request %d: %d %v: %s", i+1, resp.StatusCode, resp.Header, got)
		}
	}
	// A 401 then an open circuit: good/notools refuses, bad/m's circuit is
	// open, which reopens in 20 s.
	s.pastWindow()
	for range 5 {
		s.mock.Script(mockprovider.Step{Status: 500})
		s.post(t, chatBody("bad-one", ""))
	}
	good.Script(mockprovider.Step{Status: 401})
	before := len(s.mock.Calls())
	if resp, got := s.post(t, chatBody("notools-bad", "")); resp.StatusCode != 502 || resp.Header.Get("X-Should-Retry") != "" || len(s.mock.Calls()) != before {
		t.Errorf("a 401 then an open circuit: %d %v: %s", resp.StatusCode, resp.Header, got)
	}
}

// TestOperatorWarn is the operator's alert (phase-5 design section 5.1): a
// 402 inside a stream before the commit writes the one WARN, rate-limited,
// with the status alone, and its record carries that status; two targets
// refusing inside one limiter window each leave their text, redacted and cut
// to 2,000 characters, on their attempt line. The client's 400 writes no WARN
// and no provider_message.
func TestOperatorWarn(t *testing.T) {
	s, good, _ := newResilience(t)
	type line struct {
		Msg, Target, Cause string
		Status, Refusals   int
		ProviderMessage    string `json:"provider_message"`
	}
	read := func(from int) (warns, attempts []line) {
		for raw := range strings.SplitSeq(strings.TrimSpace(s.log.String()[from:]), "\n") {
			var l line
			if err := json.Unmarshal([]byte(raw), &l); err != nil {
				t.Fatalf("log line %s", raw)
			}
			switch l.Msg {
			case "provider refused the target":
				warns = append(warns, l)
			case "attempt":
				attempts = append(attempts, l)
			}
		}
		return warns, attempts
	}

	s.pastWindow()
	from := len(s.log.String())
	s.mock.Script(mockprovider.Step{Chunks: sse(`{"error":{"code":402,"message":"` + account + `, key ` + planted + `BAD_KEY"}}`)})
	resp, got := s.post(t, chatBody("bad-one", `,"stream":true`))
	if resp.StatusCode != 502 || !strings.Contains(got, `"message":"the provider answered 402","type":"api_error","code":"upstream_error"`) ||
		strings.Contains(got, account) {
		t.Fatalf("a 402 in a stream: %d %s", resp.StatusCode, got)
	}
	warns, attempts := read(from)
	if len(warns) != 1 || warns[0].Status != 402 || warns[0].Target != "bad/m" || warns[0].Refusals != 1 || len(attempts) != 1 ||
		attempts[0].Status != 402 || attempts[0].ProviderMessage != account+", key [redacted]" {
		t.Errorf("a 402 in a stream: warns %+v, attempts %+v", warns, attempts)
	}
	// Its record carries the effective status, so no tokens (USE-8).
	if r := s.stored(t, 1)[0]; r.Status != 402 || r.Input+r.Output != 0 || r.TokensSource != "provider" || str(r.ErrorType) != "upstream_error" {
		t.Errorf("record %+v", r)
	}

	// Within the same window: no WARN, both texts on their attempt lines.
	from = len(s.log.String())
	long := strings.Repeat("x", 2100) // within max_response_bytes
	s.mock.Script(mockprovider.Step{Status: 401, Body: []byte(`{"error":{"message":"bad key ` + planted + `BAD_KEY"}}`)})
	good.Script(mockprovider.Step{Status: 403, Body: []byte(`{"type":"error","error":{"type":"permission_error","message":"` + long + `"}}`)})
	if resp, got := s.post(t, chatBody("bad-two", "")); resp.StatusCode != 502 || !strings.Contains(got, `"code":"upstream_auth_error"`) {
		t.Fatalf("two refusals: %d %s", resp.StatusCode, got)
	}
	warns, attempts = read(from)
	if len(warns) != 0 || len(attempts) != 2 || attempts[0].ProviderMessage != "bad key [redacted]" || attempts[1].ProviderMessage != long[:2000] {
		t.Errorf("two refusals: warns %+v, attempts %+v", warns, attempts)
	}

	// The client's 400, past the window: neither.
	s.pastWindow()
	from = len(s.log.String())
	s.mock.Script(mockprovider.Step{Status: 400, Body: []byte(`{"error":{"message":"bad input"}}`)})
	if resp, got := s.post(t, chatBody("bad-one", "")); resp.StatusCode != 400 || !strings.Contains(got, "bad input") {
		t.Fatalf("a 400: %d %s", resp.StatusCode, got)
	}
	if warns, attempts = read(from); len(warns) != 0 || len(attempts) != 1 || attempts[0].ProviderMessage != "" {
		t.Errorf("a 400: warns %+v, attempts %+v", warns, attempts)
	}
}

// TestProviderMessage is the provider's text (kick-off section 15, phase-5
// design C45): read from error.message, a string error, a top-level message
// or detail; redacted, then cut to 2,000 characters, never splitting one, so
// a credential across the cut leaves no prefix of itself. A detail that is
// not a string and a body that is not JSON give the fixed text.
func TestProviderMessage(t *testing.T) {
	const credential = "sk-planted-credential"
	cases := []struct{ name, body, want string }{
		{"error.message", `{"error":{"message":"one"}}`, "one"},
		{"a string error", `{"error":"two"}`, "two"},
		{"a top-level message", `{"message":"three"}`, "three"},
		{"a top-level detail", `{"detail":"four"}`, "four"},
		{"an empty error.message, then detail", `{"error":{"message":""},"detail":"five"}`, "five"},
		{"two-byte characters", `{"error":{"message":"` + strings.Repeat("é", 512<<10) + `"}}`, strings.Repeat("é", 2000)},
		{"a credential across the cut", `{"error":{"message":"` + strings.Repeat("a", 1995) + credential + `"}}`, strings.Repeat("a", 1995) + "[reda"},
		{"a detail list", `{"detail":[{"loc":["body"],"msg":"field required"}]}`, "the provider answered 422"},
		{"not JSON", `<html>bad gateway</html>`, "the provider answered 422"},
	}
	for _, tc := range cases {
		a := &attempt{status: 422, target: &config.Target{Provider: &config.Provider{Credential: transport.NewSecret(credential)}}}
		settle(context.Background(), a, false, http.Header{}, []byte(tc.body), nil, nil)
		if a.end.err == nil || a.end.err.Message != tc.want || !utf8.ValidString(a.end.err.Message) || strings.Contains(a.end.err.Message, "sk-pl") {
			t.Errorf("%s: %+v", tc.name, a.end.err)
		}
	}
	// The same text, from an error in place of an answer.
	a := &attempt{status: 200, target: &config.Target{Provider: &config.Provider{Credential: transport.NewSecret(credential)}}}
	settle(context.Background(), a, false, http.Header{}, nil, nil, &domain.ProviderError{Status: 400, Message: strings.Repeat("a", 1995) + credential})
	if a.status != 400 || a.text != strings.Repeat("a", 1995)+"[reda" || a.end.err.Message != a.text {
		t.Errorf("in place of an answer: %d %q %+v", a.status, a.text, a.end.err)
	}
	// Never from any other 2xx body: it may be part of an answer (OPS-9).
	a = &attempt{status: 200, target: &config.Target{Provider: &config.Provider{Credential: transport.NewSecret(credential)}}}
	settle(context.Background(), a, false, http.Header{}, []byte(`{"message":"part of an answer","detail":"x"}`), nil, errors.New("not usable"))
	if a.text != "" {
		t.Errorf("a 2xx body: %q", a.text)
	}
}

// inflight is a request on the wire to the data plane: started is closed
// once the first line of the answer arrived (for a stream, the commit), or
// the call failed; answer gets the whole answer once the proxy ends it, or
// the error of a client that went away, which cancel makes it do.
type inflight struct {
	cancel  context.CancelFunc
	started chan struct{}
	answer  chan inflightAnswer
}

type inflightAnswer struct {
	status int
	header http.Header
	body   string
	err    error
}

// send posts body to path of the data plane at url with the client key.
func send(t *testing.T, url, token, path, body string) *inflight {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	f := &inflight{cancel, make(chan struct{}), make(chan inflightAnswer, 1)}
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			close(f.started)
			f.answer <- inflightAnswer{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		r := bufio.NewReader(resp.Body)
		first, err := r.ReadString('\n')
		close(f.started)
		rest, err2 := io.ReadAll(r)
		f.answer <- inflightAnswer{resp.StatusCode, resp.Header, first + string(rest), cmp.Or(err, err2)}
	}()
	return f
}

// ended waits up to 2 s for the n-th call of mock to end and returns it.
func ended(t *testing.T, mock *mockprovider.Server, n int) mockprovider.Call {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if calls := mock.Calls(); len(calls) > n && !calls[n].Ended.IsZero() {
			return calls[n]
		}
	}
	t.Fatalf("call %d of the mock did not end", n)
	return mockprovider.Call{}
}

// reached waits up to 2 s for the mock to have received n calls.
func reached(t *testing.T, mock *mockprovider.Server, n int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); len(mock.Calls()) < n; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the mock got %d calls, not %d", len(mock.Calls()), n)
		}
	}
}

// waitFlight waits up to 2 s for every request of the data plane to be done.
func waitFlight(t *testing.T, f *fixture) {
	t.Helper()
	idle := make(chan struct{})
	go func() { f.flight.Wait(); close(idle) }()
	select {
	case <-idle:
	case <-time.After(2 * time.Second):
		t.Fatal("requests still in flight")
	}
}

// cutCases are a provider call left open, on both inference routes,
// synchronous and streamed: the mock holds a synchronous answer back and
// hangs a stream after its first event, which commits the client's
// response. Timers are long enough never to fire.
func cutCases() []struct {
	name, path, body string
	step             mockprovider.Step
} {
	return []struct {
		name, path, body string
		step             mockprovider.Step
	}{
		{"chat sync", "/v1/chat/completions", chatBody("bad-one", ""), mockprovider.Step{Delay: time.Minute}},
		{"chat stream", "/v1/chat/completions", chatBody("bad-one", `,"stream":true`),
			mockprovider.Step{Chunks: sse(chunk("a")), Hang: true}},
		{"chat stream to anthropic", "/v1/chat/completions", chatBody("badanth-one", `,"stream":true`),
			mockprovider.Step{Chunks: events("message_start", messageStart, "content_block_start", textStart), Hang: true}},
		{"messages sync", "/v1/messages", messagesBody("badanth-one", ""), mockprovider.Step{Delay: time.Minute}},
		{"messages stream", "/v1/messages", messagesBody("badanth-one", `,"stream":true`),
			mockprovider.Step{Chunks: events("message_start", messageStart), Hang: true}},
	}
}

// longTimers replaces the millisecond timers of resilienceConfig.
var longTimers = strings.NewReplacer("total_ms: 300", "total_ms: 60000", "first_byte_ms: 300", "first_byte_ms: 60000",
	"stream_idle_ms: 300", "stream_idle_ms: 60000")

// TestClientGone is RES-5 and AC-23 (cancellation): a client that goes away
// while the provider call is open, before the answer or mid-stream, on both
// routes, cancels that call within 1 s, as the mock measures it (Ended and
// Cancelled). finish and done each run once: the attempt line has the cause
// "client gone" and the request line status 499 and code client_gone (no
// client reads them); the target's breaker is told nothing. The call leaves
// its usage record with error_type client_gone and the status as received:
// none before the answer, with zero tokens from the provider; 200 mid-stream,
// with tokens estimated (AC-23, phase-3 design section 4); its request span
// has error status "client gone", error.type client_gone and status 499.
func TestClientGone(t *testing.T) {
	s, _, yaml := newResilience(t)
	s.apply(t, longTimers.Replace(yaml))
	for i, tc := range cutCases() {
		t.Run(tc.name, func(t *testing.T) {
			s.mock.Script(tc.step)
			logged := len(s.log.String())
			f := send(t, s.url, s.token, tc.path, tc.body)
			reached(t, s.mock, i+1)
			if tc.step.Chunks != nil {
				<-f.started
			}
			gone := time.Now()
			f.cancel()
			if c := ended(t, s.mock, i); !c.Cancelled || c.Ended.Sub(gone) > time.Second {
				t.Errorf("the provider call ended %v after the client went away, cancelled %v", c.Ended.Sub(gone), c.Cancelled)
			}
			if a := <-f.answer; a.err == nil {
				t.Errorf("the client read a whole answer: %d %q", a.status, a.body)
			}
			waitFlight(t, s.fixture)
			log := s.log.String()[logged:]
			if attempts, requests := lines(log); attempts != 1 || requests != 1 || !strings.Contains(log, `"cause":"client gone"`) ||
				!strings.Contains(log, `"attempts":1,"status":499,"code":"client_gone"`) {
				t.Errorf("log %s", log)
			}
			r := s.stored(t, i+1)[i]
			status, tokens := 0, "provider"
			if tc.step.Chunks != nil {
				status, tokens = 200, "estimated"
			}
			if str(r.ErrorType) != "client_gone" || r.Status != status || r.TokensSource != tokens || r.Stream != (tc.step.Chunks != nil) ||
				status == 0 && r.Input+r.Output != 0 || status == 200 && r.Input == 0 {
				t.Errorf("record %+v", r)
			}
		})
	}
	uncounted(t, s)
	// Each cut request's span, sent first, is a failure: 499 client_gone.
	spans := s.exported(t)
	if len(spans) != len(cutCases())+8 {
		t.Fatalf("%d spans", len(spans))
	}
	for _, sp := range spans[:len(cutCases())] {
		if sp.Kind != 2 {
			t.Errorf("span %+v", sp)
		}
		sp.failed(t, "client_gone", 499, "client gone")
	}
}

// uncounted checks that the cut calls, two on bad/m and three on badanth/m
// within the RES-2 window, told their breaker nothing: four failures more
// each stay 502, where those cuts counted as failures would open a circuit
// (503) by the fourth.
func uncounted(t *testing.T, s *streamFixture) {
	t.Helper()
	for range 8 {
		s.mock.Script(mockprovider.Step{Status: 500})
	}
	for _, alias := range []string{"bad-one", "badanth-one"} {
		for n := range 4 {
			if resp, got := s.post(t, chatBody(alias, "")); resp.StatusCode != 502 {
				t.Errorf("%s, failure %d: %d %s", alias, n+1, resp.StatusCode, got)
			}
		}
	}
}

// TestShutdown is RES-6 and AC-23 (drain) in the data plane: when serve
// cancels the server's base context with ErrStopping, as it does when the
// shutdown grace ends, every provider call still open ends within 1 s, as
// the mock measures it; a synchronous request gets 500 internal_error
// "server stopping" in the envelope of its route, a committed stream that
// error as its one terminal event (no [DONE] on chat, an Anthropic error
// event on messages). Each request runs finish and done once, the request
// line has that status and code, Flight returns to zero, and the target's
// breaker is told nothing.
func TestShutdown(t *testing.T) {
	s, _, yaml := newResilience(t)
	s.apply(t, longTimers.Replace(yaml))
	base, halt := context.WithCancelCause(context.Background())
	defer halt(nil)
	srv := httptest.NewUnstartedServer(s.data)
	srv.Config.BaseContext = func(net.Listener) context.Context { return base }
	srv.Start()
	t.Cleanup(srv.Close)

	cases := cutCases()
	open := make([]*inflight, len(cases))
	for i, tc := range cases {
		s.mock.Script(tc.step)
		open[i] = send(t, srv.URL, s.token, tc.path, tc.body)
		reached(t, s.mock, i+1)
		if tc.step.Chunks != nil {
			<-open[i].started
		}
	}
	stopped := time.Now()
	halt(ErrStopping)
	for i, tc := range cases {
		if c := ended(t, s.mock, i); !c.Cancelled || c.Ended.Sub(stopped) > time.Second {
			t.Errorf("%s: the provider call ended %v after the stop, cancelled %v", tc.name, c.Ended.Sub(stopped), c.Cancelled)
		}
		a := <-open[i].answer
		var want string
		switch {
		case tc.path == "/v1/messages" && tc.step.Chunks != nil:
			want = ev("message_start", messageStart, "error", `{"type":"error","error":{"type":"api_error","message":"internal_error: server stopping"}}`)
		case tc.step.Chunks != nil:
			want = terminal("server stopping", "internal_error")
			if !strings.HasPrefix(a.body, "data: ") || !strings.HasSuffix(a.body, "\n\n"+want) || strings.Count(a.body, `"error"`) != 1 || strings.Contains(a.body, "[DONE]") {
				t.Errorf("%s: %d %q", tc.name, a.status, a.body)
			}
			continue
		case tc.path == "/v1/messages":
			if got := anthropicEnvelope(t, a.header, 500, "internal_error", []byte(a.body)); got != "internal_error: server stopping" {
				t.Errorf("%s: message %q", tc.name, got)
			}
			continue
		default:
			want = `{"error":{"message":"server stopping","type":"api_error","code":"internal_error"}}` + "\n"
		}
		if a.err != nil || a.body != want || a.status != map[bool]int{true: 200, false: 500}[tc.step.Chunks != nil] {
			t.Errorf("%s: %d %q: %v", tc.name, a.status, a.body, a.err)
		}
	}
	waitFlight(t, s.fixture)
	log := s.log.String()
	if attempts, requests := lines(log); attempts != len(cases) || requests != len(cases) ||
		strings.Count(log, `"cause":"server stopping"`) != len(cases) || strings.Count(log, `"status":500,"code":"internal_error"`) != len(cases) {
		t.Errorf("log %s", log)
	}
	uncounted(t, s)
}

// TestHalfOpenCut is RES-2 for a trial cut by the client or by the shutdown
// (design section 10): the cut tells the breaker nothing, so its permit comes
// back and the circuit stays half-open. Both trials of a half-open bad/m are
// cut while a third request is refused; then a passing and a failing request
// are both admitted as trials, and the failure reopens the circuit. A cut
// counted as a pass would have closed it, one counted as a failure reopened it.
func TestHalfOpenCut(t *testing.T) {
	for _, stopping := range []bool{false, true} {
		t.Run(map[bool]string{false: "client gone", true: "shutdown"}[stopping], func(t *testing.T) {
			s, _, yaml := newResilience(t)
			s.apply(t, longTimers.Replace(yaml))
			base, halt := context.WithCancelCause(context.Background())
			defer halt(nil)
			srv := httptest.NewUnstartedServer(s.data)
			srv.Config.BaseContext = func(net.Listener) context.Context { return base }
			srv.Start()
			t.Cleanup(srv.Close)
			ask := func(why string, want int) {
				t.Helper()
				before := len(s.mock.Calls())
				resp, got := s.post(t, chatBody("bad-one", ""))
				if calls := len(s.mock.Calls()) - before; resp.StatusCode != want || calls != map[bool]int{true: 0, false: 1}[want == 503] {
					t.Fatalf("%s: %d %s, %d calls", why, resp.StatusCode, got, calls)
				}
			}
			for range 5 {
				s.mock.Script(mockprovider.Step{Status: 500})
				ask("a failure", 502)
			}
			ask("five failures", 503)
			s.now.Add(20_000)
			trials := make([]*inflight, 2)
			for i := range trials {
				s.mock.Script(mockprovider.Step{Delay: time.Minute})
				trials[i] = send(t, srv.URL, s.token, "/v1/chat/completions", chatBody("bad-one", ""))
				reached(t, s.mock, 6+i)
			}
			ask("both trials out", 503)
			if stopping {
				halt(ErrStopping)
			} else {
				for _, f := range trials {
					f.cancel()
				}
			}
			for i, f := range trials {
				if c := ended(t, s.mock, 5+i); !c.Cancelled {
					t.Fatalf("trial %d was not cancelled", i+1)
				}
				if a := <-f.answer; stopping && a.status != 500 || !stopping && a.err == nil {
					t.Fatalf("trial %d: %d %q %v", i+1, a.status, a.body, a.err)
				}
			}
			waitFlight(t, s.fixture)
			ask("a trial after the cuts", 200)
			s.mock.Script(mockprovider.Step{Status: 500})
			ask("a failing trial", 502)
			ask("reopened", 503)
		})
	}
}

// stalledWriter is a client that stops reading once stall is closed: from
// then on a write blocks until its write deadline passes, as on a socket
// whose buffers are full. wrote is closed by the first write.
type stalledWriter struct {
	*httptest.ResponseRecorder
	stall, wrote chan struct{}
	once         sync.Once
	mu           sync.Mutex
	deadline     time.Time
}

func (w *stalledWriter) SetWriteDeadline(d time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadline = d
	return nil
}

func (w *stalledWriter) Write(p []byte) (int, error) {
	defer w.once.Do(func() { close(w.wrote) })
	select {
	case <-w.stall:
	default:
		return w.ResponseRecorder.Write(p)
	}
	return 0, w.blocked()
}

// blocked returns once the write deadline has passed.
func (w *stalledWriter) blocked() error {
	for ; ; time.Sleep(5 * time.Millisecond) {
		w.mu.Lock()
		d := w.deadline
		w.mu.Unlock()
		if !d.IsZero() && time.Now().After(d) {
			return os.ErrDeadlineExceeded
		}
	}
}

// TestShutdownStalledClient: a committed stream whose client stops reading
// just before the shutdown ends about 1 s after it, so within the drain
// wait: the terminal event's write keeps the 1 s infer gives a write once
// the request's context ends, not the stream's per-event deadline.
func TestShutdownStalledClient(t *testing.T) {
	s, _, yaml := newResilience(t)
	s.apply(t, longTimers.Replace(yaml))
	s.mock.Script(mockprovider.Step{Chunks: sse(chunk("a")), Hang: true})
	base, halt := context.WithCancelCause(context.Background())
	defer halt(nil)
	r := httptest.NewRequestWithContext(base, http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("bad-one", `,"stream":true`)))
	r.Header.Set("Authorization", "Bearer "+s.token)
	w := &stalledWriter{ResponseRecorder: httptest.NewRecorder(), stall: make(chan struct{}), wrote: make(chan struct{})}
	served := make(chan struct{})
	go func() { s.data.ServeHTTP(w, r); close(served) }()
	select {
	case <-w.wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream did not commit")
	}
	close(w.stall)
	stopped := time.Now()
	halt(ErrStopping)
	select {
	case <-served:
		if d := time.Since(stopped); d > 1500*time.Millisecond {
			t.Errorf("the request ended %v after the stop", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the request still runs 3 s after the stop")
	}
	waitFlight(t, s.fixture)
}

// stalledFlush is a client whose socket fills with the end of a stream: the
// second flush, infer's of the held terminator, closes stall and blocks
// until its write deadline passes.
type stalledFlush struct {
	stalledWriter
	flushes atomic.Int32
}

func (w *stalledFlush) FlushError() error {
	if w.flushes.Add(1) < 2 {
		return nil
	}
	close(w.stall)
	return w.blocked()
}

// TestShutdownStalledLastFlush: a committed stream the shutdown stops, whose
// last flush blocks on a client that stopped reading, lets Flight go about
// 1 s after the stop, so within the drain wait: the flush runs while infer's
// hook still caps a write to 1 s, not under the stream's per-event deadline
// (stream_idle_ms + 5 s).
func TestShutdownStalledLastFlush(t *testing.T) {
	s, _, yaml := newResilience(t)
	s.apply(t, longTimers.Replace(yaml))
	s.mock.Script(mockprovider.Step{Chunks: sse(chunk("a")), Hang: true})
	base, halt := context.WithCancelCause(context.Background())
	defer halt(nil)
	r := httptest.NewRequestWithContext(base, http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("bad-one", `,"stream":true`)))
	r.Header.Set("Authorization", "Bearer "+s.token)
	w := &stalledFlush{stalledWriter: stalledWriter{ResponseRecorder: httptest.NewRecorder(), stall: make(chan struct{}), wrote: make(chan struct{})}}
	go s.data.ServeHTTP(w, r)
	select {
	case <-w.wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream did not commit")
	}
	stopped := time.Now()
	halt(ErrStopping)
	select {
	case <-w.stall:
	case <-time.After(2 * time.Second):
		t.Fatal("the last flush did not start")
	}
	idle := make(chan struct{})
	go func() { s.flight.Wait(); close(idle) }()
	select {
	case <-idle:
		if d := time.Since(stopped); d > 1500*time.Millisecond {
			t.Errorf("Flight was done %v after the stop", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Flight is still held 3 s after the stop")
	}
}

// TestStreamEndOneWrite is the control of infer's flush for a stream: one
// that ends in time is not flushed by infer, so its end leaves with the
// body's last chunk when the handler returns, in one write.
func TestStreamEndOneWrite(t *testing.T) {
	s, _, _ := newResilience(t)
	s.mock.Script(mockprovider.Step{Chunks: sse(chunk("a"), "[DONE]")})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("bad-one", `,"stream":true`)))
	r.Header.Set("Authorization", "Bearer "+s.token)
	w := &heldWriter{ResponseRecorder: httptest.NewRecorder(), wrote: make(chan struct{})}
	s.data.ServeHTTP(w, r)
	if held := string(w.held); held != "data: [DONE]\n\n" {
		t.Errorf("held when the handler returned: %q; sent %q", held, w.sent)
	}
}

// TestSyncAnswerLength is the control of infer's flush: a synchronous answer
// is not flushed before the handler returns, so it goes out with its
// Content-Length, not chunked.
func TestSyncAnswerLength(t *testing.T) {
	s := newStreamFixture(t, streamConfig)
	if resp, got := s.post(t, chatBody("plain", "")); resp.StatusCode != http.StatusOK || resp.ContentLength != int64(len(got)) {
		t.Errorf("a synchronous 200 of %d bytes: %d, Content-Length %d, Transfer-Encoding %v", len(got), resp.StatusCode, resp.ContentLength, resp.TransferEncoding)
	}
}

// heldWriter keeps what the handler writes until it flushes, as the
// server's buffer does: sent is what a flush let out. wrote is closed by the
// first write.
type heldWriter struct {
	*httptest.ResponseRecorder
	mu         sync.Mutex
	held, sent []byte
	wrote      chan struct{}
	once       sync.Once
}

func (w *heldWriter) Write(p []byte) (int, error) {
	defer w.once.Do(func() { close(w.wrote) })
	w.mu.Lock()
	defer w.mu.Unlock()
	w.held = append(w.held, p...)
	return len(p), nil
}

func (w *heldWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sent, w.held = append(w.sent, w.held...), nil
}

// TestStoppedStreamLeavesBeforeFlight is RES-6 for the terminal event of a
// committed stream the shutdown stops (phase-5 design C53): done holds it in
// the server's buffer, and it has left by the time Flight lets serve go on
// to close the store and exit. The writer stands for the server's buffer,
// so the order is exact: nothing is flushed after the handler returns.
func TestStoppedStreamLeavesBeforeFlight(t *testing.T) {
	s, _, yaml := newResilience(t)
	s.apply(t, longTimers.Replace(yaml))
	s.mock.Script(mockprovider.Step{Chunks: sse(chunk("a")), Hang: true})
	base, halt := context.WithCancelCause(context.Background())
	defer halt(nil)
	r := httptest.NewRequestWithContext(base, http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("bad-one", `,"stream":true`)))
	r.Header.Set("Authorization", "Bearer "+s.token)
	w := &heldWriter{ResponseRecorder: httptest.NewRecorder(), wrote: make(chan struct{})}
	go s.data.ServeHTTP(w, r)
	select {
	case <-w.wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream did not commit")
	}
	halt(ErrStopping)
	waitFlight(t, s.fixture)
	w.mu.Lock()
	sent, held := string(w.sent), string(w.held)
	w.mu.Unlock()
	if want := "data: " + chunk("a") + "\n\n" + terminal("server stopping", "internal_error"); sent != want || held != "" {
		t.Errorf("when Flight is done the client has %q, %q still held; want %q", sent, held, want)
	}
}
