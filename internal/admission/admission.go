// Package admission decides step 4 of the hot path (kick-off section 14.1):
// the parallel slots and the rpm and tpm windows of a key, an alias and the
// instance, counted in memory on an injected clock (phase-3 design section 7),
// and the USD budgets and subscription quotas that exclude targets at step 5
// (sections 8 and 9). It reaches no internal package but domain.
package admission

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
)

// rateSpan is the window of rpm_limit and tpm_limit (LIM-5).
const rateSpan = time.Minute

// Scope is one admission scope and its limits as the request's snapshot
// publishes them.
type Scope struct {
	Name               string // "global", "key:<name>", "model:<alias>"
	Parallel, RPM, TPM int64  // 0: no limit
}

// State holds every scope's requests in flight and, while the scope declares
// rpm_limit or tpm_limit, its window. A scope with nothing in flight and no
// window is not held. It also holds the spend of every scope charged
// (budget.go), apart from them: a spend is never dropped at rest; and the
// quota windows (quota.go), each from its seed until a newer plan drops it.
type State struct {
	now     func() time.Time
	seed    Seed
	mu      sync.Mutex
	scopes  map[string]*scope
	spend   map[string]*spend
	quotas  map[holder][]*quota
	seeding map[string]chan struct{} // by provider: closed when its seed in flight ends
	flying  map[holder][]time.Time   // the Take times of the calls not yet Ended, oldest first
	rev     int64                    // the newest plan revision seen
}

// scope is the state of one scope name.
type scope struct {
	flight   int64
	rpm, tpm *window // nil while the scope declares no such limit
}

// New returns an empty State on the clock now, the service's, whose quota
// windows are seeded by seed.
func New(now func() time.Time, seed Seed) *State {
	return &State{now: now, seed: seed, scopes: map[string]*scope{}, spend: map[string]*spend{}, quotas: map[holder][]*quota{},
		seeding: map[string]chan struct{}{}, flying: map[holder][]time.Time{}}
}

// Admit admits a request into scopes, given in the order key, alias,
// instance, all or nothing (LIM-5, LIM-6). Under one mutex, for each scope
// in turn, it checks the requests in flight against Parallel, then the rpm
// window against RPM, then the tpm window against TPM; the first refusal
// answers with nothing counted anywhere: 429 parallel_limit_exceeded with
// Retry-After 1, or 429 rate_limit_exceeded with the refusing window's wait.
// Otherwise every scope takes a slot and its rpm window adds 1 now. The
// windows of a limit a scope no longer declares are dropped here. release
// gives the slots back and may run any number of times: only the first
// counts, whatever the configuration has become since.
func (s *State) Admit(scopes ...Scope) (release func(), e *domain.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, sc := range scopes {
		x := s.scopes[sc.Name]
		if x == nil {
			continue // nothing in flight and no window: nothing can refuse
		}
		if sc.RPM == 0 {
			x.rpm = nil
		}
		if sc.TPM == 0 {
			x.tpm = nil
		}
		switch {
		case sc.Parallel > 0 && x.flight >= sc.Parallel:
			e = domain.NewError(domain.CodeParallelLimitExceeded, "the parallel limit of "+what(sc.Name)+" is reached")
			e.RetryAfter = "1"
			return nil, e
		case x.rpm != nil && x.rpm.sum(now) >= sc.RPM:
			return nil, rate("rpm_limit", sc.Name, x.rpm.wait(now, float64(sc.RPM)))
		case x.tpm != nil && x.tpm.sum(now) >= sc.TPM:
			return nil, rate("tpm_limit", sc.Name, x.tpm.wait(now, float64(sc.TPM)))
		}
		s.drop(sc.Name, x)
	}
	type taken struct {
		name string
		x    *scope
	}
	held := make([]taken, len(scopes))
	for i, sc := range scopes {
		x := s.scopes[sc.Name]
		if x == nil {
			x = &scope{}
			s.scopes[sc.Name] = x
		}
		if sc.RPM > 0 && x.rpm == nil {
			x.rpm = newWindow(rateSpan, now)
		}
		if sc.TPM > 0 && x.tpm == nil {
			x.tpm = newWindow(rateSpan, now)
		}
		x.flight++
		if x.rpm != nil {
			x.rpm.add(now, 1)
		}
		held[i] = taken{sc.Name, x}
	}
	released := false
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if released {
			return
		}
		released = true
		// A scope with a slot taken is never dropped, so x is still the
		// scope held under its name.
		for _, t := range held {
			t.x.flight--
			s.drop(t.name, t.x)
		}
	}, nil
}

// Tokens adds n tokens at at to the tpm window of each scope named that
// holds one (LIM-5): finish calls it with a call's input plus output.
func (s *State) Tokens(n int64, at time.Time, scopes ...string) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range scopes {
		if x := s.scopes[name]; x != nil && x.tpm != nil {
			x.tpm.add(at, n)
		}
	}
}

// drop stops holding x, the scope of name, once it has nothing in flight
// and no window. The caller holds the mutex.
func (s *State) drop(name string, x *scope) {
	if x.flight == 0 && x.rpm == nil && x.tpm == nil {
		delete(s.scopes, name)
	}
}

// rate is 429 rate_limit_exceeded for limit of scope name, with wait as
// Retry-After in whole seconds.
func rate(limit, name string, wait time.Duration) *domain.Error {
	e := domain.NewError(domain.CodeRateLimitExceeded, "the "+limit+" of "+what(name)+" is reached")
	e.RetryAfter = strconv.FormatInt(int64(wait/time.Second), 10)
	return e
}

// what names the scope of name in an error, never with a figure: this key,
// model "<alias>", or the instance.
func what(name string) string {
	if alias, ok := strings.CutPrefix(name, "model:"); ok {
		return fmt.Sprintf("model %q", alias)
	}
	if strings.HasPrefix(name, "key:") {
		return "this key"
	}
	return "the instance"
}

// window counts over a sliding span w in buckets of g = max(1 s, w/600),
// aligned to Unix time: an entry stays counted until its whole bucket is older
// than w, so the sum is never below an exact window's (it may count up to g
// more of the past, phase-3 design §17 point 13). A time's place is the Unix
// time of origin plus the time elapsed since origin, which time.Time.Sub reads
// on the monotonic clock when both times carry a reading, as the service
// clock's (time.Now) do: for a rate window, a step of the wall clock, back or
// forward, moves no entry and no look. While the host sleeps the monotonic
// clock may stop, and entries then count as if no time had passed: for a rate
// window, at most one window of extra refusal. Without a step a time's place
// is its own Unix time within microseconds, and on a clock without monotonic
// readings (the tests', the store's) it is exactly that. A quota window has
// an origin without a monotonic reading (Round(0)), so every time is placed
// by its Unix time alone, as the store's records are when it is seeded: a
// seeded and a live window agree after a sleep or a wall step (quota.go).
// Times are after 1970.
type window struct {
	w, g    time.Duration
	origin  time.Time
	last    int64   // the newest bucket held
	buckets []int64 // ring of w/g + 1, rounded up, ending at last
}

func newWindow(w time.Duration, origin time.Time) *window {
	g := bucket(w)
	return &window{w: w, g: g, origin: origin, last: origin.UnixNano() / int64(g), buckets: make([]int64, (w+g-1)/g+1)}
}

// bucket is the width of a window's buckets over span w.
func bucket(w time.Duration) time.Duration { return max(time.Second, w/600) }

// pos is t's place, in ns.
func (x *window) pos(t time.Time) int64 { return x.origin.UnixNano() + int64(t.Sub(x.origin)) }

// advance makes b the newest bucket held when it is later, clearing the
// buckets the ring leaves behind.
func (x *window) advance(b int64) {
	size := int64(len(x.buckets))
	for i := max(x.last+1, b-size+1); i <= b; i++ {
		x.buckets[i%size] = 0
	}
	x.last = max(x.last, b)
}

// span is the buckets counted at p, a place: lo, the oldest whose end is less
// than w before p and not before 1970 (a quota window may be longer than the
// time since), to the newest held. A look placed before the newest bucket
// held (a clock without monotonic readings that went back) counts every
// bucket held; entries cleared when the ring moved ahead are lost, so such
// times must not reach a window (the service clock never gives them).
func (x *window) span(p int64) (lo, hi int64) {
	x.advance(p / int64(x.g))
	return max((p-int64(x.w))/int64(x.g), x.last-int64(len(x.buckets))+1, 0), x.last
}

// add counts n at at, in its own bucket: an entry counted late (tokens at
// finish, after a look at a later now) keeps its place. One older than the
// ring is in no window still to come.
func (x *window) add(at time.Time, n int64) {
	b := x.pos(at) / int64(x.g)
	x.advance(b)
	if size := int64(len(x.buckets)); b > x.last-size {
		x.buckets[b%size] += n
	}
}

func (x *window) sum(now time.Time) (s int64) {
	lo, hi := x.span(x.pos(now))
	for b := lo; b <= hi; b++ {
		s += x.buckets[b%int64(len(x.buckets))]
	}
	return s
}

func (x *window) wait(now time.Time, limit float64) time.Duration {
	s, v := x.sum(now), x.pos(now)
	lo, hi := x.span(v)
	var d time.Duration
	for b := lo; float64(s) >= limit && b <= hi; b++ {
		s -= x.buckets[b%int64(len(x.buckets))]
		d = time.Duration((b+1)*int64(x.g) + int64(x.w) - v)
	}
	return max(time.Second, (d + time.Second - 1).Truncate(time.Second))
}
