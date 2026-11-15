package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/api"
	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/store"
	"github.com/jorlaru/stargaite/internal/telemetry"
	"github.com/jorlaru/stargaite/internal/transport"
	"github.com/jorlaru/stargaite/internal/usage"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// verifyFixture is the admin listener with targets verify wired to a data
// plane in process (the one serve builds), the mock as every provider, on an
// httptest.Server whose read and write timeouts are 200 ms.
type verifyFixture struct {
	url     string
	client  *http.Client
	data    http.Handler // the data plane on the same Deps
	mock    *mockprovider.Server
	mockURL string
	st      *store.Store
	cfg     *config.Service
	limits  *admission.State
	flight  *sync.WaitGroup
	halt    context.CancelCauseFunc // the stop: cancels every request context
	// The data plane's writer and exporter, and the exporter's receiver,
	// which a configuration that sets telemetry points at.
	writer      *usage.Writer
	spans       *telemetry.Exporter
	receiver    *mockprovider.Server
	receiverURL string
}

func newVerifyFixture(t *testing.T) *verifyFixture {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	env := func() config.Env {
		return config.Env{Lookup: func(string) (string, bool) { return "", false }, Resolve: resolve}
	}
	now := time.UnixMilli(testNow)
	cfg, err := config.Open(context.Background(), st, env, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	writer := usage.NewWriter(st.Write, logger)
	t.Cleanup(func() { writer.Close(context.Background()) }) // before the store closes
	noDNS := func(context.Context, string) ([]netip.Addr, error) {
		return nil, errors.New("no name is resolved in tests")
	}
	limits, flight := admission.New(cfg.Now, usage.Seeder(st.Read(), logger)), &sync.WaitGroup{}
	clients := [2]*http.Client{transport.NewClient(false, noDNS), transport.NewClient(true, noDNS)}
	receiver := mockprovider.New()
	rsrv := httptest.NewServer(receiver)
	t.Cleanup(rsrv.Close)
	spans := telemetry.NewExporter(func() *config.Exporter { return cfg.Snapshot().Exporter }, clients, "test-version", logger)
	t.Cleanup(func() { // before the receiver closes
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		spans.Close(ctx)
	})
	counters, spanned := &usage.Counters{}, &usage.Counters{}
	d := api.Deps{Config: cfg, Log: logger, Clients: clients, Flight: flight, Usage: writer, Admission: limits, Counters: counters, Spanned: spanned,
		Spans: spans, Version: "test-version"}
	mock := mockprovider.New()
	msrv := httptest.NewServer(mock)
	t.Cleanup(msrv.Close)
	base, halt := context.WithCancelCause(context.Background())
	t.Cleanup(func() { halt(nil) })
	metricsHash := sha256.Sum256([]byte(testMetricsToken))
	srv := httptest.NewUnstartedServer(Handler(Deps{Config: cfg, KeyHash: sha256.Sum256([]byte(testKey)), Log: logger, Usage: st.Read(),
		Admission: limits, Spans: spans, Counters: counters, Spanned: spanned, MetricsHash: &metricsHash, Verify: api.Verify(d)}))
	srv.Config.ReadTimeout, srv.Config.WriteTimeout = 200*time.Millisecond, 200*time.Millisecond
	srv.Config.BaseContext = func(net.Listener) context.Context { return base }
	srv.Start()
	t.Cleanup(srv.Close)
	return &verifyFixture{srv.URL, srv.Client(), api.Handler(d), mock, msrv.URL, st, cfg, limits, flight, halt, writer, spans, receiver, rsrv.URL}
}

// send sends one admin request with the key and returns the status and the
// body; it takes no *testing.T, so a goroutine may call it.
func (f *verifyFixture) send(method, path, body string) (int, []byte, error) {
	req, err := http.NewRequest(method, f.url+path, strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

// must sends one admin request that must answer status.
func (f *verifyFixture) must(t *testing.T, status int, method, path, body string) []byte {
	t.Helper()
	got, data, err := f.send(method, path, body)
	if err != nil || got != status {
		t.Fatalf("%s %s: status %d, want %d: %s, error %v", method, path, got, status, data, err)
	}
	return data
}

type verifyAnswer struct {
	domain.Verification
	Stored bool `json:"stored"`
}

// run runs targets verify on id with body: the 200 answer, decoded. It takes
// no *testing.T, so a goroutine may call it.
func (f *verifyFixture) run(id, body string) (verifyAnswer, error) {
	var v verifyAnswer
	code, data, err := f.send(http.MethodPost, "/admin/v1/verify/"+strings.ReplaceAll(id, "/", "%2F"), body)
	if err == nil && code != http.StatusOK {
		err = fmt.Errorf("status %d: %s", code, data)
	}
	if err == nil {
		err = json.Unmarshal(data, &v)
	}
	return v, err
}

// verify is run, failing the test on an error.
func (f *verifyFixture) verify(t *testing.T, id, body string) verifyAnswer {
	t.Helper()
	v, err := f.run(id, body)
	if err != nil {
		t.Fatalf("verify %s: %v", id, err)
	}
	return v
}

// refused runs targets verify on id with body and checks the envelope.
func (f *verifyFixture) refused(t *testing.T, id, body string, status int, code, says string) {
	t.Helper()
	var env struct{ Error domain.Error }
	data := f.must(t, status, http.MethodPost, "/admin/v1/verify/"+strings.ReplaceAll(id, "/", "%2F"), body)
	if err := json.Unmarshal(data, &env); err != nil || env.Error.Code != code || !strings.Contains(env.Error.Message, says) {
		t.Errorf("%s: answer %s, want %s naming %q", id, data, code, says)
	}
}

func (f *verifyFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.st.Read().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// records waits until the usage writer has stored n records of _verify.
func (f *verifyFixture) records(t *testing.T, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); f.count(t, "SELECT COUNT(*) FROM usage WHERE key_name = '_verify'") != n; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d records of _verify, want %d", f.count(t, "SELECT COUNT(*) FROM usage WHERE key_name = '_verify'"), n)
		}
	}
}

func (f *verifyFixture) lastAudit(t *testing.T) string {
	t.Helper()
	var action, kind, name, value, result string
	var revision any
	if err := f.st.Read().QueryRow("SELECT action, resource_type, resource_name, new_value, result, revision FROM audit ORDER BY rowid DESC LIMIT 1").
		Scan(&action, &kind, &name, &value, &result, &revision); err != nil {
		t.Fatal(err)
	}
	if revision != nil || strings.Contains(value, "17 × 23") {
		t.Errorf("audit row of %s: revision %v, new_value %s: no revision and no body", action, revision, value)
	}
	return strings.Join([]string{action, kind, name, result}, " ")
}

// unverified lists the target_unverified findings of validate.
func (f *verifyFixture) unverified(t *testing.T) []string {
	t.Helper()
	var v struct{ Findings []domain.Finding }
	if err := json.Unmarshal(f.must(t, http.StatusOK, http.MethodGet, "/admin/v1/validate", ""), &v); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, x := range v.Findings {
		if x.Code == "target_unverified" {
			out = append(out, x.Severity+" "+x.Rule+" "+x.Resource)
		}
	}
	return out
}

// chat is an OpenAI chat answer with completion tokens and reasoning tokens.
func chat(completion, reasoning int) []byte {
	return fmt.Appendf(nil, `{"id":"c","object":"chat.completion","created":0,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"391"},`+
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":%d,"total_tokens":%d,"completion_tokens_details":{"reasoning_tokens":%d}}}`,
		completion, 20+completion, reasoning)
}

// setUp declares a pay_per_use provider on the mock, its chat target mock/m
// with levels low and high that can disable reasoning, an alias and a key
// with budgets (the key with room for the concurrent traffic of
// TestVerifyAlongsideTraffic), and a global budget; it returns the key's
// token.
func (f *verifyFixture) setUp(t *testing.T) string {
	t.Helper()
	f.must(t, http.StatusOK, http.MethodPatch, "/admin/v1/settings", `{"budget":{"limit_usd":10,"reset_interval":"monthly"}}`)
	f.must(t, http.StatusCreated, http.MethodPost, "/admin/v1/providers",
		`{"name":"mock","protocol":"openai","base_url":"`+f.mockURL+`","allow_private_network":true,"billing":"pay_per_use"}`)
	f.must(t, http.StatusCreated, http.MethodPost, "/admin/v1/targets", `{"id":"mock/m","kind":"chat","max_output_tokens":1000,`+
		`"reasoning":{"dialect":"reasoning_effort","levels":["low","high"],"can_disable":true},`+
		`"pricing":{"input_per_million":1,"output_per_million":1,"as_of":"2026-11-01"}}`)
	f.must(t, http.StatusCreated, http.MethodPost, "/admin/v1/models", `{"name":"fast","targets":["mock/m"],"budget":{"limit_usd":10,"reset_interval":"monthly"}}`)
	var res config.Result
	if err := json.Unmarshal(f.must(t, http.StatusCreated, http.MethodPost, "/admin/v1/keys",
		`{"name":"k","models":["fast"],"max_parallel_requests":50,"budget":{"limit_usd":10,"reset_interval":"monthly"}}`), &res); err != nil || res.Secrets["k"] == "" {
		t.Fatalf("key: %+v %v", res, err)
	}
	return res.Secrets["k"]
}

// infer sends one chat request to the data plane with token.
func (f *verifyFixture) infer(token string) int {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.data.ServeHTTP(w, r)
	return w.Code
}

// TestVerify is AC-47 (RSN-9, phase-4 design section 6): targets verify on
// the admin listener, api in process, on a server whose write timeout is
// 200 ms while the mock answers the first call after 300 ms.
func TestVerify(t *testing.T) {
	f := newVerifyFixture(t)
	f.setUp(t)
	monthly := domain.Budget{LimitUSD: new(10.0), ResetInterval: domain.ResetMonthly}
	if got := f.unverified(t); !slices.Equal(got, []string{"warning RSN-9 targets/mock/m"}) {
		t.Fatalf("validate before any run: %q", got)
	}
	revisions := f.count(t, "SELECT COUNT(*) FROM revisions")

	// Three calls: low, high, then none; bodies byte-equal to the mock's,
	// reasoning tokens per level, the answer whole after the write timeout.
	f.mock.Script(mockprovider.Step{Body: chat(12, 10), Delay: 300 * time.Millisecond}, mockprovider.Step{Body: chat(60, 50)}, mockprovider.Step{Body: chat(2, 0)})
	v := f.verify(t, "mock/m", `{}`)
	calls := f.mock.Calls()
	if !v.OK || !v.Stored || v.Target != "mock/m" || v.Version != "test-version" || !v.At.Equal(time.UnixMilli(testNow)) || len(v.Calls) != 3 || len(calls) != 3 {
		t.Fatalf("answer %+v, %d mock calls", v, len(calls))
	}
	for i, want := range []struct {
		effort, sent string
		reasoning    int64
		output       int64
	}{{"low", "low", 10, 12}, {"high", "high", 50, 60}, {"none", "none", 0, 2}} {
		c := v.Calls[i]
		if c.Effort != want.effort || c.EffortSent == nil || *c.EffortSent != want.sent || c.Status != 200 || c.ReasoningTokens == nil ||
			*c.ReasoningTokens != want.reasoning || c.OutputTokens != want.output || c.Error != nil || c.RequestID == "" {
			t.Errorf("call %d: %+v", i, c)
		}
		if c.Body != string(calls[i].Body) || calls[i].Path != "/chat/completions" {
			t.Errorf("call %d: body %s, the mock got %s at %s", i, c.Body, calls[i].Body, calls[i].Path)
		}
		if !strings.Contains(c.Body, `"model":"m"`) || !strings.Contains(c.Body, `"reasoning_effort":"`+want.sent+`"`) || strings.Contains(c.Body, "max_tokens") {
			t.Errorf("call %d: body %s", i, c.Body)
		}
	}
	f.records(t, 3)
	if n := f.count(t, `SELECT COUNT(*) FROM usage WHERE key_name = '_verify' AND model = '' AND attempt = 1 AND effort_source = 'client'
		AND target = 'mock/m' AND status_code = 200 AND max_output_alias IS NULL AND cost_usd > 0`); n != 3 {
		t.Errorf("%d records of _verify with model '', attempt 1 and source client, want 3", n)
	}
	if spent := f.limits.Spent(admission.Budget{Scope: "global", Spec: monthly}); spent <= 0 {
		t.Errorf("global spend %v: the verify calls count toward the global budget", spent)
	}
	for _, scope := range []string{"key:k", "model:fast", "key:" + domain.VerifyKey} {
		if spent := f.limits.Spent(admission.Budget{Scope: scope, Spec: monthly}); spent != 0 {
			t.Errorf("%s spend %v: verify passes no key or alias budget", scope, spent)
		}
	}
	if n := f.count(t, "SELECT COUNT(*) FROM targets WHERE id = 'mock/m' AND verified_at = ? AND verified_version = 'test-version'", testNow); n != 1 {
		t.Error("the result is not stored with its date and version")
	}
	if got, ok := f.cfg.Verification("mock/m"); !ok || got != (config.Verification{At: testNow, Version: "test-version"}) {
		t.Errorf("Verification %+v %v", got, ok)
	}
	if got := f.unverified(t); len(got) != 0 {
		t.Errorf("validate after the run: %q", got)
	}
	if got := f.lastAudit(t); got != "targets verify mock/m target mock/m verified" {
		t.Errorf("audit %q", got)
	}
	if n := f.count(t, "SELECT COUNT(*) FROM revisions"); n != revisions {
		t.Errorf("%d revisions, want %d: verify writes no revision", n, revisions)
	}
	if show := f.must(t, http.StatusOK, http.MethodGet, "/admin/v1/targets/mock%2Fm", ""); !bytes.Contains(show, fmt.Appendf(nil, `"verified_at":%d,"verified_version":"test-version"`, testNow)) {
		t.Errorf("targets show %s", show)
	}
	if list := f.must(t, http.StatusOK, http.MethodGet, "/admin/v1/targets", ""); !bytes.Contains(list, []byte(`"verified_version":"test-version"`)) {
		t.Errorf("targets list %s", list)
	}
	if export := f.must(t, http.StatusOK, http.MethodGet, "/admin/v1/export", ""); bytes.Contains(export, []byte("verified")) {
		t.Errorf("export holds the verification:\n%s", export)
	}

	// A target update clears the stored result.
	f.must(t, http.StatusOK, http.MethodPatch, "/admin/v1/targets/mock%2Fm", `{"pricing":{"input_per_million":2}}`)
	if n := f.count(t, "SELECT COUNT(*) FROM targets WHERE id = 'mock/m' AND verified_at IS NULL AND verified_version IS NULL"); n != 1 {
		t.Error("a target update kept the stored result")
	}
	if _, ok := f.cfg.Verification("mock/m"); ok || !slices.Equal(f.unverified(t), []string{"warning RSN-9 targets/mock/m"}) {
		t.Error("a target update kept the published result")
	}
	if show := f.must(t, http.StatusOK, http.MethodGet, "/admin/v1/targets/mock%2Fm", ""); bytes.Contains(show, []byte("verified")) {
		t.Errorf("targets show after the update %s", show)
	}

	// A 400 on one level: the other levels still run; nothing stored, failed.
	f.mock.Script(mockprovider.Step{Body: chat(12, 10)},
		mockprovider.Step{Status: http.StatusBadRequest, Body: []byte(`{"error":{"message":"unsupported value: high"}}`)}, mockprovider.Step{Body: chat(2, 0)})
	v = f.verify(t, "mock/m", `{}`)
	if v.OK || v.Stored || len(v.Calls) != 3 || v.Calls[1].Status != 400 || v.Calls[1].Error == nil || v.Calls[2].Status != 200 {
		t.Errorf("a 400 on one level: %+v", v)
	}
	f.records(t, 6)
	if _, ok := f.cfg.Verification("mock/m"); ok || f.count(t, "SELECT COUNT(*) FROM targets WHERE verified_at IS NOT NULL") != 0 {
		t.Error("a failed run was stored")
	}
	if got := f.lastAudit(t); got != "targets verify mock/m target mock/m failed" {
		t.Errorf("audit %q", got)
	}

	// The cap: written under output_cap_param, listed added, recorded as the
	// alias's; above the target's max_output_tokens it is refused (PAR-6).
	for i, param := range []string{"max_completion_tokens", "max_output_tokens"} {
		id := fmt.Sprintf("mock/c%d", i)
		f.must(t, http.StatusCreated, http.MethodPost, "/admin/v1/targets",
			`{"id":"`+id+`","kind":"chat","output_cap_param":"`+param+`","pricing":{"input_per_million":1,"output_per_million":1,"as_of":"2026-11-01"}}`)
		before := len(f.mock.Calls())
		v = f.verify(t, id, `{"max_output_tokens":64}`)
		calls := f.mock.Calls()
		if !v.OK || !v.Stored || len(v.Calls) != 1 || v.Calls[0].Effort != "" || v.Calls[0].EffortSent != nil || len(calls) != before+1 ||
			!strings.Contains(v.Calls[0].Body, `"`+param+`":64`) || v.Calls[0].Body != string(calls[before].Body) || strings.Contains(v.Calls[0].Body, "reasoning_effort") {
			t.Errorf("%s: %+v", param, v)
		}
		f.records(t, 7+i)
		if n := f.count(t, `SELECT COUNT(*) FROM usage WHERE target = ? AND params_changed = ? AND max_output_alias = 64 AND max_output_sent = 64
			AND effort_source = 'provider_default'`, id, param+":added"); n != 1 {
			t.Errorf("%s: the record does not list the cap added", param)
		}
	}
	f.refused(t, "mock/m", `{"max_output_tokens":1001}`, http.StatusBadRequest, domain.CodeInvalidRequest, "PAR-6")
	f.refused(t, "mock/m", `{"max_output_tokens":0}`, http.StatusBadRequest, domain.CodeInvalidRequest, "max_output_tokens")
	f.refused(t, "mock/nope", `{}`, http.StatusNotFound, domain.CodeNotFound, "mock/nope")
	f.refused(t, "mock/m", `{"max_output":1}`, http.StatusBadRequest, domain.CodeInvalidRequest, "max_output")

	// An anthropic target without a cap is refused (PAR-7); with one, the
	// body is translated and carries it as max_tokens.
	f.must(t, http.StatusCreated, http.MethodPost, "/admin/v1/providers",
		`{"name":"anth","protocol":"anthropic","base_url":"`+f.mockURL+`","allow_private_network":true,"billing":"none"}`)
	f.must(t, http.StatusCreated, http.MethodPost, "/admin/v1/targets", `{"id":"anth/m","kind":"chat"}`)
	f.refused(t, "anth/m", `{}`, http.StatusBadRequest, domain.CodeInvalidRequest, "PAR-7")
	v = f.verify(t, "anth/m", `{"max_output_tokens":64}`)
	if calls := f.mock.Calls(); !v.OK || len(v.Calls) != 1 || calls[len(calls)-1].Path != "/v1/messages" || v.Calls[0].Body != string(calls[len(calls)-1].Body) ||
		!strings.Contains(v.Calls[0].Body, `"max_tokens":64`) {
		t.Errorf("anthropic with a cap: %+v", v)
	}
	f.records(t, 9)
	f.must(t, http.StatusCreated, http.MethodPost, "/admin/v1/targets", `{"id":"mock/e","kind":"embeddings"}`)
	f.refused(t, "mock/e", `{}`, http.StatusBadRequest, domain.CodeInvalidRequest, "chat targets only")

	// A target changed during the run: the run succeeds, is audited stale
	// and nothing is stored.
	f.mock.Script(mockprovider.Step{Body: chat(1, 0), Delay: 300 * time.Millisecond})
	before := len(f.mock.Calls())
	done := make(chan verifyAnswer, 1)
	go func() { v, _ := f.run("mock/c0", `{"max_output_tokens":64}`); done <- v }()
	for deadline := time.Now().Add(5 * time.Second); len(f.mock.Calls()) == before; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the run never reached the mock")
		}
	}
	f.must(t, http.StatusOK, http.MethodPatch, "/admin/v1/targets/mock%2Fc0", `{"pricing":{"input_per_million":3}}`)
	if v = <-done; !v.OK || v.Stored {
		t.Errorf("a run on a target changed meanwhile: %+v", v)
	}
	if got := f.lastAudit(t); got != "targets verify mock/c0 target mock/c0 stale" {
		t.Errorf("audit %q", got)
	}
	if _, ok := f.cfg.Verification("mock/c0"); ok {
		t.Error("a stale run was stored")
	}
	// A provider update clears the result of each of its targets.
	if _, ok := f.cfg.Verification("mock/c1"); !ok {
		t.Fatal("mock/c1 lost its result")
	}
	f.must(t, http.StatusOK, http.MethodPatch, "/admin/v1/providers/mock", `{"gen_ai_provider":"mock"}`)
	if _, ok := f.cfg.Verification("mock/c1"); ok || f.count(t, "SELECT COUNT(*) FROM targets WHERE provider = 'mock' AND verified_at IS NOT NULL") != 0 {
		t.Error("a provider update kept a result of its target")
	}

	// An on/off dialect: on is sent as medium, which Build writes as the
	// enabled form, then none as the disabled form.
	f.must(t, http.StatusCreated, http.MethodPost, "/admin/v1/targets",
		`{"id":"mock/on","kind":"chat","reasoning":{"dialect":"enable_thinking","levels":["on"],"can_disable":true}}`)
	f.mock.Script(mockprovider.Step{Body: chat(5, 4)}, mockprovider.Step{Body: chat(1, 0)})
	before = len(f.mock.Calls())
	v = f.verify(t, "mock/on", `{}`)
	calls = f.mock.Calls()
	if !v.OK || !v.Stored || len(v.Calls) != 2 || len(calls) != before+2 {
		t.Fatalf("an on/off target: %+v, %d mock calls", v, len(calls)-before)
	}
	for i, want := range []struct{ effort, body string }{{"on", `"enable_thinking":true`}, {"none", `"enable_thinking":false`}} {
		c := v.Calls[i]
		if c.Effort != want.effort || c.EffortSent == nil || *c.EffortSent != want.effort || c.Body != string(calls[before+i].Body) ||
			!strings.Contains(c.Body, want.body) || strings.Contains(c.Body, "reasoning_effort") {
			t.Errorf("on/off call %d: %+v", i, c)
		}
	}
	f.records(t, 12)
}

// TestVerifyHoldsFlight: the run holds Flight from start to end (phase-4
// design section 14), so a Wait that starts during the first call returns
// only after the last call: serve never closes the store between two calls.
func TestVerifyHoldsFlight(t *testing.T) {
	f := newVerifyFixture(t)
	f.setUp(t)
	step := mockprovider.Step{Body: chat(1, 0), Delay: 50 * time.Millisecond}
	f.mock.Script(step, step, step)
	done := make(chan error, 1)
	go func() { _, err := f.run("mock/m", `{}`); done <- err }()
	for deadline := time.Now().Add(5 * time.Second); len(f.mock.Calls()) == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the run never reached the mock")
		}
	}
	f.flight.Wait()
	if n := len(f.mock.Calls()); n != 3 {
		t.Errorf("Flight reached zero after %d of 3 calls", n)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.records(t, 3)
}

// TestVerifyRefusedBeforeTheProvider: an exhausted global budget gives 402
// and an exhausted quota window 429, each on the first call, with no mock
// call and no record; the run is audited failed.
func TestVerifyRefusedBeforeTheProvider(t *testing.T) {
	f := newVerifyFixture(t)
	f.setUp(t)
	f.must(t, http.StatusOK, http.MethodPatch, "/admin/v1/settings", `{"budget":{"limit_usd":0,"reset_interval":"monthly"}}`)
	v := f.verify(t, "mock/m", `{}`)
	if v.OK || v.Stored || len(v.Calls) != 1 || v.Calls[0].Status != 0 || v.Calls[0].Error == nil || v.Calls[0].Error.Code != domain.CodeBudgetExceeded ||
		!strings.Contains(v.Calls[0].Body, `"reasoning_effort":"low"`) || len(f.mock.Calls()) != 0 {
		t.Errorf("an exhausted global budget: %+v, %d mock calls", v, len(f.mock.Calls()))
	}
	if got := f.lastAudit(t); got != "targets verify mock/m target mock/m failed" {
		t.Errorf("audit %q", got)
	}

	// A subscription whose window admits one request: the first run takes
	// it, the second is refused 429 before the mock.
	f.must(t, http.StatusCreated, http.MethodPost, "/admin/v1/providers",
		`{"name":"plan","protocol":"openai","base_url":"`+f.mockURL+`","allow_private_network":true,"billing":"subscription","quota":[{"window":"5h","requests":1}]}`)
	f.must(t, http.StatusCreated, http.MethodPost, "/admin/v1/targets", `{"id":"plan/m","kind":"chat"}`)
	if v = f.verify(t, "plan/m", `{}`); !v.OK || len(f.mock.Calls()) != 1 {
		t.Fatalf("the first run on the window: %+v", v)
	}
	v = f.verify(t, "plan/m", `{}`)
	// quota_exceeded is 429 (kick-off section 15); Status is not in the JSON.
	if v.OK || len(v.Calls) != 1 || v.Calls[0].Error == nil || v.Calls[0].Error.Code != domain.CodeQuotaExceeded || len(f.mock.Calls()) != 1 {
		t.Errorf("an exhausted window: %+v, %d mock calls", v, len(f.mock.Calls()))
	}
	if _, ok := f.cfg.Verification("plan/m"); !ok {
		t.Error("a failed run removed the stored result of an earlier one")
	}
	f.records(t, 1)
}

// TestVerifyParallelLimit: with the global parallel limit taken by a slow
// traffic request, Admit refuses the first call 429 and the run ends there,
// the two levels left not attempted.
func TestVerifyParallelLimit(t *testing.T) {
	f := newVerifyFixture(t)
	token := f.setUp(t)
	f.must(t, http.StatusOK, http.MethodPatch, "/admin/v1/settings", `{"max_parallel_requests":1,"budget":{"limit_usd":10,"reset_interval":"monthly"}}`)
	f.mock.Script(mockprovider.Step{Delay: 500 * time.Millisecond, Body: chat(5, 0)})
	busy := make(chan int, 1)
	go func() { busy <- f.infer(token) }()
	for deadline := time.Now().Add(5 * time.Second); len(f.mock.Calls()) != 1; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the traffic request never reached the mock")
		}
	}
	v := f.verify(t, "mock/m", `{}`)
	if v.OK || v.Stored || len(v.Calls) != 1 || v.Calls[0].Error == nil || v.Calls[0].Error.Code != domain.CodeParallelLimitExceeded || len(f.mock.Calls()) != 1 {
		t.Errorf("a full parallel limit: %+v, %d mock calls", v, len(f.mock.Calls()))
	}
	if code := <-busy; code != http.StatusOK {
		t.Errorf("the traffic request: status %d", code)
	}
}

// TestVerifyCircuit: verify neither opens a circuit for traffic nor obeys
// one (phase-4 design section 15 point 13): five failing verify calls leave
// traffic reaching the target, and a circuit traffic opened does not keep a
// verify call from it.
func TestVerifyCircuit(t *testing.T) {
	f := newVerifyFixture(t)
	token := f.setUp(t)
	fail := mockprovider.Step{Status: http.StatusInternalServerError, Body: []byte(`{"error":{"message":"down"}}`)}
	f.mock.Script(fail, fail, fail, fail, fail, fail)
	for range 2 {
		if v := f.verify(t, "mock/m", `{}`); v.OK || len(v.Calls) != 3 {
			t.Fatalf("a failing run: %+v", v)
		}
	}
	if code := f.infer(token); code != http.StatusOK || len(f.mock.Calls()) != 7 {
		t.Fatalf("traffic after six failed verify calls: status %d, %d mock calls", code, len(f.mock.Calls()))
	}
	f.mock.Script(fail, fail, fail, fail, fail)
	for range 5 {
		f.infer(token)
	}
	if code := f.infer(token); code != http.StatusServiceUnavailable || len(f.mock.Calls()) != 12 {
		t.Fatalf("the sixth failure in a row: status %d, %d mock calls; want the circuit open", code, len(f.mock.Calls()))
	}
	if v := f.verify(t, "mock/m", `{}`); !v.OK || len(f.mock.Calls()) != 15 {
		t.Errorf("verify under an open circuit: %+v, %d mock calls", v, len(f.mock.Calls()))
	}
}

// TestVerifyStop: a stop interrupts a verify call (its context derives from
// the server's base) and ends the run: the call ends with 500 internal_error
// "server stopping" and its record, the levels left are not attempted and
// leave no record, the run is audited failed and Flight is back at zero.
func TestVerifyStop(t *testing.T) {
	f := newVerifyFixture(t)
	f.setUp(t)
	f.mock.Script(mockprovider.Step{Hang: true})
	done := make(chan verifyAnswer, 1)
	go func() { v, _ := f.run("mock/m", `{}`); done <- v }()
	for deadline := time.Now().Add(5 * time.Second); len(f.mock.Calls()) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the run never reached the mock")
		}
	}
	f.halt(api.ErrStopping)
	v := <-done
	if v.OK || v.Stored || len(v.Calls) != 1 || v.Calls[0].Error == nil || v.Calls[0].Error.Code != domain.CodeInternalError ||
		v.Calls[0].Error.Message != api.ErrStopping.Error() || len(f.mock.Calls()) != 1 {
		t.Errorf("a stopped run: %+v, %d mock calls", v, len(f.mock.Calls()))
	}
	f.flight.Wait()
	f.records(t, 1)
	if got := f.lastAudit(t); got != "targets verify mock/m target mock/m failed" {
		t.Errorf("audit %q", got)
	}
}

// TestVerifyAlongsideTraffic runs verify runs and chat requests on one data
// plane at once (run under -race in make ci): every request and every run
// completes, each record lands under its own key, the client keys' budgets
// carry only the traffic, and Flight returns to zero.
func TestVerifyAlongsideTraffic(t *testing.T) {
	f := newVerifyFixture(t)
	token := f.setUp(t)
	const runs, requests = 4, 12
	var wg sync.WaitGroup
	var bad atomic.Int32
	for range runs {
		wg.Go(func() {
			if v, err := f.run("mock/m", `{}`); err != nil || !v.OK {
				bad.Add(1)
			}
		})
	}
	for range requests {
		wg.Go(func() {
			if f.infer(token) != http.StatusOK {
				bad.Add(1)
			}
		})
	}
	wg.Wait()
	f.flight.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d runs or requests failed", bad.Load())
	}
	f.records(t, 3*runs)
	for deadline := time.Now().Add(5 * time.Second); f.count(t, "SELECT COUNT(*) FROM usage WHERE key_name = 'k' AND model = 'fast'") != requests; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d records of k", f.count(t, "SELECT COUNT(*) FROM usage WHERE key_name = 'k'"))
		}
	}
	monthly := domain.Budget{LimitUSD: new(10.0), ResetInterval: domain.ResetMonthly}
	key := f.limits.Spent(admission.Budget{Scope: "key:k", Spec: monthly})
	global := f.limits.Spent(admission.Budget{Scope: "global", Spec: monthly})
	if key <= 0 || global <= key {
		t.Errorf("key spend %v, global spend %v: the global budget carries traffic and verify, the key's traffic alone", key, global)
	}
	if n := f.count(t, "SELECT COUNT(*) FROM audit WHERE action = 'targets verify mock/m'"); n != runs {
		t.Errorf("%d audit rows, want %d", n, runs)
	}
}
