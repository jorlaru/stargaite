package guided

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jorlaru/stargaite/internal/console/task"
)

// The guided runs of the machine tasks (C64a): Stop or start with the
// home's Start, See logs, Check everything, Back up or restore and
// Uninstall, at layer 3.

// machineFake is a docker fake that keeps the container's state in files of
// dir, so a stop, a start or a down is seen by the next read; it logs every
// argv to dir/log. @ stands for dir.
const machineFake = `#!/bin/sh
echo "$*" >>'@/log'
case "$*" in
'compose ps -a -q') if [ ! -e '@/gone' ]; then echo c0ffee; fi ;;
'compose ps -q') if [ -e '@/runs' ]; then echo c0ffee; fi ;;
'compose up -d --no-build'|'compose restart') : >'@/runs' ;;
'compose stop') echo 'the fake stopped the container'; /bin/rm -f '@/runs' ;;
'compose down') /bin/rm -f '@/runs'; : >'@/gone' ;;
'compose logs --tail 30 stargaite') ;;
'compose logs --no-log-prefix --tail 100 stargaite') echo 'a log line'; printf 'esc \033[2J end\n' ;;
'compose logs --no-log-prefix --follow --tail 0 stargaite') trap "echo INT >>'@/log'; exit 130" INT; echo 'a followed line'; /bin/sleep 60 ;;
'compose run --rm --no-deps -T stargaite restore --yes -') /bin/cat >'@/restored' ;;
'compose config --format json') echo '{"name":"proj","services":{"stargaite":{"image":"stargaite:local","environment":{"K":"sk-SECRET-x"}}},"volumes":{"stargaite-data":{"name":"proj_stargaite-data"}}}' ;;
'image inspect --format {{.Id}} stargaite:local'|'image inspect --format {{.Id}} stargaite:local-previous') echo sha256:feed ;;
'rmi stargaite:local'|'rmi stargaite:local-previous') echo "Untagged: $2" ;;
*) exit 9 ;;
esac
`

// machine is planeWorld whose docker fake is machineFake, the container
// running when runs. It returns the world, the data plane's port, the
// provider's URL and the fake's directory.
func machine(t *testing.T, runs bool) (w *aliasWorld, port, provider, dir string) {
	t.Helper()
	w, port, provider = planeWorld(t)
	dir = t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "docker"), []byte(strings.ReplaceAll(machineFake, "@", dir)), 0o755)
	if err == nil && runs {
		err = os.WriteFile(filepath.Join(dir, "runs"), nil, 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return w, port, provider, dir
}

// dockerLog is the fake's argvs, one per line.
func dockerLog(dir string) string { b, _ := os.ReadFile(filepath.Join(dir, "log")); return string(b) }

// frameCount is how many frames have been recorded.
func (h *harness) frameCount() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.frames) }

// shownAfter waits for a frame recorded after the first n that holds s, and
// returns it: a question asked again draws a frame equal to an older one.
func (h *harness) shownAfter(n int, s string) tea.View {
	h.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		h.mu.Lock()
		for _, f := range h.frames[min(n, len(h.frames)):] {
			if strings.Contains(f.Content, s) {
				h.mu.Unlock()
				return f
			}
		}
		h.mu.Unlock()
		select {
		case <-h.seen:
		case <-deadline:
			h.t.Fatalf("no frame after %d holds %q; last:\n%s", n, s, h.lastFrame())
		}
	}
}

// said is h's output with its escapes and carriage returns dropped.
func (h *harness) said() string { return strings.ReplaceAll(ansi.Strip(h.out.String()), "\r", "") }

func TestGuidedStopOrStart(t *testing.T) {
	h := start(t)
	w, port, _, dir := machine(t, false)
	done := w.home(h)
	f := h.waitShown("What do you want to do?")
	h.waitOutput("http://127.0.0.1:" + port + "/v1: stopped · clone dev")
	if !strings.Contains(f.Content, "── Suggested\n> Start the container: it is stopped\n") {
		t.Fatalf("home frame:\n%s", f.Content)
	}
	n := h.frameCount()
	h.enter()
	h.waitOutput("starting the stopped container")
	h.waitOutput("Done: the container is started and ready.")
	h.shownAfter(n, "What do you want to do?")
	h.choose("Stop or start")
	f = h.waitShown("What do you want to do with the container?")
	if !strings.Contains(f.Content, "> stop it\n  start it\n  restart it\n") {
		t.Errorf("the action pick:\n%s", f.Content)
	}
	h.choose("stop it")
	h.waitOutput("the container is stopped: requests in flight are given up to 60 s to finish, " +
		"then the data plane answers nothing until it is started")
	h.yes("● the container")
	n = h.frameCount()
	h.waitOutput("Done: the container is stopped.")
	h.shownAfter(n, "What do you want to do?")
	quitHome(t, h, done)
	h.ended()
	h.fits()
	if l := dockerLog(dir); strings.Count(l, "compose up -d --no-build\n") != 1 || strings.Count(l, "compose stop\n") != 1 {
		t.Errorf("docker log:\n%s", l)
	}
}

func TestGuidedSeeLogs(t *testing.T) {
	h := start(t)
	w, _, _, dir := machine(t, true)
	done := w.run(h, "See logs", nil)
	f := h.waitShown("following the logs…")
	if !strings.Contains(f.Content, "esc stop · ctrl+c quit") {
		t.Fatalf("follow frame:\n%s", f.Content)
	}
	h.waitOutput("a followed line")
	h.p.Send(key(tea.KeyEscape))
	if r := ended(t, done, 5*time.Second); r.err != nil || r.res != "Stopped following the logs." {
		t.Fatalf("%+v\n%s", r, h.out.String())
	}
	h.waitOutput("Stopped following the logs.")
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(dockerLog(dir), "INT\n"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the follow got no SIGINT: %q", dockerLog(dir))
		}
	}
	h.front.Done()
	h.ended()
	h.fits()
	if said := h.said(); !strings.Contains(said, "a log line\n") || !strings.Contains(said, "esc ") || !strings.Contains(said, " end\n") {
		t.Errorf("said:\n%q", said)
	}
	if out := h.out.String(); strings.Contains(out, "\x1b[2J") {
		t.Errorf("a log line's escape reached the output:\n%q", out)
	}
	for _, s := range []string{"a log line", "a followed line", " end"} {
		h.noFrameHolds(s)
	}
}

func TestGuidedCheckEverything(t *testing.T) {
	h := start(t)
	w, port, srvURL, _ := machine(t, true)
	slowReady(t, w, port)
	w.must(t, "providers", "add", "mock", "--protocol", "openai", "--base-url", srvURL, "--allow-private-network", "--billing", "none")
	w.must(t, "providers", "add", "payg", "--protocol", "openai", "--base-url", srvURL, "--allow-private-network", "--billing", "pay_per_use")
	w.must(t, "targets", "add", "--kind", "chat", "--", "mock/m")
	w.must(t, "models", "add", "a", "--target", "mock/m")
	w.must(t, "targets", "add", "--kind", "chat", "--", "payg/u")
	done := w.run(h, "Check everything", nil)
	f := h.waitShown("Which one do you want to fix?")
	if !strings.Contains(f.Content, "> none of them now\n  target mock/m is not checked yet") || strings.Contains(f.Content, "payg/u") {
		t.Errorf("the fix pick:\n%s", f.Content)
	}
	for _, s := range []string{"target payg/u has no prices, so no budget counts its spend",
		"  stargaite targets set payg/u --pricing input_per_million=<usd>,output_per_million=<usd>,as_of=<YYYY-MM-DD>",
		"target payg/u is not checked yet", "target mock/m is not checked yet"} {
		h.waitOutput(s)
		if !strings.Contains(s, "mock/m") { // mock/m's line is also the pick's option
			h.noFrameHolds(s)
		}
	}
	h.choose("mock/m")
	f = h.waitShown("1 paid call(s) in all")
	if !strings.Contains(f.Content, "Check it") || !strings.Contains(f.Content, "Not now") {
		t.Errorf("paid frame:\n%s", f.Content)
	}
	h.p.Send(key(tea.KeyEscape))
	if r := ended(t, done, 5*time.Second); r.err != nil || r.res != "Left Make an alias." {
		t.Fatalf("%+v\n%s", r, h.out.String())
	}
	h.front.Done()
	h.ended()
	h.fits()
	noCalls(t, w)
	if slices.ContainsFunc(w.cli(), func(a string) bool { return strings.HasPrefix(a, "targets verify") }) {
		t.Errorf("a target was verified: %q", w.cli())
	}
	titles := []string{"findings", "fix", "name", "model", "fallbacks", "settings", "check", "make"}
	// Make an alias waits for readiness under its first step, then asks its paid one.
	if path := h.barPath(titles); !slices.Equal(path, []string{"findings", "fix", "name", "check"}) {
		t.Errorf("the step bar went %q", path)
	}
}

func TestGuidedBackUpOrRestore(t *testing.T) {
	h := start(t)
	w, _, _, dir := machine(t, true)
	done := w.run(h, "Back up or restore", nil)
	h.waitShown("Back up or restore?")
	h.choose("back up")
	r := ended(t, done, 10*time.Second)
	m := regexp.MustCompile(`^Done: (backups/[A-Za-z0-9._-]+\.db), ([0-9]+) bytes\.$`).FindStringSubmatch(r.res)
	if r.err != nil || m == nil {
		t.Fatalf("%+v\n%s", r, h.out.String())
	}
	h.waitOutput("backing up the store to " + m[1])
	h.front.Done()
	h.ended()
	h.fits()
	copied, err := os.ReadFile(filepath.Join(w.h.Dir, m[1]))
	if err != nil || !bytes.HasPrefix(copied, []byte("SQLite format 3\x00")) || m[2] != strconv.Itoa(len(copied)) {
		t.Fatalf("%s: %d bytes, %v", m[1], len(copied), err)
	}

	h = start(t)
	done = w.run(h, "Back up or restore", nil)
	h.waitShown("Back up or restore?")
	h.choose("restore")
	f := h.waitShown("Which copy?")
	if !strings.Contains(f.Content, "> "+filepath.Base(m[1])+" · "+m[2]+" bytes · ") {
		t.Errorf("the copy pick:\n%s", f.Content)
	}
	h.enter()
	h.waitOutput("keys revoked since come back")
	h.yes("● which copy")
	r = ended(t, done, 10*time.Second)
	if r.err != nil || !strings.HasPrefix(r.res, "Done: the store is the copy "+m[1]+", taken ") {
		t.Fatalf("%+v\n%s", r, h.out.String())
	}
	h.waitOutput(r.res)
	h.front.Done()
	h.ended()
	h.fits()
	said := h.said()
	restoring := strings.Index(said, "restoring "+m[1]+", taken ")
	startIt := strings.Index(said, "if this stops before the container starts again, docker compose up -d --no-build in this clone starts it")
	stopped := strings.Index(said, "the fake stopped the container")
	if restoring < 0 || startIt < restoring || stopped < startIt {
		t.Errorf("B6's lines are not said before the stop (%d, %d, %d):\n%s", restoring, startIt, stopped, said)
	}
	if put, err := os.ReadFile(filepath.Join(dir, "restored")); err != nil || !bytes.Equal(put, copied) {
		t.Errorf("the copy put back is %d bytes, %v; want the backup's %d", len(put), err, len(copied))
	}
	l := dockerLog(dir)
	stop, run, up := strings.Index(l, "compose stop\n"), strings.Index(l, "compose run --rm --no-deps -T stargaite restore --yes -\n"),
		strings.LastIndex(l, "compose up -d --no-build\n")
	if stop < 0 || run < stop || up < run {
		t.Errorf("docker log:\n%s", l)
	}
}

func TestGuidedUninstall(t *testing.T) {
	const kept = "uninstall keeps the volume proj_stargaite-data, which holds the store (keys, configuration and usage), " +
		".env, compose.override.yaml, backups/ and tls/"
	h := start(t)
	w, _, _, dir := machine(t, false) // stopped: the home reads nothing from the proxy
	console := filepath.Join(w.h.Dir, "bin", "stargaite-console")
	if err := errors.Join(os.MkdirAll(filepath.Dir(console), 0o755), os.WriteFile(console, []byte("console"), 0o755)); err != nil {
		t.Fatal(err)
	}
	done := w.home(h)
	h.waitShown("What do you want to do?")
	h.choose("Uninstall")
	h.waitOutput(kept)
	f := h.waitShown("continue?")
	if !strings.Contains(f.Content, "● uninstall") || !strings.Contains(f.Content, "> No") {
		t.Errorf("confirm frame:\n%s", f.Content)
	}
	n := h.frameCount()
	h.enter() // the cursor is on No
	h.waitOutput("Uninstall: stopped; nothing more was changed")
	h.shownAfter(n, "What do you want to do?")
	if l := dockerLog(dir); strings.Contains(l, "compose down") || strings.Contains(l, "rmi ") {
		t.Fatalf("No removed something: %q", l)
	}
	if _, err := os.Stat(console); err != nil {
		t.Fatalf("No removed the console: %v", err)
	}

	n = h.frameCount()
	h.choose("Uninstall")
	h.shownAfter(n, "continue?")
	h.p.Send(key(tea.KeyUp))
	h.shownAfter(n, "> Yes")
	h.enter()
	select {
	case err := <-done:
		if !errors.Is(err, task.ErrEnd) {
			t.Fatalf("Home: %v; want ErrEnd", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Home did not end")
	}
	h.front.Done()
	h.ended()
	h.fits()
	said := h.said()
	for _, s := range []string{"image stargaite:local: removed\n", "image stargaite:local-previous: removed\n",
		"bin/stargaite-console: removed\n", "what it kept goes, if you want, with these commands in this clone:\n",
		"  docker volume rm proj_stargaite-data\n", "  rm .env compose.override.yaml\n", "  rm -r backups tls\n",
		"Done: uninstalled; ./setup.sh installs again on the kept volume.\n"} {
		if strings.Count(said, s) != 1 {
			t.Errorf("said %d times, not once: %q\n%s", strings.Count(said, s), s, said)
		}
	}
	if strings.Contains(said, "sk-SECRET-x") {
		t.Error("the configuration's environment was said")
	}
	if _, err := os.Stat(console); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the console is still there: %v", err)
	}
	if l := dockerLog(dir); !strings.Contains(l, "compose down\nimage inspect --format {{.Id}} stargaite:local\nrmi stargaite:local\n"+
		"image inspect --format {{.Id}} stargaite:local-previous\nrmi stargaite:local-previous\n") {
		t.Errorf("docker log:\n%s", l)
	}
}
