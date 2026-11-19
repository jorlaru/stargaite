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

// matches is the options whose group and label hold the filter, ignoring
// case.
func (p pick) matches() []ui.Option {
	f := strings.ToLower(p.filter)
	var out []ui.Option
	for _, o := range p.q.Options {
		if strings.Contains(strings.ToLower(o.Group+" "+o.Label), f) {
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
// draws it under its group's heading, and the footer with keys.
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
	off := p.firstRow(ms, rows)
	keep, used, shown := len(lines), 0, 0
	for i := off; i < len(ms); i++ {
		n := rowLines(ms, off, i)
		if used+n > rows && i > p.cursor {
			break
		}
		if n == 2 {
			lines = append(lines, "── "+ms[i].Group)
		}
		mark := "  "
		if i == p.cursor {
			mark, keep = "> ", len(lines)
		}
		lines = append(lines, mark+row(ms[i]))
		used, shown = used+n, shown+1
	}
	if hidden := len(ms) - shown; hidden > 0 {
		lines = append(lines, fmt.Sprintf("(%d more)", hidden))
	}
	lines = append(lines, footer(p.q, keys))
	return fit(lines, width, height, keep)
}

// firstRow is the window's first match: the lowest from which the rows down
// to the cursor's, headings included, fit in rows lines.
func (p pick) firstRow(ms []ui.Option, rows int) int {
	off := p.cursor
	for below := 0; off > 0; off-- {
		below += rowLines(ms, -1, off) // off's lines with a row drawn above it
		if below+rowLines(ms, off-1, off-1) > rows {
			break
		}
	}
	return off
}

// rowLines is how many lines match i takes in a window whose first match is
// off: two when its group's heading is drawn above it, above the window's
// first row or where the group changes, else one.
func rowLines(ms []ui.Option, off, i int) int {
	if ms[i].Group != "" && (i == off || ms[i].Group != ms[i-1].Group) {
		return 2
	}
	return 1
}
