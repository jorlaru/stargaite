package routing

import (
	"sync"
	"testing"
	"time"
)

// clock is a time the test moves.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// fail runs n calls to target that each fail.
func fail(t *testing.T, b *Breakers, target string, n int) {
	t.Helper()
	for range n {
		gen, ok := b.Acquire(target)
		if !ok {
			t.Fatal("a closed circuit refused a call")
		}
		b.Release(target, gen, Fail)
	}
}

// admitted takes permits for target until one is refused, at most 10, and
// returns how many it got.
func admitted(b *Breakers, target string) int {
	n := 0
	for n < 10 && ok(b.Acquire(target)) {
		n++
	}
	return n
}

// ok is whether Acquire gave a permit.
func ok(_ uint64, ok bool) bool { return ok }

// permit takes a permit for target, which must be given, and returns its gen.
func permit(t *testing.T, b *Breakers, target string) uint64 {
	t.Helper()
	gen, ok := b.Acquire(target)
	if !ok {
		t.Fatal("a permit was refused")
	}
	return gen
}

// TestBreaker is RES-2 on an injected clock: five eligible failures within
// 30 s open a circuit for 20 s; half-open it lets two trials through and
// refuses the rest; two passes close it, one failure opens it again, and a
// trial without a verdict returns its permit. Breakers are per target.
func TestBreaker(t *testing.T) {
	c := &clock{time.Unix(1_790_000_000, 0)}
	b := NewBreakers(c.now)

	// Failures spread over more than 30 s, passes and None never open it.
	for range 3 {
		fail(t, b, "a", 4)
		b.Release("a", permit(t, b, "a"), Pass)
		b.Release("a", permit(t, b, "a"), None)
		c.t = c.t.Add(31 * time.Second)
	}
	fail(t, b, "a", 4)
	c.t = c.t.Add(29 * time.Second)
	fail(t, b, "a", 1) // the fifth within 30 s of the four before it
	if ok(b.Acquire("a")) {
		t.Fatal("five failures within 30 s left the circuit closed")
	}
	b.Release("b", permit(t, b, "b"), Pass) // another target's circuit is closed

	// Open for 20 s, then half-open with two trials.
	c.t = c.t.Add(20*time.Second - time.Nanosecond)
	if ok(b.Acquire("a")) {
		t.Fatal("the circuit closed before 20 s")
	}
	c.t = c.t.Add(time.Nanosecond)
	gen := permit(t, b, "a")
	if n := admitted(b, "a"); n != 1 {
		t.Fatalf("half-open lets %d trials through", n+1)
	}
	// A trial without a verdict returns its permit.
	b.Release("a", gen, None)
	if n := admitted(b, "a"); n != 1 {
		t.Fatalf("a None trial returned %d permits", n)
	}
	// Two passes close it.
	b.Release("a", gen, Pass)
	if ok(b.Acquire("a")) {
		t.Fatal("one pass closed the circuit")
	}
	b.Release("a", gen, Pass)
	for range 10 {
		b.Release("a", permit(t, b, "a"), Pass) // closed
	}
	// Closing forgets the failures before it.
	fail(t, b, "a", 4)
	b.Release("a", permit(t, b, "a"), Pass) // four failures leave it closed

	// One failing trial opens it for another 20 s, also with a pass before.
	fail(t, b, "a", 1) // the fifth: open
	if ok(b.Acquire("a")) {
		t.Fatal("five failures after a close left the circuit closed")
	}
	c.t = c.t.Add(20 * time.Second)
	gen = permit(t, b, "a")
	permit(t, b, "a")
	b.Release("a", gen, Pass)
	b.Release("a", gen, Fail)
	if ok(b.Acquire("a")) {
		t.Fatal("a failing trial left the circuit half-open")
	}
	c.t = c.t.Add(20 * time.Second)
	b.Release("a", permit(t, b, "a"), Fail) // half-open again

	// A call admitted before the circuit opened changes nothing once it is
	// open, nor half-open with no trial out.
	c.t = c.t.Add(time.Minute)
	b = NewBreakers(c.now)
	early := permit(t, b, "a")
	fail(t, b, "a", 5)
	b.Release("a", early, Fail)
	c.t = c.t.Add(20 * time.Second)
	b.Release("a", early, Pass)
	b.Release("a", early, None)
	if n := admitted(b, "a"); n != 2 {
		t.Fatalf("a call admitted before the circuit opened left %d trials", n)
	}
}

// TestBreakerLateVerdict: half-open, the verdict of a call admitted before
// the circuit opened, a stream that outlasts the 20 s, neither frees a trial
// permit nor counts as a trial (RES-2: two trials, open for all others).
func TestBreakerLateVerdict(t *testing.T) {
	c := &clock{time.Unix(1_790_000_000, 0)}
	for _, late := range []Verdict{None, Pass} {
		b := NewBreakers(c.now)
		s := permit(t, b, "a") // the stream, admitted while closed
		fail(t, b, "a", 5)
		c.t = c.t.Add(20 * time.Second)
		p1 := permit(t, b, "a")
		b.Release("a", s, late)
		if n := admitted(b, "a"); n != 1 {
			t.Fatalf("%d: with one trial out, %d more admitted", late, n)
		}
		b.Release("a", p1, Pass)
		if ok(b.Acquire("a")) {
			t.Fatalf("%d: one trial pass closed the circuit", late)
		}
		// The other trial, from admitted, passes too: closed.
		b.Release("a", p1, Pass)
		if n := admitted(b, "a"); n != 10 {
			t.Fatalf("%d: two trial passes left %d permits", late, n)
		}
	}
}

// TestBreakerConcurrent: concurrent calls share one breaker (go test -race).
func TestBreakerConcurrent(t *testing.T) {
	b := NewBreakers(time.Now)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if gen, ok := b.Acquire("t"); ok {
				b.Release("t", gen, Fail)
			}
		})
	}
	wg.Wait()
	if ok(b.Acquire("t")) {
		t.Error("fifty failures left the circuit closed")
	}
}
