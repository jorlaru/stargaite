package domain

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The values each enumerated field accepts.
const (
	ProtocolOpenAI    = "openai"
	ProtocolAnthropic = "anthropic"

	BillingPayPerUse    = "pay_per_use"
	BillingSubscription = "subscription"
	BillingNone         = "none"

	StreamUsageRequest = "request"
	StreamUsageNative  = "native"
	StreamUsageNone    = "none"

	KindChat       = "chat"
	KindEmbeddings = "embeddings"
	KindRerank     = "rerank"

	// What an alias may list in requires: the members of Capabilities.
	CapabilityTools            = "tools"
	CapabilityStructuredOutput = "structured_output"
	CapabilityStrictSchema     = "strict_schema"
	CapabilityVision           = "vision"

	ModeDefault = "default" // the client's effort wins when present
	ModeForced  = "forced"  // the alias's effort always wins

	ParamForward = "forward"
	ParamDrop    = "drop"

	ResetDaily   = "daily"
	ResetMonthly = "monthly"
	ResetNever   = "never"

	DialectNone            = "none"
	DialectReasoningEffort = "reasoning_effort"
	DialectReasoningObject = "reasoning_object"
	DialectThinkingToggle  = "thinking_toggle"
	DialectEnableThinking  = "enable_thinking"
	DialectThinkingBudget  = "thinking_budget"
	DialectAdaptiveEffort  = "adaptive_effort"
)

// Levels is the canonical scale of reasoning effort, lowest first. Translation
// to a target measures distance by position in it (RSN-4).
var Levels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

const (
	// LevelOn is the single level an on/off dialect declares (RSN-3).
	LevelOn = "on"
	// EffortProviderDefault is the alias effort that explicitly sends no
	// reasoning control (RSN-5); it is not a level of the scale.
	EffortProviderDefault = "provider_default"
)

// DialectKeys lists, for every reasoning dialect, the top-level keys of the
// outgoing body that its default forms write (kick-off section 9.1). A forward
// rule may not name one of them (PAR-2). A target's own on and off fragments
// replace the forms, not this table.
var DialectKeys = map[string][]string{
	DialectNone:            nil,
	DialectReasoningEffort: {"reasoning_effort"},
	DialectReasoningObject: {"reasoning"},
	DialectThinkingToggle:  {"thinking"},
	DialectEnableThinking:  {"enable_thinking"},
	DialectThinkingBudget:  {"thinking"},
	DialectAdaptiveEffort:  {"thinking", "output_config"},
}

// Defaults: the value a field takes when its declaration omits it. They are
// applied when a configuration is published, never when it is decoded or
// stored, so export shows declared values and a release that changes a default
// changes it for running instances.
const (
	DefaultMaxBodyBytes     = 10 << 20 // LIM-6
	DefaultMaxResponseBytes = 32 << 20 // the kick-off states none; the value of its annex B bounds the read of RES-4

	// Timeouts, in milliseconds (RES-3).
	DefaultConnectMS    = 5_000
	DefaultFirstByteMS  = 30_000
	DefaultStreamIdleMS = 120_000
	DefaultTotalMS      = 180_000

	// Retention (USE-11).
	DefaultUsageDays        = 90
	DefaultUsageDailyMonths = 36
	DefaultAuditMonths      = 24

	DefaultTrustTraceparent = true     // TEL-6
	DefaultContentMaxBytes  = 32 << 10 // TEL-8

	DefaultKeyMaxParallelRequests = 5 // LIM-7

	DefaultOutputCapParam = "max_tokens" // PAR-6
	DefaultDialect        = DialectNone  // RSN-2: a target that declares no dialect sends no reasoning control
)

var nameSyntax = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidName reports whether s can name a provider, a model alias or a key.
func ValidName(s string) bool { return nameSyntax.MatchString(s) }

// SplitTarget splits a target id, "provider/upstream-model", at its first "/".
// ok is false unless the provider is a valid name and the upstream model id is
// non-empty text without spaces or control characters; the two parts are
// returned either way, so a caller can still say which one is wrong.
func SplitTarget(id string) (provider, upstream string, ok bool) {
	provider, upstream, ok = strings.Cut(id, "/")
	ok = ok && ValidName(provider) && upstream != "" && utf8.ValidString(upstream) &&
		!strings.ContainsFunc(upstream, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
	return provider, upstream, ok
}

// MaxQuotaSpan is the longest span a quota window counts over. A window
// declared longer counts over 100 years, which holds every record since 1970
// until 2070.
const MaxQuotaSpan = 100 * 365 * 24 * time.Hour

// QuotaSpan is the span of a quota window such as 5h or 7d (LIM-8): a whole
// number of minutes, hours or days, at most MaxQuotaSpan. ok is false for any
// other text, which validation refuses but a stored configuration may still
// hold.
func QuotaSpan(window string) (span time.Duration, ok bool) {
	if window == "" {
		return 0, false
	}
	unit := map[byte]time.Duration{'m': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour}[window[len(window)-1]]
	n, err := strconv.ParseInt(window[:len(window)-1], 10, 64)
	if unit == 0 || err != nil || n <= 0 {
		return 0, false
	}
	return time.Duration(min(n, int64(MaxQuotaSpan/unit))) * unit, true
}
