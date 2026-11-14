package usage

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
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
// aggregate moved only for a row inserted (USE-2). It is safe for concurrent
// use.
type Writer struct {
	write func(context.Context, func(*sql.Tx) error) error
	log   *slog.Logger

	mu      sync.RWMutex // held for reading while Add queues, so Close never closes the queue under it
	closed  bool
	queue   chan Record
	closing chan struct{}   // closed by Close
	ctx     context.Context // Close's: set before closing is closed, read only after
	done    chan struct{}   // closed when the goroutine has exited
	lost    atomic.Int64    // records logged instead of stored
	down    atomic.Bool     // the store is unavailable (Available)
}

// NewWriter starts the writer on write, the store's write path.
func NewWriter(write func(context.Context, func(*sql.Tx) error) error, log *slog.Logger) *Writer {
	w := &Writer{write: write, log: log, queue: make(chan Record, queueSize), closing: make(chan struct{}), done: make(chan struct{})}
	go w.run()
	return w
}

// Add queues r for the next batch. It never blocks: when the queue is full,
// or once Close has begun, r is written as one log line at critical level
// and counted instead, never lost silently and never rate-limited (USE-10,
// OPS-9).
func (w *Writer) Add(r Record) {
	w.mu.RLock()
	queued := false
	if !w.closed {
		select {
		case w.queue <- r:
			queued = true
		default:
		}
	}
	w.mu.RUnlock()
	if !queued {
		w.lose(r)
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
		batch := []Record{r}
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
func (w *Writer) commit(batch []Record) bool {
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

// lose writes each record as one log line at critical level and counts it.
func (w *Writer) lose(rs ...Record) {
	for _, r := range rs {
		w.lost.Add(1)
		w.log.Log(context.Background(), LevelCritical, "usage record not stored", "record", r)
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

// insert inserts each record or ignores it when its id is stored already;
// only an inserted row moves usage_daily, for the UTC day of completed_at
// (USE-2): one request, one error when error_type is set, each token count
// and the cost, a NULL counting as 0.
func insert(tx *sql.Tx, batch []Record) error {
	for _, r := range batch {
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
	}
	return nil
}

// null is s, or NULL when s is empty.
func null(s string) any {
	if s == "" {
		return nil
	}
	return s
}
