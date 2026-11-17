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

	"github.com/jorlaru/stargaite/internal/adapters/anthropic"
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
	case domain.EndpointEmbeddings:
		return params.Embeddings, nil, true
	}
	return params.Schema{}, nil, false
}

// Inbound is the client's side of ep, for every reader of an inbound body: the
// walk it is scanned with (PAR-1), nil on embeddings and rerank, where only
// the top level is indexed; and on chat and messages the client's output-cap
// members (PAR-6) and the path of its effort member (kick-off section 9.1),
// which embeddings and rerank do not have.
func Inbound(ep domain.Endpoint) (walk [][]string, caps, effort []string) {
	s, _, _ := schema(ep)
	if ep != domain.EndpointChat && ep != domain.EndpointMessages {
		return s.Walk, nil, nil
	}
	return s.Walk, capNames(ep), reasoning.Inbound(ep)
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
// nil: an unknown top-level key that no rule names, of the alias or in
// declared (every key a rule of the alias's targets or their providers names;
// nil refuses more, never less), a type the proxy reads, an
// effort off the scale (RSN-11), a cap above the alias's (PAR-6). On
// embeddings only the unknown key is refused, and the Client is empty: no
// stream, effort, cap or feature (phase-4 design section 4). Rerank has no
// schema: nothing is refused and the Client is empty, so a stream member is
// forwarded and the answer read whole (phase-4 design section 5).
func Check(ep domain.Endpoint, b *params.Body, m *domain.Model, declared map[string]bool) (*Client, *domain.Error) {
	if ep == domain.EndpointRerank {
		return &Client{}, nil
	}
	s, _, ok := schema(ep)
	if !ok {
		return nil, domain.NewError(domain.CodeInternalError, fmt.Sprintf("endpoint %q is not served", ep))
	}
	m = cmp.Or(m, &domain.Model{})
	for _, k := range b.Keys() {
		if !s.Known[k] && m.Params[k] == "" && !declared[k] {
			e := domain.NewError(domain.CodeUnknownParameter, fmt.Sprintf("Unknown parameter: '%.200s'.", k))
			e.Param = k
			return nil, e
		}
	}
	if ep == domain.EndpointEmbeddings {
		return &Client{}, nil
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
	Level          string // the body's level member, as Level reads it back (gen_ai.request.reasoning.level)
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
// endpoint, PAR-4, the protocol mapping, the rules and PAR-12, the
// cap, the reasoning edit and its side keys, and on a native chat stream the
// include_usage a request provider needs (USE-8), in the order of Changes. Drops
// are applied first, so the mapping, the cap and the reasoning edit see the
// body without them; the rest is one Edit. Chat toward an anthropic target is
// the translated path: the new body of anthropic.FromChat, listed
// protocol:translated, takes the cap and the reasoning edit in place of the
// client's body, and a key with no equivalent is handled like an unsupported
// one (PAR-12). Every sent value is read back from the result (PAR-8).
// Embeddings and rerank are passthrough's alone.
func Build(in Input) (*Outgoing, *Skip) {
	if in.Endpoint == domain.EndpointEmbeddings || in.Endpoint == domain.EndpointRerank {
		return passthrough(in)
	}
	ep, b, t, p := in.Endpoint, in.Body, in.Target, in.Provider
	m, c := cmp.Or(in.Model, &domain.Model{}), cmp.Or(in.Client, &Client{})
	s, outWalk, ok := schema(ep)
	chat := ok && t.Kind == domain.KindChat
	translated := chat && ep == domain.EndpointChat && p.Protocol == domain.ProtocolAnthropic
	native := chat && (ep == domain.EndpointChat && p.Protocol == domain.ProtocolOpenAI || ep == domain.EndpointMessages && p.Protocol == domain.ProtocolAnthropic)
	if !native && !translated {
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
	var translatedBody []byte
	var noEquivalent []string
	var untranslatable string
	if translated {
		translatedBody, noEquivalent, untranslatable = anthropic.FromChat(b)
		out.Changes = append(out.Changes, Change{Name: "protocol", Action: reasoning.ActionTranslated})
	}
	drops, changes, skip := rules(b, m, t, p, s, noEquivalent)
	if skip != nil {
		return nil, skip
	}
	out.Changes = append(out.Changes, changes...)
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
		if translated {
			translatedBody, _, untranslatable = anthropic.FromChat(b)
		}
	}
	// mcp_servers that a rule lets through reaches the Messages body as sent,
	// where it gives the model tools, as on /v1/messages (PAR-4).
	if _, ok := b.Get("mcp_servers"); ok && translated && t.Capabilities.Tools != nil && !*t.Capabilities.Tools {
		return nil, &Skip{Code: domain.CodeCapabilityMismatch, Param: domain.CapabilityTools}
	}
	// Content inside a dropped member does not skip the target (PAR-12).
	if untranslatable != "" {
		return nil, &Skip{Code: domain.CodeUnsupportedParameter, Param: untranslatable}
	}
	if translated {
		var e *params.Error
		if b, e = params.ReadBack(translatedBody, outWalk); e != nil {
			return nil, &Skip{Code: domain.CodeInternalError, Param: e.Message}
		}
	}
	ops := []params.Op{{Path: []string{"model"}, Value: model}}

	capName, declared := "max_tokens", false // every cap toward anthropic (PAR-6)
	if p.Protocol == domain.ProtocolOpenAI {
		capName, declared = cmp.Or(t.OutputCapParam, domain.DefaultOutputCapParam), t.OutputCapParam != ""
	}
	// The requested cap is the client's even when a drop rule removed its
	// members: it is checked and sent as is, and the alias cap only fills in
	// for a client that sent none. Toward a target that declares its cap's
	// name, the client's cap is one member under that name, holding the larger
	// value the client sent; each member under another name is listed
	// translated, and so is the declared name when the client sent it with
	// another value or a rule dropped every member (PAR-3).
	switch kept, _ := largest(b, capNames(ep)); {
	case c.Cap == nil && m.MaxOutputTokens > 0:
		ops = append(ops, params.Op{Path: []string{capName}, Value: strconv.AppendInt(nil, m.MaxOutputTokens, 10)})
		out.Changes = append(out.Changes, Change{Name: capName, Action: reasoning.ActionAdded})
	case c.Cap != nil && m.MaxOutputTokens == 0 && t.MaxOutputTokens > 0 && *c.Cap > t.MaxOutputTokens:
		return nil, &Skip{Code: domain.CodeMaxOutputTokensExceeded}
	case c.Cap != nil && (declared || kept == nil):
		v := strconv.AppendInt(nil, *c.Cap, 10)
		renamed := false
		for _, name := range capNames(ep) {
			if _, ok := b.Get(name); ok && name != capName {
				ops = append(ops, params.Op{Path: []string{name}})
				out.Changes = append(out.Changes, Change{Name: name, Action: reasoning.ActionTranslated})
				renamed = true
			}
		}
		if sent, ok := b.Get(capName); renamed || string(sent) != string(v) {
			ops = append(ops, params.Op{Path: []string{capName}, Value: v})
			if string(sent) != string(v) && (ok || !renamed) {
				out.Changes = append(out.Changes, Change{Name: capName, Action: reasoning.ActionTranslated})
			}
		}
	}

	choice := reasoning.Resolve(c.Effort, m.Reasoning)
	if strings.HasPrefix(choice.Level, reasoning.BudgetPrefix) && reasoning.Dialect(t.Reasoning) != domain.DialectThinkingBudget {
		return nil, &Skip{Code: domain.CodeCapabilityMismatch, Param: "thinking.budget_tokens"}
	}
	edit := reasoning.Translate(choice, b, ep, t.Reasoning)
	ops = append(ops, edit.Ops...)
	out.Changes = append(out.Changes, edit.Changes...)

	// USE-8: a request provider streams usage only when asked. An explicit
	// false of the client is a changed value, absent it is an addition (phase-3
	// design section 17 point 21); a target that refuses stream_options, by
	// either declaration, is not sent it.
	if native && ep == domain.EndpointChat && c.Stream && !c.Usage && p.StreamUsage == domain.StreamUsageRequest && !refuses(t, p, "stream_options") {
		action := reasoning.ActionAdded
		if _, ok := b.Get("stream_options", "include_usage"); ok {
			action = reasoning.ActionTranslated
		}
		ops = append(ops, params.Op{Path: []string{"stream_options", "include_usage"}, Value: []byte("true")})
		out.Changes = append(out.Changes, Change{Name: "stream_options", Action: action})
	}

	if out.Body, err = b.Edit(ops...); err != nil {
		return nil, &Skip{Code: domain.CodeInternalError, Param: err.Error()}
	}
	rb, e := params.ReadBack(out.Body, outWalk)
	if e != nil {
		return nil, &Skip{Code: domain.CodeInternalError, Param: e.Message}
	}

	sent := reasoning.Sent(rb, t.Reasoning)
	out.Effort = Layers[string]{Requested: opt(choice.Requested), Alias: opt(choice.Alias), Sent: opt(sent)}
	out.EffortSource, out.EffortAdjusted, out.Level = choice.Source, edit.Adjusted, reasoning.Level(rb, t.Reasoning)
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

// targetFact is the rule that states whether t, of provider p, takes k: the
// target's own, else none when t lists k in unsupported_params, which leaves
// the remedy to the alias (PAR-12), else its provider's. The more specific
// declaration wins.
func targetFact(t *domain.Target, p *domain.Provider, k string) string {
	if r := t.Params[k]; r != "" || slices.Contains(t.UnsupportedParams, k) {
		return r
	}
	return p.Params[k]
}

// refuses is the one answer to "does t, of provider p, take k": no when its
// fact is drop, or t lists k in unsupported_params (PAR-12). rules and the
// proxy's own stream_options write read it.
func refuses(t *domain.Target, p *domain.Provider, k string) bool {
	return targetFact(t, p, k) == domain.ParamDrop || slices.Contains(t.UnsupportedParams, k)
}

// rules applies the rules of the target, its provider and the alias, and
// PAR-12, to the top-level keys of b toward t, in body order (PAR-2). The
// fact about t is targetFact's; the alias's rule is a policy; between the
// two a drop wins. A key is removed when the fact is drop, or when the
// policy is drop and t refuses the key or it is unknown; else a key t
// refuses, or one with no equivalent on the translated path, skips t; a
// reference key is unchanged; an unknown key is sent when a rule forwards
// it, and skips t when none does. Each removal is an op of drops, and each
// is listed with every forward in changes (PAR-3).
func rules(b *params.Body, m *domain.Model, t *domain.Target, p *domain.Provider, s params.Schema, noEquivalent []string) (drops []params.Op, changes []Change, skip *Skip) {
	for _, k := range b.Keys() {
		fact, policy, known := targetFact(t, p, k), m.Params[k], s.Known[k]
		refused := refuses(t, p, k) || slices.Contains(noEquivalent, k)
		switch {
		case fact == domain.ParamDrop || policy == domain.ParamDrop && (refused || !known):
			drops = append(drops, params.Op{Path: []string{k}})
			changes = append(changes, Change{Name: k, Action: reasoning.ActionDropped})
		case refused:
			return nil, nil, &Skip{Code: domain.CodeUnsupportedParameter, Param: k}
		case known:
		case fact == domain.ParamForward || policy == domain.ParamForward:
			changes = append(changes, Change{Name: k, Action: "forwarded"})
		default: // a key another target of the alias declared (PAR-12)
			return nil, nil, &Skip{Code: domain.CodeUnsupportedParameter, Param: k}
		}
	}
	return drops, changes, nil
}

// passthrough is Build for embeddings and rerank (kick-off section 7.1): an
// embeddings target of the openai protocol, called at base_url +
// /embeddings, or a rerank target of the openai protocol whose provider has
// a rerank_url, called there; any other target is skipped. On embeddings the
// rules and PAR-12 apply as on chat; rerank applies none. Only
// "model" is replaced besides them, which PAR-3 does not list. No cap, effort
// or include_usage is ever written, so the source is provider_default (RSN-6)
// and every other layer is nil.
func passthrough(in Input) (*Outgoing, *Skip) {
	t, p := in.Target, in.Provider
	out := &Outgoing{Header: http.Header{"Content-Type": {"application/json"}}, EffortSource: reasoning.SourceProviderDefault}
	switch {
	case p.Protocol != domain.ProtocolOpenAI:
		return nil, &Skip{Code: domain.CodeEndpointNotSupported}
	case in.Endpoint == domain.EndpointEmbeddings && t.Kind == domain.KindEmbeddings:
		out.URL = strings.TrimSuffix(p.BaseURL, "/") + "/embeddings"
	case in.Endpoint == domain.EndpointRerank && t.Kind == domain.KindRerank && p.RerankURL != "":
		out.URL = p.RerankURL
	default:
		return nil, &Skip{Code: domain.CodeEndpointNotSupported}
	}
	model, err := jsontext.AppendQuote(nil, in.Upstream)
	if err != nil {
		return nil, &Skip{Code: domain.CodeInternalError, Param: err.Error()}
	}
	ops := []params.Op{{Path: []string{"model"}, Value: model}}
	if in.Endpoint == domain.EndpointEmbeddings {
		drops, changes, skip := rules(in.Body, cmp.Or(in.Model, &domain.Model{}), t, p, params.Embeddings, nil)
		if skip != nil {
			return nil, skip // 422 unsupported_parameter when no target is left
		}
		ops, out.Changes = append(drops, ops...), changes
	}
	if out.Body, err = in.Body.Edit(ops...); err != nil {
		return nil, &Skip{Code: domain.CodeInternalError, Param: err.Error()}
	}
	return out, nil
}

func opt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
