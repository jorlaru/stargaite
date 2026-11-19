package piece

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

// paidQuestion is a check's paid step, as ui.Paid hands it to a front.
func paidQuestion() ui.Question {
	return ui.Question{ID: "check", Step: "check", Prompt: "3 paid call(s) in all", Kind: ui.KindPaid,
		Options: []ui.Option{{Value: "check", Label: "Check it"}, {Value: "not now", Label: "Not now"}}}
}

// clock is a fake time, moved by the test.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// paidOn is a paid piece made at the clock's time.
func paidOn(c *clock) Piece { return paidAt(paidQuestion(), c.now) }

func TestPaidWaits(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	start := c.t
	p := paidOn(c)
	for _, at := range []time.Duration{0, 416 * time.Millisecond} {
		c.t = start.Add(at)
		var a *Answer
		if p, a = p.Update(keyEnter); a != nil {
			t.Fatalf("Enter at %s answered %+v", at, *a)
		}
	}
	c.t = start.Add(417 * time.Millisecond)
	if _, a := p.Update(keyEnter); a == nil || a.Err != nil || a.Value != "check" {
		t.Fatalf("Enter at 417ms: %+v; want check", a)
	}
	c2 := &clock{t: start}
	p, a := send(t, paidOn(c2), keyDown)
	if a != nil {
		t.Fatalf("down answered %+v", *a)
	}
	c2.t = start.Add(time.Second)
	if _, a := p.Update(keyEnter); a == nil || a.Value != "not now" {
		t.Fatalf("down, Enter past the wait: %+v; want not now", a)
	}
}

func TestPaidSameBatchEnter(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	if _, a := paidOn(c).Update(keyEnter); a != nil {
		t.Fatalf("an Enter at the reading the piece was made at answered %+v", *a)
	}
}

func TestPaidIgnoresPaste(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	p := paidOn(c)
	c.t = c.t.Add(time.Second)
	before := p.View(80, 24)
	p, a := send(t, p, tea.PasteMsg{Content: "\r"}, tea.PasteMsg{Content: "check\n"})
	if a != nil || p.View(80, 24) != before {
		t.Fatalf("a paste changed the paid step: %+v\n%s", a, p.View(80, 24))
	}
}

func TestPaidEscAndCtrlC(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	if _, a := paidOn(c).Update(keyEsc); a == nil || !errors.Is(a.Err, ui.ErrLeave) {
		t.Fatalf("esc: %+v; want ErrLeave", a)
	}
	if _, a := paidOn(c).Update(keyCtrlC); a == nil || !errors.Is(a.Err, ui.ErrQuit) {
		t.Fatalf("ctrl+c: %+v; want ErrQuit", a)
	}
}

func TestPaidFits(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	p := paidOn(c)
	v := p.View(80, 24)
	if v != "3 paid call(s) in all\n> Check it\n  Not now\n↑↓ move · enter pick · esc menu · ctrl+c quit" {
		t.Fatalf("view:\n%s", v)
	}
	p, _ = send(t, p, keyDown)
	for _, size := range [][2]int{{80, 24}, {20, 3}} {
		v := p.View(size[0], size[1])
		fits(t, v, size[0], size[1])
		if !strings.Contains(v, "> Not now") {
			t.Fatalf("at %dx%d the cursor row is hidden:\n%s", size[0], size[1], v)
		}
	}
}
