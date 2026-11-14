package admission

import (
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
)

// t0 is a whole second on the test clock.
var t0 = time.Unix(1_790_000_000, 0)

// clock is a test clock the test moves.
type clock struct{ ns atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.ns.Store(t0.UnixNano())
	return c
}
func (c *clock) now() time.Time      { return time.Unix(0, c.ns.Load()) }
func (c *clock) add(d time.Duration) { c.ns.Add(int64(d)) }
func (c *clock) set(t time.Time)     { c.ns.Store(t.UnixNano()) }

type event struct {
	at time.Time
	n  int64
}

// exact is the sum of an exact sliding window: the entries less than w ago.
func exact(log []event, now time.Time, w time.Duration) (s int64) {
	for _, e := range log {
		if e.at.After(now.Add(-w)) && !e.at.After(now) {
			s += e.n
		}
	}
	return s
}

// peek is x's sum at t on a copy: a look ahead moves no time of x.
func peek(x *window, t time.Time) int64 {
	c := *x
	c.buckets = slices.Clone(x.buckets)
	return c.sum(t)
}

// TestWindow is the window against an exact sliding log on random events
// (phase-3 design section 14), some of them counted up to 2 s late, as tokens
// are at finish after another request looked at a later now: never below the
// log, above it only by entries less than w + g old, and wait is never early
// and never a whole second late. Spans cover a bucket of 1 s, of 30 s, and of
// a span that is not a whole number of buckets.
func TestWindow(t *testing.T) {
	for _, w := range []time.Duration{time.Minute, 5 * time.Hour, 1001 * time.Second, 7 * time.Second} {
		r := rand.New(rand.NewPCG(1, uint64(w)))
		x := newWindow(w, t0)
		g := x.g
		if want := max(time.Second, w/600); g != want {
			t.Fatalf("%v: bucket %v, want %v", w, g, want)
		}
		var log []event
		now := t0.Add(time.Duration(r.Int64N(int64(time.Second))))
		late := 0
		for i := range 3000 {
			now = now.Add(time.Duration(r.Int64N(int64(w / 100 * 3))))
			if r.IntN(4) > 0 {
				e := event{now, r.Int64N(5) + 1}
				if r.IntN(5) == 0 {
					e.at = now.Add(-time.Duration(r.Int64N(int64(2 * time.Second))))
					late++
				}
				x.add(e.at, e.n)
				log = append(log, e)
			}
			got, want, most := x.sum(now), exact(log, now, w), exact(log, now, w+g)
			if got < want || got > most {
				t.Fatalf("%v, step %d: sum %d, exact %d, at most %d", w, i, got, want, most)
			}
			if got == 0 {
				continue
			}
			limit := float64(r.Int64N(got) + 1) // refused now
			d := x.wait(now, limit)
			if d < time.Second || d%time.Second != 0 {
				t.Fatalf("%v, step %d: wait %v", w, i, d)
			}
			if s := peek(x, now.Add(d)); float64(s) >= limit {
				t.Fatalf("%v, step %d: wait %v is early: sum %d then, limit %v", w, i, d, s, limit)
			}
			if s := peek(x, now.Add(d-time.Second)); d > time.Second && float64(s) < limit {
				t.Fatalf("%v, step %d: wait %v is late: sum %d a second before, limit %v", w, i, d, s, limit)
			}
		}
		if late < 100 {
			t.Fatalf("%v: %d late entries", w, late)
		}
	}
}

// TestWindowLongGap: after a silence longer than the ring, nothing old
// counts, and an entry older than the ring counts in no window still to come.
// A look placed before the newest bucket held, which only a clock without
// monotonic readings that went back can give, counts every bucket held:
// longer, never less.
func TestWindowLongGap(t *testing.T) {
	x := newWindow(time.Minute, t0)
	x.add(t0, 3)
	later := t0.Add(10 * time.Minute)
	if s := x.sum(later); s != 0 {
		t.Errorf("sum after 10 min %d", s)
	}
	x.add(later, 1)
	x.add(t0, 2)
	if s := x.sum(later); s != 1 {
		t.Errorf("sum after an entry older than the ring %d, want 1", s)
	}
	if s := x.sum(t0); s != 1 {
		t.Errorf("sum at a look before the newest bucket %d, want 1", s)
	}
	// Tokens counted 1.5 s late keep their place: the entries age as their
	// times say.
	x = newWindow(time.Minute, t0)
	x.add(t0, 1)
	x.sum(t0.Add(2 * time.Second))
	x.add(t0.Add(500*time.Millisecond), 1)
	if s := x.sum(t0.Add(60999 * time.Millisecond)); s != 2 {
		t.Errorf("sum after a late entry %d, want 2", s)
	}
	if s := x.sum(t0.Add(61 * time.Second)); s != 0 {
		t.Errorf("sum a window after the late entry %d, want 0", s)
	}
}

// TestMonotonicClock: the service clock's values carry a monotonic reading,
// and a window places them by the time elapsed since its origin
// (time.Time.Sub), so a step of the wall clock moves nothing
// (https://pkg.go.dev/time#hdr-Monotonic_Clocks). A test cannot step the wall
// clock under a monotonic reading; it checks that such values are placed and
// limited as on the test clock.
func TestMonotonicClock(t *testing.T) {
	base := time.Now()
	if x := newWindow(time.Minute, base); x.pos(base.Add(time.Hour)) != base.UnixNano()+int64(time.Hour) {
		t.Fatalf("place %d", x.pos(base.Add(time.Hour)))
	}
	var el atomic.Int64
	s := New(func() time.Time { return base.Add(time.Duration(el.Load())) })
	r, e := s.Admit(key(0, 1, 0))
	if e != nil {
		t.Fatal(e)
	}
	r()
	if _, e := s.Admit(key(0, 1, 0)); refusal(e)[0] != "rate_limit_exceeded" {
		t.Errorf("within the window: %v", e)
	}
	el.Store(int64(61 * time.Second))
	if r, e = s.Admit(key(0, 1, 0)); e != nil {
		t.Fatalf("a window later: %v", e)
	}
	r()
}

// refusal is what Admit answered: "" when admitted, else the code and
// Retry-After.
func refusal(e *domain.Error) [2]string {
	if e == nil {
		return [2]string{}
	}
	if e.Status != 429 || e.Type != "rate_limit_error" {
		panic(e)
	}
	return [2]string{e.Code, e.RetryAfter}
}

func key(p, rpm, tpm int64) Scope   { return Scope{"key:a", p, rpm, tpm} }
func alias(p, rpm, tpm int64) Scope { return Scope{"model:m", p, rpm, tpm} }
func global(p int64) Scope          { return Scope{"global", p, 0, 0} }

// held is the scope of name as State holds it: in flight, rpm and tpm sums,
// whether it holds windows; nil when not held.
func held(s *State, name string, now time.Time) []int64 {
	x := s.scopes[name]
	if x == nil {
		return nil
	}
	v := []int64{x.flight, -1, -1}
	if x.rpm != nil {
		v[1] = x.rpm.sum(now)
	}
	if x.tpm != nil {
		v[2] = x.tpm.sum(now)
	}
	return v
}

// TestAdmitAllOrNothing is LIM-5 and LIM-6: each limit refuses on its own,
// checked in the order key, alias, instance and, per scope, parallel, rpm,
// tpm; a refusal counts in no scope and takes no slot; Retry-After is 1 for
// a slot and the window's wait for a rate.
func TestAdmitAllOrNothing(t *testing.T) {
	c := newClock()
	s := New(c.now)
	r1, e := s.Admit(key(1, 0, 0), alias(0, 2, 0), global(0))
	if e != nil {
		t.Fatal(e)
	}
	// The key's slot is taken: refused, and the alias's rpm stays 1.
	if _, e := s.Admit(key(1, 0, 0), alias(0, 2, 0), global(0)); refusal(e) != [2]string{"parallel_limit_exceeded", "1"} ||
		e.Message != "the parallel limit of this key is reached" {
		t.Errorf("second: %v", e)
	}
	if got := held(s, "model:m", c.now()); got[0] != 1 || got[1] != 1 {
		t.Errorf("alias after a refusal by the key: %v", got)
	}
	r1()
	r1() // idempotent
	if got := held(s, "key:a", c.now()); got != nil {
		t.Errorf("key after release: %v", got)
	}
	c.add(500 * time.Millisecond)
	r2, e := s.Admit(key(1, 0, 0), alias(0, 2, 0), global(0))
	if e != nil {
		t.Fatal(e)
	}
	r2()
	// The alias's rpm is spent: refused 60.5 s before its bucket of t0
	// leaves, so 61, and the key took no slot and the instance none.
	_, e = s.Admit(key(1, 0, 0), alias(0, 2, 0), global(1))
	if refusal(e) != [2]string{"rate_limit_exceeded", "61"} || e.Message != `the rpm_limit of model "m" is reached` {
		t.Errorf("third: %v", e)
	}
	if held(s, "key:a", c.now()) != nil || held(s, "global", c.now()) != nil {
		t.Errorf("a refusal by the alias took a slot: %v %v", held(s, "key:a", c.now()), held(s, "global", c.now()))
	}
	// One second before the window admits, still refused; at it, admitted.
	c.set(t0.Add(60*time.Second + 999*time.Millisecond))
	if _, e := s.Admit(alias(0, 2, 0)); refusal(e)[0] != "rate_limit_exceeded" {
		t.Errorf("before the wait: %v", e)
	}
	c.set(t0.Add(61 * time.Second))
	r3, e := s.Admit(alias(0, 2, 0))
	if e != nil {
		t.Errorf("after the wait: %v", e)
	}
	r3()

	// tpm: tokens count at finish; the sum at the limit refuses.
	c.set(t0.Add(time.Hour))
	r4, e := s.Admit(key(0, 0, 100), alias(0, 0, 0), global(2))
	if e != nil {
		t.Fatal(e)
	}
	s.Tokens(60, c.now(), "key:a", "model:m", "global") // only the key holds a tpm window
	r4b, e := s.Admit(key(0, 0, 100), alias(0, 0, 0), global(2))
	if e != nil {
		t.Fatalf("tpm below the limit: %v", e)
	}
	s.Tokens(40, c.now(), "key:a")
	if held(s, "model:m", c.now())[2] != -1 || held(s, "global", c.now())[1] != -1 {
		t.Errorf("a scope without limits holds a window: %v %v", held(s, "model:m", c.now()), held(s, "global", c.now()))
	}
	// The instance is full (2 in flight) and is checked last: the key's tpm
	// refuses first.
	_, e = s.Admit(key(0, 0, 100), alias(0, 0, 0), global(2))
	if refusal(e) != [2]string{"rate_limit_exceeded", "61"} || e.Message != "the tpm_limit of this key is reached" {
		t.Errorf("tpm at the limit: %v", e)
	}
	_, e = s.Admit(key(0, 0, 0), alias(0, 0, 0), global(2))
	if refusal(e) != [2]string{"parallel_limit_exceeded", "1"} || e.Message != "the parallel limit of the instance is reached" {
		t.Errorf("instance full: %v", e)
	}
	// That Admit cleared the key's tpm: its window is dropped.
	if got := held(s, "key:a", c.now()); got[2] != -1 {
		t.Errorf("key after its tpm limit was cleared: %v", got)
	}
	r4()
	r4b()
	// Parallel first within a scope: a full slot and a spent rpm answer the slot.
	r5, _ := s.Admit(alias(1, 1, 0))
	if _, e := s.Admit(alias(1, 1, 0)); refusal(e)[0] != "parallel_limit_exceeded" {
		t.Errorf("parallel and rpm both refuse: %v", e)
	}
	r5()
	// Parallel before tpm: a full slot and a spent tpm answer the slot, 1.
	r6, _ := s.Admit(alias(1, 0, 1))
	s.Tokens(5, c.now(), "model:m")
	if _, e := s.Admit(alias(1, 0, 1)); refusal(e) != [2]string{"parallel_limit_exceeded", "1"} {
		t.Errorf("parallel and tpm both refuse: %v", e)
	}
	r6()
	// rpm before tpm: both spent answer the rpm_limit.
	r7, _ := s.Admit(key(0, 1, 1))
	s.Tokens(5, c.now(), "key:a")
	r7()
	if _, e := s.Admit(key(0, 1, 1)); refusal(e)[0] != "rate_limit_exceeded" || e.Message != "the rpm_limit of this key is reached" {
		t.Errorf("rpm and tpm both refuse: %v", e)
	}
}

// TestReleaseAcrossChange: limits that change while a request is in flight
// neither keep its slot nor release it twice, and a scope whose limits are
// all cleared is no longer held once idle.
func TestReleaseAcrossChange(t *testing.T) {
	c := newClock()
	s := New(c.now)
	r1, _ := s.Admit(key(1, 5, 5))
	// The limits are cleared: requests pass, taking and releasing slots.
	for range 3 {
		r, e := s.Admit(key(0, 0, 0))
		if e != nil {
			t.Fatal(e)
		}
		r()
	}
	if got := held(s, "key:a", c.now()); got[0] != 1 || got[1] != -1 || got[2] != -1 {
		t.Errorf("key in flight after its limits were cleared: %v", got)
	}
	// The limit returns, lower: the slot of r1 still counts.
	if _, e := s.Admit(key(1, 0, 0)); refusal(e)[0] != "parallel_limit_exceeded" {
		t.Errorf("r1's slot was lost: %v", e)
	}
	r1()
	r1()
	if len(s.scopes) != 0 {
		t.Errorf("scopes held after every release: %v", s.scopes)
	}
	r2, e := s.Admit(key(1, 0, 0))
	if e != nil {
		t.Errorf("released twice or kept: %v", e)
	}
	r2()
}

// TestAdmitConcurrent (-race): concurrent requests never exceed a parallel
// limit, and every slot comes back.
func TestAdmitConcurrent(t *testing.T) {
	c := newClock()
	s := New(c.now)
	var in, peak, admitted atomic.Int64
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 200 {
				release, e := s.Admit(key(3, 0, 0), alias(5, 0, 0), global(4))
				if e != nil {
					continue
				}
				admitted.Add(1)
				n := in.Add(1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				s.Tokens(10, c.now(), "key:a")
				in.Add(-1)
				release()
				release()
			}
		})
	}
	wg.Wait()
	if peak.Load() > 3 || admitted.Load() == 0 {
		t.Errorf("peak %d, admitted %d", peak.Load(), admitted.Load())
	}
	if len(s.scopes) != 0 {
		t.Errorf("scopes held after every release: %v", s.scopes)
	}
}
