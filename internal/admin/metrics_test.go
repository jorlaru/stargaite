package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/telemetry"
	"github.com/jorlaru/stargaite/internal/usage"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// sample is one sample of a scrape: its family, its labels in order and its
// value.
type sample struct {
	name   string
	labels [][2]string
	value  float64
}

// key is s without its value: the family and each label value, in order.
func (s sample) key() string {
	parts := []string{s.name}
	for _, l := range s.labels {
		parts = append(parts, l[0]+"="+l[1])
	}
	return strings.Join(parts, "|")
}

var (
	metricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelName  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// parse reads body strictly as the text format 0.0.4 that render writes:
// a body that ends with a line feed; per family one # HELP line, then one
// # TYPE line (counter or gauge), then its samples, contiguous, each named
// as its family, with no timestamp; HELP escapes only backslash and line
// feed, label values only backslash, double quote and line feed, and hold
// valid UTF-8; label names unique within a sample; values written as
// strconv.FormatFloat(v, 'g', -1, 64); counters never negative; no series
// twice and no other line.
func parse(body string) ([]sample, error) {
	text, ok := strings.CutSuffix(body, "\n")
	if !ok {
		return nil, errors.New("the body does not end with a line feed")
	}
	var out []sample
	seen := map[string]bool{} // families and series
	var current, typ string
	for i, line := range strings.Split(text, "\n") {
		fail := func(format string, args ...any) ([]sample, error) {
			return nil, fmt.Errorf("line %d %q: %s", i+1, line, fmt.Sprintf(format, args...))
		}
		if rest, ok := strings.CutPrefix(line, "# HELP "); ok {
			name, help, _ := strings.Cut(rest, " ")
			if !metricName.MatchString(name) || seen[name] {
				return fail("a family named twice or not a metric name")
			}
			if _, err := unescape(help, false); err != nil {
				return fail("%v", err)
			}
			seen[name], current, typ = true, name, ""
			continue
		}
		if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			if rest != current+" counter" && rest != current+" gauge" || typ != "" {
				return fail("TYPE must follow the family's HELP once, counter or gauge")
			}
			typ = rest[len(current)+1:]
			continue
		}
		name, rest, _ := strings.Cut(line, " ")
		var s sample
		if open := strings.IndexByte(name, '{'); open >= 0 {
			name, rest = line[:open], line[open:]
			labels, after, err := labelSet(rest)
			if err != nil {
				return fail("%v", err)
			}
			s.labels = labels
			if rest, ok = strings.CutPrefix(after, " "); !ok {
				return fail("no space before the value")
			}
		}
		if name != current || typ == "" {
			return fail("a sample outside its family's block, or before its TYPE")
		}
		v, err := strconv.ParseFloat(rest, 64)
		if err != nil || strconv.FormatFloat(v, 'g', -1, 64) != rest || typ == "counter" && v < 0 {
			return fail("value %q is not FormatFloat(v, 'g', -1, 64) of a valid figure", rest)
		}
		s.name, s.value = name, v
		if seen["series "+s.key()] {
			return fail("a series twice")
		}
		seen["series "+s.key()] = true
		out = append(out, s)
	}
	return out, nil
}

// labelSet reads {name="value",...} from the start of text and returns the
// labels and what follows the closing brace.
func labelSet(text string) ([][2]string, string, error) {
	var labels [][2]string
	rest := text[1:]
	for {
		name, after, ok := strings.Cut(rest, `="`)
		if !ok || !labelName.MatchString(name) || slices.ContainsFunc(labels, func(l [2]string) bool { return l[0] == name }) {
			return nil, "", fmt.Errorf("label name %q missing, invalid or repeated", name)
		}
		end := 0
		for end < len(after) && after[end] != '"' {
			if after[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(after) {
			return nil, "", errors.New("a label value without its closing quote")
		}
		value, err := unescape(after[:end], true)
		if err != nil {
			return nil, "", err
		}
		labels = append(labels, [2]string{name, value})
		switch rest = after[end+1:]; {
		case strings.HasPrefix(rest, ","):
			rest = rest[1:]
		case strings.HasPrefix(rest, "}"):
			return labels, rest[1:], nil
		default:
			return nil, "", errors.New("a label not followed by , or }")
		}
	}
}

// unescape undoes the escapes of a HELP text, or of a label value with
// quote: backslash, line feed and, in a label value, double quote.
func unescape(s string, quote bool) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("invalid UTF-8")
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' && quote {
			return "", errors.New("an unescaped double quote")
		}
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if i++; i == len(s) {
			return "", errors.New("a trailing backslash")
		}
		switch s[i] {
		case '\\':
			b.WriteByte('\\')
		case 'n':
			b.WriteByte('\n')
		case '"':
			if !quote {
				return "", errors.New(`\" in a HELP text`)
			}
			b.WriteByte('"')
		default:
			return "", fmt.Errorf(`unknown escape \%c`, s[i])
		}
	}
	return b.String(), nil
}

// FuzzLabel: a label value render escapes parses back to itself (any valid
// UTF-8; invalid UTF-8 is replaced first, so it parses to the replaced text).
// Its seeds are under testdata/fuzz/FuzzLabel.
func FuzzLabel(f *testing.F) {
	f.Fuzz(func(t *testing.T, v string) {
		samples, err := parse("# HELP x h\n# TYPE x gauge\nx{l=\"" + escapeLabel(v) + "\"} 1\n")
		if want := strings.ToValidUTF8(v, "\uFFFD"); err != nil || len(samples) != 1 || samples[0].labels[0][1] != want {
			t.Errorf("%q: parsed %+v, error %v, want %q", v, samples, err, want)
		}
	})
}

// TestRender checks render's format rules and escaping (phase-4 design
// section 7): every family once, sorted, HELP and TYPE first; label values
// and HELP escaped; values as FormatFloat 'g' -1; series only for the
// counters seen, one sample for each family without labels; the exporter's
// counters and last error; budget gauges with a missing limit read as 0;
// and a counter no family holds fails the render instead of vanishing.
func TestRender(t *testing.T) {
	now := time.UnixMilli(testNow)
	a := admission.New(func() time.Time { return now }, nil)
	monthly := domain.Budget{LimitUSD: new(10.0), ResetInterval: domain.ResetMonthly}
	a.Spend(0.375, now, []string{"global"}, []admission.Budget{{Scope: "global", Spec: monthly}})
	// k's never budget spent 1.5 from revision 1 and was set again at 5, so
	// it reads 0; n's spent 1.5 from revision 2, still its start.
	a.SeedNever("key:k", 1, 1.5)
	a.SeedNever("key:n", 2, 1.5)
	never := &domain.Budget{LimitUSD: new(3.0), ResetInterval: domain.ResetNever}
	snap := &config.Snapshot{Settings: domain.Settings{Budget: &monthly}, NeverSince: map[string]int64{"key:k": 5, "key:n": 2},
		Keys:   map[string]*config.Key{"k": {Name: "k", Spec: domain.Key{Budget: &domain.Budget{ResetInterval: domain.ResetNever}}}, "n": {Name: "n", Spec: domain.Key{Budget: never}}},
		Models: map[string]*config.Model{"m": {Name: "m", Spec: domain.Model{Budget: &domain.Budget{LimitUSD: new(0.5), ResetInterval: domain.ResetDaily}}}}}
	counters, spanned := &usage.Counters{}, &usage.Counters{}
	counters.Add(usage.Counter{Name: "requests", Key: "a\\b\"c\nd", Label: "200"}, 0.1)
	counters.Add(usage.Counter{Name: "requests", Key: "a\\b\"c\nd", Label: "200"}, 0.2)
	counters.Add(usage.Counter{Name: "cost_usd", Key: "k", Model: "m"}, 1e21)
	counters.Add(usage.Counter{Name: "budget_warning", Model: "m"}, 1)
	spanned.Add(usage.Counter{Name: "spans", Key: "k", Model: "m"}, 3)
	st := telemetry.Status{Sent: 3, Dropped: 1, RejectedByStatus: map[int]int64{502: 2, 400: 1}, Queued: 4, LastError: "502 Bad Gateway: a \"quoted\"\nline \\ end"}
	var b strings.Builder
	if err := render(&b, snap, counters, spanned, a, st); err != nil {
		t.Fatal(err)
	}
	body := b.String()
	samples, err := parse(body)
	if err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	var families []string
	for _, line := range strings.Split(body, "\n") {
		if name, ok := strings.CutPrefix(line, "# HELP "); ok {
			families = append(families, strings.Fields(name)[0])
		}
	}
	if len(families) != len(names)+9 || !slices.IsSorted(families) {
		t.Errorf("families %q: want the %d of names and 9 more, sorted", families, len(names))
	}
	for _, want := range []string{
		`stargaite_requests_total{key="a\\b\"c\nd",model="",status="200"} 0.30000000000000004`,
		`stargaite_cost_usd_total{key="k",model="m"} 1e+21`,
		`stargaite_budget_warnings_total{key="",model="m"} 1`,
		`stargaite_spans_emitted_total{key="k",model="m"} 3`,
		`stargaite_unauthenticated_requests_total 0`,
		`stargaite_store_check_failures_total 0`,
		`stargaite_spans_sent_total 3`,
		`stargaite_spans_dropped_total 1`,
		`stargaite_spans_queued 4`,
		"stargaite_spans_rejected_total{reason=\"backend\",status=\"\"} 0\nstargaite_spans_rejected_total{reason=\"status\",status=\"400\"} 1\n" +
			`stargaite_spans_rejected_total{reason="status",status="502"} 2`,
		`stargaite_exporter_last_error_info{message="502 Bad Gateway: a \"quoted\"\nline \\ end"} 1`,
		"stargaite_budget_limit_usd{scope=\"global\",name=\"\",interval=\"monthly\"} 10\nstargaite_budget_limit_usd{scope=\"key\",name=\"k\",interval=\"never\"} 0\n" +
			"stargaite_budget_limit_usd{scope=\"key\",name=\"n\",interval=\"never\"} 3\n" +
			`stargaite_budget_limit_usd{scope="model",name="m",interval="daily"} 0.5`,
		`stargaite_budget_spent_usd{scope="global",name="",interval="monthly"} 0.375`,
		`stargaite_budget_spent_usd{scope="key",name="k",interval="never"} 0`,
		`stargaite_budget_spent_usd{scope="key",name="n",interval="never"} 1.5`,
		`stargaite_budget_spent_usd{scope="model",name="m",interval="daily"} 0`,
		"# HELP stargaite_quota_limit Limit of each quota window held.\n# TYPE stargaite_quota_limit gauge\n# HELP stargaite_quota_used Requests or tokens used in each quota window held.",
		"# TYPE stargaite_tokens_total counter\n# HELP stargaite_unauthenticated_requests_total Data-plane requests refused with 401 (TEL-5).",
	} {
		if !strings.Contains("\n"+body, "\n"+want+"\n") {
			t.Errorf("no line %q in\n%s", want, body)
		}
	}
	if n := len(samples); n != 2+1+1+1+2+3+1+3+3+2+2 {
		t.Errorf("%d samples:\n%s", n, body)
	}
	if got := helpEscape.Replace("a\\b\nc\"d"); got != `a\\b\nc"d` {
		t.Errorf("HELP escape %q", got)
	}
	// Every name maps to a family whose labels a counter can fill.
	for name, f := range names {
		c := usage.Counter{Name: name}
		for i, v := range []*string{&c.Key, &c.Model, &c.Label}[:len(f.labels)] {
			*v = fmt.Sprint("v", i)
		}
		var b strings.Builder
		one := &usage.Counters{}
		one.Add(c, 2)
		if err := render(&b, snap, one, &usage.Counters{}, a, telemetry.Status{}); err != nil || !strings.Contains(b.String(), "\n"+f.name) {
			t.Errorf("%s: error %v", name, err)
		}
		if _, err := parse(b.String()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, c := range []usage.Counter{{Name: "nope"}, {Name: "cost_usd", Key: "k", Label: "x"}, {Name: "unauthenticated", Key: "k"}, {Name: "spans", Label: "x"}} {
		bad := &usage.Counters{}
		bad.Add(c, 1)
		for _, sets := range [][2]*usage.Counters{{bad, &usage.Counters{}}, {&usage.Counters{}, bad}} {
			if err := render(&strings.Builder{}, snap, sets[0], sets[1], a, telemetry.Status{}); err == nil || !strings.Contains(err.Error(), "has no family") {
				t.Errorf("%+v: error %v", c, err)
			}
		}
	}
}

// TestMetricsRoute: the route answers the text format whatever Accept asks,
// refuses a query, and writes no audit row (a read).
func TestMetricsRoute(t *testing.T) {
	f := newFixture(t)
	audit := f.count(t, "SELECT COUNT(*) FROM audit")
	r := httptest.NewRequest(http.MethodGet, "/admin/v1/metrics", nil)
	r.Header.Set("Authorization", "Bearer "+testMetricsToken)
	r.Header.Set("Accept", "application/openmetrics-text; version=1.0.0, application/json")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if _, err := parse(w.Body.String()); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" || err != nil {
		t.Errorf("status %d, headers %v, error %v", w.Code, w.Header(), err)
	}
	errorOf(t, f.do(t, http.MethodGet, "/admin/v1/metrics?format=json", nil), http.StatusBadRequest, domain.CodeInvalidRequest)
	if n := f.count(t, "SELECT COUNT(*) FROM audit"); n != audit {
		t.Errorf("%d audit rows, %d before: a scrape is not audited", n, audit)
	}
}

// metricsConfig: rep reports its cost (dyadic figures, so sums are exact
// in any order) and serves every endpoint on the mock; sub is a
// subscription with a requests and a tokens window; idle is a subscription
// no call uses, so a scrape that seeded its window would show it. fast has
// a daily budget, capped one of 0, tight one of 0.25 that other exhausts,
// slow an rpm limit of 1, fb falls back from rep/m to rep/b, dropper drops
// x_drop; agent has a monthly budget it crosses 80 percent of, other none;
// the instance has a monthly budget.
const metricsConfig = `version: 1
settings:
  telemetry: { otlp_endpoint: RECEIVER, allow_private_network: true }
  budget: { limit_usd: 100, reset_interval: monthly }
providers:
  rep: { protocol: openai, base_url: MOCK, rerank_url: MOCK/rerank, allow_private_network: true, billing: pay_per_use, cost_report: { path: usage.cost, usd_per_unit: 1 } }
  sub: { protocol: openai, base_url: MOCK, allow_private_network: true, billing: subscription, quota: [{ window: 5h, requests: 4, tokens: 1000 }] }
  idle: { protocol: openai, base_url: MOCK, allow_private_network: true, billing: subscription, quota: [{ window: 1h, requests: 2 }] }
targets:
  rep/m: { kind: chat }
  rep/b: { kind: chat }
  rep/e: { kind: embeddings }
  rep/r: { kind: rerank }
  sub/s: { kind: chat }
models:
  fast: { targets: [rep/m], budget: { limit_usd: 10, reset_interval: daily } }
  fb: { targets: [rep/m, rep/b] }
  slow: { targets: [rep/m], rpm_limit: 1 }
  capped: { targets: [rep/m], budget: { limit_usd: 0, reset_interval: daily } }
  tight: { targets: [rep/m], budget: { limit_usd: 0.25, reset_interval: daily } }
  dropper: { targets: [rep/m], params: { x_drop: drop } }
  plan: { targets: [sub/s] }
  embed: { targets: [rep/e] }
  rr: { targets: [rep/r] }
keys:
  agent: { models: ["*"], budget: { limit_usd: 2, reset_interval: monthly } }
  other: { models: [fast, tight] }
`

// answer is an OpenAI chat answer from model with content, finish, tokens
// and, when cost is not empty, usage.cost.
func answer(model, content, finish string, in, out int, cost string) []byte {
	if cost != "" {
		cost = `,"cost":` + cost
	}
	return fmt.Appendf(nil, `{"id":"c","object":"chat.completion","created":0,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},`+
		`"finish_reason":%q}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d%s}}`, model, content, finish, in, out, in+out, cost)
}

// TestMetricsMatchStore is AC-48 (OPS-8, TEL-3, TEL-5, LIM-3, LIM-9,
// USE-12): a scripted run through the data plane and targets verify, in
// process, with a receiver for spans; after Writer.Close returns 0 and the
// exporter has sent everything, one scrape, parsed strictly, holds with no
// tolerance: every per key and alias family the SUM or COUNT of usage
// grouped by key_name and model; requests_total and
// unauthenticated_requests_total the script; the warning counters the
// audit rows of the crossings; spans_emitted_total, by key and alias, and
// spans_sent_total the spans the receiver accepted; the budget gauges the
// usage_daily sums; the quota gauges the window's rows; the last error and
// the rejected spans what telemetry status shows.
func TestMetricsMatchStore(t *testing.T) {
	f := newVerifyFixture(t)
	var res config.Result
	yaml := strings.NewReplacer("RECEIVER", f.receiverURL, "MOCK", f.mockURL).Replace(metricsConfig)
	if err := json.Unmarshal(f.must(t, http.StatusOK, http.MethodPost, "/admin/v1/apply", yaml), &res); err != nil {
		t.Fatal(err)
	}
	agent, other := res.Secrets["agent"], res.Secrets["other"]
	if agent == "" || other == "" {
		t.Fatalf("apply: %+v", res)
	}
	// The receiver rejects the first export, the test span alone: the last
	// error, and one span rejected by status.
	f.receiver.Script(mockprovider.Step{Status: http.StatusBadRequest, Body: []byte(`{"code":3,"message":"the test batch is refused"}`)})
	f.must(t, http.StatusOK, http.MethodPost, "/admin/v1/telemetry/test", "")

	want := map[string]float64{} // requests_total by key, alias and status
	send := func(token, key, path, body string, status int, counted string, counter string, steps ...mockprovider.Step) {
		t.Helper()
		f.mock.Script(steps...)
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		f.data.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: status %d, want %d: %s", path, body, w.Code, status, w.Body)
		}
		want[sample{name: names["requests"].name, labels: [][2]string{{"key", key}, {"model", counter}, {"status", counted}}}.key()]++
	}
	chat := func(token, key, alias, extra string, status int, steps ...mockprovider.Step) {
		t.Helper()
		send(token, key, "/v1/chat/completions", `{"model":"`+alias+`","messages":[{"role":"user","content":"hi"}]`+extra+`}`, status, strconv.Itoa(status), alias, steps...)
	}
	ok := func(body []byte) mockprovider.Step { return mockprovider.Step{Body: body} }
	chunk := func(data string) []byte { return []byte("data: " + data + "\n\n") }
	first := chunk(`{"id":"c","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`)

	chat(agent, "agent", "fast", "", 200, ok(answer("m", "ok", "stop", 3, 5, "0.25")))
	chat(agent, "agent", "fast", "", 200, ok(answer("m", "ok", "stop", 2, 4, "0.125")))
	chat(other, "other", "fast", "", 200, ok(answer("m", "ok", "stop", 1, 1, "0.125")))
	// One call that exhausts tight's budget.
	chat(other, "other", "tight", "", 200, ok(answer("m", "ok", "stop", 1, 1, "0.25")))
	// A stream, its usage and cost in the last chunk.
	chat(agent, "agent", "fast", `,"stream":true`, 200, mockprovider.Step{Chunks: [][]byte{first,
		chunk(`{"id":"c","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		chunk(`{"id":"c","object":"chat.completion.chunk","created":0,"model":"m","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3,"cost":0.25}}`),
		chunk(`[DONE]`)}})
	// A fallback: two records, two spans.
	chat(agent, "agent", "fb", "", 200, mockprovider.Step{Status: 500}, ok(answer("b", "ok", "stop", 1, 2, "0.125")))
	// A cut stream: committed, so 200 on the wire and the terminal error's
	// 502 in the counter; its record is estimated and unpriced.
	send(agent, "agent", "/v1/chat/completions", `{"model":"fast","messages":[{"role":"user","content":"hi"}],"stream":true}`, 200, "502", "fast",
		mockprovider.Step{Chunks: [][]byte{first}, Cut: true})
	// Refused at admission (rpm), and by the budget, before any call.
	chat(agent, "agent", "slow", "", 200, ok(answer("m", "ok", "stop", 1, 1, "0.125")))
	chat(agent, "agent", "slow", "", 429)
	chat(agent, "agent", "capped", "", 402)
	// Unpriced, a length finish, an empty answer, a mismatch, a drop rule.
	chat(agent, "agent", "fast", "", 200, ok(answer("m", "ok", "stop", 1, 1, "")))
	chat(agent, "agent", "fast", "", 200, ok(answer("m", "cut", "length", 1, 7, "0.125")))
	chat(agent, "agent", "fast", "", 200, ok(answer("m", "", "stop", 1, 0, "0.125")))
	chat(agent, "agent", "fast", "", 200, ok(answer("other", "ok", "stop", 1, 1, "0.125")))
	chat(agent, "agent", "dropper", `,"x_drop":0.5`, 200, ok(answer("m", "ok", "stop", 1, 1, "0.125")))
	// Embeddings; rerank with and without usage (the second estimated).
	send(agent, "agent", "/v1/embeddings", `{"model":"embed","input":"hi"}`, 200, "200", "embed",
		ok([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0,0.5,1]}],"model":"e","usage":{"prompt_tokens":4,"total_tokens":4,"cost":0.25}}`)))
	send(agent, "agent", "/v1/rerank", `{"model":"rr","query":"q","documents":["a"]}`, 200, "200", "rr",
		ok([]byte(`{"results":[{"index":0,"relevance_score":0.5}],"usage":{"total_tokens":3,"cost":0.125}}`)))
	send(agent, "agent", "/v1/rerank", `{"model":"rr","query":"q","documents":["a"]}`, 200, "200", "rr",
		ok([]byte(`{"results":[{"index":0,"relevance_score":0.5}]}`)))
	// A subscription's window: four calls (the fourth crosses 80 percent),
	// then a fifth refused.
	for range 4 {
		chat(agent, "agent", "plan", "", 200, ok(answer("s", "ok", "stop", 3, 5, "")))
	}
	chat(agent, "agent", "plan", "", 429)
	// An alias that does not exist, counted under none.
	send(agent, "agent", "/v1/chat/completions", `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`, 404, "404", "")
	// A verify of rep/m: one call, under _verify and no alias.
	f.mock.Script(ok(answer("m", "391", "stop", 20, 2, "0.125")))
	if v := f.verify(t, "rep/m", `{}`); !v.OK || len(v.Calls) != 1 {
		t.Fatalf("verify %+v", v)
	}
	want[sample{name: names["requests"].name, labels: [][2]string{{"key", domain.VerifyKey}, {"model", ""}, {"status", "200"}}}.key()]++
	// Three bad tokens: on chat, on GET /v1/models, on messages.
	for _, r := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"fast"}`)),
		httptest.NewRequest(http.MethodGet, "/v1/models", nil),
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"fast"}`)),
	} {
		r.Header.Set("Authorization", "Bearer "+agent+"x")
		r.Header.Set("X-Api-Key", "sk-stargaite-not-a-key")
		w := httptest.NewRecorder()
		f.data.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: status %d", r.Method, r.URL.Path, w.Code)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if n := f.writer.Close(ctx); n != 0 {
		t.Fatalf("Writer.Close: %d records lost", n)
	}
	if n := f.spans.Close(ctx); n != 0 {
		t.Fatalf("%d spans dropped", n)
	}

	// The scrape, with the metrics token, over the listener.
	scrape, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url+"/admin/v1/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	scrape.Header.Set("Authorization", "Bearer "+testMetricsToken)
	resp, err := f.client.Do(scrape)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != metricsType {
		t.Fatalf("scrape: status %d, headers %v, error %v", resp.StatusCode, resp.Header, err)
	}
	body := string(raw)
	samples, err := parse(body)
	if err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	got := map[string]map[string]float64{} // by family, the series and its value
	for _, s := range samples {
		if got[s.name] == nil {
			got[s.name] = map[string]float64{}
		}
		got[s.name][s.key()] = s.value
	}
	expect := func(what string, family string, want map[string]float64) {
		t.Helper()
		if fmt.Sprint(got[family]) != fmt.Sprint(want) {
			t.Errorf("%s: %s\n got  %v\n want %v", what, family, got[family], want)
		}
	}
	labelsOf := map[string][]string{}
	for _, f := range slices.Concat(slices.Collect(maps.Values(names)),
		[]family{spansSent, spansDropped, spansRejected, spansQueued, lastError, budgetSpent, budgetLimit, quotaUsed, quotaLimit}) {
		labelsOf[f.name] = f.labels
	}
	// series is the key of a sample of family with these label values.
	series := func(family string, values ...string) string {
		s := sample{name: family}
		for i, v := range values {
			s.labels = append(s.labels, [2]string{labelsOf[family][i], v})
		}
		return s.key()
	}

	// requests_total and unauthenticated_requests_total: the script.
	expect("requests", names["requests"].name, want)
	expect("unauthenticated", names["unauthenticated"].name, map[string]float64{names["unauthenticated"].name: 3})
	expect("store check", names["store_check_failed"].name, map[string]float64{names["store_check_failed"].name: 0})

	// Every per key and alias family: SUM or COUNT over usage.
	db := f.st.Read()
	rows, err := db.Query(`SELECT key_name, model, SUM(input_tokens), SUM(cache_read_tokens), SUM(cache_write_tokens), SUM(output_tokens),
		SUM(COALESCE(reasoning_tokens, 0)), SUM(cost_usd), SUM(billing = 'pay_per_use' AND cost_usd IS NULL),
		SUM(COALESCE(finish_reason, '') IN ('length', 'max_tokens', 'model_context_window_exceeded')), SUM(content_empty), SUM(model_mismatch)
		FROM usage GROUP BY key_name, model`)
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]map[string]float64{}
	put := func(family, k string, v float64) {
		if v == 0 { // a counter only exists once something was added
			return
		}
		if stored[family] == nil {
			stored[family] = map[string]float64{}
		}
		stored[family][k] += v
	}
	for rows.Next() {
		var key, model string
		var in, read, write, out, reasoning, unpriced, length, empty, mismatch float64
		var cost sql.NullFloat64
		if err := rows.Scan(&key, &model, &in, &read, &write, &out, &reasoning, &cost, &unpriced, &length, &empty, &mismatch); err != nil {
			t.Fatal(err)
		}
		tokens := names["tokens"].name
		for label, v := range map[string]float64{"input": in, "cache_read": read, "cache_write": write, "output": out, "reasoning": reasoning} {
			put(tokens, series(tokens, key, model, label), v)
		}
		for name, v := range map[string]float64{"cost_usd": cost.Float64, "unpriced": unpriced, "length": length, "empty": empty, "model_mismatch": mismatch} {
			put(names[name].name, series(names[name].name, key, model), v)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows, err = db.Query(`SELECT key_name, model, params_changed FROM usage WHERE params_changed != ''`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key, model, changed string
		if err := rows.Scan(&key, &model, &changed); err != nil {
			t.Fatal(err)
		}
		for _, c := range strings.Split(changed, ",") {
			put(names["params_changed"].name, series(names["params_changed"].name, key, model, c[strings.LastIndexByte(c, ':')+1:]), 1)
		}
	}
	for _, name := range []string{"tokens", "cost_usd", "unpriced", "length", "empty", "model_mismatch", "params_changed"} {
		if stored[names[name].name] == nil {
			t.Errorf("%s: the script left no record to count", name)
		}
		expect("usage", names[name].name, stored[names[name].name])
	}

	// The warning counters: the audit rows of the crossings.
	rows, err = db.Query(`SELECT action, resource_name, COUNT(*) FROM audit WHERE actor = 'system' GROUP BY action, resource_name`)
	if err != nil {
		t.Fatal(err)
	}
	crossed := map[string]map[string]float64{}
	for rows.Next() {
		var action, scope string
		var n float64
		if err := rows.Scan(&action, &scope, &n); err != nil {
			t.Fatal(err)
		}
		key, _ := strings.CutPrefix(scope, "key:")
		if key == scope {
			key = ""
		}
		alias, _ := strings.CutPrefix(scope, "model:")
		if alias == scope {
			alias = ""
		}
		family := names[action].name
		if crossed[family] == nil {
			crossed[family] = map[string]float64{}
		}
		crossed[family][series(family, key, alias)] += n
	}
	for _, name := range []string{"budget_warning", "budget_exhausted", "quota_warning"} {
		expect("audit", names[name].name, crossed[names[name].name])
	}
	if crossed[names["budget_warning"].name] == nil || crossed[names["budget_exhausted"].name] == nil || crossed[names["quota_warning"].name] == nil {
		t.Errorf("the script warned of no budget, exhausted none or warned of no quota: %v", crossed)
	}

	// Spans: emitted by key and alias, and sent, as the receiver got them;
	// an alias that does not exist under none. The first export, the test
	// span alone, was rejected.
	aliases := f.cfg.Snapshot().Models
	received, sent := map[string]float64{}, 0.0
	exports := f.receiver.Calls()
	for i, c := range exports {
		var req struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						Attributes []struct {
							Key   string
							Value struct{ StringValue string }
						}
					}
				}
			}
		}
		if err := json.Unmarshal(c.Body, &req); err != nil || c.Path != "/v1/traces" {
			t.Fatalf("the receiver got %s %s", c.Path, c.Body)
		}
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					var key, alias string
					for _, a := range s.Attributes {
						switch a.Key {
						case "stargaite.key":
							key = a.Value.StringValue
						case "stargaite.model":
							alias = a.Value.StringValue
						}
					}
					if aliases[alias] == nil {
						alias = ""
					}
					if i == 0 {
						if key != telemetry.TestKey {
							t.Errorf("the rejected export holds a span of %q", key)
						}
						continue
					}
					sent++
					received[series(names["spans"].name, key, alias)]++
				}
			}
		}
	}
	expect("receiver", names["spans"].name, received)
	expect("receiver", spansSent.name, map[string]float64{spansSent.name: sent})
	if sent == 0 || received[series(names["spans"].name, "agent", "fb")] != 2 || received[series(names["spans"].name, domain.VerifyKey, "")] != 1 {
		t.Errorf("spans received %v", received)
	}

	// The exporter: rejected and last error as telemetry status shows them.
	var status struct{ Exporter telemetry.Status }
	if err := json.Unmarshal(f.must(t, http.StatusOK, http.MethodGet, "/admin/v1/telemetry", ""), &status); err != nil {
		t.Fatal(err)
	}
	if status.Exporter.LastError == "" || status.Exporter.Sent != int64(sent) || status.Exporter.RejectedByStatus[400] != 1 {
		t.Errorf("telemetry status %+v", status.Exporter)
	}
	expect("telemetry status", lastError.name, map[string]float64{series(lastError.name, status.Exporter.LastError): 1})
	expect("telemetry status", spansRejected.name, map[string]float64{series(spansRejected.name, "backend", ""): 0, series(spansRejected.name, "status", "400"): 1})
	expect("telemetry status", spansDropped.name, map[string]float64{spansDropped.name: 0})
	expect("telemetry status", spansQueued.name, map[string]float64{spansQueued.name: 0})

	// Budget gauges: the usage_daily sums of each scope's period.
	sum := func(where string) float64 {
		var v float64
		if err := db.QueryRow(`SELECT COALESCE(SUM(cost_usd), 0) FROM usage_daily WHERE ` + where).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	day := time.UnixMilli(testNow).UTC().Format(time.DateOnly)
	month := "substr(day, 1, 7) = '" + day[:7] + "'"
	spent := map[string]float64{
		series(budgetSpent.name, "global", "", "monthly"):    sum(month),
		series(budgetSpent.name, "key", "agent", "monthly"):  sum(month + " AND key_name = 'agent'"),
		series(budgetSpent.name, "model", "capped", "daily"): sum("day = '" + day + "' AND model = 'capped'"),
		series(budgetSpent.name, "model", "fast", "daily"):   sum("day = '" + day + "' AND model = 'fast'"),
		series(budgetSpent.name, "model", "tight", "daily"):  sum("day = '" + day + "' AND model = 'tight'"),
	}
	expect("usage_daily", budgetSpent.name, spent)
	expect("configuration", budgetLimit.name, map[string]float64{
		series(budgetLimit.name, "global", "", "monthly"): 100, series(budgetLimit.name, "key", "agent", "monthly"): 2,
		series(budgetLimit.name, "model", "capped", "daily"): 0, series(budgetLimit.name, "model", "fast", "daily"): 10,
		series(budgetLimit.name, "model", "tight", "daily"): 0.25,
	})
	if spent[series(budgetSpent.name, "key", "agent", "monthly")] < 0.8*2 || spent[series(budgetSpent.name, "global", "", "monthly")] == 0 ||
		spent[series(budgetSpent.name, "model", "tight", "daily")] != 0.25 {
		t.Errorf("spend %v", spent)
	}

	// Quota gauges: the window's rows, as held; the scrape seeded nothing,
	// so idle has no window.
	var calls, tokens float64
	if err := db.QueryRow(`SELECT COUNT(*), SUM(input_tokens + output_tokens) FROM usage WHERE target LIKE 'sub/%' AND started_at >= ?`,
		time.UnixMilli(testNow).Add(-5*time.Hour).UnixMilli()).Scan(&calls, &tokens); err != nil {
		t.Fatal(err)
	}
	expect("window rows", quotaUsed.name, map[string]float64{series(quotaUsed.name, "sub", "", "5h", "requests"): calls, series(quotaUsed.name, "sub", "", "5h", "tokens"): tokens})
	expect("configuration", quotaLimit.name, map[string]float64{series(quotaLimit.name, "sub", "", "5h", "requests"): 4, series(quotaLimit.name, "sub", "", "5h", "tokens"): 1000})
	if calls != 4 {
		t.Errorf("%v calls in the window, want 4", calls)
	}
}
