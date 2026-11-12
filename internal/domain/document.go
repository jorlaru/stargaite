// Package domain holds the declarations an operator writes: the configuration
// document and its resources, the findings a check reports and the errors the
// proxy answers with. It decides nothing and imports no other internal package.
//
// A declaration keeps what was declared and nothing else: no field gets a
// default here (see the Default constants), so encoding a decoded declaration
// gives it back. Three conventions make that hold under both encoding/json and
// encoding/json/v2:
//
//   - A number or a boolean whose zero value means "undeclared" is tagged
//     omitzero: json/v2 keeps a zero under omitempty, which the two codecs
//     read alike only for strings, lists and maps.
//   - A field whose zero value is itself a declaration (a capability set to
//     false, a sample rate or a price of 0) is a pointer: nil is undeclared.
//   - A block is a pointer when its presence carries meaning (every member of
//     settings: apply replaces a declared one, even empty) or when it holds
//     lists or maps; nil is undeclared. The other blocks are plain values.
package domain

import (
	"encoding/json"
	"time"
)

// Document is the whole configuration: what export writes and apply reads.
type Document struct {
	Version   int                 `json:"version"`
	Settings  Settings            `json:"settings,omitzero"`
	Providers map[string]Provider `json:"providers,omitempty"`
	Targets   map[string]Target   `json:"targets,omitempty"` // by id, "provider/upstream-model"
	Models    map[string]Model    `json:"models,omitempty"`
	Keys      map[string]Key      `json:"keys,omitempty"`
}

// Settings are the instance-wide values.
type Settings struct {
	Budget              *Budget    `json:"budget,omitzero"`
	MaxParallelRequests int64      `json:"max_parallel_requests,omitzero"`
	MaxBodyBytes        int64      `json:"max_body_bytes,omitzero"`
	MaxResponseBytes    int64      `json:"max_response_bytes,omitzero"`
	Timeouts            *Timeouts  `json:"timeouts,omitzero"`
	Retention           *Retention `json:"retention,omitzero"`
	Telemetry           *Telemetry `json:"telemetry,omitzero"`
}

// Budget is a USD spend limit on the instance, a model alias or a key. A
// declared budget needs a limit (LIM-2), so limit_usd is never omitted: nil is
// a missing limit, which validation refuses, and 0 is a budget that blocks
// every pay-per-use target, not an absent one.
type Budget struct {
	LimitUSD      *float64 `json:"limit_usd"`
	ResetInterval string   `json:"reset_interval,omitempty"`
}

// Timeouts bound a provider call, in milliseconds.
type Timeouts struct {
	ConnectMS    int64 `json:"connect_ms,omitzero"`
	FirstByteMS  int64 `json:"first_byte_ms,omitzero"`
	StreamIdleMS int64 `json:"stream_idle_ms,omitzero"`
	TotalMS      int64 `json:"total_ms,omitzero"`
}

// Retention says how long the store keeps each kind of record.
type Retention struct {
	UsageDays        int64 `json:"usage_days,omitzero"`
	UsageDailyMonths int64 `json:"usage_daily_months,omitzero"`
	AuditMonths      int64 `json:"audit_months,omitzero"`
}

// Telemetry configures the OTLP exporter. Credentials are only ever references:
// BasicAuthEnv names the two variables of a user and a password, HeaderEnv maps
// a header name to the variable that holds its value, and Headers carries
// static values that are not secrets.
type Telemetry struct {
	OTLPEndpoint        string            `json:"otlp_endpoint,omitempty"`
	BasicAuthEnv        []string          `json:"basic_auth_env,omitempty"`
	HeaderEnv           map[string]string `json:"header_env,omitempty"`
	Headers             map[string]string `json:"headers,omitempty"`
	AllowPrivateNetwork bool              `json:"allow_private_network,omitzero"`
	TrustTraceparent    *bool             `json:"trust_traceparent,omitzero"`
	ContentMaxBytes     int64             `json:"content_max_bytes,omitzero"`
}

// Provider is a connection to an inference service.
type Provider struct {
	Protocol            string         `json:"protocol,omitempty"`
	BaseURL             string         `json:"base_url,omitempty"`
	APIKeyEnv           string         `json:"api_key_env,omitempty"`
	Billing             string         `json:"billing,omitempty"`
	Quota               []QuotaWindow  `json:"quota,omitempty"`
	CostReport          *CostReport    `json:"cost_report,omitzero"`
	StreamUsage         string         `json:"stream_usage,omitempty"`
	RerankURL           string         `json:"rerank_url,omitempty"`
	GenAIProvider       string         `json:"gen_ai_provider,omitempty"`
	TargetDefaults      TargetDefaults `json:"target_defaults,omitzero"`
	AllowPrivateNetwork bool           `json:"allow_private_network,omitzero"`
}

// QuotaWindow is one window of a subscription plan ("5h", "7d"), limited in
// requests or in tokens.
type QuotaWindow struct {
	Window   string `json:"window,omitempty"`
	Requests int64  `json:"requests,omitzero"`
	Tokens   int64  `json:"tokens,omitzero"`
}

// CostReport says where a provider's response reports the cost of a request
// and how many USD one unit of that figure is worth.
type CostReport struct {
	Path       string  `json:"path,omitempty"`
	USDPerUnit float64 `json:"usd_per_unit,omitzero"`
}

// TargetDefaults is what a new target of a provider starts with. It is copied
// once, when the target is created; afterwards only the target counts.
type TargetDefaults struct {
	Dialect        string `json:"dialect,omitempty"`
	OutputCapParam string `json:"output_cap_param,omitempty"`
}

// Target is one upstream model at one provider.
type Target struct {
	Kind              string       `json:"kind,omitempty"`
	Capabilities      Capabilities `json:"capabilities,omitzero"`
	MaxOutputTokens   int64        `json:"max_output_tokens,omitzero"` // the model's own limit
	OutputCapParam    string       `json:"output_cap_param,omitempty"`
	Reasoning         *Reasoning   `json:"reasoning,omitzero"`
	Pricing           *Pricing     `json:"pricing,omitzero"`
	UnsupportedParams []string     `json:"unsupported_params,omitempty"`
	ResponseModels    []string     `json:"response_models,omitempty"`
	InlineThinkTags   bool         `json:"inline_think_tags,omitzero"`
}

// Capabilities are the contractual features of a target. Each has three
// states: only false blocks a request, only true satisfies an alias's
// requires, and nil forwards and lets the provider answer.
type Capabilities struct {
	Tools            *bool `json:"tools,omitzero"`
	StructuredOutput *bool `json:"structured_output,omitzero"`
	StrictSchema     *bool `json:"strict_schema,omitzero"`
	Vision           *bool `json:"vision,omitzero"`
}

// Reasoning declares how a target spells the reasoning control (RSN-3).
// Levels and CanDisable may stay undeclared, and they are never guessed.
//
// On and Off replace the dialect's default forms. Each is a fragment of the
// outgoing body, held by top-level key so that a check can read the keys while
// every value keeps its exact bytes. An empty fragment is a declaration
// ("send nothing"), so the two are omitted only when nil.
type Reasoning struct {
	Dialect    string                     `json:"dialect,omitempty"`
	Levels     []string                   `json:"levels,omitempty"`
	CanDisable *bool                      `json:"can_disable,omitzero"`
	Budgets    map[string]int64           `json:"budgets,omitempty"` // tokens per level, for thinking_budget
	On         map[string]json.RawMessage `json:"on,omitzero"`
	Off        map[string]json.RawMessage `json:"off,omitzero"`
	Map        map[string]string          `json:"map,omitempty"` // canonical level -> level sent
}

// Pricing is USD per million tokens. A price of 0 is a declared price, and an
// absent one is not, so each is a pointer. AsOf is a date kept as text.
type Pricing struct {
	InputPerMillion      *float64 `json:"input_per_million,omitzero"`
	OutputPerMillion     *float64 `json:"output_per_million,omitzero"`
	CacheReadPerMillion  *float64 `json:"cache_read_per_million,omitzero"`
	CacheWritePerMillion *float64 `json:"cache_write_per_million,omitzero"`
	AsOf                 string   `json:"as_of,omitempty"`
}

// Model is a model alias: the name clients put in the "model" field. Targets
// holds target ids in order, the primary first and then the fallbacks.
type Model struct {
	Targets             []string          `json:"targets,omitempty"`
	Requires            []string          `json:"requires,omitempty"`
	Reasoning           ModelReasoning    `json:"reasoning,omitzero"`
	MaxOutputTokens     int64             `json:"max_output_tokens,omitzero"` // the alias's output cap
	Params              map[string]string `json:"params,omitempty"`           // parameter name -> rule
	Budget              *Budget           `json:"budget,omitzero"`
	RPMLimit            int64             `json:"rpm_limit,omitzero"`
	TPMLimit            int64             `json:"tpm_limit,omitzero"`
	MaxParallelRequests int64             `json:"max_parallel_requests,omitzero"`
	TraceSampleRate     *float64          `json:"trace_sample_rate,omitzero"` // 0 turns tracing off, nil leaves it alone
}

// ModelReasoning is the effort an alias sets and whether it yields to the
// client's (RSN-5).
type ModelReasoning struct {
	Effort string `json:"effort,omitempty"`
	Mode   string `json:"mode,omitempty"`
}

// Key is a named client credential. Its secrets are not part of the
// declaration. Models is omitted only when nil: an empty list is a declared
// key that reaches nothing, and a key never gets access by omission.
type Key struct {
	Models              []string           `json:"models,omitzero"` // aliases, or ["*"] for all
	Budget              *Budget            `json:"budget,omitzero"`
	RPMLimit            int64              `json:"rpm_limit,omitzero"`
	TPMLimit            int64              `json:"tpm_limit,omitzero"`
	MaxParallelRequests int64              `json:"max_parallel_requests,omitzero"`
	QuotaShare          map[string]float64 `json:"quota_share,omitempty"` // subscription provider -> fraction of each window
	TraceSampleRate     *float64           `json:"trace_sample_rate,omitzero"`
	ContentUntil        time.Time          `json:"content_until,omitzero"` // spans carry content until then (TEL-8)
}
