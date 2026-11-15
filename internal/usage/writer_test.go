package usage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// rec is a stored-ready record of key k on day 2026-11-14 (UTC) unless edited.
func rec(id string, edit ...func(*Record)) Record {
	r := Record{ID: id, RequestID: id, TraceID: "t", Attempt: 1, Endpoint: "chat", KeyName: "k", Model: "fast", Target: "p/m",
		Billing: domain.BillingPayPerUse, StatusCode: 200, InputTokens: 10, CacheReadTokens: 2, OutputTokens: 5, ReasoningTokens: ptr(int64(1)),
		TokensSource: "provider", CostUSD: ptr(0.25), CostSource: "price_table", StartedAt: 1_794_614_400_000, CompletedAt: 1_794_614_401_000,
		ConfigRevision: 3, ProxyVersion: "v"}
	for _, e := range edit {
		e(&r)
	}
	return r
}

// daily reads usage_daily, and the same sums from usage grouped alike: the
// two must agree (USE-2).
func daily(t *testing.T, db *sql.DB) (aggregates, fromRecords string) {
	t.Helper()
	read := func(q string) string {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var b strings.Builder
		for rows.Next() {
			var day, key, model, target, billing string
			var n [7]int64
			var cost float64
			if err := rows.Scan(&day, &key, &model, &target, &billing, &n[0], &n[1], &n[2], &n[3], &n[4], &n[5], &n[6], &cost); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s %s %s %s %s %v %.6f\n", day, key, model, target, billing, n, cost)
		}
		return b.String()
	}
	return read(`SELECT day, key_name, model, target, billing, requests, errors, input_tokens, cache_read_tokens, cache_write_tokens,
			output_tokens, reasoning_tokens, cost_usd FROM usage_daily ORDER BY 1, 2, 3, 4, 5`),
		read(`SELECT strftime('%Y-%m-%d', completed_at / 1000, 'unixepoch'), key_name, model, target, billing, COUNT(*), COUNT(error_type),
			SUM(input_tokens), SUM(cache_read_tokens), SUM(cache_write_tokens), SUM(output_tokens), SUM(COALESCE(reasoning_tokens, 0)),
			SUM(COALESCE(cost_usd, 0)) FROM usage GROUP BY 1, 2, 3, 4, 5 ORDER BY 1, 2, 3, 4, 5`)
}

func count(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestWriterBatches: records reach the store in batches of at most 100,
// every column as the record holds it, NULL for an empty string or a nil
// pointer, and usage_daily equals the GROUP BY over usage, per UTC day and
// per billing mode.
func TestWriterBatches(t *testing.T) {
	st := openStore(t)
	var mu sync.Mutex
	var sizes []int
	write := func(ctx context.Context, fn func(*sql.Tx) error) error {
		return st.Write(ctx, func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRow("SELECT COUNT(*) FROM usage").Scan(&n); err != nil {
				return err
			}
			if err := fn(tx); err != nil {
				return err
			}
			var m int
			if err := tx.QueryRow("SELECT COUNT(*) FROM usage").Scan(&m); err != nil {
				return err
			}
			mu.Lock()
			sizes = append(sizes, m-n)
			mu.Unlock()
			return nil
		})
	}
	var log bytes.Buffer
	w := NewWriter(write, slog.New(slog.NewJSONHandler(&log, nil)))
	for i := range 250 {
		w.Add(rec(fmt.Sprint(i), func(r *Record) {
			switch i % 5 {
			case 1:
				r.CompletedAt += 86_400_000 // the next UTC day
			case 2:
				r.Billing, r.CostUSD, r.CostSource = domain.BillingSubscription, nil, "none"
			case 3:
				r.ErrorType, r.StatusCode, r.ReasoningTokens, r.ResponseModel = "upstream_error", 502, nil, ""
			case 4:
				r.KeyName, r.TokensSource = "other", "estimated"
			}
		}))
	}
	if n := w.Close(context.Background()); n != 0 || log.Len() != 0 {
		t.Fatalf("Close: %d, log %s", n, &log)
	}
	total := 0
	for _, n := range sizes {
		if n > batchSize {
			t.Errorf("a batch of %d records", n)
		}
		total += n
	}
	if total != 250 || len(sizes) < 3 {
		t.Errorf("batches %v", sizes)
	}
	if agg, records := daily(t, st.Read()); agg != records || strings.Count(agg, "\n") != 4 {
		t.Errorf("usage_daily\n%s\nGROUP BY usage\n%s", agg, records)
	}
	var reasoning, responseModel, errorType sql.NullString
	if err := st.Read().QueryRow("SELECT reasoning_tokens, response_model, error_type FROM usage WHERE id = '3'").Scan(&reasoning, &responseModel, &errorType); err != nil ||
		reasoning.Valid || responseModel.Valid || errorType.String != "upstream_error" {
		t.Errorf("record 3: %v %v %v, error %v", reasoning, responseModel, errorType, err)
	}
	if n := count(t, st.Read(), `SELECT COUNT(*) FROM usage WHERE id = '0' AND request_id = '0' AND trace_id = 't' AND attempt = 1 AND endpoint = 'chat'
		AND key_name = 'k' AND model = 'fast' AND target = 'p/m' AND billing = 'pay_per_use' AND response_model IS NULL AND model_mismatch = 0
		AND status_code = 200 AND error_type IS NULL AND finish_reason IS NULL AND stream = 0 AND input_tokens = 10 AND cache_read_tokens = 2
		AND cache_write_tokens = 0 AND output_tokens = 5 AND reasoning_tokens = 1 AND tokens_source = 'provider' AND cost_usd = 0.25
		AND cost_source = 'price_table' AND effort_requested IS NULL AND effort_sent IS NULL AND effort_source IS NULL AND effort_adjusted = 0
		AND max_output_sent IS NULL AND params_changed IS NULL AND content_empty = 0 AND ttft_ms IS NULL AND started_at = 1794614400000
		AND completed_at = 1794614401000 AND config_revision = 3 AND proxy_version = 'v'`); n != 1 {
		t.Error("record 0 is not stored as it was added")
	}
}

// TestWriterIdempotent: the same batch committed twice inserts nothing twice
// and moves no aggregate (USE-2).
func TestWriterIdempotent(t *testing.T) {
	st := openStore(t)
	batch := []Record{rec("a"), rec("b", func(r *Record) { r.ErrorType = "upstream_timeout" })}
	for range 2 {
		if err := st.Write(context.Background(), func(tx *sql.Tx) error { return insert(tx, entries(batch)) }); err != nil {
			t.Fatal(err)
		}
	}
	agg, records := daily(t, st.Read())
	if n := count(t, st.Read(), "SELECT COUNT(*) FROM usage"); n != 2 || agg != records || !strings.Contains(agg, " [2 1 20 4 0 10 2] 0.500000") {
		t.Errorf("%d records; usage_daily\n%s\nGROUP BY usage\n%s", n, agg, records)
	}
}

// TestWriterCloseSlowWrite: a write still running when Close's deadline
// passes is waited for, and once it commits its batch counts as stored: only
// the record queued behind it, never handed to the store, is logged at
// critical level and counted, with every column (design section 5).
func TestWriterCloseSlowWrite(t *testing.T) {
	st := openStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	write := func(ctx context.Context, fn func(*sql.Tx) error) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return st.Write(ctx, fn)
	}
	var log bytes.Buffer
	w := NewWriter(write, slog.New(slog.NewJSONHandler(&log, nil)))
	w.Add(rec("slow"))
	<-entered
	w.Add(rec("queued", func(r *Record) { r.EffortSent = ptr("high") }))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	closed := make(chan int, 1)
	go func() { closed <- w.Close(ctx) }()
	<-ctx.Done()
	select {
	case n := <-closed:
		t.Fatalf("Close returned %d with a write in progress", n)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if n := <-closed; n != 1 {
		t.Errorf("Close: %d records not stored, want 1", n)
	}
	if n := count(t, st.Read(), "SELECT COUNT(*) FROM usage WHERE id = 'slow'"); n != 1 || calls.Load() != 1 {
		t.Errorf("the slow batch: %d rows, %d writes", n, calls.Load())
	}
	var line struct {
		Level, Msg string
		Record     Record
	}
	if err := json.Unmarshal(bytes.TrimSpace(log.Bytes()), &line); err != nil || line.Level != "ERROR+4" || line.Msg != "usage record not stored" ||
		line.Record.ID != "queued" || line.Record.EffortSent == nil || *line.Record.EffortSent != "high" || line.Record.ProxyVersion != "v" {
		t.Errorf("log %s: %+v, error %v", &log, line, err)
	}

	// After Close, Add logs at once.
	w.Add(rec("late"))
	if n := w.Close(context.Background()); n != 2 || strings.Count(log.String(), `"msg":"usage record not stored"`) != 2 {
		t.Errorf("after Close: %d; log %s", n, &log)
	}
}

// TestWriterRetry: a failed commit is retried with the same batch until it
// commits, logged once when it fails and once when it commits again.
func TestWriterRetry(t *testing.T) {
	retryWait = time.Millisecond
	t.Cleanup(func() { retryWait = 50 * time.Millisecond })
	st := openStore(t)
	var calls atomic.Int32
	write := func(ctx context.Context, fn func(*sql.Tx) error) error {
		if calls.Add(1) <= 2 {
			return errors.New("disk I/O error")
		}
		return st.Write(ctx, fn)
	}
	var log bytes.Buffer
	w := NewWriter(write, slog.New(slog.NewJSONHandler(&log, nil)))
	w.Add(rec("a"))
	w.Add(rec("b"))
	if n := w.Close(context.Background()); n != 0 || calls.Load() != 3 {
		t.Errorf("Close: %d, %d writes", n, calls.Load())
	}
	if n := count(t, st.Read(), "SELECT COUNT(*) FROM usage"); n != 2 ||
		!strings.Contains(log.String(), `"msg":"usage records not committed: retrying","records":2,"error":"disk I/O error"`) ||
		!strings.Contains(log.String(), `"msg":"usage records committed again","failed_commits":2`) {
		t.Errorf("%d records; log %s", n, &log)
	}

	// A store that never recovers: Close's deadline ends the retries and the
	// batch goes to the critical log.
	failing := func(context.Context, func(*sql.Tx) error) error { return errors.New("disk I/O error") }
	log.Reset()
	w = NewWriter(failing, slog.New(slog.NewJSONHandler(&log, nil)))
	w.Add(rec("c"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if n := w.Close(ctx); n != 1 || !strings.Contains(log.String(), `"level":"ERROR+4","msg":"usage record not stored","record":{"id":"c"`) {
		t.Errorf("Close: %d; log %s", n, &log)
	}
}

// TestWriterAddRacingClose: Adds racing Close under -race; every record is
// stored or logged, none lost silently.
func TestWriterAddRacingClose(t *testing.T) {
	st := openStore(t)
	var log bytes.Buffer
	w := NewWriter(st.Write, slog.New(slog.NewJSONHandler(&log, nil)))
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 50 {
				w.Add(rec(fmt.Sprintf("%d-%d", g, i)))
			}
		})
	}
	w.Close(context.Background())
	wg.Wait()
	lost := w.Close(context.Background())
	if stored := count(t, st.Read(), "SELECT COUNT(*) FROM usage"); stored+lost != 400 || strings.Count(log.String(), "usage record not stored") != lost {
		t.Errorf("%d stored, %d logged", stored, lost)
	}
}

// TestWriterUnavailable is USE-10 and AC-34 with an injected failing write:
// the third consecutive failed commit makes the store unavailable, the retry
// then runs every unavailableRetry with the same batch while new records
// wait in the queue, and the first commit that succeeds makes it available
// again and stores every record. The log has one line when the failures
// start, one when the store becomes unavailable and one when it recovers.
// unavailableRetry is a timer, which never fires early: the gaps after the
// third failure are at least that long.
func TestWriterUnavailable(t *testing.T) {
	batchWait, retryWait, unavailableRetry = time.Millisecond, time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() {
		batchWait, retryWait, unavailableRetry = 50*time.Millisecond, 50*time.Millisecond, 5*time.Second
	})
	st := openStore(t)
	var failing atomic.Bool
	failing.Store(true)
	var mu sync.Mutex
	var calls []time.Time
	ok := 0 // the number of the first write that succeeded
	write := func(ctx context.Context, fn func(*sql.Tx) error) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, time.Now())
		if failing.Load() {
			return errors.New("disk I/O error")
		}
		if ok == 0 {
			ok = len(calls)
		}
		return st.Write(ctx, fn)
	}
	attempts := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(calls)
	}
	wait := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not within 5 s, %d writes", what, attempts())
			}
		}
	}
	var log logBuffer
	w := NewWriter(write, slog.New(slog.NewJSONHandler(&log, nil)))
	if !w.Available() {
		t.Fatal("a new writer is unavailable")
	}
	w.Add(rec("a"))
	wait("unavailable", func() bool { return !w.Available() })
	if n := attempts(); n != unavailableAfter {
		t.Errorf("unavailable after %d writes", n)
	}
	w.Add(rec("b")) // waits in the queue behind the failing batch
	wait("a retry while unavailable", func() bool { return attempts() > unavailableAfter })
	if w.Available() {
		t.Error("a failed retry made the store available")
	}
	failing.Store(false)
	wait("available again", w.Available)
	if n := w.Close(context.Background()); n != 0 {
		t.Errorf("Close: %d records logged", n)
	}
	if n := count(t, st.Read(), "SELECT COUNT(*) FROM usage"); n != 2 {
		t.Errorf("%d records stored, want 2", n)
	}
	mu.Lock()
	for i := 1; i < ok; i++ {
		if gap := calls[i].Sub(calls[i-1]); i >= unavailableAfter && gap < unavailableRetry || i < unavailableAfter && gap >= unavailableRetry {
			t.Errorf("write %d came %v after write %d", i+1, gap, i)
		}
	}
	mu.Unlock()
	got := log.String()
	if strings.Count(got, "\n") != 3 ||
		!strings.Contains(got, `"level":"ERROR","msg":"usage records not committed: retrying","records":1,"error":"disk I/O error"`) ||
		!strings.Contains(got, `"level":"ERROR","msg":"usage store unavailable: pay_per_use targets are excluded","failed_commits":3,"retry_every":"200ms","error":"disk I/O error"`) ||
		!strings.Contains(got, `"level":"INFO","msg":"usage records committed again","failed_commits":`) {
		t.Errorf("log %s", got)
	}
}

// TestWriterUnavailableClose: Close does not wait out unavailableRetry; it
// retries every retryWait while its context allows, and a store that never
// recovers sends the batch and the records queued behind it to the critical
// log.
func TestWriterUnavailableClose(t *testing.T) {
	batchWait, retryWait, unavailableRetry = time.Millisecond, time.Millisecond, time.Hour
	t.Cleanup(func() {
		batchWait, retryWait, unavailableRetry = 50*time.Millisecond, 50*time.Millisecond, 5*time.Second
	})
	st := openStore(t)
	var failing atomic.Bool
	failing.Store(true)
	write := func(ctx context.Context, fn func(*sql.Tx) error) error {
		if failing.Load() {
			return errors.New("disk I/O error")
		}
		return st.Write(ctx, fn)
	}
	var log logBuffer
	w := NewWriter(write, slog.New(slog.NewJSONHandler(&log, nil)))
	w.Add(rec("a"))
	for deadline := time.Now().Add(5 * time.Second); w.Available(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("still available after 5 s of failures")
		}
	}
	w.Add(rec("b"))
	failing.Store(false)
	if n := w.Close(context.Background()); n != 0 || count(t, st.Read(), "SELECT COUNT(*) FROM usage") != 2 {
		t.Errorf("Close during the unavailable wait: %d logged; log %s", n, log.String())
	}

	failing.Store(true)
	var lost logBuffer
	w = NewWriter(write, slog.New(slog.NewJSONHandler(&lost, nil)))
	w.Add(rec("c"))
	for deadline := time.Now().Add(5 * time.Second); w.Available(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("still available after 5 s of failures")
		}
	}
	w.Add(rec("d"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if n := w.Close(ctx); n != 2 || strings.Count(lost.String(), `"level":"ERROR+4","msg":"usage record not stored"`) != 2 {
		t.Errorf("Close: %d; log %s", n, lost.String())
	}
}

// TestWriterOverflow: while a write holds the batch, records fill the queue
// of 10,000; the one that finds it full is logged at critical level and
// counted at once, Add never blocks, and every queued record is handed to a
// write that succeeds once the held one returns (USE-10). The writes after
// the first commit nothing: 10,000 inserts under -race take seconds, and
// TestWriterBatches covers the inserts.
func TestWriterOverflow(t *testing.T) {
	batchWait = time.Millisecond
	t.Cleanup(func() { batchWait = 50 * time.Millisecond })
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	write := func(context.Context, func(*sql.Tx) error) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	var log logBuffer
	w := NewWriter(write, slog.New(slog.NewJSONHandler(&log, nil)))
	w.Add(rec("held"))
	<-entered
	for i := range queueSize {
		w.Add(rec(fmt.Sprint(i)))
	}
	if log.String() != "" {
		t.Fatalf("the queue overflowed early: %s", log.String())
	}
	w.Add(rec("over"))
	var line struct {
		Level, Msg string
		Record     Record
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(log.String())), &line); err != nil || line.Level != "ERROR+4" ||
		line.Msg != "usage record not stored" || line.Record.ID != "over" {
		t.Errorf("log %s: %+v, error %v", log.String(), line, err)
	}
	close(release)
	if n := w.Close(context.Background()); n != 1 || calls.Load() < 1+queueSize/batchSize {
		t.Errorf("Close: %d records logged, want 1; %d writes", n, calls.Load())
	}
}

// logBuffer is a log destination the writer goroutine and the test share.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// entries are the queue entries of records rs.
func entries(rs []Record) []entry {
	es := make([]entry, len(rs))
	for i, r := range rs {
		es[i] = entry{rec: r}
	}
	return es
}

// TestWriterBudgets: an inserted pay_per_use record with a cost adds it to
// the budget_totals rows of the instance, its key and its alias whose
// since_revision is at or before its config_revision; a record without a
// cost, of another billing, or already stored adds nothing; an audit row is
// inserted once by its id, also when its batch is committed twice (phase-3
// design sections 5 and 8).
func TestWriterBudgets(t *testing.T) {
	st := openStore(t)
	if err := st.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO budget_totals (scope_type, scope_name, cost_usd, since_revision) VALUES
			('global', '', 0, 2), ('key', 'k', 0, 4), ('model', 'fast', 1, 3), ('key', 'other', 0, 1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	at := time.UnixMilli(1_794_614_401_000)
	warn := admission.Warning{Event: "budget_warning", Scope: "key:k", Period: "d:2026-11-14", At: at}
	batch := entries([]Record{
		rec("a"), // revision 3: global and fast
		rec("b", func(r *Record) { r.ConfigRevision = 5 }),                                                          // all three
		rec("c", func(r *Record) { r.CostUSD = nil; r.CostSource = "none" }),                                        // unpriced
		rec("d", func(r *Record) { r.Billing = domain.BillingSubscription; r.CostUSD = nil; r.ConfigRevision = 9 }), // no USD
	})
	batch = append(batch, entry{warn: &warn})
	for range 2 {
		if err := st.Write(context.Background(), func(tx *sql.Tx) error { return insert(tx, batch) }); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.Read().Query("SELECT scope_type || ':' || scope_name, since_revision, cost_usd FROM budget_totals ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var scope string
		var since int64
		var usd float64
		if err := rows.Scan(&scope, &since, &usd); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s %d %.2f", scope, since, usd))
	}
	if want := []string{"global: 2 0.50", "key:k 4 0.25", "key:other 1 0.00", "model:fast 3 1.50"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("budget_totals %v, want %v", got, want)
	}
	if n := count(t, st.Read(), `SELECT COUNT(*) FROM audit WHERE id = 'budget_warning:key:k:d:2026-11-14' AND actor = 'system'
		AND action = 'budget_warning' AND resource_type = 'budget' AND resource_name = 'key:k' AND previous_value IS NULL
		AND new_value IS NULL AND result = 'warning' AND revision IS NULL AND created_at = 1794614401000`); n != 1 {
		t.Error("the audit row is not stored once as the warning gives it")
	}
	if n := count(t, st.Read(), "SELECT COUNT(*) FROM audit"); n != 1 {
		t.Errorf("%d audit rows", n)
	}
}

// TestWriterAuditLost: an audit row that cannot be queued is logged at
// critical level, with its id, and is not counted as a usage record.
func TestWriterAuditLost(t *testing.T) {
	var log logBuffer
	w := NewWriter(func(context.Context, func(*sql.Tx) error) error { return nil }, slog.New(slog.NewJSONHandler(&log, nil)))
	if n := w.Close(context.Background()); n != 0 {
		t.Fatalf("Close: %d", n)
	}
	w.Audit(admission.Warning{Event: "budget_exhausted", Scope: "global", Period: "m:2026-11", At: time.UnixMilli(5)})
	if l := log.String(); !strings.Contains(l, `"level":"ERROR+4","msg":"audit row not stored","id":"budget_exhausted:global:m:2026-11"`) {
		t.Errorf("log %s", l)
	}
	if n := w.Close(context.Background()); n != 0 {
		t.Errorf("Close counts %d", n)
	}
}

// TestRestore: this month's pay_per_use spend per day, from usage_daily, and
// every never total, from budget_totals, seed the counters a fresh State
// reads, for the instance, the key and the alias; a day after today counts
// as today; other months and other billing modes count nowhere (RES-9).
func TestRestore(t *testing.T) {
	st := openStore(t)
	day := func(d string, edit ...func(*Record)) func(*Record) {
		return func(r *Record) {
			at, err := time.Parse(time.DateOnly, d)
			if err != nil {
				t.Fatal(err)
			}
			r.CompletedAt = at.Add(time.Hour).UnixMilli()
			for _, e := range edit {
				e(r)
			}
		}
	}
	batch := []Record{
		rec("a", day("2026-11-14")), // today, 0.25
		rec("b", day("2026-11-14", func(r *Record) { r.KeyName = "j"; r.Model = "slow" })),
		rec("c", day("2026-11-02")), // this month
		rec("d", day("2026-10-31")), // last month
		rec("e", day("2026-11-15")), // after today: counts as today
		rec("f", day("2026-11-14", func(r *Record) { r.Billing = domain.BillingNone; r.CostUSD = nil })), // no cost
	}
	if err := st.Write(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO budget_totals (scope_type, scope_name, cost_usd, since_revision) VALUES
			('global', '', 7, 2), ('model', 'fast', 3, 6)`); err != nil {
			return err
		}
		return insert(tx, entries(batch))
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 14, 23, 0, 0, 0, time.UTC)
	a := admission.New(func() time.Time { return now }, nil)
	if err := Restore(context.Background(), st.Read(), a, now); err != nil {
		t.Fatal(err)
	}
	budget := func(scope, interval string, since int64) admission.Budget {
		return admission.Budget{Scope: scope, Spec: domain.Budget{LimitUSD: ptr(100.0), ResetInterval: interval}, Since: since}
	}
	for _, tc := range []struct {
		b    admission.Budget
		want float64
	}{
		{budget("global", "daily", 0), 0.75},
		{budget("global", "monthly", 0), 1.0},
		{budget("key:k", "daily", 0), 0.5},
		{budget("key:k", "monthly", 0), 0.75},
		{budget("key:j", "daily", 0), 0.25},
		{budget("model:fast", "monthly", 0), 0.75},
		{budget("model:slow", "monthly", 0), 0.25},
		{budget("global", "never", 2), 8.25},  // 7 and the five priced records of revision 3
		{budget("model:fast", "never", 6), 3}, // none of revision 6 or later
		{budget("model:fast", "never", 7), 0}, // set again since: starts at zero
		{budget("key:k", "never", 3), 0},      // no row
	} {
		if got := a.Spent(tc.b); got < tc.want-1e-9 || got > tc.want+1e-9 {
			t.Errorf("%+v: %v, want %v", tc.b, got, tc.want)
		}
	}
}

// TestSeeder: the quota seed reads the subscription records of the
// provider's targets that completed at or after from, each with its key,
// times and input plus output tokens; a provider whose name extends
// another's, another billing and an older record are not read; a failure is
// logged with the provider and returned.
func TestSeeder(t *testing.T) {
	st := openStore(t)
	sub := func(r *Record) { r.Billing, r.CostUSD, r.CostSource = domain.BillingSubscription, nil, "none" }
	batch := []Record{
		rec("a", sub),
		rec("b", sub, func(r *Record) {
			r.KeyName, r.Target, r.StartedAt, r.CompletedAt = "j", "p/other/x", 1_794_614_402_000, 1_794_614_403_500
		}),
		rec("c", sub, func(r *Record) { r.Target = "p0/m" }),
		rec("d", sub, func(r *Record) { r.Target = "p.x/m" }),
		rec("e"), // pay_per_use
		rec("f", sub, func(r *Record) { r.CompletedAt = 1_794_614_399_999 }),
	}
	if err := st.Write(context.Background(), func(tx *sql.Tx) error { return insert(tx, entries(batch)) }); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	seed := Seeder(st.Read(), slog.New(slog.NewJSONHandler(&log, nil)))
	events, err := seed("p", time.UnixMilli(1_794_614_400_000))
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(events, func(a, b admission.Event) int { return a.Sent.Compare(b.Sent) })
	want := []admission.Event{
		{Key: "k", Sent: time.UnixMilli(1_794_614_400_000), Ended: time.UnixMilli(1_794_614_401_000), Tokens: 15},
		{Key: "j", Sent: time.UnixMilli(1_794_614_402_000), Ended: time.UnixMilli(1_794_614_403_500), Tokens: 15},
	}
	if fmt.Sprint(events) != fmt.Sprint(want) {
		t.Errorf("events %v, want %v", events, want)
	}
	if err := st.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE usage SET started_at = 'not a time' WHERE id = 'a'")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := seed("p", time.UnixMilli(0)); err == nil || !strings.Contains(log.String(), `"level":"ERROR","msg":"quota windows not seeded","provider":"p","error":`) {
		t.Errorf("a row that cannot be read: %v; log %s", err, &log)
	}
}
