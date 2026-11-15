package telemetry

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jorlaru/stargaite/internal/config"
)

// The exporter's timings (phase-3 design section 10, TEL-12). Tests lower them.
var (
	batchEvery     = 2 * time.Second
	backoff        = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	retryAfterCap  = 30 * time.Second
	requestTimeout = 10 * time.Second
	warnEvery      = time.Minute
)

const (
	maxQueued      = 10_000   // spans (TEL-12)
	maxQueuedBytes = 32 << 20 // encoded bytes (TEL-12)
	batchSpans     = 200
	maxAnswer      = 4 << 20 // bytes of an answer read (TEL-1)
	maxMessage     = 200     // bytes of a backend's message kept
	warnAfter      = 10      // consecutive failed batches before the warning (TEL-3)
)

// The outcomes of telemetry test (TEL-2, kick-off section 3.2).
const (
	OutcomeCredentialUnresolved = "credential_unresolved"
	OutcomeUnreachable          = "unreachable"
	OutcomeUnauthorized         = "unauthorized"
	OutcomeNotFound             = "not_found"
	OutcomeRejected             = "rejected"
	OutcomeAccepted             = "accepted"
)

// TestKey is the reserved key a test span carries: no key name starts with "_".
const TestKey = "_telemetry"

// Result is what telemetry test answers.
type Result struct {
	Outcome string `json:"outcome"`
	Status  int    `json:"status,omitzero"`   // the HTTP status, when an answer came
	Message string `json:"message,omitzero"`  // the cause, or the backend's message, cut and redacted
	TraceID string `json:"trace_id,omitzero"` // of an accepted span, to look up
}

// Status is the exporter's counters, in spans, since the start (TEL-3).
type Status struct {
	Sent              int64         `json:"sent"`
	RejectedByStatus  map[int]int64 `json:"rejected_by_status"`  // by HTTP status
	RejectedByBackend int64         `json:"rejected_by_backend"` // partialSuccess.rejectedSpans
	Dropped           int64         `json:"dropped"`             // queue full, unreachable, credentials, unset or stopped
	Queued            int           `json:"queued"`
	LastError         string        `json:"last_error,omitzero"`
}

// Exporter sends spans in batches from a bounded queue, on one goroutine.
// Nothing it does ever waits on inference, nor inference on it (PRI-10).
type Exporter struct {
	cfg     func() *config.Exporter
	clients [2]*http.Client // by AllowPrivate
	version string
	log     *slog.Logger
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	ctx     context.Context // cancelled when Close's deadline ends
	cancel  context.CancelFunc

	mu       sync.Mutex
	queue    []jsontext.Value
	bytes    int
	status   Status
	failures int       // consecutive failed batches
	warnedAt time.Time // of the last warning
	closed   bool
}

// NewExporter starts the exporter. cfg is read per batch, so a publish
// changes the endpoint and the credentials without a restart; clients[1] is
// the client of an endpoint with allow_private_network, clients[0] the other
// (SEC-4 at dial time, redirects never followed).
func NewExporter(cfg func() *config.Exporter, clients [2]*http.Client, version string, log *slog.Logger) *Exporter {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Exporter{cfg: cfg, clients: clients, version: version, log: log, wake: make(chan struct{}, 1), stop: make(chan struct{}),
		done: make(chan struct{}), ctx: ctx, cancel: cancel, status: Status{RejectedByStatus: map[int]int64{}}}
	go e.run()
	return e
}

// Enqueue encodes spans and queues them. It never blocks: past 10,000 spans
// or 32 MB of encoded spans the oldest are dropped and counted (TEL-12).
func (e *Exporter) Enqueue(spans ...Span) {
	encoded := make([]jsontext.Value, len(spans))
	errs := make([]error, len(spans))
	for i, s := range spans {
		encoded[i], errs[i] = encode(s)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, data := range encoded {
		switch {
		case errs[i] != nil:
			e.status.Dropped++
			e.status.LastError = "a span was not encoded: " + errs[i].Error()
			continue
		case e.closed:
			e.status.Dropped++
			continue
		}
		e.queue = append(e.queue, data)
		e.bytes += len(data)
		for len(e.queue) > maxQueued || e.bytes > maxQueuedBytes {
			e.bytes -= len(e.queue[0])
			e.queue[0] = nil
			e.queue = e.queue[1:]
			e.status.Dropped++
		}
	}
	if len(e.queue) >= batchSpans {
		select {
		case e.wake <- struct{}{}:
		default:
		}
	}
}

// Status returns the counters and the spans queued.
func (e *Exporter) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.status
	s.RejectedByStatus = maps.Clone(e.status.RejectedByStatus)
	s.Queued = len(e.queue)
	return s
}

// Close sends what is queued, with retries, until ctx ends, and returns the
// spans it dropped meanwhile: those still queued and those of a batch cut short.
// Spans enqueued afterwards are dropped and counted.
func (e *Exporter) Close(ctx context.Context) int {
	e.mu.Lock()
	before := e.status.Dropped
	if !e.closed {
		close(e.stop)
	}
	e.closed = true
	e.mu.Unlock()
	select {
	case <-e.done:
	case <-ctx.Done():
		e.cancel()
		<-e.done
	}
	e.cancel()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status.Dropped += int64(len(e.queue))
	e.queue, e.bytes = nil, 0
	return int(e.status.Dropped - before)
}

// run sends a batch every batchEvery, or as soon as batchSpans are queued,
// until the queue is empty; on Close, it empties the queue and ends.
func (e *Exporter) run() {
	defer close(e.done)
	tick := time.NewTicker(batchEvery)
	defer tick.Stop()
	for {
		stopping := false
		select {
		case <-tick.C:
		case <-e.wake:
		case <-e.stop:
			stopping = true
		}
		for e.ctx.Err() == nil {
			e.mu.Lock()
			n := min(len(e.queue), batchSpans)
			batch := slices.Clone(e.queue[:n])
			for i := range n {
				e.bytes -= len(e.queue[i])
				e.queue[i] = nil
			}
			e.queue = e.queue[n:]
			e.mu.Unlock()
			if n == 0 {
				break
			}
			e.batch(batch)
		}
		if stopping {
			return
		}
	}
}

// batch sends one batch with the retries of TEL-12 and counts its outcome;
// one that Close's deadline cuts short is dropped.
func (e *Exporter) batch(spans []jsontext.Value) {
	c := e.cfg()
	out := sent{err: errors.New("no exporter is configured")}
	switch {
	case c == nil:
	case len(c.Unresolved) > 0:
		out.err = unresolved(c)
	default:
		out = e.send(e.ctx, c, spans, true)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx.Err() != nil && out.err != nil {
		e.status.Dropped += int64(len(spans))
		return
	}
	e.account(c, len(spans), out)
}

// unresolved is the error of an exporter whose credentials do not resolve.
func unresolved(c *config.Exporter) error {
	return fmt.Errorf("%s: %s does not resolve", OutcomeCredentialUnresolved, strings.Join(c.Unresolved, ", "))
}

// sent is the outcome of one export: the status and the answer's message, the
// spans the backend rejected inside a 2xx, or a failure with no usable answer;
// tooLarge marks an answer past maxAnswer, which is never retried.
type sent struct {
	status   int
	rejected int64
	message  string
	err      error
	tooLarge bool
}

// account counts n spans by out and logs the warning of TEL-3. e.mu is held.
func (e *Exporter) account(c *config.Exporter, n int, out sent) {
	failed := true
	switch {
	case out.err != nil && out.status == 0:
		e.status.Dropped += int64(n)
		e.status.LastError = redact(c, out.err.Error())
	case out.err != nil || out.status/100 != 2:
		e.status.RejectedByStatus[out.status] += int64(n)
		e.status.LastError = strings.TrimSuffix(fmt.Sprintf("%d %s: %s", out.status, http.StatusText(out.status), cause(out.message, out.err)), ": ")
	default:
		failed = false
		rejected := min(max(out.rejected, 0), int64(n))
		e.status.Sent += int64(n) - rejected
		e.status.RejectedByBackend += rejected
		if rejected > 0 {
			e.status.LastError = fmt.Sprintf("%d spans rejected by the backend: %s", rejected, out.message)
		}
	}
	if !failed {
		e.failures = 0
		return
	}
	e.failures++
	if e.failures >= warnAfter && time.Since(e.warnedAt) >= warnEvery {
		e.warnedAt = time.Now()
		e.log.Warn("telemetry export failing", "rule", "TEL-3", "consecutive_failures", e.failures, "last_error", e.status.LastError)
	}
}

// cause is the message of an answer, else its error.
func cause(message string, err error) string {
	if message == "" && err != nil {
		return err.Error()
	}
	return message
}

// send posts spans to c.URL. With retry, a transport failure and the statuses
// 429, 502, 503 and 504 are tried again after 1, 2 and 4 s with 20 % jitter,
// or after the answer's Retry-After up to retryAfterCap (TEL-12).
func (e *Exporter) send(ctx context.Context, c *config.Exporter, spans []jsontext.Value, retry bool) sent {
	data, err := body(e.version, spans)
	if err != nil {
		return sent{err: err}
	}
	client := e.clients[0]
	if c.AllowPrivate {
		client = e.clients[1]
	}
	for attempt := 0; ; attempt++ {
		out, wait := post(ctx, client, c, data)
		retryable := !out.tooLarge && (out.status == 0 && out.err != nil || slices.Contains([]int{429, 502, 503, 504}, out.status))
		if !retry || !retryable || attempt == len(backoff) || ctx.Err() != nil {
			return out
		}
		if wait == 0 {
			wait = time.Duration(float64(backoff[attempt]) * (0.8 + 0.4*rand.Float64()))
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return out
		}
	}
}

// post makes one request and reads at most maxAnswer bytes of the answer. It
// returns the Retry-After of the answer, capped, or 0.
func post(ctx context.Context, client *http.Client, c *config.Exporter, data []byte) (sent, time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(data))
	if err != nil {
		return sent{err: err}, 0
	}
	for name, values := range c.Header {
		req.Header[name] = slices.Clone(values)
	}
	for name, s := range c.Secrets {
		s.SetHeader(req.Header, name, "")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return sent{err: err}, 0
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	out := sent{status: resp.StatusCode}
	switch {
	case err != nil:
		out.err = fmt.Errorf("the answer was not read: %w", err)
	case len(answer) > maxAnswer:
		out.err, out.tooLarge = errors.New("the answer is larger than 4 MiB"), true
	case resp.StatusCode/100 == 2:
		out.rejected, out.message = partial(answer)
	default:
		out.message = string(answer)
	}
	out.message = redact(c, out.message)
	if out.err != nil {
		out.err = errors.New(redact(c, out.err.Error()))
	}
	return out, retryAfter(resp.Header.Get("Retry-After"))
}

// partial reads partialSuccess of an ExportTraceServiceResponse:
// rejectedSpans as a number or a decimal string, and errorMessage. Any other
// body, such as a backend's own, rejects nothing.
func partial(answer []byte) (int64, string) {
	var v struct {
		PartialSuccess struct {
			RejectedSpans jsontext.Value `json:"rejectedSpans"`
			ErrorMessage  string         `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if json.Unmarshal(answer, &v) != nil {
		return 0, ""
	}
	n, err := strconv.ParseInt(strings.Trim(string(v.PartialSuccess.RejectedSpans), `"`), 10, 64)
	if err != nil {
		return 0, ""
	}
	return n, v.PartialSuccess.ErrorMessage
}

// retryAfter reads a Retry-After in seconds or as an HTTP date, capped at
// retryAfterCap; 0 when absent, malformed or past.
func retryAfter(v string) time.Duration {
	var d time.Duration
	if s, err := strconv.ParseInt(v, 10, 64); err == nil && s > 0 {
		d = time.Duration(min(s, int64(retryAfterCap/time.Second)+1)) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
	}
	return min(max(d, 0), retryAfterCap)
}

// redact removes every credential of c from a backend's text, longest first
// as c.Redact holds them, then cuts it to maxMessage bytes at a UTF-8
// boundary: a secret is never cut in half first.
func redact(c *config.Exporter, text string) string {
	if c != nil {
		for _, s := range c.Redact {
			text = s.Redact(text)
		}
	}
	if len(text) > maxMessage {
		text = strings.ToValidUTF8(text[:maxMessage], "")
	}
	return text
}

// Test sends one span, "stargaite telemetry test" under the key TestKey,
// through the send function of the batches without retries, and names the
// outcome (TEL-2). It counts in Status like any span.
func (e *Exporter) Test(ctx context.Context) Result {
	c := e.cfg()
	if c == nil {
		return Result{Outcome: OutcomeUnreachable, Message: "no exporter is configured"}
	}
	s := Span{Name: "stargaite telemetry test", Kind: KindInternal, Start: time.Now(), Attrs: []Attr{
		{"stargaite.key", TestKey}, {"stargaite.schema.version", int64(1)}, {"stargaite.version", e.version},
		{"stargaite.config.revision", c.Revision},
	}}
	random(s.Trace[:])
	random(s.ID[:])
	s.End = s.Start
	var out sent
	if len(c.Unresolved) > 0 {
		out.err = unresolved(c)
	} else {
		data, err := encode(s)
		if err != nil {
			out.err = err
		} else {
			out = e.send(ctx, c, []jsontext.Value{data}, false)
		}
	}
	e.mu.Lock()
	e.account(c, 1, out)
	e.mu.Unlock()

	r := Result{Status: out.status, Message: cause(out.message, out.err)}
	switch {
	case len(c.Unresolved) > 0:
		r.Outcome = OutcomeCredentialUnresolved
	case out.status == 0:
		r.Outcome = OutcomeUnreachable
	case out.status == http.StatusUnauthorized || out.status == http.StatusForbidden:
		r.Outcome = OutcomeUnauthorized
	case out.status == http.StatusNotFound:
		r.Outcome = OutcomeNotFound
	case out.err != nil || out.status/100 != 2 || out.rejected > 0:
		r.Outcome = OutcomeRejected
	default:
		r.Outcome, r.Message, r.TraceID = OutcomeAccepted, "", hex.EncodeToString(s.Trace[:])
	}
	return r
}
