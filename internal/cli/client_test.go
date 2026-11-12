package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/admin"
	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/store"
)

const testKey = "test-admin-key-of-at-least-32-characters"

func getenv(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func runMain(vars map[string]string, args ...string) run {
	var stdout, stderr bytes.Buffer
	code := Main(args, getenv(vars), &stdout, &stderr)
	return run{code, stdout.String(), stderr.String()}
}

// adminServer is the admin listener in process, on an empty store, with
// every credential of config.example.yaml resolving and no name lookup
// leaving the test. It returns the environment that points the CLI at it.
func adminServer(t *testing.T) map[string]string {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	env := func() config.Env {
		return config.Env{
			Lookup: func(n string) (string, bool) { v, ok := annexBEnv[n]; return v, ok },
			Resolve: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			},
		}
	}
	cfg, err := config.Open(context.Background(), st, env, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(admin.Handler(cfg, sha256.Sum256([]byte(testKey)), slog.New(slog.NewJSONHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return map[string]string{"STARGAITE_ADMIN_URL": srv.URL, "STARGAITE_ADMIN_KEY": testKey}
}

// oneJSONValue reports whether out is exactly one JSON value.
func oneJSONValue(out string) bool {
	dec := json.NewDecoder(strings.NewReader(out))
	var v any
	return dec.Decode(&v) == nil && !dec.More() && strings.HasSuffix(out, "\n")
}

// TestCommandWalk walks every row of the command table against the admin
// listener, with standard input closed (AC-41): under --json each sample
// prints one JSON value with its exit code; without it the same exit code;
// and a sample that needs --yes exits 2 without it, before any call.
func TestCommandWalk(t *testing.T) {
	stdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	_ = r.Close()
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = stdin })

	example := "../../config.example.yaml"
	invalid := writeFile(t, "version: 2\n")
	samples := map[string][]struct {
		args []string
		code int
	}{
		"version":       {{nil, 0}},
		"validate":      {{nil, 0}, {[]string{"-f", example}, 0}, {[]string{"-f", invalid}, 1}},
		"settings show": {{nil, 0}},
		"settings set":  {{[]string{"--max-parallel-requests", "9", "--budget", "limit_usd=10,reset_interval=monthly"}, 0}, {[]string{"--max-parallel-requests", "-1"}, 1}},
		"export":        {{nil, 0}},
		"apply": {
			{[]string{"-f", example, "--dry-run"}, 0}, {[]string{"-f", example}, 0}, {[]string{"-f", invalid}, 1},
			{[]string{"-f", example, "--prune", "--yes"}, 0}, {[]string{"-f", example, "--prune", "--dry-run"}, 0},
		},
	}
	vars := adminServer(t)
	for _, c := range commands {
		name := strings.Join(c.words, " ")
		if len(samples[name]) == 0 {
			t.Errorf("command %q has no sample in the walk", name)
		}
		for _, s := range samples[name] {
			args := append(slices.Clone(c.words), s.args...)
			if r := runMain(vars, append(args, "--json")...); r.code != s.code || !oneJSONValue(r.stdout) {
				t.Errorf("%q --json: exit %d, want %d; stdout %q stderr %q", args, r.code, s.code, r.stdout, r.stderr)
			}
			if r := runMain(vars, args...); r.code != s.code || (r.code == 0 && r.stdout == "") {
				t.Errorf("%q: exit %d, want %d; stdout %q stderr %q", args, r.code, s.code, r.stdout, r.stderr)
			}
			if i := slices.Index(args, "--yes"); i >= 0 {
				if c.needsYes == nil {
					t.Errorf("%q: --yes on a command that never needs it", args)
				}
				without := slices.Delete(slices.Clone(args), i, i+1)
				if r := runMain(vars, append(without, "--json")...); r.code != 2 || r.stdout != "" {
					t.Errorf("%q: exit %d, want 2; stdout %q", without, r.code, r.stdout)
				}
			}
		}
	}
}

// recorded is the last request the recording server received.
type recorded struct {
	method, target, body, authorization, contentType string
}

// recorder answers every request with status and body, and records it.
func recorder(t *testing.T, status int, body string) (*recorded, map[string]string) {
	t.Helper()
	got := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = recorded{r.Method, r.URL.RequestURI(), string(b), r.Header.Get("Authorization"), r.Header.Get("Content-Type")}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return got, map[string]string{"STARGAITE_ADMIN_URL": srv.URL, "STARGAITE_ADMIN_KEY": testKey}
}

// TestRequests: each command makes its one call; the row, not the text of a
// value, fixes the JSON type; "--flag=" sends null (design section 6).
func TestRequests(t *testing.T) {
	got, vars := recorder(t, http.StatusOK, "{}\n")
	file := writeFile(t, "version: 1\n")
	for _, tc := range []struct {
		args                 []string
		method, target, body string
	}{
		{[]string{"settings", "show"}, "GET", "/admin/v1/settings", ""},
		{[]string{"settings", "set"}, "PATCH", "/admin/v1/settings", `{}`},
		{
			[]string{"settings", "set", "--connect-ms", "100", "--max-body-bytes=", "--budget", "limit_usd=1.5,reset_interval=monthly", "--budget", "on=true,n=01,s=1e"},
			"PATCH", "/admin/v1/settings",
			`{"budget":{"limit_usd":1.5,"n":"01","on":true,"reset_interval":"monthly","s":"1e"},"max_body_bytes":null,"timeouts":{"connect_ms":100}}`,
		},
		{[]string{"settings", "set", "--budget="}, "PATCH", "/admin/v1/settings", `{"budget":null}`},
		{[]string{"export"}, "GET", "/admin/v1/export", ""},
		{[]string{"export", "--json"}, "GET", "/admin/v1/export?format=json", ""},
		{[]string{"apply", "-f", file}, "POST", "/admin/v1/apply?dry_run=false&prune=false", "version: 1\n"},
		{[]string{"apply", "--prune", "-f", file, "--dry-run"}, "POST", "/admin/v1/apply?dry_run=true&prune=true", "version: 1\n"},
		{[]string{"apply", "--prune", "-f", file, "--yes"}, "POST", "/admin/v1/apply?dry_run=false&prune=true", "version: 1\n"},
		{[]string{"validate"}, "GET", "/admin/v1/validate", ""},
	} {
		*got = recorded{}
		if r := runMain(vars, tc.args...); r.code != 0 {
			t.Errorf("%q: exit %d, stderr %q", tc.args, r.code, r.stderr)
		}
		contentType := ""
		switch {
		case tc.method == http.MethodPost:
			contentType = "application/yaml"
		case tc.body != "":
			contentType = "application/json"
		}
		if want := (recorded{tc.method, tc.target, tc.body, "Bearer " + testKey, contentType}); *got != want {
			t.Errorf("%q:\n%+v\nwant\n%+v", tc.args, *got, want)
		}
	}
}

// TestUsage: what is decided before the call exits 2, writes only to stderr
// and sends nothing.
func TestUsage(t *testing.T) {
	got, vars := recorder(t, http.StatusOK, "{}\n")
	file := writeFile(t, "version: 1\n")
	for _, args := range [][]string{
		{"settings", "set", "--connect-ms", "x"},
		{"settings", "set", "--connect-ms", "1", "--connect-ms", "y"},
		{"settings", "set", "--max-parallel-requests", "true"},
		{"settings", "set", "--budget", "limit_usd"},
		{"settings", "set", "--budget", "=5"},
		{"settings", "set", "--budget", "limit_usd=1,"},
		{"settings", "set", "--timeouts", "1"},
		{"settings", "show", "extra"},
		{"apply"},
		{"apply", "-f", ""},
		{"apply", "-f", file, "--prune"},
		{"apply", "-f", file, "--prune", "--json"},
		{"export", "--admin-url", "ftp://127.0.0.1"},
		{"export", "--admin-url", "127.0.0.1:4001"},
		{"validate", "--env-file", file},
		{"validate", "-f", ""},
	} {
		*got = recorded{}
		r := runMain(vars, args...)
		if r.code != 2 || r.stdout != "" || r.stderr == "" || *got != (recorded{}) {
			t.Errorf("%q: exit %d, stdout %q, stderr %q, request %+v", args, r.code, r.stdout, r.stderr, *got)
		}
	}
}

// TestFailures: a failure exits 1; under --json it is one JSON object on
// stdout, the server's own body or the CLI's envelope.
func TestFailures(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := "http://" + closed.Addr().String()
	_ = closed.Close()
	_, vars := recorder(t, http.StatusOK, "{}\n")
	refusal := `{"error":{"message":"the change is refused","type":"invalid_request_error","code":"invalid_request","findings":[{"severity":"error","rule":"OPS-5","code":"unknown_target","resource":"models/m","field":"targets","message":"target \"x/y\" does not exist"}]}}` + "\n"
	_, refusing := recorder(t, http.StatusBadRequest, refusal)
	_, invalid := recorder(t, http.StatusOK, `{"findings":[{"severity":"error","rule":"OPS-3","code":"unsupported_version","resource":"document","message":"version must be 1"}]}`+"\n")
	_, redirecting := recorder(t, http.StatusTemporaryRedirect, "{}\n")

	for _, tc := range []struct {
		name   string
		vars   map[string]string
		args   []string
		stdout string // with --json, when not empty
		code   string // the code of the envelope with --json
	}{
		{"unreachable", map[string]string{"STARGAITE_ADMIN_KEY": testKey}, []string{"settings", "show", "--admin-url", refused}, "", codeUnreachable},
		{"no admin key", nil, []string{"settings", "show", "--admin-url", refused, "--data-dir", t.TempDir()}, "", codeUnreachable},
		{"unreadable file", vars, []string{"apply", "-f", t.TempDir()}, "", "invalid_request"},
		{"refused", refusing, []string{"apply", "-f", writeFile(t, "version: 1\n")}, refusal, ""},
		{"error findings in a success", invalid, []string{"validate"}, "", ""},
		{"a redirect is not followed", redirecting, []string{"settings", "show"}, "{}\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runMain(tc.vars, append(tc.args, "--json")...)
			if r.code != 1 || !oneJSONValue(r.stdout) || (tc.stdout != "" && r.stdout != tc.stdout) {
				t.Fatalf("--json: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
			}
			var env struct{ Error struct{ Code, Type string } }
			if tc.code != "" && (json.Unmarshal([]byte(r.stdout), &env) != nil || env.Error.Code != tc.code || env.Error.Type == "") {
				t.Errorf("envelope %q, want code %s", r.stdout, tc.code)
			}
			if r := runMain(tc.vars, tc.args...); r.code != 1 || (tc.stdout == refusal && (r.stdout != "" || !strings.Contains(r.stderr, `models/m targets: target "x/y" does not exist`))) {
				t.Errorf("without --json: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
			}
		})
	}
}

// TestAdminKeyFile: without STARGAITE_ADMIN_KEY the key is read from the data
// directory: --data-dir, else STARGAITE_DATA_DIR.
func TestAdminKeyFile(t *testing.T) {
	got, vars := recorder(t, http.StatusOK, "{}\n")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "admin.key"), []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	url := vars["STARGAITE_ADMIN_URL"]
	for _, tc := range []struct {
		dataDir string // STARGAITE_DATA_DIR
		args    []string
	}{
		{dir, nil},
		{t.TempDir(), []string{"--data-dir", dir}}, // the flag wins
	} {
		*got = recorded{}
		r := runMain(map[string]string{"STARGAITE_ADMIN_URL": url, "STARGAITE_DATA_DIR": tc.dataDir}, append([]string{"settings", "show"}, tc.args...)...)
		if r.code != 0 || got.authorization != "Bearer "+testKey {
			t.Errorf("exit %d, stderr %q, Authorization %q", r.code, r.stderr, got.authorization)
		}
	}
}

// TestRender: without --json an object is "name: value" lines and a list of
// objects a table; text from the server never reaches the terminal raw.
func TestRender(t *testing.T) {
	_, vars := recorder(t, http.StatusOK, `{"revision":2,"changed":true,"diff":[{"kind":"key","name":"a","action":"created","new":{"models":[]}},{"kind":"setting","name":"timeouts","action":"removed"}],`+
		`"findings":[],"secrets":{"a":"sk-x"},"resource":{"timeouts":{"connect_ms":100},"tags":["x","y"],"note":"red\u001b[31m"}}`+"\n")
	r := runMain(vars, "settings", "show")
	want := `changed: true
diff:
  action   kind     name      new
  created  key      a         {"models":[]}
  removed  setting  timeouts  -
findings: -
resource.note: "red\x1b[31m"
resource.tags: x, y
resource.timeouts.connect_ms: 100
revision: 2
secrets.a: sk-x
`
	if r.code != 0 || r.stdout != want {
		t.Errorf("exit %d, stderr %q, stdout\n%s\nwant\n%s", r.code, r.stderr, r.stdout, want)
	}
}
