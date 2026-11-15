package usage

import (
	"context"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/store"
)

// TestCutoffsAt: the defaults, declared zeros and negatives as the defaults,
// a window longer than usage_days, a window that does not parse, the UTC
// day of the usage cutoff, months back clamped to the target month's last
// day, the current month in another zone, and values too large for a
// duration.
func TestCutoffsAt(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	sub := func(windows ...string) map[string]domain.Provider {
		p := domain.Provider{Billing: domain.BillingSubscription}
		for _, w := range windows {
			p.Quota = append(p.Quota, domain.QuotaWindow{Window: w, Requests: 1})
		}
		return map[string]domain.Provider{"p": p, "other": {Billing: domain.BillingPayPerUse}}
	}
	now := at("2026-11-15T12:00:00Z")
	defaults := Cutoffs{Usage: at("2026-08-17T12:00:00Z"), Day: "2023-11-15", Audit: at("2024-11-15T12:00:00Z")}
	for _, tc := range []struct {
		name      string
		now       time.Time
		r         domain.Retention
		providers map[string]domain.Provider
		want      Cutoffs
	}{
		{"defaults", now, domain.Retention{}, nil, defaults},
		{"declared defaults", now, domain.Retention{UsageDays: 90, UsageDailyMonths: 36, AuditMonths: 24}, nil, defaults},
		{"negatives a stored configuration may hold", now, domain.Retention{UsageDays: -1, UsageDailyMonths: -1, AuditMonths: -1}, nil, defaults},
		{"windows within usage_days", now, domain.Retention{}, sub("5h", "7d"), defaults},
		{"a window longer than usage_days keeps its seed span", now, domain.Retention{UsageDays: 1, UsageDailyMonths: 1, AuditMonths: 1}, sub("5h", "7d"),
			Cutoffs{Usage: at("2026-11-08T11:43:12Z"), Day: "2026-10-15", Audit: at("2026-10-15T12:00:00Z")}},
		{"a window that does not parse keeps 100 years and one bucket", now, domain.Retention{UsageDays: 1}, sub("five hours"),
			Cutoffs{Usage: now.Add(-admission.SeedSpan(domain.MaxQuotaSpan)), Day: now.Add(-admission.SeedSpan(domain.MaxQuotaSpan)).Format(time.DateOnly), Audit: defaults.Audit}},
		{"daily keeps the UTC day of the usage cutoff", now, domain.Retention{UsageDays: 100, UsageDailyMonths: 1}, nil,
			Cutoffs{Usage: at("2026-08-07T12:00:00Z"), Day: "2026-08-07", Audit: defaults.Audit}},
		{"one month before 31 March is 28 February", at("2026-03-31T10:00:00Z"), domain.Retention{UsageDays: 1, UsageDailyMonths: 1, AuditMonths: 1}, nil,
			Cutoffs{Usage: at("2026-03-30T10:00:00Z"), Day: "2026-02-28", Audit: at("2026-02-28T10:00:00Z")}},
		{"three months before 31 May is 28 February", at("2026-05-31T10:00:00Z"), domain.Retention{UsageDays: 1, UsageDailyMonths: 3, AuditMonths: 3}, nil,
			Cutoffs{Usage: at("2026-05-30T10:00:00Z"), Day: "2026-02-28", Audit: at("2026-02-28T10:00:00Z")}},
		{"the defaults on 29 February", at("2028-02-29T10:00:00Z"), domain.Retention{}, nil,
			Cutoffs{Usage: at("2027-12-01T10:00:00Z"), Day: "2025-02-28", Audit: at("2026-02-28T10:00:00Z")}},
		{"the UTC month and day of a time in another zone", at("2026-12-01T00:30:00+02:00"), domain.Retention{UsageDays: 1, UsageDailyMonths: 1, AuditMonths: 1}, nil,
			Cutoffs{Usage: at("2026-11-29T22:30:00Z"), Day: "2026-10-30", Audit: at("2026-10-30T22:30:00Z")}},
		{"values too large keep 100 years", now, domain.Retention{UsageDays: 1 << 62, UsageDailyMonths: 1 << 62, AuditMonths: 1 << 62}, nil,
			Cutoffs{Usage: now.Add(-100 * 365 * 24 * time.Hour), Day: "1926-11-15", Audit: at("1926-11-15T12:00:00Z")}},
	} {
		got := CutoffsAt(tc.now, tc.r, tc.providers)
		if !got.Usage.Equal(tc.want.Usage) || got.Day != tc.want.Day || !got.Audit.Equal(tc.want.Audit) {
			t.Errorf("%s: %v %s %v, want %v %s %v", tc.name, got.Usage, got.Day, got.Audit, tc.want.Usage, tc.want.Day, tc.want.Audit)
		}
	}
}

// TestRetentionPlateau is AC-49 (design section 8): 500 records a simulated
// day in 100-record batches over 120 days, usage_days 10, a 7d window,
// usage_daily_months 1, audit_months 1, one audit row a day, Retain once a
// day on the test clock. The usage rows never exceed 11 days of traffic, the
// file's page_count plateaus within 2 % and the -wal stays under 8 MiB; the
// seeded quota windows, the Restore budgets and budget_totals are equal
// before and after each run; no aggregate or audit row older than its cutoff
// is left, and the current month's aggregates and the audit rows within one
// month are all kept.
func TestRetentionPlateau(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx, db := context.Background(), st.Read()
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO budget_totals (scope_type, scope_name, cost_usd, since_revision) VALUES ('global', '', 0, 0)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	retention := domain.Retention{UsageDays: 10, UsageDailyMonths: 1, AuditMonths: 1}
	providers := map[string]domain.Provider{
		"p": {Billing: domain.BillingSubscription, Quota: []domain.QuotaWindow{{Window: "7d", Requests: 1 << 40, Tokens: 1 << 40}}},
		"q": {Billing: domain.BillingPayPerUse},
	}
	keys := map[string]domain.Key{"k": {QuotaShare: map[string]float64{"p": 0.5}}}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var now time.Time
	clock := func() time.Time { return now }
	seeded := func() string {
		a := admission.New(clock, Seeder(db, slog.New(slog.DiscardHandler)))
		if err := a.Sync(1, providers, keys); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(a.Windows())
	}
	budgets := func() string {
		a := admission.New(clock, nil)
		if err := Restore(ctx, db, a, now); err != nil {
			t.Fatal(err)
		}
		var spent []float64
		for _, scope := range []string{"global", "key:k", "model:fast"} {
			for _, interval := range []string{domain.ResetDaily, domain.ResetMonthly} {
				spent = append(spent, a.Spent(admission.Budget{Scope: scope, Spec: domain.Budget{LimitUSD: ptr(1e9), ResetInterval: interval}}))
			}
		}
		spent = append(spent, a.Spent(admission.Budget{Scope: "global", Spec: domain.Budget{LimitUSD: ptr(1e9), ResetInterval: domain.ResetNever}}))
		return fmt.Sprint(spent)
	}
	totals := func() string {
		var scope, name string
		var usd float64
		var since int64
		if err := db.QueryRow("SELECT scope_type, scope_name, cost_usd, since_revision FROM budget_totals").Scan(&scope, &name, &usd, &since); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(scope, name, usd, since)
	}
	const perDay, days = 500, 120
	var pages [days]int
	ids, id := rand.NewChaCha8([32]byte{}), make([]byte, 16)
	for d := range days {
		day := start.AddDate(0, 0, d)
		for b := range perDay / batchSize {
			batch := make([]Record, batchSize)
			for i := range batchSize {
				n := b*batchSize + i
				// Ids shaped as the data plane's, a random request id
				// (rand.Text) and the attempt, from a fixed seed so that
				// every run lays the pages out alike.
				_, _ = ids.Read(id)
				batch[i] = rec(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(id)+"-1", func(r *Record) {
					r.CompletedAt = day.Add(time.Duration(n) * 24 * time.Hour / perDay).UnixMilli()
					r.StartedAt = r.CompletedAt - 1000
					if n%2 == 0 {
						r.Target, r.Billing, r.CostUSD, r.CostSource = "p/m", domain.BillingSubscription, nil, "none"
					}
				})
			}
			if err := st.Write(ctx, func(tx *sql.Tx) error { return insert(tx, entries(batch)) }); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO audit (id, actor, action, resource_type, resource_name, result, created_at)
				VALUES (?, 'operator', 'test', 'settings', 'retention', 'applied', ?)`, fmt.Sprint("a", d), day.Add(12*time.Hour).UnixMilli())
			return err
		}); err != nil {
			t.Fatal(err)
		}

		now = day.Add(24*time.Hour - time.Minute)
		c := CutoffsAt(now, retention, providers)
		month := now.Format("2006-01") + "-01"
		windows, spent, total := seeded(), budgets(), totals()
		thisMonth := count(t, db, "SELECT COUNT(*) FROM usage_daily WHERE day >= ?", month)
		// One month back, or the previous month's last day when it is shorter.
		monthAgo := min(now.AddDate(0, -1, 0).UnixMilli(), time.Date(now.Year(), now.Month(), 0, now.Hour(), now.Minute(), now.Second(), now.Nanosecond(), time.UTC).UnixMilli())
		audits := count(t, db, "SELECT COUNT(*) FROM audit WHERE created_at >= ?", monthAgo)
		if _, err := Retain(ctx, st.Write, c); err != nil {
			t.Fatal(err)
		}
		if got := seeded(); got != windows {
			t.Fatalf("day %d: quota windows %s after Retain, %s before", d, got, windows)
		}
		if got := budgets(); got != spent {
			t.Fatalf("day %d: budgets %s after Retain, %s before", d, got, spent)
		}
		if got := totals(); got != total {
			t.Fatalf("day %d: budget_totals %s after Retain, %s before", d, got, total)
		}
		if n := count(t, db, "SELECT COUNT(*) FROM usage"); n > 11*perDay {
			t.Fatalf("day %d: %d usage rows, more than 11 days of traffic", d, n)
		}
		if n := count(t, db, "SELECT COUNT(*) FROM usage_daily WHERE day < ?", c.Day); n != 0 {
			t.Fatalf("day %d: %d aggregates before %s", d, n, c.Day)
		}
		if n := count(t, db, "SELECT COUNT(*) FROM usage_daily WHERE day >= ?", month); n == 0 || n != thisMonth {
			t.Fatalf("day %d: %d aggregates of the current month, %d before Retain", d, n, thisMonth)
		}
		if n := count(t, db, "SELECT COUNT(*) FROM audit WHERE created_at < ?", monthAgo); n != 0 {
			t.Fatalf("day %d: %d audit rows older than 1 month", d, n)
		}
		if n := count(t, db, "SELECT COUNT(*) FROM audit WHERE created_at >= ?", monthAgo); n != audits {
			t.Fatalf("day %d: %d audit rows within 1 month, %d before Retain", d, n, audits)
		}
		pages[d] = count(t, db, "PRAGMA page_count")
		fi, err := os.Stat(filepath.Join(dir, "stargaite.db-wal"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() >= 8<<20 {
			t.Fatalf("day %d: -wal of %d bytes", d, fi.Size())
		}
	}
	early, late := maxOf(pages[40:80]), maxOf(pages[80:])
	if float64(late) > 1.02*float64(early) {
		t.Errorf("page_count %d over days 80 to 120, more than 2 %% above %d over days 40 to 80", late, early)
	}
	t.Logf("page_count: %d over days 40 to 80, %d over days 80 to 120", early, late)
}

func maxOf(s []int) int {
	m := 0
	for _, v := range s {
		m = max(m, v)
	}
	return m
}

// TestRetainWithWriter: Retain runs alongside the writer under -race, each
// batch one job on the store's queue: every record before the cutoff is
// deleted once, every later one kept, and the aggregates left equal the
// records left.
func TestRetainWithWriter(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	w := NewWriter(st.Write, slog.New(slog.DiscardHandler))
	cutoff := time.UnixMilli(1_794_614_400_000) // 2026-11-14, UTC
	c := Cutoffs{Usage: cutoff, Day: cutoff.UTC().Format(time.DateOnly), Audit: cutoff}
	const n = 2000
	var deleted Deleted
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range n {
			w.Add(rec(fmt.Sprint(i), func(r *Record) {
				r.CompletedAt = cutoff.UnixMilli() + int64(i)
				if i%2 == 0 {
					r.CompletedAt = cutoff.UnixMilli() - 3_600_000 - int64(i)
				}
			}))
		}
	})
	wg.Go(func() {
		for range 50 {
			d, err := Retain(ctx, st.Write, c)
			if err != nil {
				t.Error(err)
				return
			}
			deleted.Usage += d.Usage
		}
	})
	wg.Wait()
	if lost := w.Close(ctx); lost != 0 {
		t.Fatalf("%d records lost", lost)
	}
	d, err := Retain(ctx, st.Write, c)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Usage+d.Usage != n/2 {
		t.Errorf("%d records deleted, want %d", deleted.Usage+d.Usage, n/2)
	}
	if got := count(t, st.Read(), "SELECT COUNT(*) FROM usage WHERE completed_at >= ?", cutoff.UnixMilli()); got != n/2 {
		t.Errorf("%d records kept, want %d", got, n/2)
	}
	if aggregates, fromRecords := daily(t, st.Read()); aggregates != fromRecords || aggregates == "" {
		t.Errorf("usage_daily\n%s\nfrom the records\n%s", aggregates, fromRecords)
	}
	if again, err := Retain(ctx, st.Write, c); err != nil || again != (Deleted{}) {
		t.Errorf("a second run: %+v, %v", again, err)
	}
}

// TestRetainBatches: Retain deletes in batches of 500, each one write job,
// stops at the first short batch, leaves budget_totals, revisions and
// configuration rows alone, and returns between batches once ctx ends.
func TestRetainBatches(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO budget_totals (scope_type, scope_name, cost_usd, since_revision) VALUES ('global', '', 1, 0)`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO settings (name, value, updated_at) VALUES ('max_body_bytes', '1', 0)`); err != nil {
			return err
		}
		batch := make([]Record, 1001)
		for i := range batch {
			batch[i] = rec(fmt.Sprint(i), func(r *Record) { r.CompletedAt = int64(i) })
		}
		if err := insert(tx, entries(batch)); err != nil {
			return err
		}
		for i := range 3 {
			if _, err := tx.Exec(`INSERT INTO audit (id, actor, action, resource_type, resource_name, result, created_at)
				VALUES (?, 'operator', 'test', 'settings', 'max_body_bytes', 'applied', 0)`, fmt.Sprint("a", i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := fmt.Sprint(count(t, st.Read(), "SELECT COUNT(*) FROM settings"), count(t, st.Read(), "SELECT COUNT(*) FROM revisions"),
		count(t, st.Read(), "SELECT COUNT(*) FROM budget_totals WHERE cost_usd = 1"))
	c := Cutoffs{Usage: time.UnixMilli(1), Day: "9999-12-31", Audit: time.UnixMilli(1)}

	var jobs int
	counting := func(ctx context.Context, fn func(*sql.Tx) error) error { jobs++; return st.Write(ctx, fn) }
	// Records 0 and 1 are not before the cutoff for one: only record 0 is.
	d, err := Retain(ctx, counting, c)
	if err != nil || d != (Deleted{Usage: 1, Daily: 1, Audit: 3}) || jobs != 3 {
		t.Fatalf("Retain: %+v, %v, %d jobs", d, err, jobs)
	}
	jobs = 0
	d, err = Retain(ctx, counting, Cutoffs{Usage: time.UnixMilli(2000), Day: "0000-00-00", Audit: time.UnixMilli(0)})
	if err != nil || d != (Deleted{Usage: 1000}) || jobs != 3+1+1 {
		t.Errorf("Retain: %+v, %v, %d jobs", d, err, jobs)
	}
	if after := fmt.Sprint(count(t, st.Read(), "SELECT COUNT(*) FROM settings"), count(t, st.Read(), "SELECT COUNT(*) FROM revisions"),
		count(t, st.Read(), "SELECT COUNT(*) FROM budget_totals WHERE cost_usd = 1")); after != before {
		t.Errorf("settings, revisions and budget_totals: %s, want %s", after, before)
	}

	// A context that ends stops Retain before its next batch.
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		batch := make([]Record, 600)
		for i := range batch {
			batch[i] = rec(fmt.Sprint("late-", i), func(r *Record) { r.CompletedAt = int64(i) })
		}
		return insert(tx, entries(batch))
	}); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	stopping := func(_ context.Context, fn func(*sql.Tx) error) error { defer cancel(); return st.Write(ctx, fn) }
	if d, err := Retain(cancelled, stopping, Cutoffs{Usage: time.UnixMilli(2000), Day: "0000-00-00", Audit: time.UnixMilli(0)}); !errors.Is(err, context.Canceled) || d != (Deleted{Usage: 500}) {
		t.Errorf("a cancelled Retain: %+v, %v", d, err)
	}
}
