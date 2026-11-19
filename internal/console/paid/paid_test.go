package paid

import "testing"

// refused fails the test unless err is exactly want.
func refused(t *testing.T, what string, err error, want string) {
	t.Helper()
	if err == nil || err.Error() != want {
		t.Errorf("%s: err = %v, want %q", what, err, want)
	}
}

// TestSessionCaps: a step states at least one call, and the steps of one
// console run state at most 12 in all.
func TestSessionCaps(t *testing.T) {
	if MaxPerRun != 12 {
		t.Fatalf("MaxPerRun = %d, want 12 (design section 3)", MaxPerRun)
	}
	s := NewSession()
	_, err := s.Open(0)
	refused(t, "Open(0)", err, "a paid step states at least one call")
	_, err = s.Open(13)
	refused(t, "Open(13)", err, "this step states 13 paid calls and this console run has 12 left of 12")
	if _, err := s.Open(5); err != nil {
		t.Fatalf("Open(5): %v", err)
	}
	if _, err := s.Open(7); err != nil {
		t.Fatalf("Open(7) after 5: %v", err)
	}
	_, err = s.Open(1)
	refused(t, "Open(1) after 12", err, "this step states 1 paid calls and this console run has 0 left of 12")
}

// TestSessionFits: Fits refuses as Open does, with the same texts, and
// debits nothing.
func TestSessionFits(t *testing.T) {
	s := NewSession()
	refused(t, "Fits(13)", s.Fits(13), "this step states 13 paid calls and this console run has 12 left of 12")
	refused(t, "Fits(0)", s.Fits(0), "a paid step states at least one call")
	if err := s.Fits(12); err != nil {
		t.Fatalf("Fits(12): %v", err)
	}
	if _, err := s.Open(12); err != nil {
		t.Fatalf("Open(12) after Fits(12): %v: Fits debited", err)
	}
}

// TestBudgetTake: a budget gives exactly what its step stated, and a call
// with no budget is refused.
func TestBudgetTake(t *testing.T) {
	b, err := NewSession().Open(3)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Take(2); err != nil {
		t.Fatalf("Take(2): %v", err)
	}
	if err := b.Take(1); err != nil {
		t.Fatalf("Take(1): %v", err)
	}
	refused(t, "Take(1) when spent", b.Take(1), "1 paid calls asked, 0 left of what this step stated")
	refused(t, "nil Take(1)", (*Budget)(nil).Take(1), "no paid budget: this call was not announced")
	b, _ = NewSession().Open(2)
	refused(t, "Take(0)", b.Take(0), "0 paid calls asked, 2 left of what this step stated")
	refused(t, "Take(3)", b.Take(3), "3 paid calls asked, 2 left of what this step stated")
}
