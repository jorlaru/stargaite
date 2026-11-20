package guided

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/task"
	"github.com/jorlaru/stargaite/internal/console/ui"
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
'compose run --rm --no-deps -T stargaite restore --yes --json -') /bin/cat >'@/restored'; echo '{"path":"-","bytes":1,"data_dir":"/data","replaced":[]}' ;;
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
	stop, run, up := strings.Index(l, "compose stop\n"), strings.Index(l, "compose run --rm --no-deps -T stargaite restore --yes --json -\n"),
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

// The guided runs of C64b's tasks: Open to the network, Upgrade, and the
// home's Rebuild, at layer 3.

// tlsMachine is Open to the network's world: a clone whose docker fake
// answers the container as running, rootful Docker and the forced recreate,
// logging to bin/log; fakes of hostname (127.0.0.2 127.0.0.3), chown, getent
// and id; and a TLS data plane at .env's port on 127.0.0.1, 127.0.0.2 and
// 127.0.0.3 serving tls/'s pair, logging "<local address> <SNI>".
func tlsMachine(t *testing.T) (h *host.Host, port, bin string, served func() []string) {
	t.Helper()
	h, bin = host.New(t.TempDir()), t.TempDir()
	gid := strconv.Itoa(os.Getegid())
	for name, body := range map[string]string{
		"docker": "echo \"$*\" >>'" + bin + "/log'\ncase \"$*\" in\n'compose ps -q'|'compose ps -a -q') echo c0ffee ;;\n" +
			"'info --format {{.SecurityOptions}}') echo '[name=seccomp]' ;;\n'compose up -d --no-build --force-recreate') ;;\n*) exit 9 ;;\nesac\n",
		"hostname": "[ \"$*\" = -I ] || exit 9\nprintf '127.0.0.2 127.0.0.3 '\n",
		"chown":    ":\n",
		"getent":   "case \"$1\" in passwd) echo 'me:x:1000:" + gid + "::/home/me:/bin/sh';; group) echo 'me:x:" + gid + ":';; esac\n",
		"id":       "[ \"$*\" = -un ] || exit 9\necho me\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	var mu sync.Mutex
	var seen []string
	srv := &http.Server{
		TLSConfig: &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			pair, err := tls.LoadX509KeyPair(filepath.Join(h.Dir, "tls", "cert.pem"), filepath.Join(h.Dir, "tls", "key.pem"))
			return &pair, err
		}},
		Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, r.Context().Value(http.LocalAddrContextKey).(net.Addr).String()+" "+r.TLS.ServerName)
		}),
		ErrorLog: log.New(io.Discard, "", 0),
	}
	port = "0"
	for _, a := range []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"} {
		l, err := net.Listen("tcp", a+":"+port)
		if err != nil {
			t.Fatal(err)
		}
		port = strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
		go func() { _ = srv.ServeTLS(l, "", "") }()
	}
	t.Cleanup(func() { _ = srv.Close() })
	if err := os.WriteFile(filepath.Join(h.Dir, ".env"), []byte("STARGAITE_PORT="+port+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return h, port, bin, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(seen) }
}

// noCLI is a proxy whose every call fails the test.
func noCLI(t *testing.T) *proxy.Proxy {
	return proxy.New(func(_ context.Context, args []string, _ io.Reader, _, _ io.Writer) (int, error) {
		t.Errorf("unexpected CLI call %q", args)
		return 2, nil
	})
}

// TestGuidedOpenToNetwork: the address typed after this host's addresses
// are said, self-signed picked, the confirm naming the addresses, then each
// address checked over HTTPS; no pick's frame is left in what was said.
func TestGuidedOpenToNetwork(t *testing.T) {
	hst, port, bin, served := tlsMachine(t)
	h := start(t)
	done := runTask(h, "Open to the network", noCLI(t), hst, nil)
	h.waitOutput("this host's addresses: 127.0.0.2 127.0.0.3")
	h.waitShown("the addresses to publish on beside 127.0.0.1")
	h.typeText("127.0.0.2")
	h.enter()
	f := h.waitShown("the certificate: own (yours, with its key) or self-signed (made here)")
	if !strings.Contains(f.Content, "your certificate and its key") || !strings.Contains(f.Content, "a self-signed one, made here") {
		t.Errorf("the certificate pick:\n%s", f.Content)
	}
	h.choose("self-signed")
	h.waitOutput("addresses=127.0.0.1 127.0.0.2, and the container is then recreated to read it")
	h.yes("● files and override")
	r := ended(t, done, 15*time.Second)
	if r.err != nil || r.res != "Done: the data plane answers over HTTPS on 127.0.0.1 127.0.0.2." {
		t.Fatalf("%+v\n%s", r, h.said())
	}
	h.waitOutput(r.res)
	h.front.Done()
	h.ended()
	h.fits()
	said := h.said()
	if !strings.Contains(said, "https on 127.0.0.2:"+port+": ready\n") || strings.Contains(said, "a self-signed one, made here") ||
		strings.Contains(said, "your certificate and its key") {
		t.Errorf("said:\n%s", said)
	}
	override, err := os.ReadFile(filepath.Join(hst.Dir, "compose.override.yaml"))
	if err != nil || !strings.Contains(string(override), "# setup: addresses=127.0.0.1 127.0.0.2\n") {
		t.Errorf("override %q, %v", override, err)
	}
	if l := dockerLog(bin); strings.Count(l, "compose up -d --no-build --force-recreate\n") != 1 ||
		!slices.Contains(served(), "127.0.0.2:"+port+" localhost") {
		t.Errorf("docker log %q, served %q", l, served())
	}
}

// upMachine is Upgrade's world: a clone (real git, through a wrapper that
// logs each call) of a bare upstream, tagged v0.6.0-fake, with ahead
// commits "change NN" pushed to it from another clone; a docker fake that
// keeps the running version in bin/running (compose build stores
// STARGAITE_VERSION, compose up makes it the running one) and exports the
// console into bin/ of the clone; a canned CLI answering version, export,
// validate, --help (help) and backup -; and a data plane answering
// readiness. docker, git and the CLI log to bin/log, in order.
type upMachine struct {
	t                      *testing.T
	h                      *host.Host
	bin, real, other, help string
}

func newUpMachine(t *testing.T, ahead int) *upMachine {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git in this image")
	}
	root := t.TempDir()
	w := &upMachine{t: t, h: host.New(filepath.Join(root, "clone")), bin: t.TempDir(), real: real, other: filepath.Join(root, "other"),
		help: "usage: stargaite <command>\n  backup file\n  restore file\n"}
	up := filepath.Join(root, "up.git")
	w.git(root, "init", "-q", "--bare", "-b", "main", up)
	w.git(root, "init", "-q", "-b", "main", w.other)
	w.commit("base")
	w.git(w.other, "tag", "v0.6.0-fake")
	w.git(w.other, "remote", "add", "origin", up)
	w.git(w.other, "push", "-q", "--tags", "-u", "origin", "main")
	w.git(root, "clone", "-q", up, w.h.Dir)
	for i := range ahead {
		w.commit(fmt.Sprintf("change %02d", i))
	}
	if ahead > 0 {
		w.git(w.other, "push", "-q", "origin", "main")
	}
	log := filepath.Join(w.bin, "log")
	files := map[string]string{
		"git": "echo \"git $*\" >>'" + log + "'\nexec '" + real + "' \"$@\"\n",
		"docker": "echo \"docker $*\" >>'" + log + "'\ncase \"$*\" in\n" +
			"'compose ps -q'|'compose ps -a -q'|'compose ps -q stargaite') echo c0ffee ;;\n" +
			"'inspect --format {{.Image}} {{.Config.Image}} c0ffee') echo 'sha256:0123abcd stargaite:local' ;;\n" +
			"'tag sha256:0123abcd stargaite:local-previous') ;;\n" +
			"'compose build') echo \"$STARGAITE_VERSION\" >'" + w.bin + "/built' ;;\n" +
			"'compose up -d --no-build') /bin/cp '" + w.bin + "/built' '" + w.bin + "/running' ;;\n" +
			"'compose logs --no-log-prefix --tail 100 stargaite') echo \"{\\\"msg\\\":\\\"ready\\\",\\\"version\\\":\\\"$(/bin/cat '" + w.bin + "/running')\\\"}\" ;;\n" +
			"'compose logs --tail 30 stargaite') ;;\n" +
			"'build --quiet --build-arg VERSION --target console --output type=local,dest=bin .') ;;\n*) exit 9 ;;\nesac\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(w.bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(w.bin, "running"), []byte("v0.6.0-fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", w.bin+":"+filepath.Dir(real))
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	if err := os.WriteFile(filepath.Join(w.h.Dir, ".env"), []byte("STARGAITE_PORT="+srv.URL[strings.LastIndex(srv.URL, ":")+1:]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return w
}

// git runs the real git in dir, unlogged: the test's own setup.
func (w *upMachine) git(dir string, args ...string) string {
	w.t.Helper()
	cmd := exec.Command(w.real, append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit commits a file named msg in the other clone.
func (w *upMachine) commit(msg string) {
	w.t.Helper()
	if err := os.WriteFile(filepath.Join(w.other, msg), []byte(msg+"\n"), 0o644); err != nil {
		w.t.Fatal(err)
	}
	w.git(w.other, "add", msg)
	w.git(w.other, "commit", "-q", "-m", msg)
}

// log is the world's log of docker, git and the CLI.
func (w *upMachine) log() string { b, _ := os.ReadFile(filepath.Join(w.bin, "log")); return string(b) }

// changed reports whether the log holds a tag, a merge, a build, a backup
// or a start.
func (w *upMachine) changed() bool {
	for _, p := range []string{"docker tag ", "git merge ", "docker compose build", "docker compose up ", "cli backup "} {
		if strings.Contains(w.log(), p) {
			return true
		}
	}
	return false
}

func (w *upMachine) logLine(l string) {
	f, err := os.OpenFile(filepath.Join(w.bin, "log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		_, _ = io.WriteString(f, l+"\n")
		err = f.Close()
	}
	if err != nil {
		w.t.Error(err)
	}
}

// proxy is the canned CLI, each call logged as "cli <args>".
func (w *upMachine) proxy() *proxy.Proxy {
	return proxy.New(func(_ context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) (int, error) {
		key := strings.Join(args, " ")
		w.logLine("cli " + key)
		running, _ := os.ReadFile(filepath.Join(w.bin, "running"))
		switch key {
		case "version --json":
			_, _ = io.WriteString(stdout, `{"version":"`+strings.TrimSpace(string(running))+`"}`)
		case "--help":
			_, _ = io.WriteString(stdout, w.help)
		case "export --json":
			_, _ = io.WriteString(stdout, `{"version":1,"providers":{"p":{}},"models":{"main":{"targets":["p/m"]}},"keys":{"k":{}}}`)
		case "validate --json":
			_, _ = io.WriteString(stdout, `{"findings":[]}`)
		case "backup - --json":
			_, _ = io.WriteString(stdout, "SQLite format 3\x00 the store")
			_, _ = io.WriteString(stderr, `{"bytes":26,"path":"-"}`+"\n")
		default:
			w.t.Errorf("unexpected CLI call %q", key)
			return 2, nil
		}
		return 0, nil
	})
}

// TestGuidedUpgrade is U6 and U7 at a terminal: the new commits each said
// on its own line, twenty at most; No changes nothing; a CLI that predates
// backup - takes the copy made by hand, its path in the way back as a
// shell word.
func TestGuidedUpgrade(t *testing.T) {
	w := newUpMachine(t, 26)
	head := w.git(w.h.Dir, "rev-parse", "HEAD")
	h := start(t)
	done := runTask(h, "Upgrade", w.proxy(), w.h, nil)
	f := h.waitShown("continue?")
	if !strings.Contains(f.Content, "> No") {
		t.Errorf("confirm frame:\n%s", f.Content)
	}
	h.enter()
	r := ended(t, done, 15*time.Second)
	if r.err == nil || r.err.Error() != "stopped; nothing more was changed" {
		t.Fatalf("%+v\n%s", r, h.said())
	}
	h.front.Done()
	h.ended()
	h.fits()
	lines := strings.Split(h.said(), "\n")
	if !slices.Contains(lines, "26 new commit(s) on origin/main:") || slices.ContainsFunc(lines, func(l string) bool { return strings.HasSuffix(l, " change 05") }) {
		t.Errorf("said:\n%s", h.said())
	}
	for i := 6; i <= 25; i++ {
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasSuffix(l, fmt.Sprintf(" change %02d", i)) }) {
			t.Errorf("change %02d not said on its own line:\n%s", i, h.said())
		}
	}
	if w.git(w.h.Dir, "rev-parse", "HEAD") != head || w.changed() {
		t.Fatalf("No changed something: HEAD %s, log:\n%s", w.git(w.h.Dir, "rev-parse", "HEAD"), w.log())
	}

	w = newUpMachine(t, 1)
	w.help = "usage: stargaite <command>\n  backup file\n"
	if err := os.MkdirAll(filepath.Join(w.h.Dir, "my backups"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.h.Dir, "my backups", "pre.db"), []byte("SQLite format 3\x00 by hand"), 0o600); err != nil {
		t.Fatal(err)
	}
	h = start(t)
	done = runTask(h, "Upgrade", w.proxy(), w.h, nil)
	h.waitShown("the copy made by hand, on this host, such as backups/pre-upgrade.db")
	h.typeText("my backups/pre.db")
	h.enter()
	h.yes("● what changes")
	r = ended(t, done, 15*time.Second)
	if !errors.Is(r.err, task.ErrReexec) || !strings.HasPrefix(r.res, "upgraded: ") {
		t.Fatalf("%+v\n%s", r, h.said())
	}
	h.front.Done()
	h.ended()
	h.fits()
	if strings.Contains(w.log(), "cli backup ") || !strings.Contains(h.said(), "restore --yes - < 'my backups/pre.db'\n") {
		t.Errorf("log:\n%s\nsaid:\n%s", w.log(), h.said())
	}
}

// TestGuidedRebuildFromHome: the home suggests Rebuild when the proxy runs
// another version than the clone's; picking it rebuilds with no rebuild
// question, then exports the new console and ends the home with ErrReexec.
func TestGuidedRebuildFromHome(t *testing.T) {
	w := newUpMachine(t, 0)
	if err := os.WriteFile(filepath.Join(w.bin, "running"), []byte("v-old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := start(t)
	u := ui.New(h.front, nil)
	done := make(chan error, 1)
	go func() { done <- task.Home(h.ctx, u, w.proxy(), w.h, 15*time.Second) }()
	f := h.waitShown("What do you want to do?")
	if !strings.Contains(f.Content, "── Suggested\n") || !strings.Contains(f.Content, "  Rebuild to this clone's version\n── Providers\n") {
		t.Fatalf("home frame:\n%s", f.Content)
	}
	h.choose("Rebuild")
	h.waitOutput("the proxy runs v-old; this clone is at v0.6.0-fake")
	h.yes("● what changes")
	select {
	case err := <-done:
		if !errors.Is(err, task.ErrReexec) {
			t.Fatalf("Home: %v; want ErrReexec\n%s", err, h.said())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("Home did not end\n%s", h.said())
	}
	h.front.Done()
	h.ended()
	h.fits()
	l := w.log()
	if !strings.Contains(h.said(), "rebuilt: ") || strings.Contains(l, "git fetch") || strings.Contains(l, "git merge") ||
		!strings.Contains(l, "docker build --quiet --build-arg VERSION --target console --output type=local,dest=bin .") {
		t.Errorf("said:\n%s\nlog:\n%s", h.said(), l)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, fr := range h.frames {
		if strings.Contains(fr.Content, "rebuild") {
			t.Errorf("a frame asks about rebuild:\n%s", fr.Content)
		}
	}
}
