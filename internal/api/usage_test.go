package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/usage"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// row is one usage row as the tests read it back; a nil pointer is NULL.
type row struct {
	ID, RequestID, TraceID          string
	Attempt                         int
	Endpoint, Key, Model, Target    string
	Billing                         string
	ResponseModel                   *string
	Mismatch                        bool
	Status                          int
	ErrorType, FinishReason         *string
	Stream                          bool
	Input, CacheRead, CacheWrite    int64
	Output                          int64
	Reasoning                       *int64
	TokensSource                    string
	Cost                            *float64
	CostSource                      string
	EffortSent                      *string
	MaxOutputSent                   *int64
	ParamsChanged                   *string
	Empty                           bool
	TTFT                            *int64
	StartedAt, CompletedAt, Version int64
	ProxyVersion                    string
}

// stored waits up to 5 s for n usage rows, a batch committing 50 ms after
// its first record, and returns them in the order they were inserted.
func (f *fixture) stored(t *testing.T, n int) []row {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var got int
		if err := f.st.Read().QueryRow("SELECT COUNT(*) FROM usage").Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == n {
			break
		}
		if got > n || time.Now().After(deadline) {
			t.Fatalf("%d usage rows, want %d", got, n)
		}
	}
	rows, err := f.st.Read().Query(`SELECT id, request_id, trace_id, attempt, endpoint, key_name, model, target, billing, response_model,
		model_mismatch, status_code, error_type, finish_reason, stream, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens,
		reasoning_tokens, tokens_source, cost_usd, cost_source, effort_sent, max_output_sent, params_changed, content_empty, ttft_ms,
		started_at, completed_at, config_revision, proxy_version FROM usage ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.RequestID, &r.TraceID, &r.Attempt, &r.Endpoint, &r.Key, &r.Model, &r.Target, &r.Billing, &r.ResponseModel,
			&r.Mismatch, &r.Status, &r.ErrorType, &r.FinishReason, &r.Stream, &r.Input, &r.CacheRead, &r.CacheWrite, &r.Output,
			&r.Reasoning, &r.TokensSource, &r.Cost, &r.CostSource, &r.EffortSent, &r.MaxOutputSent, &r.ParamsChanged, &r.Empty, &r.TTFT,
			&r.StartedAt, &r.CompletedAt, &r.Version, &r.ProxyVersion); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func str(p *string) string { return deref(p) }

func num(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func money(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}

const usageConfig = `version: 1
providers:
  pay: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true }
  rep: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true, cost_report: { path: usage.cost, usd_per_unit: 1 } }
  sub: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: subscription, allow_private_network: true }
  anth: { protocol: anthropic, base_url: MOCK, api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true }
targets:
  pay/up-1: { kind: chat, pricing: { input_per_million: 1, output_per_million: 2, cache_read_per_million: 0.5, as_of: "2026-11-01" }, response_models: [alias-, up-2] }
  pay/free: { kind: chat }
  rep/up-2: { kind: chat, pricing: { input_per_million: 1, output_per_million: 2, as_of: "2026-11-01" } }
  sub/plan: { kind: chat }
  anth/claude: { kind: chat, pricing: { input_per_million: 3, output_per_million: 15, as_of: "2026-11-01" } }
models:
  priced: { targets: [pay/up-1], max_output_tokens: 100 }
  free: { targets: [pay/free] }
  reported: { targets: [rep/up-2] }
  plan: { targets: [sub/plan] }
  claude: { targets: [anth/claude], max_output_tokens: 100 }
  fallback: { targets: [pay/up-1, rep/up-2], max_output_tokens: 100 }
keys:
  agent: { models: ["*"] }
`

// TestRecords is AC-32, AC-33 and AC-40 (records): one record per provider
// call, successful, failed, aborted and fallen back alike, with the
// reasoning tokens a part of the output total (USE-3), the finish reason
// and the returned model, a mismatch flagged (USE-4); tokens recorded
// without a price, each cost with its source and the provider's figure over
// the price table, estimated tokens marked (USE-6, USE-8); the proxy version
// and the configuration revision, on the service clock. The OPS-8 counters
// count the same calls, and an unknown alias under no model.
func TestRecords(t *testing.T) {
	f := newFixture(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	tokens := f.apply(t, strings.ReplaceAll(usageConfig, "base_url: MOCK", "base_url: "+srv.URL))
	auth := "Bearer " + tokens["agent"]
	// An empty change answers the revision it keeps (OPS-4).
	var revision struct{ Revision int64 }
	if w := call(f.admin, "Bearer "+adminKey, http.MethodPatch, "/admin/v1/settings", []byte(`{}`)); json.Unmarshal(w.Body.Bytes(), &revision) != nil || revision.Revision == 0 {
		t.Fatalf("settings: %s", w.Body)
	}

	reply := func(model, usage string) []byte {
		return []byte(`{"id":"x","object":"chat.completion","created":0,"model":"` + model + `","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"length"}],"usage":` + usage + `}`)
	}
	mock.Script(
		mockprovider.Step{Body: reply("other-model", `{"prompt_tokens":10,"completion_tokens":4,"total_tokens":20,"completion_tokens_details":{"reasoning_tokens":6},"prompt_tokens_details":{"cached_tokens":4}}`)},
		mockprovider.Step{Status: 500},
		mockprovider.Step{Body: reply("up-2", `{"prompt_tokens":10,"completion_tokens":5,"cost":0.42}`)},
		mockprovider.Step{Body: reply("free", `{"prompt_tokens":7,"completion_tokens":3}`)},
		mockprovider.Step{Body: reply("plan", `{"prompt_tokens":7,"completion_tokens":3}`)},
		mockprovider.Step{Chunks: sse(chunk("ab"), `{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`, "[DONE]")},
		mockprovider.Step{Chunks: sse(chunk("abcdefgh")), Cut: true},
		mockprovider.Step{Status: 503},
		mockprovider.Step{Body: reply("up-2", `{"prompt_tokens":1,"completion_tokens":1}`)},
		mockprovider.Step{Chunks: events("message_start", messageStart, "content_block_start", textStart, "content_block_delta", textDelta("hi"),
			"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`, "message_stop", messageStop)},
	)
	type sent struct {
		alias, extra string
		status       int
		messages     bool
	}
	ids := map[string]string{}
	for _, s := range []sent{
		{"priced", "", 200, false}, {"priced", "", 502, false}, {"reported", "", 200, false}, {"free", "", 200, false}, {"plan", "", 200, false},
		{"priced", `,"stream":true`, 200, false}, {"priced", `,"stream":true`, 200, false}, {"fallback", "", 200, false}, {"claude", `,"stream":true`, 200, true},
	} {
		var w *httptest.ResponseRecorder
		if s.messages {
			w = f.messages(messagesBody(s.alias, s.extra), "X-Api-Key", tokens["agent"])
		} else {
			w = f.chat(auth, chatBody(s.alias, s.extra))
		}
		if w.Code != s.status {
			t.Fatalf("%s%s: %d %s", s.alias, s.extra, w.Code, w.Body)
		}
		ids[s.alias+s.extra+w.Header().Get(targetHeader)] = w.Header().Get(domain.RequestIDHeader)
	}
	// An unknown alias makes no record, and its request is counted under no model.
	if w := f.chat(auth, chatBody("nope", "")); w.Code != http.StatusNotFound {
		t.Fatalf("nope: %d %s", w.Code, w.Body)
	}
	waitFlight(t, f)
	rows := f.stored(t, 10)

	type want struct {
		model, target, billing, responseModel, errorType, finish string
		status                                                   int
		stream, mismatch                                         bool
		in, read, out, reasoning                                 int64
		tokens                                                   string
		cost                                                     float64
		costSource                                               string
	}
	priced := func(in, read, out int64) float64 {
		return (float64(in-read)*1 + float64(read)*0.5 + float64(out)*2) / 1e6
	}
	wants := []want{
		{"priced", "pay/up-1", "pay_per_use", "other-model", "", "length", 200, false, true, 10, 4, 10, 6, "provider", priced(10, 4, 10), "price_table"},
		{"priced", "pay/up-1", "pay_per_use", "", "upstream_error", "", 500, false, false, 0, 0, 0, -1, "provider", 0, "price_table"},
		{"reported", "rep/up-2", "pay_per_use", "up-2", "", "length", 200, false, false, 10, 0, 5, -1, "provider", 0.42, "provider"},
		{"free", "pay/free", "pay_per_use", "free", "", "length", 200, false, false, 7, 0, 3, -1, "provider", -1, "none"},
		{"plan", "sub/plan", "subscription", "plan", "", "length", 200, false, false, 7, 0, 3, -1, "provider", -1, "none"},
		{"priced", "pay/up-1", "pay_per_use", "up-2", "", "", 200, true, false, 5, 0, 2, -1, "provider", priced(5, 0, 2), "price_table"},
		// Cut after one event: estimated from the bytes sent and the 8 visible bytes.
		{"priced", "pay/up-1", "pay_per_use", "up-2", "upstream_error", "", 200, true, false, -2, 0, 2, -1, "estimated", -2, "price_table"},
		{"fallback", "pay/up-1", "pay_per_use", "", "upstream_error", "", 503, false, false, 0, 0, 0, -1, "provider", 0, "price_table"},
		{"fallback", "rep/up-2", "pay_per_use", "up-2", "", "length", 200, false, false, 1, 0, 1, -1, "provider", 3.0 / 1e6, "price_table"},
		{"claude", "anth/claude", "pay_per_use", "claude-up-2026", "", "end_turn", 200, true, false, 3, 0, 4, -1, "provider", (3*3.0 + 4*15.0) / 1e6, "price_table"},
	}
	for i, r := range rows {
		w := wants[i]
		if w.in == -2 { // the estimate of the cut stream, from the bytes the mock received
			body := mock.Calls()[6].Body
			w.in = int64((len(body) + 3) / 4)
			w.cost = priced(w.in, 0, 2)
		}
		got := want{r.Model, r.Target, r.Billing, str(r.ResponseModel), str(r.ErrorType), str(r.FinishReason), r.Status, r.Stream, r.Mismatch,
			r.Input, r.CacheRead, r.Output, num(r.Reasoning), r.TokensSource, money(r.Cost), r.CostSource}
		if math.Abs(got.cost-w.cost) < 1e-12 {
			got.cost = w.cost
		}
		if got != w {
			t.Errorf("record %d:\n got %+v\nwant %+v", i+1, got, w)
		}
		if r.ProxyVersion != "test-version" || r.Version != revision.Revision || r.TraceID == "" || r.CompletedAt != testNow ||
			r.StartedAt != testNow || r.ID != r.RequestID+"-"+map[bool]string{true: "2", false: "1"}[i == 8] {
			t.Errorf("record %d: id %s, version %q, revision %d (want %d), trace %q, times %d %d", i+1, r.ID, r.ProxyVersion, r.Version, revision.Revision,
				r.TraceID, r.StartedAt, r.CompletedAt)
		}
		if (r.TTFT != nil) != r.Stream {
			t.Errorf("record %d: ttft %v on a stream %v", i+1, num(r.TTFT), r.Stream)
		}
	}
	// The fallback's two calls share the request id.
	if rows[7].RequestID != rows[8].RequestID || rows[7].RequestID != ids["fallbackrep/up-2"] || rows[7].Attempt != 1 || rows[8].Attempt != 2 {
		t.Errorf("fallback records %+v %+v", rows[7], rows[8])
	}

	counted := map[string]float64{}
	f.counters.Each(func(c usage.Counter, v float64) {
		counted[c.Name+"/"+c.Key+"/"+c.Model+"/"+c.Label] = v
		if c.Model == "nope" {
			t.Errorf("counter %+v carries the unknown alias", c)
		}
	})
	for name, v := range map[string]float64{
		"requests/agent/priced/200": 2, "requests/agent/priced/502": 2, "requests/agent/fallback/200": 1, "requests/agent/claude/200": 1, "requests/agent//404": 1,
		"model_mismatch/agent/priced/": 1, "unpriced/agent/free/": 1, "length/agent/priced/": 1, "tokens/agent/plan/output": 3,
		"tokens/agent/priced/reasoning": 6, "params_changed/agent/priced/added": 4, "empty/agent/priced/": 0,
	} {
		if counted[name] != v {
			t.Errorf("counter %s = %v, want %v; all %v", name, counted[name], v, counted)
		}
	}
	if counted["cost_usd/agent/reported/"] != 0.42 || counted["cost_usd/agent/plan/"] != 0 {
		t.Errorf("cost counters %v", counted)
	}
}

// TestCutCallCost: a pay_per_use call whose request was sent and that got no
// answer, here cut by the total timer, has no cost and counts as unpriced,
// although its target has prices; a dial refused before anything was sent, a
// request whose write failed on a connection the provider reset, and a
// provider's 400 keep their known cost of 0 from the price table (USE-6,
// USE-12, phase-5 design C49).
func TestCutCallCost(t *testing.T) {
	f := newFixture(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(slow.Close)
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"no"}}`))
	}))
	t.Cleanup(refusing.Close)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := closed.Addr().String()
	_ = closed.Close()
	// A peer that accepts each connection and resets it: a request of several
	// MiB, more than the socket buffers hold, fails while it is written.
	// A regression guard (phase-5 design C52): passes on bc85529, fails when the
	// WroteRequest hook marks a failed write as sent.
	reset, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reset.Close() })
	go func() {
		for {
			c, err := reset.Accept()
			if err != nil {
				return
			}
			_ = c.(*net.TCPConn).SetLinger(0)
			_ = c.Close()
		}
	}()
	tokens := f.apply(t, `version: 1
settings: { timeouts: { total_ms: 300 } }
providers:
  slow: { protocol: openai, base_url: "`+slow.URL+`", api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true }
  refused: { protocol: openai, base_url: "http://`+refused+`", api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true }
  bad: { protocol: openai, base_url: "`+refusing.URL+`", api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true }
  reset: { protocol: openai, base_url: "http://`+reset.Addr().String()+`", api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true }
targets:
  slow/m: { kind: chat, pricing: { input_per_million: 1, output_per_million: 2, as_of: "2026-11-01" } }
  refused/m: { kind: chat, pricing: { input_per_million: 1, output_per_million: 2, as_of: "2026-11-01" } }
  bad/m: { kind: chat, pricing: { input_per_million: 1, output_per_million: 2, as_of: "2026-11-01" } }
  reset/m: { kind: chat, pricing: { input_per_million: 1, output_per_million: 2, as_of: "2026-11-01" } }
models:
  slow: { targets: [slow/m] }
  refused: { targets: [refused/m] }
  bad: { targets: [bad/m] }
  reset: { targets: [reset/m] }
keys:
  agent: { models: ["*"] }
`)
	big := `{"model":"reset","messages":[{"role":"user","content":"` + strings.Repeat("x", 5<<20) + `"}]}`
	for alias, status := range map[string]int{"slow": http.StatusGatewayTimeout, "refused": http.StatusBadGateway, "bad": http.StatusBadRequest, "reset": http.StatusBadGateway} {
		body := chatBody(alias, "")
		if alias == "reset" {
			body = big
		}
		if w := f.chat("Bearer "+tokens["agent"], body); w.Code != status {
			t.Fatalf("%s: %d %s", alias, w.Code, w.Body)
		}
	}
	waitFlight(t, f)
	got := map[string]string{}
	for _, r := range f.stored(t, 4) {
		got[r.Model] = fmt.Sprint(r.Status, " ", money(r.Cost), " ", r.CostSource) // -1: NULL
	}
	if want := map[string]string{"slow": "0 -1 none", "refused": "0 0 price_table", "bad": "400 0 price_table", "reset": "0 0 price_table"}; !reflect.DeepEqual(got, want) {
		t.Errorf("records %v, want %v", got, want)
	}
	unpriced := map[string]float64{}
	f.counters.Each(func(c usage.Counter, v float64) {
		if c.Name == "unpriced" {
			unpriced[c.Model] = v
		}
	})
	if want := map[string]float64{"slow": 1}; !reflect.DeepEqual(unpriced, want) {
		t.Errorf("unpriced counters %v, want %v", unpriced, want)
	}
}

// TestRecordTTFT is ttft_ms (design section 5): from the call's start to its
// first event, not to the provider's headers, and a comment line is no event.
// The service clock moves 300 ms between the comment and the first data event.
func TestRecordTTFT(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": ping\n\n"))
		w.(http.Flusher).Flush()
		time.Sleep(100 * time.Millisecond) // the proxy reads the headers and the comment first
		f.now.Add(300)
		for _, b := range sse(chunk("hi"), "[DONE]") {
			_, _ = w.Write(b)
		}
	}))
	t.Cleanup(srv.Close)
	tokens := f.apply(t, strings.ReplaceAll(usageConfig, "base_url: MOCK", "base_url: "+srv.URL))
	if w := f.chat("Bearer "+tokens["agent"], streamBody("priced")); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	waitFlight(t, f)
	if r := f.stored(t, 1)[0]; num(r.TTFT) != 300 {
		t.Errorf("ttft_ms %d, want 300", num(r.TTFT))
	}
}

const storeConfig = `version: 1
providers:
  pay: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true }
  payopen: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true }
  payoff: { protocol: openai, base_url: MOCK, api_key_env: MISSING_KEY, billing: pay_per_use, allow_private_network: true }
  free: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: none, allow_private_network: true }
  freeoff: { protocol: openai, base_url: MOCK, api_key_env: MISSING_KEY, billing: none, allow_private_network: true }
  sub: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: subscription, allow_private_network: true }
targets:
  pay/m: { kind: chat }
  payopen/m: { kind: chat }
  payoff/m: { kind: chat }
  free/m: { kind: chat }
  free/notools: { kind: chat, capabilities: { tools: false } }
  freeoff/m: { kind: chat }
  sub/m: { kind: chat }
models:
  pay: { targets: [pay/m] }
  payopen: { targets: [payopen/m] }
  pay-free: { targets: [pay/m, free/m] }
  free: { targets: [free/m] }
  plan: { targets: [sub/m] }
  payoff: { targets: [payoff/m] }
  freeoff-pay: { targets: [freeoff/m, pay/m] }
  notools-pay: { targets: [free/notools, pay/m] }
  free-pay: { targets: [free/m, pay/m] }
keys:
  agent: { models: ["*"] }
`

// TestStoreUnavailable is USE-10 and RES-10 with the store (AC-34): a
// trigger makes every usage insert fail, and from the third failed commit
// pay_per_use targets are passed over: an alias with only one answers 503
// usage_store_unavailable without a call, one with a none target after it
// falls back to it, and subscription and none targets keep serving. RES-10
// order: the store comes before the credential and the circuit, the first
// able target's reason is the one answered (a target without credential
// before a pay_per_use one gives no_target_available, with phase 2's
// message), a static skip yields to it, and once a provider was called its
// error wins.
func TestStoreUnavailable(t *testing.T) {
	f := newFixture(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	tokens := f.apply(t, strings.ReplaceAll(storeConfig, "base_url: MOCK", "base_url: "+srv.URL))
	auth := "Bearer " + tokens["agent"]
	if w := f.chat(auth, chatBody("pay", "")); w.Code != http.StatusOK {
		t.Fatalf("pay with the store available: %d %s", w.Code, w.Body)
	}
	// Five failures open payopen/m's circuit: the store check must come
	// before the circuit, or the "payopen" case below gets
	// no_target_available (and a half-open circuit would lose a trial permit
	// to a store exclusion). pay/m's circuit stays closed, so the cases on
	// pay/m still show that the store alone excludes it.
	for i := 0; i < 5; i++ {
		mock.Script(mockprovider.Step{Status: 500})
		if w := f.chat(auth, chatBody("payopen", "")); w.Code != http.StatusBadGateway {
			t.Fatalf("payopen failure %d: %d %s", i, w.Code, w.Body)
		}
	}
	exec := func(q string) {
		t.Helper()
		if err := f.st.Write(context.Background(), func(tx *sql.Tx) error { _, err := tx.Exec(q); return err }); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TRIGGER refuse BEFORE INSERT ON usage BEGIN SELECT RAISE(ABORT, 'refused'); END`)
	// Before the fixture closes the writer, which then stores the waiting records.
	t.Cleanup(func() { exec(`DROP TRIGGER refuse`) })
	if w := f.chat(auth, chatBody("free", "")); w.Code != http.StatusOK {
		t.Fatalf("free: %d %s", w.Code, w.Body)
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(f.log.String(), `"msg":"usage store unavailable`); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the store is still available after 5 s; log %s", f.log.String())
		}
	}

	for _, tc := range []struct {
		alias, extra  string
		step          *mockprovider.Step
		status        int
		code, message string
		target        string
		calls         int
	}{
		{"pay", "", nil, 503, "usage_store_unavailable", "the usage store is unavailable", "", 0},
		{"payopen", "", nil, 503, "usage_store_unavailable", "the usage store is unavailable", "", 0},
		{"pay-free", "", nil, 200, "", "", "free/m", 1},
		{"plan", "", nil, 200, "", "", "sub/m", 1},
		{"free", "", nil, 200, "", "", "free/m", 1},
		{"payoff", "", nil, 503, "usage_store_unavailable", "the usage store is unavailable", "", 0},
		{"freeoff-pay", "", nil, 503, "no_target_available", `no target of model "freeoff-pay" is available`, "", 0},
		{"notools-pay", `,"tools":[{"type":"function","function":{"name":"f"}}]`, nil, 503, "usage_store_unavailable", "the usage store is unavailable", "", 0},
		{"free-pay", "", &mockprovider.Step{Status: 500}, 502, "upstream_error", "the provider answered 500", "free/m", 1},
	} {
		t.Run(tc.alias, func(t *testing.T) {
			if tc.step != nil {
				mock.Script(*tc.step)
			}
			before := len(mock.Calls())
			w := f.chat(auth, chatBody(tc.alias, tc.extra))
			var env struct{ Error domain.Error }
			_ = json.Unmarshal(w.Body.Bytes(), &env)
			if w.Code != tc.status || env.Error.Code != tc.code || env.Error.Message != tc.message || w.Header().Get(targetHeader) != tc.target ||
				len(mock.Calls())-before != tc.calls {
				t.Errorf("%d %s, target %q, %d calls", w.Code, w.Body, w.Header().Get(targetHeader), len(mock.Calls())-before)
			}
			if tc.code == "usage_store_unavailable" && env.Error.Type != "overloaded_error" {
				t.Errorf("type %q", env.Error.Type)
			}
		})
	}
}
