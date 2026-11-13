package admin

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/presets"
)

// providerAnswer is the answer to an accepted provider change.
type providerAnswer struct {
	config.Result
	Resource providerView
}

func (f *fixture) addProvider(t *testing.T, body string) providerAnswer {
	t.Helper()
	var res providerAnswer
	decode(t, f.do(t, http.MethodPost, "/admin/v1/providers", []byte(body)), http.StatusCreated, &res)
	return res
}

func preset(t *testing.T, name string) domain.Provider {
	t.Helper()
	i := slices.IndexFunc(presets.Catalog, func(p presets.Preset) bool { return p.Name == name })
	if i < 0 {
		t.Fatalf("no preset %s", name)
	}
	return presets.Catalog[i].Provider
}

func codes(findings []domain.Finding) []string {
	out := []string{}
	for _, x := range findings {
		out = append(out, x.Severity+" "+x.Code+" "+x.Resource+" "+x.Field)
	}
	return out
}

// TestPathValue: a route with a wildcard reads it. The wrapper used to call
// the handler ServeMux.Handler returns, which leaves r.PathValue empty, so
// this read answered 404.
func TestPathValue(t *testing.T) {
	f := newFixture(t)
	f.addProvider(t, `{"name":"openrouter"}`)
	var got providerView
	decode(t, f.do(t, http.MethodGet, "/admin/v1/providers/openrouter", nil), http.StatusOK, &got)
	if got.Name != "openrouter" || !reflect.DeepEqual(got.Provider, preset(t, "openrouter")) {
		t.Errorf("read %+v", got)
	}
	errorOf(t, f.do(t, http.MethodGet, "/admin/v1/providers/nope", nil), http.StatusNotFound, domain.CodeNotFound)
	errorOf(t, f.do(t, http.MethodGet, "/admin/v1/providers/openrouter/x", nil), http.StatusNotFound, domain.CodeNotFound)
	w := f.do(t, http.MethodPut, "/admin/v1/providers/openrouter", nil)
	errorOf(t, w, http.StatusMethodNotAllowed, domain.CodeInvalidRequest)
	if allow := w.Header().Get("Allow"); !strings.Contains(allow, http.MethodPatch) || !strings.Contains(allow, http.MethodDelete) {
		t.Errorf("Allow %q", allow)
	}
}

// TestPresets: the catalog as GET /presets answers it (AC-53).
func TestPresets(t *testing.T) {
	f := newFixture(t)
	var got struct{ Presets []presets.Preset }
	decode(t, f.do(t, http.MethodGet, "/admin/v1/presets", nil), http.StatusOK, &got)
	if !reflect.DeepEqual(got.Presets, presets.Catalog) {
		t.Errorf("presets %+v", got.Presets)
	}
}

// TestAddProvider: a provider stores exactly its preset plus the explicit
// members (AC-53); then only the stored declaration counts.
func TestAddProvider(t *testing.T) {
	f := newFixture(t)
	var annexB domain.Document
	if err := config.DecodeYAML(readExample(t), &annexB); err != nil {
		t.Fatal(err)
	}
	withDialect := preset(t, "openai")
	withDialect.TargetDefaults.Dialect = domain.DialectReasoningObject
	noKey := preset(t, "anthropic")
	noKey.APIKeyEnv = ""

	for _, tc := range []struct {
		name, body string
		want       domain.Provider
		credential string
	}{
		{"openrouter", `{"name":"openrouter"}`, preset(t, "openrouter"), "resolved"},
		// The plan of kick-off section 7.3, which gives the provider of annex B.
		{"minimax-plan", `{"name":"minimax-plan","preset":"minimax","protocol":"anthropic","base_url":"https://api.minimax.io/anthropic",
			"api_key_env":"MINIMAX_PLAN_API_KEY","target_defaults":{"dialect":"adaptive_effort","output_cap_param":null},"billing":"subscription",
			"quota":[{"window":"5h","requests":1500},{"window":"7d","requests":15000}]}`, annexB.Providers["minimax-plan"], "resolved"},
		// A provider that no preset names takes only what the request declares.
		{"my-vllm", `{"name":"my-vllm","protocol":"openai","base_url":"http://vllm.internal:8000/v1","allow_private_network":true,"billing":"none"}`,
			domain.Provider{Protocol: "openai", BaseURL: "http://vllm.internal:8000/v1", AllowPrivateNetwork: true, Billing: "none"}, "none"},
		// An object member merges into the preset's; null removes a member.
		{"mine", `{"name":"mine","preset":"openai","target_defaults":{"dialect":"reasoning_object"}}`, withDialect, "unresolved"},
		{"anthropic", `{"name":"anthropic","api_key_env":null}`, noKey, "none"},
	} {
		res := f.addProvider(t, tc.body)
		if got := f.cfg.Snapshot().Doc.Providers[tc.name]; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: stored %+v, want %+v", tc.name, got, tc.want)
		}
		if !reflect.DeepEqual(res.Resource, providerView{tc.name, tc.want, tc.credential}) || !slices.Equal(entries(res.Diff), []string{"provider " + tc.name + " created"}) {
			t.Errorf("%s: answer %+v", tc.name, res)
		}
	}

	// The preset is copied, not referred to.
	saved := presets.Catalog[2]
	presets.Catalog[2].BaseURL = "https://changed.example.com"
	var got providerView
	decode(t, f.do(t, http.MethodGet, "/admin/v1/providers/openrouter", nil), http.StatusOK, &got)
	presets.Catalog[2] = saved
	if got.BaseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("the stored provider follows the catalog: %+v", got)
	}

	// The presets without a base URL, and ollama on plain HTTP, are refused
	// without the flag the kick-off names, and accepted with it.
	for _, tc := range []struct{ without, with, finding string }{
		{`{"name":"dashscope"}`, `{"name":"dashscope","base_url":"https://dashscope-intl.aliyuncs.com/compatible-mode/v1"}`, "error required providers/dashscope base_url"},
		{`{"name":"tei"}`, "", "error required providers/tei base_url"},
		{`{"name":"tei","base_url":"http://localhost:8080/v1"}`, `{"name":"tei","base_url":"http://localhost:8080/v1","allow_private_network":true}`, "error url_refused providers/tei base_url"},
		{`{"name":"ollama"}`, `{"name":"ollama","allow_private_network":true}`, "error url_refused providers/ollama base_url"},
	} {
		e := errorOf(t, f.do(t, http.MethodPost, "/admin/v1/providers", []byte(tc.without)), http.StatusBadRequest, domain.CodeInvalidRequest)
		if got := codes(e.Findings); !slices.Contains(got, tc.finding) || slices.ContainsFunc(got, func(c string) bool { return c != tc.finding && strings.HasPrefix(c, "error") }) {
			t.Errorf("%s: findings %q", tc.without, got)
		}
		if tc.with != "" {
			f.addProvider(t, tc.with)
		}
	}

	revision := f.cfg.Snapshot().Revision
	for _, tc := range []struct {
		body   string
		status int
		code   string
		audit  string // the last audit row, when the refusal is audited
	}{
		{`{"name":"openrouter"}`, 409, domain.CodeConflict, "providers add openrouter provider openrouter refused:conflict"},
		{`{"name":"x","preset":"nope"}`, 400, domain.CodeInvalidRequest, "providers add x provider x refused:invalid_request"},
		{`{"name":"x","preset":"openai","colour":"blue"}`, 400, domain.CodeInvalidRequest, "providers add x provider x refused:invalid_request"},
		{`{"name":"bad name","preset":"openai"}`, 400, domain.CodeInvalidRequest, "providers add bad name provider bad name refused:invalid_request"},
		{`{"preset":"openai"}`, 400, domain.CodeInvalidRequest, ""},
		{`{"name":7}`, 400, domain.CodeInvalidRequest, ""},
		{`{"name":"x","preset":null}`, 400, domain.CodeInvalidRequest, ""},
		{`{"name":"x","name":"y"}`, 400, domain.CodeInvalidRequest, ""},
		{`["x"]`, 400, domain.CodeInvalidRequest, ""},
		{`{"name":"x"}` + strings.Repeat(" ", maxBody), 413, domain.CodeRequestTooLarge, ""},
	} {
		rows := f.count(t, "SELECT COUNT(*) FROM audit")
		errorOf(t, f.do(t, http.MethodPost, "/admin/v1/providers", []byte(tc.body)), tc.status, tc.code)
		switch n := f.count(t, "SELECT COUNT(*) FROM audit"); {
		case tc.audit == "" && n != rows:
			t.Errorf("%.40s: audited", tc.body)
		case tc.audit != "" && f.lastAudit(t) != tc.audit:
			t.Errorf("%.40s: audit %q", tc.body, f.lastAudit(t))
		}
	}
	if f.cfg.Snapshot().Revision != revision {
		t.Error("a refused request published")
	}
}

func TestSetProvider(t *testing.T) {
	f := newFixture(t)
	f.addProvider(t, `{"name":"openrouter"}`)
	var res providerAnswer
	decode(t, f.do(t, http.MethodPatch, "/admin/v1/providers/openrouter", []byte(`{"stream_usage":"none","cost_report":null,"target_defaults":{"dialect":null}}`)), http.StatusOK, &res)
	want := preset(t, "openrouter")
	want.StreamUsage, want.CostReport, want.TargetDefaults.Dialect = domain.StreamUsageNone, nil, ""
	if got := f.cfg.Snapshot().Doc.Providers["openrouter"]; !reflect.DeepEqual(got, want) || !reflect.DeepEqual(res.Resource.Provider, want) || !slices.Equal(entries(res.Diff), []string{"provider openrouter updated"}) {
		t.Errorf("stored %+v, answer %+v", got, res)
	}
	decode(t, f.do(t, http.MethodPatch, "/admin/v1/providers/openrouter", []byte(`{}`)), http.StatusOK, &res)
	if res.Changed || f.lastAudit(t) != "providers set openrouter provider openrouter unchanged" {
		t.Errorf("empty patch: %+v, audit %q", res, f.lastAudit(t))
	}

	for _, tc := range []struct {
		target, body string
		status       int
		code, audit  string
	}{
		{"nope", `{}`, 404, domain.CodeNotFound, "providers set nope provider nope refused:not_found"},
		{"openrouter", `{"name":"x"}`, 400, domain.CodeInvalidRequest, "providers set openrouter provider openrouter refused:invalid_request"},
		{"openrouter", `{"billing":"monthly"}`, 400, domain.CodeInvalidRequest, "providers set openrouter provider openrouter refused:invalid_request"},
		{"openrouter", `{`, 400, domain.CodeInvalidRequest, ""}, // not JSON: refused before the pipeline, not audited
	} {
		rows := f.count(t, "SELECT COUNT(*) FROM audit")
		errorOf(t, f.do(t, http.MethodPatch, "/admin/v1/providers/"+tc.target, []byte(tc.body)), tc.status, tc.code)
		if n := f.count(t, "SELECT COUNT(*) FROM audit"); (tc.audit == "") != (n == rows) || (tc.audit != "" && f.lastAudit(t) != tc.audit) {
			t.Errorf("%s %s: %d audit rows, last %q", tc.target, tc.body, n-rows, f.lastAudit(t))
		}
	}
	if got := f.cfg.Snapshot().Doc.Providers["openrouter"]; !reflect.DeepEqual(got, want) {
		t.Errorf("a refused patch changed %+v", got)
	}
}

// TestRemoveProvider: a provider a target names is refused by Validate, whose
// finding names the target.
func TestRemoveProvider(t *testing.T) {
	f := newFixture(t)
	f.apply(t, "", readExample(t))
	e := errorOf(t, f.do(t, http.MethodDelete, "/admin/v1/providers/openrouter", nil), http.StatusBadRequest, domain.CodeInvalidRequest)
	if got := codes(e.Findings); !slices.Contains(got, "error unknown_provider targets/openrouter/qwen/qwen-3.6-flash ") {
		t.Errorf("findings %q", got)
	}
	errorOf(t, f.do(t, http.MethodDelete, "/admin/v1/providers/nope", nil), http.StatusNotFound, domain.CodeNotFound)
	if f.lastAudit(t) != "providers remove nope provider nope refused:not_found" {
		t.Errorf("audit %q", f.lastAudit(t))
	}
	f.addProvider(t, `{"name":"groq"}`)
	var res config.Result
	decode(t, f.do(t, http.MethodDelete, "/admin/v1/providers/groq", nil), http.StatusOK, &res)
	if !slices.Equal(entries(res.Diff), []string{"provider groq removed"}) {
		t.Errorf("diff %q", entries(res.Diff))
	}
	errorOf(t, f.do(t, http.MethodGet, "/admin/v1/providers/groq", nil), http.StatusNotFound, domain.CodeNotFound)
}

func TestListProviders(t *testing.T) {
	f := newFixture(t)
	var list struct{ Providers []providerView }
	decode(t, f.do(t, http.MethodGet, "/admin/v1/providers", nil), http.StatusOK, &list)
	if list.Providers == nil || len(list.Providers) != 0 {
		t.Errorf("empty instance: %+v", list)
	}
	f.addProvider(t, `{"name":"openrouter"}`)
	f.addProvider(t, `{"name":"groq"}`)
	f.addProvider(t, `{"name":"ollama","allow_private_network":true}`)
	decode(t, f.do(t, http.MethodGet, "/admin/v1/providers", nil), http.StatusOK, &list)
	var got []string
	for _, p := range list.Providers {
		got = append(got, p.Name+" "+p.Credential)
	}
	if !slices.Equal(got, []string{"groq unresolved", "ollama none", "openrouter resolved"}) {
		t.Errorf("providers %q", got)
	}
}

// TestSetTimeURL: the URL a change sets is resolved; a private address needs
// allow_private_network and a metadata address is refused even with it
// (SEC-4, AC-43 at set time).
func TestSetTimeURL(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		body    string
		refused bool
	}{
		{`{"name":"local","protocol":"openai","billing":"none","base_url":"https://llm.internal.test/v1"}`, true},
		{`{"name":"local","protocol":"openai","billing":"none","base_url":"https://llm.internal.test/v1","allow_private_network":true}`, false},
		{`{"name":"meta","protocol":"openai","billing":"none","base_url":"https://metadata.test/","allow_private_network":true}`, true},
		{`{"name":"meta","protocol":"openai","billing":"none","base_url":"http://169.254.169.254/","allow_private_network":true}`, true},
		{`{"name":"meta","protocol":"openai","billing":"none","base_url":"https://api.example.com/v1","rerank_url":"https://metadata.test/rerank","allow_private_network":true}`, true},
	} {
		w := f.do(t, http.MethodPost, "/admin/v1/providers", []byte(tc.body))
		if !tc.refused {
			decode(t, w, http.StatusCreated, &providerAnswer{})
			continue
		}
		e := errorOf(t, w, http.StatusBadRequest, domain.CodeInvalidRequest)
		if len(e.Findings) != 1 || e.Findings[0].Code != "url_refused" {
			t.Errorf("%s: findings %q", tc.body, codes(e.Findings))
		}
	}
	// A patch that points a public provider at a private address.
	f.addProvider(t, `{"name":"openrouter"}`)
	e := errorOf(t, f.do(t, http.MethodPatch, "/admin/v1/providers/openrouter", []byte(`{"base_url":"https://llm.internal.test/v1"}`)), http.StatusBadRequest, domain.CodeInvalidRequest)
	if !slices.Equal(codes(e.Findings), []string{"error url_refused providers/openrouter base_url"}) {
		t.Errorf("findings %q", codes(e.Findings))
	}
}

// TestEnvFileCredential: a credential added to the env file resolves after
// the next accepted admin write, with no restart, and an unresolved one only
// disables its provider (OPS-6, AC-44). The planted value appears in no
// answer, log line or row of the store (SEC-5).
func TestEnvFileCredential(t *testing.T) {
	const planted = "sk-PLANTED-credential-value"
	envFile := filepath.Join(t.TempDir(), "env")
	writeEnv := func(content string) {
		if err := os.WriteFile(envFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeEnv("# nothing yet\n")
	f := newFixtureEnv(t, func() config.Env {
		return config.NewEnv(func(string) (string, bool) { return "", false }, envFile, resolve)
	})
	var seen strings.Builder
	do := func(method, target, body string) []byte {
		w := f.do(t, method, target, []byte(body))
		seen.Write(w.Body.Bytes())
		return w.Body.Bytes()
	}
	credential := func() string {
		var got providerView
		w := f.do(t, http.MethodGet, "/admin/v1/providers/openrouter", nil)
		seen.Write(w.Body.Bytes())
		decode(t, w, http.StatusOK, &got)
		return got.Credential
	}

	res := f.addProvider(t, `{"name":"openrouter"}`)
	if !slices.Equal(codes(res.Findings), []string{"warning credential_unresolved providers/openrouter api_key_env"}) || res.Resource.Credential != "unresolved" {
		t.Errorf("answer %+v", res)
	}
	if p := f.cfg.Snapshot().Providers["openrouter"]; !p.Disabled {
		t.Error("an unresolved provider is not disabled")
	}

	writeEnv("OPENROUTER_API_KEY=" + planted + "\n")
	if got := credential(); got != "unresolved" {
		t.Errorf("before any write: %s", got)
	}
	do(http.MethodPatch, "/admin/v1/settings", `{"max_parallel_requests":9}`)
	if got := credential(); got != "resolved" || f.cfg.Snapshot().Providers["openrouter"].Disabled {
		t.Errorf("after an accepted write: %s", got)
	}

	for _, target := range []string{"/admin/v1/providers", "/admin/v1/export", "/admin/v1/export?format=json", "/admin/v1/validate", "/admin/v1/presets", "/admin/v1/settings"} {
		do(http.MethodGet, target, "")
	}
	do(http.MethodPost, "/admin/v1/providers", `{"name":"groq","api_key_env":"OPENROUTER_API_KEY"}`)
	do(http.MethodPatch, "/admin/v1/providers/groq", `{"billing":"none"}`)
	do(http.MethodPost, "/admin/v1/providers", `{"name":"groq"}`)
	do(http.MethodPost, "/admin/v1/apply?dry_run=true", "version: 1\nproviders:\n  x: {protocol: openai, base_url: https://api.example.com, api_key_env: OPENROUTER_API_KEY, billing: none}\n")
	do(http.MethodDelete, "/admin/v1/providers/groq", "")

	writeEnv("# removed again\n")
	do(http.MethodPatch, "/admin/v1/settings", `{"max_parallel_requests":8}`)
	if got := credential(); got != "unresolved" {
		t.Errorf("after the variable left the file: %s", got)
	}
	seen.WriteString(f.log.String())
	seen.WriteString(f.dump(t))
	if strings.Contains(seen.String(), planted) || strings.Contains(seen.String(), "PLANTED") {
		t.Error("the planted credential appears in an answer, the log or the store")
	}
	if !strings.Contains(seen.String(), "OPENROUTER_API_KEY") {
		t.Error("the check saw nothing")
	}
}
