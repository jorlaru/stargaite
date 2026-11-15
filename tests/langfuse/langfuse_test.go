// Package langfuse is the integration job of TEL-14 (AC-30): the proxy, in
// process in front of the mock provider, exports its spans with the
// langfuse preset to a self-hosted Langfuse, with and without the header
// x-langfuse-ingestion-version: 4, for a success, a stream, a fallback, a
// refusal, a subscription call and a request under an external traceparent;
// then it reads them back through Langfuse's public API (GET
// /api/public/v2/observations) and checks that each trace is readable, that
// the key, the alias and the target filter, that a session shows, and that
// the cost Langfuse shows for each pay_per_use trace equals its records'
// cost_usd. What Langfuse makes of a subscription call without a cost, and of
// a request span whose parent is external, is logged for the guide.
//
// It runs only when the three variables below are set, so make ci and a fork
// with no secret skip it (PUB-5); maintainers run it before any release that
// changes the span schema or the exporter:
//
//	STARGAITE_LANGFUSE_URL  the Langfuse base URL, such as http://localhost:3000;
//	                        spans go to <url>/api/public/otel/v1/traces
//	LANGFUSE_PUBLIC_KEY     a project's public key, the preset's user
//	LANGFUSE_SECRET_KEY     its secret key, the preset's password; never printed
//
// Use a disposable project: the test leaves its traces there. Run it with
// go test -count=1 -timeout 30m -v ./tests/langfuse: the wait for traces
// sent without the header can pass go test's default 10 minutes.
package langfuse

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
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

const adminKey = "langfuse-admin-key-of-at-least-32-characters"

// The bounded waits for Langfuse to show a phase's traces: real time with
// the header; without it Langfuse documents a delay of up to 10 minutes.
const (
	waitV4     = 3 * time.Minute
	waitLegacy = 15 * time.Minute
)

// A subscription reply under a model Langfuse's catalogue knows, so the test
// can see whether Langfuse infers a cost that the record does not have.
const planReply = `{"id":"chatcmpl-plan","object":"chat.completion","created":0,"model":"gpt-4o-mini",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"plan reply"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`

// request is one request of a phase and what Langfuse must show for it.
type request struct {
	name, alias, trace string
	spans              int    // the request span and one attempt span per failed call
	parent             string // the external parent span id, if any
}

// observation is the part of Langfuse's ObservationV2 the test reads.
type observation struct {
	ID            string             `json:"id"`
	TraceID       string             `json:"traceId"`
	Parent        *string            `json:"parentObservationId"`
	Root          *bool              `json:"isRootObservation"`
	Type          string             `json:"type"`
	Name          string             `json:"name"`
	Level         string             `json:"level"`
	StatusMessage string             `json:"statusMessage"`
	SessionID     string             `json:"sessionId"`
	Model         string             `json:"model"`
	Metadata      map[string]any     `json:"metadata"`
	Usage         map[string]int64   `json:"usageDetails"`
	CostDetails   map[string]float64 `json:"costDetails"`
	TotalCost     *float64           `json:"totalCost"`
	TraceName     string             `json:"traceName"`
}

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func TestLangfuse(t *testing.T) {
	base, public, secret := os.Getenv("STARGAITE_LANGFUSE_URL"), os.Getenv("LANGFUSE_PUBLIC_KEY"), os.Getenv("LANGFUSE_SECRET_KEY")
	if base == "" || public == "" || secret == "" {
		t.Skip("STARGAITE_LANGFUSE_URL, LANGFUSE_PUBLIC_KEY and LANGFUSE_SECRET_KEY are not set: no Langfuse")
	}
	base = strings.TrimSuffix(base, "/")
	started := time.Now()

	// Three mocks: one answering (model "mock"), one failing with 500 in
	// front of it, and a subscription plan answering as gpt-4o-mini.
	ok, bad, plan := mockprovider.New(), mockprovider.New(), mockprovider.New()
	var urls []any
	for _, m := range []*mockprovider.Server{ok, bad, plan} {
		s := httptest.NewServer(m)
		t.Cleanup(s.Close)
		urls = append(urls, s.URL)
	}

	st, err := store.Open(context.Background(), t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	resolve := func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	env := func() config.Env {
		return config.Env{Lookup: func(name string) (string, bool) {
			if name == "MOCK_KEY" {
				return "mock-key", true
			}
			return os.LookupEnv(name) // the preset's LANGFUSE_PUBLIC_KEY and LANGFUSE_SECRET_KEY
		}, Resolve: resolve}
	}
	cfg, err := config.Open(context.Background(), st, env, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	clients := [2]*http.Client{transport.NewClient(false, resolve), transport.NewClient(true, resolve)}
	spans := telemetry.NewExporter(func() *config.Exporter { return cfg.Snapshot().Exporter }, clients, "langfuse-test", log)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		spans.Close(ctx)
	})
	limits := admission.New(cfg.Now, usage.Seeder(st.Read(), log))
	records := usage.NewWriter(st.Write, log)
	t.Cleanup(func() { records.Close(context.Background()) })
	adminH := admin.Handler(admin.Deps{Config: cfg, KeyHash: sha256.Sum256([]byte(adminKey)), Log: log, Usage: st.Read(), Admission: limits, Spans: spans})
	data := api.Handler(api.Deps{Config: cfg, Log: log, Clients: clients, Flight: &sync.WaitGroup{}, Usage: records,
		Admission: limits, Counters: &usage.Counters{}, Spans: spans, Version: "langfuse-test"})
	call := func(h http.Handler, method, path, body string, header ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		for i := 0; i < len(header); i += 2 {
			r.Header.Set(header[i], header[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	adm := func(method, path, body string) []byte {
		t.Helper()
		w := call(adminH, method, path, body, "Authorization", "Bearer "+adminKey)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}

	// One key per phase, named for this run, so the key filter finds this
	// run's traces and nothing else.
	run := random(4)
	keys := [2]string{"lf-" + run + "-v4", "lf-" + run + "-legacy"}
	var res config.Result
	if err := json.Unmarshal(adm(http.MethodPost, "/admin/v1/apply", fmt.Sprintf(`version: 1
providers:
  ok: { protocol: openai, base_url: %q, api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true }
  bad: { protocol: openai, base_url: %q, api_key_env: MOCK_KEY, billing: pay_per_use, allow_private_network: true }
  plan: { protocol: openai, base_url: %q, api_key_env: MOCK_KEY, billing: subscription, allow_private_network: true }
targets:
  ok/mock: { kind: chat, pricing: { input_per_million: 1, output_per_million: 2, as_of: "2026-11-01" } }
  bad/mock: { kind: chat, pricing: { input_per_million: 1, output_per_million: 2, as_of: "2026-11-01" } }
  plan/gpt-4o-mini: { kind: chat }
models:
  lf-ok: { targets: [ok/mock] }
  lf-fallback: { targets: [bad/mock, ok/mock] }
  lf-plan: { targets: [plan/gpt-4o-mini] }
keys:
  %s: { models: ["*"] }
  %s: { models: ["*"] }
`, append(urls, keys[0], keys[1])...)), &res); err != nil {
		t.Fatal(err)
	}
	// The preset, then the endpoint: the test's Langfuse may be on loopback.
	adm(http.MethodPatch, "/admin/v1/telemetry", fmt.Sprintf(`{"preset":"langfuse","otlp_endpoint":%q,"allow_private_network":true}`, base+"/api/public/otel"))
	var tested telemetry.Result
	if err := json.Unmarshal(adm(http.MethodPost, "/admin/v1/telemetry/test", ""), &tested); err != nil || tested.Outcome != telemetry.OutcomeAccepted {
		t.Fatalf("telemetry test: %+v %v", tested, err)
	}

	// traffic sends the requests of one phase with key and waits until the
	// exporter has sent their spans, so the next phase's header change
	// reaches none of them.
	sent := int64(1) // the test span
	traffic := func(key string) []request {
		t.Helper()
		token := "Bearer " + res.Secrets[key]
		chat := func(name, alias, extra string, header ...string) request {
			w := call(data, http.MethodPost, "/v1/chat/completions",
				`{"model":"`+alias+`","messages":[{"role":"user","content":"Say hi."}]`+extra+`}`,
				append([]string{"Authorization", token}, header...)...)
			r := request{name: name, alias: alias, trace: w.Header().Get("X-Stargaite-Trace-Id"), spans: 1}
			if (w.Code == http.StatusOK) == (name == "refusal") || r.trace == "" {
				t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
			}
			return r
		}
		bad.Script(mockprovider.Step{Status: http.StatusInternalServerError, Body: []byte(`{"error":{"message":"mock failure"}}`)})
		plan.Script(mockprovider.Step{Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(planReply)})
		trace, parent := random(16), random(8)
		reqs := []request{
			chat("success", "lf-ok", "", "X-Session-Id", key+"-session"),
			chat("stream", "lf-ok", `,"stream":true,"stream_options":{"include_usage":true}`),
			chat("fallback", "lf-fallback", ""),
			chat("refusal", "lf-no-such-alias", ""),
			chat("subscription", "lf-plan", ""),
			chat("external parent", "lf-ok", "", "Traceparent", "00-"+trace+"-"+parent+"-01"),
		}
		reqs[2].spans, reqs[5].parent = 2, parent
		if reqs[5].trace != trace {
			t.Fatalf("external parent: trace %s, not the client's %s", reqs[5].trace, trace)
		}
		for _, r := range reqs {
			sent += int64(r.spans)
		}
		for deadline := time.Now().Add(30 * time.Second); spans.Status().Sent < sent; time.Sleep(100 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("spans not sent: %+v", spans.Status())
			}
		}
		return reqs
	}
	v4 := traffic(keys[0])
	adm(http.MethodPatch, "/admin/v1/telemetry", `{"headers":{"x-langfuse-ingestion-version":null}}`)
	legacy := traffic(keys[1])

	client := &http.Client{Timeout: 30 * time.Second}
	// observations lists what Langfuse shows under the query, every page.
	observations := func(q url.Values) ([]observation, error) {
		q.Set("fields", "core,basic,metadata,usage,model,trace_context")
		q.Set("fromStartTime", started.Add(-time.Minute).UTC().Format(time.RFC3339))
		q.Set("limit", "100")
		var all []observation
		for {
			r, err := http.NewRequest(http.MethodGet, base+"/api/public/v2/observations?"+q.Encode(), nil)
			if err != nil {
				return nil, err
			}
			r.SetBasicAuth(public, secret)
			resp, err := client.Do(r)
			if err != nil {
				return nil, err
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
			_ = resp.Body.Close()
			var page struct {
				Data []observation
				Meta struct{ Cursor string }
			}
			if err == nil && resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("%s: %.300s", resp.Status, body)
			}
			if err == nil {
				err = json.Unmarshal(body, &page)
			}
			if err != nil {
				return nil, err
			}
			if all = append(all, page.Data...); page.Meta.Cursor == "" {
				return all, nil
			}
			q.Set("cursor", page.Meta.Cursor)
		}
	}
	// filter is the query of observations under metadata key = value for
	// each pair.
	filter := func(pairs ...string) url.Values {
		var f []map[string]string
		for i := 0; i < len(pairs); i += 2 {
			f = append(f, map[string]string{"type": "stringObject", "column": "metadata", "key": pairs[i], "operator": "=", "value": pairs[i+1]})
		}
		b, _ := json.Marshal(f)
		return url.Values{"filter": {string(b)}}
	}
	cost := func(trace string) (sum float64) {
		rows, err := st.Read().Query("SELECT cost_usd FROM usage WHERE trace_id = ? AND cost_usd IS NOT NULL", trace)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var c float64
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			sum += c
		}
		return sum
	}

	for i, phase := range []struct {
		name string
		key  string
		reqs []request
		wait time.Duration
	}{{"with the ingestion version header", keys[0], v4, waitV4}, {"without the ingestion version header", keys[1], legacy, waitLegacy}} {
		t.Run(phase.name, func(t *testing.T) {
			want := 0
			for _, r := range phase.reqs {
				want += r.spans
			}
			var seen []observation
			for deadline := time.Now().Add(phase.wait); ; time.Sleep(3 * time.Second) {
				var err error
				if seen, err = observations(filter("stargaite_key", phase.key)); err == nil && len(seen) >= want {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("after %s, the key filter shows %d of %d observations (%v)", phase.wait, len(seen), want, err)
				}
			}
			t.Logf("shown after %s", time.Since(started).Round(time.Second))
			byTrace := map[string][]observation{}
			for _, o := range seen {
				byTrace[o.TraceID] = append(byTrace[o.TraceID], o)
			}
			if len(seen) != want || len(byTrace) != len(phase.reqs) {
				t.Errorf("the key filter shows %d observations in %d traces, want %d in %d", len(seen), len(byTrace), want, len(phase.reqs))
			}
			for _, r := range phase.reqs {
				got, err := observations(url.Values{"traceId": {r.trace}})
				if err != nil || len(got) != r.spans {
					t.Errorf("%s: trace %s shows %d observations, want %d (%v)", r.name, r.trace, len(got), r.spans, err)
				}
				// The alias filters: the traces of this alias and key, no other.
				aliased, err := observations(filter("stargaite_key", phase.key, "stargaite_model", r.alias))
				traces := map[string]bool{}
				for _, o := range aliased {
					traces[o.TraceID] = true
				}
				var want []string
				for _, q := range phase.reqs {
					if q.alias == r.alias {
						want = append(want, q.trace)
					}
				}
				if err != nil || !slices.Equal(slices.Sorted(maps.Keys(traces)), slices.Sorted(slices.Values(want))) {
					t.Errorf("%s: the alias filter %s shows traces %v, want %v (%v)", r.name, r.alias, slices.Sorted(maps.Keys(traces)), want, err)
				}
			}
			// The target filters: the request span takes the target that
			// served, the attempt span the one that failed (annex D).
			for target, n := range map[string]int{"ok/mock": 4, "bad/mock": 1, "plan/gpt-4o-mini": 1} {
				if got, err := observations(filter("stargaite_key", phase.key, "stargaite_target", target)); err != nil || len(got) != n {
					t.Errorf("the target filter %s shows %d observations, want %d (%v)", target, len(got), n, err)
				}
			}
			for _, r := range phase.reqs {
				obs := byTrace[r.trace]
				var request *observation // the span without a parent in this trace
				for j := range obs {
					if obs[j].Parent == nil || r.parent != "" && *obs[j].Parent == r.parent {
						request = &obs[j]
					}
				}
				if request == nil {
					t.Errorf("%s: no request span among %+v", r.name, obs)
					continue
				}
				shown := 0.0
				for _, o := range obs {
					if o.TotalCost != nil {
						shown += *o.TotalCost
					}
				}
				switch r.name {
				case "success":
					if request.SessionID != phase.key+"-session" {
						t.Errorf("success: session %q, want %q", request.SessionID, phase.key+"-session")
					}
					fallthrough
				case "stream", "fallback", "external parent":
					// pay_per_use: what Langfuse shows is the cost sent, the
					// records' cost_usd, to the precision Langfuse keeps.
					if want := cost(r.trace); want == 0 || math.Abs(shown-want) > 1e-12 {
						t.Errorf("%s: Langfuse shows a cost of %v, the records %v", r.name, shown, want)
					}
				case "refusal":
					if request.Level != "ERROR" {
						t.Errorf("refusal: level %q, want ERROR", request.Level)
					}
				case "subscription":
					t.Logf("%s: a subscription record without a cost attribute, response model %s: Langfuse shows model %q, cost %v (details %v), usage %v",
						phase.name, "gpt-4o-mini", request.Model, deref(request.TotalCost), request.CostDetails, request.Usage)
				}
				if r.parent != "" {
					t.Logf("%s: a request span under the external parent %s renders with parentObservationId %v and isRootObservation %v, type %s, name %q, trace name %q; the trace holds %d observations",
						phase.name, r.parent, deref(request.Parent), deref(request.Root), request.Type, request.Name, request.TraceName, len(obs))
				}
				if i == 0 && r.name == "success" {
					t.Logf("a success renders as type %s, name %q, trace name %q, model %q, usage %v, cost details %v, metadata keys %v",
						request.Type, request.Name, request.TraceName, request.Model, request.Usage, request.CostDetails, slices.Sorted(maps.Keys(request.Metadata)))
				}
			}
		})
	}
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
