package host

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
	_, err := Find(exe)
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
	link := filepath.Join(t.TempDir(), "console")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	h, err := Find(link)
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
	code, err := h.CLI(context.Background(), []string{"providers", "list", "--json"}, &out, nil)
	if err != nil || code != 0 || out.String() != "{}\n" {
		t.Fatalf("code %d, err %v, out %q", code, err, out.String())
	}
	write(t, filepath.Join(h.Dir, "compose.override.yaml"), "services: {}\n") // an override without TLS
	if _, err := h.CLI(context.Background(), []string{"providers", "list", "--json"}, &out, nil); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(h.Dir, "compose.override.yaml"), "# setup: tls_name=localhost\n# setup: tls_ca=cert.pem\nservices: {}\n")
	if _, err := h.CLI(context.Background(), []string{"providers", "list", "--json"}, &out, nil); err != nil {
		t.Fatal(err)
	}
	want := "compose exec -T stargaite /stargaite providers list --json\n" +
		"compose exec -T stargaite /stargaite providers list --json\n" +
		"compose exec -T -e STARGAITE_ADMIN_URL=https://localhost:4001 -e SSL_CERT_FILE=/tls/cert.pem stargaite /stargaite providers list --json\n"
	if got := read(t, log); got != want {
		t.Fatalf("argv:\n%s\nwant:\n%s", got, want)
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
		_, err := h.CLI(context.Background(), []string{"version", "--json"}, &strings.Builder{}, nil)
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
