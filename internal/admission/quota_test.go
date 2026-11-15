package admission

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
)

// store is a fake usage store for Seed: the events committed so far, and the
// seeds it answered.
type store struct {
	mu     sync.Mutex
	events []Event
	seeds  []string // provider@from, in order
	fail   error    // the next seed fails with it, once
	hold   chan struct{}
}

func (st *store) commit(e Event) {
	st.mu.Lock()
	defer st.mu.Unlock()
	// The store keeps milliseconds.
	e.Sent, e.Ended = time.UnixMilli(e.Sent.UnixMilli()), time.UnixMilli(e.Ended.UnixMilli())
	st.events = append(st.events, e)
}

// seed answers the events that ended at or after from, as usage.Seeder
// does; every event of the fake is the provider's.
func (st *store) seed(provider string, from time.Time) ([]Event, error) {
	if st.hold != nil {
		<-st.hold
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.seeds = append(st.seeds, fmt.Sprintf("%s@%d", provider, from.Unix()))
	if err := st.fail; err != nil {
		st.fail = nil
		return nil, err
	}
	var out []Event
	for _, e := range st.events {
		if !e.Ended.Before(from) {
			out = append(out, e)
		}
	}
	return out, nil
}

func plan(rev int64, key string, share float64, ws ...domain.QuotaWindow) Plan {
	return Plan{Provider: "sub", Windows: ws, Key: key, Share: share, Revision: rev}
}

func reqs(w string, n int64) domain.QuotaWindow { return domain.QuotaWindow{Window: w, Requests: n} }

// quotaErr is what Quota or Take answered: "" when admitted, else the code,
// Retry-After and the message.
func quotaErr(e *domain.Error) string {
	if e == nil {
		return ""
	}
	return e.Code + " " + e.RetryAfter + " " + e.Message
}

// used is the Used of every window held, as "<provider>/<key>/<window>/<unit>=<used>/<limit>".
func used(s *State) []string {
	var out []string
	for _, v := range s.Windows() {
		out = append(out, fmt.Sprintf("%s/%s/%s/%s=%d/%d", v.Provider, v.Key, v.Window, v.Unit, v.Used, v.Limit))
	}
	return out
}

// TestQuotaExhaustion is LIM-8, LIM-9 and LIM-10 on the test clock: Take
// counts a request, Quota only checks; a window at its limit refuses with
// 429 quota_exceeded and the largest wait of the windows that refuse, the
// time until each admits again, rounded up; a key's share refuses that key
// only, at its fraction rounded down, while the plan serves other keys; the
// plan's exhaustion refuses every key, naming the provider.
func TestQuotaExhaustion(t *testing.T) {
	c := newClock()
	st := &store{}
	s := New(c.now, st.seed)
	two := []domain.QuotaWindow{reqs("1h", 4), reqs("5h", 5)}
	a, b := plan(1, "a", 0.5, two...), plan(1, "b", 0, two...)
	for i := range 2 {
		if e := s.Take(a); e != nil {
			t.Fatalf("take %d: %v", i, e)
		}
	}
	// 0.5 of 4 is 2 and 0.5 of 5 is 2.5, read as 2: both windows of a
	// refuse. t0 is 2 s into a 6 s bucket of the 1h window and 20 s into a
	// 30 s bucket of the 5h window, so they admit again 1h + 4 s and 5h +
	// 10 s later: the larger is the answer.
	if e := s.Quota(a); quotaErr(e) != `quota_exceeded 18010 the quota share of this key for provider "sub" is exhausted` {
		t.Errorf("a at its share: %v", quotaErr(e))
	}
	for i := range 2 {
		if e := s.Take(b); e != nil {
			t.Fatalf("b, take %d: %v", i, e)
		}
	}
	c.add(time.Minute)
	if e := s.Take(b); quotaErr(e) != `quota_exceeded 3544 the quota of provider "sub" is exhausted` {
		t.Errorf("the 1h window at 4: %v", quotaErr(e))
	}
	if got := used(s); fmt.Sprint(got) != "[sub//1h/requests=4/4 sub//5h/requests=4/5 sub/a/1h/requests=2/2 sub/a/5h/requests=2/2]" {
		t.Errorf("windows %v", got)
	}
	// An hour later the 1h window admits; one more request fills the 5h one.
	c.add(time.Hour)
	if e := s.Take(b); e != nil {
		t.Fatalf("an hour later: %v", e)
	}
	if e := s.Take(b); quotaErr(e) != `quota_exceeded 14350 the quota of provider "sub" is exhausted` {
		t.Errorf("the 5h window at 5: %v", quotaErr(e))
	}
	if v := s.Windows()[1]; v.Window != "5h" || v.RetryAfter != 14350*time.Second {
		t.Errorf("view %+v", v)
	}
	if len(st.seeds) != 1 {
		t.Errorf("seeds %v, want one at the first use", st.seeds)
	}
}

// TestTakeAtomic (-race): concurrent Takes never pass a limit, as the check
// and the count are under one lock.
func TestTakeAtomic(t *testing.T) {
	c := newClock()
	s := New(c.now, (&store{}).seed)
	p := plan(1, "a", 0, reqs("5h", 7))
	var ok atomic.Int64
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if s.Take(p) == nil {
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 7 {
		t.Errorf("%d admitted, want 7", ok.Load())
	}
}

// TestQuotaTokens: Ended counts tokens at the call's end in the token
// windows; a token window refuses once its sum reaches the limit.
func TestQuotaTokens(t *testing.T) {
	c := newClock()
	s := New(c.now, (&store{}).seed)
	p := plan(1, "a", 0, domain.QuotaWindow{Window: "1h", Requests: 100, Tokens: 1000})
	if e := s.Take(p); e != nil {
		t.Fatal(e)
	}
	s.Ended(p, 999, c.now())
	if e := s.Quota(p); e != nil {
		t.Errorf("at 999 of 1000 tokens: %v", e)
	}
	s.Ended(p, 1, c.now())
	if e := s.Quota(p); e == nil || e.Code != domain.CodeQuotaExceeded {
		t.Errorf("at 1000 of 1000 tokens: %v", e)
	}
	if got := used(s); fmt.Sprint(got) != "[sub//1h/requests=1/100 sub//1h/tokens=1000/1000]" {
		t.Errorf("windows %v", got)
	}
}

// TestQuotaWarnings is the 80 percent crossing of LIM-9: one quota_warning
// per window and crossing, for the provider's window and the key's share,
// seen at the end of the call; none again while the window stays above, and
// one again after it has dropped below, with the Unix second of the new
// crossing in its period.
func TestQuotaWarnings(t *testing.T) {
	c := newClock()
	s := New(c.now, (&store{}).seed)
	p := plan(1, "a", 0.5, reqs("1h", 10))
	var got []string
	call := func() {
		if e := s.Take(p); e != nil {
			t.Fatal(e)
		}
		for _, w := range s.Ended(p, 3, c.now()) {
			got = append(got, w.Event+" "+w.Scope+" "+w.Period)
		}
	}
	for range 5 {
		call()
	}
	// The share's 5 crossed 80 % at 4; the plan's 10 crossed nothing.
	if want := fmt.Sprintf("[quota_warning key:a sub:1h:requests:%d]", t0.Unix()); fmt.Sprint(got) != want {
		t.Errorf("warnings %v, want %s", got, want)
	}
	pb := plan(1, "b", 0, reqs("1h", 10))
	for range 3 {
		if e := s.Take(pb); e != nil {
			t.Fatal(e)
		}
		got = append(got, fmt.Sprint(s.Ended(pb, 0, c.now())))
	}
	if n := strings.Count(fmt.Sprint(got), "provider:sub"); n != 1 {
		t.Errorf("warnings %v: want one for the plan at 8 of 10", got)
	}
	got = nil
	c.add(time.Hour + 10*time.Second)
	for range 4 {
		call()
	}
	if want := fmt.Sprintf("[quota_warning key:a sub:1h:requests:%d]", c.now().Unix()); fmt.Sprint(got) != want {
		t.Errorf("after the window: %v, want %s", got, want)
	}
}

// TestQuotaSeed is the seed of RES-9 and LIM-8: a window starts from the
// store's events of its span, requests by the time sent and tokens by the
// time ended; a key's share only from its own; a seed that fails refuses
// the provider with 503 usage_store_unavailable and holds nothing, so the
// next use seeds again; concurrent first uses share one seed in flight.
func TestQuotaSeed(t *testing.T) {
	c := newClock()
	st := &store{fail: errors.New("disk I/O error")}
	for _, e := range []Event{
		{Key: "a", Sent: t0.Add(-2 * time.Hour), Ended: t0.Add(-2 * time.Hour), Tokens: 9}, // outside the 1h window
		{Key: "a", Sent: t0.Add(-61 * time.Minute), Ended: t0.Add(-59 * time.Minute), Tokens: 5},
		{Key: "a", Sent: t0.Add(-10 * time.Minute), Ended: t0.Add(-9 * time.Minute), Tokens: 7},
		{Key: "b", Sent: t0.Add(-5 * time.Minute), Ended: t0.Add(-5 * time.Minute), Tokens: 11},
	} {
		st.commit(e)
	}
	s := New(c.now, st.seed)
	p := plan(1, "a", 0.5, domain.QuotaWindow{Window: "1h", Requests: 100, Tokens: 1000})
	if e := s.Quota(p); e == nil || e.Code != domain.CodeUsageStoreUnavailable || e.Status != 503 {
		t.Fatalf("a failing seed: %v", e)
	}
	if got := used(s); len(got) != 0 {
		t.Errorf("held after a failed seed: %v", got)
	}
	st.hold = make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if e := s.Quota(p); e != nil {
				t.Errorf("after the seed: %v", e)
			}
		})
	}
	time.Sleep(20 * time.Millisecond) // let them all wait for the one seed
	close(st.hold)
	wg.Wait()
	if len(st.seeds) != 2 || st.seeds[1] != fmt.Sprintf("sub@%d", t0.Add(-time.Hour-6*time.Second).Unix()) {
		t.Errorf("seeds %v, want the failed one and one more from an hour and a 6 s bucket back", st.seeds)
	}
	want := "[sub//1h/requests=2/100 sub//1h/tokens=23/1000 sub/a/1h/requests=1/50 sub/a/1h/tokens=12/500]"
	if got := used(s); fmt.Sprint(got) != want {
		t.Errorf("seeded %v, want %s", got, want)
	}
}

// TestQuotaDeclaredWhileRunning: a window declared, or a share added, while
// running is seeded at its first use, so earlier use counts, a call still in
// flight included; the windows already held keep their live counts; a plan
// of an older revision drops nothing; a window removed by a newer plan is
// dropped, and declared again, seeded again from the store.
func TestQuotaDeclaredWhileRunning(t *testing.T) {
	c := newClock()
	st := &store{}
	st.commit(Event{Key: "a", Sent: t0.Add(-90 * time.Minute), Ended: t0.Add(-90 * time.Minute)})
	s := New(c.now, st.seed)
	one := plan(1, "a", 0, reqs("1h", 100))
	for range 3 {
		if e := s.Take(one); e != nil {
			t.Fatal(e)
		}
		s.Ended(one, 0, c.now())
		st.commit(Event{Key: "a", Sent: c.now(), Ended: c.now()})
	}
	c.add(time.Minute)
	both := plan(2, "a", 0.5, reqs("1h", 100), reqs("2h", 100))
	if e := s.Quota(both); e != nil {
		t.Fatal(e)
	}
	want := "[sub//1h/requests=3/100 sub//2h/requests=4/100 sub/a/1h/requests=3/50 sub/a/2h/requests=4/50]"
	if got := used(s); fmt.Sprint(got) != want {
		t.Errorf("after the new window and share %v, want %s", got, want)
	}
	if e := s.Quota(one); e != nil { // a request still under revision 1
		t.Fatal(e)
	}
	if got := used(s); fmt.Sprint(got) != want {
		t.Errorf("an older plan changed the windows: %v", got)
	}
	if e := s.Take(plan(3, "a", 0, reqs("2h", 100))); e != nil {
		t.Fatal(e)
	}
	if got := used(s); fmt.Sprint(got) != "[sub//2h/requests=5/100]" {
		t.Errorf("after the 1h window and the share are removed: %v", got)
	}
	if e := s.Quota(plan(4, "a", 0, reqs("1h", 100), reqs("2h", 100))); e != nil {
		t.Fatal(e)
	}
	// The 1h window is seeded again: the store has 3 in the hour, and the
	// call of revision 3, not stored yet, is in flight.
	if got := used(s); fmt.Sprint(got) != "[sub//1h/requests=4/100 sub//2h/requests=5/100]" {
		t.Errorf("declared again: %v", got)
	}
	if n := len(st.seeds); n != 3 {
		t.Errorf("seeds %v, want 3", st.seeds)
	}
}

// TestQuotaInFlight: a window declared or lengthened, or a share added, while
// calls taken earlier are still in flight (no record yet) counts them at
// their Take, so they cannot be admitted again; a request of an older
// revision is checked against it too; once the calls have ended and their
// records are stored, a new seed counts each once.
func TestQuotaInFlight(t *testing.T) {
	c := newClock()
	st := &store{}
	s := New(c.now, st.seed)
	none, short := plan(1, "a", 0), plan(1, "a", 0, reqs("1h", 2))
	for _, p := range []Plan{none, short} {
		if e := s.Take(p); e != nil {
			t.Fatal(e)
		}
	}
	c.add(time.Minute)
	newer := func(rev int64) Plan { return plan(rev, "a", 0.5, reqs("1h", 4), reqs("2h", 2)) }
	// The 2h window waits longest: t0 is 8 s into a 12 s bucket.
	if e := s.Quota(newer(2)); quotaErr(e) != `quota_exceeded 7144 the quota of provider "sub" is exhausted` {
		t.Errorf("2 calls in flight under 2h:2: %v; windows %v", quotaErr(e), used(s))
	}
	want := "[sub//1h/requests=2/4 sub//2h/requests=2/2 sub/a/1h/requests=2/2 sub/a/2h/requests=2/1]"
	if got := used(s); fmt.Sprint(got) != want {
		t.Errorf("windows %v, want %s", got, want)
	}
	if e := s.Take(plan(1, "b", 0)); e == nil || e.Code != domain.CodeQuotaExceeded {
		t.Errorf("a request of revision 1 passed the 2h window of revision 2: %v", e)
	}
	for _, p := range []Plan{none, short} {
		s.Ended(p, 0, c.now())
		st.commit(Event{Key: "a", Sent: t0, Ended: c.now()})
	}
	if e := s.Quota(plan(3, "a", 0)); e != nil {
		t.Fatal(e)
	}
	if e := s.Quota(newer(4)); e == nil {
		t.Errorf("seeded again: admitted; windows %v", used(s))
	}
	if got := used(s); fmt.Sprint(got) != want {
		t.Errorf("seeded again after the calls ended: %v, want %s", got, want)
	}
}

// TestQuotaTakeDuringSeed: a window declared, or a share added, by a newer
// revision counts every call of the provider taken from its seed on, those of
// an older plan that declares no such window included: one taken while the
// seed is in flight waits for it, as otherwise it would count in neither the
// seed's calls in flight nor its store events, and one taken after counts in
// the window held.
func TestQuotaTakeDuringSeed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		old, newer Plan
		want       string
	}{
		{"window", plan(1, "a", 0), plan(2, "a", 0, reqs("1h", 3)), "[sub//1h/requests=3/3]"},
		{"share", plan(1, "a", 0, reqs("1h", 9)), plan(2, "a", 0.5, reqs("1h", 6)), "[sub//1h/requests=3/6 sub/a/1h/requests=3/3]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClock()
			st := &store{}
			s := New(c.now, st.seed)
			if e := s.Quota(tc.old); e != nil {
				t.Fatal(e)
			}
			st.hold = make(chan struct{})
			seeded, took := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(seeded)
				if e := s.Quota(tc.newer); e != nil {
					t.Error(e)
				}
			}()
			for inFlight := false; !inFlight; runtime.Gosched() {
				s.mu.Lock()
				inFlight = s.seeding["sub"] != nil
				s.mu.Unlock()
			}
			go func() {
				defer close(took)
				if e := s.Take(tc.old); e != nil {
					t.Error(e)
				}
			}()
			time.Sleep(20 * time.Millisecond) // let the Take reach the seed in flight
			close(st.hold)
			<-seeded
			<-took
			for _, p := range []Plan{tc.old, tc.newer} {
				if e := s.Take(p); e != nil {
					t.Fatal(e)
				}
			}
			if e := s.Take(tc.newer); e == nil {
				t.Errorf("a fourth call admitted; windows %v", used(s))
			}
			if got := used(s); fmt.Sprint(got) != tc.want {
				t.Errorf("windows %v, want %s", got, tc.want)
			}
		})
	}
}

// TestQuotaTokensDeclaredMidCall: a call's tokens count at its end in every
// token window held then, of the provider and of the key's share, though its
// own older plan declared none, and cross 80 percent of the latest limit.
func TestQuotaTokensDeclaredMidCall(t *testing.T) {
	c := newClock()
	s := New(c.now, (&store{}).seed)
	old := plan(1, "a", 0)
	if e := s.Take(old); e != nil {
		t.Fatal(e)
	}
	newer := plan(2, "a", 0.5, domain.QuotaWindow{Window: "1h", Tokens: 100})
	if e := s.Quota(newer); e != nil {
		t.Fatal(e)
	}
	var got []string
	for _, w := range s.Ended(old, 500, c.now()) {
		got = append(got, w.Scope+" "+w.Period)
	}
	if want := fmt.Sprintf("[provider:sub sub:1h:tokens:%[1]d key:a sub:1h:tokens:%[1]d]", t0.Unix()); fmt.Sprint(got) != want {
		t.Errorf("warnings %v, want %s", got, want)
	}
	if e := s.Quota(newer); e == nil || e.Code != domain.CodeQuotaExceeded {
		t.Errorf("after 500 tokens: %v", e)
	}
	if got := used(s); fmt.Sprint(got) != "[sub//1h/tokens=500/100 sub/a/1h/tokens=500/50]" {
		t.Errorf("windows %v", got)
	}
}

// TestQuotaSeededEqualsLive is the first point the Ring note of phase-3
// design section 7 hands to C28: a quota window places every time by its
// Unix time (its origin carries no monotonic reading), as the store's
// records are, so a window seeded from the committed events equals the live
// one, bucket for bucket, on the service clock's monotonic values; a test
// cannot step the wall clock under them, so it checks the origin too.
func TestQuotaSeededEqualsLive(t *testing.T) {
	base := time.Now() // carries a monotonic reading, as the service clock does
	var el atomic.Int64
	now := func() time.Time { return base.Add(time.Duration(el.Load())) }
	st := &store{}
	live := New(now, st.seed)
	r := rand.New(rand.NewPCG(3, 28))
	keys := []string{"a", "b"}
	ws := []domain.QuotaWindow{{Window: "1h", Requests: 1 << 40, Tokens: 1 << 40}, {Window: "3m", Requests: 1 << 40}}
	for i := range 2000 {
		k := keys[r.IntN(2)]
		p := plan(1, k, 0.5, ws...)
		if e := live.Take(p); e != nil {
			t.Fatal(e)
		}
		sent := now()
		el.Add(r.Int64N(int64(20 * time.Second)))
		n := r.Int64N(50)
		live.Ended(p, n, now())
		st.commit(Event{Key: k, Sent: sent, Ended: now(), Tokens: n})
		el.Add(r.Int64N(int64(20 * time.Second)))
		if i%20 != 19 {
			continue
		}
		fresh := New(now, st.seed)
		if err := fresh.Sync(1, map[string]domain.Provider{"sub": {Billing: domain.BillingSubscription, Quota: ws}},
			map[string]domain.Key{"a": {QuotaShare: map[string]float64{"sub": 0.5}}, "b": {QuotaShare: map[string]float64{"sub": 0.5}}}); err != nil {
			t.Fatal(err)
		}
		if a, b := used(live), used(fresh); fmt.Sprint(a) != fmt.Sprint(b) {
			t.Fatalf("after %d calls: live %v, seeded %v", i+1, a, b)
		}
	}
	for _, qs := range live.quotas {
		for _, q := range qs {
			if strings.Contains(q.x.origin.String(), "m=") {
				t.Errorf("a quota window's origin carries a monotonic reading: %v", q.x.origin)
			}
		}
	}
}

// TestQuotaSeedFutureRow is the second point of that note: a stored row
// dated after the seed's now (a wall clock stepped back since it was
// written) counts at now, so it cannot clear the older rows still in the
// window.
func TestQuotaSeedFutureRow(t *testing.T) {
	c := newClock()
	st := &store{}
	for _, at := range []time.Duration{-50 * time.Minute, -30 * time.Minute, -time.Minute, 2 * time.Hour} {
		st.commit(Event{Key: "a", Sent: t0.Add(at), Ended: t0.Add(at), Tokens: 10})
	}
	s := New(c.now, st.seed)
	p := plan(1, "a", 0, domain.QuotaWindow{Window: "1h", Requests: 100, Tokens: 1000})
	if e := s.Quota(p); e != nil {
		t.Fatal(e)
	}
	if got := used(s); fmt.Sprint(got) != "[sub//1h/requests=4/100 sub//1h/tokens=40/1000]" {
		t.Errorf("seeded with a row 2 h ahead: %v", got)
	}
	c.add(59 * time.Minute) // the row from 1 min before t0 still counts, and the one placed at t0
	if got := used(s); fmt.Sprint(got) != "[sub//1h/requests=2/100 sub//1h/tokens=20/1000]" {
		t.Errorf("59 min later: %v", got)
	}
}

// TestQuotaSync: Sync holds the windows of every subscription provider's
// quota and of every key's share as the configuration declares them,
// seeding the missing ones, and drops every other window; a provider of
// another billing, or a share of a provider without quota, holds none.
func TestQuotaSync(t *testing.T) {
	c := newClock()
	st := &store{}
	s := New(c.now, st.seed)
	providers := map[string]domain.Provider{
		"sub":  {Billing: domain.BillingSubscription, Quota: []domain.QuotaWindow{reqs("5h", 10), {Window: "7d", Tokens: 1000}}},
		"bare": {Billing: domain.BillingSubscription},
		"pay":  {Billing: domain.BillingPayPerUse},
	}
	keys := map[string]domain.Key{"a": {QuotaShare: map[string]float64{"sub": 0.4, "bare": 1}}, "b": {}}
	if err := s.Sync(1, providers, keys); err != nil {
		t.Fatal(err)
	}
	want := "[sub//5h/requests=0/10 sub//7d/tokens=0/1000 sub/a/5h/requests=0/4 sub/a/7d/tokens=0/400]"
	if got := used(s); fmt.Sprint(got) != want {
		t.Errorf("synced %v, want %s", got, want)
	}
	if v := s.Windows()[2]; v.Share != 0.4 || v.RetryAfter != 0 {
		t.Errorf("view %+v", v)
	}
	if len(st.seeds) != 2 || st.seeds[0] != fmt.Sprintf("sub@%d", t0.Add(-7*24*time.Hour-1008*time.Second).Unix()) {
		t.Errorf("seeds %v, want one per holder from the longest window and its bucket back", st.seeds)
	}
	delete(keys, "a")
	if err := s.Sync(0, providers, keys); err != nil { // an older revision drops nothing
		t.Fatal(err)
	}
	if got := used(s); fmt.Sprint(got) != want {
		t.Errorf("an older Sync changed %v", got)
	}
	if err := s.Sync(2, providers, keys); err != nil {
		t.Fatal(err)
	}
	if got := used(s); fmt.Sprint(got) != "[sub//5h/requests=0/10 sub//7d/tokens=0/1000]" {
		t.Errorf("after the share is gone: %v", got)
	}
	st.fail = errors.New("disk I/O error")
	providers["new"] = domain.Provider{Billing: domain.BillingSubscription, Quota: []domain.QuotaWindow{reqs("1h", 1)}}
	if err := s.Sync(3, providers, keys); err == nil || err.Error() != "disk I/O error" {
		t.Errorf("a failing seed: %v", err)
	}
}

// TestQuotaSpan: a window text validation refuses, which a stored
// configuration may still hold, counts over the longest span, and a window
// longer than the time since 1970 counts every record.
func TestQuotaSpan(t *testing.T) {
	c := newClock()
	st := &store{}
	st.commit(Event{Key: "a", Sent: time.Unix(1, 0), Ended: time.Unix(1, 0)})
	s := New(c.now, st.seed)
	for _, w := range []string{"5 hours", "999999d"} {
		p := Plan{Provider: w, Windows: []domain.QuotaWindow{reqs(w, 2)}, Revision: 1}
		if e := s.Take(p); e != nil {
			t.Fatalf("%s: %v", w, e)
		}
		if e := s.Take(p); e == nil || e.Code != domain.CodeQuotaExceeded {
			t.Errorf("%s: the record of 1970 and one Take make 2: %v", w, e)
		}
	}
}

// TestQuotaClockBack: a quota window places times by the wall clock, so a
// wall clock stepped back by more than the window, after a look moved the
// ring ahead, would give times older than the ring; they count in the
// newest bucket instead, and the window still refuses at its limit.
func TestQuotaClockBack(t *testing.T) {
	c := newClock()
	s := New(c.now, (&store{}).seed)
	p := plan(1, "a", 0, domain.QuotaWindow{Window: "1h", Requests: 3, Tokens: 100})
	c.add(5 * time.Hour)
	if e := s.Take(p); e != nil {
		t.Fatal(e)
	}
	c.set(t0)
	if e := s.Take(p); e != nil {
		t.Fatal(e)
	}
	s.Ended(p, 100, c.now())
	if e := s.Take(p); e == nil || e.Code != domain.CodeQuotaExceeded {
		t.Errorf("after the clock went back 5 h: %v; windows %v", e, used(s))
	}
	if got := used(s); fmt.Sprint(got) != "[sub//1h/requests=2/3 sub//1h/tokens=100/100]" {
		t.Errorf("windows %v", got)
	}
}
