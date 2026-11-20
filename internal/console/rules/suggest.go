package rules

import (
	"fmt"
	"maps"
	"slices"
)

// Facts is what the home read that the suggestions judge.
type Facts struct {
	Providers, Keys int
	Aliases         map[string]int    // alias → how many targets it has
	Unresolved      map[string]string // provider → the variable its credential names, which does not resolve
	Uncapped        bool              // no instance budget is set
	Unverified      map[string]string // provider none of whose chat targets is checked → the alias that holds one ("" none)
	Running, Clone  string            // the proxy's version and the clone's
	Upstream        string            // the clone's upstream
	Ahead           int               // its commits the clone lacks, as last fetched
}

// Suggestion is one row the home suggests: its words, the menu entry it
// opens, and the answers it gives that task's questions, by ID.
type Suggestion struct {
	Label, Task string
	Presets     map[string]string
}

// Suggest is what the home suggests, in this order (design section 3,
// "Suggestions"): a provider, else an alias, when none exists, and nothing
// more but Upgrades; then a key when none exists, each provider whose key
// does not resolve, each alias with one model, by name, spend not capped,
// each provider none of whose targets is checked, by name, through an alias
// that holds one, and Upgrades. The first three open the first run.
func Suggest(f Facts) []Suggestion { return append(suggest(f), Upgrades(f)...) }

// Upgrades is what the home suggests last: Rebuild when the proxy runs
// another version than the clone's, neither a dev build, then Upgrade when
// the clone's upstream has commits it lacks.
func Upgrades(f Facts) []Suggestion {
	var out []Suggestion
	if f.Running != "" && f.Clone != "" && f.Running != f.Clone && f.Running != "dev" && f.Clone != "dev" {
		out = append(out, Suggestion{Label: "Rebuild to this clone's version", Task: "Upgrade", Presets: map[string]string{"rebuild": "yes"}})
	}
	if f.Ahead > 0 {
		out = append(out, Suggestion{Label: fmt.Sprintf("Upgrade: %d new commit(s) on %s", f.Ahead, f.Upstream), Task: "Upgrade"})
	}
	return out
}

func suggest(f Facts) []Suggestion {
	switch {
	case f.Providers == 0:
		return []Suggestion{{Label: "Add a provider: none is added yet", Task: "First run"}}
	case len(f.Aliases) == 0:
		return []Suggestion{{Label: "Make an alias your apps call: none exists yet", Task: "First run"}}
	}
	var out []Suggestion
	if f.Keys == 0 {
		out = append(out, Suggestion{Label: "Give an app or agent its key: none exists yet", Task: "First run"})
	}
	for _, p := range slices.Sorted(maps.Keys(f.Unresolved)) {
		out = append(out, Suggestion{Label: fmt.Sprintf("Replace the key of %s: %s does not resolve", p, f.Unresolved[p]),
			Task: "Change a provider", Presets: map[string]string{"provider": p, "change": "key"}})
	}
	for _, a := range slices.Sorted(maps.Keys(f.Aliases)) {
		if f.Aliases[a] == 1 {
			out = append(out, Suggestion{Label: fmt.Sprintf("Give alias %s a fallback: it has one model", a),
				Task: "Change an alias", Presets: map[string]string{"alias": a, "change": "add"}})
		}
	}
	if f.Uncapped {
		out = append(out, Suggestion{Label: "Cap spending: spend is not capped", Task: "Cap spending", Presets: map[string]string{"on": "instance"}})
	}
	for _, p := range slices.Sorted(maps.Keys(f.Unverified)) {
		if a := f.Unverified[p]; a != "" {
			out = append(out, Suggestion{Label: fmt.Sprintf("Check alias %s: no target of %s is checked yet", a, p),
				Task: "Make an alias", Presets: map[string]string{"alias": a, "unverified": "yes"}})
		}
	}
	return out
}

// Missing is the first run's stages whose own thing is missing, in order
// (design section 4.1): "provider" when no provider's credential resolves,
// "alias" when no alias exists, "key" when no key exists.
func Missing(f Facts) []string {
	var out []string
	if f.Providers == len(f.Unresolved) {
		out = append(out, "provider")
	}
	if len(f.Aliases) == 0 {
		out = append(out, "alias")
	}
	if f.Keys == 0 {
		out = append(out, "key")
	}
	return out
}
