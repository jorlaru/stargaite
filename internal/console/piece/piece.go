// Package piece is the guided console's questions as Bubble Tea input: a
// pick, a multi-pick, a line of text, a hidden secret, a yes-or-no confirm,
// and the work under way. A piece takes messages and
// returns its answer, and draws itself in the size it is given. It knows no
// task, proxy or host (docs/design/phase-7.md section 5).
package piece

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

// Answer ends a question: a value, or ui.ErrLeave or ui.ErrQuit in Err.
type Answer struct {
	Value string
	Err   error
}

// Piece is one question on screen.
type Piece interface {
	// Update applies one message; a non-nil Answer means the question is answered.
	Update(msg tea.Msg) (Piece, *Answer)
	// View draws at most height lines, none wider than width.
	View(width, height int) string
}

// Cursorer is a piece that places the terminal cursor within its view of
// width x height.
type Cursorer interface {
	Cursor(width, height int) (x, y int)
}

// leaveOrQuit answers Esc (except at the home menu, which has no step) and
// Ctrl-C, the keys every piece shares.
func leaveOrQuit(q ui.Question, key string) *Answer {
	switch {
	case key == "ctrl+c":
		return &Answer{Err: ui.ErrQuit}
	case key == "esc" && q.Step != "":
		return &Answer{Err: ui.ErrLeave}
	}
	return nil
}

// footer is a piece's key help, without "esc menu" at home.
func footer(q ui.Question, keys string) string {
	if q.Step == "" {
		return keys + " · ctrl+c quit"
	}
	return keys + " · esc menu · ctrl+c quit"
}

// hasControl reports whether s holds a rune below U+0020 (a line break among
// them), DEL or a C1 control, which a terminal could read as the start of a
// sequence.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(c rune) bool { return c < 0x20 || (c >= 0x7f && c <= 0x9f) })
}

// fit cuts lines to width and, when there are more than height, keeps the
// height lines that end with the tail but still show line keep.
func fit(lines []string, width, height, keep int) string {
	height = max(1, height)
	if len(lines) > height {
		start := min(keep, len(lines)-height)
		lines = lines[start : start+height]
	}
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, width, "…")
	}
	return strings.Join(lines, "\n")
}
