package piece

import (
	tea "charm.land/bubbletea/v2"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

// work is shown while a step runs: Esc stops it, Ctrl-C quits.
type work struct{ q ui.Question }

// Work shows q.Prompt while the session works.
func Work(q ui.Question) Piece { return work{q: q} }

func (w work) Update(msg tea.Msg) (Piece, *Answer) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "esc":
			return w, &Answer{Err: ui.ErrLeave}
		case "ctrl+c":
			return w, &Answer{Err: ui.ErrQuit}
		}
	}
	return w, nil // a paste included
}

func (w work) View(width, height int) string {
	return fit([]string{w.q.Prompt + "…", "esc stop · ctrl+c quit"}, width, height, 0)
}
