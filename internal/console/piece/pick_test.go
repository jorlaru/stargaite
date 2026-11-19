package piece

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

// Keys as Bubble Tea decodes them (research: the key table).
var (
	keyEnter = tea.KeyPressMsg{Code: tea.KeyEnter}
	keyEsc   = tea.KeyPressMsg{Code: tea.KeyEscape}
	keyUp    = tea.KeyPressMsg{Code: tea.KeyUp}
	keyDown  = tea.KeyPressMsg{Code: tea.KeyDown}
	keyLeft  = tea.KeyPressMsg{Code: tea.KeyLeft}
	keyBack  = tea.KeyPressMsg{Code: tea.KeyBackspace}
	keyCtrlC = tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	keyCtrlV = tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl}
)

// send feeds msgs to p and fails on an answer before the last one.
func send(t *testing.T, p Piece, msgs ...tea.Msg) (Piece, *Answer) {
	t.Helper()
	var a *Answer
	for i, m := range msgs {
		if a != nil {
			t.Fatalf("answered %+v before message %d", *a, i)
		}
		p, a = p.Update(m)
	}
	return p, a
}

// typed is one key press per rune of s.
func typed(s string) []tea.Msg {
	var out []tea.Msg
	for _, r := range s {
		out = append(out, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return out
}

// fits fails unless view has at most height lines, none wider than width.
func fits(t *testing.T, view string, width, height int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	if len(lines) > height {
		t.Fatalf("%d lines, height %d:\n%s", len(lines), height, view)
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > width {
			t.Fatalf("line %q is %d wide, width %d", l, w, width)
		}
	}
}

func hundred(step string) ui.Question {
	q := ui.Question{ID: "model", Step: step, Prompt: "Which model?", Kind: ui.KindPick}
	for i := range 100 {
		id := fmt.Sprintf("vendor-%02d/model-%03d", i/10, i)
		q.Options = append(q.Options, ui.Option{Value: "v:" + id, Label: id})
	}
	return q
}

func TestPickFilterAndEnter(t *testing.T) {
	p, a := send(t, Pick(hundred("model")), append(typed("07/model-0"), keyEnter)...)
	if a == nil || a.Err != nil || a.Value != "v:vendor-07/model-070" {
		t.Fatalf("answer %+v; want v:vendor-07/model-070", a)
	}
	_ = p
}

func TestPickFilterCaseInsensitive(t *testing.T) {
	p, _ := send(t, Pick(hundred("model")), typed("VENDOR-09")...)
	v := p.View(80, 24)
	if !strings.Contains(v, "> vendor-09/model-090") || strings.Contains(v, "vendor-08") {
		t.Fatalf("view:\n%s", v)
	}
	p, _ = send(t, p, keyDown, typed("/")[0]) // a filter change puts the cursor on the first match
	if v := p.View(80, 24); !strings.Contains(v, "> vendor-09/model-090") {
		t.Fatalf("cursor not back on the first match:\n%s", v)
	}
	p, _ = send(t, p, keyDown, keyBack) // so does a Backspace
	if v := p.View(80, 24); !strings.Contains(v, "> vendor-09/model-090") {
		t.Fatalf("cursor not back on the first match after backspace:\n%s", v)
	}
}

func TestPickAt80x24(t *testing.T) {
	v := Pick(hundred("model")).View(80, 21)
	fits(t, v, 80, 21)
	if !strings.Contains(v, " more)") || !strings.Contains(v, "filter: type to filter") {
		t.Fatalf("view:\n%s", v)
	}
	if !strings.HasSuffix(v, "↑↓ move · type to filter · enter pick · esc menu · ctrl+c quit") {
		t.Fatalf("footer:\n%s", v)
	}
}

func TestPickWindowsAfterResize(t *testing.T) {
	var downs []tea.Msg
	for range 30 {
		downs = append(downs, keyDown)
	}
	p, _ := send(t, Pick(hundred("model")), downs...)
	want := "> vendor-03/model-030"
	for _, size := range [][2]int{{60, 9}, {100, 37}, {60, 9}, {20, 3}, {10, 1}} {
		v := p.View(size[0], size[1])
		fits(t, v, size[0], size[1])
		if size[0] >= 21 && !strings.Contains(v, want) {
			t.Fatalf("at %dx%d the selected row is hidden:\n%s", size[0], size[1], v)
		}
		if !strings.Contains(v, "> ") {
			t.Fatalf("at %dx%d no selected row:\n%s", size[0], size[1], v)
		}
	}
}

func TestPickEscAtHomeIgnored(t *testing.T) {
	p, a := send(t, Pick(hundred("")), keyEsc)
	if a != nil {
		t.Fatalf("esc at home answered %+v", *a)
	}
	if v := p.View(80, 24); strings.Contains(v, "esc menu") {
		t.Fatalf("home footer offers esc:\n%s", v)
	}
	_, a = send(t, p, keyEnter)
	if a == nil || a.Value != "v:vendor-00/model-000" {
		t.Fatalf("answer %+v", a)
	}
}

func TestPickEscInTaskLeaves(t *testing.T) {
	_, a := send(t, Pick(hundred("model")), keyEsc)
	if a == nil || !errors.Is(a.Err, ui.ErrLeave) {
		t.Fatalf("answer %+v; want ErrLeave", a)
	}
}

func TestPickCtrlCQuits(t *testing.T) {
	for _, step := range []string{"", "model"} {
		_, a := send(t, Pick(hundred(step)), keyCtrlC)
		if a == nil || !errors.Is(a.Err, ui.ErrQuit) {
			t.Fatalf("step %q: answer %+v; want ErrQuit", step, a)
		}
	}
}

func TestPickIgnoresPaste(t *testing.T) {
	p := Pick(hundred("model"))
	before := p.View(80, 24)
	p, a := send(t, p, tea.PasteMsg{Content: "vendor-05"}, keyCtrlV)
	if a != nil || p.View(80, 24) != before {
		t.Fatalf("paste changed the pick: %+v\n%s", a, p.View(80, 24))
	}
}

func TestPickNoMatch(t *testing.T) {
	p, _ := send(t, Pick(hundred("model")), typed("zz")...)
	v := p.View(80, 24)
	if !strings.Contains(v, `nothing matches "zz"`) || strings.Contains(v, "> ") {
		t.Fatalf("view:\n%s", v)
	}
	p, a := send(t, p, keyEnter, keyDown, keyUp)
	if a != nil {
		t.Fatalf("enter on no match answered %+v", *a)
	}
	_, a = send(t, p, keyBack, keyBack, keyEnter)
	if a == nil || a.Value != "v:vendor-00/model-000" {
		t.Fatalf("after clearing the filter: %+v", a)
	}
}

// unmarked is a line of a pick without its cursor mark.
func unmarked(l string) string { return strings.TrimPrefix(strings.TrimPrefix(l, "> "), "  ") }

// lineBefore is the line of view just above the row want, unmarked, or fails.
func lineBefore(t *testing.T, view, want string) string {
	t.Helper()
	lines := strings.Split(view, "\n")
	for i, l := range lines {
		if unmarked(l) == want && i > 0 {
			return unmarked(lines[i-1])
		}
	}
	t.Fatalf("no row %q in\n%s", want, view)
	return ""
}

func TestPickGroups(t *testing.T) {
	q := ui.Question{ID: "model", Step: "model", Prompt: "Which model?", Kind: ui.KindPick, Options: []ui.Option{
		{Value: "a1", Label: "a1", Group: "A"}, {Value: "a2", Label: "a2", Group: "A"},
		{Value: "b1", Label: "b1", Group: "B"}, {Value: "x", Label: "x"}}}
	v := Pick(q).View(80, 24)
	if lineBefore(t, v, "a1") != "── A" || lineBefore(t, v, "a2") != "a1" ||
		lineBefore(t, v, "b1") != "── B" || lineBefore(t, v, "x") != "b1" {
		t.Fatalf("headings:\n%s", v)
	}
	p, _ := send(t, Pick(q), keyDown)
	if v := p.View(80, 24); !strings.Contains(v, "> a2") {
		t.Fatalf("down from a1:\n%s", v)
	}
	if _, a := send(t, p, keyDown, keyEnter); a == nil || a.Value != "b1" {
		t.Fatalf("down from a2, enter: %+v; want b1, the heading skipped", a)
	}
	p, _ = send(t, Pick(q), typed("b")...)
	v = p.View(80, 24)
	if lineBefore(t, v, "b1") != "── B" || strings.Contains(v, "A") || strings.Contains(v, "a1") {
		t.Fatalf("filter b:\n%s", v)
	}
	// The filter reads the group as well as the label.
	q.Options = append(q.Options, ui.Option{Value: "q1", Label: "q1", Group: "Zed"})
	p, _ = send(t, Pick(q), typed("zed")...)
	if v = p.View(80, 24); lineBefore(t, v, "q1") != "── Zed" {
		t.Fatalf("filter zed:\n%s", v)
	}
}

func TestPickGroupsWindow(t *testing.T) {
	q := ui.Question{ID: "model", Step: "model", Prompt: "Which model?", Kind: ui.KindPick}
	for i := range 100 {
		id := fmt.Sprintf("model-%03d", i)
		q.Options = append(q.Options, ui.Option{Value: id, Label: id, Group: fmt.Sprintf("Vendor %d", i/10)})
	}
	var downs []tea.Msg
	for range 99 {
		downs = append(downs, keyDown)
	}
	p, _ := send(t, Pick(q), downs...)
	v := p.View(80, 20)
	fits(t, v, 80, 20)
	if !strings.Contains(v, "> model-099") {
		t.Fatalf("the cursor row is hidden:\n%s", v)
	}
	lines := strings.Split(v, "\n") // prompt, filter, then the window
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(lines[3]), "model-%d", &n); err != nil {
		t.Fatalf("line 3 is not a row (%v):\n%s", err, v)
	}
	if lines[2] != fmt.Sprintf("── Vendor %d", n/10) {
		t.Fatalf("the window's first row has no heading:\n%s", v)
	}
}

func TestPickMoves(t *testing.T) {
	_, a := send(t, Pick(hundred("model")), keyUp, keyDown, keyDown, keyUp, keyEnter)
	if a == nil || a.Value != "v:vendor-00/model-001" {
		t.Fatalf("answer %+v", a)
	}
	var downs []tea.Msg
	for range 150 {
		downs = append(downs, keyDown)
	}
	_, a = send(t, Pick(hundred("model")), append(downs, keyEnter)...)
	if a == nil || a.Value != "v:vendor-09/model-099" {
		t.Fatalf("answer %+v", a)
	}
}
