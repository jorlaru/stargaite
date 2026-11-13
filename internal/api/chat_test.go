package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// apply applies a YAML configuration and returns the tokens it issued.
func (f *fixture) apply(t *testing.T, yaml string) map[string]string {
	t.Helper()
	w := call(f.admin, "Bearer "+adminKey, http.MethodPost, "/admin/v1/apply", []byte(yaml))
	var res config.Result
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &res) != nil {
		t.Fatalf("apply: %d %s", w.Code, w.Body)
	}
	return res.Secrets
}

// chat sends a chat completion with a client token and the headers a client
// may send, none of which may reach the provider (PAR-14).
func (f *fixture) chat(authorization, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	r.Header.Set("X-Custom", "client-header")
	r.Header.Set("Traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	r.Header.Set("X-Stargaite-Target", "forged")
	w := httptest.NewRecorder()
	f.data.ServeHTTP(w, r)
	return w
}

func chatBody(alias, extra string) string {
	return `{"model":"` + alias + `","messages":[{"role":"user","content":"hi"}]` + extra + `}`
}

const mockReply = `{"id":"chatcmpl-mock","object":"chat.completion","created":0,"model":"mock",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"mock reply"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`

const chatConfig = `version: 1
providers:
  mock: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: none, allow_private_network: true }
  off: { protocol: openai, base_url: "http://127.0.0.1:1", api_key_env: MISSING_KEY, billing: none, allow_private_network: true }
  anth: { protocol: anthropic, base_url: "http://127.0.0.1:1", api_key_env: ANTH_KEY, billing: none, allow_private_network: true }
targets:
  mock/up-1: { kind: chat, reasoning: { dialect: reasoning_effort, levels: [low, medium, high], can_disable: true } }
  mock/up-2: { kind: chat }
  mock/up-3: { kind: chat, capabilities: { tools: false } }
  off/m: { kind: chat }
  anth/m: { kind: chat }
models:
  fast: { targets: [mock/up-1], reasoning: { effort: high, mode: default }, max_output_tokens: 500 }
  forced: { targets: [mock/up-1], reasoning: { effort: high, mode: forced } }
  plain: { targets: [mock/up-2] }
  rules: { targets: [mock/up-2], params: { enable_search: forward, x_drop: drop } }
  notools: { targets: [mock/up-3] }
  mixed: { targets: [mock/up-3, mock/up-2] }
  anth: { targets: [anth/m], max_output_tokens: 100 }
  off: { targets: [off/m] }
keys:
  agent: { models: [fast, forced, plain, rules, notools, mixed, anth, off] }
  other: { models: [plain] }
  none: { models: [] }
`

// TestChatHeaders runs chat completions through the data plane against the
// mock provider: the PAR-9 header matrix (AC-12), the refusals in the order
// of the steps with no provider called (AC-06, AC-08, AC-11), the four
// combinations of client effort and alias mode (AC-14), the alias's cap
// (AC-10), the static selection of the next target (AC-11), the provider's
// errors as section 15 gives them, and the exact bytes the provider got.
func TestChatHeaders(t *testing.T) {
	f := newFixture(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	tokens := f.apply(t, strings.Replace(chatConfig, "MOCK", srv.URL, 1))
	tools := `,"tools":[{"type":"function","function":{"name":"f"}}]`
	echo := []byte(`{"error":{"message":"bad key ` + planted + `MOCK_KEY"}}`)

	for _, tc := range []struct {
		name, key, body string
		step            *mockprovider.Step
		status          int
		code, param     string
		target          string // PAR-9 values; "" for absent
		effort, changed string
		upstream        string // the body the provider got; "" when it was not called
		message         string // a part of the error message, when it matters
	}{
		{name: "no key", body: chatBody("plain", ""), status: 401, code: "invalid_api_key"},
		{name: "the admin key", key: "admin", body: chatBody("plain", ""), status: 401, code: "invalid_api_key"},
		{name: "a key with no alias", key: "none", body: chatBody("plain", ""), status: 403, code: "model_not_allowed"},
		{name: "an alias the key does not list", key: "other", body: chatBody("fast", ""), status: 403, code: "model_not_allowed"},
		{name: "unknown alias", key: "agent", body: chatBody("nope", ""), status: 404, code: "model_not_found"},
		{name: "unknown alias of 100 KB", key: "agent", body: chatBody(strings.Repeat("x", 100<<10), ""), status: 404, code: "model_not_found"},
		{name: "no model", key: "agent", body: `{"messages":[{"role":"user","content":"hi"}]}`, status: 400, code: "invalid_request", param: "model"},
		{name: "model not a string", key: "agent", body: `{"model":1,"messages":[]}`, status: 400, code: "invalid_request", param: "model"},
		{name: "not JSON", key: "agent", body: `{"model":`, status: 400, code: "invalid_request"},
		{name: "repeated key", key: "agent", body: `{"model":"plain","model":"plain","messages":[]}`, status: 400, code: "invalid_request", param: "model"},
		{name: "unknown parameter", key: "agent", body: chatBody("plain", `,"enable_search":true`), status: 400, code: "unknown_parameter", param: "enable_search"},
		{name: "effort off the scale", key: "agent", body: chatBody("fast", `,"reasoning_effort":"default"`), status: 400, code: "invalid_reasoning_effort", param: "reasoning_effort"},
		{name: "cap above the alias's", key: "agent", body: chatBody("fast", `,"max_tokens":501`), status: 422, code: "max_output_tokens_exceeded", param: "max_tokens"},
		{name: "no target serves the endpoint", key: "agent", body: chatBody("anth", ""), status: 422, code: "endpoint_not_supported"},
		{name: "no target takes tools", key: "agent", body: chatBody("notools", tools), status: 422, code: "capability_mismatch", param: "tools"},
		{name: "credential unresolved", key: "agent", body: chatBody("off", ""), status: 503, code: "no_target_available"},

		{name: "alias effort and cap added", key: "agent", body: chatBody("fast", ""), status: 200, target: "mock/up-1", effort: "high",
			changed: "max_tokens:added,reasoning_effort:added", upstream: chatBody("up-1", `,"max_tokens":500,"reasoning_effort":"high"`)},
		{name: "client effort in default mode", key: "agent", body: chatBody("fast", `,"reasoning_effort":"low","max_tokens":20`), status: 200, target: "mock/up-1",
			effort: "low", upstream: chatBody("up-1", `,"reasoning_effort":"low","max_tokens":20`)},
		{name: "alias effort in forced mode", key: "agent", body: chatBody("forced", ""), status: 200, target: "mock/up-1", effort: "high",
			changed: "reasoning_effort:added", upstream: chatBody("up-1", `,"reasoning_effort":"high"`)},
		{name: "client effort replaced in forced mode", key: "agent", body: chatBody("forced", `,"reasoning_effort":"low"`), status: 200, target: "mock/up-1",
			effort: "high", changed: "reasoning_effort:forced", upstream: chatBody("up-1", `,"reasoning_effort":"high"`)},
		{name: "nothing changed", key: "agent", body: chatBody("plain", ""), status: 200, target: "mock/up-2", upstream: chatBody("up-2", "")},
		{name: "forward and drop rules", key: "agent", body: chatBody("rules", `,"enable_search":true,"x_drop":1`), status: 200, target: "mock/up-2",
			changed: "enable_search:forwarded,x_drop:dropped", upstream: chatBody("up-2", `,"enable_search":true`)},
		{name: "the next target takes tools", key: "agent", body: chatBody("mixed", tools), status: 200, target: "mock/up-2", upstream: chatBody("up-2", tools)},

		{name: "provider 400", key: "agent", body: chatBody("plain", ""), step: &mockprovider.Step{Status: 400, Body: []byte(`{"error":{"message":"bad thing"}}`)},
			status: 400, code: "upstream_error", target: "mock/up-2", upstream: chatBody("up-2", ""), message: "bad thing"},
		{name: "provider 404", key: "agent", body: chatBody("plain", ""), step: &mockprovider.Step{Status: 404}, status: 404, code: "upstream_error",
			target: "mock/up-2", upstream: chatBody("up-2", ""), message: "the provider answered 404"},
		{name: "provider 422", key: "agent", body: chatBody("plain", ""), step: &mockprovider.Step{Status: 422, Body: []byte(`{"error":{"message":"too long"}}`)},
			status: 422, code: "upstream_error", target: "mock/up-2", upstream: chatBody("up-2", ""), message: "too long"},
		{name: "provider 429 naming the account", key: "agent", body: chatBody("plain", ""), step: &mockprovider.Step{Status: 429, Header: http.Header{"Retry-After": {"3"}},
			Body: []byte(`{"error":{"message":"Rate limit reached in organization org-operator: Limit 30000"}}`)},
			status: 429, code: "upstream_error", target: "mock/up-2", upstream: chatBody("up-2", ""), message: "the provider answered 429"},
		{name: "provider 401 echoing the credential", key: "agent", body: chatBody("plain", ""), step: &mockprovider.Step{Status: 401, Body: echo},
			status: 502, code: "upstream_auth_error", target: "mock/up-2", upstream: chatBody("up-2", ""), message: "the provider refused the operator's credential"},
		{name: "provider 403", key: "agent", body: chatBody("plain", ""), step: &mockprovider.Step{Status: 403, Body: echo},
			status: 502, code: "upstream_auth_error", target: "mock/up-2", upstream: chatBody("up-2", "")},
		{name: "provider 500", key: "agent", body: chatBody("plain", ""), step: &mockprovider.Step{Status: 500, Body: echo}, status: 502, code: "upstream_error",
			target: "mock/up-2", upstream: chatBody("up-2", ""), message: "the provider answered 500"},
		{name: "provider redirect", key: "agent", body: chatBody("plain", ""), step: &mockprovider.Step{Status: 302, Header: http.Header{"Location": {srv.URL + "/v1/messages"}}},
			status: 502, code: "upstream_error", target: "mock/up-2", upstream: chatBody("up-2", "")},
		{name: "provider 200 that is not JSON", key: "agent", body: chatBody("plain", ""), step: &mockprovider.Step{Body: []byte("oops")},
			status: 502, code: "upstream_error", target: "mock/up-2", upstream: chatBody("up-2", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.step != nil {
				mock.Script(*tc.step)
			}
			before := len(mock.Calls())
			auth := ""
			switch tc.key {
			case "":
			case "admin":
				auth = "Bearer " + adminKey
			default:
				auth = "Bearer " + tokens[tc.key]
			}
			w := f.chat(auth, tc.body)
			h := w.Header()
			if w.Code != tc.status || h.Get(domain.RequestIDHeader) == "" || h.Get("Content-Type") != "application/json" {
				t.Fatalf("status %d, headers %v: %s", w.Code, h, w.Body)
			}
			if trace := h.Get(traceHeader); (tc.status != 401) != regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(trace) {
				t.Errorf("trace id %q", trace)
			}
			if h.Get(targetHeader) != tc.target || h.Get(effortHeader) != tc.effort || h.Get(changedHeader) != tc.changed {
				t.Errorf("target %q effort %q changed %q", h.Get(targetHeader), h.Get(effortHeader), h.Get(changedHeader))
			}
			if want := map[bool]string{true: "3"}[tc.status == 429]; h.Get("Retry-After") != want {
				t.Errorf("Retry-After %q", h.Get("Retry-After"))
			}
			for name, v := range h {
				if strings.Contains(name+strings.Join(v, ""), planted) {
					t.Errorf("the credential reached the client in %s", name)
				}
			}
			if strings.Contains(w.Body.String(), planted) {
				t.Error("the credential reached the client")
			}
			if tc.status == 200 {
				if w.Body.String() != mockReply {
					t.Errorf("body %s", w.Body)
				}
			} else {
				var env struct{ Error domain.Error }
				want := domain.NewError(tc.code, "")
				if tc.code == "upstream_error" {
					want = domain.UpstreamError(tc.status, "", "")
				}
				if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Error.Code != tc.code || env.Error.Type != want.Type ||
					env.Error.Param != tc.param || !strings.Contains(env.Error.Message, tc.message) {
					t.Errorf("envelope %s", w.Body)
				}
			}
			calls := mock.Calls()[before:]
			if tc.upstream == "" {
				if len(calls) != 0 {
					t.Errorf("the provider was called: %+v", calls)
				}
				return
			}
			if len(calls) != 1 {
				t.Fatalf("%d calls", len(calls))
			}
			c := calls[0]
			if c.Method != http.MethodPost || c.Path != "/chat/completions" || string(c.Body) != tc.upstream {
				t.Errorf("call %s %s %s", c.Method, c.Path, c.Body)
			}
			if c.Header.Get("Authorization") != "Bearer "+planted+"MOCK_KEY" || c.Header.Get("X-Custom") != "" || c.Header.Get("Traceparent") != "" ||
				c.Header.Get("X-Stargaite-Target") != "" || c.Header.Get("Content-Type") != "application/json" {
				t.Errorf("provider headers %v", c.Header)
			}
		})
	}

	// OPS-9: JSON lines with the request id, never a credential or a token;
	// one attempt line per call, one request line per authenticated request.
	log := f.log.String()
	for _, secret := range []string{planted, tokens["agent"], tokens["other"], tokens["none"], adminKey} {
		if strings.Contains(log, secret) {
			t.Errorf("the log holds %q", secret)
		}
	}
	var attempts, requests, refused int
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		var l struct{ Msg, RequestID, TraceID, Key, Message string }
		if err := json.Unmarshal([]byte(strings.ReplaceAll(line, "_id", "ID")), &l); err != nil || l.RequestID == "" || len(line) > 2000 {
			t.Errorf("log line %s", line)
		}
		switch l.Msg {
		case "attempt":
			attempts++
		case "request":
			requests++
		case "provider refused the credential":
			refused++
			if !strings.Contains(l.Message, "bad key [redacted]") {
				t.Errorf("refusal line %s", line)
			}
		}
	}
	if calls := len(mock.Calls()); attempts != calls || requests != 30 || refused != 1 {
		t.Errorf("%d attempt lines for %d calls, %d request lines, %d refusal lines", attempts, calls, requests, refused)
	}
	if !strings.Contains(log, `"msg":"attempt"`) || !strings.Contains(log, `"usage":{"input":1,"cache_read":0,"cache_write":0,"output":2}`) ||
		!strings.Contains(log, `"response_model":"mock","finish_reason":"stop"`) || !strings.Contains(log, `"alias":"fast","attempts":1,"status":200`) {
		t.Errorf("log %s", log)
	}
}

// TestChatLimits: a body over max_body_bytes is 413 before anything else is
// read (LIM-6), and a 2xx over max_response_bytes is 502 (RES-4).
func TestChatLimits(t *testing.T) {
	f := newFixture(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	tokens := f.apply(t, strings.Replace(`version: 1
settings: { max_body_bytes: 300, max_response_bytes: 100 }
providers:
  mock: { protocol: openai, base_url: MOCK, billing: none, allow_private_network: true }
models:
  plain: { targets: [mock/up] }
targets:
  mock/up: { kind: chat }
keys:
  agent: { models: [plain] }
`, "MOCK", srv.URL, 1))
	auth := "Bearer " + tokens["agent"]
	w := f.chat(auth, chatBody("plain", `,"user":"`+strings.Repeat("x", 300)+`"`))
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), `"code":"request_too_large"`) || len(mock.Calls()) != 0 {
		t.Errorf("large body: %d %s", w.Code, w.Body)
	}
	w = f.chat(auth, chatBody("plain", ""))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"code":"upstream_error"`) || w.Header().Get(targetHeader) != "mock/up" {
		t.Errorf("large response: %d %s", w.Code, w.Body)
	}
	// A provider without a credential gets no Authorization header.
	if calls := mock.Calls(); len(calls) != 1 || calls[0].Header.Values("Authorization") != nil {
		t.Errorf("calls %+v", calls)
	}
}

// TestChatTimers: the connect timer covers the TLS handshake and the total
// timer the whole call (RES-3), each answered 504 upstream_timeout; a refused
// dial and a body cut while it is read are 502 upstream_error. Durations are
// milliseconds.
func TestChatTimers(t *testing.T) {
	f := newFixture(t)
	// A peer that accepts and never answers the TLS handshake.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }() // when the listener closes
		}
	}()
	released := make(chan struct{}, 1)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the server notices a closed connection only once the body is read
		<-r.Context().Done()
		released <- struct{}{}
	}))
	t.Cleanup(slow.Close)
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte(`{"choices":[`))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(cut.Close)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := closed.Addr().String()
	_ = closed.Close()

	tokens := f.apply(t, `version: 1
settings: { timeouts: { connect_ms: 100, total_ms: 400 } }
providers:
  silent: { protocol: openai, base_url: "https://`+silent.Addr().String()+`", billing: none, allow_private_network: true }
  slow: { protocol: openai, base_url: "`+slow.URL+`", billing: none, allow_private_network: true }
  cut: { protocol: openai, base_url: "`+cut.URL+`", billing: none, allow_private_network: true }
  refused: { protocol: openai, base_url: "http://`+refused+`", billing: none, allow_private_network: true }
targets:
  silent/m: { kind: chat }
  slow/m: { kind: chat }
  cut/m: { kind: chat }
  refused/m: { kind: chat }
models:
  silent: { targets: [silent/m] }
  slow: { targets: [slow/m] }
  cut: { targets: [cut/m] }
  refused: { targets: [refused/m] }
keys:
  agent: { models: ["*"] }
`)
	for _, tc := range []struct {
		alias, code, message string
		within               time.Duration
	}{
		{"silent", "upstream_timeout", "connect timeout", 350 * time.Millisecond},
		{"slow", "upstream_timeout", "total timeout", 2 * time.Second},
		{"cut", "upstream_error", "the provider call failed", 2 * time.Second},
		{"refused", "upstream_error", "the provider call failed", 2 * time.Second},
	} {
		start := time.Now()
		w := f.chat("Bearer "+tokens["agent"], chatBody(tc.alias, ""))
		var env struct{ Error domain.Error }
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Error.Code != tc.code || w.Code != domain.NewError(tc.code, "").Status ||
			!strings.Contains(env.Error.Message, tc.message) || w.Header().Get(targetHeader) != tc.alias+"/m" {
			t.Errorf("%s: %d %s", tc.alias, w.Code, w.Body)
		}
		if d := time.Since(start); d > tc.within || tc.alias == "slow" && d < 400*time.Millisecond {
			t.Errorf("%s: answered after %v", tc.alias, d)
		}
	}
	select {
	case <-released: // the total timer cancelled the provider's request
	case <-time.After(2 * time.Second):
		t.Error("the slow provider's request was not cancelled")
	}
	if log := f.log.String(); !strings.Contains(log, `"target":"silent/m","status":0`) || !strings.Contains(log, `"cause":"connect timeout"`) ||
		!strings.Contains(log, `"cause":"total timeout"`) {
		t.Errorf("log %s", log)
	}
}

// TestSDKRequests is AC-01 for synchronous requests: request bytes recorded
// from the OpenAI Python SDK 3.50.0 and Node SDK 7.56.0 (testdata/*.http, with
// their headers) reach the provider with only the alias's edits, and the
// answer holds every field the SDKs' ChatCompletion requires.
func TestSDKRequests(t *testing.T) {
	f := newFixture(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	tokens := f.apply(t, strings.Replace(chatConfig, "MOCK", srv.URL, 1))
	for _, name := range []string{"python-sync", "python-sync-tools", "node-sync", "node-sync-tools"} {
		raw, err := os.ReadFile("testdata/openai-" + name + ".http")
		if err != nil {
			t.Fatal(err)
		}
		recorded, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(raw)))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(recorded.Body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(recorded.Method, recorded.URL.Path, bytes.NewReader(body))
		r.Header = recorded.Header.Clone()
		r.Header.Set("Authorization", "Bearer "+tokens["agent"])
		w := httptest.NewRecorder()
		f.data.ServeHTTP(w, r)

		var got struct {
			ID, Object, Model *string
			Created           *int64
			Choices           []struct {
				Index        *int
				FinishReason *string `json:"finish_reason"`
				Message      *struct{ Role *string }
			}
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || got.ID == nil || got.Object == nil || *got.Object != "chat.completion" ||
			got.Model == nil || got.Created == nil || len(got.Choices) == 0 || got.Choices[0].Index == nil || got.Choices[0].FinishReason == nil ||
			got.Choices[0].Message == nil || got.Choices[0].Message.Role == nil {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
		want := strings.Replace(strings.TrimSuffix(string(body), "}"), `"model":"fast"`, `"model":"up-1"`, 1) + `,"max_tokens":500,"reasoning_effort":"high"}`
		if calls := mock.Calls(); len(calls) == 0 || string(calls[len(calls)-1].Body) != want || calls[len(calls)-1].Header.Get("X-Stainless-Lang") != "" {
			t.Errorf("%s: the provider got %+v", name, calls)
		}
	}
}
