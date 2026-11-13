// Package openai reads the responses of providers of the openai protocol to
// chat completions on the native path (design section 7). The relay sends
// their bytes unchanged; this package only observes them.
package openai

import (
	"encoding/json/jsontext"
	"errors"
	"strconv"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/params"
)

// Observe reads a synchronous chat completion: the response model, the
// first choice's finish reason, the usage (USE-3), the visible bytes of every
// message content and the tool calls. The error says the body is not a JSON
// object or repeats a key in an object it reads, a 2xx that is not relayed
// (design section 10).
func Observe(body []byte) (domain.Observed, error) {
	b, e := params.Scan(body, params.ChatReply)
	if e != nil {
		return domain.Observed{}, errors.New(e.Message)
	}
	o := domain.Observed{ResponseModel: str(b, "model")}
	for i := 0; ; i++ {
		c := strconv.Itoa(i)
		if _, ok := b.Get("choices", c); !ok {
			break
		}
		if i == 0 {
			o.FinishReason = str(b, "choices", c, "finish_reason")
		}
		o.VisibleBytes += int64(len(str(b, "choices", c, "message", "content")))
		for j := 0; ; j++ {
			if _, ok := b.Get("choices", c, "message", "tool_calls", strconv.Itoa(j)); !ok {
				break
			}
			o.ToolCalls++
		}
	}
	if u, ok := b.Get("usage"); ok && u[0] == '{' {
		o.Usage = &domain.Usage{
			Input:      num(b, "usage", "prompt_tokens"),
			CacheRead:  num(b, "usage", "prompt_tokens_details", "cached_tokens"),
			CacheWrite: num(b, "usage", "prompt_tokens_details", "cache_write_tokens"),
			Output:     num(b, "usage", "completion_tokens"),
		}
		if _, ok := b.Get("usage", "completion_tokens_details", "reasoning_tokens"); ok {
			r := num(b, "usage", "completion_tokens_details", "reasoning_tokens")
			o.Usage.Reasoning = &r
		}
	}
	return o, nil
}

// str is the string at path, unescaped; "" when absent or not a string.
func str(b *params.Body, path ...string) string {
	v, ok := b.Get(path...)
	if !ok || v[0] != '"' {
		return ""
	}
	s, err := jsontext.AppendUnquote(nil, v)
	if err != nil {
		return ""
	}
	return string(s)
}

// num is the integer at path; 0 when absent or not an integer.
func num(b *params.Body, path ...string) int64 {
	v, _ := b.Get(path...)
	n, _ := strconv.ParseInt(string(v), 10, 64)
	return n
}
