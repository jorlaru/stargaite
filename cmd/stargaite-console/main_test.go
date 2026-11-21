package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/task"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
)

// console is the binary TestMain builds, in a clone: <tmp>/clone/bin/; or,
// for tests/setup/e2e.sh, the one of the tree it set up.
var console string

func TestMain(m *testing.M) {
	// e2e.sh runs this test binary on its host, which has no go: the console
	// is the tree's own, built by the bootstrap, and nothing is built here.
	if tree := os.Getenv("STARGAITE_E2E_TREE"); tree != "" {
		console = filepath.Join(tree, "bin", "stargaite-console")
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "stargaite-console-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	console = filepath.Join(dir, "clone", "bin", "stargaite-console")
	code := 1
	switch out, err := exec.Command("go", "build", "-o", console, "-ldflags", "-X main.version=v-test", ".").CombinedOutput(); {
	case err != nil:
		fmt.Fprintf(os.Stderr, "the console was not built: %v\n%s", err, out)
	case os.WriteFile(filepath.Join(dir, "clone", "compose.yaml"), []byte("services: {}\n"), 0o644) != nil:
		fmt.Fprintln(os.Stderr, "compose.yaml was not written")
	// The home's readiness check reaches this port, where nothing listens,
	// never a service of the machine running the tests.
	case os.WriteFile(filepath.Join(dir, "clone", ".env"), []byte("STARGAITE_PORT="+closedPort()+"\n"), 0o600) != nil:
		fmt.Fprintln(os.Stderr, ".env was not written")
	default:
		code = m.Run()
	}
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// closedPort is a loopback port nothing listens on: 1, outside the
// ephemeral range, so no listener of a test running alongside is ever handed
// it, as one could be a port closed a moment ago, and then gets the home's
// readiness checks.
func closedPort() string { return "1" }

// replies are what the fake docker answers for the seven reads, in the
// shapes the CLI prints (RR section 1).
const replies = `case "$*" in
"compose ps -q") echo c0ffee ;;
"compose exec -T stargaite /stargaite version --json") echo '{"version":"v1.2.3"}' ;;
"compose exec -T stargaite /stargaite providers list --json") echo '{"providers":[{"name":"acme","credential":"resolved"}]}' ;;
"compose exec -T stargaite /stargaite models list --json") echo '{"models":[{"name":"main","targets":["acme/a"]}]}' ;;
"compose exec -T stargaite /stargaite targets list --json") echo '{"targets":[{"id":"acme/a"}]}' ;;
"compose exec -T stargaite /stargaite keys list --json") echo '{"keys":[]}' ;;
"compose exec -T stargaite /stargaite settings show --json") echo '{"timeouts":{}}' ;;
"compose exec -T stargaite /stargaite telemetry status --json") echo '{"telemetry":{"otlp_endpoint":""},"unresolved":[]}' ;;
*) exit 9 ;;
esac
`

// hang is a compose exec that traps SIGINT and waits on a child that does
// not hold the capture pipe.
const hang = `case "$*" in
"compose ps -q") echo c0ffee ;;
"compose exec"*) trap 'echo INT >> "$FAKE_LOG"; exit 130' INT; /bin/sleep 60 >/dev/null 2>&1 & wait ;;
*) exit 9 ;;
esac
`

// fake is a directory that is the child's whole PATH, holding a docker
// script with body, which logs its arguments to FAKE_LOG. It returns the
// child's environment and the log's path.
func fake(t *testing.T, body string) ([]string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "docker.log")
	script := "#!/bin/sh\necho \"$*\" >> \"$FAKE_LOG\"\n" + body
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"PATH=" + dir, "FAKE_LOG=" + log, "HOME=" + dir}, log
}

func readLog(t *testing.T, log string) string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

// runConsole runs bin with args and env, standard input /dev/null.
func runConsole(t *testing.T, bin string, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return out.String(), errb.String(), cmd.ProcessState.ExitCode()
}

func TestVersionFirst(t *testing.T) {
	out, errOut, code := runConsole(t, console, []string{}, "--version")
	if out != "v-test\n" || errOut != "" || code != 0 {
		t.Fatalf("stdout %q, stderr %q, exit %d", out, errOut, code)
	}
}

func TestUsage(t *testing.T) {
	env, log := fake(t, replies)
	out, _, code := runConsole(t, console, env, "help")
	if code != 0 || !strings.HasPrefix(out, "usage: stargaite-console [install | provider | alias | key | budget | telemetry | remote | status | logs | stop | start | restart | backup | restore | uninstall | upgrade | help] [flags] [--version]\n") {
		t.Fatalf("help: exit %d\n%s", code, out)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"status", "--json"}, "stargaite-console: unknown flag --json\n"},
		{[]string{"status", "x"}, "stargaite-console: unexpected argument: x\n"},
		{[]string{"nope"}, "stargaite-console: unknown subcommand \"nope\"; stargaite-console help lists them\n"},
		{nil, "stargaite-console: no subcommand; stargaite-console help lists them\n"},
	} {
		if _, errOut, code := runConsole(t, console, env, c.args...); code != 2 || errOut != c.want {
			t.Fatalf("%q: exit %d, stderr %q; want 2, %q", c.args, code, errOut, c.want)
		}
	}
	if l := readLog(t, log); l != "" {
		t.Fatalf("docker ran on a usage error: %q", l)
	}
}

func TestUsageText(t *testing.T) {
	out, _, _ := runConsole(t, console, nil, "--help")
	want := "usage: stargaite-console [install | provider | alias | key | budget | telemetry | remote | status | logs | stop | start | restart | backup | restore | uninstall | upgrade | help] [flags] [--version]\n" +
		"At a terminal with no subcommand: the guided console.\n" +
		"  install   the first run: this machine, then a provider, an alias with its fallbacks and a key, each kept when it\n" +
		"            exists, ending with one test call; at a terminal it starts at the first missing step\n" +
		"            --port <n> (4000), and the flags of provider, alias and key\n" +
		"            its check and its test call are each announced first, capped at 256 output tokens, never retried\n" +
		"  provider  add providers and their keys; each key is checked for free before it is written\n" +
		"            --provider <a,b>   presets, a provider already added, or one new name\n" +
		"            --base-url <url>   --protocol <openai|anthropic>   --api-key-env <VAR|none>\n" +
		"            --key-env <VAR> or <name=VAR,…>   names the variable that holds a key\n" +
		"            --replace   --allow-private-network   --yes\n" +
		"  alias     an alias toward a provider's model, kept when it exists; with --fallback it gets fallbacks\n" +
		"            --alias <name> (main)   --provider <name>   --model <id>   --fallback <provider/model,…>\n" +
		"            --effort <level>   --max-output-tokens <n>   --input-price <usd>   --output-price <usd>   --reverify\n" +
		"            and, when --provider is not added yet, provider's flags\n" +
		"            its new targets are checked first: each paid call is announced, capped at 256 output tokens, never retried\n" +
		"  key       a client key for one or more aliases, shown once, then one test call through the first\n" +
		"            --alias <a,b> (main)   --key-name <name> (my-app)   --rpm-limit <n> (60)\n" +
		"            the test call is announced first: up to one paid call per target of that alias, capped at 256 output tokens, never retried\n" +
		"  budget    a spending cap on the instance, an alias or a key; the prices the proxy says it needs are asked first\n" +
		"            --on <instance|alias:<name>|key:<name>>   --limit-usd <usd>   --reset <daily|monthly|never> (monthly)\n" +
		"            --input-price <usd>   --output-price <usd>   --yes replaces a cap   --reverify checks again what a price cleared\n" +
		"  telemetry traces to Langfuse or any OTLP endpoint, replacing what was set whole, then one test span (free)\n" +
		"            --backend <langfuse|otlp|off>   --endpoint <url>   --langfuse-public-env <VAR>   --langfuse-secret-env <VAR>\n" +
		"            --header-name <name>   --key-env <VAR>   --replace   --allow-private-network   --yes\n" +
		"  remote    publish the data plane beyond loopback, always with TLS: your certificate or a self-signed one; run again, it renews it\n" +
		"            --address <a,b>   --tls <own|self-signed>   --tls-cert <file>   --tls-key <file>   --tls-name <name>   --tls-ca <file>   --yes\n" +
		"  logs      the container's last 100 log lines; at a terminal it then follows them until Esc, an hour at most\n" +
		"            --follow is refused without a terminal: the console follows logs only while someone watches\n" +
		"  stop, start, restart   the container; stop and restart interrupt traffic   --yes\n" +
		"  backup    a copy of the store into backups/, mode 0600\n" +
		"  restore   a copy put back: keys revoked since come back, spend since is forgotten; the container is stopped\n" +
		"            and started again   --from <file>   --yes\n" +
		"  uninstall the container, its two images and bin/stargaite-console; the volume, .env, the override, backups/\n" +
		"            and tls/ are kept, and the commands that remove them printed   --yes\n" +
		"  upgrade   fast-forward this clone to its upstream and rebuild, keeping the previous image and taking a backup just before the new version starts, then checking it; it prints the way back\n" +
		"            --rebuild rebuilds this clone's version, without fetching   --legacy-backup <file>   --yes\n" +
		"  status    what exists: providers, aliases, targets without an alias, keys, budgets, telemetry and both versions; --check first says whether the clone's upstream has an upgrade (one git fetch)\n" +
		"For what has no flag here (reorder an alias, see what it sends, change a provider or a key, see usage and cost, check everything):\n" +
		"the guided console, or the stargaite command: docker compose exec -T stargaite /stargaite help\n"
	if out != want {
		t.Fatalf("usage:\n%q\nwant:\n%q", out, want)
	}
}

func TestParseFlags(t *testing.T) {
	e := task.Entry{Flags: []string{"provider", "base-url"}, Bools: []string{"yes"}}
	for _, c := range []struct {
		args []string
		want map[string]string
		err  string
	}{
		{[]string{"--provider", "a,b", "--yes"}, map[string]string{"provider": "a,b", "yes": "yes"}, ""},
		{[]string{"--base-url=http://x/v1?a=b"}, map[string]string{"base-url": "http://x/v1?a=b"}, ""},
		{[]string{"--provider", "a", "--provider=b"}, map[string]string{"provider": "b"}, ""},
		{nil, map[string]string{}, ""},
		{[]string{"--yes", "--provider"}, nil, "--provider needs a value"},
		{[]string{"--provider", "--yes"}, nil, "--provider needs a value"},
		{[]string{"--yes=no"}, nil, "--yes takes no value"},
		{[]string{"--nope"}, nil, "unknown flag --nope"},
		{[]string{"--nope=\x1b[2J"}, nil, "unknown flag --nope"},
		{[]string{"x"}, nil, "unexpected argument: x"},
		{[]string{"-y"}, nil, "unexpected argument: -y"},
	} {
		got, err := parseFlags(e, c.args)
		if c.err != "" {
			if err == nil || err.Error() != c.err || got != nil {
				t.Errorf("%q: got %v, %v; want %q", c.args, got, err, c.err)
			}
			continue
		}
		if err != nil || !maps.Equal(got, c.want) {
			t.Errorf("%q: got %v, %v; want %v", c.args, got, err, c.want)
		}
	}

	// A subcommand's Preset answers by ID beside its flags; a preset ID is not a flag.
	restore := task.Entry{Flags: []string{"from"}, Bools: []string{"yes"}, Preset: map[string]string{"what": "restore"}}
	got, err := parseFlags(restore, []string{"--from", "b.db", "--yes"})
	if want := map[string]string{"from": "b.db", "yes": "yes", "what": "restore"}; err != nil || !maps.Equal(got, want) {
		t.Errorf("preset: got %v, %v; want %v", got, err, want)
	}
	if got, err := parseFlags(restore, []string{"--what", "backup"}); err == nil || err.Error() != "unknown flag --what" || got != nil {
		t.Errorf("--what: got %v, %v; want unknown flag --what", got, err)
	}
}

// TestRunOneEnd: a task that ends the session (task.ErrEnd) exits 0, its
// result said, with no error.
func TestRunOneEnd(t *testing.T) {
	for _, res := range []string{"bye.", ""} {
		var out bytes.Buffer
		u := ui.New(plain.New(&out), nil)
		e := task.Entry{Label: "End", Run: func(context.Context, *ui.UI, *proxy.Proxy, *host.Host) (string, error) {
			return res, task.ErrEnd
		}}
		code, err := runOne(context.Background(), u, e, nil, nil)
		want := ""
		if res != "" {
			want = res + "\n"
		}
		if code != exitOK || err != nil || out.String() != want {
			t.Errorf("result %q: exit %d, err %v, output %q; want 0, nil, %q", res, code, err, out.String(), want)
		}
	}
}

// TestRunOneReexec: a task that restarts the console succeeds with its
// result said; one that ends the session with an error fails with it.
func TestRunOneReexec(t *testing.T) {
	var out bytes.Buffer
	e := task.Entry{Label: "Upgrade", Run: func(context.Context, *ui.UI, *proxy.Proxy, *host.Host) (string, error) {
		return "upgraded: x", task.ErrReexec
	}}
	if code, err := runOne(context.Background(), ui.New(plain.New(&out), nil), e, nil, nil); code != exitOK || err != nil || out.String() != "upgraded: x\n" {
		t.Fatalf("reexec: exit %d, err %v, output %q", code, err, out.String())
	}
	out.Reset()
	boom := task.Entry{Label: "Upgrade", Run: func(context.Context, *ui.UI, *proxy.Proxy, *host.Host) (string, error) {
		return "", ended{errors.New("boom")}
	}}
	if code, err := runOne(context.Background(), ui.New(plain.New(&out), nil), boom, nil, nil); code != exitFail || err == nil || err.Error() != "boom" {
		t.Fatalf("ended: exit %d, err %v", code, err)
	}
}

// ended is an error that ends the session with err's text, as Upgrade
// returns when the new console was not exported: one that errors.Is
// task.ErrEnd and is not it.
type ended struct{ err error }

func (e ended) Error() string        { return e.err.Error() }
func (e ended) Is(target error) bool { return target == task.ErrEnd }

// TestRestartDecision: the new console starts only after a session that
// asked for it ended cleanly, once per session.
func TestRestartDecision(t *testing.T) {
	clean := ending{restart: true, ended: true, session: exitOK}
	marked := []string{"PATH=/bin", "STARGAITE_CONSOLE_REEXEC=1"}
	for _, tc := range []struct {
		name    string
		e       ending
		environ []string
		exec    bool
		line    string
	}{
		{"clean", clean, []string{"PATH=/bin"}, true, ""},
		{"marked", clean, marked, false, "the console was restarted once in this session; ./setup.sh starts the new one"},
		{"SIGINT", ending{restart: true, ended: true, signal: syscall.SIGINT}, nil, false, ""},
		{"SIGTERM", ending{restart: true, ended: true, signal: syscall.SIGTERM}, nil, false, ""},
		{"SIGHUP", ending{restart: true, ended: true, signal: syscall.SIGHUP}, nil, false, ""},
		{"interrupted", ending{restart: true, ended: true, interrupted: true}, nil, false, ""},
		{"panicked", ending{restart: true, ended: true, panicked: true}, nil, false, ""},
		{"not ended", ending{restart: true}, nil, false, ""},
		{"session 1", ending{restart: true, ended: true, session: exitFail}, nil, false, ""},
		{"not asked", ending{ended: true}, nil, false, ""},
		{"not asked, marked", ending{ended: true}, marked, false, ""},
	} {
		if exec, line := restartDecision(tc.e, tc.environ); exec != tc.exec || line != tc.line {
			t.Errorf("%s: exec %v, line %q; want %v, %q", tc.name, exec, line, tc.exec, tc.line)
		}
	}
}

// TestRunOneQuit: Ctrl-C is exit 130, with nothing to print, even wrapped by
// a task's last words, unless a restore ran to its end after it or an
// upgrade did not finish: how it ended is then printed.
func TestRunOneQuit(t *testing.T) {
	ran := fmt.Errorf("%w; %w: %w", ui.ErrQuit, task.ErrRanOn, errors.New("x"))
	unfinished := fmt.Errorf("%w; %w: %s", ui.ErrQuit, task.ErrUnfinished, "y")
	wrapped := fmt.Errorf("%w; no alias or key was created", ui.ErrQuit)
	for _, row := range []struct{ ended, printed error }{{ui.ErrQuit, nil}, {wrapped, nil}, {ran, ran}, {unfinished, unfinished}} {
		e := task.Entry{Label: "Quit", Run: func(context.Context, *ui.UI, *proxy.Proxy, *host.Host) (string, error) { return "", row.ended }}
		code, err := runOne(context.Background(), ui.New(plain.New(&bytes.Buffer{}), nil), e, nil, nil)
		if code != exitINT || err != row.printed {
			t.Errorf("%v: exit %d, err %v; want 130, %v", row.ended, code, err, row.printed)
		}
	}
}

func TestProviderUsageErrorsRunNothing(t *testing.T) {
	env, log := fake(t, replies)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"provider", "--nope"}, "stargaite-console: unknown flag --nope\n"},
		{[]string{"provider", "--yes=no"}, "stargaite-console: --yes takes no value\n"},
		{[]string{"provider", "--provider"}, "stargaite-console: --provider needs a value\n"},
		{[]string{"provider", "--nope\x1b[2J"}, "stargaite-console: unknown flag --nope[2J\n"},
	} {
		if out, errOut, code := runConsole(t, console, env, c.args...); code != 2 || errOut != c.want || out != "" {
			t.Fatalf("%q: exit %d, stdout %q, stderr %q; want 2, %q", c.args, code, out, errOut, c.want)
		}
	}
	if l := readLog(t, log); l != "" {
		t.Fatalf("docker ran on a usage error: %q", l)
	}
}

func TestOutsideClone(t *testing.T) {
	env, log := fake(t, replies)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere", "bin", "stargaite-console")
	b, err := os.ReadFile(console)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(elsewhere), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(elsewhere, b, 0o755); err != nil {
		t.Fatal(err)
	}
	_, errOut, code := runConsole(t, elsewhere, env, "status")
	if code != 1 || !strings.HasPrefix(errOut, "stargaite-console: compose.yaml is not in ") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if l := readLog(t, log); l != "" {
		t.Fatalf("docker ran outside a clone: %q", l)
	}
}

func TestStatusPlain(t *testing.T) {
	env, _ := fake(t, replies)
	out, errOut, code := runConsole(t, console, env, "status")
	want := "version: v1.2.3 running, dev in this clone\nprovider acme: credential resolved\nalias main: acme/a\n" +
		"keys: none\nspend: not capped\ntelemetry: not set\n"
	if code != 0 || out != want || errOut != "" {
		t.Fatalf("exit %d, stderr %q, stdout:\n%s", code, errOut, out)
	}
}

func TestStatusFails(t *testing.T) {
	env, _ := fake(t, "case \"$*\" in \"compose ps -q\") echo c0ffee ;; *) echo 'sk-hand-edited-secret'; exit 1 ;; esac\n")
	out, errOut, code := runConsole(t, console, env, "status")
	if code != 1 || out != "" || errOut != "stargaite-console: stargaite version: failed with exit 1\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// signalDuringHang starts status with the hanging compose exec, sends sig
// once the exec has started, and returns the exit code, how long the exit
// took, and the log.
func signalDuringHang(t *testing.T, sig syscall.Signal) (int, time.Duration, string) {
	t.Helper()
	env, log := fake(t, hang)
	cmd := exec.Command(console, "status")
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(readLog(t, log), "compose exec"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("compose exec never ran; log %q", readLog(t, log))
		}
	}
	time.Sleep(100 * time.Millisecond) // the fake has installed its trap
	sent := time.Now()
	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the console did not exit")
	}
	return cmd.ProcessState.ExitCode(), time.Since(sent), readLog(t, log)
}

func TestStatusSIGTERMWhileDockerHangs(t *testing.T) {
	code, took, log := signalDuringHang(t, syscall.SIGTERM)
	if code != 143 || took > 5*time.Second || !strings.Contains(log, "INT\n") {
		t.Fatalf("exit %d after %s, log %q; want 143 within 5s with INT", code, took, log)
	}
}

func TestStatusSIGINT(t *testing.T) {
	code, took, log := signalDuringHang(t, syscall.SIGINT)
	if code != 130 || took > 5*time.Second || !strings.Contains(log, "INT\n") {
		t.Fatalf("exit %d after %s, log %q; want 130 within 5s with INT", code, took, log)
	}
}

func TestStatusSIGHUP(t *testing.T) {
	code, took, log := signalDuringHang(t, syscall.SIGHUP)
	if code != 129 || took > 5*time.Second || !strings.Contains(log, "INT\n") {
		t.Fatalf("exit %d after %s, log %q; want 129 within 5s with INT", code, took, log)
	}
}

// A signal while the session is stuck on a write nobody reads: the console
// exits after sessionBound with the signal's code and says why.
func TestStatusSIGTERMBound(t *testing.T) {
	var providers strings.Builder
	for i := range 4000 {
		if i > 0 {
			providers.WriteString(",")
		}
		fmt.Fprintf(&providers, `{"name":"p%04d","credential":"resolved"}`, i)
	}
	answer := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(answer, []byte(`{"providers":[`+providers.String()+`]}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(replies, `echo '{"providers":[{"name":"acme","credential":"resolved"}]}'`, "/bin/cat "+answer, 1)
	env, _ := fake(t, body)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }() // never read
	var errb bytes.Buffer
	cmd := exec.Command(console, "status")
	cmd.Env, cmd.Stdout, cmd.Stderr = env, w, &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	select {
	case <-waited:
		t.Fatalf("status ended with its output unread: exit %d, stderr %q", cmd.ProcessState.ExitCode(), errb.String())
	case <-time.After(2 * time.Second): // the pipe is full, the session blocked
	}
	sent := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waited:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-waited
		t.Fatal("the console did not exit")
	}
	took := time.Since(sent)
	if code := cmd.ProcessState.ExitCode(); code != 143 || took < 11*time.Second || took > 15*time.Second ||
		errb.String() != "stargaite-console: a step did not stop within 12s\n" {
		t.Fatalf("exit %d after %s, stderr %q; want 143 after about 12s", code, took, errb.String())
	}
}

func TestAwaitBound(t *testing.T) {
	if sessionBound != 12*time.Second {
		t.Fatalf("sessionBound = %s", sessionBound)
	}
	var errb bytes.Buffer
	start := time.Now()
	code, ok := await(make(chan int), 20*time.Millisecond, &errb)
	if ok || code != 1 || time.Since(start) > time.Second || errb.String() != "stargaite-console: a step did not stop within 20ms\n" {
		t.Fatalf("code %d, ok %v, after %s, stderr %q", code, ok, time.Since(start), errb.String())
	}
	done := make(chan int, 1)
	done <- 2
	if code, ok := await(done, time.Second, &errb); code != 2 || !ok {
		t.Fatalf("code %d, ok %v; want 2, true", code, ok)
	}
}

// order records, in order, the writes to it and the kills.
type order struct{ log []string }

func (o *order) Write(p []byte) (int, error) { o.log = append(o.log, "write"); return len(p), nil }

func TestSupervisePanicOneLine(t *testing.T) {
	var errb bytes.Buffer
	var kills atomic.Int32
	done := supervise(func() int { panic("boom\nmore") }, func() { kills.Add(1) }, &errb)
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("code %d; want 1", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no code")
	}
	if errb.String() != "stargaite-console: internal error: boom more\n" || kills.Load() != 1 {
		t.Fatalf("stderr %q, kills %d", errb.String(), kills.Load())
	}
	done = supervise(func() int { return 2 }, func() { kills.Add(1) }, &errb)
	if code := <-done; code != 2 || kills.Load() != 1 {
		t.Fatalf("code %d, kills %d", code, kills.Load())
	}
	// Kill first: in guided mode it returns once the terminal is restored, so
	// the line is not drawn into the frame.
	var o order
	<-supervise(func() int { panic("boom") }, func() { o.log = append(o.log, "kill") }, &o)
	if strings.Join(o.log, " ") != "kill write" {
		t.Fatalf("order %q; want kill, then write", o.log)
	}
}

func TestExitCode(t *testing.T) {
	for _, c := range []struct {
		name string
		e    ending
		want int
	}{
		{"session", ending{session: 2, ended: true}, 2},
		{"term first", ending{signal: syscall.SIGTERM, panicked: true, interrupted: true, session: 0, ended: true}, 143},
		{"int", ending{signal: syscall.SIGINT, session: 1, ended: true}, 130},
		{"hup", ending{signal: syscall.SIGHUP, ended: true}, 129},
		{"panic before ctrl+c", ending{panicked: true, interrupted: true, session: 130, ended: true}, 1},
		{"ctrl+c", ending{interrupted: true, session: 0, ended: true}, 130},
		{"ctrl+c before the bound", ending{interrupted: true, ended: false}, 130},
		{"bound", ending{session: 1, ended: false}, 1},
	} {
		if got := c.e.code(); got != c.want {
			t.Errorf("%s: %d; want %d", c.name, got, c.want)
		}
	}
}

// ownClone copies the console into a clone of its own, so a test that
// writes .env or the override sees no other test's files. Its .env names the
// port of a server that answers readiness and, under /v1/, models when set.
// It returns the console, the clone and the port.
func ownClone(t *testing.T, models http.Handler) (bin, dir, port string) {
	t.Helper()
	dir = t.TempDir()
	bin = filepath.Join(dir, "bin", "stargaite-console")
	b, err := os.ReadFile(console)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(bin), 0o755)
	}
	if err == nil {
		err = errors.Join(os.WriteFile(bin, b, 0o755), os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {}\n"), 0o644))
	}
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health/ready", func(http.ResponseWriter, *http.Request) {})
	if models != nil {
		mux.Handle("/v1/", models)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("STARGAITE_PORT="+u.Port()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return bin, dir, u.Port()
}

// acmeClone is a clone of its own whose model list accepts secret, and a
// fake docker that answers Add providers for acme there. It returns the
// console, the clone, the fake's environment and log, the flags that add
// acme, and the Authorization headers the model list got.
func acmeClone(t *testing.T, secret string) (bin, dir string, env []string, log string, flags []string, auth func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	bin, dir, port := ownClone(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"m1"}]}`)
	}))
	url := "http://host.docker.internal:" + port + "/v1"
	env, log = fake(t, "case \"$*\" in\n"+presetsAndList+
		`"compose exec -T stargaite /stargaite providers add --json --base-url `+url+` --api-key-env ACME_API_KEY --protocol openai --billing pay_per_use --allow-private-network -- acme") `+
		`echo '{"findings":[],"resource":{"name":"acme","credential":"unresolved","protocol":"openai","base_url":"`+url+`","api_key_env":"ACME_API_KEY","allow_private_network":true}}' ;;
"compose exec -T stargaite /stargaite providers show --json -- acme") echo '{"name":"acme","credential":"resolved"}' ;;
"compose up -d --no-build") ;;
*) exit 9 ;;
esac
`)
	flags = []string{"provider", "--provider", "acme", "--protocol", "openai", "--base-url", "http://127.0.0.1:" + port + "/v1",
		"--api-key-env", "ACME_API_KEY", "--yes"}
	return bin, dir, env, log, flags, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(got) }
}

// presetsAndList are the fake docker's answers to compose ps and the first
// two reads of Add providers.
const presetsAndList = `"compose ps -q") echo c0ffee ;;
"compose exec -T stargaite /stargaite providers presets --json") echo '{"presets":[{"name":"ollama","protocol":"openai","base_url":"http://localhost:11434/v1"}]}' ;;
"compose exec -T stargaite /stargaite providers list --json") echo '{"providers":[]}' ;;
`

// ollama answers the CLI calls of Add providers for ollama; up is the shell
// of its compose up.
func ollama(up string) string {
	return "case \"$*\" in\n" + presetsAndList +
		`"compose exec -T stargaite /stargaite providers add --json --base-url http://host.docker.internal:11434/v1 --allow-private-network -- ollama") ` +
		`echo '{"findings":[],"resource":{"name":"ollama","credential":"none","protocol":"openai","base_url":"http://host.docker.internal:11434/v1","allow_private_network":true}}' ;;
"compose exec -T stargaite /stargaite providers show --json -- ollama") echo '{"name":"ollama","credential":"none"}' ;;
"compose up -d --no-build") ` + up + ` ;;
*) exit 9 ;;
esac
`
}

// trapUp is a compose up that logs INT on SIGINT and waits on a child that
// holds no pipe.
const trapUp = `trap 'echo INT >> "$FAKE_LOG"; exit 130' INT; /bin/sleep 60 >/dev/null 2>&1 & wait`

func TestProviderPlainMissing(t *testing.T) {
	env, log := fake(t, "case \"$*\" in\n"+presetsAndList+"*) exit 9 ;;\nesac\n")
	bin, _, _ := ownClone(t, nil)
	out, errOut, code := runConsole(t, bin, env, "provider")
	if code != 2 || errOut != "stargaite-console: missing --provider: Which providers do you want to add?\n" || out != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	want := "compose ps -q\ncompose exec -T stargaite /stargaite providers presets --json\ncompose exec -T stargaite /stargaite providers list --json\n"
	if l := readLog(t, log); l != want {
		t.Fatalf("docker log %q", l)
	}
}

// Without a terminal, alias asks nothing: a missing --model exits 2 naming
// it, after reading only what it needs to name the question.
func TestAliasPlainMissing(t *testing.T) {
	env, log := fake(t, replies)
	bin, _, _ := ownClone(t, nil)
	_, errOut, code := runConsole(t, bin, env, "alias", "--provider", "acme", "--alias", "other")
	if code != 2 || errOut != "stargaite-console: missing --model: the model id at acme\n" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	want := "compose ps -q\ncompose exec -T stargaite /stargaite models list --json\ncompose exec -T stargaite /stargaite providers list --json\n"
	if l := readLog(t, log); l != want {
		t.Fatalf("docker log %q", l)
	}
}

// waitLog waits until the fake's log holds s.
func waitLog(t *testing.T, log, s string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(readLog(t, log), s); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the log never held %q: %q", s, readLog(t, log))
		}
	}
	time.Sleep(100 * time.Millisecond) // the fake has installed its trap
}

// temporaries are the temporary files of .env and the override left in dir.
func temporaries(t *testing.T, dir string) []string {
	t.Helper()
	env, _ := filepath.Glob(filepath.Join(dir, ".env.*"))
	override, _ := filepath.Glob(filepath.Join(dir, "compose.override.*"))
	return slices.DeleteFunc(append(env, override...), func(p string) bool { return filepath.Base(p) == "compose.override.yaml" })
}

func TestProviderSIGINTDuringUp(t *testing.T) {
	env, log := fake(t, ollama(trapUp))
	bin, dir, _ := ownClone(t, nil)
	cmd := exec.Command(bin, "provider", "--provider", "ollama", "--yes")
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitLog(t, log, "compose up")
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(12 * time.Second):
		_ = cmd.Process.Kill()
		<-waited
		t.Fatal("the console did not exit within 12 s")
	}
	if code := cmd.ProcessState.ExitCode(); code != 130 || !strings.Contains(readLog(t, log), "INT\n") || len(temporaries(t, dir)) > 0 {
		t.Fatalf("exit %d, log %q, left %q", code, readLog(t, log), temporaries(t, dir))
	}
}

// --key-env names the variable that holds the key, in the console's
// environment: it is checked once and written to .env.
func TestProviderKeyEnvPlain(t *testing.T) {
	const secret = `s3cr3t $HOME #x y\z`
	bin, dir, env, _, flags, auth := acmeClone(t, secret)
	out, errOut, code := runConsole(t, bin, append(env, "SRC="+secret), append(flags, "--key-env", "SRC")...)
	if code != 0 || !strings.HasSuffix(out, "Done: acme added, key checked.\n") || errOut != "" || strings.Contains(out, "s3cr3t") {
		t.Fatalf("exit %d, stderr %q, stdout:\n%s", code, errOut, out)
	}
	if dotEnv, err := os.ReadFile(filepath.Join(dir, ".env")); err != nil || !strings.Contains(string(dotEnv), "\nACME_API_KEY='"+secret+"'\n") {
		t.Fatalf(".env %q, %v", dotEnv, err)
	}
	if got := auth(); len(got) != 1 || got[0] != "Bearer "+secret {
		t.Fatalf("the model list got %q", got)
	}
}

// keySecret is the secret the fake CLI's keys create gives, in the shape
// the proxy makes.
const keySecret = "sk-stargaite-abcdefghijkl-abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst"

// keyCLI answers Give an app or agent its key for my-app toward alias main.
const keyCLI = `case "$*" in
"compose ps -q") echo c0ffee ;;
"compose exec -T stargaite /stargaite models list --json") echo '{"models":[{"name":"main","targets":["acme/a"]}]}' ;;
"compose exec -T stargaite /stargaite keys list --json") echo '{"keys":[]}' ;;
"compose exec -T stargaite /stargaite keys create --json --model main --rpm-limit 60 -- my-app") ` +
	`echo '{"findings":[],"secrets":{"my-app":"` + keySecret + `"},"resource":{"name":"my-app"}}' ;;
*) exit 9 ;;
esac
`

// firstCLI is keyCLI with the container that exists and the first run's
// reads: one provider resolved, alias main, no key.
var firstCLI = strings.Replace(keyCLI, `"compose ps -q") echo c0ffee ;;`, `"compose ps -q"|"compose ps -a -q") echo c0ffee ;;
"compose exec -T stargaite /stargaite export --json") echo '{"version":1,"providers":{"acme":{}},"models":{"main":{"targets":["acme/a"]}}}' ;;
"compose exec -T stargaite /stargaite validate --json") echo '{"findings":[]}' ;;`, 1)

// chatBody is the shim's chat completion (tests/setup/shim).
const chatBody = `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"fake reply"},"finish_reason":"stop"}]}`

// keyClone is a clone of its own whose data plane answers each chat
// completion with status and body, and a fake docker with cli, such as
// keyCLI. It returns the console, the fake's environment and log, and the
// Authorization headers the data plane got.
func keyClone(t *testing.T, status int, body, cli string) (bin string, env []string, log string, auth func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	bin, _, _ = ownClone(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("X-Stargaite-Target", "fake/target")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	env, log = fake(t, cli)
	return bin, env, log, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(got) }
}

// key without a terminal prints the new secret once, alone on its line,
// then tries it with that key: one request; a try the plane refuses exits
// 1 with its words.
func TestKeyPlain(t *testing.T) {
	bin, env, log, auth := keyClone(t, http.StatusOK, chatBody, keyCLI)
	out, errOut, code := runConsole(t, bin, env, "key")
	lines := strings.Split(out, "\n")
	if code != 0 || strings.Count(out, keySecret) != 1 || !slices.Contains(lines, keySecret) || !strings.Contains(out, "answer: fake reply") {
		t.Fatalf("exit %d, stderr %q, stdout:\n%s", code, errOut, out)
	}
	if got := auth(); len(got) != 1 || got[0] != "Bearer "+keySecret {
		t.Fatalf("the data plane got %q", got)
	}
	if strings.Contains(readLog(t, log), keySecret) {
		t.Fatal("the secret is in the docker log")
	}
	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusBadGateway, `{"error":{"message":"the provider failed","code":"provider_error"}}`, "stargaite-console: the test call answered 502: the provider failed"},
		{http.StatusInternalServerError, "", "stargaite-console: the test call answered 500"},
	} {
		bin, env, _, auth := keyClone(t, c.status, c.body, keyCLI)
		out, errOut, code := runConsole(t, bin, env, "key")
		if code != 1 || !slices.Contains(strings.Split(errOut, "\n"), c.want) || len(auth()) != 1 {
			t.Errorf("%d: exit %d, %d requests, stderr %q, stdout:\n%s", c.status, code, len(auth()), errOut, out)
		}
	}
}
