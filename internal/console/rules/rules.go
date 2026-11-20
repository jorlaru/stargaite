// Package rules holds the console's two tables. Warnings: for each code
// internal/config can warn with, the step of the running task that resolves
// it, so the warning is held until then (design section 3, "Warnings"), and
// the plain words that say it. Suggestions: what the home suggests from what
// it read (design section 3, "Suggestions"; suggest.go).
package rules

import (
	"fmt"
	"strings"
)

// Warning is one row of the table.
type Warning struct {
	Code  string
	Held  string // the step of the running task that resolves it: "recreate" or "check"; "everything": only Check everything says it; "" is shown at once
	Words string // plain words, %s is the resource's name; "" shows the CLI's own message
}

// Warnings is the table, a fresh slice on each call: one row per code
// internal/config can warn with. Words land with the tasks that fix them.
func Warnings() []Warning {
	return []Warning{
		{"credential_unresolved", "recreate", "%s: its key is not in the container yet: Add providers writes it to .env and recreates the container"},
		{"url_unresolved", "", "%s: its host name did not resolve from the container; the proxy tries again on each call, so check the URL if this lasts"},
		{"target_unverified", "check", "%s is not checked yet: Make an alias or Change an alias checks it, after stating its paid calls"},
		{"unpriced", "", "%s has no prices, so no budget counts its spend; a budget that reaches it asks them"},
		{"no_fallback_prices", "", "%s has no prices: its cost is the provider's own report, and a call without one counts nothing"},
		{"reasoning_undeclared", "everything", ""}, // design section 3, Reasoning: an effort set shows what is sent instead
		{"effort_dropped", "", "%s: the alias's effort is not sent toward a target whose reasoning dialect is none"},
		{"endpoint_unservable", "", ""},
		{"unknown_alias", "", "%s names an alias that does not exist: its calls to that alias get an error: Change a key sets the aliases it may call"},
		{"unknown_provider", "", ""},
		{"quota_beyond_retention", "", ""},
		{"env_file_unreadable", "", ""},
		{"env_file_malformed", "", ""},
		{"env_file_quoted", "", ""},
	}
}

// Lookup returns the row of code.
func Lookup(code string) (Warning, bool) {
	for _, w := range Warnings() {
		if w.Code == code {
			return w, true
		}
	}
	return Warning{}, false
}

// Plain says a warning about the resource name: the row's words, or else the
// resource and the CLI's own message. The resource is named in words, such
// as "target a" for targets/a.
func Plain(code, name, message string) string {
	name = named(name)
	if w, ok := Lookup(code); ok && w.Words != "" {
		return fmt.Sprintf(w.Words, name)
	}
	return name + ": " + message
}

// named turns a resource of internal/config, such as targets/a, into words;
// a resource of another form is kept as it is.
func named(res string) string {
	kinds := map[string]string{"providers": "provider", "targets": "target", "models": "alias", "keys": "key"}
	if kind, name, ok := strings.Cut(res, "/"); ok && kinds[kind] != "" {
		return kinds[kind] + " " + name
	}
	return res
}
