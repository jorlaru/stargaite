package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// messagesConfig has an anthropic target that strips think tags and reasons
// with adaptive_effort, one without tools, and an openai target, which
// /v1/messages never serves. Stream timers are milliseconds.
const messagesConfig = `version: 1
settings: { timeouts: { first_byte_ms: 300, stream_idle_ms: 300 } }
providers:
  mock: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: none, allow_private_network: true }
  anth: { protocol: anthropic, base_url: MOCK, api_key_env: ANTH_KEY, billing: none, allow_private_network: true }
targets:
  anth/claude-up: { kind: chat, inline_think_tags: true, reasoning: { dialect: adaptive_effort, levels: [low, medium, high], can_disable: true } }
  anth/plain: { kind: chat, capabilities: { tools: false } }
  mock/up-2: { kind: chat }
models:
  claude: { targets: [anth/claude-up], reasoning: { effort: high, mode: default }, max_output_tokens: 500 }
  plain: { targets: [anth/plain], max_output_tokens: 300 }
  both: { targets: [mock/up-2, anth/plain], max_output_tokens: 300 }
  openai: { targets: [mock/up-2] }
keys:
  agent: { models: [claude, plain, both, openai] }
  other: { models: [openai] }
`

// messagesReply is the mock's default Messages answer.
const messagesReply = `{"id":"msg_mock","type":"message","role":"assistant","model":"mock",` +
	`"content":[{"type":"text","text":"mock reply"}],"stop_reason":"end_turn","stop_sequence":null,` +
	`"usage":{"input_tokens":1,"output_tokens":2}}`

func messagesBody(alias, extra string) string {
	return `{"model":"` + alias + `","max_tokens":100,"messages":[{"role":"user","content":"hi"}]` + extra + `}`
}

// messages sends a Messages request with the headers given, as name and
// value pairs, plus headers a client may send that must not reach the
// provider (PAR-14).
func (f *fixture) messages(body string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	for i := 0; i < len(header); i += 2 {
		r.Header.Add(header[i], header[i+1])
	}
	r.Header.Set("X-Custom", "client-header")
	r.Header.Set("Traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	w := httptest.NewRecorder()
	f.data.ServeHTTP(w, r)
	return w
}

// anthropicEnvelope checks that body is exactly the Anthropic error envelope
// of kick-off section 15 for code, of the type the error model gives the
// status, and returns its message.
func anthropicEnvelope(t *testing.T, h http.Header, status int, code string, body []byte) string {
	t.Helper()
	var env struct {
		Type  string
		Error map[string]string
	}
	want := domain.NewError(code, "")
	if code == domain.CodeUpstreamError {
		want = domain.UpstreamError(status, "", "")
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Type != "error" || len(env.Error) != 2 || env.Error["type"] != want.Type ||
		!strings.HasPrefix(env.Error["message"], code+": ") || h.Get(errorCodeHeader) != code || h.Get("Content-Type") != "application/json" ||
		!bytes.HasPrefix(body, []byte(`{"type":"error","error":{"type":`)) {
		t.Errorf("envelope %s, headers %v", body, h)
	}
	return env.Error["message"]
}

// TestMessages runs /v1/messages through the data plane against the mock
// provider: the client key in x-api-key or Authorization, Authorization
// winning when both are sent; every refusal in the Anthropic envelope with
// its code in x-stargaite-error-code and no provider called (AC-06 for
// messages); an openai target skipped; RSN-1 and the dialect on the body;
// PAR-14, the client's anthropic-version and anthropic-beta forwarded, the
// pinned version when it sends none, and no other client header; the
// answer relayed byte for byte, but for think tags in text blocks; the
// provider's errors as section 15 gives them.
func TestMessages(t *testing.T) {
	f := newFixture(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	tokens := f.apply(t, strings.ReplaceAll(messagesConfig, "base_url: MOCK", "base_url: "+srv.URL))
	agent, other := tokens["agent"], tokens["other"]
	tools := `,"tools":[{"name":"f","input_schema":{"type":"object"}}]`
	thinking := `{"type":"thinking","thinking":"I should <think> answer.","signature":"c2ln\u0000"}`
	reply := `{"id":"msg_01","type":"message","role":"assistant","model":"claude-up-2026","content":[` + thinking +
		`,{"type":"text","text":"<think>plan</think>Hi \u00e9"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`
	textReply := func(text string) string {
		return `{"id":"msg_02","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"` + text +
			`"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`
	}
	escaped, tagged := textReply(`Par\u00eds \/`), textReply("<think>x</think>y")

	for _, tc := range []struct {
		name    string
		header  []string // name and value pairs
		body    string
		step    *mockprovider.Step
		status  int
		code    string
		target  string // the PAR-9 target; "" for absent
		effort  string
		changed string
		version string // the anthropic-version the provider got
		beta    []string
		sent    string // the body the provider got; "" when it was not called
		answer  string // the body the client got on a 200; "" for the mock's default
		message string // a part of the error message, when it matters
	}{
		{name: "no key", body: messagesBody("plain", ""), status: 401, code: "invalid_api_key"},
		{name: "a wrong x-api-key", header: []string{"X-Api-Key", "sk-stargaite-wrong"}, body: messagesBody("plain", ""), status: 401, code: "invalid_api_key"},
		{name: "the admin key in x-api-key", header: []string{"X-Api-Key", adminKey}, body: messagesBody("plain", ""), status: 401, code: "invalid_api_key"},
		{name: "a wrong Authorization wins over a valid x-api-key", header: []string{"Authorization", "Bearer sk-stargaite-wrong", "X-Api-Key", agent},
			body: messagesBody("plain", ""), status: 401, code: "invalid_api_key"},
		{name: "x-api-key for an alias the key does not list", header: []string{"X-Api-Key", other}, body: messagesBody("plain", ""), status: 403, code: "model_not_allowed"},
		{name: "Authorization for an alias the key does not list", header: []string{"Authorization", "Bearer " + other}, body: messagesBody("plain", ""),
			status: 403, code: "model_not_allowed"},
		{name: "unknown alias", header: []string{"X-Api-Key", agent}, body: messagesBody("nope", ""), status: 404, code: "model_not_found"},
		{name: "a chat parameter", header: []string{"X-Api-Key", agent}, body: messagesBody("plain", `,"reasoning_effort":"low"`), status: 400, code: "unknown_parameter"},
		{name: "effort off the scale", header: []string{"X-Api-Key", agent}, body: messagesBody("claude", `,"output_config":{"effort":"default"}`),
			status: 400, code: "invalid_reasoning_effort"},
		{name: "thinking not an object", header: []string{"X-Api-Key", agent}, body: messagesBody("claude", `,"thinking":true`), status: 400, code: "invalid_request"},
		{name: "repeated key", header: []string{"X-Api-Key", agent}, body: messagesBody("claude", `,"thinking":{"type":"adaptive","type":"disabled"}`),
			status: 400, code: "invalid_request"},
		{name: "cap above the alias's", header: []string{"X-Api-Key", agent}, body: `{"model":"claude","max_tokens":501,"messages":[{"role":"user","content":"hi"}]}`,
			status: 422, code: "max_output_tokens_exceeded"},
		{name: "only an openai target", header: []string{"X-Api-Key", agent}, body: messagesBody("openai", ""), status: 422, code: "endpoint_not_supported"},
		{name: "no target takes tools", header: []string{"X-Api-Key", agent}, body: messagesBody("plain", tools), status: 422, code: "capability_mismatch"},
		{name: "a budget toward adaptive_effort", header: []string{"X-Api-Key", agent}, body: messagesBody("claude", `,"thinking":{"type":"enabled","budget_tokens":2048}`),
			status: 422, code: "capability_mismatch"},

		{name: "x-api-key, the pinned version", header: []string{"X-Api-Key", agent}, body: messagesBody("plain", ""), status: 200, target: "anth/plain",
			version: "2023-06-01", sent: messagesBody("plain", "")},
		{name: "a valid Authorization wins over a wrong x-api-key, the client's version and betas", header: []string{"Authorization", "Bearer " + agent,
			"X-Api-Key", "sk-stargaite-wrong", "Anthropic-Version", "2026-10-21", "Anthropic-Beta", "b1,b2", "Anthropic-Beta", "b3"},
			body: messagesBody("plain", ""), status: 200, target: "anth/plain", version: "2026-10-21", beta: []string{"b1,b2", "b3"}, sent: messagesBody("plain", "")},
		{name: "the openai target is skipped", header: []string{"X-Api-Key", agent}, body: messagesBody("both", ""), status: 200, target: "anth/plain",
			version: "2023-06-01", sent: messagesBody("plain", "")},
		{name: "the alias effort and cap", header: []string{"X-Api-Key", agent}, body: `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`,
			status: 200, target: "anth/claude-up", effort: "high", changed: "max_tokens:added,output_config.effort:added", version: "2023-06-01",
			sent: `{"model":"claude-up","messages":[{"role":"user","content":"hi"}],"max_tokens":500,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`},
		{name: "the client's thinking kept, display included (RSN-1)", header: []string{"X-Api-Key", agent},
			body:   messagesBody("claude", `,"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"format":{"type":"json_schema","schema":{}}}`),
			status: 200, target: "anth/claude-up", effort: "high", changed: "output_config.effort:added", version: "2023-06-01",
			sent: messagesBody("claude-up", `,"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"format":{"type":"json_schema","schema":{}},"effort":"high"}`)},
		{name: "a client effort above none keeps its thinking (RSN-1)", header: []string{"X-Api-Key", agent},
			body: messagesBody("claude", `,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}`), status: 200, target: "anth/claude-up", effort: "low",
			version: "2023-06-01", sent: messagesBody("claude-up", `,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}`)},
		{name: "effort none writes the off form (RSN-1)", header: []string{"X-Api-Key", agent},
			body: messagesBody("claude", `,"thinking":{"type":"adaptive"},"output_config":{"effort":"none"}`), status: 200, target: "anth/claude-up", effort: "none",
			changed: "output_config.effort:translated,thinking:translated", version: "2023-06-01", sent: messagesBody("claude-up", `,"thinking":{"type":"disabled"}`)},
		{name: "think tags stripped from text blocks, thinking untouched", header: []string{"X-Api-Key", agent}, body: messagesBody("claude", ""),
			step: &mockprovider.Step{Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(reply)}, status: 200, target: "anth/claude-up",
			effort: "high", changed: "output_config.effort:added", version: "2023-06-01",
			sent:   messagesBody("claude-up", `,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}`),
			answer: strings.Replace(reply, `"<think>plan</think>Hi \u00e9"`, `"Hi é"`, 1)},
		{name: "a text without think tags keeps its bytes", header: []string{"X-Api-Key", agent}, body: messagesBody("claude", ""),
			step: &mockprovider.Step{Body: []byte(escaped)}, status: 200, target: "anth/claude-up", effort: "high", changed: "output_config.effort:added",
			version: "2023-06-01", sent: messagesBody("claude-up", `,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}`), answer: escaped},
		{name: "think tags kept toward a target without inline_think_tags", header: []string{"X-Api-Key", agent}, body: messagesBody("plain", ""),
			step: &mockprovider.Step{Body: []byte(tagged)}, status: 200, target: "anth/plain", version: "2023-06-01", sent: messagesBody("plain", ""), answer: tagged},

		{name: "provider 400 keeps its message after the code", header: []string{"X-Api-Key", agent}, body: messagesBody("plain", ""),
			step:   &mockprovider.Step{Status: 400, Body: []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: too large"}}`)},
			status: 400, code: "upstream_error", target: "anth/plain", version: "2023-06-01", sent: messagesBody("plain", ""), message: "upstream_error: max_tokens: too large"},
		{name: "provider 429", header: []string{"X-Api-Key", agent}, body: messagesBody("plain", ""),
			step:   &mockprovider.Step{Status: 429, Header: http.Header{"Retry-After": {"3"}}, Body: []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"org-x"}}`)},
			status: 429, code: "upstream_error", target: "anth/plain", version: "2023-06-01", sent: messagesBody("plain", ""), message: "the provider answered 429"},
		{name: "provider 401", header: []string{"X-Api-Key", agent}, body: messagesBody("plain", ""),
			step:   &mockprovider.Step{Status: 401, Body: []byte(`{"type":"error","error":{"type":"authentication_error","message":"bad ` + planted + `ANTH_KEY"}}`)},
			status: 502, code: "upstream_auth_error", target: "anth/plain", version: "2023-06-01", sent: messagesBody("plain", "")},
		{name: "a 200 that is not JSON", header: []string{"X-Api-Key", agent}, body: messagesBody("plain", ""), step: &mockprovider.Step{Body: []byte("oops")},
			status: 502, code: "upstream_error", target: "anth/plain", version: "2023-06-01", sent: messagesBody("plain", ""), message: "not usable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.step != nil {
				mock.Script(*tc.step)
			}
			before := len(mock.Calls())
			w := f.messages(tc.body, tc.header...)
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
			if strings.Contains(w.Body.String(), planted) || strings.Contains(w.Body.String(), agent) {
				t.Error("a credential reached the client")
			}
			switch {
			case tc.status != 200:
				if m := anthropicEnvelope(t, h, tc.status, tc.code, w.Body.Bytes()); !strings.Contains(m, tc.message) {
					t.Errorf("message %q", m)
				}
			case w.Body.String() != or(tc.answer, messagesReply) || h.Get(errorCodeHeader) != "":
				t.Errorf("body %s", w.Body)
			}
			calls := mock.Calls()[before:]
			if tc.sent == "" {
				if len(calls) != 0 {
					t.Errorf("the provider was called: %+v", calls)
				}
				return
			}
			if len(calls) != 1 {
				t.Fatalf("%d calls", len(calls))
			}
			c := calls[0]
			if c.Path != "/v1/messages" || string(c.Body) != tc.sent {
				t.Errorf("call %s %s", c.Path, c.Body)
			}
			want := http.Header{"X-Api-Key": {planted + "ANTH_KEY"}, "Anthropic-Version": {tc.version}, "Content-Type": {"application/json"},
				"Content-Length": {strconv.Itoa(len(tc.sent))}, "User-Agent": {"Go-http-client/1.1"}, "Accept-Encoding": {"gzip"}}
			if tc.beta != nil {
				want["Anthropic-Beta"] = tc.beta
			}
			if !maps.EqualFunc(c.Header, want, slices.Equal) {
				t.Errorf("provider headers %v", c.Header)
			}
		})
	}
	if log := f.log.String(); strings.Contains(log, planted) || strings.Contains(log, agent) ||
		!strings.Contains(log, `"response_model":"claude-up-2026","finish_reason":"end_turn","usage":{"input":3,"cache_read":0,"cache_write":0,"output":4},`+
			`"visible_bytes":5,"tool_calls":0,"stripped":true`) {
		t.Errorf("log %s", log)
	}
}

// or is s, or otherwise when s is empty.
func or(s, otherwise string) string {
	if s != "" {
		return s
	}
	return otherwise
}

// The thinking tool loop the Anthropic Node SDK was recorded against
// (testdata/anthropic-node-*.http): its answers, with the bytes the SDK
// writes back.
const (
	loopThinking = `{"type":"thinking","thinking":"The user wants the weather in Paris.","signature":"EqQBCkYIBxgCKkBzaWduYXR1cmU="}`
	loopContent  = `[` + loopThinking + `,{"type":"text","text":"Let me check."},{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{"city":"Paris"}}]`
	loopReply    = `{"id":"msg_01","type":"message","role":"assistant","model":"claude-up-2026","content":` + loopContent +
		`,"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":20,"output_tokens":30}}`
)

// loopEvents is loopReply as a Messages stream.
var loopEvents = events(
	"message_start", `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-up-2026","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":20,"output_tokens":1}}}`,
	"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
	"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"The user wants the weather in Paris."}}`,
	"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EqQBCkYIBxgCKkBzaWduYXR1cmU="}}`,
	"content_block_stop", `{"type":"content_block_stop","index":0}`,
	"ping", `{"type":"ping"}`,
	"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
	"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Let me check."}}`,
	"content_block_stop", `{"type":"content_block_stop","index":1}`,
	"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{}}}`,
	"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Paris\"}"}}`,
	"content_block_stop", `{"type":"content_block_stop","index":2}`,
	"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":30}}`,
	"message_stop", `{"type":"message_stop"}`,
)

// sdkRequest is a request recorded from an SDK, its key replaced by token.
func sdkRequest(t *testing.T, name, token string) (*http.Request, string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name + ".http")
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
	if r.Header.Get("X-Api-Key") != "sk-stargaite-recorded" {
		t.Fatalf("%s: recorded with another key", name)
	}
	r.Header.Set("X-Api-Key", token)
	return r, string(body)
}

// TestMessagesSDK is AC-02 with the request bytes recorded from the
// Anthropic Node SDK 0.93.0 (testdata/anthropic-node-*.http, headers
// included): a plain request, and a tool loop with thinking enabled, sync
// and streamed. Each reaches the provider with only the alias's edits, the
// SDK's anthropic-version and anthropic-beta and none of its other headers;
// the answer, a thinking block with its signature and a tool_use, reaches the
// client byte for byte; and the SDK's next turn, which sends that content
// back with a tool_result, reaches the provider with the thinking block and
// its signature byte-identical.
func TestMessagesSDK(t *testing.T) {
	f := newFixture(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	token := f.apply(t, strings.ReplaceAll(messagesConfig, "base_url: MOCK", "base_url: "+srv.URL))["agent"]
	for _, tc := range []struct {
		name   string
		step   *mockprovider.Step
		answer string // the client's bytes; "" for the mock's default
		beta   string
	}{
		{name: "anthropic-node-sync"},
		{name: "anthropic-node-loop-1", step: &mockprovider.Step{Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(loopReply)},
			answer: loopReply, beta: "interleaved-thinking-2025-05-14"},
		{name: "anthropic-node-loop-2", beta: "interleaved-thinking-2025-05-14"},
		{name: "anthropic-node-stream-loop-1", step: &mockprovider.Step{Chunks: loopEvents}, answer: string(bytes.Join(loopEvents, nil)),
			beta: "interleaved-thinking-2025-05-14"},
		{name: "anthropic-node-stream-loop-2", beta: "interleaved-thinking-2025-05-14"},
	} {
		r, body := sdkRequest(t, tc.name, token)
		if tc.step != nil {
			mock.Script(*tc.step)
		}
		before := len(mock.Calls())
		w := httptest.NewRecorder()
		f.data.ServeHTTP(w, r)
		calls := mock.Calls()[before:]

		// The alias's edits: the upstream model, and its effort, which keeps
		// the client's adaptive thinking (RSN-1).
		want := strings.Replace(body, `"model":"claude"`, `"model":"claude-up"`, 1)
		if strings.Contains(body, `"thinking":{"type":"adaptive"}`) {
			want = strings.TrimSuffix(want, "}") + `,"output_config":{"effort":"high"}}`
		} else {
			want = strings.TrimSuffix(want, "}") + `,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`
		}
		if len(calls) != 1 || string(calls[0].Body) != want {
			t.Fatalf("%s: the provider got %+v", tc.name, calls)
		}
		h := calls[0].Header
		if h.Get("X-Api-Key") != planted+"ANTH_KEY" || h.Get("Anthropic-Version") != "2023-06-01" || h.Get("Anthropic-Beta") != tc.beta ||
			h.Get("X-Stainless-Lang") != "" || h.Get("User-Agent") != "Go-http-client/1.1" || h.Get("Accept-Language") != "" {
			t.Errorf("%s: provider headers %v", tc.name, h)
		}
		if strings.HasSuffix(tc.name, "-2") && !strings.Contains(string(calls[0].Body), `{"role":"assistant","content":`+loopContent+`}`) {
			t.Errorf("%s: the thinking block and its signature did not reach the provider byte for byte: %s", tc.name, calls[0].Body)
		}
		stream := strings.Contains(body, `"stream":true`)
		if w.Code != http.StatusOK || tc.answer != "" && w.Body.String() != tc.answer || tc.answer == "" && !stream && w.Body.String() != messagesReply {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body)
		}
		if !stream {
			// The fields the SDK's Message requires.
			var m struct {
				ID, Type, Role, Model *string
				Content               []struct{ Type *string }
				StopReason            *string `json:"stop_reason"`
				Usage                 *struct {
					Input  *int `json:"input_tokens"`
					Output *int `json:"output_tokens"`
				}
			}
			if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil || m.ID == nil || *m.Type != "message" || *m.Role != "assistant" || m.Model == nil ||
				len(m.Content) == 0 || m.Content[0].Type == nil || m.StopReason == nil || m.Usage == nil || m.Usage.Input == nil || m.Usage.Output == nil {
				t.Errorf("%s: %s", tc.name, w.Body)
			}
		}
	}
}

// TestMessagesStream: the native relay re-emits every Messages event under
// its own name, pings included, with its data unchanged but for the think
// tags of a text block, split and escaped across text deltas, which it
// removes, re-encoding only those deltas (RSN-12). The fixture of upstream
// bytes, split at every byte offset and one byte per chunk, gives the same
// client bytes each time; the attempt line keeps the provider's stop_reason,
// the usage of message_start and message_delta, the visible text, the tool
// call and the removal.
func TestMessagesStream(t *testing.T) {
	s := newStreamFixture(t, messagesConfig)
	up, err := os.ReadFile("testdata/anthropic-stream.up.sse")
	if err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile("testdata/messages-stream.down.sse")
	if err != nil {
		t.Fatal(err)
	}
	var bytewise [][]byte
	for i := range up {
		bytewise = append(bytewise, up[i:i+1])
	}
	splits := [][][]byte{bytewise}
	for i := 0; i <= len(up); i++ {
		splits = append(splits, [][]byte{up[:i], up[i:]})
	}
	visible := len("Let me check the weather in París.")
	for _, chunks := range splits {
		s.mock.Script(mockprovider.Step{Chunks: chunks})
		resp, got := s.postTo(t, "/v1/messages", messagesBody("claude", `,"stream":true`))
		log := s.log.String()
		if resp.StatusCode != http.StatusOK || got != string(down) || resp.Header.Get("Content-Type") != "text/event-stream" ||
			resp.Header.Get(targetHeader) != "anth/claude-up" || resp.Header.Get(changedHeader) != "output_config.effort:added" ||
			!strings.Contains(log[strings.LastIndex(log, `"msg":"attempt"`):], `"response_model":"claude-up-2026","finish_reason":"tool_use",`+
				`"usage":{"input":125,"cache_read":100,"cache_write":5,"output":30},"visible_bytes":`+strconv.Itoa(visible)+`,"tool_calls":1,"stripped":true`) {
			t.Fatalf("split %d: %d %v %q; %s", len(chunks[0]), resp.StatusCode, resp.Header, got, log[strings.LastIndex(log, `"msg":"attempt"`):])
		}
	}
	calls := s.mock.Calls()
	if c := calls[len(calls)-1]; c.Path != "/v1/messages" ||
		string(c.Body) != messagesBody("claude-up", `,"stream":true,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}`) {
		t.Errorf("the provider got %s %s", c.Path, c.Body)
	}

	// Each text block has its own stripper, the text of content_block_start
	// included; a text it leaves unchanged keeps its bytes; the held bytes of
	// a block go out under its index, counted as visible. A target without
	// inline_think_tags gets its tags back.
	block := func(i, text string) string {
		return `{"type":"content_block_start","index":` + i + `,"content_block":{"type":"text","text":"` + text + `"}}`
	}
	delta := func(i, text string) string {
		return `{"type":"content_block_delta","index":` + i + `,"delta":{"type":"text_delta","text":"` + text + `"}}`
	}
	stop := func(i string) string { return `{"type":"content_block_stop","index":` + i + `}` }
	end := `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`
	for _, tc := range []struct{ alias, target, up, down, observed string }{
		{alias: "claude", target: "anth/claude-up",
			up: ev("message_start", messageStart, "content_block_start", block("0", "<think>x</think>y"), "content_block_delta", delta("0", `Par\u00eds \/`),
				"content_block_stop", stop("0"), "content_block_start", block("1", ""), "content_block_delta", delta("1", "<think>z</think>w"),
				"content_block_delta", delta("1", "a <"), "content_block_stop", stop("1"), "message_delta", end, "message_stop", messageStop),
			down: ev("message_start", messageStart, "content_block_start", block("0", "y"), "content_block_delta", delta("0", `Par\u00eds \/`),
				"content_block_stop", stop("0"), "content_block_start", block("1", ""), "content_block_delta", delta("1", "w"),
				"content_block_delta", delta("1", "a "), "content_block_delta", delta("1", "<"), "content_block_stop", stop("1"),
				"message_delta", end, "message_stop", messageStop),
			observed: `"visible_bytes":13,"tool_calls":0,"stripped":true`},
		{alias: "plain", target: "anth/plain",
			up: ev("message_start", messageStart, "content_block_start", block("0", "<think>x</think>y"), "content_block_stop", stop("0"),
				"message_delta", end, "message_stop", messageStop),
			observed: `"visible_bytes":17,"tool_calls":0,"stripped":false`},
	} {
		s.mock.Script(mockprovider.Step{Chunks: [][]byte{[]byte(tc.up)}})
		resp, got := s.postTo(t, "/v1/messages", messagesBody(tc.alias, `,"stream":true`))
		log := s.log.String()
		if line := log[strings.LastIndex(log, `"msg":"attempt"`):]; resp.StatusCode != http.StatusOK || got != or(tc.down, tc.up) ||
			resp.Header.Get(targetHeader) != tc.target || !strings.Contains(line, tc.observed) {
			t.Errorf("%s: %d %q; %s", tc.alias, resp.StatusCode, got, line)
		}
	}
}

// ev is the SSE bytes of events given as name and data pairs.
func ev(pairs ...string) string { return string(bytes.Join(events(pairs...), nil)) }

// postTo sends body to path of the data plane with the client key in
// x-api-key and reads the whole answer.
func (s *streamFixture) postTo(t *testing.T, path, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", s.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v after %q", err, got)
	}
	return resp, string(got)
}

// TestMessagesStreamFaults: the commit rule on the native relay. An error
// event before anything reached the client is not relayed: 502
// upstream_error in the Anthropic envelope, its type and documented status
// in the cause. After the commit an upstream error event, already in the
// client's format, is relayed with the credential redacted; any other
// failure ends the stream with the proxy's own error event in the Anthropic
// envelope. A ping is an event: relayed, and it commits. An event named ":"
// is refused, as that name stands for a comment in the relay.
func TestMessagesStreamFaults(t *testing.T) {
	s := newStreamFixture(t, messagesConfig)
	overloaded := `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded ` + planted + `ANTH_KEY"}}`
	start := `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":3,"output_tokens":1}}}`
	own := func(code, message string) string {
		return ev("error", `{"type":"error","error":{"type":"api_error","message":"`+code+": "+message+`"}}`)
	}
	for _, tc := range []struct {
		name    string
		step    mockprovider.Step
		status  int
		message string // the envelope's, before the commit
		body    string // the client's bytes, after the commit
		cause   string
	}{
		{name: "an error event first", step: mockprovider.Step{Chunks: events("error", overloaded)}, status: 502,
			message: "upstream_error: the provider's stream reported an error", cause: `\"overloaded_error\", status 529: Overloaded [redacted]`},
		{name: "an event named colon", step: mockprovider.Step{Chunks: events(":", start)}, status: 502,
			message: "upstream_error: the provider's stream failed", cause: `an event named`},
		{name: "an event name with a CR", step: mockprovider.Step{Chunks: events("ping\revent: content_block_delta", `{"type":"ping"}`)}, status: 502,
			message: "upstream_error: the provider's stream failed", cause: `with a CR`},
		{name: "not JSON", step: mockprovider.Step{Chunks: events("message_start", "{")}, status: 502,
			message: "upstream_error: the provider's stream failed", cause: "an event of the stream"},
		{name: "provider 529", step: mockprovider.Step{Status: 529, Body: []byte(overloaded)}, status: 502,
			message: "upstream_error: the provider answered 529"},
		{name: "a ping commits", step: mockprovider.Step{Chunks: events("ping", `{"type":"ping"}`, "message_start", start, "message_stop", messageStop)},
			status: 200, body: ev("ping", `{"type":"ping"}`, "message_start", start, "message_stop", messageStop)},
		{name: "an error event after the commit is relayed", step: mockprovider.Step{Chunks: events("message_start", start, "error", overloaded, "message_stop", messageStop)},
			status: 200, body: ev("message_start", start, "error", strings.Replace(overloaded, planted+"ANTH_KEY", "[redacted]", 1)), cause: "overloaded_error"},
		{name: "cut after the commit", step: mockprovider.Step{Chunks: events("message_start", start), Cut: true},
			status: 200, body: ev("message_start", start) + own("upstream_error", "the provider's stream failed"), cause: "unexpected EOF"},
		{name: "ended before message_stop", step: mockprovider.Step{Chunks: events("message_start", start)},
			status: 200, body: ev("message_start", start) + own("upstream_error", "the provider's stream failed"), cause: "the stream ended before its last event"},
		{name: "an event name with a CR after the commit", step: mockprovider.Step{Chunks: events("message_start", start, "ping\revent: content_block_delta", `{"type":"ping"}`)},
			status: 200, body: ev("message_start", start) + own("upstream_error", "the provider's stream failed"), cause: "with a CR"},
		{name: "silent after the commit", step: mockprovider.Step{Chunks: events("message_start", start), Hang: true},
			status: 200, body: ev("message_start", start) + own("upstream_timeout", "the provider did not answer in time: idle timeout"), cause: "idle timeout"},
		{name: "a text that is not a string", step: mockprovider.Step{Chunks: events("message_start", start, "content_block_delta",
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":1}}`)},
			status: 200, body: ev("message_start", start) + own("upstream_error", "the provider's stream failed"), cause: "a text without a string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.pastWindow()
			s.mock.Script(tc.step)
			before := len(s.mock.Calls())
			resp, got := s.postTo(t, "/v1/messages", messagesBody("claude", `,"stream":true`))
			if resp.StatusCode != tc.status || resp.Header.Get(targetHeader) != "anth/claude-up" || strings.Contains(got, planted) {
				t.Fatalf("status %d, headers %v: %q", resp.StatusCode, resp.Header, got)
			}
			if tc.status == 200 {
				if resp.Header.Get("Content-Type") != "text/event-stream" || got != tc.body {
					t.Errorf("body %q", got)
				}
			} else if m := anthropicEnvelope(t, resp.Header, tc.status, "upstream_error", []byte(got)); m != tc.message {
				t.Errorf("message %q", m)
			}
			if len(s.mock.Calls()) != before+1 {
				t.Errorf("%d calls", len(s.mock.Calls())-before)
			}
			log := s.log.String()
			line := log[strings.LastIndex(log, `"msg":"attempt"`):]
			if strings.Contains(line, `"cause":"`) != (tc.cause != "") || !strings.Contains(line, tc.cause) || strings.Contains(log, planted) {
				t.Errorf("attempt line %s", line)
			}
		})
	}
	// Thinking deltas, which the client gets, keep the stream alive.
	thinking := `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`
	s.pastWindow()
	s.mock.Script(mockprovider.Step{Chunks: events("message_start", start, "content_block_delta", thinking, "content_block_delta", thinking,
		"content_block_delta", thinking, "message_stop", messageStop), Gap: 120 * time.Millisecond})
	if resp, got := s.postTo(t, "/v1/messages", messagesBody("claude", `,"stream":true`)); resp.StatusCode != 200 ||
		got != ev("message_start", start, "content_block_delta", thinking, "content_block_delta", thinking, "content_block_delta", thinking, "message_stop", messageStop) {
		t.Errorf("%d %q", resp.StatusCode, got)
	}
}
