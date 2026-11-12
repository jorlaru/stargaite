// Package cli is the command line: a command table, one flag set per command
// and deterministic exit codes. Every command but version and validate -f is
// one call to the admin API. No command ever reads standard input (PRI-8).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Version is the version string. Release builds set it at link time.
var Version = "dev"

// Exit codes: success, failure, wrong usage (OPS-2).
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

type input struct {
	json   bool
	args   []string           // the positional arguments
	flags  map[string]*string // the string flags of the row, by name
	bools  map[string]*bool   // the boolean flags of the row, by name
	set    map[string]bool    // the flags given, by name
	body   map[string]any     // what the body flags given fill, by path
	getenv func(string) string
}

type flagKind int

const (
	kindString      flagKind = iota // a value the command reads itself or, with a path, a JSON string at path
	kindBool                        // the same, given without a value; with a path, true or false at path
	kindNumber                      // a JSON number at path
	kindTypedPairs                  // name=value pairs under path, comma-separated or repeated: a JSON number, true or false is sent as that literal, anything else as a string
	kindStringPairs                 // the same, every value sent as a string
	kindQuota                       // <window>:<unit>=<n>, repeated: appends {"window": window, unit: n} to the list at path
	kindList                        // repeated: appends the value, a JSON string, to the list at path, in order
	kindRaw                         // one JSON value, sent verbatim at path
)

// flagSpec is one flag of a command. A one-letter name is the short form the
// kick-off writes ("-f"); the flag package also accepts it with two dashes.
type flagSpec struct {
	name  string
	kind  flagKind
	path  string // the member of the request body a body flag fills: "timeouts.connect_ms"
	usage string
}

type command struct {
	words    []string // leading words: {"settings", "set"}
	args     []string // positional arguments, all required
	flags    []flagSpec
	needsYes func(in input) bool                                      // nil is never; without --yes, exit 2 (OPS-2)
	request  func(in input) (method, path string, body any)           // the one admin call
	local    func(ctx context.Context, in input, out io.Writer) error // runs without a server; errRemote hands over to request
}

// yamlFile is a request body: the file at that path, sent as YAML.
type yamlFile string

func get(path string) func(input) (string, string, any) {
	return func(input) (string, string, any) { return http.MethodGet, path, nil }
}

// settingsFlags are the scalar and structured settings; telemetry has its own
// command from phase 3.
var settingsFlags = []flagSpec{
	{"budget", kindTypedPairs, "budget", "limit_usd=<usd>,reset_interval=<daily|monthly|never>"},
	{"max-parallel-requests", kindNumber, "max_parallel_requests", "requests in flight on the instance"},
	{"max-body-bytes", kindNumber, "max_body_bytes", "the largest request body"},
	{"max-response-bytes", kindNumber, "max_response_bytes", "the largest provider response"},
	{"connect-ms", kindNumber, "timeouts.connect_ms", "provider connection timeout"},
	{"first-byte-ms", kindNumber, "timeouts.first_byte_ms", "time to the first byte of a provider response"},
	{"stream-idle-ms", kindNumber, "timeouts.stream_idle_ms", "longest pause inside a stream"},
	{"total-ms", kindNumber, "timeouts.total_ms", "longest provider call"},
	{"usage-days", kindNumber, "retention.usage_days", "days usage records are kept"},
	{"usage-daily-months", kindNumber, "retention.usage_daily_months", "months daily aggregates are kept"},
	{"audit-months", kindNumber, "retention.audit_months", "months audit records are kept"},
}

// providerFlags are the fields of a provider (kick-off section 6).
var providerFlags = []flagSpec{
	{"protocol", kindString, "protocol", "openai or anthropic"},
	{"base-url", kindString, "base_url", "the provider's base URL"},
	{"api-key-env", kindString, "api_key_env", "the environment variable that holds the credential, never its value"},
	{"billing", kindString, "billing", "pay_per_use, subscription or none"},
	{"quota", kindQuota, "quota", "<window>:<requests|tokens>=<n>, repeated: a window of a subscription, such as 5h:requests=1500"},
	{"cost-report", kindTypedPairs, "cost_report", "path=<dotted path>,usd_per_unit=<usd>: where a response reports its cost"},
	{"stream-usage", kindString, "stream_usage", "request, native or none"},
	{"rerank-url", kindString, "rerank_url", "where rerank requests go"},
	{"gen-ai-provider", kindString, "gen_ai_provider", "the provider's well-known name for telemetry"},
	{"target-defaults", kindStringPairs, "target_defaults", "dialect=<dialect>,output_cap_param=<name>: what a new target starts with"},
	{"allow-private-network", kindBool, "allow_private_network", "allow plain HTTP, loopback and private addresses"},
}

// targetFlags are the fields of a target (kick-off section 6), with the
// flags of its reasoning block (kick-off section 13.3).
var targetFlags = []flagSpec{
	{"kind", kindString, "kind", "chat, embeddings or rerank"},
	{"capabilities", kindTypedPairs, "capabilities", "tools=<true|false>,structured_output=...,strict_schema=...,vision=..."},
	{"max-output-tokens", kindNumber, "max_output_tokens", "the model's own output limit"},
	{"output-cap-param", kindString, "output_cap_param", "the parameter an alias cap is sent as toward an openai target"},
	{"dialect", kindString, "reasoning.dialect", "the reasoning dialect"},
	{"level", kindList, "reasoning.levels", "a level the provider accepts, repeated in scale order"},
	{"can-disable", kindBool, "reasoning.can_disable", "whether reasoning can be turned off"},
	{"thinking-budget", kindTypedPairs, "reasoning.budgets", "<level>=<tokens>: the budget of a level, for thinking_budget"},
	{"effort-map", kindStringPairs, "reasoning.map", "<level>=<level>: the level sent for a canonical level"},
	{"reasoning-on", kindRaw, "reasoning.on", "the exact JSON fragment that turns reasoning on"},
	{"reasoning-off", kindRaw, "reasoning.off", "the exact JSON fragment that turns reasoning off"},
	{"pricing", kindTypedPairs, "pricing", "input_per_million=<usd>,output_per_million=<usd>,cache_read_per_million=...,cache_write_per_million=...,as_of=<date>"},
	{"unsupported-param", kindList, "unsupported_params", "a parameter the target does not accept, repeated"},
	{"response-model", kindList, "response_models", "a model name the provider may answer with, repeated"},
	{"inline-think-tags", kindBool, "inline_think_tags", "remove <think> blocks from the visible content"},
}

// modelFlags are the fields of a model alias (kick-off section 6).
var modelFlags = []flagSpec{
	{"target", kindList, "targets", "a target id, repeated: the primary first, then the fallbacks"},
	{"require", kindList, "requires", "a capability every target must declare true, repeated"},
	{"effort", kindString, "reasoning.effort", "the alias's reasoning effort, or provider_default"},
	{"effort-mode", kindString, "reasoning.mode", "default (the client's effort wins) or forced"},
	{"max-output-tokens", kindNumber, "max_output_tokens", "the alias's output cap"},
	{"param", kindStringPairs, "params", "<parameter>=<forward|drop>: a rule for a parameter"},
	{"budget", kindTypedPairs, "budget", "limit_usd=<usd>,reset_interval=<daily|monthly|never>"},
	{"rpm-limit", kindNumber, "rpm_limit", "requests per minute"},
	{"tpm-limit", kindNumber, "tpm_limit", "tokens per minute"},
	{"max-parallel-requests", kindNumber, "max_parallel_requests", "requests in flight"},
	{"trace-sample-rate", kindNumber, "trace_sample_rate", "the share of requests traced, 0 to 1"},
}

// add is the request of a command that creates the resource its positional
// argument names: the body flags given, plus that argument as member key.
func add(collection, key string) func(input) (string, string, any) {
	return func(in input) (string, string, any) {
		in.body[key] = in.args[0]
		return http.MethodPost, collection, in.body
	}
}

// named is the request of a command on the resource its positional argument
// names; a PATCH sends the body flags given as the merge patch. The argument
// is escaped whole, so each "/" of a target id travels as %2F and no segment
// of an upstream id is cleaned away on the server.
func named(method, collection string) func(input) (string, string, any) {
	return func(in input) (string, string, any) {
		var body any
		if method == http.MethodPatch {
			body = in.body
		}
		return method, collection + "/" + url.PathEscape(in.args[0]), body
	}
}

var commands = []command{
	{words: []string{"version"}, local: version},
	{words: []string{"validate"}, local: validateFile, request: get("/validate"), flags: []flagSpec{
		{"f", kindString, "", "the YAML file to check, with no server running; without it, the server checks its configuration"},
		{"env-file", kindString, "", "with -f: a file of NAME=value lines that credential references also resolve from"},
	}},
	{words: []string{"settings", "show"}, request: get("/settings")},
	{words: []string{"settings", "set"}, flags: settingsFlags, request: func(in input) (string, string, any) {
		return http.MethodPatch, "/settings", in.body
	}},
	{words: []string{"providers", "presets"}, request: get("/presets")},
	{words: []string{"providers", "add"}, args: []string{"name"}, flags: append(slices.Clone(providerFlags),
		flagSpec{"preset", kindString, "preset", "the preset to start from (default: the one named like the provider, if any)"}),
		request: add("/providers", "name")},
	{words: []string{"providers", "list"}, request: get("/providers")},
	{words: []string{"providers", "show"}, args: []string{"name"}, request: named(http.MethodGet, "/providers")},
	{words: []string{"providers", "set"}, args: []string{"name"}, flags: providerFlags, request: named(http.MethodPatch, "/providers")},
	{words: []string{"providers", "remove"}, args: []string{"name"}, needsYes: func(input) bool { return true }, request: named(http.MethodDelete, "/providers")},
	{words: []string{"targets", "add"}, args: []string{"id"}, flags: targetFlags, request: add("/targets", "id")},
	{words: []string{"targets", "list"}, request: get("/targets")},
	{words: []string{"targets", "show"}, args: []string{"id"}, request: named(http.MethodGet, "/targets")},
	{words: []string{"targets", "set"}, args: []string{"id"}, flags: targetFlags, request: named(http.MethodPatch, "/targets")},
	{words: []string{"targets", "remove"}, args: []string{"id"}, needsYes: func(input) bool { return true }, request: named(http.MethodDelete, "/targets")},
	{words: []string{"models", "add"}, args: []string{"name"}, flags: modelFlags, request: add("/models", "name")},
	{words: []string{"models", "list"}, request: get("/models")},
	{words: []string{"models", "show"}, args: []string{"name"}, request: named(http.MethodGet, "/models")},
	{words: []string{"models", "set"}, args: []string{"name"}, flags: modelFlags, request: named(http.MethodPatch, "/models")},
	{words: []string{"models", "remove"}, args: []string{"name"}, needsYes: func(input) bool { return true }, request: named(http.MethodDelete, "/models")},
	{words: []string{"export"}, request: func(in input) (string, string, any) {
		if in.json {
			return http.MethodGet, "/export?format=json", nil
		}
		return http.MethodGet, "/export", nil
	}},
	{words: []string{"apply"}, flags: []flagSpec{
		{"f", kindString, "", "the YAML file to apply"},
		{"dry-run", kindBool, "", "validate and show the difference, and publish nothing"},
		{"prune", kindBool, "", "remove absent providers, targets and aliases, revoke absent keys and clear absent settings"},
	}, needsYes: func(in input) bool { return *in.bools["prune"] && !*in.bools["dry-run"] },
		request: func(in input) (string, string, any) {
			q := url.Values{"dry_run": {strconv.FormatBool(*in.bools["dry-run"])}, "prune": {strconv.FormatBool(*in.bools["prune"])}}
			return http.MethodPost, "/apply?" + q.Encode(), yamlFile(*in.flags["f"])
		}},
}

var (
	// errUsage marks a wrong usage found by a command itself: exit 2.
	errUsage = errors.New("wrong usage")
	// errRemote is what a local function returns to send the request instead.
	errRemote = errors.New("ask the server")
)

// Main runs one command and returns the exit code of the process. getenv is
// the only way a command reads the process environment.
func Main(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
		usage(stdout)
		return exitOK
	}
	cmd, rest := match(args)
	if cmd == nil {
		if len(args) == 0 {
			say(stderr, "stargaite: no command given\n")
		} else {
			say(stderr, "stargaite: unknown command %q\n", args[0])
		}
		usage(stderr)
		return exitUsage
	}

	name := "stargaite " + strings.Join(cmd.words, " ")
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	in := input{flags: map[string]*string{}, bools: map[string]*bool{}, set: map[string]bool{}, body: map[string]any{}, getenv: getenv}
	fs.BoolVar(&in.json, "json", false, "print JSON")
	specs := slices.Clone(cmd.flags)
	if cmd.request != nil {
		specs = append(specs,
			flagSpec{"admin-url", kindString, "", "the admin API (default $STARGAITE_ADMIN_URL, else " + defaultAdminURL + ")"},
			flagSpec{"data-dir", kindString, "", "where admin.key is when STARGAITE_ADMIN_KEY is unset (default $STARGAITE_DATA_DIR, else ./data)"})
	}
	if cmd.needsYes != nil {
		specs = append(specs, flagSpec{"yes", kindBool, "", "confirm that the command deletes or revokes"})
	}
	for _, f := range specs {
		switch {
		case f.kind == kindString && f.path == "":
			in.flags[f.name] = fs.String(f.name, "", f.usage)
		case f.kind == kindBool && f.path == "":
			in.bools[f.name] = fs.Bool(f.name, false, f.usage)
		case f.kind == kindBool:
			fs.BoolFunc(f.name, f.usage, func(v string) error { return fill(in.body, f, v) })
		default:
			fs.Func(f.name, f.usage, func(v string) error { return fill(in.body, f, v) })
		}
	}

	pos, err := parse(fs, rest)
	switch {
	case errors.Is(err, flag.ErrHelp):
		say(stdout, "usage: %s [flags]\n", strings.Join(append([]string{name}, cmd.args...), " "))
		fs.SetOutput(stdout)
		fs.PrintDefaults()
		return exitOK
	case err != nil:
		say(stderr, "%s: %v\n", name, err)
		return exitUsage
	case len(pos) != len(cmd.args):
		say(stderr, "%s: takes %d positional argument(s), got %d\n", name, len(cmd.args), len(pos))
		return exitUsage
	}
	in.args = pos
	fs.Visit(func(f *flag.Flag) { in.set[f.Name] = true })

	ctx := context.Background()
	if cmd.local != nil {
		err := cmd.local(ctx, in, stdout)
		switch {
		case errors.Is(err, errRemote):
		case errors.Is(err, errUsage):
			say(stderr, "%s: %v\n", name, err)
			return exitUsage
		case err != nil:
			say(stderr, "%s: %v\n", name, err)
			return exitFailure
		default:
			return exitOK
		}
	}
	if cmd.needsYes != nil && cmd.needsYes(in) && !*in.bools["yes"] {
		say(stderr, "%s: this deletes or revokes; give --yes to confirm\n", name)
		return exitUsage
	}
	method, path, body := cmd.request(in)
	return call(ctx, name, in, method, path, body, stdout, stderr)
}

// match returns the command whose words lead args, and the arguments after them.
func match(args []string) (*command, []string) {
	for i := range commands {
		n := len(commands[i].words)
		if len(args) >= n && slices.Equal(commands[i].words, args[:n]) {
			return &commands[i], args[n:]
		}
	}
	return nil, nil
}

// parse reads flags written before, between and after the positional
// arguments, which it returns: the flag package stops at the first positional.
// The first "--" ends flag parsing and is never taken as the value of a flag;
// "--flag=--" passes that literal.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos, tail []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, tail = args[:i], args[i+1:]
	}
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if args = fs.Args(); len(args) > 0 {
			pos, args = append(pos, args[0]), args[1:]
		}
	}
	return append(pos, tail...), nil
}

func version(_ context.Context, in input, stdout io.Writer) error {
	if in.json {
		return json.NewEncoder(stdout).Encode(map[string]string{"version": Version})
	}
	_, err := fmt.Fprintln(stdout, Version)
	return err
}

func usage(w io.Writer) {
	say(w, "usage: stargaite <command> [arguments] [flags]\n\ncommands:\n  serve\n")
	for _, c := range commands {
		say(w, "  %s\n", strings.Join(append(slices.Clone(c.words), c.args...), " "))
	}
}

// say writes a message for a person. A failed write has nowhere to be reported.
func say(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}
