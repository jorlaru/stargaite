package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/usage"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// budgetConfig: every call to pay/m costs 0.03 USD (the mock's 1 input and
// 2 output tokens at 10,000 USD per million); sub and free share the mock
// without a USD cost. agent and capped have daily budgets of 0.05, zero a
// budget of 0 and every limit at 1 or 3, once a never budget and rpm_limit 1.
const budgetConfig = `version: 1
providers:
  pay: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true }
  sub: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: subscription, allow_private_network: true }
  free: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: none, allow_private_network: true }
targets:
  pay/m: { kind: chat, pricing: { input_per_million: 10000, output_per_million: 10000, as_of: "2026-11-01" } }
  sub/m: { kind: chat }
  free/m: { kind: chat }
models:
  fast: { targets: [pay/m] }
  fb: { targets: [pay/m, free/m] }
  plan: { targets: [sub/m] }
  capped: { targets: [pay/m], budget: { limit_usd: 0.05, reset_interval: daily } }
keys:
  agent: { models: ["*"], budget: { limit_usd: 0.05, reset_interval: daily } }
  other: { models: ["*"] }
  zero: { models: ["*"], budget: { limit_usd: 0, reset_interval: monthly }, rpm_limit: 3, tpm_limit: 1, max_parallel_requests: 1 }
  once: { models: ["*"], rpm_limit: 1, budget: { limit_usd: 10, reset_interval: never } }
`

// callCost is what one call to pay/m costs.
const callCost = 0.03

// newBudgets is the data plane with budgetConfig toward one mock, and the
// token of each key.
func newBudgets(t *testing.T) (*streamFixture, map[string]string) {
	t.Helper()
	f := newFixture(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	tokens := f.apply(t, strings.ReplaceAll(budgetConfig, "base_url: MOCK", "base_url: "+srv.URL))
	data := httptest.NewServer(f.data)
	t.Cleanup(data.Close)
	return &streamFixture{fixture: f, mock: mock, mockURL: srv.URL, url: data.URL}, tokens
}

// exceeded checks a budget refusal: 402 budget_exceeded in the OpenAI
// envelope with message, no Retry-After, and the headers of a request that
// reached no target.
func exceeded(t *testing.T, w *httptest.ResponseRecorder, message string) {
	t.Helper()
	var env struct{ Error domain.Error }
	if w.Code != http.StatusPaymentRequired || json.Unmarshal(w.Body.Bytes(), &env) != nil || env.Error.Code != domain.CodeBudgetExceeded ||
		env.Error.Type != "billing_error" || env.Error.Message != message || w.Header().Values("Retry-After") != nil {
		t.Errorf("want 402 budget_exceeded %q; got %d %s Retry-After %q", message, w.Code, w.Body, w.Header().Get("Retry-After"))
	}
	if w.Header().Get(domain.RequestIDHeader) == "" || len(w.Header().Get(traceHeader)) != 32 || w.Header().Values(targetHeader) != nil {
		t.Errorf("headers %v", w.Header())
	}
}

// TestBudgets is AC-37 and LIM-3 on the test clock: an exhausted budget
// makes pay_per_use targets unavailable, so the request falls back to a
// target of another billing or gets 402 budget_exceeded, naming only the
// kind of scope, before any provider is contacted and with no record; it
// never blocks a subscription or none target; key and alias budgets are
// independent; the 80 and 100 percent crossings give one WARN line, one
// audit row and one counter each; at 00:00 UTC the daily budget starts
// again (LIM-2).
func TestBudgets(t *testing.T) {
	s, tokens := newBudgets(t)
	f, mock := s.fixture, s.mock
	post := func(key, alias string) *httptest.ResponseRecorder {
		t.Helper()
		return f.chat("Bearer "+tokens[key], chatBody(alias, ""))
	}
	ok := func(key, alias, target string) {
		t.Helper()
		if w := post(key, alias); w.Code != http.StatusOK || w.Header().Get(targetHeader) != target {
			t.Errorf("%s to %s: %d from %q, want 200 from %s: %s", key, alias, w.Code, w.Header().Get(targetHeader), target, w.Body)
		}
	}

	ok("agent", "fast", "pay/m")
	ok("agent", "fast", "pay/m") // 0.06 of 0.05: both crossings at once
	exceeded(t, post("agent", "fast"), "the budget of this key is exhausted")
	if n := len(mock.Calls()); n != 2 {
		t.Errorf("the mock got %d calls, want 2", n)
	}
	ok("other", "fast", "pay/m") // another key's figures do not count
	ok("agent", "fb", "free/m")  // the next target, of billing none
	ok("agent", "plan", "sub/m") // a subscription target is never blocked

	// The alias's budget counts every key: other exhausts it, and a key
	// with an exhausted budget of its own is refused by its key first.
	ok("other", "capped", "pay/m")
	ok("other", "capped", "pay/m")
	exceeded(t, post("other", "capped"), `the budget of model "capped" is exhausted`)
	exceeded(t, post("agent", "capped"), "the budget of this key is exhausted")
	f.stored(t, 7)

	lines := strings.Split(f.log.String(), "\n")
	for _, msg := range []string{`"level":"WARN","msg":"budget warning"`, `"level":"WARN","msg":"budget exhausted"`} {
		for _, scope := range []string{`"scope":"key:agent","period":"d:2026-09-21"`, `"scope":"model:capped","period":"d:2026-09-21"`} {
			n := 0
			for _, l := range lines {
				if strings.Contains(l, msg) && strings.Contains(l, scope) {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%s %s: %d lines", msg, scope, n)
			}
		}
	}
	audit := rows(t, f, `SELECT id || ' ' || action || ' ' || resource_type || ' ' || resource_name || ' ' || result || ' ' || created_at
		FROM audit WHERE actor = 'system' ORDER BY id`)
	if want := fmt.Sprint([]string{
		"budget_exhausted:key:agent:d:2026-09-21 budget_exhausted budget key:agent warning 1790000000000",
		"budget_exhausted:model:capped:d:2026-09-21 budget_exhausted budget model:capped warning 1790000000000",
		"budget_warning:key:agent:d:2026-09-21 budget_warning budget key:agent warning 1790000000000",
		"budget_warning:model:capped:d:2026-09-21 budget_warning budget model:capped warning 1790000000000",
	}); fmt.Sprint(audit) != want {
		t.Errorf("audit %v, want %v", audit, want)
	}
	counted := map[usage.Counter]float64{}
	f.counters.Each(func(c usage.Counter, v float64) {
		if strings.HasPrefix(c.Name, "budget_") {
			counted[c] = v
		}
	})
	if want := map[usage.Counter]float64{{Name: "budget_warning", Key: "agent"}: 1, {Name: "budget_exhausted", Key: "agent"}: 1,
		{Name: "budget_warning", Model: "capped"}: 1, {Name: "budget_exhausted", Model: "capped"}: 1}; fmt.Sprint(counted) != fmt.Sprint(want) {
		t.Errorf("counters %v, want %v", counted, want)
	}

	// 2026-09-22T00:00:00Z: a new day, no reset task.
	f.now.Add(35_200_000 - 1)
	exceeded(t, post("agent", "fast"), "the budget of this key is exhausted")
	f.now.Add(1)
	ok("agent", "fast", "pay/m")
	f.stored(t, 8)
}

// rows renders the first column of each row of query on f's store.
func rows(t *testing.T, f *fixture, query string) []string {
	t.Helper()
	r, err := f.st.Read().Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	var out []string
	for r.Next() {
		var s string
		if err := r.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestBudgetsAndLimits is the rest of AC-38: an exhausted budget changes no
// rpm, tpm or slot count (zero's three requests, each admitted and refused
// at its target, count in rpm as admitted requests do, take no tpm and give
// their slot back, so the fourth meets rpm_limit only), and a limit refusal
// charges no budget (once's second request).
func TestBudgetsAndLimits(t *testing.T) {
	s, tokens := newBudgets(t)
	f := s.fixture
	for range 3 {
		exceeded(t, f.chat("Bearer "+tokens["zero"], chatBody("fast", "")), "the budget of this key is exhausted")
	}
	w := f.chat("Bearer "+tokens["zero"], chatBody("fast", ""))
	limited(t, w.Header(), w.Code, w.Body.Bytes(), "rate_limit_exceeded", "the rpm_limit of this key is reached", "61")

	if w := f.chat("Bearer "+tokens["once"], chatBody("fast", "")); w.Code != http.StatusOK {
		t.Fatalf("once: %d %s", w.Code, w.Body)
	}
	w = f.chat("Bearer "+tokens["once"], chatBody("fast", ""))
	limited(t, w.Header(), w.Code, w.Body.Bytes(), "rate_limit_exceeded", "the rpm_limit of this key is reached", "61")
	if n := len(s.mock.Calls()); n != 1 {
		t.Errorf("the mock got %d calls, want 1", n)
	}
	f.stored(t, 1)
	snap := f.cfg.Snapshot()
	once := admission.Budget{Scope: "key:once", Spec: *snap.Keys["once"].Spec.Budget, Since: snap.NeverSince["key:once"]}
	if got := f.limits.Spent(once); got != callCost {
		t.Errorf("once spent %v, want %v", got, callCost)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if got := rows(t, f, "SELECT since_revision || ' ' || cost_usd FROM budget_totals WHERE scope_name = 'once'"); fmt.Sprint(got) == "[1 0.03]" {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("budget_totals of once: %v", got)
		}
	}
}

// TestBudgetOvershoot is LIM-4 and AC-37's bound: twenty requests of agent
// at once, against a daily budget of 0.05 and the default 5 slots (LIM-7),
// spend at most the limit plus the cost of the calls in flight when it is
// crossed, at most 5; every refused request reaches no provider.
func TestBudgetOvershoot(t *testing.T) {
	s, tokens := newBudgets(t)
	f, mock := s.fixture, s.mock
	for range 20 {
		mock.Script(mockprovider.Step{Delay: 100 * time.Millisecond, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(mockReply)})
	}
	var wg sync.WaitGroup
	var served, refused atomic.Int64
	for range 20 {
		wg.Go(func() {
			switch w := f.chat("Bearer "+tokens["agent"], chatBody("fast", "")); w.Code {
			case http.StatusOK:
				served.Add(1)
			case http.StatusPaymentRequired, http.StatusTooManyRequests:
				refused.Add(1)
			default:
				t.Errorf("%d %s", w.Code, w.Body)
			}
		})
	}
	wg.Wait()
	waitFlight(t, f)
	spent := f.limits.Spent(admission.Budget{Scope: "key:agent", Spec: domain.Budget{ResetInterval: domain.ResetDaily}})
	if n := served.Load(); n < 2 || n > 5 || int64(len(mock.Calls())) != n || spent > 0.05+5*callCost+1e-9 || spent < float64(n)*callCost-1e-9 {
		t.Errorf("%d served, %d refused, %d calls, %v spent", n, refused.Load(), len(mock.Calls()), spent)
	}
	exceeded(t, f.chat("Bearer "+tokens["agent"], chatBody("fast", "")), "the budget of this key is exhausted")
}

// TestBudgetChargeOrder is LIM-4's order: when done writes the request
// line, before it releases the slot, the call is already charged, so no
// request admitted after the slot is free reads a counter without it.
func TestBudgetChargeOrder(t *testing.T) {
	s, tokens := newBudgets(t)
	f := s.fixture
	var seen atomic.Int64
	f.log.setHook(func(line []byte) {
		if !bytes.Contains(line, []byte(`"msg":"request"`)) || !bytes.Contains(line, []byte(`"key":"agent"`)) {
			return
		}
		seen.Add(1)
		if got := f.limits.Spent(admission.Budget{Scope: "key:agent", Spec: domain.Budget{ResetInterval: domain.ResetDaily}}); got < callCost-1e-9 {
			t.Errorf("at the request line, spent %v, want at least %v", got, callCost)
		}
		if release, e := f.limits.Admit(admission.Scope{Name: "key:agent", Parallel: 1}); e == nil {
			release()
			t.Error("at the request line, the slot is already free")
		}
	})
	if w := f.chat("Bearer "+tokens["agent"], chatBody("fast", "")); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	f.log.setHook(nil)
	if n := seen.Load(); n != 1 {
		t.Errorf("%d request lines of agent", n)
	}
}

// TestBudgetBeforeStore is RES-10's order: with the usage store unavailable,
// a pay_per_use target of a key whose budget is exhausted gets 402
// budget_exceeded, not 503 usage_store_unavailable.
func TestBudgetBeforeStore(t *testing.T) {
	s, tokens := newBudgets(t)
	f := s.fixture
	exec := func(q string) {
		t.Helper()
		if err := f.st.Write(context.Background(), func(tx *sql.Tx) error { _, err := tx.Exec(q); return err }); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TRIGGER refuse BEFORE INSERT ON usage BEGIN SELECT RAISE(ABORT, 'refused'); END`)
	// Before the fixture closes the writer, which then stores the waiting records.
	t.Cleanup(func() { exec(`DROP TRIGGER refuse`) })
	if w := f.chat("Bearer "+tokens["other"], chatBody("plan", "")); w.Code != http.StatusOK {
		t.Fatalf("plan: %d %s", w.Code, w.Body)
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(f.log.String(), `"msg":"usage store unavailable`); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the store is still available after 5 s; log %s", f.log.String())
		}
	}
	exceeded(t, f.chat("Bearer "+tokens["zero"], chatBody("fast", "")), "the budget of this key is exhausted")
}

// ledgerConfig is the template of TestLedger: four pay_per_use providers,
// one answering, one failing with 500 behind a fallback, one cutting its
// streams and one holding them open, a subscription plan with a request and
// a token window that the key shares, and budgets that each apply sets.
const ledgerConfig = `version: 1
settings: { GLOBAL }
providers:
  ok: { protocol: openai, base_url: OK, api_key_env: K, billing: pay_per_use, allow_private_network: true }
  bad: { protocol: openai, base_url: BAD, api_key_env: K, billing: pay_per_use, allow_private_network: true }
  cut: { protocol: openai, base_url: CUT, api_key_env: K, billing: pay_per_use, allow_private_network: true }
  hang: { protocol: openai, base_url: HANG, api_key_env: K, billing: pay_per_use, allow_private_network: true }
  plan: { protocol: openai, base_url: OK, api_key_env: K, billing: subscription, allow_private_network: true,
    quota: [{ window: 1h, requests: 100000 }, { window: 5h, tokens: 100000000 }] }
targets:
  ok/m: { kind: chat, pricing: { input_per_million: 1000, output_per_million: 3000, as_of: "2026-11-01" } }
  bad/m: { kind: chat, pricing: { input_per_million: 1000, output_per_million: 3000, as_of: "2026-11-01" } }
  cut/m: { kind: chat, pricing: { input_per_million: 1000, output_per_million: 3000, as_of: "2026-11-01" } }
  hang/m: { kind: chat, pricing: { input_per_million: 1000, output_per_million: 3000, as_of: "2026-11-01" } }
  plan/m: { kind: chat }
models:
  fast: { targets: [ok/m], FAST }
  fb: { targets: [bad/m, ok/m] }
  cut: { targets: [cut/m] }
  hang: { targets: [hang/m] }
  sub: { targets: [plan/m] }
keys:
  agent: { models: ["*"], max_parallel_requests: 100, quota_share: { plan: 0.5 }, KEY }
`

// TestLedger (-race) is the ledger of phase-3 design section 14: concurrent
// requests with fallbacks, cancels and cut streams while applies set, change
// and remove never budgets, which races the charges, and subscription calls
// counted in the plan's windows and the key's share; afterwards the records
// equal the provider calls, usage_daily the GROUP BY over usage, every budget
// counter in memory the one a fresh Restore reads from the store, every
// quota window the one a fresh seed reads, and every slot is back.
func TestLedger(t *testing.T) {
	f := newFixture(t)
	mock := mockprovider.New()
	okSrv := httptest.NewServer(mock)
	t.Cleanup(okSrv.Close)
	var badCalls, cutCalls, hangCalls atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		badCalls.Add(1)
		http.Error(w, `{"error":{"message":"down"}}`, http.StatusInternalServerError)
	}))
	t.Cleanup(bad.Close)
	stream := func(calls *atomic.Int64, end func(*http.Request)) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: " + chunk("partial answer") + "\n\n"))
			_ = http.NewResponseController(w).Flush()
			end(r)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	cut := stream(&cutCalls, func(*http.Request) { panic(http.ErrAbortHandler) })
	hang := stream(&hangCalls, func(r *http.Request) { <-r.Context().Done() })
	config := func(global, fast, key string) string {
		return strings.NewReplacer("OK", okSrv.URL, "BAD", bad.URL, "CUT", cut.URL, "HANG", hang.URL,
			"GLOBAL", global, "FAST", fast, "KEY", key).Replace(ledgerConfig)
	}
	never := func(limit int) string { return fmt.Sprintf("budget: { limit_usd: %d, reset_interval: never }", limit) }
	steps := [][3]string{
		{never(5000), never(1000), never(1000)},
		{never(5000), never(2000), never(2000)}, // limit_usd changes: the counters stay
		{"max_body_bytes: 1048576", never(2000), "budget: { limit_usd: 1000, reset_interval: monthly }"},
		{never(5000), "budget: { limit_usd: 1000, reset_interval: daily }", never(1000)}, // set again: from zero
		{never(5000), never(1000), "rpm_limit: 100000"},
	}
	tokens := f.apply(t, config(steps[0][0], steps[0][1], steps[0][2]))
	data := httptest.NewServer(f.data)
	t.Cleanup(data.Close)

	// Ten rounds of 32 requests, each racing one apply of the next step.
	for round := range 10 {
		var wg sync.WaitGroup
		wg.Go(func() {
			st := steps[(round+1)%len(steps)]
			if w := call(f.admin, "Bearer "+adminKey, http.MethodPost, "/admin/v1/apply", []byte(config(st[0], st[1], st[2]))); w.Code != http.StatusOK {
				t.Errorf("apply %d: %d %s", round, w.Code, w.Body)
			}
		})
		for g := range 8 {
			wg.Go(func() {
				for i := range 4 {
					switch (round + g + i) % 6 {
					case 0:
						<-send(t, data.URL, tokens["agent"], "/v1/chat/completions", chatBody("fast", "")).answer
					case 1:
						<-send(t, data.URL, tokens["agent"], "/v1/chat/completions", streamBody("fast")).answer
					case 2:
						<-send(t, data.URL, tokens["agent"], "/v1/chat/completions", chatBody("fb", "")).answer
					case 3:
						<-send(t, data.URL, tokens["agent"], "/v1/chat/completions", streamBody("cut")).answer
					case 4:
						r := send(t, data.URL, tokens["agent"], "/v1/chat/completions", streamBody("hang"))
						<-r.started
						r.cancel()
						<-r.answer
					case 5:
						<-send(t, data.URL, tokens["agent"], "/v1/chat/completions", chatBody("sub", "")).answer
					}
				}
			})
		}
		wg.Wait()
	}
	waitFlight(t, f)

	calls := int(int64(len(mock.Calls())) + badCalls.Load() + cutCalls.Load() + hangCalls.Load())
	// Fewer calls than requests: once a circuit opens, cut and fb's bad
	// target are passed over without a call.
	if calls < 200 {
		t.Errorf("%d provider calls for 320 requests", calls)
	}
	f.stored(t, calls)
	daily := `SELECT day || ' ' || key_name || ' ' || model || ' ' || target || ' ' || billing || ' ' || requests || ' ' || errors || ' ' ||
		input_tokens || ' ' || output_tokens || ' ' || printf('%.9f', cost_usd) FROM usage_daily ORDER BY 1`
	grouped := `SELECT strftime('%Y-%m-%d', completed_at / 1000, 'unixepoch') || ' ' || key_name || ' ' || model || ' ' || target || ' ' ||
		billing || ' ' || COUNT(*) || ' ' || COUNT(error_type) || ' ' || SUM(input_tokens) || ' ' || SUM(output_tokens) || ' ' ||
		printf('%.9f', SUM(COALESCE(cost_usd, 0))) FROM usage GROUP BY strftime('%Y-%m-%d', completed_at / 1000, 'unixepoch'),
		key_name, model, target, billing ORDER BY 1`
	if a, b := rows(t, f, daily), rows(t, f, grouped); fmt.Sprint(a) != fmt.Sprint(b) {
		t.Errorf("usage_daily\n%v\nGROUP BY usage\n%v", a, b)
	}

	fresh := admission.New(f.cfg.Now, usage.Seeder(f.st.Read(), slog.New(slog.DiscardHandler)))
	if err := usage.Restore(context.Background(), f.st.Read(), fresh, f.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	snap := f.cfg.Snapshot()
	if err := fresh.Sync(snap.Revision, snap.Doc.Providers, snap.Doc.Keys); err != nil {
		t.Fatal(err)
	}
	live, seeded := f.limits.Windows(), fresh.Windows()
	if fmt.Sprint(live) != fmt.Sprint(seeded) || len(live) != 4 || live[0].Used == 0 || live[3].Used == 0 {
		t.Errorf("quota windows in memory %+v, seeded %+v", live, seeded)
	}
	var bs []admission.Budget
	for _, scope := range []string{"global", "key:agent", "model:fast", "model:fb", "model:cut", "model:hang"} {
		for _, r := range []string{domain.ResetDaily, domain.ResetMonthly} {
			bs = append(bs, admission.Budget{Scope: scope, Spec: domain.Budget{ResetInterval: r}})
		}
	}
	for scope, since := range snap.NeverSince {
		bs = append(bs, admission.Budget{Scope: scope, Spec: domain.Budget{ResetInterval: domain.ResetNever}, Since: since})
	}
	charged := false
	for _, b := range bs {
		got, want := f.limits.Spent(b), fresh.Spent(b)
		if got < want-1e-9 || got > want+1e-9 {
			t.Errorf("%s %s since %d: %v in memory, %v restored", b.Scope, b.Spec.ResetInterval, b.Since, got, want)
		}
		charged = charged || got > 0
	}
	if !charged || len(snap.NeverSince) == 0 {
		t.Errorf("nothing to compare: NeverSince %v", snap.NeverSince)
	}
	release, e := f.limits.Admit(admission.Scope{Name: "key:agent", Parallel: 1}, admission.Scope{Name: "model:fast", Parallel: 1},
		admission.Scope{Name: "model:fb", Parallel: 1}, admission.Scope{Name: "model:cut", Parallel: 1},
		admission.Scope{Name: "model:hang", Parallel: 1}, admission.Scope{Name: "global", Parallel: 1})
	if e != nil {
		t.Errorf("a slot is still held: %v", e)
	} else {
		release()
	}
}
