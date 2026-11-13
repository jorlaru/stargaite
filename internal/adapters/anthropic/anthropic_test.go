package anthropic

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/params"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func golden(t *testing.T, path string, v any) {
	t.Helper()
	got, err := jsonv2.Marshal(v, jsonv2.Deterministic(true), jsontext.Multiline(true))
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *update {
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

func scan(t *testing.T, body string) *params.Body {
	t.Helper()
	b, e := params.Scan([]byte(body), params.Chat.Walk)
	if e != nil {
		t.Fatalf("scan %s: %+v", body, e)
	}
	return b
}

type requestPair struct {
	Name           string   `json:"name"`
	Chat           string   `json:"chat"`
	Messages       string   `json:"messages"`
	NoEquivalent   []string `json:"no_equivalent,omitempty"`
	Untranslatable string   `json:"untranslatable,omitempty"`
}

// TestFromChat is the chat-to-Messages golden of design section 7, in
// testdata/from-chat.json: every row of the mapping table, the members
// absorbed and the keys with no equivalent. Every body is valid JSON.
func TestFromChat(t *testing.T) {
	const hi = `"messages":[{"role":"user","content":"hi"}]`
	const fn = `{"type":"function","function":{"name":"get_weather","description":"Weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}},"strict":true}}`
	var got []requestPair
	for _, c := range []struct{ name, body string }{
		{"minimal", `{"model":"fast",` + hi + `}`},
		{"system and developer wherever they stand, raw strings", `{"messages":[{"role":"system","content":"be \u0062rief \"q\""},{"role":"user","content":"h\u00e9llo"},` +
			`{"role":"developer","content":[{"type":"text","text":"rule 1"},{"type":"text","text":"rule 2"}]},{"role":"assistant","content":"ok"}],"model":"fast"}`},
		{"images: data URIs split, URLs kept, detail dropped", `{"model":"fast","messages":[{"role":"user","content":[{"type":"text","text":"see"},` +
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo=","detail":"high"}},` +
			`{"type":"image_url","image_url":{"url":"data:image\/webp;base64,UklGRg\u003d\u003d"}},` +
			`{"type":"image_url","image_url":{"url":"HTTPS://example.com/a.jpg?x=1\u0026y=2","detail":"low"}}]}]}`},
		{"a tool loop: tool messages merged, results first, the next user message joined", `{"model":"fast","messages":[` +
			`{"role":"user","content":"weather in Paris and Rome?"},` +
			`{"role":"assistant","content":null,"refusal":null,"tool_calls":[` +
			`{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\": \"Paris\"}"}},` +
			`{"id":"call_2","type":"function","function":{"name":"get_weather","arguments":" {\n\"city\":\"Rome\"} "}}]},` +
			`{"role":"tool","tool_call_id":"call_1","content":"18 C"},` +
			`{"role":"tool","tool_call_id":"call_2","content":[{"type":"text","text":"21 C"}]},` +
			`{"role":"user","content":"and tomorrow?"},` +
			`{"role":"assistant","content":"Let me check.","tool_calls":[{"id":"call_3","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},` +
			`{"role":"tool","tool_call_id":"call_3","content":"rain"}],"tools":[` + fn + `]}`},
		{"tools, a named choice and parallel_tool_calls false", `{"model":"fast",` + hi + `,"tools":[` + fn + `,{"type":"function","function":{"name":"now"}}],` +
			`"tool_choice":{"type":"function","function":{"name":"now"}},"parallel_tool_calls":false}`},
		{"required becomes any", `{"model":"fast",` + hi + `,"tools":[` + fn + `],"tool_choice":"required"}`},
		{"parallel_tool_calls false without a choice", `{"model":"fast",` + hi + `,"tools":[` + fn + `],"parallel_tool_calls":false}`},
		{"parallel_tool_calls false with none, absorbed", `{"model":"fast",` + hi + `,"tools":[` + fn + `],"tool_choice":"none","parallel_tool_calls":false}`},
		{"parallel_tool_calls false without tools, absorbed", `{"model":"fast",` + hi + `,"tool_choice":"auto","parallel_tool_calls":false}`},
		{"stop as a string, structured output, sampling and the effort", `{"model":"fast",` + hi + `,"stop":"END","temperature":0.2,"top_p":0.9,` +
			`"response_format":{"type":"json_schema","json_schema":{"name":"r","strict":true,"schema":{"type":"object","properties":{"a":{"type":"string"}}}}},` +
			`"reasoning_effort":"high"}`},
		{"stop as an array, both caps, defaults absorbed, nulls absent", `{"model":"fast",` + hi + `,"stop":["a","b"],"max_tokens":300,"max_completion_tokens":900,` +
			`"n":1,"logprobs":false,"store":false,"stream":false,"parallel_tool_calls":true,"response_format":{"type":"text"},` +
			`"stream_options":{"include_usage":true},"seed":null,"user":null}`},
		{"a stream", `{"model":"fast",` + hi + `,"stream":true,"stream_options":{"include_usage":true}}`},
		{"forwarded members in the client's order, after the mapped ones", `{"x_b":{"k":[1,2]},"model":"fast",` + hi + `,"x_a":"v\u0061l","temperature":1}`},
		{"keys with no equivalent", `{"model":"fast",` + hi + `,"n":2,"logprobs":true,"top_logprobs":2,"logit_bias":{},"frequency_penalty":0,` +
			`"presence_penalty":0,"seed":1,"prediction":{},"audio":{},"modalities":["text"],"verbosity":"low","web_search_options":{},"moderation":{},` +
			`"prompt_cache_key":"k","prompt_cache_options":{},"prompt_cache_retention":"24h","store":true,"metadata":{},"service_tier":"auto",` +
			`"user":"u","safety_identifier":"s","functions":[],"function_call":"auto"}`},
		{"custom tools, allowed_tools and json_object", `{"model":"fast",` + hi + `,"tools":[` + fn + `,{"type":"custom","custom":{"name":"c"}}],` +
			`"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[]}},"response_format":{"type":"json_object"}}`},
	} {
		body, noEq, bad := FromChat(scan(t, c.body))
		if !jsontext.Value(body).IsValid() {
			t.Errorf("%s: not valid JSON: %s", c.name, body)
		}
		got = append(got, requestPair{c.name, c.body, string(body), noEq, bad})
	}
	golden(t, "testdata/from-chat.json", got)
}

// TestFromChatUntranslatable is the nested content that skips the target
// with its JSON path (PAR-12, design section 7).
func TestFromChatUntranslatable(t *testing.T) {
	user := func(parts string) string {
		return `{"model":"a","messages":[{"role":"user","content":"hi"},{"role":"user","content":[{"type":"text","text":"x"},` + parts + `]}]}`
	}
	call := func(args string) string {
		return `{"model":"a","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":` + args + `}}]}]}`
	}
	for _, tt := range []struct{ body, path string }{
		{user(`{"type":"input_audio","input_audio":{"data":"AA==","format":"wav"}}`), "messages[1].content[1]"},
		{user(`{"type":"file","file":{"file_id":"f"}}`), "messages[1].content[1]"},
		{user(`{"type":"image_url","image_url":{"url":"data:image/svg+xml;base64,AA=="}}`), "messages[1].content[1].image_url.url"},
		{user(`{"type":"image_url","image_url":{"url":"data:image/png,AA"}}`), "messages[1].content[1].image_url.url"},
		{user(`{"type":"image_url","image_url":{"url":"ftp://example.com/a.png"}}`), "messages[1].content[1].image_url.url"},
		{user(`{"type":"image_url","image_url":"https://example.com/a.png"}`), "messages[1].content[1].image_url.url"},
		{user(`{"type":"text","text":1}`), "messages[1].content[1]"},
		{user(`"x"`), "messages[1].content[1]"},
		{`{"model":"a","messages":[{"role":"user","content":7}]}`, "messages[0].content"},
		{`{"model":"a","messages":[{"role":"user","name":"ann","content":"hi"}]}`, "messages[0].name"},
		{`{"model":"a","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"refusal":"no"}]}`, "messages[1].refusal"},
		{`{"model":"a","messages":[{"role":"assistant","content":null,"function_call":{"name":"f","arguments":"{}"}}]}`, "messages[0].function_call"},
		{`{"model":"a","messages":[{"role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]}`, "messages[0].content[0]"},
		{call(`"[1,2]"`), "messages[1].tool_calls[0].function.arguments"},
		{call(`""`), "messages[1].tool_calls[0].function.arguments"},
		{call(`"{\"a\":"`), "messages[1].tool_calls[0].function.arguments"},
		{call(`"{\"a\":1,\"a\":2}"`), "messages[1].tool_calls[0].function.arguments"},
		{call(`{"a":1}`), "messages[1].tool_calls[0].function.arguments"},
		{`{"model":"a","messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"custom","custom":{"name":"f","input":"x"}}]}]}`, "messages[0].tool_calls[0]"},
		{`{"model":"a","messages":[{"role":"function","name":"f","content":"x"}]}`, "messages[0].role"},
		{`{"model":"a","messages":[{"role":"tool","content":"x"}]}`, "messages[0].tool_call_id"},
		{`{"model":"a","messages":[{"role":"tool","tool_call_id":"c","content":[{"type":"image_url","image_url":{"url":"https://e.com/a.png"}}]}]}`, "messages[0].content[0]"},
		{`{"model":"a","messages":[{"role":"system","content":[{"type":"image_url","image_url":{"url":"https://e.com/a.png"}}]},{"role":"user","content":"hi"}]}`, "messages[0].content[0]"},
		{`{"model":"a","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"r"}}}`, "response_format.json_schema.schema"},
		{`{"model":"a","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"description":"d"}}]}`, "tools[0].function.name"},
		{`{"model":"a","messages":[{"role":"system","content":"x"},{"role":"developer","content":"y"}]}`, "messages"},
	} {
		if _, _, bad := FromChat(scan(t, tt.body)); bad != tt.path {
			t.Errorf("%s: path %q, want %q", tt.body, bad, tt.path)
		}
	}
}

type responsePair struct {
	Name     string          `json:"name"`
	Messages string          `json:"messages"`
	Chat     string          `json:"chat"`
	Observed domain.Observed `json:"observed"`
}

var now = time.Unix(1_790_000_000, 0)

// TestToChat is the Messages-to-chat golden of design section 7, in
// testdata/to-chat.json: content, tool calls, finish reasons, usage, and the
// think tags of a target that declares inline_think_tags (RSN-12, AC-19),
// removed per text block. The answer holds every field the OpenAI SDKs'
// ChatCompletion requires.
func TestToChat(t *testing.T) {
	const head = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",`
	const usage = `"usage":{"input_tokens":10,"output_tokens":5}}`
	var got []responsePair
	for _, c := range []struct {
		name, body string
		think      bool
	}{
		{"text", head + `"content":[{"type":"text","text":"Hi \u00e9 \"q\""}],"stop_reason":"end_turn","stop_sequence":null,` + usage, false},
		{"thinking dropped, text joined, tool calls with their input bytes", head + `"content":[` +
			`{"type":"thinking","thinking":"plan","signature":"sig"},{"type":"redacted_thinking","data":"x"},{"type":"text","text":"Checking"},` +
			`{"type":"text","text":" both."},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city": "Paris"}},` +
			`{"type":"tool_use","id":"toolu_2","name":"get_weather","input":{}}],"stop_reason":"tool_use",` +
			`"usage":{"input_tokens":10,"cache_creation_input_tokens":3,"cache_read_input_tokens":20,"output_tokens":40,"output_tokens_details":{"thinking_tokens":12}}}`, false},
		{"only a tool call: content null", head + `"content":[{"type":"tool_use","id":"t","name":"now","input":{}}],"stop_reason":"tool_use",` + usage, false},
		{"no content, no usage", head + `"content":[],"stop_reason":"max_tokens"}`, false},
		{"stop_sequence", head + `"content":[{"type":"text","text":"a"}],"stop_reason":"stop_sequence","stop_sequence":"END",` + usage, false},
		{"context window", head + `"content":[{"type":"text","text":"a"}],"stop_reason":"model_context_window_exceeded",` + usage, false},
		{"refusal", head + `"content":[],"stop_reason":"refusal",` + usage, false},
		{"pause_turn", head + `"content":[{"type":"text","text":"a"}],"stop_reason":"pause_turn",` + usage, false},
		{"think tags in a text block, escaped, per block", head + `"content":[{"type":"text","text":"\u003cthink\u003eplan\u003c/think\u003e\n\nHi"},` +
			`{"type":"text","text":" more<think>unclosed"},{"type":"text","text":" \u00e9 kept"},{"type":"tool_use","id":"t","name":"f","input":{"q":"<think>"}}],` +
			`"stop_reason":"end_turn",` + usage, true},
		{"think tags not declared", head + `"content":[{"type":"text","text":"<think>a</think>b"}],"stop_reason":"end_turn",` + usage, false},
		{"only a block", head + `"content":[{"type":"text","text":"<think>a</think>"}],"stop_reason":"end_turn",` + usage, true},
	} {
		body, o, err := ToChat(&domain.Target{InlineThinkTags: c.think}, &domain.Provider{}, []byte(c.body), now)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var sdk struct {
			ID, Object, Model *string
			Created           *int64
			Choices           []struct {
				Index        *int
				FinishReason *string `json:"finish_reason"`
				Message      *struct {
					Role      *string
					Content   json.RawMessage
					ToolCalls []struct {
						ID, Type *string
						Function *struct{ Name, Arguments *string }
					} `json:"tool_calls"`
				}
			}
			Usage *struct {
				Prompt     *int64 `json:"prompt_tokens"`
				Completion *int64 `json:"completion_tokens"`
				Total      *int64 `json:"total_tokens"`
			}
		}
		if err := json.Unmarshal(body, &sdk); err != nil || sdk.ID == nil || *sdk.Object != "chat.completion" || sdk.Model == nil || *sdk.Created != now.Unix() ||
			len(sdk.Choices) != 1 || *sdk.Choices[0].Index != 0 || sdk.Choices[0].FinishReason == nil || *sdk.Choices[0].Message.Role != "assistant" ||
			sdk.Choices[0].Message.Content == nil || strings.Contains(c.body, "usage") && (sdk.Usage == nil || sdk.Usage.Total == nil) {
			t.Errorf("%s: not a chat completion: %v %s", c.name, err, body)
		}
		for _, tc := range sdk.Choices[0].Message.ToolCalls {
			if tc.ID == nil || *tc.Type != "function" || tc.Function == nil || tc.Function.Name == nil || !json.Valid([]byte(*tc.Function.Arguments)) {
				t.Errorf("%s: tool call %s", c.name, body)
			}
		}
		got = append(got, responsePair{c.name, c.body, string(body), o})
	}
	golden(t, "testdata/to-chat.json", got)

	for _, body := range []string{``, `[]`, `{"id":"m","model":"x","content":[]`, `{"model":"x","content":[]}`, `{"id":"m","content":[]}`,
		`{"id":"m","model":"x","content":{}}`, `{"id":"m","model":"x","content":[{"type":"text","text":null}]}`,
		`{"id":"m","model":"x","content":[{"type":"tool_use","id":"t","name":"f"}]}`, `{"id":"m","model":"x","content":[{"type":"tool_use","id":"t","name":"f","input":[]}]}`,
		`{"id":"m","model":"x","content":[],"usage":{"output_tokens":1,"output_tokens":2}}`, `{"id":"m","model":"x","content":[{"type":"text","type":"text"}]}`} {
		if _, _, err := ToChat(&domain.Target{}, &domain.Provider{}, []byte(body), now); err == nil {
			t.Errorf("%q: no error", body)
		}
	}
}

// FuzzFromChat: whatever body the inbound scan accepts, the Messages body is
// one valid JSON object; repeated names are possible only through a member
// outside params.Chat.Known that a forward rule names, which validation
// refuses toward anthropic and Build's read-back turns into a skip.
func FuzzFromChat(f *testing.F) {
	for _, s := range []string{
		`{"model":"a","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"a","messages":[{"role":"system","content":[{"type":"text","text":"s"}]},{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]},` +
			`{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"r"},` +
			`{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"auto","parallel_tool_calls":false,"stop":"e","max_tokens":5,` +
			`"response_format":{"type":"json_schema","json_schema":{"schema":{}}},"x":1}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		b, e := params.Scan(raw, params.Chat.Walk)
		if e != nil {
			return
		}
		body, _, _ := FromChat(b)
		if !jsontext.Value(body).IsValid(jsontext.AllowDuplicateNames(true)) || body[0] != '{' {
			t.Fatalf("%s gave %s", raw, body)
		}
	})
}
