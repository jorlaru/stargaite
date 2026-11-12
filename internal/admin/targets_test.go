package admin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
)

type targetAnswer struct {
	config.Result
	Resource targetView
}

type modelAnswer struct {
	config.Result
	Resource modelView
}

func (f *fixture) addTarget(t *testing.T, body string) targetAnswer {
	t.Helper()
	var res targetAnswer
	decode(t, f.do(t, http.MethodPost, "/admin/v1/targets", []byte(body)), http.StatusCreated, &res)
	return res
}

func (f *fixture) addModel(t *testing.T, body string) modelAnswer {
	t.Helper()
	var res modelAnswer
	decode(t, f.do(t, http.MethodPost, "/admin/v1/models", []byte(body)), http.StatusCreated, &res)
	return res
}

// TestTargetIDs: a target id holds "/" after its provider, and its upstream
// part may hold more, encoded characters and dots; every route reads it
// whether the path carries it as is or escaped whole, as the CLI sends it.
func TestTargetIDs(t *testing.T) {
	f := newFixture(t)
	f.addProvider(t, `{"name":"ollama","allow_private_network":true}`)
	for _, id := range []string{
		"ollama/m",
		"ollama/qwen/qwen-3.6-flash",
		"ollama/a/b/c/d.e",
		"ollama/a%2Fb",
		"ollama/x%25y?z#w",
		"ollama/a/./b",
		"ollama/a//b",
		"ollama/a/../b",
		"ollama/..",
		"ollama/é",
	} {
		body, _ := json.Marshal(map[string]string{"id": id, "kind": "embeddings"})
		if res := f.addTarget(t, string(body)); res.Resource.ID != id || !slices.Equal(entries(res.Diff), []string{"target " + id + " created"}) {
			t.Errorf("%s: answer %+v", id, res)
		}
		paths := []string{"/admin/v1/targets/" + url.PathEscape(id)}
		if !strings.ContainsAny(id, "%?#") && !strings.Contains(id, "/.") && !strings.Contains(id, "//") {
			paths = append(paths, "/admin/v1/targets/"+id) // what the mux would clean or cut needs the escaped form
		}
		for _, path := range paths {
			var got targetView
			decode(t, f.do(t, http.MethodGet, path, nil), http.StatusOK, &got)
			if got.ID != id || got.Kind != domain.KindEmbeddings {
				t.Errorf("GET %s: %+v", path, got)
			}
			var res targetAnswer
			decode(t, f.do(t, http.MethodPatch, path, []byte(`{"max_output_tokens":10}`)), http.StatusOK, &res)
			if res.Resource.ID != id || res.Resource.MaxOutputTokens != 10 {
				t.Errorf("PATCH %s: %+v", path, res)
			}
		}
		var res config.Result
		decode(t, f.do(t, http.MethodDelete, paths[len(paths)-1], nil), http.StatusOK, &res)
		if !slices.Equal(entries(res.Diff), []string{"target " + id + " removed"}) {
			t.Errorf("DELETE %s: %+v", id, res)
		}
		if _, ok := f.cfg.Snapshot().Doc.Targets[id]; ok {
			t.Errorf("%s: still there", id)
		}
	}
	errorOf(t, f.do(t, http.MethodGet, "/admin/v1/targets/ollama%2Fnope", nil), http.StatusNotFound, domain.CodeNotFound)
	errorOf(t, f.do(t, http.MethodGet, "/admin/v1/targets/", nil), http.StatusNotFound, domain.CodeNotFound)
	errorOf(t, f.do(t, http.MethodPut, "/admin/v1/targets/ollama/m", nil), http.StatusMethodNotAllowed, domain.CodeInvalidRequest)
	errorOf(t, f.do(t, http.MethodGet, "/admin/v1/models/a/b", nil), http.StatusNotFound, domain.CodeNotFound)
}

// TestAddTarget: a new target starts from its provider's target_defaults
// (RSN-2), and the explicit members merge over them; null removes one.
func TestAddTarget(t *testing.T) {
	f := newFixture(t)
	f.addProvider(t, `{"name":"openrouter"}`)
	f.addProvider(t, `{"name":"openai"}`)
	f.addProvider(t, `{"name":"tei","base_url":"http://localhost:8080/v1","allow_private_network":true}`)
	for _, tc := range []struct {
		body string
		want domain.Target
	}{
		{`{"id":"openrouter/qwen/qwen-3.6-flash","kind":"chat"}`,
			domain.Target{Kind: "chat", OutputCapParam: "max_tokens", Reasoning: &domain.Reasoning{Dialect: domain.DialectReasoningObject}}},
		{`{"id":"openrouter/a","kind":"chat","output_cap_param":null,"reasoning":{"levels":["low","high"],"can_disable":true}}`,
			domain.Target{Kind: "chat", Reasoning: &domain.Reasoning{Dialect: domain.DialectReasoningObject, Levels: []string{"low", "high"}, CanDisable: new(true)}}},
		{`{"id":"openrouter/b","kind":"chat","reasoning":{"dialect":"reasoning_effort"}}`,
			domain.Target{Kind: "chat", OutputCapParam: "max_tokens", Reasoning: &domain.Reasoning{Dialect: domain.DialectReasoningEffort}}},
		{`{"id":"openai/gpt","kind":"chat"}`, domain.Target{Kind: "chat", OutputCapParam: "max_completion_tokens"}},
		// A provider without target_defaults gives nothing: the dialect stays undeclared, which is none.
		{`{"id":"tei/bge-m3","kind":"embeddings"}`, domain.Target{Kind: "embeddings"}},
	} {
		res := f.addTarget(t, tc.body)
		if got := f.cfg.Snapshot().Doc.Targets[res.Resource.ID]; !reflect.DeepEqual(got, tc.want) || !reflect.DeepEqual(res.Resource.Target, tc.want) {
			t.Errorf("%s: stored %+v, answer %+v", tc.body, got, res.Resource)
		}
	}
	// The defaults are copied once: changing the provider changes no target.
	decode(t, f.do(t, http.MethodPatch, "/admin/v1/providers/openrouter", []byte(`{"target_defaults":{"dialect":"none"}}`)), http.StatusOK, &providerAnswer{})
	if got := f.cfg.Snapshot().Doc.Targets["openrouter/qwen/qwen-3.6-flash"]; got.Reasoning.Dialect != domain.DialectReasoningObject {
		t.Errorf("the target follows the provider: %+v", got)
	}

	revision := f.cfg.Snapshot().Revision
	for _, tc := range []struct {
		body, code, audit string
		status            int
		findings          []string
	}{
		{`{"id":"openrouter/a","kind":"chat"}`, domain.CodeConflict, "targets add openrouter/a target openrouter/a refused:conflict", 409, nil},
		{`{"id":"nope/m","kind":"chat"}`, domain.CodeInvalidRequest, "targets add nope/m target nope/m refused:invalid_request", 400,
			[]string{"error unknown_provider targets/nope/m "}},
		{`{"id":"openrouter","kind":"chat"}`, domain.CodeInvalidRequest, "targets add openrouter target openrouter refused:invalid_request", 400,
			[]string{"error invalid_name targets/openrouter "}},
		{`{"id":"openrouter/a b","kind":"chat"}`, domain.CodeInvalidRequest, "targets add openrouter/a b target openrouter/a b refused:invalid_request", 400,
			[]string{"error invalid_name targets/openrouter/a b "}},
		{`{"id":"openrouter/c"}`, domain.CodeInvalidRequest, "targets add openrouter/c target openrouter/c refused:invalid_request", 400,
			[]string{"error required targets/openrouter/c kind"}},
		{`{"id":"openrouter/c","kind":"chat","reasoning":{"can_disable":false}}`, domain.CodeInvalidRequest, "targets add openrouter/c target openrouter/c refused:invalid_request", 400,
			[]string{"error levels_required targets/openrouter/c reasoning.levels"}},
		{`{"id":"openrouter/c","kind":"chat","reasoning":{"dialect":"thinking_budget","levels":["low"]}}`, domain.CodeInvalidRequest, "targets add openrouter/c target openrouter/c refused:invalid_request", 400,
			[]string{"error budgets_required targets/openrouter/c reasoning.budgets"}},
		{`{"id":"openrouter/c","kind":"chat","colour":"blue"}`, domain.CodeInvalidRequest, "targets add openrouter/c target openrouter/c refused:invalid_request", 400, nil},
		{`{"kind":"chat"}`, domain.CodeInvalidRequest, "", 400, nil},
		{`{"id":["openrouter/c"]}`, domain.CodeInvalidRequest, "", 400, nil},
	} {
		rows := f.count(t, "SELECT COUNT(*) FROM audit")
		e := errorOf(t, f.do(t, http.MethodPost, "/admin/v1/targets", []byte(tc.body)), tc.status, tc.code)
		for _, want := range tc.findings {
			if !slices.Contains(codes(e.Findings), want) {
				t.Errorf("%s: findings %q, want %q", tc.body, codes(e.Findings), want)
			}
		}
		if n := f.count(t, "SELECT COUNT(*) FROM audit"); (tc.audit == "") != (n == rows) || (tc.audit != "" && f.lastAudit(t) != tc.audit) {
			t.Errorf("%s: %d audit rows, last %q", tc.body, n-rows, f.lastAudit(t))
		}
	}
	if f.cfg.Snapshot().Revision != revision {
		t.Error("a refused request published")
	}
}

// TestSetTarget: a merge patch; a list is replaced whole, a map entry merges,
// and a reasoning fragment is replaced whole, as given.
func TestSetTarget(t *testing.T) {
	f := newFixture(t)
	f.addProvider(t, `{"name":"anthropic"}`)
	f.addTarget(t, `{"id":"anthropic/m","kind":"chat","reasoning":{"dialect":"thinking_budget","levels":["low","high"],"budgets":{"low":1024,"high":8192},`+
		`"on":{"thinking":{"type":"enabled","budget_tokens":1}},"off":{"thinking":{"type":"disabled"},"x":1}}}`)
	var res targetAnswer
	decode(t, f.do(t, http.MethodPatch, "/admin/v1/targets/anthropic%2Fm", []byte(`{"reasoning":{"levels":["low"],"budgets":{"high":null},`+
		`"on":{"thinking":{"type":"adaptive"}},"off":{"y":[1,2]}},"capabilities":{"tools":true}}`)), http.StatusOK, &res)
	want := domain.Target{Kind: "chat", Capabilities: domain.Capabilities{Tools: new(true)}, Reasoning: &domain.Reasoning{
		Dialect: domain.DialectThinkingBudget, Levels: []string{"low"}, Budgets: map[string]int64{"low": 1024},
		On:  map[string]json.RawMessage{"thinking": json.RawMessage(`{"type":"adaptive"}`)},
		Off: map[string]json.RawMessage{"y": json.RawMessage(`[1,2]`)},
	}}
	got := f.cfg.Snapshot().Doc.Targets["anthropic/m"]
	if mustEncode(t, got) != mustEncode(t, want) || mustEncode(t, res.Resource.Target) != mustEncode(t, want) || !slices.Equal(entries(res.Diff), []string{"target anthropic/m updated"}) {
		t.Errorf("stored %s, answer %+v", mustEncode(t, got), res)
	}
	for _, tc := range []struct {
		target, body string
		status       int
		code, audit  string
	}{
		{"anthropic%2Fnope", `{}`, 404, domain.CodeNotFound, "targets set anthropic/nope target anthropic/nope refused:not_found"},
		{"anthropic%2Fm", `{"kind":"image"}`, 400, domain.CodeInvalidRequest, "targets set anthropic/m target anthropic/m refused:invalid_request"},
		{"anthropic%2Fm", `{"reasoning":"x"}`, 400, domain.CodeInvalidRequest, "targets set anthropic/m target anthropic/m refused:invalid_request"},
		{"anthropic%2Fm", `{"id":"anthropic/n"}`, 400, domain.CodeInvalidRequest, "targets set anthropic/m target anthropic/m refused:invalid_request"},
	} {
		errorOf(t, f.do(t, http.MethodPatch, "/admin/v1/targets/"+tc.target, []byte(tc.body)), tc.status, tc.code)
		if f.lastAudit(t) != tc.audit {
			t.Errorf("%s: audit %q", tc.body, f.lastAudit(t))
		}
	}
}

// TestReasoningFragments: a reasoning fragment sent by command is stored as
// given, as apply stores it from a file (kick-off section 13.3): the nulls
// inside it are kept, targets set replaces the stored fragment instead of
// joining the two, {} declares "send nothing" and a null fragment is removed.
func TestReasoningFragments(t *testing.T) {
	f := newFixture(t)
	f.addProvider(t, `{"name":"anthropic"}`)
	file := string(f.export(t)) + `targets:
  anthropic/m:
    kind: chat
    reasoning:
      dialect: adaptive_effort
      "on": {thinking: {type: enabled, budget_tokens: null}, x: null}
      "off": {}
`
	f.addTarget(t, `{"id":"anthropic/m","kind":"chat","reasoning":{"on":{"thinking": {"type": "enabled","budget_tokens":null},"x":null},"off":{}}}`)
	applied := newFixture(t)
	applied.apply(t, "", []byte(file))
	got, want := f.cfg.Snapshot().Doc.Targets["anthropic/m"], applied.cfg.Snapshot().Doc.Targets["anthropic/m"]
	if !reflect.DeepEqual(got, want) || want.Reasoning == nil || len(want.Reasoning.On) != 2 || want.Reasoning.Off == nil {
		t.Errorf("targets add stored %s, apply %s", mustEncode(t, got), mustEncode(t, want))
	}
	for _, tc := range []struct{ patch, want string }{
		{`{"reasoning":{"off":{"reasoning":{"enabled":false},"y":null}}}`,
			`{"dialect":"adaptive_effort","on":{"thinking":{"budget_tokens":null,"type":"enabled"},"x":null},"off":{"reasoning":{"enabled":false},"y":null}}`},
		{`{"reasoning":{"on":{"reasoning":{"enabled":true}},"off":{}}}`, `{"dialect":"adaptive_effort","on":{"reasoning":{"enabled":true}},"off":{}}`},
		{`{"reasoning":{"on":null}}`, `{"dialect":"adaptive_effort","off":{}}`},
	} {
		var res targetAnswer
		decode(t, f.do(t, http.MethodPatch, "/admin/v1/targets/anthropic%2Fm", []byte(tc.patch)), http.StatusOK, &res)
		stored := f.cfg.Snapshot().Doc.Targets["anthropic/m"].Reasoning
		if !res.Changed || mustEncode(t, stored) != tc.want || mustEncode(t, res.Resource.Reasoning) != tc.want {
			t.Errorf("%s: changed %v, stored %s, answer %s", tc.patch, res.Changed, mustEncode(t, stored), mustEncode(t, res.Resource.Reasoning))
		}
	}
}

// TestAddModel: an alias names its targets; models add creates a missing one
// in the same change, as kind chat with its provider's target_defaults, and
// reports it in the diff (kick-off section 13.3).
func TestAddModel(t *testing.T) {
	f := newFixture(t)
	f.addProvider(t, `{"name":"openrouter"}`)
	res := f.addModel(t, `{"name":"fast","targets":["openrouter/qwen/qwen-3.6-flash"]}`)
	if !slices.Equal(entries(res.Diff), []string{"target openrouter/qwen/qwen-3.6-flash created", "model fast created"}) {
		t.Errorf("diff %q", entries(res.Diff))
	}
	wantTarget := domain.Target{Kind: "chat", OutputCapParam: "max_tokens", Reasoning: &domain.Reasoning{Dialect: domain.DialectReasoningObject}}
	if got := f.cfg.Snapshot().Doc.Targets["openrouter/qwen/qwen-3.6-flash"]; !reflect.DeepEqual(got, wantTarget) {
		t.Errorf("created target %+v", got)
	}
	if !reflect.DeepEqual(res.Resource, modelView{"fast", domain.Model{Targets: []string{"openrouter/qwen/qwen-3.6-flash"}}}) {
		t.Errorf("resource %+v", res.Resource)
	}
	// An existing target is used as it is.
	res = f.addModel(t, `{"name":"deep","targets":["openrouter/qwen/qwen-3.6-flash","openrouter/other"],"reasoning":{"effort":"high","mode":"forced"},"max_output_tokens":100}`)
	if !slices.Equal(entries(res.Diff), []string{"target openrouter/other created", "model deep created"}) {
		t.Errorf("diff %q", entries(res.Diff))
	}

	// The refusals: Validate's rules on the whole resulting configuration.
	f.addProvider(t, `{"name":"minimax-plan","preset":"minimax","protocol":"anthropic","base_url":"https://api.minimax.io/anthropic",
		"api_key_env":"MINIMAX_PLAN_API_KEY","target_defaults":{"dialect":"adaptive_effort"},"billing":"subscription","quota":[{"window":"5h","requests":1500}]}`)
	f.addTarget(t, `{"id":"minimax-plan/M","kind":"chat","capabilities":{"tools":true},"max_output_tokens":64000,"reasoning":{"levels":["low","high"],"can_disable":false}}`)
	revision := f.cfg.Snapshot().Revision
	for _, tc := range []struct {
		body, code string
		status     int
		findings   []string
	}{
		{`{"name":"fast","targets":["openrouter/x"]}`, domain.CodeConflict, 409, nil},
		{`{"name":"m","targets":["minimax-plan/M"],"max_output_tokens":1000}`, domain.CodeInvalidRequest, 400, []string{"error effort_required models/m reasoning.effort"}},
		{`{"name":"m","targets":["minimax-plan/M"],"reasoning":{"effort":"low","mode":"default"}}`, domain.CodeInvalidRequest, 400, []string{"error output_cap_required models/m max_output_tokens"}},
		{`{"name":"m","targets":["minimax-plan/M"],"reasoning":{"effort":"low","mode":"default"},"max_output_tokens":64001}`, domain.CodeInvalidRequest, 400, []string{"error output_cap_exceeds_target models/m max_output_tokens"}},
		{`{"name":"m","targets":["minimax-plan/M","openrouter/other"],"requires":["tools"],"reasoning":{"effort":"low","mode":"default"},"max_output_tokens":100}`, domain.CodeInvalidRequest, 400,
			[]string{"error requirement_not_met models/m requires"}},
		{`{"name":"m","targets":["openrouter/other"],"params":{"reasoning":"forward"}}`, domain.CodeInvalidRequest, 400, []string{"error param_collision models/m params.reasoning"}},
		{`{"name":"m","targets":["openrouter/a","openrouter/b","openrouter/c","openrouter/d"]}`, domain.CodeInvalidRequest, 400, []string{"error too_many_targets models/m targets"}},
		{`{"name":"m"}`, domain.CodeInvalidRequest, 400, []string{"error no_targets models/m targets"}},
		{`{"name":"m","targets":["nope/x"]}`, domain.CodeInvalidRequest, 400, []string{"error unknown_provider targets/nope/x "}},
		{`{"name":"m","targets":["fast"]}`, domain.CodeInvalidRequest, 400, []string{"error invalid_name targets/fast "}},
		{`{"name":"bad name","targets":["openrouter/other"]}`, domain.CodeInvalidRequest, 400, []string{"error invalid_name models/bad name "}},
		{`{"name":"m","targets":"openrouter/other"}`, domain.CodeInvalidRequest, 400, nil},
	} {
		e := errorOf(t, f.do(t, http.MethodPost, "/admin/v1/models", []byte(tc.body)), tc.status, tc.code)
		for _, want := range tc.findings {
			if !slices.Contains(codes(e.Findings), want) {
				t.Errorf("%s: findings %q, want %q", tc.body, codes(e.Findings), want)
			}
		}
		if !strings.HasSuffix(f.lastAudit(t), " refused:"+tc.code) {
			t.Errorf("%s: audit %q", tc.body, f.lastAudit(t))
		}
	}
	if f.cfg.Snapshot().Revision != revision {
		t.Error("a refused request published")
	}
	if _, ok := f.cfg.Snapshot().Doc.Targets["openrouter/a"]; ok {
		t.Error("a refused models add left a target behind")
	}
	// The RSN-7 refusal names the alias and the target.
	e := errorOf(t, f.do(t, http.MethodPost, "/admin/v1/models", []byte(`{"name":"m","targets":["minimax-plan/M"],"max_output_tokens":1000}`)), 400, domain.CodeInvalidRequest)
	if i := slices.IndexFunc(e.Findings, func(x domain.Finding) bool { return x.Rule == "RSN-7" }); i < 0 || !strings.Contains(e.Findings[i].Message, "minimax-plan/M") {
		t.Errorf("findings %+v", e.Findings)
	}
	// RSN-7 is met by an effort, provider_default included.
	f.addModel(t, `{"name":"m","targets":["minimax-plan/M"],"reasoning":{"effort":"provider_default","mode":"default"},"max_output_tokens":1000}`)
}

// TestSetAndRemove: models set merges and does not create targets; a provider
// or a target still referenced is refused by Validate, whose findings name
// the dependents; models remove leaves a key's reference as a warning.
func TestSetAndRemove(t *testing.T) {
	f := newFixture(t)
	f.apply(t, "", readExample(t))

	var res modelAnswer
	decode(t, f.do(t, http.MethodPatch, "/admin/v1/models/fast", []byte(`{"reasoning":{"effort":"high"},"params":{"enable_search":null,"x":"drop"},"targets":["openrouter/qwen/qwen-3.6-flash"]}`)), http.StatusOK, &res)
	want := domain.Model{Targets: []string{"openrouter/qwen/qwen-3.6-flash"}, Requires: []string{"tools", "structured_output"},
		Reasoning: domain.ModelReasoning{Effort: "high", Mode: "default"}, MaxOutputTokens: 32000, Params: map[string]string{"x": "drop"}}
	if got := f.cfg.Snapshot().Doc.Models["fast"]; !reflect.DeepEqual(got, want) || !reflect.DeepEqual(res.Resource, modelView{"fast", want}) {
		t.Errorf("stored %+v, answer %+v", got, res.Resource)
	}
	e := errorOf(t, f.do(t, http.MethodPatch, "/admin/v1/models/fast", []byte(`{"targets":["openrouter/new"]}`)), 400, domain.CodeInvalidRequest)
	if !slices.Contains(codes(e.Findings), "error unknown_target models/fast targets") {
		t.Errorf("findings %q", codes(e.Findings))
	}
	errorOf(t, f.do(t, http.MethodPatch, "/admin/v1/models/nope", []byte(`{}`)), 404, domain.CodeNotFound)
	if f.lastAudit(t) != "models set nope model nope refused:not_found" {
		t.Errorf("audit %q", f.lastAudit(t))
	}

	e = errorOf(t, f.do(t, http.MethodDelete, "/admin/v1/targets/openrouter%2Fqwen%2Fqwen-3.6-flash", nil), 400, domain.CodeInvalidRequest)
	if got := codes(e.Findings); !slices.Contains(got, "error unknown_target models/fast targets") || f.lastAudit(t) != "targets remove openrouter/qwen/qwen-3.6-flash target openrouter/qwen/qwen-3.6-flash refused:invalid_request" {
		t.Errorf("findings %q, audit %q", got, f.lastAudit(t))
	}
	e = errorOf(t, f.do(t, http.MethodDelete, "/admin/v1/providers/anthropic", nil), 400, domain.CodeInvalidRequest)
	if got := codes(e.Findings); !slices.Contains(got, "error unknown_provider targets/anthropic/claude-sonnet-5-5 ") {
		t.Errorf("findings %q", got)
	}
	errorOf(t, f.do(t, http.MethodDelete, "/admin/v1/models/nope", nil), 404, domain.CodeNotFound)
	errorOf(t, f.do(t, http.MethodDelete, "/admin/v1/targets/a%2Fnope", nil), 404, domain.CodeNotFound)

	var removed config.Result
	decode(t, f.do(t, http.MethodDelete, "/admin/v1/models/deep", nil), http.StatusOK, &removed)
	if !slices.Equal(entries(removed.Diff), []string{"model deep removed"}) || !slices.Contains(codes(removed.Findings), "warning unknown_alias keys/invoice-agent models") {
		t.Errorf("result %+v", removed)
	}
	decode(t, f.do(t, http.MethodDelete, "/admin/v1/targets/anthropic/claude-sonnet-5-5", nil), http.StatusOK, &removed)
	decode(t, f.do(t, http.MethodDelete, "/admin/v1/providers/anthropic", nil), http.StatusOK, &removed)
	errorOf(t, f.do(t, http.MethodGet, "/admin/v1/models/deep", nil), 404, domain.CodeNotFound)
}

// TestListTargetsAndModels: the list shapes, sorted.
func TestListTargetsAndModels(t *testing.T) {
	f := newFixture(t)
	var targets struct{ Targets []targetView }
	var models struct{ Models []modelView }
	decode(t, f.do(t, http.MethodGet, "/admin/v1/targets", nil), http.StatusOK, &targets)
	decode(t, f.do(t, http.MethodGet, "/admin/v1/models", nil), http.StatusOK, &models)
	if targets.Targets == nil || len(targets.Targets) != 0 || models.Models == nil || len(models.Models) != 0 {
		t.Errorf("empty instance: %+v %+v", targets, models)
	}
	f.apply(t, "", readExample(t))
	decode(t, f.do(t, http.MethodGet, "/admin/v1/targets", nil), http.StatusOK, &targets)
	decode(t, f.do(t, http.MethodGet, "/admin/v1/models", nil), http.StatusOK, &models)
	doc := f.cfg.Snapshot().Doc
	var ids, names []string
	for _, x := range targets.Targets {
		ids = append(ids, x.ID)
		if !reflect.DeepEqual(x.Target, doc.Targets[x.ID]) {
			t.Errorf("target %s: %+v", x.ID, x.Target)
		}
	}
	for _, x := range models.Models {
		names = append(names, x.Name)
		if !reflect.DeepEqual(x.Model, doc.Models[x.Name]) {
			t.Errorf("model %s: %+v", x.Name, x.Model)
		}
	}
	if !slices.IsSorted(ids) || len(ids) != len(doc.Targets) || !slices.Equal(names, []string{"deep", "embed", "fast", "rerank"}) {
		t.Errorf("ids %q, names %q", ids, names)
	}
	var one targetView
	decode(t, f.do(t, http.MethodGet, "/admin/v1/targets/openrouter/qwen/qwen-3.6-flash", nil), http.StatusOK, &one)
	if !reflect.DeepEqual(one, targetView{"openrouter/qwen/qwen-3.6-flash", doc.Targets["openrouter/qwen/qwen-3.6-flash"]}) {
		t.Errorf("show %+v", one)
	}
}
