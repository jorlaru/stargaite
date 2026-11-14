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
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/adapters/anthropic"
	"github.com/jorlaru/stargaite/internal/adapters/openai"
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
// provider bad; goodoai/m is an openai target on the GOOD mock. Timers are
// milliseconds.
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
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := closed.Addr().String()
	_ = closed.Close()
	yaml = strings.NewReplacer("base_url: BAD,", "base_url: "+badSrv.URL+",", "base_url: GOOD,", "base_url: "+goodSrv.URL+",",
		"base_url: REFUSED,", "base_url: http://"+refused+",",
		"base_url: REBOUND,", "base_url: https://rebound.test:"+badSrv.URL[strings.LastIndex(badSrv.URL, ":")+1:]+",").Replace(resilienceConfig)
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
			{name: "401", first: "bad", step: &mockprovider.Step{Status: 401}, status: 502, code: "upstream_auth_error"},
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
			status: 502, code: "upstream_error", eligible: true},
		fault{name: "an anthropic invalid_request_error event first", stream: true, first: "badanth", step: &mockprovider.Step{Chunks: anthError("invalid_request_error")},
			status: 502, code: "upstream_error"},
		fault{name: "an openai error chunk first", stream: true, first: "bad", step: &mockprovider.Step{Chunks: sse(`{"error":{"message":"boom","type":"server_error"}}`)},
			status: 502, code: "upstream_error"},
		fault{name: "an oversized event first", stream: true, first: "bad", step: &mockprovider.Step{Chunks: sse(big)}, status: 502, code: "upstream_error"},
		fault{name: "cut after the commit", stream: true, first: "bad", step: &mockprovider.Step{Chunks: [][]byte{[]byte(first)}, Cut: true},
			status: 200, code: "upstream_error", eligible: true, committed: true},
		fault{name: "gap past idle after the commit", stream: true, first: "bad", step: &mockprovider.Step{Chunks: [][]byte{[]byte(first), []byte("data: [DONE]\n\n")}, Gap: 600 * time.Millisecond},
			status: 200, code: "upstream_timeout", eligible: true, committed: true},
		fault{name: "an error chunk after the commit", stream: true, first: "bad", step: &mockprovider.Step{Chunks: sse(chunk("a"), `{"error":{"message":"boom"}}`)},
			status: 200, code: "", committed: true},
	)

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
						!strings.Contains(got, `"error":{`) || !strings.Contains(got, `"code":"`+f.code+`"`) && f.code != "" {
						t.Errorf("committed: %d %q, %d calls to good/m", resp.StatusCode, got, goodCalls)
					}
				default:
					var env struct{ Error domain.Error }
					if err := json.Unmarshal([]byte(got), &env); err != nil || resp.StatusCode != f.status || env.Error.Code != f.code || goodCalls != 0 ||
						h.Get(targetHeader) != f.first+"/m" {
						t.Errorf("%d %s %v, %d calls to good/m", resp.StatusCode, got, h, goodCalls)
					}
					if want := map[bool]string{true: "3"}[f.status == 429]; h.Get("Retry-After") != want {
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
// clock: five eligible failures within 30 s open bad/m, so the next request
// to an alias with only bad/m answers 503 no_target_available without a call
// (RES-10), also after a static skip of the target before it, and an alias
// with a fallback goes straight to good/m; the circuit is keyed by target id,
// not provider, and outlives an apply (RES-7). After 20 s two trials
// pass and close it; five more failures open it again, and one failing trial
// reopens it for another 20 s. Non-eligible failures never open it.
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

	fail(10, 400) // never eligible
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

// TestClassify is classify per row of design section 10's table.
func TestClassify(t *testing.T) {
	refused := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	body := []byte(`{"error":{"message":"bad thing"}}`)
	retry := http.Header{"Retry-After": {"9"}}
	for _, tc := range []struct {
		name         string
		stream       bool
		status       int
		cause, relay error
		eligible     bool
		breaker      routing.Verdict
		code         string // "" for no error
		errStatus    int
		message      string
	}{
		{name: "2xx relayed", status: 200, breaker: routing.Pass},
		{name: "dial refused", cause: refused, eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 502, message: "the provider call failed"},
		{name: "connect timer", cause: errConnectTimeout, eligible: true, breaker: routing.Fail, code: "upstream_timeout", errStatus: 504, message: "connect timeout"},
		{name: "idle timer in a stream", stream: true, status: 200, cause: errIdleTimeout, eligible: true, breaker: routing.Fail, code: "upstream_timeout", errStatus: 504},
		{name: "429", status: 429, eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 429, message: "the provider answered 429"},
		{name: "500", status: 500, eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 502},
		{name: "529", status: 529, eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 502, message: "the provider answered 529"},
		{name: "sync body cut", status: 200, cause: io.ErrUnexpectedEOF, eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 502, message: "the provider call failed"},
		{name: "stream cut", stream: true, status: 200, cause: errStreamEnded, eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 502, message: "the provider's stream failed"},
		{name: "anthropic overloaded_error event", stream: true, status: 200, relay: &anthropic.ErrorEvent{Type: "overloaded_error", Status: 529},
			eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 502, message: "the provider's stream reported an error"},
		{name: "anthropic timeout_error event", stream: true, status: 200, relay: &anthropic.ErrorEvent{Type: "timeout_error", Status: 504},
			eligible: true, breaker: routing.Fail, code: "upstream_error", errStatus: 502},
		{name: "anthropic invalid_request_error event", stream: true, status: 200, relay: &anthropic.ErrorEvent{Type: "invalid_request_error", Status: 400},
			breaker: routing.Pass, code: "upstream_error", errStatus: 502},
		{name: "anthropic event of an unknown type", stream: true, status: 200, relay: &anthropic.ErrorEvent{Type: "new_error"},
			breaker: routing.Pass, code: "upstream_error", errStatus: 502},
		{name: "openai error chunk", stream: true, status: 200, relay: &openai.ErrorChunk{Message: "boom"}, breaker: routing.Pass, code: "upstream_error", errStatus: 502},
		{name: "400", status: 400, breaker: routing.Pass, code: "upstream_error", errStatus: 400, message: "bad thing"},
		{name: "404", status: 404, breaker: routing.Pass, code: "upstream_error", errStatus: 404, message: "bad thing"},
		{name: "413", status: 413, breaker: routing.Pass, code: "upstream_error", errStatus: 413},
		{name: "422", status: 422, breaker: routing.Pass, code: "upstream_error", errStatus: 422},
		{name: "401", status: 401, breaker: routing.Pass, code: "upstream_auth_error", errStatus: 502, message: "the provider refused the operator's credential"},
		{name: "403", status: 403, breaker: routing.Pass, code: "upstream_auth_error", errStatus: 502},
		{name: "302", status: 302, breaker: routing.Pass, code: "upstream_error", errStatus: 502, message: "the provider answered 302"},
		{name: "409", status: 409, breaker: routing.Pass, code: "upstream_error", errStatus: 502},
		{name: "2xx not usable", status: 200, relay: errors.New("not JSON"), breaker: routing.Pass, code: "upstream_error", errStatus: 502, message: "not usable"},
		{name: "2xx not a stream", stream: true, status: 200, relay: errNotStream, breaker: routing.Pass, code: "upstream_error", errStatus: 502, message: "not an event stream"},
		{name: "oversized event", stream: true, status: 200, relay: transport.ErrEventTooLarge, breaker: routing.Pass, code: "upstream_error", errStatus: 502, message: "stream failed"},
		{name: "client gone", stream: true, status: 200, cause: errClientGone, breaker: routing.None},
		{name: "shutdown", stream: true, status: 200, cause: ErrStopping, breaker: routing.None, code: "internal_error", errStatus: 500, message: "server stopping"},
		{name: "shutdown before an answer", cause: ErrStopping, breaker: routing.None, code: "internal_error", errStatus: 500, message: "server stopping"},
	} {
		v := classify(tc.stream, tc.status, tc.cause, retry, body, tc.relay)
		if v.eligible != tc.eligible || v.breaker != tc.breaker || (v.err == nil) != (tc.code == "") {
			t.Errorf("%s: %+v", tc.name, v)
			continue
		}
		if v.err != nil && (v.err.Code != tc.code || v.err.Status != tc.errStatus || !strings.Contains(v.err.Message, tc.message) ||
			(v.err.RetryAfter == "9") != (tc.status == 429)) {
			t.Errorf("%s: %+v", tc.name, v.err)
		}
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
// client reads them); the target's breaker is told nothing.
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
		})
	}
	uncounted(t, s)
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
	for ; ; time.Sleep(5 * time.Millisecond) {
		w.mu.Lock()
		d := w.deadline
		w.mu.Unlock()
		if !d.IsZero() && time.Now().After(d) {
			return 0, os.ErrDeadlineExceeded
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
