package piece

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

// pick is one of q.Options, filtered by typing, windowed to the height.
type pick struct {
	q      ui.Question
	filter string
	cursor int // index into matches()
}

// Pick asks for one of q.Options.
func Pick(q ui.Question) Piece { return pick{q: q} }

// matches is the options whose label holds the filter, ignoring case.
func (p pick) matches() []ui.Option {
	f := strings.ToLower(p.filter)
	var out []ui.Option
	for _, o := range p.q.Options {
		if strings.Contains(strings.ToLower(o.Label), f) {
			out = append(out, o)
		}
	}
	return out
}

func (p pick) Update(msg tea.Msg) (Piece, *Answer) {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return p, nil // a paste is never a pick
	}
	if a := leaveOrQuit(p.q, k.String()); a != nil {
		return p, a
	}
	ms := p.matches()
	switch k.String() {
	case "enter":
		if len(ms) > 0 {
			return p, &Answer{Value: ms[p.cursor].Value}
		}
	case "up":
		p.cursor = max(0, p.cursor-1)
	case "down":
		p.cursor = max(0, min(len(ms)-1, p.cursor+1))
	case "backspace":
		if r := []rune(p.filter); len(r) > 0 {
			p.filter, p.cursor = string(r[:len(r)-1]), 0
		}
	default:
		if k.Text != "" {
			p.filter, p.cursor = p.filter+k.Text, 0
		}
	}
	return p, nil
}

func (p pick) View(width, height int) string {
	return p.view(width, height, func(o ui.Option) string { return o.Label }, "↑↓ move · type to filter · enter pick")
}

// view draws the prompt, the filter, the window of matches, each as row
// draws it, and the footer with keys.
func (p pick) view(width, height int, row func(ui.Option) string, keys string) string {
	f := p.filter
	if f == "" {
		f = "type to filter"
	}
	lines := []string{p.q.Prompt, "filter: " + ui.OneLine(f)}
	ms := p.matches()
	if len(ms) == 0 {
		lines = append(lines, fmt.Sprintf("nothing matches %q", ui.OneLine(p.filter)))
	}
	rows := max(1, height-4) // prompt, filter, "(n more)", footer
	off := max(0, p.cursor-rows+1)
	keep := len(lines)
	for i := off; i < len(ms) && i < off+rows; i++ {
		mark := "  "
		if i == p.cursor {
			mark, keep = "> ", len(lines)
		}
		lines = append(lines, mark+row(ms[i]))
	}
	if hidden := len(ms) - min(rows, len(ms)); hidden > 0 {
		lines = append(lines, fmt.Sprintf("(%d more)", hidden))
	}
	lines = append(lines, footer(p.q, keys))
	return fit(lines, width, height, keep)
}
