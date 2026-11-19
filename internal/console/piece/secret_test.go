package piece

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

func secretQuestion() ui.Question {
	return ui.Question{ID: "key-env", Step: "key", Prompt: "ACME_API_KEY", Kind: ui.KindSecret}
}

// hidden fails if view draws anything of a value: its parts or a mask.
func hidden(t *testing.T, view string) {
	t.Helper()
	for _, s := range []string{"sk-", "typed-1", "#\\", "**"} {
		if strings.Contains(view, s) {
			t.Fatalf("the view holds %q:\n%s", s, view)
		}
	}
}

func TestSecretNeverDrawn(t *testing.T) {
	p := Secret(secretQuestion())
	v := p.View(80, 20)
	if !strings.Contains(v, "ACME_API_KEY\ntyped hidden: nothing is shown; type or paste it, then enter\n") ||
		!strings.HasSuffix(v, "enter done · esc menu · ctrl+c quit") {
		t.Fatalf("view:\n%s", v)
	}
	var a *Answer
	for _, m := range append(typed("sk-typed-1"), tea.PasteMsg{Content: "$ #\\z"}) {
		if p, a = p.Update(m); a != nil {
			t.Fatalf("answered %+v before enter", *a)
		}
		hidden(t, p.View(80, 20))
		if got := p.View(80, 20); got != v {
			t.Fatalf("the view changed with the value:\n%s", got)
		}
	}
	if _, a = send(t, p, keyEnter); a == nil || a.Err != nil || a.Value != "sk-typed-1$ #\\z" {
		t.Fatalf("answer %+v", a)
	}
}

func TestSecretBackspace(t *testing.T) {
	_, a := send(t, Secret(secretQuestion()), append(typed("ab日"), keyBack, keyEnter)...)
	if a == nil || a.Value != "ab" {
		t.Fatalf("answer %+v; want ab", a)
	}
}

func TestSecretRefusesTrailingBackslash(t *testing.T) {
	p, a := send(t, Secret(secretQuestion()), tea.PasteMsg{Content: "$ #\\"}, keyEnter)
	if a != nil || !strings.Contains(p.View(80, 20), ".env cannot hold it") {
		t.Fatalf("answer %+v, view:\n%s", a, p.View(80, 20))
	}
	if _, a = send(t, p, keyEnter); a != nil || !strings.Contains(p.View(80, 20), "ACME_API_KEY is empty") {
		t.Fatalf("the refused value was kept: %+v", a)
	}
}

func TestSecretPasteTrailingNewline(t *testing.T) {
	for _, paste := range []string{"sk-x\n", "sk-x\r\n", "sk-x\r"} {
		p, a := send(t, Secret(secretQuestion()), tea.PasteMsg{Content: paste})
		if a != nil {
			t.Fatalf("%q: answered before enter: %+v", paste, *a)
		}
		hidden(t, p.View(80, 20))
		if _, a = send(t, p, keyEnter); a == nil || a.Err != nil || a.Value != "sk-x" {
			t.Fatalf("%q: answer %+v; want sk-x", paste, a)
		}
	}
}

func TestSecretRefusesMultiline(t *testing.T) {
	for _, paste := range []string{"a\nb", "a\n\n", "a\x1bb", "a\u009bb", "a\x7fb"} {
		p, a := send(t, Secret(secretQuestion()), tea.PasteMsg{Content: paste})
		if a != nil || !strings.Contains(p.View(80, 20), "a pasted value with a line break or a control character is refused") {
			t.Fatalf("%q: answer %+v, view:\n%s", paste, a, p.View(80, 20))
		}
		if _, a = send(t, p, keyEnter); a != nil || !strings.Contains(p.View(80, 20), "ACME_API_KEY is empty") {
			t.Fatalf("%q: the refused paste was kept: %+v\n%s", paste, a, p.View(80, 20))
		}
	}
}

func TestSecretRefusesQuote(t *testing.T) {
	p, a := send(t, Secret(secretQuestion()), append(typed("it's"), keyEnter)...)
	if a != nil || !strings.Contains(p.View(80, 20), ".env cannot hold it") {
		t.Fatalf("answer %+v, view:\n%s", a, p.View(80, 20))
	}
	hidden(t, p.View(80, 20))
}

func TestSecretCtrlV(t *testing.T) {
	p, _ := send(t, Secret(secretQuestion()), append(typed("x"), keyCtrlV)...)
	if _, a := send(t, p, keyEnter); a == nil || a.Value != "x" {
		t.Fatalf("answer %+v; want x", a)
	}
}

func TestSecretEscAndCtrlC(t *testing.T) {
	if _, a := send(t, Secret(secretQuestion()), append(typed("x"), keyEsc)...); a == nil || !errors.Is(a.Err, ui.ErrLeave) || a.Value != "" {
		t.Fatalf("esc: %+v", a)
	}
	if _, a := send(t, Secret(secretQuestion()), keyCtrlC); a == nil || !errors.Is(a.Err, ui.ErrQuit) {
		t.Fatalf("ctrl+c: %+v", a)
	}
}
