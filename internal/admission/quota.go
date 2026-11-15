package admission

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
)

// Plan is the quota of one subscription provider as a request's snapshot
// publishes it, and the request's key with its share of it (LIM-8, LIM-10).
type Plan struct {
	Provider string
	Windows  []domain.QuotaWindow
	Key      string
	Share    float64 // the key's quota_share of this provider; 0 when none
	// Revision is the snapshot's: a plan of an older revision than one
	// already seen drops no window, so a request still under the older
	// snapshot never drops a window the newer one declares.
	Revision int64
}

// Event is one stored subscription call as a quota seed reads it.
type Event struct {
	Key         string
	Sent, Ended time.Time // started_at, completed_at
	Tokens      int64     // input + output
}

// Seed reads the events of provider that ended at or after from, from the
// store (usage.Seeder).
type Seed func(provider string, from time.Time) ([]Event, error)

// WindowView is one quota window as usage quota shows it.
type WindowView struct {
	Provider string
	Key      string  // "" for the provider's own window, else the key whose share it is
	Share    float64 // the key's quota_share; 0 for the provider's window
	Window   string  // as declared, such as 5h
	Unit     string  // requests or tokens
	Used     int64
	Limit    int64 // the declared limit; for a key, times its share, rounded down
	// RetryAfter is, once Used reaches Limit, the time until the window
	// admits again, in whole seconds; else 0.
	RetryAfter time.Duration
}

// holder names whose windows they are: a provider's own (key "") or a key's
// share of it.
type holder struct{ provider, key string }

// quota is one window of a holder: its span and unit identify it, and the
// rest is as the latest plan declared it. warned is set while the window is
// at or above 80 percent when Ended last looked.
type quota struct {
	span   time.Duration
	tokens bool
	window string
	share  float64
	limit  int64
	warned bool
	x      *window
}

// wants are the windows of p, as quotas without a window: the provider's,
// then, with a share, the key's at the share of each limit, rounded down so
// that a key never passes its fraction. A window declared twice counts once,
// at the lower limit. A window whose text a stored configuration holds but
// validation refuses counts over MaxQuotaSpan, the reading that refuses
// soonest; a negative limit or share always refuses.
func (p Plan) wants() (h [2]holder, ws [2][]quota) {
	h = [2]holder{{p.Provider, ""}, {p.Provider, p.Key}}
	for i, share := range []float64{1, p.Share} {
		if share == 0 || i == 1 && p.Key == "" {
			continue
		}
		for _, q := range p.Windows {
			span, ok := domain.QuotaSpan(q.Window)
			if !ok {
				span = domain.MaxQuotaSpan
			}
			for _, u := range []struct {
				tokens bool
				n      int64
			}{{false, q.Requests}, {true, q.Tokens}} {
				if u.n == 0 {
					continue
				}
				w := quota{span: span, tokens: u.tokens, window: q.Window, limit: u.n}
				if i == 1 {
					w.share = share
					if share < 1 {
						w.limit = int64(math.Floor(share*float64(u.n) + 1e-9))
					}
				}
				if j := slices.IndexFunc(ws[i], w.same); j >= 0 {
					ws[i][j].limit = min(ws[i][j].limit, w.limit)
					continue
				}
				ws[i] = append(ws[i], w)
			}
		}
	}
	return h, ws
}

func (q quota) same(o quota) bool { return q.span == o.span && q.tokens == o.tokens }

func (q quota) unit() string {
	if q.tokens {
		return "tokens"
	}
	return "requests"
}

// find is the window of h like w, nil when not held. The caller holds the
// mutex.
func (s *State) find(h holder, w quota) *quota {
	if i := slices.IndexFunc(s.quotas[h], func(q *quota) bool { return q.same(w) }); i >= 0 {
		return s.quotas[h][i]
	}
	return nil
}

// errSeed is the refusal while a window cannot be seeded (RES-10): no
// window admits on a partial count.
func errSeed() *domain.Error {
	return domain.NewError(domain.CodeUsageStoreUnavailable, "the usage store is unavailable")
}

// hold locks the mutex with every window of p held. A plan as recent as any
// seen first drops the windows of its holders it no longer declares and
// takes its limits; then the windows not held are seeded from the store,
// outside the mutex, one seed in flight per provider, at the first use that
// needs them (phase-3 design section 9); every use of the provider waits for
// its seed in flight. On a failed seed it returns the
// error with the mutex unlocked and nothing held, so the next use tries
// again; else nil with the mutex locked.
func (s *State) hold(p Plan, h [2]holder, ws [2][]quota) error {
	s.mu.Lock()
	for {
		if p.Revision >= s.rev {
			s.rev = p.Revision
			for i := range h {
				if i == 1 && h[1].key == "" {
					break
				}
				s.quotas[h[i]] = slices.DeleteFunc(s.quotas[h[i]], func(q *quota) bool {
					j := slices.IndexFunc(ws[i], q.same)
					if j >= 0 {
						q.window, q.share, q.limit = ws[i][j].window, ws[i][j].share, ws[i][j].limit
					}
					return j < 0
				})
				if len(s.quotas[h[i]]) == 0 {
					delete(s.quotas, h[i])
				}
			}
		}
		var miss [2][]quota
		var longest time.Duration
		for i := range h {
			for _, w := range ws[i] {
				if s.find(h[i], w) == nil {
					miss[i] = append(miss[i], w)
					// A window counts up to one bucket more of the past
					// than its span: the seed reads that bucket too.
					longest = max(longest, SeedSpan(w.span))
				}
			}
		}
		// A use that needs no seed still waits for the provider's seed in
		// flight: a call it takes between the seed's flying snapshot and the
		// install would count in neither the new window nor the store.
		if ch := s.seeding[p.Provider]; ch != nil {
			s.mu.Unlock()
			<-ch
			s.mu.Lock()
			continue
		}
		if longest == 0 {
			return nil
		}
		ch := make(chan struct{})
		s.seeding[p.Provider] = ch
		from := s.now().Add(-longest)
		// The calls taken and not yet ended have no record for the seed to
		// read; taken before it starts, a call ending during the query may
		// count twice, never none.
		flying := [2][]time.Time{slices.Clone(s.flying[h[0]]), slices.Clone(s.flying[h[1]])}
		s.mu.Unlock()
		events, err := s.seedOutside(p.Provider, from, ch)
		if err != nil {
			return err
		}
		now := s.now()
		for i := range h {
			for _, w := range miss[i] {
				if s.find(h[i], w) == nil { // another seed may have held it since
					s.quotas[h[i]] = append(s.quotas[h[i]], seeded(w, h[i].key, events, flying[i], now))
				}
			}
		}
	}
}

// seedOutside runs the seed with the mutex unlocked, and ends the provider's
// seed in flight whatever happens, a panic included. After a seed that
// succeeds it returns with the mutex locked, so the caller holds the windows
// in the lock section that ends the seed in flight: no waiter can see neither
// and seed again. Otherwise it unlocks.
func (s *State) seedOutside(provider string, from time.Time, ch chan struct{}) (events []Event, err error) {
	ok := false
	defer func() {
		s.mu.Lock()
		delete(s.seeding, provider)
		close(ch)
		if !ok {
			s.mu.Unlock()
		}
	}()
	events, err = s.seed(provider, from)
	ok = err == nil
	return events, err
}

// seeded is window w of a holder of key ("" for the provider) counted from
// the store's events at now: requests by Sent and tokens by Ended, a key's
// window only from the key's own events; and, for requests, the holder's
// calls in flight, at their Take, flying. Its times are placed by their Unix
// time, as the events' are, so a seeded window equals the live one for the
// same committed events (window, Round(0)). An event dated after now (a
// clock stepped back) counts at now: placed later, it would move the ring
// ahead and clear older events still in the window. A window seeded at or
// above 80 percent does not warn again until it has dropped below.
func seeded(w quota, key string, events []Event, flying []time.Time, now time.Time) *quota {
	q := w
	q.x = newWindow(w.span, now.Round(0))
	if !w.tokens {
		for _, t := range flying {
			q.x.add(earliest(t, now), 1)
		}
	}
	for _, e := range events {
		switch {
		case key != "" && e.Key != key:
		case w.tokens && e.Tokens > 0:
			q.x.add(earliest(e.Ended, now), e.Tokens)
		case !w.tokens:
			q.x.add(earliest(e.Sent, now), 1)
		}
	}
	q.warned = float64(q.x.sum(now)) >= 0.8*float64(q.limit)
	return &q
}

func earliest(t, now time.Time) time.Time {
	if t.After(now) {
		return now
	}
	return t
}

// Quota is the check of p alone (LIM-9, LIM-10): 429 quota_exceeded when a
// window held of the provider, or of the key's share, is at or above its
// latest limit now (for a plan of an older revision, also a window only the
// newer declares), with the largest wait of those windows as Retry-After, rounded up and
// uncapped; 503 usage_store_unavailable while a window cannot be seeded.
func (s *State) Quota(p Plan) *domain.Error {
	h, ws := p.wants()
	if s.hold(p, h, ws) != nil {
		return errSeed()
	}
	defer s.mu.Unlock()
	return s.exhausted(p, h, s.now())
}

// Take checks p again and, when no window refuses, counts one request now in
// every request window held of p's holders, under the same lock, so
// concurrent requests cannot pass a limit between the check and the count
// (LIM-8), and keeps the time as a call in flight of each holder until
// Ended, for a window seeded meanwhile. exclude calls it last, after the
// circuit's permit, so every request counted is a call that leaves a record.
func (s *State) Take(p Plan) *domain.Error {
	h, ws := p.wants()
	if s.hold(p, h, ws) != nil {
		return errSeed()
	}
	defer s.mu.Unlock()
	now := s.now()
	if e := s.exhausted(p, h, now); e != nil {
		return e
	}
	for i := range h {
		if i == 1 && h[1].key == "" {
			break
		}
		for _, q := range s.quotas[h[i]] {
			if !q.tokens {
				count(q.x, now, 1)
			}
		}
		s.flying[h[i]] = append(s.flying[h[i]], now)
	}
	return nil
}

// count adds n at at to a quota window held. A time older than the window's
// ring, which only a wall clock stepped back by more than the window can
// give, counts in the newest bucket held instead of in none, so that it
// still refuses: it then stays counted until the wall clock is a window past
// that bucket.
func count(x *window, at time.Time, n int64) {
	if x.pos(at)/int64(x.g) <= x.last-int64(len(x.buckets)) {
		at = time.Unix(0, x.last*int64(x.g))
	}
	x.add(at, n)
}

// exhausted is Quota's answer at now. The caller holds the mutex with every
// window of p held.
func (s *State) exhausted(p Plan, h [2]holder, now time.Time) *domain.Error {
	var wait time.Duration
	provider := false
	for i := range h {
		if i == 1 && h[1].key == "" {
			break
		}
		for _, q := range s.quotas[h[i]] {
			if q.x.sum(now) >= q.limit {
				wait = max(wait, q.x.wait(now, float64(q.limit)))
				provider = provider || i == 0
			}
		}
	}
	if wait == 0 { // a refusing window waits at least 1 s
		return nil
	}
	msg := fmt.Sprintf("the quota of provider %q is exhausted", p.Provider)
	if !provider {
		msg = fmt.Sprintf("the quota share of this key for provider %q is exhausted", p.Provider)
	}
	e := domain.NewError(domain.CodeQuotaExceeded, msg)
	e.RetryAfter = strconv.FormatInt(int64(wait/time.Second), 10)
	return e
}

// Ended ends a call that Take counted: it drops the oldest call in flight
// of each of p's holders (which one does not matter for the count, and those
// left are placed no earlier than the calls still in flight), counts tokens,
// the call's input plus output, at at, its completed_at, in every token
// window held now of p's holders, those declared since Take included
// (LIM-8), and returns a quota_warning for each of their windows that is at
// or above 80 percent of its limit now and was below it when Ended last
// looked: a sliding window has no period, so its period is the provider, the
// window, the unit and the Unix second of at. The request a call counted at
// Take is seen here, at its end.
func (s *State) Ended(p Plan, tokens int64, at time.Time) []Warning {
	h, _ := p.wants()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Warning
	for i := range h {
		if i == 1 && h[1].key == "" {
			break
		}
		if f := s.flying[h[i]]; len(f) > 1 {
			s.flying[h[i]] = f[1:]
		} else {
			delete(s.flying, h[i])
		}
		for _, q := range s.quotas[h[i]] {
			if q.tokens && tokens > 0 {
				count(q.x, at, tokens)
			}
			if float64(q.x.sum(at)) < 0.8*float64(q.limit) {
				q.warned = false
				continue
			}
			if !q.warned {
				q.warned = true
				scope := "provider:" + p.Provider
				if i == 1 {
					scope = "key:" + p.Key
				}
				out = append(out, Warning{Event: "quota_warning", Scope: scope,
					Period: fmt.Sprintf("%s:%s:%s:%d", p.Provider, q.window, q.unit(), at.Unix()), At: at})
			}
		}
	}
	return out
}

// Sync holds every window of every subscription provider's quota, and of
// every key's share of one, as the configuration of revision rev declares
// them, seeding those not held (RES-9); at a revision as recent as any seen
// it then drops every other window. serve calls it before the listeners
// bind, and usage quota before Windows.
func (s *State) Sync(rev int64, providers map[string]domain.Provider, keys map[string]domain.Key) error {
	var ps []Plan
	for _, name := range slices.Sorted(maps.Keys(providers)) {
		if p := providers[name]; p.Billing == domain.BillingSubscription {
			ps = append(ps, Plan{Provider: name, Windows: p.Quota, Revision: rev})
			for _, k := range slices.Sorted(maps.Keys(keys)) {
				if share := keys[k].QuotaShare[name]; share != 0 {
					ps = append(ps, Plan{Provider: name, Windows: p.Quota, Key: k, Share: share, Revision: rev})
				}
			}
		}
	}
	for _, p := range ps {
		h, ws := p.wants()
		if err := s.hold(p, h, ws); err != nil {
			return err
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rev < s.rev {
		return nil
	}
	for h := range s.quotas {
		if !slices.ContainsFunc(ps, func(p Plan) bool { return p.Provider == h.provider && (h.key == "" || p.Key == h.key) }) {
			delete(s.quotas, h)
		}
	}
	return nil
}

// Windows is every quota window held, by provider, the provider's own before
// the keys' by name, then by span, requests before tokens, as last declared,
// with its use now (usage quota).
func (s *State) Windows() []WindowView {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	type held struct {
		h holder
		q *quota
	}
	var all []held
	for h, qs := range s.quotas {
		for _, q := range qs {
			all = append(all, held{h, q})
		}
	}
	slices.SortFunc(all, func(a, b held) int {
		return cmp.Or(cmp.Compare(a.h.provider, b.h.provider), cmp.Compare(a.h.key, b.h.key), cmp.Compare(a.q.span, b.q.span),
			cmp.Compare(a.q.unit(), b.q.unit()))
	})
	vs := make([]WindowView, len(all))
	for i, a := range all {
		v := WindowView{Provider: a.h.provider, Key: a.h.key, Share: a.q.share, Window: a.q.window, Unit: a.q.unit(),
			Used: a.q.x.sum(now), Limit: a.q.limit}
		if v.Used >= v.Limit {
			v.RetryAfter = a.q.x.wait(now, float64(v.Limit))
		}
		vs[i] = v
	}
	return vs
}
