package host

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakes writes each script into a new directory and makes that directory the
// whole PATH. A script calls other tools by absolute path.
func fakes(t *testing.T, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

func TestFindNeedsComposeYAML(t *testing.T) {
	clone := t.TempDir()
	exe := filepath.Join(clone, "bin", "stargaite-console")
	write(t, exe, "")
	_, err := Find(exe, os.Getuid())
	want := "compose.yaml is not in " + clone + ", the parent of bin/: run ./setup.sh from the clone of stargaite"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestFindFollowsSymlink(t *testing.T) {
	clone := t.TempDir()
	exe := filepath.Join(clone, "bin", "stargaite-console")
	write(t, exe, "")
	write(t, filepath.Join(clone, "compose.yaml"), "services: {}\n")
	t.Chdir(t.TempDir()) // Find changes to the clone; this puts the directory back
	link := filepath.Join(t.TempDir(), "console")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	h, err := Find(link, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(clone)
	if h.Dir != want || h.bound != localBound {
		t.Fatalf("Dir = %q, bound %v; want %q", h.Dir, h.bound, want)
	}
}

func TestCLIArgv(t *testing.T) {
	logs := t.TempDir()
	log := filepath.Join(logs, "argv")
	fakes(t, map[string]string{"docker": `printf '%s\n' "$*" >> ` + log + "\necho '{}'\n"})
	h := New(t.TempDir())
	var out strings.Builder
	code, err := h.CLI(context.Background(), []string{"providers", "list", "--json"}, nil, &out, nil)
	if err != nil || code != 0 || out.String() != "{}\n" {
		t.Fatalf("code %d, err %v, out %q", code, err, out.String())
	}
	write(t, filepath.Join(h.Dir, "compose.override.yaml"), "services: {}\n") // an override without TLS
	if _, err := h.CLI(context.Background(), []string{"providers", "list", "--json"}, nil, &out, nil); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(h.Dir, "compose.override.yaml"), "# setup: tls_name=localhost\n# setup: tls_ca=cert.pem\nservices: {}\n")
	if _, err := h.CLI(context.Background(), []string{"providers", "list", "--json"}, nil, &out, nil); err != nil {
		t.Fatal(err)
	}
	want := "compose exec -T stargaite /stargaite providers list --json\n" +
		"compose exec -T stargaite /stargaite providers list --json\n" +
		"compose exec -T -e STARGAITE_ADMIN_URL=https://localhost:4001 -e SSL_CERT_FILE=/tls/cert.pem stargaite /stargaite providers list --json\n"
	if got := read(t, log); got != want {
		t.Fatalf("argv:\n%s\nwant:\n%s", got, want)
	}
}

// TestCLIStdinPipe: CLI hands its standard input, the reader of an os.Pipe,
// to the command itself. A command that exits without reading it returns at
// once though the writer stays open, and one that reads it ends when the
// writer closes.
func TestCLIStdinPipe(t *testing.T) {
	logs := t.TempDir()
	log := filepath.Join(logs, "log")
	fakes(t, map[string]string{"docker": `[ "$FAKE" = read ] || exit 0
/bin/cat >/dev/null; echo eof >>` + log + "\n"})
	h := New(t.TempDir())
	pipe := func() (*os.File, *os.File) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
		return r, w
	}

	r, _ := pipe()
	start := time.Now()
	code, err := h.CLI(context.Background(), []string{"version"}, r, &strings.Builder{}, nil)
	if code != 0 || err != nil || time.Since(start) > 2*time.Second {
		t.Fatalf("a command that does not read: code %d, err %v, after %v", code, err, time.Since(start))
	}

	t.Setenv("FAKE", "read")
	r, w := pipe()
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := h.CLI(context.Background(), []string{"version"}, r, &strings.Builder{}, nil)
		done <- result{code, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("returned before the writer closed: %+v", got)
	case <-time.After(300 * time.Millisecond):
	}
	_ = w.Close()
	select {
	case got := <-done:
		if got.code != 0 || got.err != nil || read(t, log) != "eof\n" {
			t.Fatalf("after the close: %+v, log %q", got, read(t, log))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CLI did not return within 2 s of the writer's close")
	}
}

func TestCLIRefusesBadTLSName(t *testing.T) {
	logs := t.TempDir()
	log := filepath.Join(logs, "argv")
	fakes(t, map[string]string{"docker": `printf '%s\n' "$*" >> ` + log + "\n"})
	for _, tc := range []struct{ override, want string }{
		{"# setup: tls_name=a b\n", "compose.override.yaml: tls_name is not a host name"},
		{"# setup: tls_name=localhost\n# setup: tls_ca=../ca.pem\n", "compose.override.yaml: tls_ca is not a file name in tls/"},
	} {
		h := New(t.TempDir())
		write(t, filepath.Join(h.Dir, "compose.override.yaml"), tc.override)
		_, err := h.CLI(context.Background(), []string{"version", "--json"}, nil, &strings.Builder{}, nil)
		if err == nil || err.Error() != tc.want {
			t.Fatalf("err = %v, want %q", err, tc.want)
		}
	}
	if got := read(t, log); got != "" {
		t.Fatalf("docker ran: %q", got)
	}
}

func TestContainerRuns(t *testing.T) {
	fakes(t, map[string]string{"docker": `[ "$*" = "compose ps -q" ] || exit 9
case "$FAKE" in
runs) echo c0ffee ;;
none) ;;
fail) echo sk-hand-edited-secret >&2; echo sk-hand-edited-secret; exit 1 ;;
esac
`})
	h := New(t.TempDir())
	for _, tc := range []struct {
		fake string
		want bool
		err  string
	}{
		{"runs", true, ""},
		{"none", false, ""},
		{"fail", false, "docker compose ps failed with exit 1"},
	} {
		t.Setenv("FAKE", tc.fake)
		got, err := h.ContainerRuns(context.Background())
		if got != tc.want || (err == nil) != (tc.err == "") || (err != nil && err.Error() != tc.err) {
			t.Fatalf("%s: got %v, %v; want %v, %q", tc.fake, got, err, tc.want, tc.err)
		}
	}
}

func TestCloneVersion(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git in this image")
	}
	repo := t.TempDir()
	write(t, filepath.Join(repo, "f"), "x\n")
	for _, args := range [][]string{
		{"init", "-q"}, {"add", "f"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "one"},
	} {
		cmd := exec.Command(gitPath, args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	cmd := exec.Command(gitPath, "rev-parse", "--short", "HEAD")
	cmd.Dir = repo
	short, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := New(repo).CloneVersion(context.Background()), strings.TrimSpace(string(short)); got != want {
		t.Fatalf("CloneVersion = %q, want %q", got, want)
	}
	t.Setenv("PATH", t.TempDir())
	if got := New(repo).CloneVersion(context.Background()); got != "dev" {
		t.Fatalf("CloneVersion without git = %q, want dev", got)
	}
}

// gone reports whether pid is no longer a live process: it does not exist,
// or it is a zombie that nobody reaps (a container whose init does not).
func gone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}

func TestCaptureCancelSignalsGroup(t *testing.T) {
	logs := t.TempDir()
	log, pids := filepath.Join(logs, "log"), filepath.Join(logs, "pids")
	fakes(t, map[string]string{"hang": "trap 'echo INT >> " + log + "; exit 130' INT\n" +
		"/bin/sleep 60 &\necho $! > " + pids + ".tmp\n/bin/mv " + pids + ".tmp " + pids + "\nwait\n"})
	h := New(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		err error
		at  time.Time
	}
	done := make(chan result, 1)
	go func() {
		_, _, err := h.Capture(ctx, "hang")
		done <- result{err, time.Now()}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for read(t, pids) == "" {
		if time.Now().After(deadline) {
			t.Fatal("the fake never started its grandchild")
		}
		time.Sleep(10 * time.Millisecond)
	}
	grandchild, err := strconv.Atoi(strings.TrimSpace(read(t, pids)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })
	cancelled := time.Now()
	cancel()
	var r result
	select {
	case r = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Capture did not return within 30 s of the cancel")
	}
	if took := r.at.Sub(cancelled); took < 9*time.Second || took > 15*time.Second {
		t.Fatalf("Capture returned %v after the cancel, want 9 s to 15 s (killDelay)", took)
	}
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", r.err)
	}
	if got := read(t, log); got != "INT\n" {
		t.Fatalf("log = %q, want INT", got)
	}
	// SIGKILL is delivered asynchronously: the grandchild may still be on
	// its way out for a moment after Capture returns.
	for deadline := time.Now().Add(2 * time.Second); !gone(grandchild); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived the cancel", grandchild)
		}
	}
}

func TestLocalBoundCuts(t *testing.T) {
	if New("x").bound != 20*time.Second || localBound != 20*time.Second {
		t.Fatalf("bound = %v, want 20s", New("x").bound)
	}
	fakes(t, map[string]string{"docker": "exec /bin/sleep 60\n", "git": "exec /bin/sleep 60\n"})
	h := New(t.TempDir())
	h.bound = 100 * time.Millisecond
	start := time.Now()
	_, err := h.ContainerRuns(context.Background())
	if err == nil || err.Error() != "docker compose ps did not answer within 100ms" || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
	start = time.Now()
	if got := h.CloneVersion(context.Background()); got != "dev" || time.Since(start) > 2*time.Second {
		t.Fatalf("CloneVersion = %q after %v", got, time.Since(start))
	}
}

func TestCaptureLimit(t *testing.T) {
	fakes(t, map[string]string{"big": "/usr/bin/head -c 9437184 /dev/zero\n"})
	_, _, err := New(t.TempDir()).Capture(context.Background(), "big")
	if err == nil || err.Error() != "big wrote more than 8 MiB" {
		t.Fatalf("err = %v", err)
	}
}

func TestCaptureExitCode(t *testing.T) {
	fakes(t, map[string]string{"seven": "echo out; echo err >&2; exit 7\n"})
	out, code, err := New(t.TempDir()).Capture(context.Background(), "seven")
	if err != nil || code != 7 || string(out) != "out\n" {
		t.Fatalf("out %q, code %d, err %v", out, code, err)
	}
}

const rootText = "run as root in a clone that belongs to another user, the console would leave root-owned files " +
	"(.env, tls/, backups/, git objects) that break its next run and that user's git: run it as the clone's owner"

func TestRootRule(t *testing.T) {
	for _, tc := range []struct {
		uid, owner int
		refused    bool
	}{{0, 1000, true}, {0, 0, false}, {1000, 1000, false}, {1000, 0, false}} {
		err := rootRule(tc.uid, tc.owner)
		if tc.refused != (err != nil) || (err != nil && err.Error() != rootText) {
			t.Fatalf("rootRule(%d, %d) = %v", tc.uid, tc.owner, err)
		}
	}
	if err := New(t.TempDir()).RootRule(0); err == nil || err.Error() != rootText {
		t.Fatalf("root in the test's own temp dir: %v", err)
	}
	if err := New("/").RootRule(0); err != nil {
		t.Fatalf("root in a directory root owns: %v", err)
	}
	if err := New(t.TempDir()).RootRule(os.Getuid()); err != nil {
		t.Fatalf("the owner: %v", err)
	}
}

func TestFindRootRule(t *testing.T) {
	logs := t.TempDir()
	log := filepath.Join(logs, "log")
	fakes(t, map[string]string{"docker": `echo "$*" >> ` + log + "\n"})
	clone := t.TempDir()
	exe := filepath.Join(clone, "bin", "stargaite-console")
	write(t, exe, "")
	write(t, filepath.Join(clone, "compose.yaml"), "services: {}\n")
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)

	if _, err := Find(exe, 0); err == nil || err.Error() != rootText {
		t.Fatalf("Find as root: %v", err)
	}
	if wd, _ := os.Getwd(); wd != elsewhere || read(t, log) != "" {
		t.Fatalf("after the refusal: working directory %q, docker log %q", wd, read(t, log))
	}
	h, err := Find(exe, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(clone)
	if wd, _ := os.Getwd(); h.Dir != want || wd != want {
		t.Fatalf("Dir %q, working directory %q; want %q", h.Dir, wd, want)
	}
}

// display runs Display and returns the lines said.
func display(t *testing.T, h *Host, name string, args ...string) ([]string, int, error) {
	t.Helper()
	var lines []string
	code, err := h.Display(context.Background(), func(l string) { lines = append(lines, l) }, name, args...)
	return lines, code, err
}

func TestDisplayLines(t *testing.T) {
	fakes(t, map[string]string{"two": "printf 'a\\nb\\n'; echo c >&2\n"})
	lines, code, err := display(t, New(t.TempDir()), "two")
	slices.Sort(lines) // standard error may interleave
	if code != 0 || err != nil || !slices.Equal(lines, []string{"a", "b", "c"}) {
		t.Fatalf("lines %q, code %d, err %v", lines, code, err)
	}
}

func TestDisplayCutsEnvLine(t *testing.T) {
	fakes(t, map[string]string{"docker": `echo 'failed to read /x/.env: line 3: unexpected character "'"'"'" in variable name "sk-hand-edited-secret"'
exit 1
`})
	lines, code, err := display(t, New(t.TempDir()), "docker", "compose", "up", "-d")
	if code != 1 || err != nil || !slices.Equal(lines, []string{"failed to read /x/.env: line 3"}) {
		t.Fatalf("lines %q, code %d, err %v", lines, code, err)
	}
	if strings.Contains(strings.Join(lines, "\n"), "sk-") {
		t.Fatalf("a line holds the secret: %q", lines)
	}
}

func TestDisplayLongLine(t *testing.T) {
	fakes(t, map[string]string{"long": "/usr/bin/head -c 70000 /dev/zero | /usr/bin/tr '\\0' x; echo; echo next\n"})
	lines, _, err := display(t, New(t.TempDir()), "long")
	if err != nil || len(lines) != 2 || lines[0] != strings.Repeat("x", lineLimit) || lines[1] != "next" {
		t.Fatalf("err %v, %d lines, the first %d bytes", err, len(lines), len(lines[0]))
	}
}

func TestChildOpensNoTTY(t *testing.T) {
	fakes(t, map[string]string{"tty": "(: </dev/tty) 2>/dev/null && echo tty || echo no-tty\n"})
	if lines, _, _ := display(t, New(t.TempDir()), "tty"); !slices.Equal(lines, []string{"no-tty"}) {
		t.Fatalf("lines %q", lines)
	}
}

// upHost is a Host whose readiness check counts its tries and answers ok.
func upHost(t *testing.T, ok bool) (*Host, *int) {
	t.Helper()
	h := New(t.TempDir())
	tries := 0
	h.readyCheck = func(context.Context) error {
		tries++
		if ok {
			return nil
		}
		return errors.New("not ready")
	}
	return h, &tries
}

func TestUp(t *testing.T) {
	logs := t.TempDir()
	log := filepath.Join(logs, "argv")
	fakes(t, map[string]string{"docker": `echo "$*" >> ` + log + "\necho Started\n"})
	h, tries := upHost(t, true)
	var said []string
	if err := h.Up(context.Background(), func(l string) { said = append(said, l) }, "--no-build"); err != nil {
		t.Fatal(err)
	}
	if read(t, log) != "compose up -d --no-build\n" || !slices.Equal(said, []string{"Started"}) || *tries != 1 {
		t.Fatalf("argv %q, said %q, tries %d", read(t, log), said, *tries)
	}
}

func TestUpBound(t *testing.T) {
	if New("x").upBound != 20*time.Minute {
		t.Fatalf("upBound = %v, want 20m", New("x").upBound)
	}
	fakes(t, map[string]string{"docker": "exec /bin/sleep 60\n"})
	h, tries := upHost(t, true)
	h.upBound = 100 * time.Millisecond
	start := time.Now()
	err := h.Up(context.Background(), func(string) {})
	if err == nil || err.Error() != "docker compose up was not done within 20 minutes; its output is above" ||
		time.Since(start) > 15*time.Second || *tries != 0 {
		t.Fatalf("err = %v after %v, %d readiness tries", err, time.Since(start), *tries)
	}
}

func TestUpFails(t *testing.T) {
	fakes(t, map[string]string{"docker": "exit 3\n"})
	h, tries := upHost(t, true)
	err := h.Up(context.Background(), func(string) {})
	if err == nil || err.Error() != "docker compose up failed or was interrupted; its output is above" || *tries != 0 {
		t.Fatalf("err = %v, %d readiness tries", err, *tries)
	}
	t.Setenv("PATH", t.TempDir()) // no docker: a start failure
	if err := h.Up(context.Background(), func(string) {}); err == nil ||
		err.Error() != "docker compose up failed or was interrupted; its output is above" || *tries != 0 {
		t.Fatalf("no docker: err = %v, %d readiness tries", err, *tries)
	}
}

func TestUpSIGINT(t *testing.T) {
	logs := t.TempDir()
	log, started := filepath.Join(logs, "log"), filepath.Join(logs, "started")
	fakes(t, map[string]string{"docker": "trap 'echo INT >> " + log + "; exit 130' INT\n" +
		"echo > " + started + "\n/bin/sleep 60\n"})
	h, tries := upHost(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.Up(ctx, func(string) {}) }()
	for deadline := time.Now().Add(10 * time.Second); read(t, started) == ""; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the fake never started")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || read(t, log) != "INT\n" || *tries != 0 {
			t.Fatalf("err = %v, log %q, %d readiness tries", err, read(t, log), *tries)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("Up did not return within 12 s of the cancel")
	}
}

func TestWaitReadyNever(t *testing.T) {
	if r := New("x").ready; r.tries != 120 || r.pause != time.Second || r.try != 2*time.Second || r.bound != 120*time.Second {
		t.Fatalf("ready = %+v", r)
	}
	logs := t.TempDir()
	log := filepath.Join(logs, "argv")
	fakes(t, map[string]string{"docker": `echo "$*" >> ` + log + "\necho 'a log line'\n"})
	for _, tls := range []bool{false, true} {
		_ = os.Remove(log)
		h, tries := upHost(t, false)
		h.ready.tries, h.ready.pause, h.ready.bound = 3, time.Millisecond, 2*time.Second
		if tls {
			write(t, filepath.Join(h.Dir, "compose.override.yaml"), "# setup: tls_name=localhost\nservices: {}\n")
		}
		var said []string
		err := h.WaitReady(context.Background(), func(l string) { said = append(said, l) })
		want := []string{"not ready within 120 s; the last 30 log lines:", "a log line"}
		if tls {
			want = append(want, "./setup.sh remote again repairs a remote that did not finish")
		}
		if err == nil || err.Error() != "the proxy did not become ready" || *tries != 3 || !slices.Equal(said, want) ||
			read(t, log) != "compose logs --tail 30 stargaite\n" {
			t.Fatalf("tls %v: err %v, tries %d, said %q, docker %q", tls, err, *tries, said, read(t, log))
		}
	}
}

func TestWaitReadyBound(t *testing.T) {
	fakes(t, map[string]string{"docker": "exit 0\n"})
	h, tries := upHost(t, false)
	h.ready.tries, h.ready.pause, h.ready.bound = 1000, time.Millisecond, 100*time.Millisecond
	start := time.Now()
	if err := h.WaitReady(context.Background(), func(string) {}); err == nil || time.Since(start) > 2*time.Second || *tries >= 1000 {
		t.Fatalf("err %v after %v, %d tries", err, time.Since(start), *tries)
	}
}

// TestWaitReadyEndedContext: a cancelled wait returns the context's error,
// says nothing and reads no log.
func TestWaitReadyEndedContext(t *testing.T) {
	logs := t.TempDir()
	log := filepath.Join(logs, "argv")
	fakes(t, map[string]string{"docker": `echo "$*" >> ` + log + "\n"})
	h, _ := upHost(t, false)
	h.ready.pause = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var said []string
	err := h.WaitReady(ctx, func(l string) { said = append(said, l) })
	if !errors.Is(err, context.Canceled) || len(said) != 0 || read(t, log) != "" {
		t.Fatalf("err %v, said %q, docker %q", err, said, read(t, log))
	}
}

// TestWaitReadyBadPlane: a data plane .env or the override cannot name is
// said at once, with no readiness try and no log read.
func TestWaitReadyBadPlane(t *testing.T) {
	logs := t.TempDir()
	log := filepath.Join(logs, "argv")
	fakes(t, map[string]string{"docker": `echo "$*" >> ` + log + "\n"})
	h, tries := upHost(t, false)
	h.ready.tries, h.ready.pause = 3, time.Millisecond
	write(t, filepath.Join(h.Dir, ".env"), "STARGAITE_PORT=0\n")
	var said []string
	err := h.WaitReady(context.Background(), func(l string) { said = append(said, l) })
	want := "STARGAITE_PORT in .env is not a port number, 1 to 65535: write it as STARGAITE_PORT=<n>"
	if err == nil || err.Error() != want || *tries != 0 || len(said) != 0 || read(t, log) != "" {
		t.Fatalf("err %v, tries %d, said %q, docker %q", err, *tries, said, read(t, log))
	}
}
