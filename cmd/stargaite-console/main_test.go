package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// console is the binary TestMain builds, in a clone: <tmp>/clone/bin/.
var console string

func TestMain(m *testing.M) {
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
	default:
		code = m.Run()
	}
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

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
	if code != 0 || !strings.HasPrefix(out, "usage: stargaite-console [status | help] [--version]\n") {
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
	want := "usage: stargaite-console [status | help] [--version]\n" +
		"At a terminal with no subcommand: the guided console.\n" +
		"  status   what exists: providers, aliases, targets without an alias, keys, budgets, telemetry and both versions\n"
	if out != want {
		t.Fatalf("usage:\n%q\nwant:\n%q", out, want)
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
