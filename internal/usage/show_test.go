package usage

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
)

// TestShow is usage show's sums (USE-9): per billing mode and per grouped
// column, over [since, now), filtered; a cost only for pay_per_use groups;
// errors, estimated and unpriced counted; an unknown or repeated by refused.
func TestShow(t *testing.T) {
	st := openStore(t)
	at := time.UnixMilli(1_794_614_401_000) // the completed_at of rec
	batch := []Record{
		rec("1"),
		rec("2", func(r *Record) {
			r.ErrorType, r.StatusCode, r.InputTokens, r.CacheReadTokens, r.OutputTokens, r.CostUSD = "upstream_error", 502, 0, 0, 0, ptr(0.0)
		}),
		rec("3", func(r *Record) { r.TokensSource, r.CostUSD, r.CostSource, r.Model = "estimated", nil, "none", "deep" }),
		rec("4", func(r *Record) {
			r.Billing, r.CostUSD, r.CostSource, r.KeyName = domain.BillingSubscription, nil, "none", "other"
		}),
		rec("old", func(r *Record) { r.CompletedAt -= 3_600_001 }), // before the window of 1 h
		rec("now", func(r *Record) { r.CompletedAt += 3_600_000 }), // at now: outside [since, now)
		rec("t2", func(r *Record) { r.Target, r.CostUSD = "q/m", ptr(1.0) }),
	}
	if err := st.Write(context.Background(), func(tx *sql.Tx) error { return insert(tx, batch) }); err != nil {
		t.Fatal(err)
	}
	now := at.Add(time.Hour)
	since := now.Add(-time.Hour - time.Millisecond)
	pay := func(g Group) Group { g.Billing = domain.BillingPayPerUse; return g }
	for _, tc := range []struct {
		name string
		q    Query
		want []Group
	}{
		{"by billing", Query{Since: since}, []Group{
			pay(Group{Requests: 4, Errors: 1, Estimated: 1, Unpriced: 1, Input: 30, CacheRead: 6, Output: 15, Reasoning: 4, CostUSD: ptr(1.25)}),
			{Billing: domain.BillingSubscription, Requests: 1, Input: 10, CacheRead: 2, Output: 5, Reasoning: 1},
		}},
		{"by key and model", Query{Since: since, By: []string{"model", "key"}}, []Group{
			pay(Group{Key: "k", Model: "deep", Requests: 1, Estimated: 1, Unpriced: 1, Input: 10, CacheRead: 2, Output: 5, Reasoning: 1}),
			pay(Group{Key: "k", Model: "fast", Requests: 3, Errors: 1, Input: 20, CacheRead: 4, Output: 10, Reasoning: 3, CostUSD: ptr(1.25)}),
			{Billing: domain.BillingSubscription, Key: "other", Model: "fast", Requests: 1, Input: 10, CacheRead: 2, Output: 5, Reasoning: 1},
		}},
		{"filtered by target", Query{Since: since, Target: "q/m", By: []string{"target"}}, []Group{
			pay(Group{Target: "q/m", Requests: 1, Input: 10, CacheRead: 2, Output: 5, Reasoning: 1, CostUSD: ptr(1.0)}),
		}},
		{"filtered by key and model", Query{Since: since, Key: "k", Model: "deep"}, []Group{
			pay(Group{Requests: 1, Estimated: 1, Unpriced: 1, Input: 10, CacheRead: 2, Output: 5, Reasoning: 1}),
		}},
		{"a window with nothing", Query{Since: now.Add(-time.Millisecond)}, []Group{}},
		{"a longer window", Query{Since: since.Add(-time.Hour), Key: "k", Target: "p/m", Model: "fast"}, []Group{
			pay(Group{Requests: 3, Errors: 1, Input: 20, CacheRead: 4, Output: 10, Reasoning: 3, CostUSD: ptr(0.5)}),
		}},
	} {
		rep, err := Show(context.Background(), st.Read(), tc.q, now)
		if err != nil || rep.Source != "records" || !reflect.DeepEqual(rep.Groups, tc.want) {
			t.Errorf("%s: %+v, error %v\nwant %+v", tc.name, rep, err, tc.want)
		}
	}
	for _, by := range [][]string{{"effort-sent"}, {"key", "key"}, {"key_name"}, {"model; DROP TABLE usage"}} {
		var de *domain.Error
		if _, err := Show(context.Background(), st.Read(), Query{Since: since, By: by}, now); !errors.As(err, &de) || de.Status != http.StatusBadRequest {
			t.Errorf("by %q: %v", by, err)
		}
	}
}
