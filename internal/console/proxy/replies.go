package proxy

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
	Name       string `json:"name"`
	Credential string `json:"credential"`
}

// ModelsReply is "models list --json".
type ModelsReply struct {
	Models []Model `json:"models"`
}

// Required names the members the answer must have.
func (*ModelsReply) Required() []string { return []string{"models"} }

// Model is an alias: its targets in order, primary first, and its budget.
type Model struct {
	Name    string   `json:"name"`
	Targets []string `json:"targets"`
	Budget  *Budget  `json:"budget"`
}

// TargetsReply is "targets list --json".
type TargetsReply struct {
	Targets []Target `json:"targets"`
}

// Required names the members the answer must have.
func (*TargetsReply) Required() []string { return []string{"targets"} }

// Target is a provider's model, "provider/upstream-model".
type Target struct {
	ID string `json:"id"`
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
	Budget *Budget `json:"budget"`
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
