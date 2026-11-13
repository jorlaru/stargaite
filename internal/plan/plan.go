// Package plan holds the one path from an inbound body to the bytes sent to a
// target, shared by the data plane and models explain (ADR-009, PAR-10):
// Check refuses a request once, Build edits the body per target. Neither
// reads anything but the body, the alias, the target and its provider, so the
// same input gives the same bytes (AC-17).
package plan

import (
	"cmp"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/params"
	"github.com/jorlaru/stargaite/internal/reasoning"
)

// Client is what Check read from the request, for Build.
type Client struct {
	Effort   string // requested effort: a canonical level, "budget:<N>" (RSN-1), or "" for none
	Cap      *int64 // requested output cap; nil when the client sent none
	Stream   bool
	Usage    bool // stream_options.include_usage is true (chat)
	Features params.Features
}

// schema is the inbound contract of ep; ok is false for an endpoint this
// package does not serve.
func schema(ep domain.Endpoint) (s params.Schema, out [][]string, ok bool) {
	switch ep {
	case domain.EndpointChat:
		return params.Chat, params.ChatOut, true
	case domain.EndpointMessages:
		return params.Messages, params.MessagesOut, true
	}
	return params.Schema{}, nil, false
}

// capNames are the client's output-cap members on ep (PAR-6).
func capNames(ep domain.Endpoint) []string {
	if ep == domain.EndpointMessages {
		return []string{"max_tokens"}
	}
	return []string{"max_completion_tokens", "max_tokens"}
}

// Check refuses a request before any target is tried (design section 5), for
// a body scanned with the inbound walk of ep and the alias m, which may be
// nil: an unknown top-level key without a rule, a type the proxy reads, an
// effort off the scale (RSN-11), a cap above the alias's (PAR-6).
func Check(ep domain.Endpoint, b *params.Body, m *domain.Model) (*Client, *domain.Error) {
	s, _, ok := schema(ep)
	if !ok {
		return nil, domain.NewError(domain.CodeInternalError, fmt.Sprintf("endpoint %q is not served", ep))
	}
	m = cmp.Or(m, &domain.Model{})
	for _, k := range b.Keys() {
		if !s.Known[k] && m.Params[k] == "" {
			e := domain.NewError(domain.CodeUnknownParameter, fmt.Sprintf("Unknown parameter: '%.200s'.", k))
			e.Param = k
			return nil, e
		}
	}
	if e := checkTypes(ep, b); e != nil {
		return nil, e
	}

	c := &Client{Features: params.Detect(string(ep), b)}
	c.Stream = raw(b, "stream") == "true"
	c.Usage = raw(b, "stream_options", "include_usage") == "true"
	in := reasoning.Inbound(ep)
	if v, ok := b.Get(in...); ok {
		l, ok := unquote(v)
		if !ok || !slices.Contains(domain.Levels, l) {
			e := domain.NewError(domain.CodeInvalidReasoningEffort,
				strings.Join(in, ".")+" must be one of "+strings.Join(domain.Levels, ", "))
			e.Param = strings.Join(in, ".")
			return nil, e
		}
		c.Effort = l
	} else if ep == domain.EndpointMessages {
		if n, ok := integer(b, "thinking", "budget_tokens"); ok {
			c.Effort = reasoning.BudgetPrefix + strconv.FormatInt(n, 10)
		} else if str(b, "thinking", "type") == "disabled" {
			c.Effort = "none"
		}
	}
	var name string
	c.Cap, name = largest(b, capNames(ep))
	if c.Cap != nil && m.MaxOutputTokens > 0 && *c.Cap > m.MaxOutputTokens {
		e := domain.NewError(domain.CodeMaxOutputTokensExceeded,
			fmt.Sprintf("%s %d is above the alias's max_output_tokens %d", name, *c.Cap, m.MaxOutputTokens))
		e.Param = name
		return nil, e
	}
	return c, nil
}

// checkTypes checks the type of every value the proxy reads (PAR-1).
func checkTypes(ep domain.Endpoint, b *params.Body) *domain.Error {
	bad := func(path, want string) *domain.Error {
		e := domain.NewError(domain.CodeInvalidRequest, path+" must be "+want)
		e.Param = path
		return e
	}
	kind := func(path ...string) byte {
		if v, ok := b.Get(path...); ok {
			return v[0]
		}
		return 0
	}
	is := func(k byte, allowed ...byte) bool { return k == 0 || slices.Contains(allowed, k) }
	switch {
	case !is(kind("stream"), 't', 'f'):
		return bad("stream", "a boolean")
	case !is(kind("stream_options"), '{'):
		return bad("stream_options", "an object")
	case !is(kind("stream_options", "include_usage"), 't', 'f'):
		return bad("stream_options.include_usage", "a boolean")
	case kind("messages") != '[':
		return bad("messages", "a non-empty array of messages")
	case !is(kind("response_format"), '{'):
		return bad("response_format", "an object")
	case !is(kind("response_format", "type"), '"'):
		return bad("response_format.type", "a string")
	}
	for _, n := range capNames(ep) {
		if _, ok := b.Get(n); ok {
			if _, ok := integer(b, n); !ok {
				return bad(n, "a non-negative integer")
			}
		}
	}
	for i := 0; ; i++ {
		at := strconv.Itoa(i)
		k := kind("messages", at)
		if k == 0 {
			if i == 0 {
				return bad("messages", "a non-empty array of messages")
			}
			break
		}
		if k != '{' {
			return bad("messages["+at+"]", "an object")
		}
		if kind("messages", at, "role") != '"' {
			return bad("messages["+at+"].role", "a string")
		}
	}
	if ep != domain.EndpointMessages {
		return nil
	}
	// The objects Build reads and edits on messages (RSN-1, design section 6).
	switch {
	case !is(kind("thinking"), '{'):
		return bad("thinking", "an object")
	case !is(kind("thinking", "type"), '"'):
		return bad("thinking.type", "a string")
	case !is(kind("output_config"), '{'):
		return bad("output_config", "an object")
	}
	if _, ok := b.Get("thinking", "budget_tokens"); ok {
		if _, ok := integer(b, "thinking", "budget_tokens"); !ok {
			return bad("thinking.budget_tokens", "a non-negative integer")
		}
	}
	return nil
}

// raw is the value at path as text; "" when absent.
func raw(b *params.Body, path ...string) string {
	v, _ := b.Get(path...)
	return string(v)
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
	v, _ := b.Get(path...)
	s, _ := unquote(v)
	return s
}

// integer is the non-negative integer at path.
func integer(b *params.Body, path ...string) (int64, bool) {
	v, ok := b.Get(path...)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(string(v), 10, 64)
	return n, err == nil && n >= 0
}

// largest is the largest cap among names in b, and its name; nil when none.
func largest(b *params.Body, names []string) (v *int64, name string) {
	for _, n := range names {
		if x, ok := integer(b, n); ok && (v == nil || x > *v) {
			v, name = &x, n
		}
	}
	return v, name
}

// Input is everything Build reads (phase-1 design section 9).
type Input struct {
	Endpoint domain.Endpoint
	Body     *params.Body     // scanned once with the endpoint's inbound walk
	Header   http.Header      // only anthropic-version and anthropic-beta, from /v1/messages (PAR-14)
	Model    *domain.Model    // the alias's settings; nil for targets verify (RSN-9)
	Upstream string           // upstream model id
	Target   *domain.Target   //
	Provider *domain.Provider // declarations only: no credential is reachable
	Client   *Client          // what Check returned
}

// Layers are the three recorded values of the effort and the cap (RSN-6, PAR-6).
type Layers[T any] struct{ Requested, Alias, Sent *T }

// Change is one entry of the change list (PAR-3).
type Change = reasoning.Change

// Outgoing is one request to one target.
type Outgoing struct {
	URL            string
	Header         http.Header // without the credential; transport adds it afterwards
	Body           []byte      // the exact bytes sent, and the bytes explain returns
	Changes        []Change
	Effort         Layers[string]
	EffortSource   string // client, alias_default, alias_forced, provider_default (RSN-6)
	EffortAdjusted bool
	MaxOutput      Layers[int64]
}

// Skip is a target excluded for the request, with its section 15 code and
// the parameter at fault; never an error of the request. Code internal_error
// marks a body Build could not edit, a fault of the proxy, with the reason in
// Param.
type Skip struct{ Code, Param string }

// Header joins the changes as the x-stargaite-params-changed value (PAR-3).
func Header(cs []Change) string {
	s := make([]string, len(cs))
	for i, c := range cs {
		s[i] = c.Name + ":" + c.Action
	}
	return strings.Join(s, ",")
}

// Build edits the inbound body for one target (design section 5): the
// endpoint, PAR-4, the alias's rules and PAR-12, the cap, the reasoning edit
// and its side keys, in the order of Changes. Drops are applied first, so the
// cap and the reasoning edit see the body without them; the rest is one Edit.
// Every sent value is read back from the result (PAR-8). Chat toward an anthropic target is
// the translated path of step C17 and is skipped until then.
func Build(in Input) (*Outgoing, *Skip) {
	ep, b, t, p := in.Endpoint, in.Body, in.Target, in.Provider
	m, c := cmp.Or(in.Model, &domain.Model{}), cmp.Or(in.Client, &Client{})
	s, outWalk, ok := schema(ep)
	native := ok && t.Kind == domain.KindChat &&
		(ep == domain.EndpointChat && p.Protocol == domain.ProtocolOpenAI || ep == domain.EndpointMessages && p.Protocol == domain.ProtocolAnthropic)
	if !native {
		return nil, &Skip{Code: domain.CodeEndpointNotSupported}
	}
	for _, f := range []struct {
		used bool
		cap  *bool
		name string
	}{
		{c.Features.Tools, t.Capabilities.Tools, domain.CapabilityTools},
		{c.Features.Structured, t.Capabilities.StructuredOutput, domain.CapabilityStructuredOutput},
		{c.Features.Strict, t.Capabilities.StrictSchema, domain.CapabilityStrictSchema},
		{c.Features.Vision, t.Capabilities.Vision, domain.CapabilityVision},
	} {
		if f.used && f.cap != nil && !*f.cap {
			return nil, &Skip{Code: domain.CodeCapabilityMismatch, Param: f.name}
		}
	}

	out := &Outgoing{Header: http.Header{"Content-Type": {"application/json"}}}
	model, err := jsontext.AppendQuote(nil, in.Upstream)
	if err != nil {
		return nil, &Skip{Code: domain.CodeInternalError, Param: err.Error()}
	}
	var drops []params.Op
	for _, k := range b.Keys() {
		rule := m.Params[k]
		switch {
		case slices.Contains(t.UnsupportedParams, k) && rule != domain.ParamDrop:
			return nil, &Skip{Code: domain.CodeUnsupportedParameter, Param: k}
		case rule == domain.ParamDrop && (!s.Known[k] || slices.Contains(t.UnsupportedParams, k)):
			drops = append(drops, params.Op{Path: []string{k}})
			out.Changes = append(out.Changes, Change{Name: k, Action: reasoning.ActionDropped})
		case rule == domain.ParamForward && !s.Known[k]:
			out.Changes = append(out.Changes, Change{Name: k, Action: "forwarded"})
		}
	}
	// The cap and the reasoning edit see the body without the dropped members,
	// so a member they write is written even when the client sent it.
	if len(drops) > 0 {
		raw, err := b.Edit(drops...)
		if err != nil {
			return nil, &Skip{Code: domain.CodeInternalError, Param: err.Error()}
		}
		var e *params.Error
		if b, e = params.ReadBack(raw, s.Walk); e != nil {
			return nil, &Skip{Code: domain.CodeInternalError, Param: e.Message}
		}
	}
	ops := []params.Op{{Path: []string{"model"}, Value: model}}

	capName := "max_tokens" // every cap toward anthropic (PAR-6)
	if p.Protocol == domain.ProtocolOpenAI {
		capName = cmp.Or(t.OutputCapParam, domain.DefaultOutputCapParam)
	}
	switch kept, _ := largest(b, capNames(ep)); {
	case kept == nil && m.MaxOutputTokens > 0:
		ops = append(ops, params.Op{Path: []string{capName}, Value: strconv.AppendInt(nil, m.MaxOutputTokens, 10)})
		out.Changes = append(out.Changes, Change{Name: capName, Action: reasoning.ActionAdded})
	case kept != nil && m.MaxOutputTokens == 0 && t.MaxOutputTokens > 0 && *kept > t.MaxOutputTokens:
		return nil, &Skip{Code: domain.CodeMaxOutputTokensExceeded}
	}

	choice := reasoning.Resolve(c.Effort, m.Reasoning)
	if strings.HasPrefix(choice.Level, reasoning.BudgetPrefix) && reasoning.Dialect(t.Reasoning) != domain.DialectThinkingBudget {
		return nil, &Skip{Code: domain.CodeCapabilityMismatch, Param: "thinking.budget_tokens"}
	}
	edit := reasoning.Translate(choice, b, ep, t.Reasoning)
	ops = append(ops, edit.Ops...)
	out.Changes = append(out.Changes, edit.Changes...)

	if out.Body, err = b.Edit(ops...); err != nil {
		return nil, &Skip{Code: domain.CodeInternalError, Param: err.Error()}
	}
	rb, e := params.ReadBack(out.Body, outWalk)
	if e != nil {
		return nil, &Skip{Code: domain.CodeInternalError, Param: e.Message}
	}

	sent := reasoning.Sent(rb, t.Reasoning)
	out.Effort = Layers[string]{Requested: opt(choice.Requested), Alias: opt(choice.Alias), Sent: opt(sent)}
	out.EffortSource, out.EffortAdjusted = choice.Source, edit.Adjusted
	if sent == "" {
		out.EffortSource = reasoning.SourceProviderDefault
	}
	out.MaxOutput = Layers[int64]{Requested: c.Cap}
	if v := m.MaxOutputTokens; v > 0 {
		out.MaxOutput.Alias = &v
	}
	out.MaxOutput.Sent, _ = largest(rb, append(capNames(ep), capName))

	base := strings.TrimSuffix(p.BaseURL, "/")
	if p.Protocol == domain.ProtocolOpenAI {
		out.URL = base + "/chat/completions"
		return out, nil
	}
	out.URL = base + "/v1/messages"
	out.Header.Set("anthropic-version", params.AnthropicVersion)
	for _, h := range []string{"anthropic-version", "anthropic-beta"} {
		if v := in.Header.Values(h); len(v) > 0 {
			out.Header[http.CanonicalHeaderKey(h)] = slices.Clone(v)
		}
	}
	return out, nil
}

func opt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
