package plan

import (
	"bytes"
	"cmp"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/params"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var (
	openai = &domain.Provider{Protocol: domain.ProtocolOpenAI, BaseURL: "https://api.example.com/v1"}
	anth   = &domain.Provider{Protocol: domain.ProtocolAnthropic, BaseURL: "https://api.example.com/"}
)

// provider is the native provider of ep.
func provider(ep domain.Endpoint) *domain.Provider {
	if ep == domain.EndpointMessages {
		return anth
	}
	return openai
}

// run scans body with the inbound walk of ep, checks it against the alias m
// and builds it for target t of the native protocol, as the data plane does.
func run(t *testing.T, ep domain.Endpoint, body string, m *domain.Model, tg domain.Target) (*Outgoing, *Skip) {
	t.Helper()
	return runTo(t, ep, provider(ep), body, m, tg)
}

// runTo is run toward a target of provider p.
func runTo(t *testing.T, ep domain.Endpoint, p *domain.Provider, body string, m *domain.Model, tg domain.Target) (*Outgoing, *Skip) {
	t.Helper()
	s, _, _ := schema(ep)
	b, e := params.Scan([]byte(body), s.Walk)
	if e != nil {
		t.Fatalf("scan %s: %+v", body, e)
	}
	c, de := Check(ep, b, m)
	if de != nil {
		t.Fatalf("check %s: %+v", body, de)
	}
	if tg.Kind == "" {
		tg.Kind = domain.KindChat
	}
	return Build(Input{Endpoint: ep, Body: b, Model: m, Upstream: "up-1", Target: &tg, Provider: p, Client: c})
}

func val(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// chatBody and messagesBody are the golden requests, with the client effort
// effort when it is not empty.
func chatBody(effort string) string {
	e := ""
	if effort != "" {
		e = `,"reasoning_effort":"` + effort + `"`
	}
	return `{"model":"fast","messages":[{"role":"user","content":"hi"}]` + e + `}`
}

func messagesBody(effort string) string {
	e := ""
	if effort != "" {
		e = `,"effort":"` + effort + `"`
	}
	return `{"model":"fast","max_tokens":8192,"messages":[{"role":"user","content":"hi"}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}` + e + `}}`
}

var dialects = []string{"none", "reasoning_effort", "reasoning_object", "thinking_toggle", "enable_thinking", "thinking_budget", "adaptive_effort"}

func rm(s string) json.RawMessage { return json.RawMessage(s) }

// fragments are an on and an off fragment per dialect, as an operator would
// declare them.
var fragments = map[string][2]map[string]json.RawMessage{
	"none":             {{"temperature": rm(`1`)}, {"temperature": rm(`0`)}},
	"reasoning_effort": {{"temperature": rm(`1`)}, {"reasoning_effort": rm(`"minimal"`)}},
	"reasoning_object": {{"temperature": rm(`1`)}, {"reasoning": rm(`{"enabled":false}`)}},
	"thinking_toggle":  {{"thinking": rm(`{"type":"adaptive"}`)}, {"thinking": rm(`{"type":"disabled"}`), "temperature": rm(`0.6`)}},
	"enable_thinking":  {{"enable_thinking": rm(`true`), "thinking_budget": rm(`4096`)}, {"enable_thinking": rm(`false`), "temperature": rm(`0.7`)}},
	"thinking_budget":  {{"thinking": rm(`{"type":"enabled","display":"summarized"}`)}, {"thinking": rm(`{"type":"disabled"}`), "temperature": rm(`1`)}},
	"adaptive_effort":  {{"thinking": rm(`{"type":"adaptive","display":"summarized"}`)}, {"thinking": rm(`{"type":"between_tools"}`)}},
}

// reasoningFor is the target declaration of a golden case.
func reasoningFor(d, c string) *domain.Reasoning {
	r := &domain.Reasoning{Dialect: d, CanDisable: new(true)}
	switch d {
	case "none":
	case "thinking_toggle", "enable_thinking":
		r.Levels = []string{"on"}
	default:
		r.Levels = []string{"low", "medium", "high"}
	}
	if d == "thinking_budget" {
		r.Budgets = map[string]int64{"low": 1024, "medium": 2048, "high": 4096, "xhigh": 6144}
	}
	switch c {
	case "fragments":
		r.On, r.Off = fragments[d][0], fragments[d][1]
	case "map":
		r.Map = map[string]string{"minimal": "low", "max": "xhigh"}
		if d == "thinking_toggle" || d == "enable_thinking" {
			r.Map = map[string]string{"minimal": "none"}
		}
	case "no-levels":
		r.Levels, r.CanDisable = nil, nil
	case "cannot-disable":
		r.CanDisable = new(false)
	}
	return r
}

type entry struct {
	Level    string `json:"level"`
	Sent     string `json:"sent,omitempty"`
	Source   string `json:"source"`
	Adjusted bool   `json:"adjusted,omitzero"`
	Changes  string `json:"changes,omitempty"`
	Body     string `json:"body"`
}

// TestDialectGoldens is RSN-11 and AC-13 at unit level: per endpoint and
// dialect, the seven levels of the scale through the default forms, the
// fragments, a map, undeclared levels, can_disable false and both alias
// modes, in testdata/<endpoint>/<dialect>/<case>.json; chat toward an
// anthropic target, the translated path, in testdata/chat-anthropic, with
// the alias cap every anthropic target needs (PAR-7).
func TestDialectGoldens(t *testing.T) {
	cases := []string{"default", "fragments", "map", "no-levels", "cannot-disable", "alias-default", "alias-forced"}
	for _, dir := range []string{"chat", "messages", "chat-anthropic"} {
		ep, p, body := domain.Endpoint(dir), provider(domain.Endpoint(dir)), chatBody
		switch dir {
		case "messages":
			body = messagesBody
		case "chat-anthropic":
			ep, p = domain.EndpointChat, anth
		}
		for _, d := range dialects {
			for _, c := range cases {
				if d == "thinking_budget" && c == "no-levels" {
					continue // refused by validation (RSN-3)
				}
				var got []entry
				for _, l := range domain.Levels {
					m := &domain.Model{}
					if p == anth && ep == domain.EndpointChat {
						m.MaxOutputTokens = 8192
					}
					in := body(l)
					switch c {
					case "alias-default":
						m.Reasoning, in = domain.ModelReasoning{Effort: l, Mode: domain.ModeDefault}, body("")
					case "alias-forced":
						m.Reasoning, in = domain.ModelReasoning{Effort: l, Mode: domain.ModeForced}, body("medium")
					}
					out, skip := runTo(t, ep, p, in, m, domain.Target{Reasoning: reasoningFor(d, strings.TrimPrefix(c, "alias-"))})
					if skip != nil {
						t.Fatalf("%s %s %s %s: skipped %+v", ep, d, c, l, skip)
					}
					got = append(got, entry{l, val(out.Effort.Sent), out.EffortSource, out.EffortAdjusted, Header(out.Changes), string(out.Body)})
				}
				golden(t, filepath.Join("testdata", dir, d, c+".json"), got)
			}
		}
	}
}

func golden(t *testing.T, path string, v any) {
	t.Helper()
	got, err := jsonv2.Marshal(v, jsonv2.Deterministic(true), jsontext.Multiline(true))
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs (run the test with -update after a deliberate change):\n%s", path, got)
	}
}

// TestModes is the RSN-11 regression of AC-14: client with or without an
// effort, alias in default or forced mode, toward a levelled target and
// toward a none target, where the effort is dropped and listed.
func TestModes(t *testing.T) {
	levelled := domain.Target{Reasoning: &domain.Reasoning{Dialect: "reasoning_effort", Levels: []string{"low", "medium", "high"}, CanDisable: new(true)}}
	none := domain.Target{Reasoning: &domain.Reasoning{Dialect: "none"}}
	for _, tt := range []struct {
		client, mode string
		target       domain.Target
		sent, source string
		changes      string
		requested    string
	}{
		{"low", "default", levelled, "low", "client", "", "low"},
		{"", "default", levelled, "high", "alias_default", "reasoning_effort:added", ""},
		{"low", "forced", levelled, "high", "alias_forced", "reasoning_effort:forced", "low"},
		{"", "forced", levelled, "high", "alias_forced", "reasoning_effort:added", ""},
		{"low", "default", none, "", "provider_default", "reasoning_effort:dropped", "low"},
		{"", "default", none, "", "provider_default", "reasoning_effort:dropped", ""},
		{"low", "forced", none, "", "provider_default", "reasoning_effort:dropped", "low"},
		{"", "forced", none, "", "provider_default", "reasoning_effort:dropped", ""},
	} {
		m := &domain.Model{Reasoning: domain.ModelReasoning{Effort: "high", Mode: tt.mode}}
		out, skip := run(t, domain.EndpointChat, chatBody(tt.client), m, tt.target)
		if skip != nil {
			t.Fatal(skip)
		}
		if val(out.Effort.Sent) != tt.sent || out.EffortSource != tt.source || Header(out.Changes) != tt.changes ||
			val(out.Effort.Requested) != tt.requested || val(out.Effort.Alias) != "high" {
			t.Errorf("client %q mode %s dialect %s: sent %q source %s changes %q layers %q/%q; body %s", tt.client, tt.mode, tt.target.Reasoning.Dialect,
				val(out.Effort.Sent), out.EffortSource, Header(out.Changes), val(out.Effort.Requested), val(out.Effort.Alias), out.Body)
		}
		if tt.target.Reasoning.Dialect != "none" && !bytes.Contains(out.Body, []byte(`"reasoning_effort":"`+tt.sent+`"`)) {
			t.Errorf("client %q mode %s: the effort did not leave: %s", tt.client, tt.mode, out.Body)
		}
	}
}

// TestProviderDefault: an alias effort provider_default sends nothing, and in
// forced mode removes the client's effort.
func TestProviderDefault(t *testing.T) {
	tg := domain.Target{Reasoning: &domain.Reasoning{Dialect: "reasoning_effort"}}
	for _, mode := range []string{"default", "forced"} {
		m := &domain.Model{Reasoning: domain.ModelReasoning{Effort: "provider_default", Mode: mode}}
		out, _ := run(t, domain.EndpointChat, chatBody(""), m, tg)
		if string(out.Body) != `{"model":"up-1","messages":[{"role":"user","content":"hi"}]}` || out.EffortSource != "provider_default" || len(out.Changes) != 0 {
			t.Errorf("%s: %s %s %v", mode, out.Body, out.EffortSource, out.Changes)
		}
	}
	m := &domain.Model{Reasoning: domain.ModelReasoning{Effort: "provider_default", Mode: "forced"}}
	out, _ := run(t, domain.EndpointChat, chatBody("high"), m, tg)
	if string(out.Body) != `{"model":"up-1","messages":[{"role":"user","content":"hi"}]}` || Header(out.Changes) != "reasoning_effort:dropped" {
		t.Errorf("forced over a client effort: %s %v", out.Body, out.Changes)
	}
	// On messages the client's thinking is its effort (RSN-1), and goes too.
	adaptive := domain.Target{Reasoning: &domain.Reasoning{Dialect: "adaptive_effort", Levels: []string{"low", "medium", "high"}, CanDisable: new(true)}}
	const head = `{"model":"a","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]`
	for _, tt := range []struct{ body, requested, changes string }{
		{head + `,"thinking":{"type":"enabled","budget_tokens":2048}}`, "budget:2048", "thinking:dropped"},
		{head + `,"thinking":{"type":"disabled"}}`, "none", "thinking:dropped"},
		{head + `,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`, "high", "output_config.effort:dropped,thinking:dropped"},
	} {
		out, skip := run(t, domain.EndpointMessages, tt.body, m, adaptive)
		if skip != nil || string(out.Body) != `{"model":"up-1","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}` || Header(out.Changes) != tt.changes ||
			val(out.Effort.Requested) != tt.requested || out.Effort.Sent != nil || out.EffortSource != "provider_default" {
			t.Errorf("messages %s: %+v %+v", tt.body, skip, out)
		}
	}
}

// TestCaps is the PAR-6 table of AC-10: a cap is sent only from the client or
// the alias, never lowered, under the client's own names or the target's
// output_cap_param toward openai and as max_tokens toward anthropic.
func TestCaps(t *testing.T) {
	const msgs = `"messages":[{"role":"user","content":"hi"}]`
	for _, tt := range []struct {
		name       string
		ep         domain.Endpoint
		body       string
		alias      int64
		target     domain.Target
		want       string // the outgoing body
		changes    string
		req, sent  int64 // -1 for none
		skip       string
		aliasLayer int64
	}{
		{"no cap anywhere", "chat", `{"model":"a",` + msgs + `}`, 0, domain.Target{},
			`{"model":"up-1",` + msgs + `}`, "", -1, -1, "", 0},
		{"alias cap under max_tokens", "chat", `{"model":"a",` + msgs + `}`, 1000, domain.Target{},
			`{"model":"up-1",` + msgs + `,"max_tokens":1000}`, "max_tokens:added", -1, 1000, "", 1000},
		{"alias cap under output_cap_param", "chat", `{"model":"a",` + msgs + `}`, 1000, domain.Target{OutputCapParam: "max_completion_tokens"},
			`{"model":"up-1",` + msgs + `,"max_completion_tokens":1000}`, "max_completion_tokens:added", -1, 1000, "", 1000},
		{"client cap kept under its name", "chat", `{"model":"a",` + msgs + `,"max_tokens":500}`, 1000, domain.Target{OutputCapParam: "max_completion_tokens"},
			`{"model":"up-1",` + msgs + `,"max_tokens":500}`, "", 500, 500, "", 1000},
		{"client cap equal to the alias cap", "chat", `{"model":"a",` + msgs + `,"max_completion_tokens":1000}`, 1000, domain.Target{},
			`{"model":"up-1",` + msgs + `,"max_completion_tokens":1000}`, "", 1000, 1000, "", 1000},
		{"both names: the larger is requested, both forwarded", "chat", `{"model":"a",` + msgs + `,"max_tokens":900,"max_completion_tokens":300}`, 1000, domain.Target{},
			`{"model":"up-1",` + msgs + `,"max_tokens":900,"max_completion_tokens":300}`, "", 900, 900, "", 1000},
		{"client cap above the target limit, no alias cap", "chat", `{"model":"a",` + msgs + `,"max_tokens":9000}`, 0, domain.Target{MaxOutputTokens: 8192},
			"", "", 0, 0, "max_output_tokens_exceeded", 0},
		{"client cap at the target limit", "chat", `{"model":"a",` + msgs + `,"max_tokens":8192}`, 0, domain.Target{MaxOutputTokens: 8192},
			`{"model":"up-1",` + msgs + `,"max_tokens":8192}`, "", 8192, 8192, "", 0},
		{"zero is a cap", "messages", `{"model":"a","max_tokens":0,` + msgs + `}`, 1000, domain.Target{OutputCapParam: "max_completion_tokens"},
			`{"model":"up-1","max_tokens":0,` + msgs + `}`, "", 0, 0, "", 1000},
		{"alias cap toward anthropic is max_tokens", "messages", `{"model":"a",` + msgs + `}`, 1000, domain.Target{OutputCapParam: "max_completion_tokens"},
			`{"model":"up-1",` + msgs + `,"max_tokens":1000}`, "max_tokens:added", -1, 1000, "", 1000},
		{"alias cap under a custom output_cap_param", "chat", `{"model":"a",` + msgs + `}`, 77, domain.Target{OutputCapParam: "max_new_tokens"},
			`{"model":"up-1",` + msgs + `,"max_new_tokens":77}`, "max_new_tokens:added", -1, 77, "", 77},
		{"client cap dropped by PAR-12: the client's cap under output_cap_param, not the alias cap", "chat", `{"model":"a",` + msgs + `,"max_tokens":500}`, 1000,
			domain.Target{OutputCapParam: "max_completion_tokens", UnsupportedParams: []string{"max_tokens"}},
			`{"model":"up-1",` + msgs + `,"max_completion_tokens":500}`, "max_tokens:dropped,max_completion_tokens:translated", 500, 500, "", 1000},
		{"client cap dropped by PAR-12, no alias cap: still checked against the target limit", "chat", `{"model":"a",` + msgs + `,"max_tokens":9000}`, 0,
			domain.Target{MaxOutputTokens: 8192, OutputCapParam: "max_completion_tokens", UnsupportedParams: []string{"max_tokens"}},
			"", "", 0, 0, "max_output_tokens_exceeded", 0},
		{"client cap dropped by PAR-12, no alias cap, within the target limit", "chat", `{"model":"a",` + msgs + `,"max_tokens":500}`, 0,
			domain.Target{MaxOutputTokens: 8192, UnsupportedParams: []string{"max_tokens"}, OutputCapParam: "max_completion_tokens"},
			`{"model":"up-1",` + msgs + `,"max_completion_tokens":500}`, "max_tokens:dropped,max_completion_tokens:translated", 500, 500, "", 0},
		{"client cap dropped by PAR-12 under the target's own cap name: written again", "chat", `{"model":"a",` + msgs + `,"max_tokens":500,"seed":1}`, 1000,
			domain.Target{UnsupportedParams: []string{"max_tokens"}},
			`{"model":"up-1",` + msgs + `,"seed":1,"max_tokens":500}`, "max_tokens:dropped,max_tokens:translated", 500, 500, "", 1000},
		{"one client cap member dropped by PAR-12: the other is forwarded", "chat", `{"model":"a",` + msgs + `,"max_tokens":900,"max_completion_tokens":300}`, 1000,
			domain.Target{UnsupportedParams: []string{"max_tokens"}},
			`{"model":"up-1",` + msgs + `,"max_completion_tokens":300}`, "max_tokens:dropped", 900, 300, "", 1000},
		{"one client cap member dropped by PAR-12, no alias cap: the requested cap is checked", "chat", `{"model":"a",` + msgs + `,"max_tokens":900,"max_completion_tokens":300}`, 0,
			domain.Target{MaxOutputTokens: 500, UnsupportedParams: []string{"max_tokens"}},
			"", "", 0, 0, "max_output_tokens_exceeded", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// The drop rule acts only toward a target that lists max_tokens as unsupported (PAR-12).
			m := &domain.Model{MaxOutputTokens: tt.alias, Params: map[string]string{"max_tokens": "drop"}}
			out, skip := run(t, tt.ep, tt.body, m, tt.target)
			if tt.skip != "" {
				if skip == nil || skip.Code != tt.skip {
					t.Fatalf("got %+v, want skip %s", skip, tt.skip)
				}
				return
			}
			if skip != nil {
				t.Fatal(skip)
			}
			layer := func(p *int64) int64 {
				if p == nil {
					return -1
				}
				return *p
			}
			if string(out.Body) != tt.want || Header(out.Changes) != tt.changes || layer(out.MaxOutput.Requested) != tt.req ||
				layer(out.MaxOutput.Sent) != tt.sent || max(layer(out.MaxOutput.Alias), 0) != tt.aliasLayer {
				t.Errorf("got %s %q layers %d/%d/%d", out.Body, Header(out.Changes), layer(out.MaxOutput.Requested), layer(out.MaxOutput.Alias), layer(out.MaxOutput.Sent))
			}
		})
	}
}

// TestSkips is the Build skip table: each static exclusion with its code and
// parameter, and the drop rule that turns PAR-12 into a listed drop.
func TestSkips(t *testing.T) {
	const msgs = `"messages":[{"role":"user","content":"hi"}]`
	no := new(false)
	for _, tt := range []struct {
		name   string
		ep     domain.Endpoint
		body   string
		m      *domain.Model
		target domain.Target
		p      *domain.Provider
		code   string
		param  string
	}{
		{"messages toward openai", "messages", `{"model":"a","max_tokens":1,` + msgs + `}`, nil, domain.Target{}, openai, "endpoint_not_supported", ""},
		{"a chat stream toward anthropic until C18", "chat", `{"model":"a",` + msgs + `,"stream":true}`, nil, domain.Target{}, anth, "endpoint_not_supported", "stream"},
		{"chat toward an embeddings target", "chat", `{"model":"a",` + msgs + `}`, nil, domain.Target{Kind: domain.KindEmbeddings}, openai, "endpoint_not_supported", ""},
		{"tools", "chat", `{"model":"a",` + msgs + `,"tool_choice":"none"}`, nil, domain.Target{Capabilities: domain.Capabilities{Tools: no}}, openai, "capability_mismatch", "tools"},
		{"structured output", "chat", `{"model":"a",` + msgs + `,"response_format":{"type":"json_schema","json_schema":{}}}`, nil,
			domain.Target{Capabilities: domain.Capabilities{StructuredOutput: no}}, openai, "capability_mismatch", "structured_output"},
		{"strict schema", "messages", `{"model":"a","max_tokens":1,` + msgs + `,"tools":[{"name":"f","strict":true}]}`, nil,
			domain.Target{Capabilities: domain.Capabilities{StrictSchema: no}}, anth, "capability_mismatch", "strict_schema"},
		{"vision", "messages", `{"model":"a","max_tokens":1,"messages":[{"role":"user","content":[{"type":"image"}]}]}`, nil,
			domain.Target{Capabilities: domain.Capabilities{Vision: no}}, anth, "capability_mismatch", "vision"},
		{"unsupported known parameter", "chat", `{"model":"a",` + msgs + `,"seed":1}`, nil, domain.Target{UnsupportedParams: []string{"seed"}}, openai, "unsupported_parameter", "seed"},
		{"unsupported forwarded parameter", "chat", `{"model":"a",` + msgs + `,"x_tag":1}`, &domain.Model{Params: map[string]string{"x_tag": "forward"}},
			domain.Target{UnsupportedParams: []string{"x_tag"}}, openai, "unsupported_parameter", "x_tag"},
		{"client cap above the target", "chat", `{"model":"a",` + msgs + `,"max_completion_tokens":10}`, nil, domain.Target{MaxOutputTokens: 9}, openai, "max_output_tokens_exceeded", ""},
		{"client budget toward another dialect", "messages", `{"model":"a","max_tokens":4096,` + msgs + `,"thinking":{"type":"enabled","budget_tokens":2048}}`, nil,
			domain.Target{Reasoning: &domain.Reasoning{Dialect: "adaptive_effort"}}, anth, "capability_mismatch", "thinking.budget_tokens"},
		{"client budget toward none", "messages", `{"model":"a","max_tokens":4096,` + msgs + `,"thinking":{"type":"enabled","budget_tokens":2048}}`, nil,
			domain.Target{}, anth, "capability_mismatch", "thinking.budget_tokens"},
		// Chat toward anthropic: keys with no equivalent and untranslatable
		// content (PAR-12, design section 7), unless a drop rule removes the key.
		{"no equivalent toward anthropic", "chat", `{"model":"a",` + msgs + `,"temperature":1,"seed":1}`, nil, domain.Target{}, anth, "unsupported_parameter", "seed"},
		{"n above 1 toward anthropic", "chat", `{"model":"a",` + msgs + `,"n":2}`, nil, domain.Target{}, anth, "unsupported_parameter", "n"},
		{"json_object toward anthropic", "chat", `{"model":"a",` + msgs + `,"response_format":{"type":"json_object"}}`, nil, domain.Target{}, anth, "unsupported_parameter", "response_format"},
		{"a drop rule on another key does not help", "chat", `{"model":"a",` + msgs + `,"logprobs":true,"user":"u"}`, &domain.Model{Params: map[string]string{"user": "drop"}},
			domain.Target{}, anth, "unsupported_parameter", "logprobs"},
		{"a message name toward anthropic", "chat", `{"model":"a","messages":[{"role":"user","name":"ann","content":"hi"}]}`, nil, domain.Target{}, anth,
			"unsupported_parameter", "messages[0].name"},
		{"an audio part toward anthropic", "chat", `{"model":"a","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}]}`, nil, domain.Target{}, anth,
			"unsupported_parameter", "messages[0].content[0]"},
		{"unsupported known parameter toward anthropic", "chat", `{"model":"a",` + msgs + `,"temperature":1}`, nil, domain.Target{UnsupportedParams: []string{"temperature"}}, anth,
			"unsupported_parameter", "temperature"},
		{"PAR-4 before the translation", "chat", `{"model":"a","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}]}`, nil,
			domain.Target{Capabilities: domain.Capabilities{Vision: no}}, anth, "capability_mismatch", "vision"},
		{"client cap above the target toward anthropic", "chat", `{"model":"a",` + msgs + `,"max_tokens":10}`, nil, domain.Target{MaxOutputTokens: 9}, anth, "max_output_tokens_exceeded", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, _, _ := schema(tt.ep)
			b, e := params.Scan([]byte(tt.body), s.Walk)
			if e != nil {
				t.Fatal(e)
			}
			c, de := Check(tt.ep, b, tt.m)
			if de != nil {
				t.Fatal(de)
			}
			tt.target.Kind = cmp.Or(tt.target.Kind, domain.KindChat)
			out, skip := Build(Input{Endpoint: tt.ep, Body: b, Model: tt.m, Upstream: "up-1", Target: &tt.target, Provider: tt.p, Client: c})
			if out != nil || skip == nil || skip.Code != tt.code || skip.Param != tt.param {
				t.Errorf("got %+v, %+v; want %s %s", out, skip, tt.code, tt.param)
			}
		})
	}
}

// TestTranslated is chat toward an anthropic target through Build: the new
// body of the adapter, listed protocol:translated once and first, then the
// alias's rules, the cap and the reasoning edit on that body; a key with no
// equivalent is dropped under a drop rule (PAR-12); a forwarded member is
// copied byte for byte; every sent value is read back from the result.
func TestTranslated(t *testing.T) {
	const msgs = `"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]`
	const sent = `"system":[{"type":"text","text":"be brief"}],"messages":[{"role":"user","content":"hi"}]`
	adaptive := domain.Target{Reasoning: &domain.Reasoning{Dialect: "adaptive_effort", Levels: []string{"low", "medium", "high"}, CanDisable: new(true)}}
	budget := domain.Target{Reasoning: &domain.Reasoning{Dialect: "thinking_budget", Levels: []string{"low", "high"}, Budgets: map[string]int64{"low": 1024, "high": 4096}}}
	effort := domain.Target{Reasoning: &domain.Reasoning{Dialect: "reasoning_effort"}}
	high := domain.ModelReasoning{Effort: "high", Mode: "default"}
	for _, tt := range []struct {
		name, body string
		m          domain.Model
		target     domain.Target
		want       string
		changes    string
		effort     string
		cap        int64
	}{
		{"alias cap and effort added", `{"model":"a",` + msgs + `}`, domain.Model{MaxOutputTokens: 1000, Reasoning: high}, adaptive,
			`{"model":"up-1",` + sent + `,"max_tokens":1000,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`,
			"protocol:translated,max_tokens:added,output_config.effort:added", "high", 1000},
		{"the client's effort and cap, with structured output", `{"model":"a",` + msgs + `,"reasoning_effort":"low","max_completion_tokens":300,"max_tokens":200,` +
			`"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object"}}}}`, domain.Model{MaxOutputTokens: 1000, Reasoning: high}, adaptive,
			`{"model":"up-1","max_tokens":300,` + sent + `,"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}},"effort":"low"},"thinking":{"type":"adaptive"}}`,
			"protocol:translated,reasoning_effort:translated", "low", 300},
		{"a budget for the client's effort", `{"model":"a",` + msgs + `,"reasoning_effort":"medium","max_tokens":8000}`, domain.Model{}, budget,
			`{"model":"up-1","max_tokens":8000,` + sent + `,"thinking":{"type":"enabled","budget_tokens":1024}}`,
			"protocol:translated,reasoning_effort:translated", "budget:1024", 8000},
		{"the client's effort kept toward a reasoning_effort dialect", `{"model":"a",` + msgs + `,"reasoning_effort":"low","max_tokens":10}`, domain.Model{}, effort,
			`{"model":"up-1","max_tokens":10,` + sent + `,"reasoning_effort":"low"}`, "protocol:translated", "low", 10},
		{"an effort toward none is dropped", `{"model":"a",` + msgs + `,"reasoning_effort":"low","max_tokens":10}`, domain.Model{}, domain.Target{},
			`{"model":"up-1","max_tokens":10,` + sent + `}`, "protocol:translated,reasoning_effort:dropped", "", 10},
		{"drop rules on keys with no equivalent, a forward rule", `{"model":"a","seed":7,` + msgs + `,"x_tag":{"b":[1, 2]},"user":"u","temperature":0.5,"max_tokens":10}`,
			domain.Model{Params: map[string]string{"seed": "drop", "user": "drop", "x_tag": "forward", "temperature": "drop"}}, domain.Target{},
			`{"model":"up-1","max_tokens":10,` + sent + `,"temperature":0.5,"x_tag":{"b":[1, 2]}}`, "protocol:translated,seed:dropped,x_tag:forwarded,user:dropped", "", 10},
		{"an unsupported key dropped", `{"model":"a",` + msgs + `,"temperature":0.5,"top_p":1,"max_tokens":10}`,
			domain.Model{Params: map[string]string{"temperature": "drop"}}, domain.Target{UnsupportedParams: []string{"temperature"}},
			`{"model":"up-1","max_tokens":10,` + sent + `,"top_p":1}`, "protocol:translated,temperature:dropped", "", 10},
		{"the client's cap dropped: sent as max_tokens again", `{"model":"a",` + msgs + `,"max_tokens":500}`,
			domain.Model{MaxOutputTokens: 1000, Params: map[string]string{"max_tokens": "drop"}}, domain.Target{UnsupportedParams: []string{"max_tokens"}},
			`{"model":"up-1",` + sent + `,"max_tokens":500}`, "protocol:translated,max_tokens:dropped,max_tokens:translated", "", 500},
		{"untranslatable content in dropped members", `{"model":"a",` + msgs + `,"response_format":{"type":"json_schema","json_schema":{"name":"r"}},` +
			`"tools":[{"type":"function","function":{"description":"d"}}],"max_tokens":10}`,
			domain.Model{Params: map[string]string{"response_format": "drop", "tools": "drop"}}, domain.Target{UnsupportedParams: []string{"response_format", "tools"}},
			`{"model":"up-1","max_tokens":10,` + sent + `}`, "protocol:translated,response_format:dropped,tools:dropped", "", 10},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, skip := runTo(t, domain.EndpointChat, anth, tt.body, &tt.m, tt.target)
			if skip != nil {
				t.Fatal(skip)
			}
			if string(out.Body) != tt.want || Header(out.Changes) != tt.changes || val(out.Effort.Sent) != tt.effort || out.MaxOutput.Sent == nil || *out.MaxOutput.Sent != tt.cap {
				t.Errorf("got %s %q effort %q cap %v", out.Body, Header(out.Changes), val(out.Effort.Sent), out.MaxOutput.Sent)
			}
			if out.URL != "https://api.example.com/v1/messages" || out.Header.Get("anthropic-version") != "2023-06-01" || len(out.Header) != 2 {
				t.Errorf("%s %v", out.URL, out.Header)
			}
		})
	}
}

// TestRules: drop and forward rules (PAR-2) and a PAR-12 drop, listed in body
// order before the cap; a drop rule on a supported known key changes nothing.
func TestRules(t *testing.T) {
	m := &domain.Model{MaxOutputTokens: 100, Params: map[string]string{"enable_search": "drop", "x_tag": "forward", "seed": "drop", "top_p": "drop"}}
	tg := domain.Target{UnsupportedParams: []string{"seed"}}
	out, skip := run(t, "chat", `{"model":"a","enable_search":true,"messages":[{"role":"user","content":"hi"}],"seed":7,"x_tag":{"a":1},"top_p":1}`, m, tg)
	if skip != nil {
		t.Fatal(skip)
	}
	const want = `{"model":"up-1","messages":[{"role":"user","content":"hi"}],"x_tag":{"a":1},"top_p":1,"max_tokens":100}`
	if string(out.Body) != want || Header(out.Changes) != "enable_search:dropped,seed:dropped,x_tag:forwarded,max_tokens:added" {
		t.Errorf("got %s %q", out.Body, Header(out.Changes))
	}

	// A drop rule on a key the dialect writes: the client's member is dropped
	// and the alias's effort is still written (RSN-5).
	high := domain.ModelReasoning{Effort: "high", Mode: "default"}
	m = &domain.Model{Reasoning: high, Params: map[string]string{"enable_thinking": "drop"}}
	tg = domain.Target{Reasoning: &domain.Reasoning{Dialect: "enable_thinking", Levels: []string{"on"}, CanDisable: new(true)}}
	out, _ = run(t, "chat", `{"model":"a","messages":[{"role":"user","content":"hi"}],"enable_thinking":true}`, m, tg)
	if string(out.Body) != `{"model":"up-1","messages":[{"role":"user","content":"hi"}],"enable_thinking":true}` ||
		Header(out.Changes) != "enable_thinking:dropped,enable_thinking:added" || val(out.Effort.Sent) != "on" || out.EffortSource != "alias_default" {
		t.Errorf("drop on a dialect key: %s %q %q %s", out.Body, Header(out.Changes), val(out.Effort.Sent), out.EffortSource)
	}

	// A fragment's side key the client sent with other bytes is forced (PAR-13).
	tg = domain.Target{Reasoning: &domain.Reasoning{Dialect: "reasoning_effort", On: map[string]json.RawMessage{"temperature": rm(`1`)}}}
	out, _ = run(t, "chat", `{"model":"a","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`, &domain.Model{Reasoning: high}, tg)
	if string(out.Body) != `{"model":"up-1","messages":[{"role":"user","content":"hi"}],"temperature":1,"reasoning_effort":"high"}` ||
		Header(out.Changes) != "reasoning_effort:added,temperature:forced" {
		t.Errorf("side key the client sent: %s %q", out.Body, Header(out.Changes))
	}
}

// TestQuoting: the upstream id and a level from the declarations are quoted as
// JSON, whatever characters a valid configuration lets them hold.
func TestQuoting(t *testing.T) {
	b, _ := params.Scan([]byte(chatBody("")), params.Chat.Walk)
	m := &domain.Model{Reasoning: domain.ModelReasoning{Effort: "high", Mode: "default"}}
	c, _ := Check("chat", b, m)
	tg := domain.Target{Kind: domain.KindChat, Reasoning: &domain.Reasoning{Dialect: "reasoning_effort", Map: map[string]string{"high": "h\a"}}}
	out, skip := Build(Input{Endpoint: "chat", Body: b, Model: m, Upstream: "p/m\a\U000E0001", Target: &tg, Provider: openai, Client: c})
	if skip != nil {
		t.Fatal(skip)
	}
	if string(out.Body) != `{"model":"p/m\u0007`+"\U000E0001"+`","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"h\u0007"}` {
		t.Errorf("got %s", out.Body)
	}
}

// TestURLAndHeaders: the URL per protocol, and PAR-14 on messages: the
// client's anthropic-version and anthropic-beta, else the pinned version.
func TestURLAndHeaders(t *testing.T) {
	out, _ := run(t, "chat", chatBody(""), nil, domain.Target{})
	if out.URL != "https://api.example.com/v1/chat/completions" || len(out.Header) != 1 || out.Header.Get("Content-Type") != "application/json" {
		t.Errorf("chat: %s %v", out.URL, out.Header)
	}
	s, _, _ := schema("messages")
	b, _ := params.Scan([]byte(messagesBody("")), s.Walk)
	c, _ := Check("messages", b, nil)
	tg := domain.Target{Kind: domain.KindChat}
	for _, tt := range []struct {
		in            http.Header
		version, beta string
	}{
		{nil, "2023-06-01", ""},
		{http.Header{"Anthropic-Version": {"2026-10-21"}, "Anthropic-Beta": {"a,b"}, "X-Other": {"x"}}, "2026-10-21", "a,b"},
	} {
		out, _ := Build(Input{Endpoint: "messages", Body: b, Header: tt.in, Upstream: "up-1", Target: &tg, Provider: anth, Client: c})
		if out.URL != "https://api.example.com/v1/messages" || out.Header.Get("anthropic-version") != tt.version ||
			out.Header.Get("anthropic-beta") != tt.beta || out.Header.Get("X-Other") != "" {
			t.Errorf("messages %v: %s %v", tt.in, out.URL, out.Header)
		}
	}
}

// TestMessagesThinking is RSN-1 with design section 15, point 10, on the
// native messages path.
func TestMessagesThinking(t *testing.T) {
	const head = `{"model":"a","max_tokens":8192,"messages":[{"role":"user","content":"hi"}]`
	const out = `{"model":"up-1","max_tokens":8192,"messages":[{"role":"user","content":"hi"}]`
	adaptive := domain.Target{Reasoning: &domain.Reasoning{Dialect: "adaptive_effort", Levels: []string{"low", "medium", "high"}, CanDisable: new(true)}}
	toggle := domain.Target{Reasoning: &domain.Reasoning{Dialect: "thinking_toggle", Levels: []string{"on"}, CanDisable: new(true)}}
	budget := domain.Target{Reasoning: &domain.Reasoning{Dialect: "thinking_budget", Levels: []string{"low"}, Budgets: map[string]int64{"low": 1024}}}
	high := domain.ModelReasoning{Effort: "high", Mode: "default"}
	// Targets that cannot disable reasoning, which validation pairs with an alias effort (RSN-7).
	toggleOn := domain.Target{Reasoning: &domain.Reasoning{Dialect: "thinking_toggle", Levels: []string{"on"}, CanDisable: new(false)}}
	budgetOn := domain.Target{Reasoning: &domain.Reasoning{Dialect: "thinking_budget", Levels: []string{"low"}, Budgets: map[string]int64{"low": 1024}, CanDisable: new(false)}}
	adaptiveOn := domain.Target{Reasoning: &domain.Reasoning{Dialect: "adaptive_effort", Levels: []string{"low", "high"}, CanDisable: new(false)}}
	low := domain.ModelReasoning{Effort: "low", Mode: "default"}
	forcedHigh := domain.ModelReasoning{Effort: "high", Mode: "forced"}
	enable := domain.Target{Reasoning: &domain.Reasoning{Dialect: "enable_thinking", Levels: []string{"on"}, CanDisable: new(true)}}
	levelled := domain.Target{Reasoning: &domain.Reasoning{Dialect: "reasoning_effort", Levels: []string{"low", "medium", "high"}, CanDisable: new(true)}}
	for _, tt := range []struct {
		name, body string
		alias      domain.ModelReasoning
		target     domain.Target
		want       string
		changes    string
		requested  string
		sent       string
		source     string
	}{
		{"disabled without an effort is none, already the off form", head + `,"thinking":{"type":"disabled"}}`, domain.ModelReasoning{}, adaptive,
			out + `,"thinking":{"type":"disabled"}}`, "", "none", "none", "client"},
		{"effort none replaces the client's thinking", head + `,"thinking":{"type":"adaptive"},"output_config":{"effort":"none"}}`, domain.ModelReasoning{}, adaptive,
			out + `,"thinking":{"type":"disabled"}}`, "output_config.effort:translated,thinking:translated", "none", "none", "client"},
		{"an effort above none keeps the client's thinking", head + `,"thinking":{"type":"enabled","budget_tokens":2048},"output_config":{"effort":"high"}}`, high, adaptive,
			out + `,"thinking":{"type":"enabled","budget_tokens":2048},"output_config":{"effort":"high"}}`, "", "high", "high", "client"},
		{"adaptive thinking without an effort is kept toward adaptive_effort", head + `,"thinking":{"type":"adaptive","display":"summarized"}}`, high, adaptive,
			out + `,"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"high"}}`, "output_config.effort:added", "", "high", "alias_default"},
		{"between_tools is kept in forced mode too", head + `,"thinking":{"type":"between_tools"}}`, domain.ModelReasoning{Effort: "low", Mode: "forced"}, adaptive,
			out + `,"thinking":{"type":"between_tools"},"output_config":{"effort":"low"}}`, "output_config.effort:added", "", "low", "alias_forced"},
		{"adaptive thinking is replaced toward another dialect", head + `,"thinking":{"type":"adaptive"}}`, high, toggle,
			out + `,"thinking":{"type":"enabled"}}`, "thinking:forced", "", "on", "alias_default"},
		{"a forced effort replaces the client's", head + `,"thinking":{"type":"disabled"}}`, domain.ModelReasoning{Effort: "medium", Mode: "forced"}, adaptive,
			out + `,"thinking":{"type":"adaptive"},"output_config":{"effort":"medium"}}`, "output_config.effort:forced,thinking:forced", "none", "medium", "alias_forced"},
		{"a client budget reaches thinking_budget unchanged", head + `,"thinking":{"type":"enabled","budget_tokens":2048}}`, high, budget,
			out + `,"thinking":{"type":"enabled","budget_tokens":2048}}`, "", "budget:2048", "budget:2048", "client"},
		{"an emptied output_config is deleted", head + `,"output_config":{"effort":"high"}}`, domain.ModelReasoning{}, toggle,
			out + `,"thinking":{"type":"enabled"}}`, "output_config.effort:translated", "high", "on", "client"},
		{"disabled toward a toggle that cannot disable is replaced by the on form", head + `,"thinking":{"type":"disabled"}}`, low, toggleOn,
			out + `,"thinking":{"type":"enabled"}}`, "output_config.effort:translated,thinking:translated", "none", "on", "client"},
		{"disabled toward a budget that cannot disable gets the lowest budget", head + `,"thinking":{"type":"disabled"}}`, low, budgetOn,
			out + `,"thinking":{"type":"enabled","budget_tokens":1024}}`, "output_config.effort:translated,thinking:translated", "none", "budget:1024", "client"},
		{"disabled toward adaptive_effort that cannot disable", head + `,"thinking":{"type":"disabled"}}`, low, adaptiveOn,
			out + `,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`, "output_config.effort:translated,thinking:translated", "none", "low", "client"},
		{"an effort a kept thinking does not carry is dropped", head + `,"thinking":{"type":"enabled"},"output_config":{"effort":"high"}}`, domain.ModelReasoning{}, budget,
			out + `,"thinking":{"type":"enabled"}}`, "output_config.effort:dropped", "high", "", "provider_default"},
		{"an effort a kept adaptive thinking does not carry toward a toggle", head + `,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`, domain.ModelReasoning{}, toggle,
			out + `,"thinking":{"type":"adaptive"}}`, "output_config.effort:dropped", "high", "", "provider_default"},
		// A forced effort replaces the client's control (RSN-1): a thinking the
		// form does not write is dropped, whatever the dialect.
		{"a forced effort drops a client budget toward enable_thinking", head + `,"thinking":{"type":"enabled","budget_tokens":2048}}`, forcedHigh, enable,
			out + `,"enable_thinking":true}`, "output_config.effort:forced,thinking:dropped", "budget:2048", "on", "alias_forced"},
		{"a forced effort drops a client budget toward reasoning_effort", head + `,"thinking":{"type":"enabled","budget_tokens":2048}}`, forcedHigh, levelled,
			out + `,"reasoning_effort":"high"}`, "output_config.effort:forced,thinking:dropped", "budget:2048", "high", "alias_forced"},
		{"a forced effort drops a client budget toward none", head + `,"thinking":{"type":"enabled","budget_tokens":2048}}`, forcedHigh, domain.Target{},
			out + `}`, "output_config.effort:dropped,thinking:dropped", "budget:2048", "", "provider_default"},
		{"a forced effort drops the client's effort and thinking toward reasoning_effort", head + `,"thinking":{"type":"enabled"},"output_config":{"effort":"low"}}`, forcedHigh, levelled,
			out + `,"reasoning_effort":"high"}`, "output_config.effort:forced,thinking:dropped", "low", "high", "alias_forced"},
		{"a forced budget is written into a new thinking when the on fragment writes none", head + `,"thinking":{"type":"disabled","budget_tokens":1024}}`, forcedHigh,
			domain.Target{Reasoning: &domain.Reasoning{Dialect: "thinking_budget", Levels: []string{"high"}, Budgets: map[string]int64{"high": 1024}, On: map[string]json.RawMessage{"temperature": rm(`1`)}}},
			out + `,"temperature":1,"thinking":{"budget_tokens":1024}}`, "output_config.effort:forced,thinking:dropped,temperature:added", "budget:1024", "budget:1024", "alias_forced"},
		// Settled in C13: an adaptive or between_tools thinking without a client
		// effort is replaced by the alias level's form in both modes (RSN-1); a
		// form that writes no thinking drops it, listed.
		{"adaptive thinking is dropped toward reasoning_effort", head + `,"thinking":{"type":"adaptive"}}`, high, levelled,
			out + `,"reasoning_effort":"high"}`, "reasoning_effort:added,thinking:dropped", "", "high", "alias_default"},
		{"between_tools is dropped toward enable_thinking in forced mode", head + `,"thinking":{"type":"between_tools"}}`, forcedHigh, enable,
			out + `,"enable_thinking":true}`, "enable_thinking:added,thinking:dropped", "", "on", "alias_forced"},
		{"adaptive thinking is dropped by the off form of reasoning_effort", head + `,"thinking":{"type":"adaptive","display":"summarized"}}`, domain.ModelReasoning{Effort: "none", Mode: "default"}, levelled,
			out + `,"reasoning_effort":"none"}`, "reasoning_effort:added,thinking:dropped", "", "none", "alias_default"},
		{"adaptive thinking is dropped toward none", head + `,"thinking":{"type":"adaptive"}}`, high, domain.Target{},
			out + `}`, "output_config.effort:dropped,thinking:dropped", "", "", "provider_default"},
		{"a budget goes into a new thinking in place of an adaptive one", head + `,"thinking":{"type":"adaptive"}}`, high,
			domain.Target{Reasoning: &domain.Reasoning{Dialect: "thinking_budget", Levels: []string{"high"}, Budgets: map[string]int64{"high": 1024}, On: map[string]json.RawMessage{"temperature": rm(`1`)}}},
			out + `,"temperature":1,"thinking":{"budget_tokens":1024}}`, "thinking.budget_tokens:added,thinking:dropped,temperature:added", "", "budget:1024", "alias_default"},
		{"without an alias level the adaptive thinking is forwarded", head + `,"thinking":{"type":"adaptive"}}`, domain.ModelReasoning{}, levelled,
			out + `,"thinking":{"type":"adaptive"}}`, "", "", "", "provider_default"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, skip := run(t, "messages", tt.body, &domain.Model{Reasoning: tt.alias}, tt.target)
			if skip != nil {
				t.Fatal(skip)
			}
			if string(got.Body) != tt.want || Header(got.Changes) != tt.changes || val(got.Effort.Requested) != tt.requested ||
				val(got.Effort.Sent) != tt.sent || got.EffortSource != tt.source {
				t.Errorf("got %s %q requested %q sent %q source %s", got.Body, Header(got.Changes), val(got.Effort.Requested), val(got.Effort.Sent), got.EffortSource)
			}
		})
	}
}

// TestOneTarget is AC-16 at unit level: two aliases share a target, and a
// change to one leaves the body the other sends byte-identical (PAR-11);
// every level the model declares is reached through that one target (RSN-8).
func TestOneTarget(t *testing.T) {
	tg := domain.Target{Reasoning: &domain.Reasoning{Dialect: "reasoning_object", Levels: domain.Levels[1:], CanDisable: new(true)},
		Capabilities: domain.Capabilities{Tools: new(true)}}
	a := &domain.Model{Reasoning: domain.ModelReasoning{Effort: "low", Mode: "default"}, MaxOutputTokens: 500}
	b := &domain.Model{Reasoning: domain.ModelReasoning{Effort: "high", Mode: "default"}}
	body := `{"model":"x","messages":[{"role":"user","content":"hi"}],"tools":[]}`
	before, _ := run(t, "chat", body, a, tg)
	run(t, "chat", body, b, tg)
	*b = domain.Model{Reasoning: domain.ModelReasoning{Effort: "max", Mode: "forced"}, MaxOutputTokens: 9, Params: map[string]string{"tools": "drop"}}
	run(t, "chat", body, b, tg)
	after, _ := run(t, "chat", body, a, tg)
	if !bytes.Equal(before.Body, after.Body) || Header(before.Changes) != Header(after.Changes) {
		t.Errorf("alias a changed: %s, then %s", before.Body, after.Body)
	}
	for _, l := range domain.Levels {
		b.Reasoning = domain.ModelReasoning{Effort: l, Mode: "forced"}
		out, _ := run(t, "chat", body, b, tg)
		if val(out.Effort.Sent) != l || out.EffortAdjusted || !bytes.Contains(out.Body, []byte(`"reasoning":{"effort":"`+l+`"}`)) {
			t.Errorf("level %s: sent %q adjusted %v: %s", l, val(out.Effort.Sent), out.EffortAdjusted, out.Body)
		}
	}
}

// TestCheck is the Check refusal table: PAR-2, the types the proxy reads
// (PAR-1), the effort (RSN-11: the empty string, default and an unknown
// value, besides other non-levels) and the alias cap (PAR-6).
func TestCheck(t *testing.T) {
	const msgs = `"messages":[{"role":"user","content":"hi"}]`
	rules := &domain.Model{MaxOutputTokens: 1000, Params: map[string]string{"x_tag": "forward"}}
	for _, tt := range []struct {
		ep          domain.Endpoint
		body        string
		code, param string
		status      int
	}{
		{"chat", `{"model":"a",` + msgs + `,"enable_search":true}`, "unknown_parameter", "enable_search", 400},
		{"messages", `{"model":"a","max_tokens":1,` + msgs + `,"reasoning_effort":"low"}`, "unknown_parameter", "reasoning_effort", 400},
		{"chat", `{"model":"a",` + msgs + `,"stream":"yes"}`, "invalid_request", "stream", 400},
		{"chat", `{"model":"a",` + msgs + `,"stream_options":[]}`, "invalid_request", "stream_options", 400},
		{"chat", `{"model":"a",` + msgs + `,"stream_options":{"include_usage":1}}`, "invalid_request", "stream_options.include_usage", 400},
		{"chat", `{"model":"a"}`, "invalid_request", "messages", 400},
		{"chat", `{"model":"a","messages":[]}`, "invalid_request", "messages", 400},
		{"chat", `{"model":"a","messages":{}}`, "invalid_request", "messages", 400},
		{"chat", `{"model":"a","messages":[{"role":"user"},"x"]}`, "invalid_request", "messages[1]", 400},
		{"messages", `{"model":"a","max_tokens":1,"messages":[{"content":"x"}]}`, "invalid_request", "messages[0].role", 400},
		{"chat", `{"model":"a",` + msgs + `,"max_tokens":-1}`, "invalid_request", "max_tokens", 400},
		{"chat", `{"model":"a",` + msgs + `,"max_completion_tokens":1.5}`, "invalid_request", "max_completion_tokens", 400},
		{"chat", `{"model":"a",` + msgs + `,"response_format":"json"}`, "invalid_request", "response_format", 400},
		{"chat", `{"model":"a",` + msgs + `,"response_format":{"type":null}}`, "invalid_request", "response_format.type", 400},
		{"messages", `{"model":"a","max_tokens":1,` + msgs + `,"thinking":"on"}`, "invalid_request", "thinking", 400},
		{"messages", `{"model":"a","max_tokens":1,` + msgs + `,"thinking":{"type":true}}`, "invalid_request", "thinking.type", 400},
		{"messages", `{"model":"a","max_tokens":1,` + msgs + `,"thinking":{"type":"enabled","budget_tokens":"1024"}}`, "invalid_request", "thinking.budget_tokens", 400},
		{"messages", `{"model":"a","max_tokens":1,` + msgs + `,"output_config":"x"}`, "invalid_request", "output_config", 400},
		{"chat", `{"model":"a",` + msgs + `,"reasoning_effort":""}`, "invalid_reasoning_effort", "reasoning_effort", 400},
		{"chat", `{"model":"a",` + msgs + `,"reasoning_effort":"default"}`, "invalid_reasoning_effort", "reasoning_effort", 400},
		{"chat", `{"model":"a",` + msgs + `,"reasoning_effort":"turbo"}`, "invalid_reasoning_effort", "reasoning_effort", 400},
		{"chat", `{"model":"a",` + msgs + `,"reasoning_effort":"High"}`, "invalid_reasoning_effort", "reasoning_effort", 400},
		{"chat", `{"model":"a",` + msgs + `,"reasoning_effort":null}`, "invalid_reasoning_effort", "reasoning_effort", 400},
		{"chat", `{"model":"a",` + msgs + `,"reasoning_effort":3}`, "invalid_reasoning_effort", "reasoning_effort", 400},
		{"messages", `{"model":"a","max_tokens":1,` + msgs + `,"output_config":{"effort":"provider_default"}}`, "invalid_reasoning_effort", "output_config.effort", 400},
		{"chat", `{"model":"a",` + msgs + `,"max_tokens":1001}`, "max_output_tokens_exceeded", "max_tokens", 422},
		{"chat", `{"model":"a",` + msgs + `,"max_tokens":10,"max_completion_tokens":1001}`, "max_output_tokens_exceeded", "max_completion_tokens", 422},
		{"messages", `{"model":"a","max_tokens":1001,` + msgs + `}`, "max_output_tokens_exceeded", "max_tokens", 422},
	} {
		s, _, _ := schema(tt.ep)
		b, se := params.Scan([]byte(tt.body), s.Walk)
		if se != nil {
			t.Fatalf("%s: %+v", tt.body, se)
		}
		c, e := Check(tt.ep, b, rules)
		if c != nil || e == nil || e.Code != tt.code || e.Param != tt.param || e.Status != tt.status {
			t.Errorf("%s %s: got %+v, want %s %s", tt.ep, tt.body, e, tt.code, tt.param)
		}
	}
}

// TestCheckReads: what Check hands Build, and the bodies it accepts.
func TestCheckReads(t *testing.T) {
	for _, tt := range []struct {
		ep   domain.Endpoint
		body string
		want Client
	}{
		{"chat", `{"model":"a","messages":[{"role":"user","content":"hi"}],"x_tag":1,"reasoning_effort":"high","stream":true,"stream_options":{"include_usage":true},` +
			`"max_tokens":5,"max_completion_tokens":7,"tools":[{"type":"function","function":{"name":"f","strict":true}}]}`,
			Client{Effort: "high", Cap: new(int64(7)), Stream: true, Usage: true, Features: params.Features{Tools: true, Strict: true}}},
		{"messages", `{"model":"a","max_tokens":0,"messages":[{"role":"user","content":[{"type":"image"}]}],"thinking":{"type":"enabled","budget_tokens":2048},"stream":false}`,
			Client{Effort: "budget:2048", Cap: new(int64(0)), Features: params.Features{Vision: true}}},
		{"messages", `{"model":"a","max_tokens":9,"messages":[{"role":"user","content":"x"}],"thinking":{"type":"disabled"},"output_config":{"effort":"minimal"}}`,
			Client{Effort: "minimal", Cap: new(int64(9))}},
		{"messages", `{"model":"a","max_tokens":9,"messages":[{"role":"user","content":"x"}],"thinking":{"type":"between_tools"}}`,
			Client{Cap: new(int64(9))}},
	} {
		s, _, _ := schema(tt.ep)
		b, se := params.Scan([]byte(strings.ReplaceAll(tt.body, `g`, `g`)), s.Walk)
		if se != nil {
			t.Fatal(se)
		}
		c, e := Check(tt.ep, b, &domain.Model{MaxOutputTokens: 10, Params: map[string]string{"x_tag": "forward"}})
		if e != nil {
			t.Fatalf("%s: %+v", tt.body, e)
		}
		if c.Effort != tt.want.Effort || *c.Cap != *tt.want.Cap || c.Stream != tt.want.Stream || c.Usage != tt.want.Usage || c.Features != tt.want.Features {
			t.Errorf("%s: got %+v cap %d", tt.body, c, *c.Cap)
		}
	}
	if _, e := Check("embeddings", nil, nil); e == nil || e.Code != "internal_error" {
		t.Errorf("an endpoint not served: %+v", e)
	}
}

// TestBudgetRewritten: a forced level toward thinking_budget replaces the
// client's thinking and writes the budget inside the new one, even when the
// client's budget had the same value.
func TestBudgetRewritten(t *testing.T) {
	tg := domain.Target{Reasoning: &domain.Reasoning{Dialect: "thinking_budget", Levels: []string{"high"}, Budgets: map[string]int64{"high": 4096}}}
	m := &domain.Model{Reasoning: domain.ModelReasoning{Effort: "high", Mode: "forced"}}
	out, _ := run(t, "messages", `{"model":"a","max_tokens":8192,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","display":"omitted","budget_tokens":4096}}`, m, tg)
	const want = `{"model":"up-1","max_tokens":8192,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":4096}}`
	if string(out.Body) != want || Header(out.Changes) != "output_config.effort:forced,thinking:forced" || val(out.Effort.Sent) != "budget:4096" {
		t.Errorf("got %s %q %q", out.Body, Header(out.Changes), val(out.Effort.Sent))
	}
}
