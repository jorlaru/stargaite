package piece

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

func workQuestion() ui.Question {
	return ui.Question{ID: "up", Step: "recreate", Prompt: "restarting", Kind: ui.KindWork}
}

func TestWorkKeys(t *testing.T) {
	if v := Work(workQuestion()).View(80, 20); v != "restarting…\nesc stop · ctrl+c quit" {
		t.Fatalf("view %q", v)
	}
	if _, a := send(t, Work(workQuestion()), keyEsc); a == nil || !errors.Is(a.Err, ui.ErrLeave) {
		t.Fatalf("esc: %+v", a)
	}
	if _, a := send(t, Work(workQuestion()), keyCtrlC); a == nil || !errors.Is(a.Err, ui.ErrQuit) {
		t.Fatalf("ctrl+c: %+v", a)
	}
	if _, a := send(t, Work(workQuestion()), typed("a")[0], keyEnter, tea.PasteMsg{Content: "x"}); a != nil {
		t.Fatalf("answered %+v", *a)
	}
}
