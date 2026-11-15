package usage

import (
	"context"
	"database/sql"
	"time"

	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/domain"
)

// retainBatch is the most rows one retention job deletes (USE-11).
const retainBatch = 500

// The longest retention a setting can ask for: a larger value keeps 100
// years, as domain.MaxQuotaSpan does for a window, so no value overflows a
// time.Duration or a date into a cutoff that would delete what it must keep.
const (
	maxRetainDays   = 100 * 365
	maxRetainMonths = 100 * 12
)

// Cutoffs are the retention bounds at one instant: rows before them go.
type Cutoffs struct {
	Usage time.Time // usage.completed_at: now - max(usage_days, the longest SeedSpan of any declared window)
	Day   string    // usage_daily.day: the earliest of now - usage_daily_months, the UTC day of Usage and the current month's first
	Audit time.Time // audit.created_at: now - audit_months
}

// CutoffsAt are the cutoffs of r at now (USE-11, design section 8). A value
// of 0 is the default, as is a negative one, which validation refuses but a
// stored configuration may hold. Every provider's declared quota windows
// count, whatever its billing, and a window whose text does not parse counts
// over domain.MaxQuotaSpan, as the seed reads it. Day never falls inside the
// current UTC month, which budget Restore reads.
func CutoffsAt(now time.Time, r domain.Retention, providers map[string]domain.Provider) Cutoffs {
	keep := func(v, def, most int64) int {
		if v <= 0 {
			v = def
		}
		return int(min(v, most))
	}
	span := time.Duration(keep(r.UsageDays, domain.DefaultUsageDays, maxRetainDays)) * 24 * time.Hour
	for _, p := range providers {
		for _, q := range p.Quota {
			w, ok := domain.QuotaSpan(q.Window)
			if !ok {
				w = domain.MaxQuotaSpan
			}
			span = max(span, admission.SeedSpan(w))
		}
	}
	now = now.UTC()
	// m months back, clamped to the target month's last day where AddDate
	// would carry into the next month (31 March less one month is 28
	// February, not 3 March).
	back := func(m int) time.Time {
		t := now.AddDate(0, -m, 0)
		if t.Day() != now.Day() {
			t = t.AddDate(0, 0, -t.Day())
		}
		return t
	}
	c := Cutoffs{Usage: now.Add(-span), Audit: back(keep(r.AuditMonths, domain.DefaultAuditMonths, maxRetainMonths))}
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	c.Day = min(back(keep(r.UsageDailyMonths, domain.DefaultUsageDailyMonths, maxRetainMonths)).Format(time.DateOnly),
		c.Usage.Format(time.DateOnly), month.Format(time.DateOnly))
	return c
}

// Deleted counts the rows one Retain deleted, per table.
type Deleted struct{ Usage, Daily, Audit int64 }

// Retain deletes, per table, batches of at most 500 rows before c, each one
// write job, until a batch is short. budget_totals, revisions and
// configuration rows are never touched. Each batch is a rowid IN (SELECT ...
// LIMIT) delete, since DELETE ... LIMIT is not compiled in: usage by
// completed_at and usage_daily by day, each along its index, and audit in
// rowid order, which is insertion order. ctx is checked between batches; a
// batch once queued runs to its end. Deleted holds what was deleted until an
// error.
func Retain(ctx context.Context, write func(context.Context, func(*sql.Tx) error) error, c Cutoffs) (Deleted, error) {
	var d Deleted
	for _, t := range []struct {
		n     *int64
		query string
		arg   any
	}{
		{&d.Usage, `DELETE FROM usage WHERE rowid IN (SELECT rowid FROM usage WHERE completed_at < ? ORDER BY completed_at LIMIT ?)`, c.Usage.UnixMilli()},
		{&d.Daily, `DELETE FROM usage_daily WHERE rowid IN (SELECT rowid FROM usage_daily WHERE day < ? ORDER BY day LIMIT ?)`, c.Day},
		{&d.Audit, `DELETE FROM audit WHERE rowid IN (SELECT rowid FROM audit WHERE created_at < ? ORDER BY rowid LIMIT ?)`, c.Audit.UnixMilli()},
	} {
		for n := int64(retainBatch); n == retainBatch; *t.n += n {
			if err := ctx.Err(); err != nil {
				return d, err
			}
			if err := write(ctx, func(tx *sql.Tx) error {
				res, err := tx.Exec(t.query, t.arg, retainBatch)
				if err != nil {
					return err
				}
				n, err = res.RowsAffected()
				return err
			}); err != nil {
				return d, err
			}
		}
	}
	return d, nil
}
