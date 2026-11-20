package task

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/cli"
	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
)

// Restore's texts (plan B5, B6) for backups/a.db, taken at copyTime.
const (
	copyTaken   = "2026-11-20 10:00 UTC"
	restoreText = "the store is replaced by the copy backups/a.db, taken " + copyTaken + ": keys revoked since come back, " +
		"spend counted since is forgotten, and no audit record is written; the container is stopped and started again, which interrupts traffic"
	restoring = "restoring backups/a.db, taken " + copyTaken
	startHint = "if this stops before the container starts again, docker compose up -d --no-build in this clone starts it"
	startsIt  = "; docker compose up -d --no-build in this clone starts it"
)

var copyTime = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

func (w *world) backUpOrRestore(u *ui.UI) (string, error) {
	return u.Run(context.Background(), backupSteps(), func(ctx context.Context) (string, error) {
		return BackUpOrRestore(ctx, u, w.proxy(), w.h)
	})
}

func TestBackUpFlags(t *testing.T) {
	w := newWorld(t)
	var out bytes.Buffer
	res, err := w.backUpOrRestore(ui.New(plain.New(&out), map[string]string{"what": "backup"}))
	m := regexp.MustCompile(`^Done: (backups/` + regexp.QuoteMeta(cli.Version) + `-[0-9]{8}T[0-9]{6}Z\.db), ([0-9]+) bytes\.$`).FindStringSubmatch(res)
	if err != nil || m == nil {
		t.Fatalf("%q, %v\n%s", res, err, out.String())
	}
	info, err := os.Stat(filepath.Join(w.h.Dir, m[1]))
	if err != nil || info.Mode().Perm() != 0o600 || m[2] != strconv.FormatInt(info.Size(), 10) ||
		!strings.HasPrefix(w.file(m[1]), "SQLite format 3\x00") || !strings.Contains(out.String(), "backing up the store to "+m[1]+"\n") {
		t.Errorf("%s: %v, %v\n%s", m[1], info, err, out.String())
	}

	w = newWorld(t)
	w.canned(map[string]reply{"version --json": {out: `{"version":"v1"}`}, "backup - --json": {code: 1}})
	_, err = w.backUpOrRestore(ui.New(plain.New(&out), map[string]string{"what": "backup"}))
	if err == nil || err.Error() != "the backup failed (exit 1), so nothing was kept" {
		t.Errorf("err = %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(w.h.Dir, "backups")); len(entries) != 0 {
		t.Errorf("backups/ holds %v", entries)
	}

	// Esc while the store is copied leaves the task, and keeps nothing.
	w = newWorld(t)
	w.runner = func(ctx context.Context, args []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
		if args[0] == "version" {
			_, _ = io.WriteString(stdout, `{"version":"v1"}`)
			return 0, nil
		}
		_, _ = io.WriteString(stdout, "SQLite format 3\x00 a part")
		<-ctx.Done()
		return 0, ctx.Err()
	}
	s := &scripted{answers: map[string][]string{"what": {"backup"}}, leaveWork: func(q ui.Question) bool { return q.ID == "backup" }}
	if _, err = w.backUpOrRestore(ui.New(s, nil)); !errors.Is(err, ui.ErrLeave) || err.Error() != ui.ErrLeave.Error() {
		t.Errorf("Esc: %v\n%s", err, s.output())
	}
	if entries, _ := os.ReadDir(filepath.Join(w.h.Dir, "backups")); len(entries) != 0 {
		t.Errorf("Esc: backups/ holds %v", entries)
	}
}

// restoreClone is a clone with backups/a.db, taken at copyTime, whose docker
// fake answers compose ps -a -q with psa, compose ps -q, compose stop with
// stop, the put-back with run and up with up, logging each call; readiness
// is served once up is logged.
func restoreClone(t *testing.T, psa, stop, run, up string) *machineWorld {
	t.Helper()
	bin := t.TempDir()
	m := &machineWorld{t: t, h: host.New(t.TempDir()), log: filepath.Join(bin, "log")}
	writeFile(t, filepath.Join(bin, "docker"), "#!/bin/sh\necho \"$*\" >>'"+m.log+"'\ncase \"$*\" in\n"+
		"'compose ps -a -q') "+psa+" ;;\n'compose ps -q') echo c0ffee ;;\n'compose stop') "+stop+" ;;\n"+
		"'compose run --rm --no-deps -T stargaite restore --yes --json -')\n"+run+"\n;;\n'compose up -d --no-build') "+up+" ;;\n"+
		"'compose logs --tail 30 stargaite') ;;\n*) exit 9 ;;\nesac\n", 0o755)
	t.Setenv("PATH", bin)
	copyAt := filepath.Join(m.h.Dir, "backups", "a.db")
	writeFile(t, copyAt, "SQLite format 3\x00 the copy", 0o600)
	if err := os.Chtimes(copyAt, copyTime, copyTime); err != nil {
		t.Fatal(err)
	}
	p := closedPort(t)
	m.env("STARGAITE_PORT=" + p + "\n")
	m.serveOnUp(p)
	return m
}

// stops is a stop that succeeds, and putBack a put-back that reads the copy,
// each saying so.
const (
	stops   = "echo 'Container stopping'"
	putBack = "/bin/cat >/dev/null; echo 'restored' >&2; echo '" + `{"path":"-","bytes":25,"data_dir":"/data","replaced":["stargaite.db"]}` + "'"
	// putIntoEmpty is putBack into a volume that held no store.
	putIntoEmpty = "/bin/cat >/dev/null; echo '" + `{"path":"-","bytes":25,"data_dir":"/data","replaced":[]}` + "'"
	// replacedB8 and emptyB8 end B8's two results.
	replacedB8 = ": 25 bytes; the store it replaced is kept in the volume under replaced/ until the next restore."
	emptyB8    = ": 25 bytes; the volume held no store."
)

func (m *machineWorld) restore(u *ui.UI) (string, error) {
	return u.Run(context.Background(), backupSteps(), func(ctx context.Context) (string, error) {
		return BackUpOrRestore(ctx, u, nil, m.h)
	})
}

// changes is the docker log without the compose ps reads.
func (m *machineWorld) changes() string {
	var out []string
	for _, l := range strings.Split(m.dockerLog(), "\n") {
		if l != "" && !strings.HasPrefix(l, "compose ps") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestRestoreFlags(t *testing.T) {
	const all = "compose stop\ncompose run --rm --no-deps -T stargaite restore --yes --json -\ncompose up -d --no-build"
	given := map[string]string{"what": "restore", "from": "backups/a.db", "yes": "yes"}
	notPut := "the copy was not put back (restore exits 1): its words above say whether anything moved"
	var usage *ui.UsageError
	for _, row := range []struct {
		name, psa, run, up string
		answers            map[string]string
		res, err, prefix   string // prefix: the error starts with it and ends with startsIt
		changes            string
	}{
		{"restored", psRuns, putBack, upPause, given, "Done: the store is the copy backups/a.db, taken " + copyTaken + replacedB8, "", "", all},
		{"into an empty volume", psRuns, putIntoEmpty, upPause, given, "Done: the store is the copy backups/a.db, taken " + copyTaken + emptyB8, "", "", all},
		{"without --yes", psRuns, putBack, upPause, map[string]string{"what": "restore", "from": "backups/a.db"}, "", "missing --yes: " + restoreText, "", ""},
		{"not a copy", psRuns, putBack, upPause, map[string]string{"what": "restore", "from": "nope.db", "yes": "yes"}, "",
			"nope.db is not a file with a copy in it; nothing was changed", "", ""},
		{"put back fails", psRuns, "/bin/cat >/dev/null; exit 1", upPause, given, "", notPut + "; the container is started again", "", all},
		{"start fails", psRuns, putBack, "exit 1", given, "", "", "the store is the copy backups/a.db, but the container did not start: ", all},
		{"both fail", psRuns, "/bin/cat >/dev/null; exit 1", "exit 1", given, "", "", notPut + ", and the container did not start: ", all},
		{"no container", psNone, putBack, upPause, given, "", "no container here: ./setup.sh install makes it", "", ""},
		{"no --from", psRuns, putBack, upPause, map[string]string{"what": "restore", "yes": "yes"}, "", "missing --from: Which copy?", "", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			m := restoreClone(t, row.psa, stops, row.run, row.up)
			var out bytes.Buffer
			res, err := m.restore(ui.New(plain.New(&out), row.answers))
			switch {
			case row.prefix != "":
				if err == nil || !strings.HasPrefix(err.Error(), row.prefix) || !strings.HasSuffix(err.Error(), startsIt) {
					t.Fatalf("err = %v\n%s", err, out.String())
				}
			case res != row.res || (row.err == "") != (err == nil) || (err != nil && err.Error() != row.err):
				t.Fatalf("res %q, err %v; want %q, %q\n%s", res, err, row.res, row.err, out.String())
			}
			if (row.name == "without --yes" || row.name == "no --from") && !errors.As(err, &usage) {
				t.Errorf("err %T is not a usage error", err)
			}
			if err != nil && strings.Contains(err.Error(), "the store is as it was") {
				t.Errorf("err = %v", err)
			}
			if m.changes() != row.changes {
				t.Errorf("docker log %q; want %q", m.changes(), row.changes)
			}
			if row.changes != "" && !inOrder(out.String(), restoreText, restoring, startHint, "Container stopping") {
				t.Errorf("output:\n%s", out.String())
			}
		})
	}
}

// TestRestoreStopFails: a stop that fails puts nothing back and starts
// nothing (OPS-10), B6 said before it.
func TestRestoreStopFails(t *testing.T) {
	m := restoreClone(t, psRuns, "echo 'cannot stop' >&2; exit 1", putBack, upPause)
	var out bytes.Buffer
	res, err := m.restore(ui.New(plain.New(&out), map[string]string{"what": "restore", "from": "backups/a.db", "yes": "yes"}))
	if res != "" || err == nil || err.Error() != "docker compose stop failed with exit 1" {
		t.Fatalf("%q, %v\n%s", res, err, out.String())
	}
	if m.changes() != "compose stop" || !inOrder(out.String(), restoreText, restoring, startHint, "cannot stop") {
		t.Errorf("docker log %q\n%s", m.changes(), out.String())
	}
}

// TestRestored: a put-back that did not start or did not end, whatever its
// exit code, may have moved the store, and is never Done (B7).
func TestRestored(t *testing.T) {
	put, up := errors.New("cut"), errors.New("exit 1")
	may := "the copy may not be put back: cut; its words above say whether anything moved"
	for _, row := range []struct {
		upErr error
		want  string
	}{
		{nil, may + "; the container is started again"},
		{up, may + ", and the container did not start: exit 1" + startsIt},
	} {
		if res, err := restored("backups/a.db", copyTaken, 0, host.Restored{}, put, row.upErr); res != "" || err == nil || err.Error() != row.want {
			t.Errorf("start %v: %q, %v; want %q", row.upErr, res, err, row.want)
		}
	}
	// Put back and started: B8's words when restore's answer was decoded,
	// B7's line when it was not.
	done := "Done: the store is the copy backups/a.db, taken " + copyTaken
	for _, row := range []struct {
		r    host.Restored
		want string
	}{
		{host.Restored{Bytes: 25, Replaced: []string{"stargaite.db"}, Decoded: true}, done + replacedB8},
		{host.Restored{Bytes: 25, Decoded: true}, done + emptyB8},
		{host.Restored{}, done + "."},
	} {
		if res, err := restored("backups/a.db", copyTaken, 0, row.r, nil, nil); res != row.want || err != nil {
			t.Errorf("%+v: %q, %v; want %q", row.r, res, err, row.want)
		}
	}
}

func TestRestoreAtTerminal(t *testing.T) {
	m := restoreClone(t, psRuns, stops, putBack, upPause)
	newer := filepath.Join(m.h.Dir, "backups", "b\x1b[2J.db")
	writeFile(t, newer, "SQLite format 3\x00 newer", 0o600)
	if err := os.Chtimes(newer, copyTime, copyTime.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	s := &scripted{answers: map[string][]string{"what": {"restore"}, "from": {leave}}}
	if _, err := m.restore(ui.New(s, nil)); !errors.Is(err, ui.ErrLeave) {
		t.Fatalf("err = %v\n%s", err, s.output())
	}
	q := s.questions[1]
	if q.ID != "from" || q.Prompt != "Which copy?" || !slices.Equal(valueLabels(q), []string{
		"backups/b\x1b[2J.db|b[2J.db · 22 bytes · 2026-11-20 11:00 UTC",
		"backups/a.db|a.db · 25 bytes · 2026-11-20 10:00 UTC"}) {
		t.Errorf("pick %s %q: %q", q.ID, q.Prompt, valueLabels(q))
	}

	m = restoreClone(t, psRuns, stops, putBack, upPause)
	if err := os.Remove(filepath.Join(m.h.Dir, "backups", "a.db")); err != nil {
		t.Fatal(err)
	}
	s = &scripted{answers: map[string][]string{"what": {"restore"}}}
	res, err := m.restore(ui.New(s, nil))
	if err != nil || res != "No copy in backups/ yet: Back up or restore backs up the store now." || len(s.questions) != 1 {
		t.Errorf("%q, %v, asked %q", res, err, s.asked)
	}
	q = s.questions[0]
	if q.Prompt != "Back up or restore?" || !slices.Equal(valueLabels(q), []string{"backup|back up the store now", "restore|restore a copy"}) {
		t.Errorf("pick %q: %q", q.Prompt, valueLabels(q))
	}
}

// slowPutBack is a put-back that reads the copy, then takes 1 s, logging a
// SIGINT that would cut it.
func slowPutBack(sig string) string {
	return "trap 'echo INT >>\"" + sig + "\"; exit 130' INT\n/bin/cat >/dev/null\n/bin/sleep 1\necho restored"
}

// TestRestoreEscDuringPutBack: Esc while the copy is put back lets the
// put-back and the start after it run to their end.
func TestRestoreEscDuringPutBack(t *testing.T) {
	sig := filepath.Join(t.TempDir(), "sig")
	m := restoreClone(t, psRuns, stops, slowPutBack(sig), upPause)
	s := &scripted{answers: map[string][]string{"what": {"restore"}, "from": {"backups/a.db"}, "yes": {"yes"}}}
	s.leaveWork = func(q ui.Question) bool { return q.ID == "put" }
	start := time.Now()
	res, err := m.restore(ui.New(s, nil))
	if err != nil || res != "Done: the store is the copy backups/a.db, taken "+copyTaken+"." || time.Since(start) < time.Second {
		t.Fatalf("%q, %v after %v\n%s", res, err, time.Since(start), s.output())
	}
	if readFile(sig) != "" || !strings.HasSuffix(m.changes(), "compose run --rm --no-deps -T stargaite restore --yes --json -\ncompose up -d --no-build") {
		t.Errorf("sig %q, docker log %q", readFile(sig), m.changes())
	}
}

// quitFront is scripted, but answers Ctrl-C to the work piece id, and
// from then on its Show blocks until its context ends, as the guided front
// does once its program has quit.
type quitFront struct {
	*scripted
	id   string
	quit atomic.Bool
}

func (f *quitFront) Ask(ctx context.Context, q ui.Question) (string, error) {
	if q.Kind == ui.KindWork && q.ID == f.id {
		f.quit.Store(true)
		return "", ui.ErrQuit
	}
	return f.scripted.Ask(ctx, q)
}

func (f *quitFront) Show(ctx context.Context, text string) error {
	if f.quit.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.scripted.Show(ctx, text)
}

// TestRestoreQuitDuringPutBack: Ctrl-C while the copy is put back sends it
// no signal; the task returns once the put-back and the start have run,
// saying nothing to a front that has quit: its error carries how it ended.
func TestRestoreQuitDuringPutBack(t *testing.T) {
	sig := filepath.Join(t.TempDir(), "sig")
	m := restoreClone(t, psRuns, stops, slowPutBack(sig), upPause)
	f := &quitFront{scripted: &scripted{answers: map[string][]string{"what": {"restore"}, "from": {"backups/a.db"}, "yes": {"yes"}}}, id: "put"}
	done := make(chan error, 1)
	go func() { _, err := m.restore(ui.New(f, nil)); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ui.ErrQuit) || !errors.Is(err, ErrRanOn) || err.Error() != "quit; the restore ran to its end: Done: the store is the copy backups/a.db, taken "+copyTaken+"." {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the task did not return within 5 s of the quit")
	}
	if readFile(sig) != "" || !strings.HasSuffix(m.changes(), "compose run --rm --no-deps -T stargaite restore --yes --json -\ncompose up -d --no-build") ||
		!inOrder(f.output(), restoring, startHint, "Container stopping") {
		t.Errorf("sig %q, docker log %q\n%s", readFile(sig), m.changes(), f.output())
	}
}
