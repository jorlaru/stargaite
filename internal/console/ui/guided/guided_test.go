package guided

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

// recorder wraps the root model and records every frame View returns.
type recorder struct {
	inner  tea.Model
	mu     *sync.Mutex
	frames *[]tea.View
	seen   chan struct{} // poked, without blocking, after each frame
}

func (r recorder) Init() tea.Cmd { return r.inner.Init() }

func (r recorder) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	r.inner, cmd = r.inner.Update(msg)
	return r, cmd
}

func (r recorder) View() tea.View {
	v := r.inner.View()
	r.mu.Lock()
	*r.frames = append(*r.frames, v)
	r.mu.Unlock()
	select {
	case r.seen <- struct{}{}:
	default:
	}
	return v
}

// syncBuffer is the program's output, read while the program writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	t      *testing.T
	ctx    context.Context
	cancel context.CancelFunc
	p      *tea.Program
	front  *Front
	out    *syncBuffer
	mu     sync.Mutex
	frames []tea.View
	seen   chan struct{}
	run    chan runResult
}

type runResult struct {
	model tea.Model
	err   error
}

// start runs a program on the root model, headless at 80x24, with its
// output in h.out.
func start(t *testing.T) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &harness{t: t, ctx: ctx, cancel: cancel, out: &syncBuffer{}, seen: make(chan struct{}, 1), run: make(chan runResult, 1)}
	rec := recorder{inner: NewModel("v-test"), mu: &h.mu, frames: &h.frames, seen: h.seen}
	h.p = tea.NewProgram(rec,
		tea.WithContext(ctx),
		tea.WithInput(nil),
		tea.WithOutput(h.out),
		tea.WithWindowSize(80, 24),
		tea.WithoutSignalHandler(),
		tea.WithEnvironment([]string{"TERM=xterm-256color"}),
		tea.WithColorProfile(colorprofile.ASCII),
	)
	h.front = NewFront(h.p)
	go func() {
		m, err := h.p.Run()
		h.run <- runResult{m, err}
	}()
	return h
}

// waitFrame waits until a recorded frame satisfies ok, and returns it.
func (h *harness) waitFrame(ok func(tea.View) bool) tea.View {
	h.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		h.mu.Lock()
		for _, f := range h.frames {
			if ok(f) {
				h.mu.Unlock()
				return f
			}
		}
		last := ""
		if n := len(h.frames); n > 0 {
			last = h.frames[n-1].Content
		}
		h.mu.Unlock()
		select {
		case <-h.seen:
		case <-deadline:
			h.t.Fatalf("no matching frame; last:\n%s", last)
		}
	}
}

// waitShown waits for a frame holding s.
func (h *harness) waitShown(s string) tea.View {
	h.t.Helper()
	return h.waitFrame(func(v tea.View) bool { return strings.Contains(v.Content, s) })
}

// waitOutput waits until the output holds s.
func (h *harness) waitOutput(s string) string {
	h.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if out := h.out.String(); strings.Contains(out, s) {
			return out
		}
	}
	h.t.Fatalf("output never held %q:\n%q", s, h.out.String())
	return ""
}

// ended waits for Run to return.
func (h *harness) ended() runResult {
	h.t.Helper()
	select {
	case r := <-h.run:
		return r
	case <-time.After(5 * time.Second):
		h.t.Fatal("the program did not end")
	}
	return runResult{}
}

// asked runs Ask on its own goroutine, as the session does.
func (h *harness) asked(q ui.Question) <-chan result {
	got := make(chan result, 1)
	go func() {
		a, err := h.front.Ask(h.ctx, q)
		got <- result{a, err}
	}()
	return got
}

func answer(t *testing.T, got <-chan result) result {
	t.Helper()
	select {
	case r := <-got:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
	}
	return result{}
}

func key(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }

var ctrlC = tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}

func homeQuestion() ui.Question {
	return ui.Question{ID: "home", Prompt: "What do you want to do?", Kind: ui.KindPick, Bar: ui.Bar{Current: -1},
		Options: []ui.Option{{Value: "a", Label: "See what exists"}, {Value: "b", Label: "Something else"}}}
}

func revealQuestion() ui.Question {
	return ui.Question{ID: "key", Step: "key", Prompt: "Your key, shown once:", Kind: ui.KindReveal,
		Value: "FAKE-REVEAL-7f3a", Bar: ui.Bar{Titles: []string{"key"}, Current: 0}}
}

func TestHomePickAnswers(t *testing.T) {
	h := start(t)
	got := h.asked(homeQuestion())
	f := h.waitShown("What do you want to do?")
	lines := strings.Split(f.Content, "\n")
	if lines[0] != "StargAIte console v-test" || lines[1] != "" || lines[2] != "What do you want to do?" {
		t.Fatalf("home frame:\n%s", f.Content)
	}
	h.p.Send(key(tea.KeyDown))
	h.p.Send(key(tea.KeyEnter))
	if r := answer(t, got); r.err != nil || r.text != "b" {
		t.Fatalf("answer %+v; want b", r)
	}
	h.waitShown("working…")
}

func TestStepBarFrames(t *testing.T) {
	h := start(t)
	q := ui.Question{ID: "model", Step: "model", Prompt: "Which model?", Kind: ui.KindPick,
		Options: []ui.Option{{Value: "x", Label: "x"}}, Bar: ui.Bar{Titles: []string{"name", "model", "check"}, Current: 1}}
	h.asked(q)
	f := h.waitShown("Which model?")
	if lines := strings.Split(f.Content, "\n"); lines[1] != "✓ name → ● model → check" || lines[2] != "" {
		t.Fatalf("frame:\n%s", f.Content)
	}
}

func TestStepBarNarrow(t *testing.T) {
	titles := []string{"machine", "provider", "model", "fallbacks", "check", "alias", "key", "first answer"}
	s := stepBar(ui.Bar{Titles: titles, Current: 6}, 80)
	if ansi.StringWidth(s) > 80 || !strings.Contains(s, "● key") || !strings.HasPrefix(s, "✓ → ✓") {
		t.Fatalf("bar %q (%d cells)", s, ansi.StringWidth(s))
	}
	if s := stepBar(ui.Bar{Titles: titles, Current: 6}, 20); ansi.StringWidth(s) > 20 || !strings.HasSuffix(s, "…") {
		t.Fatalf("bar at 20: %q", s)
	}
}

func TestEscLeaves(t *testing.T) {
	h := start(t)
	q := ui.Question{ID: "name", Step: "name", Prompt: "Name?", Kind: ui.KindText, Bar: ui.Bar{Titles: []string{"name"}}}
	got := h.asked(q)
	h.waitShown("Name?")
	h.p.Send(key(tea.KeyEscape))
	if r := answer(t, got); !errors.Is(r.err, ui.ErrLeave) {
		t.Fatalf("answer %+v; want ErrLeave", r)
	}
}

func TestTextCursorBelowHeader(t *testing.T) {
	h := start(t)
	q := ui.Question{ID: "name", Step: "name", Prompt: "Name?", Kind: ui.KindText, Bar: ui.Bar{Titles: []string{"name"}}}
	h.asked(q)
	h.waitShown("Name?")
	h.p.Send(tea.KeyPressMsg{Code: 'é', Text: "é"})
	f := h.waitShown("> é")
	if f.Cursor == nil || f.Cursor.X != 3 || f.Cursor.Y != 4 { // title, bar, blank, prompt; "> é"
		t.Fatalf("cursor %+v in\n%s", f.Cursor, f.Content)
	}
	// Five rows leave the piece one: only the input, on row 3 of the frame.
	h.p.Send(tea.WindowSizeMsg{Width: 80, Height: 5})
	f = h.waitFrame(func(f tea.View) bool {
		return strings.Contains(f.Content, "> é") && !strings.Contains(f.Content, "Name?")
	})
	if f.Cursor == nil || f.Cursor.X != 3 || f.Cursor.Y != 3 {
		t.Fatalf("cursor %+v at height 5 in\n%s", f.Cursor, f.Content)
	}
}

func TestCtrlCQuitsProgram(t *testing.T) {
	h := start(t)
	got := h.asked(homeQuestion())
	h.waitShown("What do you want to do?")
	h.p.Send(ctrlC)
	r := h.ended()
	if r.err != nil || !r.model.(recorder).inner.(Model).Interrupted() {
		t.Fatalf("run: %v, interrupted %v", r.err, r.model.(recorder).inner.(Model).Interrupted())
	}
	if a := answer(t, got); !errors.Is(a.err, ui.ErrQuit) {
		t.Fatalf("answer %+v; want ErrQuit", a)
	}
}

func TestCtrlCWhileWorking(t *testing.T) {
	h := start(t)
	h.waitShown("working…")
	h.p.Send(ctrlC)
	if r := h.ended(); !r.model.(recorder).inner.(Model).Interrupted() {
		t.Fatal("ctrl+c while working did not interrupt")
	}
}

func TestRevealAltScreenOneStep(t *testing.T) {
	h := start(t)
	got := h.asked(revealQuestion())
	f := h.waitShown("FAKE-REVEAL-7f3a")
	if !f.AltScreen || !strings.Contains(f.Content, "\n  FAKE-REVEAL-7f3a\n\nenter: I have stored it\n") {
		t.Fatalf("reveal frame alt=%v:\n%s", f.AltScreen, f.Content)
	}
	h.waitOutput("FAKE-REVEAL-7f3a")
	h.p.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	h.p.Send(tea.PasteMsg{Content: "x"})
	h.p.Send(key(tea.KeyEnter))
	if r := answer(t, got); r.err != nil || r.text != "" {
		t.Fatalf("answer %+v", r)
	}
	h.front.Done()
	h.ended()
	out := h.out.String()
	on, val, off := strings.Index(out, "\x1b[?1049h"), strings.Index(out, "FAKE-REVEAL-7f3a"), strings.LastIndex(out, "\x1b[?1049l")
	if on < 0 || on >= val || val >= off || strings.Count(out, "FAKE-REVEAL-7f3a") != 1 {
		t.Fatalf("1049h at %d, value at %d (%d times), 1049l at %d:\n%q", on, val, strings.Count(out, "FAKE-REVEAL-7f3a"), off, out)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	after := false
	for _, fr := range h.frames {
		if strings.Contains(fr.Content, "FAKE-REVEAL-7f3a") {
			after = true
			continue
		}
		if after && fr.AltScreen {
			t.Fatalf("a frame after the reveal is on the alternate screen:\n%s", fr.Content)
		}
	}
}

// A client key is 78 characters (internal/auth); on a terminal narrower than
// its line it is wrapped, never cut.
func TestRevealWraps(t *testing.T) {
	q := revealQuestion()
	q.Value = "sk-stargaite-" + strings.Repeat("a", 12) + "-" + strings.Repeat("b", 52)
	for _, w := range []int{60, 40} {
		view := (&reveal{q: q}).View(w, 24)
		var shown []string
		for _, l := range strings.Split(view, "\n") {
			if ansi.StringWidth(l) > w {
				t.Fatalf("width %d: line %q is %d wide", w, l, ansi.StringWidth(l))
			}
			if v, ok := strings.CutPrefix(l, "  "); ok {
				shown = append(shown, v)
			}
		}
		if got := strings.Join(shown, ""); got != q.Value {
			t.Fatalf("width %d: shown %q; want %q", w, got, q.Value)
		}
		// A wrapped value says its rows are one value, since a selection
		// across them would copy the breaks and the indent.
		note := fmt.Sprintf("one value on %d lines: copy it without the line breaks or spaces", len(shown))
		if !strings.Contains(strings.Join(strings.Fields(view), " "), note) {
			t.Fatalf("width %d: no %q in\n%s", w, note, view)
		}
	}
	if v := (&reveal{q: q}).View(80, 24); strings.Contains(v, "one value on") {
		t.Fatalf("unwrapped value carries the note:\n%s", v)
	}
}

// A question whose context ends leaves the screen and takes no more keys.
func TestAskCancelClearsQuestion(t *testing.T) {
	h := start(t)
	ctx, cancel := context.WithCancel(h.ctx)
	got := make(chan result, 1)
	go func() {
		a, err := h.front.Ask(ctx, homeQuestion())
		got <- result{a, err}
	}()
	h.waitShown("What do you want to do?")
	h.mu.Lock()
	n := len(h.frames)
	h.mu.Unlock()
	cancel()
	if r := answer(t, got); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("answer %+v; want context.Canceled", r)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		h.mu.Lock()
		last := h.frames[len(h.frames)-1].Content
		cleared := len(h.frames) > n && strings.Contains(last, "working…") && !strings.Contains(last, "What do you want to do?")
		h.mu.Unlock()
		if cleared {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cancelled question is still shown:\n%s", last)
		}
	}
	h.front.Done()
	if r := h.ended(); r.err != nil {
		t.Fatalf("run: %v", r.err)
	}
}

func TestRevealHoldsSay(t *testing.T) {
	h := start(t)
	got := h.asked(revealQuestion())
	h.waitShown("FAKE-REVEAL-7f3a")
	h.waitOutput("FAKE-REVEAL-7f3a")
	shown := make(chan error, 1)
	go func() { shown <- h.front.Show(h.ctx, "later") }()
	time.Sleep(50 * time.Millisecond) // the line reaches the model while the reveal shows
	if strings.Contains(h.out.String(), "later") {
		t.Fatal("a line was printed on the alternate screen")
	}
	h.p.Send(key(tea.KeyEnter))
	answer(t, got)
	out := h.waitOutput("later")
	if off := strings.LastIndex(out, "\x1b[?1049l"); off < 0 || strings.Index(out, "later") < off {
		t.Fatalf("later before the alternate screen was left:\n%q", out)
	}
	if err := <-shown; err != nil {
		t.Fatal(err)
	}
}

func TestShowKeepsOrder(t *testing.T) {
	h := start(t)
	h.waitShown("working…")
	u := ui.New(h.front, nil)
	for i := range 20 {
		if err := u.Say(h.ctx, fmt.Sprintf("line-%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	out := h.waitOutput("line-19")
	last := -1
	for i := range 20 {
		at := strings.Index(out, fmt.Sprintf("line-%02d", i))
		if at < last {
			t.Fatalf("line-%02d out of order:\n%q", i, out)
		}
		last = at
	}
}

func TestRevealCtrlCAsksFirst(t *testing.T) {
	h := start(t)
	got := h.asked(revealQuestion())
	h.waitShown("FAKE-REVEAL-7f3a")
	h.waitOutput("FAKE-REVEAL-7f3a")
	h.p.Send(ctrlC)
	f := h.waitShown("ctrl+c again quits; it will not be shown again")
	if !f.AltScreen {
		t.Fatal("the warning left the alternate screen")
	}
	select {
	case r := <-got:
		t.Fatalf("the first ctrl+c answered %+v", r)
	default:
	}
	h.p.Send(ctrlC)
	if r := answer(t, got); !errors.Is(r.err, ui.ErrQuit) {
		t.Fatalf("answer %+v; want ErrQuit", r)
	}
	if r := h.ended(); !r.model.(recorder).inner.(Model).Interrupted() {
		t.Fatal("not interrupted")
	}
	out := h.out.String()
	if !strings.HasSuffix(out[strings.LastIndex(out, "FAKE-REVEAL-7f3a"):], "\x1b[?25h\x1b[?2004l") ||
		!strings.Contains(out[strings.LastIndex(out, "FAKE-REVEAL-7f3a"):], "\x1b[?1049l") {
		t.Fatalf("not restored after the reveal:\n%q", out)
	}
}

func TestRevealLeftOnCancel(t *testing.T) {
	h := start(t)
	got := h.asked(revealQuestion())
	h.waitShown("FAKE-REVEAL-7f3a")
	h.waitOutput("FAKE-REVEAL-7f3a")
	h.cancel()
	if r := answer(t, got); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("answer %+v; want context.Canceled", r)
	}
	if r := h.ended(); !errors.Is(r.err, tea.ErrProgramKilled) {
		t.Fatalf("run: %v", r.err)
	}
	out := h.out.String()
	tail := out[strings.LastIndex(out, "FAKE-REVEAL-7f3a"):]
	off := strings.Index(tail, "\x1b[?1049l")
	if off < 0 || !strings.Contains(tail[off:], "\x1b[?25h") || !strings.HasSuffix(tail, "\x1b[?2004l") {
		t.Fatalf("after cancel:\n%q", tail)
	}
}

// After the program has exited, Ask returns only through its context: Send
// is a no-op then (research: RB section 3).
func TestAskAfterExitUnblocksOnCancel(t *testing.T) {
	h := start(t)
	h.waitShown("working…")
	h.front.Done()
	h.ended()
	got := h.asked(homeQuestion())
	select {
	case r := <-got:
		t.Fatalf("Ask returned before cancel: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
	h.cancel()
	if r := answer(t, got); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("answer %+v; want context.Canceled", r)
	}
}

func TestGuidedShowCleans(t *testing.T) {
	h := start(t)
	h.waitShown("working…")
	if err := ui.New(h.front, nil).Say(h.ctx, "a\x1b[2Jb\u202ec"); err != nil {
		t.Fatal(err)
	}
	out := h.waitOutput("a[2Jbc")
	if strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\u202e") {
		t.Fatalf("control reached the terminal:\n%q", out)
	}
}

func TestAllowed(t *testing.T) {
	got := Allowed([]string{"TERM=x", "TMUX=/s", "LC_ALL=C", "TEA_TRACE=f", "HOME=/h", "COLORTERM=truecolor", "NO_COLOR=1", "LANG=C.UTF-8", "TERMINFO=/t"})
	want := []string{"TERM=x", "LC_ALL=C", "COLORTERM=truecolor", "NO_COLOR=1", "LANG=C.UTF-8"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q; want %q", got, want)
	}
	// Empty, not nil: Bubble Tea reads a nil environment as the whole process's.
	if got := Allowed([]string{"TMUX=/s", "HOME=/h"}); got == nil || len(got) != 0 {
		t.Fatalf("got %#v; want an empty, non-nil slice", got)
	}
}

func TestProfile(t *testing.T) {
	if p := profile([]string{"TERM=x"}); p != colorprofile.ANSI {
		t.Fatalf("profile %v; want ANSI", p)
	}
	if p := profile([]string{"TERM=x", "NO_COLOR="}); p != colorprofile.ASCII {
		t.Fatalf("NO_COLOR set: profile %v; want ASCII", p)
	}
}

func TestDoneQuits(t *testing.T) {
	h := start(t)
	h.waitShown("working…")
	h.front.Done()
	r := h.ended()
	if r.err != nil || r.model.(recorder).inner.(Model).Interrupted() {
		t.Fatalf("run: %v, interrupted %v", r.err, r.model.(recorder).inner.(Model).Interrupted())
	}
}

// panicky panics in View once the session asks a question.
type panicky struct{ Model }

func (p panicky) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m, cmd := p.Model.Update(msg)
	return panicky{m.(Model)}, cmd
}

func (p panicky) View() tea.View {
	if p.ask != nil {
		panic("boom")
	}
	return p.Model.View()
}

func TestModelPanicRestores(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncBuffer{}
	opts := append(Options(ctx, []string{"TERM=xterm-256color"}), tea.WithInput(nil), tea.WithOutput(out), tea.WithWindowSize(80, 24))
	p := tea.NewProgram(panicky{NewModel("v-test")}, opts...)
	ran := make(chan error, 1)
	go func() { _, err := p.Run(); ran <- err }()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(out.String(), "working"); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) { // the first frame sets the modes the panic must reset
			t.Fatalf("no first frame: %q", out.String())
		}
	}
	go func() { _, _ = NewFront(p).Ask(ctx, homeQuestion()) }()
	if err := <-ran; !errors.Is(err, tea.ErrProgramPanic) {
		t.Fatalf("run: %v; want ErrProgramPanic", err)
	}
	if s := out.String(); !strings.HasSuffix(s, "\x1b[?25h\x1b[?2004l") {
		t.Fatalf("not restored:\n%q", s)
	}
}

// TestFrameFits checks every frame of a session: home, a 100-option pick in
// a task with a step bar, a text, a confirm, a reveal and the narrow bar.
func TestFrameFits(t *testing.T) {
	h := start(t)
	big := ui.Question{ID: "model", Step: "model", Prompt: "Which model?", Kind: ui.KindPick,
		Bar: ui.Bar{Titles: []string{"machine", "provider", "model", "fallbacks", "check", "alias", "key", "first answer"}, Current: 2}}
	for i := range 100 {
		id := fmt.Sprintf("vendor-%02d/model-%03d", i/10, i)
		big.Options = append(big.Options, ui.Option{Value: id, Label: id})
	}
	text := ui.Question{ID: "name", Step: "name", Prompt: "Name?", Kind: ui.KindText, Default: "main", HasDefault: true,
		Bar: ui.Bar{Titles: []string{"name"}}}
	confirm := ui.Question{ID: "apply", Step: "name", Prompt: "Apply?", Kind: ui.KindConfirm,
		Options: []ui.Option{{Value: "yes", Label: "Yes"}, {Value: "no", Label: "No"}}, Bar: ui.Bar{Titles: []string{"name"}}}
	for _, q := range []ui.Question{homeQuestion(), big, text, confirm, revealQuestion()} {
		got := h.asked(q)
		h.waitShown(q.Prompt)
		if q.Kind == ui.KindText {
			for range 100 {
				h.p.Send(tea.KeyPressMsg{Code: '日', Text: "日"})
			}
		}
		for range 40 {
			h.p.Send(key(tea.KeyDown))
		}
		h.p.Send(key(tea.KeyEnter))
		if r := answer(t, got); r.err != nil {
			t.Fatal(r.err)
		}
	}
	h.front.Done()
	h.ended()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.frames) < 10 {
		t.Fatalf("only %d frames", len(h.frames))
	}
	for _, f := range h.frames {
		if !strings.HasSuffix(f.Content, "\n") {
			t.Fatalf("frame without a final newline:\n%s", f.Content)
		}
		lines := strings.Split(f.Content, "\n")
		if len(lines) > 24 {
			t.Fatalf("%d lines:\n%s", len(lines), f.Content)
		}
		for _, l := range lines {
			if ansi.StringWidth(l) > 80 {
				t.Fatalf("line %q is %d wide", l, ansi.StringWidth(l))
			}
		}
	}
}

func TestGuidedSecretPaste(t *testing.T) {
	h := start(t)
	q := ui.Question{ID: "key-env", Step: "key", Prompt: "ACME_API_KEY", Kind: ui.KindSecret, Bar: ui.Bar{Titles: []string{"key"}}}
	got := h.asked(q)
	h.waitShown("typed hidden")
	h.p.Send(tea.PasteMsg{Content: "sk-pasted-9"})
	h.p.Send(key(tea.KeyEnter))
	if r := answer(t, got); r.err != nil || r.text != "sk-pasted-9" {
		t.Fatalf("answer %+v", r)
	}
	h.front.Done()
	h.ended()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.frames {
		if strings.Contains(f.Content, "sk-pasted-9") {
			t.Fatalf("a frame holds the secret:\n%s", f.Content)
		}
	}
	if strings.Contains(h.out.String(), "sk-pasted-9") {
		t.Fatal("the output holds the secret")
	}
}

// work runs u.Work with a question of prompt "restarting" in a one-step task
// on its own goroutine, with fn, and sends Work's error.
func (h *harness) work(fn func(context.Context) error) <-chan error {
	u := ui.New(h.front, nil)
	done := make(chan error, 1)
	go func() {
		_, err := u.Run(h.ctx, []ui.Step{{ID: "recreate", Title: "recreate"}}, func(ctx context.Context) (string, error) {
			return "", u.Work(ctx, ui.Question{ID: "up", Step: "recreate", Prompt: "restarting"}, fn)
		})
		done <- err
	}()
	return done
}

// workGone waits until the last frame shows no work piece.
func (h *harness) workGone() {
	h.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		h.mu.Lock()
		last := h.frames[len(h.frames)-1].Content
		h.mu.Unlock()
		if strings.Contains(last, "working…") && !strings.Contains(last, "restarting…") {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("the work piece is still shown:\n%s", last)
		}
	}
}

func TestGuidedWorkEsc(t *testing.T) {
	h := start(t)
	stopped := make(chan struct{})
	done := h.work(func(ctx context.Context) error {
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	})
	f := h.waitShown("restarting…")
	if !strings.Contains(f.Content, "● recreate") || !strings.Contains(f.Content, "esc stop · ctrl+c quit") {
		t.Fatalf("work frame:\n%s", f.Content)
	}
	h.p.Send(key(tea.KeyEscape))
	select {
	case err := <-done:
		if !errors.Is(err, ui.ErrLeave) {
			t.Fatalf("Work: %v; want ErrLeave", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Work did not return within 2s")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("fn's context was not cancelled")
	}
	h.workGone()
}

func TestGuidedWorkDone(t *testing.T) {
	h := start(t)
	done := h.work(func(ctx context.Context) error {
		select {
		case <-time.After(50 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	h.waitShown("restarting…")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Work: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Work did not return")
	}
	h.workGone()
}

func TestGuidedPickManyFrame(t *testing.T) {
	h := start(t)
	q := ui.Question{ID: "provider", Step: "which", Prompt: "Which providers do you want to add?", Kind: ui.KindPickMany,
		Bar: ui.Bar{Titles: []string{"which", "add", "key", "write", "recreate"}}}
	for i := range 19 {
		name := fmt.Sprintf("provider-%02d", i)
		q.Options = append(q.Options, ui.Option{Value: name, Label: name + ", untested, asks for its URL  ✓ added"})
	}
	got := h.asked(q)
	h.waitShown("[ ] provider-00")
	for range 30 {
		h.p.Send(key(tea.KeyDown))
	}
	h.p.Send(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	h.waitShown("> [x] provider-18")
	h.p.Send(key(tea.KeyEnter))
	if r := answer(t, got); r.err != nil || r.text != "provider-18" {
		t.Fatalf("answer %+v", r)
	}
	h.front.Done()
	h.ended()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.frames {
		lines := strings.Split(f.Content, "\n")
		if len(lines) > 24 {
			t.Fatalf("%d lines:\n%s", len(lines), f.Content)
		}
		for _, l := range lines {
			if ansi.StringWidth(l) > 80 {
				t.Fatalf("line %q is %d wide", l, ansi.StringWidth(l))
			}
		}
	}
}
