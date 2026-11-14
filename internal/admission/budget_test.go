package admission

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
)

// budget is a budget of limit USD on scope, with since for never.
func budget(scope, interval string, limit float64, since int64) Budget {
	return Budget{Scope: scope, Spec: domain.Budget{LimitUSD: &limit, ResetInterval: interval}, Since: since}
}

// everyScope is the scopes a charge of key a on alias m goes to.
var everyScope = []string{"key:a", "model:m", "global"}

// exhausted is Exhausted's code and message, or "" when nothing is.
func exhausted(s *State, bs ...Budget) string {
	e := s.Exhausted(bs...)
	if e == nil {
		return ""
	}
	if e.Status != 402 || e.Type != "billing_error" || e.RetryAfter != "" {
		return fmt.Sprintf("%+v", e)
	}
	return e.Code + ": " + e.Message
}

// TestExhausted is LIM-1 and LIM-3: only the budgets given are checked, in
// the order key, alias, instance; each is independent of the others; a
// counter at or above limit_usd answers 402 budget_exceeded without
// Retry-After, naming the kind of scope and no figure; a limit of 0, or a
// missing one, blocks at once.
func TestExhausted(t *testing.T) {
	c := newClock()
	s := New(c.now)
	k, m, g := budget("key:a", "daily", 1, 0), budget("model:m", "monthly", 2, 0), budget("global", "daily", 3, 0)
	if got := exhausted(s, k, m, g); got != "" {
		t.Errorf("nothing spent: %s", got)
	}
	s.Spend(0.5, c.now(), everyScope, []Budget{k, m, g})
	s.Spend(0.5, c.now(), []string{"key:b", "model:m", "global"}, nil) // another key: the alias and the instance only
	if got := exhausted(s, k, m, g); got != "" {
		t.Errorf("below every limit: %s", got)
	}
	s.Spend(0.5, c.now(), everyScope, nil) // a charge counts whether or not a budget is given
	if got := exhausted(s, k, m, g); got != "budget_exceeded: the budget of this key is exhausted" {
		t.Errorf("key at its limit: %q", got)
	}
	if got := exhausted(s, budget("key:b", "daily", 1, 0), m, g); got != "" {
		t.Errorf("another key: %q", got)
	}
	s.Spend(0.5, c.now(), []string{"key:b", "model:m", "global"}, nil)
	if got := exhausted(s, budget("key:b", "daily", 5, 0), m, g); got != `budget_exceeded: the budget of model "m" is exhausted` {
		t.Errorf("alias at its limit: %q", got)
	}
	if got := exhausted(s, budget("key:b", "daily", 5, 0), budget("model:m", "monthly", 5, 0), g); got != "" {
		t.Errorf("instance below its limit: %q", got)
	}
	if got := exhausted(s, budget("key:b", "daily", 5, 0), budget("model:m", "monthly", 5, 0), budget("global", "daily", 2, 0)); got != "budget_exceeded: the budget of the instance is exhausted" {
		t.Errorf("instance at its limit: %q", got)
	}
	if got := exhausted(s, budget("key:c", "daily", 0, 0)); got != "budget_exceeded: the budget of this key is exhausted" {
		t.Errorf("a limit of 0: %q", got)
	}
	if got := exhausted(s, Budget{Scope: "key:c", Spec: domain.Budget{ResetInterval: "daily"}}); got == "" {
		t.Error("a missing limit admits")
	}
	if got := exhausted(s); got != "" {
		t.Errorf("no budget: %q", got)
	}
	if got := s.Spent(budget("global", "daily", 3, 0)); got != 2 {
		t.Errorf("global spent %v", got)
	}
}

// TestRollover is LIM-2 on the service clock: a daily counter starts again
// at 00:00 UTC and a monthly one on day 1 at 00:00 UTC, with no reset task;
// the older period is dropped on the first charge of the new one; a charge
// dated before the period held counts in it.
func TestRollover(t *testing.T) {
	c := newClock()
	c.set(time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC))
	s := New(c.now)
	d, m := budget("key:a", "daily", 1, 0), budget("key:a", "monthly", 1.5, 0)
	late := c.now()
	s.Spend(1, late, everyScope, nil)
	if got := exhausted(s, d); got == "" {
		t.Error("daily: not exhausted before midnight")
	}
	c.add(time.Second) // 2026-12-01T00:00:00Z
	if got := exhausted(s, d); got != "" {
		t.Errorf("daily after midnight: %q", got)
	}
	if got := exhausted(s, m); got != "" {
		t.Errorf("monthly on day 1: %q", got)
	}
	s.Spend(0.25, c.now(), everyScope, nil)
	if x := s.spend["key:a"]; x.day.period != "d:2026-12-01" || x.month.period != "m:2026-12" {
		t.Errorf("periods held %q %q", x.day.period, x.month.period)
	}
	s.Spend(0.5, late, everyScope, nil) // ended at 23:59:59, charged after a charge of the new day
	if got, got2 := s.Spent(d), s.Spent(m); got != 0.75 || got2 != 0.75 {
		t.Errorf("after a late charge: daily %v, monthly %v", got, got2)
	}
	c.add(24 * time.Hour)
	if got, got2 := s.Spent(d), s.Spent(m); got != 0 || got2 != 0.75 {
		t.Errorf("next day: daily %v, monthly %v", got, got2)
	}
}

// TestNever is LIM-2 for never budgets: the counter starts at zero at its
// since, keeps counting under the same since whatever the limit, does not
// count a charge of an older since (a request under an older snapshot), and
// starts again at zero at a later since. A daily or monthly charge does not
// touch it, and it never rolls over.
func TestNever(t *testing.T) {
	c := newClock()
	s := New(c.now)
	n3 := budget("key:a", "never", 1, 3)
	s.Spend(0.5, c.now(), everyScope, []Budget{n3})
	s.Spend(0.25, c.now(), everyScope, []Budget{budget("key:a", "never", 10, 3)}) // limit_usd changed, same since
	if got := s.Spent(n3); got != 0.75 {
		t.Errorf("since 3: %v", got)
	}
	s.Spend(5, c.now(), everyScope, []Budget{budget("key:a", "never", 1, 2)}) // an older snapshot
	s.Spend(5, c.now(), everyScope, []Budget{budget("key:a", "daily", 1, 0)})
	c.add(40 * 24 * time.Hour)
	if got := s.Spent(n3); got != 0.75 {
		t.Errorf("since 3 after other charges and 40 days: %v", got)
	}
	n5 := budget("key:a", "never", 1, 5)
	if got := s.Spent(n5); got != 0 {
		t.Errorf("set again at 5, before a charge: %v", got)
	}
	s.Spend(0.125, c.now(), everyScope, []Budget{n5})
	s.Spend(1, c.now(), everyScope, []Budget{n3}) // a request that started before 5
	if got, old := s.Spent(n5), s.Spent(n3); got != 0.125 || old != 0.125 {
		t.Errorf("since 5: %v; read under since 3: %v", got, old)
	}
	s.SeedNever("model:m", 7, 2)
	if got := exhausted(s, budget("model:m", "never", 2, 7)); got == "" {
		t.Error("a seeded total at its limit admits")
	}
}

// TestWarnings is LIM-3 and kick-off section 10: the charge that moves a
// counter from below 80 percent to 80 percent or above returns one
// budget_warning, and the one that reaches 100 percent one budget_exhausted,
// once per period, both from one charge when it crosses both, dated at the
// charge's end and keyed by the budget's period; a new period warns again;
// a counter seeded above a threshold does not warn on its next charge; a
// daily limit raised in its period does not warn again in it, a never
// limit raised does.
func TestWarnings(t *testing.T) {
	c := newClock()
	c.set(time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))
	s := New(c.now)
	d, n := budget("key:a", "daily", 1, 0), budget("model:m", "never", 2, 4)
	bs := []Budget{d, n}
	spend := func(usd float64) string { return fmt.Sprint(s.Spend(usd, c.now(), everyScope, bs)) }
	at := c.now().String()
	if got := spend(0.5); got != "[]" {
		t.Errorf("at 50 percent: %s", got)
	}
	if got, want := spend(0.375), "[{budget_warning key:a d:2026-11-14 "+at+"}]"; got != want {
		t.Errorf("at 87.5 percent: %s, want %s", got, want)
	}
	if got := spend(0.0625); got != "[]" {
		t.Errorf("at 93.75 percent: %s", got)
	}
	if got, want := spend(0.75), "[{budget_exhausted key:a d:2026-11-14 "+at+"} {budget_warning model:m n:4:2 "+at+"}]"; got != want {
		t.Errorf("at 168.75 and 84.375 percent: %s, want %s", got, want)
	}
	if got, want := spend(0.5), "[{budget_exhausted model:m n:4:2 "+at+"}]"; got != want {
		t.Errorf("at 109.375 percent: %s, want %s", got, want)
	}
	// limit_usd raised to 10 under the same since: a new period n:4:10.
	if got, want := fmt.Sprint(s.Spend(6, c.now(), everyScope, []Budget{budget("model:m", "never", 10, 4)})),
		"[{budget_warning model:m n:4:10 "+at+"}]"; got != want {
		t.Errorf("never at 81.875 percent of a raised limit: %s, want %s", got, want)
	}
	c.add(24 * time.Hour)
	at = c.now().String()
	if got, want := spend(1.5), "[{budget_warning key:a d:2026-11-15 "+at+"} {budget_exhausted key:a d:2026-11-15 "+at+"}]"; got != want {
		t.Errorf("next day: %s, want %s", got, want)
	}

	r := New(c.now)
	r.SeedSpend("key:a", "d:2026-11-15", 0.9)
	if got := fmt.Sprint(r.Spend(0.05, c.now(), everyScope, []Budget{d})); got != "[]" {
		t.Errorf("seeded at 90 percent: %s", got)
	}

	// A daily limit doubled on the day it warned: its new 80 percent is
	// crossed in the same period, which already warned.
	r = New(c.now)
	r.Spend(0.875, c.now(), everyScope, []Budget{d})
	if got := fmt.Sprint(r.Spend(0.75, c.now(), everyScope, []Budget{budget("key:a", "daily", 2, 0)})); got != "[]" {
		t.Errorf("at 81.25 percent of a doubled limit, same day: %s", got)
	}
}

// TestSeedSpend: a later period replaces the one held, an earlier one is
// ignored, the same one adds.
func TestSeedSpend(t *testing.T) {
	c := newClock()
	c.set(time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))
	s := New(c.now)
	for _, p := range []struct {
		period string
		usd    float64
	}{{"d:2026-11-02", 1}, {"m:2026-11", 1}, {"d:2026-11-14", 2}, {"m:2026-11", 2}, {"d:2026-11-03", 4}, {"d:2026-11-14", 8}} {
		s.SeedSpend("global", p.period, p.usd)
	}
	if d, m := s.Spent(budget("global", "daily", 1, 0)), s.Spent(budget("global", "monthly", 1, 0)); d != 10 || m != 3 {
		t.Errorf("daily %v, monthly %v", d, m)
	}
}

// TestSpendConcurrent: concurrent charges and checks under -race; every
// charge counts once.
func TestSpendConcurrent(t *testing.T) {
	c := newClock()
	s := New(c.now)
	n := budget("global", "never", 1000, 1)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			for range 20 {
				s.Spend(0.25, c.now(), everyScope, []Budget{n})
				_ = s.Exhausted(n)
			}
		})
	}
	wg.Wait()
	if got := s.Spent(n); got != 250 {
		t.Errorf("spent %v, want 250", got)
	}
	if got := s.Spent(budget("key:a", "daily", 1, 0)); got != 250 {
		t.Errorf("key spent %v, want 250", got)
	}
}
