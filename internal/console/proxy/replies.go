package proxy

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
)

// The replies hold only the members the console reads; the CLI's answers
// carry more, which decoding ignores.

// VersionReply is "version --json".
type VersionReply struct {
	Version string `json:"version"`
}

// Required names the members the answer must have.
func (*VersionReply) Required() []string { return []string{"version"} }

// ProvidersReply is "providers list --json".
type ProvidersReply struct {
	Providers []Provider `json:"providers"`
}

// Required names the members the answer must have.
func (*ProvidersReply) Required() []string { return []string{"providers"} }

// Provider is a provider and the state of its credential: resolved,
// unresolved or none.
type Provider struct {
	Name                string   `json:"name"`
	Credential          string   `json:"credential"`
	Protocol            string   `json:"protocol"`
	BaseURL             string   `json:"base_url"`
	APIKeyEnv           string   `json:"api_key_env"`
	AllowPrivateNetwork bool     `json:"allow_private_network"`
	Timeouts            Timeouts `json:"timeouts"`
}

// Timeouts is the longest one provider call may take, in ms; 0 is
// undeclared.
type Timeouts struct {
	TotalMS int64 `json:"total_ms"`
}

// ProviderReply is "providers show --json": the provider's members at the
// top level.
type ProviderReply struct {
	Provider
}

// Required names the members the answer must have.
func (*ProviderReply) Required() []string { return []string{"name", "credential"} }

// ProviderChange is "providers add --json": the findings of the whole
// configuration after the change, and the provider as published.
type ProviderChange struct {
	Findings []Finding `json:"findings"`
	Resource Provider  `json:"resource"`
}

// Required names the members the answer must have.
func (*ProviderChange) Required() []string { return []string{"findings", "resource"} }

// Finding is one warning of the configuration, or an error of a refusal.
type Finding struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Resource string `json:"resource"`
	Message  string `json:"message"`
}

// PresetsReply is "providers presets --json".
type PresetsReply struct {
	Presets []Preset `json:"presets"`
}

// Required names the members the answer must have.
func (*PresetsReply) Required() []string { return []string{"presets"} }

// Preset is a provider the proxy knows how to reach; untested ones were
// never tried against the live service.
type Preset struct {
	Name      string `json:"name"`
	Untested  bool   `json:"untested"`
	Protocol  string `json:"protocol"`
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env"`
}

// VerifyReply is "targets verify --json": whether every call answered 2xx,
// whether the result was stored, and each call.
type VerifyReply struct {
	Target string       `json:"target"`
	OK     bool         `json:"ok"`
	Stored bool         `json:"stored"`
	Calls  []VerifyCall `json:"calls"`
}

// Required names the members the answer must have.
func (*VerifyReply) Required() []string { return []string{"target", "ok", "calls"} }

// VerifyCall is one call of a check: the level asked ("" for none), the
// provider's status and, when it refused, its reason or the proxy's error.
type VerifyCall struct {
	Effort          string `json:"effort"`
	Status          int    `json:"status"`
	ProviderMessage string `json:"provider_message"`
	Error           *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ModelsReply is "models list --json".
type ModelsReply struct {
	Models []Model `json:"models"`
}

// Required names the members the answer must have.
func (*ModelsReply) Required() []string { return []string{"models"} }

// Model is an alias: its targets in order, primary first, its budget, its
// output cap (0 for none) and its default reasoning effort ("" for none).
type Model struct {
	Name            string   `json:"name"`
	Targets         []string `json:"targets"`
	Budget          *Budget  `json:"budget"`
	MaxOutputTokens int64    `json:"max_output_tokens"`
	Reasoning       struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
}

// TargetsReply is "targets list --json".
type TargetsReply struct {
	Targets []Target `json:"targets"`
}

// Required names the members the answer must have.
func (*TargetsReply) Required() []string { return []string{"targets"} }

// Target is a provider's model, "provider/upstream-model": its reasoning,
// when declared, and when its check last passed (Unix ms, 0 for never or
// since it changed).
type Target struct {
	ID         string     `json:"id"`
	Reasoning  *Reasoning `json:"reasoning"`
	VerifiedAt int64      `json:"verified_at"`
}

// Reasoning is the levels a target accepts and whether it can stop
// reasoning, each as declared.
type Reasoning struct {
	Levels     []string `json:"levels"`
	CanDisable *bool    `json:"can_disable"`
}

// Change is the answer of an accepted change: the findings of the whole
// configuration after it.
type Change struct {
	Findings []Finding `json:"findings"`
}

// Required names the members the answer must have.
func (*Change) Required() []string { return []string{"findings"} }

// TargetChange is "targets add --json": the findings and the target as
// published.
type TargetChange struct {
	Findings []Finding `json:"findings"`
	Resource Target    `json:"resource"`
}

// Required names the members the answer must have.
func (*TargetChange) Required() []string { return []string{"findings", "resource"} }

// ExplainReply is "models explain --json": what the alias would send toward
// each target.
type ExplainReply struct {
	Targets []Explained `json:"targets"`
}

// Required names the members the answer must have.
func (*ExplainReply) Required() []string { return []string{"targets"} }

// Explained is one target of an explain: skipped with its code, or the
// effort and output cap it would be sent (nil for none).
type Explained struct {
	Target  string `json:"target"`
	Skipped *struct {
		Code string `json:"code"`
	} `json:"skipped"`
	Effort struct {
		Sent *string `json:"sent"`
	} `json:"effort"`
	MaxOutput struct {
		Sent *int64 `json:"sent"`
	} `json:"max_output"`
}

// AliasDecl is an alias's whole declaration as "models show --json" gives it
// (its name and its members), kept as raw members so that nothing the
// console does not know is lost when it is sent back.
type AliasDecl map[string]jsontext.Value

// Required names the members the answer must have.
func (AliasDecl) Required() []string { return []string{"name", "targets"} }

// With is a copy of d with member set to v's JSON. v is the console's own
// value (ids, a number, a small struct), which always encodes.
func (d AliasDecl) With(member string, v any) AliasDecl {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("proxy: AliasDecl member %s does not encode: %v", member, err))
	}
	out := maps.Clone(d)
	if out == nil {
		out = AliasDecl{}
	}
	out[member] = data
	return out
}

// NewTarget is a chat target a change would add; a nil Pricing is a target
// without prices.
type NewTarget struct {
	ID      string
	Pricing *Pricing
}

// Pricing is a target's prices in USD per million tokens, as of a date.
type Pricing struct {
	In   float64 `json:"input_per_million"`
	Out  float64 `json:"output_per_million"`
	AsOf string  `json:"as_of"`
}

// KeysReply is "keys list --json".
type KeysReply struct {
	Keys []Key `json:"keys"`
}

// Required names the members the answer must have.
func (*KeysReply) Required() []string { return []string{"keys"} }

// Key is a client key: the aliases it may call (["*"] for every one) and
// its budget.
type Key struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
	Budget *Budget  `json:"budget"`
}

// SettingsReply is "settings show --json". timeouts is always in the answer;
// budget only when spend is capped.
type SettingsReply struct {
	Budget   *Budget  `json:"budget"`
	Timeouts Timeouts `json:"timeouts"`
}

// Required names the members the answer must have.
func (*SettingsReply) Required() []string { return []string{"timeouts"} }

// Budget is a spend limit and its reset interval, which may be empty.
type Budget struct {
	LimitUSD      float64 `json:"limit_usd"`
	ResetInterval string  `json:"reset_interval"`
}

// TelemetryReply is "telemetry status --json".
type TelemetryReply struct {
	Telemetry  *TelemetryBlock `json:"telemetry"`
	Unresolved []string        `json:"unresolved"`
}

// Required names the members the answer must have.
func (*TelemetryReply) Required() []string { return []string{"telemetry"} }

// TelemetryBlock is where traces go and the variables that hold their
// credentials, never the credentials.
type TelemetryBlock struct {
	OTLPEndpoint string            `json:"otlp_endpoint"`
	BasicAuthEnv []string          `json:"basic_auth_env"`
	HeaderEnv    map[string]string `json:"header_env"`
}
