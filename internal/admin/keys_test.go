package admin

import (
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
)

type keyAnswer struct {
	config.Result
	Resource keyView
}

var tokenShape = regexp.MustCompile(`^sk-stargaite-([a-z2-7]{12})-[a-z2-7]{52}$`)

func (f *fixture) createKey(t *testing.T, body string) keyAnswer {
	t.Helper()
	var res keyAnswer
	decode(t, f.do(t, http.MethodPost, "/admin/v1/keys", []byte(body)), http.StatusCreated, &res)
	return res
}

// TestCreateKey: 201 with the token once and the key as a read shows it;
// models is required, a name is unique for ever and RSN-9 holds.
func TestCreateKey(t *testing.T) {
	f := newFixture(t)
	res := f.createKey(t, `{"name":"my-agent","models":["fast"],"rpm_limit":60,"quota_share":{"p":0.5}}`)
	m := tokenShape.FindStringSubmatch(res.Secrets["my-agent"])
	if m == nil || len(res.Secrets) != 1 || !slices.Equal(entries(res.Diff), []string{"key my-agent created"}) || res.Revision != 1 {
		t.Fatalf("answer %+v", res)
	}
	want := keyView{"my-agent", domain.Key{Models: []string{"fast"}, RPMLimit: 60, QuotaShare: map[string]float64{"p": 0.5}}, []config.SecretView{{PublicID: m[1]}}}
	if !reflect.DeepEqual(res.Resource, want) {
		t.Errorf("resource %+v", res.Resource)
	}
	// An unknown alias and an unknown provider in quota_share are warnings.
	if got := codes(res.Findings); !slices.Contains(got, "warning unknown_alias keys/my-agent models") || !slices.Contains(got, "warning unknown_provider keys/my-agent quota_share.p") {
		t.Errorf("findings %q", got)
	}
	if res := f.createKey(t, `{"name":"none","models":[]}`); res.Resource.Models == nil || len(res.Resource.Models) != 0 {
		t.Errorf("an empty list: %+v", res.Resource)
	}
	f.createKey(t, `{"name":"all","models":["*"]}`)
	decode(t, f.do(t, http.MethodPost, "/admin/v1/keys/all/revoke", nil), http.StatusOK, &config.Result{})

	revision := f.cfg.Snapshot().Revision
	for _, tc := range []struct {
		body, code, audit string
		status            int
		finding           string
	}{
		{`{"name":"my-agent","models":["fast"]}`, domain.CodeConflict, "keys create my-agent key my-agent refused:conflict", 409, ""},
		{`{"name":"all","models":["fast"]}`, domain.CodeConflict, "keys create all key all refused:conflict", 409, ""},
		{`{"name":"k"}`, domain.CodeInvalidRequest, "keys create k key k refused:invalid_request", 400, "error required keys/k models"},
		{`{"name":"k","models":null}`, domain.CodeInvalidRequest, "keys create k key k refused:invalid_request", 400, "error required keys/k models"},
		{`{"name":"_k","models":[]}`, domain.CodeInvalidRequest, "keys create _k key _k refused:invalid_request", 400, "error reserved_name keys/_k "},
		{`{"name":"a/b","models":[]}`, domain.CodeInvalidRequest, "keys create a/b key a/b refused:invalid_request", 400, "error invalid_name keys/a/b "},
		{`{"name":"k","models":["*","fast"]}`, domain.CodeInvalidRequest, "keys create k key k refused:invalid_request", 400, "error invalid_value keys/k models"},
		{`{"name":"k","models":["a","a"]}`, domain.CodeInvalidRequest, "keys create k key k refused:invalid_request", 400, "error duplicate_alias keys/k models"},
		{`{"name":"k","models":[],"secret":"x"}`, domain.CodeInvalidRequest, "keys create k key k refused:invalid_request", 400, ""},
		{`{"models":[]}`, domain.CodeInvalidRequest, "", 400, ""},
		{`{"name":1,"models":[]}`, domain.CodeInvalidRequest, "", 400, ""},
		{`[]`, domain.CodeInvalidRequest, "", 400, ""},
	} {
		rows := f.count(t, "SELECT COUNT(*) FROM audit")
		e := errorOf(t, f.do(t, http.MethodPost, "/admin/v1/keys", []byte(tc.body)), tc.status, tc.code)
		if tc.finding != "" && !slices.Contains(codes(e.Findings), tc.finding) {
			t.Errorf("%s: findings %q", tc.body, codes(e.Findings))
		}
		if n := f.count(t, "SELECT COUNT(*) FROM audit"); (tc.audit == "") != (n == rows) || (tc.audit != "" && f.lastAudit(t) != tc.audit) {
			t.Errorf("%s: %d audit rows, last %q", tc.body, n-rows, f.lastAudit(t))
		}
	}
	if f.cfg.Snapshot().Revision != revision {
		t.Error("a refused request published")
	}
}

// TestKeyReads: the list shape, sorted, and show; a revoked key is absent.
func TestKeyReads(t *testing.T) {
	f := newFixture(t)
	var list struct{ Keys []keyView }
	decode(t, f.do(t, http.MethodGet, "/admin/v1/keys", nil), http.StatusOK, &list)
	if list.Keys == nil || len(list.Keys) != 0 {
		t.Errorf("empty instance: %+v", list)
	}
	f.apply(t, "", readExample(t))
	f.createKey(t, `{"name":"a","models":["*"]}`)
	f.createKey(t, `{"name":"z","models":[]}`)
	decode(t, f.do(t, http.MethodPost, "/admin/v1/keys/z/revoke", []byte(`{}`)), http.StatusOK, &config.Result{})
	decode(t, f.do(t, http.MethodGet, "/admin/v1/keys", nil), http.StatusOK, &list)
	doc := f.cfg.Snapshot().Doc
	var names []string
	for _, k := range list.Keys {
		names = append(names, k.Name)
		if !reflect.DeepEqual(k.Key, doc.Keys[k.Name]) || len(k.Secrets) != 1 || k.Secrets[0].ValidUntil != 0 {
			t.Errorf("key %s: %+v", k.Name, k)
		}
	}
	if !slices.Equal(names, []string{"a", "classifier-flow", "invoice-agent"}) {
		t.Errorf("names %q", names)
	}
	var one keyView
	decode(t, f.do(t, http.MethodGet, "/admin/v1/keys/invoice-agent", nil), http.StatusOK, &one)
	if !reflect.DeepEqual(one, list.Keys[2]) {
		t.Errorf("show %+v", one)
	}
	errorOf(t, f.do(t, http.MethodGet, "/admin/v1/keys/z", nil), http.StatusNotFound, domain.CodeNotFound)
	errorOf(t, f.do(t, http.MethodGet, "/admin/v1/keys/nope", nil), http.StatusNotFound, domain.CodeNotFound)
	errorOf(t, f.do(t, http.MethodDelete, "/admin/v1/keys/a", nil), http.StatusMethodNotAllowed, domain.CodeInvalidRequest)
}

// TestKeyChanges: set merges and keeps the secrets; rotate and revoke act on
// a declared key only; refusals are audited, a malformed body is not.
func TestKeyChanges(t *testing.T) {
	f := newFixture(t)
	created := f.createKey(t, `{"name":"k","models":["fast"],"budget":{"limit_usd":5,"reset_interval":"daily"}}`)
	var res keyAnswer
	decode(t, f.do(t, http.MethodPatch, "/admin/v1/keys/k", []byte(`{"models":["*"],"budget":{"limit_usd":7},"rpm_limit":3}`)), http.StatusOK, &res)
	want := domain.Key{Models: []string{"*"}, Budget: &domain.Budget{LimitUSD: new(7.0), ResetInterval: "daily"}, RPMLimit: 3}
	if got := f.cfg.Snapshot().Doc.Keys["k"]; !reflect.DeepEqual(got, want) || !reflect.DeepEqual(res.Resource.Key, want) ||
		!reflect.DeepEqual(res.Resource.Secrets, created.Resource.Secrets) || len(res.Secrets) != 0 || !slices.Equal(entries(res.Diff), []string{"key k updated"}) {
		t.Errorf("stored %+v, answer %+v", got, res)
	}

	decode(t, f.do(t, http.MethodPost, "/admin/v1/keys/k/rotate", []byte(`{"grace":"90s"}`)), http.StatusOK, &res)
	m := tokenShape.FindStringSubmatch(res.Secrets["k"])
	old := created.Resource.Secrets[0].PublicID
	if m == nil || !slices.Equal(entries(res.Diff), []string{"key k rotated"}) || len(res.Resource.Secrets) != 2 ||
		!slices.Contains(res.Resource.Secrets, config.SecretView{PublicID: old, ValidUntil: testNow + 90_000}) ||
		!slices.Contains(res.Resource.Secrets, config.SecretView{PublicID: m[1]}) {
		t.Fatalf("rotate: %+v", res)
	}
	if string(res.Diff[0].Previous) != `[{"public_id":"`+old+`"}]` {
		t.Errorf("previous %s", res.Diff[0].Previous)
	}

	revision := f.cfg.Snapshot().Revision
	for _, tc := range []struct {
		method, path, body, code, audit string
		status                          int
	}{
		{http.MethodPatch, "/admin/v1/keys/nope", `{}`, domain.CodeNotFound, "keys set nope key nope refused:not_found", 404},
		{http.MethodPatch, "/admin/v1/keys/k", `{"models":null}`, domain.CodeInvalidRequest, "keys set k key k refused:invalid_request", 400},
		{http.MethodPatch, "/admin/v1/keys/k", `{"name":"x"}`, domain.CodeInvalidRequest, "keys set k key k refused:invalid_request", 400},
		{http.MethodPatch, "/admin/v1/keys/k", `{"secrets":[]}`, domain.CodeInvalidRequest, "keys set k key k refused:invalid_request", 400},
		{http.MethodPost, "/admin/v1/keys/nope/rotate", ``, domain.CodeNotFound, "keys rotate nope key nope refused:not_found", 404},
		{http.MethodPost, "/admin/v1/keys/k/rotate", `{"grace":"-1s"}`, domain.CodeInvalidRequest, "", 400},
		{http.MethodPost, "/admin/v1/keys/k/rotate", `{"grace":"1 hour"}`, domain.CodeInvalidRequest, "", 400},
		{http.MethodPost, "/admin/v1/keys/k/rotate", `{"grace":3600}`, domain.CodeInvalidRequest, "", 400},
		{http.MethodPost, "/admin/v1/keys/k/rotate", `{"grace":"1h","grace":"2h"}`, domain.CodeInvalidRequest, "", 400},
		{http.MethodPost, "/admin/v1/keys/k/rotate", `{"graze":"1h"}`, domain.CodeInvalidRequest, "", 400},
		{http.MethodPost, "/admin/v1/keys/k/revoke", `{"grace":"1h"}`, domain.CodeInvalidRequest, "", 400},
		{http.MethodPost, "/admin/v1/keys/nope/revoke", ``, domain.CodeNotFound, "keys revoke nope key nope refused:not_found", 404},
	} {
		rows := f.count(t, "SELECT COUNT(*) FROM audit")
		errorOf(t, f.do(t, tc.method, tc.path, []byte(tc.body)), tc.status, tc.code)
		if n := f.count(t, "SELECT COUNT(*) FROM audit"); (tc.audit == "") != (n == rows) || (tc.audit != "" && f.lastAudit(t) != tc.audit) {
			t.Errorf("%s %s %s: %d audit rows, last %q", tc.method, tc.path, tc.body, n-rows, f.lastAudit(t))
		}
	}
	if f.cfg.Snapshot().Revision != revision {
		t.Error("a refused request published")
	}

	var revoked config.Result
	decode(t, f.do(t, http.MethodPost, "/admin/v1/keys/k/revoke", nil), http.StatusOK, &revoked)
	if !slices.Equal(entries(revoked.Diff), []string{"key k revoked"}) || f.count(t, "SELECT COUNT(*) FROM key_secrets") != 0 ||
		f.count(t, "SELECT COUNT(*) FROM keys WHERE name = 'k' AND revoked_at = 1790000000000") != 1 {
		t.Errorf("revoke: %+v", revoked)
	}
	// A revoked key is kept and can no longer change.
	for _, tc := range []struct{ method, path, body, audit string }{
		{http.MethodPost, "/admin/v1/keys/k/revoke", ``, "keys revoke k key k refused:conflict"},
		{http.MethodPost, "/admin/v1/keys/k/rotate", ``, "keys rotate k key k refused:conflict"},
		{http.MethodPatch, "/admin/v1/keys/k", `{}`, "keys set k key k refused:conflict"},
		{http.MethodPost, "/admin/v1/keys", `{"name":"k","models":[]}`, "keys create k key k refused:conflict"},
	} {
		errorOf(t, f.do(t, tc.method, tc.path, []byte(tc.body)), http.StatusConflict, domain.CodeConflict)
		if f.lastAudit(t) != tc.audit {
			t.Errorf("%s %s: audit %q", tc.method, tc.path, f.lastAudit(t))
		}
	}
	if bytes := f.export(t); strings.Contains(string(bytes), "k:") {
		t.Errorf("export shows a revoked key:\n%s", bytes)
	}
}

// TestPlantedKeyToken: a token issued by keys create or keys rotate is in
// that one response and nowhere else: not in a list or a show, an export, a
// validate, an error message, an audit row or any other row, or the log.
func TestPlantedKeyToken(t *testing.T) {
	f := newFixture(t)
	f.apply(t, "", readExample(t))
	var tokens []string
	tokens = append(tokens, f.createKey(t, `{"name":"k","models":["fast"]}`).Secrets["k"])
	var res keyAnswer
	decode(t, f.do(t, http.MethodPost, "/admin/v1/keys/k/rotate", nil), http.StatusOK, &res)
	tokens = append(tokens, res.Secrets["k"])

	var seen strings.Builder
	for _, target := range []string{"GET /admin/v1/keys", "GET /admin/v1/keys/k", "GET /admin/v1/export", "GET /admin/v1/export?format=json", "GET /admin/v1/validate"} {
		method, path, _ := strings.Cut(target, " ")
		seen.WriteString(f.do(t, method, path, nil).Body.String())
	}
	for _, body := range []string{`{"models":null}`, `{"models":["*","x"]}`, `{"colour":1}`} {
		seen.WriteString(f.do(t, http.MethodPatch, "/admin/v1/keys/k", []byte(body)).Body.String())
	}
	seen.WriteString(f.do(t, http.MethodPost, "/admin/v1/keys", []byte(`{"name":"k","models":[]}`)).Body.String())
	seen.WriteString(f.do(t, http.MethodPost, "/admin/v1/keys/k/rotate", []byte(`{"grace":"x"}`)).Body.String())
	decode(t, f.do(t, http.MethodPost, "/admin/v1/keys/k/revoke", nil), http.StatusOK, &config.Result{})
	seen.WriteString(f.log.String())
	seen.WriteString(f.dump(t))
	for _, token := range tokens {
		if !tokenShape.MatchString(token) {
			t.Fatalf("token %q", token)
		}
		if strings.Contains(seen.String(), token) || strings.Contains(seen.String(), token[26:]) {
			t.Error("a token appears outside the response that issued it")
		}
	}
	if strings.Contains(seen.String(), testKey) {
		t.Error("the admin key appears in an answer, the log or the store")
	}
}

// TestNoRedirect: a path the mux would clean, or complete with a trailing
// "/", is refused in the envelope instead of redirected, so a client that
// follows redirects never writes to another target than the one it named.
// Authentication still comes first.
func TestNoRedirect(t *testing.T) {
	f := newFixture(t)
	f.addProvider(t, `{"name":"ollama","allow_private_network":true}`)
	for _, id := range []string{"ollama/m", "ollama/q", "ollama/a/b"} {
		f.addTarget(t, `{"id":"`+id+`","kind":"embeddings"}`)
	}
	before := f.export(t)
	for _, path := range []string{
		"/admin/v1/targets/ollama/x/../m", "/admin/v1/targets/ollama//m", "/admin/v1/targets/ollama/./q",
		"/admin/v1/targets/ollama/a//b", "/admin/v1/targets/ollama/a/b/..", "/admin/v1/keys/../providers/ollama",
		"/admin/v1//settings", "/admin/v1/providers/ollama/../ollama",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete} {
			errorOf(t, f.as(t, "", method, path, []byte(`{"max_output_tokens":1}`)), http.StatusUnauthorized, domain.CodeInvalidAPIKey)
			w := f.do(t, method, path, []byte(`{"max_output_tokens":1}`))
			e := errorOf(t, w, http.StatusBadRequest, domain.CodeInvalidRequest)
			if w.Header().Get("Location") != "" || !strings.Contains(e.Message, "%2F") {
				t.Errorf("%s %s: Location %q, message %q", method, path, w.Header().Get("Location"), e.Message)
			}
		}
	}
	// The mux would send a write to a collection on to its trailing "/".
	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		w := f.do(t, method, "/admin/v1/targets", []byte(`{}`))
		if errorOf(t, w, http.StatusBadRequest, domain.CodeInvalidRequest); w.Header().Get("Location") != "" {
			t.Errorf("%s: Location %q", method, w.Header().Get("Location"))
		}
	}
	if got := f.export(t); string(got) != string(before) {
		t.Errorf("a refused path changed the configuration:\n%s", got)
	}
	// The request line records the answer, never the redirect.
	refused := 0
	for l := range strings.Lines(f.log.String()) {
		var line struct{ Status int }
		if err := json.Unmarshal([]byte(l), &line); err != nil || line.Status/100 == 3 {
			t.Errorf("request line %s", l)
		}
		if line.Status == http.StatusBadRequest {
			refused++
		}
	}
	if refused != 8*3+2 {
		t.Errorf("%d request lines with status 400", refused)
	}
}
