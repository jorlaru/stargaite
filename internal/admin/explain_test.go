package admin

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
)

const explainConfig = `version: 1
settings: { max_body_bytes: 300 }
providers:
  local: { protocol: openai, base_url: "http://127.0.0.1:1/v1", api_key_env: LOCAL_KEY, billing: none, allow_private_network: true }
  anth: { protocol: anthropic, base_url: "http://127.0.0.1:1", api_key_env: ANTH_KEY, billing: none, allow_private_network: true }
targets:
  local/up-1: { kind: chat, capabilities: { tools: false }, reasoning: { dialect: reasoning_effort, levels: [low, medium, high] } }
  local/up-2: { kind: chat }
  anth/m: { kind: chat, reasoning: { dialect: adaptive_effort, levels: [low, medium, high] } }
models:
  fast: { targets: [local/up-1, local/up-2], reasoning: { effort: medium, mode: default }, max_output_tokens: 500 }
  deep: { targets: [anth/m, local/up-2], max_output_tokens: 1000 }
`

// planted is the value every credential resolves to: no answer may hold it.
const planted = "sk-PLANTED-credential-value"

// TestExplain: models explain answers, per target in order, the skip or what
// Build gives (PAR-10): the layers, the change list, the URL, the headers
// without the credential and the exact body. It refuses what the data plane
// refuses, with the same code and param, and it is a read: no audit row, no
// new revision. The bytes against what a provider receives are checked in
// the api package, with the mock provider.
func TestExplain(t *testing.T) {
	f := newFixtureEnv(t, func() config.Env {
		return config.Env{Lookup: func(string) (string, bool) { return planted, true }, Resolve: resolve}
	})
	revision := f.apply(t, "", []byte(explainConfig)).Revision
	audits := f.count(t, "SELECT COUNT(*) FROM audit")
	msg := `"messages":[{"role":"user","content":"hi"}]`
	chat := `"url":"http://127.0.0.1:1/v1/chat/completions","header":{"Content-Type":["application/json"]}`
	none := `"effort":{"requested":null,"alias":null,"sent":null,"source":"provider_default","adjusted":false}`
	// The alias's effort toward a target of dialect none is dropped, and listed.
	dropped := `"effort":{"requested":null,"alias":"medium","sent":null,"source":"provider_default","adjusted":false}`
	for _, tc := range []struct {
		name, alias, body string
		status            int
		answer            string // the whole answer, when the status is 200
		code, param       string
	}{
		{name: "the minimal request", alias: "fast", status: 200, answer: `{"targets":[` +
			`{"target":"local/up-1","effort":{"requested":null,"alias":"medium","sent":"medium","source":"alias_default","adjusted":false},` +
			`"max_output":{"requested":null,"alias":500,"sent":500},"changes":"max_tokens:added,reasoning_effort:added",` + chat +
			`,"body":"{\"model\":\"up-1\",` + strings.ReplaceAll(msg, `"`, `\"`) + `,\"max_tokens\":500,\"reasoning_effort\":\"medium\"}"},` +
			`{"target":"local/up-2",` + dropped + `,"max_output":{"requested":null,"alias":500,"sent":500},"changes":"max_tokens:added,reasoning_effort:dropped",` + chat +
			`,"body":"{\"model\":\"up-2\",` + strings.ReplaceAll(msg, `"`, `\"`) + `,\"max_tokens\":500}"}]}`},
		{name: "an empty request body", alias: "fast", body: "", status: 200},
		{name: "effort and cap set on the body", alias: "fast", body: `{"effort":"low","max_output_tokens":20,"endpoint":null}`, status: 200, answer: `{"targets":[` +
			`{"target":"local/up-1","effort":{"requested":"low","alias":"medium","sent":"low","source":"client","adjusted":false},` +
			`"max_output":{"requested":20,"alias":500,"sent":20},"changes":"",` + chat +
			`,"body":"{\"model\":\"up-1\",` + strings.ReplaceAll(msg, `"`, `\"`) + `,\"reasoning_effort\":\"low\",\"max_completion_tokens\":20}"},` +
			`{"target":"local/up-2","effort":{"requested":"low","alias":"medium","sent":null,"source":"provider_default","adjusted":false},` +
			`"max_output":{"requested":20,"alias":500,"sent":20},"changes":"reasoning_effort:dropped",` + chat +
			`,"body":"{\"model\":\"up-2\",` + strings.ReplaceAll(msg, `"`, `\"`) + `,\"max_completion_tokens\":20}"}]}`},
		{name: "the exact bytes of a body", alias: "fast", body: `{"body":"{ \"model\" : \"f\\u0061st\",\n` + strings.ReplaceAll(msg, `"`, `\"`) +
			`,\"tools\":[{\"type\":\"function\",\"function\":{\"name\":\"f\"}}]}"}`, status: 200, answer: `{"targets":[` +
			`{"target":"local/up-1","skipped":{"code":"capability_mismatch","param":"tools"}},` +
			`{"target":"local/up-2",` + dropped + `,"max_output":{"requested":null,"alias":500,"sent":500},"changes":"max_tokens:added,reasoning_effort:dropped",` + chat +
			`,"body":"{ \"model\" : \"up-2\",\n` + strings.ReplaceAll(msg, `"`, `\"`) + `,\"tools\":[{\"type\":\"function\",\"function\":{\"name\":\"f\"}}],\"max_tokens\":500}"}]}`},
		{name: "chat toward an anthropic target", alias: "deep", body: `{"endpoint":"chat","effort":"low"}`, status: 200, answer: `{"targets":[` +
			`{"target":"anth/m","effort":{"requested":"low","alias":null,"sent":"low","source":"client","adjusted":false},` +
			`"max_output":{"requested":null,"alias":1000,"sent":1000},"changes":"protocol:translated,max_tokens:added,reasoning_effort:translated",` +
			`"url":"http://127.0.0.1:1/v1/messages","header":{"Anthropic-Version":["2023-06-01"],"Content-Type":["application/json"]},` +
			`"body":"{\"model\":\"m\",` + strings.ReplaceAll(msg, `"`, `\"`) + `,\"max_tokens\":1000,\"thinking\":{\"type\":\"adaptive\"},\"output_config\":{\"effort\":\"low\"}}"},` +
			`{"target":"local/up-2","effort":{"requested":"low","alias":null,"sent":null,"source":"provider_default","adjusted":false},` +
			`"max_output":{"requested":null,"alias":1000,"sent":1000},"changes":"max_tokens:added,reasoning_effort:dropped",` + chat +
			`,"body":"{\"model\":\"up-2\",` + strings.ReplaceAll(msg, `"`, `\"`) + `,\"max_tokens\":1000}"}]}`},
		{name: "a chat stream toward an anthropic target", alias: "deep", body: `{"body":"{\"model\":\"deep\",` + strings.ReplaceAll(msg, `"`, `\"`) + `,\"stream\":true}"}`,
			status: 200, answer: `{"targets":[{"target":"anth/m","skipped":{"code":"endpoint_not_supported","param":"stream"}},` +
				`{"target":"local/up-2",` + none + `,"max_output":{"requested":null,"alias":1000,"sent":1000},"changes":"max_tokens:added",` + chat +
				`,"body":"{\"model\":\"up-2\",` + strings.ReplaceAll(msg, `"`, `\"`) + `,\"stream\":true,\"max_tokens\":1000}"}]}`},
		{name: "messages", alias: "deep", body: `{"endpoint":"messages","effort":"high"}`, status: 200, answer: `{"targets":[` +
			`{"target":"anth/m","effort":{"requested":"high","alias":null,"sent":"high","source":"client","adjusted":false},` +
			`"max_output":{"requested":null,"alias":1000,"sent":1000},"changes":"max_tokens:added,output_config.effort:translated",` +
			`"url":"http://127.0.0.1:1/v1/messages","header":{"Anthropic-Version":["2023-06-01"],"Content-Type":["application/json"]},` +
			`"body":"{\"model\":\"m\",` + strings.ReplaceAll(msg, `"`, `\"`) + `,\"output_config\":{\"effort\":\"high\"},\"max_tokens\":1000,\"thinking\":{\"type\":\"adaptive\"}}"},` +
			`{"target":"local/up-2","skipped":{"code":"endpoint_not_supported"}}]}`},

		{name: "unknown alias", alias: "nope", status: 404, code: domain.CodeNotFound},
		{name: "unknown endpoint", alias: "fast", body: `{"endpoint":"embeddings"}`, status: 400, code: domain.CodeInvalidRequest},
		{name: "unknown member", alias: "fast", body: `{"model":"fast"}`, status: 400, code: domain.CodeInvalidRequest},
		{name: "repeated member", alias: "fast", body: `{"effort":"low","effort":"high"}`, status: 400, code: domain.CodeInvalidRequest},
		{name: "not an object", alias: "fast", body: `[]`, status: 400, code: domain.CodeInvalidRequest},
		{name: "a cap that is not an integer", alias: "fast", body: `{"max_output_tokens":1.5}`, status: 400, code: domain.CodeInvalidRequest},
		{name: "a body that is not JSON", alias: "fast", body: `{"body":"{"}`, status: 400, code: domain.CodeInvalidRequest},
		{name: "a repeated key in the body", alias: "fast", body: `{"body":"{\"model\":\"fast\",\"mod\\u0065l\":\"fast\"}"}`, status: 400,
			code: domain.CodeInvalidRequest, param: "model"},
		{name: "a body larger than max_body_bytes", alias: "fast", body: `{"body":"` + strings.Repeat(" ", 301) + `"}`, status: 413, code: domain.CodeRequestTooLarge},
		{name: "a body naming another alias", alias: "fast", body: `{"body":"{\"model\":\"deep\",` + strings.ReplaceAll(msg, `"`, `\"`) + `}"}`, status: 400,
			code: domain.CodeInvalidRequest, param: "model"},
		{name: "a body naming no alias", alias: "fast", body: `{"body":"{` + strings.ReplaceAll(msg, `"`, `\"`) + `}"}`, status: 400,
			code: domain.CodeInvalidRequest, param: "model"},
		{name: "no messages", alias: "fast", body: `{"body":"{\"model\":\"fast\"}"}`, status: 400, code: domain.CodeInvalidRequest, param: "messages"},
		{name: "unknown parameter", alias: "fast", body: `{"body":"{\"model\":\"fast\",` + strings.ReplaceAll(msg, `"`, `\"`) + `,\"enable_search\":true}"}`,
			status: 400, code: domain.CodeUnknownParameter, param: "enable_search"},
		{name: "effort off the scale", alias: "fast", body: `{"effort":"default"}`, status: 400, code: domain.CodeInvalidReasoningEffort, param: "reasoning_effort"},
		{name: "messages effort off the scale", alias: "deep", body: `{"endpoint":"messages","effort":""}`, status: 400,
			code: domain.CodeInvalidReasoningEffort, param: "output_config.effort"},
		{name: "a negative cap", alias: "fast", body: `{"max_output_tokens":-1}`, status: 400, code: domain.CodeInvalidRequest, param: "max_completion_tokens"},
		{name: "a cap above the alias's", alias: "fast", body: `{"max_output_tokens":501}`, status: 422, code: domain.CodeMaxOutputTokensExceeded,
			param: "max_completion_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := f.do(t, http.MethodPost, "/admin/v1/models/"+tc.alias+"/explain", []byte(tc.body))
			if strings.Contains(w.Body.String(), planted) {
				t.Errorf("the credential is in the answer: %s", w.Body)
			}
			if tc.status != http.StatusOK {
				if e := errorOf(t, w, tc.status, tc.code); e.Param != tc.param {
					t.Errorf("param %q, want %q", e.Param, tc.param)
				}
				return
			}
			decode(t, w, http.StatusOK, &struct{ Targets []map[string]any }{})
			if tc.answer != "" && w.Body.String() != tc.answer+"\n" {
				t.Errorf("answer\n%s\nwant\n%s", w.Body, tc.answer)
			}
		})
	}
	// The body string of the empty request is the default one.
	if a, b := f.do(t, http.MethodPost, "/admin/v1/models/fast/explain", nil).Body.String(), f.do(t, http.MethodPost, "/admin/v1/models/fast/explain", []byte("{}")).Body.String(); a != b {
		t.Errorf("no body %s, {} %s", a, b)
	}
	if n := f.count(t, "SELECT COUNT(*) FROM audit"); n != audits {
		t.Errorf("%d audit rows after explains, want %d", n, audits)
	}
	if r := f.cfg.Snapshot().Revision; r != revision {
		t.Errorf("revision %d after explains, want %d", r, revision)
	}
	if strings.Contains(f.log.String(), planted) {
		t.Error("the credential is in the log")
	}
}
