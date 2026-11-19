// Package paid is the one door to a paid call. A console run holds one
// Session; each paid step opens a Budget of the calls it states, and every
// paid call takes from that Budget first, so a call nobody announced, or
// more calls than announced, is refused before it is made (design section 3,
// "Paid calls, one door").
package paid

import (
	"errors"
	"fmt"
)

// MaxPerRun is how many paid calls one console run may make (design
// section 3).
const MaxPerRun = 12

// Session is what is left of a console run's paid calls.
type Session struct{ left int }

// NewSession returns a run's session, with MaxPerRun calls left.
func NewSession() *Session { return &Session{left: MaxPerRun} }

// Fits refuses the calls a step states when Open would, and debits nothing,
// so a step can be refused before the reader is asked.
func (s *Session) Fits(calls int) error {
	switch {
	case calls < 1:
		return errors.New("a paid step states at least one call")
	case calls > s.left:
		return fmt.Errorf("this step states %d paid calls and this console run has %d left of %d", calls, s.left, MaxPerRun)
	}
	return nil
}

// Open debits the calls a step states, at once, and returns a budget of
// exactly that many.
func (s *Session) Open(calls int) (*Budget, error) {
	if err := s.Fits(calls); err != nil {
		return nil, err
	}
	s.left -= calls
	return &Budget{left: calls}, nil
}

// Budget is what is left of the calls one step stated.
type Budget struct{ left int }

// Take debits n calls, or refuses them when the budget is nil, n is not
// positive, or fewer than n are left.
func (b *Budget) Take(n int) error {
	switch {
	case b == nil:
		return errors.New("no paid budget: this call was not announced")
	case n < 1 || n > b.left:
		return fmt.Errorf("%d paid calls asked, %d left of what this step stated", n, b.left)
	}
	b.left -= n
	return nil
}
