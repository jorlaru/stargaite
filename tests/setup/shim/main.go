// Command shim stands in for every external command setup.sh and the
// console run in the tests of tests/setup (see setup_test.go), which link it
// under each name: it appends a record of the call to $SETUP_LOG, then
// answers as a fake (docker, id, stat -c %u ., sleep, chown, hostname,
// getent), refuses (curl, which nothing runs since C64b) or runs the real
// command; FAKE_TIMEOUT replaces the 1200 s bound given to the real timeout.
// Run as stargaite-plane, it is the fake container's data plane.
// A separate binary, built once without the race detector, so each call
// starts fast.
package main

import (
	"bufio"
	"cmp"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
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

// record is one shim call: its argv and its working directory.
type record struct {
	Argv []string `json:"argv"`
	Dir  string   `json:"dir"`
}

// state is what the fake docker knows between calls: whether the container
// runs, or exists stopped, the variable names of .env it was last created
// with, whether its override gave serve a certificate, so both listeners
// speak TLS, the override it was last started with and the certificate and
// key it serves, read from tls/ only when it starts anew, as serve does, the
// version the image was last built with and the one the container runs, and
// the configuration the fake CLI holds: budgets by scope (instance,
// alias:<name> or key:<name>) and the telemetry flags given since the last
// unset, merged as telemetry set merges its patch; Plane is the pid of the
// running container's data plane.
type state struct {
	Plane     int                          `json:"plane"`
	Built     string                       `json:"built"`
	Version   string                       `json:"version"`
	Up        bool                         `json:"up"`
	Stopped   bool                         `json:"stopped"`
	TLS       bool                         `json:"tls"`
	Override  string                       `json:"override"`
	Cert      string                       `json:"cert"`
	Key       string                       `json:"key"`
	Env       []string                     `json:"env"`
	Providers map[string]map[string]string `json:"providers"`
	Targets   map[string]map[string]any    `json:"targets"`
	Models    map[string][]string          `json:"models"`
	Keys      []string                     `json:"keys"`
	Budgets   map[string]string            `json:"budgets"`
	Telemetry map[string]string            `json:"telemetry"`
}

func main() {
	if filepath.Base(os.Args[0]) == "stargaite-plane" {
		os.Exit(plane(os.Args[1], strings.Fields(os.Args[2])))
	}
	os.Exit(shim(filepath.Base(os.Args[0]), os.Args[1:]))
}

// The chat completion the data plane answers, and its answer with
// FAKE_CHAT=fail, as a 502.
const (
	chatReply   = `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"fake reply"},"finish_reason":"stop"}]}`
	chatFailure = `{"error":{"message":"the provider failed","code":"provider_error"}}`
)

// appendLine appends line and a newline to the file at path.
func appendLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return errors.Join(err, f.Close())
}

// shim runs as the command name: it appends its record to $SETUP_LOG, then
// answers as a fake or runs the real command.
func shim(name string, args []string) int {
	rec := record{Argv: append([]string{name}, args...)}
	rec.Dir, _ = os.Getwd()
	line, _ := json.Marshal(rec)
	if err := appendLine(os.Getenv("SETUP_LOG"), line); err != nil {
		fmt.Fprintln(os.Stderr, "shim:", err)
		return 127
	}
	switch {
	case name == "docker":
		return fakeDocker(args)
	case name == "curl":
		fmt.Fprintln(os.Stderr, "shim: setup.sh runs no curl since C64b")
		return 1
	case name == "id" && slices.Equal(args, []string{"-u"}):
		fmt.Println(env("FAKE_UID", "1000"))
		return 0
	case name == "id" && slices.Equal(args, []string{"-un"}):
		fmt.Println("me")
		return 0
	case name == "stat" && slices.Equal(args, []string{"-c", "%u", "."}):
		fmt.Println(env("FAKE_OWNER", "1000"))
		return 0
	case name == "hostname" && slices.Equal(args, []string{"-I"}):
		fmt.Println(env("FAKE_ADDRESSES", "127.0.0.2 127.0.0.3 "))
		return 0
	case name == "getent" && len(args) > 0:
		// FAKE_GETENT_PASSWD lists the users; FAKE_GETENT_GROUP is the line
		// of one group, printed only for its own gid.
		out := env("FAKE_GETENT_"+strings.ToUpper(args[0]), "")
		if len(args) == 1 || strings.Contains(out, ":"+args[1]+":") {
			fmt.Print(out)
		}
		return 0
	case name == "sleep", name == "chown":
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

// save writes the state through a rename, so a call running at the same
// time, as the console's concurrent reads do, never reads it half written.
func (st *state) save() {
	data, _ := json.Marshal(st)
	path := os.Getenv("SETUP_STATE")
	tmp := path + ".tmp" + strconv.Itoa(os.Getpid())
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
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

// envPort is the port .env in the working directory publishes, 4000
// without its STARGAITE_PORT line, as compose.yaml reads it.
func envPort() string {
	data, _ := os.ReadFile(".env")
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "STARGAITE_PORT="); ok {
			return strings.Trim(v, `'"`)
		}
	}
	return "4000"
}

// published is the addresses the override's "# setup: addresses=" line
// publishes on, 127.0.0.1 without it.
func published(override string) string {
	for _, line := range strings.Split(override, "\n") {
		if v, ok := strings.CutPrefix(line, "# setup: addresses="); ok {
			return v
		}
	}
	return "127.0.0.1"
}

// startPlane starts the container's data plane, after stopping the one it
// replaces: this binary as stargaite-plane, in its own session, so a console
// that kills its command's process group leaves it running, as a container
// outlives the docker command that started it. It listens on the addresses
// the override publishes, at .env's port, serving the certificate and key
// the container started with when it speaks TLS.
func (st *state) startPlane() {
	st.stopPlane()
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := &exec.Cmd{Path: exe, Args: []string{"stargaite-plane", envPort(), published(st.Override)}, SysProcAttr: &syscall.SysProcAttr{Setsid: true}}
	if st.TLS {
		cmd.Env = append(os.Environ(), "PLANE_CERT="+st.Cert, "PLANE_KEY="+st.Key)
	}
	if cmd.Start() == nil {
		st.Plane = cmd.Process.Pid
		_ = cmd.Process.Release()
	}
}

func (st *state) stopPlane() {
	if st.Plane > 0 {
		_ = syscall.Kill(st.Plane, syscall.SIGKILL)
	}
	st.Plane = 0
}

// plane serves the data plane on each of addrs at port, as a running
// container does: /health/ready, and a chat completion, each appended to
// $SETUP_STATE.chat as its Authorization and body; over TLS with the pair of
// $PLANE_CERT and $PLANE_KEY when given. Every request is appended to
// $SETUP_STATE.plane as its local address, server name and path. It waits
// up to 2 s for each address, which the plane it replaces may still hold,
// and exits once $SETUP_STATE is gone, or after 10 minutes: a test killed by
// its timeout runs no cleanup.
func plane(port string, addrs []string) int {
	srv := &http.Server{Handler: http.HandlerFunc(planeAnswer), ReadHeaderTimeout: 10 * time.Second}
	if cert := os.Getenv("PLANE_CERT"); cert != "" {
		pair, err := tls.X509KeyPair([]byte(cert), []byte(os.Getenv("PLANE_KEY")))
		if err != nil {
			return 1
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{pair}}
	}
	for _, a := range addrs {
		var ln net.Listener
		var err error
		for end := time.Now().Add(2 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if ln, err = net.Listen("tcp", net.JoinHostPort(a, port)); err == nil || time.Now().After(end) {
				break
			}
		}
		if err != nil {
			return 1
		}
		if srv.TLSConfig != nil {
			ln = tls.NewListener(ln, srv.TLSConfig)
		}
		go func() { _ = srv.Serve(ln) }()
	}
	for end := time.Now().Add(10 * time.Minute); time.Now().Before(end); time.Sleep(time.Second) {
		if _, err := os.Stat(os.Getenv("SETUP_STATE")); err != nil {
			break
		}
	}
	return 0
}

// planeAnswer is the data plane's handler. FAKE_CHAT=fail fails the chat
// completion with a 502, and FAKE_CHAT=timeout never answers it.
func planeAnswer(w http.ResponseWriter, r *http.Request) {
	sni := ""
	if r.TLS != nil {
		sni = r.TLS.ServerName
	}
	hit, _ := json.Marshal(map[string]string{"addr": r.Context().Value(http.LocalAddrContextKey).(net.Addr).String(), "sni": sni, "path": r.URL.Path})
	_ = appendLine(os.Getenv("SETUP_STATE")+".plane", hit)
	switch {
	case r.URL.Path == "/health/ready":
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		line, _ := json.Marshal(map[string]string{"auth": r.Header.Get("Authorization"), "body": string(body)})
		_ = appendLine(os.Getenv("SETUP_STATE")+".chat", line)
		w.Header().Set("X-Stargaite-Target", "fake/target")
		switch os.Getenv("FAKE_CHAT") {
		case "fail":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, chatFailure)
		case "timeout":
			<-r.Context().Done()
		default:
			_, _ = io.WriteString(w, chatReply)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// fakeDocker answers what setup.sh asks Docker and Compose. A container is
// created by up, which takes the variable names of .env at that moment, as
// Compose gives the container the file's values when it creates it.
// A container that runs serves its data plane (startPlane); down and stop
// end it. FAKE_UP=hang makes up last 30 s, a build that takes long; FAKE_UP=noenv
// gives the container none of the variables of .env; FAKE_UP=fail leaves it
// stopped, as Compose does when the new one cannot bind its port. Compose
// starts the container anew, and serve reads tls/cert.pem, when none runs,
// when the override changed, or with --force-recreate. Once the container
// speaks TLS, a CLI call without the admin URL of section 2 fails, as the
// real one would. FAKE_SECURITY is what docker info gives as its security
// options (rootless Docker names rootless). FAKE_DOCKER=denied
// fails info and ps as a user outside the docker group sees;
// FAKE_DOCKER=unreadable fails ps alone, as a .env Compose cannot parse does.
// An upgrade: build takes the version from STARGAITE_VERSION, which the
// container runs from its next up; FAKE_UPGRADE=build-fails fails the build,
// start-fails the up after it, invalid makes validate fail on the new
// version, backup-short cuts backup - short, and legacy is a CLI from before
// restore. FAKE_VERSION is the version before any build. FAKE_BUILDX=missing
// is a Docker without its buildx component; the console's export is
// fakeExport.
func fakeDocker(args []string) int {
	st := loadState()
	defer st.save()
	upgrade := os.Getenv("FAKE_UPGRADE")
	switch {
	case len(args) > 0 && args[0] == "inspect":
		fmt.Println("sha256:0123abcd", env("STARGAITE_IMAGE", "stargaite:local"))
	case len(args) > 0 && args[0] == "tag": // recorded, nothing to answer
	case len(args) > 0 && args[0] == "info":
		switch os.Getenv("FAKE_DOCKER") {
		case "denied":
			fmt.Fprintln(os.Stderr, "permission denied while trying to connect to the docker API at unix:///var/run/docker.sock")
			return 1
		case "down":
			fmt.Fprintln(os.Stderr, "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")
			return 1
		}
		if slices.Contains(args, "{{.SecurityOptions}}") {
			fmt.Println(env("FAKE_SECURITY", "[name=seccomp,profile=builtin name=cgroupns]"))
			return 0
		}
		fmt.Println("28.5.1")
	case slices.Equal(args, []string{"buildx", "version"}):
		if os.Getenv("FAKE_BUILDX") == "missing" {
			fmt.Fprintln(os.Stderr, "docker: unknown command: docker buildx")
			return 1
		}
		fmt.Println("github.com/docker/buildx v0.39.0-fake")
	case len(args) > 0 && args[0] == "build" && slices.Contains(args, "console"):
		return fakeExport(args)
	case len(args) < 2 || args[0] != "compose":
		fmt.Fprintln(os.Stderr, "fake docker: not handled:", args)
		return 1
	case args[1] == "version":
		fmt.Println(env("FAKE_COMPOSE", "2.29.1"))
	case slices.Equal(args[1:], []string{"config", "--format", "json"}):
		// As Compose prints it, with the environment env_file gives in clear;
		// the project is the directory's base name, lower-cased.
		dir, _ := os.Getwd()
		project := strings.ToLower(filepath.Base(dir))
		fmt.Printf(`{"name":%q,"services":{"stargaite":{"image":%q,"environment":{"SHIM_SECRET":"sk-SHIM-SECRET"}}},`+
			`"volumes":{"stargaite-data":{"name":%q}}}`+"\n", project, env("STARGAITE_IMAGE", "stargaite:local"), project+"_stargaite-data")
	case args[1] == "build":
		if upgrade == "build-fails" {
			fmt.Fprintln(os.Stderr, "failed to solve: process did not complete successfully: exit code: 1")
			return 1
		}
		st.Built = os.Getenv("STARGAITE_VERSION")
	case args[1] == "ps" && os.Getenv("FAKE_DOCKER") == "denied":
		fmt.Fprintln(os.Stderr, "permission denied while trying to connect to the docker API at unix:///var/run/docker.sock")
		return 1
	case args[1] == "ps" && os.Getenv("FAKE_DOCKER") == "unreadable":
		fmt.Fprintln(os.Stderr, "failed to read .env: line 1: unterminated quoted value 'sk-hand-edited-secret")
		return 1
	case args[1] == "ps":
		if st.Up || st.Stopped && slices.Contains(args, "-a") {
			fmt.Println("c0ffee")
		}
	case args[1] == "up":
		if os.Getenv("FAKE_UP") == "hang" {
			fmt.Println("fake build: 30 s")
			time.Sleep(30 * time.Second)
		}
		if os.Getenv("FAKE_UP") == "fail" || upgrade == "start-fails" && st.Built != "" {
			st.Up, st.Stopped = false, true
			st.stopPlane()
			fmt.Fprintln(os.Stderr, "Error response from daemon: failed to bind host port: address already in use")
			return 1
		}
		override, _ := os.ReadFile("compose.override.yaml")
		if !st.Up || string(override) != st.Override || slices.Contains(args, "--force-recreate") {
			cert, _ := os.ReadFile("tls/cert.pem")
			key, _ := os.ReadFile("tls/key.pem")
			st.Cert, st.Key = string(cert), string(key)
		}
		st.Up, st.Stopped, st.Env, st.Override = true, false, envNames(), string(override)
		if st.Built != "" {
			st.Version = st.Built
		}
		st.TLS = strings.Contains(string(override), `"--tls-cert"`)
		if os.Getenv("FAKE_UP") == "noenv" {
			st.Env = nil
		}
		st.startPlane()
		fmt.Println(" Container fake-stargaite-1 Started")
	case args[1] == "down" || args[1] == "stop":
		st.stopPlane()
		st.Up, st.Stopped = false, args[1] == "stop"
	case args[1] == "logs":
		fmt.Println("a log line")
		if slices.Contains(args, "--no-log-prefix") {
			fmt.Printf(`{"level":"INFO","msg":"ready","version":%q}`+"\n", st.Version)
		}
	case args[1] == "exec" && slices.Contains(args, "/stargaite"):
		i := slices.Index(args, "/stargaite")
		if st.TLS && !slices.ContainsFunc(args[:i], func(a string) bool { return strings.HasPrefix(a, "STARGAITE_ADMIN_URL=https://") }) {
			fmt.Println(`{"error":{"message":"the admin API at http://127.0.0.1:4001 did not answer: it speaks TLS","type":"api_error"}}`)
			return 1
		}
		switch strings.Join(args[i+1:], " ") {
		case "version --json":
			fmt.Printf(`{"version":%q}`+"\n", cmp.Or(st.Version, env("FAKE_VERSION", "v0.6.0-fake")))
			return 0
		case "--help":
			fmt.Print("usage: stargaite <command> [arguments] [flags]\n\ncommands:\n  serve\n  backup file\n")
			if upgrade != "legacy" {
				fmt.Print("  restore file\n")
			}
			return 0
		case "export --json":
			fmt.Println(st.export())
			return 0
		case "validate --json":
			if upgrade == "invalid" && st.Built != "" && st.Version == st.Built {
				fmt.Println(`{"findings":[{"severity":"error","rule":"R","resource":"targets/x","message":"refused"}]}`)
				return 1
			}
			fmt.Println(`{"findings":[]}`)
			return 0
		case "backup - --json":
			if upgrade == "backup-short" {
				fmt.Print("SQLite")
				fmt.Fprintln(os.Stderr, `{"error":{"message":"the copy on standard output is not whole: unexpected EOF","type":"api_error","code":"internal_error"}}`)
				return 1
			}
			fmt.Print("SQLite format 3\x00 the store")
			fmt.Fprintln(os.Stderr, `{"bytes":26,"path":"-"}`)
			return 0
		}
		out, code := fakeCLI(st, args[i+1:])
		fmt.Println(out)
		return code
	default:
		fmt.Fprintln(os.Stderr, "fake docker: not handled:", args)
		return 1
	}
	return 0
}

// fakeExport answers docker build --build-arg VERSION --target console
// --output type=local,dest=<d> ., with VERSION in the environment: it copies
// $SETUP_CONSOLE to <d>/stargaite-console through a rename, a new inode as
// BuildKit's local exporter gives. FAKE_EXPORT=fails fails the build, and
// FAKE_EXPORT=hang makes it last 30 s.
func fakeExport(args []string) int {
	i := slices.Index(args, "--output")
	if i < 0 || i+1 == len(args) || !strings.HasPrefix(args[i+1], "type=local,dest=") || !slices.Contains(args, "--target") ||
		!slices.Contains(args, "--build-arg") || os.Getenv("VERSION") == "" {
		fmt.Fprintln(os.Stderr, "fake docker: not handled:", args)
		return 1
	}
	switch os.Getenv("FAKE_EXPORT") {
	case "fails":
		fmt.Fprintln(os.Stderr, "ERROR: failed to solve: fake")
		return 1
	case "hang":
		time.Sleep(30 * time.Second)
	}
	dest := strings.TrimPrefix(args[i+1], "type=local,dest=")
	data, err := os.ReadFile(os.Getenv("SETUP_CONSOLE"))
	if err == nil {
		err = os.MkdirAll(dest, 0o755)
	}
	tmp := filepath.Join(dest, ".stargaite-console.tmp")
	if err == nil {
		err = os.WriteFile(tmp, data, 0o755)
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dest, "stargaite-console"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake docker build:", err)
		return 1
	}
	return 0
}

// export is the store as export --json shows it: the members the console
// reads to judge what exists.
func (st *state) export() string {
	models, keys := map[string]any{}, map[string]any{}
	for name, targets := range st.Models {
		models[name] = map[string]any{"targets": targets}
	}
	for _, name := range st.Keys {
		keys[name] = map[string]any{}
	}
	return encode(map[string]any{"version": 1, "providers": st.Providers, "models": models, "keys": keys})
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
		case slices.Contains([]string{"json", "allow-private-network", "until-stdin-closes", "dry-run", "yes"}, a):
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

// fakeCLI answers the CLI commands setup.sh and the console run, in the
// shapes of the admin API's --json answers.
func fakeCLI(st *state, args []string) (string, int) {
	// apply is one word: the console's dry run, apply --json --dry-run -f -,
	// with a document on standard input.
	if len(args) > 1 && args[0] == "apply" && args[1] == "--json" {
		var doc struct {
			Version *int `json:"version"`
		}
		in, _ := io.ReadAll(os.Stdin)
		if json.Unmarshal(in, &doc) != nil || doc.Version == nil || *doc.Version != 1 {
			return failure(fmt.Sprintf("fake CLI: apply takes a JSON object with \"version\":1, not %q", in))
		}
		return encode(map[string]any{"revision": 1, "changed": false, "diff": []any{}, "findings": []any{}}), 0
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
	case "targets remove":
		if _, ok := st.Targets[pos[0]]; !ok || !slices.Contains(args, "--yes") {
			return failure("targets remove takes a target that exists, and --yes")
		}
		delete(st.Targets, pos[0])
		return encode(map[string]any{"revision": 1, "changed": true, "diff": []any{}, "findings": []any{}}), 0
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
			list = append(list, map[string]any{"name": name, "targets": targets, "budget": st.budget("alias:" + name)})
		}
		return encode(map[string]any{"models": list}), 0
	case "models explain": // which effort and output cap are sent is the proxy's to say: none here
		list := []any{}
		for _, id := range st.Models[pos[0]] {
			list = append(list, map[string]any{"target": id})
		}
		return encode(map[string]any{"targets": list}), 0
	case "models add":
		st.Models[pos[0]] = flags["target"]
		return written(map[string]any{"name": pos[0], "targets": flags["target"]}), 0
	case "keys list":
		list := []any{}
		for _, name := range st.Keys {
			list = append(list, map[string]any{"name": name, "models": []string{"main"}, "secrets": []any{}, "budget": st.budget("key:" + name)})
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
		delete(t, "verified_at") // as the proxy does: a target's edit clears its verification
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
	case "telemetry test": // the outcome of one test span, as the CLI's --json gives it (TEL-2)
		switch {
		case st.Telemetry["otlp-endpoint"] == "":
			return failure("no exporter is configured: telemetry set --otlp-endpoint <url> sets one")
		case len(st.unresolved()) > 0:
			return encode(map[string]any{"outcome": "credential_unresolved", "message": strings.Join(st.unresolved(), ", ") + " does not resolve"}), 1
		case os.Getenv("FAKE_TELEMETRY") == "unauthorized":
			return encode(map[string]any{"outcome": "unauthorized", "status": 401, "message": "bad credentials"}), 1
		}
		return encode(map[string]any{"outcome": "accepted", "status": 200, "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736"}), 0
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
