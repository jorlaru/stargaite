package piece

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

func applyQuestion(def string) ui.Question {
	return ui.Question{ID: "apply", Step: "apply", Prompt: "Apply?", Kind: ui.KindConfirm,
		Options: []ui.Option{{Value: "yes", Label: "Yes"}, {Value: "no", Label: "No"}}, Default: def}
}

func TestConfirmDefaultNo(t *testing.T) {
	p := Confirm(applyQuestion(""))
	v := p.View(80, 24)
	if !strings.Contains(v, "> No") || !strings.Contains(v, "  Yes") ||
		!strings.HasSuffix(v, "↑↓ move · enter pick · esc menu · ctrl+c quit") {
		t.Fatalf("view:\n%s", v)
	}
	_, a := send(t, p, keyEnter)
	if a == nil || a.Value != "no" {
		t.Fatalf("answer %+v; want no", a)
	}
	_, a = send(t, Confirm(applyQuestion("")), keyUp, keyEnter)
	if a == nil || a.Value != "yes" {
		t.Fatalf("up, enter: %+v; want yes", a)
	}
}

func TestConfirmDefaultYes(t *testing.T) {
	_, a := send(t, Confirm(applyQuestion("yes")), keyEnter)
	if a == nil || a.Value != "yes" {
		t.Fatalf("answer %+v; want yes", a)
	}
	_, a = send(t, Confirm(applyQuestion("yes")), keyDown, keyDown, keyEnter)
	if a == nil || a.Value != "no" {
		t.Fatalf("down, enter: %+v; want no", a)
	}
}

func TestConfirmIgnoresPaste(t *testing.T) {
	p := Confirm(applyQuestion(""))
	before := p.View(80, 24)
	p, a := send(t, p, tea.PasteMsg{Content: "yes\n"}, typed("y")[0], keyCtrlV)
	if a != nil || p.View(80, 24) != before {
		t.Fatalf("paste or typing changed the confirm: %+v\n%s", a, p.View(80, 24))
	}
}

func TestConfirmEscAndCtrlC(t *testing.T) {
	_, a := send(t, Confirm(applyQuestion("")), keyEsc)
	if a == nil || !errors.Is(a.Err, ui.ErrLeave) {
		t.Fatalf("esc: %+v", a)
	}
	_, a = send(t, Confirm(applyQuestion("")), keyCtrlC)
	if a == nil || !errors.Is(a.Err, ui.ErrQuit) {
		t.Fatalf("ctrl+c: %+v", a)
	}
	v := Confirm(applyQuestion("")).View(80, 2)
	fits(t, v, 80, 2)
	if !strings.Contains(v, "> No") {
		t.Fatalf("at height 2 the cursor row is hidden:\n%s", v)
	}
}
