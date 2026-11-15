package reasoning

import (
	"testing"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/params"
)

// TestResolve is RSN-5: the alias's mode against the client's effort.
func TestResolve(t *testing.T) {
	for _, tt := range []struct {
		client, effort, mode string
		want                 Choice
	}{
		{"low", "high", "default", Choice{"low", "high", "low", SourceClient}},
		{"", "high", "default", Choice{"", "high", "high", SourceAliasDefault}},
		{"low", "high", "forced", Choice{"low", "high", "high", SourceAliasForced}},
		{"", "high", "forced", Choice{"", "high", "high", SourceAliasForced}},
		{"budget:2048", "high", "forced", Choice{"budget:2048", "high", "high", SourceAliasForced}},
		{"low", "provider_default", "default", Choice{"low", "provider_default", "low", SourceClient}},
		{"low", "provider_default", "forced", Choice{"low", "provider_default", "", SourceProviderDefault}},
		{"", "provider_default", "default", Choice{"", "provider_default", "", SourceProviderDefault}},
		{"", "", "", Choice{"", "", "", SourceProviderDefault}},
		{"none", "", "", Choice{"none", "", "none", SourceClient}},
	} {
		if got := Resolve(tt.client, domain.ModelReasoning{Effort: tt.effort, Mode: tt.mode}); got != tt.want {
			t.Errorf("Resolve(%q, %s %s) = %+v, want %+v", tt.client, tt.effort, tt.mode, got, tt.want)
		}
	}
}

// TestTranslate is the RSN-4 order: map, declared level, none and
// can_disable, then the nearest level by position, the lower on a tie.
func TestTranslate(t *testing.T) {
	no := new(false)
	for _, tt := range []struct {
		l, d string
		r    domain.Reasoning
		want string
	}{
		{"medium", "reasoning_effort", domain.Reasoning{Levels: []string{"low", "high"}}, "low"},
		{"high", "reasoning_effort", domain.Reasoning{Levels: []string{"minimal", "xhigh"}}, "xhigh"},
		{"max", "reasoning_effort", domain.Reasoning{Levels: []string{"minimal", "low"}}, "low"},
		{"minimal", "reasoning_effort", domain.Reasoning{Levels: []string{"high", "max"}}, "high"},
		{"medium", "reasoning_effort", domain.Reasoning{Levels: []string{"low", "high"}, Map: map[string]string{"medium": "high"}}, "high"},
		{"none", "reasoning_effort", domain.Reasoning{Levels: []string{"low", "high"}}, "none"},
		{"none", "reasoning_effort", domain.Reasoning{Levels: []string{"low", "high"}, CanDisable: no}, "low"},
		{"none", "reasoning_effort", domain.Reasoning{}, "none"},
		{"xhigh", "reasoning_effort", domain.Reasoning{}, "xhigh"},
		{"none", "enable_thinking", domain.Reasoning{Levels: []string{"on"}, CanDisable: no}, "on"},
		{"minimal", "enable_thinking", domain.Reasoning{Map: map[string]string{"minimal": "none"}}, "none"},
		{"minimal", "thinking_toggle", domain.Reasoning{Map: map[string]string{"minimal": "low"}}, "on"},
		{"low", "thinking_toggle", domain.Reasoning{}, "on"},
	} {
		if got := translate(tt.l, tt.d, &tt.r); got != tt.want {
			t.Errorf("translate(%s, %s, %+v) = %s, want %s", tt.l, tt.d, tt.r, got, tt.want)
		}
	}
}

// Level is the string of the dialect's level member in the outgoing body,
// for gen_ai.request.reasoning.level: reasoning_effort, reasoning.effort or
// output_config.effort; none for a dialect without one, a number or an
// absent member.
func TestLevel(t *testing.T) {
	for _, tt := range []struct {
		dialect, body, want string
	}{
		{"reasoning_effort", `{"reasoning_effort":"high"}`, "high"},
		{"reasoning_effort", `{"reasoning_effort":"none"}`, "none"},
		{"reasoning_effort", `{}`, ""},
		{"reasoning_object", `{"reasoning":{"effort":"xhigh"}}`, "xhigh"},
		{"reasoning_object", `{"reasoning":{"enabled":false}}`, ""},
		{"adaptive_effort", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"medium"}}`, "medium"},
		{"adaptive_effort", `{"thinking":{"type":"disabled"}}`, ""},
		{"thinking_budget", `{"thinking":{"type":"enabled","budget_tokens":2048}}`, ""},
		{"thinking_toggle", `{"thinking":{"type":"enabled"}}`, ""},
		{"enable_thinking", `{"enable_thinking":true,"reasoning_effort":"high"}`, ""},
		{"none", `{"reasoning_effort":"high"}`, ""},
	} {
		out, e := params.ReadBack([]byte(tt.body), params.ChatOut)
		if e != nil {
			t.Fatal(e)
		}
		if got := Level(out, &domain.Reasoning{Dialect: tt.dialect}); got != tt.want {
			t.Errorf("Level(%s, %s) = %q, want %q", tt.dialect, tt.body, got, tt.want)
		}
	}
}
