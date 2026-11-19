package piece

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

// EnterWait is how long after the paid step, or a secret's reveal, is made an
// Enter is taken: 400 ms after it is drawn (design section 3, Paid consent
// and The client key), and it is drawn within one frame, 1/60 s, of being
// made.
const EnterWait = 400*time.Millisecond + time.Second/60

// paid is a paid step: q.Options, "Check it" then "Not now", on "Check it".
type paid struct {
	q       ui.Question
	cursor  int
	created time.Time
	now     func() time.Time
}

// Paid asks for a paid step's order. An Enter earlier than EnterWait after it
// was made, a buffered one among them, is ignored, and so is any paste.
func Paid(q ui.Question) Piece { return paidAt(q, time.Now) }

// paidAt is Paid on the clock now.
func paidAt(q ui.Question, now func() time.Time) Piece { return paid{q: q, created: now(), now: now} }

func (p paid) Update(msg tea.Msg) (Piece, *Answer) {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return p, nil // a paste never pays
	}
	if a := leaveOrQuit(p.q, k.String()); a != nil {
		return p, a
	}
	switch k.String() {
	case "enter":
		if p.now().Sub(p.created) >= EnterWait && p.cursor < len(p.q.Options) {
			return p, &Answer{Value: p.q.Options[p.cursor].Value}
		}
	case "up":
		p.cursor = max(0, p.cursor-1)
	case "down":
		p.cursor = max(0, min(len(p.q.Options)-1, p.cursor+1))
	}
	return p, nil
}

func (p paid) View(width, height int) string {
	lines := []string{p.q.Prompt}
	for i, o := range p.q.Options {
		mark := "  "
		if i == p.cursor {
			mark = "> "
		}
		lines = append(lines, mark+o.Label)
	}
	lines = append(lines, footer(p.q, "↑↓ move · enter pick"))
	return fit(lines, width, height, 1+p.cursor)
}
