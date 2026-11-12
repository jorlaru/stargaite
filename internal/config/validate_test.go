package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"net"
	"net/http/httptest"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/transport"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// Target ids of annex B, the document every row starts from.
const (
	mm = "minimax-plan/MiniMax-M3.1-Flash" // can_disable false, anthropic protocol, subscription
	qw = "openrouter/qwen/qwen-3.6-flash"  // reasoning_object, can_disable true, priced, max_output_tokens 32000
	sn = "anthropic/claude-sonnet-5-5"     // adaptive_effort, can_disable false, priced, vision
)

// pastedKey stands for a credential an operator pasted where a reference goes.
const pastedKey = "sk-" + planted + "-0123456789"

func edit[V any](m map[string]V, name string, f func(*V)) {
	v := m[name]
	f(&v)
	m[name] = v
}

// summary renders a finding as "severity rule code resource field".
func summary(fs []domain.Finding) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, strings.Join([]string{f.Severity, f.Rule, f.Code, f.Resource, f.Field}, " "))
	}
	return out
}

func TestValidate(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(d *domain.Document)
		want   []string // every finding, in order; empty means the document is clean
	}{
		{"annex B is clean", func(*domain.Document) {}, nil},
		{"OPS-3 version must be 1", func(d *domain.Document) { d.Version = 2 },
			[]string{"error OPS-3 unsupported_version document version"}},
		{"OPS-3 version is required", func(d *domain.Document) { d.Version = 0 },
			[]string{"error OPS-3 unsupported_version document version"}},

		// Settings.
		{"LIM-2 reset_interval outside the enumeration", func(d *domain.Document) { d.Settings.Budget.ResetInterval = "weekly" },
			[]string{"error LIM-2 invalid_value settings budget.reset_interval"}},
		{"LIM-2 reset_interval is required", func(d *domain.Document) { d.Settings.Budget.ResetInterval = "" },
			[]string{"error LIM-2 required settings budget.reset_interval"}},
		{"LIM-2 negative limit", func(d *domain.Document) { d.Settings.Budget.LimitUSD = new(-1.0) },
			[]string{"error LIM-2 invalid_value settings budget.limit_usd"}},
		{"LIM-2 a zero limit is a limit", func(d *domain.Document) { d.Settings.Budget.LimitUSD = new(0.0) }, nil},
		{"LIM-2 limit_usd is required", func(d *domain.Document) { d.Settings.Budget.LimitUSD = nil },
			[]string{"error LIM-2 required settings budget.limit_usd"}},
		{"LIM-5 negative global parallel limit", func(d *domain.Document) { d.Settings.MaxParallelRequests = -1 },
			[]string{"error LIM-5 invalid_value settings max_parallel_requests"}},
		{"LIM-6 negative body cap", func(d *domain.Document) { d.Settings.MaxBodyBytes = -1 },
			[]string{"error LIM-6 invalid_value settings max_body_bytes"}},
		{"RES-4 negative response cap", func(d *domain.Document) { d.Settings.MaxResponseBytes = -1 },
			[]string{"error RES-4 invalid_value settings max_response_bytes"}},
		{"RES-3 negative timeout", func(d *domain.Document) { d.Settings.Timeouts.TotalMS = -1 },
			[]string{"error RES-3 invalid_value settings timeouts.total_ms"}},
		{"USE-11 negative retention", func(d *domain.Document) { d.Settings.Retention.AuditMonths = -1 },
			[]string{"error USE-11 invalid_value settings retention.audit_months"}},
		{"TEL-8 negative content bound", func(d *domain.Document) { d.Settings.Telemetry.ContentMaxBytes = -1 },
			[]string{"error TEL-8 invalid_value settings telemetry.content_max_bytes"}},
		{"TEL-2 basic_auth_env names two variables", func(d *domain.Document) { d.Settings.Telemetry.BasicAuthEnv = []string{"ONLY_USER"} },
			[]string{"error TEL-2 invalid_value settings telemetry.basic_auth_env"}},
		{"TEL-2 an empty basic_auth_env", func(d *domain.Document) { d.Settings.Telemetry.BasicAuthEnv = []string{} },
			[]string{"error TEL-2 invalid_value settings telemetry.basic_auth_env"}},
		{"TEL-2 reserved header in lower case", func(d *domain.Document) { d.Settings.Telemetry.Headers["authorization"] = pastedKey },
			[]string{"error TEL-2 reserved_header settings telemetry.headers.authorization"}},
		{"TEL-2 reserved header x-api-key in mixed case", func(d *domain.Document) { d.Settings.Telemetry.Headers["X-Api-Key"] = pastedKey },
			[]string{"error TEL-2 reserved_header settings telemetry.headers.X-Api-Key"}},
		{"TEL-2 reserved Cookie and Proxy-Authorization", func(d *domain.Document) {
			d.Settings.Telemetry.Headers["Cookie"] = pastedKey
			d.Settings.Telemetry.Headers["Proxy-Authorization"] = pastedKey
		}, []string{"error TEL-2 reserved_header settings telemetry.headers.Cookie", "error TEL-2 reserved_header settings telemetry.headers.Proxy-Authorization"}},
		{"TEL-2 a credential header by reference is fine", func(d *domain.Document) {
			d.Settings.Telemetry.HeaderEnv = map[string]string{"Authorization": "OTLP_AUTH"}
		}, nil},
		{"TEL-2 header name with a space", func(d *domain.Document) { d.Settings.Telemetry.Headers["a b"] = "x" },
			[]string{"error TEL-2 invalid_value settings telemetry.headers"}},
		{"TEL-2 header value with a line break", func(d *domain.Document) { d.Settings.Telemetry.Headers["x-tag"] = "a\r\nInjected: 1" },
			[]string{"error TEL-2 invalid_value settings telemetry.headers.x-tag"}},
		{"TEL-2 header_env name with a colon", func(d *domain.Document) { d.Settings.Telemetry.HeaderEnv = map[string]string{"x:y": "VAR"} },
			[]string{"error TEL-2 invalid_value settings telemetry.header_env"}},

		// Credential references.
		{"SEC-5 api_key_env holding a pasted credential", func(d *domain.Document) {
			edit(d.Providers, "openrouter", func(p *domain.Provider) { p.APIKeyEnv = pastedKey })
		}, []string{"error SEC-5 invalid_credential_reference providers/openrouter api_key_env"}},
		{"SEC-5 reserved prefix in mixed case", func(d *domain.Document) {
			edit(d.Providers, "openrouter", func(p *domain.Provider) { p.APIKeyEnv = "StarGaite_Admin_Key" })
		}, []string{"error SEC-5 invalid_credential_reference providers/openrouter api_key_env"}},
		{"SEC-5 reserved prefix in lower case", func(d *domain.Document) {
			edit(d.Providers, "openrouter", func(p *domain.Provider) { p.APIKeyEnv = "stargaite_admin_key" })
		}, []string{"error SEC-5 invalid_credential_reference providers/openrouter api_key_env"}},
		{"SEC-5 the prefix needs its underscore", func(d *domain.Document) {
			edit(d.Providers, "openrouter", func(p *domain.Provider) { p.APIKeyEnv = "STARGAITEX_KEY" })
		}, nil},
		{"SEC-5 lower case and a leading underscore are names", func(d *domain.Document) {
			edit(d.Providers, "openrouter", func(p *domain.Provider) { p.APIKeyEnv = "_or_key" })
		}, nil},
		{"SEC-5 name starting with a digit", func(d *domain.Document) { d.Settings.Telemetry.BasicAuthEnv[0] = "1USER" },
			[]string{"error SEC-5 invalid_credential_reference settings telemetry.basic_auth_env"}},
		{"SEC-5 header_env reference", func(d *domain.Document) {
			d.Settings.Telemetry.HeaderEnv = map[string]string{"x-token": "STARGAITE_ADMIN_KEY"}
		},
			[]string{"error SEC-5 invalid_credential_reference settings telemetry.header_env.x-token"}},

		// Outbound URLs, literal checks.
		{"SEC-4 base_url is required", func(d *domain.Document) {
			edit(d.Providers, "openrouter", func(p *domain.Provider) { p.BaseURL = "" })
		}, []string{"error SEC-4 required providers/openrouter base_url"}},
		{"SEC-4 plain http without the flag", func(d *domain.Document) {
			edit(d.Providers, "openrouter", func(p *domain.Provider) { p.BaseURL = "http://openrouter.ai/api/v1" })
		}, []string{"error SEC-4 url_refused providers/openrouter base_url"}},
		{"SEC-4 plain http with the flag", func(d *domain.Document) {
			edit(d.Providers, "openrouter", func(p *domain.Provider) {
				p.BaseURL, p.AllowPrivateNetwork = "http://openrouter.ai/api/v1", true
			})
		}, nil},
		{"SEC-4 loopback without the flag", func(d *domain.Document) {
			edit(d.Providers, "tei-embed", func(p *domain.Provider) { p.AllowPrivateNetwork = false })
		}, []string{"error SEC-4 url_refused providers/tei-embed base_url"}},
		{"SEC-4 metadata rerank_url, even with the flag", func(d *domain.Document) {
			edit(d.Providers, "tei-rerank", func(p *domain.Provider) { p.RerankURL = "http://169.254.169.254/rerank" })
		}, []string{"error SEC-4 url_refused providers/tei-rerank rerank_url"}},
		{"SEC-4 telemetry endpoint over plain http", func(d *domain.Document) {
			d.Settings.Telemetry.OTLPEndpoint = "http://langfuse.example.com/api/public/otel"
		}, []string{"error SEC-4 url_refused settings telemetry.otlp_endpoint"}},
		{"SEC-4 telemetry endpoint with its own flag", func(d *domain.Document) {
			d.Settings.Telemetry.OTLPEndpoint = "http://127.0.0.1:4318"
			d.Settings.Telemetry.AllowPrivateNetwork = true
		}, nil},
		{"SEC-4 a provider's flag does not open the telemetry endpoint", func(d *domain.Document) {
			d.Settings.Telemetry.OTLPEndpoint = "http://127.0.0.1:4318"
		}, []string{"error SEC-4 url_refused settings telemetry.otlp_endpoint"}},

		// Providers.
		{"OPS-5 provider name syntax", func(d *domain.Document) { d.Providers["-tei"] = d.Providers["tei-embed"] },
			[]string{"error OPS-5 invalid_name providers/-tei "}},
		{"OPS-5 provider name of 65 characters", func(d *domain.Document) { d.Providers[strings.Repeat("p", 65)] = d.Providers["tei-embed"] },
			[]string{"error OPS-5 invalid_name providers/" + strings.Repeat("p", 65) + " "}},
		{"OPS-5 provider name of 64 characters", func(d *domain.Document) { d.Providers[strings.Repeat("p", 64)] = d.Providers["tei-embed"] }, nil},
		{"OPS-5 protocol outside the enumeration", func(d *domain.Document) {
			edit(d.Providers, "tei-embed", func(p *domain.Provider) { p.Protocol = "grpc" })
		}, []string{"error OPS-5 invalid_value providers/tei-embed protocol"}},
		{"OPS-5 protocol is required", func(d *domain.Document) {
			edit(d.Providers, "tei-embed", func(p *domain.Provider) { p.Protocol = "" })
		}, []string{"error OPS-5 required providers/tei-embed protocol"}},
		{"OPS-5 billing outside the enumeration", func(d *domain.Document) {
			edit(d.Providers, "tei-embed", func(p *domain.Provider) { p.Billing = "free" })
		}, []string{"error OPS-5 invalid_value providers/tei-embed billing"}},
		{"OPS-5 billing is required", func(d *domain.Document) {
			edit(d.Providers, "tei-embed", func(p *domain.Provider) { p.Billing = "" })
		}, []string{"error OPS-5 required providers/tei-embed billing"}},
		{"LIM-8 quota on a pay-per-use provider", func(d *domain.Document) {
			edit(d.Providers, "openrouter", func(p *domain.Provider) { p.Quota = []domain.QuotaWindow{{Window: "1h", Requests: 10}} })
		}, []string{"error LIM-8 invalid_value providers/openrouter quota"}},
		{"LIM-8 quota window syntax", func(d *domain.Document) {
			edit(d.Providers, "minimax-plan", func(p *domain.Provider) { p.Quota[0].Window = "5 hours" })
		}, []string{"error LIM-8 invalid_value providers/minimax-plan quota"}},
		{"LIM-8 quota window of zero", func(d *domain.Document) {
			edit(d.Providers, "minimax-plan", func(p *domain.Provider) { p.Quota[0].Window = "0h" })
		}, []string{"error LIM-8 invalid_value providers/minimax-plan quota"}},
		{"LIM-8 quota window without a limit", func(d *domain.Document) {
			edit(d.Providers, "minimax-plan", func(p *domain.Provider) { p.Quota[0].Requests = 0 })
		}, []string{"error LIM-8 invalid_value providers/minimax-plan quota"}},
		{"LIM-8 negative quota", func(d *domain.Document) {
			edit(d.Providers, "minimax-plan", func(p *domain.Provider) { p.Quota[0].Tokens = -1 })
		}, []string{"error LIM-8 invalid_value providers/minimax-plan quota"}},
		{"LIM-8 a token window", func(d *domain.Document) {
			edit(d.Providers, "minimax-plan", func(p *domain.Provider) { p.Quota[0] = domain.QuotaWindow{Window: "30m", Tokens: 100000} })
		}, nil},
		{"USE-6 cost_report path with an empty segment", func(d *domain.Document) {
			edit(d.Providers, "openrouter", func(p *domain.Provider) { p.CostReport.Path = "usage..cost" })
		}, []string{"error USE-6 invalid_value providers/openrouter cost_report.path"}},
		{"USE-6 cost_report without a unit", func(d *domain.Document) {
			edit(d.Providers, "openrouter", func(p *domain.Provider) { p.CostReport.USDPerUnit = 0 })
		}, []string{"error USE-6 invalid_value providers/openrouter cost_report.usd_per_unit"}},
		{"USE-8 stream_usage outside the enumeration", func(d *domain.Document) {
			edit(d.Providers, "openrouter", func(p *domain.Provider) { p.StreamUsage = "always" })
		}, []string{"error USE-8 invalid_value providers/openrouter stream_usage"}},
		{"RSN-2 target_defaults dialect outside the table", func(d *domain.Document) {
			edit(d.Providers, "openrouter", func(p *domain.Provider) { p.TargetDefaults.Dialect = "reasoning" })
		}, []string{"error RSN-2 invalid_value providers/openrouter target_defaults.dialect"}},

		// Targets.
		{"OPS-5 target of an unknown provider", func(d *domain.Document) { d.Targets["nope/model"] = domain.Target{Kind: "chat"} },
			[]string{"error OPS-5 unknown_provider targets/nope/model "}},
		{"OPS-5 removing a provider that targets still use", func(d *domain.Document) { delete(d.Providers, "tei-embed") },
			[]string{"error OPS-5 unknown_provider targets/tei-embed/bge-m3 "}},
		{"OPS-5 target id without a slash", func(d *domain.Document) { d.Targets["model"] = domain.Target{Kind: "chat"} },
			[]string{"error OPS-5 invalid_name targets/model "}},
		{"OPS-5 upstream id with a space", func(d *domain.Document) { d.Targets["tei-embed/a b"] = domain.Target{Kind: "embeddings"} },
			[]string{"error OPS-5 invalid_name targets/tei-embed/a b "}},
		{"OPS-5 kind is required", func(d *domain.Document) {
			edit(d.Targets, "tei-embed/bge-m3", func(t *domain.Target) { t.Kind = "" })
		}, []string{"error OPS-5 required targets/tei-embed/bge-m3 kind"}},
		{"OPS-5 kind outside the enumeration", func(d *domain.Document) {
			edit(d.Targets, "tei-embed/bge-m3", func(t *domain.Target) { t.Kind = "image" })
		}, []string{"error OPS-5 invalid_value targets/tei-embed/bge-m3 kind"}},
		{"PAR-6 negative target limit", func(d *domain.Document) {
			edit(d.Targets, "tei-embed/bge-m3", func(t *domain.Target) { t.MaxOutputTokens = -1 })
		}, []string{"error PAR-6 invalid_value targets/tei-embed/bge-m3 max_output_tokens"}},
		{"RSN-3 dialect outside the table", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Reasoning.Dialect = "effort" })
		}, []string{"error RSN-3 invalid_value targets/" + qw + " reasoning.dialect"}},
		{"RSN-3 level outside the scale", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Reasoning.Levels = []string{"low", "huge"} })
		}, []string{"error RSN-3 invalid_value targets/" + qw + " reasoning.levels"}},
		{"RSN-3 levels out of scale order", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Reasoning.Levels = []string{"high", "low"} })
		}, []string{"error RSN-3 invalid_value targets/" + qw + " reasoning.levels"}},
		{"RSN-3 repeated level", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Reasoning.Levels = []string{"low", "low"} })
		}, []string{"error RSN-3 invalid_value targets/" + qw + " reasoning.levels"}},
		{"RSN-3 none is not a level", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Reasoning.Levels = []string{"none", "low"} })
		}, []string{"error RSN-3 invalid_value targets/" + qw + " reasoning.levels"}},
		{"RSN-3 an on/off dialect has the single level on", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Reasoning.Dialect = "thinking_toggle" })
		}, []string{"error RSN-3 invalid_value targets/" + qw + " reasoning.levels"}},
		{"RSN-3 an on/off dialect with level on", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Reasoning.Dialect, t.Reasoning.Levels = "enable_thinking", []string{"on"} })
		}, nil},
		{"RSN-3 can_disable false without levels", func(d *domain.Document) {
			edit(d.Targets, mm, func(t *domain.Target) { t.Reasoning.Levels = nil })
		}, []string{"error RSN-3 levels_required targets/" + mm + " reasoning.levels", "warning RSN-3 reasoning_undeclared targets/" + mm + " reasoning"}},
		{"RSN-3 thinking_budget without levels", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Reasoning.Dialect, t.Reasoning.Levels = "thinking_budget", nil })
		}, []string{"error RSN-3 levels_required targets/" + qw + " reasoning.levels", "warning RSN-3 reasoning_undeclared targets/" + qw + " reasoning"}},
		{"RSN-3 thinking_budget level without a budget", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) {
				t.Reasoning.Dialect, t.Reasoning.Levels, t.Reasoning.Budgets = "thinking_budget", []string{"low", "high"}, map[string]int64{"low": 1024}
			})
		}, []string{"error RSN-3 budgets_required targets/" + qw + " reasoning.budgets"}},
		{"RSN-3 1,000 invalid levels and budgets give one finding", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) {
				t.Reasoning.Dialect, t.Reasoning.Levels, t.Reasoning.Budgets = "thinking_budget", nil, map[string]int64{}
				for i := range 1000 {
					t.Reasoning.Levels = append(t.Reasoning.Levels, "low")
					t.Reasoning.Budgets[fmt.Sprint("l", i)] = 1
				}
			})
		}, []string{"error RSN-3 invalid_value targets/" + qw + " reasoning.levels"}},
		{"RSN-3 thinking_budget with a budget per level", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) {
				t.Reasoning.Dialect, t.Reasoning.Levels, t.Reasoning.Budgets = "thinking_budget", []string{"low", "high"}, map[string]int64{"low": 1024, "high": 8192}
			})
		}, nil},
		{"RSN-3 budget for an undeclared level", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) {
				t.Reasoning.Dialect, t.Reasoning.Levels, t.Reasoning.Budgets = "thinking_budget", []string{"low"}, map[string]int64{"low": 1024, "max": 1}
			})
		}, []string{"error RSN-3 invalid_value targets/" + qw + " reasoning.budgets"}},
		{"RSN-3 budgets on another dialect", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Reasoning.Budgets = map[string]int64{"low": 1024} })
		}, []string{"error RSN-3 invalid_value targets/" + qw + " reasoning.budgets"}},
		{"RSN-3 map from a level outside the scale", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Reasoning.Map = map[string]string{"huge": "high"} })
		}, []string{"error RSN-3 invalid_value targets/" + qw + " reasoning.map"}},
		{"RSN-3 map to an empty level", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Reasoning.Map = map[string]string{"max": ""} })
		}, []string{"error RSN-3 invalid_value targets/" + qw + " reasoning.map"}},
		{"RSN-3 map to the provider's own spelling", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Reasoning.Map = map[string]string{"max": "x-high", "none": "off"} })
		}, nil},
		{"RSN-3 warning for undeclared can_disable", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Reasoning.CanDisable = nil })
		}, []string{"warning RSN-3 reasoning_undeclared targets/" + qw + " reasoning"}},
		{"RSN-3 warning for undeclared levels", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Reasoning.Levels = nil })
		}, []string{"warning RSN-3 reasoning_undeclared targets/" + qw + " reasoning"}},
		{"RSN-3 no warning for a dialect none", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Reasoning = &domain.Reasoning{Dialect: "none"} })
		}, []string{"warning RSN-5 effort_dropped models/fast reasoning.effort"}},
		{"RSN-3 no warning for a target that is not chat", func(d *domain.Document) {
			edit(d.Targets, "tei-embed/bge-m3", func(t *domain.Target) { t.Reasoning = &domain.Reasoning{Dialect: "reasoning_effort"} })
		}, nil},
		{"RSN-2 a reasoning block without a dialect has the dialect none", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) {
				t.Reasoning = &domain.Reasoning{Levels: []string{"high"}, CanDisable: new(true)}
			})
		}, []string{"warning RSN-5 effort_dropped models/fast reasoning.effort"}},
		{"USE-6 pricing without an output price", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Pricing.OutputPerMillion = nil })
		}, []string{"error USE-6 required targets/" + qw + " pricing"}},
		{"USE-6 negative price", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Pricing.CacheReadPerMillion = new(-0.01) })
		}, []string{"error USE-6 invalid_value targets/" + qw + " pricing"}},
		{"USE-6 negative cache write price", func(d *domain.Document) {
			edit(d.Targets, sn, func(t *domain.Target) { t.Pricing.CacheWritePerMillion = new(-1.0) })
		}, []string{"error USE-6 invalid_value targets/" + sn + " pricing"}},
		{"USE-6 a free price", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Pricing.InputPerMillion = new(0.0) })
		}, nil},
		{"USE-6 as_of is a date", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Pricing.AsOf = "November 2026" })
		}, []string{"error USE-6 invalid_value targets/" + qw + " pricing.as_of"}},
		{"USE-6 as_of is required", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Pricing.AsOf = "" })
		}, []string{"error USE-6 invalid_value targets/" + qw + " pricing.as_of"}},

		// USE-12: unpriced pay-per-use targets.
		{"USE-12 priced only by the cost report", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Pricing = nil })
		}, []string{"warning USE-12 no_fallback_prices targets/" + qw + " pricing"}},
		{"USE-12 unpriced under the instance budget", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Pricing = nil })
			edit(d.Providers, "openrouter", func(p *domain.Provider) { p.CostReport = nil })
			for name := range d.Keys {
				edit(d.Keys, name, func(k *domain.Key) { k.Budget = nil })
			}
		}, []string{"error USE-12 unpriced_under_budget targets/" + qw + " pricing"}},
		{"USE-12 unpriced under a key budget", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Pricing = nil })
			edit(d.Providers, "openrouter", func(p *domain.Provider) { p.CostReport = nil })
			d.Settings.Budget = nil
		}, []string{"error USE-12 unpriced_under_budget targets/" + qw + " pricing"}},
		{"USE-12 unpriced under an alias budget", func(d *domain.Document) {
			edit(d.Targets, sn, func(t *domain.Target) { t.Pricing = nil })
			d.Settings.Budget = nil
		}, []string{"error USE-12 unpriced_under_budget targets/" + sn + " pricing"}},
		{"USE-12 unpriced under the budget of a key with every alias", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Pricing = nil })
			edit(d.Providers, "openrouter", func(p *domain.Provider) { p.CostReport = nil })
			d.Settings.Budget = nil
			for name := range d.Keys {
				edit(d.Keys, name, func(k *domain.Key) { k.Budget = nil })
			}
			d.Keys["all"] = domain.Key{Models: []string{"*"}, Budget: &domain.Budget{LimitUSD: new(1.0), ResetInterval: "daily"}}
		}, []string{"error USE-12 unpriced_under_budget targets/" + qw + " pricing"}},
		{"USE-12 unpriced and under no budget", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Pricing = nil })
			edit(d.Providers, "openrouter", func(p *domain.Provider) { p.CostReport = nil })
			d.Settings.Budget = nil
			for name := range d.Keys {
				edit(d.Keys, name, func(k *domain.Key) { k.Budget = nil })
			}
		}, []string{"warning USE-12 unpriced targets/" + qw + " pricing"}},

		// Model aliases.
		{"OPS-5 alias name syntax", func(d *domain.Document) { d.Models["*"] = d.Models["embed"] },
			[]string{"error OPS-5 invalid_name models/* "}},
		{"OPS-5 alias with no target", func(d *domain.Document) {
			edit(d.Models, "embed", func(m *domain.Model) { m.Targets = nil })
		}, []string{"error OPS-5 no_targets models/embed targets"}},
		{"OPS-5 alias with four targets", func(d *domain.Document) {
			d.Targets["openrouter/b"], d.Targets["openrouter/c"] = d.Targets[qw], d.Targets[qw]
			edit(d.Models, "fast", func(m *domain.Model) { m.Targets = append(m.Targets, "openrouter/b", "openrouter/c") })
		}, []string{"error OPS-5 too_many_targets models/fast targets"}},
		{"OPS-5 alias with 1,000 targets gives one finding", func(d *domain.Document) {
			edit(d.Models, "embed", func(m *domain.Model) {
				m.Targets = nil
				for i := range 1000 {
					m.Targets = append(m.Targets, fmt.Sprintf("tei-embed/m%d", i%500)) // unknown, and each listed twice
				}
			})
		}, []string{"error OPS-5 too_many_targets models/embed targets"}},
		{"OPS-5 alias with three targets", func(d *domain.Document) {
			d.Targets["openrouter/b"] = d.Targets[qw]
			edit(d.Models, "fast", func(m *domain.Model) { m.Targets = append(m.Targets, "openrouter/b") })
		}, nil},
		{"OPS-5 alias naming an unknown target", func(d *domain.Document) {
			edit(d.Models, "embed", func(m *domain.Model) { m.Targets = []string{"tei-embed/other"} })
		}, []string{"error OPS-5 unknown_target models/embed targets"}},
		{"OPS-5 removing a target an alias uses", func(d *domain.Document) { delete(d.Targets, "tei-embed/bge-m3") },
			[]string{"error OPS-5 unknown_target models/embed targets"}},
		{"OPS-5 target listed twice", func(d *domain.Document) {
			edit(d.Models, "embed", func(m *domain.Model) { m.Targets = append(m.Targets, m.Targets[0]) })
		}, []string{"error OPS-5 duplicate_target models/embed targets"}},
		{"PAR-5 required capability undeclared", func(d *domain.Document) {
			edit(d.Models, "deep", func(m *domain.Model) { m.Targets = []string{sn, qw}; m.Requires = append(m.Requires, "vision") })
		}, []string{"error PAR-5 requirement_not_met models/deep requires"}},
		{"PAR-5 required capability declared false", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.Capabilities.Tools = new(false) })
		}, []string{"error PAR-5 requirement_not_met models/fast requires"}},
		{"PAR-5 every target declares it true", func(d *domain.Document) {
			edit(d.Models, "deep", func(m *domain.Model) { m.Requires = append(m.Requires, "vision") })
		}, nil},
		{"PAR-5 unknown capability", func(d *domain.Document) {
			edit(d.Models, "embed", func(m *domain.Model) { m.Requires = []string{"gpu"} })
		}, []string{"error PAR-5 invalid_value models/embed requires"}},
		{"PAR-6 alias cap above a target limit", func(d *domain.Document) {
			edit(d.Models, "fast", func(m *domain.Model) { m.MaxOutputTokens = 32001 })
		}, []string{"error PAR-6 output_cap_exceeds_target models/fast max_output_tokens"}},
		{"PAR-6 alias cap equal to the lowest target limit", func(d *domain.Document) {
			edit(d.Models, "fast", func(m *domain.Model) { m.MaxOutputTokens = 32000 })
		}, nil},
		{"PAR-6 negative alias cap", func(d *domain.Document) {
			edit(d.Models, "embed", func(m *domain.Model) { m.MaxOutputTokens = -1 })
		}, []string{"error PAR-6 invalid_value models/embed max_output_tokens"}},
		{"PAR-7 anthropic target and no alias cap", func(d *domain.Document) {
			edit(d.Models, "deep", func(m *domain.Model) { m.MaxOutputTokens = 0 })
		}, []string{"error PAR-7 output_cap_required models/deep max_output_tokens"}},
		{"PAR-7 an openai target needs no alias cap", func(d *domain.Document) {
			edit(d.Models, "fast", func(m *domain.Model) { m.Targets, m.MaxOutputTokens = []string{qw}, 0 })
		}, nil},
		{"PAR-2 forward rule naming the dialect's key", func(d *domain.Document) {
			edit(d.Models, "fast", func(m *domain.Model) { m.Params["reasoning"] = "forward" })
		}, []string{"error PAR-2 param_collision models/fast params.reasoning"}},
		{"PAR-2 forward rule naming a key of an adaptive_effort target", func(d *domain.Document) {
			edit(d.Models, "fast", func(m *domain.Model) { m.Params["output_config"] = "forward" })
		}, []string{"error PAR-2 param_collision models/fast params.output_config"}},
		{"PAR-2 forward rule naming a key of a target's off fragment", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) {
				t.Reasoning.Off = map[string]json.RawMessage{"enable_reasoning": json.RawMessage("false")}
			})
			edit(d.Models, "fast", func(m *domain.Model) { m.Params["enable_reasoning"] = "forward" })
		}, []string{"error PAR-2 param_collision models/fast params.enable_reasoning"}},
		{"PAR-2 forward rule naming a key of a target's on fragment", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) {
				t.Reasoning.On = map[string]json.RawMessage{"chat_template_kwargs": json.RawMessage(`{"thinking":true}`)}
			})
			edit(d.Models, "fast", func(m *domain.Model) { m.Params["chat_template_kwargs"] = "forward" })
		}, []string{"error PAR-2 param_collision models/fast params.chat_template_kwargs"}},
		{"PAR-2 the same key in a fragment of another alias's target", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) {
				t.Reasoning.Off = map[string]json.RawMessage{"enable_reasoning": json.RawMessage("false")}
			})
			edit(d.Models, "deep", func(m *domain.Model) { m.Params = map[string]string{"enable_reasoning": "forward"} })
		}, nil},
		{"PAR-2 forward rule naming the declared output_cap_param", func(d *domain.Document) {
			edit(d.Targets, qw, func(t *domain.Target) { t.OutputCapParam = "max_completion_tokens" })
			edit(d.Models, "fast", func(m *domain.Model) { m.Params["max_completion_tokens"] = "forward" })
		}, []string{"error PAR-2 param_collision models/fast params.max_completion_tokens"}},
		{"PAR-2 forward rule naming the default output_cap_param", func(d *domain.Document) {
			edit(d.Models, "fast", func(m *domain.Model) { m.Params["max_tokens"] = "forward" })
		}, []string{"error PAR-2 param_collision models/fast params.max_tokens", "error PAR-2 param_collision models/fast params.max_tokens"}},
		{"PAR-2 every cap toward anthropic is max_tokens", func(d *domain.Document) {
			edit(d.Targets, sn, func(t *domain.Target) { t.OutputCapParam = "max_completion_tokens" })
			edit(d.Models, "deep", func(m *domain.Model) { m.Params = map[string]string{"max_tokens": "forward"} })
		}, []string{"error PAR-2 param_collision models/deep params.max_tokens"}},
		{"PAR-2 forward rule naming stream_options", func(d *domain.Document) {
			edit(d.Models, "embed", func(m *domain.Model) { m.Params = map[string]string{"stream_options": "forward"} })
		}, []string{"error PAR-2 param_collision models/embed params.stream_options"}},
		{"PAR-2 a drop rule may name any key", func(d *domain.Document) {
			edit(d.Models, "fast", func(m *domain.Model) { m.Params["reasoning"], m.Params["stream_options"] = "drop", "drop" })
		}, nil},
		{"PAR-2 a forward rule for a key nobody writes", func(d *domain.Document) {
			edit(d.Models, "fast", func(m *domain.Model) { m.Params["enable_search"] = "forward" })
		}, nil},
		{"PAR-2 rule outside the enumeration", func(d *domain.Document) {
			edit(d.Models, "fast", func(m *domain.Model) { m.Params["enable_search"] = "keep" })
		}, []string{"error PAR-2 invalid_value models/fast params.enable_search"}},
		{"RSN-5 effort outside the scale", func(d *domain.Document) {
			edit(d.Models, "fast", func(m *domain.Model) { m.Reasoning.Effort = "default" })
		}, []string{"error RSN-5 invalid_value models/fast reasoning.effort"}},
		{"RSN-5 mode outside the enumeration", func(d *domain.Document) {
			edit(d.Models, "fast", func(m *domain.Model) { m.Reasoning.Mode = "always" })
		}, []string{"error RSN-5 invalid_value models/fast reasoning.mode"}},
		{"RSN-5 effort without a mode", func(d *domain.Document) {
			edit(d.Models, "fast", func(m *domain.Model) { m.Reasoning.Mode = "" })
		}, []string{"error RSN-5 required models/fast reasoning.mode"}},
		{"RSN-5 mode without an effort", func(d *domain.Document) {
			edit(d.Models, "embed", func(m *domain.Model) { m.Reasoning.Mode = "forced" })
		}, []string{"error RSN-5 required models/embed reasoning.effort"}},
		{"RSN-5 warning for an effort toward a none dialect", func(d *domain.Document) {
			edit(d.Models, "embed", func(m *domain.Model) { m.Reasoning = domain.ModelReasoning{Effort: "low", Mode: "default"} })
		}, []string{"warning RSN-5 effort_dropped models/embed reasoning.effort"}},
		{"RSN-5 provider_default toward a none dialect", func(d *domain.Document) {
			edit(d.Models, "embed", func(m *domain.Model) {
				m.Reasoning = domain.ModelReasoning{Effort: "provider_default", Mode: "default"}
			})
		}, nil},
		{"RSN-7 always-reasoning target and no effort", func(d *domain.Document) {
			edit(d.Models, "fast", func(m *domain.Model) { m.Reasoning = domain.ModelReasoning{} })
		}, []string{"error RSN-7 effort_required models/fast reasoning.effort"}},
		{"RSN-7 always-reasoning fallback and no effort", func(d *domain.Document) {
			edit(d.Models, "fast", func(m *domain.Model) { m.Targets, m.Reasoning = []string{qw, mm}, domain.ModelReasoning{} })
		}, []string{"error RSN-7 effort_required models/fast reasoning.effort"}},
		{"RSN-7 provider_default counts as an effort", func(d *domain.Document) {
			edit(d.Models, "fast", func(m *domain.Model) { m.Reasoning.Effort = "provider_default" })
		}, nil},
		{"RSN-7 an output cap does not replace the effort", func(d *domain.Document) {
			edit(d.Models, "deep", func(m *domain.Model) { m.Reasoning = domain.ModelReasoning{} })
		}, []string{"error RSN-7 effort_required models/deep reasoning.effort"}},
		{"LIM-2 alias budget without an interval", func(d *domain.Document) {
			edit(d.Models, "deep", func(m *domain.Model) { m.Budget.ResetInterval = "" })
		}, []string{"error LIM-2 required models/deep budget.reset_interval"}},
		{"LIM-5 negative alias limit", func(d *domain.Document) {
			edit(d.Models, "deep", func(m *domain.Model) { m.TPMLimit = -1 })
		}, []string{"error LIM-5 invalid_value models/deep tpm_limit"}},
		{"TEL-7 alias sample rate above 1", func(d *domain.Document) {
			edit(d.Models, "embed", func(m *domain.Model) { m.TraceSampleRate = new(1.5) })
		}, []string{"error TEL-7 invalid_value models/embed trace_sample_rate"}},
		{"TEL-7 alias sample rate of 1", func(d *domain.Document) {
			edit(d.Models, "embed", func(m *domain.Model) { m.TraceSampleRate = new(1.0) })
		}, nil},

		// Keys.
		{"RSN-9 key name starting with an underscore", func(d *domain.Document) { d.Keys["_verify"] = domain.Key{Models: []string{}} },
			[]string{"error RSN-9 reserved_name keys/_verify "}},
		{"OPS-5 key name syntax", func(d *domain.Document) { d.Keys["my agent"] = domain.Key{Models: []string{}} },
			[]string{"error OPS-5 invalid_name keys/my agent "}},
		{"OPS-5 key without models", func(d *domain.Document) { d.Keys["new"] = domain.Key{} },
			[]string{"error OPS-5 required keys/new models"}},
		{"OPS-5 key with an empty list reaches nothing", func(d *domain.Document) { d.Keys["new"] = domain.Key{Models: []string{}} }, nil},
		{"OPS-5 key with every alias", func(d *domain.Document) { d.Keys["new"] = domain.Key{Models: []string{"*"}} }, nil},
		{"OPS-5 every alias is listed alone", func(d *domain.Document) { d.Keys["new"] = domain.Key{Models: []string{"*", "fast"}} },
			[]string{"error OPS-5 invalid_value keys/new models"}},
		{"OPS-5 an alias is listed once", func(d *domain.Document) { d.Keys["new"] = domain.Key{Models: []string{"fast", "rerank", "fast"}} },
			[]string{"error OPS-5 duplicate_alias keys/new models"}},
		{"OPS-5 warning for a key naming an unknown alias", func(d *domain.Document) { delete(d.Models, "rerank") },
			[]string{"warning OPS-5 unknown_alias keys/classifier-flow models"}},
		{"LIM-2 key budget with a wrong interval", func(d *domain.Document) {
			edit(d.Keys, "invoice-agent", func(k *domain.Key) { k.Budget.ResetInterval = "hourly" })
		}, []string{"error LIM-2 invalid_value keys/invoice-agent budget.reset_interval"}},
		{"LIM-5 negative key limit", func(d *domain.Document) {
			edit(d.Keys, "invoice-agent", func(k *domain.Key) { k.RPMLimit = -1 })
		}, []string{"error LIM-5 invalid_value keys/invoice-agent rpm_limit"}},
		{"TEL-7 negative key sample rate", func(d *domain.Document) {
			edit(d.Keys, "classifier-flow", func(k *domain.Key) { k.TraceSampleRate = new(-0.1) })
		}, []string{"error TEL-7 invalid_value keys/classifier-flow trace_sample_rate"}},
		{"LIM-10 share above 1", func(d *domain.Document) {
			edit(d.Keys, "invoice-agent", func(k *domain.Key) { k.QuotaShare["minimax-plan"] = 1.5 })
		}, []string{"error LIM-10 invalid_value keys/invoice-agent quota_share.minimax-plan"}},
		{"LIM-10 share of 0", func(d *domain.Document) {
			edit(d.Keys, "invoice-agent", func(k *domain.Key) { k.QuotaShare["minimax-plan"] = 0 })
		}, []string{"error LIM-10 invalid_value keys/invoice-agent quota_share.minimax-plan"}},
		{"LIM-10 share of a whole plan", func(d *domain.Document) {
			edit(d.Keys, "invoice-agent", func(k *domain.Key) { k.QuotaShare["minimax-plan"] = 1 })
		}, nil},
		{"LIM-10 share of a provider that is not a subscription", func(d *domain.Document) {
			edit(d.Keys, "invoice-agent", func(k *domain.Key) { k.QuotaShare["openrouter"] = 0.5 })
		}, []string{"error LIM-10 invalid_value keys/invoice-agent quota_share.openrouter"}},
		{"LIM-10 warning for a share of an unknown provider", func(d *domain.Document) {
			edit(d.Keys, "invoice-agent", func(k *domain.Key) { k.QuotaShare["gone"] = 0.5 })
		}, []string{"warning LIM-10 unknown_provider keys/invoice-agent quota_share.gone"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			doc := annexBJSON(t)
			tt.mutate(&doc)
			got := Validate(&doc)
			want := tt.want
			if want == nil {
				want = []string{}
			}
			if s := summary(got); !slices.Equal(s, want) {
				t.Errorf("findings\n%q\nwant\n%q", s, want)
			}
			for _, f := range got {
				if f.Message == "" || strings.Contains(f.Message, pastedKey) {
					t.Errorf("message %q is empty or carries the pastedKey credential", f.Message)
				}
			}
		})
	}
}

// Many findings, from every map of the document, come out in one order.
func TestValidateOrder(t *testing.T) {
	doc := annexBJSON(t)
	doc.Version = 0
	for _, m := range []string{"fast", "deep", "embed", "rerank"} {
		edit(doc.Models, m, func(m *domain.Model) { m.TPMLimit = -1 })
	}
	for name := range doc.Keys {
		edit(doc.Keys, name, func(k *domain.Key) { k.RPMLimit = -1 })
	}
	for name := range doc.Providers {
		edit(doc.Providers, name, func(p *domain.Provider) { p.StreamUsage = "x" })
	}
	first := summary(Validate(&doc))
	if len(first) != 1+4+2+5 {
		t.Fatalf("got %d findings: %q", len(first), first)
	}
	if !slices.IsSorted(first[1:6]) || first[0] != "error OPS-3 unsupported_version document version" {
		t.Errorf("not in document order: %q", first)
	}
	for range 20 {
		if again := summary(Validate(&doc)); !slices.Equal(again, first) {
			t.Fatalf("order changed:\n%q\n%q", again, first)
		}
	}
}

// fakeResolver answers from a table and fails the test for any other name,
// so no test reaches the network.
func fakeResolver(t *testing.T, answers map[string][]string) func(context.Context, string) ([]netip.Addr, error) {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		texts, ok := answers[host]
		if !ok {
			t.Errorf("unexpected lookup of %q", host)
			return nil, errors.New("unexpected lookup")
		}
		if texts == nil {
			return nil, errors.New("no such host")
		}
		var addrs []netip.Addr
		for _, s := range texts {
			addrs = append(addrs, netip.MustParseAddr(s))
		}
		return addrs, nil
	}
}

func TestCheckResolvedCredentials(t *testing.T) {
	doc := annexBJSON(t)
	edit(doc.Providers, "openrouter", func(p *domain.Provider) { p.APIKeyEnv = pastedKey }) // refused by Validate, so never looked up
	vars := map[string]string{"ANTHROPIC_API_KEY": "a", "MINIMAX_PLAN_API_KEY": "", "LANGFUSE_PUBLIC_KEY": "p"}
	env := Env{
		Lookup: func(name string) (string, bool) {
			if name == pastedKey {
				t.Error("a refused reference was looked up")
			}
			v, ok := vars[name]
			return v, ok
		},
		Findings: []domain.Finding{{Severity: "warning", Rule: "OPS-6", Code: "env_file_malformed", Resource: "env_file", Message: "line 3"}},
	}
	got := CheckResolved(context.Background(), &doc, nil, env)
	want := []string{
		"warning OPS-6 env_file_malformed env_file ",
		"warning OPS-6 credential_unresolved providers/minimax-plan api_key_env", // set but empty
		"warning OPS-6 credential_unresolved settings telemetry.basic_auth_env",  // LANGFUSE_SECRET_KEY unset
	}
	if s := summary(got); !slices.Equal(s, want) {
		t.Errorf("findings\n%q\nwant\n%q", s, want)
	}
	if got[2].Message != "LANGFUSE_SECRET_KEY does not resolve" {
		t.Errorf("message %q", got[2].Message)
	}
}

// SEC-4 at set time: the addresses a name resolves to, classified.
func TestCheckResolvedURLs(t *testing.T) {
	const host = "llm.internal.example"
	for _, tt := range []struct {
		name        string
		url         string
		allow       bool
		answers     map[string][]string
		nilResolver bool
		want        []string
	}{
		{name: "SEC-4 public address", url: "https://" + host + "/v1", answers: map[string][]string{host: {"8.8.8.8"}}},
		{name: "SEC-4 private address without the flag", url: "https://" + host + "/v1", answers: map[string][]string{host: {"10.0.0.5"}},
			want: []string{"error SEC-4 url_refused providers/p base_url"}},
		{name: "SEC-4 private address with the flag", url: "https://" + host + "/v1", allow: true, answers: map[string][]string{host: {"10.0.0.5"}}},
		{name: "SEC-4 one private address among public ones", url: "https://" + host + "/v1", answers: map[string][]string{host: {"fd00::1", "8.8.8.8"}},
			want: []string{"error SEC-4 url_refused providers/p base_url"}},
		{name: "SEC-4 metadata address with the flag", url: "https://" + host + "/v1", allow: true, answers: map[string][]string{host: {"8.8.8.8", "169.254.169.254"}},
			want: []string{"error SEC-4 url_refused providers/p base_url"}},
		{name: "SEC-4 IPv4-mapped loopback", url: "https://" + host + "/v1", answers: map[string][]string{host: {"::ffff:127.0.0.1"}},
			want: []string{"error SEC-4 url_refused providers/p base_url"}},
		{name: "SEC-4 failed lookup warns", url: "https://" + host + "/v1", answers: map[string][]string{host: nil},
			want: []string{"warning SEC-4 url_unresolved providers/p base_url"}},
		{name: "SEC-4 empty answer warns", url: "https://" + host + "/v1", answers: map[string][]string{host: {}},
			want: []string{"warning SEC-4 url_unresolved providers/p base_url"}},
		{name: "SEC-4 no resolver warns", url: "https://" + host + "/v1", nilResolver: true,
			want: []string{"warning SEC-4 url_unresolved providers/p base_url"}},
		{name: "SEC-4 a literal address is not looked up", url: "https://8.8.8.8/v1"},
		{name: "SEC-4 a literal IPv6 address is not looked up", url: "http://[::1]:8080/v1", allow: true},
		{name: "SEC-4 a URL Validate refuses is not looked up", url: "ftp://" + host},
	} {
		t.Run(tt.name, func(t *testing.T) {
			doc := domain.Document{Version: 1}
			env := Env{Lookup: func(string) (string, bool) { return "", false }}
			if !tt.nilResolver {
				env.Resolve = fakeResolver(t, tt.answers)
			}
			urls := []OutboundURL{{Resource: "providers/p", Field: "base_url", URL: tt.url, AllowPrivate: tt.allow}}
			got := CheckResolved(context.Background(), &doc, urls, env)
			want := tt.want
			if want == nil {
				want = []string{}
			}
			if s := summary(got); !slices.Equal(s, want) {
				t.Errorf("findings %q, want %q", s, want)
			}
		})
	}
}

// SEC-4, AC-43: one resolver serves set time and dial time. A name that
// resolves to a public address when it is set and to a private one when it
// is dialled passes CheckResolved and is refused by the client, before any
// byte reaches the provider.
func TestRebindingRefusedAtDialTime(t *testing.T) {
	m := mockprovider.New()
	srv := httptest.NewServer(m)
	defer srv.Close()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	const host = "rebind.example"
	answers := []string{"8.8.8.8", "127.0.0.1"} // set time, then dial time
	lookups := 0
	resolve := func(_ context.Context, h string) ([]netip.Addr, error) {
		if h != host || lookups == len(answers) {
			t.Errorf("unexpected lookup of %q", h)
			return nil, errors.New("unexpected lookup")
		}
		lookups++
		return []netip.Addr{netip.MustParseAddr(answers[lookups-1])}, nil
	}
	url := "https://" + host + ":" + port + "/chat/completions"
	urls := []OutboundURL{{Resource: "providers/p", Field: "base_url", URL: url}}
	env := Env{Lookup: func(string) (string, bool) { return "", false }, Resolve: resolve}
	if got := CheckResolved(context.Background(), &domain.Document{Version: 1}, urls, env); len(got) != 0 {
		t.Fatalf("set time: %q", summary(got))
	}
	resp, err := transport.NewClient(false, env.Resolve).Get(url)
	if err == nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), host+": address 127.0.0.1 is private") || lookups != 2 || len(m.Calls()) != 0 {
		t.Errorf("dial time: err %v, %d lookups, %d calls", err, lookups, len(m.Calls()))
	}
}

func TestOutboundURLs(t *testing.T) {
	doc := annexBJSON(t)
	var got []string
	for _, u := range OutboundURLs(&doc) {
		got = append(got, u.Resource+" "+u.Field+" "+u.URL)
		if u.AllowPrivate != strings.HasPrefix(u.Resource, "providers/tei-") {
			t.Errorf("%s %s: AllowPrivate %v", u.Resource, u.Field, u.AllowPrivate)
		}
	}
	want := []string{
		"providers/anthropic base_url https://api.anthropic.com",
		"providers/minimax-plan base_url https://api.minimax.io/anthropic",
		"providers/openrouter base_url https://openrouter.ai/api/v1",
		"providers/tei-embed base_url http://localhost:8080/v1",
		"providers/tei-rerank base_url http://localhost:8081/v1",
		"providers/tei-rerank rerank_url http://localhost:8081/rerank",
		"settings telemetry.otlp_endpoint https://langfuse.example.com/api/public/otel",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
}

// config.example.yaml is annex B of the kick-off, decodes strictly and
// validates clean. A new warning on it fails here, so it is noticed.
func TestExampleConfiguration(t *testing.T) {
	data, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(annexBYAML(t))+"\n" {
		t.Error("config.example.yaml differs from annex B of the kick-off")
	}
	var doc domain.Document
	if err := DecodeYAML(data, &doc); err != nil {
		t.Fatal(err)
	}
	if got := Validate(&doc); len(got) != 0 {
		t.Errorf("Validate: %q", summary(got))
	}
	all := map[string]string{"OPENROUTER_API_KEY": "x", "ANTHROPIC_API_KEY": "x", "MINIMAX_PLAN_API_KEY": "x", "LANGFUSE_PUBLIC_KEY": "x", "LANGFUSE_SECRET_KEY": "x"}
	env := Env{
		Lookup: func(n string) (string, bool) { v, ok := all[n]; return v, ok },
		Resolve: fakeResolver(t, map[string][]string{
			"api.anthropic.com": {"8.8.8.8"}, "api.minimax.io": {"8.8.4.4"}, "openrouter.ai": {"1.1.1.1"},
			"localhost": {"127.0.0.1", "::1"}, "langfuse.example.com": {"2606:4700:4700::1111"},
		}),
	}
	if got := CheckResolved(context.Background(), &doc, OutboundURLs(&doc), env); len(got) != 0 {
		t.Errorf("CheckResolved: %q", summary(got))
	}
}

// budgetScopeOneTarget is the plain reading of USE-12 that budgetScopes must
// match: for target id, the aliases by name, and for each the instance, the
// alias, then its keys by name.
func budgetScopeOneTarget(doc *domain.Document, id string) string {
	for _, alias := range slices.Sorted(maps.Keys(doc.Models)) {
		m := doc.Models[alias]
		switch {
		case !slices.Contains(m.Targets, id):
			continue
		case doc.Settings.Budget != nil:
			return "the instance"
		case m.Budget != nil:
			return "alias " + alias
		}
		for _, name := range slices.Sorted(maps.Keys(doc.Keys)) {
			k := doc.Keys[name]
			if k.Budget != nil && (slices.Contains(k.Models, alias) || slices.Contains(k.Models, "*")) {
				return "key " + name
			}
		}
	}
	return ""
}

// USE-12: the one-pass map names the same scope as the plain reading, on
// many small documents with budgets in every place.
func TestBudgetScopes(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	budget := &domain.Budget{LimitUSD: new(1.0), ResetInterval: domain.ResetNever}
	for range 500 {
		doc := domain.Document{Targets: map[string]domain.Target{}, Models: map[string]domain.Model{}, Keys: map[string]domain.Key{}}
		if rng.IntN(8) == 0 {
			doc.Settings.Budget = budget
		}
		for i := range 6 {
			doc.Targets[fmt.Sprint("p/t", i)] = domain.Target{}
		}
		for i := range 5 {
			m := domain.Model{}
			for range rng.IntN(4) {
				m.Targets = append(m.Targets, fmt.Sprint("p/t", rng.IntN(6)))
			}
			if rng.IntN(4) == 0 {
				m.Budget = budget
			}
			doc.Models[fmt.Sprint("a", i)] = m
		}
		for range 6 {
			k := domain.Key{}
			for range rng.IntN(3) {
				k.Models = append(k.Models, []string{"a0", "a1", "a2", "a3", "a4", "gone", "*"}[rng.IntN(7)])
			}
			if rng.IntN(2) == 0 {
				k.Budget = budget
			}
			doc.Keys[fmt.Sprint("k", rng.IntN(10))] = k
		}
		scopes := budgetScopes(&doc)
		for id := range doc.Targets {
			if got, want := scopes[id], budgetScopeOneTarget(&doc, id); got != want {
				t.Fatalf("target %s: scope %q, want %q, in %+v", id, got, want, doc)
			}
		}
	}
}

// scale18 is a document at the sizes of kick-off section 18: 1,000 unpriced
// pay-per-use targets, 1,000 aliases with one target each and 10,000 keys
// with one alias each. Key k00000 has a budget, which reaches alias a0000.
func scale18() domain.Document {
	doc := domain.Document{
		Version:   1,
		Providers: map[string]domain.Provider{"p": {Protocol: domain.ProtocolOpenAI, BaseURL: "https://p.example/v1", Billing: domain.BillingPayPerUse}},
		Targets:   map[string]domain.Target{},
		Models:    map[string]domain.Model{},
		Keys:      map[string]domain.Key{},
	}
	for i := range 1000 {
		id := fmt.Sprintf("p/m%04d", i)
		doc.Targets[id] = domain.Target{Kind: domain.KindChat}
		doc.Models[fmt.Sprintf("a%04d", i)] = domain.Model{Targets: []string{id}}
	}
	for i := range 10000 {
		doc.Keys[fmt.Sprintf("k%05d", i)] = domain.Key{Models: []string{fmt.Sprintf("a%04d", i%1000)}}
	}
	doc.Keys["k00000"] = domain.Key{Models: []string{"a0000"}, Budget: &domain.Budget{LimitUSD: new(1.0), ResetInterval: domain.ResetMonthly}}
	return doc
}

func TestValidateScale18(t *testing.T) {
	doc := scale18()
	got := summary(Validate(&doc))
	want := []string{"error USE-12 unpriced_under_budget targets/p/m0000 pricing"}
	for i := 1; i < 1000; i++ {
		want = append(want, fmt.Sprintf("warning USE-12 unpriced targets/p/m%04d pricing", i))
	}
	if !slices.Equal(got, want) {
		t.Errorf("%d findings, want %d: %q", len(got), len(want), got[:min(len(got), 5)])
	}
}

// Kick-off section 18 gives a configuration change 1 s to publish, and
// Validate runs on every change.
func BenchmarkValidateScale18(b *testing.B) {
	doc := scale18()
	for b.Loop() {
		Validate(&doc)
	}
}
