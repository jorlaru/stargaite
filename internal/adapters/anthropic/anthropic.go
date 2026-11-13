// Package anthropic translates chat completions toward providers of the
// anthropic protocol (design section 7): the request into a Messages body,
// built from the client's raw values, and the Messages answer back into a
// chat completion, synchronous or streamed (design section 9), with the
// think-tag edits of a target that declares inline_think_tags (RSN-12). On
// the native path (/v1/messages, design section 8) it observes the Messages
// answer and stream the relay passes through, with the same edits.
package anthropic

import (
	"bytes"
	"cmp"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/params"
	"github.com/jorlaru/stargaite/internal/reasoning"
)

// media are the image types a data: URI may carry toward the Messages API.
var media = []string{"image/jpeg", "image/png", "image/gif", "image/webp"}

// message is one Messages turn being built: a string content, or blocks.
type message struct {
	role    string
	text    []byte   // a raw JSON string; nil when the content is blocks
	blocks  [][]byte // content blocks
	results bool     // a user turn of tool_result blocks, which a following tool or user turn joins
}

// FromChat builds the Messages body of the chat completion b, scanned with
// params.Chat.Walk (design section 7). Members are written in a fixed order:
// model, max_tokens (the larger client cap), system, messages, tools,
// tool_choice, stop_sequences, stream, temperature, top_p, output_config,
// reasoning_effort (left for the dialect edit to translate or delete), then
// every member outside params.Chat.Known, which only a forward rule lets
// through, byte for byte in the client's order. JSON strings are copied as
// raw tokens. A known member whose value is null is absent. noEquivalent
// lists the top-level keys with no faithful equivalent (PAR-12), in body
// order; untranslatable is the JSON path of the first content that cannot
// be translated, "" when none.
func FromChat(b *params.Body) (body []byte, noEquivalent []string, untranslatable string) {
	var model, maxTokens, stop, stream, temperature, topP, effort, tools, format []byte
	var choice string // the tool_choice members, without braces; "" when absent
	var forwarded [][]byte
	maxCap, parallel := int64(-1), true
	fail := func(path string) {
		if untranslatable == "" {
			untranslatable = path
		}
	}
	for _, k := range b.Keys() {
		v, _ := b.Get(k)
		if string(v) == "null" && params.Chat.Known[k] {
			continue
		}
		switch k {
		case "model":
			model = v
		case "messages", "stream_options": // below; stream_options is the relay's (usage chunk)
		case "max_tokens", "max_completion_tokens":
			if n, err := strconv.ParseInt(string(v), 10, 64); err == nil && n > maxCap {
				maxCap, maxTokens = n, v
			}
		case "stop":
			stop = v
			if v[0] == '"' {
				stop = fmt.Appendf(nil, "[%s]", v)
			}
		case "stream":
			if string(v) == "true" {
				stream = v
			}
		case "temperature":
			temperature = v
		case "top_p":
			topP = v
		case "reasoning_effort":
			effort = v
		case "parallel_tool_calls":
			parallel = string(v) != "false"
		case "n", "logprobs", "store": // only their defaults have an equivalent: none
			if string(v) != map[string]string{"n": "1", "logprobs": "false", "store": "false"}[k] {
				noEquivalent = append(noEquivalent, k)
			}
		case "tools":
			var custom bool
			var bad string
			if tools, custom, bad = fromTools(b); custom {
				noEquivalent = append(noEquivalent, k)
			}
			fail(bad)
		case "tool_choice":
			var ok bool
			if choice, ok = fromToolChoice(b, v); !ok {
				noEquivalent = append(noEquivalent, k)
			}
		case "response_format":
			switch str(b, k, "type") {
			case "text":
			case "json_schema":
				if s, ok := b.Get(k, "json_schema", "schema"); ok {
					format = fmt.Appendf(nil, `{"format":{"type":"json_schema","schema":%s}}`, s)
				} else {
					fail("response_format.json_schema.schema")
				}
			default: // json_object and anything else
				noEquivalent = append(noEquivalent, k)
			}
		default:
			if params.Chat.Known[k] {
				noEquivalent = append(noEquivalent, k)
			} else {
				forwarded = append(forwarded, member(k, v))
			}
		}
	}
	system, msgs, path := fromMessages(b)
	fail(path)

	if !parallel && tools != nil && choice != `"type":"none"` {
		choice = cmp.Or(choice, `"type":"auto"`) + `,"disable_parallel_tool_use":true`
	}
	out := []byte{'{'}
	for _, m := range [][]byte{
		member("model", model), member("max_tokens", maxTokens), member("system", system), member("messages", msgs),
		member("tools", tools), member("tool_choice", wrap(choice)), member("stop_sequences", stop), member("stream", stream),
		member("temperature", temperature), member("top_p", topP), member("output_config", format), member("reasoning_effort", effort),
	} {
		out = appendMember(out, m)
	}
	for _, m := range forwarded {
		out = appendMember(out, m)
	}
	return append(out, '}'), noEquivalent, untranslatable
}

// fromMessages maps the chat messages: system and developer messages become
// the top-level system blocks, in order; consecutive tool messages become
// one user turn of tool_result blocks, which a user message right after them
// joins after those blocks. path is the first content that cannot be
// translated, or messages when only system and developer messages are sent.
func fromMessages(b *params.Body) (system, msgs []byte, path string) {
	var sys [][]byte
	var turns []*message
	for i := 0; ; i++ {
		at := strconv.Itoa(i)
		if _, ok := b.Get("messages", at); !ok {
			break
		}
		p := "messages[" + at + "]"
		role := str(b, "messages", at, "role")
		if !slices.Contains([]string{"system", "developer", "user", "assistant", "tool"}, role) {
			return nil, nil, p + ".role"
		}
		for _, n := range []string{"name", "refusal", "function_call", "audio"} {
			if v, ok := b.Get("messages", at, n); ok && string(v) != "null" {
				return nil, nil, p + "." + n
			}
		}
		var last *message
		if len(turns) > 0 {
			last = turns[len(turns)-1]
		}
		content, _ := b.Get("messages", at, "content")
		switch role {
		case "system", "developer":
			blocks, bad := parts(b, at, false)
			if bad != "" {
				return nil, nil, bad
			}
			sys = append(sys, blocks...)
		case "user":
			if len(content) > 0 && content[0] == '"' && (last == nil || !last.results) {
				turns = append(turns, &message{role: role, text: content})
				break
			}
			blocks, bad := parts(b, at, true)
			if bad != "" {
				return nil, nil, bad
			}
			if last != nil && last.results {
				last.blocks, last.results = append(last.blocks, blocks...), false
				break
			}
			turns = append(turns, &message{role: role, blocks: blocks})
		case "assistant":
			calls, bad := toolUses(b, at)
			if bad != "" {
				return nil, nil, bad
			}
			if len(content) > 0 && content[0] == '"' && calls == nil {
				turns = append(turns, &message{role: role, text: content})
				break
			}
			var blocks [][]byte
			if len(content) > 0 && content[0] != 'n' && string(content) != `""` {
				if blocks, bad = parts(b, at, false); bad != "" {
					return nil, nil, bad
				}
			}
			turns = append(turns, &message{role: role, blocks: append(blocks, calls...)})
		case "tool":
			id, ok := b.Get("messages", at, "tool_call_id")
			if !ok || id[0] != '"' {
				return nil, nil, p + ".tool_call_id"
			}
			result := content
			if len(content) == 0 || content[0] != '"' {
				blocks, bad := parts(b, at, false)
				if bad != "" {
					return nil, nil, bad
				}
				result = list(blocks)
			}
			block := fmt.Appendf(nil, `{"type":"tool_result","tool_use_id":%s,"content":%s}`, id, result)
			if last != nil && last.results {
				last.blocks = append(last.blocks, block)
				break
			}
			turns = append(turns, &message{role: "user", blocks: [][]byte{block}, results: true})
		}
	}
	if len(turns) == 0 {
		return nil, nil, "messages" // the Messages API needs at least one message
	}
	if sys != nil {
		system = list(sys)
	}
	msgs = []byte{'['}
	for i, m := range turns {
		if i > 0 {
			msgs = append(msgs, ',')
		}
		content := m.text
		if content == nil {
			content = list(m.blocks)
		}
		msgs = fmt.Appendf(msgs, `{"role":%q,"content":%s}`, m.role, content)
	}
	return system, append(msgs, ']'), ""
}

// parts maps the content of messages[at]: a string is one text block; an
// array holds text parts and, when images is set, image_url parts. bad is
// the path of the first content that cannot be translated.
func parts(b *params.Body, at string, images bool) (blocks [][]byte, bad string) {
	p := "messages[" + at + "].content"
	content, _ := b.Get("messages", at, "content")
	switch {
	case len(content) > 0 && content[0] == '"':
		return [][]byte{fmt.Appendf(nil, `{"type":"text","text":%s}`, content)}, ""
	case len(content) == 0 || content[0] != '[':
		return nil, p
	}
	for j := 0; ; j++ {
		pj := strconv.Itoa(j)
		if _, ok := b.Get("messages", at, "content", pj); !ok {
			return blocks, ""
		}
		var block []byte
		switch str(b, "messages", at, "content", pj, "type") {
		case "text":
			if t, ok := b.Get("messages", at, "content", pj, "text"); ok && t[0] == '"' {
				block = fmt.Appendf(nil, `{"type":"text","text":%s}`, t)
			}
		case "image_url":
			if images {
				if block = image(b, at, pj); block == nil {
					return nil, p + "[" + pj + "].image_url.url"
				}
			}
		}
		if block == nil {
			return nil, p + "[" + pj + "]"
		}
		blocks = append(blocks, block)
	}
}

// image maps an image_url part: a data: URI of a supported type to a base64
// source, an http(s) URL to a url source; detail has no equivalent and is
// dropped. nil when the URL is neither.
func image(b *params.Body, at, pj string) []byte {
	raw, _ := b.Get("messages", at, "content", pj, "image_url", "url")
	u, ok := unquote(raw)
	if !ok {
		return nil
	}
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		mt, data, ok := strings.Cut(rest, ";base64,")
		if !ok || !slices.Contains(media, mt) {
			return nil
		}
		return fmt.Appendf(nil, `{"type":"image","source":{"type":"base64","media_type":%s,"data":%s}}`, quote(mt), quote(data))
	}
	if l := strings.ToLower(u); strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") {
		return fmt.Appendf(nil, `{"type":"image","source":{"type":"url","url":%s}}`, raw)
	}
	return nil
}

// toolUses maps the tool_calls of assistant messages[at] to tool_use blocks;
// the arguments must be a JSON object, which becomes the input.
func toolUses(b *params.Body, at string) (blocks [][]byte, bad string) {
	for j := 0; ; j++ {
		pj := strconv.Itoa(j)
		if _, ok := b.Get("messages", at, "tool_calls", pj); !ok {
			return blocks, ""
		}
		p := "messages[" + at + "].tool_calls[" + pj + "]"
		id, okID := b.Get("messages", at, "tool_calls", pj, "id")
		name, okName := b.Get("messages", at, "tool_calls", pj, "function", "name")
		if str(b, "messages", at, "tool_calls", pj, "type") != "function" || !okID || id[0] != '"' || !okName || name[0] != '"' {
			return nil, p
		}
		raw, _ := b.Get("messages", at, "tool_calls", pj, "function", "arguments")
		args, ok := unquote(raw)
		input := bytes.TrimSpace([]byte(args))
		if !ok || len(input) == 0 || input[0] != '{' || !jsontext.Value(input).IsValid() {
			return nil, p + ".function.arguments"
		}
		blocks = append(blocks, fmt.Appendf(nil, `{"type":"tool_use","id":%s,"name":%s,"input":%s}`, id, name, input))
	}
}

// fromTools maps the function tools; custom reports a tool of another type,
// which has no equivalent, and bad names a function tool without a name.
func fromTools(b *params.Body) (tools []byte, custom bool, bad string) {
	if v := get(b, "tools"); v[0] != '[' {
		return nil, true, ""
	}
	var all [][]byte
	for j := 0; ; j++ {
		pj := strconv.Itoa(j)
		if _, ok := b.Get("tools", pj); !ok {
			break
		}
		if str(b, "tools", pj, "type") != "function" {
			custom = true
			continue
		}
		name, ok := b.Get("tools", pj, "function", "name")
		if !ok && bad == "" {
			bad = "tools[" + pj + "].function.name"
		}
		schema, ok := b.Get("tools", pj, "function", "parameters")
		if !ok {
			schema = []byte(`{"type":"object"}`)
		}
		t := appendMember([]byte{'{'}, member("name", name))
		for _, m := range [][]byte{member("description", get(b, "tools", pj, "function", "description")),
			member("input_schema", schema), member("strict", get(b, "tools", pj, "function", "strict"))} {
			t = appendMember(t, m)
		}
		all = append(all, append(t, '}'))
	}
	return list(all), custom, bad
}

// fromToolChoice maps tool_choice to the members of the Messages one; ok is
// false for a choice with no equivalent (allowed_tools, custom).
func fromToolChoice(b *params.Body, v []byte) (string, bool) {
	if s, ok := unquote(v); ok {
		c, ok := map[string]string{"none": `"type":"none"`, "auto": `"type":"auto"`, "required": `"type":"any"`}[s]
		return c, ok
	}
	name, ok := b.Get("tool_choice", "function", "name")
	if str(b, "tool_choice", "type") != "function" || !ok {
		return "", false
	}
	return `"type":"tool","name":` + string(name), true
}

// member is "name":v; nil when v is nil.
func member(name string, v []byte) []byte {
	if v == nil {
		return nil
	}
	return append(append(quote(name), ':'), v...)
}

func appendMember(obj, m []byte) []byte {
	if m == nil {
		return obj
	}
	if len(obj) > 1 {
		obj = append(obj, ',')
	}
	return append(obj, m...)
}

func wrap(members string) []byte {
	if members == "" {
		return nil
	}
	return []byte("{" + members + "}")
}

// list is the JSON array of the values vs, [] when empty.
func list(vs [][]byte) []byte {
	return append(append([]byte{'['}, bytes.Join(vs, []byte{','})...), ']')
}

func get(b *params.Body, path ...string) []byte {
	v, _ := b.Get(path...)
	return v
}

// quote is s as a JSON string; s comes from decoded JSON or is ASCII, so it
// is valid UTF-8.
func quote(s string) []byte {
	q, _ := jsontext.AppendQuote(nil, s)
	return q
}

// unquote is the JSON string v, unescaped; ok is false for any other value.
func unquote(v []byte) (string, bool) {
	if len(v) == 0 || v[0] != '"' {
		return "", false
	}
	s, err := jsontext.AppendUnquote(nil, v)
	return string(s), err == nil
}

// str is the string at path, unescaped; "" when absent or not a string.
func str(b *params.Body, path ...string) string {
	s, _ := unquote(get(b, path...))
	return s
}

// counts are the token counts of Anthropic usage objects read so far: the
// latest integer of each field wins, as message_delta repeats them
// cumulatively (design section 8).
type counts struct {
	seen                                 bool // a usage object was read
	input, cacheRead, cacheWrite, output int64
	thinking                             *int64 // output_tokens_details.thinking_tokens; nil when not reported
}

// read takes each field of the usage object at path that is an integer.
func (c *counts) read(b *params.Body, path ...string) {
	if u := get(b, path...); len(u) == 0 || u[0] != '{' {
		return
	}
	c.seen = true
	for _, f := range []struct {
		name []string
		to   *int64
	}{{[]string{"input_tokens"}, &c.input}, {[]string{"cache_read_input_tokens"}, &c.cacheRead},
		{[]string{"cache_creation_input_tokens"}, &c.cacheWrite}, {[]string{"output_tokens"}, &c.output}} {
		if n, err := strconv.ParseInt(string(get(b, slices.Concat(path, f.name)...)), 10, 64); err == nil {
			*f.to = n
		}
	}
	if n, err := strconv.ParseInt(string(get(b, slices.Concat(path, []string{"output_tokens_details", "thinking_tokens"})...)), 10, 64); err == nil {
		c.thinking = &n
	}
}

// usage is the domain.Usage of the counts (USE-3), Anthropic's three input
// counts summed, and the chat usage object written back (design section 7);
// nil when no usage object was read.
func (c *counts) usage() (*domain.Usage, []byte) {
	if !c.seen {
		return nil, nil
	}
	u := &domain.Usage{Input: c.input + c.cacheRead + c.cacheWrite, CacheRead: c.cacheRead, CacheWrite: c.cacheWrite, Output: c.output, Reasoning: c.thinking}
	chat := fmt.Appendf(nil, `{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d,"cache_write_tokens":%d}`,
		u.Input, u.Output, u.Input+u.Output, u.CacheRead, u.CacheWrite)
	if c.thinking != nil {
		chat = fmt.Appendf(chat, `,"completion_tokens_details":{"reasoning_tokens":%d}`, *c.thinking)
	}
	return u, append(chat, '}')
}

// finish maps a stop_reason to a finish_reason (design section 7);
// pause_turn, which only server tools cause, and any reason Anthropic adds
// later are stop.
var finish = map[string]string{
	"max_tokens": "length", "model_context_window_exceeded": "length", "tool_use": "tool_calls", "refusal": "content_filter",
}

// ToChat maps a synchronous Messages answer from target t of provider p back
// to a chat completion (design section 7): the text blocks joined as the
// content, null when there is none, each through its own think-tag stripper
// when t declares inline_think_tags (RSN-12); each tool_use block a tool
// call whose arguments are its input bytes as a JSON string; thinking,
// redacted_thinking and any other block dropped; the finish reason mapped;
// the usage with Anthropic's three input counts summed; created is now.
// Observed keeps the provider's own stop_reason (USE-3). The error says the
// body is not a Messages answer: not a JSON object, a key repeated in an
// object read, no id, model or content array, or a malformed text or
// tool_use block. Phase 2 does not read p (design section 11, point 1).
func ToChat(t *domain.Target, _ *domain.Provider, body []byte, now time.Time) ([]byte, domain.Observed, error) {
	b, e := params.Scan(body, params.MessagesReply)
	if e != nil {
		return nil, domain.Observed{}, errors.New(e.Message)
	}
	id, model, content := get(b, "id"), get(b, "model"), get(b, "content")
	if len(id) == 0 || id[0] != '"' || len(model) == 0 || model[0] != '"' || len(content) == 0 || content[0] != '[' {
		return nil, domain.Observed{}, errors.New("the answer has no id, model or content array")
	}
	o := domain.Observed{ResponseModel: str(b, "model"), FinishReason: str(b, "stop_reason")}
	var text []byte // the joined text blocks, without their quotes
	var calls [][]byte
	hasText := false
	for j := 0; ; j++ {
		at := strconv.Itoa(j)
		if _, ok := b.Get("content", at); !ok {
			break
		}
		switch str(b, "content", at, "type") {
		case "text":
			raw := get(b, "content", at, "text")
			s, ok := unquote(raw)
			if !ok {
				return nil, domain.Observed{}, fmt.Errorf("content[%d] is a text block without text", j)
			}
			if t.InlineThinkTags {
				var st reasoning.Stripper
				if visible := st.Feed(s) + st.Flush(); st.Stripped {
					s, raw, o.Stripped = visible, quote(visible), true
				}
			}
			o.VisibleBytes += int64(len(s))
			text, hasText = append(text, raw[1:len(raw)-1]...), true
		case "tool_use":
			id, name, input := get(b, "content", at, "id"), get(b, "content", at, "name"), get(b, "content", at, "input")
			if len(id) == 0 || id[0] != '"' || len(name) == 0 || name[0] != '"' || len(input) == 0 || input[0] != '{' {
				return nil, domain.Observed{}, fmt.Errorf("content[%d] is a malformed tool_use block", j)
			}
			calls = append(calls, fmt.Appendf(nil, `{"id":%s,"type":"function","function":{"name":%s,"arguments":%s}}`, id, name, quote(string(input))))
			o.ToolCalls++
		}
	}
	msg := []byte(`{"role":"assistant","content":null`)
	if hasText {
		msg = fmt.Appendf(nil, `{"role":"assistant","content":"%s"`, text)
	}
	if calls != nil {
		msg = fmt.Appendf(msg, `,"tool_calls":%s`, list(calls))
	}
	msg = append(msg, '}')
	reason := cmp.Or(finish[o.FinishReason], "stop")
	out := fmt.Appendf(nil, `{"id":%s,"object":"chat.completion","created":%d,"model":%s,"choices":[{"index":0,"message":%s,"finish_reason":%q,"logprobs":null}]`,
		id, now.Unix(), model, msg, reason)
	var c counts
	c.read(b, "usage")
	if u, chat := c.usage(); u != nil {
		o.Usage = u
		out = fmt.Appendf(out, `,"usage":%s`, chat)
	}
	return append(out, '}'), o, nil
}
