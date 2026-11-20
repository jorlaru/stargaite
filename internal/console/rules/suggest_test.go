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
	uncapped := Suggestion{Label: "Cap spending: spend is not capped", Task: "Cap spending", Presets: map[string]string{"on": "instance"}}
	check := Suggestion{Label: "Check alias a: no target of mock is checked yet", Task: "Make an alias",
		Presets: map[string]string{"alias": "a", "unverified": "yes"}}
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
		{Facts{Providers: 1, Keys: 1, Aliases: map[string]int{"a": 2}, Uncapped: true}, []Suggestion{uncapped}},
		{Facts{Uncapped: true}, []Suggestion{provider}},
		{Facts{Providers: 1, Aliases: map[string]int{"a": 1}, Uncapped: true}, []Suggestion{key, fallback, uncapped}},
		{Facts{Providers: 2, Keys: 1, Aliases: map[string]int{"a": 2}, Unverified: map[string]string{"mock": "a", "other": ""}}, []Suggestion{check}},
		{Facts{Providers: 2, Keys: 1, Aliases: map[string]int{"a": 2}, Unverified: map[string]string{"mock": "a", "other": ""}, Uncapped: true},
			[]Suggestion{uncapped, check}},
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

// TestSuggestRebuildAndUpgrade: M1 when the proxy's and the clone's
// versions differ and neither is dev, then M2 when the clone's upstream has
// new commits, after every other row, the first run's single row included.
func TestSuggestRebuildAndUpgrade(t *testing.T) {
	rebuild := Suggestion{Label: "Rebuild to this clone's version", Task: "Upgrade", Presets: map[string]string{"rebuild": "yes"}}
	upgrade := Suggestion{Label: "Upgrade: 3 new commit(s) on origin/main", Task: "Upgrade"}
	check := Suggestion{Label: "Check alias main: no target of p is checked yet", Task: "Make an alias",
		Presets: map[string]string{"alias": "main", "unverified": "yes"}}
	ready := Facts{Providers: 1, Keys: 1, Aliases: map[string]int{"main": 1}, Unverified: map[string]string{"p": "main"}}
	fallback := Suggest(ready)[0]
	with := func(f Facts, running, clone string, ahead int) Facts {
		f.Running, f.Clone, f.Ahead = running, clone, ahead
		if ahead > 0 {
			f.Upstream = "origin/main"
		}
		return f
	}
	for _, tc := range []struct {
		facts Facts
		want  []Suggestion
	}{
		{with(ready, "v1", "v2", 0), []Suggestion{fallback, check, rebuild}},
		{with(ready, "dev", "v2", 0), []Suggestion{fallback, check}},
		{with(ready, "v1", "dev", 0), []Suggestion{fallback, check}},
		{with(ready, "v2", "v2", 0), []Suggestion{fallback, check}},
		{with(ready, "v1", "v2", 3), []Suggestion{fallback, check, rebuild, upgrade}},
		{with(ready, "v2", "v2", 3), []Suggestion{fallback, check, upgrade}},
		{with(Facts{}, "v1", "v2", 0), []Suggestion{{Label: "Add a provider: none is added yet", Task: "First run"}, rebuild}},
		{with(Facts{Providers: 1}, "v1", "v2", 0), []Suggestion{{Label: "Make an alias your apps call: none exists yet", Task: "First run"}, rebuild}},
	} {
		if got := Suggest(tc.facts); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Suggest(%+v) = %+v\nwant %+v", tc.facts, got, tc.want)
		}
	}
	if got := Upgrades(Facts{Running: "v1", Clone: "v2"}); !reflect.DeepEqual(got, []Suggestion{rebuild}) {
		t.Errorf("Upgrades = %+v", got)
	}
}
