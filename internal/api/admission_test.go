package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// admissionConfig: rpm, tpm and parallel limits on keys, on aliases and on
// the instance; broken has no credential, so a request admitted to it is
// refused later, at the target; fb falls back from pay/m to pay/n.
const admissionConfig = `version: 1
settings: { max_parallel_requests: 2 }
providers:
  pay: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true }
  off: { protocol: openai, base_url: MOCK, api_key_env: MISSING_KEY, billing: pay_per_use, allow_private_network: true }
targets:
  pay/m: { kind: chat, pricing: { input_per_million: 1, output_per_million: 2, as_of: "2026-11-01" } }
  pay/n: { kind: chat }
  off/m: { kind: chat }
models:
  fast: { targets: [pay/m] }
  capped: { targets: [pay/m], rpm_limit: 1 }
  capped2: { targets: [pay/m], rpm_limit: 1 }
  tcap: { targets: [pay/m], tpm_limit: 3 }
  single: { targets: [pay/m], max_parallel_requests: 1 }
  broken: { targets: [off/m] }
  fb: { targets: [pay/m, pay/n] }
keys:
  agent: { models: ["*"], rpm_limit: 2 }
  other: { models: ["*"] }
  tok: { models: ["*"], tpm_limit: 3 }
  par: { models: ["*"], max_parallel_requests: 1 }
  ra: { models: ["*"], rpm_limit: 1, tpm_limit: 3 }
  rb: { models: ["*"], rpm_limit: 1, tpm_limit: 3 }
`

// newAdmission is the data plane on a real listener with admissionConfig
// applied toward one mock, and the token of each key.
func newAdmission(t *testing.T) (*streamFixture, map[string]string) {
	t.Helper()
	f := newFixture(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	tokens := f.apply(t, strings.ReplaceAll(admissionConfig, "base_url: MOCK", "base_url: "+srv.URL))
	data := httptest.NewServer(f.data)
	t.Cleanup(data.Close)
	return &streamFixture{fixture: f, mock: mock, mockURL: srv.URL, url: data.URL}, tokens
}

// limited checks a refusal at admission: 429 in the OpenAI envelope with the
// code, the message and Retry-After, and the PAR-9 headers due to a request
// that reached no target: the request id and the trace id, no target.
func limited(t *testing.T, w http.Header, status int, body []byte, code, message, retryAfter string) {
	t.Helper()
	var env struct{ Error domain.Error }
	if status != http.StatusTooManyRequests || json.Unmarshal(body, &env) != nil || env.Error.Code != code || env.Error.Type != "rate_limit_error" ||
		env.Error.Message != message || w.Get("Retry-After") != retryAfter {
		t.Errorf("want 429 %s %q Retry-After %s; got %d %s Retry-After %q", code, message, retryAfter, status, body, w.Get("Retry-After"))
	}
	if w.Get(domain.RequestIDHeader) == "" || len(w.Get(traceHeader)) != 32 || w.Values(targetHeader) != nil {
		t.Errorf("headers %v", w)
	}
}

// TestRateLimits is AC-38 for rpm_limit and tpm_limit on the test clock
// (LIM-5): per key and per alias, independent of each other; a request
// refused by any of them reaches no provider, leaves no usage record and
// counts in none of them, while one admitted and refused later still counts;
// tpm counts the recorded input plus output of each call, dated at finish.
// The window admits again once the Retry-After has passed. The key refuses
// before the alias, and the instance carries no rpm or tpm.
func TestRateLimits(t *testing.T) {
	s, tokens := newAdmission(t)
	f, mock := s.fixture, s.mock
	post := func(key, alias string) *httptest.ResponseRecorder {
		t.Helper()
		return f.chat("Bearer "+tokens[key], chatBody(alias, ""))
	}
	ok := func(key, alias string) {
		t.Helper()
		if w := post(key, alias); w.Code != http.StatusOK {
			t.Errorf("%s to %s: %d %s", key, alias, w.Code, w.Body)
		}
	}

	ok("agent", "fast")
	ok("agent", "fast")
	// The third within a minute, at the same instant: the bucket of now
	// leaves the window 61 s from now (a whole second on the test clock).
	logged := len(f.log.String())
	w := post("agent", "fast")
	limited(t, w.Header(), w.Code, w.Body.Bytes(), "rate_limit_exceeded", "the rpm_limit of this key is reached", "61")
	if log := f.log.String()[logged:]; !strings.Contains(log, `"key":"agent","alias":"fast","attempts":0,"status":429,"code":"rate_limit_exceeded"`) ||
		strings.Contains(log, `"msg":"attempt"`) {
		t.Errorf("log %s", log)
	}
	ok("other", "fast") // another key is served
	if n := len(mock.Calls()); n != 3 {
		t.Errorf("the mock got %d calls, want 3", n)
	}
	f.stored(t, 3) // none for the refusal

	f.now.Add(60_999)
	if w := post("agent", "fast"); w.Code != http.StatusTooManyRequests {
		t.Errorf("a millisecond before the wait: %d", w.Code)
	}
	f.now.Add(1)
	// The alias's rpm refuses agent: that refusal counts in agent's rpm
	// neither, so agent still has its second request of the window.
	ok("agent", "fast")
	ok("other", "capped")
	w = post("agent", "capped")
	limited(t, w.Header(), w.Code, w.Body.Bytes(), "rate_limit_exceeded", `the rpm_limit of model "capped" is reached`, "61")
	ok("agent", "fast")
	w = post("agent", "fast")
	limited(t, w.Header(), w.Code, w.Body.Bytes(), "rate_limit_exceeded", "the rpm_limit of this key is reached", "61")

	// Admitted, then refused at its target: it still counts.
	f.now.Add(61_000)
	if w := post("agent", "broken"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("broken: %d %s", w.Code, w.Body)
	}
	ok("agent", "fast")
	if w := post("agent", "fast"); w.Code != http.StatusTooManyRequests {
		t.Errorf("after an admitted request refused later: %d", w.Code)
	}

	// tpm: the first call's 1 + 2 tokens reach the limit of 3.
	ok("tok", "fast")
	w = post("tok", "fast")
	limited(t, w.Header(), w.Code, w.Body.Bytes(), "rate_limit_exceeded", "the tpm_limit of this key is reached", "61")
	// /v1/messages answers a refusal in its own envelope, with the header.
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(messagesBody("fast", "")))
	r.Header.Set("X-Api-Key", tokens["tok"])
	mw := httptest.NewRecorder()
	f.data.ServeHTTP(mw, r)
	if got := anthropicEnvelope(t, mw.Header(), 429, "rate_limit_exceeded", mw.Body.Bytes()); got != "rate_limit_exceeded: the tpm_limit of this key is reached" ||
		mw.Header().Get("Retry-After") != "61" {
		t.Errorf("messages: %q, Retry-After %q", got, mw.Header().Get("Retry-After"))
	}
	f.now.Add(61_000)
	ok("tok", "fast")
	f.stored(t, 9) // the 9 provider calls; broken called none

	// The key and the alias both refuse: the key answers. Another key's rpm
	// and tpm are its own, not the instance's.
	ok("ra", "capped")
	w = post("ra", "capped")
	limited(t, w.Header(), w.Code, w.Body.Bytes(), "rate_limit_exceeded", "the rpm_limit of this key is reached", "61")
	ok("other", "capped2") // the instance carries no alias's rpm_limit
	ok("rb", "fast")

	// A stream's tokens count when it ends, not when it started: open for
	// longer than the window, they still refuse the next request.
	f.now.Add(61_000)
	held := open(t, s, tokens["tok"], "fast")
	f.now.Add(62_000)
	held.cancel()
	<-held.answer
	waitFlight(t, f)
	w = post("tok", "fast")
	limited(t, w.Header(), w.Code, w.Body.Bytes(), "rate_limit_exceeded", "the tpm_limit of this key is reached", "61")

	// An alias's tpm counts every key's tokens: par's 1 + 2 refuse other.
	ok("par", "tcap")
	w = post("other", "tcap")
	limited(t, w.Header(), w.Code, w.Body.Bytes(), "rate_limit_exceeded", `the tpm_limit of model "tcap" is reached`, "61")
}

// open starts a stream that the mock holds open after its first event and
// waits until the client has it.
func open(t *testing.T, s *streamFixture, token, alias string) *inflight {
	t.Helper()
	s.mock.Script(mockprovider.Step{Chunks: sse(chunk("a")), Hang: true})
	f := send(t, s.url, token, "/v1/chat/completions", streamBody(alias))
	<-f.started
	return f
}

// TestParallelLimits is AC-38 for max_parallel_requests (LIM-5, LIM-6): per
// key, per alias and per instance, a request beyond the slots gets 429
// parallel_limit_exceeded with Retry-After 1 and reaches no provider; a
// stream holds its slot until it ends, a fallback until its last attempt
// ends, and the slot comes back when its client goes away. Limits changed while a stream is open neither keep its
// slot nor release it twice.
func TestParallelLimits(t *testing.T) {
	s, tokens := newAdmission(t)
	refused := func(key, alias, message string) {
		t.Helper()
		a := <-send(t, s.url, tokens[key], "/v1/chat/completions", chatBody(alias, "")).answer
		limited(t, a.header, a.status, []byte(a.body), "parallel_limit_exceeded", message, "1")
	}
	first := open(t, s, tokens["par"], "fast")
	refused("par", "fast", "the parallel limit of this key is reached")
	second := open(t, s, tokens["other"], "single")
	refused("agent", "single", `the parallel limit of model "single" is reached`)
	refused("agent", "fast", "the parallel limit of the instance is reached")
	if n := len(s.mock.Calls()); n != 2 {
		t.Errorf("the mock got %d calls, want 2", n)
	}
	second.cancel()
	<-second.answer

	// par's limit rises while its stream is open, then falls back: the open
	// stream's slot counts throughout and comes back once.
	raised := strings.Replace(admissionConfig, "par: { models: [\"*\"], max_parallel_requests: 1 }", "par: { models: [\"*\"], max_parallel_requests: 2 }", 1)
	s.apply(t, strings.ReplaceAll(raised, "base_url: MOCK", "base_url: "+s.mockURL))
	waitIdle := func() {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			if a := <-send(t, s.url, tokens["par"], "/v1/chat/completions", chatBody("fast", "")).answer; a.status == http.StatusOK {
				return
			} else if time.Now().After(deadline) {
				t.Fatalf("par is still refused: %d %s", a.status, a.body)
			}
		}
	}
	waitIdle() // a second slot of two
	s.apply(t, strings.ReplaceAll(admissionConfig, "base_url: MOCK", "base_url: "+s.mockURL))
	refused("par", "fast", "the parallel limit of this key is reached")
	first.cancel()
	<-first.answer
	waitFlight(t, s.fixture)
	waitIdle()
	// Released once: a new stream takes the only slot again.
	third := open(t, s, tokens["par"], "fast")
	refused("par", "fast", "the parallel limit of this key is reached")
	third.cancel()
	<-third.answer
	waitFlight(t, s.fixture)

	// A fallback holds the slot from its first attempt to the end of its
	// last: pay/m answers 500, pay/n streams.
	s.mock.Script(mockprovider.Step{Status: 500})
	fourth := open(t, s, tokens["par"], "fb")
	refused("par", "fast", "the parallel limit of this key is reached")
	fourth.cancel()
	<-fourth.answer
	waitFlight(t, s.fixture)
}

// panicWriter is a client whose write panics, so the handler unwinds from
// inside the call.
type panicWriter struct{ *httptest.ResponseRecorder }

func (panicWriter) Write([]byte) (int, error) { panic("write") }

// TestSlotReleasedOnPanic is LIM-6 on a panic: the request's finish and done
// still run, its record is stored and its slot comes back.
func TestSlotReleasedOnPanic(t *testing.T) {
	s, tokens := newAdmission(t)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("fast", "")))
	r.Header.Set("Authorization", "Bearer "+tokens["par"])
	func() {
		defer func() {
			if recover() == nil {
				t.Error("no panic")
			}
		}()
		s.data.ServeHTTP(panicWriter{httptest.NewRecorder()}, r)
	}()
	waitFlight(t, s.fixture)
	s.stored(t, 1)
	if w := s.chat("Bearer "+tokens["par"], chatBody("fast", "")); w.Code != http.StatusOK {
		t.Errorf("after the panic: %d %s", w.Code, w.Body)
	}
}
