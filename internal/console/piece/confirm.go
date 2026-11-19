package piece

import (
	tea "charm.land/bubbletea/v2"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

// confirm is Yes or No, on No unless the default is yes.
type confirm struct {
	q   ui.Question
	yes bool
}

// Confirm asks yes or no; the answers are "yes" and "no".
func Confirm(q ui.Question) Piece { return confirm{q: q, yes: q.Default == "yes"} }

func (c confirm) Update(msg tea.Msg) (Piece, *Answer) {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return c, nil // a paste is never a confirm
	}
	if a := leaveOrQuit(c.q, k.String()); a != nil {
		return c, a
	}
	switch k.String() {
	case "enter":
		if c.yes {
			return c, &Answer{Value: "yes"}
		}
		return c, &Answer{Value: "no"}
	case "up":
		c.yes = true
	case "down":
		c.yes = false
	}
	return c, nil
}

func (c confirm) View(width, height int) string {
	yes, no, keep := "> Yes", "  No", 1
	if !c.yes {
		yes, no, keep = "  Yes", "> No", 2
	}
	return fit([]string{c.q.Prompt, yes, no, footer(c.q, "↑↓ move · enter pick")}, width, height, keep)
}
