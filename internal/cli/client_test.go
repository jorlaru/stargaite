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
	"regexp"
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
// and a sample that needs --yes exits 2 without it, before any call. Each
// sample runs twice on one server, so an "@" in an argument is "1" in the
// --json run and "2" in the other: both runs create, read and remove a
// resource of their own.
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
	bodies := t.TempDir()
	for _, n := range []string{"1", "2"} {
		body := `{"model":"m` + n + `","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`
		if err := os.WriteFile(filepath.Join(bodies, "body"+n+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	samples := map[string][]struct {
		args []string
		code int
	}{
		"version":           {{nil, 0}},
		"validate":          {{nil, 0}, {[]string{"-f", example}, 0}, {[]string{"-f", invalid}, 1}},
		"settings show":     {{nil, 0}},
		"settings set":      {{[]string{"--max-parallel-requests", "9", "--budget", "limit_usd=10,reset_interval=monthly"}, 0}, {[]string{"--max-parallel-requests", "-1"}, 1}},
		"providers presets": {{nil, 0}},
		"providers add": {
			{[]string{"walk@", "--preset", "openrouter", "--billing", "subscription", "--quota", "5h:requests=10", "--target-defaults", "dialect=adaptive_effort"}, 0},
			{[]string{"bare@", "--protocol", "openai", "--base-url", "http://127.0.0.1:8000/v1", "--allow-private-network", "--billing", "none"}, 0},
			{[]string{"walk@"}, 1}, {[]string{"other@", "--preset", "nope"}, 1}, {[]string{"dashscope"}, 1},
		},
		"providers list":   {{nil, 0}},
		"providers show":   {{[]string{"walk@"}, 0}, {[]string{"nope"}, 1}},
		"providers set":    {{[]string{"walk@", "--stream-usage", "none", "--cost-report="}, 0}, {[]string{"walk@", "--billing", "monthly"}, 1}},
		"providers remove": {{[]string{"bare@", "--yes"}, 0}, {[]string{"nope", "--yes"}, 1}},
		"targets add": {
			{[]string{"walk@/up/model-1.5", "--kind", "chat", "--capabilities", "tools=true", "--max-output-tokens", "1000", "--level", "low", "--level", "high", "--can-disable"}, 0},
			{[]string{"walk@/up/model-1.5", "--kind", "chat"}, 1}, {[]string{"nope/x", "--kind", "chat"}, 1},
		},
		"targets list":   {{nil, 0}},
		"targets show":   {{[]string{"walk@/up/model-1.5"}, 0}, {[]string{"walk@/nope"}, 1}},
		"targets set":    {{[]string{"walk@/up/model-1.5", "--reasoning-on", `{"thinking":{"type":"enabled"}}`, "--level="}, 0}, {[]string{"walk@/up/model-1.5", "--kind", "image"}, 1}},
		"targets remove": {{[]string{"walk@/up/model-1.5", "--yes"}, 0}, {[]string{"walk@/nope", "--yes"}, 1}},
		"models add": {
			{[]string{"m@", "--target", "walk@/created", "--effort", "low", "--effort-mode", "forced", "--max-output-tokens", "100"}, 0},
			{[]string{"m@", "--target", "walk@/created"}, 1},
			{[]string{"x", "--target", "walk@/a", "--target", "walk@/b", "--target", "walk@/c", "--target", "walk@/d"}, 1},
		},
		"models list": {{nil, 0}},
		"models show": {{[]string{"m@"}, 0}, {[]string{"nope"}, 1}},
		"models set":  {{[]string{"m@", "--param", "enable_search=drop", "--trace-sample-rate", "0"}, 0}, {[]string{"m@", "--target", "nope/x"}, 1}},
		"models explain": {
			{[]string{"m@"}, 0}, {[]string{"m@", "--effort", "low", "--max-output-tokens", "50", "--body", filepath.Join(bodies, "body@.json")}, 0},
			{[]string{"m@", "--endpoint", "messages"}, 0}, {[]string{"m@", "--effort", "bogus"}, 1}, {[]string{"m@", "--max-output-tokens", "101"}, 1},
			{[]string{"m@", "--body", filepath.Join(bodies, "none.json")}, 1}, {[]string{"m@", "--endpoint", "rerank"}, 1}, {[]string{"nope"}, 1},
		},
		"models remove": {{[]string{"m@", "--yes"}, 0}, {[]string{"nope", "--yes"}, 1}},
		"keys create": {
			{[]string{"k@", "--model", "fast", "--model", "deep", "--budget", "limit_usd=10,reset_interval=monthly", "--rpm-limit", "5", "--trace-sample-rate", "0.5"}, 0},
			{[]string{"all@", "--all-models"}, 0},
			{[]string{"k@", "--model", "fast"}, 1}, {[]string{"none"}, 1}, {[]string{"x", "--all-models", "--model", "fast"}, 1},
		},
		"keys list": {{nil, 0}},
		"keys show": {{[]string{"k@"}, 0}, {[]string{"nope"}, 1}},
		"keys set": {
			{[]string{"k@", "--all-models", "--budget=", "--max-parallel-requests", "2"}, 0}, {[]string{"k@", "--model="}, 0},
			{[]string{"nope", "--rpm-limit", "1"}, 1},
		},
		"keys rotate": {{[]string{"k@"}, 0}, {[]string{"k@", "--grace", "5m"}, 0}, {[]string{"k@", "--grace", "soon"}, 1}, {[]string{"nope"}, 1}},
		"keys revoke": {{[]string{"all@", "--yes"}, 0}, {[]string{"all@", "--yes"}, 1}, {[]string{"nope", "--yes"}, 1}},
		"export":      {{nil, 0}},
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
			run := func(n string) []string {
				out := slices.Clone(args)
				for i := range out {
					out[i] = strings.ReplaceAll(out[i], "@", n)
				}
				return out
			}
			if r := runMain(vars, append(run("1"), "--json")...); r.code != s.code || !oneJSONValue(r.stdout) {
				t.Errorf("%q --json: exit %d, want %d; stdout %q stderr %q", args, r.code, s.code, r.stdout, r.stderr)
			}
			if r := runMain(vars, run("2")...); r.code != s.code || (r.code == 0 && r.stdout == "") {
				t.Errorf("%q: exit %d, want %d; stdout %q stderr %q", args, r.code, s.code, r.stdout, r.stderr)
			}
			args = run("1")
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
	body := writeFile(t, "{\"model\": \"fast\",\n \"x\":\"\\u001b\"}\n")
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
		{[]string{"providers", "presets"}, "GET", "/admin/v1/presets", ""},
		{[]string{"providers", "list"}, "GET", "/admin/v1/providers", ""},
		{[]string{"providers", "add", "openrouter"}, "POST", "/admin/v1/providers", `{"name":"openrouter"}`},
		{
			[]string{"providers", "add", "minimax-plan", "--preset", "minimax", "--protocol", "anthropic", "--base-url", "https://api.minimax.io/anthropic",
				"--api-key-env", "MINIMAX_PLAN_API_KEY", "--target-defaults", "dialect=adaptive_effort,n=1", "--billing", "subscription",
				"--quota", "5h:requests=1500", "--quota", "7d:tokens=1e6"},
			"POST", "/admin/v1/providers",
			`{"api_key_env":"MINIMAX_PLAN_API_KEY","base_url":"https://api.minimax.io/anthropic","billing":"subscription","name":"minimax-plan","preset":"minimax",` +
				`"protocol":"anthropic","quota":[{"requests":1500,"window":"5h"},{"tokens":1e6,"window":"7d"}],"target_defaults":{"dialect":"adaptive_effort","n":"1"}}`,
		},
		{
			[]string{"providers", "add", "local", "--allow-private-network", "--cost-report", "path=usage.cost,usd_per_unit=1"},
			"POST", "/admin/v1/providers", `{"allow_private_network":true,"cost_report":{"path":"usage.cost","usd_per_unit":1},"name":"local"}`,
		},
		{[]string{"providers", "show", "a.b"}, "GET", "/admin/v1/providers/a.b", ""},
		{
			[]string{"providers", "set", "local", "--allow-private-network=false", "--quota=", "--rerank-url=", "--gen-ai-provider", "x", "--stream-usage", "native"},
			"PATCH", "/admin/v1/providers/local", `{"allow_private_network":false,"gen_ai_provider":"x","quota":null,"rerank_url":null,"stream_usage":"native"}`,
		},
		{[]string{"providers", "set", "local", "--quota=", "--quota", "1d:requests=5"}, "PATCH", "/admin/v1/providers/local", `{"quota":[{"requests":5,"window":"1d"}]}`},
		{[]string{"providers", "set", "local", "--allow-private-network="}, "PATCH", "/admin/v1/providers/local", `{"allow_private_network":null}`},
		{[]string{"providers", "remove", "local", "--yes"}, "DELETE", "/admin/v1/providers/local", ""},
		{[]string{"targets", "list"}, "GET", "/admin/v1/targets", ""},
		{
			[]string{"targets", "add", "openrouter/qwen/qwen-3.6-flash", "--kind", "chat", "--capabilities", "tools=true,vision=false", "--level", "low", "--level", "high",
				"--can-disable=false", "--thinking-budget", "low=1024", "--effort-map", "max=high", "--reasoning-on", ` {"thinking": {"type":"enabled"}} `, "--reasoning-off", "{}",
				"--pricing", "input_per_million=0.15,as_of=2026-11-01", "--unsupported-param", "a", "--unsupported-param", "b", "--response-model", "4",
				"--inline-think-tags", "--max-output-tokens", "4", "--output-cap-param=", "--dialect", "thinking_budget"},
			"POST", "/admin/v1/targets",
			`{"capabilities":{"tools":true,"vision":false},"id":"openrouter/qwen/qwen-3.6-flash","inline_think_tags":true,"kind":"chat","max_output_tokens":4,` +
				`"output_cap_param":null,"pricing":{"as_of":"2026-11-01","input_per_million":0.15},"reasoning":{"budgets":{"low":1024},"can_disable":false,` +
				`"dialect":"thinking_budget","levels":["low","high"],"map":{"max":"high"},"off":{},"on":{"thinking":{"type":"enabled"}}},"response_models":["4"],"unsupported_params":["a","b"]}`,
		},
		{[]string{"targets", "show", "openrouter/qwen/qwen-3.6-flash"}, "GET", "/admin/v1/targets/openrouter%2Fqwen%2Fqwen-3.6-flash", ""},
		{[]string{"targets", "set", "p/a b%c/./d", "--level="}, "PATCH", "/admin/v1/targets/p%2Fa%20b%25c%2F.%2Fd", `{"reasoning":{"levels":null}}`},
		{[]string{"targets", "set", "p/m", "--level", "x", "--level=", "--level", "y", "--reasoning-on="}, "PATCH", "/admin/v1/targets/p%2Fm", `{"reasoning":{"levels":["y"],"on":null}}`},
		{[]string{"targets", "remove", "p/m", "--yes"}, "DELETE", "/admin/v1/targets/p%2Fm", ""},
		{[]string{"models", "list"}, "GET", "/admin/v1/models", ""},
		{
			[]string{"models", "add", "fast", "--target", "a/b", "--target", "c/d", "--require", "tools", "--effort", "medium", "--effort-mode", "default",
				"--max-output-tokens", "32000", "--param", "enable_search=drop", "--budget", "limit_usd=500,reset_interval=monthly", "--trace-sample-rate", "0",
				"--rpm-limit", "5", "--tpm-limit", "6", "--max-parallel-requests", "7"},
			"POST", "/admin/v1/models",
			`{"budget":{"limit_usd":500,"reset_interval":"monthly"},"max_output_tokens":32000,"max_parallel_requests":7,"name":"fast","params":{"enable_search":"drop"},` +
				`"reasoning":{"effort":"medium","mode":"default"},"requires":["tools"],"rpm_limit":5,"targets":["a/b","c/d"],"tpm_limit":6,"trace_sample_rate":0}`,
		},
		{[]string{"models", "show", "fast"}, "GET", "/admin/v1/models/fast", ""},
		{[]string{"models", "set", "fast", "--target", "4", "--effort="}, "PATCH", "/admin/v1/models/fast", `{"reasoning":{"effort":null},"targets":["4"]}`},
		{[]string{"models", "remove", "fast", "--yes"}, "DELETE", "/admin/v1/models/fast", ""},
		{[]string{"models", "explain", "fast"}, "POST", "/admin/v1/models/fast/explain", `{}`},
		{
			[]string{"models", "explain", "a/b", "--endpoint", "messages", "--effort", "4", "--max-output-tokens", "100", "--body", body},
			"POST", "/admin/v1/models/a%2Fb/explain", `{"body":"{\"model\": \"fast\",\n \"x\":\"\\u001b\"}\n","effort":"4","endpoint":"messages","max_output_tokens":100}`,
		},
		{[]string{"models", "explain", "fast", "--body=", "--effort="}, "POST", "/admin/v1/models/fast/explain", `{"body":null,"effort":null}`},
		{[]string{"keys", "list"}, "GET", "/admin/v1/keys", ""},
		{
			[]string{"keys", "create", "my-agent", "--model", "fast", "--model", "4", "--budget", "limit_usd=100,reset_interval=monthly", "--rpm-limit", "60",
				"--tpm-limit", "7", "--max-parallel-requests", "5", "--quota-share", "minimax-plan=0.4", "--trace-sample-rate", "0.1"},
			"POST", "/admin/v1/keys",
			`{"budget":{"limit_usd":100,"reset_interval":"monthly"},"max_parallel_requests":5,"models":["fast","4"],"name":"my-agent","quota_share":{"minimax-plan":0.4},` +
				`"rpm_limit":60,"tpm_limit":7,"trace_sample_rate":0.1}`,
		},
		{[]string{"keys", "create", "all", "--all-models"}, "POST", "/admin/v1/keys", `{"models":["*"],"name":"all"}`},
		{[]string{"keys", "create", "none", "--model="}, "POST", "/admin/v1/keys", `{"models":[],"name":"none"}`},
		{[]string{"keys", "set", "k", "--model="}, "PATCH", "/admin/v1/keys/k", `{"models":[]}`},
		{[]string{"keys", "set", "k", "--model=", "--model", "fast"}, "PATCH", "/admin/v1/keys/k", `{"models":["fast"]}`},
		{[]string{"keys", "show", "a.b"}, "GET", "/admin/v1/keys/a.b", ""},
		{[]string{"keys", "set", "a.b", "--all-models", "--quota-share", "p=", "--rpm-limit="}, "PATCH", "/admin/v1/keys/a.b", `{"models":["*"],"quota_share":{"p":null},"rpm_limit":null}`},
		{[]string{"keys", "set", "k", "--model", "fast", "--all-models"}, "PATCH", "/admin/v1/keys/k", `{"models":["fast","*"]}`},
		{[]string{"keys", "rotate", "k"}, "POST", "/admin/v1/keys/k/rotate", `{}`},
		{[]string{"keys", "rotate", "k", "--grace", "30m"}, "POST", "/admin/v1/keys/k/rotate", `{"grace":"30m"}`},
		{[]string{"keys", "revoke", "a/b", "--yes"}, "POST", "/admin/v1/keys/a%2Fb/revoke", `{}`},
		// An empty pair value clears that entry; "--flag=" after pairs clears the block.
		{[]string{"targets", "set", "p/m", "--capabilities", "tools=,vision=true", "--pricing", "as_of=", "--effort-map", "max="}, "PATCH", "/admin/v1/targets/p%2Fm",
			`{"capabilities":{"tools":null,"vision":true},"pricing":{"as_of":null},"reasoning":{"map":{"max":null}}}`},
		{[]string{"providers", "set", "p", "--target-defaults", "dialect=", "--cost-report", "path=a,usd_per_unit=1", "--cost-report="}, "PATCH", "/admin/v1/providers/p",
			`{"cost_report":null,"target_defaults":{"dialect":null}}`},
	} {
		*got = recorded{}
		if r := runMain(vars, tc.args...); r.code != 0 {
			t.Errorf("%q: exit %d, stderr %q", tc.args, r.code, r.stderr)
		}
		contentType := ""
		switch {
		case tc.method == http.MethodPost && tc.args[0] == "apply":
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
		{"providers", "add"},
		{"providers", "add", "a", "b"},
		{"providers", "add", "a", "--quota", "5h"},
		{"providers", "add", "a", "--quota", "5h:requests"},
		{"providers", "add", "a", "--quota", "5h:requests=x"},
		{"providers", "add", "a", "--quota", ":requests=1"},
		{"providers", "add", "a", "--quota", "5h:=1"},
		{"providers", "add", "a", "--allow-private-network=maybe"},
		{"providers", "add", "a", "--target-defaults", "dialect"},
		{"providers", "add", "a", "--protocol"},
		{"providers", "set", "a", "--preset", "openai"},
		{"providers", "show"},
		{"providers", "remove", "a"},
		{"providers", "remove", "a", "--json"},
		{"targets", "add"},
		{"targets", "add", "p/m", "--reasoning-on", "{"},
		{"targets", "add", "p/m", "--reasoning-off", "{} {}"},
		{"targets", "add", "p/m", "--can-disable=maybe"},
		{"targets", "add", "p/m", "--max-output-tokens", "4k"},
		{"targets", "add", "p/m", "--thinking-budget", "low"},
		{"targets", "set", "p/m", "--effort", "low"},
		{"targets", "remove", "p/m"},
		{"models", "add", "m", "--max-output-tokens", "x"},
		{"models", "add", "m", "--param", "x"},
		{"models", "add", "m", "--level", "low"},
		{"models", "show", "m", "n"},
		{"models", "explain"},
		{"models", "explain", "m", "--max-output-tokens", "x"},
		{"models", "explain", "m", "--body"},
		{"models", "remove", "m"},
		{"keys", "create"},
		{"keys", "create", "k", "--all-models=false"},
		{"keys", "create", "k", "--all-models=yes"},
		{"keys", "set", "k", "--quota-share", "p"},
		{"keys", "set", "k", "--content", "24h"},
		{"keys", "rotate"},
		{"keys", "rotate", "k", "--model", "fast"},
		{"keys", "revoke", "k"},
		{"keys", "revoke", "k", "--json"},
		// A merge patch cannot clear a block and set one of its entries.
		{"targets", "set", "p/m", "--capabilities=", "--capabilities", "tools=true"},
		{"keys", "set", "k", "--budget=", "--budget", "limit_usd=1"},
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
		{"unreadable body file", vars, []string{"models", "explain", "m", "--body", t.TempDir()}, "", "invalid_request"},
		{"body file not UTF-8", vars, []string{"models", "explain", "m", "--body", writeFile(t, "{\"model\":\"m\xff\"}")}, "", "invalid_request"},
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

// TestClientIgnoresProxyVariables: the admin key never goes to a proxy
// (design section 13, point 23). Checked on the field, as in transport:
// http.ProxyFromEnvironment reads the variables once per process.
func TestClientIgnoresProxyVariables(t *testing.T) {
	if tr, ok := client.Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Error("the admin client has a proxy function")
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
secrets.a (shown once, never again):
sk-x
`
	if r.code != 0 || r.stdout != want {
		t.Errorf("exit %d, stderr %q, stdout\n%s\nwant\n%s", r.code, r.stderr, r.stdout, want)
	}
}

// TestExplainRender: models explain prints the layout of kick-off section
// 3.2, one block per target; text from the server never reaches the
// terminal raw.
func TestExplainRender(t *testing.T) {
	_, vars := recorder(t, http.StatusOK, `{"targets":[{"target":"anth/m","skipped":{"code":"endpoint_not_supported"}},`+
		`{"target":"mock/up-1","effort":{"requested":"none","alias":"medium","sent":"low","source":"client","adjusted":true},`+
		`"max_output":{"requested":null,"alias":32000,"sent":32000},"changes":"reasoning_effort:translated,max_tokens:added",`+
		`"url":"http://127.0.0.1:4010/chat/completions","header":{"Content-Type":["application/json"]},"body":"{\"model\":\"up-1\",\n\"max_tokens\":32000}"},`+
		`{"target":"mock/up-2","skipped":{"code":"capability_mismatch","param":"tools"}},`+
		`{"target":"mock/up-3","effort":{"requested":null,"alias":null,"sent":null,"source":"provider_default","adjusted":false},`+
		`"max_output":{"requested":null,"alias":null,"sent":null},"changes":"","url":"u","header":{},"body":"{}"}]}`+"\n")
	r := runMain(vars, "models", "explain", "fast")
	want := `target      anth/m
skipped     endpoint_not_supported

target      mock/up-1
effort      requested=none · alias=medium · sent=low · source=client · adjusted
max output  requested=- · alias=32000 · sent=32000
changes     reasoning_effort:translated,max_tokens:added
url         http://127.0.0.1:4010/chat/completions
header      Content-Type: application/json
body        "{\"model\":\"up-1\",\n\"max_tokens\":32000}"

target      mock/up-2
skipped     capability_mismatch tools

target      mock/up-3
effort      requested=- · alias=- · sent=- · source=provider_default
max output  requested=- · alias=- · sent=-
changes     -
url         u
body        {}
`
	if r.code != 0 || r.stdout != want {
		t.Errorf("exit %d, stderr %q, stdout\n%s\nwant\n%s", r.code, r.stderr, r.stdout, want)
	}
}

// TestPairClears: an empty pair value sends null for that entry, so a
// capability or a target default goes back to undeclared, and "--model="
// empties a key's models, which stops the key (design section 6).
func TestPairClears(t *testing.T) {
	vars := adminServer(t)
	for _, args := range [][]string{
		{"providers", "add", "openrouter"},
		{"targets", "add", "openrouter/m", "--kind", "chat", "--capabilities", "tools=true,vision=false", "--effort-map", "max=high,low=minimal"},
		{"targets", "set", "openrouter/m", "--capabilities", "tools=", "--effort-map", "max="},
		{"providers", "set", "openrouter", "--target-defaults", "dialect="},
		{"keys", "create", "k", "--all-models"},
		{"keys", "set", "k", "--model="},
	} {
		if r := runMain(vars, args...); r.code != 0 {
			t.Fatalf("%q: exit %d, stderr %s", args, r.code, r.stderr)
		}
	}
	doc := exported(t, vars)
	if got := string(doc["targets"]); !strings.Contains(got, `"capabilities":{"vision":false}`) || !strings.Contains(got, `"map":{"low":"minimal"}`) {
		t.Errorf("targets %s", got)
	}
	if got := string(doc["providers"]); !strings.Contains(got, `"target_defaults":{"output_cap_param":"max_tokens"}`) {
		t.Errorf("providers %s", got)
	}
	if got := string(doc["keys"]); !strings.Contains(got, `"models":[]`) {
		t.Errorf("keys %s", got)
	}
}

// TestAnnexBByCommand: the providers, targets, aliases and keys of annex B
// built one command at a time equal those of the applied file, secrets aside,
// which export never shows (AC-39 over annex B).
// Two targets are created by models add and completed by targets set.
func TestAnnexBByCommand(t *testing.T) {
	built := adminServer(t)
	levels := []string{"--level", "low", "--level", "medium", "--level", "high", "--level", "xhigh", "--level", "max"}
	for _, args := range [][]string{
		{"providers", "add", "openrouter"},
		{"providers", "add", "anthropic"},
		{"providers", "add", "minimax-plan", "--preset", "minimax", "--protocol", "anthropic", "--base-url", "https://api.minimax.io/anthropic",
			"--api-key-env", "MINIMAX_PLAN_API_KEY", "--target-defaults", "dialect=adaptive_effort,output_cap_param=", "--billing", "subscription",
			"--quota", "5h:requests=1500", "--quota", "7d:requests=15000"},
		{"providers", "add", "tei-embed", "--protocol", "openai", "--base-url", "http://localhost:8080/v1", "--billing", "none", "--allow-private-network"},
		{"providers", "add", "tei-rerank", "--protocol", "openai", "--base-url", "http://localhost:8081/v1", "--rerank-url", "http://localhost:8081/rerank",
			"--billing", "none", "--allow-private-network"},
		append([]string{"targets", "add", "minimax-plan/MiniMax-M3.1-Flash", "--kind", "chat", "--capabilities", "tools=true,structured_output=true",
			"--max-output-tokens", "64000", "--can-disable=false"}, levels...),
		{"targets", "add", "openrouter/qwen/qwen-3.6-flash", "--kind", "chat", "--capabilities", "tools=true,structured_output=true,strict_schema=true",
			"--max-output-tokens", "32000", "--output-cap-param=", "--level", "minimal", "--level", "low", "--level", "medium", "--level", "high", "--can-disable",
			"--pricing", "input_per_million=0.15,output_per_million=0.60,cache_read_per_million=0.05,as_of=2026-11-01"},
		append([]string{"targets", "add", "anthropic/claude-sonnet-5-5", "--kind", "chat", "--capabilities", "tools=true,structured_output=true,strict_schema=true,vision=true",
			"--max-output-tokens", "64000", "--can-disable=false",
			"--pricing", "input_per_million=3.00,output_per_million=15.00,cache_read_per_million=0.30,cache_write_per_million=3.75,as_of=2026-11-01"}, levels...),
		{"models", "add", "fast", "--target", "minimax-plan/MiniMax-M3.1-Flash", "--target", "openrouter/qwen/qwen-3.6-flash", "--require", "tools",
			"--require", "structured_output", "--effort", "medium", "--effort-mode", "default", "--max-output-tokens", "32000", "--param", "enable_search=drop"},
		{"models", "add", "deep", "--target", "anthropic/claude-sonnet-5-5", "--require", "tools", "--require", "structured_output", "--require", "strict_schema",
			"--effort", "high", "--effort-mode", "forced", "--max-output-tokens", "32000", "--budget", "limit_usd=500,reset_interval=monthly"},
		{"models", "add", "embed", "--target", "tei-embed/bge-m3", "--trace-sample-rate", "0"},
		{"targets", "set", "tei-embed/bge-m3", "--kind", "embeddings"},
		{"models", "add", "rerank", "--target", "tei-rerank/bge-reranker-v2-m3", "--trace-sample-rate", "0"},
		{"targets", "set", "tei-rerank/bge-reranker-v2-m3", "--kind", "rerank"},
		{"keys", "create", "classifier-flow", "--model", "fast", "--model", "embed", "--model", "rerank", "--budget", "limit_usd=30,reset_interval=monthly",
			"--max-parallel-requests", "10", "--trace-sample-rate", "0.1"},
	} {
		r := runMain(built, append(args, "--json")...)
		if r.code != 0 {
			t.Fatalf("%q: exit %d, stdout %s", args, r.code, r.stdout)
		}
		if args[1] == "add" && args[0] == "models" && strings.HasPrefix(args[4], "tei-") && !strings.Contains(r.stdout, `{"kind":"target","name":"`+args[4]+`","action":"created"`) {
			t.Errorf("%q: the created target is not in the diff: %s", args, r.stdout)
		}
	}
	// Without --json, keys create prints the token on a line of its own.
	r := runMain(built, "keys", "create", "invoice-agent", "--model", "fast", "--model", "deep", "--budget", "limit_usd=100,reset_interval=monthly",
		"--rpm-limit", "60", "--max-parallel-requests", "5", "--quota-share", "minimax-plan=0.4")
	if r.code != 0 || !regexp.MustCompile(`(?m)^secrets\.invoice-agent \(shown once, never again\):\nsk-stargaite-[a-z2-7]{12}-[a-z2-7]{52}$`).MatchString(r.stdout) {
		t.Errorf("keys create: exit %d, stdout %s", r.code, r.stdout)
	}
	applied := adminServer(t)
	if r := runMain(applied, "apply", "-f", "../../config.example.yaml"); r.code != 0 {
		t.Fatalf("apply: exit %d, stderr %s", r.code, r.stderr)
	}
	got, want := exported(t, built), exported(t, applied)
	for _, kind := range []string{"providers", "targets", "models", "keys"} {
		if g, w := got[kind], want[kind]; !bytes.Equal(g, w) || len(w) < 100 {
			t.Errorf("%s built by command:\n%s\napplied:\n%s", kind, g, w)
		}
	}
}

// exported is the canonical export of the instance at vars, by member.
func exported(t *testing.T, vars map[string]string) map[string]json.RawMessage {
	t.Helper()
	r := runMain(vars, "export", "--json")
	var doc map[string]json.RawMessage
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &doc) != nil {
		t.Fatalf("export: exit %d, stdout %s", r.code, r.stdout)
	}
	return doc
}
