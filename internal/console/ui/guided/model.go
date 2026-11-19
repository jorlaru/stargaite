// Package guided is the console's front at a terminal: a Bubble Tea root
// model that draws the question a session goroutine asks, and a bridge of
// messages between the two (docs/design/phase-7.md section 5). The model
// only draws and answers; the work stays on the session goroutine.
package guided

import (
	"errors"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jorlaru/stargaite/internal/console/piece"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// releaseWait is how long lines said after a reveal are held, so the frame
// that leaves the alternate screen is drawn before they are printed: Bubble
// Tea prints a line at once, on whichever screen was drawn last (research:
// RB section 0.6), and draws a frame at most 1/60 s after it changes.
const releaseWait = 100 * time.Millisecond

// Messages from the bridge.
type (
	result struct {
		text string
		err  error
	}
	askMsg struct {
		q     ui.Question
		reply chan<- result // buffered (1): the model never blocks on it
	}
	cancelMsg struct{ reply chan<- result }
	sayMsg    struct {
		text string
		done chan struct{} // closed once the line is printed
	}
	doneMsg    struct{}
	releaseMsg struct{}
)

// Model is the root model: a title, the step bar, and the asked piece.
type Model struct {
	version       string
	width, height int
	ask           *askMsg     // the question asked, nil while the session works
	piece         piece.Piece // its piece
	holding       bool        // a reveal is shown, or just ended: lines wait
	held          []sayMsg
	interrupted   bool
}

// NewModel returns the root model, titled with the console's version.
func NewModel(version string) Model {
	return Model{version: ui.OneLine(version), width: 80, height: 24}
}

// Interrupted reports whether Ctrl-C was pressed.
func (m Model) Interrupted() bool { return m.interrupted }

// Init implements tea.Model.
func (m Model) Init() tea.Cmd { return nil }

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case askMsg:
		m.ask = &msg
		m.piece = newPiece(msg.q)
		if msg.q.Kind == ui.KindReveal {
			m.holding = true
		}
	case cancelMsg:
		if m.ask != nil && m.ask.reply == msg.reply {
			return m.answered()
		}
	case sayMsg:
		if m.holding {
			m.held = append(m.held, msg)
			return m, nil
		}
		return m, printLine(msg)
	case releaseMsg:
		if _, revealing := m.piece.(*reveal); revealing {
			return m, nil // a new reveal began: its end releases them
		}
		cmds := make([]tea.Cmd, len(m.held))
		for i, s := range m.held {
			cmds[i] = printLine(s)
		}
		m.holding, m.held = false, nil
		return m, tea.Sequence(cmds...)
	case doneMsg:
		return m, tea.Quit
	case tea.KeyPressMsg:
		if m.piece == nil {
			if msg.String() == "ctrl+c" {
				m.interrupted = true
				return m, tea.Quit
			}
			return m, nil
		}
		return m.update(msg)
	case tea.PasteMsg:
		if m.piece != nil {
			return m.update(msg)
		}
	}
	return m, nil
}

// update hands msg to the piece and replies when it answers. Ctrl-C quits
// the program as well as answering ui.ErrQuit.
func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	p, a := m.piece.Update(msg)
	m.piece = p
	if a == nil {
		return m, nil
	}
	m.ask.reply <- result{text: a.Value, err: a.Err}
	if errors.Is(a.Err, ui.ErrQuit) {
		m.interrupted = true
		m.ask, m.piece = nil, nil
		return m, tea.Quit
	}
	return m.answered()
}

// answered clears the question; after a reveal, the held lines are released
// once the frame that leaves the alternate screen has been drawn.
func (m Model) answered() (tea.Model, tea.Cmd) {
	_, revealed := m.piece.(*reveal)
	m.ask, m.piece = nil, nil
	if revealed {
		return m, tea.Tick(releaseWait, func(time.Time) tea.Msg { return releaseMsg{} })
	}
	return m, nil
}

// printLine prints a said line above the frame, then tells the bridge, so the
// session's next message comes after it.
func printLine(s sayMsg) tea.Cmd {
	return tea.Sequence(tea.Println(s.text), func() tea.Msg { close(s.done); return nil })
}

// newPiece is the piece that asks q.
func newPiece(q ui.Question) piece.Piece {
	switch q.Kind {
	case ui.KindText:
		return piece.Text(q)
	case ui.KindConfirm:
		return piece.Confirm(q)
	case ui.KindReveal:
		return &reveal{q: q}
	}
	return piece.Pick(q)
}

// View implements tea.Model. The content always ends with "\n", so the last
// line stays when the program exits (research: RB section 0.5).
func (m Model) View() tea.View {
	lines := []string{ansi.Truncate("StargAIte console "+m.version, m.width, "…")}
	if m.ask != nil && m.ask.q.Bar.Current >= 0 {
		lines = append(lines, stepBar(m.ask.q.Bar, m.width))
	}
	lines = append(lines, "")
	var v tea.View
	if m.piece == nil {
		lines = append(lines, "working…")
	} else {
		used := len(lines)
		lines = append(lines, m.piece.View(m.width, m.height-used-1))
		if c, ok := m.piece.(piece.Cursorer); ok {
			x, y := c.Cursor(m.width, m.height-used-1)
			v.Cursor = tea.NewCursor(x, y+used)
		}
		_, v.AltScreen = m.piece.(*reveal)
	}
	v.SetContent(strings.Join(lines, "\n") + "\n")
	return v
}

// stepBar is "✓ done → ● current → pending" in width cells: the done steps
// lose their titles first, then the bar is cut.
func stepBar(b ui.Bar, width int) string {
	join := func(short bool) string {
		parts := make([]string, len(b.Titles))
		for i, t := range b.Titles {
			switch {
			case i < b.Current && short:
				parts[i] = "✓"
			case i < b.Current:
				parts[i] = "✓ " + t
			case i == b.Current:
				parts[i] = "● " + t
			default:
				parts[i] = t
			}
		}
		return strings.Join(parts, " → ")
	}
	s := join(false)
	if ansi.StringWidth(s) > width {
		s = join(true)
	}
	return ansi.Truncate(s, width, "…")
}
