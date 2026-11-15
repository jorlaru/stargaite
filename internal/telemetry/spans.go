package telemetry

import (
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/usage"
)

// Schema v1 (kick-off annex D, phase-3 design section 10): the gen_ai names
// are those of semantic-conventions-genai at commit
// e6658f14c9d9ea0760825f029cb82e7673f48fab, and spans carry no schemaUrl
// (TEL-9). testdata/spans.json, the golden, records the same commit; a change
// of a name or of the attribute set is a new schema version.
const (
	SchemaVersion = 1
	operation     = "chat" // gen_ai.operation.name of both endpoints
)

// Context is the trace a request belongs to (TEL-6).
type Context struct {
	Trace  [16]byte
	Parent [8]byte // zero for a root
	Remote bool    // Parent came from a traceparent
}

// Parse reads the values of the traceparent header as W3C Trace Context
// Level 1, strictly: exactly one value; a version of two lowercase hex digits,
// not ff; for version 00 exactly 55 characters, for a higher one at least 55
// and either exactly 55 or a "-" at offset 55, the fields read at fixed
// offsets; a trace id of 32 and a parent id of 16 lowercase hex digits,
// neither all zero; flags of two lowercase hex digits, read for nothing
// (TEL-7 ignores the sampled bit). ok is false for anything else.
func Parse(values []string) (Context, bool) {
	if len(values) != 1 {
		return Context{}, false
	}
	v := values[0]
	if len(v) < 55 || !lowerHex(v[:2]) || v[:2] == "ff" || v[:2] == "00" && len(v) != 55 || len(v) > 55 && v[55] != '-' ||
		v[2] != '-' || v[35] != '-' || v[52] != '-' || !lowerHex(v[3:35]) || !lowerHex(v[36:52]) || !lowerHex(v[53:55]) {
		return Context{}, false
	}
	c := Context{Remote: true}
	_, _ = hex.Decode(c.Trace[:], []byte(v[3:35])) // lowerHex checked it
	_, _ = hex.Decode(c.Parent[:], []byte(v[36:52]))
	if c.Trace == [16]byte{} || c.Parent == [8]byte{} {
		return Context{}, false
	}
	return c, true
}

// lowerHex reports whether s is made of lowercase hex digits only.
func lowerHex(s string) bool {
	return strings.Trim(s, "0123456789abcdef") == ""
}

// NewContext is a new root: 16 random bytes, never all zero (TEL-6).
func NewContext() Context {
	var c Context
	random(c.Trace[:])
	return c
}

// Sampled reports whether a request with the 64-bit random draw is traced
// at rate (TEL-7): always at 1, never at 0, else when the draw is below
// rate × 2^64.
func Sampled(draw uint64, rate float64) bool {
	return rate >= 1 || rate > 0 && draw < uint64(rate*0x1p64)
}

// Request is what done hands Build: the request's own facts and one Attempt
// per provider call, in order.
type Request struct {
	Context             Context
	Key, Alias, Session string
	Endpoint            domain.Endpoint
	Stream              bool
	Version             string
	Revision            int64
	Status              int    // what the request line logs
	Code, Message       string // the kick-off section 15 error; "" on success
	Started, Ended      time.Time
	Attempts            []Attempt
	Input, Output       []byte // content, set only under the opt-in (TEL-8)
	ContentMax          int64  // content_max_bytes
}

// Attempt is one provider call: its record, and what the record does not
// hold.
type Attempt struct {
	Record   usage.Record
	Provider string // gen_ai.provider.name
	Host     string // of the provider's base_url
	Port     int64
	Dialect  string
	Stripped bool
	Level    string    // the outgoing body's level member: gen_ai.request.reasoning.level
	Message  string    // the kick-off section 15 message the call ended with, already redacted; "" on success
	Started  time.Time // the call's start and end, at the precision of the request span's
	Ended    time.Time
}

// Build returns the spans of a request (phase-3 design section 10): the
// request span (SERVER, a child of the remote parent when there is one, else
// a root), then one CLIENT child "attempt {target}" for each attempt but the
// last, each with a fresh random id (TEL-6). The request span takes the last
// attempt's attributes. Usage and cost sit on an attempt span only when its
// call answered 2xx, and on the request span only for the last attempt, so a
// backend that adds them up never counts a call twice (TEL-10). A failure
// sets status ERROR with its message, error.type and the HTTP status (TEL-5).
// Content, given only under the opt-in, goes on the request span alone
// (TEL-8).
func Build(r Request) []Span {
	req := Span{Trace: r.Context.Trace, Parent: r.Context.Parent, Remote: r.Context.Remote, Name: operation, Kind: KindServer,
		Start: r.Started, End: r.Ended, Failed: r.Code != "", Message: r.Message}
	random(req.ID[:])
	if r.Alias != "" {
		req.Name += " " + r.Alias
	}
	spans := []Span{req}
	n := len(r.Attempts)
	for i := range max(n-1, 0) {
		a := &r.Attempts[i]
		s := Span{Trace: req.Trace, Parent: req.ID, Name: "attempt " + a.Record.Target, Kind: KindClient,
			Start: a.Started, End: a.Ended, Failed: a.Record.ErrorType != "", Message: a.Message}
		random(s.ID[:])
		s.Attrs = append(common(r, a), called(a, a.Record.StartedAt)...)
		if a.Record.ErrorType != "" {
			s.Attrs = append(s.Attrs, Attr{"error.type", a.Record.ErrorType})
		}
		if a.Record.StatusCode != 0 {
			s.Attrs = append(s.Attrs, Attr{"http.response.status_code", int64(a.Record.StatusCode)})
		}
		spans = append(spans, s)
	}
	var last *Attempt
	if n > 0 {
		last = &r.Attempts[n-1]
	}
	attrs := common(r, last)
	if r.Stream {
		attrs = append(attrs, Attr{"gen_ai.request.stream", true})
	}
	if last != nil {
		attrs = append(attrs, called(last, r.Started.UnixMilli())...)
	}
	attrs = append(attrs, Attr{"stargaite.attempts", int64(n)})
	if r.Code != "" {
		attrs = append(attrs, Attr{"error.type", r.Code})
	}
	attrs = append(attrs, Attr{"http.response.status_code", int64(r.Status)})
	cut := false
	for _, c := range []struct {
		name  string
		value []byte
	}{{"gen_ai.input.messages", r.Input}, {"gen_ai.output.messages", r.Output}} {
		if len(c.value) > 0 {
			s, short := content(c.value, r.ContentMax)
			attrs, cut = append(attrs, Attr{c.name, s}), cut || short
		}
	}
	if cut {
		attrs = append(attrs, Attr{"stargaite.content.truncated", true})
	}
	spans[0].Attrs = attrs
	return spans
}

// content is b as a content attribute (TEL-8): a string, invalid UTF-8
// replaced (the JSON encoding carries none), cut at a UTF-8 boundary to at
// most limit bytes; cut reports whether it was.
func content(b []byte, limit int64) (s string, cut bool) {
	s = strings.ToValidUTF8(string(b), "\uFFFD")
	if int64(len(s)) <= limit {
		return s, false
	}
	i := int(max(limit, 0))
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i], true
}

// common are the attributes of every span of r (TEL-4, TEL-13), with the
// compatibility attributes Langfuse filters by (TEL-11): the target only once
// a provider was called (a, nil before).
func common(r Request, a *Attempt) []Attr {
	attrs := []Attr{
		{"stargaite.schema.version", int64(SchemaVersion)}, {"stargaite.version", r.Version}, {"stargaite.config.revision", r.Revision},
		{"stargaite.key", r.Key}, {"langfuse.observation.metadata.stargaite_key", r.Key},
		{"stargaite.endpoint", string(r.Endpoint)}, {"gen_ai.operation.name", operation},
	}
	if r.Alias != "" {
		attrs = append(attrs, Attr{"stargaite.model", r.Alias}, Attr{"langfuse.observation.metadata.stargaite_model", r.Alias},
			Attr{"gen_ai.request.model", r.Alias})
	}
	if a != nil {
		attrs = append(attrs, Attr{"stargaite.target", a.Record.Target}, Attr{"langfuse.observation.metadata.stargaite_target", a.Record.Target})
	}
	if r.Session != "" {
		attrs = append(attrs, Attr{"session.id", r.Session}, Attr{"gen_ai.conversation.id", r.Session})
	}
	return attrs
}

// called are the attributes of a provider call, from its record (PAR-8,
// RSN-6, USE-3 to USE-7): usage and cost only when it answered 2xx, cost
// only when known, cache and reasoning parts only when not zero; the time to
// the first chunk of a stream counted from the Unix milliseconds from.
func called(a *Attempt, from int64) []Attr {
	rec := &a.Record
	_, upstream, _ := strings.Cut(rec.Target, "/")
	attrs := []Attr{{"gen_ai.provider.name", a.Provider}}
	if a.Host != "" {
		attrs = append(attrs, Attr{"server.address", a.Host}, Attr{"server.port", a.Port})
	}
	attrs = append(attrs, Attr{"stargaite.upstream.model", upstream}, Attr{"stargaite.billing", rec.Billing}, Attr{"stargaite.cost.source", rec.CostSource})
	if rec.ResponseModel != "" {
		attrs = append(attrs, Attr{"gen_ai.response.model", rec.ResponseModel})
	}
	if rec.StatusCode != 0 { // a provider answered
		attrs = append(attrs, Attr{"stargaite.model_mismatch", rec.ModelMismatch}, Attr{"stargaite.response.empty", rec.ContentEmpty})
	}
	if rec.FinishReason != "" {
		attrs = append(attrs, Attr{"gen_ai.response.finish_reasons", []string{rec.FinishReason}})
	}
	if rec.StatusCode/100 == 2 {
		attrs = append(attrs, Attr{"gen_ai.usage.input_tokens", rec.InputTokens})
		if rec.CacheReadTokens != 0 {
			attrs = append(attrs, Attr{"gen_ai.usage.cache_read.input_tokens", rec.CacheReadTokens})
		}
		if rec.CacheWriteTokens != 0 {
			attrs = append(attrs, Attr{"gen_ai.usage.cache_write.input_tokens", rec.CacheWriteTokens})
		}
		attrs = append(attrs, Attr{"gen_ai.usage.output_tokens", rec.OutputTokens})
		if rec.ReasoningTokens != nil && *rec.ReasoningTokens != 0 {
			attrs = append(attrs, Attr{"gen_ai.usage.reasoning.output_tokens", *rec.ReasoningTokens})
		}
		if rec.TokensSource == "estimated" {
			attrs = append(attrs, Attr{"stargaite.usage.estimated", true})
		}
		if rec.CostUSD != nil {
			attrs = append(attrs, Attr{"stargaite.cost.usd", *rec.CostUSD}, Attr{"gen_ai.usage.cost", *rec.CostUSD})
		}
	}
	for _, e := range []struct {
		name  string
		value *string
	}{{"stargaite.reasoning.effort.requested", rec.EffortRequested}, {"stargaite.reasoning.effort.alias", rec.EffortAlias},
		{"stargaite.reasoning.effort.sent", rec.EffortSent}} {
		if e.value != nil {
			attrs = append(attrs, Attr{e.name, *e.value})
		}
	}
	if a.Level != "" {
		attrs = append(attrs, Attr{"gen_ai.request.reasoning.level", a.Level})
	}
	attrs = append(attrs, Attr{"stargaite.reasoning.effort.source", rec.EffortSource}, Attr{"stargaite.reasoning.effort.adjusted", rec.EffortAdjusted},
		Attr{"stargaite.reasoning.dialect", a.Dialect})
	if a.Stripped {
		attrs = append(attrs, Attr{"stargaite.reasoning.stripped", true})
	}
	for _, c := range []struct {
		name  string
		value *int64
	}{{"stargaite.max_output_tokens.requested", rec.MaxOutputRequested}, {"stargaite.max_output_tokens.alias", rec.MaxOutputAlias},
		{"stargaite.max_output_tokens.sent", rec.MaxOutputSent}, {"gen_ai.request.max_tokens", rec.MaxOutputSent}} {
		if c.value != nil {
			attrs = append(attrs, Attr{c.name, *c.value})
		}
	}
	if rec.ParamsChanged != "" {
		attrs = append(attrs, Attr{"stargaite.params.changed", rec.ParamsChanged})
	}
	if rec.TTFTMS != nil {
		attrs = append(attrs, Attr{"gen_ai.response.time_to_first_chunk", float64(rec.StartedAt+*rec.TTFTMS-from) / 1000})
	}
	return attrs
}
