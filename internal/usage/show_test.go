package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/store"
)

// counted gives g the counts a records window always holds, zero unless set.
func counted(g Group) Group {
	for _, p := range []**int64{&g.Estimated, &g.Unpriced, &g.Length, &g.Empty} {
		if *p == nil {
			*p = ptr(int64(0))
		}
	}
	return g
}

// ranked gives g its percentiles: output p50 and p90, then reasoning p50
// and p90; a negative value is absent.
func ranked(g Group, p ...int64) Group {
	for i, dst := range []**int64{&g.OutputP50, &g.OutputP90, &g.ReasoningP50, &g.ReasoningP90} {
		if p[i] >= 0 {
			*dst = ptr(p[i])
		}
	}
	return g
}

// seed stores batch as the writer does, usage_daily included.
func seed(t *testing.T, st *store.Store, batch []Record) {
	t.Helper()
	if err := st.Write(context.Background(), func(tx *sql.Tx) error { return insert(tx, entries(batch)) }); err != nil {
		t.Fatal(err)
	}
}

// TestShow is usage show's sums (USE-9): per billing mode and per grouped
// column, over [since, now), filtered; a cost only for pay_per_use groups;
// errors, estimated and unpriced counted; an unknown or repeated by refused.
func TestShow(t *testing.T) {
	st := openStore(t)
	at := time.UnixMilli(1_794_614_401_000) // the completed_at of rec
	seed(t, st, []Record{
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
	})
	now := at.Add(time.Hour)
	since := now.Add(-time.Hour - time.Millisecond)
	pay := func(g Group) Group { g.Billing = domain.BillingPayPerUse; return counted(g) }
	for _, tc := range []struct {
		name string
		q    Query
		want []Group
	}{
		{"by billing", Query{Since: since}, []Group{
			ranked(pay(Group{Requests: 4, Errors: 1, Estimated: ptr(int64(1)), Unpriced: ptr(int64(1)), Input: 30, CacheRead: 6, Output: 15, Reasoning: ptr(int64(4)), CostUSD: ptr(1.25)}), 5, 5, 1, 1),
			ranked(counted(Group{Billing: domain.BillingSubscription, Requests: 1, Input: 10, CacheRead: 2, Output: 5, Reasoning: ptr(int64(1))}), 5, 5, 1, 1),
		}},
		{"by key and model", Query{Since: since, By: []string{"model", "key"}}, []Group{
			pay(Group{Key: "k", Model: "deep", Requests: 1, Estimated: ptr(int64(1)), Unpriced: ptr(int64(1)), Input: 10, CacheRead: 2, Output: 5, Reasoning: ptr(int64(1))}),
			ranked(pay(Group{Key: "k", Model: "fast", Requests: 3, Errors: 1, Input: 20, CacheRead: 4, Output: 10, Reasoning: ptr(int64(3)), CostUSD: ptr(1.25)}), 5, 5, 1, 1),
			ranked(counted(Group{Billing: domain.BillingSubscription, Key: "other", Model: "fast", Requests: 1, Input: 10, CacheRead: 2, Output: 5, Reasoning: ptr(int64(1))}), 5, 5, 1, 1),
		}},
		{"filtered by target", Query{Since: since, Target: "q/m", By: []string{"target"}}, []Group{
			ranked(pay(Group{Target: "q/m", Requests: 1, Input: 10, CacheRead: 2, Output: 5, Reasoning: ptr(int64(1)), CostUSD: ptr(1.0)}), 5, 5, 1, 1),
		}},
		{"filtered by key and model", Query{Since: since, Key: "k", Model: "deep"}, []Group{
			pay(Group{Requests: 1, Estimated: ptr(int64(1)), Unpriced: ptr(int64(1)), Input: 10, CacheRead: 2, Output: 5, Reasoning: ptr(int64(1))}),
		}},
		{"a window with nothing", Query{Since: now.Add(-time.Millisecond)}, []Group{}},
		{"a longer window", Query{Since: since.Add(-time.Hour), Key: "k", Target: "p/m", Model: "fast"}, []Group{
			ranked(pay(Group{Requests: 3, Errors: 1, Input: 20, CacheRead: 4, Output: 10, Reasoning: ptr(int64(3)), CostUSD: ptr(0.5)}), 5, 5, 1, 1),
		}},
	} {
		rep, err := Show(context.Background(), st.Read(), tc.q, now, 90)
		if err != nil || rep.Source != "records" || !rep.Since.Equal(tc.q.Since) || !rep.Until.Equal(now) || rep.Previous != nil || !reflect.DeepEqual(rep.Groups, tc.want) {
			t.Errorf("%s: %+v, error %v\nwant %+v", tc.name, rep, err, tc.want)
		}
	}
	for _, by := range [][]string{{"effort"}, {"key", "key"}, {"effort-sent", "effort-sent"}, {"key_name"}, {"model; DROP TABLE usage"}} {
		var de *domain.Error
		if _, err := Show(context.Background(), st.Read(), Query{Since: since, By: by}, now, 90); !errors.As(err, &de) || de.Status != http.StatusBadRequest {
			t.Errorf("by %q: %v", by, err)
		}
	}
}

// TestShowRecords seeds two windows of records and reads them as AC-35
// does, by key, model and effort sent with the previous window: nearest-rank
// p50 and p90 of output and reasoning tokens over the 2xx records whose
// tokens the provider reported, length finishes and empty responses; and
// the percentiles of random values against a sort in Go.
func TestShowRecords(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) int64 { return now.Add(-ago).UnixMilli() }
	var batch []Record
	add := func(id string, edit func(*Record)) {
		batch = append(batch, rec(id, func(r *Record) { r.CompletedAt = at(time.Hour) }, edit))
	}
	for i := int64(1); i <= 10; i++ { // key a, effort low: outputs 1 to 10, reasoning ten times that, none reported for 10
		add(fmt.Sprint("low", i), func(r *Record) {
			r.KeyName, r.EffortSent, r.OutputTokens, r.ReasoningTokens = "a", ptr("low"), i, ptr(10*i)
			if i == 10 {
				r.ReasoningTokens = nil
			}
		})
	}
	add("estimated", func(r *Record) {
		r.KeyName, r.EffortSent, r.OutputTokens, r.TokensSource = "a", ptr("low"), 1000, "estimated"
	})
	add("429", func(r *Record) {
		r.KeyName, r.EffortSent, r.StatusCode, r.ErrorType, r.OutputTokens, r.ReasoningTokens = "a", ptr("low"), 429, "rate_limited", 0, ptr(int64(0))
	})
	add("unsent", func(r *Record) {
		r.KeyName, r.OutputTokens, r.ReasoningTokens, r.FinishReason, r.ContentEmpty = "a", 7, nil, "length", true
	})
	for i, reason := range []string{"max_tokens", "model_context_window_exceeded", "stop"} {
		add(reason, func(r *Record) {
			r.KeyName, r.EffortSent, r.OutputTokens, r.ReasoningTokens, r.FinishReason = "b", ptr("high"), int64(100*(i+1)), ptr(int64(i)), reason
		})
	}
	for i, out := range []int64{4, 2} { // the previous window: key a, effort low
		batch = append(batch, rec(fmt.Sprint("before", i), func(r *Record) {
			r.KeyName, r.EffortSent, r.OutputTokens, r.ReasoningTokens, r.CompletedAt = "a", ptr("low"), out, ptr(out), at(30*time.Hour)
		}))
	}
	rnd := rand.New(rand.NewPCG(3, 25))
	var outs, reasons []int64
	for i := range 37 {
		out := rnd.Int64N(500)
		outs = append(outs, out)
		reasoning := ptr(rnd.Int64N(out + 1))
		if i%4 == 0 {
			reasoning = nil
		} else {
			reasons = append(reasons, *reasoning)
		}
		add(fmt.Sprint("random", i), func(r *Record) { r.KeyName, r.OutputTokens, r.ReasoningTokens = "r", out, reasoning })
	}
	seed(t, st, batch)

	q := Query{Since: now.Add(-24 * time.Hour), By: []string{"key", "model", "effort-sent"}, ComparePrevious: true}
	rep, err := Show(context.Background(), st.Read(), q, now, 90)
	pay := func(g Group) Group { g.Billing, g.Model = domain.BillingPayPerUse, "fast"; return counted(g) }
	want := Report{Source: "records", Since: q.Since, Until: now, Groups: []Group{
		ranked(pay(Group{Key: "a", EffortSent: "-", Requests: 1, Length: ptr(int64(1)), Empty: ptr(int64(1)), Input: 10, CacheRead: 2, Output: 7, CostUSD: ptr(0.25)}), 7, 7, -1, -1),
		ranked(pay(Group{Key: "a", EffortSent: "low", Requests: 12, Errors: 1, Estimated: ptr(int64(1)), Input: 120, CacheRead: 24, Output: 1055, Reasoning: ptr(int64(451)), CostUSD: ptr(3.0)}), 5, 9, 50, 90),
		ranked(pay(Group{Key: "b", EffortSent: "high", Requests: 3, Length: ptr(int64(2)), Input: 30, CacheRead: 6, Output: 600, Reasoning: ptr(int64(3)), CostUSD: ptr(0.75)}), 200, 300, 1, 2),
	}, Previous: &Report{Source: "records", Since: q.Since.Add(-24 * time.Hour), Until: q.Since, Groups: []Group{
		ranked(pay(Group{Key: "a", EffortSent: "low", Requests: 2, Input: 20, CacheRead: 4, Output: 6, Reasoning: ptr(int64(6)), CostUSD: ptr(0.5)}), 2, 4, 2, 4),
	}}}
	rep.Groups = slices.DeleteFunc(rep.Groups, func(g Group) bool { return g.Key == "r" })
	if err != nil || !reflect.DeepEqual(rep, want) {
		t.Errorf("by key, model and effort-sent, compared: %+v, error %v\nwant %+v", rep, err, want)
	}

	nearest := func(v []int64, p int) int64 {
		v = slices.Sorted(slices.Values(v))
		return v[(len(v)*p+99)/100-1]
	}
	rep, err = Show(context.Background(), st.Read(), Query{Since: q.Since, Key: "r"}, now, 90)
	if err != nil || len(rep.Groups) != 1 || rep.Groups[0].OutputP50 == nil || rep.Groups[0].ReasoningP90 == nil {
		t.Fatalf("random values: %+v, error %v", rep, err)
	}
	got := [4]int64{*rep.Groups[0].OutputP50, *rep.Groups[0].OutputP90, *rep.Groups[0].ReasoningP50, *rep.Groups[0].ReasoningP90}
	if want := [4]int64{nearest(outs, 50), nearest(outs, 90), nearest(reasons, 50), nearest(reasons, 90)}; got != want {
		t.Errorf("random values: output p50, p90, reasoning p50, p90 %v, want %v", got, want)
	}
}

// TestShowDaily seeds records older than usage_days, which the writer also
// summed in usage_daily: a window that starts before the retention reads
// those sums over whole UTC days, through today, without the counts and
// percentiles they do not hold and without a cost for a group no record
// priced; a previous window that starts before it is read the same
// way, to 00:00 of the day the current window starts; and effort-sent is
// refused whenever a window would be read from the sums.
func TestShowDaily(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	day := func(ago, hour int) int64 { return time.Date(2026, 11, 14-ago, hour, 0, 0, 0, time.UTC).UnixMilli() }
	seed(t, st, []Record{
		rec("before the first day", func(r *Record) { r.CompletedAt = day(101, 23) }),
		rec("first day, before since", func(r *Record) { r.CompletedAt, r.ErrorType, r.StatusCode = day(100, 1), "upstream_error", 502 }),
		rec("first day", func(r *Record) { r.CompletedAt, r.EffortSent = day(100, 20), ptr("low") }),
		rec("unpriced", func(r *Record) { r.CompletedAt, r.KeyName, r.CostUSD, r.CostSource = day(99, 12), "u", nil, "none" }),
		rec("subscription", func(r *Record) {
			r.CompletedAt, r.Billing, r.CostUSD, r.CostSource, r.ReasoningTokens = day(80, 12), domain.BillingSubscription, nil, "none", nil
		}),
		rec("previous end day, before since", func(r *Record) { r.CompletedAt = day(60, 6) }),
		rec("within", func(r *Record) { r.CompletedAt = day(30, 12) }),
		rec("today", func(r *Record) { r.CompletedAt = day(0, 11) }),
	})
	since := now.AddDate(0, 0, -100).Add(-2 * time.Hour) // 2026-08-06 10:00 UTC
	pay := func(g Group) Group { g.Billing = domain.BillingPayPerUse; return g }
	rep, err := Show(context.Background(), st.Read(), Query{Since: since, By: []string{"key"}}, now, 90)
	want := Report{Source: "daily", Since: time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC), Until: now, Groups: []Group{
		pay(Group{Key: "k", Requests: 5, Errors: 1, Input: 50, CacheRead: 10, Output: 25, Reasoning: ptr(int64(5)), CostUSD: ptr(1.25)}),
		pay(Group{Key: "u", Requests: 1, Input: 10, CacheRead: 2, Output: 5, Reasoning: ptr(int64(1))}),
		{Billing: domain.BillingSubscription, Key: "k", Requests: 1, Input: 10, CacheRead: 2, Output: 5},
	}}
	if err != nil || !reflect.DeepEqual(rep, want) {
		t.Errorf("a window before the retention: %+v, error %v\nwant %+v", rep, err, want)
	}
	if rep, err := Show(context.Background(), st.Read(), Query{Since: since, Key: "u"}, now, 90); err != nil || rep.Source != "daily" ||
		!reflect.DeepEqual(rep.Groups, []Group{pay(Group{Requests: 1, Input: 10, CacheRead: 2, Output: 5, Reasoning: ptr(int64(1))})}) {
		t.Errorf("filtered by key: %+v, error %v", rep, err)
	}

	// The record of 2026-09-15 06:00 is in neither window: the daily previous
	// one ends at 00:00 that day, the records one starts at 12:00.
	since = now.AddDate(0, 0, -60)
	rep, err = Show(context.Background(), st.Read(), Query{Since: since, ComparePrevious: true}, now, 90)
	want = Report{Source: "records", Since: since, Until: now, Groups: []Group{
		ranked(counted(pay(Group{Requests: 2, Input: 20, CacheRead: 4, Output: 10, Reasoning: ptr(int64(2)), CostUSD: ptr(0.5)})), 5, 5, 1, 1),
	}, Previous: &Report{Source: "daily", Since: time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), Groups: []Group{
		pay(Group{Requests: 4, Errors: 1, Input: 40, CacheRead: 8, Output: 20, Reasoning: ptr(int64(4)), CostUSD: ptr(0.75)}),
		{Billing: domain.BillingSubscription, Requests: 1, Input: 10, CacheRead: 2, Output: 5},
	}}}
	if err != nil || !reflect.DeepEqual(rep, want) {
		t.Errorf("a previous window before the retention: %+v, error %v\nwant %+v", rep, err, want)
	}
	// usage_days decides the source: with 30, the same window is read from
	// usage_daily over whole days, the record of 06:00 included.
	rep, err = Show(context.Background(), st.Read(), Query{Since: since}, now, 30)
	want = Report{Source: "daily", Since: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), Until: now, Groups: []Group{
		pay(Group{Requests: 3, Input: 30, CacheRead: 6, Output: 15, Reasoning: ptr(int64(3)), CostUSD: ptr(0.75)}),
	}}
	if err != nil || !reflect.DeepEqual(rep, want) {
		t.Errorf("usage_days 30: %+v, error %v\nwant %+v", rep, err, want)
	}
	// The boundary: a window that starts at now - usage_days exactly, or
	// later, is read from the records, effort-sent included; one that starts
	// a millisecond earlier from usage_daily.
	edge := now.AddDate(0, 0, -90)
	for _, s := range []time.Time{edge, now.AddDate(0, 0, -89)} {
		if rep, err := Show(context.Background(), st.Read(), Query{Since: s, By: []string{"effort-sent"}}, now, 90); err != nil || rep.Source != "records" {
			t.Errorf("since %v, usage_days 90: %s, error %v", s, rep.Source, err)
		}
	}
	if rep, err := Show(context.Background(), st.Read(), Query{Since: edge.Add(-time.Millisecond)}, now, 90); err != nil || rep.Source != "daily" {
		t.Errorf("a millisecond before the boundary: %s, error %v", rep.Source, err)
	}

	if _, err := Show(context.Background(), st.Read(), Query{Since: since, By: []string{"effort-sent"}}, now, 90); err != nil {
		t.Errorf("effort-sent within the retention: %v", err)
	}
	for _, q := range []Query{
		{Since: now.AddDate(0, 0, -91), By: []string{"effort-sent"}},
		{Since: edge.Add(-time.Millisecond), By: []string{"effort-sent"}},
		{Since: since, By: []string{"key", "effort-sent"}, ComparePrevious: true},
	} {
		var de *domain.Error
		if _, err := Show(context.Background(), st.Read(), q, now, 90); !errors.As(err, &de) || de.Status != http.StatusBadRequest {
			t.Errorf("effort-sent with %+v: %v", q, err)
		}
	}
}

// TestShowReasoning: a group's reasoning tokens and cost are absent where no
// record reported them, the sum of those reported in a mix, and 0 where a
// record reported 0, read from the records and from the daily sums alike
// (USE-9, phase-5 design C49).
func TestShowReasoning(t *testing.T) {
	st := openStore(t)
	unreported := func(r *Record) { r.ReasoningTokens, r.CostUSD, r.CostSource = nil, nil, "none" }
	seed(t, st, []Record{
		rec("none", func(r *Record) { r.KeyName = "none" }, unreported),
		rec("mixed, none", func(r *Record) { r.KeyName = "mixed" }, unreported),
		rec("mixed, reported", func(r *Record) { r.KeyName, r.ReasoningTokens = "mixed", ptr(int64(3)) }),
		rec("zero", func(r *Record) { r.KeyName, r.ReasoningTokens, r.CostUSD = "zero", ptr(int64(0)), ptr(0.0) }),
	})
	now := time.UnixMilli(1_794_614_401_000).Add(time.Hour)
	want := map[string]string{"mixed": "3 0.25", "none": "<nil> <nil>", "zero": "0 0"}
	for source, usageDays := range map[string]int64{"records": 90, "daily": 0} {
		rep, err := Show(context.Background(), st.Read(), Query{Since: now.Add(-2 * time.Hour), By: []string{"key"}}, now, usageDays)
		got := map[string]string{}
		for _, g := range rep.Groups {
			got[g.Key] = fmt.Sprint(deref(g.Reasoning), deref(g.CostUSD))
		}
		if err != nil || rep.Source != source || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %s %v, error %v; want %v", source, rep.Source, got, err, want)
		}
	}
}
