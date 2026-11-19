package piece

import (
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

const prompt = "> "

// text is a hand-written single-line input (docs/design/phase-7.md section
// 3): no clipboard, and a paste only when it is one line.
type text struct {
	q     ui.Question
	value []rune
	pos   int // insertion point, in runes
}

// Text asks for a line of text.
func Text(q ui.Question) Piece { return &text{q: q} }

func (t *text) Update(msg tea.Msg) (Piece, *Answer) {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		t.insert(msg.Content)
	case tea.KeyPressMsg:
		if a := leaveOrQuit(t.q, msg.String()); a != nil {
			return t, a
		}
		switch msg.String() {
		case "enter":
			return t, &Answer{Value: string(t.value)}
		case "backspace":
			if t.pos > 0 {
				t.value = append(t.value[:t.pos-1], t.value[t.pos:]...)
				t.pos--
			}
		case "left":
			t.pos = max(0, t.pos-1)
		case "right":
			t.pos = min(len(t.value), t.pos+1)
		default:
			t.insert(msg.Text) // empty for ctrl+v and the other special keys
		}
	}
	return t, nil
}

// insert adds s at the insertion point, unless it holds a control: a rune
// below U+0020 (a paste of several lines among them), DEL or a C1 control,
// which a terminal could read as the start of a sequence.
func (t *text) insert(s string) {
	r := []rune(s)
	for _, c := range r {
		if c < 0x20 || (c >= 0x7f && c <= 0x9f) {
			return
		}
	}
	t.value = append(t.value[:t.pos], append(r, t.value[t.pos:]...)...)
	t.pos += len(r)
}

// line is the input row as shown in width cells and the cursor's column in
// it. When it is wider than width-1, "…" stands for the cells cut on the
// left; the cut ends at the line's end, or earlier to keep the cursor shown.
func (t *text) line(width int) (string, int) {
	full := prompt + string(t.value)
	x := ansi.StringWidth(prompt + string(t.value[:t.pos]))
	w, room := ansi.StringWidth(full), width-2 // the cursor's last column, and "…"
	if w <= width-1 || room < 1 {
		return full, min(x, max(0, width-1))
	}
	start := min(w-room, x)
	if ansi.StringWidth(ansi.Cut(full, start, start+room)) > room {
		start++ // the cut fell inside a wide character, which Cut keeps whole
	}
	return "…" + ansi.Cut(full, start, start+room), 1 + x - start
}

func (t *text) View(width, height int) string {
	p := t.q.Prompt
	if t.q.HasDefault {
		p += " [" + ui.OneLine(t.q.Default) + "]"
	}
	in, _ := t.line(width)
	return fit([]string{p, in, footer(t.q, "enter accept")}, width, height, 1)
}

// Cursor is the insertion point's display column on the input row: row 1,
// below the prompt, or row 0 when fit keeps only the input row and the footer.
func (t *text) Cursor(width, height int) (x, y int) {
	_, x = t.line(width)
	if height < 3 {
		return x, 0
	}
	return x, 1
}
