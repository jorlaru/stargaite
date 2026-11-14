// Package routing holds the circuit breaker of each target (RES-2, phase-2
// design section 10). Its state lives outside the configuration snapshot, so
// it outlives every apply (RES-7).
package routing

import (
	"sync"
	"time"
)

// The RES-2 constants.
const (
	failures = 5                // eligible failures that open a circuit
	window   = 30 * time.Second // within which they count
	openFor  = 20 * time.Second // how long it stays open
	trials   = 2                // requests a half-open circuit lets through, which must all pass
)

// Verdict is what a provider call tells the breaker of its target. The zero
// value is None, so a call that ends without one, a panic included, only
// returns its permit.
type Verdict int

const (
	None Verdict = iota // the call says nothing of the target: the client went away
	Pass                // the target answered, whatever the answer
	Fail                // a failure eligible for fallback (RES-1)
)

// Breakers are the circuit breakers by target id; a target never seen has a
// closed circuit. They are safe for concurrent use. An entry stays for the
// life of the process, also after its target is removed.
type Breakers struct {
	mu  sync.Mutex
	by  map[string]*breaker
	now func() time.Time
}

// breaker is one target's circuit: closed while openUntil is zero, open
// until openUntil, then half-open, with trials permits out and passed of
// them passed. gen counts the times it opened: the permits of a half-open
// circuit carry the current one, every earlier permit an older one.
type breaker struct {
	fails          [failures]time.Time // the latest failures, a ring; next is the oldest
	next           int
	openUntil      time.Time
	trials, passed int
	gen            uint64
}

// NewBreakers returns breakers that read the time from now.
func NewBreakers(now func() time.Time) *Breakers {
	return &Breakers{by: map[string]*breaker{}, now: now}
}

// Acquire reports whether a call to target may start: true while its circuit
// is closed, false while it is open; a half-open circuit gives out its trial
// permits and then refuses until they come back. Each true must be followed
// by exactly one Release with the generation it returned.
func (b *Breakers) Acquire(target string) (gen uint64, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	br := b.by[target]
	switch {
	case br == nil:
		return 0, true
	case br.openUntil.IsZero():
		return br.gen, true
	case b.now().Before(br.openUntil) || br.trials == trials:
		return 0, false
	}
	br.trials++
	return br.gen, true
}

// Release ends a call that Acquire let start. Closed, five Fail within 30 s
// open the circuit for 20 s. Half-open, a Fail opens it again for 20 s, the
// trials passing close it, and None returns the permit, so a call that
// ended without a verdict never leaves the circuit half-open for good. Open
// or half-open, the verdict of a call admitted before the circuit last
// opened, an older gen, changes nothing.
func (b *Breakers) Release(target string, gen uint64, v Verdict) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now, br := b.now(), b.by[target]
	if br == nil {
		if v != Fail {
			return
		}
		br = &breaker{}
		b.by[target] = br
	}
	switch {
	case br.openUntil.IsZero():
		if v == Fail {
			br.fails[br.next] = now
			br.next = (br.next + 1) % failures
			if oldest := br.fails[br.next]; !oldest.IsZero() && now.Sub(oldest) < window {
				*br = breaker{openUntil: now.Add(openFor), gen: br.gen + 1}
			}
		}
	case gen != br.gen: // not a trial
	case v == Fail:
		*br = breaker{openUntil: now.Add(openFor), gen: br.gen + 1}
	case v == Pass:
		if br.passed++; br.passed == trials {
			*br = breaker{gen: br.gen}
		}
	default:
		br.trials--
	}
}
