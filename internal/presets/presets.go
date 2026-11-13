// Package presets is the built-in catalog of providers (kick-off section 7.2,
// ADR-019). A preset is data copied into a provider when it is created;
// afterwards only the stored declaration counts.
//
// The catalog holds only what kick-off section 7.2 and annex B state. Hosted
// presets carry billing pay_per_use, and everything else stays undeclared
// until the data is completed from each provider's public documentation
// (phase-1 design section 13, point 24; phase-2 design section 15, point 19):
// deepinfra declares no cost report, because none was found in its documentation.
// No preset sets allow_private_network.
package presets

import "github.com/jorlaru/stargaite/internal/domain"

// Preset is one entry of the catalog. Untested marks a preset no maintainer
// has run the contract suite against yet; it is catalog metadata, shown by
// "providers presets" and never copied into a provider.
type Preset struct {
	Name     string `json:"name"`
	Untested bool   `json:"untested,omitzero"`
	domain.Provider
}

// Catalog is the catalog, in the order of kick-off section 7.2.
var Catalog = []Preset{
	{Name: "openai", Provider: domain.Provider{
		Protocol: domain.ProtocolOpenAI, BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY", Billing: domain.BillingPayPerUse,
		TargetDefaults: domain.TargetDefaults{Dialect: domain.DialectReasoningEffort, OutputCapParam: "max_completion_tokens"},
	}},
	{Name: "anthropic", Provider: domain.Provider{ // annex B
		Protocol: domain.ProtocolAnthropic, BaseURL: "https://api.anthropic.com", APIKeyEnv: "ANTHROPIC_API_KEY", Billing: domain.BillingPayPerUse,
		GenAIProvider: "anthropic", TargetDefaults: domain.TargetDefaults{Dialect: domain.DialectAdaptiveEffort},
	}},
	{Name: "openrouter", Provider: domain.Provider{ // annex B; OpenRouter states usage.cost in credits, kept at 1 USD per unit
		Protocol: domain.ProtocolOpenAI, BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY", Billing: domain.BillingPayPerUse,
		CostReport: &domain.CostReport{Path: "usage.cost", USDPerUnit: 1}, StreamUsage: domain.StreamUsageNative, GenAIProvider: "openrouter",
		TargetDefaults: domain.TargetDefaults{Dialect: domain.DialectReasoningObject, OutputCapParam: "max_tokens"},
	}},
	{Name: "deepinfra", Provider: domain.Provider{Protocol: domain.ProtocolOpenAI, BaseURL: "https://api.deepinfra.com/v1/openai", APIKeyEnv: "DEEPINFRA_API_KEY", Billing: domain.BillingPayPerUse}},
	{Name: "minimax", Provider: domain.Provider{
		Protocol: domain.ProtocolOpenAI, BaseURL: "https://api.minimax.io/v1", APIKeyEnv: "MINIMAX_API_KEY", Billing: domain.BillingPayPerUse,
		TargetDefaults: domain.TargetDefaults{OutputCapParam: "max_completion_tokens"},
	}},
	{Name: "zai", Provider: domain.Provider{Protocol: domain.ProtocolOpenAI, BaseURL: "https://api.z.ai/api/paas/v4", APIKeyEnv: "ZAI_API_KEY", Billing: domain.BillingPayPerUse}},
	{Name: "moonshot", Provider: domain.Provider{Protocol: domain.ProtocolOpenAI, BaseURL: "https://api.moonshot.ai/v1", APIKeyEnv: "MOONSHOT_API_KEY", Billing: domain.BillingPayPerUse}},
	{Name: "dashscope", Provider: domain.Provider{ // the URL depends on the region and the workspace: --base-url
		Protocol: domain.ProtocolOpenAI, APIKeyEnv: "DASHSCOPE_API_KEY", Billing: domain.BillingPayPerUse,
		TargetDefaults: domain.TargetDefaults{OutputCapParam: "max_completion_tokens"},
	}},
	{Name: "ollama", Provider: domain.Provider{ // plain HTTP on loopback: --allow-private-network
		Protocol: domain.ProtocolOpenAI, BaseURL: "http://localhost:11434/v1", Billing: domain.BillingNone,
	}},
	{Name: "tei", Provider: domain.Provider{ // one instance per model: --base-url
		Protocol: domain.ProtocolOpenAI, Billing: domain.BillingNone,
	}},
	{Name: "deepseek", Untested: true, Provider: domain.Provider{Protocol: domain.ProtocolOpenAI, BaseURL: "https://api.deepseek.com", APIKeyEnv: "DEEPSEEK_API_KEY", Billing: domain.BillingPayPerUse}},
	{Name: "gemini", Untested: true, Provider: domain.Provider{Protocol: domain.ProtocolOpenAI, BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", APIKeyEnv: "GEMINI_API_KEY", Billing: domain.BillingPayPerUse}},
	{Name: "xai", Untested: true, Provider: domain.Provider{ // cost in ticks of 1e-10 USD; in streams only with include_usage
		Protocol: domain.ProtocolOpenAI, BaseURL: "https://api.x.ai/v1", APIKeyEnv: "XAI_API_KEY", Billing: domain.BillingPayPerUse,
		CostReport: &domain.CostReport{Path: "usage.cost_in_usd_ticks", USDPerUnit: 1e-10}, StreamUsage: domain.StreamUsageRequest,
	}},
	{Name: "mistral", Untested: true, Provider: domain.Provider{Protocol: domain.ProtocolOpenAI, BaseURL: "https://api.mistral.ai/v1", APIKeyEnv: "MISTRAL_API_KEY", Billing: domain.BillingPayPerUse}},
	{Name: "groq", Untested: true, Provider: domain.Provider{
		Protocol: domain.ProtocolOpenAI, BaseURL: "https://api.groq.com/openai/v1", APIKeyEnv: "GROQ_API_KEY", Billing: domain.BillingPayPerUse,
		TargetDefaults: domain.TargetDefaults{OutputCapParam: "max_completion_tokens"},
	}},
	{Name: "together", Untested: true, Provider: domain.Provider{Protocol: domain.ProtocolOpenAI, BaseURL: "https://api.together.ai/v1", APIKeyEnv: "TOGETHER_API_KEY", Billing: domain.BillingPayPerUse}},
	{Name: "fireworks", Untested: true, Provider: domain.Provider{Protocol: domain.ProtocolOpenAI, BaseURL: "https://api.fireworks.ai/inference/v1", APIKeyEnv: "FIREWORKS_API_KEY", Billing: domain.BillingPayPerUse}},
	{Name: "cerebras", Untested: true, Provider: domain.Provider{
		Protocol: domain.ProtocolOpenAI, BaseURL: "https://api.cerebras.ai/v1", APIKeyEnv: "CEREBRAS_API_KEY", Billing: domain.BillingPayPerUse,
		TargetDefaults: domain.TargetDefaults{OutputCapParam: "max_completion_tokens"},
	}},
}
