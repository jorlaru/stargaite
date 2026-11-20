// Package rules holds the console's two tables. Warnings: for each code
// internal/config can warn with, the step of the running task that resolves
// it, so the warning is held until then (design section 3, "Warnings"), and
// the plain words that say it. Suggestions: what the home suggests from what
// it read (design section 3, "Suggestions"; suggest.go).
package rules

import (
	"fmt"
	"regexp"
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
		{"reasoning_undeclared", "everything", "%s: its reasoning levels are not declared, so an effort is sent as given and never in the off form"},
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
	name = Named(name)
	if w, ok := Lookup(code); ok && w.Words != "" {
		return fmt.Sprintf(w.Words, name)
	}
	return name + ": " + message
}

// Command is the stargaite command that fixes a finding of code on resource,
// with the target's id in it as a shell word, or "" when no command does: a
// target's reasoning levels and prices, which no task sets (design section 3).
func Command(code, resource string) string {
	id, ok := strings.CutPrefix(resource, "targets/")
	id = ShellWord(id)
	switch {
	case !ok:
		return ""
	case code == "reasoning_undeclared":
		return "targets set " + id + " --level <level> --can-disable=<true|false>, one --level per level it takes"
	case code == "unpriced" || code == "no_fallback_prices":
		return "targets set " + id + " --pricing input_per_million=<usd>,output_per_million=<usd>,as_of=<YYYY-MM-DD>"
	}
	return ""
}

// ShellWord is s as it is when a shell reads it so, else single-quoted: a
// target's id comes from a provider's model list and may hold ; $ or a
// quote, a file name a space.
func ShellWord(s string) string {
	if regexp.MustCompile(`^[A-Za-z0-9._:/@+=-]+$`).MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Fix is the task, and its presets, that fixes a finding of code on
// resource, alias being the alias that holds a target ("" none); false when
// no task does. A target never checked is checked again through an alias
// that holds it, its unverified targets only.
func Fix(code, resource, alias string) (Suggestion, bool) {
	kind, name, _ := strings.Cut(resource, "/")
	switch {
	case code == "credential_unresolved" && kind == "providers":
		return Suggestion{Task: "Change a provider", Presets: map[string]string{"provider": name, "change": "key"}}, true
	case code == "credential_unresolved" && resource == "settings":
		return Suggestion{Task: "Send traces"}, true
	case code == "target_unverified" && kind == "targets" && alias != "":
		return Suggestion{Task: "Make an alias", Presets: map[string]string{"alias": alias, "unverified": "yes"}}, true
	case code == "unknown_alias" && kind == "keys":
		return Suggestion{Task: "Change a key", Presets: map[string]string{"key": name, "change": "aliases"}}, true
	case code == "effort_dropped" && kind == "models":
		return Suggestion{Task: "Change an alias", Presets: map[string]string{"alias": name, "change": "effort"}}, true
	case code == "endpoint_unservable" && kind == "models":
		return Suggestion{Task: "Change an alias", Presets: map[string]string{"alias": name}}, true
	}
	return Suggestion{}, false
}

// Named turns a resource of internal/config, such as targets/a, into words;
// a resource of another form, such as settings, is kept as it is.
func Named(res string) string {
	kinds := map[string]string{"providers": "provider", "targets": "target", "models": "alias", "keys": "key"}
	if kind, name, ok := strings.Cut(res, "/"); ok && kinds[kind] != "" {
		return kinds[kind] + " " + name
	}
	return res
}
