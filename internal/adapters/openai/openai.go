// Package openai reads the responses of providers of the openai protocol to
// chat completions on the native path (design sections 7 and 9), synchronous
// and streamed. The relay sends their bytes unchanged, except the think-tag
// edits of a target that declares inline_think_tags (RSN-12); this package
// observes them and makes those edits.
package openai

import (
	"encoding/json/jsontext"
	"errors"
	"strconv"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/params"
	"github.com/jorlaru/stargaite/internal/reasoning"
)

// Observe reads a synchronous chat completion from target t of provider p
// and returns the body to relay: the provider's bytes, except that for a
// target that declares inline_think_tags every message content that held a
// think block is re-encoded without it (RSN-12). Observed: the response
// model, the first choice's finish reason, the usage (USE-3), the visible
// bytes of every message content, the tool calls and whether a block was
// removed, and the cost at p's cost_report path. The error says the body is
// not a JSON object or repeats a key in an object it reads, a 2xx that is not
// relayed (design section 10).
func Observe(t *domain.Target, p *domain.Provider, body []byte) ([]byte, domain.Observed, error) {
	cr := report(p)
	b, e := params.Scan(body, params.Reply(params.ChatReply, cr.Path))
	if e != nil {
		return nil, domain.Observed{}, errors.New(e.Message)
	}
	o := domain.Observed{ResponseModel: str(b, "model"), Cost: params.Cost(b, cr.Path, cr.USDPerUnit)}
	var ops []params.Op
	for i := 0; ; i++ {
		c := strconv.Itoa(i)
		if _, ok := b.Get("choices", c); !ok {
			break
		}
		if i == 0 {
			o.FinishReason = str(b, "choices", c, "finish_reason")
		}
		text := str(b, "choices", c, "message", "content")
		if t.InlineThinkTags {
			var s reasoning.Stripper
			if visible := s.Feed(text) + s.Flush(); s.Stripped {
				ops = append(ops, quote([]string{"choices", c, "message", "content"}, visible))
				text, o.Stripped = visible, true
			}
		}
		o.VisibleBytes += int64(len(text))
		for j := 0; ; j++ {
			if _, ok := b.Get("choices", c, "message", "tool_calls", strconv.Itoa(j)); !ok {
				break
			}
			o.ToolCalls++
		}
	}
	o.Usage = usage(b)
	if ops != nil {
		var err error
		if body, err = b.Edit(ops...); err != nil {
			return nil, domain.Observed{}, err
		}
	}
	return body, o, nil
}

// quote is the op that sets the string at path to s, re-encoded. s comes
// from decoded JSON cut at ASCII tags only, so it is valid UTF-8.
func quote(path []string, s string) params.Op {
	v, _ := jsontext.AppendQuote(nil, s)
	return params.Op{Path: path, Value: v}
}

// report is p's cost_report; the zero one, which reads no cost, when p
// declares none.
func report(p *domain.Provider) domain.CostReport {
	if p.CostReport == nil {
		return domain.CostReport{}
	}
	return *p.CostReport
}

// usage is the usage object of a response or a chunk (USE-3, phase-3 design
// section 6); nil when it is absent or null. The output is the larger of
// completion_tokens and total_tokens minus prompt_tokens, since some
// providers leave reasoning out of completion_tokens; reasoning is only an
// integer the provider reports, nil when absent or null; cache reads fall
// back to Mistral's prompt_token_details.
func usage(b *params.Body) *domain.Usage {
	if u, ok := b.Get("usage"); !ok || u[0] != '{' {
		return nil
	}
	u := &domain.Usage{
		Input:      num(b, "usage", "prompt_tokens"),
		CacheRead:  num(b, "usage", "prompt_tokens_details", "cached_tokens"),
		CacheWrite: num(b, "usage", "prompt_tokens_details", "cache_write_tokens"),
	}
	if _, ok := integer(b, "usage", "prompt_tokens_details", "cached_tokens"); !ok {
		u.CacheRead = num(b, "usage", "prompt_token_details", "cached_tokens")
	}
	u.Output = max(num(b, "usage", "completion_tokens"), num(b, "usage", "total_tokens")-u.Input)
	if r, ok := integer(b, "usage", "completion_tokens_details", "reasoning_tokens"); ok {
		u.Reasoning = &r
	}
	return u
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
	n, _ := integer(b, path...)
	return n
}

// integer is the integer at path; false when absent or not an integer.
func integer(b *params.Body, path ...string) (int64, bool) {
	v, _ := b.Get(path...)
	n, err := strconv.ParseInt(string(v), 10, 64)
	return n, err == nil
}
