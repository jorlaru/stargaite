package usage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/domain"
)

// LevelCritical is the level of the log line of a record the writer could
// not store (USE-10); serve names it CRITICAL.
const LevelCritical = slog.LevelError + 4

// The writer's batches (USE-10): at most batchSize records, committed
// batchWait after the first; a failed commit is retried after retryWait, and
// every unavailableRetry from the unavailableAfter-th consecutive failure,
// when the store is unavailable. The waits are variables that tests lower.
var (
	batchWait        = 50 * time.Millisecond
	retryWait        = 50 * time.Millisecond
	unavailableRetry = 5 * time.Second
)

const (
	batchSize        = 100
	queueSize        = 10_000
	unavailableAfter = 3
)

// Writer is the one writer of usage records (design section 5): a goroutine
// that commits them in batches through the store's write path, each batch in
// one transaction, each record inserted or ignored by its id and the daily
// aggregate and the never budgets' totals moved only for a row inserted
// (USE-2). The audit rows of budget warnings share its queue and batches.
// It is safe for concurrent use.
type Writer struct {
	write func(context.Context, func(*sql.Tx) error) error
	log   *slog.Logger

	mu      sync.RWMutex // held for reading while Add queues, so Close never closes the queue under it
	closed  bool
	queue   chan entry
	closing chan struct{}   // closed by Close
	ctx     context.Context // Close's: set before closing is closed, read only after
	done    chan struct{}   // closed when the goroutine has exited
	lost    atomic.Int64    // records logged instead of stored
	down    atomic.Bool     // the store is unavailable (Available)
}

// NewWriter starts the writer on write, the store's write path.
func NewWriter(write func(context.Context, func(*sql.Tx) error) error, log *slog.Logger) *Writer {
	w := &Writer{write: write, log: log, queue: make(chan entry, queueSize), closing: make(chan struct{}), done: make(chan struct{})}
	go w.run()
	return w
}

// Add queues r for the next batch. It never blocks: when the queue is full,
// or once Close has begun, r is written as one log line at critical level
// and counted instead, never lost silently and never rate-limited (USE-10,
// OPS-9).
func (w *Writer) Add(r Record) { w.enqueue(entry{rec: r}) }

// Audit queues the audit row of warning a for the next batch (kick-off
// section 10): id <event>:<scope>:<period>, inserted or ignored, so a retried
// batch or a second crossing in the period adds no row twice. Like Add it
// never blocks: a row that cannot be queued is logged at critical level, but
// not counted by Close, which counts usage records.
func (w *Writer) Audit(a admission.Warning) { w.enqueue(entry{warn: &a}) }

// entry is one item of the queue: a usage record, or the audit row of warn.
type entry struct {
	rec  Record
	warn *admission.Warning
}

func (w *Writer) enqueue(e entry) {
	w.mu.RLock()
	queued := false
	if !w.closed {
		select {
		case w.queue <- e:
			queued = true
		default:
		}
	}
	w.mu.RUnlock()
	if !queued {
		w.lose(e)
	}
}

// Available reports whether the store takes records: false from the third
// consecutive failed commit until a commit succeeds (USE-10). Meanwhile
// exclude passes over pay_per_use targets and records wait in the queue.
func (w *Writer) Available() bool {
	return !w.down.Load()
}

// Close stops accepting records and waits for the write in progress, which
// always ends (within the store's busy timeout and the commit); a committed
// batch counts as stored. It then commits the rest of the queue while ctx
// allows, retrying a failed commit every retryWait, and returns only once
// the goroutine has exited, so no write starts after it. Every record not
// stored by then, and every one Add could not queue, has been logged at
// critical level; Close returns how many, over the writer's life.
func (w *Writer) Close(ctx context.Context) int {
	w.mu.Lock()
	if !w.closed {
		w.closed, w.ctx = true, ctx
		close(w.closing)
		close(w.queue)
	}
	w.mu.Unlock()
	<-w.done
	return int(w.lost.Load())
}

// run is the writer goroutine: a batch starts with the first record queued
// and ends at batchSize records or batchWait later, whichever comes first;
// once the queue is closed and empty it exits.
func (w *Writer) run() {
	defer close(w.done)
	for r := range w.queue {
		batch := []entry{r}
		wait := time.NewTimer(batchWait)
	fill:
		for len(batch) < batchSize {
			select {
			case r, ok := <-w.queue:
				if !ok {
					break fill
				}
				batch = append(batch, r)
			case <-wait.C:
				break fill
			}
		}
		wait.Stop()
		if !w.commit(batch) {
			w.lose(batch...)
			for r := range w.queue {
				w.lose(r)
			}
			return
		}
	}
}

// commit writes batch in one transaction, retrying a failed commit with the
// same batch after retryWait; from the third consecutive failure the store
// is unavailable and the retry runs every unavailableRetry, until a commit
// makes it available again (USE-10). A run of failures logs one line when it
// starts, one when the store becomes unavailable and one when a commit
// succeeds again, never one per retry. False when Close's context ended
// before a write could start.
func (w *Writer) commit(batch []entry) bool {
	for failures := 0; ; {
		if w.ended() {
			return false
		}
		err := w.write(context.Background(), func(tx *sql.Tx) error { return insert(tx, batch) })
		if err == nil {
			w.down.Store(false)
			if failures > 0 {
				w.log.Info("usage records committed again", "failed_commits", failures)
			}
			return true
		}
		failures++
		wait := retryWait
		switch failures {
		case 1:
			w.log.Error("usage records not committed: retrying", "records", len(batch), "error", err.Error())
		case unavailableAfter:
			w.down.Store(true)
			w.log.Error("usage store unavailable: pay_per_use targets are excluded", "failed_commits", failures,
				"retry_every", unavailableRetry.String(), "error", err.Error())
		}
		if failures >= unavailableAfter {
			wait = unavailableRetry
		}
		if !w.sleep(wait) {
			return false
		}
	}
}

// ended reports whether Close's context has ended.
func (w *Writer) ended() bool {
	select {
	case <-w.closing:
		return w.ctx.Err() != nil
	default:
		return false
	}
}

// sleep waits d, or retryWait from the moment Close begins, so the flush
// never waits out unavailableRetry; false when Close's context ends first.
func (w *Writer) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-w.closing:
	}
	t.Reset(retryWait)
	select {
	case <-t.C:
		return true
	case <-w.ctx.Done():
		return false
	}
}

// lose writes each entry as one log line at critical level, and counts the
// records.
func (w *Writer) lose(es ...entry) {
	for _, e := range es {
		if a := e.warn; a != nil {
			w.log.Log(context.Background(), LevelCritical, "audit row not stored", "id", a.Event+":"+a.Scope+":"+a.Period,
				"action", a.Event, "resource_name", a.Scope, "created_at", a.At.UnixMilli())
			continue
		}
		w.lost.Add(1)
		w.log.Log(context.Background(), LevelCritical, "usage record not stored", "record", e.rec)
	}
}

const insertUsage = `INSERT OR IGNORE INTO usage (id, request_id, trace_id, attempt, endpoint, key_name, model, target, billing,
	response_model, model_mismatch, status_code, error_type, finish_reason, stream, input_tokens, cache_read_tokens,
	cache_write_tokens, output_tokens, reasoning_tokens, tokens_source, cost_usd, cost_source, effort_requested,
	effort_alias, effort_sent, effort_source, effort_adjusted, max_output_requested, max_output_alias, max_output_sent,
	params_changed, content_empty, ttft_ms, started_at, completed_at, config_revision, proxy_version)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const upsertDaily = `INSERT INTO usage_daily (day, key_name, model, target, billing, requests, errors, input_tokens,
	cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd)
VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (day, key_name, model, target, billing) DO UPDATE SET requests = requests + 1, errors = errors + excluded.errors,
	input_tokens = input_tokens + excluded.input_tokens, cache_read_tokens = cache_read_tokens + excluded.cache_read_tokens,
	cache_write_tokens = cache_write_tokens + excluded.cache_write_tokens, output_tokens = output_tokens + excluded.output_tokens,
	reasoning_tokens = reasoning_tokens + excluded.reasoning_tokens, cost_usd = cost_usd + excluded.cost_usd`

const chargeNever = `UPDATE budget_totals SET cost_usd = cost_usd + ?
WHERE scope_type = ? AND scope_name = ? AND since_revision <= ?`

const insertWarning = `INSERT OR IGNORE INTO audit (id, actor, action, resource_type, resource_name, result, created_at)
VALUES (?, 'system', ?, ?, ?, 'warning', ?)`

// insert inserts each record or ignores it when its id is stored already;
// only an inserted row moves usage_daily, for the UTC day of completed_at
// (USE-2): one request, one error when error_type is set, each token count
// and the cost, a NULL counting as 0; and, for a pay_per_use record with a
// cost, the total of each never budget of the instance, its key and its
// alias whose counter started at or before the record's revision (phase-3
// design section 8). An audit row is inserted or ignored by its id.
func insert(tx *sql.Tx, batch []entry) error {
	for _, e := range batch {
		if a := e.warn; a != nil {
			kind := "quota"
			if strings.HasPrefix(a.Event, "budget_") {
				kind = "budget"
			}
			if _, err := tx.Exec(insertWarning, a.Event+":"+a.Scope+":"+a.Period, a.Event, kind, a.Scope, a.At.UnixMilli()); err != nil {
				return err
			}
			continue
		}
		r := e.rec
		res, err := tx.Exec(insertUsage, r.ID, r.RequestID, r.TraceID, r.Attempt, r.Endpoint, r.KeyName, r.Model, r.Target, r.Billing,
			null(r.ResponseModel), r.ModelMismatch, r.StatusCode, null(r.ErrorType), null(r.FinishReason), r.Stream, r.InputTokens,
			r.CacheReadTokens, r.CacheWriteTokens, r.OutputTokens, r.ReasoningTokens, r.TokensSource, r.CostUSD, r.CostSource,
			r.EffortRequested, r.EffortAlias, r.EffortSent, null(r.EffortSource), r.EffortAdjusted, r.MaxOutputRequested,
			r.MaxOutputAlias, r.MaxOutputSent, null(r.ParamsChanged), r.ContentEmpty, r.TTFTMS, r.StartedAt, r.CompletedAt,
			r.ConfigRevision, r.ProxyVersion)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			continue
		}
		failed := 0
		if r.ErrorType != "" {
			failed = 1
		}
		var reasoning int64
		if r.ReasoningTokens != nil {
			reasoning = *r.ReasoningTokens
		}
		var cost float64
		if r.CostUSD != nil {
			cost = *r.CostUSD
		}
		day := time.UnixMilli(r.CompletedAt).UTC().Format(time.DateOnly)
		if _, err := tx.Exec(upsertDaily, day, r.KeyName, r.Model, r.Target, r.Billing, failed, r.InputTokens, r.CacheReadTokens,
			r.CacheWriteTokens, r.OutputTokens, reasoning, cost); err != nil {
			return err
		}
		if r.Billing != domain.BillingPayPerUse || r.CostUSD == nil {
			continue
		}
		for _, scope := range [][2]string{{"global", ""}, {"key", r.KeyName}, {"model", r.Model}} {
			if _, err := tx.Exec(chargeNever, cost, scope[0], scope[1], r.ConfigRevision); err != nil {
				return err
			}
		}
	}
	return nil
}

// Restore seeds a's budget counters from the store before the listeners
// bind (RES-9): this month's pay_per_use spend from usage_daily, per day, for
// the instance, the key and the alias of each row, and every never budget's
// total from budget_totals. A day after today on now, which only a clock
// set back gives, counts as today, so no spend is left out. Records written
// to the critical log are in neither (phase-3 design section 17, point 15).
func Restore(ctx context.Context, db *sql.DB, a *admission.State, now time.Time) error {
	today := now.UTC().Format(time.DateOnly)
	rows, err := db.QueryContext(ctx, `SELECT key_name, model, day, SUM(cost_usd) FROM usage_daily
		WHERE billing = 'pay_per_use' AND day >= ? GROUP BY 1, 2, 3`, today[:8]+"01")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key, model, day string
		var usd float64
		if err := rows.Scan(&key, &model, &day, &usd); err != nil {
			return err
		}
		if len(day) != len(today) {
			return fmt.Errorf("usage_daily holds the day %q", day)
		}
		day = min(day, today)
		for _, scope := range []string{"global", "key:" + key, "model:" + model} {
			a.SeedSpend(scope, "d:"+day, usd)
			a.SeedSpend(scope, "m:"+day[:7], usd)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	totals, err := db.QueryContext(ctx, "SELECT scope_type, scope_name, since_revision, cost_usd FROM budget_totals")
	if err != nil {
		return err
	}
	defer func() { _ = totals.Close() }()
	for totals.Next() {
		var kind, name string
		var since int64
		var usd float64
		if err := totals.Scan(&kind, &name, &since, &usd); err != nil {
			return err
		}
		scope := kind + ":" + name
		if kind == "global" {
			scope = kind
		}
		a.SeedNever(scope, since, usd)
	}
	return totals.Err()
}

// Seeder is the seed of the quota windows from db, the store's read pool
// (phase-3 design section 9): the subscription records of a provider's
// targets (the ids from "<provider>/" up to "<provider>0") that completed at
// or after from, read on idx_usage_time; each event is sent at started_at and
// ended at completed_at with its input plus output tokens. A failure is
// logged, with the provider, and returned: admission then refuses the
// provider's targets until a seed succeeds.
func Seeder(db *sql.DB, log *slog.Logger) admission.Seed {
	return func(provider string, from time.Time) ([]admission.Event, error) {
		events, err := readEvents(db, provider, from)
		if err != nil {
			log.Error("quota windows not seeded", "provider", provider, "error", err.Error())
		}
		return events, err
	}
}

func readEvents(db *sql.DB, provider string, from time.Time) ([]admission.Event, error) {
	rows, err := db.Query(`SELECT key_name, started_at, completed_at, input_tokens + output_tokens FROM usage
		WHERE billing = 'subscription' AND completed_at >= ? AND target >= ? AND target < ?`, from.UnixMilli(), provider+"/", provider+"0")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var events []admission.Event
	for rows.Next() {
		var e admission.Event
		var sent, ended int64
		if err := rows.Scan(&e.Key, &sent, &ended, &e.Tokens); err != nil {
			return nil, err
		}
		e.Sent, e.Ended = time.UnixMilli(sent), time.UnixMilli(ended)
		events = append(events, e)
	}
	return events, rows.Err()
}

// null is s, or NULL when s is empty.
func null(s string) any {
	if s == "" {
		return nil
	}
	return s
}
