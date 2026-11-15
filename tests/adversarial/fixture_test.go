// Package adversarial is the adversarial suite of AC-51 (phase-4 design
// section 9): the data plane (api.Handler) and the admin listener
// (admin.Handler) in process behind httptest, the mock as provider, and
// injected resolvers for the SSRF cases. Each test is one row of the
// design's table, and so one row of the kick-off section 21 adversarial
// list, and names its rules; a request case runs on every inference
// endpoint it applies to. The cases that depend on serve's server options
// are TestServeAdversarial in cmd/stargaite. It dials loopback only and
// resolves no name.
package adversarial

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/admin"
	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/api"
	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/store"
	"github.com/jorlaru/stargaite/internal/telemetry"
	"github.com/jorlaru/stargaite/internal/transport"
	"github.com/jorlaru/stargaite/internal/usage"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

const (
	adminKey     = "adversarial-admin-key-of-at-least-32-characters"
	metricsToken = "adversarial-metrics-token-of-at-least-32-characters"
	// planted is the start of every credential value: a reference resolves
	// to planted plus its name, and none may leave the proxy (SEC-5).
	planted    = "sk-PLANTED-credential-of-"
	credential = planted + "MOCK_KEY"
	testNow    = 1_790_000_000_000
	maxBody    = 65536 // settings.max_body_bytes
	maxAnswer  = 4096  // settings.max_response_bytes
)

// suiteConfig: on the mock, a chat, an embeddings and a rerank target of the
// openai protocol, an anthropic chat target, a target without tools, two
// that declare inline_think_tags, a rerank target whose provider has no
// rerank_url, a priced pay_per_use target and a subscription of 5 requests
// an hour; on rebound.test, which set time resolves public and the dial
// finds on loopback, a chat, an embeddings and a rerank target. agent may
// use every alias; one has one parallel slot; budget a daily budget of 0.05
// USD, which two calls to paid cross; plan room for 50 requests at once.
const suiteConfig = `version: 1
settings: { max_body_bytes: 65536, max_response_bytes: 4096, timeouts: { first_byte_ms: 2000, stream_idle_ms: 2000 }, telemetry: { otlp_endpoint: RECEIVERURL, allow_private_network: true } }
providers:
  mock: { protocol: openai, base_url: MOCKURL, rerank_url: MOCKURL/rerank, api_key_env: MOCK_KEY, billing: none, allow_private_network: true }
  anth: { protocol: anthropic, base_url: MOCKURL, api_key_env: MOCK_KEY, billing: none, allow_private_network: true }
  bare: { protocol: openai, base_url: MOCKURL, api_key_env: MOCK_KEY, billing: none, allow_private_network: true }
  pay: { protocol: openai, base_url: MOCKURL, api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true }
  sub: { protocol: openai, base_url: MOCKURL, api_key_env: MOCK_KEY, billing: subscription, allow_private_network: true, quota: [{ window: 1h, requests: 5 }] }
  rebound: { protocol: openai, base_url: "https://rebound.test:REBOUNDPORT/v1", rerank_url: "https://rebound.test:REBOUNDPORT/rerank", api_key_env: MOCK_KEY, billing: none }
targets:
  mock/chat: { kind: chat }
  mock/notools: { kind: chat, capabilities: { tools: false } }
  mock/think: { kind: chat, inline_think_tags: true }
  mock/emb: { kind: embeddings }
  mock/rr: { kind: rerank }
  bare/rr: { kind: rerank }
  anth/claude: { kind: chat }
  anth/think: { kind: chat, inline_think_tags: true }
  pay/m: { kind: chat, pricing: { input_per_million: 10000, output_per_million: 10000, as_of: "2026-11-01" } }
  sub/m: { kind: chat }
  rebound/chat: { kind: chat }
  rebound/emb: { kind: embeddings }
  rebound/rr: { kind: rerank }
models:
  chat: { targets: [mock/chat] }
  notools: { targets: [mock/notools] }
  think: { targets: [mock/think] }
  embed: { targets: [mock/emb] }
  rerank: { targets: [mock/rr] }
  nourl: { targets: [bare/rr] }
  claude: { targets: [anth/claude], max_output_tokens: 500 }
  claudethink: { targets: [anth/think], max_output_tokens: 500 }
  paid: { targets: [pay/m] }
  plan: { targets: [sub/m] }
  rebound: { targets: [rebound/chat] }
  reboundemb: { targets: [rebound/emb] }
  reboundrr: { targets: [rebound/rr] }
keys:
  agent: { models: ["*"] }
  one: { models: ["*"], max_parallel_requests: 1 }
  revoked: { models: ["*"] }
  budget: { models: [paid], budget: { limit_usd: 0.05, reset_interval: daily }, max_parallel_requests: 5 }
  plan: { models: [plan], max_parallel_requests: 100 }
`

// callCost is what one call to paid costs: the mock's 1 input and 2 output
// tokens at 10,000 USD per million.
const callCost = 0.03

// logBuffer is a log destination that concurrent requests share.
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

// fixture is both listeners on one configuration service, on a clock the
// test moves.
type fixture struct {
	t                 *testing.T
	data, admin       string       // the listeners' base URLs
	handler           http.Handler // the data plane, for in-process requests
	mock, receiver    *mockprovider.Server
	mockURL           string
	log               *logBuffer
	now               *atomic.Int64 // Unix milliseconds
	st                *store.Store
	flight            *sync.WaitGroup
	counters, spanned *usage.Counters
	limits            *admission.State
	records           *usage.Writer
	spans             *telemetry.Exporter
	lookups           atomic.Int64 // dial-time lookups of rebound.test
	accepted          atomic.Int64 // connections the loopback listener behind rebound.test accepted
	tokens            map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, log: &logBuffer{}, now: &atomic.Int64{}, flight: &sync.WaitGroup{}, counters: &usage.Counters{}, spanned: &usage.Counters{},
		mock: mockprovider.New(), receiver: mockprovider.New()}
	f.now.Store(testNow)
	st, err := store.Open(context.Background(), t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f.st = st
	// Set time: localhost is loopback, every other name a public address.
	env := func() config.Env {
		return config.Env{
			Lookup: func(name string) (string, bool) { return planted + name, true },
			Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
				if host == "localhost" {
					return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
				}
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			},
		}
	}
	cfg, err := config.Open(context.Background(), st, env, func() time.Time { return time.UnixMilli(f.now.Load()) })
	if err != nil {
		t.Fatal(err)
	}
	// Dial time: no name resolves but rebound.test, now on loopback.
	dial := func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "rebound.test" {
			f.lookups.Add(1)
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return nil, errors.New("no name is resolved in tests")
	}
	logger := slog.New(slog.NewJSONHandler(f.log, nil))
	f.records, f.limits = usage.NewWriter(st.Write, logger), admission.New(cfg.Now, usage.Seeder(st.Read(), logger))
	t.Cleanup(func() { f.records.Close(context.Background()) }) // before the store closes
	clients := [2]*http.Client{transport.NewClient(false, dial), transport.NewClient(true, dial)}
	rsrv := httptest.NewServer(f.receiver)
	t.Cleanup(rsrv.Close)
	f.spans = telemetry.NewExporter(func() *config.Exporter { return cfg.Snapshot().Exporter }, clients, "test-version", logger)
	t.Cleanup(func() { // before the receiver closes
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f.spans.Close(ctx)
	})
	d := api.Deps{Config: cfg, Log: logger, Clients: clients, Flight: f.flight, Usage: f.records, Admission: f.limits, Counters: f.counters,
		Spanned: f.spanned, Spans: f.spans, Version: "test-version"}
	hash := sha256.Sum256([]byte(metricsToken))
	adminSrv := httptest.NewServer(admin.Handler(admin.Deps{Config: cfg, KeyHash: sha256.Sum256([]byte(adminKey)), Log: logger, Usage: st.Read(),
		Admission: f.limits, Spans: f.spans, Counters: f.counters, Spanned: f.spanned, MetricsHash: &hash, Backup: st.Backup, Verify: api.Verify(d)}))
	t.Cleanup(adminSrv.Close)
	f.handler = api.Handler(d)
	dataSrv := httptest.NewServer(f.handler)
	t.Cleanup(dataSrv.Close)
	mockSrv := httptest.NewServer(f.mock)
	t.Cleanup(mockSrv.Close)
	f.data, f.admin, f.mockURL = dataSrv.URL, adminSrv.URL, mockSrv.URL

	// The private address behind rebound.test: a listener that counts.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.accepted.Add(1)
			_ = conn.Close()
		}
	}()
	yaml := strings.NewReplacer("MOCKURL", mockSrv.URL, "RECEIVERURL", rsrv.URL, "REBOUNDPORT", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)).Replace(suiteConfig)
	res := f.adminCall(http.MethodPost, "/admin/v1/apply", yaml, adminKey)
	var applied config.Result
	if res.status != http.StatusOK || json.Unmarshal([]byte(res.body), &applied) != nil {
		t.Fatalf("apply: %d %s", res.status, res.body)
	}
	f.tokens = applied.Secrets
	return f
}

// answer is a response as the tests read it: the status, the headers, the
// body and the error code of either envelope (the Anthropic one carries it
// in X-Stargaite-Error-Code).
type answer struct {
	status int
	header http.Header
	body   string
	code   string
}

var client = &http.Client{Timeout: 20 * time.Second}

// do sends one request and reads the whole answer; header holds "Name",
// "value" pairs, each added once. A failed request is the zero answer.
func (f *fixture) do(method, url string, body io.Reader, header ...string) answer {
	f.t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		f.t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Add(header[i], header[i+1])
	}
	resp, err := client.Do(req)
	if err != nil {
		f.t.Errorf("%s %s: %v", method, url, err) // not Fatal: concurrent requests call it
		return answer{}
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Errorf("%s %s: %v after %q", method, url, err, b)
	}
	return answer{resp.StatusCode, resp.Header, string(b), codeOf(resp.Header, b)}
}

func codeOf(h http.Header, body []byte) string {
	if c := h.Get("X-Stargaite-Error-Code"); c != "" {
		return c
	}
	var env struct{ Error struct{ Code string } }
	_ = json.Unmarshal(body, &env)
	return env.Error.Code
}

// post sends body to the data plane's path with the key's token.
func (f *fixture) post(path, key, body string, header ...string) answer {
	f.t.Helper()
	return f.do(http.MethodPost, f.data+path, strings.NewReader(body), append([]string{"Authorization", "Bearer " + f.tokens[key]}, header...)...)
}

// adminCall sends body to the admin listener with token.
func (f *fixture) adminCall(method, path, body, token string) answer {
	f.t.Helper()
	return f.do(method, f.admin+path, strings.NewReader(body), "Authorization", "Bearer "+token)
}

// calls is the number of provider calls the mock received.
func (f *fixture) calls() int { return len(f.mock.Calls()) }

// idle waits until no data-plane request is in flight: every finish and
// done has run, so every slot is back.
func (f *fixture) idle() {
	f.t.Helper()
	done := make(chan struct{})
	go func() { f.flight.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		f.t.Fatal("requests still in flight")
	}
}

// stored flushes the usage writer, which then takes no more records, and
// returns the number of rows stored for the request id.
func (f *fixture) stored(requestID string) int {
	f.t.Helper()
	f.idle()
	if lost := f.records.Close(context.Background()); lost != 0 {
		f.t.Fatalf("%d records lost", lost)
	}
	var n int
	if err := f.st.Read().QueryRow("SELECT COUNT(*) FROM usage WHERE request_id = ?", requestID).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// exported stops the exporter, which sends what is queued, and returns the
// bodies the receiver got, joined.
func (f *fixture) exported() string {
	f.t.Helper()
	f.idle()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if n := f.spans.Close(ctx); n != 0 {
		f.t.Fatalf("%d spans dropped", n)
	}
	var b strings.Builder
	for _, c := range f.receiver.Calls() {
		b.Write(c.Body)
	}
	return b.String()
}

// endpoint is an inference endpoint with a body naming an alias that serves
// it; every body holds the string "hi" once.
type endpoint struct {
	path, alias string
	body        func(alias string) string
}

var endpoints = []endpoint{
	{"/v1/chat/completions", "chat", func(a string) string { return `{"model":"` + a + `","messages":[{"role":"user","content":"hi"}]}` }},
	{"/v1/messages", "claude", func(a string) string {
		return `{"model":"` + a + `","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	}},
	{"/v1/embeddings", "embed", func(a string) string { return `{"model":"` + a + `","input":"hi"}` }},
	{"/v1/rerank", "rerank", func(a string) string { return `{"model":"` + a + `","query":"hi","documents":["a","b"]}` }},
}

func (e endpoint) valid() string { return e.body(e.alias) }

// event is the SSE bytes of one event: data alone, or a name and data.
func event(parts ...string) []byte {
	if len(parts) == 1 {
		return []byte("data: " + parts[0] + "\n\n")
	}
	return []byte("event: " + parts[0] + "\ndata: " + parts[1] + "\n\n")
}

// bytewise cuts the joined chunks into chunks of one byte.
func bytewise(chunks [][]byte) [][]byte {
	var out [][]byte
	for _, b := range bytes.Join(chunks, nil) {
		out = append(out, []byte{b})
	}
	return out
}

// logLines are the log lines written since offset, each decoded; a line
// that is not one JSON object fails the test (OPS-9).
func (f *fixture) logLines(offset int) []map[string]any {
	f.t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSuffix(f.log.String()[offset:], "\n"), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			f.t.Errorf("log line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}
