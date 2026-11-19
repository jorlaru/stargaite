package piece

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

// pasteRefused is said when a paste holds a line break inside it or a control.
const pasteRefused = "a pasted value with a line break or a control character is refused"

// secret is a value typed or pasted hidden: nothing of it is drawn, not even
// its length (docs/design/phase-7.md section 3).
type secret struct {
	q      ui.Question
	value  []rune
	reason string // why the last value or paste was refused
}

// Secret asks for a value .env can hold, named by q.Prompt.
func Secret(q ui.Question) Piece { return &secret{q: q} }

func (s *secret) Update(msg tea.Msg) (Piece, *Answer) {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		v := trimBreak(msg.Content)
		if hasControl(v) {
			s.reason = pasteRefused
			return s, nil
		}
		s.value = append(s.value, []rune(v)...)
	case tea.KeyPressMsg:
		if a := leaveOrQuit(s.q, msg.String()); a != nil {
			return s, a
		}
		switch msg.String() {
		case "enter":
			v := string(s.value)
			if err := ui.Storable(s.q.Prompt, v); err != nil {
				s.value, s.reason = nil, err.Error()
				return s, nil
			}
			return s, &Answer{Value: v}
		case "backspace":
			if n := len(s.value); n > 0 {
				s.value = s.value[:n-1]
			}
		default:
			if !hasControl(msg.Text) { // empty for ctrl+v and the other special keys
				s.value = append(s.value, []rune(msg.Text)...)
			}
		}
	}
	return s, nil
}

// trimBreak drops one trailing line break, as a copied line carries it: a
// terminal's paste often turns it into a lone "\r".
func trimBreak(v string) string {
	for _, b := range []string{"\r\n", "\n", "\r"} {
		if t, ok := strings.CutSuffix(v, b); ok {
			return t
		}
	}
	return v
}

func (s *secret) View(width, height int) string {
	lines := []string{s.q.Prompt, "typed hidden: nothing is shown; type or paste it, then enter"}
	if s.reason != "" {
		lines = append(lines, s.reason)
	}
	return fit(append(lines, footer(s.q, "enter done")), width, height, 1)
}
