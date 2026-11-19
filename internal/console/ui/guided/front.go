package guided

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

// Front is the session goroutine's side of the bridge: a ui.Front that
// sends each question to the program and waits for its answer.
type Front struct{ p *tea.Program }

// NewFront returns the bridge to p, which runs a Model.
func NewFront(p *tea.Program) *Front { return &Front{p: p} }

// Ask shows q and waits for its answer, or for ctx to end. After the
// program has exited, Send delivers nothing, so the wait on ctx is what
// frees the session: main cancels it when Run returns.
func (f *Front) Ask(ctx context.Context, q ui.Question) (string, error) {
	reply := make(chan result, 1)
	f.p.Send(askMsg{q: q, reply: reply})
	select {
	case r := <-reply:
		return r.text, r.err
	case <-ctx.Done():
		f.p.Send(cancelMsg{reply: reply})
		return "", ctx.Err()
	}
}

// Show prints text, already cleaned, above the frame, and returns once it
// is printed, so the lines of a session keep their order.
func (f *Front) Show(ctx context.Context, text string) error {
	done := make(chan struct{})
	f.p.Send(sayMsg{text: text, done: done})
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done ends the program gracefully once the session has returned.
func (f *Front) Done() { f.p.Send(doneMsg{}) }

// Options are the program's options (docs/design/phase-7.md section 3): it
// ends with ctx, leaves signals to main, sees only the allowed environment
// and detects no colour profile. Bubble Tea's own catch of a panic in the
// model is kept: its restore of the terminal is verified.
func Options(ctx context.Context, environ []string) []tea.ProgramOption {
	env := Allowed(environ)
	return []tea.ProgramOption{
		tea.WithContext(ctx),
		tea.WithoutSignalHandler(),
		tea.WithEnvironment(env),
		tea.WithColorProfile(profile(env)),
	}
}

// Allowed keeps TERM, COLORTERM, NO_COLOR, LANG and LC_*: what drawing
// needs, and nothing that leads Bubble Tea to detect or run anything. The
// result is never nil: Bubble Tea reads nil as the whole process environment.
func Allowed(environ []string) []string {
	out := []string{}
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if k == "TERM" || k == "COLORTERM" || k == "NO_COLOR" || k == "LANG" || strings.HasPrefix(k, "LC_") {
			out = append(out, kv)
		}
	}
	return out
}

// profile is ANSI, or ASCII when NO_COLOR is set, even empty.
func profile(env []string) colorprofile.Profile {
	for _, kv := range env {
		if strings.HasPrefix(kv, "NO_COLOR=") {
			return colorprofile.ASCII
		}
	}
	return colorprofile.ANSI
}
