package reasoning

import (
	"testing"

	"github.com/jorlaru/stargaite/internal/domain"
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
