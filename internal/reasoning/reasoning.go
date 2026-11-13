// Package reasoning resolves the effort of a request (RSN-5), translates it
// into a target's dialect as edits of the outgoing body (RSN-4, kick-off
// section 9.1) and reads the effort sent back from the final body (RSN-6).
// It reads declarations and bodies only, so the data plane and models explain
// get the same answer from the same input (ADR-009).
package reasoning

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/params"
)

// Sources of the effort sent (RSN-6).
const (
	SourceClient       = "client"
	SourceAliasDefault = "alias_default"
	SourceAliasForced  = "alias_forced"
	// SourceProviderDefault is also the source of every body that carries no
	// reasoning control, whatever the cause.
	SourceProviderDefault = domain.EffortProviderDefault
)

// Actions of a change (PAR-3) that this package lists.
const (
	ActionAdded      = "added"
	ActionDropped    = "dropped"
	ActionForced     = "forced"
	ActionTranslated = "translated"
)

// BudgetPrefix starts the requested effort of a client thinking.budget_tokens
// (RSN-1), "budget:<N>", and the effort read back from a thinking_budget body.
const BudgetPrefix = "budget:"

// Choice is the resolved effort of a request toward any target.
type Choice struct {
	Requested, Alias string // the RSN-6 layers; "" when absent
	Level            string // what to send: a canonical level, "budget:<N>", or "" for nothing
	Source           string // client, alias_default, alias_forced, provider_default
}

// Resolve applies the alias's mode to the client's effort (RSN-5). An alias
// effort of provider_default resolves to sending nothing.
func Resolve(client string, m domain.ModelReasoning) Choice {
	c := Choice{Requested: client, Alias: m.Effort}
	alias := m.Effort
	if alias == domain.EffortProviderDefault {
		alias = ""
	}
	switch {
	case m.Mode == domain.ModeForced && m.Effort != "":
		c.Level, c.Source = alias, SourceAliasForced
	case client != "":
		c.Level, c.Source = client, SourceClient
	default:
		c.Level, c.Source = alias, SourceAliasDefault
	}
	if c.Level == "" {
		c.Source = SourceProviderDefault
	}
	return c
}

// Change is one entry of the change list (PAR-3).
type Change struct{ Name, Action string }

// Edit is the reasoning edit of one body toward one target.
type Edit struct {
	Ops      []params.Op
	Changes  []Change // the effort change, then each client member a form replaced, then the fragment's side keys (PAR-13)
	Adjusted bool     // the level sent differs from the level resolved (RSN-4)
}

// Inbound returns the path of the client's effort member on ep (kick-off
// section 9.1): reasoning_effort on chat, output_config.effort on messages.
func Inbound(ep domain.Endpoint) []string {
	if ep == domain.EndpointMessages {
		return []string{"output_config", "effort"}
	}
	return []string{"reasoning_effort"}
}

// Dialect is the dialect r declares; none when undeclared (RSN-2).
func Dialect(r *domain.Reasoning) string {
	if r == nil || r.Dialect == "" {
		return domain.DefaultDialect
	}
	return r.Dialect
}

func onOff(d string) bool {
	return d == domain.DialectThinkingToggle || d == domain.DialectEnableThinking
}

// member is one top-level member a form writes whole.
type member struct {
	name  string
	value []byte
}

// form is a dialect's spelling: the on part and the off form, written whole,
// and the path of the level member, written inside them (kick-off section
// 9.1, design section 6).
type form struct {
	on, off []member
	level   []string
}

var forms = map[string]form{
	domain.DialectReasoningEffort: {off: []member{{"reasoning_effort", []byte(`"none"`)}}, level: []string{"reasoning_effort"}},
	domain.DialectReasoningObject: {off: []member{{"reasoning", []byte(`{"effort":"none"}`)}}, level: []string{"reasoning", "effort"}},
	domain.DialectThinkingToggle: {
		on: []member{{"thinking", []byte(`{"type":"enabled"}`)}}, off: []member{{"thinking", []byte(`{"type":"disabled"}`)}},
	},
	domain.DialectEnableThinking: {on: []member{{"enable_thinking", []byte(`true`)}}, off: []member{{"enable_thinking", []byte(`false`)}}},
	domain.DialectThinkingBudget: {
		on: []member{{"thinking", []byte(`{"type":"enabled"}`)}}, off: []member{{"thinking", []byte(`{"type":"disabled"}`)}},
		level: []string{"thinking", "budget_tokens"},
	},
	domain.DialectAdaptiveEffort: {
		on: []member{{"thinking", []byte(`{"type":"adaptive"}`)}}, off: []member{{"thinking", []byte(`{"type":"disabled"}`)}},
		level: []string{"output_config", "effort"},
	},
}

// fragment returns a declared fragment as members in name order.
func fragment(f map[string]json.RawMessage) []member {
	var ms []member
	for _, k := range slices.Sorted(maps.Keys(f)) {
		ms = append(ms, member{k, f[k]})
	}
	return ms
}

// translate maps the resolved level L to the level sent (RSN-4): "none" is
// the off form, and an on/off dialect sends "on" for anything else.
func translate(l, d string, r *domain.Reasoning) string {
	if m, ok := r.Map[l]; ok {
		if onOff(d) && m != "none" {
			return domain.LevelOn
		}
		return m
	}
	cannotDisable := r.CanDisable != nil && !*r.CanDisable
	switch {
	case l == "none" && cannotDisable && onOff(d):
		return domain.LevelOn
	case l == "none" && cannotDisable && len(r.Levels) > 0:
		return r.Levels[0]
	case l == "none":
		return l
	case onOff(d):
		return domain.LevelOn
	case len(r.Levels) == 0 || slices.Contains(r.Levels, l):
		return l
	}
	at, best := slices.Index(domain.Levels, l), ""
	for _, c := range r.Levels {
		i := slices.Index(domain.Levels, c)
		if best == "" || abs(i-at) < abs(slices.Index(domain.Levels, best)-at) {
			best = c // levels are in scale order, so a tie keeps the lower one
		}
	}
	return best
}

func abs(i int) int { return max(i, -i) }

// Translate writes the resolved choice c into body b of endpoint ep toward a
// target that declares r (design section 6). The client's effort member is
// deleted unless the form writes it; the on part or the off form, or the
// fragment in its place, is written whole; then the level member. A member
// the client sent byte-identical is neither rewritten nor listed.
func Translate(c Choice, b *params.Body, ep domain.Endpoint, r *domain.Reasoning) Edit {
	var e Edit
	in := Inbound(ep)
	inName := strings.Join(in, ".")
	_, clientSent := b.Get(in...)
	d := Dialect(r)
	if strings.HasPrefix(c.Level, BudgetPrefix) {
		return e // forwarded unchanged toward thinking_budget; Build skips other dialects (RSN-1)
	}
	if c.Level == "" || d == domain.DialectNone {
		if clientSent {
			e.Ops = deleteEffort(b, ep)
		}
		if clientSent || c.Level != "" {
			e.Changes = []Change{{inName, ActionDropped}}
		}
		// A forced provider_default replaces any client control (RSN-1), so on
		// messages the client's thinking goes too.
		if _, ok := b.Get("thinking"); ok && c.Level == "" && c.Requested != "" && ep == domain.EndpointMessages {
			e.Ops = append(e.Ops, params.Op{Path: []string{"thinking"}})
			e.Changes = append(e.Changes, Change{"thinking", ActionDropped})
		}
		return e
	}

	f := forms[d]
	sent := translate(c.Level, d, r)
	off := sent == "none"
	if onOff(d) {
		e.Adjusted = off != (c.Level == "none")
	} else {
		e.Adjusted = sent != c.Level
	}
	write := f.on
	if off {
		write = f.off
		if r.Off != nil {
			write = fragment(r.Off)
		}
	} else if r.On != nil {
		write = fragment(r.On)
	}
	var level []byte
	if !off && f.level != nil {
		var err error
		if level, err = jsontext.AppendQuote(nil, sent); err != nil {
			level = []byte(sent) // invalid UTF-8 is not JSON: Edit refuses it, Build skips the target
		}
		if d == domain.DialectThinkingBudget {
			level = strconv.AppendInt(nil, r.Budgets[sent], 10)
		}
	}

	// The form writes the client's member when it is the level member or a
	// whole top-level member the form writes; otherwise it is deleted first.
	writesInbound := level != nil && slices.Equal(f.level, in) ||
		slices.ContainsFunc(write, func(m member) bool { return m.name == in[0] })
	changed, deleted := false, false
	if clientSent && !writesInbound {
		e.Ops, deleted = deleteEffort(b, ep), true
	}

	// RSN-1 on messages: the client's thinking stays with a client effort
	// above none, and, without a client effort, a thinking of type adaptive or
	// between_tools stays toward adaptive_effort.
	// Kept, it is not touched at all, a level member inside it included.
	kind := str(b, "thinking", "type")
	_, thinking := b.Get("thinking")
	// The level resolved, not the one sent: none raised to a level toward a
	// target that cannot disable reasoning keeps nothing.
	keep := thinking && ep == domain.EndpointMessages && !off && c.Level != "none" && (c.Source == SourceClient ||
		c.Requested == "" && d == domain.DialectAdaptiveEffort && (kind == "adaptive" || kind == "between_tools"))
	action := ActionForced
	if c.Source == SourceClient {
		action = ActionTranslated
	}
	var replaced, side []Change
	rewrote := false // the level member's top-level object was written whole
	for _, m := range write {
		old, ok := b.Get(m.name)
		if ok && (bytes.Equal(old, m.value) || keep && m.name == "thinking") {
			continue
		}
		e.Ops = append(e.Ops, params.Op{Path: []string{m.name}, Value: m.value})
		rewrote = rewrote || level != nil && m.name == f.level[0]
		switch {
		case !slices.Contains(domain.DialectKeys[d], m.name):
			if ok {
				side = append(side, Change{m.name, ActionForced})
			} else {
				side = append(side, Change{m.name, ActionAdded})
			}
		case ok && m.name != inName:
			replaced = append(replaced, Change{m.name, action})
			changed = true
		default:
			changed = true
		}
	}
	if level != nil && (!keep || f.level[0] != "thinking") {
		if old, ok := b.Get(f.level...); rewrote || !ok || !bytes.Equal(old, level) {
			e.Ops = append(e.Ops, params.Op{Path: f.level, Value: level})
			changed = true
		}
	}
	if changed || deleted {
		switch {
		case c.Source == SourceAliasForced && c.Requested != "":
			e.Changes = append(e.Changes, Change{inName, ActionForced})
		case c.Source == SourceClient && keep && !changed:
			e.Changes = append(e.Changes, Change{inName, ActionDropped}) // deleted, and nothing the proxy wrote carries it
		case c.Source == SourceClient:
			e.Changes = append(e.Changes, Change{inName, ActionTranslated})
		case !slices.ContainsFunc(replaced, func(r Change) bool { return r.Name == changeName(d) }):
			e.Changes = append(e.Changes, Change{changeName(d), ActionAdded}) // unless the replaced member names it
		}
	}
	e.Changes = append(append(e.Changes, replaced...), side...)
	return e
}

// changeName is the PAR-3 name of an effort a dialect adds (design section 6).
func changeName(d string) string {
	switch d {
	case domain.DialectReasoningObject:
		return "reasoning.effort"
	case domain.DialectThinkingBudget:
		return "thinking.budget_tokens"
	case domain.DialectAdaptiveEffort:
		return "output_config.effort"
	}
	return domain.DialectKeys[d][0]
}

// deleteEffort deletes the client's effort member, and on messages an
// output_config it leaves empty.
func deleteEffort(b *params.Body, ep domain.Endpoint) []params.Op {
	in := Inbound(ep)
	if ep == domain.EndpointMessages {
		raw, _ := b.Get("output_config")
		if oc, e := params.Scan(raw, nil); e == nil && slices.Equal(oc.Keys(), []string{"effort"}) {
			return []params.Op{{Path: in[:1]}}
		}
	}
	return []params.Op{{Path: in}}
}

// str is the string at path, unescaped; "" when absent or not a string.
func str(b *params.Body, path ...string) string {
	raw, ok := b.Get(path...)
	if !ok || len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	s, err := jsontext.AppendUnquote(nil, raw)
	if err != nil {
		return ""
	}
	return string(s)
}

// present reports whether every member of a non-empty fragment is in out
// byte for byte.
func present(out *params.Body, f map[string]json.RawMessage) bool {
	for k, v := range f {
		if got, ok := out.Get(k); !ok || !bytes.Equal(got, v) {
			return false
		}
	}
	return len(f) > 0
}

// Sent reads the effort sent from out, the final outgoing body scanned with
// an Out walk (RSN-6, PAR-8), in the terms of the dialect r declares: the
// level; "on" for an on/off dialect; "budget:<N>" for thinking_budget; "none"
// when the body turns reasoning off; "" when it carries no reasoning control.
func Sent(out *params.Body, r *domain.Reasoning) string {
	d := Dialect(r)
	if d == domain.DialectNone {
		return ""
	}
	if present(out, r.Off) {
		return "none"
	}
	thinking := str(out, "thinking", "type")
	switch d {
	case domain.DialectReasoningEffort:
		return str(out, "reasoning_effort")
	case domain.DialectReasoningObject:
		return str(out, "reasoning", "effort")
	case domain.DialectThinkingBudget:
		if thinking == "disabled" {
			return "none"
		}
		raw, _ := out.Get("thinking", "budget_tokens")
		if n, err := strconv.ParseInt(string(raw), 10, 64); err == nil {
			return BudgetPrefix + strconv.FormatInt(n, 10)
		}
		return ""
	case domain.DialectAdaptiveEffort:
		if v := str(out, "output_config", "effort"); v != "" {
			return v
		}
		if thinking == "disabled" {
			return "none"
		}
		return ""
	}
	// thinking_toggle and enable_thinking
	switch raw, _ := out.Get("enable_thinking"); {
	case d == domain.DialectThinkingToggle && thinking == "enabled", d == domain.DialectEnableThinking && string(raw) == "true":
		return domain.LevelOn
	case d == domain.DialectThinkingToggle && thinking == "disabled", d == domain.DialectEnableThinking && string(raw) == "false":
		return "none"
	case present(out, r.On):
		return domain.LevelOn
	}
	return ""
}
