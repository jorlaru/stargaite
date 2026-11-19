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
	"github.com/jorlaru/stargaite/internal/console/task"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// addWorld is Add providers' fixture for the guided front: the admin API in
// process through the real CLI, a clone whose docker fake answers compose
// ps, up and logs, and a model list on 127.0.0.1 that accepts the key good,
// counts its requests and answers readiness on the data plane's port.
type addWorld struct {
	h        *host.Host
	px       *proxy.Proxy
	port     string
	log      string
	mu       sync.Mutex
	requests int
}

// newAddWorld builds the fixture; up is the shell of the fake's compose up.
func newAddWorld(t *testing.T, up string) *addWorld {
	t.Helper()
	bin := t.TempDir()
	w := &addWorld{h: host.New(t.TempDir()), log: filepath.Join(bin, "log")}
	fake := "#!/bin/sh\necho \"$*\" >>'" + w.log + "'\ncase \"$*\" in\n'compose ps -q') echo c0ffee ;;\n" +
		"'compose up -d --no-build') " + up + " ;;\n'compose logs --tail 30 stargaite') ;;\n*) exit 9 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			return
		}
		w.mu.Lock()
		w.requests++
		w.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer good" {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(rw, `{"data":[{"id":"m1"}]}`)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	w.port = u.Port()
	if err := os.WriteFile(filepath.Join(w.h.Dir, ".env"), []byte("STARGAITE_PORT="+w.port+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := admintest.Server(t)
	w.px = proxy.New(func(_ context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if stdin == nil {
			stdin = strings.NewReader("")
		}
		return cli.Main(args, func(k string) string { return env[k] }, stdin, stdout, stderr), nil
	})
	return w
}

func (w *addWorld) count() int { w.mu.Lock(); defer w.mu.Unlock(); return w.requests }

func (w *addWorld) env(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(w.h.Dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// add runs Add providers through the guided front, with the steps of its
// entry in the menu, on its own goroutine, and sends its result and error;
// the result is also said, as the console says it.
func (w *addWorld) add(h *harness, answers map[string]string) <-chan runAdd {
	var entry task.Entry
	for _, e := range task.Menu() {
		if e.Sub == "provider" {
			entry = e
		}
	}
	u := ui.New(h.front, answers)
	done := make(chan runAdd, 1)
	go func() {
		res, err := u.Run(h.ctx, entry.Steps, func(ctx context.Context) (string, error) { return entry.Run(ctx, u, w.px, w.h) })
		if err == nil {
			err = u.Say(h.ctx, res)
		}
		done <- runAdd{res, err}
	}()
	return done
}

type runAdd struct {
	res string
	err error
}

func ended(t *testing.T, done <-chan runAdd, within time.Duration) runAdd {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(within):
		t.Fatalf("Add providers did not end within %s", within)
	}
	return runAdd{}
}

// typeText sends each rune of s as a key.
func (h *harness) typeText(s string) {
	for _, r := range s {
		h.p.Send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// toKey answers the questions before acme's key: another service, named
// acme, on openai, at the model list, with ANTHROPIC_API_KEY.
func (w *addWorld) toKey(h *harness) {
	h.waitShown("Which providers do you want to add?")
	h.typeText("another")
	h.waitShown("> [ ] another OpenAI")
	h.p.Send(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	h.waitShown("> [x] another OpenAI")
	h.p.Send(key(tea.KeyEnter))
	h.waitShown("a name for the service")
	h.typeText("acme")
	h.p.Send(key(tea.KeyEnter))
	h.waitShown("its protocol")
	h.p.Send(key(tea.KeyEnter))
	h.waitShown("its base URL")
	h.typeText("http://127.0.0.1:" + w.port + "/v1")
	h.p.Send(key(tea.KeyEnter))
	h.waitShown("the variable for its credential")
	h.typeText("ANTHROPIC_API_KEY")
	h.p.Send(key(tea.KeyEnter))
	h.waitFrame(func(v tea.View) bool {
		return strings.Contains(v.Content, "● key") && strings.Contains(v.Content, "typed hidden")
	})
}

func TestGuidedAddProviders(t *testing.T) {
	h := start(t)
	w := newAddWorld(t, "/bin/sleep 0.3") // long enough for the work piece to be drawn
	done := w.add(h, nil)
	w.toKey(h)
	h.mu.Lock()
	pasteAt := len(h.frames)
	h.mu.Unlock()
	h.p.Send(tea.PasteMsg{Content: "good"})
	h.p.Send(key(tea.KeyEnter))
	h.waitShown("continue?")
	out := h.waitOutput("the rate-limit windows restart")
	if !strings.Contains(out, "writing ANTHROPIC_API_KEY to .env") {
		t.Fatalf("the write was not said before its confirm:\n%q", out)
	}
	h.p.Send(key(tea.KeyUp))
	h.p.Send(key(tea.KeyEnter))
	h.waitShown("recreating the container and waiting until it is ready…")
	if r := ended(t, done, 10*time.Second); r.err != nil || r.res != "Done: acme added, key checked." {
		t.Fatalf("%+v", r)
	}
	out = h.waitOutput("Done: acme added, key checked.")
	if !strings.Contains(out, "acme: key checked") {
		t.Errorf("output:\n%q", out)
	}
	if !strings.Contains(w.env(t), "\nANTHROPIC_API_KEY='good'\n") || w.count() != 1 {
		t.Errorf(".env %q, %d requests", w.env(t), w.count())
	}
	h.front.Done()
	h.ended()
	h.mu.Lock()
	defer h.mu.Unlock()
	var bars []string
	for i, f := range h.frames {
		lines := strings.Split(f.Content, "\n")
		if len(lines) > 24 {
			t.Fatalf("%d lines:\n%s", len(lines), f.Content)
		}
		for _, l := range lines {
			if ansi.StringWidth(l) > 80 {
				t.Fatalf("line %q is %d wide", l, ansi.StringWidth(l))
			}
		}
		if i >= pasteAt && strings.Contains(f.Content, "good") {
			t.Fatalf("a frame holds the key:\n%s", f.Content)
		}
		for _, s := range []string{"which", "add", "key", "write", "recreate"} {
			if strings.Contains(f.Content, "● "+s) && !slices.Contains(bars, s) {
				bars = append(bars, s)
			}
		}
	}
	if !slices.Equal(bars, []string{"which", "add", "key", "write", "recreate"}) {
		t.Errorf("the step bar went %q", bars)
	}
}

func TestGuidedAddProvidersEscAtKey(t *testing.T) {
	h := start(t)
	w := newAddWorld(t, ":")
	env := w.env(t)
	done := w.add(h, nil)
	w.toKey(h)
	h.p.Send(key(tea.KeyEscape))
	if r := ended(t, done, 5*time.Second); !errors.Is(r.err, ui.ErrLeave) {
		t.Fatalf("%+v; want ErrLeave", r)
	}
	if w.env(t) != env || w.count() != 0 {
		t.Errorf(".env %q, %d requests", w.env(t), w.count())
	}
}

func TestGuidedAddProvidersEscDuringRecreate(t *testing.T) {
	h := start(t)
	logs := t.TempDir()
	log, started := filepath.Join(logs, "log"), filepath.Join(logs, "started")
	w := newAddWorld(t, "trap 'echo INT >> "+log+"; exit 130' INT; echo > "+started+"; /bin/sleep 60")
	done := w.add(h, map[string]string{"provider": "ollama", "yes": "yes"})
	h.waitShown("recreating the container and waiting until it is ready…")
	// The work piece is drawn before compose up starts: Esc once its trap is set.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("compose up never started")
		}
	}
	h.p.Send(key(tea.KeyEscape))
	if r := ended(t, done, 12*time.Second); !errors.Is(r.err, ui.ErrLeave) {
		t.Fatalf("%+v; want ErrLeave", r)
	}
	if b, _ := os.ReadFile(log); !bytes.Equal(b, []byte("INT\n")) {
		t.Fatalf("the fake's log: %q", b)
	}
}
