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
	"testing"
	"time"
)

// gitWorld is a bare upstream, the clone the console works in (branch main,
// tracking origin/main) and another clone that pushes to the upstream. The
// Host runs git through a wrapper first on PATH that logs each argv with its
// two variables, then runs the real git, unless the wrapper's case catches
// the subcommand first.
type gitWorld struct {
	real, up, clone, other, log, sig string
	h                                *Host
}

// newGitWorld makes the world; catch is shell case arms run by the wrapper
// before the real git, such as `merge) ...;;`.
func newGitWorld(t *testing.T, catch string) *gitWorld {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git in this image")
	}
	root := t.TempDir()
	w := &gitWorld{real: real, up: filepath.Join(root, "up.git"), clone: filepath.Join(root, "clone"), other: filepath.Join(root, "other"),
		log: filepath.Join(root, "argv"), sig: filepath.Join(root, "sig")}
	w.git(t, root, "init", "-q", "--bare", "-b", "main", w.up)
	w.git(t, root, "init", "-q", "-b", "main", w.other)
	w.commit(t, w.other, "f", "one\n")
	w.git(t, w.other, "remote", "add", "origin", w.up)
	w.git(t, w.other, "push", "-q", "-u", "origin", "main")
	w.git(t, root, "clone", "-q", w.up, w.clone)
	bin := t.TempDir()
	wrapper := "#!/bin/sh\necho \"$* | $GIT_TERMINAL_PROMPT | $GIT_SSH_COMMAND\" >> " + w.log + "\n" +
		"case \"$1\" in\n" + catch + "\nesac\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	w.h = New(w.clone)
	return w
}

// git runs the real git in dir with a test identity and returns its output.
func (w *gitWorld) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(w.real, append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes file in dir and commits it.
func (w *gitWorld) commit(t *testing.T, dir, file, content string) {
	t.Helper()
	write(t, filepath.Join(dir, file), content)
	w.git(t, dir, "add", file)
	w.git(t, dir, "commit", "-q", "-m", file+" "+strings.TrimSpace(content))
}

// push commits file in the other clone and pushes it.
func (w *gitWorld) push(t *testing.T, file, content string) {
	t.Helper()
	w.commit(t, w.other, file, content)
	w.git(t, w.other, "push", "-q", "origin", "main")
}

// logged is the wrapper's log, one argv per line, cleared.
func (w *gitWorld) logged(t *testing.T) []string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(read(t, w.log), "\n"), "\n")
	_ = os.Remove(w.log)
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// argvs is the argv part of logged lines; each line must carry both git
// variables.
func argvs(t *testing.T, lines []string) []string {
	t.Helper()
	var out []string
	for _, l := range lines {
		argv, ok := strings.CutSuffix(l, " | 0 | ssh -o BatchMode=yes")
		if !ok {
			t.Fatalf("a git call without GIT_TERMINAL_PROMPT=0 and GIT_SSH_COMMAND: %q", l)
		}
		out = append(out, argv)
	}
	return out
}

func count(lines []string, prefix string) int {
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

// slowly is a wrapper case arm: sub sleeps 30 s, logging the SIGINT it gets.
func slowly(sub, sig string) string {
	return sub + ") trap 'echo INT >> " + sig + "; exit 130' INT; /bin/sleep 30 >/dev/null 2>&1 & wait; exit 0;;"
}

func TestFetch(t *testing.T) {
	w := newGitWorld(t, "")
	upstream, ahead, err := w.h.Fetch(context.Background())
	if upstream != "origin/main" || ahead != 0 || err != nil {
		t.Fatalf("up to date: %q, %d, %v", upstream, ahead, err)
	}
	if got := argvs(t, w.logged(t)); count(got, "fetch --quiet") != 1 || !slices.Contains(got, "fetch --quiet") {
		t.Fatalf("argv %q", got)
	}
	w.push(t, "g", "two\n")
	upstream, ahead, err = w.h.Fetch(context.Background())
	if upstream != "origin/main" || ahead != 1 || err != nil {
		t.Fatalf("one ahead: %q, %d, %v", upstream, ahead, err)
	}
	if got := argvs(t, w.logged(t)); count(got, "fetch ") != 1 {
		t.Fatalf("argv %q", got)
	}
}

func TestFetchRefusals(t *testing.T) {
	w := newGitWorld(t, "")
	head := w.git(t, w.clone, "rev-parse", "HEAD")
	for _, tc := range []struct {
		name, want string
		setup      func()
	}{
		{"not a repository", "upgrades come from a git clone of stargaite, with git: see https://git-scm.com/downloads",
			func() { w.h = New(t.TempDir()) }},
		{"detached", "HEAD is detached, so no branch says what to upgrade to: git switch <branch>, then run this again",
			func() { w.h = New(w.clone); w.git(t, w.clone, "checkout", "-q", "--detach", head) }},
		{"no upstream", "branch main has no upstream to upgrade from: git branch --set-upstream-to <remote>/<branch>",
			func() { w.git(t, w.clone, "checkout", "-q", "main"); w.git(t, w.clone, "branch", "--unset-upstream") }},
	} {
		tc.setup()
		_ = os.Remove(w.log)
		_, _, err := w.h.Fetch(context.Background())
		if err == nil || err.Error() != tc.want {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
		if got := argvs(t, w.logged(t)); count(got, "fetch") != 0 {
			t.Fatalf("%s: argv %q", tc.name, got)
		}
	}
	if os.Getuid() == 0 {
		t.Skip("the root rule's row needs a clone that root does not own")
	}
	w.h = New(w.clone)
	w.h.UID = 0
	if _, _, err := w.h.Fetch(context.Background()); err == nil || err.Error() != rootText || len(w.logged(t)) != 0 {
		t.Fatalf("root in another's clone: %v", err)
	}
}

func TestFetchBound(t *testing.T) {
	if New("x").fetchBound != 20*time.Second {
		t.Fatalf("fetchBound = %v, want 20s", New("x").fetchBound)
	}
	root := t.TempDir()
	sig := filepath.Join(root, "sig")
	w := newGitWorld(t, slowly("fetch", sig))
	w.h.fetchBound = 200 * time.Millisecond
	start := time.Now()
	_, _, err := w.h.Fetch(context.Background())
	if err == nil || err.Error() != "git fetch failed or did not finish within 20 s; nothing was changed" || time.Since(start) > 12*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
	if read(t, sig) != "INT\n" {
		t.Fatalf("the fetch got no SIGINT: %q", read(t, sig))
	}
}

func TestAheadLocal(t *testing.T) {
	w := newGitWorld(t, "")
	w.push(t, "g", "two\n")
	if upstream, ahead, ok := w.h.Ahead(context.Background()); upstream != "origin/main" || ahead != 0 || !ok {
		t.Fatalf("before a fetch: %q, %d, %v", upstream, ahead, ok)
	}
	if got := argvs(t, w.logged(t)); count(got, "fetch") != 0 || len(got) == 0 {
		t.Fatalf("argv %q", got)
	}
	if _, _, err := w.h.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ahead, ok := w.h.Ahead(context.Background()); ahead != 1 || !ok {
		t.Fatalf("after a fetch: %d, %v", ahead, ok)
	}
	w.h = New(t.TempDir())
	if _, _, ok := w.h.Ahead(context.Background()); ok {
		t.Fatal("Ahead in no repository is ok")
	}
}

func TestGitReads(t *testing.T) {
	w := newGitWorld(t, "")
	ctx := context.Background()
	write(t, filepath.Join(w.clone, "untracked"), "x\n")
	if !w.h.Clean(ctx) {
		t.Fatal("an untracked file is not clean")
	}
	write(t, filepath.Join(w.clone, "f"), "changed\n")
	if w.h.Clean(ctx) {
		t.Fatal("a tracked change is clean")
	}
	if !New(t.TempDir()).Clean(ctx) {
		t.Fatal("no repository is not clean")
	}
	w.git(t, w.clone, "checkout", "-q", "--", "f")

	for i := 1; i <= 26; i++ {
		w.push(t, "n"+strconv.Itoa(i), strconv.Itoa(i)+"\n")
	}
	if _, _, err := w.h.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(w.log)
	commits, err := w.h.NewCommits(ctx)
	if err != nil || len(commits) != 20 || !strings.HasSuffix(commits[0], " n26 26") || !strings.HasSuffix(commits[19], " n7 7") {
		t.Fatalf("NewCommits = %q, %v", commits, err)
	}
	if got := argvs(t, w.logged(t)); !slices.Equal(got, []string{"--no-pager log --oneline -20 HEAD..@{upstream}"}) {
		t.Fatalf("argv %q", got)
	}
	if changed, err := w.h.MigrationsChanged(ctx); changed || err != nil {
		t.Fatalf("no migration: %v, %v", changed, err)
	}
	w.push(t, "internal/store/migrations/9_x.sql", "select 1;\n")
	if _, _, err := w.h.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if changed, err := w.h.MigrationsChanged(ctx); !changed || err != nil {
		t.Fatalf("a migration: %v, %v", changed, err)
	}
	if ff, err := w.h.FastForwards(ctx); !ff || err != nil {
		t.Fatalf("behind only: %v, %v", ff, err)
	}
	w.commit(t, w.clone, "local", "mine\n")
	if ff, err := w.h.FastForwards(ctx); ff || err != nil {
		t.Fatalf("diverged: %v, %v", ff, err)
	}

	if got, want := w.h.Commit(ctx, "HEAD"), w.git(t, w.clone, "rev-parse", "HEAD"); got != want || len(got) != 40 {
		t.Fatalf("Commit(HEAD) = %q, want %q", got, want)
	}
	for _, rev := range []string{"nope", "--all", "-q"} {
		if got := w.h.Commit(ctx, rev); got != "" {
			t.Fatalf("Commit(%q) = %q", rev, got)
		}
	}

	t.Setenv("PATH", t.TempDir())
	if !w.h.Clean(ctx) {
		t.Fatal("no git on PATH is not clean")
	}
}

func TestMerge(t *testing.T) {
	w := newGitWorld(t, "")
	ctx := context.Background()
	w.push(t, "g", "two\n")
	if _, _, err := w.h.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	var said []string
	say := func(l string) { said = append(said, l) }
	_ = w.logged(t)
	if err := w.h.Merge(ctx, say); err != nil || len(said) != 0 {
		t.Fatalf("fast-forward: %v, said %q", err, said)
	}
	if got := argvs(t, w.logged(t)); !slices.Equal(got, []string{"merge --ff-only --quiet @{upstream}"}) {
		t.Fatalf("fast-forward ran %q", got)
	}
	if got, want := w.git(t, w.clone, "rev-parse", "HEAD"), w.git(t, w.other, "rev-parse", "HEAD"); got != want {
		t.Fatalf("HEAD %s, want %s", got, want)
	}

	w.push(t, "h", "three\n")
	if _, _, err := w.h.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(w.clone, "h"), "in the way\n")
	before := w.git(t, w.clone, "rev-parse", "HEAD")
	err := w.h.Merge(ctx, say)
	if err == nil || errors.Is(err, ErrMergeStopped) || len(said) < 2 || said[0] != "git merge's last lines:" ||
		!strings.Contains(strings.Join(said, "\n"), "untracked") {
		t.Fatalf("in the way: %v, said %q", err, said)
	}
	if got := w.git(t, w.clone, "rev-parse", "HEAD"); got != before {
		t.Fatalf("HEAD moved to %s", got)
	}

	root := t.TempDir()
	sig := filepath.Join(root, "sig")
	w = newGitWorld(t, slowly("merge", sig))
	w.h.ShortenWaits(200 * time.Millisecond)
	start := time.Now()
	err = w.h.Merge(ctx, func(string) {})
	if !errors.Is(err, ErrMergeStopped) || time.Since(start) > 12*time.Second {
		t.Fatalf("bound: %v after %v", err, time.Since(start))
	}
	if read(t, sig) != "INT\n" {
		t.Fatalf("the merge got no SIGINT: %q", read(t, sig))
	}
}

func TestShortenWaits(t *testing.T) {
	h := New("x")
	if h.upBound != 20*time.Minute || h.mergeBound != 2*time.Minute || h.ready.tries != 120 || h.ready.pause != time.Second ||
		h.ready.bound != 120*time.Second || h.UID != os.Getuid() {
		t.Fatalf("New: %+v", h)
	}
	h.ShortenWaits(50 * time.Millisecond)
	if h.upBound != 50*time.Millisecond || h.mergeBound != 50*time.Millisecond || h.ready.tries != 3 ||
		h.ready.pause != time.Millisecond || h.ready.bound != 50*time.Millisecond {
		t.Fatalf("ShortenWaits: %+v", h)
	}
}
