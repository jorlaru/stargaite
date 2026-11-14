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
	By                 []string  // of key, model and target
}

// Group is one row of usage show: the sums over the records of one billing
// mode and one value of each grouped column, the others empty.
type Group struct {
	Billing    string   `json:"billing"`
	Key        string   `json:"key,omitempty"`
	Model      string   `json:"model,omitempty"`
	Target     string   `json:"target,omitempty"`
	Requests   int64    `json:"requests"`  // provider calls: a request that fell back counts once per attempt
	Errors     int64    `json:"errors"`    // records with an error_type
	Estimated  int64    `json:"estimated"` // records whose tokens are estimated (USE-8)
	Unpriced   int64    `json:"unpriced"`  // pay_per_use records without a cost (USE-12)
	Input      int64    `json:"input_tokens"`
	CacheRead  int64    `json:"cache_read_tokens"`
	CacheWrite int64    `json:"cache_write_tokens"`
	Output     int64    `json:"output_tokens"`
	Reasoning  int64    `json:"reasoning_tokens"`
	CostUSD    *float64 `json:"cost_usd,omitempty"` // pay_per_use groups with a priced record only (USE-7)
}

// Report is the answer of usage show; Source says where it was read from.
type Report struct {
	Source string  `json:"source"`
	Groups []Group `json:"groups"`
}

// byColumn is the column of each grouping usage show takes.
var byColumn = map[string]string{"key": "key_name", "model": "model", "target": "target"}

// Show answers q from the records in db, the store's read pool, completed
// in [q.Since, now): one group per billing mode and per value of the q.By
// columns, ordered by them. An unknown or repeated q.By entry is a 400
// invalid_request.
func Show(ctx context.Context, db *sql.DB, q Query, now time.Time) (Report, error) {
	cols := []string{"billing"}
	for _, b := range q.By {
		c, ok := byColumn[b]
		if !ok || slices.Contains(cols, c) {
			return Report{}, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("by %.64q is unknown or repeated; it takes key, model and target", b))
		}
		cols = append(cols, c)
	}
	group := strings.Join(cols, ", ")
	text := "SELECT " + group + `, COUNT(*), COUNT(error_type), SUM(tokens_source = 'estimated'),
		SUM(billing = 'pay_per_use' AND cost_usd IS NULL), SUM(input_tokens), SUM(cache_read_tokens), SUM(cache_write_tokens),
		SUM(output_tokens), SUM(COALESCE(reasoning_tokens, 0)), SUM(cost_usd)
		FROM usage WHERE completed_at >= ? AND completed_at < ?`
	args := []any{q.Since.UnixMilli(), now.UnixMilli()}
	for _, f := range []struct{ column, value string }{{"key_name", q.Key}, {"model", q.Model}, {"target", q.Target}} {
		if f.value != "" {
			text += " AND " + f.column + " = ?"
			args = append(args, f.value)
		}
	}
	rows, err := db.QueryContext(ctx, text+" GROUP BY "+group+" ORDER BY "+group, args...)
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = rows.Close() }()
	rep := Report{Source: "records", Groups: []Group{}}
	for rows.Next() {
		var g Group
		var cost sql.NullFloat64
		dest := []any{&g.Billing}
		for _, c := range cols[1:] {
			dest = append(dest, map[string]any{"key_name": &g.Key, "model": &g.Model, "target": &g.Target}[c])
		}
		dest = append(dest, &g.Requests, &g.Errors, &g.Estimated, &g.Unpriced, &g.Input, &g.CacheRead, &g.CacheWrite, &g.Output, &g.Reasoning, &cost)
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
