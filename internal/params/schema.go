package params

import (
	"encoding/json/jsontext"
	"slices"
)

// Schema is an inbound endpoint's contract (PAR-1): its known top-level keys,
// pinned to the provider's public reference as read on AsOf, and the nested
// objects the proxy reads or writes, which Scan indexes and checks for
// repeated keys.
type Schema struct {
	AsOf  string          // the reference read date
	Known map[string]bool // top-level keys
	Walk  [][]string      // objects indexed and checked for repeats, inbound
}

// Chat is OpenAI Chat Completions: OpenAI publishes no versioned schema, so the
// pin is the API reference as read on AsOf, moved each release (R7).
var Chat = Schema{
	AsOf: "2026-11-13",
	Known: set("messages", "model", "audio", "frequency_penalty", "function_call", "functions", "logit_bias",
		"logprobs", "max_completion_tokens", "max_tokens", "metadata", "modalities", "moderation", "n",
		"parallel_tool_calls", "prediction", "presence_penalty", "prompt_cache_key", "prompt_cache_options",
		"prompt_cache_retention", "reasoning_effort", "response_format", "safety_identifier", "seed",
		"service_tier", "stop", "store", "stream", "stream_options", "temperature", "tool_choice", "tools",
		"top_logprobs", "top_p", "user", "verbosity", "web_search_options"),
	Walk: [][]string{
		{"stream_options"}, {"response_format"}, {"response_format", "json_schema"}, {"tool_choice"},
		{"tool_choice", "function"}, {"tools", "*"}, {"tools", "*", "function"}, {"messages", "*"},
		{"messages", "*", "content", "*"}, {"messages", "*", "content", "*", "image_url"},
		{"messages", "*", "tool_calls", "*"}, {"messages", "*", "tool_calls", "*", "function"},
		{"reasoning"}, {"thinking"}, {"output_config"},
	},
}

// Messages is Anthropic Messages: the key list is the API reference as read on
// AsOf, independent of the version header, because a client may send its own
// anthropic-version (PAR-14). Content blocks nested in a tool_result are
// walked for PAR-4 detection.
var Messages = Schema{
	AsOf: "2026-11-13",
	Known: set("max_tokens", "messages", "model", "cache_control", "container", "diagnostics", "inference_geo",
		"metadata", "output_config", "service_tier", "speed", "stop_sequences", "stream", "system", "temperature",
		"thinking", "tool_choice", "tools", "top_k", "top_p"),
	Walk: [][]string{
		{"thinking"}, {"output_config"}, {"output_config", "format"}, {"tools", "*"}, {"messages", "*"},
		{"messages", "*", "content", "*"}, {"messages", "*", "content", "*", "content", "*"}, {"reasoning"},
	},
}

// AnthropicVersion is the anthropic-version header sent when the client sends
// none (PAR-14).
const AnthropicVersion = "2023-06-01"

// ChatOut and MessagesOut are the walks for reading an outgoing body back
// (PAR-8): the objects any reasoning dialect writes. Caps, fragments and the
// other dialect keys are top-level members. Each inbound walk holds its Out
// walk, so a repeat that a forward rule would carry to the read-back is
// refused at step 2 instead.
var (
	ChatOut     = [][]string{{"reasoning"}, {"thinking"}, {"output_config"}}
	MessagesOut = [][]string{{"reasoning"}, {"thinking"}, {"output_config"}}
)

// ChatReply and MessagesReply are the walks for observing a response, a
// stream chunk or event, or a provider's error body: model, finish reason,
// usage, text, tool calls and the error object.
var (
	ChatReply = [][]string{
		{"choices", "*"}, {"choices", "*", "message"}, {"choices", "*", "message", "tool_calls", "*"},
		{"choices", "*", "delta"}, {"choices", "*", "delta", "tool_calls", "*"},
		{"usage"}, {"usage", "prompt_tokens_details"}, {"usage", "completion_tokens_details"}, {"error"},
	}
	MessagesReply = [][]string{
		{"content", "*"}, {"usage"}, {"usage", "output_tokens_details"},
		{"message"}, {"message", "usage"}, {"message", "usage", "output_tokens_details"},
		{"delta"}, {"content_block"}, {"error"},
	}
)

func set(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// Features are the contractual features a request uses (PAR-4).
type Features struct{ Tools, Structured, Strict, Vision bool }

// Detect reads the features of a body scanned with the walk of endpoint ep,
// "chat" or "messages"; any other endpoint uses none.
func Detect(ep string, b *Body) Features {
	present := func([]byte) bool { return true }
	isTrue := func(v []byte) bool { return string(v) == "true" }
	switch ep {
	case "chat":
		return Features{
			Tools:      b.has(present, "tools") || b.has(present, "tool_choice"),
			Structured: b.has(is("json_schema"), "response_format", "type"),
			Strict: b.has(isTrue, "response_format", "json_schema", "strict") ||
				b.has(isTrue, "tools", "*", "function", "strict"),
			Vision: b.has(is("image_url", "input_audio", "file"), "messages", "*", "content", "*", "type"),
		}
	case "messages":
		media := is("image", "document")
		return Features{
			Tools:      b.has(present, "tools") || b.has(present, "tool_choice"),
			Structured: b.has(is("json_schema"), "output_config", "format", "type"),
			Strict:     b.has(isTrue, "tools", "*", "strict"),
			Vision: b.has(media, "messages", "*", "content", "*", "type") ||
				b.has(media, "messages", "*", "content", "*", "content", "*", "type"),
		}
	}
	return Features{}
}

// is matches a JSON string equal, once unescaped, to one of values.
func is(values ...string) func([]byte) bool {
	return func(v []byte) bool {
		if len(v) == 0 || v[0] != '"' {
			return false
		}
		s, err := jsontext.AppendUnquote(nil, v)
		return err == nil && slices.Contains(values, string(s))
	}
}

// has reports whether ok holds for the value at any path matching pattern,
// where "*" stands for each element of an indexed array.
func (b *Body) has(ok func([]byte) bool, pattern ...string) bool {
	var match func(n *node, p []string) bool
	match = func(n *node, p []string) bool {
		if len(p) == 0 {
			return ok(b.raw[n.start:n.end])
		}
		if p[0] == "*" && n.indexed && !n.obj {
			for _, k := range n.kids {
				if match(k, p[1:]) {
					return true
				}
			}
			return false
		}
		c := n.child(p[0])
		return c != nil && match(c, p[1:])
	}
	return match(b.root, pattern)
}
