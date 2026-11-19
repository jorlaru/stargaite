package piece

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

func nameQuestion() ui.Question {
	return ui.Question{ID: "name", Step: "name", Prompt: "Name?", Kind: ui.KindText}
}

// cursor is the text piece's cursor in a view of width x height; it fails
// if p is not a Cursorer.
func cursor(t *testing.T, p Piece, width, height int) (int, int) {
	t.Helper()
	c, ok := p.(Cursorer)
	if !ok {
		t.Fatalf("%T is not a Cursorer", p)
	}
	return c.Cursor(width, height)
}

func TestTextWideCursor(t *testing.T) {
	p, _ := send(t, Text(nameQuestion()), typed("日本語")...)
	if x, y := cursor(t, p, 80, 24); x != 8 || y != 1 {
		t.Fatalf("cursor (%d, %d); want (8, 1)", x, y)
	}
	if v := p.View(80, 24); !strings.Contains(v, "> 日本語") {
		t.Fatalf("view:\n%s", v)
	}
	// Below three rows fit drops the prompt: the input is row 0.
	if x, y := cursor(t, p, 80, 2); x != 8 || y != 0 {
		t.Fatalf("cursor at height 2 (%d, %d); want (8, 0)", x, y)
	}
	if v := p.View(80, 2); !strings.HasPrefix(v, "> 日本語") {
		t.Fatalf("view at height 2:\n%s", v)
	}
	_, a := send(t, p, keyEnter)
	if a == nil || a.Err != nil || a.Value != "日本語" {
		t.Fatalf("answer %+v", a)
	}
}

func TestTextBackspace(t *testing.T) {
	p, _ := send(t, Text(nameQuestion()), append(typed("日本語"), keyBack)...)
	if x, _ := cursor(t, p, 80, 24); x != 6 {
		t.Fatalf("cursor x %d; want 6", x)
	}
	p, _ = send(t, p, keyLeft, keyBack, keyBack) // the rune before the insertion point; none left at the start
	_, a := send(t, p, keyEnter)
	if a == nil || a.Value != "本" {
		t.Fatalf("answer %+v; want 本", a)
	}
}

func TestTextLeftRight(t *testing.T) {
	right := tea.KeyPressMsg{Code: tea.KeyRight}
	msgs := append(typed("ac"), keyLeft, typed("b")[0], right, right, typed("d")[0])
	_, a := send(t, Text(nameQuestion()), append(msgs, keyEnter)...)
	if a == nil || a.Value != "abcd" {
		t.Fatalf("answer %+v; want abcd", a)
	}
}

func TestTextDefaultShown(t *testing.T) {
	q := nameQuestion()
	q.Default, q.HasDefault = "main", true
	if v := Text(q).View(80, 24); !strings.Contains(v, "Name? [main]") {
		t.Fatalf("view:\n%s", v)
	}
	q.Default = "a\x1b[2Jb"
	v := Text(q).View(80, 24)
	if !strings.Contains(v, "[a[2Jb]") || strings.Contains(v, "\x1b") {
		t.Fatalf("default not cleaned: %q", v)
	}
	q.Default = "" // an empty default (Enter for none) shows no empty brackets
	if v := Text(q).View(80, 24); !strings.HasPrefix(v, "Name?\n") || strings.Contains(v, "[]") {
		t.Fatalf("empty default shown: %q", v)
	}
}

func TestTextPasteMultiLineIgnored(t *testing.T) {
	p, _ := send(t, Text(nameQuestion()), tea.PasteMsg{Content: "a\nb"}, tea.PasteMsg{Content: "a\u009bb"},
		tea.PasteMsg{Content: "a\x7fb"}, tea.PasteMsg{Content: "yz"})
	_, a := send(t, p, keyEnter)
	if a == nil || a.Value != "yz" {
		t.Fatalf("answer %+v; want yz (the one-line paste only)", a)
	}
}

func TestTextCtrlVNothing(t *testing.T) {
	p, _ := send(t, Text(nameQuestion()), append(typed("x"), keyCtrlV)...)
	_, a := send(t, p, keyEnter)
	if a == nil || a.Value != "x" {
		t.Fatalf("answer %+v; want x", a)
	}
}

func TestTextLongValueTruncatesLeft(t *testing.T) {
	p, _ := send(t, Text(nameQuestion()), typed(strings.Repeat("abcdefghij", 5))...)
	v := p.View(20, 24)
	fits(t, v, 20, 24)
	line := strings.Split(v, "\n")[1]
	if !strings.HasPrefix(line, "…") || !strings.HasSuffix(line, "j") {
		t.Fatalf("input line %q", line)
	}
	if x, _ := cursor(t, p, 20, 24); x > 19 {
		t.Fatalf("cursor x %d past width 20", x)
	}
	var lefts []tea.Msg
	for range 45 {
		lefts = append(lefts, keyLeft)
	}
	p, _ = send(t, p, lefts...) // the insertion point stays on screen
	line = strings.Split(p.View(20, 24), "\n")[1]
	x, _ := cursor(t, p, 20, 24)
	if x < 1 || x > 19 || !strings.HasPrefix(line, "…") || !strings.HasPrefix(line[len("…"):][x-1:], "fghij") {
		t.Fatalf("cursor x %d in %q", x, line)
	}
	// Wide characters: a cut that would fall inside one starts after it, so
	// the line fits and the cursor sits right after the text.
	p, _ = send(t, Text(nameQuestion()), typed(strings.Repeat("日", 30))...)
	for width := 20; width <= 23; width++ {
		line := strings.Split(p.View(width, 24), "\n")[1]
		x, _ := cursor(t, p, width, 24)
		if w := ansi.StringWidth(line); w > width-1 || x != w {
			t.Fatalf("width %d: line %q is %d cells, cursor x %d", width, line, w, x)
		}
	}
}

func TestTextEscAndCtrlC(t *testing.T) {
	_, a := send(t, Text(nameQuestion()), keyEsc)
	if a == nil || !errors.Is(a.Err, ui.ErrLeave) {
		t.Fatalf("esc: %+v", a)
	}
	_, a = send(t, Text(nameQuestion()), keyCtrlC)
	if a == nil || !errors.Is(a.Err, ui.ErrQuit) {
		t.Fatalf("ctrl+c: %+v", a)
	}
	home := nameQuestion()
	home.Step = ""
	if _, a = send(t, Text(home), keyEsc); a != nil {
		t.Fatalf("esc at home: %+v", *a)
	}
}

func TestTextFooter(t *testing.T) {
	v := Text(nameQuestion()).View(80, 24)
	if !strings.HasSuffix(v, "enter accept · esc menu · ctrl+c quit") || len(strings.Split(v, "\n")) != 3 {
		t.Fatalf("view:\n%s", v)
	}
}
