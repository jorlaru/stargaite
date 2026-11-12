package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/auth"
	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/store"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const (
	testKey = "test-admin-key-of-at-least-32-characters"
	testNow = 1_790_000_000_000
	example = "../../config.example.yaml"
	golden  = "testdata/export.yaml"
)

// exampleEnv resolves every credential config.example.yaml references.
var exampleEnv = map[string]string{
	"OPENROUTER_API_KEY": "x", "ANTHROPIC_API_KEY": "x", "MINIMAX_PLAN_API_KEY": "x",
	"LANGFUSE_PUBLIC_KEY": "x", "LANGFUSE_SECRET_KEY": "x",
}

// resolve answers every lookup without the network: localhost is loopback,
// any other name a public address.
func resolve(_ context.Context, host string) ([]netip.Addr, error) {
	if host == "localhost" {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

type fixture struct {
	h   http.Handler
	cfg *config.Service
	st  *store.Store
	log *bytes.Buffer
}

// newFixture is an admin handler on an empty store. Requests run through
// ServeHTTP in the test's goroutine, so the log buffer needs no lock.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	env := func() config.Env {
		return config.Env{Lookup: func(n string) (string, bool) { v, ok := exampleEnv[n]; return v, ok }, Resolve: resolve}
	}
	cfg, err := config.Open(context.Background(), st, env, func() time.Time { return time.UnixMilli(testNow) })
	if err != nil {
		t.Fatal(err)
	}
	log := &bytes.Buffer{}
	return &fixture{Handler(cfg, sha256.Sum256([]byte(testKey)), slog.New(slog.NewJSONHandler(log, nil))), cfg, st, log}
}

// as sends one request with the Authorization header given.
func (f *fixture) as(t *testing.T, authorization, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Header().Get(RequestIDHeader) == "" {
		t.Errorf("%s %s: no %s", method, target, RequestIDHeader)
	}
	return w
}

// do sends one request with the admin key.
func (f *fixture) do(t *testing.T, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	return f.as(t, "Bearer "+testKey, method, target, body)
}

// decode checks the status and reads a JSON answer into v.
func decode(t *testing.T, w *httptest.ResponseRecorder, status int, v any) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q: %s", ct, w.Body)
	}
	dec := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		t.Fatalf("answer %s: %v", w.Body, err)
	}
}

// errorOf checks that w is the envelope of code with status.
func errorOf(t *testing.T, w *httptest.ResponseRecorder, status int, code string) domain.Error {
	t.Helper()
	var env struct{ Error domain.Error }
	decode(t, w, status, &env)
	if env.Error.Code != code || env.Error.Type == "" || env.Error.Message == "" {
		t.Fatalf("error %+v, want code %s", env.Error, code)
	}
	return env.Error
}

func readExample(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// apply posts file and returns the result of an accepted change.
func (f *fixture) apply(t *testing.T, query string, file []byte) config.Result {
	t.Helper()
	var res config.Result
	decode(t, f.do(t, http.MethodPost, "/admin/v1/apply"+query, file), http.StatusOK, &res)
	return res
}

func (f *fixture) export(t *testing.T) []byte {
	t.Helper()
	w := f.do(t, http.MethodGet, "/admin/v1/export", nil)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/yaml" {
		t.Fatalf("export: status %d, Content-Type %q: %s", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	return w.Body.Bytes()
}

func (f *fixture) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := f.st.Read().QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fixture) lastAudit(t *testing.T) string {
	t.Helper()
	var action, kind, name, result string
	if err := f.st.Read().QueryRow("SELECT action, resource_type, resource_name, result FROM audit ORDER BY rowid DESC LIMIT 1").Scan(&action, &kind, &name, &result); err != nil {
		t.Fatal(err)
	}
	return strings.Join([]string{action, kind, name, result}, " ")
}

// dump renders every row of every table, for the planted-token check.
func (f *fixture) dump(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	tables, err := f.st.Read().Query("SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	_ = tables.Close()
	for _, name := range names {
		rows, err := f.st.Read().Query("SELECT * FROM " + name)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s %s\n", name, vals)
		}
		_ = rows.Close()
	}
	return b.String()
}

func entries(diff []config.DiffEntry) []string {
	out := []string{}
	for _, e := range diff {
		out = append(out, e.Kind+" "+e.Name+" "+e.Action)
	}
	return out
}

func TestKey(t *testing.T) {
	none := func(string) string { return "" }
	given := func(key string) func(string) string { return func(string) string { return key } }

	// Generated: 52 lowercase base32 characters, mode 0600, and nothing else
	// left in the directory.
	dir := t.TempDir()
	hash, created, err := Key(dir, none)
	if err != nil || !created {
		t.Fatalf("created %v, error %v", created, err)
	}
	path := filepath.Join(dir, KeyFile)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, error %v", info.Mode(), err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSuffix(string(data), "\n")
	if !regexp.MustCompile(`^[a-z2-7]{52}$`).MatchString(key) || hash != sha256.Sum256([]byte(key)) {
		t.Errorf("key file %q", data)
	}
	if list, _ := os.ReadDir(dir); len(list) != 1 {
		t.Errorf("directory holds %v", list)
	}

	// Read back on the next start.
	if again, created, err := Key(dir, none); err != nil || created || again != hash {
		t.Errorf("second start: created %v, error %v, same key %v", created, err, again == hash)
	}
	// STARGAITE_ADMIN_KEY wins, and no file is written for it.
	other := t.TempDir()
	if h, created, err := Key(other, given(testKey)); err != nil || created || h != sha256.Sum256([]byte(testKey)) {
		t.Errorf("key from the environment: created %v, error %v", created, err)
	}
	if list, _ := os.ReadDir(other); len(list) != 0 {
		t.Errorf("directory holds %v", list)
	}

	// A short key, from either source, a key with surrounding whitespace and a
	// key shaped as a client key stop serve; no error carries the key.
	for _, bad := range []string{testKey[:31], auth.Prefix + testKey, testKey + " ", "\t" + testKey} {
		if _, _, err := Key(t.TempDir(), given(bad)); err == nil || strings.Contains(err.Error(), bad) {
			t.Errorf("key %q: error %v", bad, err)
		}
	}
	short := t.TempDir()
	if err := os.WriteFile(filepath.Join(short, KeyFile), []byte("too-short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Key(short, none); err == nil || strings.Contains(err.Error(), "too-short") {
		t.Errorf("short key file: error %v", err)
	}

	// The file is whole or absent: a write that fails leaves neither the key
	// nor its temporary file behind.
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	if _, _, err := Key(ro, none); err == nil {
		t.Error("read-only directory: no error")
	}
	if list, _ := os.ReadDir(ro); len(list) != 0 {
		t.Errorf("directory holds %v", list)
	}
}

// TestEveryRouteNeedsTheKey: 401 on every route, an unknown one included,
// for anything but the admin key; a client key fails the same way (SEC-2).
// A failed authentication writes nothing and is logged, rate-limited, without
// the token presented.
func TestEveryRouteNeedsTheKey(t *testing.T) {
	f := newFixture(t)
	token, _, _, err := auth.Issue(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	targets := []string{"GET /admin/v1/nope", "DELETE /admin/v1/settings", "GET /"}
	for _, rt := range routes {
		targets = append(targets, rt.pattern)
	}
	for _, target := range targets {
		method, path, _ := strings.Cut(target, " ")
		for _, authorization := range []string{
			"", testKey, "Bearer", "Bearer ", "Bearer  " + testKey, "Bearer " + testKey + "x", "Bearer " + testKey[:len(testKey)-1],
			"Bearer " + strings.ToUpper(testKey), "Basic " + testKey, "Bearer " + token,
		} {
			errorOf(t, f.as(t, authorization, method, path, []byte("version: 1\n")), http.StatusUnauthorized, domain.CodeInvalidAPIKey)
		}
	}
	lines := strings.Split(strings.TrimSpace(f.log.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"msg":"admin authentication failed"`) || !strings.Contains(lines[0], `"failures":1`) {
		t.Errorf("log %q, want one rate-limited line", f.log)
	}
	if strings.Contains(f.log.String(), testKey[:20]) || strings.Contains(f.log.String(), token) {
		t.Errorf("the log holds a presented token: %q", f.log)
	}
	if n := f.count(t, "SELECT COUNT(*) FROM audit"); n != 0 {
		t.Errorf("%d audit rows after failed authentications", n)
	}
	// The scheme is case-insensitive (RFC 9110).
	if w := f.as(t, "bearer "+testKey, http.MethodGet, "/admin/v1/settings", nil); w.Code != http.StatusOK {
		t.Errorf("lower-case scheme: status %d", w.Code)
	}
}

// TestEnvelope: the mux's plain-text 404 and 405 come back in the envelope.
func TestEnvelope(t *testing.T) {
	f := newFixture(t)
	errorOf(t, f.do(t, http.MethodGet, "/admin/v1/nope", nil), http.StatusNotFound, domain.CodeNotFound)
	errorOf(t, f.do(t, http.MethodGet, "/admin/v1/settings/x", nil), http.StatusNotFound, domain.CodeNotFound)
	w := f.do(t, http.MethodDelete, "/admin/v1/settings", nil)
	errorOf(t, w, http.StatusMethodNotAllowed, domain.CodeInvalidRequest)
	if allow := w.Header().Get("Allow"); !strings.Contains(allow, http.MethodPatch) {
		t.Errorf("Allow %q", allow)
	}
	errorOf(t, f.do(t, http.MethodGet, "/admin/v1/apply", nil), http.StatusMethodNotAllowed, domain.CodeInvalidRequest)
	for _, status := range []string{`"status":404`, `"status":405`} {
		if !strings.Contains(f.log.String(), status) {
			t.Errorf("no request line with %s: %q", status, f.log)
		}
	}
}

// TestExportGolden: the YAML export after applying config.example.yaml
// (design section 10), and its canonical JSON form.
func TestExportGolden(t *testing.T) {
	f := newFixture(t)
	res := f.apply(t, "", readExample(t))
	if !res.Changed || res.Revision != 1 || len(res.Secrets) != 2 {
		t.Fatalf("apply: %+v", res)
	}
	got := f.export(t)
	if *update {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("export differs from %s (run the test with -update after a deliberate change):\n%s", golden, got)
	}

	w := f.do(t, http.MethodGet, "/admin/v1/export?format=json", nil)
	canonical, err := config.Encode(f.cfg.Snapshot().Doc)
	if err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || w.Body.String() != string(canonical)+"\n" {
		t.Errorf("JSON export: status %d\n%s", w.Code, w.Body)
	}
	var fromYAML, fromJSON domain.Document
	if err := config.DecodeYAML(got, &fromYAML); err != nil {
		t.Fatal(err)
	}
	if err := config.Decode(canonical, &fromJSON); err != nil {
		t.Fatal(err)
	}
	if a, b := mustEncode(t, fromYAML), mustEncode(t, fromJSON); a != b {
		t.Errorf("the two forms differ:\n%s\n%s", a, b)
	}
	for _, target := range []string{"/admin/v1/export?format=xml", "/admin/v1/export?fmt=json", "/admin/v1/export?format=json&format=yaml"} {
		errorOf(t, f.do(t, http.MethodGet, target, nil), http.StatusBadRequest, domain.CodeInvalidRequest)
	}
}

func mustEncode(t *testing.T, v any) string {
	t.Helper()
	data, err := config.Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestApplyExportFixedPoint: for several states S, apply(export(S)) on S
// changes nothing, and on an empty instance it gives a document equal to S
// with new key secrets (AC-39).
func TestApplyExportFixedPoint(t *testing.T) {
	var pruned domain.Document
	if err := config.DecodeYAML(readExample(t), &pruned); err != nil {
		t.Fatal(err)
	}
	delete(pruned.Keys, "classifier-flow")
	delete(pruned.Models, "rerank")
	delete(pruned.Targets, "tei-rerank/bge-reranker-v2-m3")
	delete(pruned.Providers, "tei-rerank")
	pruned.Settings.Telemetry = nil
	prunedYAML, err := config.EncodeYAML(&pruned)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		build func(t *testing.T, f *fixture)
	}{
		{"empty", func(*testing.T, *fixture) {}},
		{"the example", func(t *testing.T, f *fixture) { f.apply(t, "", readExample(t)) }},
		{"the example, pruned, with a revoked key and a patched setting", func(t *testing.T, f *fixture) {
			f.apply(t, "", readExample(t))
			f.apply(t, "?prune=true", prunedYAML)
			var res written
			decode(t, f.do(t, http.MethodPatch, "/admin/v1/settings", []byte(`{"timeouts":{"total_ms":1000}}`)), http.StatusOK, &res)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.build(t, f)
			exported, revision := f.export(t), f.cfg.Snapshot().Revision
			before := f.count(t, "SELECT COUNT(*) FROM audit")

			res := f.apply(t, "", exported)
			if res.Changed || len(res.Diff) != 0 || len(res.Secrets) != 0 || res.Revision != revision {
				t.Errorf("apply(export(S)) on S: %+v", res)
			}
			if again := f.export(t); !bytes.Equal(again, exported) {
				t.Errorf("export changed:\n%s", again)
			}
			if n := f.count(t, "SELECT COUNT(*) FROM audit"); n != before+1 || f.lastAudit(t) != "apply configuration  unchanged" {
				t.Errorf("%d audit rows, last %q", n-before, f.lastAudit(t))
			}

			g := newFixture(t)
			res = g.apply(t, "", exported)
			if got := g.export(t); !bytes.Equal(got, exported) {
				t.Errorf("on an empty instance:\n%s\nwant\n%s", got, exported)
			}
			if len(res.Secrets) != len(f.cfg.Snapshot().Keys) {
				t.Errorf("secrets %d, keys %d", len(res.Secrets), len(f.cfg.Snapshot().Keys))
			}
		})
	}
}

// TestApplyDryRun: a dry run shows the difference and writes nothing, an
// audit row included; it shows no secret.
func TestApplyDryRun(t *testing.T) {
	f := newFixture(t)
	empty := f.export(t)
	res := f.apply(t, "?dry_run=true", readExample(t))
	if !res.Changed || len(res.Diff) == 0 || len(res.Secrets) != 0 || res.Revision != 0 {
		t.Errorf("dry run: %+v", res)
	}
	if !slices.Contains(entries(res.Diff), "key invoice-agent created") {
		t.Errorf("diff %q", entries(res.Diff))
	}
	if n := f.count(t, "SELECT (SELECT COUNT(*) FROM audit) + (SELECT COUNT(*) FROM revisions) + (SELECT COUNT(*) FROM keys)"); n != 0 || !bytes.Equal(f.export(t), empty) {
		t.Errorf("a dry run wrote %d rows", n)
	}
}

// TestApplyPrune: without --prune what the file omits stays; with it an
// absent key is revoked, never deleted, and its name is refused afterwards.
func TestApplyPrune(t *testing.T) {
	f := newFixture(t)
	f.apply(t, "", readExample(t))
	var doc domain.Document
	if err := config.DecodeYAML(readExample(t), &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc.Keys, "classifier-flow")
	file, err := config.EncodeYAML(&doc)
	if err != nil {
		t.Fatal(err)
	}

	if res := f.apply(t, "", []byte("version: 1\n")); res.Changed {
		t.Errorf("a file that names nothing, without --prune: %q", entries(res.Diff))
	}
	res := f.apply(t, "?prune=true", file)
	if got := entries(res.Diff); !slices.Equal(got, []string{"key classifier-flow revoked"}) || res.Revision != 2 {
		t.Errorf("prune: revision %d, diff %q", res.Revision, got)
	}
	if bytes.Contains(f.export(t), []byte("classifier-flow")) {
		t.Error("export shows a revoked key")
	}
	if n := f.count(t, "SELECT COUNT(*) FROM keys WHERE name = 'classifier-flow' AND revoked_at = 1790000000000"); n != 1 {
		t.Error("the revoked key's row is not kept with its revocation time")
	}
	if n := f.count(t, "SELECT COUNT(*) FROM key_secrets WHERE key_name = 'classifier-flow'"); n != 0 {
		t.Errorf("%d secrets left on a revoked key", n)
	}

	// The revoked name in a file is 409, audited as refused; a dry run is
	// refused the same way and writes nothing.
	revision := f.cfg.Snapshot().Revision
	e := errorOf(t, f.do(t, http.MethodPost, "/admin/v1/apply", readExample(t)), http.StatusConflict, domain.CodeConflict)
	if !strings.Contains(e.Message, "classifier-flow") || f.lastAudit(t) != "apply configuration  refused:conflict" {
		t.Errorf("message %q, audit %q", e.Message, f.lastAudit(t))
	}
	rows := f.count(t, "SELECT COUNT(*) FROM audit")
	errorOf(t, f.do(t, http.MethodPost, "/admin/v1/apply?dry_run=true", readExample(t)), http.StatusConflict, domain.CodeConflict)
	if f.count(t, "SELECT COUNT(*) FROM audit") != rows || f.cfg.Snapshot().Revision != revision {
		t.Error("a refused dry run wrote a row or published")
	}

	// --prune clears absent settings and removes absent resources.
	res = f.apply(t, "?prune=true", []byte("version: 1\n"))
	if s := f.cfg.Snapshot(); len(s.Doc.Providers)+len(s.Doc.Targets)+len(s.Doc.Models)+len(s.Doc.Keys) != 0 || s.Doc.Settings != (domain.Settings{}) {
		t.Errorf("after pruning everything: %q", entries(res.Diff))
	}
	if n := f.count(t, "SELECT COUNT(*) FROM keys WHERE revoked_at IS NOT NULL"); n != 2 {
		t.Errorf("%d revoked keys, want 2", n)
	}
}

// TestApplyEmptyBlock: a settings member the file declares, even empty,
// replaces the stored one, so its fields return to their defaults (OPS-3).
func TestApplyEmptyBlock(t *testing.T) {
	f := newFixture(t)
	f.apply(t, "", []byte("version: 1\nsettings:\n  timeouts: {connect_ms: 777}\n  retention: {audit_months: 7}\n"))
	res := f.apply(t, "", []byte("version: 1\nsettings:\n  timeouts: {}\n  retention: {}\n"))
	if got := entries(res.Diff); !slices.Equal(got, []string{"setting retention updated", "setting timeouts updated"}) {
		t.Errorf("diff %q", got)
	}
	var effective domain.Settings
	decode(t, f.do(t, http.MethodGet, "/admin/v1/settings", nil), http.StatusOK, &effective)
	if !reflect.DeepEqual(effective, config.WithDefaults(domain.Settings{})) {
		t.Errorf("effective timeouts %+v, retention %+v", effective.Timeouts, effective.Retention)
	}
}

// TestApplyRefused: a file that is not a valid configuration, or a request
// that is not well formed, leaves the active configuration untouched.
func TestApplyRefused(t *testing.T) {
	f := newFixture(t)
	f.apply(t, "", readExample(t))
	before := f.export(t)
	change := []byte("version: 1\nsettings:\n  max_parallel_requests: 3\nkeys:\n  k: {models: [fast]}\n")
	for _, tc := range []struct {
		name, query string
		file        []byte
		status      int
		code        string
		finding     string // a finding the error carries
	}{
		{"an unknown target", "", []byte("version: 1\nmodels:\n  m:\n    targets: [nope/x]\n"), 400, domain.CodeInvalidRequest, "unknown_target"},
		{"version 2", "", []byte("version: 2\n"), 400, domain.CodeInvalidRequest, "unsupported_version"},
		{"no version", "", []byte("providers: {}\n"), 400, domain.CodeInvalidRequest, "unsupported_version"},
		{"not YAML", "", []byte("version: [\n"), 400, domain.CodeInvalidRequest, ""},
		{"an unknown member", "", []byte("version: 1\ncolour: blue\n"), 400, domain.CodeInvalidRequest, ""},
		{"a malformed flag", "?dry_run=maybe", readExample(t), 400, domain.CodeInvalidRequest, ""},
		{"a misspelt flag", "?dryrun=true", readExample(t), 400, domain.CodeInvalidRequest, ""},
		{"a repeated flag", "?prune=false&prune=true", readExample(t), 400, domain.CodeInvalidRequest, ""},
		{"a flag without a value", "?dry_run", change, 400, domain.CodeInvalidRequest, ""},
		{"a flag with an empty value", "?prune=", change, 400, domain.CodeInvalidRequest, ""},
		{"too large", "", bytes.Repeat([]byte("#"), config.MaxYAMLBytes+1), 413, domain.CodeRequestTooLarge, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := errorOf(t, f.do(t, http.MethodPost, "/admin/v1/apply"+tc.query, tc.file), tc.status, tc.code)
			if tc.finding != "" && !slices.ContainsFunc(e.Findings, func(x domain.Finding) bool { return x.Code == tc.finding && x.Severity == domain.SeverityError }) {
				t.Errorf("findings %+v, want %s", e.Findings, tc.finding)
			}
			if got := f.export(t); !bytes.Equal(got, before) || f.cfg.Snapshot().Revision != 1 {
				t.Errorf("the configuration changed:\n%s", got)
			}
		})
	}
	if got := f.lastAudit(t); got != "apply configuration  refused:invalid_request" {
		t.Errorf("last audit row %q", got)
	}
}

func TestSettings(t *testing.T) {
	f := newFixture(t)
	var effective domain.Settings
	decode(t, f.do(t, http.MethodGet, "/admin/v1/settings", nil), http.StatusOK, &effective)
	if !reflect.DeepEqual(effective, config.WithDefaults(domain.Settings{})) || effective.Timeouts.ConnectMS != domain.DefaultConnectMS {
		t.Errorf("effective settings of an empty instance: %+v", effective)
	}

	patch := func(body string) written {
		t.Helper()
		var res struct {
			config.Result
			Resource domain.Settings
		}
		decode(t, f.do(t, http.MethodPatch, "/admin/v1/settings", []byte(body)), http.StatusOK, &res)
		return written{res.Result, res.Resource}
	}
	res := patch(`{"timeouts":{"connect_ms":100},"max_parallel_requests":7}`)
	if got := entries(res.Diff); !slices.Equal(got, []string{"setting max_parallel_requests created", "setting timeouts created"}) {
		t.Errorf("diff %q", got)
	}
	if s := res.Resource.(domain.Settings); s.Timeouts.ConnectMS != 100 || s.Timeouts.TotalMS != domain.DefaultTotalMS || s.MaxParallelRequests != 7 {
		t.Errorf("resource %+v", s)
	}
	// A member merges; null removes; the stored declaration holds only what
	// was declared.
	patch(`{"timeouts":{"total_ms":9}}`)
	if got := f.cfg.Snapshot().Doc.Settings.Timeouts; *got != (domain.Timeouts{ConnectMS: 100, TotalMS: 9}) {
		t.Errorf("merged timeouts %+v", got)
	}
	// Nulling every field leaves the empty block RFC 7396 gives; nulling the
	// block removes it.
	res = patch(`{"timeouts":{"connect_ms":null,"total_ms":null},"budget":{"limit_usd":5,"reset_interval":"monthly"}}`)
	if got := entries(res.Diff); !slices.Equal(got, []string{"setting budget created", "setting timeouts updated"}) {
		t.Errorf("diff %q", got)
	}
	if got := f.cfg.Snapshot().Doc.Settings.Timeouts; got == nil || *got != (domain.Timeouts{}) {
		t.Errorf("emptied timeouts %+v", got)
	}
	res = patch(`{"timeouts":null}`)
	if got := entries(res.Diff); !slices.Equal(got, []string{"setting timeouts removed"}) {
		t.Errorf("diff %q", got)
	}
	patch(`{"budget":null}`)
	if res = patch(`{}`); res.Changed || f.lastAudit(t) != "settings set settings  unchanged" {
		t.Errorf("empty patch: %+v, audit %q", res, f.lastAudit(t))
	}
	if got := f.cfg.Snapshot().Doc.Settings; got != (domain.Settings{MaxParallelRequests: 7}) {
		t.Errorf("declared settings %+v", got)
	}

	revision := f.cfg.Snapshot().Revision
	for _, tc := range []struct {
		body   string
		status int
		code   string
		want   string // in the message
	}{
		{`{"timeouts":{"connect_ms":1,"connect_ms":2}}`, 400, domain.CodeInvalidRequest, `"/timeouts/connect_ms" is repeated`},
		{`{"colour":1}`, 400, domain.CodeInvalidRequest, `unknown member "/colour"`},
		{`{"max_parallel_requests":"7"}`, 400, domain.CodeInvalidRequest, "max_parallel_requests"},
		{`{"max_parallel_requests":-1}`, 400, domain.CodeInvalidRequest, "refused"},
		{`[]`, 400, domain.CodeInvalidRequest, ""},
		{`null`, 400, domain.CodeInvalidRequest, ""},
		{`{`, 400, domain.CodeInvalidRequest, ""},
		{`{"max_parallel_requests":1}` + strings.Repeat(" ", maxBody), 413, domain.CodeRequestTooLarge, ""},
	} {
		e := errorOf(t, f.do(t, http.MethodPatch, "/admin/v1/settings", []byte(tc.body)), tc.status, tc.code)
		if !strings.Contains(e.Message, tc.want) {
			t.Errorf("%.40s: message %q, want %q", tc.body, e.Message, tc.want)
		}
	}
	if f.cfg.Snapshot().Revision != revision {
		t.Error("a refused patch published")
	}
}

// TestInternalErrorIsGeneric: a 500 answers a fixed message with its request
// id; the cause goes to the log under that id (OPS-9).
func TestInternalErrorIsGeneric(t *testing.T) {
	f := newFixture(t)
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	w := f.do(t, http.MethodPatch, "/admin/v1/settings", []byte(`{"max_parallel_requests":7}`))
	e := errorOf(t, w, http.StatusInternalServerError, domain.CodeInternalError)
	id := w.Header().Get(RequestIDHeader)
	if strings.Contains(e.Message, "closed") || !strings.Contains(e.Message, id) || e.Type != "api_error" {
		t.Errorf("message %q", e.Message)
	}
	var line struct {
		Level, Msg, Error string
		RequestID         string `json:"request_id"`
	}
	for l := range strings.Lines(f.log.String()) {
		if err := json.Unmarshal([]byte(l), &line); err == nil && line.Msg == "internal error" {
			break
		}
	}
	if line.Level != "ERROR" || line.RequestID != id || !strings.Contains(line.Error, store.ErrClosed.Error()) {
		t.Errorf("log %q", f.log)
	}
}

// TestPlantedToken: a token issued by apply is in that one response and
// nowhere else: not in a later answer, the log or any row of the store.
func TestPlantedToken(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, http.MethodPost, "/admin/v1/apply", readExample(t))
	var res config.Result
	decode(t, w, http.StatusOK, &res)
	if len(res.Secrets) != 2 {
		t.Fatalf("secrets %v", res.Secrets)
	}
	var seen strings.Builder
	for _, target := range []string{"GET /admin/v1/settings", "GET /admin/v1/export", "GET /admin/v1/export?format=json", "GET /admin/v1/validate"} {
		method, path, _ := strings.Cut(target, " ")
		seen.WriteString(f.do(t, method, path, nil).Body.String())
	}
	seen.WriteString(f.do(t, http.MethodPost, "/admin/v1/apply", readExample(t)).Body.String())
	seen.WriteString(f.do(t, http.MethodPost, "/admin/v1/apply?dry_run=true&prune=true", []byte("version: 1\n")).Body.String())
	seen.WriteString(f.log.String())
	seen.WriteString(f.dump(t))
	for name, token := range res.Secrets {
		if !regexp.MustCompile(`^sk-stargaite-[a-z2-7]{12}-[a-z2-7]{52}$`).MatchString(token) {
			t.Errorf("token of %s: %q", name, token)
		}
		if strings.Contains(seen.String(), token) || strings.Contains(seen.String(), token[26:]) {
			t.Errorf("the token of %s appears outside the response that issued it", name)
		}
	}
	if strings.Contains(seen.String(), testKey) {
		t.Error("the admin key appears in an answer, the log or the store")
	}
}

// TestValidate: the findings of the published configuration, in the object
// validate -f prints.
func TestValidate(t *testing.T) {
	f := newFixture(t)
	var body struct{ Findings []domain.Finding }
	decode(t, f.do(t, http.MethodGet, "/admin/v1/validate", nil), http.StatusOK, &body)
	if body.Findings == nil || len(body.Findings) != 0 {
		t.Errorf("empty instance: %+v", body)
	}
	f.apply(t, "", readExample(t))
	decode(t, f.do(t, http.MethodGet, "/admin/v1/validate", nil), http.StatusOK, &body)
	for _, x := range body.Findings {
		if x.Severity == domain.SeverityError {
			t.Errorf("finding %+v", x)
		}
	}
}

func TestMergePatch(t *testing.T) {
	// The examples of RFC 7396, appendix A, that apply to an object.
	for _, tc := range []struct{ target, patch, want string }{
		{`{"a":"b"}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":"b"}`, `{"b":"c"}`, `{"a":"b","b":"c"}`},
		{`{"a":"b"}`, `{"a":null}`, `{}`},
		{`{"a":"b","b":"c"}`, `{"a":null}`, `{"b":"c"}`},
		{`{"a":["b"]}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":"c"}`, `{"a":["b"]}`, `{"a":["b"]}`},
		{`{"a":{"b":"c"}}`, `{"a":{"b":"d","c":null}}`, `{"a":{"b":"d"}}`},
		{`{"a":[{"b":"c"}]}`, `{"a":[1]}`, `{"a":[1]}`},
		{`{"e":null}`, `{"a":1}`, `{"a":1,"e":null}`},
		{`{}`, `{"a":{"bb":{"ccc":null}}}`, `{"a":{"bb":{}}}`},
		{`{"a":1}`, `{}`, `{"a":1}`},
	} {
		got, err := mergePatch([]byte(tc.target), []byte(tc.patch))
		if err != nil || string(got) != tc.want {
			t.Errorf("%s + %s = %s, %v; want %s", tc.target, tc.patch, got, err, tc.want)
		}
	}
}
