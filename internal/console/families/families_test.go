package families

import (
	"slices"
	"testing"
)

func TestSort(t *testing.T) {
	for _, tc := range []struct{ id, vendor, family string }{
		// Research's example table (RR section 3.2).
		{"MiniMax-M3.1-Flash", "MiniMax", "MiniMax"},
		{"qwen/qwen-3.6-flash", "Qwen", "Qwen"},
		{"claude-sonnet-5-5", "Anthropic", "Claude Sonnet"},
		{"gpt-5-mini", "OpenAI", "GPT"},
		{"gpt-a", "OpenAI", "GPT"},
		{"gpt-b", "OpenAI", "GPT"},
		{"openai/gpt-oss-120b", "OpenAI", "gpt-oss"},
		{"o4-mini", "OpenAI", "o-series"},
		{"llama3:latest", "Meta", "Llama"},
		{"meta-llama/Llama-3.3-70B-Instruct-Turbo", "Meta", "Llama"},
		{"accounts/fireworks/models/llama-v3p1-70b-instruct", "Meta", "Llama"},
		{"models/gemini-2.5-flash", "Google", "Gemini"},
		{"deepseek/deepseek-r1:free", "DeepSeek", "DeepSeek"},
		{"nousresearch/hermes-3-llama-3.1-405b", "nousresearch", "Other"},
		{"other-model", "Other", "Other"},
		{"m1", "Other", "Other"},
		{"mock", "Other", "Other"},
		// The plan's rows.
		{"olmo-7b", "Other", "Other"},
		{"ft:gpt-4o-mini:acme::x", "Other", "Other"},
		{"anthropic/claude-3.5-sonnet", "Anthropic", "Claude Sonnet"},
		{"anthropic/claude-3.7-sonnet", "Anthropic", "Claude Sonnet"},
		{"anthropic/claude-3.5-haiku", "Anthropic", "Claude Haiku"},
		{"mistralai/mixtral-8x7b-instruct", "Mistral", "Mistral"},
		{"open-mixtral-8x22b", "Mistral", "Mistral"},
		{"meta-llama/Meta-Llama-3.1-70B-Instruct", "Meta", "Llama"},
		{"zai-glm-4.6", "Z.ai", "GLM"},
		{"x-ai/grok-4", "xAI", "Grok"},
		{"llama3.1:8b", "Meta", "Llama"},
		{"~anthropic/claude-sonnet-latest", "Anthropic", "Claude Sonnet"}, // OpenRouter's "-latest" aliases
	} {
		got, hidden := Sort([]string{tc.id}, false)
		want := []Model{{ID: tc.id, Vendor: tc.vendor, Family: tc.family}}
		if !slices.Equal(got, want) || hidden != 0 {
			t.Errorf("Sort(%q) = %+v, %d hidden; want %+v", tc.id, got, hidden, want)
		}
	}

	nonChat := []string{"bge-m3", "text-embedding-3-small", "whisper-1", "dall-e-3"}
	if got, hidden := Sort(nonChat, false); len(got) != 0 || hidden != 4 {
		t.Errorf("non-chat ids: %+v, %d hidden; want none shown, 4 hidden", got, hidden)
	}
	if got, hidden := Sort(nonChat, true); len(got) != 4 || hidden != 0 {
		t.Errorf("non-chat ids with all: %+v, %d hidden; want all 4, none hidden", got, hidden)
	}
	if got, hidden := Sort([]string{"imagen-4"}, false); len(got) != 0 || hidden != 1 {
		t.Errorf("imagen-4: %+v, %d hidden; want hidden", got, hidden)
	}

	got, _ := Sort([]string{"other-model", "gpt-b", "claude-sonnet-5-5", "gpt-a", "x-ai/grok-4", "zz/a"}, false)
	var ids []string
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	if want := []string{"gpt-a", "gpt-b", "claude-sonnet-5-5", "x-ai/grok-4", "zz/a", "other-model"}; !slices.Equal(ids, want) {
		t.Errorf("order %q; want %q", ids, want)
	}
	if got[4] != (Model{ID: "zz/a", Vendor: "zz", Family: "Other"}) {
		t.Errorf("zz/a: %+v", got[4])
	}
}

// TestSortOrder: families in table order, then Other, within a vendor; ids
// case-insensitively; unknown namespaces alphabetically.
func TestSortOrder(t *testing.T) {
	got, _ := Sort([]string{"openai/zeta", "o3", "GPT-Z", "gpt-a", "gpt-oss-20b", "b/x", "a/x"}, false)
	var ids []string
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	if want := []string{"gpt-oss-20b", "gpt-a", "GPT-Z", "o3", "openai/zeta", "a/x", "b/x"}; !slices.Equal(ids, want) {
		t.Errorf("order %q; want %q", ids, want)
	}
}

func TestTableFresh(t *testing.T) {
	first := table()
	first[0].name = "changed"
	first[0].families[0].prefixes[0] = "changed"
	second := table()
	if second[0].name == "changed" || second[0].families[0].prefixes[0] == "changed" {
		t.Fatal("a change to one call's table reached a second call")
	}
	markers := nonChat()
	markers[0] = "changed"
	if nonChat()[0] == "changed" {
		t.Fatal("a change to one call's markers reached a second call")
	}
}
