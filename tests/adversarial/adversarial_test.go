package adversarial

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/usage"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

const (
	requestID   = "X-Stargaite-Request-Id"
	traceHeader = "X-Stargaite-Trace-Id"
	chatReply   = `{"id":"chatcmpl-mock","object":"chat.completion","created":0,"model":"mock",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"mock reply"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
)

// want checks a refusal: its status and error code, and that the mock got
// no call since calls.
func (f *fixture) want(name string, a answer, status int, code string, calls int) {
	f.t.Helper()
	if a.status != status || a.code != code {
		f.t.Errorf("%s: %d %s, want %d %s: %s", name, a.status, a.code, status, code, a.body)
	}
	if n := f.calls(); n != calls {
		f.t.Errorf("%s: the provider was called (%d calls, want %d)", name, n, calls)
	}
}

// TestMalformedBody (PAR-1, phase-4 design section 15 point 26): malformed
// JSON, trailing bytes, invalid UTF-8, a body that is not an object and
// 10,001 levels of nesting within max_body_bytes get 400 invalid_request on
// every endpoint, with no provider call, and the process serves the next
// request; 10,000 levels, the tokenizer's depth, pass.
func TestMalformedBody(t *testing.T) {
	f := newFixture(t)
	nested := func(e endpoint, depth int) string { // the top-level object is the first level
		v := strings.Repeat("[", depth-1) + strings.Repeat("]", depth-1)
		if e.path == "/v1/chat/completions" || e.path == "/v1/messages" {
			return strings.Replace(e.valid(), `{"model"`, `{"metadata":`+v+`,"model"`, 1) // a member the proxy does not walk
		}
		return strings.Replace(e.valid(), `"hi"`, v, 1)
	}
	for _, e := range endpoints {
		v := e.valid()
		calls := f.calls()
		for _, tc := range []struct{ name, body string }{
			{"malformed JSON", v[:len(v)-1]},
			{"a missing value", strings.Replace(v, `"hi"`, ``, 1)},
			{"trailing bytes", v + `{}`},
			{"invalid UTF-8", strings.Replace(v, `"hi"`, "\"h\xffi\"", 1)},
			{"a lone surrogate", strings.Replace(v, `"hi"`, `"h\udc00i"`, 1)},
			{"an array", "[" + v + "]"},
			{"a string", `"` + e.alias + `"`},
			{"null", "null"},
			{"empty", ""},
			{"10,001 levels", nested(e, 10_001)},
		} {
			if len(tc.body) > maxBody {
				t.Fatalf("%s: %d bytes", tc.name, len(tc.body))
			}
			f.want(e.path+" "+tc.name, f.post(e.path, "agent", tc.body), http.StatusBadRequest, domain.CodeInvalidRequest, calls)
		}
		if a := f.post(e.path, "agent", nested(e, 10_000)); a.status != http.StatusOK || f.calls() != calls+1 {
			t.Errorf("%s 10,000 levels: %d %s", e.path, a.status, a.body)
		}
		if a := f.post(e.path, "agent", v); a.status != http.StatusOK {
			t.Errorf("%s after the refusals: %d %s", e.path, a.status, a.body)
		}
	}
}

// TestRepeatedKeys (PAR-1, AC-08): a repeated key and its escaped spelling
// get 400 invalid_request with no provider call, at the top level on every
// endpoint and in the objects the proxy walks on chat completions and
// messages (embeddings and rerank walk only the top level).
func TestRepeatedKeys(t *testing.T) {
	f := newFixture(t)
	var cases []struct{ path, body string }
	for _, e := range endpoints {
		m := `"model":"` + e.alias + `"`
		cases = append(cases, struct{ path, body string }{e.path, strings.Replace(e.valid(), m, m+","+m, 1)},
			struct{ path, body string }{e.path, strings.Replace(e.valid(), m, m+`,"\u006dodel":"`+e.alias+`"`, 1)})
	}
	const chat, msgs = "/v1/chat/completions", "/v1/messages"
	cases = append(cases, []struct{ path, body string }{
		{chat, `{"model":"chat","messages":[{"role":"user","content":"hi","role":"user"}]}`},
		{chat, `{"model":"chat","messages":[{"role":"user","content":"hi","\u0063ontent":"hi"}]}`},
		{chat, `{"model":"chat","messages":[{"role":"user","content":[{"type":"text","text":"hi","\u0074ext":"hi"}]}]}`},
		{chat, `{"model":"chat","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true,"include_usage":false}}`},
		{chat, `{"model":"chat","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","\u006eame":"g"}}]}`},
		{chat, `{"model":"chat","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"text","type":"json_object"}}`},
		{msgs, `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi","role":"user"}]}`},
		{msgs, `{"model":"claude","max_tokens":100,"thinking":{"type":"disabled","\u0074ype":"disabled"},"messages":[{"role":"user","content":"hi"}]}`},
		{msgs, `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi","text":"x"}]}]}`},
	}...)
	for _, tc := range cases {
		f.want(tc.path+" "+tc.body, f.post(tc.path, "agent", tc.body), http.StatusBadRequest, domain.CodeInvalidRequest, 0)
	}
}

// raw sends head, the blank line and body on a connection of its own, sends
// nothing more and reads the answer.
func (f *fixture) raw(head, body string) answer {
	f.t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(f.data, "http://"))
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, head+"\r\n"+body); err != nil {
		f.t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		f.t.Fatalf("no answer before the body ended: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	return answer{resp.StatusCode, resp.Header, string(b), codeOf(resp.Header, b)}
}

// TestOversizedBody (LIM-6): a body one byte over max_body_bytes gets 413
// request_too_large on every endpoint, with Content-Length and chunked,
// and before the body is read in full: a Content-Length declaring 1 MiB
// more, or a chunked body without its last chunk, of which only the first
// max_body_bytes + 1 bytes are sent. No provider is called, and the key's
// one parallel slot is back after them; a body of exactly max_body_bytes
// is served.
func TestOversizedBody(t *testing.T) {
	f := newFixture(t)
	sized := func(e endpoint, n int) string {
		v := e.valid()
		return strings.Replace(v, `"hi"`, `"`+strings.Repeat("x", n-len(v)+2)+`"`, 1)
	}
	head := func(e endpoint, framing string) string {
		return "POST " + e.path + " HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + f.tokens["one"] + "\r\n" + framing + "\r\n"
	}
	for _, e := range endpoints {
		over := sized(e, maxBody+1)
		calls := f.calls()
		for name, a := range map[string]answer{
			"Content-Length": f.do(http.MethodPost, f.data+e.path, strings.NewReader(over), "Authorization", "Bearer "+f.tokens["one"]),
			// A reader of unknown length: the client sends it chunked.
			"chunked":                           f.do(http.MethodPost, f.data+e.path, io.MultiReader(strings.NewReader(over)), "Authorization", "Bearer "+f.tokens["one"]),
			"Content-Length, body not all sent": f.raw(head(e, fmt.Sprintf("Content-Length: %d", len(over)+1<<20)), over),
			"chunked, no last chunk":            f.raw(head(e, "Transfer-Encoding: chunked"), fmt.Sprintf("%x\r\n%s\r\n", len(over), over)),
		} {
			f.want(e.path+" "+name, a, http.StatusRequestEntityTooLarge, domain.CodeRequestTooLarge, calls)
		}
		if a := f.post(e.path, "one", sized(e, maxBody)); a.status != http.StatusOK {
			t.Errorf("%s at max_body_bytes: %d %s", e.path, a.status, a.body)
		}
	}
	f.idle()
	if a := f.post("/v1/chat/completions", "one", endpoints[0].valid()); a.status != http.StatusOK {
		t.Errorf("after the refusals, the key's slot: %d %s", a.status, a.body)
	}
}

// inferencePaths are the routes a client key opens, with a body for each.
var inferencePaths = []struct{ method, path, body string }{
	{http.MethodPost, endpoints[0].path, endpoints[0].valid()},
	{http.MethodPost, endpoints[1].path, endpoints[1].valid()},
	{http.MethodPost, endpoints[2].path, endpoints[2].valid()},
	{http.MethodPost, endpoints[3].path, endpoints[3].valid()},
	{http.MethodGet, "/v1/models", ""},
}

// adminRoutes are the admin listener's routes, each with a path that names
// existing resources, and variants of the metrics route that the metrics
// token must not open.
var adminRoutes = []string{
	"GET /admin/v1/settings", "PATCH /admin/v1/settings", "GET /admin/v1/export", "POST /admin/v1/apply", "GET /admin/v1/validate",
	"GET /admin/v1/presets", "GET /admin/v1/providers", "POST /admin/v1/providers", "GET /admin/v1/providers/mock",
	"PATCH /admin/v1/providers/mock", "DELETE /admin/v1/providers/mock", "GET /admin/v1/targets", "POST /admin/v1/targets",
	"GET /admin/v1/targets/mock%2Fchat", "PATCH /admin/v1/targets/mock%2Fchat", "DELETE /admin/v1/targets/mock%2Fchat",
	"GET /admin/v1/models", "POST /admin/v1/models", "GET /admin/v1/models/chat", "PATCH /admin/v1/models/chat",
	"DELETE /admin/v1/models/chat", "POST /admin/v1/models/chat/explain", "GET /admin/v1/keys", "POST /admin/v1/keys",
	"GET /admin/v1/keys/agent", "PATCH /admin/v1/keys/agent", "POST /admin/v1/keys/agent/rotate", "POST /admin/v1/keys/agent/revoke",
	"GET /admin/v1/usage", "GET /admin/v1/usage/quota", "GET /admin/v1/telemetry", "PATCH /admin/v1/telemetry",
	"DELETE /admin/v1/telemetry", "POST /admin/v1/telemetry/test", "POST /admin/v1/secrets/reload", "POST /admin/v1/backup",
	"POST /admin/v1/verify/mock%2Fchat", "GET /admin/v1/no-such-route",
	"HEAD /admin/v1/metrics", "POST /admin/v1/metrics", "GET /admin/v1/metrics/", "GET /admin/v1//metrics", "GET /admin/v1/metrics/../metrics",
}

// TestAuthentication (SEC-1, SEC-2, SEC-3, SEC-8): on every route a client
// key opens, a revoked key, the wrong secret of a real public id, malformed
// Bearer forms, the admin key and the metrics token get 401
// invalid_api_key, Bearer and, on /v1/messages, x-api-key alike, and no
// provider is called; a revoked key is refused within 1 s of the revoke. A
// client key and the metrics token get 401 on every other admin route, and
// change nothing.
func TestAuthentication(t *testing.T) {
	f := newFixture(t)
	token := f.tokens["agent"]
	bad := []string{
		token[:len(token)-52] + strings.Repeat("a", 52), // the public id of agent, another secret
		token[:len(token)-1], token + "a", strings.ToUpper(token), " " + token, token + " x", adminKey, metricsToken, "sk-stargaite-", "",
	}
	forms := []string{"", "Bearer", "Basic " + token, token, "Bearer" + token, "Token " + token}
	for _, b := range bad {
		forms = append(forms, "Bearer "+b)
	}

	// Revoked: a client calling in a loop is refused within 1 s.
	revoked := f.tokens["revoked"]
	started, refused := make(chan struct{}), make(chan time.Time, 1)
	go func() {
		defer close(refused)
		for once := false; ; {
			req, err := http.NewRequest(http.MethodGet, f.data+"/v1/models", nil)
			if err != nil {
				return
			}
			req.Header.Set("Authorization", "Bearer "+revoked)
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				refused <- time.Now()
				return
			}
			if !once {
				once = true
				close(started)
			}
		}
	}()
	select {
	case <-started:
	case <-refused:
	}
	if a := f.adminCall(http.MethodPost, "/admin/v1/keys/revoked/revoke", "", adminKey); a.status != http.StatusOK {
		t.Fatalf("revoke: %d %s", a.status, a.body)
	}
	at := time.Now()
	if when, ok := <-refused; !ok || when.Sub(at) > time.Second {
		t.Errorf("the revoked key was refused %v after the revoke (ok %v)", when.Sub(at), ok)
	}
	forms = append(forms, "Bearer "+revoked)

	calls := f.calls()
	for _, p := range inferencePaths {
		for _, form := range forms {
			header := []string{}
			if form != "" {
				header = []string{"Authorization", form}
			}
			a := f.do(p.method, f.data+p.path, strings.NewReader(p.body), header...)
			f.want(fmt.Sprintf("%s %s with %q", p.method, p.path, form), a, http.StatusUnauthorized, domain.CodeInvalidAPIKey, calls)
			if len(token) > 20 && strings.Contains(a.body, token[len(token)-20:]) {
				t.Errorf("%s: the 401 echoes the token", p.path)
			}
		}
		if p.path == "/v1/messages" {
			for _, b := range append(bad, revoked) {
				if b != strings.TrimSpace(b) {
					continue // net/http trims the value: it is the token
				}
				a := f.do(p.method, f.data+p.path, strings.NewReader(p.body), "X-Api-Key", b)
				f.want(fmt.Sprintf("x-api-key %q", b), a, http.StatusUnauthorized, domain.CodeInvalidAPIKey, calls)
			}
		}
	}

	for _, route := range adminRoutes {
		method, path, _ := strings.Cut(route, " ")
		for _, b := range []string{token, metricsToken, adminKey[:len(adminKey)-1], ""} {
			a := f.do(method, f.admin+path, strings.NewReader(`{}`), "Authorization", "Bearer "+b)
			if a.status != http.StatusUnauthorized || method != http.MethodHead && a.code != domain.CodeInvalidAPIKey {
				t.Errorf("%s with %.12q: %d %s", route, b, a.status, a.body)
			}
		}
	}
	if a := f.adminCall(http.MethodGet, "/admin/v1/metrics", "", metricsToken); a.status != http.StatusOK {
		t.Errorf("the metrics token on its route: %d %s", a.status, a.body)
	}
	if a := f.adminCall(http.MethodGet, "/admin/v1/metrics", "", token); a.status != http.StatusUnauthorized {
		t.Errorf("a client key on the metrics route: %d", a.status)
	}
	if a := f.adminCall(http.MethodGet, "/admin/v1/keys/agent", "", adminKey); a.status != http.StatusOK || !strings.Contains(a.body, `"name":"agent"`) {
		t.Errorf("agent after the refused calls: %d %s", a.status, a.body)
	}
	if a := f.post("/v1/chat/completions", "agent", endpoints[0].valid()); a.status != http.StatusOK {
		t.Errorf("agent after the refused calls: %d %s", a.status, a.body)
	}
}

// TestUnknownAlias (OPS-8, OPS-9): an unknown alias and random ones get 404
// model_not_found on every endpoint, with no provider call; the counters
// gain no name of the client's (one requests series under model ""), and
// the alias is cut to 200 characters in the request line, the error
// message and the spans.
func TestUnknownAlias(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("x", 1000)
	aliases := []string{"nope", long, "é" + long, "chat ", "CHAT"}
	for range 50 {
		aliases = append(aliases, rand.Text())
	}
	series := func() []usage.Counter {
		var out []usage.Counter
		f.counters.Each(func(c usage.Counter, _ float64) { out = append(out, c) })
		f.spanned.Each(func(c usage.Counter, _ float64) { out = append(out, c) })
		return out
	}
	offset := len(f.log.String())
	for _, e := range endpoints {
		for _, alias := range aliases {
			a := f.post(e.path, "agent", e.body(alias))
			f.want(e.path+" "+alias[:min(len(alias), 20)], a, http.StatusNotFound, domain.CodeModelNotFound, 0)
			if strings.Contains(a.body, long[:201]) {
				t.Errorf("%s: the error message holds more than 200 characters of the alias", e.path)
			}
		}
	}
	f.idle()
	if got, want := series(), []usage.Counter{{Name: "requests", Key: "agent", Label: "404"}, {Name: "spans", Key: "agent"}}; !slices.Equal(got, want) {
		t.Errorf("counters %+v, want %+v", got, want)
	}
	cut := 0
	for _, l := range f.logLines(offset) {
		if l["msg"] != "request" {
			continue
		}
		alias, _ := l["alias"].(string)
		if n := utf8.RuneCountInString(alias); n > 200 || n == 0 {
			t.Errorf("request line alias of %d characters", n)
		}
		if alias == long[:200] || alias == "é"+long[:199] {
			cut++
		}
	}
	if cut != 2*len(endpoints) {
		t.Errorf("%d request lines with the long aliases cut to 200 characters", cut)
	}
	if spans := f.exported(); strings.Contains(spans, long[:201]) || !strings.Contains(spans, long[:200]) {
		t.Error("the spans do not cut the alias to 200 characters")
	}
}

// TestIncompatibleTarget (PAR-4, PAR-12, kick-off section 7.1): tools
// toward a target with tools false, an endpoint the alias's targets do not
// serve (embeddings or rerank to a chat alias, chat to an embeddings alias,
// messages to an openai target, rerank to a provider without rerank_url) and
// a chat feature with no faithful equivalent toward anthropic get 422 with
// zero provider calls.
func TestIncompatibleTarget(t *testing.T) {
	f := newFixture(t)
	const hi = `"messages":[{"role":"user","content":"hi"}]`
	for _, tc := range []struct{ name, path, body, code string }{
		{"tools toward tools false", "/v1/chat/completions", `{"model":"notools",` + hi + `,"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`, domain.CodeCapabilityMismatch},
		{"tool_choice toward tools false", "/v1/chat/completions", `{"model":"notools",` + hi + `,"tool_choice":"none"}`, domain.CodeCapabilityMismatch},
		{"embeddings to a chat alias", "/v1/embeddings", `{"model":"chat","input":"hi"}`, domain.CodeEndpointNotSupported},
		{"rerank to a chat alias", "/v1/rerank", `{"model":"chat","query":"hi","documents":["a"]}`, domain.CodeEndpointNotSupported},
		{"rerank to an embeddings alias", "/v1/rerank", `{"model":"embed","query":"hi","documents":["a"]}`, domain.CodeEndpointNotSupported},
		{"rerank without rerank_url", "/v1/rerank", `{"model":"nourl","query":"hi","documents":["a"]}`, domain.CodeEndpointNotSupported},
		{"chat to an embeddings alias", "/v1/chat/completions", `{"model":"embed",` + hi + `}`, domain.CodeEndpointNotSupported},
		{"chat to a rerank alias", "/v1/chat/completions", `{"model":"rerank",` + hi + `}`, domain.CodeEndpointNotSupported},
		{"messages to an openai target", "/v1/messages", `{"model":"chat","max_tokens":10,` + hi + `}`, domain.CodeEndpointNotSupported},
		{"embeddings to an anthropic target", "/v1/embeddings", `{"model":"claude","input":"hi"}`, domain.CodeEndpointNotSupported},
		{"n above 1 toward anthropic", "/v1/chat/completions", `{"model":"claude",` + hi + `,"n":2}`, domain.CodeUnsupportedParameter},
	} {
		f.want(tc.name, f.post(tc.path, "agent", tc.body), http.StatusUnprocessableEntity, tc.code, 0)
	}
}

// stream posts body to path with agent's token and returns the status, the
// request id and the whole answer.
func (f *fixture) stream(path, body string) (int, string, string) {
	f.t.Helper()
	a := f.post(path, "agent", body)
	return a.status, a.header.Get(requestID), a.body
}

func chunk(delta string) []byte {
	return event(`{"id":"c","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":` + delta + `,"finish_reason":null}]}`)
}

var (
	chatEvents = [][]byte{
		chunk(`{"role":"assistant","content":""}`),
		chunk(`{"content":"é \u00e9 \"quoted\" \\ end"}`),
		chunk(`{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]}`),
		chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}`),
		event(`{"id":"c","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		event(`{"id":"c","object":"chat.completion.chunk","created":0,"model":"m","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`),
		event(`[DONE]`),
	}
	messagesEvents = [][]byte{
		event("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":3,"output_tokens":1}}}`),
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		event("ping", `{"type":"ping"}`),
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"é \u00e9 \"quoted\""}}`),
		event("content_block_stop", `{"type":"content_block_stop","index":0}`),
		event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`),
		event("message_stop", `{"type":"message_stop"}`),
	}
)

const (
	chatStream     = `{"model":"ALIAS","messages":[{"role":"user","content":"hi"}],"stream":true}`
	messagesStream = `{"model":"ALIAS","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`
)

// TestFragmentedSSE (RES-4): an upstream stream cut into one-byte chunks,
// with LF or CRLF line ends, gives the client the same events as the
// stream sent whole, on the native chat and messages paths and on chat
// translated toward anthropic; natively relayed, the events are the
// provider's bytes.
func TestFragmentedSSE(t *testing.T) {
	f := newFixture(t)
	crlf := func(chunks [][]byte) [][]byte {
		return [][]byte{bytes.ReplaceAll(bytes.Join(chunks, nil), []byte("\n"), []byte("\r\n"))}
	}
	for _, tc := range []struct {
		name, path, body string
		events           [][]byte
		native           bool
	}{
		{"chat", "/v1/chat/completions", strings.Replace(chatStream, "ALIAS", "chat", 1), chatEvents, true},
		{"messages", "/v1/messages", strings.Replace(messagesStream, "ALIAS", "claude", 1), messagesEvents, true},
		{"chat toward anthropic", "/v1/chat/completions", strings.Replace(chatStream, "ALIAS", "claude", 1), messagesEvents, false},
	} {
		f.mock.Script(mockprovider.Step{Chunks: tc.events})
		status, _, whole := f.stream(tc.path, tc.body)
		if status != http.StatusOK || tc.native && whole != string(bytes.Join(tc.events, nil)) || !strings.Contains(whole, `quoted`) {
			t.Errorf("%s whole: %d %q", tc.name, status, whole)
		}
		for name, chunks := range map[string][][]byte{"bytewise": bytewise(tc.events), "CRLF bytewise": bytewise(crlf(tc.events)), "CRLF": crlf(tc.events)} {
			f.mock.Script(mockprovider.Step{Chunks: chunks})
			if status, _, got := f.stream(tc.path, tc.body); status != http.StatusOK || got != whole {
				t.Errorf("%s %s: %d %q, want %q", tc.name, name, status, got, whole)
			}
		}
	}
}

// visible is the text a client reads from the events of an answer: the
// chat deltas' content or the messages text deltas; ok is false when an
// event is not valid JSON (or [DONE] last).
func visible(body string) (string, bool) {
	var text strings.Builder
	events := strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n")
	for i, e := range events {
		_, data, found := strings.Cut(e, "data: ")
		if !found || strings.Contains(data, "\n") {
			return "", false
		}
		if data == "[DONE]" && i == len(events)-1 {
			continue
		}
		var v struct {
			Choices []struct{ Delta struct{ Content string } }
			Delta   struct{ Text string }
		}
		if json.Unmarshal([]byte(data), &v) != nil {
			return "", false
		}
		for _, c := range v.Choices {
			text.WriteString(c.Delta.Content)
		}
		text.WriteString(v.Delta.Text)
	}
	return text.String(), true
}

// TestThinkTags (RSN-12, RES-4): toward targets that declare
// inline_think_tags, think blocks split across chunks, one left unclosed and
// one written with JSON escapes are removed from the visible text, on chat
// and messages, with the stream cut into one-byte chunks; every event stays
// valid JSON.
func TestThinkTags(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name   string
		pieces []string // JSON strings
		want   string
	}{
		{"split", []string{`"<th"`, `"ink>plan</th"`, `"ink>answer"`}, "answer"},
		{"split at every tag byte", []string{`"a<"`, `"t"`, `"h"`, `"i"`, `"n"`, `"k"`, `">p<"`, `"/think"`, `">b"`}, "ab"},
		{"unclosed", []string{`"shown <think>never"`, `" closed"`}, "shown "},
		{"escaped", []string{`"\u003cthink\u003eplan\u003c/think\u003eanswer"`}, "answer"},
		{"escaped and split", []string{`"\u003cth"`, `"ink>plan\u003c\/think"`, `"\u003eanswer"`}, "answer"},
		{"two blocks", []string{`"<think>a</think>x<think>"`, `"b</think>y"`}, "xy"},
	} {
		var chat, msgs [][]byte
		msgs = append(msgs, messagesEvents[:2]...)
		for _, p := range tc.pieces {
			chat = append(chat, chunk(`{"content":`+p+`}`))
			msgs = append(msgs, event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+p+`}}`))
		}
		chat = append(chat, chatEvents[4:]...)
		msgs = append(msgs, messagesEvents[4:]...)
		for _, run := range []struct {
			path, body string
			events     [][]byte
		}{
			{"/v1/chat/completions", strings.Replace(chatStream, "ALIAS", "think", 1), chat},
			{"/v1/messages", strings.Replace(messagesStream, "ALIAS", "claudethink", 1), msgs},
		} {
			f.mock.Script(mockprovider.Step{Chunks: bytewise(run.events)})
			status, _, got := f.stream(run.path, run.body)
			if text, ok := visible(got); status != http.StatusOK || !ok || text != tc.want || strings.Contains(got, "think") {
				t.Errorf("%s %s: %d, visible %q (valid %v), want %q: %q", run.path, tc.name, status, text, ok, tc.want, got)
			}
		}
	}
}

// TestTruncatedStream (RES-4, USE-1): a stream the provider cuts midway
// ends with one terminal error event in the format of the endpoint (no
// [DONE] after it on chat) and leaves one usage record.
func TestTruncatedStream(t *testing.T) {
	f := newFixture(t)
	runs := []struct {
		name, path, body, terminal string
		events                     [][]byte
	}{
		{"chat", "/v1/chat/completions", strings.Replace(chatStream, "ALIAS", "chat", 1), "data: {\"error\":", chatEvents[:2]},
		{"messages", "/v1/messages", strings.Replace(messagesStream, "ALIAS", "claude", 1), "event: error\ndata: {\"type\":\"error\"", messagesEvents[:4]},
		{"chat toward anthropic", "/v1/chat/completions", strings.Replace(chatStream, "ALIAS", "claude", 1), "data: {\"error\":", messagesEvents[:4]},
	}
	ids := map[string]string{}
	for _, run := range runs {
		f.mock.Script(mockprovider.Step{Chunks: run.events, Cut: true})
		status, id, got := f.stream(run.path, run.body)
		events := strings.Split(strings.TrimSuffix(got, "\n\n"), "\n\n")
		if status != http.StatusOK || strings.Count(got, run.terminal) != 1 || !strings.HasPrefix(events[len(events)-1], run.terminal) ||
			strings.Contains(got, "[DONE]") || strings.Contains(got, "message_stop") {
			t.Errorf("%s: %d %q", run.name, status, got)
		}
		ids[run.name] = id
	}
	for name, id := range ids {
		if n := f.stored(id); n != 1 {
			t.Errorf("%s: %d usage records", name, n)
		}
	}
}

// TestSSRF (SEC-4, AC-43): base_url and rerank_url at a metadata address,
// at loopback written as numbers (0x7f.1, 2130706433, 017700000001), at
// IPv4-mapped and NAT64 forms, at a zoned address, with userinfo, are
// refused when set, with allow_private_network or without as the class
// demands, and the refusal never quotes the URL; a provider's 302 is not
// followed (502, the Location never reached); a name that set time saw
// public and the dial finds private is refused at the dial: 502, one
// lookup, no connection, no second dial; on chat, embeddings and rerank.
func TestSSRF(t *testing.T) {
	f := newFixture(t)
	never := []string{"http://169.254.169.254/v1", "https://169.254.169.254/v1", "https://[::ffff:169.254.169.254]/v1", "https://[64:ff9b::a9fe:a9fe]/v1",
		"https://0x7f.1/v1", "https://2130706433/v1", "https://017700000001/v1", "https://127.1/v1", "https://[fe80::1%25eth0]/v1",
		"https://user:pass-SECRET@api.example.com/v1", "https://[fd00:ec2::254]/v1", "https://100.100.100.200/v1", "ftp://api.example.com/v1"}
	private := []string{"https://127.0.0.1/v1", "https://[::ffff:127.0.0.1]/v1", "https://[64:ff9b::7f00:1]/v1", "https://[::1]/v1",
		"https://10.0.0.1/v1", "https://localhost/v1", "http://api.example.com/v1"}
	for _, field := range []string{"base_url", "rerank_url"} {
		for _, url := range append(never, private...) {
			flags := []bool{false}
			if slices.Contains(never, url) {
				flags = append(flags, true)
			}
			for _, flag := range flags {
				urls := `"base_url":"` + url + `"`
				if field == "rerank_url" {
					urls = `"base_url":"https://api.example.com/v1","rerank_url":"` + url + `"`
				}
				a := f.adminCall(http.MethodPost, "/admin/v1/providers", `{"name":"p","protocol":"openai","billing":"none",`+urls+
					`,"allow_private_network":`+strconv.FormatBool(flag)+`}`, adminKey)
				if a.status != http.StatusBadRequest || a.code != domain.CodeInvalidRequest || !strings.Contains(a.body, `"code":"url_refused"`) ||
					strings.Contains(a.body, "SECRET") || strings.Contains(a.body, "0x7f") {
					t.Errorf("%s %s, allow_private_network %v: %d %s", field, url, flag, a.status, a.body)
				}
			}
		}
	}
	if a := f.adminCall(http.MethodGet, "/admin/v1/providers/p", "", adminKey); a.status != http.StatusNotFound {
		t.Errorf("a refused provider was stored: %d %s", a.status, a.body)
	}

	var reached atomic.Int64
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	t.Cleanup(elsewhere.Close)
	redirect := mockprovider.Step{Status: http.StatusFound, Header: http.Header{"Location": {elsewhere.URL + "/v1/chat/completions"}}}
	for _, e := range []struct{ path, body string }{
		{endpoints[0].path, endpoints[0].valid()}, {endpoints[0].path, strings.Replace(chatStream, "ALIAS", "chat", 1)},
		{endpoints[2].path, endpoints[2].valid()}, {endpoints[3].path, endpoints[3].valid()},
	} {
		f.mock.Script(redirect)
		calls := f.calls()
		f.want(e.path+" 302", f.post(e.path, "agent", e.body), http.StatusBadGateway, domain.CodeUpstreamError, calls+1)
	}
	if reached.Load() != 0 {
		t.Error("a redirect was followed")
	}

	for i, e := range []struct{ path, body string }{
		{"/v1/chat/completions", endpoints[0].body("rebound")},
		{"/v1/embeddings", endpoints[2].body("reboundemb")},
		{"/v1/rerank", endpoints[3].body("reboundrr")},
	} {
		f.want(e.path+" rebound", f.post(e.path, "agent", e.body), http.StatusBadGateway, domain.CodeUpstreamError, f.calls())
		if n := f.lookups.Load(); n != int64(i+1) {
			t.Errorf("%s: %d dial-time lookups of rebound.test, want %d", e.path, n, i+1)
		}
	}
	if n := f.accepted.Load(); n != 0 {
		t.Errorf("the private address got %d connections", n)
	}
}

// race sends n requests at once and counts the answers by status and code.
func (f *fixture) race(n int, path, key, body string) map[string]int {
	f.t.Helper()
	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Go(func() {
			<-start
			a := f.post(path, key, body)
			mu.Lock()
			got[fmt.Sprintf("%d %s", a.status, a.code)]++
			mu.Unlock()
		})
	}
	close(start)
	wg.Wait()
	f.idle()
	return got
}

// TestBudgetRace (LIM-4, LIM-8, AC-37): 50 requests at once on a key whose
// budget two calls cross and that has 5 parallel slots spend at most the
// limit plus the cost of the 5 calls in flight when it is crossed, each
// call charged; 50 more at the exhausted budget get 402 (or 429 for want
// of a slot) and reach no provider; 50 at once on a subscription window of 5 requests make exactly 5
// provider calls, the rest 429 quota_exceeded.
func TestBudgetRace(t *testing.T) {
	f := newFixture(t)
	for range 50 {
		f.mock.Script(mockprovider.Step{Delay: 50 * time.Millisecond, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(chatReply)})
	}
	body := endpoints[0].body("paid")
	got := f.race(50, endpoints[0].path, "budget", body)
	served := got["200 "]
	spent := f.limits.Spent(admission.Budget{Scope: "key:budget", Spec: domain.Budget{ResetInterval: domain.ResetDaily}})
	if served < 2 || served > 5+1 || f.calls() != served || got["200 "]+got["429 parallel_limit_exceeded"]+got["402 budget_exceeded"] != 50 ||
		spent > 0.05+5*callCost+1e-9 || spent < float64(served)*callCost-1e-9 {
		t.Errorf("answers %v, %d calls, %v spent", got, f.calls(), spent)
	}
	calls := f.calls()
	// Admission (the 5 slots) comes before the budget (kick-off section 14.1).
	if got := f.race(50, endpoints[0].path, "budget", body); got["402 budget_exceeded"] == 0 ||
		got["402 budget_exceeded"]+got["429 parallel_limit_exceeded"] != 50 || f.calls() != calls {
		t.Errorf("at the exhausted budget: %v, %d calls", got, f.calls()-calls)
	}
	if got := f.race(50, endpoints[0].path, "plan", endpoints[0].body("plan")); got["200 "] != 5 || got["429 quota_exceeded"] != 45 || f.calls() != calls+5 {
		t.Errorf("at a window of 5 requests: %v, %d calls", got, f.calls()-calls)
	}
}

// gzipped is data compressed with gzip.
func gzipped(t *testing.T, data []byte) []byte {
	var b bytes.Buffer
	w, err := gzip.NewWriterLevel(&b, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// bomb is gzip members that inflate to head, 64 MiB of zeros and tail: a
// gzip reader reads concatenated members as one stream.
func bomb(t *testing.T, head, tail string) []byte {
	mib := gzipped(t, bytes.Repeat([]byte("0"), 1<<20))
	out := gzipped(t, []byte(head))
	for range 64 {
		out = append(out, mib...)
	}
	return append(out, gzipped(t, []byte(tail))...)
}

// TestUpstreamOversize (RES-4): an answer one byte over max_response_bytes,
// an event over it and a gzip bomb (64 MiB once inflated, synchronous and
// streamed) get 502 upstream_error on every endpoint, the memory the
// process allocates for them bounded far below what they inflate to; an
// answer of exactly max_response_bytes is relayed.
func TestUpstreamOversize(t *testing.T) {
	f := newFixture(t)
	padded := func(n int) []byte { return []byte(`{"pad":"` + strings.Repeat("x", n-10) + `"}`) }
	syncBomb, sseBomb := bomb(t, `{"pad":"`, `"}`), bomb(t, "data: ", "\n\n")
	zipped := http.Header{"Content-Encoding": {"gzip"}, "Content-Type": {"application/json"}}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for _, e := range endpoints {
		for name, step := range map[string]mockprovider.Step{
			"one byte over": {Header: http.Header{"Content-Type": {"application/json"}}, Body: padded(maxAnswer + 1)},
			"gzip bomb":     {Header: zipped, Body: syncBomb},
		} {
			f.mock.Script(step)
			calls := f.calls()
			f.want(e.path+" "+name, f.post(e.path, "agent", e.valid()), http.StatusBadGateway, domain.CodeUpstreamError, calls+1)
		}
	}
	for name, step := range map[string]mockprovider.Step{
		"an event over the bound": {Chunks: [][]byte{event(`"` + strings.Repeat("x", maxAnswer) + `"`)}},
		"an SSE gzip bomb":        {Header: http.Header{"Content-Encoding": {"gzip"}}, Chunks: [][]byte{sseBomb}},
	} {
		for _, run := range []struct{ path, body string }{
			{"/v1/chat/completions", strings.Replace(chatStream, "ALIAS", "chat", 1)},
			{"/v1/messages", strings.Replace(messagesStream, "ALIAS", "claude", 1)},
		} {
			f.mock.Script(step)
			calls := f.calls()
			f.want(run.path+" "+name, f.post(run.path, "agent", run.body), http.StatusBadGateway, domain.CodeUpstreamError, calls+1)
		}
	}
	f.idle()
	runtime.ReadMemStats(&after)
	if n := after.TotalAlloc - before.TotalAlloc; n > 32<<20 {
		t.Errorf("%d MiB allocated for twelve answers, six of them inflating to 64 MiB", n>>20)
	}
	f.mock.Script(mockprovider.Step{Header: http.Header{"Content-Type": {"application/json"}}, Body: padded(maxAnswer)})
	if a := f.post("/v1/rerank", "agent", endpoints[3].valid()); a.status != http.StatusOK || a.body != string(padded(maxAnswer)) {
		t.Errorf("an answer of max_response_bytes: %d %.100s", a.status, a.body)
	}
}

// TestCredentialEcho (SEC-5, TEL-8): a provider whose answers echo the
// operator's credential, synchronous (400, 401, 429, 500, and a 2xx) and as
// a stream's error event, on every endpoint, never has it reach the response, the log,
// the spans (content capture on) or the usage records.
func TestCredentialEcho(t *testing.T) {
	f := newFixture(t)
	if a := f.adminCall(http.MethodPatch, "/admin/v1/keys/agent", `{"content":"1h"}`, adminKey); a.status != http.StatusOK {
		t.Fatalf("content: %d %s", a.status, a.body)
	}
	openai := `{"error":{"message":"invalid key ` + credential + `","type":"invalid_request_error","code":"` + credential + `"}}`
	anthropic := `{"type":"error","error":{"type":"overloaded_error","message":"invalid key ` + credential + `"}}`
	echoed := http.Header{"Content-Type": {"application/json"}, "X-Echo": {credential}}
	var bodies []string
	for _, e := range endpoints {
		echo := openai
		if e.path == "/v1/messages" {
			echo = anthropic
		}
		for _, status := range []int{400, 401, 429, 500} {
			f.mock.Script(mockprovider.Step{Status: status, Header: echoed, Body: []byte(echo)})
			a := f.post(e.path, "agent", e.valid())
			bodies = append(bodies, fmt.Sprint(a.status, a.header, a.body))
		}
	}
	// A 2xx answer that echoes it, synchronous, on every endpoint: relayed
	// with the credential redacted, the rest of the answer intact.
	for i, answer := range []string{
		`{"id":"c","object":"chat.completion","created":0,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"key ` + credential + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
		`{"id":"msg","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"key ` + credential + `"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`,
		`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.5]}],"model":"` + credential + `","usage":{"prompt_tokens":1,"total_tokens":1}}`,
		`{"results":[{"index":0,"relevance_score":0.5}],"meta":"` + credential + `"}`,
	} {
		f.mock.Script(mockprovider.Step{Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(answer)})
		a := f.post(endpoints[i].path, "agent", endpoints[i].valid())
		if a.status != http.StatusOK || !strings.Contains(a.body, "[redacted]") {
			t.Errorf("%s: a 2xx echo: %d %s", endpoints[i].path, a.status, a.body)
		}
		bodies = append(bodies, fmt.Sprint(a.status, a.header, a.body))
	}
	for _, run := range []struct {
		path, body string
		events     [][]byte
	}{
		{"/v1/chat/completions", strings.Replace(chatStream, "ALIAS", "chat", 1), [][]byte{chatEvents[1], event(openai)}},
		{"/v1/chat/completions", strings.Replace(chatStream, "ALIAS", "chat", 1), [][]byte{event(openai)}},
		{"/v1/messages", strings.Replace(messagesStream, "ALIAS", "claude", 1), append(slices.Clone(messagesEvents[:4]), event("error", anthropic))},
		{"/v1/chat/completions", strings.Replace(chatStream, "ALIAS", "claude", 1), append(slices.Clone(messagesEvents[:4]), event("error", anthropic))},
	} {
		f.mock.Script(mockprovider.Step{Header: http.Header{"X-Echo": {credential}}, Chunks: run.events})
		a := f.post(run.path, "agent", run.body)
		bodies = append(bodies, fmt.Sprint(a.status, a.header, a.body))
	}
	if c := f.mock.Calls(); len(c) != 4*4+4+4 || c[0].Header.Get("Authorization") != "Bearer "+credential {
		t.Fatalf("%d calls; the provider got %q", len(c), c[0].Header.Get("Authorization"))
	}
	spans := f.exported()
	f.stored("")
	rows, err := f.st.Read().Query("SELECT * FROM usage")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols, _ := rows.Columns()
	var records strings.Builder
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&records, "%s\n", values)
	}
	for name, text := range map[string]string{"responses": strings.Join(bodies, "\n"), "log": f.log.String(), "spans": spans, "records": records.String()} {
		if strings.Contains(text, planted) || text == "" {
			t.Errorf("the credential reaches the %s: %s", name, text)
		}
	}
}

// TestControlBytes (OPS-9, TEL-6): CR, LF and other control bytes in the
// alias give one escaped JSON request line, never a forged one; in
// traceparent and x-session-id the header is ignored: a new trace, no
// session. The handler is called in process, since net/http refuses such
// header values on the wire (400) before any handler runs, as the last case
// checks.
func TestControlBytes(t *testing.T) {
	f := newFixture(t)
	const trace, parent = "0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331"
	valid := "00-" + trace + "-" + parent + "-01"
	send := func(body string, header ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+f.tokens["agent"])
		for i := 0; i+1 < len(header); i += 2 {
			r.Header.Add(header[i], header[i+1])
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}
	offset := len(f.log.String())
	forged := `a\r\n{\"time\":\"x\",\"level\":\"INFO\",\"msg\":\"forged\"}\n\u0000\u0001\u001b[31m\u2028\u007f`
	w := send(endpoints[0].body(forged))
	if w.Code != http.StatusNotFound {
		t.Errorf("the alias: %d %s", w.Code, w.Body)
	}
	id := w.Header().Get(requestID)
	for _, v := range []string{valid + "\r\nX-Injected: 1", valid + "\n", valid[:20] + "\x00" + valid[21:], valid + "\x7f", "\x01" + valid[1:], valid + "\t"} {
		if w := send(endpoints[0].valid(), "Traceparent", v); w.Code != http.StatusOK || w.Header().Get(traceHeader) == trace {
			t.Errorf("traceparent %q: %d, trace %s", v, w.Code, w.Header().Get(traceHeader))
		}
	}
	if w := send(endpoints[0].valid(), "Traceparent", valid); w.Header().Get(traceHeader) != trace {
		t.Errorf("a valid traceparent: trace %s", w.Header().Get(traceHeader))
	}
	for _, v := range []string{"s\r\nx", "s\n", "s\x00", "s\x01", "s\x1b[31m", "s\x7f", "s "} {
		if w := send(endpoints[0].valid(), "X-Session-Id", v); w.Code != http.StatusOK {
			t.Errorf("x-session-id %q: %d", v, w.Code)
		}
	}
	send(endpoints[0].valid(), "X-Session-Id", "session-1")

	requests := 0
	for _, l := range f.logLines(offset) {
		if l["msg"] == "forged" {
			t.Error("a forged log line")
		}
		if l["msg"] == "request" && l["request_id"] == id {
			requests++
			if want := "a\r\n{\"time\":\"x\",\"level\":\"INFO\",\"msg\":\"forged\"}\n\x00\x01\x1b[31m\u2028\x7f"; l["alias"] != want {
				t.Errorf("the alias in the request line: %q, want %q", l["alias"], want)
			}
		}
	}
	if requests != 1 {
		t.Errorf("%d request lines for the alias", requests)
	}
	spans := f.exported()
	if n, m := strings.Count(spans, `"key":"session.id"`), strings.Count(spans, `"key":"session.id","value":{"stringValue":"session-1"}`); n == 0 || n != m {
		t.Errorf("session.id %d times, %d of them session-1", n, m)
	}

	// On the wire, net/http refuses the header before the handler.
	for _, h := range []string{"X-Session-Id: s\x01", "Traceparent: " + valid + "\x00", "X-Session-Id: s\x7f"} {
		a := f.raw("POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer "+f.tokens["agent"]+"\r\n"+h+"\r\nContent-Length: 2\r\n", "{}")
		if a.status != http.StatusBadRequest || a.header.Get(requestID) != "" {
			t.Errorf("%q on the wire: %d %v %s", h, a.status, a.header, a.body)
		}
	}
}

// TestBadTokenFlood (TEL-5, OPS-9): 1,000 wrong tokens on every route a
// client key opens write one log line per 10 s, with the count of failures
// and never a token, and stargaite_unauthenticated_requests_total counts
// every one, read through the metrics route with the metrics token.
func TestBadTokenFlood(t *testing.T) {
	f := newFixture(t)
	offset := len(f.log.String())
	const n = 1000
	var tokens []string
	for i := range n {
		p := inferencePaths[i%len(inferencePaths)]
		token := "sk-stargaite-" + strings.ToLower(rand.Text()[:12]) + "-" + strings.ToLower(rand.Text()+rand.Text())
		tokens = append(tokens, token)
		r := httptest.NewRequest(p.method, p.path, strings.NewReader(p.body))
		if p.path == "/v1/messages" && i%2 == 0 {
			r.Header.Set("X-Api-Key", token)
		} else {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: %d", p.path, w.Code)
		}
	}
	f.now.Add((10 * time.Second).Milliseconds())
	a := f.do(http.MethodGet, f.data+"/v1/models", nil, "Authorization", "Bearer "+tokens[0])
	if a.status != http.StatusUnauthorized {
		t.Fatalf("%d", a.status)
	}
	var lines []map[string]any
	for _, l := range f.logLines(offset) {
		if l["msg"] == "authentication failed" {
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 || lines[0]["failures"] != 1.0 || lines[1]["failures"] != float64(n) {
		t.Errorf("authentication lines %v", lines)
	}
	log := f.log.String()
	for _, token := range tokens {
		if strings.Contains(log, token[13:]) {
			t.Fatal("the log holds a token")
		}
	}
	m := f.adminCall(http.MethodGet, "/admin/v1/metrics", "", metricsToken)
	if want := fmt.Sprintf("\nstargaite_unauthenticated_requests_total %d\n", n+1); m.status != http.StatusOK || !strings.Contains(m.body, want) {
		t.Errorf("metrics %d, want %q in %s", m.status, want, m.body)
	}
}
