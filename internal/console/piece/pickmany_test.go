package piece

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

var keySpace = tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}

func manyQuestion(labels ...string) ui.Question {
	q := ui.Question{ID: "provider", Step: "which", Prompt: "Which providers?", Kind: ui.KindPickMany}
	for _, l := range labels {
		q.Options = append(q.Options, ui.Option{Value: l, Label: l})
	}
	return q
}

func TestPickManyToggle(t *testing.T) {
	p, a := send(t, PickMany(manyQuestion("a", "b", "c")), keySpace, keyDown, keySpace)
	if v := p.View(80, 24); a != nil || !strings.Contains(v, "  [x] a") || !strings.Contains(v, "> [x] b") || !strings.Contains(v, "  [ ] c") {
		t.Fatalf("answer %+v, view:\n%s", a, v)
	}
	if _, a := send(t, p, keyEnter); a == nil || a.Err != nil || a.Value != "a,b" {
		t.Fatalf("answer %+v; want a,b", a)
	}
	_, a = send(t, p, keyUp, keySpace, keyEnter) // a again: off
	if a == nil || a.Value != "b" {
		t.Fatalf("answer %+v; want b", a)
	}
	_, a = send(t, PickMany(manyQuestion("a", "b", "c")), keyDown, keyDown, keyEnter)
	if a == nil || a.Value != "c" {
		t.Fatalf("none toggled: answer %+v; want the cursor's c", a)
	}
}

func TestPickManyFilterKeepsToggles(t *testing.T) {
	msgs := []tea.Msg{keySpace}
	msgs = append(msgs, typed("anth")...)
	msgs = append(msgs, keySpace, keyBack, keyBack, keyBack, keyBack, keyEnter)
	_, a := send(t, PickMany(manyQuestion("openai", "gemini", "anthropic")), msgs...)
	if a == nil || a.Value != "openai,anthropic" {
		t.Fatalf("answer %+v; want openai,anthropic in option order", a)
	}
}

func TestPickManyFits(t *testing.T) {
	var labels []string
	for i := range 19 {
		labels = append(labels, fmt.Sprintf("provider-%02d, untested, asks for its URL", i))
	}
	var downs []tea.Msg
	for range 18 {
		downs = append(downs, keyDown)
	}
	p, _ := send(t, PickMany(manyQuestion(labels...)), downs...)
	for _, size := range [][2]int{{80, 20}, {60, 8}} { // the piece's share of 80x24 and 60x12
		v := p.View(size[0], size[1])
		fits(t, v, size[0], size[1])
		if !strings.Contains(v, "> [ ] provider-18") {
			t.Fatalf("at %dx%d the cursor row is hidden:\n%s", size[0], size[1], v)
		}
	}
	if v := PickMany(manyQuestion(labels...)).View(80, 20); !strings.HasSuffix(v, "↑↓ move · space select · type to filter · enter done · esc menu · ctrl+c quit") {
		t.Fatalf("footer:\n%s", v)
	}
}

func TestPickManyIgnoresPaste(t *testing.T) {
	p := PickMany(manyQuestion("openai", "anthropic"))
	before := p.View(80, 24)
	p, a := send(t, p, tea.PasteMsg{Content: "anth"})
	if a != nil || p.View(80, 24) != before {
		t.Fatalf("paste changed the pick: %+v\n%s", a, p.View(80, 24))
	}
	if _, a = send(t, p, keyEnter); a == nil || a.Value != "openai" {
		t.Fatalf("answer %+v; want openai, nothing toggled", a)
	}
}

func TestPickManyEscAndCtrlC(t *testing.T) {
	if _, a := send(t, PickMany(manyQuestion("a")), keyEsc); a == nil || a.Err != ui.ErrLeave {
		t.Fatalf("esc: %+v", a)
	}
	if _, a := send(t, PickMany(manyQuestion("a")), keyCtrlC); a == nil || a.Err != ui.ErrQuit {
		t.Fatalf("ctrl+c: %+v", a)
	}
}
