// Command shim stands in for every external command setup.sh runs in the
// tests of tests/setup (see setup_test.go), which link it under each name:
// it appends a record of the call to $SETUP_LOG, then answers as a fake
// (docker, curl, id, stat, sleep) or runs the real command; FAKE_TIMEOUT
// replaces the 1200 s bound given to the real timeout. A separate
// binary, built once without the race detector, so each call starts fast.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jorlaru/stargaite/internal/presets"
)

// record is one shim call: its argv, its working directory and, for a curl
// given -K -, the config it read from standard input.
type record struct {
	Argv  []string `json:"argv"`
	Dir   string   `json:"dir"`
	Stdin string   `json:"stdin,omitempty"`
}

// state is what the fake docker knows between calls: whether the container
// exists, the variable names of .env it was last created with, and the
// configuration the fake CLI holds: budgets by scope (instance, alias:<name>
// or key:<name>) and the telemetry flags given since the last unset, merged
// as telemetry set merges its patch.
type state struct {
	Up        bool                         `json:"up"`
	Env       []string                     `json:"env"`
	Providers map[string]map[string]string `json:"providers"`
	Targets   map[string]map[string]any    `json:"targets"`
	Models    map[string][]string          `json:"models"`
	Keys      []string                     `json:"keys"`
	Budgets   map[string]string            `json:"budgets"`
	Telemetry map[string]string            `json:"telemetry"`
}

func main() {
	os.Exit(shim(filepath.Base(os.Args[0]), os.Args[1:]))
}

// shim runs as the command name: it appends its record to $SETUP_LOG, then
// answers as a fake or runs the real command.
func shim(name string, args []string) int {
	rec := record{Argv: append([]string{name}, args...)}
	rec.Dir, _ = os.Getwd()
	if name == "curl" && slices.Contains(args, "-K") {
		in, _ := io.ReadAll(os.Stdin)
		rec.Stdin = string(in)
	}
	line, _ := json.Marshal(rec)
	f, err := os.OpenFile(os.Getenv("SETUP_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "shim:", err)
		return 127
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
	switch {
	case name == "docker":
		return fakeDocker(args)
	case name == "curl":
		return fakeCurl(args)
	case name == "id" && slices.Equal(args, []string{"-u"}):
		fmt.Println(env("FAKE_UID", "1000"))
		return 0
	case name == "stat" && slices.Equal(args, []string{"-c", "%u", "."}):
		fmt.Println(env("FAKE_OWNER", "1000"))
		return 0
	case name == "sleep":
		return 0
	case name == "timeout" && os.Getenv("FAKE_TIMEOUT") != "":
		if i := slices.Index(args, "1200"); i >= 0 {
			args[i] = os.Getenv("FAKE_TIMEOUT")
		}
	}
	// A pass-through: the real command, found on the test's own PATH, run
	// with this environment, so a command it runs in turn is a shim too.
	path := os.Getenv("PATH")
	_ = os.Setenv("PATH", os.Getenv("SETUP_REAL_PATH"))
	real, err := exec.LookPath(name)
	_ = os.Setenv("PATH", path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "shim:", err)
		return 127
	}
	err = syscall.Exec(real, append([]string{name}, args...), os.Environ())
	fmt.Fprintln(os.Stderr, "shim:", err)
	return 127
}

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func loadState() *state {
	st := &state{Providers: map[string]map[string]string{}, Targets: map[string]map[string]any{}, Models: map[string][]string{},
		Budgets: map[string]string{}, Telemetry: map[string]string{}}
	if data, err := os.ReadFile(os.Getenv("SETUP_STATE")); err == nil {
		_ = json.Unmarshal(data, st)
	}
	return st
}

func (st *state) save() {
	data, _ := json.Marshal(st)
	_ = os.WriteFile(os.Getenv("SETUP_STATE"), data, 0o600)
}

// envNames are the variable names .env in the working directory sets.
func envNames() []string {
	var names []string
	f, err := os.Open(".env")
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	for s := bufio.NewScanner(f); s.Scan(); {
		if name, _, ok := strings.Cut(s.Text(), "="); ok {
			names = append(names, name)
		}
	}
	return names
}

// fakeDocker answers what setup.sh asks Docker and Compose. A container is
// created by up, which takes the variable names of .env at that moment, as
// Compose gives the container the file's values when it creates it.
// FAKE_UP=hang makes up last 30 s, a build that takes long; FAKE_UP=noenv
// gives the container none of the variables of .env. FAKE_DOCKER=denied
// fails info and ps as a user outside the docker group sees;
// FAKE_DOCKER=unreadable fails ps alone, as a .env Compose cannot parse does.
func fakeDocker(args []string) int {
	st := loadState()
	defer st.save()
	switch {
	case len(args) > 0 && args[0] == "info":
		switch os.Getenv("FAKE_DOCKER") {
		case "denied":
			fmt.Fprintln(os.Stderr, "permission denied while trying to connect to the docker API at unix:///var/run/docker.sock")
			return 1
		case "down":
			fmt.Fprintln(os.Stderr, "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")
			return 1
		}
		fmt.Println("28.5.1")
	case len(args) < 2 || args[0] != "compose":
		fmt.Fprintln(os.Stderr, "fake docker: not handled:", args)
		return 1
	case args[1] == "version":
		fmt.Println(env("FAKE_COMPOSE", "2.29.1"))
	case args[1] == "ps" && os.Getenv("FAKE_DOCKER") == "denied":
		fmt.Fprintln(os.Stderr, "permission denied while trying to connect to the docker API at unix:///var/run/docker.sock")
		return 1
	case args[1] == "ps" && os.Getenv("FAKE_DOCKER") == "unreadable":
		fmt.Fprintln(os.Stderr, "failed to read .env: line 1: unterminated quoted value 'sk-hand-edited-secret")
		return 1
	case args[1] == "ps":
		if st.Up {
			fmt.Println("c0ffee")
		}
	case args[1] == "up":
		if os.Getenv("FAKE_UP") == "hang" {
			fmt.Println("fake build: 30 s")
			time.Sleep(30 * time.Second)
		}
		st.Up, st.Env = true, envNames()
		if os.Getenv("FAKE_UP") == "noenv" {
			st.Env = nil
		}
		fmt.Println(" Container fake-stargaite-1 Started")
	case args[1] == "logs":
		fmt.Println("a log line")
	case args[1] == "exec" && slices.Equal(args[4:], []string{"/stargaite", "version", "--json"}):
		fmt.Println(`{"version":"v0.6.0-fake"}`)
	case args[1] == "exec" && len(args) > 4 && args[4] == "/stargaite":
		out, code := fakeCLI(st, args[5:])
		fmt.Println(out)
		return code
	default:
		fmt.Fprintln(os.Stderr, "fake docker: not handled:", args)
		return 1
	}
	return 0
}

// cliArgs splits the arguments after the command words: flags by name, each
// with its values in order, and the positionals after "--".
func cliArgs(args []string) (map[string][]string, []string) {
	flags := map[string][]string{}
	for i := 0; i < len(args); i++ {
		a := strings.TrimPrefix(args[i], "--")
		switch {
		case args[i] == "--":
			return flags, args[i+1:]
		case a == "json" || a == "allow-private-network":
			flags[a] = append(flags[a], "true")
		case strings.Contains(a, "="):
			name, v, _ := strings.Cut(a, "=")
			flags[name] = append(flags[name], v)
		case i+1 < len(args):
			flags[a] = append(flags[a], args[i+1])
			i++
		}
	}
	return flags, nil
}

func encode(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func failure(message string) (string, int) {
	return encode(map[string]any{"error": map[string]any{"message": message, "type": "invalid_request_error"}}), 1
}

func written(resource any) string {
	return encode(map[string]any{"revision": 1, "changed": true, "diff": []any{}, "findings": []any{}, "resource": resource})
}

// fakeCLI answers the CLI commands setup.sh runs, in the shapes of the admin
// API's --json answers.
func fakeCLI(st *state, args []string) (string, int) {
	// telemetry test without --json prints the line the CLI renders (TEL-2).
	if slices.Equal(args, []string{"telemetry", "test"}) {
		switch {
		case st.Telemetry["otlp-endpoint"] == "":
			return "stargaite: no exporter is configured: telemetry set --otlp-endpoint <url> sets one", 1
		case len(st.unresolved()) > 0:
			return "credentials: unresolved · credential_unresolved: " + strings.Join(st.unresolved(), ", ") + " does not resolve", 1
		case os.Getenv("FAKE_TELEMETRY") == "unauthorized":
			return "credentials: resolved · reachable: yes · authorized: no (401) · bad credentials", 1
		}
		return "credentials: resolved · reachable: yes · authorized: yes · span: accepted (200) · trace_id=4bf92f3577b34da6a3ce929d0e0e4736", 0
	}
	if len(args) < 3 || args[2] != "--json" {
		return failure(fmt.Sprintf("fake CLI: --json is not the third argument: %q", args))
	}
	flags, pos := cliArgs(args[3:])
	view := func(name string) map[string]any {
		p := st.Providers[name]
		v := map[string]any{"name": name, "protocol": p["protocol"], "base_url": p["base_url"], "billing": p["billing"], "credential": "none"}
		if p["api_key_env"] != "" {
			v["api_key_env"], v["credential"] = p["api_key_env"], "unresolved"
			if slices.Contains(st.Env, p["api_key_env"]) {
				v["credential"] = "resolved"
			}
		}
		return v
	}
	one := func(name string) string { return encode(view(name)) }
	switch args[0] + " " + args[1] {
	case "providers presets":
		return encode(map[string]any{"presets": presets.Catalog}), 0
	case "providers list":
		list := []any{}
		for name := range st.Providers {
			list = append(list, view(name))
		}
		return encode(map[string]any{"providers": list}), 0
	case "providers show":
		if _, ok := st.Providers[pos[0]]; !ok {
			return failure("provider does not exist")
		}
		return one(pos[0]), 0
	case "providers add":
		name := pos[0]
		if _, ok := st.Providers[name]; ok {
			return failure(fmt.Sprintf("provider %q exists", name))
		}
		p := map[string]string{}
		for _, pr := range presets.Catalog {
			if pr.Name == name {
				p = map[string]string{"protocol": string(pr.Protocol), "base_url": pr.BaseURL, "api_key_env": pr.APIKeyEnv, "billing": string(pr.Billing)}
			}
		}
		for flag, member := range map[string]string{"protocol": "protocol", "base-url": "base_url", "api-key-env": "api_key_env", "billing": "billing"} {
			if v := flags[flag]; len(v) > 0 {
				p[member] = v[len(v)-1]
			}
		}
		if p["base_url"] == "" || p["protocol"] == "" {
			return failure("a provider needs a protocol and a base_url")
		}
		st.Providers[name] = p
		return written(view(name)), 0
	case "settings show":
		return encode(map[string]any{"timeouts": map[string]int{"connect_ms": 5000, "first_byte_ms": 30000, "stream_idle_ms": 120000, "total_ms": 180000},
			"budget": st.budget("instance")}), 0
	case "models show", "keys show":
		scope := map[string]string{"models": "alias:", "keys": "key:"}[args[0]] + pos[0]
		if _, ok := st.Models[pos[0]]; !ok && !slices.Contains(st.Keys, pos[0]) {
			return failure(args[0] + " " + pos[0] + " does not exist")
		}
		return encode(map[string]any{"name": pos[0], "budget": st.budget(scope)}), 0
	case "targets list":
		list := []any{}
		for _, t := range st.Targets {
			list = append(list, t)
		}
		return encode(map[string]any{"targets": list}), 0
	case "targets add":
		if _, ok := st.Targets[pos[0]]; ok {
			return failure(fmt.Sprintf("target %q exists", pos[0]))
		}
		st.Targets[pos[0]] = map[string]any{"id": pos[0], "kind": flags["kind"][0]}
		return written(st.Targets[pos[0]]), 0
	case "targets verify":
		if os.Getenv("FAKE_VERIFY") == "fail" {
			return encode(map[string]any{"target": pos[0], "ok": false, "stored": true, "calls": []any{
				map[string]any{"effort": "", "status": 404, "output_tokens": 0, "provider_message": "model not found: " + pos[0]}}}), 1
		}
		st.Targets[pos[0]]["verified_at"] = 1767225600000
		return encode(map[string]any{"target": pos[0], "ok": true, "stored": true, "calls": []any{
			map[string]any{"effort": "", "status": 200, "output_tokens": 2}}}), 0
	case "models list":
		list := []any{}
		for name, targets := range st.Models {
			list = append(list, map[string]any{"name": name, "targets": targets})
		}
		return encode(map[string]any{"models": list}), 0
	case "models add":
		st.Models[pos[0]] = flags["target"]
		return written(map[string]any{"name": pos[0], "targets": flags["target"]}), 0
	case "keys list":
		list := []any{}
		for _, name := range st.Keys {
			list = append(list, map[string]any{"name": name, "models": []string{"main"}, "secrets": []any{}})
		}
		return encode(map[string]any{"keys": list}), 0
	case "keys create":
		st.Keys = append(st.Keys, pos[0])
		return encode(map[string]any{"revision": 2, "changed": true, "diff": []any{}, "findings": []any{},
			"secrets": map[string]string{pos[0]: "sk-stargaite-fake-" + pos[0]}, "resource": map[string]any{"name": pos[0]}}), 0
	case "settings set", "models set", "keys set":
		return fakeBudget(st, args[0], flags, pos)
	case "targets set":
		t, ok := st.Targets[pos[0]]
		if !ok {
			return failure("target does not exist")
		}
		t["pricing"] = flags["pricing"][0]
		return written(t), 0
	case "telemetry set":
		if e := flags["otlp-endpoint"][0]; !strings.HasPrefix(e, "http://") && !strings.HasPrefix(e, "https://") {
			return failure("url scheme must be http or https")
		}
		for flag, v := range flags {
			if flag != "json" {
				st.Telemetry[flag] = v[len(v)-1]
			}
		}
		return written(st.Telemetry), 0
	case "telemetry unset":
		if !slices.Contains(args, "--yes") {
			return failure("telemetry unset needs --yes")
		}
		st.Telemetry = map[string]string{}
		return encode(map[string]any{"revision": 1, "changed": true, "diff": []any{}, "findings": []any{}}), 0
	case "telemetry status":
		v := map[string]any{"telemetry": nil, "unresolved": st.unresolved()}
		if e := st.Telemetry["otlp-endpoint"]; e != "" {
			t := map[string]any{"otlp_endpoint": e}
			if st.Telemetry["preset"] == "langfuse" {
				t["basic_auth_env"] = []string{"LANGFUSE_PUBLIC_KEY", "LANGFUSE_SECRET_KEY"}
			}
			if name, ref, ok := strings.Cut(st.Telemetry["header-env"], "="); ok {
				t["header_env"] = map[string]string{name: ref}
			}
			v["telemetry"] = t
		}
		return encode(v), 0
	}
	return failure(fmt.Sprintf("fake CLI: not handled: %q", args))
}

// fakeCurl answers the readiness check, a provider's model list and the
// first answer, whose headers it puts first with -D -, the names as Go's
// server writes them over HTTP/1.1. FAKE_MODELS picks the list's answer (list, error, empty or
// nonjson); FAKE_CHAT=fail fails the chat completion with a 502, and
// FAKE_CHAT=timeout fails curl itself.
func fakeCurl(args []string) int {
	var url, format string
	quiet, headers := false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o":
			quiet = true
			i++
		case "-D":
			headers = true
			i++
		case "--max-time", "-K", "-H", "-d":
			i++
		case "-w":
			format = args[i+1]
			i++
		default:
			if !strings.HasPrefix(args[i], "-") {
				url = args[i]
			}
		}
	}
	body, code := "", "200"
	switch {
	case strings.HasSuffix(url, "/health/ready"):
		if os.Getenv("FAKE_READY") == "never" {
			fmt.Fprintln(os.Stderr, "curl: (7) Failed to connect")
			return 7
		}
		body = "ready"
	case strings.Contains(url, "/models"):
		switch os.Getenv("FAKE_MODELS") {
		case "error":
			fmt.Fprintln(os.Stderr, "curl: (22) The requested URL returned error: 401")
			return 22
		case "empty":
			body = `{"data":[]}`
		case "nonjson":
			body = "<html>not a list</html>"
		default:
			body = `{"object":"list","data":[{"id":"gpt-a"},{"id":"gpt-b"},{"id":"other-model"}]}`
		}
	case strings.HasSuffix(url, "/chat/completions"):
		body = `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"fake reply"},"finish_reason":"stop"}]}`
		switch os.Getenv("FAKE_CHAT") {
		case "fail":
			body, code = `{"error":{"message":"the provider failed","code":"provider_error"}}`, "502"
		case "timeout":
			fmt.Fprintln(os.Stderr, "curl: (28) Operation timed out after 30001 milliseconds with 0 bytes received")
			return 28
		}
	default:
		fmt.Fprintln(os.Stderr, "fake curl: not handled:", url)
		return 6
	}
	if headers {
		fmt.Print("HTTP/1.1 " + code + " X\r\nContent-Type: application/json\r\nX-Stargaite-Target: fake/target\r\n\r\n")
	}
	if !quiet {
		fmt.Print(body)
	}
	fmt.Print(strings.NewReplacer(`\n`, "\n", "%{http_code}", code, "%{time_total}", "0.012").Replace(format))
	return 0
}

// budget is the budget set on scope, as the API shows it, or nil.
func (st *state) budget(scope string) any {
	pairs, ok := st.Budgets[scope]
	if !ok {
		return nil
	}
	b := map[string]any{}
	for _, pair := range strings.Split(pairs, ",") {
		name, v, _ := strings.Cut(pair, "=")
		b[name] = v
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			b[name] = n
		}
	}
	return b
}

// unresolved are the variables the telemetry flags reference that the
// container does not have.
func (st *state) unresolved() []string {
	var refs, missing []string
	if st.Telemetry["preset"] == "langfuse" {
		refs = append(refs, "LANGFUSE_PUBLIC_KEY", "LANGFUSE_SECRET_KEY")
	}
	if _, v, ok := strings.Cut(st.Telemetry["header-env"], "="); ok {
		refs = append(refs, v)
	}
	for _, r := range refs {
		if !slices.Contains(st.Env, r) {
			missing = append(missing, r)
		}
	}
	return missing
}

// fakeBudget sets the budget of settings, models or keys set. As USE-12 does,
// it refuses one that reaches a pay-per-use target without prices, naming
// each in an unpriced_under_budget finding: an alias's budget reaches the
// targets of that alias, the instance's or a key's those of every alias.
func fakeBudget(st *state, kind string, flags map[string][]string, pos []string) (string, int) {
	scope, aliases := "instance", slices.Collect(maps.Keys(st.Models))
	switch kind {
	case "models":
		if _, ok := st.Models[pos[0]]; !ok {
			return failure("alias does not exist")
		}
		scope, aliases = "alias:"+pos[0], pos[:1]
	case "keys":
		if !slices.Contains(st.Keys, pos[0]) {
			return failure("key does not exist")
		}
		scope = "key:" + pos[0]
	}
	ids := map[string]bool{}
	for _, a := range aliases {
		for _, id := range st.Models[a] {
			provider, _, _ := strings.Cut(id, "/")
			if st.Providers[provider]["billing"] == "pay_per_use" && st.Targets[id]["pricing"] == nil {
				ids[id] = true
			}
		}
	}
	findings := []any{}
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		findings = append(findings, map[string]any{"severity": "error", "rule": "USE-12", "code": "unpriced_under_budget", "resource": "targets/" + id,
			"field": "pricing", "message": "pay-per-use target with neither prices nor a provider cost report, reachable under the budget of " + scope})
	}
	if len(findings) > 0 {
		return encode(map[string]any{"error": map[string]any{"message": "the change is refused: the configuration it leads to has errors",
			"type": "invalid_request_error", "findings": findings}}), 1
	}
	st.Budgets[scope] = flags["budget"][0]
	return written(map[string]any{"budget": flags["budget"][0]}), 0
}
