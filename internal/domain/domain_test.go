package domain

import (
	"bytes"
	"cmp"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"maps"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// annexB is annex B of the kick-off as a Go literal. testdata/annex-b.json is
// the same annex extracted from the kick-off by a YAML parser, so the two are
// independent transcriptions.
var annexB = Document{
	Version: 1,
	Settings: Settings{
		Budget:              &Budget{LimitUSD: new(2000.0), ResetInterval: ResetMonthly},
		MaxParallelRequests: 500,
		MaxBodyBytes:        10485760,
		MaxResponseBytes:    33554432,
		Timeouts:            &Timeouts{ConnectMS: 5000, FirstByteMS: 30000, StreamIdleMS: 120000, TotalMS: 180000},
		Retention:           &Retention{UsageDays: 90, UsageDailyMonths: 36, AuditMonths: 24},
		Telemetry: &Telemetry{
			OTLPEndpoint:     "https://langfuse.example.com/api/public/otel",
			BasicAuthEnv:     []string{"LANGFUSE_PUBLIC_KEY", "LANGFUSE_SECRET_KEY"},
			Headers:          map[string]string{"x-langfuse-ingestion-version": "4"},
			TrustTraceparent: new(true),
			ContentMaxBytes:  32768,
		},
	},
	Providers: map[string]Provider{
		"openrouter": {
			Protocol:       ProtocolOpenAI,
			BaseURL:        "https://openrouter.ai/api/v1",
			APIKeyEnv:      "OPENROUTER_API_KEY",
			Billing:        BillingPayPerUse,
			CostReport:     &CostReport{Path: "usage.cost", USDPerUnit: 1},
			StreamUsage:    StreamUsageNative,
			GenAIProvider:  "openrouter",
			TargetDefaults: TargetDefaults{Dialect: DialectReasoningObject, OutputCapParam: "max_tokens"},
		},
		"anthropic": {
			Protocol:       ProtocolAnthropic,
			BaseURL:        "https://api.anthropic.com",
			APIKeyEnv:      "ANTHROPIC_API_KEY",
			Billing:        BillingPayPerUse,
			GenAIProvider:  "anthropic",
			TargetDefaults: TargetDefaults{Dialect: DialectAdaptiveEffort},
		},
		"minimax-plan": {
			Protocol:  ProtocolAnthropic,
			BaseURL:   "https://api.minimax.io/anthropic",
			APIKeyEnv: "MINIMAX_PLAN_API_KEY",
			Billing:   BillingSubscription,
			Quota: []QuotaWindow{
				{Window: "5h", Requests: 1500},
				{Window: "7d", Requests: 15000},
			},
			TargetDefaults: TargetDefaults{Dialect: DialectAdaptiveEffort},
		},
		"tei-embed": {
			Protocol:            ProtocolOpenAI,
			BaseURL:             "http://localhost:8080/v1",
			Billing:             BillingNone,
			AllowPrivateNetwork: true,
		},
		"tei-rerank": {
			Protocol:            ProtocolOpenAI,
			BaseURL:             "http://localhost:8081/v1",
			RerankURL:           "http://localhost:8081/rerank",
			Billing:             BillingNone,
			AllowPrivateNetwork: true,
		},
	},
	Targets: map[string]Target{
		"minimax-plan/MiniMax-M3.1-Flash": {
			Kind:            KindChat,
			Capabilities:    Capabilities{Tools: new(true), StructuredOutput: new(true)},
			MaxOutputTokens: 64000,
			Reasoning: &Reasoning{
				Dialect:    DialectAdaptiveEffort,
				Levels:     []string{"low", "medium", "high", "xhigh", "max"},
				CanDisable: new(false),
			},
		},
		"openrouter/qwen/qwen-3.6-flash": {
			Kind:            KindChat,
			Capabilities:    Capabilities{Tools: new(true), StructuredOutput: new(true), StrictSchema: new(true)},
			MaxOutputTokens: 32000,
			Reasoning: &Reasoning{
				Dialect:    DialectReasoningObject,
				Levels:     []string{"minimal", "low", "medium", "high"},
				CanDisable: new(true),
			},
			Pricing: &Pricing{
				InputPerMillion:     new(0.15),
				OutputPerMillion:    new(0.60),
				CacheReadPerMillion: new(0.05),
				AsOf:                "2026-11-01",
			},
		},
		"anthropic/claude-sonnet-5-5": {
			Kind: KindChat,
			Capabilities: Capabilities{
				Tools: new(true), StructuredOutput: new(true), StrictSchema: new(true), Vision: new(true),
			},
			MaxOutputTokens: 64000,
			Reasoning: &Reasoning{
				Dialect:    DialectAdaptiveEffort,
				Levels:     []string{"low", "medium", "high", "xhigh", "max"},
				CanDisable: new(false),
			},
			Pricing: &Pricing{
				InputPerMillion:      new(3.00),
				OutputPerMillion:     new(15.00),
				CacheReadPerMillion:  new(0.30),
				CacheWritePerMillion: new(3.75),
				AsOf:                 "2026-11-01",
			},
		},
		"tei-embed/bge-m3":              {Kind: KindEmbeddings},
		"tei-rerank/bge-reranker-v2-m3": {Kind: KindRerank},
	},
	Models: map[string]Model{
		"fast": {
			Targets:         []string{"minimax-plan/MiniMax-M3.1-Flash", "openrouter/qwen/qwen-3.6-flash"},
			Requires:        []string{CapabilityTools, CapabilityStructuredOutput},
			Reasoning:       ModelReasoning{Effort: "medium", Mode: ModeDefault},
			MaxOutputTokens: 32000,
			Params:          map[string]string{"enable_search": ParamDrop},
		},
		"deep": {
			Targets:         []string{"anthropic/claude-sonnet-5-5"},
			Requires:        []string{CapabilityTools, CapabilityStructuredOutput, CapabilityStrictSchema},
			Reasoning:       ModelReasoning{Effort: "high", Mode: ModeForced},
			MaxOutputTokens: 32000,
			Budget:          &Budget{LimitUSD: new(500.0), ResetInterval: ResetMonthly},
		},
		"embed": {
			Targets:         []string{"tei-embed/bge-m3"},
			TraceSampleRate: new(0.0),
		},
		"rerank": {
			Targets:         []string{"tei-rerank/bge-reranker-v2-m3"},
			TraceSampleRate: new(0.0),
		},
	},
	Keys: map[string]Key{
		"invoice-agent": {
			Models:              []string{"fast", "deep"},
			Budget:              &Budget{LimitUSD: new(100.0), ResetInterval: ResetMonthly},
			RPMLimit:            60,
			MaxParallelRequests: 5,
			QuotaShare:          map[string]float64{"minimax-plan": 0.4},
		},
		"classifier-flow": {
			Models:              []string{"fast", "embed", "rerank"},
			Budget:              &Budget{LimitUSD: new(30.0), ResetInterval: ResetMonthly},
			MaxParallelRequests: 10,
			TraceSampleRate:     new(0.1),
		},
	},
}

// decodeStrict fails on a member no field takes, so a field missing from a
// declaration type cannot pass unnoticed.
func decodeStrict(t *testing.T, data []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestAnnexBRoundTrip(t *testing.T) {
	file, err := os.ReadFile("testdata/annex-b.json")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(annexB)
	if err != nil {
		t.Fatal(err)
	}

	var fromFile Document
	decodeStrict(t, file, &fromFile)
	if !reflect.DeepEqual(fromFile, annexB) {
		got, _ := json.Marshal(fromFile)
		t.Errorf("the literal is not annex B\nfile:    %s\nliteral: %s", got, encoded)
	}

	var back Document
	decodeStrict(t, encoded, &back)
	if !reflect.DeepEqual(back, annexB) {
		got, _ := json.Marshal(back)
		t.Errorf("round trip changed the document\n got: %s\nwant: %s", got, encoded)
	}

	// Compared member by member, the encoding holds exactly what the annex
	// holds: nothing lost, nothing added by a default.
	var want, got any
	if err := json.Unmarshal(file, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the encoding differs from annex B\n got: %s", encoded)
	}
}

// The stored value is the declared value: decoding then encoding gives the
// input back, under the standard codec and under json/v2 alike.
func TestDeclaredValueSurvives(t *testing.T) {
	tests := []struct {
		name string
		zero any
		in   string
		want string // empty: the same as in
	}{
		{name: "zero provider", zero: Provider{}, in: `{}`},
		{name: "zero target", zero: Target{}, in: `{}`},
		{name: "zero model", zero: Model{}, in: `{}`},
		{name: "zero key", zero: Key{}, in: `{}`},
		{name: "zero settings", zero: Settings{}, in: `{}`},
		{name: "document without resources", zero: Document{}, in: `{"version":1}`},

		{name: "capability false is not undeclared", zero: Target{}, in: `{"capabilities":{"tools":false,"vision":true}}`},
		{name: "can_disable false is not undeclared", zero: Target{}, in: `{"reasoning":{"can_disable":false}}`},
		{name: "levels omitted when empty", zero: Target{}, in: `{"reasoning":{"levels":[]}}`, want: `{"reasoning":{}}`},
		{name: "empty off fragment is a declaration", zero: Target{}, in: `{"reasoning":{"off":{}}}`},
		{name: "fragment values keep their bytes", zero: Target{}, in: `{"reasoning":{"on":{"thinking":{"budget_tokens":12345678901234567890,"type":"enabled"}}}}`},
		{name: "price of zero is a price", zero: Target{}, in: `{"pricing":{"input_per_million":0,"output_per_million":0}}`},
		{name: "sample rate of zero turns tracing off", zero: Model{}, in: `{"trace_sample_rate":0}`},
		{name: "empty models list is not an absent one", zero: Key{}, in: `{"models":[]}`},
		{name: "trust_traceparent false", zero: Settings{}, in: `{"telemetry":{"trust_traceparent":false}}`},
		{name: "budget of zero", zero: Settings{}, in: `{"budget":{"limit_usd":0,"reset_interval":"never"}}`},
		{name: "a budget always shows its limit", zero: Model{}, in: `{"budget":{}}`, want: `{"budget":{"limit_usd":null}}`},

		// The fields annex B does not use.
		{name: "provider, rest", zero: Provider{}, in: `{"quota":[{"window":"7d","tokens":1000000}]}`},
		{name: "target, rest", zero: Target{}, in: `{"output_cap_param":"max_completion_tokens",` +
			`"reasoning":{"dialect":"thinking_budget","levels":["low","high"],"budgets":{"high":16000,"low":2000},` +
			`"on":{"thinking":{"type":"enabled"}},"off":{"thinking":{"type":"disabled"}},"map":{"max":"high"}},` +
			`"unsupported_params":["logprobs"],"response_models":["upstream-2026"],"inline_think_tags":true}`},
		{name: "model, rest", zero: Model{}, in: `{"rpm_limit":60,"tpm_limit":100000,"max_parallel_requests":4,"trace_sample_rate":0.5}`},
		{name: "key, rest", zero: Key{}, in: `{"models":["*"],"tpm_limit":100000,"content_until":"2026-11-13T10:00:00Z"}`},
		{name: "telemetry, rest", zero: Settings{}, in: `{"telemetry":{"header_env":{"x-token":"OTLP_TOKEN"},"allow_private_network":true}}`},
	}
	codecs := []struct {
		name      string
		unmarshal func([]byte, any) error
		marshal   func(any) ([]byte, error)
	}{
		{"v1", json.Unmarshal, json.Marshal},
		{
			"v2",
			func(b []byte, v any) error { return jsonv2.Unmarshal(b, v) },
			// Without the option, v2 writes map entries in no fixed order.
			func(v any) ([]byte, error) { return jsonv2.Marshal(v, jsonv2.Deterministic(true)) },
		},
	}
	for _, tt := range tests {
		for _, codec := range codecs {
			t.Run(tt.name+"/"+codec.name, func(t *testing.T) {
				v := reflect.New(reflect.TypeOf(tt.zero)).Interface()
				if err := codec.unmarshal([]byte(tt.in), v); err != nil {
					t.Fatalf("decode: %v", err)
				}
				got, err := codec.marshal(v)
				if err != nil {
					t.Fatalf("encode: %v", err)
				}
				want := cmp.Or(tt.want, tt.in)
				if string(got) != want {
					t.Errorf("got %s, want %s", got, want)
				}
			})
		}
	}
}

// The forms are those of the dialect table of kick-off section 9.1, for a
// level and for "none". DialectKeys must hold their top-level keys, no more.
func TestDialectKeys(t *testing.T) {
	forms := []struct{ dialect, level, none string }{
		{"none", `{}`, `{}`},
		{"reasoning_effort", `{"reasoning_effort": "high"}`, `{"reasoning_effort": "none"}`},
		{"reasoning_object", `{"reasoning": {"effort": "high"}}`, `{"reasoning": {"effort": "none"}}`},
		{"thinking_toggle", `{"thinking": {"type": "enabled"}}`, `{"thinking": {"type": "disabled"}}`},
		{"enable_thinking", `{"enable_thinking": true}`, `{"enable_thinking": false}`},
		{"thinking_budget", `{"thinking": {"type": "enabled", "budget_tokens": 1024}}`, `{"thinking": {"type": "disabled"}}`},
		{"adaptive_effort", `{"thinking": {"type": "adaptive"}, "output_config": {"effort": "high"}}`, `{"thinking": {"type": "disabled"}}`},
	}
	if len(DialectKeys) != len(forms) {
		t.Errorf("DialectKeys has %d dialects, the kick-off has %d", len(DialectKeys), len(forms))
	}
	for _, f := range forms {
		t.Run(f.dialect, func(t *testing.T) {
			got, ok := DialectKeys[f.dialect]
			if !ok {
				t.Fatal("dialect missing from DialectKeys")
			}
			written := map[string]json.RawMessage{}
			for _, form := range []string{f.level, f.none} {
				if err := json.Unmarshal([]byte(form), &written); err != nil {
					t.Fatal(err)
				}
			}
			want := slices.Sorted(maps.Keys(written))
			if !slices.Equal(slices.Sorted(slices.Values(got)), want) {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}

func TestLevels(t *testing.T) {
	want := []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
	if !slices.Equal(Levels, want) {
		t.Errorf("got %v, want %v", Levels, want)
	}
	for _, notALevel := range []string{LevelOn, EffortProviderDefault, "default", ""} {
		if slices.Contains(Levels, notALevel) {
			t.Errorf("%q is in the scale", notALevel)
		}
	}
}

// Each default is written here in the unit the kick-off states it in.
func TestDefaults(t *testing.T) {
	numbers := []struct {
		name      string
		got, want int64
	}{
		{"LIM-6 max_body_bytes", DefaultMaxBodyBytes, 10 * 1024 * 1024},
		{"annex B max_response_bytes", DefaultMaxResponseBytes, annexB.Settings.MaxResponseBytes},
		{"RES-3 connect", DefaultConnectMS, (5 * time.Second).Milliseconds()},
		{"RES-3 first byte", DefaultFirstByteMS, (30 * time.Second).Milliseconds()},
		{"RES-3 stream idle", DefaultStreamIdleMS, (120 * time.Second).Milliseconds()},
		{"RES-3 total", DefaultTotalMS, (180 * time.Second).Milliseconds()},
		{"USE-11 usage records", DefaultUsageDays, 90},
		{"USE-11 daily aggregates", DefaultUsageDailyMonths, 36},
		{"USE-11 audit", DefaultAuditMonths, 24},
		{"TEL-8 content_max_bytes", DefaultContentMaxBytes, 32 * 1024},
		{"LIM-7 key max_parallel_requests", DefaultKeyMaxParallelRequests, 5},
	}
	for _, tt := range numbers {
		if tt.got != tt.want {
			t.Errorf("%s: got %d, want %d", tt.name, tt.got, tt.want)
		}
	}
	if !DefaultTrustTraceparent {
		t.Error("TEL-6: a valid traceparent is honoured unless the setting says false")
	}
	if DefaultOutputCapParam != "max_tokens" {
		t.Errorf("PAR-6: got %q", DefaultOutputCapParam)
	}
	if DefaultDialect != "none" {
		t.Errorf("RSN-2: got %q", DefaultDialect)
	}
}

func TestValidName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"a", true},
		{"9", true},
		{"openrouter", true},
		{"minimax-plan", true},
		{"A.b_c-9", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"", false},
		{"_verify", false}, // RSN-9: reserved, and outside the syntax
		{"-a", false},
		{".a", false},
		{"a b", false},
		{"a/b", false},
		{"a\n", false},
		{"\na", false},
		{"modèle", false},
		{"*", false},
	}
	for _, tt := range tests {
		if got := ValidName(tt.name); got != tt.want {
			t.Errorf("ValidName(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSplitTarget(t *testing.T) {
	tests := []struct {
		id, provider, upstream string
		ok                     bool
	}{
		{"openrouter/qwen/qwen-3.6-flash", "openrouter", "qwen/qwen-3.6-flash", true},
		{"tei-embed/bge-m3", "tei-embed", "bge-m3", true},
		{"p/accounts/x/models/y:free@v1", "p", "accounts/x/models/y:free@v1", true},
		{"p/模型", "p", "模型", true},
		{"no-slash", "no-slash", "", false},
		{"", "", "", false},
		{"/model", "", "model", false},
		{"p/", "p", "", false},
		{"bad name/model", "bad name", "model", false},
		{"_p/model", "_p", "model", false},
		{"p/a b", "p", "a b", false},
		{"p/a\tb", "p", "a\tb", false},
		{"p/a\u00a0b", "p", "a\u00a0b", false},
		{"p/a\x00", "p", "a\x00", false},
		{"p/a\u0085", "p", "a\u0085", false},
		{"p/a\xff", "p", "a\xff", false},
	}
	for _, tt := range tests {
		provider, upstream, ok := SplitTarget(tt.id)
		if provider != tt.provider || upstream != tt.upstream || ok != tt.ok {
			t.Errorf("SplitTarget(%q) = %q, %q, %v; want %q, %q, %v",
				tt.id, provider, upstream, ok, tt.provider, tt.upstream, tt.ok)
		}
	}
}

// The rows of the error model (kick-off section 15) that phase 1 answers with.
func TestNewError(t *testing.T) {
	tests := []struct {
		code   string
		status int
		typ    string
	}{
		{CodeInvalidRequest, http.StatusBadRequest, "invalid_request_error"},
		{CodeInvalidAPIKey, http.StatusUnauthorized, "authentication_error"},
		{CodeNotFound, http.StatusNotFound, "not_found_error"},
		{CodeConflict, http.StatusConflict, "invalid_request_error"},
		{CodeRequestTooLarge, http.StatusRequestEntityTooLarge, "invalid_request_error"},
		{CodeInternalError, http.StatusInternalServerError, "api_error"},
	}
	for _, tt := range tests {
		var err error = NewError(tt.code, "the message")
		e := err.(*Error)
		if e.Status != tt.status || e.Type != tt.typ || e.Code != tt.code || err.Error() != "the message" {
			t.Errorf("NewError(%q) = %+v, want status %d and type %q", tt.code, e, tt.status, tt.typ)
		}
	}
	if e := NewError("no_such_code", "x"); e.Status != http.StatusInternalServerError || e.Type != "api_error" || e.Code != CodeInternalError {
		t.Errorf("an unknown code must be an internal error, got %+v", e)
	}
}

// The two envelopes are the examples of the kick-off (section 15) and of the
// design (a refused change), byte for byte once compacted.
func TestErrorEnvelope(t *testing.T) {
	refused := NewError(CodeInvalidRequest, "change refused: 1 check failed")
	refused.Findings = []Finding{{
		Severity: SeverityError, Rule: "RSN-7", Code: "effort_required", Resource: "models/fast", Field: "reasoning.effort",
		Message: "target minimax-plan/MiniMax-M3.1-Flash always reasons and the alias sets no effort",
	}}
	tests := []struct {
		name string
		err  *Error
		want string
	}{
		{
			"kick-off example",
			&Error{Status: 400, Message: "Unknown parameter: 'enable_search'.", Type: "invalid_request_error", Param: "enable_search", Code: "unknown_parameter"},
			`{"error":{"message":"Unknown parameter: 'enable_search'.","type":"invalid_request_error","param":"enable_search","code":"unknown_parameter"}}`,
		},
		{
			"refused change",
			refused,
			`{"error":{"message":"change refused: 1 check failed","type":"invalid_request_error","code":"invalid_request",` +
				`"findings":[{"severity":"error","rule":"RSN-7","code":"effort_required","resource":"models/fast",` +
				`"field":"reasoning.effort","message":"target minimax-plan/MiniMax-M3.1-Flash always reasons and the alias sets no effort"}]}}`,
		},
	}
	for _, tt := range tests {
		got, err := json.Marshal(map[string]*Error{"error": tt.err})
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tt.want {
			t.Errorf("%s:\n got %s\nwant %s", tt.name, got, tt.want)
		}
	}
}
