package task

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
)

// upWorld is Upgrade's fixture: a clone (real git, through a wrapper that
// logs each call) of a bare upstream, tagged v0.6.0-fake, with commits
// pushed to the upstream from another clone; a docker fake that keeps the
// running version in a file (compose build stores STARGAITE_VERSION,
// compose up makes it the running one); a canned CLI that answers version,
// --help, validate and backup -; and a data plane answering readiness.
// docker, git and the CLI log to one file, in order. The fakes' hooks are
// variables: PS_DO (compose ps), BUILD_DO, UP_DO, EXPORT_DO, LOGS_DO (the
// last 100 log lines after the first), MERGE_SLEEP.
type upWorld struct {
	t                               *testing.T
	h                               *host.Host
	bin, log, real, up, other, base string                                                  // base: the commit tagged v0.6.0-fake
	help                            string                                                  // the CLI's --help
	export                          string                                                  // the CLI's export, for the home
	refuseAfter                     bool                                                    // validate refuses once another version runs
	oddValidate                     bool                                                    // validate answers what this console cannot decode
	backup                          func(ctx context.Context, stdout, stderr io.Writer) int // backup -
	mu                              sync.Mutex
}

const (
	withRestore = "usage: stargaite <command>\n  backup file\n  restore file\n"
	storeCopy   = "SQLite format 3\x00 the store"
)

func upgradeWorld(t *testing.T, ahead int) *upWorld {
	t.Helper()
	dir, gitDir := gitClone(t, 0)
	w := &upWorld{t: t, h: host.New(dir), bin: t.TempDir(), real: filepath.Join(gitDir, "git"), up: filepath.Join(filepath.Dir(dir), "up.git"),
		other: filepath.Join(filepath.Dir(dir), "other"), help: withRestore,
		export: `{"version":1}`}
	w.log = filepath.Join(w.bin, "log")
	w.git(dir, "tag", "v0.6.0-fake")
	w.base = w.git(dir, "rev-parse", "HEAD")
	w.push(ahead)
	writeFile(t, filepath.Join(w.bin, "git"), "#!/bin/sh\necho \"git $*\" >>'"+w.log+"'\n"+
		"[ \"$1\" = merge ] && [ -n \"$MERGE_SLEEP\" ] && /bin/sleep \"$MERGE_SLEEP\" >/dev/null 2>&1\nexec '"+w.real+"' \"$@\"\n", 0o755)
	writeFile(t, filepath.Join(w.bin, "docker"), "#!/bin/sh\necho \"docker $*\" >>'"+w.log+"'\ncase \"$*\" in\n"+
		"'compose ps -q'|'compose ps -a -q'|'compose ps -q stargaite') eval \"${PS_DO:-echo c0ffee}\" ;;\n"+
		"'inspect --format {{.Image}} {{.Config.Image}} c0ffee') echo 'sha256:0123abcd stargaite:local' ;;\n"+
		"'tag sha256:0123abcd stargaite:local-previous') ;;\n"+
		"'compose build') echo \"STARGAITE_VERSION=$STARGAITE_VERSION\" >>'"+w.log+"'; eval \"${BUILD_DO:-:}\" || exit 1\n"+
		"  echo \"$STARGAITE_VERSION\" >'"+w.bin+"/built' ;;\n"+
		"'compose up -d --no-build') eval \"${UP_DO:-:}\" || exit 1; /bin/cp '"+w.bin+"/built' '"+w.bin+"/running' ;;\n"+
		"'compose logs --no-log-prefix --tail 100 stargaite') echo 'a log line'; if [ -n \"$LOGS_DO\" ]; then eval \"$LOGS_DO\"; else "+
		"echo \"{\\\"level\\\":\\\"INFO\\\",\\\"msg\\\":\\\"ready\\\",\\\"version\\\":\\\"$(/bin/cat '"+w.bin+"/running')\\\"}\"; fi ;;\n"+
		"'compose logs --tail 30 stargaite') echo 'a log line' ;;\n"+
		"'build --quiet --build-arg VERSION --target console --output type=local,dest=bin .') echo \"VERSION=$VERSION\" >>'"+w.log+"'; "+
		"eval \"${EXPORT_DO:-:}\" ;;\n*) exit 9 ;;\nesac\n", 0o755)
	writeFile(t, filepath.Join(w.bin, "running"), "v0.6.0-fake\n", 0o644)
	t.Setenv("PATH", w.bin+":"+gitDir)
	for _, v := range []string{"PS_DO", "BUILD_DO", "UP_DO", "EXPORT_DO", "LOGS_DO", "MERGE_SLEEP"} {
		t.Setenv(v, "")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	writeFile(t, filepath.Join(dir, ".env"), "STARGAITE_PORT="+u.Port()+"\n", 0o600)
	w.backup = goodBackup
	// Review Focus 4: no git call of any row rewrites the clone's history
	// or work tree on its own.
	t.Cleanup(func() {
		for _, l := range w.entries() {
			if f := strings.Fields(l); len(f) > 1 && f[0] == "git" && slices.Contains([]string{"reset", "checkout", "clean", "pull", "stash"}, f[1]) {
				t.Errorf("the console ran %q", l)
			}
		}
	})
	return w
}

// goodBackup is the CLI's backup -: the copy on standard output, its
// summary on standard error.
func goodBackup(_ context.Context, stdout, stderr io.Writer) int {
	_, _ = io.WriteString(stdout, storeCopy)
	_, _ = io.WriteString(stderr, `{"bytes":26,"path":"-"}`+"\n")
	return 0
}

// shortBackup is a backup - whose copy is shorter than its summary says.
func shortBackup(_ context.Context, stdout, stderr io.Writer) int {
	_, _ = io.WriteString(stdout, "SQLite")
	_, _ = io.WriteString(stderr, `{"bytes":26,"path":"-"}`+"\n")
	return 0
}

// git runs the real git in dir, unlogged: the test's own setup.
func (w *upWorld) git(dir string, args ...string) string {
	w.t.Helper()
	cmd := exec.Command(w.real, append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// push pushes n commits to the upstream from the other clone, each adding
// a file; files names them, the first n, when given.
func (w *upWorld) push(n int, files ...string) {
	w.t.Helper()
	for i := range n {
		f := "n" + time.Now().Format("150405.000000000")
		if i < len(files) {
			f = files[i]
		}
		writeFile(w.t, filepath.Join(w.other, f), f+"\n", 0o644)
		w.git(w.other, "add", f)
		w.git(w.other, "commit", "-q", "-m", f)
	}
	if n > 0 {
		w.git(w.other, "push", "-q", "origin", "main")
	}
}

func (w *upWorld) logLine(l string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f, err := os.OpenFile(w.log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		w.t.Error(err)
		return
	}
	_, _ = io.WriteString(f, l+"\n")
	_ = f.Close()
}

// proxy is the canned CLI, each call logged as "cli <args>".
func (w *upWorld) proxy() *proxy.Proxy {
	return proxy.New(func(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) (int, error) {
		key := strings.Join(args, " ")
		w.logLine("cli " + key)
		running := strings.TrimSpace(readFile(filepath.Join(w.bin, "running")))
		switch key {
		case "version --json":
			_, _ = io.WriteString(stdout, `{"version":"`+running+`"}`)
		case "--help":
			_, _ = io.WriteString(stdout, w.help)
		case "export --json":
			_, _ = io.WriteString(stdout, w.export)
		case "validate --json":
			if w.oddValidate {
				_, _ = io.WriteString(stdout, `{"problems":[]}`)
				return 0, nil
			}
			if w.refuseAfter && running != "v0.6.0-fake" {
				_, _ = io.WriteString(stdout, `{"findings":[{"severity":"error","code":"x","resource":"model/main","message":"no"}]}`)
				return 1, nil
			}
			_, _ = io.WriteString(stdout, `{"findings":[]}`)
		case "backup - --json":
			return w.backup(ctx, stdout, stderr), nil
		default:
			w.t.Errorf("unexpected CLI call %q", key)
			return 2, nil
		}
		return 0, nil
	})
}

func (w *upWorld) entries() []string {
	s := strings.TrimSpace(readFile(w.log))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// changing is the log's entries that read the proxy or change something:
// the CLI's, the tag, the merge, the build, the start and the export.
func (w *upWorld) changing() []string {
	var out []string
	for _, l := range w.entries() {
		for _, p := range []string{"cli ", "docker tag ", "git merge ", "docker compose build", "docker compose up ", "docker build "} {
			if strings.HasPrefix(l, p) {
				out = append(out, l)
			}
		}
	}
	return out
}

// changed reports whether the log holds a tag, a merge, a build, a backup or
// a start.
func (w *upWorld) changed() bool {
	return slices.ContainsFunc(w.changing(), func(l string) bool {
		return !strings.HasPrefix(l, "cli ") || l == "cli backup - --json"
	})
}

func (w *upWorld) head() string { return w.git(w.h.Dir, "rev-parse", "HEAD") }

// headFile is what .git/HEAD and the branch it names hold, read as files,
// so a clone whose .git is moved away reads as none.
func (w *upWorld) headFile() string {
	head := readFile(filepath.Join(w.h.Dir, ".git", "HEAD"))
	return head + readFile(filepath.Join(w.h.Dir, ".git", strings.TrimSpace(strings.TrimPrefix(head, "ref:"))))
}

func (w *upWorld) run(u *ui.UI) (string, error) {
	return u.Run(context.Background(), upgradeSteps(), func(ctx context.Context) (string, error) {
		return Upgrade(ctx, u, w.proxy(), w.h)
	})
}

// plain runs Upgrade with answers and the plain front.
func (w *upWorld) plain(answers map[string]string) (string, string, error) {
	var out bytes.Buffer
	res, err := w.run(ui.New(plain.New(&out), answers))
	return res, out.String(), err
}

// copies is the files in backups/.
func (w *upWorld) copies() []string {
	entries, _ := os.ReadDir(filepath.Join(w.h.Dir, "backups"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func (w *upWorld) upstreamHead() string { return w.git(w.other, "rev-parse", "HEAD") }

// newVersion is the clone's describe once at the upstream's head.
func (w *upWorld) newVersion() string {
	return "v0.6.0-fake-" + w.git(w.other, "rev-list", "--count", w.base+"..HEAD") + "-g" +
		w.git(w.other, "rev-parse", "--short", "HEAD")
}

const wayBackHead = "the way back to v0.6.0-fake, from this directory, losing what changed since the backup; " +
	"with the new image, as the previous one may predate restore:"

// TestUpgradeUpToDate is U1.
func TestUpgradeUpToDate(t *testing.T) {
	w := upgradeWorld(t, 0)
	res, out, err := w.plain(map[string]string{"yes": "yes"})
	if err != nil || res != "up to date with origin/main; nothing was changed" || w.changed() {
		t.Fatalf("res %q, err %v, log %q\n%s", res, err, w.changing(), out)
	}
}

// TestUpgrade is U2: the clone fast-forwarded, the previous image kept, the
// image built, a backup taken, the new version started and checked, and the
// way back said once, before the start.
func TestUpgrade(t *testing.T) {
	w := upgradeWorld(t, 1)
	old, nv := w.head(), w.newVersion()
	res, out, err := w.plain(map[string]string{"yes": "yes"})
	if err != nil || res != "upgraded: version "+nv+" is ready" || w.head() != w.upstreamHead() {
		t.Fatalf("res %q, err %v\n%s", res, err, out)
	}
	want := []string{"cli version --json", "cli --help", "cli validate --json", "docker tag sha256:0123abcd stargaite:local-previous",
		"git merge --ff-only --quiet @{upstream}", "docker compose build", "cli backup - --json", "docker compose up -d --no-build",
		"cli version --json", "cli validate --json"}
	if got := w.changing(); !slices.Equal(got, want) || strings.Count(readFile(w.log), "git fetch --quiet\n") != 1 ||
		!strings.Contains(readFile(w.log), "STARGAITE_VERSION="+nv+"\n") {
		t.Fatalf("log %q\n%s", got, readFile(w.log))
	}
	copies := w.copies()
	if len(copies) != 1 || !regexp.MustCompile(`^v0\.6\.0-fake-[0-9]{8}T[0-9]{6}Z\.db$`).MatchString(copies[0]) {
		t.Fatalf("copies %q", copies)
	}
	copyPath := "backups/" + copies[0]
	if mode(t, filepath.Join(w.h.Dir, copyPath)) != 0o600 || readFile(filepath.Join(w.h.Dir, copyPath)) != storeCopy {
		t.Fatalf("the copy's mode or content")
	}
	if !inOrder(out, "1 new commit(s) on origin/main:", "the running image is kept as stargaite:local-previous",
		"if this stops before the new version starts, git reset --hard "+old+" returns the clone to where it was",
		"building version "+nv, "backing up the store to "+copyPath, wayBackHead, "  docker compose stop",
		"  docker compose run --rm --no-deps -T stargaite restore --yes - < "+copyPath, "  docker tag stargaite:local-previous stargaite:local",
		"  docker compose up -d --no-build", "and, before any later build, git reset --hard "+old+", which the console does not run",
		"starting version "+nv, "if this stops before the new version starts, docker compose up -d --no-build in this clone starts it") {
		t.Fatalf("said:\n%s", out)
	}
	if strings.Count(out, wayBackHead) != 1 || strings.Contains(out, "the upgrade failed") || strings.Contains(out, "a migration") ||
		strings.Contains(out, "dev build") || strings.Contains(readFile(w.log), "docker build ") {
		t.Fatalf("said:\n%s", out)
	}
	if res, _, err := w.plain(map[string]string{"yes": "yes"}); err != nil || res != "up to date with origin/main; nothing was changed" {
		t.Fatalf("again: %q, %v", res, err)
	}
}

// TestUpgradeReadyLine: the result says a version only from a log line that
// decodes as JSON with msg ready and a version, the last such line;
// otherwise the clone's version runs.
func TestUpgradeReadyLine(t *testing.T) {
	for _, c := range []struct{ name, logs, want string }{
		{"no ready line", `echo 'not json: "msg":"ready"'`, ""},
		{"prefixed JSON", `echo 'stargaite  | {"msg":"ready","version":"x"}'`, ""},
		{"ready, no version", `echo '{"msg":"ready"}'`, ""},
		{"two ready lines", `echo '{"msg":"ready","version":"a"}'; echo '{"msg":"ready","version":"b"}'`, "upgraded: version b is ready"},
		{"a versionless ready line last", `echo '{"msg":"ready","version":"a"}'; echo '{"msg":"ready"}'`, "upgraded: version a is ready"},
		{"another message", `echo '{"msg":"started","version":"a"}'`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := upgradeWorld(t, 1)
			t.Setenv("LOGS_DO", c.logs)
			want := c.want
			if want == "" {
				want = "upgraded: running " + w.newVersion()
			}
			res, out, err := w.plain(map[string]string{"yes": "yes"})
			if err != nil || res != want || w.head() != w.upstreamHead() {
				t.Fatalf("res %q, want %q, err %v\n%s", res, want, err, out)
			}
		})
	}
}

// TestUpgradeMigrationDevBuild is U3.
func TestUpgradeMigrationDevBuild(t *testing.T) {
	w := upgradeWorld(t, 0)
	w.push(1, "internal/store/migrations/0002_x.sql")
	writeFile(t, filepath.Join(w.bin, "running"), "dev\n", 0o644)
	nv := w.newVersion()
	res, out, err := w.plain(map[string]string{"yes": "yes"})
	copies := w.copies()
	if err != nil || !strings.HasPrefix(res, "upgraded: ") || len(copies) != 1 || !strings.HasPrefix(copies[0], "dev-") ||
		!inOrder(out, "a migration of the store changed: migrations are one-way, so the previous version cannot open the store they migrate",
			"the running version is unknown (a dev build); this clone is at v0.6.0-fake", "building version "+nv) ||
		strings.Contains(out, "is not this clone's") {
		t.Fatalf("res %q, err %v, copies %q\n%s", res, err, copies, out)
	}
}

// TestUpgradeRefusals is U4, U5 and V1: each refused with nothing changed
// and HEAD where it was; mended, the upgrade runs. Without --yes, a usage
// error.
func TestUpgradeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, err string
		ahead     int
		set, mend func(w *upWorld)
	}{
		{"not a repository", "upgrades come from a git clone of stargaite, with git: see https://git-scm.com/downloads", 1,
			func(w *upWorld) { mv(t, filepath.Join(w.h.Dir, ".git"), filepath.Join(w.h.Dir, "git.away")) },
			func(w *upWorld) { mv(t, filepath.Join(w.h.Dir, "git.away"), filepath.Join(w.h.Dir, ".git")) }},
		{"detached", "HEAD is detached, so no branch says what to upgrade to: git switch <branch>, then run this again", 1,
			func(w *upWorld) { w.git(w.h.Dir, "checkout", "-q", "--detach", "HEAD") },
			func(w *upWorld) { w.git(w.h.Dir, "checkout", "-q", "main") }},
		{"no upstream", "branch main has no upstream to upgrade from: git branch --set-upstream-to <remote>/<branch>", 1,
			func(w *upWorld) { w.git(w.h.Dir, "branch", "--unset-upstream") },
			func(w *upWorld) { w.git(w.h.Dir, "branch", "-q", "--set-upstream-to", "origin/main") }},
		{"dirty", "the clone has local changes to tracked files (git status lists them): commit or stash them, then run this again", 1,
			func(w *upWorld) { writeFile(t, filepath.Join(w.h.Dir, "f0"), "changed\n", 0o644) },
			func(w *upWorld) { writeFile(t, filepath.Join(w.h.Dir, "f0"), "f0\n", 0o644) }},
		{"diverged", "the clone has commits that origin/main lacks, so it cannot fast-forward: merge or rebase it yourself, then run this again", 1,
			func(w *upWorld) { w.git(w.h.Dir, "commit", "-q", "--allow-empty", "-m", "mine") },
			func(w *upWorld) { w.git(w.h.Dir, "reset", "-q", "--hard", "v0.6.0-fake") }},
		{"fetch fails", "git fetch failed or did not finish within 20 s; nothing was changed", 1,
			func(w *upWorld) { w.git(w.h.Dir, "remote", "set-url", "origin", filepath.Join(w.bin, "nowhere.git")) },
			func(w *upWorld) { w.git(w.h.Dir, "remote", "set-url", "origin", w.up) }},
		{"no container", "no container runs here: ./setup.sh install starts it", 1,
			func(*upWorld) { t.Setenv("PS_DO", ":") }, func(*upWorld) { t.Setenv("PS_DO", "") }},
		{"unfinished", "up to date with origin/main, but v0.6.0-fake runs, not this clone's commit: an upgrade did not finish; " +
			"git reset --hard v0.6.0-fake, then run this again", 1,
			func(w *upWorld) {
				w.git(w.h.Dir, "fetch", "-q")
				w.git(w.h.Dir, "merge", "-q", "--ff-only", "@{upstream}")
			},
			func(w *upWorld) { w.git(w.h.Dir, "reset", "-q", "--hard", "v0.6.0-fake") }},
	} {
		w := upgradeWorld(t, tc.ahead)
		tc.set(w)
		head := w.headFile()
		_, out, err := w.plain(map[string]string{"yes": "yes"})
		if err == nil || err.Error() != tc.err || w.changed() || len(w.copies()) != 0 || w.headFile() != head {
			t.Fatalf("%s: err %v, log %q\n%s", tc.name, err, w.changing(), out)
		}
		tc.mend(w)
		if res, out, err := w.plain(map[string]string{"yes": "yes"}); err != nil || !strings.HasPrefix(res, "upgraded: ") {
			t.Fatalf("%s mended: res %q, err %v\n%s", tc.name, res, err, out)
		}
	}
	w := upgradeWorld(t, 1)
	head := w.head()
	_, out, err := w.plain(nil)
	var usage *ui.UsageError
	if !errors.As(err, &usage) || !strings.HasPrefix(err.Error(), "missing --yes: upgrading: the clone fast-forwards to origin/main") ||
		w.changed() || w.head() != head {
		t.Fatalf("no --yes: err %v, log %q\n%s", err, w.changing(), out)
	}
}

func mv(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

// TestUpgradeLegacy is U8: a running CLI that predates backup - takes a copy
// made by hand, said how; a current one takes its own backup whatever is
// given.
func TestUpgradeLegacy(t *testing.T) {
	w := upgradeWorld(t, 1)
	w.help = "usage: stargaite <command>\n  backup file\n"
	head := w.head()
	_, out, err := w.plain(map[string]string{"yes": "yes"})
	var usage *ui.UsageError
	if !errors.As(err, &usage) || err.Error() != "missing --legacy-backup: the copy made by hand, on this host, such as backups/pre-upgrade.db" ||
		!inOrder(out, "the running stargaite predates backup -, so the console cannot take the backup: make it by hand, a copy that also stays in the volume,",
			"  docker compose exec -T stargaite /stargaite backup /data/pre-upgrade.db && mkdir -p backups && "+
				"docker compose cp stargaite:/data/pre-upgrade.db backups/") || w.changed() || w.head() != head {
		t.Fatalf("err %v\n%s", err, out)
	}
	for _, tc := range []struct{ path, word string }{{"backups/pre.db", "backups/pre.db"}, {"my backups/pre.db", "'my backups/pre.db'"}} {
		w := upgradeWorld(t, 1)
		w.help = "usage: stargaite <command>\n  backup file\n"
		writeFile(t, filepath.Join(w.h.Dir, tc.path), storeCopy, 0o600)
		res, out, err := w.plain(map[string]string{"yes": "yes", "legacy-backup": tc.path})
		if err != nil || !strings.HasPrefix(res, "upgraded: ") || slices.Contains(w.changing(), "cli backup - --json") ||
			!slices.Contains(strings.Split(out, "\n"), "  docker compose run --rm --no-deps -T stargaite restore --yes - < "+tc.word) {
			t.Fatalf("%s: res %q, err %v\n%s", tc.path, res, err, out)
		}
	}
	w = upgradeWorld(t, 1)
	w.help = "usage: stargaite <command>\n  backup file\n"
	writeFile(t, filepath.Join(w.h.Dir, "backups", "empty.db"), "", 0o600)
	if _, out, err := w.plain(map[string]string{"yes": "yes", "legacy-backup": "backups/empty.db"}); err == nil ||
		err.Error() != "backups/empty.db is not a file with a copy in it; nothing was changed" || w.changed() {
		t.Fatalf("empty: err %v\n%s", err, out)
	}
	w = upgradeWorld(t, 1)
	writeFile(t, filepath.Join(w.h.Dir, "backups", "pre.db"), storeCopy, 0o600)
	res, out, err := w.plain(map[string]string{"yes": "yes", "legacy-backup": "backups/pre.db"})
	var fresh string
	for _, c := range w.copies() {
		if c != "pre.db" {
			fresh = "backups/" + c
		}
	}
	if err != nil || !strings.HasPrefix(res, "upgraded: ") || !slices.Contains(w.changing(), "cli backup - --json") || len(w.copies()) != 2 ||
		!slices.Contains(strings.Split(out, "\n"), "  docker compose run --rm --no-deps -T stargaite restore --yes - < "+fresh) {
		t.Fatalf("current CLI: res %q, err %v, copies %q\n%s", res, err, w.copies(), out)
	}
}

// TestUpgradeFailures is U9: each failure says how to go back, the way back
// said exactly when a backup exists; after a failure before the start, the
// clone is refused until reset, then upgrades.
func TestUpgradeFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    func(w *upWorld)
		err    func(old string) string // exact, or a part with contains
		exact  bool
		copies int
		ups    int
		again  bool // run again: refused, then reset, then upgraded
	}{
		{"build fails", func(*upWorld) { t.Setenv("BUILD_DO", "exit 1") }, func(old string) string {
			return "the build failed or took over 20 minutes: v0.6.0-fake still runs, the store unchanged; git reset --hard " + old + " returns the clone to it"
		}, true, 0, 0, true},
		{"backup short", func(w *upWorld) { w.backup = shortBackup }, func(string) string { return "the backup failed (the copy is 6 bytes, the proxy said 26)" },
			false, 0, 0, true},
		{"backups a file", func(w *upWorld) { writeFile(t, filepath.Join(w.h.Dir, "backups"), "a file", 0o600) },
			func(string) string { return "not a directory), so the new version was not started" }, false, 0, 0, false},
		{"start fails", func(*upWorld) { t.Setenv("UP_DO", "exit 1") }, func(string) string {
			return "the upgrade failed. Nothing rolls back by itself, since a migration may be one-way: the way back is above"
		}, true, 1, 1, false},
		{"validate refuses", func(w *upWorld) { w.refuseAfter = true }, func(string) string {
			return "the upgrade failed. Nothing rolls back by itself, since a migration may be one-way: the way back is above"
		}, true, 1, 1, false},
	} {
		w := upgradeWorld(t, 1)
		old := w.head()
		tc.set(w)
		_, out, err := w.plain(map[string]string{"yes": "yes"})
		copies := len(w.copies())
		want := tc.err(old)
		if err == nil || tc.exact && err.Error() != want || !tc.exact && !strings.Contains(err.Error(), want) || copies != tc.copies ||
			strings.Count(readFile(w.log), "docker compose up -d --no-build\n") != tc.ups || strings.Count(out, wayBackHead) != tc.copies {
			t.Fatalf("%s: err %v, %d copies\n%s", tc.name, err, copies, out)
		}
		if tc.copies > 0 && !inOrder(out, wayBackHead, "starting version "+w.newVersion()) {
			t.Fatalf("%s: the way back is not said before the start:\n%s", tc.name, out)
		}
		if tc.name == "validate refuses" && !slices.Contains(strings.Split(out, "\n"),
			"validate exits 1, and 0 before: the new version refuses part of the stored configuration") {
			t.Fatalf("validate: %s", out)
		}
		if tc.name == "backup short" && !strings.Contains(err.Error(), "docker tag stargaite:local-previous stargaite:local and git reset --hard "+old+" return to it") {
			t.Fatalf("backup: %v", err)
		}
		if !tc.again {
			continue
		}
		t.Setenv("BUILD_DO", "")
		w.backup = goodBackup
		if _, _, err := w.plain(map[string]string{"yes": "yes"}); err == nil || !strings.HasPrefix(err.Error(), "up to date with origin/main, but v0.6.0-fake runs") {
			t.Fatalf("%s, again: %v", tc.name, err)
		}
		w.git(w.h.Dir, "reset", "-q", "--hard", "v0.6.0-fake")
		if res, out, err := w.plain(map[string]string{"yes": "yes"}); err != nil || !strings.HasPrefix(res, "upgraded: ") {
			t.Fatalf("%s, reset: %q, %v\n%s", tc.name, res, err, out)
		}
	}
}

// pressAt is a scripted front that answers yes and presses err during the
// work piece id.
func pressAt(id string, err error) *scripted {
	s := &scripted{answers: map[string][]string{"yes": {"yes"}}, workErr: err}
	s.leaveWork = func(q ui.Question) bool { return q.ID == id }
	return s
}

// TestUpgradeEscDuringMerge: Esc or Ctrl-C during the merge lets it run to
// its end, then says where the clone is and how to return it (on the error
// after Ctrl-C, the front having quit), and builds nothing.
func TestUpgradeEscDuringMerge(t *testing.T) {
	for _, press := range []error{ui.ErrLeave, ui.ErrQuit} {
		w := upgradeWorld(t, 1)
		t.Setenv("MERGE_SLEEP", "1")
		old, nv := w.head(), w.newVersion()
		s := pressAt("merge", press)
		_, err := w.run(ui.New(s, nil))
		if !errors.Is(err, press) || w.head() != w.upstreamHead() || strings.Contains(readFile(w.log), "docker compose build") ||
			!strings.Contains(sayWhere(s, err, press), "the clone is at "+nv+" and nothing was built: v0.6.0-fake still runs, the store unchanged; "+
				"git reset --hard "+old+" returns the clone to where it was\n") {
			t.Fatalf("%v: err %v\n%s", press, err, s.output())
		}
	}
}

// sayWhere is where a step's failure is said after press: the front's
// output after Esc, the error after Ctrl-C, the front having quit.
func sayWhere(s *scripted, err error, press error) string {
	if press == ui.ErrQuit {
		return err.Error() + "\n"
	}
	return s.output()
}

// TestUpgradeCancelBuild: Esc or Ctrl-C during the build stops it, then
// says how to return (on the error after Ctrl-C); no backup, no start.
func TestUpgradeCancelBuild(t *testing.T) {
	for _, press := range []error{ui.ErrLeave, ui.ErrQuit} {
		w := upgradeWorld(t, 1)
		t.Setenv("BUILD_DO", "trap 'echo INT >>\""+w.log+"\"; exit 130' INT; /bin/sleep 5 >/dev/null 2>&1 & wait")
		old := w.head()
		s := pressAt("build", press)
		_, err := w.run(ui.New(s, nil))
		if !errors.Is(err, press) || !strings.Contains(readFile(w.log), "\nINT\n") || slices.Contains(w.changing(), "cli backup - --json") ||
			strings.Contains(readFile(w.log), "compose up") || !strings.Contains(sayWhere(s, err, press), "the build failed or took over 20 minutes: v0.6.0-fake "+
			"still runs, the store unchanged; git reset --hard "+old+" returns the clone to it\n") {
			t.Fatalf("%v: err %v, log %q\n%s", press, err, w.entries(), s.output())
		}
	}
}

// TestUpgradeEscDuringBackup: Esc during the backup stops it, leaves
// nothing in backups/, and says how to return, the image name already the
// new build's.
func TestUpgradeEscDuringBackup(t *testing.T) {
	w := upgradeWorld(t, 1)
	w.backup = func(ctx context.Context, _, _ io.Writer) int { <-ctx.Done(); return 1 }
	old := w.head()
	s := pressAt("backup", ui.ErrLeave)
	_, err := w.run(ui.New(s, nil))
	if !errors.Is(err, ui.ErrLeave) || len(w.copies()) != 0 || strings.Contains(readFile(w.log), "compose up") ||
		!strings.Contains(s.output(), "the backup failed (stopped), so the new version was not started: v0.6.0-fake still runs, the store unchanged; "+
			"docker tag stargaite:local-previous stargaite:local and git reset --hard "+old+" return to it\n") {
		t.Fatalf("err %v, copies %q\n%s", err, w.copies(), s.output())
	}
}

// TestUpgradeMergeBound: a merge that outlives its bound is stopped, with
// its own sentence.
func TestUpgradeMergeBound(t *testing.T) {
	w := upgradeWorld(t, 1)
	t.Setenv("MERGE_SLEEP", "30")
	w.h.ShortenWaits(200 * time.Millisecond)
	old := w.head()
	start := time.Now()
	_, out, err := w.plain(map[string]string{"yes": "yes"})
	if err == nil || err.Error() != "git merge --ff-only did not finish within 2 minutes and was stopped: v0.6.0-fake still runs, the store unchanged; "+
		"files it wrote may be left untracked (git status --untracked-files=all lists them): remove them, then git reset --hard "+old ||
		time.Since(start) > 12*time.Second || strings.Contains(readFile(w.log), "docker compose build") || len(w.copies()) != 0 {
		t.Fatalf("err %v after %s\n%s", err, time.Since(start), out)
	}
}

// TestUpgradeCtrlCCarriesWayBack: Ctrl-C during the merge, the build or the
// backup quits the front, so how to return is carried on the error, for main
// to print once the terminal is restored; nothing is started.
func TestUpgradeCtrlCCarriesWayBack(t *testing.T) {
	for _, tc := range []struct {
		id   string
		text func(old, nv string) string
	}{
		{"merge", func(old, nv string) string {
			return "the clone is at " + nv + " and nothing was built: v0.6.0-fake still runs, the store unchanged; git reset --hard " + old +
				" returns the clone to where it was"
		}},
		{"build", func(old, _ string) string {
			return "the build failed or took over 20 minutes: v0.6.0-fake still runs, the store unchanged; git reset --hard " + old + " returns the clone to it"
		}},
		{"backup", func(old, _ string) string {
			return "the backup failed (stopped), so the new version was not started: v0.6.0-fake still runs, the store unchanged; " +
				"docker tag stargaite:local-previous stargaite:local and git reset --hard " + old + " return to it"
		}},
	} {
		w := upgradeWorld(t, 1)
		w.backup = func(ctx context.Context, _, _ io.Writer) int { <-ctx.Done(); return 1 }
		old, nv := w.head(), w.newVersion()
		f := &quitFront{scripted: &scripted{answers: map[string][]string{"yes": {"yes"}}}, id: tc.id}
		_, err := w.run(ui.New(f, nil))
		if !errors.Is(err, ui.ErrQuit) || !errors.Is(err, ErrUnfinished) || err.Error() != "quit; the upgrade did not finish: "+tc.text(old, nv) ||
			strings.Contains(readFile(w.log), "compose up") {
			t.Fatalf("%s: err %v\n%s", tc.id, err, f.output())
		}
	}
}

// sessionEnds is a front on which, as the line starting with trigger is
// said, cancel (when set) ends the session, and unless prints that line and
// every later one are not printed; a work piece's ask waits for its context.
type sessionEnds struct {
	mu      sync.Mutex
	out     strings.Builder
	trigger string
	cancel  context.CancelFunc
	prints  bool
	ended   bool
}

func (f *sessionEnds) Ask(ctx context.Context, q ui.Question) (string, error) {
	if q.Kind == ui.KindWork {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return "yes", ctx.Err()
}

func (f *sessionEnds) Show(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ended && strings.HasPrefix(text, f.trigger) {
		f.ended = true
		if f.cancel != nil {
			f.cancel()
		}
	}
	if f.ended && !f.prints {
		return errors.New("not printed")
	}
	f.out.WriteString(text + "\n")
	return nil
}

// TestUpgradeEndedBeforeStep: the merge and the start, which run to their
// end once begun, begin only once their line is printed and the session
// goes on: not after the guided front quits (the session ends, nothing more
// printed), nor after a signal in plain mode (the session ends, lines still
// printed), nor when a line cannot be printed. Before the start, how to
// return is the error.
func TestUpgradeEndedBeforeStep(t *testing.T) {
	for _, trigger := range []string{"the running image is kept as", "if this stops before the new version starts, git reset", "the way back to",
		"if this stops before the new version starts, docker compose up"} {
		for _, mode := range []struct {
			name           string
			cancels, print bool
		}{{"front quit", true, false}, {"signal, plain", true, true}, {"not printed", false, false}} {
			w := upgradeWorld(t, 1)
			old := w.head()
			ctx, cancel := context.WithCancel(context.Background())
			f := &sessionEnds{trigger: trigger, prints: mode.print}
			if mode.cancels {
				f.cancel = cancel
			}
			u := ui.New(f, nil)
			_, err := u.Run(ctx, upgradeSteps(), func(ctx context.Context) (string, error) { return Upgrade(ctx, u, w.proxy(), w.h) })
			cancel()
			log := readFile(w.log)
			if err == nil || strings.Contains(log, "compose up") {
				t.Fatalf("%s, %s: err %v, log %q", trigger, mode.name, err, w.changing())
			}
			if strings.HasPrefix(trigger, "the running") || strings.HasSuffix(trigger, "git reset") {
				if w.head() != old || strings.Contains(log, "git merge --ff-only") {
					t.Fatalf("%s, %s: merged: err %v", trigger, mode.name, err)
				}
				continue
			}
			if errors.Is(err, ErrUnfinished) != mode.cancels || !strings.HasSuffix(err.Error(), "the session ended before the start, so the new version "+
				"was not started: v0.6.0-fake still runs, the store unchanged; docker tag stargaite:local-previous stargaite:local and git reset --hard "+
				old+" return to it") {
				t.Fatalf("%s, %s: err %v", trigger, mode.name, err)
			}
		}
	}
}

// TestUpgradeEscDuringStart: Esc during the start lets it run to its end,
// then the checks run as without Esc; Ctrl-C there lets it run to its end
// and checks nothing, the front having quit.
func TestUpgradeEscDuringStart(t *testing.T) {
	w := upgradeWorld(t, 1)
	t.Setenv("UP_DO", "/bin/sleep 1")
	s := pressAt("start", ui.ErrLeave)
	res, err := w.run(ui.New(s, nil))
	if !errors.Is(err, ErrReexec) || !strings.HasPrefix(res, "upgraded: ") || strings.Count(readFile(w.log), "cli validate --json\n") != 2 {
		t.Fatalf("res %q, err %v\n%s", res, err, s.output())
	}
	w = upgradeWorld(t, 1)
	t.Setenv("UP_DO", "/bin/sleep 1")
	s = pressAt("start", ui.ErrQuit)
	_, err = w.run(ui.New(s, nil))
	log := readFile(w.log)
	up := strings.Index(log, "docker compose up -d --no-build\n")
	if err != ui.ErrQuit || up < 0 || strings.TrimSpace(readFile(filepath.Join(w.bin, "running"))) != w.newVersion() ||
		strings.Contains(log[up:], "cli version --json") {
		t.Fatalf("Ctrl-C: err %v, log %q", err, w.entries())
	}
}

// TestUpgradeNotReady is TestSetupBounds' "never ready": a proxy that never
// gets ready stops the upgrade before any CLI call.
func TestUpgradeNotReady(t *testing.T) {
	w := upgradeWorld(t, 1)
	writeFile(t, filepath.Join(w.h.Dir, ".env"), "STARGAITE_PORT="+closedPort(t)+"\n", 0o600)
	w.h.ShortenWaits(200 * time.Millisecond)
	_, out, err := w.plain(map[string]string{"yes": "yes"})
	if err == nil || err.Error() != "the proxy did not become ready" || !inOrder(out, "not ready within 120 s; the last 30 log lines:", "a log line") ||
		strings.Contains(out, "remote again") || strings.Count(readFile(w.log), "docker compose logs --tail 30 stargaite\n") != 1 ||
		slices.ContainsFunc(w.entries(), func(l string) bool { return strings.HasPrefix(l, "cli ") }) {
		t.Fatalf("err %v, log %q\n%s", err, w.entries(), out)
	}
}

// TestRebuild: the clone's own version, dirty included, built and started
// without fetching, merging or a git reset clause.
func TestRebuild(t *testing.T) {
	w := upgradeWorld(t, 1)
	writeFile(t, filepath.Join(w.bin, "running"), "v-old\n", 0o644)
	writeFile(t, filepath.Join(w.h.Dir, "f0"), "changed\n", 0o644)
	res, out, err := w.plain(map[string]string{"yes": "yes", "rebuild": "yes"})
	var git []string
	for _, l := range w.entries() {
		if strings.HasPrefix(l, "git ") {
			git = append(git, l)
		}
	}
	want := []string{"docker tag sha256:0123abcd stargaite:local-previous", "docker compose build", "cli backup - --json", "docker compose up -d --no-build",
		"cli version --json", "cli validate --json"}
	if got := w.changing(); err != nil || res != "rebuilt: version v0.6.0-fake-dirty is ready" ||
		!slices.Equal(got[len(got)-len(want):], want) || slices.ContainsFunc(git, func(l string) bool { return !strings.HasPrefix(l, "git describe ") }) ||
		!strings.Contains(readFile(w.log), "STARGAITE_VERSION=v0.6.0-fake-dirty\n") {
		t.Fatalf("res %q, err %v, git %q, log %q\n%s", res, err, git, w.changing(), out)
	}
	if !inOrder(out, "the proxy runs v-old; this clone is at v0.6.0-fake-dirty",
		"rebuilding: the image of this clone's version, v0.6.0-fake-dirty, is built; then a backup of the store is taken and the container "+
			"recreated on it, which interrupts traffic briefly", "  docker compose up -d --no-build") || strings.Contains(out, "git reset") {
		t.Fatalf("said:\n%s", out)
	}
	// D1's mend: a validate answer this console cannot decode is compared
	// by its exit, as setup.sh did.
	w = upgradeWorld(t, 0)
	w.oddValidate = true
	if res, out, err := w.plain(map[string]string{"yes": "yes", "rebuild": "yes"}); err != nil || !strings.HasPrefix(res, "rebuilt: ") {
		t.Fatalf("undecodable validate: res %q, err %v\n%s", res, err, out)
	}
}

// TestRebuildFailures: a Rebuild's failures say how to return with no git
// reset, and name the rebuild.
func TestRebuildFailures(t *testing.T) {
	for _, tc := range []struct {
		name, err string
		set       func(w *upWorld)
	}{
		{"build fails", "the build failed or took over 20 minutes: v0.6.0-fake still runs, the store unchanged",
			func(*upWorld) { t.Setenv("BUILD_DO", "exit 1") }},
		{"backup short", "the backup failed (the copy is 6 bytes, the proxy said 26), so the new version was not started: v0.6.0-fake still runs, " +
			"the store unchanged; docker tag stargaite:local-previous stargaite:local returns to it",
			func(w *upWorld) { w.backup = shortBackup }},
		{"start fails", "the rebuild failed. Nothing rolls back by itself, since a migration may be one-way: the way back is above",
			func(*upWorld) { t.Setenv("UP_DO", "exit 1") }},
	} {
		w := upgradeWorld(t, 1)
		tc.set(w)
		_, out, err := w.plain(map[string]string{"yes": "yes", "rebuild": "yes"})
		if err == nil || err.Error() != tc.err || strings.Contains(out, "git reset") {
			t.Fatalf("%s: err %v\n%s", tc.name, err, out)
		}
		t.Setenv("BUILD_DO", "")
		t.Setenv("UP_DO", "")
	}
}

// TestUpgradeReexec: at a terminal, the new console is exported once the
// upgrade succeeded, and the session restarts on it; an export that fails
// ends the session; a failed check exports nothing.
func TestUpgradeReexec(t *testing.T) {
	w := upgradeWorld(t, 1)
	nv := w.newVersion()
	res, err := w.run(ui.New(&scripted{answers: map[string][]string{"yes": {"yes"}}}, nil))
	got := w.changing()
	if !errors.Is(err, ErrReexec) || !strings.HasPrefix(res, "upgraded: ") ||
		got[len(got)-1] != "docker build --quiet --build-arg VERSION --target console --output type=local,dest=bin ." ||
		!strings.HasSuffix(readFile(w.log), "VERSION="+nv+"\n") {
		t.Fatalf("res %q, err %v, log %q", res, err, got)
	}
	w = upgradeWorld(t, 1)
	t.Setenv("EXPORT_DO", "exit 1")
	_, err = w.run(ui.New(&scripted{answers: map[string][]string{"yes": {"yes"}}}, nil))
	if !errors.Is(err, ErrEnd) || errors.Is(err, ErrReexec) || err.Error() != "the new version runs, but the new console was not exported "+
		"(docker build of the console failed or was interrupted); ./setup.sh exports and starts it" {
		t.Fatalf("export fails: err %v", err)
	}
	w = upgradeWorld(t, 1)
	w.refuseAfter = true
	t.Setenv("EXPORT_DO", "")
	_, err = w.run(ui.New(&scripted{answers: map[string][]string{"yes": {"yes"}}}, nil))
	if err == nil || errors.Is(err, ErrReexec) || errors.Is(err, ErrEnd) || strings.Contains(readFile(w.log), "docker build ") {
		t.Fatalf("failed check: err %v", err)
	}
}
