package params

import (
	"bytes"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// bs writes test bodies: "~" stands for a backslash, so escapes such as
// ~u0065 reach the scanner as written.
func bs(s string) []byte { return []byte(strings.ReplaceAll(s, "~", `\`)) }

// nest wraps inner in the containers of a walk path, "*" as a one-element array.
func nest(path []string, inner string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == "*" {
			inner = "[" + inner + "]"
		} else {
			inner = `{"` + path[i] + `":` + inner + "}"
		}
	}
	return inner
}

// param is the error path of key k inside the object at a walk path.
func param(path []string, k string) string {
	var p string
	for _, s := range path {
		if s == "*" {
			p += "[0]"
		} else if p == "" {
			p = s
		} else {
			p += "." + s
		}
	}
	if p == "" {
		return k
	}
	return p + "." + k
}

// TestRepeatedKeys is the PAR-1 table: in the top-level object and in every
// object of both inbound walks, a repeated key is refused with its path,
// spelled the same or escaped differently; one level below, where nothing
// is walked, the same repeat is forwarded content and accepted.
func TestRepeatedKeys(t *testing.T) {
	for name, s := range map[string]Schema{"chat": Chat, "messages": Messages} {
		for _, path := range append([][]string{{}}, s.Walk...) {
			for _, inner := range []string{`{"k":1,"k":2}`, `{"k":1,"~u006b":2}`, `{"k":1,"x":[],"~u006B":{}}`} {
				_, e := Scan(bs(nest(path, inner)), s.Walk)
				if e == nil || e.Code != "invalid_request" || e.Param != param(path, "k") {
					t.Errorf("%s %v %s: got %+v, want a refusal at %s", name, path, inner, e, param(path, "k"))
				}
				below := nest(path, `{"zz":`+inner+`}`)
				if _, e := Scan(bs(below), s.Walk); e != nil {
					t.Errorf("%s %s: refused %+v", name, below, e)
				}
			}
		}
	}
}

// TestEscapedModel settles phase-1 section 2: "model" and its escaped
// spelling in one object are refused, though the decoder's own check is off.
func TestEscapedModel(t *testing.T) {
	for _, body := range []string{`{"model":"a","mod~u0065l":"b"}`, `{"~u006d~u006f~u0064~u0065~u006c":"a", "model" :"b"}`} {
		_, e := Scan(bs(body), Chat.Walk)
		if e == nil || e.Param != "model" || e.Message != `repeated key "model"` {
			t.Errorf("%s: got %+v", body, e)
		}
	}
}

func TestScanRefuses(t *testing.T) {
	for _, body := range []string{
		``, ` `, `[]`, `"x"`, `null`, `1`, `{`, `{"a":1`, `{"a":1,}`, `{"a" 1}`, `{a:1}`, `{"a":1} {}`, `{"a":1}x`,
		"{\"a\":\"\xff\"}", "{\"\xc3\":1}", "{\"metadata\":{\"k\":\"\xed\xa0\x80\"}}", `{"a":"~ud800"}`,
		`{"metadata":{"x":[1,]}}`, `{"a":01}`, `{"a":"~x"}`,
	} {
		if b, e := Scan(bs(body), Chat.Walk); e == nil || e.Code != "invalid_request" || e.Message == "" || b != nil {
			t.Errorf("%q: accepted or wrong error %+v", body, e)
		}
	}
}

func TestScanAccepts(t *testing.T) {
	for _, body := range []string{
		`{}`, " \n{ }\t", `{"model":"m","metadata":{"a":1,"a":2}}`,
		`{"tools":[{"type":"function","function":{"name":"f","parameters":{"p":1,"p":2}}}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"u","x":{"d":1,"d":2}}}]}]}`,
		`{"messages":"not an array","tools":{"*":{"a":1,"a":2}},"stream_options":3}`,
		`{"stop":[[{"a":1,"a":2}]],"model":"~u00e9"}`,
	} {
		if _, e := Scan(bs(body), Chat.Walk); e != nil {
			t.Errorf("%s: %+v", body, e)
		}
	}
}

// TestSchemas pins the key lists of design section 5 and kick-off PAR-1.
func TestSchemas(t *testing.T) {
	if len(Chat.Known) != 37 || len(Messages.Known) != 20 {
		t.Errorf("known keys: chat %d, messages %d", len(Chat.Known), len(Messages.Known))
	}
	if Chat.AsOf != "2026-11-13" || Messages.AsOf != "2026-11-13" || AnthropicVersion != "2023-06-01" {
		t.Errorf("pins %s %s %s", Chat.AsOf, Messages.AsOf, AnthropicVersion)
	}
	for _, k := range []string{"max_tokens", "max_completion_tokens", "reasoning_effort", "stream_options"} {
		if !Chat.Known[k] {
			t.Errorf("chat lacks %s", k)
		}
	}
	for _, k := range []string{"thinking", "output_config", "temperature", "top_p", "top_k"} {
		if !Messages.Known[k] {
			t.Errorf("messages lacks %s", k)
		}
	}
	out := [][]string{{"reasoning"}, {"thinking"}, {"output_config"}}
	for _, c := range []struct {
		name      string
		got, want [][]string
	}{
		{"Chat.Walk", Chat.Walk, [][]string{
			{"stream_options"}, {"response_format"}, {"response_format", "json_schema"}, {"tool_choice"},
			{"tool_choice", "function"}, {"tools", "*"}, {"tools", "*", "function"}, {"messages", "*"},
			{"messages", "*", "content", "*"}, {"messages", "*", "content", "*", "image_url"},
			{"messages", "*", "tool_calls", "*"}, {"messages", "*", "tool_calls", "*", "function"},
			{"reasoning"}, {"thinking"}, {"output_config"},
		}},
		{"Messages.Walk", Messages.Walk, [][]string{
			{"thinking"}, {"output_config"}, {"output_config", "format"}, {"tools", "*"}, {"messages", "*"},
			{"messages", "*", "content", "*"}, {"messages", "*", "content", "*", "content", "*"}, {"reasoning"},
		}},
		{"ChatOut", ChatOut, out},
		{"MessagesOut", MessagesOut, out},
		{"ChatReply", ChatReply, [][]string{
			{"choices", "*"}, {"choices", "*", "message"}, {"choices", "*", "message", "tool_calls", "*"},
			{"choices", "*", "delta"}, {"choices", "*", "delta", "tool_calls", "*"},
			{"usage"}, {"usage", "prompt_tokens_details"}, {"usage", "completion_tokens_details"}, {"error"},
		}},
		{"MessagesReply", MessagesReply, [][]string{
			{"content", "*"}, {"usage"}, {"usage", "output_tokens_details"},
			{"message"}, {"message", "usage"}, {"message", "usage", "output_tokens_details"},
			{"delta"}, {"content_block"}, {"error"},
		}},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestOutInInbound checks that each inbound walk holds its Out walk, so a
// repeat a forward rule carries into an object the read-back (PAR-8) reads
// is refused at step 2 (PAR-1).
func TestOutInInbound(t *testing.T) {
	for _, c := range []struct {
		in         Schema
		out        [][]string
		name, body string
	}{
		{Chat, ChatOut, "chat", `{"model":"a","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"x","type":"y"}}`},
		{Chat, ChatOut, "chat", `{"model":"a","reasoning":{"effort":"x","effort":"y"}}`},
		{Messages, MessagesOut, "messages", `{"model":"a","max_tokens":1,"reasoning":{"x":1,"x":2}}`},
	} {
		for _, p := range c.out {
			if !slices.ContainsFunc(c.in.Walk, func(w []string) bool { return slices.Equal(w, p) }) {
				t.Errorf("%s walk lacks the Out path %v", c.name, p)
			}
		}
		if _, e := Scan([]byte(c.body), c.in.Walk); e == nil || e.Code != "invalid_request" {
			t.Errorf("%s %s: got %+v, want a refusal", c.name, c.body, e)
		}
	}
}

// TestMaxValues bounds the index of a dense body: maxValues members and
// elements are indexed, one more is refused before Check can run.
func TestMaxValues(t *testing.T) {
	array := func(n int) []byte { // "messages" plus n elements
		return []byte(`{"messages":[` + strings.Repeat("0,", n-1) + `0]}`)
	}
	members := func(n int) []byte {
		var b strings.Builder
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`"` + strconv.Itoa(i) + `":0`)
		}
		return []byte("{" + b.String() + "}")
	}
	for _, c := range []struct {
		name string
		walk [][]string
		at   []byte
		over []byte
	}{
		{"array", Chat.Walk, array(maxValues - 1), array(maxValues)},
		{"members", nil, members(maxValues), members(maxValues + 1)},
	} {
		if _, e := Scan(c.at, c.walk); e != nil {
			t.Errorf("%s at the cap: %+v", c.name, e)
		}
		if b, e := Scan(c.over, c.walk); b != nil || e == nil || e.Code != "invalid_request" || !strings.Contains(e.Message, "100000") {
			t.Errorf("%s over the cap: got %+v", c.name, e)
		}
	}
}

// TestBoundHeadroom: a body Scan accepts with exactly maxValues indexed
// values still takes Edit ops that append into output_config and the top
// level, and its read-back succeeds, though Scan would refuse the result.
func TestBoundHeadroom(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"model":"m","output_config":{`)
	for i := range maxValues - 2 { // the two top-level members count too
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"` + strconv.Itoa(i) + `":0`)
	}
	b.WriteString(`}}`)
	body, e := Scan([]byte(b.String()), Messages.Walk)
	if e != nil {
		t.Fatalf("at the bound: %+v", e)
	}
	out, err := body.Edit(
		Op{Path: []string{"output_config", "effort"}, Value: []byte(`"high"`)},
		Op{Path: []string{"output_config", "format"}, Value: []byte(`{}`)}, // its scan sees one value over the bound
		Op{Path: []string{"thinking"}, Value: []byte(`{"type":"adaptive"}`)},
		Op{Path: []string{"max_tokens"}, Value: []byte(`1024`)},
	)
	if err != nil {
		t.Fatalf("Edit at the bound: %v", err)
	}
	if _, e := Scan(out, MessagesOut); e == nil {
		t.Fatal("Scan accepted a body over the bound")
	}
	rb, e := ReadBack(out, MessagesOut)
	if e != nil {
		t.Fatalf("ReadBack: %+v", e)
	}
	if v, ok := rb.Get("output_config", "effort"); !ok || string(v) != `"high"` {
		t.Errorf("read back %s, %v", v, ok)
	}
}

// TestUnknownKeys is AC-08 at unit level: Keys gives the caller each
// top-level name, unescaped, to compare with Known (PAR-2).
func TestUnknownKeys(t *testing.T) {
	b, e := Scan(bs(`{"model":"m","enable_search":true,"messages":[],"~u0078_tag":1}`), Chat.Walk)
	if e != nil {
		t.Fatal(e)
	}
	var unknown []string
	for _, k := range b.Keys() {
		if !Chat.Known[k] {
			unknown = append(unknown, k)
		}
	}
	if !slices.Equal(unknown, []string{"enable_search", "x_tag"}) {
		t.Errorf("unknown %v", unknown)
	}
}

func TestGet(t *testing.T) {
	const body = ` { "mod~u0065l" : "a~"b" , "messages":[ {"role" :"user","content":"hi"}, 7 ],"metadata":{"a": [1]} } `
	raw := bs(body)
	b, e := Scan(raw, Chat.Walk)
	if e != nil {
		t.Fatal(e)
	}
	if !slices.Equal(b.Keys(), []string{"model", "messages", "metadata"}) {
		t.Errorf("keys %v", b.Keys())
	}
	for _, c := range []struct {
		path []string
		want string
	}{
		{[]string{"model"}, `"a~"b"`},
		{[]string{"messages", "0"}, `{"role" :"user","content":"hi"}`},
		{[]string{"messages", "0", "role"}, `"user"`},
		{[]string{"messages", "1"}, `7`},
		{[]string{"metadata"}, `{"a": [1]}`},
		{nil, string(bytes.TrimSpace(raw))},
	} {
		got, ok := b.Get(c.path...)
		if !ok || string(got) != string(bs(c.want)) {
			t.Errorf("Get(%v) = %s, %v", c.path, got, ok)
		}
	}
	for _, path := range [][]string{
		{"absent"}, {"metadata", "a"}, {"messages", "2"}, {"messages", "01"}, {"messages", "-0"}, {"messages", "+1"},
		{"messages", "1", "x"}, {"messages", "x"}, {"model", "0"}, {"messages", "0", "content", "0"},
	} {
		if got, ok := b.Get(path...); ok {
			t.Errorf("Get(%v) = %s, want a failure", path, got)
		}
	}
	got, _ := b.Get("model")
	if ext := append(got, '!'); string(ext) != string(bs(`"a~"b"!`)) || !bytes.Equal(raw, bs(body)) {
		t.Errorf("append to a Get result gave %s and changed the body to %s", ext, raw)
	}
}

func op(value string, path ...string) Op {
	if value == "-" {
		return Op{Path: path}
	}
	return Op{Path: path, Value: []byte(value)}
}

func TestEdit(t *testing.T) {
	for _, c := range []struct {
		name, body string
		ops        []Op
		want       string // "" for an error
	}{
		{"replace", `{"model":"a","x":1}`, []Op{op(`"b"`, "model")}, `{"model":"b","x":1}`},
		{"replace escaped name", `{"mod~u0065l":"a"}`, []Op{op(`"b"`, "model")}, `{"mod~u0065l":"b"}`},
		{"replace keeps spacing", `{ "a" : 1 , "b" : 2 }`, []Op{op(`3`, "b")}, `{ "a" : 1 , "b" : 3 }`},
		{"append", `{"a":1}`, []Op{op(`2`, "b")}, `{"a":1,"b":2}`},
		{"append before space", "{\"a\":1\n}", []Op{op(`2`, "b")}, "{\"a\":1,\"b\":2\n}"},
		{"append to empty", `{ }`, []Op{op(`2`, "b")}, `{"b":2 }`},
		{"appends in op order", `{}`, []Op{op(`1`, "z"), op(`2`, "a")}, `{"z":1,"a":2}`},
		{"append quotes the name", `{}`, []Op{op(`1`, "a\"b")}, `{"a~"b":1}`},
		{"delete first", `{"a":1, "b":2}`, []Op{op("-", "a")}, `{"b":2}`},
		{"delete middle", `{"a":1,"b":2,"c":3}`, []Op{op("-", "b")}, `{"a":1,"c":3}`},
		{"delete last", `{"a":1 , "b":2 }`, []Op{op("-", "b")}, `{"a":1 }`},
		{"delete only", `{ "a":1 }`, []Op{op("-", "a")}, `{  }`},
		{"delete absent", `{"a":1}`, []Op{op("-", "b"), op("-", "c", "d")}, `{"a":1}`},
		{"delete element", `{"a":[1, 2,3]}`, []Op{op("-", "a", "1")}, `{"a":[1, 3]}`},
		{"create parent", `{"model":"m"}`, []Op{op(`"high"`, "output_config", "effort")}, `{"model":"m","output_config":{"effort":"high"}}`},
		{"create parents", `{"a":{}}`, []Op{op(`true`, "a", "b", "c", "d")}, `{"a":{"b":{"c":{"d":true}}}}`},
		{"merge into parent", `{"output_config":{"format":{"type":"json_schema"}}}`, []Op{op(`"low"`, "output_config", "effort")},
			`{"output_config":{"format":{"type":"json_schema"},"effort":"low"}}`},
		{"inside an earlier op", `{"thinking":{"type":"adaptive","x":1},"m":1}`,
			[]Op{op(`{"type":"enabled"}`, "thinking"), op(`1024`, "thinking", "budget_tokens")},
			`{"thinking":{"type":"enabled","budget_tokens":1024},"m":1}`},
		{"delete then append", `{"reasoning_effort":"high","m":1}`, []Op{op("-", "reasoning_effort"), op(`"low"`, "reasoning_effort")},
			`{"m":1,"reasoning_effort":"low"}`},
		{"array element", `{"choices":[{"index":0},{"message":{"content":"<think>x</think>hi"}}]}`,
			[]Op{op(`"hi"`, "choices", "1", "message", "content")}, `{"choices":[{"index":0},{"message":{"content":"hi"}}]}`},
		{"member named 0", `{"a":{"0":1}}`, []Op{op(`2`, "a", "0")}, `{"a":{"0":2}}`},
		{"no op", ` {"a":1} `, nil, ` {"a":1} `},
		{"empty path", `{}`, []Op{op(`1`)}, ``},
		{"invalid value", `{}`, []Op{op(`{`, "a")}, ``},
		{"empty value", `{}`, []Op{{Path: []string{"a"}, Value: []byte{}}}, ``},
		{"value repeats a name", `{}`, []Op{op(`{"a":1,"a":2}`, "a")}, ``},
		{"parent is a string", `{"output_config":"x"}`, []Op{op(`"high"`, "output_config", "effort")}, ``},
		{"parent is null", `{"output_config":null}`, []Op{op(`"high"`, "output_config", "effort")}, ``},
		{"index out of range", `{"a":[1]}`, []Op{op(`2`, "a", "1")}, ``},
		{"name in an array", `{"a":[1]}`, []Op{op(`2`, "a", "x")}, ``},
		{"repeat on the path", `{"thinking":{"type":"a","type":"b"}}`, []Op{op(`1`, "thinking", "budget_tokens")}, ``},
		{"invalid name", `{}`, []Op{op(`1`, "\xff")}, ``},
		{"error after a good op", `{"a":1}`, []Op{op(`2`, "a"), op(`3`, "a", "b")}, ``},
	} {
		raw := bs(c.body)
		b, e := Scan(raw, nil)
		if e != nil {
			t.Fatalf("%s: %+v", c.name, e)
		}
		got, err := b.Edit(c.ops...)
		switch {
		case c.want == "" && err == nil:
			t.Errorf("%s: got %s, want an error", c.name, got)
		case c.want != "" && (err != nil || string(got) != string(bs(c.want))):
			t.Errorf("%s: got %s, %v; want %s", c.name, got, err, bs(c.want))
		}
		if string(raw) != string(bs(c.body)) {
			t.Errorf("%s: Edit changed the scanned body", c.name)
		}
	}
}

func TestDetect(t *testing.T) {
	none, tools, structured, strict, vision := Features{}, Features{Tools: true}, Features{Structured: true}, Features{Strict: true}, Features{Vision: true}
	for _, c := range []struct {
		ep, body string
		want     Features
	}{
		{"chat", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, none},
		{"chat", `{"tools":[]}`, tools},
		{"chat", `{"tool_choice":"none"}`, tools},
		{"chat", `{"response_format":{"type":"json_schema","json_schema":{"name":"x"}}}`, structured},
		{"chat", `{"response_format":{"type":"json_sch~u0065ma"}}`, structured},
		{"chat", `{"response_format":{"type":"json_object"}}`, none},
		{"chat", `{"response_format":{"json_schema":{"strict":true}}}`, strict},
		{"chat", `{"response_format":{"json_schema":{"strict":false}}}`, none},
		{"chat", `{"tools":[{"function":{"strict":false}},{"function":{"strict":true}}]}`, Features{Tools: true, Strict: true}},
		{"chat", `{"messages":[{"content":"x"},{"content":[{"type":"text"},{"type":"image_url"}]}]}`, vision},
		{"chat", `{"messages":[{"content":[{"type":"input_audio"}]}]}`, vision},
		{"chat", `{"messages":[{"content":[{"type":"file"}]}]}`, vision},
		{"chat", `{"messages":[{"content":[{"type":"image"}]}]}`, none},
		{"chat", `{"output_config":{"format":{"type":"json_schema"}}}`, none},
		{"messages", `{"tools":[{"name":"t","strict":true}]}`, Features{Tools: true, Strict: true}},
		{"messages", `{"tool_choice":{"type":"auto"}}`, tools},
		{"messages", `{"output_config":{"format":{"type":"json_schema","schema":{}}}}`, structured},
		{"messages", `{"output_config":{"effort":"high"}}`, none},
		{"messages", `{"messages":[{"content":[{"type":"text"},{"type":"document"}]}]}`, vision},
		{"messages", `{"messages":[{"content":[{"type":"image"}]}]}`, vision},
		{"messages", `{"messages":[{"content":[{"type":"tool_result","content":[{"type":"text"},{"type":"image"}]}]}]}`, vision},
		{"messages", `{"messages":[{"content":[{"type":"image_url"}]}]}`, none},
		{"messages", `{"response_format":{"type":"json_schema"}}`, none},
		{"embeddings", `{"tools":[]}`, none},
	} {
		s := Chat
		if c.ep == "messages" {
			s = Messages
		}
		b, e := Scan(bs(c.body), s.Walk)
		if e != nil {
			t.Fatalf("%s: %+v", c.body, e)
		}
		if got := Detect(c.ep, b); got != c.want {
			t.Errorf("%s %s: got %+v, want %+v", c.ep, c.body, got, c.want)
		}
	}
}

// TestReadWalks checks that the outgoing and reply walks reach every value
// the read-back (PAR-8) and the observers of design section 7 read.
func TestReadWalks(t *testing.T) {
	for _, c := range []struct {
		walk [][]string
		body string
		path []string
		want string
	}{
		{ChatOut, `{"reasoning":{"effort":"low"}}`, []string{"reasoning", "effort"}, `"low"`},
		{ChatOut, `{"thinking":{"type":"enabled","budget_tokens":2048}}`, []string{"thinking", "budget_tokens"}, `2048`},
		{MessagesOut, `{"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`, []string{"output_config", "effort"}, `"max"`},
		{ChatReply, `{"choices":[{"message":{"tool_calls":[{"id":"a"},{"id":"b"}]},"finish_reason":"tool_calls"}]}`,
			[]string{"choices", "0", "message", "tool_calls", "1", "id"}, `"b"`},
		{ChatReply, `{"choices":[{"delta":{"tool_calls":[{"index":3}]}}]}`, []string{"choices", "0", "delta", "tool_calls", "0", "index"}, `3`},
		{ChatReply, `{"choices":[],"usage":{"prompt_tokens_details":{"cached_tokens":5}}}`, []string{"usage", "prompt_tokens_details", "cached_tokens"}, `5`},
		{ChatReply, `{"usage":{"completion_tokens_details":{"reasoning_tokens":7}}}`, []string{"usage", "completion_tokens_details", "reasoning_tokens"}, `7`},
		{ChatReply, `{"error":{"message":"m","type":"t"}}`, []string{"error", "message"}, `"m"`},
		{MessagesReply, `{"content":[{"type":"text"},{"type":"tool_use","input":{"a":1}}]}`, []string{"content", "1", "input"}, `{"a":1}`},
		{MessagesReply, `{"usage":{"output_tokens_details":{"thinking_tokens":9}}}`, []string{"usage", "output_tokens_details", "thinking_tokens"}, `9`},
		{MessagesReply, `{"type":"message_start","message":{"model":"x","usage":{"input_tokens":4}}}`, []string{"message", "usage", "input_tokens"}, `4`},
		{MessagesReply, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`, []string{"delta", "stop_reason"}, `"end_turn"`},
		{MessagesReply, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"f"}}`, []string{"content_block", "name"}, `"f"`},
		{MessagesReply, `{"type":"error","error":{"type":"overloaded_error"}}`, []string{"error", "type"}, `"overloaded_error"`},
	} {
		b, e := Scan([]byte(c.body), c.walk)
		if e != nil {
			t.Fatalf("%s: %+v", c.body, e)
		}
		if got, ok := b.Get(c.path...); !ok || string(got) != c.want {
			t.Errorf("%s: Get(%v) = %s, %v", c.body, c.path, got, ok)
		}
	}
}
