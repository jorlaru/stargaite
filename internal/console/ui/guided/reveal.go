package guided

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jorlaru/stargaite/internal/console/piece"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// reveal shows a value once, on the alternate screen, so it leaves no copy
// in the terminal's scrollback (docs/design/phase-7.md section 3, "The
// client key"). The root model switches screens while it is the piece.
type reveal struct {
	q       ui.Question
	warned  bool      // Ctrl-C was pressed once
	created time.Time // when it was made
}

// Update answers "" on Enter, but an Enter earlier than piece.EnterWait
// after the reveal was made is ignored, as the paid step's is, so a late
// Enter meant for the question before cannot dismiss an unseen value. Esc
// and paste do nothing; the first Ctrl-C warns that the value will not be
// shown again, the second quits.
func (r *reveal) Update(msg tea.Msg) (piece.Piece, *piece.Answer) {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return r, nil
	}
	switch k.String() {
	case "enter":
		if time.Since(r.created) >= piece.EnterWait {
			return r, &piece.Answer{}
		}
	case "ctrl+c":
		if r.warned {
			return r, &piece.Answer{Err: ui.ErrQuit}
		}
		r.warned = true
	}
	return r, nil
}

// View shows the value verbatim: it is the one text no front cleans. It is
// wrapped to the width, since the renderer would cut it without a mark, and
// a wrapped value says so: a selection across its rows copies the breaks.
func (r *reveal) View(width, _ int) string {
	v := ansi.Hardwrap(r.q.Value, max(1, width-2), true)
	s := r.q.Prompt + "\n\n  " + strings.ReplaceAll(v, "\n", "\n  ")
	if n := strings.Count(v, "\n") + 1; n > 1 {
		s += "\n" + ansi.Wordwrap(fmt.Sprintf("one value on %d lines: copy it without the line breaks or spaces", n), max(1, width), "")
	}
	s += "\n\nenter: I have stored it"
	if r.warned {
		s += "\nctrl+c again quits; it will not be shown again"
	}
	return s
}
