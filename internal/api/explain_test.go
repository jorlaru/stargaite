package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/params"
	"github.com/jorlaru/stargaite/internal/reasoning"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// corpusConfig has a target per dialect, with levels, maps, fragments,
// can_disable false, a target cap and unsupported parameters, three targets
// of the anthropic protocol on the same mock, and three aliases per target:
// an effort in default mode with a cap, a forced effort, and parameter
// rules. Three aliases list several targets, so a static skip sends the
// request to the next one (a stream skips an anthropic target until step
// C18); another skips both targets with different codes, so the first skip
// is told from the last (RES-10).
const corpusConfig = `version: 1
providers:
  mock: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: none, allow_private_network: true }
  anth: { protocol: anthropic, base_url: MOCK, api_key_env: ANTH_KEY, billing: none, allow_private_network: true }
targets:
  mock/none: { kind: chat }
  mock/re: { kind: chat, reasoning: { dialect: reasoning_effort, levels: [low, medium, high], can_disable: true } }
  mock/re-free: { kind: chat, reasoning: { dialect: reasoning_effort, map: { minimal: low, max: high } } }
  mock/ro: { kind: chat, reasoning: { dialect: reasoning_object, levels: [low, medium, high, xhigh], off: { reasoning: { enabled: false } } } }
  mock/tt: { kind: chat, reasoning: { dialect: thinking_toggle, levels: ["on"], can_disable: true } }
  mock/et: { kind: chat, reasoning: { dialect: enable_thinking, levels: ["on"], on: { enable_thinking: true, thinking_budget: 4096 }, off: { enable_thinking: false, temperature: 0.7 } } }
  mock/tb: { kind: chat, reasoning: { dialect: thinking_budget, levels: [low, medium, high], budgets: { low: 1024, medium: 2048, high: 4096 }, map: { max: high } } }
  mock/ae: { kind: chat, reasoning: { dialect: adaptive_effort, levels: [low, medium, high], can_disable: false } }
  mock/cap: { kind: chat, max_output_tokens: 1000, output_cap_param: max_completion_tokens, unsupported_params: [temperature, top_p, max_tokens], capabilities: { tools: false }, reasoning: { dialect: reasoning_effort } }
  mock/plain: { kind: chat, capabilities: { tools: false, vision: false } }
  anth/ae: { kind: chat, inline_think_tags: true, reasoning: { dialect: adaptive_effort, levels: [low, medium, high], can_disable: true } }
  anth/tb: { kind: chat, max_output_tokens: 8000, unsupported_params: [top_p], reasoning: { dialect: thinking_budget, levels: [low, medium, high], budgets: { low: 1024, medium: 2048, high: 4096 } } }
  anth/none: { kind: chat, capabilities: { vision: false } }
models:
TARGETS  mixed: { targets: [mock/plain, mock/cap, mock/re], reasoning: { effort: low, mode: default } }
  mixed3: { targets: [anth/ae, mock/re], max_output_tokens: 900, reasoning: { effort: medium, mode: default } }
  mixed2: { targets: [mock/cap, mock/none], params: { max_tokens: drop, x_fwd: forward } }
  skips: { targets: [mock/cap, mock/plain] }
keys:
  agent: { models: ["*"] }
`

var corpusTargets = []string{"mock/none", "mock/re", "mock/re-free", "mock/ro", "mock/tt", "mock/et", "mock/tb", "mock/ae", "mock/cap", "mock/plain",
	"anth/ae", "anth/tb", "anth/none"}

// corpusAliases are the three aliases of each target, in YAML, named after
// the target with its slash as a dash. An anthropic target needs a cap on
// every alias (PAR-7).
func corpusAliases() (string, []string) {
	var yaml strings.Builder
	names := []string{"mixed", "mixed2", "mixed3", "skips"}
	for _, id := range corpusTargets {
		_, dialect, _ := strings.Cut(id, "/")
		a := strings.ReplaceAll(id, "/", "-")
		limit, cap := 900, ""
		if dialect == "tb" {
			limit = 5000 // above every budget (RSN-3)
		}
		if strings.HasPrefix(id, "anth/") {
			cap = fmt.Sprintf(", max_output_tokens: %d", limit)
		}
		effort := ""
		if id == "mock/ae" {
			effort = ", reasoning: { effort: provider_default, mode: default }" // it always reasons (RSN-7)
		}
		fmt.Fprintf(&yaml, "  %s-d: { targets: [%s], reasoning: { effort: medium, mode: default }, max_output_tokens: %d }\n", a, id, limit)
		fmt.Fprintf(&yaml, "  %s-f: { targets: [%s], reasoning: { effort: high, mode: forced }%s }\n", a, id, cap)
		fmt.Fprintf(&yaml, "  %s-p: { targets: [%s], params: { x_fwd: forward, x_drop: drop, temperature: drop, max_tokens: drop, seed: drop }%s%s }\n", a, id, effort, cap)
		names = append(names, a+"-d", a+"-f", a+"-p")
	}
	return yaml.String(), names
}

// corpusBody is a generated chat body for alias: the members a client sends,
// in a random order, with random separators.
func corpusBody(r *rand.Rand, alias string) string {
	pick := func(p float64) bool { return r.Float64() < p }
	messages := []string{
		`[{"role":"user","content":"hi"}]`,
		`[{"role":"system","content":"be brief"},{"role":"user","content":"h\u00e9llo \"q\" é"}]`,
		`[{"role":"user","content":[{"type":"text","text":"see"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}]`,
		`[{"role":"developer","content":"terse"},{"role":"user","content":"weather?"},{"role":"assistant","content":null,"tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"city\":\"Paris\"}"}}]},{"role":"tool","tool_call_id":"c1","content":"18 C"},` +
			`{"role":"user","content":[{"type":"text","text":"thanks"},{"type":"image_url","image_url":{"url":"https://example.com/a.png","detail":"low"}}]}]`,
		`[{"role":"user","name":"ann","content":"hi"}]`, // a name has no equivalent toward anthropic
	}
	members := []string{`"messages":` + messages[r.IntN(len(messages))]}
	if pick(0.5) {
		effort := domain.Levels[r.IntN(len(domain.Levels))]
		if pick(0.05) {
			effort = "default" // off the scale (RSN-11)
		}
		members = append(members, `"reasoning_effort":"`+effort+`"`)
	}
	if pick(0.3) {
		members = append(members, `"max_tokens":`+strconv.Itoa(r.IntN(1200)))
	}
	if pick(0.2) {
		members = append(members, `"max_completion_tokens":`+strconv.Itoa(r.IntN(1200)))
	}
	if pick(0.3) {
		members = append(members, `"temperature":0.5`)
	}
	if pick(0.2) {
		members = append(members, `"top_p":0.9`)
	}
	if pick(0.2) {
		members = append(members, `"seed":7`)
	}
	if pick(0.2) {
		members = append(members, `"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]`)
		if pick(0.5) {
			members = append(members, []string{`"tool_choice":"required"`, `"tool_choice":{"type":"function","function":{"name":"f"}}`, `"parallel_tool_calls":false`}[r.IntN(3)])
		}
	}
	if pick(0.15) {
		members = append(members, []string{`"stop":"END"`, `"stop":["a","b"]`}[r.IntN(2)])
	}
	if pick(0.1) {
		members = append(members, []string{`"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object"}}}`,
			`"response_format":{"type":"json_object"}`, `"n":1`}[r.IntN(3)])
	}
	if pick(0.2) {
		members = append(members, `"stream":true`)
		if pick(0.5) {
			members = append(members, `"stream_options":{"include_usage":true}`)
		}
	}
	// Only the aliases with rules take these; any other refuses them (PAR-2).
	if rules := strings.HasSuffix(alias, "-p") || alias == "mixed2"; rules && pick(0.4) {
		members = append(members, `"x_fwd":{"a":[1,2]}`)
	}
	if rules := strings.HasSuffix(alias, "-p"); rules && pick(0.4) {
		members = append(members, `"x_drop":1`)
	}
	if pick(0.03) {
		members = append(members, `"x_other":1`) // no rule (PAR-2)
	}
	r.Shuffle(len(members), func(i, j int) { members[i], members[j] = members[j], members[i] })
	members = slices.Insert(members, r.IntN(len(members)+1), `"model":"`+alias+`"`)
	return "{" + strings.Join(members, []string{",", ", ", ",\n  "}[r.IntN(3)]) + "}"
}

// explainBlock is one target of the answer of models explain, as a client
// reads it.
type explainBlock struct {
	Target    string
	Skipped   *struct{ Code, Param string }
	Effort    *struct{ Sent *string }
	MaxOutput *struct{ Sent *int64 } `json:"max_output"`
	Changes   string
	URL       string
	Header    http.Header
	Body      string
}

// attemptLine is what the log line of finish says the attempt sent.
type attemptLine struct {
	Msg       string  `json:"msg"`
	RequestID string  `json:"request_id"`
	Changed   *string `json:"params_changed"`
	Effort    *string `json:"effort_sent"`
	MaxOutput *int64  `json:"max_output_sent"`
}

// TestCorpus is the corpus property of phase-2 design section 12 for chat
// toward openai targets, over generated bodies, aliases of every dialect and
// both alias modes, with a fixed seed. For every body, models explain and the
// data plane are given the same bytes and must agree:
//   - a refusal of explain is the data plane's, with the same status, code
//     and param, and no provider is called;
//   - every target skipped answers the 422 of the first skip (RES-10);
//   - otherwise the first target explain did not skip is called once, with
//     the exact bytes explain shows (AC-17), at its URL, with its headers plus
//     the credential, which explain never shows;
//   - the PAR-9 headers and the attempt handed to finish carry the change
//     list and the effort and cap that explain shows, which equal the values
//     read from the bytes the provider received (AC-18);
//   - the top-level members that differ between the inbound body and the
//     bytes received are exactly the ones the change list names (AC-09).
func TestCorpus(t *testing.T) {
	f := newFixture(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	aliases, names := corpusAliases()
	tokens := f.apply(t, strings.Replace(strings.ReplaceAll(corpusConfig, "base_url: MOCK", "base_url: "+srv.URL), "TARGETS", aliases, 1))
	var doc domain.Document
	if w := call(f.admin, "Bearer "+adminKey, http.MethodGet, "/admin/v1/export?format=json", nil); json.Unmarshal(w.Body.Bytes(), &doc) != nil {
		t.Fatalf("export: %s", w.Body)
	}

	const seed, n = 15, 800
	r := rand.New(rand.NewPCG(seed, seed))
	served, refused, allSkipped, streamed := map[string]int{}, 0, 0, 0
	logged := 0
	for i := range n {
		alias := names[r.IntN(len(names))]
		body := corpusBody(r, alias)
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("seed %d, body %d to %s:\n%s\n%s", seed, i, alias, body, fmt.Sprintf(format, args...))
		}
		req, _ := json.Marshal(map[string]string{"body": body})
		before := len(mock.Calls())
		ew := call(f.admin, "Bearer "+adminKey, http.MethodPost, "/admin/v1/models/"+alias+"/explain", req)
		w := f.chat("Bearer "+tokens["agent"], body)
		calls := mock.Calls()[before:]
		if strings.Contains(ew.Body.String(), planted) {
			fail("explain shows the credential: %s", ew.Body)
		}
		h := w.Header()

		if ew.Code != http.StatusOK {
			refused++
			var a, b struct{ Error domain.Error }
			_ = json.Unmarshal(ew.Body.Bytes(), &a)
			_ = json.Unmarshal(w.Body.Bytes(), &b)
			if ew.Code != w.Code || a.Error.Code != b.Error.Code || a.Error.Param != b.Error.Param || a.Error.Code == "" || len(calls) != 0 {
				fail("explain %d %s, data plane %d %s, %d calls", ew.Code, ew.Body, w.Code, w.Body, len(calls))
			}
			continue
		}
		var ex struct{ Targets []explainBlock }
		if err := json.Unmarshal(ew.Body.Bytes(), &ex); err != nil {
			fail("explain: %v: %s", err, ew.Body)
		}
		k := slices.IndexFunc(ex.Targets, func(b explainBlock) bool { return b.Skipped == nil })
		if k < 0 {
			allSkipped++
			var e struct{ Error domain.Error }
			_ = json.Unmarshal(w.Body.Bytes(), &e)
			s := ex.Targets[0].Skipped
			if w.Code != http.StatusUnprocessableEntity || e.Error.Code != s.Code || e.Error.Param != s.Param || len(calls) != 0 {
				fail("every target skipped (%s), data plane %d %s, %d calls", ew.Body, w.Code, w.Body, len(calls))
			}
			continue
		}
		b := ex.Targets[k]
		if w.Code != http.StatusOK || len(calls) != 1 {
			fail("data plane %d %s, %d calls; explain %s", w.Code, w.Body, len(calls), ew.Body)
		}
		got := calls[0]
		served[b.Target]++
		if strings.Contains(body, `"stream":true`) {
			streamed++
		}

		// AC-17: the bytes, the URL and the headers explain shows.
		if string(got.Body) != b.Body {
			fail("the provider received\n%s\nexplain shows\n%s", got.Body, b.Body)
		}
		received := srv.URL + got.Path
		if got.Query != "" {
			received += "?" + got.Query
		}
		if received != b.URL {
			fail("explain URL %q, the provider got %q", b.URL, received)
		}
		// Only the credential and what net/http adds on its own are not
		// shown: User-Agent and Accept-Encoding only with their defaults.
		anthropic := strings.HasPrefix(b.Target, "anth/")
		sent := got.Header.Clone()
		sent.Del("Authorization")
		sent.Del("X-Api-Key")
		sent.Del("Content-Length") // the body bytes are compared above
		for name, value := range map[string]string{"User-Agent": "Go-http-client/1.1", "Accept-Encoding": "gzip"} {
			if slices.Equal(sent.Values(name), []string{value}) {
				sent.Del(name)
			}
		}
		if !maps.EqualFunc(sent, b.Header, slices.Equal) {
			fail("headers: explain %q, the provider got %q", b.Header, got.Header)
		}
		switch {
		case anthropic && (got.Header.Get("X-Api-Key") != planted+"ANTH_KEY" || got.Header.Values("Authorization") != nil || got.Path != "/v1/messages"):
			fail("toward anthropic: %s %q", got.Path, got.Header)
		case !anthropic && (got.Header.Get("Authorization") != "Bearer "+planted+"MOCK_KEY" || got.Header.Values("X-Api-Key") != nil):
			fail("Authorization %q", got.Header.Get("Authorization"))
		}

		// AC-18 and PAR-9: the header, the attempt and explain agree with
		// the values read from the bytes received.
		decl := doc.Targets[b.Target]
		rb, pe := params.ReadBack(got.Body, params.ChatOut)
		if pe != nil {
			fail("read back: %+v", pe)
		}
		effort, maxOutput := reasoning.Sent(rb, decl.Reasoning), sentCap(got.Body)
		var line attemptLine
		for _, l := range strings.Split(strings.TrimSpace(f.log.String()[logged:]), "\n") {
			if err := json.Unmarshal([]byte(l), &line); err == nil && line.Msg == "attempt" && line.RequestID == h.Get(domain.RequestIDHeader) {
				break
			}
			line = attemptLine{}
		}
		logged = len(f.log.String())
		switch {
		case line.Msg == "":
			fail("no attempt line for request %s", h.Get(domain.RequestIDHeader))
		case h.Get(targetHeader) != b.Target:
			fail("target header %q, explain %q", h.Get(targetHeader), b.Target)
		case h.Get(changedHeader) != b.Changes || deref(line.Changed) != b.Changes:
			fail("changes: header %q, attempt %q, explain %q", h.Get(changedHeader), deref(line.Changed), b.Changes)
		case h.Get(effortHeader) != effort || deref(line.Effort) != effort || deref(b.Effort.Sent) != effort || (effort == "") != (h.Values(effortHeader) == nil):
			fail("effort sent %q: header %q, attempt %q, explain %q", effort, h.Get(effortHeader), deref(line.Effort), deref(b.Effort.Sent))
		case !reflect.DeepEqual(line.MaxOutput, maxOutput) || !reflect.DeepEqual(b.MaxOutput.Sent, maxOutput):
			fail("cap sent %s: attempt %s, explain %s", toJSON(maxOutput), toJSON(line.MaxOutput), toJSON(b.MaxOutput.Sent))
		}

		// AC-09: what differs is what is listed.
		check := listed
		if anthropic {
			check = translated
		}
		if err := check(body, got.Body, b.Changes, reasoning.Dialect(decl.Reasoning), b.Target); err != nil {
			fail("%v; changes %q; the provider received\n%s", err, b.Changes, got.Body)
		}
	}

	t.Logf("seed %d: %d bodies; %d refused, %d with every target skipped, %d served (%d streams): %v", seed, n, refused, allSkipped, n-refused-allSkipped, streamed, served)
	for _, id := range corpusTargets {
		if served[id] == 0 {
			t.Errorf("no generated body was served by %s", id)
		}
	}
	if refused == 0 || allSkipped == 0 || streamed == 0 {
		t.Errorf("the corpus has %d refusals, %d bodies every target skipped and %d streams; each kind needs one", refused, allSkipped, streamed)
	}
}

// sentCap is the output cap in the bytes received: the larger of the two
// chat cap members, read here without the proxy's code.
func sentCap(body []byte) *int64 {
	var v struct {
		MaxTokens           *int64 `json:"max_tokens"`
		MaxCompletionTokens *int64 `json:"max_completion_tokens"`
	}
	_ = json.Unmarshal(body, &v)
	if v.MaxTokens == nil || (v.MaxCompletionTokens != nil && *v.MaxCompletionTokens > *v.MaxTokens) {
		return v.MaxCompletionTokens
	}
	return v.MaxTokens
}

func toJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// upstream is the upstream model id of a corpus target.
func upstream(target string) string {
	_, id, _ := strings.Cut(target, "/")
	return id
}

// mapped are the top-level members of a Messages body that the chat
// adapter writes (design section 7).
var mapped = []string{"model", "max_tokens", "system", "messages", "tools", "tool_choice", "stop_sequences", "stream", "temperature", "top_p", "output_config"}

// translated checks AC-09 on one request toward an anthropic target, where
// protocol:translated stands for every member the mapping writes: it is
// listed first; the body is a Messages request with the upstream model, a
// cap and user and assistant turns only; every member outside the mapping
// and the dialect's members is listed; a forwarded member arrives byte for
// byte and a dropped one is absent unless listed again as written.
func translated(inbound string, received []byte, changes, dialect, target string) error {
	in, e1 := params.Scan([]byte(inbound), nil)
	out, e2 := params.Scan(received, nil)
	if e1 != nil || e2 != nil {
		return fmt.Errorf("scan: %v %v", e1, e2)
	}
	list := strings.Split(changes, ",")
	if list[0] != "protocol:translated" {
		return fmt.Errorf("protocol:translated is not listed first")
	}
	var m struct {
		Model     string
		MaxTokens *int64 `json:"max_tokens"`
		Messages  []struct{ Role string }
	}
	if err := json.Unmarshal(received, &m); err != nil || m.Model != upstream(target) || m.MaxTokens == nil || len(m.Messages) == 0 {
		return fmt.Errorf("not a Messages request for %s: %v", target, err)
	}
	for _, msg := range m.Messages {
		if msg.Role != "user" && msg.Role != "assistant" {
			return fmt.Errorf("a %s turn", msg.Role)
		}
	}
	names := map[string]bool{}
	for i, c := range list[1:] {
		name, action, _ := strings.Cut(c, ":")
		top, _, _ := strings.Cut(name, ".")
		names[top] = true
		a, okIn := in.Get(top)
		b, okOut := out.Get(top)
		again := slices.ContainsFunc(list[i+2:], func(l string) bool { return strings.HasPrefix(l, name+":") })
		switch {
		case action == "forwarded" && (!okIn || !okOut || !bytes.Equal(a, b)):
			return fmt.Errorf("%s is listed forwarded but changed", c)
		case action == "dropped" && okOut && !again:
			return fmt.Errorf("%s is listed dropped but was sent", c)
		}
	}
	group := append([]string{"reasoning_effort"}, domain.DialectKeys[dialect]...)
	for _, n := range out.Keys() {
		if !slices.Contains(mapped, n) && !slices.Contains(group, n) && !names[n] {
			return fmt.Errorf("%s was sent but is neither mapped nor listed", n)
		}
	}
	return nil
}

// listed checks AC-09 on one request: every top-level member but model that
// differs between the inbound body and the bytes received is named by the
// change list, and every change names a member that changed. The effort
// change is named after the client's member or the dialect's (PAR-3), so it
// stands for every member the dialect writes; a forwarded member is listed
// unchanged; a dropped one is absent, unless listed again as written; an
// alias effort dropped toward a none target is listed though the client sent
// none (C12).
func listed(inbound string, received []byte, changes, dialect, target string) error {
	in, e1 := params.Scan([]byte(inbound), nil)
	out, e2 := params.Scan(received, nil)
	if e1 != nil || e2 != nil {
		return fmt.Errorf("scan: %v %v", e1, e2)
	}
	if v, _ := out.Get("model"); string(v) != strconv.Quote(upstream(target)) {
		return fmt.Errorf("model %s sent to %s", v, target)
	}
	group := append([]string{"reasoning_effort"}, domain.DialectKeys[dialect]...)
	present := func(b *params.Body, n string) bool { _, ok := b.Get(n); return ok }
	differs := func(n string) bool {
		a, okA := in.Get(n)
		b, okB := out.Get(n)
		return okA != okB || !bytes.Equal(a, b)
	}
	names := map[string]bool{}
	effortListed := false
	var list []string
	if changes != "" {
		list = strings.Split(changes, ",")
	}
	for i, c := range list {
		name, action, _ := strings.Cut(c, ":")
		top, _, _ := strings.Cut(name, ".")
		again := slices.ContainsFunc(list[i+1:], func(l string) bool { return strings.HasPrefix(l, name+":") })
		switch {
		case action == "forwarded":
			if !present(in, top) || differs(top) {
				return fmt.Errorf("%s is listed forwarded but changed", c)
			}
			continue
		case action == "dropped":
			if present(out, top) && !again {
				return fmt.Errorf("%s is listed dropped but was sent", c)
			}
		case slices.Contains(group, top):
			if !slices.ContainsFunc(group, differs) {
				return fmt.Errorf("%s is listed but no member of the dialect changed", c)
			}
		case !differs(top):
			return fmt.Errorf("%s is listed but did not change", c)
		}
		names[top] = true
		effortListed = effortListed || slices.Contains(group, top)
	}
	for _, n := range slices.Concat(in.Keys(), out.Keys()) {
		if n != "model" && differs(n) && !names[n] && (!effortListed || !slices.Contains(group, n)) {
			return fmt.Errorf("%s changed but is not listed", n)
		}
	}
	return nil
}
