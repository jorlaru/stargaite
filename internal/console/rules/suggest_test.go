package rules

import (
	"reflect"
	"testing"
)

func TestSuggest(t *testing.T) {
	provider := Suggestion{Label: "Add a provider: none is added yet", Task: "First run"}
	alias := Suggestion{Label: "Make an alias your apps call: none exists yet", Task: "First run"}
	key := Suggestion{Label: "Give an app or agent its key: none exists yet", Task: "First run"}
	replace := Suggestion{Label: "Replace the key of acme: ACME_KEY does not resolve", Task: "Change a provider",
		Presets: map[string]string{"provider": "acme", "change": "key"}}
	fallback := Suggestion{Label: "Give alias a a fallback: it has one model", Task: "Change an alias",
		Presets: map[string]string{"alias": "a", "change": "add"}}
	for _, tc := range []struct {
		facts Facts
		want  []Suggestion
	}{
		{Facts{}, []Suggestion{provider}},
		{Facts{Keys: 1, Unresolved: map[string]string{"acme": "ACME_KEY"}}, []Suggestion{provider}},
		{Facts{Providers: 1, Unresolved: map[string]string{"acme": "ACME_KEY"}}, []Suggestion{alias}},
		{Facts{Providers: 1, Aliases: map[string]int{"a": 1, "b": 2}, Unresolved: map[string]string{"acme": "ACME_KEY"}},
			[]Suggestion{key, replace, fallback}},
		{Facts{Providers: 1, Aliases: map[string]int{"b": 2}, Keys: 1}, nil},
	} {
		if got := Suggest(tc.facts); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Suggest(%+v) = %+v\nwant %+v", tc.facts, got, tc.want)
		}
	}
	f := Facts{Providers: 1, Aliases: map[string]int{"a": 1}, Keys: 1, Unresolved: map[string]string{"zed": "Z", "acme": "A"}}
	got := Suggest(f)
	if len(got) != 3 || got[0].Presets["provider"] != "acme" || got[1].Presets["provider"] != "zed" {
		t.Fatalf("not by name: %+v", got)
	}
	got[0].Presets["change"] = "x"
	if again := Suggest(f); again[0].Presets["change"] != "key" {
		t.Fatalf("a change to one result reached the next: %+v", again[0])
	}
}

func TestMissing(t *testing.T) {
	two := map[string]string{"a": "A", "b": "B"}
	for _, tc := range []struct {
		facts Facts
		want  []string
	}{
		{Facts{}, []string{"provider", "alias", "key"}},
		{Facts{Providers: 2, Unresolved: two, Aliases: map[string]int{"main": 1}, Keys: 1}, []string{"provider"}},
		{Facts{Providers: 2, Unresolved: map[string]string{"a": "A"}}, []string{"alias", "key"}},
		{Facts{Providers: 1, Aliases: map[string]int{"main": 1}}, []string{"key"}},
		{Facts{Providers: 1, Aliases: map[string]int{"main": 1}, Keys: 1}, nil},
	} {
		if got := Missing(tc.facts); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Missing(%+v) = %q; want %q", tc.facts, got, tc.want)
		}
	}
}
