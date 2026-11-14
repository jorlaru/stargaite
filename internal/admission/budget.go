package admission

import (
	"fmt"
	"strings"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
)

// Budget is one USD budget of a request's scope as the request's snapshot
// publishes it (LIM-1, LIM-2).
type Budget struct {
	Scope string // as Scope.Name
	Spec  domain.Budget
	Since int64 // reset_interval never: the revision its counter started at, snap.NeverSince[Scope]
}

// Warning is one crossing that goes to the log, the audit and the counters
// (kick-off section 10). Event: budget_warning or budget_exhausted; Period:
// the budget's period key (d:<UTC day>, m:<UTC month>, or n:<since>:<limit>
// for a never budget, so a limit_usd change warns again); At: the charge's
// a.ended.
type Warning struct {
	Event, Scope, Period string
	At                   time.Time
}

// spend is the spend of one scope: today's, this month's and, while the
// scope's budget is never, its running total since the revision since.
type spend struct {
	day, month, never counter
	since             int64
}

// counter is a scope's spend in one period and the crossings already
// returned in it, each once per period.
type counter struct {
	period            string
	usd               float64
	warned, exhausted bool
}

// add adds usd in period p. A later period starts the counter again from
// zero, which drops the older one; a charge dated before the period held (a
// call that ended at 23:59:59 and was charged after one that ended at
// 00:00, or a wall clock stepped back) counts in the period held, so the
// counter is never below the spend of its period.
func (c *counter) add(p string, usd float64) {
	if p > c.period {
		*c = counter{period: p}
	}
	c.usd += usd
}

// periods are the period keys of t: its UTC day and its UTC month.
func periods(t time.Time) (day, month string) {
	d := t.UTC().Format(time.DateOnly)
	return "d:" + d, "m:" + d[:7]
}

// counter is the counter b reads in the periods given, nil when it holds
// nothing of them: a daily or monthly counter of an earlier period, or a
// never budget whose counter started before b's since (b was set again,
// so its counter starts at zero). A request under an older snapshot reads
// the newer counter. Any interval but daily and monthly reads as never.
// The caller holds the mutex.
func (s *State) counter(b Budget, day, month string) *counter {
	x := s.spend[b.Scope]
	switch {
	case x == nil:
		return nil
	case b.Spec.ResetInterval == domain.ResetDaily:
		return x.day.in(day)
	case b.Spec.ResetInterval == domain.ResetMonthly:
		return x.month.in(month)
	case b.Since > x.since:
		return nil
	}
	return &x.never
}

// in is c when it holds period p or a later one (a clock stepped back),
// else nil.
func (c *counter) in(p string) *counter {
	if c.period < p {
		return nil
	}
	return c
}

// spent is what b's counter reads in the periods given.
func (s *State) spent(b Budget, day, month string) float64 {
	if c := s.counter(b, day, month); c != nil {
		return c.usd
	}
	return 0
}

// limit is b's limit_usd; a missing one, which validation refuses but a
// stored configuration may still hold, is 0, which blocks.
func limit(b Budget) float64 {
	if b.Spec.LimitUSD == nil {
		return 0
	}
	return *b.Spec.LimitUSD
}

// Spent is what b's counter reads now on the service clock: the spend of
// its scope today, this month, or since its never budget was set.
func (s *State) Spent(b Budget) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	day, month := periods(s.now())
	return s.spent(b, day, month)
}

// Exhausted is 402 budget_exceeded when one of bs, given in the order key,
// alias, instance, has its counter at or above its limit_usd now (LIM-3);
// the message names the kind of scope, never a figure. exclude calls it for
// pay_per_use targets only.
func (s *State) Exhausted(bs ...Budget) *domain.Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	day, month := periods(s.now())
	for _, b := range bs {
		if s.spent(b, day, month) >= limit(b) {
			return domain.NewError(domain.CodeBudgetExceeded, "the budget of "+what(b.Scope)+" is exhausted")
		}
	}
	return nil
}

// Spend charges usd, the cost of a pay_per_use call that ended at at, before
// its slot is released (LIM-4): to the day and the month of at of every
// scope in scopes, whether or not it has a budget, and to the counter of
// each never budget in bs whose since is the counter's (a later since
// starts it again at zero; an earlier one, a request under an older
// snapshot, is not counted, as the writer's since_revision rule does in the
// store). It returns the crossings of 80 and 100 percent of each budget in
// bs that this charge made, each once per period; a never budget's period
// is its since and its limit_usd, so a raised limit warns again.
func (s *State) Spend(usd float64, at time.Time, scopes []string, bs []Budget) []Warning {
	s.mu.Lock()
	defer s.mu.Unlock()
	day, month := periods(at)
	before := make([]float64, len(bs))
	for i, b := range bs {
		before[i] = s.spent(b, day, month)
	}
	for _, name := range scopes {
		x := s.spendOf(name)
		x.day.add(day, usd)
		x.month.add(month, usd)
	}
	for _, b := range bs {
		if r := b.Spec.ResetInterval; r == domain.ResetDaily || r == domain.ResetMonthly {
			continue
		}
		x := s.spendOf(b.Scope)
		if b.Since > x.since {
			x.never, x.since = counter{}, b.Since
		}
		if b.Since == x.since {
			x.never.usd += usd
			if p := fmt.Sprintf("n:%d:%g", b.Since, limit(b)); x.never.period != p {
				x.never.period, x.never.warned, x.never.exhausted = p, false, false
			}
		}
	}
	var ws []Warning
	for i, b := range bs {
		c := s.counter(b, day, month)
		if c == nil {
			continue
		}
		for _, cross := range []struct {
			event string
			share float64
			done  *bool
		}{{"budget_warning", 0.8, &c.warned}, {"budget_exhausted", 1, &c.exhausted}} {
			if v := cross.share * limit(b); !*cross.done && before[i] < v && c.usd >= v {
				*cross.done = true
				ws = append(ws, Warning{Event: cross.event, Scope: b.Scope, Period: c.period, At: at})
			}
		}
	}
	return ws
}

// SeedSpend adds usd to scope's counter of period, a day (d:) or a month
// (m:), at start (RES-9): a later period than the one held replaces it, an
// earlier one is in the past and is ignored.
func (s *State) SeedSpend(scope, period string, usd float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x := s.spendOf(scope)
	c := &x.day
	if strings.HasPrefix(period, "m:") {
		c = &x.month
	}
	if period >= c.period {
		c.add(period, usd)
	}
}

// SeedNever sets scope's never counter to usd since revision since, at
// start, from its budget_totals row (RES-9).
func (s *State) SeedNever(scope string, since int64, usd float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x := s.spendOf(scope)
	x.never, x.since = counter{period: fmt.Sprintf("n:%d", since), usd: usd}, since
}

// spendOf is the spend of scope name, made when missing. The caller holds
// the mutex.
func (s *State) spendOf(name string) *spend {
	x := s.spend[name]
	if x == nil {
		x = &spend{}
		s.spend[name] = x
	}
	return x
}
