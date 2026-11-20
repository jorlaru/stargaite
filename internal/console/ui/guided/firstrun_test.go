package guided

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

// resolving is a variable admintest's container environment holds, so a
// provider whose credential names it resolves.
const resolving = "MINIMAX_PLAN_API_KEY"

// eight is the first run's bar.
var eight = []string{"machine", "provider", "alias", "model", "fallbacks", "check", "key", "first answer"}

// slowReady puts a front before the data plane at port that answers each
// readiness check 0.3 s late, so the work frame of every readiness wait is
// drawn; .env names the front.
func slowReady(t *testing.T, w *aliasWorld, port string) {
	t.Helper()
	plane, _ := url.Parse("http://127.0.0.1:" + port)
	rp := httputil.NewSingleHostReverseProxy(plane)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health/ready" {
			time.Sleep(300 * time.Millisecond)
		}
		rp.ServeHTTP(rw, r)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := os.WriteFile(filepath.Join(w.h.Dir, ".env"), []byte("STARGAITE_PORT="+u.Port()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// barPath is the step bar's current title, among titles, of each frame that
// draws one, in order, a title repeated in a row kept once: a bar that went
// back shows a title twice.
func (h *harness) barPath(titles []string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, f := range h.frames {
		for _, s := range titles {
			if strings.Contains(f.Content, "● "+s) && (len(out) == 0 || out[len(out)-1] != s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// count is how many of argvs equal want.
func count(argvs []string, want string) int {
	n := 0
	for _, a := range argvs {
		if a == want {
			n++
		}
	}
	return n
}

// TestGuidedFirstRun: from an empty store, every stage asked at the
// terminal, under the first run's bar (successor of setup_test.go's
// TestSetupPrompts install row at layer 3).
func TestGuidedFirstRun(t *testing.T) {
	const providerKey = "sk-provider-pasted-7f3a"
	h := start(t)
	w, port, srvURL := planeWorld(t)
	slowReady(t, w, port)
	done := w.run(h, "First run", nil)
	f := h.waitShown("Which providers do you want to add?")
	for _, s := range []string{"openai", "dashscope, asks for its URL", "deepseek, untested"} {
		if !strings.Contains(f.Content, s) {
			t.Errorf("the provider pick lacks %q:\n%s", s, f.Content)
		}
	}
	h.typeText("another") // the pick is windowed: its last option is shown once filtered
	h.waitShown("> [ ] another OpenAI- or Anthropic-compatible service")
	h.p.Send(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	h.waitShown("> [x] another OpenAI")
	h.enter()
	h.waitShown("a name for the service")
	h.choose("acme")
	h.waitShown("its protocol")
	h.enter()
	h.waitShown("its base URL")
	h.choose(srvURL)
	h.waitShown("the variable for its credential")
	h.choose(resolving)
	h.waitShown("typed hidden")
	h.p.Send(tea.PasteMsg{Content: providerKey})
	h.enter()
	h.waitShown("continue?")
	h.p.Send(key(tea.KeyUp))
	h.waitShown("> Yes")
	h.enter()

	h.waitShown("a name for the alias your clients call [main]")
	h.enter()
	h.waitShown("Which provider has the model?")
	h.choose("acme")
	all := h.waitShown("Which model of acme?")
	h.typeText("pt-")
	if filtered := h.waitShown("filter: pt-"); strings.Count(filtered.Content, "\n") >= strings.Count(all.Content, "\n") {
		t.Errorf("the filter left every row:\n%s", filtered.Content)
	}
	h.enter()
	h.waitShown("Fallback 1 of 2")
	h.enter() // the cursor is on "no more fallbacks"
	h.waitShown("a default reasoning effort")
	h.enter()
	h.waitShown("1 paid call(s) in all")
	afterWait(time.Now())
	h.enter()

	h.waitShown("Which aliases may it call?")
	if n := len(w.mock.Calls()); n != 1 {
		t.Errorf("%d mock calls after the check, want 1", n)
	}
	h.enter()
	h.waitShown("a name for the key [my-app]")
	h.enter()
	h.waitShown("the requests a minute the key may send [60]")
	h.enter()
	secret := h.waitReveal()
	afterWait(time.Now())
	h.enter()
	h.waitShown("> Try it")
	afterWait(time.Now())
	h.enter()
	if r := ended(t, done, 10*time.Second); r.err != nil || r.res != "Done: key my-app, which may call main." {
		t.Fatalf("%+v\n%s", r, h.out.String())
	}
	if n := len(w.mock.Calls()); n != 2 {
		t.Errorf("%d mock calls, want the check's and the try's", n)
	}
	h.waitOutput("answer: mock reply")
	h.waitOutput("Done: key my-app, which may call main.")
	h.front.Done()
	h.ended()
	h.fits()
	if path := h.barPath(eight); !slices.Equal(path, eight) {
		t.Errorf("the step bar went %q", path)
	}
	if strings.Contains(h.out.String(), providerKey) {
		t.Errorf("the provider's key is in the output:\n%q", h.out.String())
	}
	if secret == "" || !onAltScreenOnly(h.out.String(), secret) {
		t.Errorf("the secret %q is in the output off the alternate screen:\n%q", secret, h.out.String())
	}
	h.mu.Lock()
	for _, f := range h.frames {
		if strings.Contains(f.Content, providerKey) {
			t.Errorf("a frame holds the provider's key:\n%s", f.Content)
		}
		if strings.Contains(f.Content, secret) && (!f.AltScreen || !strings.Contains(f.Content, "enter: I have stored it")) {
			t.Errorf("a frame but the reveal's holds the secret:\n%s", f.Content)
		}
	}
	h.mu.Unlock()
	provider := "providers add --json --base-url http://host.docker.internal:" + strings.TrimPrefix(srvURL, "http://127.0.0.1:") +
		" --api-key-env " + resolving + " --protocol openai --billing pay_per_use --allow-private-network -- acme"
	for _, want := range []string{provider, "targets add --json --kind chat -- acme/gpt-a", "models add --json --target acme/gpt-a -- main",
		"keys create --json --model main --rpm-limit 60 -- my-app"} {
		if n := count(w.cli(), want); n != 1 {
			t.Errorf("%q made %d time(s); CLI calls:\n%q", want, n, w.cli())
		}
	}
}

// TestGuidedFirstRunResume: a provider and no alias: the run starts at the
// alias, the first two steps done.
func TestGuidedFirstRunResume(t *testing.T) {
	h := start(t)
	w, _, srvURL := planeWorld(t)
	w.must(t, "providers", "add", "mock", "--protocol", "openai", "--base-url", srvURL, "--allow-private-network", "--billing", "none")
	done := w.run(h, "First run", nil)
	h.waitShown("a name for the alias your clients call")
	h.p.Send(key(tea.KeyEscape))
	if r := ended(t, done, 5*time.Second); !errors.Is(r.err, ui.ErrLeave) {
		t.Fatalf("%+v; want ErrLeave", r)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.frames {
		if strings.Contains(f.Content, "Which providers do you want to add?") {
			t.Fatalf("the provider stage was asked:\n%s", f.Content)
		}
	}
	i := slices.IndexFunc(h.frames, func(f tea.View) bool {
		return strings.Contains(f.Content, "●") && !strings.Contains(f.Content, "● machine")
	})
	if i < 0 || !strings.HasPrefix(h.frames[i].Content, "✓ → ✓ → ● alias → ") {
		t.Fatalf("the first frame after the Machine step's is not at the alias, two steps done: frame %d of %d", i, len(h.frames))
	}
}

// TestGuidedFirstRunEscAtCheck: Esc on the check's paid frame leaves with
// no call made, and says the target added is kept.
func TestGuidedFirstRunEscAtCheck(t *testing.T) {
	h := start(t)
	w, _, srvURL := planeWorld(t)
	w.must(t, "providers", "add", "mock", "--protocol", "openai", "--base-url", srvURL, "--allow-private-network", "--billing", "none")
	done := w.run(h, "First run", nil)
	toPaid(t, h, false)
	h.p.Send(key(tea.KeyEscape))
	if r := ended(t, done, 5*time.Second); !errors.Is(r.err, ui.ErrLeave) {
		t.Fatalf("%+v; want ErrLeave", r)
	}
	h.waitOutput("already changed and kept: target mock/m added")
	noCalls(t, w)
}
