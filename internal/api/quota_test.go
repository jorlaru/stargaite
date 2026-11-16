package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/usage"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// quotaConfig: sub is a subscription plan of QUOTA; free a target of billing
// none; gone a provider whose credential does not resolve. shared has a
// share of the plan (SHARE), agent a USD budget that the plan never touches.
const quotaConfig = `version: 1
providers:
  sub: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: subscription, allow_private_network: true, QUOTA }
  free: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: none, allow_private_network: true }
  gone: { protocol: openai, base_url: MOCK, api_key_env: MISSING_KEY, billing: none, allow_private_network: true }
targets:
  sub/m: { kind: chat }
  free/m: { kind: chat }
  gone/m: { kind: chat }
models:
  plan: { targets: [sub/m] }
  fb: { targets: [sub/m, free/m] }
  dead: { targets: [sub/m, gone/m] }
keys:
  agent: { models: ["*"], budget: { limit_usd: 1, reset_interval: daily } }
  shared: { models: ["*"], SHARE }
`

// newQuotas is the data plane with quotaConfig toward one mock.
func newQuotas(t *testing.T, quota, share string) (*fixture, *mockprovider.Server, func(quota, share string) map[string]string, map[string]string) {
	t.Helper()
	f := newFixture(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	apply := func(quota, share string) map[string]string {
		return f.apply(t, strings.NewReplacer("base_url: MOCK", "base_url: "+srv.URL, "QUOTA", quota, "SHARE", share).Replace(quotaConfig))
	}
	return f, mock, apply, apply(quota, share)
}

// quotaRefused checks a quota refusal: 429 quota_exceeded with message and
// Retry-After, and the headers of a request that reached no target.
func quotaRefused(t *testing.T, w *httptest.ResponseRecorder, message, retryAfter string) {
	t.Helper()
	var env struct{ Error domain.Error }
	if w.Code != http.StatusTooManyRequests || json.Unmarshal(w.Body.Bytes(), &env) != nil || env.Error.Code != domain.CodeQuotaExceeded ||
		env.Error.Type != "rate_limit_error" || env.Error.Message != message || w.Header().Get("Retry-After") != retryAfter {
		t.Errorf("want 429 quota_exceeded %q, Retry-After %s; got %d %s Retry-After %q", message, retryAfter, w.Code, w.Body, w.Header().Get("Retry-After"))
	}
	if w.Header().Values(targetHeader) != nil {
		t.Errorf("headers %v", w.Header())
	}
}

// TestQuotas is AC-36 on the test clock: traffic through a subscription
// provider changes no USD total; a key with a quota_share is refused at its
// fraction while another key is still served (LIM-10); the plan's exhausted
// window makes the alias fall back to its next target, or gets 429
// quota_exceeded with Retry-After when the quota excluded the first able
// target (LIM-9, RES-10), before any provider is contacted and with no
// record (USE-1); crossing 80 percent gives one WARN line, one audit row and
// one counter per window; usage quota shows the windows; a window later the
// plan serves again.
func TestQuotas(t *testing.T) {
	f, mock, _, keys := newQuotas(t, "quota: [{ window: 1h, requests: 5 }]", "quota_share: { sub: 0.4 }")
	tokens, shared := keys["agent"], keys["shared"]
	ok := func(token, alias, target string) {
		t.Helper()
		if w := f.chat("Bearer "+token, chatBody(alias, "")); w.Code != http.StatusOK || w.Header().Get(targetHeader) != target {
			t.Errorf("%s: %d from %q, want 200 from %s: %s", alias, w.Code, w.Header().Get(targetHeader), target, w.Body)
		}
	}
	ok(shared, "plan", "sub/m")
	ok(shared, "plan", "sub/m")
	// 0.4 of 5 is 2. t0 is 2 s into a 6 s bucket of the 1h window, which
	// admits again 1h + 4 s later.
	quotaRefused(t, f.chat("Bearer "+shared, chatBody("plan", "")), `the quota share of this key for provider "sub" is exhausted`, "3604")
	ok(tokens, "plan", "sub/m") // the other key is served
	ok(tokens, "plan", "sub/m")
	ok(tokens, "plan", "sub/m")
	quotaRefused(t, f.chat("Bearer "+tokens, chatBody("plan", "")), `the quota of provider "sub" is exhausted`, "3604")
	ok(tokens, "fb", "free/m") // the next target
	quotaRefused(t, f.chat("Bearer "+tokens, chatBody("dead", "")), `the quota of provider "sub" is exhausted`, "3604")
	if n := len(mock.Calls()); n != 6 {
		t.Errorf("the mock got %d calls, want 6", n)
	}
	recs := f.stored(t, 6)
	for _, r := range recs {
		if r.Cost != nil || r.CostSource != "none" {
			t.Errorf("record %s of %s: cost %v from %s", r.ID, r.Target, r.Cost, r.CostSource)
		}
	}
	if spent := f.limits.Spent(admission.Budget{Scope: "key:agent", Spec: domain.Budget{ResetInterval: domain.ResetDaily}}); spent != 0 {
		t.Errorf("agent's budget reads %v after subscription traffic", spent)
	}
	if got := rows(t, f, "SELECT billing || ' ' || SUM(requests) || ' ' || quote(SUM(cost_usd)) FROM usage_daily GROUP BY billing ORDER BY billing"); fmt.Sprint(got) != "[none 1 NULL subscription 5 NULL]" {
		t.Errorf("usage_daily %v", got)
	}

	// One crossing of 80 percent per window: the share's at 2 of 2 and the
	// plan's at 4 of 5.
	n := strings.Count(f.log.String(), `"level":"WARN","msg":"quota warning"`)
	period := "sub:1h:requests:1790000000"
	for _, scope := range []string{"key:shared", "provider:sub"} {
		if !strings.Contains(f.log.String(), `"msg":"quota warning","request_id":`) || !strings.Contains(f.log.String(), `"scope":"`+scope+`","period":"`+period+`"`) {
			t.Errorf("no warning line for %s; log %s", scope, f.log)
		}
	}
	if n != 2 {
		t.Errorf("%d quota warning lines, want 2", n)
	}
	audit := rows(t, f, `SELECT id || ' ' || action || ' ' || resource_type || ' ' || resource_name || ' ' || result || ' ' || created_at
		FROM audit WHERE actor = 'system' ORDER BY id`)
	if want := fmt.Sprint([]string{
		"quota_warning:key:shared:" + period + " quota_warning quota key:shared warning 1790000000000",
		"quota_warning:provider:sub:" + period + " quota_warning quota provider:sub warning 1790000000000",
	}); fmt.Sprint(audit) != want {
		t.Errorf("audit %v, want %v", audit, want)
	}
	counted := map[usage.Counter]float64{}
	f.counters.Each(func(c usage.Counter, v float64) {
		if c.Name == "quota_warning" {
			counted[c] = v
		}
	})
	if want := map[usage.Counter]float64{{Name: "quota_warning"}: 1, {Name: "quota_warning", Key: "shared"}: 1}; fmt.Sprint(counted) != fmt.Sprint(want) {
		t.Errorf("counters %v, want %v", counted, want)
	}

	w := call(f.admin, "Bearer "+adminKey, http.MethodGet, "/admin/v1/usage/quota", nil)
	want := `{"note":"counts this proxy's traffic only","windows":[` +
		`{"provider":"sub","window":"1h","unit":"requests","used":5,"limit":5,"used_pct":100,"retry_after_s":3604},` +
		`{"provider":"sub","key":"shared","share":0.4,"window":"1h","unit":"requests","used":2,"limit":2,"used_pct":100,"retry_after_s":3604}]}` + "\n"
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Errorf("usage quota: %d %s, want %s", w.Code, w.Body, want)
	}
	if w := call(f.admin, "Bearer "+adminKey, http.MethodGet, "/admin/v1/usage/quota?since=1h", nil); w.Code != http.StatusBadRequest {
		t.Errorf("usage quota with a query: %d %s", w.Code, w.Body)
	}

	f.now.Add(3_604_000 - 1)
	quotaRefused(t, f.chat("Bearer "+tokens, chatBody("plan", "")), `the quota of provider "sub" is exhausted`, "1")
	f.now.Add(1)
	ok(tokens, "plan", "sub/m")
	f.stored(t, 7)
}

// TestQuotaDeclaredWhileRunning: a window declared by apply while running,
// and a share added to a key, are seeded from the store at their first use,
// so the use made before counts (LIM-8, LIM-10); removing the quota drops
// the windows, so usage quota lists none.
func TestQuotaDeclaredWhileRunning(t *testing.T) {
	f, _, apply, keys := newQuotas(t, "quota: []", "rpm_limit: 0")
	tokens, shared := keys["agent"], keys["shared"]
	for _, token := range []string{tokens, shared, shared} {
		if w := f.chat("Bearer "+token, chatBody("plan", "")); w.Code != http.StatusOK {
			t.Fatalf("before the quota: %d %s", w.Code, w.Body)
		}
	}
	f.stored(t, 3)
	apply("quota: [{ window: 1h, requests: 5 }]", "quota_share: { sub: 0.6 }")
	// 0.6 of 5 is 3: shared used 2 of them, the plan 3 of 5.
	if w := f.chat("Bearer "+shared, chatBody("plan", "")); w.Code != http.StatusOK {
		t.Fatalf("the share's third request: %d %s", w.Code, w.Body)
	}
	quotaRefused(t, f.chat("Bearer "+shared, chatBody("plan", "")), `the quota share of this key for provider "sub" is exhausted`, "3604")
	if w := f.chat("Bearer "+tokens, chatBody("plan", "")); w.Code != http.StatusOK {
		t.Fatalf("the plan's fifth request: %d %s", w.Code, w.Body)
	}
	quotaRefused(t, f.chat("Bearer "+tokens, chatBody("plan", "")), `the quota of provider "sub" is exhausted`, "3604")
	apply("quota: []", "rpm_limit: 0")
	if w := f.chat("Bearer "+tokens, chatBody("plan", "")); w.Code != http.StatusOK {
		t.Fatalf("without the quota: %d %s", w.Code, w.Body)
	}
	if w := call(f.admin, "Bearer "+adminKey, http.MethodGet, "/admin/v1/usage/quota", nil); w.Body.String() != `{"note":"counts this proxy's traffic only","windows":[]}`+"\n" {
		t.Errorf("usage quota without a quota: %d %s", w.Code, w.Body)
	}
}

// TestQuotaSeedFails: a window whose seed fails while running holds nothing
// and excludes the provider's targets with 503 usage_store_unavailable, so
// no window admits on a partial count; the alias's next target still serves;
// the failure is logged with its cause; the next use seeds again.
func TestQuotaSeedFails(t *testing.T) {
	f, _, _, keys := newQuotas(t, "quota: [{ window: 1h, requests: 4 }]", "rpm_limit: 0")
	tokens := keys["agent"]
	// A stored row the seed cannot read: its started_at is not a number.
	bad := `INSERT INTO usage (id, request_id, endpoint, key_name, model, target, billing, status_code, tokens_source, cost_source,
		started_at, completed_at, config_revision, proxy_version)
		VALUES ('bad-1', 'bad', 'chat', 'agent', 'plan', 'sub/m', 'subscription', 200, 'provider', 'none', 'not a time', 1790000000000, 1, 'v')`
	exec := func(query string) {
		t.Helper()
		if err := f.st.Write(t.Context(), func(tx *sql.Tx) error { _, err := tx.Exec(query); return err }); err != nil {
			t.Fatal(err)
		}
	}
	exec(bad)
	w := f.chat("Bearer "+tokens, chatBody("plan", ""))
	var env struct{ Error domain.Error }
	if w.Code != http.StatusServiceUnavailable || json.Unmarshal(w.Body.Bytes(), &env) != nil || env.Error.Code != domain.CodeUsageStoreUnavailable {
		t.Errorf("a failing seed: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(f.log.String(), `"level":"ERROR","msg":"quota windows not seeded","provider":"sub","error":`) {
		t.Errorf("no log line for the failed seed: %s", f.log)
	}
	if w := f.chat("Bearer "+tokens, chatBody("fb", "")); w.Code != http.StatusOK || w.Header().Get(targetHeader) != "free/m" {
		t.Errorf("fb while the seed fails: %d from %q", w.Code, w.Header().Get(targetHeader))
	}
	if w := call(f.admin, "Bearer "+adminKey, http.MethodGet, "/admin/v1/usage/quota", nil); w.Code != http.StatusInternalServerError {
		t.Errorf("usage quota while the seed fails: %d %s", w.Code, w.Body)
	}
	exec("DELETE FROM usage WHERE id = 'bad-1'")
	if w := f.chat("Bearer "+tokens, chatBody("plan", "")); w.Code != http.StatusOK {
		t.Errorf("after the row is gone: %d %s", w.Code, w.Body)
	}
}

// TestQuotaTokensAtEnd: a call's tokens count in a token window at its
// completed_at (LIM-8), so those of a 10-minute call refuse until a window
// after its end, not after its start.
func TestQuotaTokensAtEnd(t *testing.T) {
	f := newFixture(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.now.Add(600_000) // the call lasts 10 minutes
		mock.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	tokens := f.apply(t, strings.NewReplacer("base_url: MOCK", "base_url: "+srv.URL, "QUOTA", "quota: [{ window: 1h, tokens: 3 }]",
		"SHARE", "rpm_limit: 0").Replace(quotaConfig))["agent"]
	if w := f.chat("Bearer "+tokens, chatBody("plan", "")); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	// 3 tokens at t0 + 600 s, 2 s into a 6 s bucket: the window admits again
	// 1h + 4 s later.
	quotaRefused(t, f.chat("Bearer "+tokens, chatBody("plan", "")), `the quota of provider "sub" is exhausted`, "3604")
	f.now.Add(3_004_000) // an hour and a bucket after the call's start
	quotaRefused(t, f.chat("Bearer "+tokens, chatBody("plan", "")), `the quota of provider "sub" is exhausted`, "600")
}

// TestQuotaOrder is the RES-10 order around a quota: a subscription whose
// credential does not resolve is passed over without counting a request, as
// no call leaves a record; an exhausted window answers 429 quota_exceeded
// with Retry-After before the credential is looked at (LIM-9).
func TestQuotaOrder(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(mockprovider.New())
	t.Cleanup(srv.Close)
	apply := func(env string) map[string]string {
		return f.apply(t, strings.NewReplacer("base_url: MOCK", "base_url: "+srv.URL, "MOCK_KEY, billing: subscription",
			env+", billing: subscription", "QUOTA", "quota: [{ window: 1h, requests: 2 }]", "SHARE", "rpm_limit: 0").Replace(quotaConfig))
	}
	tokens := apply("MISSING_KEY")["agent"]
	chat := func(alias, target string) {
		t.Helper()
		if w := f.chat("Bearer "+tokens, chatBody(alias, "")); w.Code != http.StatusOK || w.Header().Get(targetHeader) != target {
			t.Fatalf("%s: %d from %q, want 200 from %s: %s", alias, w.Code, w.Header().Get(targetHeader), target, w.Body)
		}
	}
	for range 3 {
		chat("fb", "free/m")
	}
	if got, want := fmt.Sprint(f.limits.Windows()), fmt.Sprint([]admission.WindowView{{Provider: "sub", Window: "1h", Unit: "requests", Limit: 2}}); got != want {
		t.Errorf("windows %s after the plan was passed over, want %s", got, want)
	}
	apply("MOCK_KEY")
	chat("plan", "sub/m")
	chat("plan", "sub/m")
	apply("MISSING_KEY")
	quotaRefused(t, f.chat("Bearer "+tokens, chatBody("plan", "")), `the quota of provider "sub" is exhausted`, "3604")
}

// TestQuotaTakeRefusedReturnsPermit: when Take refuses after Quota passed,
// because another request filled the window in between, exclude gives the
// half-open circuit's trial permit back, so the circuit still closes once
// the window frees. A permit kept would leave one trial out for good, and
// the circuit could then never pass both trials.
func TestQuotaTakeRefusedReturnsPermit(t *testing.T) {
	f, mock, _, keys := newQuotas(t, "quota: [{ window: 1h, requests: 6 }]", "rpm_limit: 0")
	token := keys["agent"]
	for range 5 { // five eligible failures open sub/m's circuit, and use 5 of the 6 requests
		mock.Script(mockprovider.Step{Status: 500})
		if w := f.chat("Bearer "+token, chatBody("plan", "")); w.Code != http.StatusBadGateway {
			t.Fatalf("failing call: %d %s", w.Code, w.Body)
		}
	}
	f.now.Add(21_000) // the circuit is half-open
	// The breaker reads the clock in Acquire only while half-open: between
	// this request's Quota and its Take, another request takes the last one.
	other := admission.Plan{Provider: "sub", Windows: []domain.QuotaWindow{{Window: "1h", Requests: 6}}, Key: "agent"}
	var fired atomic.Bool
	hook := func() {
		pc := make([]uintptr, 32)
		frames := runtime.CallersFrames(pc[:runtime.Callers(2, pc)])
		for {
			fr, more := frames.Next()
			if strings.HasSuffix(fr.Function, "routing.(*Breakers).Acquire") {
				if fired.CompareAndSwap(false, true) {
					if e := f.limits.Take(other); e != nil {
						t.Errorf("the other request's Take: %v", e)
					}
				}
				return
			}
			if !more {
				return
			}
		}
	}
	f.tick.Store(&hook)
	w := f.chat("Bearer "+token, chatBody("plan", ""))
	f.tick.Store(nil)
	var env struct{ Error domain.Error }
	if !fired.Load() || w.Code != http.StatusTooManyRequests || json.Unmarshal(w.Body.Bytes(), &env) != nil || env.Error.Code != domain.CodeQuotaExceeded {
		t.Fatalf("fired %v; want 429 quota_exceeded from Take, got %d %s", fired.Load(), w.Code, w.Body)
	}
	f.now.Add(3_700_000) // the window frees; the circuit is still half-open
	for i := range 3 {
		if w := f.chat("Bearer "+token, chatBody("plan", "")); w.Code != http.StatusOK || w.Header().Get(targetHeader) != "sub/m" {
			t.Fatalf("request %d after the window freed: %d from %q: %s", i, w.Code, w.Header().Get(targetHeader), w.Body)
		}
	}
}
