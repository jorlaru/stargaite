package task

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/admintest"
	"github.com/jorlaru/stargaite/internal/cli"
	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
)

// cliRunner runs the CLI in process against env, as the container would.
func cliRunner(env map[string]string) proxy.Runner {
	return func(_ context.Context, args []string, stdout, stderr io.Writer) (int, error) {
		return cli.Main(args, func(k string) string { return env[k] }, strings.NewReader(""), stdout, stderr), nil
	}
}

// reply is a canned answer: what the CLI prints and its exit code.
type reply struct {
	out  string
	code int
}

// cannedRunner answers by the joined arguments and records each call; a call
// it has no answer for fails the test.
func cannedRunner(t *testing.T, answers map[string]reply, calls *[]string) proxy.Runner {
	return func(_ context.Context, args []string, stdout, _ io.Writer) (int, error) {
		key := strings.Join(args, " ")
		*calls = append(*calls, key)
		r, ok := answers[key]
		if !ok {
			t.Errorf("unexpected call %q", key)
			return 2, nil
		}
		_, _ = io.WriteString(stdout, r.out)
		return r.code, nil
	}
}

// clone is a host whose docker compose ps prints ps, with no git on PATH,
// so the clone's version is dev.
func clone(t *testing.T, ps string) *host.Host {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\n[ \"$*\" = \"compose ps -q\" ] || exit 9\nprintf '%s' '" + ps + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return host.New(t.TempDir())
}

func run(t *testing.T, h *host.Host, runner proxy.Runner) (string, error) {
	t.Helper()
	var out bytes.Buffer
	got, err := SeeWhatExists(context.Background(), ui.New(plain.New(&out), nil), proxy.New(runner), h)
	if got != "" {
		t.Fatalf("result = %q, want the lines alone", got)
	}
	return out.String(), err
}

func hasLines(t *testing.T, out string, want ...string) {
	t.Helper()
	lines := strings.Split(out, "\n")
	for _, w := range want {
		found := false
		for _, l := range lines {
			found = found || l == w
		}
		if !found {
			t.Errorf("no line %q in:\n%s", w, out)
		}
	}
}

func TestSeeWhatExistsExample(t *testing.T) {
	env := admintest.Server(t)
	var applied, errOut bytes.Buffer
	if code := cli.Main([]string{"apply", "-f", "../../../config.example.yaml", "--json"}, func(k string) string { return env[k] },
		strings.NewReader(""), &applied, &errOut); code != 0 {
		t.Fatalf("apply: exit %d:\n%s%s", code, applied.String(), errOut.String())
	}
	out, err := run(t, clone(t, "c0ffee\n"), cliRunner(env))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.HasPrefix(out, "version: dev running, dev in this clone\n") {
		t.Errorf("the version line is not first:\n%s", out)
	}
	hasLines(t, out,
		"version: dev running, dev in this clone",
		"provider anthropic: credential resolved",
		"provider tei-embed: credential none",
		"alias deep: anthropic/claude-sonnet-5-5; budget 500 USD, resets monthly",
		"alias fast: minimax-plan/MiniMax-M3.1-Flash, then openrouter/qwen/qwen-3.6-flash",
		"key invoice-agent: may call fast, deep; budget 100 USD, resets monthly",
		"spend: capped at 2000 USD, resets monthly",
		"telemetry https://langfuse.example.com/api/public/otel: credentials resolved",
	)
	if strings.Contains(out, "in no alias") {
		t.Errorf("a target in no alias in:\n%s", out)
	}
}

func TestSeeWhatExistsEmpty(t *testing.T) {
	out, err := run(t, clone(t, "c0ffee\n"), cliRunner(admintest.Server(t)))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := "version: dev running, dev in this clone\nproviders: none\naliases: none\nkeys: none\nspend: not capped\ntelemetry: not set\n"
	if out != want {
		t.Fatalf("out:\n%s\nwant:\n%s", out, want)
	}
}

// shapes is a canned store that covers every line of See what exists.
func shapes() map[string]reply {
	return map[string]reply{
		"version --json":        {`{"version":"v1.2.3"}`, 0},
		"providers list --json": {`{"providers":[{"name":"acme","credential":"unresolved"}]}`, 0},
		"models list --json": {`{"models":[{"name":"main","targets":["acme/a"],"budget":{"limit_usd":2.5}},` +
			`{"name":"spare","targets":["acme/a","acme/b","acme/c"]}]}`, 0},
		"targets list --json": {`{"targets":[{"id":"acme/a"},{"id":"acme/b"},{"id":"acme/c"},{"id":"x/y"}]}`, 0},
		"keys list --json": {`{"keys":[{"name":"all","models":["*"]},{"name":"none","models":[]},{"name":"nil"},` +
			`{"name":"two","models":["main","spare"],"budget":{"limit_usd":0.25,"reset_interval":"daily"}}]}`, 0},
		"settings show --json":    {`{"timeouts":{},"budget":{"limit_usd":10}}`, 0},
		"telemetry status --json": {`{"telemetry":{"otlp_endpoint":"https://otel.example.com","header_env":{}},"unresolved":["A","B"]}`, 0},
	}
}

func TestSeeWhatExistsShapes(t *testing.T) {
	var calls []string
	answers := shapes()
	out, err := run(t, clone(t, "c0ffee"), cannedRunner(t, answers, &calls))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := `version: v1.2.3 running, dev in this clone
provider acme: credential unresolved
alias main: acme/a; budget 2.5 USD
alias spare: acme/a, then acme/b, then acme/c
target x/y: in no alias
key all: may call every alias
key none: may call no alias
key nil: may call no alias
key two: may call main, spare; budget 0.25 USD, resets daily
spend: capped at 10 USD
telemetry https://otel.example.com: credentials unresolved: A, B
`
	if out != want {
		t.Fatalf("out:\n%s\nwant:\n%s", out, want)
	}
	wantCalls := "version --json|providers list --json|models list --json|targets list --json|keys list --json|settings show --json|telemetry status --json"
	if got := strings.Join(calls, "|"); got != wantCalls {
		t.Fatalf("calls = %s", got)
	}
	for _, tc := range []struct{ telemetry, want string }{
		{`{"telemetry":{"otlp_endpoint":"https://otel.example.com"}}`, "telemetry https://otel.example.com: no credential"},
		{`{"telemetry":{"otlp_endpoint":"https://otel.example.com","basic_auth_env":[]}}`, "telemetry https://otel.example.com: credentials resolved"},
		{`{"telemetry":null}`, "telemetry: not set"},
	} {
		answers["telemetry status --json"] = reply{tc.telemetry, 0}
		out, err := run(t, clone(t, "c0ffee"), cannedRunner(t, answers, &calls))
		if err != nil || !strings.HasSuffix(out, "\n"+tc.want+"\n") {
			t.Fatalf("%s: %v\n%s", tc.telemetry, err, out)
		}
	}
}

func TestSeeWhatExistsNothingRuns(t *testing.T) {
	var calls []string
	out, err := run(t, clone(t, ""), cannedRunner(t, nil, &calls))
	if err != nil || out != "version: dev in this clone; no container runs (./setup.sh install starts it)\n" {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(calls) != 0 {
		t.Fatalf("the CLI was called: %q", calls)
	}
}

func TestSeeWhatExistsCleansProxyText(t *testing.T) {
	var calls []string
	answers := shapes()
	answers["models list --json"] = reply{`{"models":[{"name":"a\u001b[2Jb","targets":["acme/a"]}]}`, 0}
	answers["targets list --json"] = reply{`{"targets":[{"id":"acme/a"},{"id":"x/\u001b]52;c;aGk=\u0007"}]}`, 0}
	answers["telemetry status --json"] = reply{`{"telemetry":{"otlp_endpoint":"https://otel.example.com/\u202egpj"}}`, 0}
	out, err := run(t, clone(t, "c0ffee"), cannedRunner(t, answers, &calls))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, r := range out {
		if (r < 0x20 && r != '\n') || r == 0x202e {
			t.Fatalf("rune %U printed in:\n%q", r, out)
		}
	}
	hasLines(t, out, "alias a[2Jb: acme/a", "target x/]52;c;aGk=: in no alias", "telemetry https://otel.example.com/gpj: no credential")
}

func TestSeeWhatExistsStopsOnError(t *testing.T) {
	var calls []string
	answers := shapes()
	answers["keys list --json"] = reply{`{"error":{"message":"the admin API was not reached: refused"}}`, 1}
	out, err := run(t, clone(t, "c0ffee"), cannedRunner(t, answers, &calls))
	if err == nil || err.Error() != "stargaite keys list: the admin API was not reached: refused" {
		t.Fatalf("err = %v", err)
	}
	hasLines(t, out, "version: v1.2.3 running, dev in this clone", "provider acme: credential unresolved", "target x/y: in no alias")
	if strings.Contains(out, "key ") || strings.Contains(out, "spend") || len(calls) != 5 {
		t.Fatalf("the task went on after the error: %q\n%s", calls, out)
	}
}

func TestSeeWhatExistsDockerFails(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho sk-secret >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	var calls []string
	out, err := run(t, host.New(t.TempDir()), cannedRunner(t, nil, &calls))
	if err == nil || err.Error() != "docker compose ps failed with exit 1" || out != "" || len(calls) != 0 {
		t.Fatalf("err = %v, out %q, calls %q", err, out, calls)
	}
}

func TestMenuFresh(t *testing.T) {
	m := Menu()
	if len(m) != 1 || m[0].Group != "This machine" || m[0].Label != "See what exists" || m[0].Sub != "status" ||
		len(m[0].Steps) != 1 || m[0].Steps[0] != (ui.Step{ID: "show", Title: "what exists"}) || m[0].Run == nil {
		t.Fatalf("Menu() = %+v", m)
	}
	m[0].Label = "changed"
	m[0].Steps[0].Title = "changed"
	if again := Menu(); again[0].Label != "See what exists" || again[0].Steps[0].Title != "what exists" {
		t.Fatal("a change to Menu()'s result reached a second call")
	}
}
