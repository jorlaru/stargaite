// Package rules holds the console's tables about the proxy's warnings: for
// each code internal/config can warn with, the step of the running task that
// resolves it, so the warning is held until then (design section 3,
// "Warnings"), and the plain words that say it.
package rules

import "fmt"

// Warning is one row of the table.
type Warning struct {
	Code  string
	Held  string // the step of the running task that resolves it: "recreate" or "check"; "" is shown at once
	Words string // plain words, %s is the resource's name; "" shows the CLI's own message
}

// Warnings is the table, a fresh slice on each call: one row per code
// internal/config can warn with. Words land with the tasks that fix them.
func Warnings() []Warning {
	return []Warning{
		{"credential_unresolved", "recreate", "%s: its key is not in the container yet: Add providers writes it to .env and recreates the container"},
		{"url_unresolved", "", "%s: its host name did not resolve from the container; the proxy tries again on each call, so check the URL if this lasts"},
		{"target_unverified", "check", ""},
		{"unpriced", "", ""},
		{"no_fallback_prices", "", ""},
		{"reasoning_undeclared", "", ""},
		{"effort_dropped", "", ""},
		{"endpoint_unservable", "", ""},
		{"unknown_alias", "", ""},
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
// CLI's own message and where to find it again.
func Plain(code, name, message string) string {
	if w, ok := Lookup(code); ok && w.Words != "" {
		return fmt.Sprintf(w.Words, name)
	}
	return message + " (stargaite validate lists it)"
}
