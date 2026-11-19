package guided

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jorlaru/stargaite/internal/admintest"
	"github.com/jorlaru/stargaite/internal/cli"
	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// aliasWorld is the alias and provider tasks' fixture for the guided front:
// the admin API in process through the real CLI, every argv recorded;
// providers mock and mock2 on one mock provider, behind a handler that
// answers the model list (gpt-a and m) and readiness on the data plane's
// port; and a clone whose docker fake answers compose ps.
type aliasWorld struct {
	h     *host.Host
	env   map[string]string
	mock  *mockprovider.Server
	mu    sync.Mutex
	argvs []string
}

func newAliasWorld(t *testing.T) *aliasWorld {
	t.Helper()
	bin := t.TempDir()
	w := &aliasWorld{h: host.New(t.TempDir()), env: admintest.Server(t), mock: mockprovider.New()}
	fake := "#!/bin/sh\ncase \"$*\" in\n'compose ps -q') echo c0ffee ;;\n*) exit 9 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health/ready":
		case "/models":
			_, _ = io.WriteString(rw, `{"data":[{"id":"gpt-a"},{"id":"m"}]}`)
		default:
			w.mock.ServeHTTP(rw, r)
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := os.WriteFile(filepath.Join(w.h.Dir, ".env"), []byte("STARGAITE_PORT="+u.Port()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mock", "mock2"} {
		w.must(t, "providers", "add", name, "--protocol", "openai", "--base-url", srv.URL, "--allow-private-network", "--billing", "none")
	}
	return w
}

// must runs the CLI before the run, unrecorded, failing the test on a
// non-zero exit.
func (w *aliasWorld) must(t *testing.T, args ...string) {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := cli.Main(args, func(k string) string { return w.env[k] }, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("%q: exit %d: %s%s", args, code, out.String(), errOut.String())
	}
}

// proxy is the world's CLI, every argv recorded.
func (w *aliasWorld) proxy() *proxy.Proxy {
	return proxy.New(func(_ context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		w.mu.Lock()
		w.argvs = append(w.argvs, strings.Join(args, " "))
		w.mu.Unlock()
		if stdin == nil {
			stdin = strings.NewReader("")
		}
		return cli.Main(args, func(k string) string { return w.env[k] }, stdin, stdout, stderr), nil
	})
}

func (w *aliasWorld) cli() []string { w.mu.Lock(); defer w.mu.Unlock(); return slices.Clone(w.argvs) }

// run runs the menu's task labelled label through the guided front.
func (w *aliasWorld) run(h *harness, label string, answers map[string]string) <-chan runAdd {
	return runTask(h, label, w.proxy(), w.h, answers)
}

// enter sends Enter.
func (h *harness) enter() { h.p.Send(key(tea.KeyEnter)) }

// choose types s into the shown pick and takes the first match.
func (h *harness) choose(s string) { h.typeText(s); h.enter() }

// toPaid answers Make an alias up to its paid step: alias fb toward mock's
// m, with mock2's m as its fallback when fallback; it returns when the paid
// frame is drawn.
func toPaid(t *testing.T, h *harness, fallback bool) {
	t.Helper()
	h.waitShown("a name for the alias")
	h.choose("fb")
	h.waitShown("Which provider has the model?")
	h.choose("mock")
	f := h.waitShown("Which model of mock?")
	if !strings.Contains(f.Content, "── OpenAI · GPT\n") || !strings.Contains(f.Content, "── Other\n") {
		t.Errorf("the model frame is not grouped:\n%s", f.Content)
	}
	h.choose("m")
	h.waitShown("Fallback 1 of 2")
	if fallback {
		h.choose("mock2")
		h.waitShown("Which model of mock2?")
		h.choose("m")
		h.waitShown("Fallback 2 of 2")
	}
	h.enter() // the cursor is on "no more fallbacks"
	h.waitShown("a default reasoning effort")
	h.enter()
	h.waitShown("paid call(s) in all")
}

// steps is the step bar's current titles, among titles, in the order the
// frames first drew them.
func (h *harness) steps(titles ...string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, f := range h.frames {
		for _, s := range titles {
			if strings.Contains(f.Content, "● "+s) && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// fits fails the test on a frame over 24 lines or 80 cells.
func (h *harness) fits() {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.frames {
		lines := strings.Split(f.Content, "\n")
		if len(lines) > 24 {
			h.t.Fatalf("%d lines:\n%s", len(lines), f.Content)
		}
		for _, l := range lines {
			if ansi.StringWidth(l) > 80 {
				h.t.Fatalf("line %q is %d wide", l, ansi.StringWidth(l))
			}
		}
	}
}

func TestGuidedMakeAlias(t *testing.T) {
	h := start(t)
	w := newAliasWorld(t)
	done := w.run(h, "Make an alias", nil)
	toPaid(t, h, true)
	shown := time.Now()
	h.enter() // at once: ignored
	time.Sleep(300 * time.Millisecond)
	if n := len(w.mock.Calls()); n != 0 {
		t.Fatalf("%d mock calls after an Enter sent at once", n)
	}
	time.Sleep(time.Until(shown.Add(450 * time.Millisecond)))
	h.enter()
	if r := ended(t, done, 10*time.Second); r.err != nil || r.res != "Done: alias fb toward mock/m, mock2/m." {
		t.Fatalf("%+v\n%s", r, h.out.String())
	}
	if n := len(w.mock.Calls()); n != 2 {
		t.Errorf("%d mock calls, want 2", n)
	}
	h.waitOutput("Done: alias fb toward mock/m, mock2/m.")
	h.front.Done()
	h.ended()
	h.fits()
	if bars := h.steps("name", "model", "fallbacks", "settings", "check", "make"); !slices.Equal(bars, []string{"name", "model", "fallbacks", "settings", "check"}) {
		t.Errorf("the step bar went %q", bars)
	}
	if last := h.lastFrame(); last != "" {
		t.Errorf("a frame stayed after Done:\n%s", last)
	}
}

func TestGuidedMakeAliasEscAtPaid(t *testing.T) {
	h := start(t)
	w := newAliasWorld(t)
	done := w.run(h, "Make an alias", nil)
	toPaid(t, h, false)
	h.p.Send(key(tea.KeyEscape))
	if r := ended(t, done, 5*time.Second); !errors.Is(r.err, ui.ErrLeave) {
		t.Fatalf("%+v; want ErrLeave", r)
	}
	h.waitOutput("already changed and kept: target mock/m added")
	if n := len(w.mock.Calls()); n != 0 {
		t.Errorf("%d mock calls, want 0", n)
	}
}

func TestGuidedMakeAliasEscDuringCheck(t *testing.T) {
	h := start(t)
	w := newAliasWorld(t)
	w.mock.Script(mockprovider.Step{Hang: true})
	done := w.run(h, "Make an alias", nil)
	toPaid(t, h, false)
	time.Sleep(450 * time.Millisecond)
	h.enter()
	h.waitShown("checking mock/m…")
	for deadline := time.Now().Add(5 * time.Second); len(w.mock.Calls()) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the check never reached the mock")
		}
	}
	h.p.Send(key(tea.KeyEscape))
	if r := ended(t, done, 12*time.Second); !errors.Is(r.err, ui.ErrLeave) {
		t.Fatalf("%+v; want ErrLeave", r)
	}
	h.waitOutput("the check of mock/m was stopped: the call in flight may still be billed")
	// The mock marks a call cancelled when its handler returns, after the
	// server dropped it: wait for it, as the task's own test does.
	for deadline := time.Now().Add(5 * time.Second); !w.mock.Calls()[0].Cancelled; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the call in flight was never cancelled at the mock")
		}
	}
	if calls := w.mock.Calls(); len(calls) != 1 {
		t.Errorf("mock calls %+v; want one", calls)
	}
}

func TestGuidedMakeAliasKept(t *testing.T) {
	h := start(t)
	w := newAliasWorld(t)
	w.must(t, "targets", "add", "--kind", "chat", "--", "mock/m")
	w.must(t, "models", "add", "--target", "mock/m", "--", "main")
	done := w.run(h, "Make an alias", nil)
	h.waitShown("a name for the alias")
	h.enter()
	if r := ended(t, done, 5*time.Second); r.err != nil || r.res != "alias main exists, toward mock/m; kept: Change an alias changes it" {
		t.Fatalf("%+v", r)
	}
	if got := w.cli(); !slices.Equal(got, []string{"models list --json"}) {
		t.Errorf("CLI calls %q", got)
	}
}

func TestGuidedChangeAliasReorder(t *testing.T) {
	h := start(t)
	w := newAliasWorld(t)
	for _, id := range []string{"mock/m", "mock2/m"} {
		w.must(t, "targets", "add", "--kind", "chat", "--", id)
	}
	w.must(t, "models", "add", "--target", "mock/m", "--target", "mock2/m", "--", "a")
	done := w.run(h, "Change an alias", nil)
	h.waitShown("Which alias?")
	h.enter()
	h.waitShown("What do you want to change?")
	h.choose("reorder")
	h.waitShown("Which target moves?")
	h.choose("mock2")
	h.waitShown("To which place?")
	h.choose("first")
	if r := ended(t, done, 5*time.Second); r.err != nil || r.res != "Done: alias a toward mock2/m, mock/m." {
		t.Fatalf("%+v", r)
	}
	if !slices.Contains(w.cli(), "models set --json --target mock2/m --target mock/m -- a") {
		t.Errorf("CLI calls %q", w.cli())
	}
}

func TestGuidedChangeProviderTimeout(t *testing.T) {
	h := start(t)
	w := newAliasWorld(t)
	w.must(t, "targets", "add", "--kind", "chat", "--", "mock/m")
	w.must(t, "targets", "verify", "--", "mock/m")
	before := len(w.mock.Calls())
	done := w.run(h, "Change a provider", nil)
	h.waitShown("Which provider?")
	h.choose("mock")
	h.waitShown("What do you want to change?")
	h.choose("timeout")
	h.waitShown("the longest one call to mock may take")
	h.choose("9")
	h.waitShown("1 target(s) must be checked again: 1 paid call(s)")
	shown := time.Now()
	h.p.Send(key(tea.KeyDown))
	h.waitShown("> Not now")
	time.Sleep(time.Until(shown.Add(450 * time.Millisecond)))
	h.enter()
	if r := ended(t, done, 5*time.Second); r.err != nil || r.res != "Done: mock, timeout 9 s." {
		t.Fatalf("%+v", r)
	}
	h.waitOutput("1 target(s) left unchecked: mock/m")
	if n := len(w.mock.Calls()) - before; n != 0 {
		t.Errorf("%d mock calls, want 0", n)
	}
}
