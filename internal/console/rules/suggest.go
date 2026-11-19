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
}

// Suggestion is one row the home suggests: its words, the menu entry it
// opens, and the answers it gives that task's questions, by ID.
type Suggestion struct {
	Label, Task string
	Presets     map[string]string
}

// Suggest is what the home suggests, in this order (design section 3,
// "Suggestions"): a provider, else an alias, when none exists, and nothing
// more; then a key when none exists, each provider whose key does not
// resolve, and each alias with one model, by name. A row whose task is not
// ported yet lands with that task.
func Suggest(f Facts) []Suggestion {
	switch {
	case f.Providers == 0:
		return []Suggestion{{Label: "Add a provider: none is added yet", Task: "Add providers"}}
	case len(f.Aliases) == 0:
		return []Suggestion{{Label: "Make an alias your apps call: none exists yet", Task: "Make an alias"}}
	}
	var out []Suggestion
	if f.Keys == 0 {
		out = append(out, Suggestion{Label: "Give an app or agent its key: none exists yet", Task: "Give an app or agent its key"})
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
	return out
}
