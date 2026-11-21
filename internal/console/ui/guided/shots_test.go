package guided

import (
	"context"
	"flag"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jorlaru/stargaite/internal/console/task"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// The README's images are what an 80x24 terminal shows at four points of
// the console, composed here from the lines said and the frame, and
// compared with docs/console/*.svg; -update rewrites them.
var update = flag.Bool("update", false, "rewrite docs/console/*.svg from the console's frames")

// tap is the guided front, recording each line the session says.
type tap struct {
	*Front
	mu    sync.Mutex
	lines []string
}

func (t *tap) Show(ctx context.Context, text string) error {
	t.mu.Lock()
	t.lines = append(t.lines, strings.Split(text, "\n")...)
	t.mu.Unlock()
	return t.Front.Show(ctx, text)
}

func (t *tap) said() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.lines...)
}

// screen is what an 80x24 terminal shows: the frame's rows, and above them
// whole said lines, wrapped at 80 columns, as many as fit in 23 rows. The
// title is said once at start, so no screen shows it.
func screen(said []string, frame string) []string {
	var rows []string
	if frame != "" {
		rows = strings.Split(strings.TrimSuffix(frame, "\n"), "\n")
	}
	for i := len(said) - 1; i >= 0; i-- {
		wrapped := strings.Split(ansi.Hardwrap(said[i], 80, true), "\n")
		if len(rows)+len(wrapped) > 23 {
			break
		}
		rows = append(wrapped, rows...)
	}
	return rows
}

// from is rows from the first one starting with prefix.
func from(t *testing.T, rows []string, prefix string) []string {
	t.Helper()
	for i, r := range rows {
		if strings.HasPrefix(r, prefix) {
			return rows[i:]
		}
	}
	t.Fatalf("no row starts with %q:\n%s", prefix, strings.Join(rows, "\n"))
	return nil
}

var duration = regexp.MustCompile(`\d+\.\d{3} s`)

// fixed makes lines the same on every run: the data plane's port the
// system chose reads 4000, the provider's 8000, and the try's duration
// 0.912 s. The longer port is replaced first, so a port that is a prefix of
// the other cannot take its match.
func fixed(lines []string, plane, provider string) []string {
	pairs := []string{":" + plane, ":4000", ":" + provider, ":8000"}
	if len(provider) > len(plane) {
		pairs = []string{":" + provider, ":8000", ":" + plane, ":4000"}
	}
	r := strings.NewReplacer(pairs...)
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = duration.ReplaceAllString(r.Replace(l), "0.912 s")
	}
	return out
}

// svg draws rows as monospace text on a dark terminal background.
func svg(rows []string) []byte {
	const cell, line, pad = 8.4, 18.0, 12.0
	w, h := 80*cell+2*pad, float64(len(rows))*line+2*pad
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" `+
		`font-family="ui-monospace,SFMono-Regular,Menlo,Consolas,'DejaVu Sans Mono','Liberation Mono',monospace" font-size="14">`+"\n", w, h, w, h)
	b.WriteString(`<rect width="100%" height="100%" rx="6" fill="#1e1e2e"/>` + "\n" + `<g fill="#e6e6e6">` + "\n")
	for i, r := range rows {
		if strings.TrimSpace(r) != "" {
			fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" xml:space="preserve">%s</text>`+"\n", pad, pad+float64(i)*line+14, html.EscapeString(r))
		}
	}
	b.WriteString("</g>\n</svg>\n")
	return []byte(b.String())
}

// holdsKey: a client or provider key is in rows, joined to undo the wrap.
func holdsKey(rows []string) bool {
	all := strings.Join(rows, "")
	return strings.Contains(all, "sk-stargaite-") || strings.Contains(all, "sk-provider-")
}

// TestShotsKeyGuard: a key that the 80-column wrap split across two rows is
// still found.
func TestShotsKeyGuard(t *testing.T) {
	rows := screen([]string{strings.Repeat("x", 75) + "sk-stargaite-abc"}, "")
	if len(rows) != 2 || strings.Contains(rows[0], "sk-stargaite-") || strings.Contains(rows[1], "sk-stargaite-") {
		t.Fatalf("the probe does not split the key: %q", rows)
	}
	if !holdsKey(rows) {
		t.Error("a key split by the wrap is not found")
	}
}

// shot compares rows, drawn, with docs/console/name.svg, or writes it with
// -update; rows holding a key fail first.
func shot(t *testing.T, name string, rows []string) {
	t.Helper()
	if holdsKey(rows) {
		t.Fatalf("%s holds a key", name)
	}
	path := filepath.Join("..", "..", "..", "..", "docs", "console", name+".svg")
	img := svg(rows)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, img, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(img) {
		t.Errorf("%s differs from the console's frame: go test ./internal/console/ui/guided -run TestShots -update rewrites it", name+".svg")
	}
}

var shotModels = []string{"openai/gpt-4.1", "openai/gpt-4.1-mini", "openai/o4-mini", "anthropic/claude-sonnet-4",
	"anthropic/claude-haiku-4.5", "google/gemini-2.5-flash", "mistralai/mistral-small", "meta-llama/llama-3.3-70b-instruct",
	"qwen/qwen3-32b", "openai/text-embedding-3-small"}

// TestShotsFirstRun: the first run's model pick, its paid check and its
// first answer.
func TestShotsFirstRun(t *testing.T) {
	h := start(t)
	tp := &tap{Front: h.front}
	w, port, srvURL := planeWorld(t, shotModels...)
	pport := strings.TrimPrefix(srvURL, "http://127.0.0.1:")
	at := func(frame string) []string {
		return screen(fixed(tp.said(), port, pport), fixed([]string{frame}, port, pport)[0])
	}
	var entry task.Entry
	for _, e := range task.Menu() {
		if e.Label == "First run" {
			entry = e
		}
	}
	u, px := ui.New(tp, nil), w.proxy()
	done := make(chan runAdd, 1)
	go func() {
		res, err := u.Run(h.ctx, entry.Steps, func(ctx context.Context) (string, error) { return entry.Run(ctx, u, px, w.h) })
		if err == nil {
			err = u.Say(h.ctx, res)
		}
		done <- runAdd{res, err}
	}()

	h.waitShown("Which providers do you want to add?")
	h.typeText("another")
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
	h.p.Send(tea.PasteMsg{Content: "sk-provider-example"})
	h.enter()
	h.waitShown("continue?")
	h.p.Send(key(tea.KeyUp))
	h.waitShown("> Yes")
	h.enter()
	h.waitShown("a name for the alias your clients call [main]")
	h.enter()
	h.waitShown("Which provider has the model?")
	h.choose("acme")
	shot(t, "first-run-model", at(h.waitShown("Which model of acme?").Content))
	h.choose("gpt-4.1-mini")
	h.waitShown("Fallback 1 of 2")
	h.enter()
	h.waitShown("a default reasoning effort")
	h.enter()
	shot(t, "paid-check", from(t, at(h.waitShown("1 paid call(s) in all").Content), "check acme/"))
	afterWait(time.Now())
	h.enter()
	h.waitShown("Which aliases may it call?")
	h.enter()
	h.waitShown("a name for the key [my-app]")
	h.enter()
	h.waitShown("the requests a minute the key may send [60]")
	h.enter()
	h.waitReveal()
	afterWait(time.Now())
	h.enter()
	h.waitShown("> Try it")
	afterWait(time.Now())
	h.enter()
	if r := ended(t, done, 10*time.Second); r.err != nil {
		t.Fatalf("%+v\n%s", r, h.out.String())
	}
	h.waitOutput("Done: key my-app, which may call main.")
	shot(t, "first-answer", from(t, at(""), "answer: "))
	h.front.Done()
	h.ended()
}

// TestShotsHome: the home after a first run, as a reader sees it at 80x24.
func TestShotsHome(t *testing.T) {
	h := start(t)
	tp := &tap{Front: h.front}
	w, port, srvURL := planeWorld(t)
	w.must(t, "providers", "add", "acme", "--protocol", "openai", "--base-url", srvURL, "--allow-private-network", "--billing", "none")
	w.must(t, "targets", "add", "--kind", "chat", "--", "acme/gpt-4.1-mini")
	w.must(t, "models", "add", "main", "--target", "acme/gpt-4.1-mini")
	w.must(t, "keys", "create", "my-app", "--model", "main")
	done := make(chan error, 1)
	go func() { done <- task.Home(h.ctx, ui.New(tp, nil), w.proxy(), w.h, 15*time.Second) }()
	f := h.waitShown("What do you want to do?")
	h.waitOutput(": ready · proxy ")
	pport := strings.TrimPrefix(srvURL, "http://127.0.0.1:")
	shot(t, "home", screen(fixed(tp.said(), port, pport), fixed([]string{f.Content}, port, pport)[0]))
	quitHome(t, h, done)
}
