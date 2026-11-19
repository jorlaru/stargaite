package guided

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jorlaru/stargaite/internal/admintest"
	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/task"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// newKeyWorld is aliasWorld on admintest.Plane: .env names the data plane's
// port, so readiness and every try reach it; providers mock and mock2 on one
// mock, without a credential; alias a toward targets, when given. It returns
// the world and the data plane's port.
func newKeyWorld(t *testing.T, targets ...string) (*aliasWorld, string) {
	t.Helper()
	env, port := admintest.Plane(t)
	bin := t.TempDir()
	w := &aliasWorld{h: host.New(t.TempDir()), env: env, mock: mockprovider.New()}
	fake := "#!/bin/sh\ncase \"$*\" in\n'compose ps -q') echo c0ffee ;;\n'compose logs --tail 30 stargaite') echo 'a log line' ;;\n*) exit 9 ;;\nesac\n"
	if err := errors.Join(os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0o755),
		os.WriteFile(filepath.Join(w.h.Dir, ".env"), []byte("STARGAITE_PORT="+port+"\n"), 0o600)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	srv := httptest.NewServer(w.mock)
	t.Cleanup(srv.Close)
	for _, p := range []string{"mock", "mock2"} {
		w.must(t, "providers", "add", p, "--protocol", "openai", "--base-url", srv.URL, "--allow-private-network", "--billing", "none")
	}
	if len(targets) > 0 {
		args := []string{"models", "add", "a"}
		for _, id := range targets {
			w.must(t, "targets", "add", "--kind", "chat", "--", id)
			args = append(args, "--target", id)
		}
		w.must(t, args...)
	}
	return w, port
}

// secretRE matches a client key's secret.
var secretRE = regexp.MustCompile(`sk-stargaite-[a-z2-7]{12}-[a-z2-7]{52}`)

// waitReveal waits for the reveal's frame and returns its secret.
func (h *harness) waitReveal() string {
	h.t.Helper()
	f := h.waitFrame(func(v tea.View) bool { return v.AltScreen && strings.Contains(v.Content, "enter: I have stored it") })
	return secretRE.FindString(f.Content)
}

// afterWait sleeps until 450 ms after shown: past a reveal's or a paid step's
// guard on Enter.
func afterWait(shown time.Time) { time.Sleep(time.Until(shown.Add(450 * time.Millisecond))) }

// toTry answers Give an app or agent its key up to its paid frame: the first
// alias, key app, the default rpm, the reveal stored once its guard has
// passed. It returns the secret and when the paid frame was seen.
func toTry(t *testing.T, h *harness) (string, time.Time) {
	t.Helper()
	h.waitShown("Which aliases may it call?")
	h.enter() // the cursor is on a
	h.waitShown("a name for the key")
	h.choose("app")
	h.waitShown("the requests a minute the key may send")
	h.enter()
	secret := h.waitReveal()
	afterWait(time.Now())
	h.enter()
	h.waitShown("> Try it")
	return secret, time.Now()
}

// noCalls fails the test when the mock has had a call.
func noCalls(t *testing.T, w *aliasWorld) {
	t.Helper()
	if n := len(w.mock.Calls()); n != 0 {
		t.Fatalf("%d mock calls, want 0", n)
	}
}

// onAltScreenOnly reports whether every occurrence of s in out lies after an
// alternate-screen enter and before the next leave.
func onAltScreenOnly(out, s string) bool {
	for i := strings.Index(out, s); i >= 0; {
		on, off := strings.LastIndex(out[:i], "\x1b[?1049h"), strings.LastIndex(out[:i], "\x1b[?1049l")
		if on < 0 || off > on {
			return false
		}
		next := strings.Index(out[i+len(s):], s)
		if next < 0 {
			break
		}
		i += len(s) + next
	}
	return true
}

func TestRevealWaitsForEnter(t *testing.T) {
	if _, a := (&reveal{q: revealQuestion(), created: time.Now()}).Update(key(tea.KeyEnter)); a != nil {
		t.Fatalf("an Enter as the reveal is made answered %+v", *a)
	}
	if _, a := (&reveal{q: revealQuestion(), created: time.Now().Add(-time.Second)}).Update(key(tea.KeyEnter)); a == nil || a.Err != nil {
		t.Fatalf("an Enter a second after the reveal is made answered %+v", a)
	}
}

func TestGuidedGiveKey(t *testing.T) {
	h := start(t)
	w, _ := newKeyWorld(t, "mock/m")
	done := w.run(h, "Give an app or agent its key", nil)
	h.waitShown("Which aliases may it call?")
	h.enter()
	h.waitShown("a name for the key")
	h.choose("app")
	h.waitShown("the requests a minute the key may send")
	h.enter()
	secret := h.waitReveal()
	shown := time.Now()
	h.enter() // as the reveal appears: stores nothing
	time.Sleep(300 * time.Millisecond)
	if !strings.Contains(h.lastFrame(), "enter: I have stored it") {
		t.Fatalf("an Enter sent as the reveal appeared dismissed it:\n%s", h.lastFrame())
	}
	afterWait(shown)
	h.enter()
	f := h.waitShown("> Try it")
	paid := time.Now()
	if !strings.Contains(f.Content, "try it: up to 1 paid call(s)") {
		t.Fatalf("paid frame:\n%s", f.Content)
	}
	h.enter() // at once: ignored
	time.Sleep(300 * time.Millisecond)
	noCalls(t, w)
	afterWait(paid)
	h.enter()
	if r := ended(t, done, 10*time.Second); r.err != nil || r.res != "Done: key app, which may call a." {
		t.Fatalf("%+v\n%s", r, h.out.String())
	}
	if n := len(w.mock.Calls()); n != 1 {
		t.Errorf("%d mock calls, want 1", n)
	}
	h.waitOutput("answer: mock reply")
	h.waitOutput("Done: key app, which may call a.")
	h.front.Done()
	h.ended()
	h.fits()
	if bars := h.steps("aliases", "name", "limits", "key", "try"); !slices.Equal(bars, []string{"aliases", "name", "limits", "key", "try"}) {
		t.Errorf("the step bar went %q", bars)
	}
	if secret == "" || !onAltScreenOnly(h.out.String(), secret) {
		t.Errorf("the secret %q is in the output off the alternate screen:\n%q", secret, h.out.String())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.frames {
		if strings.Contains(f.Content, secret) && (!f.AltScreen || !strings.Contains(f.Content, "enter: I have stored it")) {
			t.Fatalf("a frame but the reveal's holds the secret:\n%s", f.Content)
		}
	}
}

// Two Enters sent together at the reveal: the first stores, the second is
// dropped while no piece is up or by the paid step's guard, which then still
// waits for its own Enter.
func TestGuidedGiveKeyDoubleEnter(t *testing.T) {
	h := start(t)
	w, _ := newKeyWorld(t, "mock/m")
	done := w.run(h, "Give an app or agent its key", nil)
	h.waitShown("Which aliases may it call?")
	h.enter()
	h.waitShown("a name for the key")
	h.choose("app")
	h.waitShown("the requests a minute the key may send")
	h.enter()
	h.waitReveal()
	afterWait(time.Now())
	h.enter()
	h.enter()
	h.waitShown("> Try it")
	time.Sleep(600 * time.Millisecond)
	noCalls(t, w)
	if !strings.Contains(h.lastFrame(), "> Try it") {
		t.Fatalf("the paid step was answered:\n%s", h.lastFrame())
	}
	h.p.Send(key(tea.KeyEscape))
	if r := ended(t, done, 5*time.Second); !errors.Is(r.err, ui.ErrLeave) {
		t.Fatalf("%+v; want ErrLeave", r)
	}
	noCalls(t, w)
}

func TestGuidedGiveKeyEscAtTry(t *testing.T) {
	h := start(t)
	w, _ := newKeyWorld(t, "mock/m")
	done := w.run(h, "Give an app or agent its key", nil)
	toTry(t, h)
	h.p.Send(key(tea.KeyEscape))
	if r := ended(t, done, 5*time.Second); !errors.Is(r.err, ui.ErrLeave) {
		t.Fatalf("%+v; want ErrLeave", r)
	}
	h.waitOutput("already changed and kept: key app made")
	noCalls(t, w)
}

func TestGuidedGiveKeyEscDuringTry(t *testing.T) {
	h := start(t)
	w, _ := newKeyWorld(t, "mock/m", "mock2/m")
	w.mock.Script(mockprovider.Step{Hang: true})
	done := w.run(h, "Give an app or agent its key", nil)
	_, paid := toTry(t, h)
	afterWait(paid)
	h.enter()
	h.waitShown("trying alias a…")
	for deadline := time.Now().Add(5 * time.Second); len(w.mock.Calls()) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the try never reached the mock")
		}
	}
	h.p.Send(key(tea.KeyEscape))
	if r := ended(t, done, 12*time.Second); !errors.Is(r.err, ui.ErrLeave) {
		t.Fatalf("%+v; want ErrLeave", r)
	}
	h.waitOutput("the try through alias a was stopped: the call in flight may still be billed")
	// The mock marks a call cancelled when its handler returns.
	for deadline := time.Now().Add(5 * time.Second); !w.mock.Calls()[0].Cancelled; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the call in flight was never cancelled at the mock")
		}
	}
	time.Sleep(500 * time.Millisecond)
	if n := len(w.mock.Calls()); n != 1 {
		t.Errorf("%d mock calls; the second target was called", n)
	}
}

func TestGuidedChangeKeyRotateNotNow(t *testing.T) {
	h := start(t)
	w, port := newKeyWorld(t, "mock/m")
	w.must(t, "keys", "create", "--model", "a", "--", "k")
	done := w.run(h, "Change a key", nil)
	h.waitShown("Which key?")
	h.enter()
	h.waitShown("What do you want to change?")
	h.choose("rotate")
	h.waitShown("What happens to the secret in use?")
	h.choose("ends now")
	h.waitShown("continue?")
	h.p.Send(key(tea.KeyUp))
	h.waitShown("> Yes")
	h.enter()
	h.waitReveal()
	afterWait(time.Now())
	h.enter()
	h.waitShown("> Try it")
	paid := time.Now()
	h.p.Send(key(tea.KeyDown))
	h.waitShown("> Not now")
	afterWait(paid)
	h.enter()
	if r := ended(t, done, 5*time.Second); r.err != nil || r.res != "Done: key k rotated; not tried." {
		t.Fatalf("%+v\n%s", r, h.out.String())
	}
	base := "http://127.0.0.1:" + port + "/v1"
	h.waitOutput("Point an OpenAI-compatible client at base URL " + base + ", with model a and this key, such as:")
	h.waitOutput("  curl " + base + `/chat/completions -H "Authorization: Bearer $STARGAITE_KEY" -H "Content-Type: application/json" -d '{"model":"a","max_tokens":256,"messages":[{"role":"user","content":"Hello"}]}'`)
	noCalls(t, w)
}

// home runs task.Home through the guided front on its own goroutine and
// sends its error.
func (w *aliasWorld) home(h *harness) <-chan error {
	u := ui.New(h.front, nil)
	done := make(chan error, 1)
	go func() { done <- task.Home(h.ctx, u, w.proxy(), w.h, 15*time.Second) }()
	return done
}

// waitCount waits until the output holds s n times.
func (h *harness) waitCount(s string, n int) {
	h.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); strings.Count(h.out.String(), s) < n; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			h.t.Fatalf("output never held %q %d times:\n%q", s, n, h.out.String())
		}
	}
}

// quitHome presses Ctrl-C at the home and waits for Home's ErrQuit.
func quitHome(t *testing.T, h *harness, done <-chan error) {
	t.Helper()
	h.p.Send(ctrlC)
	select {
	case err := <-done:
		if !errors.Is(err, ui.ErrQuit) {
			t.Fatalf("Home: %v; want ErrQuit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Home did not end")
	}
}

func TestGuidedHome(t *testing.T) {
	h := start(t)
	w, port := newKeyWorld(t, "mock/m")
	done := w.home(h)
	f := h.waitShown("What do you want to do?")
	h.waitOutput("http://127.0.0.1:" + port + "/v1: ready · proxy ")
	if lines := strings.Split(f.Content, "\n"); len(lines) < 6 || lines[4] != "── Suggested" || lines[5] != "> Give an app or agent its key: none exists yet" {
		t.Fatalf("home frame:\n%s", f.Content)
	}
	h.typeText("fallback")
	h.waitFrame(func(v tea.View) bool {
		return strings.Contains(v.Content, "> Give alias a a fallback: it has one model") &&
			!strings.Contains(v.Content, "Give an app or agent its key") && !strings.Contains(v.Content, "Change an alias")
	})
	h.enter()
	h.waitShown("A fallback for a: from which provider?")
	h.p.Send(key(tea.KeyEscape))
	h.waitOutput("Left Change an alias.")
	h.waitCount(": ready · proxy ", 2)
	h.waitShown("What do you want to do?")
	quitHome(t, h, done)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.frames {
		if strings.Contains(f.Content, "Which alias?") || strings.Contains(f.Content, "What do you want to change?") {
			t.Fatalf("the suggestion's presets were asked:\n%s", f.Content)
		}
	}
}

func TestGuidedHomeNotReady(t *testing.T) {
	h := start(t)
	w, _ := newKeyWorld(t, "mock/m")
	srv := httptest.NewServer(nil)
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
	srv.Close()
	if err := os.WriteFile(filepath.Join(w.h.Dir, ".env"), []byte("STARGAITE_PORT="+port+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := w.home(h)
	f := h.waitShown("What do you want to do?")
	h.waitOutput("http://127.0.0.1:" + port + "/v1: not ready · clone dev")
	if strings.Contains(f.Content, "── Suggested") {
		t.Fatalf("home frame:\n%s", f.Content)
	}
	quitHome(t, h, done)
	if got := w.cli(); len(got) != 0 {
		t.Errorf("CLI calls %q", got)
	}
}
