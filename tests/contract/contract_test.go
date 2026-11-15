// Package contract runs the per-target cases of phase 2 against a live
// provider through the proxy: a simple request, a stream, structured output
// and tools when the target declares them, each declared level, a provider
// error, a client that goes away mid-stream (RES-5), toward the anthropic
// protocol /v1/messages with a thinking tool loop (AC-02), and the usage
// cases of phase 3 (TestUsage). It runs only when STARGAITE_CONTRACT_BASE_URL and
// STARGAITE_CONTRACT_API_KEY are set, so make ci and a fork with no secret
// skip it (PUB-4, PUB-5); it costs tokens. The variables:
//
//	STARGAITE_CONTRACT_BASE_URL  the provider's base_url (HTTPS, public)
//	STARGAITE_CONTRACT_API_KEY   its credential; never printed
//	STARGAITE_CONTRACT_MODEL     the upstream model id (required)
//	STARGAITE_CONTRACT_PROTOCOL  openai or anthropic (default anthropic)
//	STARGAITE_CONTRACT_TARGET    the target declaration, a YAML flow mapping
//	                             (default "{ kind: chat }"), as in a
//	                             configuration: its levels and capabilities
//	                             choose the cases
//	STARGAITE_CONTRACT_PROVIDER  more members of the provider declaration, as
//	                             in a YAML flow mapping without its braces:
//	                             the preset's stream_usage and cost_report
//	                             under test (providers presets --json shows
//	                             them), for example "stream_usage: request,
//	                             cost_report: { path: usage.cost, usd_per_unit: 1 }";
//	                             protocol, base_url, api_key_env and billing
//	                             (pay_per_use) are the test's
package contract

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/admin"
	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/api"
	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/store"
	"github.com/jorlaru/stargaite/internal/transport"
	"github.com/jorlaru/stargaite/internal/usage"
)

const adminKey = "contract-admin-key-of-at-least-32-characters"

// proxy is both listeners of an in-process proxy in front of the live
// provider, with one client key that reaches every alias.
type proxy struct {
	data, admin http.Handler
	token, key  string // the client token; the provider's credential, kept out of every message
	doc         domain.Document
	target      string
	flight      *sync.WaitGroup // the data plane's requests in flight
	db          *sql.DB         // the store, where the usage records land
}

func live(t *testing.T) *proxy {
	t.Helper()
	base, key := os.Getenv("STARGAITE_CONTRACT_BASE_URL"), os.Getenv("STARGAITE_CONTRACT_API_KEY")
	if base == "" || key == "" {
		t.Skip("STARGAITE_CONTRACT_BASE_URL and STARGAITE_CONTRACT_API_KEY are not set: no live provider")
	}
	model := os.Getenv("STARGAITE_CONTRACT_MODEL")
	if model == "" {
		t.Fatal("STARGAITE_CONTRACT_MODEL is not set")
	}
	protocol := os.Getenv("STARGAITE_CONTRACT_PROTOCOL")
	if protocol == "" {
		protocol = domain.ProtocolAnthropic
	}
	target := os.Getenv("STARGAITE_CONTRACT_TARGET")
	if target == "" {
		target = "{ kind: chat }"
	}
	provider := ""
	if more := os.Getenv("STARGAITE_CONTRACT_PROVIDER"); more != "" {
		provider = ", " + more
	}

	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	resolve := func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	env := func() config.Env {
		return config.Env{Lookup: func(name string) (string, bool) { return key, name == "CONTRACT_KEY" }, Resolve: resolve}
	}
	cfg, err := config.Open(context.Background(), st, env, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler) // a provider's message may be logged; the test shows only redacted answers
	limits := admission.New(cfg.Now, usage.Seeder(st.Read(), log))
	p := &proxy{
		admin: admin.Handler(admin.Deps{Config: cfg, KeyHash: sha256.Sum256([]byte(adminKey)), Log: log, Usage: st.Read(), Admission: limits}),
		key:   key, target: "live/" + model, flight: &sync.WaitGroup{}, db: st.Read(),
	}
	records := usage.NewWriter(st.Write, log)
	t.Cleanup(func() { records.Close(context.Background()) })
	p.data = api.Handler(api.Deps{Config: cfg, Log: log,
		Clients: [2]*http.Client{transport.NewClient(false, resolve), transport.NewClient(true, resolve)}, Flight: p.flight,
		Usage: records, Admission: limits, Counters: &usage.Counters{}})
	// A cap on every alias: the anthropic protocol needs one (PAR-7), and
	// the budgets of a thinking_budget target must stay below it (RSN-3).
	// provider_default sends no effort unless the request has one, and is
	// what a target that cannot disable reasoning needs (RSN-7). Billing
	// pay_per_use lets a record take the provider's cost (USE-6).
	yaml := fmt.Sprintf(`version: 1
providers:
  live: { protocol: %s, base_url: %q, api_key_env: CONTRACT_KEY, billing: pay_per_use%s }
targets:
  %q: %s
  %q: { kind: chat }
models:
  live: { targets: [%q], max_output_tokens: 8192, reasoning: { effort: provider_default, mode: default } }
  missing: { targets: [%q], max_output_tokens: 8192, reasoning: { effort: provider_default, mode: default } }
keys:
  contract: { models: ["*"] }
`, protocol, base, provider, p.target, target, "live/stargaite-contract-no-such-model", p.target, "live/stargaite-contract-no-such-model")
	w := p.call(p.admin, http.MethodPost, "/admin/v1/apply", yaml, "Authorization", "Bearer "+adminKey)
	var res config.Result
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &res) != nil {
		t.Fatalf("apply: %d %s", w.Code, p.show(w.Body.Bytes()))
	}
	p.token = res.Secrets["contract"]
	w = p.call(p.admin, http.MethodGet, "/admin/v1/export?format=json", "", "Authorization", "Bearer "+adminKey)
	if json.Unmarshal(w.Body.Bytes(), &p.doc) != nil {
		t.Fatalf("export: %s", p.show(w.Body.Bytes()))
	}
	return p
}

func (p *proxy) call(h http.Handler, method, path, body string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// show is an answer as the test may print it: the proxy redacts the
// credential, and so does the test, in case a provider spelled it otherwise.
func (p *proxy) show(b []byte) string {
	return strings.ReplaceAll(string(b), p.key, "[redacted]")
}

// chat sends a chat completion to alias with the members extra.
func (p *proxy) chat(t *testing.T, alias, extra string) (int, http.Header, []byte) {
	t.Helper()
	w := p.call(p.data, http.MethodPost, "/v1/chat/completions",
		`{"model":"`+alias+`","messages":[{"role":"user","content":"Answer in one word: what colour is the sky?"}]`+extra+`}`,
		"Authorization", "Bearer "+p.token)
	return w.Code, w.Header(), w.Body.Bytes()
}

// messages sends a Messages request with the client key in x-api-key.
func (p *proxy) messages(t *testing.T, body string) (int, []byte) {
	t.Helper()
	w := p.call(p.data, http.MethodPost, "/v1/messages", body, "X-Api-Key", p.token)
	return w.Code, w.Body.Bytes()
}

// TestChat is the chat completion cases, toward either protocol.
func TestChat(t *testing.T) {
	p := live(t)
	decl := p.doc.Targets[p.target]
	declared := func(b *bool) bool { return b != nil && *b }
	type reply struct {
		Choices []struct {
			Message struct {
				Content   *string
				ToolCalls []struct{ Function struct{ Arguments string } } `json:"tool_calls"`
			}
			FinishReason string `json:"finish_reason"`
		}
		Usage *struct {
			Total int `json:"total_tokens"`
		}
	}
	ok := func(name string, status int, body []byte) reply {
		t.Helper()
		var r reply
		if err := json.Unmarshal(body, &r); status != http.StatusOK || err != nil || len(r.Choices) == 0 || r.Choices[0].FinishReason == "" {
			t.Errorf("%s: %d %s", name, status, p.show(body))
		}
		return r
	}

	status, _, body := p.chat(t, "live", "")
	if r := ok("simple", status, body); len(r.Choices) > 0 && (r.Choices[0].Message.Content == nil || r.Usage == nil) {
		t.Errorf("simple: no content or usage: %s", p.show(body))
	}

	status, _, body = p.chat(t, "live", `,"stream":true,"stream_options":{"include_usage":true}`)
	if text := string(body); status != http.StatusOK || !strings.HasSuffix(text, "data: [DONE]\n\n") || !strings.Contains(text, `"usage":{`) {
		t.Errorf("stream: %d %s", status, p.show(body))
	}
	for e := range strings.SplitSeq(strings.TrimSuffix(string(body), "\n\n"), "\n\n") {
		if data, found := strings.CutPrefix(e, "data: "); found && data != "[DONE]" && !json.Valid([]byte(data)) {
			t.Errorf("stream: an event that is not JSON: %q", p.show([]byte(e)))
		}
	}

	if decl.Reasoning != nil {
		for _, level := range decl.Reasoning.Levels {
			status, h, body := p.chat(t, "live", `,"reasoning_effort":"`+level+`"`)
			ok("level "+level, status, body)
			if h.Get("X-Stargaite-Reasoning-Effort") == "" {
				t.Errorf("level %s: no effort sent, changes %q", level, h.Get("X-Stargaite-Params-Changed"))
			}
		}
	}
	if declared(decl.Capabilities.StructuredOutput) {
		status, _, body := p.chat(t, "live", `,"response_format":{"type":"json_schema","json_schema":{"name":"sky",`+
			`"schema":{"type":"object","properties":{"colour":{"type":"string"}},"required":["colour"],"additionalProperties":false}}}`)
		if r := ok("structured output", status, body); len(r.Choices) > 0 && (r.Choices[0].Message.Content == nil || !json.Valid([]byte(*r.Choices[0].Message.Content))) {
			t.Errorf("structured output: the content is not JSON: %s", p.show(body))
		}
	}
	if declared(decl.Capabilities.Tools) {
		status, _, body := p.chat(t, "live", `,"tools":[{"type":"function","function":{"name":"report_colour","description":"Report the colour of the sky",`+
			`"parameters":{"type":"object","properties":{"colour":{"type":"string"}},"required":["colour"]}}}]`)
		for _, c := range ok("tools", status, body).Choices {
			for _, call := range c.Message.ToolCalls {
				if !json.Valid([]byte(call.Function.Arguments)) {
					t.Errorf("tools: arguments that are not JSON: %s", p.show(body))
				}
			}
		}
	}

	// A provider error passes through with its status (section 15).
	status, h, body := p.chat(t, "missing", "")
	if status != http.StatusBadRequest && status != http.StatusNotFound && status != http.StatusUnprocessableEntity || !strings.Contains(string(body), `"code":"upstream_error"`) ||
		h.Get("X-Stargaite-Target") != "live/stargaite-contract-no-such-model" {
		t.Errorf("unknown upstream model: %d %s", status, p.show(body))
	}
}

// record is the usage record of a provider call that answered 2xx.
type record struct {
	input, cacheRead, cacheWrite, output int64
	reasoning                            *int64
	tokens, costSource                   string
	cost                                 *float64
}

func (r record) String() string {
	reasoning, cost := "NULL", "NULL"
	if r.reasoning != nil {
		reasoning = fmt.Sprint(*r.reasoning)
	}
	if r.cost != nil {
		cost = fmt.Sprint(*r.cost)
	}
	return fmt.Sprintf("input %d (cache read %d, write %d), output %d (reasoning %s), tokens %s, cost %s from %s",
		r.input, r.cacheRead, r.cacheWrite, r.output, reasoning, r.tokens, cost, r.costSource)
}

// record reads the record of the 2xx call of trace, once the writer has
// committed it (a batch takes 50 ms).
func (p *proxy) record(t *testing.T, trace string) record {
	t.Helper()
	var r record
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		err := p.db.QueryRow(`SELECT input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens,
			tokens_source, cost_usd, cost_source FROM usage WHERE trace_id = ? AND status_code BETWEEN 200 AND 299`, trace).
			Scan(&r.input, &r.cacheRead, &r.cacheWrite, &r.output, &r.reasoning, &r.tokens, &r.cost, &r.costSource)
		if err == nil {
			return r
		}
		if !errors.Is(err, sql.ErrNoRows) || time.Now().After(deadline) {
			t.Fatalf("the record of trace %q: %v", trace, err)
		}
	}
}

// TestUsage is the usage cases of phase-3 design section 14: the usage a
// synchronous answer reports gives the record the USE-3 totals, and the
// provider's own total is input plus output, so no part counts twice or goes
// missing; a stream whose client did not ask for usage records the
// provider's tokens when the declared stream_usage says they arrive (native;
// request, which the proxy then asks for; and toward anthropic unless
// declared none) and estimated ones under none; and, when the provider declares a cost_report,
// its path holds a number in the answer and the record's cost is that figure
// with source provider, for the stream too once its usage arrived (USE-6).
func TestUsage(t *testing.T) {
	p := live(t)
	prov := p.doc.Providers["live"]
	anthropic := prov.Protocol == domain.ProtocolAnthropic
	path, auth := "/v1/chat/completions", "Authorization"
	ask := `{"model":"live","messages":[{"role":"user","content":"Answer in one word: what colour is the sky?"}]`
	if anthropic {
		path, auth = "/v1/messages", "X-Api-Key"
		ask = `{"model":"live","max_tokens":64,"messages":[{"role":"user","content":"Say hi."}]`
	}
	token := p.token
	if !anthropic {
		token = "Bearer " + token
	}
	w := p.call(p.data, http.MethodPost, path, ask+"}", auth, token)
	var reply struct {
		Usage *struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
			Total      int64 `json:"total_tokens"`
			Details    struct {
				Reasoning *int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
			Input      int64 `json:"input_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
			Output     int64 `json:"output_tokens"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); w.Code != http.StatusOK || err != nil || reply.Usage == nil {
		t.Fatalf("synchronous answer without usage: %d %s", w.Code, p.show(w.Body.Bytes()))
	}
	u, r := reply.Usage, p.record(t, w.Header().Get("X-Stargaite-Trace-Id"))
	if r.tokens != "provider" {
		t.Errorf("synchronous answer: tokens %s, not the provider's", r.tokens)
	}
	if anthropic {
		if r.input != u.Input+u.CacheRead+u.CacheWrite || r.cacheRead != u.CacheRead || r.cacheWrite != u.CacheWrite || r.output != u.Output {
			t.Errorf("USE-3: recorded %v from %s", r, p.show(w.Body.Bytes()))
		}
	} else {
		if r.input != u.Prompt || r.output != max(u.Completion, u.Total-u.Prompt) || (u.Details.Reasoning == nil) != (r.reasoning == nil) ||
			u.Details.Reasoning != nil && *u.Details.Reasoning != *r.reasoning {
			t.Errorf("USE-3: recorded %v from %s", r, p.show(w.Body.Bytes()))
		}
		if u.Total != 0 && u.Prompt+r.output != u.Total {
			t.Errorf("USE-3: the reported total %d is not input %d plus output %d", u.Total, u.Prompt, r.output)
		}
	}
	if r.cacheRead+r.cacheWrite > r.input || r.reasoning != nil && *r.reasoning > r.output {
		t.Errorf("USE-3: a part above its total: %v", r)
	}
	if prov.CostReport != nil {
		var v any = map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		for name := range strings.SplitSeq(prov.CostReport.Path, ".") {
			m, _ := v.(map[string]any)
			v = m[name]
		}
		if f, ok := v.(float64); !ok || f < 0 || r.costSource != "provider" || r.cost == nil || *r.cost != f*prov.CostReport.USDPerUnit {
			t.Errorf("cost_report %s: the answer holds %v, the record %v", prov.CostReport.Path, v, r)
		}
	}

	w = p.call(p.data, http.MethodPost, path, ask+`,"stream":true}`, auth, token)
	if w.Code != http.StatusOK {
		t.Fatalf("stream: %d %s", w.Code, p.show(w.Body.Bytes()))
	}
	r = p.record(t, w.Header().Get("X-Stargaite-Trace-Id"))
	arrives := prov.StreamUsage != domain.StreamUsageNone && (anthropic || prov.StreamUsage == domain.StreamUsageNative || prov.StreamUsage == domain.StreamUsageRequest)
	switch {
	case !anthropic && prov.StreamUsage == "":
		t.Logf("stream_usage is undeclared; a stream that did not ask for usage recorded %s tokens (provider: native fits; estimated: run again with request)", r.tokens)
	case arrives && r.tokens != "provider":
		t.Errorf("stream_usage %q: the stream's usage did not arrive (tokens %s)", prov.StreamUsage, r.tokens)
	case !arrives && r.tokens != "estimated":
		t.Errorf("stream_usage none: the provider sent usage unasked (tokens %s): native fits", r.tokens)
	}
	if prov.CostReport != nil && r.tokens == "provider" && r.costSource != "provider" {
		t.Errorf("cost_report %s: the stream's usage arrived without the cost (source %s)", prov.CostReport.Path, r.costSource)
	}
}

// TestCancel is RES-5 against the live provider: a client that goes away
// after the first line of a stream ends the proxy's provider call, its
// finish and done included, within 1 s; the request in flight is the only
// one of this proxy.
func TestCancel(t *testing.T) {
	p := live(t)
	srv := httptest.NewServer(p.data) // a real listener, so the client can go away
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"live","stream":true,"messages":[{"role":"user","content":"Count from 1 to 500, one number per line."}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(resp.Body).ReadString('\n'); resp.StatusCode != http.StatusOK || err != nil {
		t.Fatalf("stream: %d %s: %v", resp.StatusCode, p.show([]byte(line)), err)
	}
	_ = resp.Body.Close() // before the end of the body: the connection closes
	ended := make(chan struct{})
	go func() { p.flight.Wait(); close(ended) }()
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("the provider call did not end within 1 s of the client going away")
	}
}

// TestMessages is the /v1/messages cases toward an anthropic target: a
// simple request, a stream and, for a target that reasons and takes tools, a
// tool loop with thinking whose second turn sends the thinking blocks and
// their signatures back as they came (AC-02, RSN-12).
func TestMessages(t *testing.T) {
	p := live(t)
	if os.Getenv("STARGAITE_CONTRACT_PROTOCOL") == domain.ProtocolOpenAI {
		t.Skip("/v1/messages serves anthropic targets only")
	}
	decl := p.doc.Targets[p.target]
	status, body := p.messages(t, `{"model":"live","max_tokens":64,"messages":[{"role":"user","content":"Say hi."}]}`)
	var m struct {
		Type       string
		Content    []json.RawMessage
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(body, &m); status != http.StatusOK || err != nil || m.Type != "message" || len(m.Content) == 0 || m.StopReason == "" {
		t.Errorf("simple: %d %s", status, p.show(body))
	}

	status, body = p.messages(t, `{"model":"live","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Say hi."}]}`)
	if text := string(body); status != http.StatusOK || !strings.HasPrefix(text, "event: message_start\n") ||
		!strings.HasSuffix(text, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n") {
		t.Errorf("stream: %d %s", status, p.show(body))
	}

	dialect := ""
	if decl.Reasoning != nil {
		dialect = decl.Reasoning.Dialect
	}
	if decl.Capabilities.Tools == nil || !*decl.Capabilities.Tools || dialect != domain.DialectAdaptiveEffort && dialect != domain.DialectThinkingBudget {
		t.Log("the tool loop with thinking needs a target that declares tools and an adaptive_effort or thinking_budget dialect")
		return
	}
	effort := `"output_config":{"effort":"medium"}`
	if dialect == domain.DialectThinkingBudget {
		effort = `"thinking":{"type":"enabled","budget_tokens":1024}`
	}
	tools := `"tools":[{"name":"get_weather","description":"Current weather in a city","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]`
	ask := `{"role":"user","content":"What is the weather in Paris? Use the tool."}`
	status, body = p.messages(t, `{"model":"live","max_tokens":4096,`+effort+`,`+tools+`,"messages":[`+ask+`]}`)
	var first struct {
		Content []struct {
			Type, ID string
		}
		StopReason string `json:"stop_reason"`
	}
	var raw struct{ Content json.RawMessage }
	if err := json.Unmarshal(body, &first); status != http.StatusOK || err != nil || json.Unmarshal(body, &raw) != nil {
		t.Fatalf("tool loop, first turn: %d %s", status, p.show(body))
	}
	id, thought := "", false
	for _, b := range first.Content {
		switch b.Type {
		case "tool_use":
			id = b.ID
		case "thinking", "redacted_thinking":
			thought = true
		}
	}
	if id == "" {
		t.Fatalf("tool loop: no tool_use in %s", p.show(body))
	}
	if !thought {
		t.Skip("tool loop: the provider sent no thinking block, so AC-02 was not exercised")
	}
	status, body = p.messages(t, `{"model":"live","max_tokens":4096,`+effort+`,`+tools+`,"messages":[`+ask+
		`,{"role":"assistant","content":`+string(raw.Content)+`},{"role":"user","content":[{"type":"tool_result","tool_use_id":"`+id+`","content":"18 C and sunny"}]}]}`)
	if status != http.StatusOK {
		t.Errorf("tool loop, second turn: %d %s", status, p.show(body))
	}
}
