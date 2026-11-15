package telemetry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/transport"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// lower sets the exporter's timings to milliseconds for one test.
func lower(t *testing.T) {
	t.Helper()
	saved := []any{batchEvery, backoff, retryAfterCap, requestTimeout, warnEvery}
	batchEvery, backoff, retryAfterCap, requestTimeout, warnEvery = 10*time.Millisecond, []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond},
		300*time.Millisecond, 500*time.Millisecond, time.Hour
	t.Cleanup(func() {
		batchEvery, backoff, retryAfterCap = saved[0].(time.Duration), saved[1].([]time.Duration), saved[2].(time.Duration)
		requestTimeout, warnEvery = saved[3].(time.Duration), saved[4].(time.Duration)
	})
}

// logBuffer is a log the exporter's goroutine writes to.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// resolver answers every name with addr, as a name that resolves elsewhere
// after it was validated does.
func resolver(addr string) transport.Resolver {
	return func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr(addr)}, nil
	}
}

// receiver is a second mock provider serving OTLP exports on loopback.
func receiver(t *testing.T) (*mockprovider.Server, string) {
	t.Helper()
	m := mockprovider.New()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return m, srv.URL
}

// start is an exporter of *c, read per batch, through clients that resolve
// every name to loopback; it is closed at the end of the test.
func start(t *testing.T, c **config.Exporter) (*Exporter, *logBuffer) {
	t.Helper()
	log := &logBuffer{}
	var mu sync.Mutex
	e := NewExporter(func() *config.Exporter { mu.Lock(); defer mu.Unlock(); return *c },
		[2]*http.Client{transport.NewClient(false, resolver("127.0.0.1")), transport.NewClient(true, resolver("127.0.0.1"))}, "test", slog.New(slog.NewJSONHandler(log, nil)))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		e.Close(ctx)
	})
	return e, log
}

func local(url string) *config.Exporter {
	return &config.Exporter{URL: url + "/v1/traces", Header: http.Header{}, Secrets: map[string]transport.Secret{}, AllowPrivate: true, Revision: 7}
}

// until polls cond for up to 5 s.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 5 s", what)
		}
	}
}

// exported decodes the spans of one export call.
func exported(t *testing.T, c mockprovider.Call) []map[string]any {
	t.Helper()
	var req struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []map[string]any
			}
			ScopeSpans []struct {
				Scope struct{ Name, Version string }
				Spans []map[string]any
			}
		}
	}
	if err := json.Unmarshal(c.Body, &req); err != nil || len(req.ResourceSpans) != 1 || len(req.ResourceSpans[0].ScopeSpans) != 1 {
		t.Fatalf("export body %s: %v", c.Body, err)
	}
	if s := req.ResourceSpans[0].ScopeSpans[0].Scope; s.Name != "stargaite" || s.Version != "test" {
		t.Errorf("scope %+v", s)
	}
	return req.ResourceSpans[0].ScopeSpans[0].Spans
}

// TestTest walks every outcome of telemetry test (AC-24) against a second
// mock provider as receiver, each without retries, with what Status counts.
func TestTest(t *testing.T) {
	lower(t)
	m, base := receiver(t)
	big := bytes.Repeat([]byte("x"), maxAnswer+1)
	for _, tt := range []struct {
		name      string
		step      *mockprovider.Step
		mutate    func(c *config.Exporter)
		outcome   string
		status    int
		message   string // a part of the message
		calls     int
		counted   func(s Status) bool
		lastError string // a part of it
	}{
		{"accepted with 200 {}", &mockprovider.Step{Body: []byte(`{}`)}, nil, OutcomeAccepted, 200, "", 1,
			func(s Status) bool { return s.Sent == 1 }, ""},
		{"accepted with a backend's own body", &mockprovider.Step{Body: []byte(`{"status":"ok"}`)}, nil, OutcomeAccepted, 200, "", 1,
			func(s Status) bool { return s.Sent == 1 }, ""},
		{"accepted with an empty 202", &mockprovider.Step{Status: 202}, nil, OutcomeAccepted, 202, "", 1,
			func(s Status) bool { return s.Sent == 1 }, ""},
		{"accepted with partialSuccess rejecting none", &mockprovider.Step{Body: []byte(`{"partialSuccess":{}}`)}, nil, OutcomeAccepted, 200, "", 1,
			func(s Status) bool { return s.Sent == 1 }, ""},
		{"rejected inside a 200, as a number", &mockprovider.Step{Body: []byte(`{"partialSuccess":{"rejectedSpans":1,"errorMessage":"bad span"}}`)}, nil,
			OutcomeRejected, 200, "bad span", 1, func(s Status) bool { return s.RejectedByBackend == 1 && s.Sent == 0 }, "1 spans rejected by the backend: bad span"},
		{"rejected inside a 200, as a decimal string", &mockprovider.Step{Body: []byte(`{"partialSuccess":{"rejectedSpans":"1","errorMessage":"too old"}}`)}, nil,
			OutcomeRejected, 200, "too old", 1, func(s Status) bool { return s.RejectedByBackend == 1 }, "too old"},
		{"unauthorized on 401", &mockprovider.Step{Status: 401, Body: []byte(`{"error":"invalid credentials"}`)}, nil, OutcomeUnauthorized, 401, "invalid credentials", 1,
			func(s Status) bool { return s.RejectedByStatus[401] == 1 }, "401 Unauthorized: {\"error\":\"invalid credentials\"}"},
		{"unauthorized on 403", &mockprovider.Step{Status: 403}, nil, OutcomeUnauthorized, 403, "", 1,
			func(s Status) bool { return s.RejectedByStatus[403] == 1 }, "403 Forbidden"},
		{"not_found on 404", &mockprovider.Step{Status: 404, Body: []byte("no such path")}, nil, OutcomeNotFound, 404, "no such path", 1,
			func(s Status) bool { return s.RejectedByStatus[404] == 1 }, "404"},
		{"rejected on 400", &mockprovider.Step{Status: 400, Body: []byte(`{"error":"bad"}`)}, nil, OutcomeRejected, 400, "bad", 1,
			func(s Status) bool { return s.RejectedByStatus[400] == 1 }, "400"},
		{"rejected on 500", &mockprovider.Step{Status: 500}, nil, OutcomeRejected, 500, "", 1,
			func(s Status) bool { return s.RejectedByStatus[500] == 1 }, "500 Internal Server Error"},
		{"rejected on 503, not retried by a test", &mockprovider.Step{Status: 503, Header: http.Header{"Retry-After": {"0"}}}, nil, OutcomeRejected, 503, "", 1,
			func(s Status) bool { return s.RejectedByStatus[503] == 1 }, "503"},
		{"rejected on a 302, never followed (AC-43)", &mockprovider.Step{Status: 302, Header: http.Header{"Location": {base + "/elsewhere"}}}, nil, OutcomeRejected, 302, "", 1,
			func(s Status) bool { return s.RejectedByStatus[302] == 1 }, "302"},
		{"rejected past 4 MiB of answer", &mockprovider.Step{Body: big}, nil, OutcomeRejected, 200, "larger than 4 MiB", 1,
			func(s Status) bool { return s.RejectedByStatus[200] == 1 && s.Sent == 0 }, "larger than 4 MiB"},
		{"unreachable past the request timeout", &mockprovider.Step{Delay: time.Second}, nil, OutcomeUnreachable, 0, "deadline", 1,
			func(s Status) bool { return s.Dropped == 1 }, "deadline"},
		{"unreachable with nothing listening", nil, func(c *config.Exporter) { c.URL = "http://127.0.0.1:1/v1/traces" }, OutcomeUnreachable, 0, "refused", 0,
			func(s Status) bool { return s.Dropped == 1 }, "refused"},
		{"unreachable: a name that resolves private after validation (AC-43)", nil, func(c *config.Exporter) {
			c.URL, c.AllowPrivate = strings.Replace(c.URL, "127.0.0.1", "otel.example.com", 1), false
		}, OutcomeUnreachable, 0, "private", 0, func(s Status) bool { return s.Dropped == 1 }, "allow_private_network is not set"},
		{"credential_unresolved, without sending", nil, func(c *config.Exporter) { c.Unresolved = []string{"OTLP_PASSWORD", "OTLP_USER"} },
			OutcomeCredentialUnresolved, 0, "OTLP_PASSWORD, OTLP_USER does not resolve", 0,
			func(s Status) bool { return s.Dropped == 1 }, "credential_unresolved: OTLP_PASSWORD, OTLP_USER does not resolve"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := local(base)
			if tt.mutate != nil {
				tt.mutate(c)
			}
			e, _ := start(t, &c)
			if tt.step != nil {
				m.Script(*tt.step)
			}
			before := len(m.Calls())
			r := e.Test(context.Background())
			if r.Outcome != tt.outcome || r.Status != tt.status || !strings.Contains(r.Message, tt.message) || (r.Outcome == OutcomeAccepted) != traceID.MatchString(r.TraceID) {
				t.Errorf("result %+v", r)
			}
			if n := len(m.Calls()) - before; n != tt.calls {
				t.Errorf("%d calls, want %d", n, tt.calls)
			}
			if s := e.Status(); !tt.counted(s) || !strings.Contains(s.LastError, tt.lastError) || (tt.lastError == "") != (s.LastError == "") || len(s.LastError) > maxMessage+40 {
				t.Errorf("status %+v", s)
			}
		})
	}

	// The test span, as the receiver got it: the reserved key, the version
	// and the revision, and the trace id the result names.
	c := local(base)
	e, _ := start(t, &c)
	r := e.Test(context.Background())
	calls := m.Calls()
	last := calls[len(calls)-1]
	spans := exported(t, last)
	attrs := fmt.Sprint(spans[0]["attributes"])
	if len(spans) != 1 || spans[0]["name"] != "stargaite telemetry test" || spans[0]["traceId"] != r.TraceID || spans[0]["kind"] != 1.0 ||
		!strings.Contains(attrs, "map[key:stargaite.key value:map[stringValue:_telemetry]]") ||
		!strings.Contains(attrs, "map[key:stargaite.config.revision value:map[intValue:7]]") || last.Path != "/v1/traces" || last.Method != http.MethodPost {
		t.Errorf("test span %v at %s %s", spans, last.Method, last.Path)
	}
	for _, c := range calls {
		if c.Path == "/elsewhere" {
			t.Errorf("the 302 was followed")
		}
	}
}

var traceID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TestHeaders: the receiver gets Content-Type application/json, the static
// headers and each secret, and a backend's echo of a credential, or of a
// part of it, is redacted from the message and from Status (SEC-5).
func TestHeaders(t *testing.T) {
	lower(t)
	m, base := receiver(t)
	c := local(base)
	encoded := base64.StdEncoding.EncodeToString([]byte("pk-user:sk-password-1"))
	c.Header.Set("x-langfuse-ingestion-version", "4")
	c.Header.Set("Content-Type", "text/plain") // the exporter's own type wins
	c.Secrets["Authorization"] = transport.NewSecret("Basic " + encoded)
	c.Secrets["X-Token"] = transport.NewSecret("tok-123")
	c.Redact = []transport.Secret{transport.NewSecret("Basic " + encoded), transport.NewSecret(encoded), transport.NewSecret("sk-password-1"),
		transport.NewSecret("tok-123"), transport.NewSecret("pk-user")} // every secret value, longest first, as config composes it
	e, log := start(t, &c)
	echo := "Basic " + encoded + " / " + encoded + " / pk-user / sk-password-1 / tok-123 / " + strings.Repeat("é", 200)
	m.Script(mockprovider.Step{Status: 401, Body: []byte(echo)})
	r := e.Test(context.Background())
	h := m.Calls()[0].Header
	if h.Get("Authorization") != "Basic "+encoded || h.Get("X-Token") != "tok-123" || h.Get("X-Langfuse-Ingestion-Version") != "4" ||
		h.Get("Content-Type") != "application/json" {
		t.Errorf("header %v", h)
	}
	s := e.Status()
	for _, text := range []string{r.Message, s.LastError, log.String(), fmt.Sprintf("%+v %#v", c, c)} {
		for _, secret := range []string{encoded, "pk-user", "sk-password-1", "tok-123"} {
			if strings.Contains(text, secret) {
				t.Errorf("%q holds %q", text, secret)
			}
		}
	}
	if !strings.HasPrefix(r.Message, "[redacted] / [redacted] / [redacted] / [redacted] / [redacted] / é") || len(r.Message) > maxMessage || !strings.HasSuffix(r.Message, "é") {
		t.Errorf("message %q (%d bytes)", r.Message, len(r.Message))
	}
}

// TestBatches: spans go in batches, every batchEvery or at batchSpans; only
// 429, 502, 503, 504 and transport failures are retried, three times, and a
// Retry-After in seconds or as an HTTP date is honoured up to the cap
// (TEL-12); other statuses reject the batch at once.
func TestBatches(t *testing.T) {
	lower(t)
	span := Span{Name: "s", Kind: KindServer, Start: time.Unix(1, 0), End: time.Unix(2, 0)}
	for _, tt := range []struct {
		name    string
		steps   []mockprovider.Step
		calls   int
		counted func(s Status) bool
		gap     time.Duration // at least, between the first two calls
	}{
		{"accepted", nil, 1, func(s Status) bool { return s.Sent == 2 }, 0},
		{"429, 502, 503, 504 retried, then accepted", []mockprovider.Step{{Status: 429}, {Status: 502}, {Status: 503}, {Status: 200}}, 4,
			func(s Status) bool { return s.Sent == 2 && len(s.RejectedByStatus) == 0 }, 0},
		{"504 three times retried, then rejected", []mockprovider.Step{{Status: 504}, {Status: 504}, {Status: 504}, {Status: 504}}, 4,
			func(s Status) bool { return s.RejectedByStatus[504] == 2 && s.Sent == 0 }, 0},
		{"503 past 4 MiB of answer not retried", []mockprovider.Step{{Status: 503, Body: bytes.Repeat([]byte("x"), maxAnswer+1)}}, 1,
			func(s Status) bool { return s.RejectedByStatus[503] == 2 && s.Sent == 0 }, 0},
		{"503 whose answer is cut retried, then accepted", []mockprovider.Step{{Status: 503, Body: []byte("xxxxx"), Cut: true}, {}}, 2,
			func(s Status) bool { return s.Sent == 2 && len(s.RejectedByStatus) == 0 }, 0},
		{"500 not retried", []mockprovider.Step{{Status: 500}}, 1, func(s Status) bool { return s.RejectedByStatus[500] == 2 }, 0},
		{"400 not retried", []mockprovider.Step{{Status: 400}}, 1, func(s Status) bool { return s.RejectedByStatus[400] == 2 }, 0},
		{"401 not retried", []mockprovider.Step{{Status: 401}}, 1, func(s Status) bool { return s.RejectedByStatus[401] == 2 }, 0},
		{"a transport failure retried", []mockprovider.Step{{Delay: time.Second}, {}}, 2, func(s Status) bool { return s.Sent == 2 }, 0},
		{"partialSuccess counts the rejected", []mockprovider.Step{{Body: []byte(`{"partialSuccess":{"rejectedSpans":"1","errorMessage":"one bad"}}`)}}, 1,
			func(s Status) bool { return s.Sent == 1 && s.RejectedByBackend == 1 }, 0},
		{"a rejected count above the batch is the batch", []mockprovider.Step{{Body: []byte(`{"partialSuccess":{"rejectedSpans":99}}`)}}, 1,
			func(s Status) bool { return s.Sent == 0 && s.RejectedByBackend == 2 }, 0},
		{"Retry-After in seconds, capped", []mockprovider.Step{{Status: 503, Header: http.Header{"Retry-After": {"120"}}}, {}}, 2,
			func(s Status) bool { return s.Sent == 2 }, 300 * time.Millisecond},
		{"Retry-After as an HTTP date, capped", []mockprovider.Step{{Status: 429, Header: http.Header{"Retry-After": {time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)}}}, {}}, 2,
			func(s Status) bool { return s.Sent == 2 }, 300 * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, base := receiver(t)
			m.Script(tt.steps...)
			c := local(base)
			e, _ := start(t, &c)
			e.Enqueue(span, span)
			until(t, "the outcome", func() bool { return tt.counted(e.Status()) })
			time.Sleep(20 * time.Millisecond) // no further call comes
			calls := m.Calls()
			if len(calls) != tt.calls {
				t.Fatalf("%d calls, want %d", len(calls), tt.calls)
			}
			if spans := exported(t, calls[0]); len(spans) != 2 {
				t.Errorf("%d spans in the batch", len(spans))
			}
			if tt.gap > 0 {
				if gap := calls[1].Ended.Sub(calls[0].Ended); gap < tt.gap || gap > tt.gap+time.Second {
					t.Errorf("gap %v, want %v", gap, tt.gap)
				}
			}
			if s := e.Status(); s.Queued != 0 {
				t.Errorf("status %+v", s)
			}
		})
	}
}

// TestQueue: Enqueue never blocks; past 10,000 spans or 32 MB of encoded
// spans the oldest are dropped and counted (TEL-12); batchSpans queued send
// a batch at once; Close sends what is queued, and drops a span enqueued
// after it.
func TestQueue(t *testing.T) {
	lower(t)
	batchEvery = time.Hour
	m, base := receiver(t)
	c := local(base)

	// A batch of batchSpans goes at once, without waiting for batchEvery.
	e, _ := start(t, &c)
	for range batchSpans {
		e.Enqueue(Span{Name: "s"})
	}
	until(t, "the batch", func() bool { return e.Status().Sent == batchSpans })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.Enqueue(Span{Name: "last"})
	if n := e.Close(ctx); n != 0 || e.Status().Sent != batchSpans+1 || len(m.Calls()) != 2 {
		t.Errorf("Close dropped %d; status %+v, %d calls", n, e.Status(), len(m.Calls()))
	}
	e.Enqueue(Span{Name: "late"})
	if s := e.Status(); s.Dropped != 1 || s.Queued != 0 {
		t.Errorf("a span after Close: %+v", s)
	}

	// The bound in spans, while the receiver holds the first batch.
	m.Script(mockprovider.Step{Delay: time.Hour})
	requestTimeout = time.Hour
	e1, _ := start(t, &c)
	began := time.Now()
	for i := range batchSpans + maxQueued + 3 {
		e1.Enqueue(Span{Name: fmt.Sprint(i)})
	}
	until(t, "the first batch in flight", func() bool { return len(m.Calls()) == 3 })
	e1.mu.Lock()
	queued, oldest := len(e1.queue), string(e1.queue[0])
	e1.mu.Unlock()
	if s := e1.Status(); s.Dropped != 3 || queued != maxQueued || !strings.Contains(oldest, `"name":"203"`) || time.Since(began) > 5*time.Second {
		t.Errorf("status %+v, %d queued, the oldest %s, after %v", s, queued, oldest, time.Since(began))
	}
	short, cancelShort := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShort()
	if n := e1.Close(short); n != batchSpans+maxQueued {
		t.Errorf("Close dropped %d", n)
	}
	requestTimeout = 500 * time.Millisecond

	// The bound in bytes: spans of 1 MiB each.
	e2, _ := start(t, &c)
	huge := Span{Name: "huge", Attrs: []Attr{{"x", strings.Repeat("a", 1<<20)}}}
	for range 40 {
		e2.Enqueue(huge)
	}
	e2.mu.Lock()
	queued, size := len(e2.queue), e2.bytes
	e2.mu.Unlock()
	if s := e2.Status(); queued < 30 || queued > 31 || size > maxQueuedBytes || s.Dropped != int64(40-queued) {
		t.Errorf("%d spans, %d bytes queued; status %+v", queued, size, s)
	}
}

// TestClose: Close drops what it cannot send by its deadline and says how
// many; a receiver that hangs cannot hold the stop.
func TestClose(t *testing.T) {
	lower(t)
	batchEvery = time.Hour
	m, base := receiver(t)
	m.Script(mockprovider.Step{Delay: time.Hour})
	requestTimeout = time.Hour
	c := local(base)
	e, _ := start(t, &c)
	for range 3 {
		e.Enqueue(Span{Name: "s"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	began := time.Now()
	if n := e.Close(ctx); n != 3 || time.Since(began) > 2*time.Second {
		t.Errorf("Close dropped %d after %v", n, time.Since(began))
	}
	if s := e.Status(); s.Dropped != 3 || s.Queued != 0 || s.Sent != 0 {
		t.Errorf("status %+v", s)
	}
}

// TestUnsetAndCredentials: queued spans are dropped and counted when the
// exporter is unset or its credentials do not resolve, and nothing is sent;
// ten failed batches in a row log one warning, rate-limited (TEL-3).
func TestUnsetAndCredentials(t *testing.T) {
	lower(t)
	m, base := receiver(t)
	c := local(base)
	c.Unresolved = []string{"OTLP_TOKEN"}
	e, log := start(t, &c)
	for range warnAfter + 2 {
		e.Enqueue(Span{Name: "s"})
		until(t, "a batch", func() bool { return e.Status().Queued == 0 })
	}
	until(t, "the drops", func() bool { return e.Status().Dropped == warnAfter+2 })
	if s := e.Status(); s.LastError != "credential_unresolved: OTLP_TOKEN does not resolve" || len(m.Calls()) != 0 {
		t.Errorf("status %+v, %d calls", s, len(m.Calls()))
	}
	if n := strings.Count(log.String(), `"msg":"telemetry export failing"`); n != 1 || !strings.Contains(log.String(), `"consecutive_failures":10`) {
		t.Errorf("log %s", log)
	}

	var none *config.Exporter
	e2, _ := start(t, &none)
	e2.Enqueue(Span{Name: "s"})
	until(t, "the drop", func() bool { return e2.Status().Dropped == 1 })
	if r := e2.Test(context.Background()); r.Outcome != OutcomeUnreachable || r.Message != "no exporter is configured" {
		t.Errorf("Test without an exporter: %+v", r)
	}
}

// TestConfigPerBatch: the exporter reads the published configuration per
// batch, so a change of endpoint applies without a restart.
func TestConfigPerBatch(t *testing.T) {
	lower(t)
	first, base1 := receiver(t)
	second, base2 := receiver(t)
	c := local(base1)
	var mu sync.Mutex
	e := NewExporter(func() *config.Exporter { mu.Lock(); defer mu.Unlock(); return c },
		[2]*http.Client{transport.NewClient(false, resolver("127.0.0.1")), transport.NewClient(true, resolver("127.0.0.1"))}, "test", slog.New(slog.DiscardHandler))
	defer e.Close(context.Background())
	e.Enqueue(Span{Name: "a"})
	until(t, "the first export", func() bool { return len(first.Calls()) == 1 })
	mu.Lock()
	c = local(base2)
	mu.Unlock()
	e.Enqueue(Span{Name: "b"})
	until(t, "the second export", func() bool { return len(second.Calls()) == 1 })
	if len(first.Calls()) != 1 || second.Calls()[0].Header.Get("Content-Type") != "application/json" {
		t.Errorf("calls %d and %d", len(first.Calls()), len(second.Calls()))
	}
}
