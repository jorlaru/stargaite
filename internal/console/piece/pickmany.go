package piece

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

// pickMany is a pick whose rows Space toggles; a filter change keeps them.
type pickMany struct {
	pick
	on map[string]bool // by value
}

// PickMany asks for one or more of q.Options: the toggled values in option
// order, joined by ",", or the cursor's row when none is toggled.
func PickMany(q ui.Question) Piece { return &pickMany{pick: pick{q: q}, on: map[string]bool{}} }

func (p *pickMany) Update(msg tea.Msg) (Piece, *Answer) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "space":
			if ms := p.matches(); len(ms) > 0 {
				v := ms[p.cursor].Value
				p.on[v] = !p.on[v]
			}
			return p, nil
		case "enter":
			if a := p.toggled(); a != "" {
				return p, &Answer{Value: a}
			}
		}
	}
	next, a := p.pick.Update(msg)
	p.pick = next.(pick)
	return p, a
}

// toggled is the toggled values in option order, joined by ",".
func (p *pickMany) toggled() string {
	var vs []string
	for _, o := range p.q.Options {
		if p.on[o.Value] {
			vs = append(vs, o.Value)
		}
	}
	return strings.Join(vs, ",")
}

func (p *pickMany) View(width, height int) string {
	return p.view(width, height, func(o ui.Option) string {
		if p.on[o.Value] {
			return "[x] " + o.Label
		}
		return "[ ] " + o.Label
	}, "↑↓ move · space select · type to filter · enter done")
}
