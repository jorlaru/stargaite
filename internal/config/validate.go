package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/transport"
)

// OutboundURL is one URL the proxy will connect to, with the flag that may
// open private addresses to it (SEC-4).
type OutboundURL struct {
	Resource, Field, URL string
	AllowPrivate         bool
}

// report collects findings in the order the checks run.
type report []domain.Finding

func (r *report) add(severity, rule, code, resource, field, format string, args ...any) {
	*r = append(*r, domain.Finding{
		Severity: severity, Rule: rule, Code: code, Resource: resource, Field: field,
		Message: fmt.Sprintf(format, args...),
	})
}

func (r *report) fail(rule, code, resource, field, format string, args ...any) {
	r.add(domain.SeverityError, rule, code, resource, field, format, args...)
}

func (r *report) warn(rule, code, resource, field, format string, args ...any) {
	r.add(domain.SeverityWarning, rule, code, resource, field, format, args...)
}

// oneOf refuses a value outside allowed; an empty value is refused only when
// the field is required. Values are quoted with a bound, since a file can put
// any text there.
func (r *report) oneOf(rule, resource, field, value string, required bool, allowed ...string) {
	switch {
	case value == "" && required:
		r.fail(rule, "required", resource, field, "%s is required: one of %s", field, strings.Join(allowed, ", "))
	case value != "" && !slices.Contains(allowed, value):
		r.fail(rule, "invalid_value", resource, field, "%s %.64q is not one of %s", field, value, strings.Join(allowed, ", "))
	}
}

func (r *report) nonNegative(rule, resource, field string, v int64) {
	if v < 0 {
		r.fail(rule, "invalid_value", resource, field, "%s must not be negative", field)
	}
}

func (r *report) budget(resource, field string, b *domain.Budget) {
	if b == nil {
		return
	}
	switch {
	case b.LimitUSD == nil:
		r.fail("LIM-2", "required", resource, field+".limit_usd", "%s.limit_usd is required", field)
	case *b.LimitUSD < 0:
		r.fail("LIM-2", "invalid_value", resource, field+".limit_usd", "%s.limit_usd must not be negative", field)
	}
	r.oneOf("LIM-2", resource, field+".reset_interval", b.ResetInterval, true, domain.ResetDaily, domain.ResetMonthly, domain.ResetNever)
}

// scope checks what an alias and a key share: budget, limits and sampling.
func (r *report) scope(resource string, b *domain.Budget, rpm, tpm, parallel int64, rate *float64) {
	r.budget(resource, "budget", b)
	r.nonNegative("LIM-5", resource, "rpm_limit", rpm)
	r.nonNegative("LIM-5", resource, "tpm_limit", tpm)
	r.nonNegative("LIM-5", resource, "max_parallel_requests", parallel)
	if rate != nil && (*rate < 0 || *rate > 1) {
		r.fail("TEL-7", "invalid_value", resource, "trace_sample_rate", "trace_sample_rate must be between 0 and 1")
	}
}

var (
	// headerName is the token syntax of an HTTP field name (RFC 9110).
	headerName  = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	quotaWindow = regexp.MustCompile(`^[1-9][0-9]{0,5}[mhd]$`)
	// reservedHeaders carry credentials, which are only ever references (TEL-2).
	reservedHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie", "x-api-key"}
	capabilities    = []string{domain.CapabilityTools, domain.CapabilityStructuredOutput, domain.CapabilityStrictSchema, domain.CapabilityVision}
)

// validReference reports whether name may be a credential reference (SEC-5):
// a variable name outside the STARGAITE_ prefix, in any letter case.
func validReference(name string) bool {
	const prefix = "STARGAITE_"
	return envName.MatchString(name) && (len(name) < len(prefix) || !strings.EqualFold(name[:len(prefix)], prefix))
}

type reference struct{ resource, field, name string }

// references lists every credential reference of doc, in a fixed order.
func references(doc *domain.Document) []reference {
	var refs []reference
	for _, name := range slices.Sorted(maps.Keys(doc.Providers)) {
		if p := doc.Providers[name]; p.APIKeyEnv != "" {
			refs = append(refs, reference{"providers/" + name, "api_key_env", p.APIKeyEnv})
		}
	}
	if t := doc.Settings.Telemetry; t != nil {
		for _, v := range t.BasicAuthEnv {
			refs = append(refs, reference{"settings", "telemetry.basic_auth_env", v})
		}
		for _, h := range slices.Sorted(maps.Keys(t.HeaderEnv)) {
			refs = append(refs, reference{"settings", "telemetry.header_env." + h, t.HeaderEnv[h]})
		}
	}
	return refs
}

// OutboundURLs lists every URL of doc the proxy connects to, in a fixed
// order: validate passes them all to CheckResolved, a change only those it sets.
func OutboundURLs(doc *domain.Document) []OutboundURL {
	var urls []OutboundURL
	for _, name := range slices.Sorted(maps.Keys(doc.Providers)) {
		p := doc.Providers[name]
		if p.BaseURL != "" {
			urls = append(urls, OutboundURL{"providers/" + name, "base_url", p.BaseURL, p.AllowPrivateNetwork})
		}
		if p.RerankURL != "" {
			urls = append(urls, OutboundURL{"providers/" + name, "rerank_url", p.RerankURL, p.AllowPrivateNetwork})
		}
	}
	if t := doc.Settings.Telemetry; t != nil && t.OTLPEndpoint != "" {
		urls = append(urls, OutboundURL{"settings", "telemetry.otlp_endpoint", t.OTLPEndpoint, t.AllowPrivateNetwork})
	}
	return urls
}

// dialect is the reasoning dialect a target sends in; undeclared is none (RSN-2).
func dialect(t domain.Target) string {
	if t.Reasoning == nil {
		return domain.DefaultDialect
	}
	return cmp.Or(t.Reasoning.Dialect, domain.DefaultDialect)
}

// written lists the top-level body keys the proxy itself can write toward a
// target, which no forward rule may name (PAR-2). The fields of the protocol
// adapter are not listed yet (design section 13, point 6).
func written(t domain.Target, protocol string) []string {
	keys := slices.Clone(domain.DialectKeys[dialect(t)])
	if t.Reasoning != nil {
		keys = slices.AppendSeq(keys, maps.Keys(t.Reasoning.On))
		keys = slices.AppendSeq(keys, maps.Keys(t.Reasoning.Off))
	}
	keys = append(keys, cmp.Or(t.OutputCapParam, domain.DefaultOutputCapParam), "stream_options")
	if protocol == domain.ProtocolAnthropic {
		keys = append(keys, "max_tokens") // every cap toward anthropic is max_tokens (PAR-6)
	}
	return keys
}

func capability(c domain.Capabilities, name string) *bool {
	switch name {
	case domain.CapabilityTools:
		return c.Tools
	case domain.CapabilityStructuredOutput:
		return c.StructuredOutput
	case domain.CapabilityStrictSchema:
		return c.StrictSchema
	case domain.CapabilityVision:
		return c.Vision
	}
	return nil
}

// Validate runs every check that reads only the declarations (OPS-5). It is
// pure: no clock, I/O, name lookup or environment, so a stored configuration
// gives the same findings on every start. Findings come in a fixed order and
// never echo a credential reference, which may be a value pasted by mistake.
func Validate(doc *domain.Document) []domain.Finding {
	var r report
	if doc.Version != 1 {
		r.fail("OPS-3", "unsupported_version", "document", "version", "version must be 1")
	}
	validateSettings(&r, doc.Settings)
	for _, name := range slices.Sorted(maps.Keys(doc.Providers)) {
		validateProvider(&r, name, doc.Providers[name])
	}
	scopes := budgetScopes(doc)
	for _, id := range slices.Sorted(maps.Keys(doc.Targets)) {
		validateTarget(&r, doc, id, scopes)
	}
	for _, name := range slices.Sorted(maps.Keys(doc.Models)) {
		validateModel(&r, doc, name)
	}
	for _, name := range slices.Sorted(maps.Keys(doc.Keys)) {
		validateKey(&r, doc, name)
	}
	for _, u := range OutboundURLs(doc) {
		if err := transport.CheckURL(u.URL, u.AllowPrivate); err != nil {
			r.fail("SEC-4", "url_refused", u.Resource, u.Field, "%s: %v", u.Field, err)
		}
	}
	for _, ref := range references(doc) {
		if !validReference(ref.name) {
			r.fail("SEC-5", "invalid_credential_reference", ref.resource, ref.field,
				"%s must name an environment variable (letters, digits and _, not starting with a digit) that does not start with STARGAITE_ in any letter case", ref.field)
		}
	}
	return r
}

func validateSettings(r *report, s domain.Settings) {
	const res = "settings"
	r.budget(res, "budget", s.Budget)
	r.nonNegative("LIM-5", res, "max_parallel_requests", s.MaxParallelRequests)
	r.nonNegative("LIM-6", res, "max_body_bytes", s.MaxBodyBytes)
	r.nonNegative("RES-4", res, "max_response_bytes", s.MaxResponseBytes)
	r.nonNegative("RES-3", res, "timeouts.connect_ms", s.Timeouts.ConnectMS)
	r.nonNegative("RES-3", res, "timeouts.first_byte_ms", s.Timeouts.FirstByteMS)
	r.nonNegative("RES-3", res, "timeouts.stream_idle_ms", s.Timeouts.StreamIdleMS)
	r.nonNegative("RES-3", res, "timeouts.total_ms", s.Timeouts.TotalMS)
	r.nonNegative("USE-11", res, "retention.usage_days", s.Retention.UsageDays)
	r.nonNegative("USE-11", res, "retention.usage_daily_months", s.Retention.UsageDailyMonths)
	r.nonNegative("USE-11", res, "retention.audit_months", s.Retention.AuditMonths)
	t := s.Telemetry
	if t == nil {
		return
	}
	r.nonNegative("TEL-8", res, "telemetry.content_max_bytes", t.ContentMaxBytes)
	if t.BasicAuthEnv != nil && len(t.BasicAuthEnv) != 2 {
		r.fail("TEL-2", "invalid_value", res, "telemetry.basic_auth_env", "telemetry.basic_auth_env must name two variables: the user and the password")
	}
	for _, h := range slices.Sorted(maps.Keys(t.HeaderEnv)) {
		if !headerName.MatchString(h) {
			r.fail("TEL-2", "invalid_value", res, "telemetry.header_env", "telemetry.header_env %.64q is not a valid header name", h)
		}
	}
	for _, h := range slices.Sorted(maps.Keys(t.Headers)) {
		field := "telemetry.headers." + h
		switch {
		case !headerName.MatchString(h):
			r.fail("TEL-2", "invalid_value", res, "telemetry.headers", "telemetry.headers %.64q is not a valid header name", h)
		case slices.ContainsFunc(reservedHeaders, func(n string) bool { return strings.EqualFold(n, h) }):
			r.fail("TEL-2", "reserved_header", res, field, "a static header may not be named %s: it carries a credential, which is given with header_env", h)
		case strings.ContainsAny(t.Headers[h], "\r\n\x00"):
			r.fail("TEL-2", "invalid_value", res, field, "the value of header %s holds a line break or a NUL byte", h)
		}
	}
}

func validateProvider(r *report, name string, p domain.Provider) {
	res := "providers/" + name
	if !domain.ValidName(name) {
		r.fail("OPS-5", "invalid_name", res, "", "provider name %.64q must match [A-Za-z0-9][A-Za-z0-9._-]* with at most 64 characters", name)
	}
	r.oneOf("OPS-5", res, "protocol", p.Protocol, true, domain.ProtocolOpenAI, domain.ProtocolAnthropic)
	if p.BaseURL == "" {
		r.fail("SEC-4", "required", res, "base_url", "base_url is required")
	}
	r.oneOf("OPS-5", res, "billing", p.Billing, true, domain.BillingPayPerUse, domain.BillingSubscription, domain.BillingNone)
	if len(p.Quota) > 0 && p.Billing != domain.BillingSubscription {
		r.fail("LIM-8", "invalid_value", res, "quota", "quota applies only to billing subscription")
	}
	for _, q := range p.Quota {
		if !quotaWindow.MatchString(q.Window) {
			r.fail("LIM-8", "invalid_value", res, "quota", "quota window %.64q must be a whole number of minutes, hours or days, such as 5h or 7d", q.Window)
		}
		if q.Requests < 0 || q.Tokens < 0 || q.Requests == 0 && q.Tokens == 0 {
			r.fail("LIM-8", "invalid_value", res, "quota", "quota window %.64q needs a positive limit in requests or tokens", q.Window)
		}
	}
	if c := p.CostReport; c != nil {
		if c.Path == "" || slices.Contains(strings.Split(c.Path, "."), "") {
			r.fail("USE-6", "invalid_value", res, "cost_report.path", "cost_report.path must be a dotted path such as usage.cost")
		}
		if c.USDPerUnit <= 0 {
			r.fail("USE-6", "invalid_value", res, "cost_report.usd_per_unit", "cost_report.usd_per_unit must be positive")
		}
	}
	r.oneOf("USE-8", res, "stream_usage", p.StreamUsage, false, domain.StreamUsageRequest, domain.StreamUsageNative, domain.StreamUsageNone)
	r.oneOf("RSN-2", res, "target_defaults.dialect", p.TargetDefaults.Dialect, false, slices.Sorted(maps.Keys(domain.DialectKeys))...)
}

func validateTarget(r *report, doc *domain.Document, id string, scopes map[string]string) {
	t, res := doc.Targets[id], "targets/"+id
	provider, _, ok := domain.SplitTarget(id)
	if !ok {
		r.fail("OPS-5", "invalid_name", res, "", "a target id is provider/upstream-model: a valid provider name, then an upstream id without spaces or control characters")
	} else if _, ok := doc.Providers[provider]; !ok {
		r.fail("OPS-5", "unknown_provider", res, "", "provider %s does not exist", provider)
	}
	r.oneOf("OPS-5", res, "kind", t.Kind, true, domain.KindChat, domain.KindEmbeddings, domain.KindRerank)
	r.nonNegative("PAR-6", res, "max_output_tokens", t.MaxOutputTokens)

	if rs := t.Reasoning; rs != nil {
		validateReasoning(r, res, rs)
		if t.Kind == domain.KindChat && dialect(t) != domain.DialectNone && (len(rs.Levels) == 0 || rs.CanDisable == nil) {
			r.warn("RSN-3", "reasoning_undeclared", res, "reasoning",
				"levels or can_disable is undeclared: a level is sent as is and none in the off form")
		}
	}

	if p := t.Pricing; p != nil {
		if p.InputPerMillion == nil || p.OutputPerMillion == nil {
			r.fail("USE-6", "required", res, "pricing", "pricing needs input_per_million and output_per_million")
		}
		for _, price := range []*float64{p.InputPerMillion, p.OutputPerMillion, p.CacheReadPerMillion, p.CacheWritePerMillion} {
			if price != nil && *price < 0 {
				r.fail("USE-6", "invalid_value", res, "pricing", "a price must not be negative")
				break
			}
		}
		if _, err := time.Parse(time.DateOnly, p.AsOf); err != nil {
			r.fail("USE-6", "invalid_value", res, "pricing.as_of", "pricing.as_of must be the date of the prices, such as 2026-11-01")
		}
	}

	// USE-12: a pay-per-use target that nothing can price.
	p, ok := doc.Providers[provider]
	if !ok || p.Billing != domain.BillingPayPerUse || t.Pricing != nil {
		return
	}
	switch {
	case p.CostReport != nil:
		r.warn("USE-12", "no_fallback_prices", res, "pricing",
			"priced only by the cost report of provider %s: a response whose report never arrives has no cost", provider)
	case scopes[id] != "":
		r.fail("USE-12", "unpriced_under_budget", res, "pricing",
			"pay-per-use target with neither prices nor a provider cost report, reachable under the budget of %s, which cannot see its cost", scopes[id])
	default:
		r.warn("USE-12", "unpriced", res, "pricing", "pay-per-use target with neither prices nor a provider cost report: its usage has no cost")
	}
}

// budgetScopes maps every target a budget reaches to the first scope with a
// budget that reaches it: aliases by name, and for each the instance, the
// alias, then its keys by name. It takes one pass over the aliases and one
// over the keys, so that Validate stays linear at the sizes of kick-off
// section 18.
func budgetScopes(doc *domain.Document) map[string]string {
	var everyAlias string          // the first key with a budget that lists "*"
	byAlias := map[string]string{} // alias -> the first key with a budget that lists it
	for _, name := range slices.Sorted(maps.Keys(doc.Keys)) {
		k := doc.Keys[name]
		if k.Budget == nil {
			continue
		}
		for _, alias := range k.Models {
			switch _, seen := byAlias[alias]; {
			case alias == "*" && everyAlias == "":
				everyAlias = name
			case alias != "*" && !seen:
				byAlias[alias] = name
			}
		}
	}
	scopes := map[string]string{}
	for _, alias := range slices.Sorted(maps.Keys(doc.Models)) {
		m := doc.Models[alias]
		var scope string
		switch key := byAlias[alias]; {
		case doc.Settings.Budget != nil:
			scope = "the instance"
		case m.Budget != nil:
			scope = "alias " + alias
		case everyAlias != "" && (key == "" || everyAlias < key):
			scope = "key " + everyAlias
		case key != "":
			scope = "key " + key
		default:
			continue
		}
		for _, id := range m.Targets {
			if _, ok := scopes[id]; !ok {
				scopes[id] = scope
			}
		}
	}
	return scopes
}

func validateReasoning(r *report, res string, rs *domain.Reasoning) {
	r.oneOf("RSN-3", res, "reasoning.dialect", rs.Dialect, false, slices.Sorted(maps.Keys(domain.DialectKeys))...)
	scale := domain.Levels[1:] // none is the off form, not a level a provider accepts
	if rs.Dialect == domain.DialectThinkingToggle || rs.Dialect == domain.DialectEnableThinking {
		scale = []string{domain.LevelOn}
	}
	last, levelsOK := -1, true
	for _, l := range rs.Levels {
		i := slices.Index(scale, l)
		if i <= last {
			r.fail("RSN-3", "invalid_value", res, "reasoning.levels",
				"reasoning.levels must be taken from %s, in that order, without repeats; %.64q is not", strings.Join(scale, ", "), l)
			levelsOK = false
			break
		}
		last = i
	}
	if rs.CanDisable != nil && !*rs.CanDisable && len(rs.Levels) == 0 {
		r.fail("RSN-3", "levels_required", res, "reasoning.levels", "a target that cannot disable reasoning must declare its levels")
	}
	if rs.Dialect == domain.DialectThinkingBudget {
		if len(rs.Levels) == 0 {
			r.fail("RSN-3", "levels_required", res, "reasoning.levels", "a thinking_budget target must declare its levels")
		}
		for _, l := range rs.Levels {
			if levelsOK && rs.Budgets[l] <= 0 {
				r.fail("RSN-3", "budgets_required", res, "reasoning.budgets", "level %.64q needs a positive entry in reasoning.budgets", l)
			}
		}
	}
	// The budgets are checked against valid levels only: a valid list holds at
	// most seven entries, so a long invalid one costs no more than one finding.
	for _, l := range slices.Sorted(maps.Keys(rs.Budgets)) {
		if levelsOK && (rs.Dialect != domain.DialectThinkingBudget || !slices.Contains(rs.Levels, l)) {
			r.fail("RSN-3", "invalid_value", res, "reasoning.budgets", "reasoning.budgets %.64q is not a declared level of a thinking_budget target", l)
		}
	}
	for _, l := range slices.Sorted(maps.Keys(rs.Map)) {
		if !slices.Contains(domain.Levels, l) || rs.Map[l] == "" {
			r.fail("RSN-3", "invalid_value", res, "reasoning.map", "reasoning.map %.64q must map a level of the canonical scale to a non-empty level", l)
		}
	}
}

func validateModel(r *report, doc *domain.Document, name string) {
	m, res := doc.Models[name], "models/"+name
	if !domain.ValidName(name) {
		r.fail("OPS-5", "invalid_name", res, "", "alias name %.64q must match [A-Za-z0-9][A-Za-z0-9._-]* with at most 64 characters", name)
	}
	switch n := len(m.Targets); {
	case n == 0:
		r.fail("OPS-5", "no_targets", res, "targets", "an alias needs one to three targets")
	case n > 3:
		r.fail("OPS-5", "too_many_targets", res, "targets", "an alias has at most three targets, not %d", n)
	}
	for _, c := range m.Requires {
		r.oneOf("PAR-5", res, "requires", c, true, capabilities...)
	}
	r.oneOf("RSN-5", res, "reasoning.effort", m.Reasoning.Effort, false, append(slices.Clone(domain.Levels), domain.EffortProviderDefault)...)
	r.oneOf("RSN-5", res, "reasoning.mode", m.Reasoning.Mode, m.Reasoning.Effort != "", domain.ModeDefault, domain.ModeForced)
	if m.Reasoning.Mode != "" && m.Reasoning.Effort == "" {
		r.fail("RSN-5", "required", res, "reasoning.effort", "reasoning.mode needs a reasoning.effort")
	}
	r.nonNegative("PAR-6", res, "max_output_tokens", m.MaxOutputTokens)
	for _, p := range slices.Sorted(maps.Keys(m.Params)) {
		r.oneOf("PAR-2", res, "params."+p, m.Params[p], true, domain.ParamForward, domain.ParamDrop)
	}
	r.scope(res, m.Budget, m.RPMLimit, m.TPMLimit, m.MaxParallelRequests, m.TraceSampleRate)
	if len(m.Targets) > 3 {
		return // refused as too_many_targets: a long list costs no more than that one finding
	}

	for i, id := range m.Targets {
		t, ok := doc.Targets[id]
		switch {
		case slices.Index(m.Targets, id) < i:
			r.fail("OPS-5", "duplicate_target", res, "targets", "target %.64q is listed twice", id)
			continue
		case !ok:
			r.fail("OPS-5", "unknown_target", res, "targets", "target %.64q does not exist", id)
			continue
		}
		provider, _, _ := domain.SplitTarget(id)
		protocol := doc.Providers[provider].Protocol
		for _, c := range m.Requires {
			if v := capability(t.Capabilities, c); slices.Contains(capabilities, c) && (v == nil || !*v) {
				r.fail("PAR-5", "requirement_not_met", res, "requires", "target %s does not declare %s true", id, c)
			}
		}
		if t.MaxOutputTokens > 0 && m.MaxOutputTokens > t.MaxOutputTokens {
			r.fail("PAR-6", "output_cap_exceeds_target", res, "max_output_tokens",
				"max_output_tokens %d is above the %d that target %s declares", m.MaxOutputTokens, t.MaxOutputTokens, id)
		}
		if protocol == domain.ProtocolAnthropic && m.MaxOutputTokens == 0 {
			r.fail("PAR-7", "output_cap_required", res, "max_output_tokens", "target %s speaks the anthropic protocol, which requires an output cap: set max_output_tokens", id)
		}
		keys := written(t, protocol)
		for _, p := range slices.Sorted(maps.Keys(m.Params)) {
			if m.Params[p] == domain.ParamForward && slices.Contains(keys, p) {
				r.fail("PAR-2", "param_collision", res, "params."+p,
					"a forward rule may not name %.64q: the proxy writes it toward target %s", p, id)
			}
		}
		if t.Reasoning != nil && t.Reasoning.CanDisable != nil && !*t.Reasoning.CanDisable && m.Reasoning.Effort == "" {
			r.fail("RSN-7", "effort_required", res, "reasoning.effort", "target %s always reasons and the alias sets no effort", id)
		}
		if e := m.Reasoning.Effort; e != "" && e != domain.EffortProviderDefault && dialect(t) == domain.DialectNone {
			r.warn("RSN-5", "effort_dropped", res, "reasoning.effort", "target %s has the reasoning dialect none: the effort is dropped toward it", id)
		}
	}
}

func validateKey(r *report, doc *domain.Document, name string) {
	k, res := doc.Keys[name], "keys/"+name
	switch {
	case strings.HasPrefix(name, "_"):
		r.fail("RSN-9", "reserved_name", res, "", "a key name may not start with _: such names are reserved for the proxy's own calls")
	case !domain.ValidName(name):
		r.fail("OPS-5", "invalid_name", res, "", "key name %.64q must match [A-Za-z0-9][A-Za-z0-9._-]* with at most 64 characters", name)
	}
	switch {
	case k.Models == nil:
		r.fail("OPS-5", "required", res, "models", `models is required: a list of aliases, ["*"] for all, or [] for none`)
	case slices.Contains(k.Models, "*") && len(k.Models) > 1:
		r.fail("OPS-5", "invalid_value", res, "models", `"*" stands for every alias and is listed alone`)
	}
	for i, alias := range k.Models {
		// key_models has one row per alias, so a repeat could not be stored.
		switch _, ok := doc.Models[alias]; {
		case slices.Index(k.Models, alias) < i:
			r.fail("OPS-5", "duplicate_alias", res, "models", "alias %.64q is listed twice", alias)
		case !ok && alias != "*":
			r.warn("OPS-5", "unknown_alias", res, "models", "alias %.64q does not exist", alias)
		}
	}
	r.scope(res, k.Budget, k.RPMLimit, k.TPMLimit, k.MaxParallelRequests, k.TraceSampleRate)
	for _, p := range slices.Sorted(maps.Keys(k.QuotaShare)) {
		share := k.QuotaShare[p]
		provider, ok := doc.Providers[p]
		switch {
		case share <= 0 || share > 1:
			r.fail("LIM-10", "invalid_value", res, "quota_share."+p, "a quota share is a fraction above 0 and at most 1")
		case !ok:
			r.warn("LIM-10", "unknown_provider", res, "quota_share."+p, "provider %.64q does not exist", p)
		case provider.Billing != domain.BillingSubscription:
			r.fail("LIM-10", "invalid_value", res, "quota_share."+p, "provider %s is not a subscription, so it has no quota to share", p)
		}
	}
}

// CheckResolved runs the two checks that need the environment (OPS-5,
// OPS-6, SEC-4): every credential reference of doc through env.Lookup, and
// the host of every URL in urls through env.Resolve. It returns
// env.Findings first. A reference or a URL that Validate refuses is skipped
// here, so its text is never echoed and never looked up.
func CheckResolved(ctx context.Context, doc *domain.Document, urls []OutboundURL, env Env) []domain.Finding {
	r := report(slices.Clone(env.Findings))
	for _, ref := range references(doc) {
		if !validReference(ref.name) {
			continue
		}
		if v, ok := env.Lookup(ref.name); !ok || v == "" {
			r.warn("OPS-6", "credential_unresolved", ref.resource, ref.field, "%s does not resolve", ref.name)
		}
	}
	for _, u := range urls {
		if transport.CheckURL(u.URL, u.AllowPrivate) != nil {
			continue
		}
		parsed, err := url.Parse(u.URL)
		if err != nil {
			continue // CheckURL parsed it already
		}
		host := parsed.Hostname()
		if _, err := netip.ParseAddr(host); err == nil {
			continue // a literal address, which CheckURL classified
		}
		var addrs []netip.Addr
		err = errors.New("no resolver")
		if env.Resolve != nil {
			addrs, err = env.Resolve(ctx, host)
		}
		if err == nil && len(addrs) == 0 {
			err = errors.New("no address")
		}
		worst := transport.Public
		for _, a := range addrs {
			worst = max(worst, transport.Classify(a))
		}
		switch {
		case err != nil:
			r.warn("SEC-4", "url_unresolved", u.Resource, u.Field,
				"%s: the host name did not resolve (%v); it is checked again at connection time", u.Field, err)
		case worst == transport.Forbidden:
			r.fail("SEC-4", "url_refused", u.Resource, u.Field,
				"%s: the host name resolves to a link-local, cloud metadata or reserved address, which is never allowed", u.Field)
		case worst == transport.Private && !u.AllowPrivate:
			r.fail("SEC-4", "url_refused", u.Resource, u.Field,
				"%s: the host name resolves to a private address: set allow_private_network to allow it", u.Field)
		}
	}
	return r
}
