package usage

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
)

// Query is what usage show asks (USE-9).
type Query struct {
	Since              time.Time // the window is [Since, now)
	Key, Model, Target string    // filters; "" for any
	By                 []string  // of key, model, target and effort-sent
	ComparePrevious    bool      // also the window of equal length before it
}

// Group is one row of usage show: the sums over the records of one billing
// mode and one value of each grouped column, the others empty. The counts
// and percentiles that usage_daily does not hold are nil in a daily window,
// and the reasoning tokens and the cost are nil where no record reported
// them: absent, never zero (USE-9).
type Group struct {
	Billing      string   `json:"billing"`
	Key          string   `json:"key,omitempty"`
	Model        string   `json:"model,omitempty"`
	Target       string   `json:"target,omitempty"`
	EffortSent   string   `json:"effort_sent,omitempty"` // "-" for records that sent no effort
	Requests     int64    `json:"requests"`              // provider calls: a request that fell back counts once per attempt
	Errors       int64    `json:"errors"`                // records with an error_type
	Estimated    *int64   `json:"estimated,omitempty"`   // records whose tokens are estimated (USE-8)
	Unpriced     *int64   `json:"unpriced,omitempty"`    // pay_per_use records without a cost (USE-12)
	Length       *int64   `json:"length,omitempty"`      // records whose finish reason Length accepts
	Empty        *int64   `json:"empty,omitempty"`       // 2xx records with no text and no tool call
	Input        int64    `json:"input_tokens"`
	CacheRead    int64    `json:"cache_read_tokens"`
	CacheWrite   int64    `json:"cache_write_tokens"`
	Output       int64    `json:"output_tokens"`
	Reasoning    *int64   `json:"reasoning_tokens,omitempty"`
	CostUSD      *float64 `json:"cost_usd,omitempty"` // pay_per_use groups with a known cost only (USE-7)
	OutputP50    *int64   `json:"output_tokens_p50,omitempty"`
	OutputP90    *int64   `json:"output_tokens_p90,omitempty"`
	ReasoningP50 *int64   `json:"reasoning_tokens_p50,omitempty"`
	ReasoningP90 *int64   `json:"reasoning_tokens_p90,omitempty"`
}

// Report is the answer of usage show for one window: Source says where it
// was read from, records or daily, and [Since, Until) is the span read,
// which a daily window widens to whole UTC days. Previous is the window
// before it, with its own source and span, when the query asks for it.
type Report struct {
	Source   string    `json:"source"`
	Since    time.Time `json:"since"`
	Until    time.Time `json:"until"`
	Groups   []Group   `json:"groups"`
	Previous *Report   `json:"previous,omitempty"`
}

// byColumn is the column of each grouping usage show takes.
var byColumn = map[string]string{"key": "key_name", "model": "model", "target": "target", "effort-sent": "effort_sent"}

// recordsText is usage show over the records: the sums per group, and the
// nearest-rank p50 and p90 of output and reasoning tokens over the 2xx
// records whose tokens the provider reported (ok), each ranked within its
// group by ROW_NUMBER; a NULL reasoning count sorts after the reported ones
// and is not counted. %[1]s is the group, %[2]s the filter. The length
// reasons are those of Length.
const recordsText = `SELECT %[1]s, COUNT(*), COUNT(error_type), SUM(tokens_source = 'estimated'),
	SUM(billing = 'pay_per_use' AND cost_usd IS NULL), SUM(IFNULL(finish_reason, '') IN ('length', 'max_tokens', 'model_context_window_exceeded')),
	SUM(content_empty), SUM(input_tokens), SUM(cache_read_tokens), SUM(cache_write_tokens), SUM(output_tokens),
	SUM(reasoning_tokens), SUM(cost_usd),
	MAX(CASE WHEN ok AND o = (n + 1) / 2 THEN output_tokens END), MAX(CASE WHEN ok AND o = (9 * n + 9) / 10 THEN output_tokens END),
	MAX(CASE WHEN ok AND r = (nr + 1) / 2 THEN reasoning_tokens END), MAX(CASE WHEN ok AND r = (9 * nr + 9) / 10 THEN reasoning_tokens END)
FROM (SELECT *, ROW_NUMBER() OVER (PARTITION BY %[1]s, ok ORDER BY output_tokens) AS o, COUNT(*) OVER (PARTITION BY %[1]s, ok) AS n,
	ROW_NUMBER() OVER (PARTITION BY %[1]s, ok ORDER BY reasoning_tokens IS NULL, reasoning_tokens) AS r,
	COUNT(reasoning_tokens) OVER (PARTITION BY %[1]s, ok) AS nr
	FROM (SELECT billing, key_name, model, target, IFNULL(effort_sent, '-') AS effort_sent, error_type, tokens_source, cost_usd,
		finish_reason, content_empty, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens,
		status_code BETWEEN 200 AND 299 AND tokens_source = 'provider' AS ok
		FROM usage WHERE %[2]s))
GROUP BY %[1]s ORDER BY %[1]s`

// dailyText is usage show over usage_daily: the sums it holds per group.
const dailyText = `SELECT %[1]s, SUM(requests), SUM(errors), SUM(input_tokens), SUM(cache_read_tokens), SUM(cache_write_tokens),
	SUM(output_tokens), SUM(reasoning_tokens), SUM(cost_usd)
FROM usage_daily WHERE %[2]s GROUP BY %[1]s ORDER BY %[1]s`

// Show answers q from db, the store's read pool, for the window [q.Since,
// now) and, with q.ComparePrevious, the window of equal length before it:
// one group per billing mode and per value of the q.By columns, ordered by
// them. A window that starts within usageDays of now is read from the
// records; one that starts earlier from the usage_daily sums, over whole
// UTC days: from 00:00 of the day it starts, through today for the window
// that ends now, else to 00:00 of the day it ends. An unknown or repeated
// q.By entry, or effort-sent with a window read from usage_daily, is a 400
// invalid_request.
func Show(ctx context.Context, db *sql.DB, q Query, now time.Time, usageDays int64) (Report, error) {
	cols := []string{"billing"}
	for _, b := range q.By {
		c, ok := byColumn[b]
		if !ok || slices.Contains(cols, c) {
			return Report{}, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("by %.64q is unknown or repeated; it takes key, model, target and effort-sent", b))
		}
		cols = append(cols, c)
	}
	retained := now.UTC().AddDate(0, 0, -int(usageDays))
	previous := q.Since.Add(-now.Sub(q.Since))
	if slices.Contains(cols, "effort_sent") && (q.Since.Before(retained) || q.ComparePrevious && previous.Before(retained)) {
		return Report{}, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf(
			"by effort-sent reads the records within usage_days (%d days), and a window asked starts before them: shorten since or group without effort-sent", usageDays))
	}
	rep, err := window(ctx, db, q, cols, q.Since, now, now, retained)
	if err != nil || !q.ComparePrevious {
		return rep, err
	}
	prev, err := window(ctx, db, q, cols, previous, q.Since, now, retained)
	rep.Previous = &prev
	return rep, err
}

// window answers q over [since, until) from the records when since is not
// before retained, else from usage_daily over whole UTC days.
func window(ctx context.Context, db *sql.DB, q Query, cols []string, since, until, now, retained time.Time) (Report, error) {
	rep := Report{Source: "records", Since: since.UTC(), Until: until.UTC(), Groups: []Group{}}
	text, where, args := recordsText, "completed_at >= ? AND completed_at < ?", []any{since.UnixMilli(), until.UnixMilli()}
	if since.Before(retained) {
		day := func(t time.Time) time.Time {
			y, m, d := t.UTC().Date()
			return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		}
		rep.Source, rep.Since, text = "daily", day(since), dailyText
		where, args = "day >= ? AND day < ?", []any{rep.Since.Format(time.DateOnly), day(until).Format(time.DateOnly)}
		if until.Equal(now) {
			where = "day >= ? AND day <= ?"
		} else {
			rep.Until = day(until)
		}
	}
	for _, f := range []struct{ column, value string }{{"key_name", q.Key}, {"model", q.Model}, {"target", q.Target}} {
		if f.value != "" {
			where += " AND " + f.column + " = ?"
			args = append(args, f.value)
		}
	}
	group := strings.Join(cols, ", ")
	rows, err := db.QueryContext(ctx, fmt.Sprintf(text, group, where), args...)
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = rows.Close() }()
	records := rep.Source == "records"
	for rows.Next() {
		var g Group
		var cost sql.NullFloat64
		dest := []any{&g.Billing}
		for _, c := range cols[1:] {
			dest = append(dest, map[string]any{"key_name": &g.Key, "model": &g.Model, "target": &g.Target, "effort_sent": &g.EffortSent}[c])
		}
		dest = append(dest, &g.Requests, &g.Errors)
		if records {
			g.Estimated, g.Unpriced, g.Length, g.Empty = new(int64), new(int64), new(int64), new(int64)
			dest = append(dest, g.Estimated, g.Unpriced, g.Length, g.Empty)
		}
		dest = append(dest, &g.Input, &g.CacheRead, &g.CacheWrite, &g.Output, &g.Reasoning, &cost)
		if records {
			dest = append(dest, &g.OutputP50, &g.OutputP90, &g.ReasoningP50, &g.ReasoningP90)
		}
		if err := rows.Scan(dest...); err != nil {
			return Report{}, err
		}
		if cost.Valid && g.Billing == domain.BillingPayPerUse {
			g.CostUSD = &cost.Float64
		}
		rep.Groups = append(rep.Groups, g)
	}
	return rep, rows.Err()
}
