// Package setup runs setup.sh (OPS-11) against shims, as the stubbed layer of
// docs/design/phase-6.md section 7: setup.sh is copied into a temporary
// directory and run with a PATH that holds only shims, links to the command
// of ./shim, which record every external command and answer for docker and
// curl. jq is the real one make tools puts in bin/; the runs at a terminal go
// through script(1). Linux only; make setup-test runs it end to end.
package setup

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The commands setup.sh may run. The run's PATH holds only these, each a link
// to the shim of ./shim, so any other external command fails the run: every
// external command is recorded. docker, curl, id, stat, sleep, chown and
// hostname answer as fakes; the rest are recording pass-throughs to the real
// command.
var shimNames = []string{"docker", "curl", "jq", "git", "id", "stat", "mktemp", "mv", "rm", "timeout", "sleep", "stty",
	"openssl", "mkdir", "cp", "chmod", "chown", "hostname", "getent"}

// shimBin is the shim, built once for every run; consoleBin is the console,
// version v0.0.0-shim, which the fake docker build exports; consoleDirtyBin
// and consoleDevBin report v0.0.0-shim-dirty and dev.
var shimBin, consoleBin, consoleDirtyBin, consoleDevBin string

// record is one shim call, as the shim logs it.
type record struct {
	Argv  []string `json:"argv"`
	Dir   string   `json:"dir"`
	Stdin string   `json:"stdin,omitempty"`
}

func TestMain(m *testing.M) {
	code := 0
	if runtime.GOOS == "linux" {
		dir, err := os.MkdirTemp("", "setup-shim-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		shimBin, consoleBin = filepath.Join(dir, "shim"), filepath.Join(dir, "stargaite-console")
		consoleDirtyBin, consoleDevBin = filepath.Join(dir, "dirty", "stargaite-console"), filepath.Join(dir, "dev", "stargaite-console")
		build := func(out string, ldflags ...string) error {
			b, err := exec.Command("go", append(append([]string{"build", "-o", out}, ldflags...), "../../cmd/stargaite-console")...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%w\n%s", err, b)
			}
			return nil
		}
		if out, err := exec.Command("go", "build", "-o", shimBin, "./shim").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "the shim was not built: %v\n%s", err, out)
			code = 1
		} else if err := errors.Join(build(consoleBin, "-ldflags", "-X main.version=v0.0.0-shim"),
			build(consoleDirtyBin, "-ldflags", "-X main.version=v0.0.0-shim-dirty"), build(consoleDevBin)); err != nil {
			fmt.Fprintf(os.Stderr, "the console was not built: %v\n", err)
			code = 1
		} else {
			code = m.Run()
		}
		_ = os.RemoveAll(dir)
	} else {
		code = m.Run()
	}
	os.Exit(code)
}

// run is one temporary copy of setup.sh and compose.yaml with its shims.
type run struct {
	t               *testing.T
	dir, log, bash  string
	shims, realPath string
	env             []string
}

// newRun prepares a run; env adds variables, such as the FAKE_ ones of
// fake_test.go, and without lists shims left out, as missing commands.
func newRun(t *testing.T, env []string, without ...string) *run {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("setup.sh supports Linux only")
	}
	r := &run{t: t, dir: t.TempDir(), shims: t.TempDir()}
	tmp := t.TempDir()
	r.log = filepath.Join(tmp, "log")
	var err error
	if r.bash, err = exec.LookPath("bash"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"setup.sh", "compose.yaml"} {
		data, err := os.ReadFile(filepath.Join("..", "..", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.dir, f), data, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range shimNames {
		if !slices.Contains(without, name) {
			if err := os.Symlink(shimBin, filepath.Join(r.shims, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	r.realPath = toolsDir(t) + string(os.PathListSeparator) + os.Getenv("PATH")
	r.env = append([]string{"PATH=" + r.shims, "HOME=" + tmp, "LANG=C", "SETUP_LOG=" + r.log,
		"SETUP_STATE=" + filepath.Join(tmp, "state"), "SETUP_REAL_PATH=" + r.realPath, "SETUP_CONSOLE=" + consoleBin}, env...)
	return r
}

// toolsDir is the directory of bin/ that make tools fills for this system,
// which holds jq.
func toolsDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("uname", "-sm").Output()
	if err != nil {
		t.Fatal(err)
	}
	prefix := strings.ReplaceAll(strings.TrimSpace(string(out)), " ", "-") + "-"
	dirs, _ := filepath.Glob(filepath.Join("..", "..", "bin", prefix+"*", "jq"))
	if len(dirs) == 0 {
		t.Fatalf("no jq in bin/%s*: run make tools, which make ci runs", prefix)
	}
	dir, err := filepath.Abs(filepath.Dir(dirs[len(dirs)-1]))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// exec runs setup.sh with no terminal and standard input empty; out holds
// standard output and standard error.
func (r *run) exec(args ...string) (int, string) {
	r.t.Helper()
	cmd := exec.Command(r.bash, append([]string{filepath.Join(r.dir, "setup.sh")}, args...)...)
	cmd.Env, cmd.Dir = r.env, r.dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	timer := time.AfterFunc(60*time.Second, func() { _ = cmd.Process.Kill() })
	defer timer.Stop()
	return r.exitCode(cmd.Wait()), out.String()
}

// exitCode is the exit code of a finished run; any other error fails the test.
func (r *run) exitCode(err error) int {
	r.t.Helper()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		r.t.Fatal(err)
	}
	if exit == nil {
		return 0
	}
	return exit.ExitCode()
}

// step is one exchange at the terminal: once prompt appears after the
// previous one, answer and a newline are typed.
type step struct{ prompt, answer string }

// pty runs setup.sh at a terminal through script(1), typing each answer only
// once its prompt is on screen; it returns the exit code and the transcript.
func (r *run) pty(args []string, steps ...step) (int, string) {
	r.t.Helper()
	script, err := exec.LookPath("script")
	if err != nil {
		r.t.Fatal("script(1) is needed for the runs at a terminal: ", err)
	}
	line := strings.Join(append([]string{r.bash, filepath.Join(r.dir, "setup.sh")}, args...), " ")
	cmd := exec.Command(script, "-q", "-e", "-E", "always", "-c", line, "/dev/null")
	cmd.Env, cmd.Dir = append(slices.Clone(r.env), "SHELL="+r.bash, "TERM=dumb"), r.dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		r.t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		r.t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	var mu sync.Mutex
	var buf bytes.Buffer
	read := make(chan struct{})
	go func() {
		chunk := make([]byte, 4096)
		for {
			n, err := stdout.Read(chunk)
			mu.Lock()
			buf.Write(chunk[:n])
			mu.Unlock()
			if err != nil {
				close(read)
				return
			}
		}
	}()
	text := func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
	from := 0
	for _, s := range steps {
		deadline := time.Now().Add(30 * time.Second)
		for {
			if i := strings.Index(text()[from:], s.prompt); i >= 0 {
				from += i + len(s.prompt)
				break
			}
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				r.t.Fatalf("no prompt %q within 30 s; transcript:\n%s", s.prompt, text())
			}
			time.Sleep(20 * time.Millisecond)
		}
		if _, err := io.WriteString(stdin, s.answer+"\n"); err != nil {
			r.t.Fatal(err)
		}
	}
	timer := time.AfterFunc(60*time.Second, func() { _ = cmd.Process.Kill() })
	defer timer.Stop()
	<-read
	err = cmd.Wait()
	_ = stdin.Close()
	return r.exitCode(err), text()
}

// calls are the recorded shim calls, in order.
func (r *run) calls() []record {
	r.t.Helper()
	data, err := os.ReadFile(r.log)
	if err != nil && !os.IsNotExist(err) {
		r.t.Fatal(err)
	}
	var list []record
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var rec record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			r.t.Fatalf("log line %q: %v", line, err)
		}
		list = append(list, rec)
	}
	return list
}

// cli are the CLI commands run in the container, each from its first word,
// "--json" left out.
func (r *run) cli() []string {
	var list []string
	for _, c := range r.calls() {
		if i := slices.Index(c.Argv, "/stargaite"); i > 0 && c.Argv[0] == "docker" && c.Argv[2] == "exec" {
			list = append(list, strings.Join(slices.DeleteFunc(slices.Clone(c.Argv[i+1:]), func(a string) bool { return a == "--json" }), " "))
		}
	}
	return list
}

// count is how many recorded calls of command hold text in their argv
// joined by spaces, "--json" left out.
func (r *run) count(command, text string) int {
	n := 0
	for _, c := range r.calls() {
		argv := slices.DeleteFunc(slices.Clone(c.Argv), func(a string) bool { return a == "--json" })
		if c.Argv[0] == command && strings.Contains(strings.Join(argv, " "), text) {
			n++
		}
	}
	return n
}

// curlOf is the recorded curl call of url, its last argument.
func (r *run) curlOf(url string) record {
	r.t.Helper()
	for _, c := range r.calls() {
		if c.Argv[0] == "curl" && c.Argv[len(c.Argv)-1] == url {
			return c
		}
	}
	r.t.Fatalf("no curl of %s", url)
	return record{}
}

func (r *run) file(name string) string {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(data)
}

// leaks names every place secret is found but .env: a recorded argv, the
// output, or a file of the run's directory.
func (r *run) leaks(secret, out string) []string {
	r.t.Helper()
	var found []string
	for _, c := range r.calls() {
		for _, a := range c.Argv {
			if strings.Contains(a, secret) {
				found = append(found, "the argv of "+c.Argv[0])
			}
		}
	}
	if strings.Contains(out, secret) {
		found = append(found, "the output")
	}
	_ = filepath.WalkDir(r.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == ".env" {
			return err
		}
		if data, err := os.ReadFile(path); err == nil && bytes.Contains(data, []byte(secret)) {
			found = append(found, "the file "+d.Name())
		}
		return nil
	})
	return found
}

// written is what .env and compose.override.yaml hold, a missing one as
// empty: what a refused run must leave as it was.
func (r *run) written() string {
	var s string
	for _, name := range []string{".env", "compose.override.yaml"} {
		data, _ := os.ReadFile(filepath.Join(r.dir, name))
		s += name + ":\n" + string(data)
	}
	return s
}

// freePort is a loopback port nothing listens on.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

// acme is install with every answer of a compatible service as a flag, its
// credential read from SRC.
func acme(port string, extra ...string) []string {
	return append([]string{"install", "--port", port, "--provider", "acme", "--protocol", "openai",
		"--base-url", "https://api.acme.example/v1", "--api-key-env", "ACME_API_KEY", "--key-env", "SRC",
		"--model", "m1", "--alias", "main", "--key-name", "app", "--rpm-limit", "30", "--yes"}, extra...)
}

func without(args []string, flag string) []string {
	i := slices.Index(args, flag)
	if flag == "--yes" {
		return slices.Delete(slices.Clone(args), i, i+1)
	}
	return slices.Delete(slices.Clone(args), i, i+2)
}

// TestSetupFlags: install with every answer as a flag, for a compatible
// service, a local preset and an anthropic preset, runs every step through
// the CLI; run again, it resumes, keeping what exists and calling nothing
// paid.
func TestSetupFlags(t *testing.T) {
	t.Run("compatible service", func(t *testing.T) {
		r := newRun(t, []string{"SRC=secret-value"})
		port := freePort(t)
		code, out := r.exec(acme(port)...)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		want := []string{
			"providers presets", "providers list",
			"providers add --base-url https://api.acme.example/v1 --api-key-env ACME_API_KEY --protocol openai --billing pay_per_use -- acme",
			"providers show -- acme",
			"targets list", "targets add --kind chat -- acme/m1", "settings show", "targets verify --max-output-tokens 256 -- acme/m1",
			"models list", "models add --target acme/m1 -- main",
			"keys list", "keys create --model main --rpm-limit 30 -- app",
		}
		if got := r.cli(); !slices.Equal(got, want) {
			t.Errorf("CLI calls:\n%q\nwant:\n%q", got, want)
		}
		if r.count("docker", "compose up -d --build") != 1 || r.count("docker", "compose up -d --no-build") != 1 {
			t.Errorf("want one build and one recreate:\n%s", out)
		}
		for _, s := range []string{"answer: fake reply", "finish_reason: stop", "from fake/target", "sk-stargaite-fake-app", "base URL http://127.0.0.1:" + port + "/v1"} {
			if !strings.Contains(out, s) {
				t.Errorf("no %q in:\n%s", s, out)
			}
		}
		// The client key: printed once, read by curl from standard input,
		// in no argv and no file.
		const key = "sk-stargaite-fake-app"
		if n := strings.Count(out, key); n != 1 {
			t.Errorf("the client key printed %d times:\n%s", n, out)
		}
		if found := r.leaks(key, ""); len(found) > 0 {
			t.Errorf("the client key is in %q", found)
		}
		if c := r.curlOf("http://127.0.0.1:" + port + "/v1/chat/completions"); c.Stdin != `header = "Authorization: Bearer `+key+`"`+"\n" {
			t.Errorf("curl config of the test call: %q", c.Stdin)
		}
		if env := r.file(".env"); env != "STARGAITE_PORT='"+port+"'\nACME_API_KEY='secret-value'\n" {
			t.Errorf(".env: %q", env)
		}
		if _, err := os.Stat(filepath.Join(r.dir, "compose.override.yaml")); !os.IsNotExist(err) {
			t.Errorf("an override for a remote provider: %v", err)
		}

		code, out = r.exec(acme(port)...)
		if code != 0 {
			t.Fatalf("again: exit %d:\n%s", code, out)
		}
		for _, s := range []string{"the container runs; kept", "provider acme exists; kept", "ACME_API_KEY is in .env; kept",
			"target acme/m1 exists; kept", "target acme/m1 is verified; not verified again", "alias main exists, toward acme/m1; kept",
			"key app exists", "the first answer is skipped"} {
			if !strings.Contains(out, s) {
				t.Errorf("again: no %q in:\n%s", s, out)
			}
		}
		if r.count("docker", "providers add") != 1 || r.count("docker", "targets verify") != 1 || r.count("docker", "keys create") != 1 ||
			r.count("docker", "compose up") != 2 || r.count("curl", "chat/completions") != 1 {
			t.Errorf("again: something was made or called twice:\n%q", r.cli())
		}
	})

	t.Run("local preset", func(t *testing.T) {
		r := newRun(t, nil)
		code, out := r.exec("install", "--port", freePort(t), "--provider", "ollama", "--model", "llama3", "--yes")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if r.count("docker", "providers add --base-url http://host.docker.internal:11434/v1 --allow-private-network -- ollama") != 1 {
			t.Errorf("providers add: %q", r.cli())
		}
		if !strings.Contains(out, "localhost inside the container is the container") {
			t.Errorf("no word on localhost:\n%s", out)
		}
		if !strings.Contains(r.file("compose.override.yaml"), `      - "host.docker.internal:host-gateway"`) || r.count("docker", "compose up -d --no-build") != 1 {
			t.Errorf("override or recreate missing:\n%s", out)
		}
	})

	t.Run("anthropic preset", func(t *testing.T) {
		r := newRun(t, []string{"SRC=sk-ant-x"})
		code, out := r.exec("install", "--port", freePort(t), "--provider", "anthropic", "--key-env", "SRC", "--model", "claude-x",
			"--max-output-tokens", "4096", "--effort", "high", "--yes")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if r.count("docker", "models add --target anthropic/claude-x --max-output-tokens 4096 --effort high --effort-mode default -- main") != 1 {
			t.Errorf("models add: %q", r.cli())
		}
	})

	// An installation started by hand: .env without STARGAITE_PORT and the
	// container on 4000. The port it already publishes is kept, with no
	// recreate.
	t.Run("started by hand", func(t *testing.T) {
		r := newRun(t, []string{"SRC=v"})
		port := freePort(t)
		if code, out := r.exec(acme(port)...); code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if err := os.WriteFile(filepath.Join(r.dir, ".env"), []byte("ACME_API_KEY='v'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out := r.exec(without(without(acme(port), "--port"), "--yes")...)
		if code != 0 || r.count("docker", "compose up") != 2 || !strings.Contains(out, "the container runs; kept") {
			t.Errorf("exit %d, %d compose up (want 2):\n%s", code, r.count("docker", "compose up"), out)
		}
	})

	t.Run("replace and allow private network", func(t *testing.T) {
		r := newRun(t, []string{"SRC=old-value", "SRC2=new-value"})
		port := freePort(t)
		if code, out := r.exec(acme(port, "--base-url", "http://llm.acme.example/v1", "--allow-private-network")...); code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if r.count("docker", "providers add --base-url http://llm.acme.example/v1 --api-key-env ACME_API_KEY --protocol openai --billing pay_per_use --allow-private-network -- acme") != 1 {
			t.Errorf("providers add: %q", r.cli())
		}
		code, out := r.exec(acme(port, "--key-env", "SRC2", "--replace")...)
		if code != 0 || !strings.Contains(r.file(".env"), "ACME_API_KEY='new-value'") || r.count("docker", "compose up -d --no-build") != 2 {
			t.Errorf("--replace: exit %d, .env %q:\n%s", code, r.file(".env"), out)
		}
	})

	// A credential the container does not get stops the run before any paid
	// call.
	t.Run("credential unresolved", func(t *testing.T) {
		r := newRun(t, []string{"SRC=v", "FAKE_UP=noenv"})
		code, out := r.exec(acme(freePort(t))...)
		if code != 1 || !strings.Contains(out, "does not resolve") || r.count("docker", "targets verify") != 0 {
			t.Errorf("exit %d, %d verify:\n%s", code, r.count("docker", "targets verify"), out)
		}
	})

	// Run again (C55): each subcommand alone on the running installation,
	// every answer a flag, through the CLI only.
	t.Run("run again", func(t *testing.T) {
		r := newRun(t, []string{"SRC=v", "PK=pk-lf-public", "SK=sk-lf-secret", "OT=Bearer otlp-secret"})
		if code, out := r.exec(acme(freePort(t))...); code != 0 {
			t.Fatalf("install: exit %d:\n%s", code, out)
		}
		// sub runs a subcommand and returns its CLI calls; all holds every
		// output, for the leak check.
		var all string
		sub := func(args ...string) ([]string, string) {
			t.Helper()
			before := len(r.cli())
			code, out := r.exec(args...)
			all += out
			if code != 0 {
				t.Fatalf("%q: exit %d:\n%s", args, code, out)
			}
			return r.cli()[before:], out
		}

		got, out := sub("provider", "--provider", "ollama", "--yes")
		want := []string{"providers presets", "providers list", "providers add --base-url http://host.docker.internal:11434/v1 --allow-private-network -- ollama",
			"providers show -- ollama"}
		if !slices.Equal(got, want) || r.count("docker", "compose up -d --no-build") != 2 || !strings.Contains(r.file("compose.override.yaml"), "host-gateway") {
			t.Errorf("provider: %q, want %q:\n%s", got, want, out)
		}

		got, out = sub("alias", "--provider", "ollama", "--model", "llama3", "--alias", "local")
		want = []string{"models list", "providers presets", "providers list", "providers show -- ollama", "providers show -- ollama", "targets list",
			"targets add --kind chat -- ollama/llama3", "settings show", "targets verify --max-output-tokens 256 -- ollama/llama3", "models add --target ollama/llama3 -- local"}
		if !slices.Equal(got, want) || !strings.Contains(out, "provider ollama exists; kept") {
			t.Errorf("alias: %q, want %q:\n%s", got, want, out)
		}
		// An alias that exists (main, the default) is kept before any paid
		// call.
		got, out = sub("alias", "--provider", "acme", "--model", "m2")
		if !slices.Equal(got, []string{"models list"}) || !strings.Contains(out, "alias main exists, toward acme/m1; kept") {
			t.Errorf("alias that exists: %q:\n%s", got, out)
		}

		got, out = sub("key", "--alias", "local", "--key-name", "second", "--rpm-limit", "10")
		if !slices.Equal(got, []string{"models list", "keys list", "keys create --model local --rpm-limit 10 -- second"}) ||
			!strings.Contains(out, "answer: fake reply") || r.count("curl", "chat/completions") != 2 || r.count("curl", `"model":"local"`) != 1 {
			t.Errorf("key: %q:\n%s", got, out)
		}
		// A key that exists is kept, as anything that exists is: exit 0.
		got, out = sub("key", "--alias", "local", "--key-name", "second")
		if !slices.Equal(got, []string{"models list", "keys list"}) || !strings.Contains(out, "key second exists") {
			t.Errorf("key that exists: %q:\n%s", got, out)
		}
		// A key for an alias that does not exist is never made.
		before := len(r.cli())
		code, out := r.exec("key", "--alias", "nope", "--key-name", "third")
		if got := r.cli()[before:]; code != 1 || !strings.Contains(out, "alias nope does not exist: ./setup.sh alias makes it") || !slices.Equal(got, []string{"models list"}) {
			t.Errorf("key for no alias: exit %d, %q:\n%s", code, got, out)
		}

		// The alias's budget is refused for acme/m1, which has no price: its
		// prices are set, as of today, and the budget applied once more.
		// ollama/llama3, of a provider with billing none, is never asked about.
		got, out = sub("budget", "--on", "alias:main", "--limit-usd", "5", "--input-price", "1.5", "--output-price", "6")
		today := time.Now().Format(time.DateOnly)
		want = []string{"models show -- main", "models set --budget limit_usd=5,reset_interval=monthly -- main",
			"targets set --pricing input_per_million=1.5,output_per_million=6,as_of=" + today + " -- acme/m1",
			"models set --budget limit_usd=5,reset_interval=monthly -- main"}
		if !slices.Equal(got, want) || !strings.Contains(out, "error: targets/acme/m1: pay-per-use target") ||
			!strings.Contains(out, "setting the prices of acme/m1 to 1.5 and 6 USD per million input and output tokens, as of "+today) ||
			!strings.Contains(out, "which clears its verification: a later install or alias toward it verifies it again") {
			t.Errorf("budget: %q, want %q:\n%s", got, want, out)
		}
		// A budget that replaces one is confirmed: without --yes, exit 2 and
		// nothing set.
		before = len(r.cli())
		code, out = r.exec("budget", "--on", "alias:main", "--limit-usd", "7")
		if got := r.cli()[before:]; code != 2 || !strings.Contains(out, "missing --yes: the budget on alias:main, 5 USD, reset monthly, is replaced") ||
			!slices.Equal(got, []string{"models show -- main"}) {
			t.Errorf("budget replaced without --yes: exit %d, %q:\n%s", code, got, out)
		}
		if got, out = sub("budget", "--on", "alias:main", "--limit-usd", "7", "--yes"); !slices.Equal(got, []string{"models show -- main",
			"models set --budget limit_usd=7,reset_interval=monthly -- main"}) {
			t.Errorf("budget replaced: %q:\n%s", got, out)
		}
		got, out = sub("budget", "--on", "instance", "--limit-usd", "20", "--reset", "daily")
		if !slices.Equal(got, []string{"settings show", "settings set --budget limit_usd=20,reset_interval=daily"}) {
			t.Errorf("budget on the instance: %q:\n%s", got, out)
		}
		if got, out = sub("budget", "--on", "key:app", "--limit-usd", "0.5", "--reset", "never"); !slices.Equal(got,
			[]string{"keys show -- app", "keys set --budget limit_usd=0.5,reset_interval=never -- app"}) {
			t.Errorf("budget on a key: %q:\n%s", got, out)
		}

		// Langfuse: both keys into .env, then set, the container recreated
		// to read them, and the test's line as the CLI prints it.
		got, out = sub("telemetry", "--backend", "langfuse", "--endpoint", "https://lf.example/api/public/otel",
			"--langfuse-public-env", "PK", "--langfuse-secret-env", "SK", "--yes")
		want = []string{"telemetry status", "telemetry set --otlp-endpoint https://lf.example/api/public/otel --preset langfuse", "telemetry status", "telemetry test"}
		if !slices.Equal(got, want) || r.count("docker", "compose up -d --no-build") != 3 ||
			!strings.Contains(out, "span: accepted (200) · trace_id=4bf92f3577b34da6a3ce929d0e0e4736") {
			t.Errorf("telemetry langfuse: %q, want %q:\n%s", got, want, out)
		}
		// Any OTLP endpoint on the host, after Langfuse: the block is unset
		// first, so Langfuse's keys never go to it; a header credential into
		// .env, the endpoint reached as a local provider is.
		got, out = sub("telemetry", "--backend", "otlp", "--endpoint", "http://localhost:4318", "--header-name", "Authorization", "--key-env", "OT", "--yes")
		want = []string{"telemetry status", "telemetry unset --yes",
			"telemetry set --otlp-endpoint http://host.docker.internal:4318 --header-env Authorization=OTLP_AUTHORIZATION --allow-private-network",
			"telemetry status", "telemetry test"}
		if !slices.Equal(got, want) || r.count("docker", "compose up -d --no-build") != 4 ||
			!strings.Contains(out, "telemetry toward https://lf.example/api/public/otel is replaced whole") {
			t.Errorf("telemetry otlp: %q, want %q:\n%s", got, want, out)
		}
		env := r.file(".env")
		for _, s := range []string{"LANGFUSE_PUBLIC_KEY='pk-lf-public'\n", "LANGFUSE_SECRET_KEY='sk-lf-secret'\n", "OTLP_AUTHORIZATION='Bearer otlp-secret'\n"} {
			if !strings.Contains(env, s) {
				t.Errorf(".env has no %q: %q", s, env)
			}
		}

		got, out = sub("status")
		if !slices.Equal(got, []string{"version", "providers list", "models list", "targets list", "keys list", "settings show", "telemetry status"}) {
			t.Errorf("status: %q", got)
		}
		for _, s := range []string{"version: v0.6.0-fake running, dev in this clone", "provider acme: credential resolved", "provider ollama: credential none",
			"telemetry http://host.docker.internal:4318: credentials resolved", "spend: capped at 20 USD, resets daily"} {
			if !strings.Contains(out, s) {
				t.Errorf("status: no %q in:\n%s", s, out)
			}
		}
		if n := r.count("docker", "targets verify"); n != 2 {
			t.Errorf("%d verify, want 2: the install's and the alias's", n)
		}
		// A block the CLI refuses after the unset: said, since nothing is
		// set any more.
		code, out = r.exec("telemetry", "--backend", "otlp", "--endpoint", "otlp.example:4318", "--yes")
		all += out
		if code != 1 || !strings.Contains(out, "telemetry is now unset; it was toward http://host.docker.internal:4318\n") {
			t.Errorf("telemetry refused: exit %d:\n%s", code, out)
		}
		for _, secret := range []string{"pk-lf-public", "sk-lf-secret", "otlp-secret"} {
			if found := r.leaks(secret, all); len(found) > 0 {
				t.Errorf("%s is in %q", secret, found)
			}
		}
	})

	// A run refused at the confirmation writes nothing, so the same run with
	// --yes recreates the container and reaches its end; a credential in .env
	// that the container lacks, as after a run interrupted before its
	// recreate, is read by a recreate too.
	t.Run("resume", func(t *testing.T) {
		r := newRun(t, []string{"SRC=v", "OT=Bearer otlp-secret"})
		if code, out := r.exec(acme(freePort(t))...); code != 0 {
			t.Fatalf("install: exit %d:\n%s", code, out)
		}
		otlp := []string{"telemetry", "--backend", "otlp", "--endpoint", "https://otlp.example", "--header-name", "Authorization", "--key-env", "OT"}
		for _, args := range [][]string{{"provider", "--provider", "ollama"}, otlp} {
			up := r.count("docker", "compose up")
			files := r.written()
			if code, out := r.exec(args...); code != 2 || !strings.Contains(out, "missing --yes: writing ") || r.written() != files {
				t.Errorf("%q without --yes: exit %d, files:\n%s\nwere:\n%s\n%s", args, code, r.written(), files, out)
			}
			code, out := r.exec(append(args, "--yes")...)
			if code != 0 || r.count("docker", "compose up") != up+1 {
				t.Errorf("%q again with --yes: exit %d, %d compose up, want %d:\n%s", args, code, r.count("docker", "compose up"), up+1, out)
			}
		}
		if !strings.Contains(r.file(".env"), "OTLP_AUTHORIZATION='Bearer otlp-secret'") || !strings.Contains(r.file("compose.override.yaml"), "host-gateway") {
			t.Errorf(".env %q, override %q", r.file(".env"), r.file("compose.override.yaml"))
		}
		// Written to .env by a run that stopped before its recreate.
		env := r.file(".env") + "OTLP_X_TOK='v'\n"
		if err := os.WriteFile(filepath.Join(r.dir, ".env"), []byte(env), 0o600); err != nil {
			t.Fatal(err)
		}
		up := r.count("docker", "compose up")
		code, out := r.exec("telemetry", "--backend", "otlp", "--endpoint", "https://otlp.example", "--header-name", "X-Tok", "--yes")
		if code != 0 || r.count("docker", "compose up") != up+1 || !strings.Contains(out, "OTLP_X_TOK is in .env; kept") ||
			!strings.Contains(out, "the container is recreated to read OTLP_X_TOK") || !strings.Contains(out, "span: accepted (200)") {
			t.Errorf("credential kept but unresolved: exit %d, %d compose up, want %d:\n%s", code, r.count("docker", "compose up"), up+1, out)
		}
	})

	// A local telemetry endpoint on an installation without the override:
	// the override is written only once the recreate is confirmed.
	t.Run("local telemetry", func(t *testing.T) {
		r := newRun(t, []string{"SRC=v"})
		if code, out := r.exec(acme(freePort(t))...); code != 0 {
			t.Fatalf("install: exit %d:\n%s", code, out)
		}
		args := []string{"telemetry", "--backend", "otlp", "--endpoint", "http://127.0.0.1:4318"}
		if code, out := r.exec(args...); code != 2 || !strings.Contains(out, "missing --yes: writing compose.override.yaml with host_gateway=1") ||
			strings.Contains(r.written(), "host_gateway") {
			t.Errorf("without --yes: exit %d:\n%s", code, out)
		}
		before := len(r.cli())
		code, out := r.exec(append(args, "--yes")...)
		want := []string{"telemetry status", "telemetry set --otlp-endpoint http://host.docker.internal:4318 --allow-private-network", "telemetry status", "telemetry test"}
		if got := r.cli()[before:]; code != 0 || !slices.Equal(got, want) || r.count("docker", "compose up -d --no-build") != 2 ||
			!strings.Contains(r.file("compose.override.yaml"), `      - "host.docker.internal:host-gateway"`) || strings.Contains(out, "Ollama") {
			t.Errorf("exit %d, %q, want %q:\n%s", code, got, want, out)
		}
		if code, out := r.exec("status"); code != 0 || !strings.Contains(out, "telemetry http://host.docker.internal:4318: no credential") {
			t.Errorf("status: exit %d:\n%s", code, out)
		}
	})

	// A subcommand of a running installation needs one; status says what
	// the clone holds. Only Compose answering with no container means none:
	// a Docker or Compose that fails is named, and status exits 1 too.
	t.Run("nothing runs", func(t *testing.T) {
		r := newRun(t, nil)
		code, out := r.exec("key", "--alias", "main")
		if code != 1 || !strings.Contains(out, "no container runs here: ./setup.sh install starts it") || len(r.cli()) != 0 {
			t.Errorf("key: exit %d:\n%s", code, out)
		}
		code, out = r.exec("status")
		if code != 0 || !strings.Contains(out, "version: dev in this clone; no container runs") || len(r.cli()) != 0 {
			t.Errorf("status: exit %d:\n%s", code, out)
		}
		// The port .env names, taken by another service, is not the matter.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		env := "STARGAITE_PORT='" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port) + "'\n"
		if err := os.WriteFile(filepath.Join(r.dir, ".env"), []byte(env), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"status"}, {"key", "--alias", "main"}} {
			if code, out := r.exec(args...); strings.Contains(out, "is taken") || code == 0 != (args[0] == "status") {
				t.Errorf("%q with the port taken: exit %d:\n%s", args, code, out)
			}
		}
		for _, c := range []struct{ env, want string }{
			{"FAKE_DOCKER=denied", "permission to use Docker: run as root, or join the docker group"},
			{"FAKE_DOCKER=unreadable", "a .env and compose.override.yaml that Compose reads (docker compose ps: failed to read .env: line 1"},
		} {
			r := newRun(t, []string{c.env})
			for _, args := range [][]string{{"status"}, {"key", "--alias", "main"}} {
				if code, out := r.exec(args...); code != 1 || !strings.Contains(out, c.want) || strings.Contains(out, "no container runs") || len(r.cli()) != 0 ||
					strings.Contains(out, "sk-hand-edited-secret") || strings.Contains(out, "is taken") {
					t.Errorf("%s, %q: exit %d, want 1 and %q:\n%s", c.env, args, code, c.want, out)
				}
			}
		}
	})

	// A telemetry test that fails prints its line and exits 1.
	t.Run("telemetry test fails", func(t *testing.T) {
		r := newRun(t, []string{"SRC=v", "FAKE_TELEMETRY=unauthorized"})
		if code, out := r.exec(acme(freePort(t))...); code != 0 {
			t.Fatalf("install: exit %d:\n%s", code, out)
		}
		code, out := r.exec("telemetry", "--backend", "otlp", "--endpoint", "https://otlp.example")
		if code != 1 || !strings.Contains(out, "authorized: no (401) · bad credentials") || strings.Contains(out, "failed with exit") {
			t.Errorf("exit %d:\n%s", code, out)
		}
	})

	// --key-env names an exported variable only, never one of the script's.
	t.Run("key-env not exported", func(t *testing.T) {
		r := newRun(t, nil)
		code, out := r.exec(acme(freePort(t), "--key-env", "from")...)
		if code != 2 || !strings.Contains(out, "exported to this script") || strings.Contains(r.file(".env"), "ACME_API_KEY") {
			t.Errorf("exit %d:\n%s", code, out)
		}
	})

	// status runs the console, exported to bin/ from the Dockerfile's
	// console stage when it is missing, of another version, or of a dev or
	// dirty clone; the export needs buildx and is bounded to 20 minutes.
	t.Run("console export", func(t *testing.T) {
		const export = "docker build --quiet --build-arg VERSION --target console --output type=local,dest=bin ."
		for _, c := range []struct{ name, env, want string }{
			{"no buildx", "FAKE_BUILDX=missing", "Docker's buildx component, which builds the console: docker-buildx-plugin from Docker's packages, or docker-buildx on Debian"},
			{"export fails", "FAKE_EXPORT=fails", "the console was not built"},
		} {
			r := newRun(t, []string{c.env})
			code, out := r.exec("status")
			if code != 1 || !strings.Contains(out, c.want) || len(r.cli()) != 0 || (c.env == "FAKE_BUILDX=missing") != (r.count("docker", export) == 0) {
				t.Errorf("%s: exit %d, %d exports:\n%s", c.name, code, r.count("docker", export), out)
			}
		}

		r := newRun(t, nil)
		gitIn(t, r.dir, "init", "-q")
		gitIn(t, r.dir, "add", "setup.sh", "compose.yaml")
		gitIn(t, r.dir, "commit", "-q", "-m", "first")
		gitIn(t, r.dir, "tag", "v0.0.0-shim")
		code, out := r.exec("status")
		if code != 0 || !strings.Contains(out, "version: v0.0.0-shim in this clone; no container runs") ||
			r.count("timeout", "timeout --foreground -k 10 1200 "+export) != 1 || r.count("docker", export) != 1 {
			t.Errorf("first status: exit %d, %d exports:\n%s", code, r.count("docker", export), out)
		}
		if fi, err := os.Stat(filepath.Join(r.dir, "bin", "stargaite-console")); err != nil || fi.Mode().Perm() != 0o755 {
			t.Errorf("bin/stargaite-console: %v", err)
		}
		if code, out := r.exec("status"); code != 0 || !strings.Contains(out, "version: v0.0.0-shim in this clone") || r.count("docker", export) != 1 {
			t.Errorf("second status: exit %d, %d exports, want 1:\n%s", code, r.count("docker", export), out)
		}
		// A dirty clone exports every time.
		if err := os.WriteFile(filepath.Join(r.dir, "compose.yaml"), []byte(r.file("compose.yaml")+"# changed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for i := 2; i <= 3; i++ {
			if code, out := r.exec("status"); code != 0 || !strings.Contains(out, "version: v0.0.0-shim-dirty in this clone") || r.count("docker", export) != i {
				t.Errorf("dirty status: exit %d, %d exports, want %d:\n%s", code, r.count("docker", export), i, out)
			}
		}

		// The console's own version decides too: one that reports a dirty or
		// a dev version is exported again although it equals the clone's.
		for _, c := range []struct {
			name, bin, want string
			git             bool
		}{
			{"dirty console", consoleDirtyBin, "version: v0.0.0-shim-dirty in this clone", true},
			{"dev console", consoleDevBin, "version: dev in this clone", false},
		} {
			r := newRun(t, []string{"SETUP_CONSOLE=" + c.bin})
			if c.git {
				gitIn(t, r.dir, "init", "-q")
				gitIn(t, r.dir, "add", "setup.sh", "compose.yaml")
				gitIn(t, r.dir, "commit", "-q", "-m", "first")
				gitIn(t, r.dir, "tag", "v0.0.0-shim")
				if err := os.WriteFile(filepath.Join(r.dir, "compose.yaml"), []byte(r.file("compose.yaml")+"# changed\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for i := 1; i <= 2; i++ {
				if code, out := r.exec("status"); code != 0 || !strings.Contains(out, c.want) || r.count("docker", export) != i {
					t.Errorf("%s, status %d: exit %d, %d exports, want %d:\n%s", c.name, i, code, r.count("docker", export), i, out)
				}
			}
		}

		r = newRun(t, []string{"FAKE_EXPORT=hang", "FAKE_TIMEOUT=1"})
		start := time.Now()
		code, out = r.exec("status")
		if took := time.Since(start); code != 1 || took > 15*time.Second || !strings.Contains(out, "the console was not built within 20 minutes") || len(r.cli()) != 0 {
			t.Errorf("export outlives its bound: exit %d after %s:\n%s", code, took, out)
		}
	})
}

// TestSetupNoTTY: without a terminal, each missing answer exits 2 naming its
// flag; control: the same run with every answer exits 0. A wrong usage exits
// 2 too.
func TestSetupNoTTY(t *testing.T) {
	port := freePort(t)
	full := acme(port)
	r := newRun(t, []string{"SRC=v"})
	if code, out := r.exec(full...); code != 0 {
		t.Fatalf("control: exit %d:\n%s", code, out)
	}
	for _, c := range []struct{ name, flag, missing string }{
		{"provider", "--provider", "missing --provider"},
		{"base URL", "--base-url", "missing --base-url"},
		{"credential", "--key-env", "missing --key-env"},
		{"model", "--model", "missing --model"},
		{"confirmation", "--yes", "missing --yes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRun(t, []string{"SRC=v"})
			code, out := r.exec(without(full, c.flag)...)
			if code != 2 || !strings.Contains(out, c.missing) {
				t.Errorf("exit %d, want 2 and %q:\n%s", code, c.missing, out)
			}
		})
	}
	// Standard input a terminal is not enough: standard output must be one.
	t.Run("terminal in, pipe out", func(t *testing.T) {
		r := newRun(t, []string{"SRC=v"})
		// The run's PATH has no cat: the pipe ends in builtins.
		code, out := r.pty(append(without(full, "--provider"), `| { read -r -d '' x; printf %s "$x"; }; exit ${PIPESTATUS[0]}`))
		if code != 2 || !strings.Contains(out, "missing --provider") {
			t.Errorf("exit %d, want 2 and missing --provider:\n%s", code, out)
		}
	})
	// On the control's installation, a new port replaces STARGAITE_PORT in
	// .env only once confirmed.
	other := freePort(t)
	if code, out := r.exec(without(append(without(full, "--port"), "--port", other), "--yes")...); code != 2 ||
		!strings.Contains(out, "missing --yes: STARGAITE_PORT is replaced in .env, from "+port+" to "+other) ||
		!strings.Contains(r.file(".env"), "STARGAITE_PORT='"+port+"'") || r.count("docker", "compose up") != 2 {
		t.Errorf("port replaced: exit %d, .env %q:\n%s", code, r.file(".env"), out)
	}
	for _, args := range [][]string{nil, {"install", "--nope"}, {"install", "--port"}, {"frobnicate"}, {"install", "--port", "0"}} {
		r := newRun(t, nil)
		if code, out := r.exec(args...); code != 2 {
			t.Errorf("%q: exit %d, want 2:\n%s", args, code, out)
		}
	}
	// The subcommands of a running installation (C55), each on a new one:
	// a missing answer exits 2 naming its flag, before the change it is for,
	// with .env and the override as they were; a wrong value exits 2 too.
	// Their controls are TestSetupFlags/run again.
	budget := []string{"budget", "--on", "alias:main", "--limit-usd", "5", "--input-price", "1", "--output-price", "2"}
	langfuse := []string{"telemetry", "--backend", "langfuse", "--endpoint", "https://lf.example/api/public/otel",
		"--langfuse-public-env", "SRC", "--langfuse-secret-env", "SRC", "--yes"}
	otlp := []string{"telemetry", "--backend", "otlp", "--endpoint", "https://otlp.example", "--header-name", "Authorization", "--key-env", "SRC", "--yes"}
	for _, c := range []struct {
		args    []string
		missing string
		cli     []string // the CLI calls after the install's, which change nothing
	}{
		{without(budget, "--on"), "missing --on: what the budget is on", nil},
		{without(budget, "--limit-usd"), "missing --limit-usd", nil},
		{without(budget, "--input-price"), "missing --input-price: the price of acme/m1 in USD per million input tokens",
			[]string{"models show -- main", "models set --budget limit_usd=5,reset_interval=monthly -- main"}},
		{append(slices.Clone(budget), "--on", "alias"), "--on takes instance, alias:<name> or key:<name>", nil},
		{append(slices.Clone(budget), "--reset", "weekly"), "--reset takes daily, monthly or never", nil},
		{append(slices.Clone(budget), "--limit-usd", "5,reset_interval=never"), "--limit-usd takes a number of USD", nil},
		{append(slices.Clone(budget), "--input-price", "1,as_of=2020-01-01"), "--input-price and --output-price take a number of USD",
			[]string{"models show -- main", "models set --budget limit_usd=5,reset_interval=monthly -- main"}},
		{without(langfuse, "--backend"), "missing --backend", nil},
		{append(slices.Clone(langfuse), "--backend", "zipkin"), "--backend takes langfuse or otlp", nil},
		{without(langfuse, "--endpoint"), "missing --endpoint", nil},
		{without(langfuse, "--langfuse-public-env"), "missing --langfuse-public-env", []string{"telemetry status"}},
		{without(langfuse, "--langfuse-secret-env"), "missing --langfuse-secret-env", []string{"telemetry status"}},
		{append(slices.Clone(langfuse), "--header-name", "Authorization"), "--header-name and --key-env are for --backend otlp", nil},
		{without(otlp, "--header-name"), "--key-env holds the credential of --header-name: give both", nil},
		{without(otlp, "--key-env"), "missing --key-env: the variable of this script's environment that holds OTLP_AUTHORIZATION", []string{"telemetry status"}},
		{without(otlp, "--yes"), "missing --yes: writing OTLP_AUTHORIZATION to .env, and the container is then recreated", []string{"telemetry status"}},
		{[]string{"provider", "--provider", "ollama"}, "missing --yes: writing compose.override.yaml with host_gateway=1", []string{"providers presets", "providers list",
			"providers add --base-url http://host.docker.internal:11434/v1 --allow-private-network -- ollama"}},
		{[]string{"alias", "--provider", "acme", "--alias", "other"}, "missing --model", []string{"models list", "providers presets", "providers list",
			"providers show -- acme", "providers show -- acme"}},
		{[]string{"status", "--json"}, "unknown flag --json", nil},
		{[]string{"key", "extra"}, "unexpected argument: extra", nil},
	} {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			r := newRun(t, []string{"SRC=v"})
			if code, out := r.exec(acme(freePort(t))...); code != 0 {
				t.Fatalf("install: exit %d:\n%s", code, out)
			}
			before, up, files := len(r.cli()), r.count("docker", "compose up"), r.written()
			code, out := r.exec(c.args...)
			if got := r.cli()[before:]; code != 2 || !strings.Contains(out, c.missing) || !slices.Equal(got, c.cli) || r.count("docker", "compose up") != up {
				t.Errorf("exit %d, want 2 and %q; CLI calls %q, want %q:\n%s", code, c.missing, got, c.cli, out)
			}
			if r.written() != files {
				t.Errorf("files changed:\n%s\nwere:\n%s", r.written(), files)
			}
		})
	}
	r = newRun(t, nil)
	if code, out := r.exec("help"); code != 0 || !strings.Contains(out, "--key-env <VAR>") {
		t.Errorf("help: exit %d:\n%s", code, out)
	}
}

// TestSetupPrompts: at a terminal, install with no flag asks each question
// with its default in brackets, offers the presets and the model list, and
// reaches the first answer.
func TestSetupPrompts(t *testing.T) {
	r := newRun(t, nil)
	code, out := r.pty([]string{"install", "--port", freePort(t)},
		step{"provider: a number or a name: ", "1"},
		step{"OPENAI_API_KEY (typed hidden): ", "typed-secret-1"},
		step{"continue? [y/N]: ", "y"},
		step{"model: a number, an id, or part of one to filter: ", "pt-"},
		step{"model: a number, an id, or part of one to filter: ", "2"},
		step{"a name for the alias your clients call [main]: ", ""},
		step{"empty for none: ", ""},
		step{"a name for the client key [my-app]: ", ""},
		step{"the requests a minute the key may send [60]: ", ""},
	)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	// The terminal echoes what is typed, so the hidden prompt of
	// TestSetupSecrets proves something by echoing nothing.
	for _, s := range []string{"to filter: pt-", "1) openai", "8) dashscope, asks for its URL", "11) deepseek, untested", "19) another OpenAI- or Anthropic-compatible service",
		"3) other-model", "answer: fake reply"} {
		if !strings.Contains(out, s) {
			t.Errorf("no %q in:\n%s", s, out)
		}
	}
	for _, c := range []string{"providers add --base-url https://api.openai.com/v1 -- openai", "targets add --kind chat -- openai/gpt-b",
		"models add --target openai/gpt-b -- main", "keys create --model main --rpm-limit 60 -- my-app"} {
		if !slices.Contains(r.cli(), c) {
			t.Errorf("no CLI call %q in %q", c, r.cli())
		}
	}

	// Run again with no subcommand (C55): the menu of the others; budget
	// asks for the prices of the target its budget reaches unpriced.
	before := len(r.cli())
	code, out = r.pty(nil,
		step{"what to do: a number or a name: ", "4"},
		step{"what the budget is on: instance, alias:<name> or key:<name>: ", "alias:main"},
		step{"the limit in USD: ", "5"},
		step{"when it resets: daily, monthly or never [monthly]: ", ""},
		step{"the price of openai/gpt-b in USD per million input tokens: ", "2"},
		step{"the price of openai/gpt-b in USD per million output tokens: ", "8"},
	)
	want := []string{"models show -- main", "models set --budget limit_usd=5,reset_interval=monthly -- main",
		"targets set --pricing input_per_million=2,output_per_million=8,as_of=" + time.Now().Format(time.DateOnly) + " -- openai/gpt-b",
		"models set --budget limit_usd=5,reset_interval=monthly -- main"}
	if got := r.cli()[before:]; code != 0 || !strings.Contains(out, "1) provider  2) alias  3) key  4) budget  5) telemetry  6) status") || !slices.Equal(got, want) {
		t.Errorf("menu, budget: exit %d, %q, want %q:\n%s", code, got, want, out)
	}

	// telemetry reads a header credential hidden.
	const secret = "typed-otlp-secret"
	code, out = r.pty([]string{"telemetry"},
		step{"the backend: langfuse or otlp: ", "otlp"},
		step{"spans go to <endpoint>/v1/traces: ", "https://otlp.example"},
		step{"empty for none: ", "Authorization"},
		step{"OTLP_AUTHORIZATION (typed hidden): ", secret},
		step{"continue? [y/N]: ", "y"},
	)
	if code != 0 || !strings.Contains(out, "span: accepted") || !strings.Contains(r.file(".env"), "OTLP_AUTHORIZATION='"+secret+"'") {
		t.Errorf("telemetry: exit %d, .env %q:\n%s", code, r.file(".env"), out)
	}
	if found := r.leaks(secret, out); len(found) > 0 {
		t.Errorf("the credential is in %q", found)
	}

	// Langfuse replaces it, once confirmed, and reads its two keys hidden.
	const public, private = "typed-lf-public", "typed-lf-secret"
	code, out = r.pty([]string{"telemetry"},
		step{"the backend: langfuse or otlp: ", "langfuse"},
		step{"https://<host>/api/public/otel: ", "https://lf.example/api/public/otel"},
		step{"continue? [y/N]: ", "y"},
		step{"LANGFUSE_PUBLIC_KEY (typed hidden): ", public},
		step{"LANGFUSE_SECRET_KEY (typed hidden): ", private},
		step{"continue? [y/N]: ", "y"},
	)
	env := r.file(".env")
	if code != 0 || !strings.Contains(out, "is replaced whole") || !strings.Contains(out, "span: accepted") ||
		!strings.Contains(env, "LANGFUSE_PUBLIC_KEY='"+public+"'") || !strings.Contains(env, "LANGFUSE_SECRET_KEY='"+private+"'") {
		t.Errorf("langfuse: exit %d, .env %q:\n%s", code, env, out)
	}
	for _, secret := range []string{public, private} {
		if found := r.leaks(secret, out); len(found) > 0 {
			t.Errorf("%s is in %q", secret, found)
		}
	}

	// A key that exists: Enter skips it, said, and is no failure.
	code, out = r.pty([]string{"key"},
		step{"a name for the alias your clients call [main]: ", ""},
		step{"a name for the client key [my-app]: ", ""},
		step{"another name, or Enter to skip the first answer: ", ""},
	)
	if code != 0 || !strings.Contains(out, "no key made; the first answer is skipped") {
		t.Errorf("key skipped: exit %d:\n%s", code, out)
	}

	// alias asks the alias's name first: one that exists is kept, with no
	// other question and no other call.
	before = len(r.cli())
	code, out = r.pty([]string{"alias"}, step{"a name for the alias your clients call [main]: ", ""})
	if got := r.cli()[before:]; code != 0 || !strings.Contains(out, "alias main exists, toward openai/gpt-b; kept") || !slices.Equal(got, []string{"models list"}) {
		t.Errorf("alias kept: exit %d, %q:\n%s", code, got, out)
	}

	// remote lists this host's addresses, asks for those to publish on and
	// the certificate, then confirms.
	code, out = r.pty([]string{"remote"},
		step{"separated by commas; 127.0.0.1 alone for loopback only: ", "192.0.2.10"},
		step{"the certificate: own (yours, with its key) or self-signed (made here with openssl): ", "self-signed"},
		step{"continue? [y/N]: ", "y"},
	)
	if code != 0 || !strings.Contains(out, "this host's addresses: 192.0.2.10 198.51.100.7\r\n") || !strings.Contains(out, "https on 192.0.2.10:") ||
		!strings.Contains(r.file("compose.override.yaml"), "# setup: addresses=127.0.0.1 192.0.2.10\n") {
		t.Errorf("remote: exit %d:\n%s", code, out)
	}
}

// TestSetupSecrets: a credential reaches .env (mode 0600) byte for byte and
// nothing else: no argv, no output, no other file, not the transcript of the
// hidden prompt. Values .env cannot hold are refused. Control: a copy of
// setup.sh that passes the credential in an argv is caught.
func TestSetupSecrets(t *testing.T) {
	const value = `s3cr3t $HOME #x y\z`
	t.Run("flags", func(t *testing.T) {
		r := newRun(t, []string{"SRC=" + value})
		code, out := r.exec(acme(freePort(t))...)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if found := r.leaks(value, out); len(found) > 0 {
			t.Errorf("the credential is in %q", found)
		}
		if !strings.Contains(r.file(".env"), "\nACME_API_KEY='"+value+"'\n") {
			t.Errorf(".env: %q", r.file(".env"))
		}
		if info, err := os.Stat(filepath.Join(r.dir, ".env")); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf(".env mode: %v %v", info.Mode(), err)
		}
	})
	t.Run("hidden prompt", func(t *testing.T) {
		r := newRun(t, nil)
		args := append(without(acme(freePort(t)), "--key-env"), "--effort=")
		code, out := r.pty(args, step{"ACME_API_KEY (typed hidden): ", value})
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if found := r.leaks(value, out); len(found) > 0 {
			t.Errorf("the credential is in %q", found)
		}
	})
	for _, bad := range []string{"it's", "two\nlines", `ends\`} {
		r := newRun(t, []string{"SRC=" + bad})
		code, out := r.exec(acme(freePort(t))...)
		if code != 1 || !strings.Contains(out, ".env cannot hold it") || strings.Contains(r.file(".env"), "ACME_API_KEY") {
			t.Errorf("%q: exit %d:\n%s", bad, code, out)
		}
	}
	// A value given to --key-env instead of a name is not printed back.
	for _, pasted := range []string{"gsk_PastedValue123", "sk-pasted-value"} {
		r := newRun(t, nil)
		code, out := r.exec(append(without(acme(freePort(t)), "--key-env"), "--key-env", pasted)...)
		if code != 2 || strings.Contains(out, pasted) || !strings.Contains(out, "--key-env takes the name of a variable") {
			t.Errorf("--key-env %s: exit %d:\n%s", pasted, code, out)
		}
	}
	t.Run("leaking control", func(t *testing.T) {
		r := newRun(t, []string{"SRC=" + value})
		src := r.file("setup.sh")
		leaky := strings.Replace(src, "env_set() {\n", "env_set() {\n\tsleep 0 \"$2\"\n", 1)
		if leaky == src {
			t.Fatal("env_set not found in setup.sh")
		}
		if err := os.WriteFile(filepath.Join(r.dir, "setup.sh"), []byte(leaky), 0o700); err != nil {
			t.Fatal(err)
		}
		_, out := r.exec(acme(freePort(t))...)
		if found := r.leaks(value, out); !slices.Contains(found, "the argv of sleep") {
			t.Errorf("the leak was not caught: %q", found)
		}
	})
}

// TestSetupOverride: an override without the marker is the reader's and is
// left untouched, the step that needs it stopping with what to add; control:
// one with the marker is regenerated, keeping the "# setup:" lines it holds.
func TestSetupOverride(t *testing.T) {
	r := newRun(t, nil)
	mine := "services:\n  stargaite:\n    environment:\n      MINE: \"1\"\n"
	if err := os.WriteFile(filepath.Join(r.dir, "compose.override.yaml"), []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	local := []string{"install", "--port", freePort(t), "--provider", "ollama", "--model", "llama3", "--yes"}
	code, out := r.exec(local...)
	if code != 1 || !strings.Contains(out, "compose.override.yaml was not written by setup.sh") || !strings.Contains(out, "host.docker.internal:host-gateway") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if r.file("compose.override.yaml") != mine {
		t.Errorf("the reader's override changed: %q", r.file("compose.override.yaml"))
	}

	// The reader's own override that already holds the line goes on, untouched.
	r = newRun(t, nil)
	theirs := mine + "    extra_hosts:\n      - host.docker.internal:host-gateway\n"
	if err := os.WriteFile(filepath.Join(r.dir, "compose.override.yaml"), []byte(theirs), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := r.exec(local...); code != 0 || r.file("compose.override.yaml") != theirs {
		t.Errorf("an override that holds the line: exit %d, %q:\n%s", code, r.file("compose.override.yaml"), out)
	}

	// The control: an override that holds TLS, as remote writes it, keeps it
	// when the local provider's line joins.
	r = newRun(t, nil)
	marker := "# Written by setup.sh from its \"# setup:\" lines below; it rewrites this file whole.\n"
	setup := "# setup: tls=self-signed\n# setup: tls_name=localhost\n# setup: tls_ca=cert.pem\n# setup: addresses=127.0.0.1\n"
	tls := "    command: [\"serve\", \"--listen\", \"0.0.0.0:4000\", \"--tls-cert\", \"/tls/cert.pem\", \"--tls-key\", \"/tls/key.pem\"]\n" +
		"    ports: !override\n      - \"127.0.0.1:${STARGAITE_PORT:-4000}:4000\"\n    volumes:\n      - ./tls:/tls:ro\n"
	marked := marker + setup + "services:\n  stargaite:\n    extra_hosts:\n      - \"localhost:127.0.0.1\"\n" + tls
	if err := os.WriteFile(filepath.Join(r.dir, "compose.override.yaml"), []byte(marked), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := r.exec(local...); code != 0 {
		t.Fatalf("control: exit %d:\n%s", code, out)
	}
	want := marker + setup + "# setup: host_gateway=1\nservices:\n  stargaite:\n    extra_hosts:\n      - \"host.docker.internal:host-gateway\"\n" +
		"      - \"localhost:127.0.0.1\"\n" + tls
	if got := r.file("compose.override.yaml"); got != want {
		t.Errorf("override:\n%s\nwant:\n%s", got, want)
	}
	if !slices.Contains(r.cli(), "providers add --base-url http://host.docker.internal:11434/v1 --allow-private-network -- ollama") {
		t.Errorf("CLI calls: %q", r.cli())
	}
}

// TestSetupRemote: remote publishes the data plane beyond loopback only with
// TLS (SEC-6). Self-signed, as a user: tls/ is 0750 and its files 0640, of
// the reader's group, which group_add gives the container; the override
// gives serve the certificate, publishes the port on loopback and each
// address with !override and maps the name to loopback; the leaf is CA:FALSE,
// serverAuth only, and names every address; every later CLI call carries the
// admin URL and the certificate (the fake CLI fails without them, as the
// real one does), and every curl speaks HTTPS. The reader's certificate, as
// root: tls/ owned by uid 65532, 0700 and 0600, the files copied as given, a
// private CA trusted per call and a public one not named. Run again, it
// renews keeping the addresses, and repairs a run whose container did not
// start, or a certificate that no longer verifies; a tls/ it cannot write
// leaves the override as it was. Controls: an address beyond loopback
// without a certificate, one that is not this host's, rootless Docker and
// the reader's own override are refused, with nothing written.
func TestSetupRemote(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Fatal("openssl makes the self-signed certificate: ", err)
	}
	marker := "# Written by setup.sh from its \"# setup:\" lines below; it rewrites this file whole.\n"
	// compose.yaml's command, the TLS flags in place of --behind-tls-proxy.
	compose, err := os.ReadFile(filepath.Join("..", "..", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, command, _ := strings.Cut(string(compose), "\n    command: ")
	command, _, _ = strings.Cut(command, "\n")
	if !strings.Contains(command, `"--behind-tls-proxy"`) {
		t.Fatalf("compose.yaml's command has no --behind-tls-proxy: %s", command)
	}
	command = "    command: " + strings.Replace(command, `"--behind-tls-proxy"`, `"--tls-cert", "/tls/cert.pem", "--tls-key", "/tls/key.pem"`, 1) + "\n"
	published := func(addrs ...string) string {
		s := "    ports: !override\n"
		for _, a := range addrs {
			s += "      - \"" + a + ":${STARGAITE_PORT:-4000}:4000\"\n"
		}
		return s + "    volumes:\n      - ./tls:/tls:ro\n"
	}
	mode := func(r *run, name string) fs.FileMode {
		t.Helper()
		info, err := os.Stat(filepath.Join(r.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode().Perm()
	}
	// certOf is the certificate in tls/cert.pem, ipsOf the addresses on it.
	certOf := func(t *testing.T, r *run) *x509.Certificate {
		t.Helper()
		block, _ := pem.Decode([]byte(r.file("tls/cert.pem")))
		if block == nil {
			t.Fatalf("no PEM certificate: %q", r.file("tls/cert.pem"))
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return cert
	}
	ipsOf := func(cert *x509.Certificate) string {
		var list []string
		for _, ip := range cert.IPAddresses {
			list = append(list, ip.String())
		}
		return strings.Join(list, " ")
	}
	// execs are the docker compose exec calls of the CLI from the n-th
	// recorded call on.
	execs := func(r *run, n int) []string {
		var list []string
		for _, c := range r.calls()[n:] {
			if c.Argv[0] == "docker" && slices.Contains(c.Argv, "/stargaite") {
				list = append(list, strings.Join(c.Argv, " "))
			}
		}
		return list
	}
	// ready checks that every readiness check from the n-th recorded call on
	// reaches name on an address and trusts the certificate of cacert.
	ready := func(t *testing.T, r *run, n int, name, port, cacert string) {
		t.Helper()
		for _, c := range r.calls()[n:] {
			if line := strings.Join(c.Argv, " "); c.Argv[0] == "curl" && strings.HasSuffix(line, "/health/ready") &&
				(!strings.Contains(line, " --resolve "+name+":"+port+":") || !strings.Contains(line, cacert)) {
				t.Errorf("a readiness check without --resolve or %q: %s", cacert, line)
			}
		}
	}

	t.Run("self-signed", func(t *testing.T) {
		r := newRun(t, []string{"SRC=v"})
		port := freePort(t)
		if code, out := r.exec(acme(port)...); code != 0 {
			t.Fatalf("install: exit %d:\n%s", code, out)
		}
		args := []string{"remote", "--address", "192.0.2.10,198.51.100.7,192.0.2.10", "--tls", "self-signed"}
		files := r.written()
		code, out := r.exec(args...)
		if _, err := os.Stat(filepath.Join(r.dir, "tls")); code != 2 || !strings.Contains(out, "missing --yes: writing compose.override.yaml with tls=self-signed") ||
			r.written() != files || !os.IsNotExist(err) {
			t.Errorf("without --yes: exit %d, tls/ %v:\n%s", code, err, out)
		}
		n := len(r.calls())
		code, out = r.exec(append(args, "--yes")...)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		gid := strconv.Itoa(os.Getegid())
		want := marker + "# setup: tls=self-signed\n# setup: tls_name=localhost\n# setup: tls_ca=cert.pem\n# setup: tls_group=" + gid +
			"\n# setup: addresses=127.0.0.1 192.0.2.10 198.51.100.7\nservices:\n  stargaite:\n    extra_hosts:\n      - \"localhost:127.0.0.1\"\n" +
			command + published("127.0.0.1", "192.0.2.10", "198.51.100.7") + "    group_add:\n      - \"" + gid + "\"\n"
		if got := r.file("compose.override.yaml"); got != want {
			t.Errorf("override:\n%s\nwant:\n%s", got, want)
		}
		if mode(r, "tls") != 0o750 || mode(r, "tls/cert.pem") != 0o640 || mode(r, "tls/key.pem") != 0o640 || r.count("chown", "chown -R :"+gid+" tls") != 1 {
			t.Errorf("tls/ %v, cert %v, key %v, chown %d", mode(r, "tls"), mode(r, "tls/cert.pem"), mode(r, "tls/key.pem"), r.count("chown", ""))
		}
		cert := certOf(t, r)
		host, err := os.Hostname()
		if err != nil {
			t.Fatal(err)
		}
		if !cert.BasicConstraintsValid || cert.IsCA || !slices.Equal(cert.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) ||
			!slices.Contains(cert.DNSNames, "localhost") || !slices.Contains(cert.DNSNames, host) || cert.NotAfter.Sub(cert.NotBefore) != 825*24*time.Hour {
			t.Errorf("certificate: CA %v (valid %v), usages %v, names %q, valid %s", cert.IsCA, cert.BasicConstraintsValid, cert.ExtKeyUsage,
				cert.DNSNames, cert.NotAfter.Sub(cert.NotBefore))
		}
		if ips := ipsOf(cert); ips != "127.0.0.1 192.0.2.10 198.51.100.7" {
			t.Errorf("addresses on the certificate: %s", ips)
		}
		for _, a := range []string{"127.0.0.1", "192.0.2.10", "198.51.100.7"} {
			c := r.count("curl", "--resolve localhost:"+port+":"+a+" --cacert tls/cert.pem https://localhost:"+port+"/health/ready")
			if c < 1 {
				t.Errorf("no HTTPS check on %s", a)
			}
		}
		for _, s := range []string{"tls/ gets the certificate and its key, and from now on the data plane speaks HTTPS only", "https on 192.0.2.10:" + port + ": ready",
			"warning: Docker publishes past ufw and firewalld: every machine that reaches 192.0.2.10 198.51.100.7 reaches port " + port + ";",
			"the certificate expires on ", "curl --cacert cert.pem", "NODE_EXTRA_CA_CERTS=cert.pem",
			"base URL https://localhost:" + port + "/v1 here",
			"alias stargaite='docker compose exec -T -e STARGAITE_ADMIN_URL=https://localhost:4001 -e SSL_CERT_FILE=/tls/cert.pem stargaite /stargaite'"} {
			if !strings.Contains(out, s) {
				t.Errorf("no %q in:\n%s", s, out)
			}
		}
		if r.count("docker", "compose up -d --no-build") != 2 {
			t.Errorf("%d recreates, want 2", r.count("docker", "compose up -d --no-build"))
		}

		// Every later CLI call carries the admin URL and the certificate,
		// every readiness check the name and the certificate, and the first
		// answer goes over HTTPS.
		ready(t, r, n, "localhost", port, " --cacert tls/cert.pem ")
		n = len(r.calls())
		if code, out := r.exec("key", "--alias", "main", "--key-name", "second"); code != 0 || !strings.Contains(out, "answer: fake reply") {
			t.Errorf("key: exit %d:\n%s", code, out)
		}
		if code, out := r.exec("status"); code != 0 || !strings.Contains(out, "provider acme: credential resolved") {
			t.Errorf("status: exit %d:\n%s", code, out)
		}
		for _, c := range execs(r, n) {
			if !strings.Contains(c, "docker compose exec -T -e STARGAITE_ADMIN_URL=https://localhost:4001 -e SSL_CERT_FILE=/tls/cert.pem stargaite /stargaite ") {
				t.Errorf("a CLI call without the admin variables: %s", c)
			}
		}
		ready(t, r, n, "localhost", port, " --cacert tls/cert.pem ")
		if c := r.curlOf("https://localhost:" + port + "/v1/chat/completions"); !strings.Contains(strings.Join(c.Argv, " "), "--resolve localhost:"+port+":127.0.0.1 --cacert tls/cert.pem") {
			t.Errorf("first answer: %q", c.Argv)
		}

		// Run again without --address, it renews keeping the addresses: a
		// new certificate, the container recreated although its
		// configuration did not change; not without --yes.
		old, up := r.file("tls/cert.pem"), r.count("docker", "compose up")
		code, out = r.exec("remote", "--tls", "self-signed")
		if code != 2 || !strings.Contains(out, "missing --yes: the certificate and key in tls/ are replaced") || r.file("tls/cert.pem") != old ||
			r.count("docker", "compose up") != up {
			t.Errorf("renew without --yes: exit %d, %d ups after %d:\n%s", code, r.count("docker", "compose up"), up, out)
		}
		code, out = r.exec("remote", "--tls", "self-signed", "--yes")
		if code != 0 || !strings.Contains(out, "the certificate and key in tls/ are replaced, and the container is then recreated") ||
			r.file("tls/cert.pem") == old || r.file("compose.override.yaml") != want || r.count("docker", "compose up -d --no-build --force-recreate") != 2 {
			t.Errorf("renew: exit %d:\n%s\noverride:\n%s", code, out, r.file("compose.override.yaml"))
		}
	})

	t.Run("own certificate as root", func(t *testing.T) {
		src := t.TempDir()
		for name, text := range map[string]string{"chain.pem": "CHAIN\n", "key.pem": "KEY\n", "ca.pem": "CA\n"} {
			if err := os.WriteFile(filepath.Join(src, name), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		own := []string{"remote", "--address", "192.0.2.10", "--tls", "own", "--tls-cert", filepath.Join(src, "chain.pem"),
			"--tls-key", filepath.Join(src, "key.pem"), "--tls-name", "proxy.example.com", "--yes"}
		for _, c := range []struct {
			name, ca, cacert string
			admin            string
		}{
			{"private CA", "ca.pem", " --cacert tls/ca.pem", "-e STARGAITE_ADMIN_URL=https://proxy.example.com:4001 -e SSL_CERT_FILE=/tls/ca.pem stargaite"},
			{"public CA", "", "", "-e STARGAITE_ADMIN_URL=https://proxy.example.com:4001 stargaite"},
		} {
			t.Run(c.name, func(t *testing.T) {
				r := newRun(t, []string{"SRC=v", "FAKE_UID=0", "FAKE_OWNER=0"})
				port := freePort(t)
				if code, out := r.exec(acme(port)...); code != 0 {
					t.Fatalf("install: exit %d:\n%s", code, out)
				}
				args := own
				if c.ca != "" {
					args = append(slices.Clone(own), "--tls-ca", filepath.Join(src, "ca.pem"))
				}
				n := len(r.calls())
				code, out := r.exec(args...)
				if code != 0 {
					t.Fatalf("exit %d:\n%s", code, out)
				}
				ready(t, r, n, "proxy.example.com", port, c.cacert)
				ca := ""
				if c.ca != "" {
					ca = "# setup: tls_ca=ca.pem\n"
				}
				want := marker + "# setup: tls=own\n# setup: tls_name=proxy.example.com\n" + ca + "# setup: addresses=127.0.0.1 192.0.2.10\n" +
					"services:\n  stargaite:\n    extra_hosts:\n      - \"proxy.example.com:127.0.0.1\"\n" + command + published("127.0.0.1", "192.0.2.10")
				if got := r.file("compose.override.yaml"); got != want {
					t.Errorf("override:\n%s\nwant:\n%s", got, want)
				}
				if r.file("tls/cert.pem") != "CHAIN\n" || r.file("tls/key.pem") != "KEY\n" || mode(r, "tls") != 0o700 || mode(r, "tls/cert.pem") != 0o600 ||
					mode(r, "tls/key.pem") != 0o600 || r.count("chown", "chown -R 65532:65532 tls") != 1 {
					t.Errorf("tls/: %v %v %v, chown %d", mode(r, "tls"), mode(r, "tls/cert.pem"), mode(r, "tls/key.pem"), r.count("chown", "65532"))
				}
				if _, err := os.Stat(filepath.Join(r.dir, "tls", "ca.pem")); (c.ca != "") != (err == nil) {
					t.Errorf("tls/ca.pem: %v", err)
				}
				if r.count("curl", "--resolve proxy.example.com:"+port+":192.0.2.10"+c.cacert+" https://proxy.example.com:"+port+"/health/ready") != 1 ||
					strings.Contains(out, "NODE_EXTRA_CA_CERTS") {
					t.Errorf("HTTPS checks or trust lines:\n%s", out)
				}
				n = len(r.calls())
				if code, out := r.exec("status"); code != 0 {
					t.Errorf("status: exit %d:\n%s", code, out)
				}
				for _, e := range execs(r, n) {
					if !strings.Contains(e, " exec -T "+c.admin+" /stargaite ") {
						t.Errorf("a CLI call without %q: %s", c.admin, e)
					}
				}
			})
		}
	})

	// A remote whose container did not start leaves it stopped: run again
	// with the right answer, it repairs it; 0.0.0.0 stands alone, and the
	// certificate names the addresses given and this host's instead.
	t.Run("repairs a failed start", func(t *testing.T) {
		r := newRun(t, []string{"SRC=v"})
		if code, out := r.exec("remote", "--tls", "self-signed", "--yes"); code != 1 || !strings.Contains(out, "no container here: ./setup.sh install makes it") {
			t.Errorf("before install: exit %d:\n%s", code, out)
		}
		port := freePort(t)
		if code, out := r.exec(acme(port)...); code != 0 {
			t.Fatalf("install: exit %d:\n%s", code, out)
		}
		r.env = append(r.env, "FAKE_UP=fail")
		if code, out := r.exec("remote", "--address", "192.0.2.10", "--tls", "self-signed", "--yes"); code != 1 || !strings.Contains(out, "docker compose up failed") ||
			!strings.Contains(out, "setup.sh: ./setup.sh remote again repairs it") {
			t.Errorf("failed start: exit %d:\n%s", code, out)
		}
		r.env = r.env[:len(r.env)-1]
		code, out := r.exec("remote", "--address", "198.51.100.7,0.0.0.0", "--tls", "self-signed", "--yes")
		if code != 0 || !strings.Contains(out, "https on 0.0.0.0:"+port+": ready") || strings.Contains(out, "https on 198.51.100.7") ||
			!strings.Contains(r.file("compose.override.yaml"), "# setup: addresses=0.0.0.0\n") || !strings.Contains(r.file("compose.override.yaml"), "      - \"0.0.0.0:${STARGAITE_PORT:-4000}:4000\"\n    volumes:") {
			t.Errorf("repair: exit %d:\n%s\noverride:\n%s", code, out, r.file("compose.override.yaml"))
		}
		if ips := ipsOf(certOf(t, r)); ips != "127.0.0.1 198.51.100.7 192.0.2.10" || !strings.Contains(out, "every machine that reaches this host reaches port "+port+";") {
			t.Errorf("addresses on the certificate: %s:\n%s", ips, out)
		}
		r.env = append(r.env, "FAKE_ADDRESSES=192.0.2.10 2001:db8::1 198.51.100.7 ")
		if code, out := r.exec("remote", "--address", "0.0.0.0", "--tls", "self-signed", "--yes"); code != 0 || ipsOf(certOf(t, r)) != "127.0.0.1 192.0.2.10 198.51.100.7" {
			t.Errorf("0.0.0.0 alone: exit %d, addresses on the certificate %s:\n%s", code, ipsOf(certOf(t, r)), out)
		}
	})

	// openssl is checked for self-signed only: without it, self-signed is
	// refused with nothing written, and own works, with no expiry line.
	t.Run("without openssl", func(t *testing.T) {
		r := newRun(t, []string{"SRC=v"}, "openssl")
		if code, out := r.exec(acme(freePort(t))...); code != 0 {
			t.Fatalf("install: exit %d:\n%s", code, out)
		}
		files, up := r.written(), r.count("docker", "compose up")
		code, out := r.exec("remote", "--address", "192.0.2.10", "--tls", "self-signed", "--yes")
		if _, err := os.Stat(filepath.Join(r.dir, "tls")); code != 1 || !strings.Contains(out, "openssl makes the self-signed certificate") ||
			r.written() != files || !os.IsNotExist(err) || r.count("docker", "compose up") != up {
			t.Errorf("self-signed: exit %d, tls/ %v:\n%s", code, err, out)
		}
		abs, err := filepath.Abs("setup_test.go")
		if err != nil {
			t.Fatal(err)
		}
		code, out = r.exec("remote", "--address", "192.0.2.10", "--tls", "own", "--tls-cert", abs, "--tls-key", abs, "--tls-name", "proxy.example.com", "--yes")
		if code != 0 || strings.Contains(out, "the certificate expires on") || !strings.Contains(out, "https on 192.0.2.10:") {
			t.Errorf("own: exit %d:\n%s", code, out)
		}
	})

	// The group the container joins to read tls/ is warned of when another
	// user has it as primary group or it lists members, as NSS reports them.
	t.Run("group members", func(t *testing.T) {
		gid := strconv.Itoa(os.Getegid())
		warning := "warning: group " + gid + ", which the container joins to read tls/, has other members, who can read its key too"
		r := newRun(t, []string{"SRC=v"})
		if code, out := r.exec(acme(freePort(t))...); code != 0 {
			t.Fatalf("install: exit %d:\n%s", code, out)
		}
		env := r.env
		me := "me:x:1000:" + gid + "::/home/me:/bin/sh\n"
		for _, c := range []struct {
			passwd, group string
			warn          bool
		}{
			{me, "me:x:" + gid + ":\n", false},
			{me + "you:x:1001:" + gid + "::/home/you:/bin/sh\n", "me:x:" + gid + ":\n", true},
			{me, "users:x:" + gid + ":you\n", true},
			{me, "users:x:" + gid + ":me\n", false},
			{me, "users:x:" + gid + ":me,you\n", true},
		} {
			r.env = append(slices.Clone(env), "FAKE_GETENT_PASSWD="+c.passwd, "FAKE_GETENT_GROUP="+c.group)
			if code, out := r.exec("remote", "--address", "192.0.2.10", "--tls", "self-signed", "--yes"); code != 0 || strings.Contains(out, warning) != c.warn {
				t.Errorf("%q %q: exit %d, want warning %v:\n%s", c.passwd, c.group, code, c.warn, out)
			}
		}
	})

	// Without hostname and getent: no error line, and 0.0.0.0 alone is warned
	// of, its certificate naming no address of this host. The flags of own are
	// refused with self-signed. Once remote is on, install again prints a
	// client example that trusts the certificate, and a container that never
	// gets ready names remote as the repair.
	t.Run("without hostname and getent", func(t *testing.T) {
		r := newRun(t, []string{"SRC=v"}, "hostname", "getent")
		port := freePort(t)
		if code, out := r.exec(acme(port)...); code != 0 {
			t.Fatalf("install: exit %d:\n%s", code, out)
		}
		files := r.written()
		code, out := r.exec("remote", "--tls", "self-signed", "--tls-name", "proxy.example.com", "--yes")
		if code != 2 || !strings.Contains(out, "--tls-cert, --tls-key, --tls-name and --tls-ca are for --tls own") || r.written() != files {
			t.Errorf("own flags with self-signed: exit %d:\n%s", code, out)
		}
		code, out = r.exec("remote", "--address", "0.0.0.0", "--tls", "self-signed", "--yes")
		if code != 0 || strings.Contains(out, "not found") || strings.Contains(out, "getent") || ipsOf(certOf(t, r)) != "127.0.0.1" ||
			!strings.Contains(out, "warning: this host's addresses are unknown here (no hostname -I), so the certificate names only those given beside 0.0.0.0") {
			t.Errorf("0.0.0.0: exit %d, addresses on the certificate %s:\n%s", code, ipsOf(certOf(t, r)), out)
		}
		if code, out := r.exec(acme(port)...); code != 0 || !strings.Contains(out, "  curl --cacert tls/cert.pem https://localhost:"+port+"/v1/chat/completions -H") {
			t.Errorf("install again: exit %d:\n%s", code, out)
		}
		r.env = append(r.env, "FAKE_READY=never")
		if code, out := r.exec("key", "--alias", "main", "--key-name", "second"); code != 1 || !strings.Contains(out, "a log line\n./setup.sh remote again repairs a remote that did not finish") {
			t.Errorf("never ready: exit %d:\n%s", code, out)
		}
	})

	// tls/ that cannot be written (a file in its place) leaves the override
	// byte for byte, so the next run reads the state that works.
	t.Run("tls write fails", func(t *testing.T) {
		r := newRun(t, []string{"SRC=v"})
		if code, out := r.exec(acme(freePort(t))...); code != 0 {
			t.Fatalf("install: exit %d:\n%s", code, out)
		}
		if code, out := r.exec("remote", "--address", "192.0.2.10", "--tls", "self-signed", "--yes"); code != 0 {
			t.Fatalf("remote: exit %d:\n%s", code, out)
		}
		before := r.file("compose.override.yaml")
		tls := filepath.Join(r.dir, "tls")
		if err := os.RemoveAll(tls); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tls, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out := r.exec("remote", "--address", "198.51.100.7", "--tls", "self-signed", "--yes")
		if code != 1 || !strings.Contains(out, "tls/ was not written, or not given its owner and modes; compose.override.yaml is as it was") ||
			r.file("compose.override.yaml") != before {
			t.Errorf("exit %d:\n%s\noverride:\n%s", code, out, r.file("compose.override.yaml"))
		}
		if code, out := r.exec("status"); code != 0 {
			t.Errorf("status after: exit %d:\n%s", code, out)
		}
	})

	// The certificate the container serves has expired, so no readiness
	// check passes: remote still renews it, and the container serves the
	// new one.
	t.Run("renews an expired certificate", func(t *testing.T) {
		r := newRun(t, []string{"SRC=v"})
		port := freePort(t)
		if code, out := r.exec(acme(port)...); code != 0 {
			t.Fatalf("install: exit %d:\n%s", code, out)
		}
		if code, out := r.exec("remote", "--address", "192.0.2.10", "--tls", "self-signed", "--yes"); code != 0 {
			t.Fatalf("remote: exit %d:\n%s", code, out)
		}
		before := r.file("compose.override.yaml")
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"localhost"},
			NotBefore: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
		if err != nil {
			t.Fatal(err)
		}
		expired := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		if err := os.WriteFile(filepath.Join(r.dir, "tls", "cert.pem"), expired, 0o640); err != nil {
			t.Fatal(err)
		}
		// The container restarted on it, as one does 825 days on.
		restart := exec.Command(filepath.Join(r.shims, "docker"), "compose", "up", "-d", "--force-recreate")
		restart.Env, restart.Dir = r.env, r.dir
		if out, err := restart.CombinedOutput(); err != nil {
			t.Fatalf("restart: %v: %s", err, out)
		}
		if code, out := r.exec("key", "--alias", "main", "--key-name", "k2"); code != 1 || !strings.Contains(out, "not ready within 120 s") {
			t.Fatalf("the shim does not refuse the expired certificate: exit %d:\n%s", code, out)
		}
		code, out := r.exec("remote", "--tls", "self-signed", "--yes")
		if code != 0 || !strings.Contains(out, "the certificate and key in tls/ are replaced") || !strings.Contains(out, "https on 192.0.2.10:"+port+": ready") ||
			r.file("tls/cert.pem") == string(expired) || r.file("compose.override.yaml") != before {
			t.Errorf("renew: exit %d:\n%s", code, out)
		}
	})

	// Refused, with nothing written: an address beyond loopback without a
	// certificate, an unreadable one or a relative path, a wrong address, one
	// that is not this host's, a wrong choice, rootless Docker or
	// user-namespace remapping, and the reader's own override.
	abs, err := filepath.Abs("setup_test.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		env  []string
		args []string
		code int
		want string
		mine bool
	}{
		{nil, []string{"--address", "192.0.2.10", "--yes"}, 2, "missing --tls: the certificate", false},
		{nil, []string{"--address", "192.0.2.10", "--tls", "own", "--tls-key", "k.pem", "--tls-name", "proxy.example.com", "--yes"}, 2, "missing --tls-cert", false},
		{nil, []string{"--address", "192.0.2.10", "--tls", "own", "--tls-cert", "/nonexistent/nope.pem", "--tls-key", abs, "--tls-name", "p", "--yes"}, 1,
			"/nonexistent/nope.pem is not a file setup.sh can read; nothing was changed", false},
		{nil, []string{"--tls", "own", "--tls-cert", abs, "--tls-key", "setup.sh", "--tls-name", "p", "--yes"}, 2,
			"--tls-cert, --tls-key and --tls-ca take absolute paths: not setup.sh", false},
		{nil, []string{"--address", "192.0.2", "--tls", "self-signed", "--yes"}, 2, "--address takes IPv4 addresses", false},
		{nil, []string{"--address", "192.0.2.256", "--tls", "self-signed", "--yes"}, 2, "--address takes IPv4 addresses", false},
		{nil, []string{"--address", "192.0.2.10,203.0.113.5", "--tls", "self-signed", "--yes"}, 2,
			"203.0.113.5 is not an address of this host, which has 192.0.2.10 198.51.100.7", false},
		{nil, []string{"--tls", "plain", "--yes"}, 2, "--tls takes own or self-signed", false},
		{nil, []string{"--tls", "own", "--tls-cert", abs, "--tls-key", abs, "--tls-name", "a b", "--yes"}, 2, "--tls-name takes a host name", false},
		{[]string{"FAKE_SECURITY=[name=seccomp,profile=builtin name=rootless name=cgroupns]"}, []string{"--tls", "self-signed", "--yes"}, 1, "Docker runs rootless", false},
		{[]string{"FAKE_SECURITY=[name=userns name=seccomp,profile=builtin]"}, []string{"--tls", "self-signed", "--yes"}, 1, "user-namespace remapping", false},
		{nil, []string{"--tls", "self-signed", "--yes"}, 1, "compose.override.yaml was not written by setup.sh, so it is left as it is: move it away and run this again", true},
	} {
		t.Run(strings.Join(append(c.env, c.args...), " "), func(t *testing.T) {
			r := newRun(t, append([]string{"SRC=v"}, c.env...))
			if code, out := r.exec(acme(freePort(t))...); code != 0 {
				t.Fatalf("install: exit %d:\n%s", code, out)
			}
			if c.mine {
				if err := os.WriteFile(filepath.Join(r.dir, "compose.override.yaml"), []byte("services: {}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			files, up := r.written(), r.count("docker", "compose up")
			code, out := r.exec(append([]string{"remote"}, c.args...)...)
			if _, err := os.Stat(filepath.Join(r.dir, "tls")); code != c.code || !strings.Contains(out, c.want) || r.written() != files ||
				!os.IsNotExist(err) || r.count("docker", "compose up") != up || r.count("openssl", "") != 0 {
				t.Errorf("exit %d, want %d and %q; tls/ %v:\n%s", code, c.code, c.want, err, out)
			}
		})
	}
}

// TestSetupPrereqs: everything missing is reported at once, each with where
// to get it, before anything runs; control: Compose 2.24.4 passes.
func TestSetupPrereqs(t *testing.T) {
	port := freePort(t)
	for _, c := range []struct {
		name    string
		env     []string
		without []string
		want    []string
	}{
		{"missing tools", nil, []string{"docker", "curl", "jq"}, []string{"Docker: see https://docs.docker.com/engine/install/",
			"curl: see https://curl.se/download.html", "jq: see https://jqlang.org/download/"}},
		{"no permission", []string{"FAKE_DOCKER=denied"}, nil, []string{"join the docker group, which is equivalent to root"}},
		{"daemon down", []string{"FAKE_DOCKER=down"}, nil, []string{"a reachable Docker daemon"}},
		{"old compose", []string{"FAKE_COMPOSE=v2.24.3"}, nil, []string{"Docker Compose 2.24.4 or later, found v2.24.3"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRun(t, append([]string{"SRC=v"}, c.env...), c.without...)
			code, out := r.exec(acme(port)...)
			for _, s := range c.want {
				if !strings.Contains(out, s) {
					t.Errorf("no %q in:\n%s", s, out)
				}
			}
			if code != 1 || r.count("docker", "compose up") != 0 {
				t.Errorf("exit %d, or it went on:\n%s", code, out)
			}
		})
	}
	t.Run("port taken", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		taken := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		r := newRun(t, []string{"SRC=v"})
		code, out := r.exec(acme(taken)...)
		if code != 1 || !strings.Contains(out, "127.0.0.1:"+taken+" is taken; give another with --port <n>") {
			t.Errorf("exit %d:\n%s", code, out)
		}
	})
	// While the container runs, a new port is checked too, before .env or
	// the container change.
	t.Run("port taken while it runs", func(t *testing.T) {
		r := newRun(t, []string{"SRC=v"})
		if code, out := r.exec(acme(port)...); code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		taken := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		code, out := r.exec(acme(taken)...)
		if code != 1 || !strings.Contains(out, "127.0.0.1:"+taken+" is taken") || !strings.Contains(r.file(".env"), "STARGAITE_PORT='"+port+"'") ||
			r.count("docker", "compose up") != 2 {
			t.Errorf("exit %d, .env %q:\n%s", code, r.file(".env"), out)
		}
	})
	t.Run("control", func(t *testing.T) {
		r := newRun(t, []string{"SRC=v", "FAKE_COMPOSE=v2.24.4"})
		if code, out := r.exec(acme(port)...); code != 0 {
			t.Errorf("exit %d:\n%s", code, out)
		}
	})
}

// TestSetupModels: at a terminal, the model is offered from the provider's
// list: openai with a Bearer header, anthropic with x-api-key and
// anthropic-version, a local provider at its host-side URL while the target
// gets host.docker.internal; the credential is in no argv, only in the
// config curl reads from standard input. Controls: an error, an empty list
// and an answer that is not JSON each print one line and fall back to a
// typed id.
func TestSetupModels(t *testing.T) {
	rest := []string{"--alias", "main", "--effort=", "--key-name", "app", "--rpm-limit", "30", "--yes"}
	pick := step{"model: a number, an id, or part of one to filter: ", "2"}
	// The value holds " and \, which the curl config escapes.
	const value = `k"1\2`
	t.Run("openai", func(t *testing.T) {
		r := newRun(t, []string{"SRC=" + value})
		args := append(without(acme(freePort(t)), "--model"), "--effort=")
		code, out := r.pty(args, pick)
		if code != 0 || !slices.Contains(r.cli(), "targets add --kind chat -- acme/gpt-b") {
			t.Fatalf("exit %d, %q:\n%s", code, r.cli(), out)
		}
		if c := r.curlOf("https://api.acme.example/v1/models"); c.Stdin != `header = "Authorization: Bearer k\"1\\2"`+"\n" {
			t.Errorf("curl config: %q", c.Stdin)
		}
		if found := r.leaks(value, out); len(found) > 0 {
			t.Errorf("the credential is in %q", found)
		}
	})
	t.Run("anthropic", func(t *testing.T) {
		r := newRun(t, []string{"SRC=sk-ant-1"})
		code, out := r.pty(append([]string{"install", "--port", freePort(t), "--provider", "anthropic", "--key-env", "SRC", "--max-output-tokens", "1024"}, rest...), pick)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		c := r.curlOf("https://api.anthropic.com/v1/models?limit=1000")
		if c.Stdin != "header = \"x-api-key: sk-ant-1\"\n" || !slices.Contains(c.Argv, "anthropic-version: 2023-06-01") {
			t.Errorf("curl: %q, config %q", c.Argv, c.Stdin)
		}
	})
	t.Run("local", func(t *testing.T) {
		r := newRun(t, nil)
		code, out := r.pty(append([]string{"install", "--port", freePort(t), "--provider", "ollama"}, rest...), pick)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if c := r.curlOf("http://localhost:11434/v1/models"); slices.Contains(c.Argv, "-K") {
			t.Errorf("a config for a provider without a credential: %q", c.Argv)
		}
		if !slices.Contains(r.cli(), "providers add --base-url http://host.docker.internal:11434/v1 --allow-private-network -- ollama") ||
			!slices.Contains(r.cli(), "targets add --kind chat -- ollama/gpt-b") {
			t.Errorf("CLI calls: %q", r.cli())
		}
	})
	for _, answer := range []string{"error", "empty", "nonjson"} {
		t.Run("control "+answer, func(t *testing.T) {
			r := newRun(t, []string{"SRC=" + value, "FAKE_MODELS=" + answer})
			args := append(without(acme(freePort(t)), "--model"), "--effort=")
			code, out := r.pty(args, step{"the model id at acme: ", "typed-id"})
			if code != 0 || !slices.Contains(r.cli(), "targets add --kind chat -- acme/typed-id") {
				t.Fatalf("exit %d, %q:\n%s", code, r.cli(), out)
			}
			if n := strings.Count(out, "the model list at https://api.acme.example/v1"); n != 1 {
				t.Errorf("%d lines on the list, want 1:\n%s", n, out)
			}
			if found := r.leaks(value, out); len(found) > 0 {
				t.Errorf("the credential is in %q", found)
			}
		})
	}
}

// TestSetupPaidCalls: one verify and one completion per run, each announced;
// none after a failed verify, no second attempt of either, no verify of a
// verified target unless --reverify.
func TestSetupPaidCalls(t *testing.T) {
	port := freePort(t)
	r := newRun(t, []string{"SRC=v"})
	code, out := r.exec(acme(port)...)
	if code != 0 || r.count("docker", "targets verify") != 1 || r.count("curl", "chat/completions") != 1 {
		t.Fatalf("exit %d, %q:\n%s", code, r.cli(), out)
	}
	for _, s := range []string{"targets verify acme/m1: 1 paid call(s) to acme, each capped at 256 output tokens, never retried; interrupted, the server still ends them within 185 s",
		"one test chat completion through the proxy: a paid call through alias main, capped at 256 output tokens, never retried"} {
		if !strings.Contains(out, s) {
			t.Errorf("no %q in:\n%s", s, out)
		}
	}
	// Run again with a new key: the target is verified, so no verify.
	if code, out := r.exec(append(without(acme(port), "--key-name"), "--key-name", "second")...); code != 0 || r.count("docker", "targets verify") != 1 || r.count("curl", "chat/completions") != 2 {
		t.Errorf("verified target: exit %d, %q:\n%s", code, r.cli(), out)
	}
	if code, out := r.exec(append(without(acme(port), "--key-name"), "--key-name", "third", "--reverify")...); code != 0 || r.count("docker", "targets verify") != 2 {
		t.Errorf("--reverify: exit %d, %q:\n%s", code, r.cli(), out)
	}

	r = newRun(t, []string{"SRC=v", "FAKE_VERIFY=fail"})
	code, out = r.exec(acme(freePort(t))...)
	if code != 1 || r.count("docker", "targets verify") != 1 || r.count("docker", "models add") != 0 || r.count("docker", "keys create") != 0 || r.count("curl", "chat/completions") != 0 {
		t.Errorf("failed verify: exit %d, %q:\n%s", code, r.cli(), out)
	}
	if !strings.Contains(out, "status 404, model not found: acme/m1") || !strings.Contains(out, "no alias or key was created") {
		t.Errorf("failed verify: no provider message:\n%s", out)
	}

	r = newRun(t, []string{"SRC=v", "FAKE_CHAT=fail"})
	code, out = r.exec(acme(freePort(t))...)
	if code != 1 || r.count("curl", "chat/completions") != 1 || !strings.Contains(out, "the test call answered 502: the provider failed") {
		t.Errorf("failed completion: exit %d:\n%s", code, out)
	}

	r = newRun(t, []string{"SRC=v", "FAKE_CHAT=timeout"})
	code, out = r.exec(acme(freePort(t))...)
	if code != 1 || r.count("curl", "chat/completions") != 1 || !strings.Contains(out, "the test call did not complete (curl exit 28)") {
		t.Errorf("curl failed: exit %d:\n%s", code, out)
	}
}

// TestSetupBounds: every wait is bounded (rule 6) and the paid test call is
// capped: every curl has --max-time, docker compose up runs under timeout,
// the test completion asks for 256 tokens at most; a container never ready
// stops after 120 checks with the last log lines; a docker compose up that
// outlives its bound is stopped.
func TestSetupBounds(t *testing.T) {
	r := newRun(t, []string{"SRC=v"})
	port := freePort(t)
	if code, out := r.exec(acme(port)...); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, c := range r.calls() {
		if c.Argv[0] == "curl" && !slices.Contains(c.Argv, "--max-time") {
			t.Errorf("curl without --max-time: %q", c.Argv)
		}
	}
	if c := r.curlOf("http://127.0.0.1:" + port + "/v1/chat/completions"); !strings.Contains(strings.Join(c.Argv, " "), `"max_tokens":256`) {
		t.Errorf("the test call is not capped: %q", c.Argv)
	}
	if n := r.count("timeout", "timeout --foreground -k 10 1200 docker compose up -d"); n != 2 || r.count("docker", "compose up") != n {
		t.Errorf("%d bounded docker compose up, want 2, all of the %d", n, r.count("docker", "compose up"))
	}

	t.Run("never ready", func(t *testing.T) {
		r := newRun(t, []string{"SRC=v", "FAKE_READY=never"})
		code, out := r.exec(acme(freePort(t))...)
		if code != 1 || !strings.Contains(out, "not ready within 120 s; the last 30 log lines:\na log line") || strings.Contains(out, "remote again") ||
			r.count("curl", "/health/ready") != 120 || r.count("docker", "compose logs --tail 30 stargaite") != 1 || len(r.cli()) != 0 {
			t.Errorf("exit %d, %d readiness checks:\n%s", code, r.count("curl", "/health/ready"), out)
		}
	})
	t.Run("up outlives its bound", func(t *testing.T) {
		r := newRun(t, []string{"SRC=v", "FAKE_UP=hang", "FAKE_TIMEOUT=1"})
		start := time.Now()
		code, out := r.exec(acme(freePort(t))...)
		if took := time.Since(start); code != 1 || took > 15*time.Second || !strings.Contains(out, "docker compose up failed or was interrupted") ||
			r.count("curl", "/health/ready") != 0 || len(r.cli()) != 0 {
			t.Errorf("exit %d after %s:\n%s", code, took, out)
		}
	})
}

// TestSetupInterrupt: Ctrl-C at the terminal while docker compose up builds
// stops the run at once with 130, though the build would last 30 s.
func TestSetupInterrupt(t *testing.T) {
	r := newRun(t, []string{"FAKE_UP=hang"})
	start := time.Now()
	code, out := r.pty([]string{"install", "--port", freePort(t), "--provider", "ollama", "--model", "m"}, step{"fake build: 30 s", "\x03"})
	if took := time.Since(start); code != 130 || took > 10*time.Second || len(r.cli()) != 0 {
		t.Errorf("exit %d after %s, CLI calls %q:\n%s", code, took, r.cli(), out)
	}
}

// TestSetupRoot: root in a clone of another user is refused before anything
// runs, and status --check before its git fetch, with no container running;
// control: root in its own clone goes on.
func TestSetupRoot(t *testing.T) {
	port := freePort(t)
	r := newRun(t, []string{"SRC=v", "FAKE_UID=0", "FAKE_OWNER=1000"})
	code, out := r.exec(acme(port)...)
	if code != 1 || !strings.Contains(out, "run it as the clone's owner") || r.count("docker", "") != 0 {
		t.Errorf("exit %d:\n%s", code, out)
	}
	r = newRun(t, []string{"SRC=v", "FAKE_UID=0", "FAKE_OWNER=0"})
	if code, out := r.exec(acme(port)...); code != 0 {
		t.Errorf("control: exit %d:\n%s", code, out)
	}

	r = newRun(t, []string{"SRC=v", "FAKE_UID=0", "FAKE_OWNER=1000"})
	remote := t.TempDir()
	gitIn(t, remote, "init", "-q", "--bare")
	gitIn(t, r.dir, "init", "-q")
	gitIn(t, r.dir, "add", "setup.sh", "compose.yaml")
	gitIn(t, r.dir, "commit", "-q", "-m", "first")
	gitIn(t, r.dir, "remote", "add", "origin", remote)
	gitIn(t, r.dir, "push", "-q", "-u", "origin", "main")
	code, out = r.exec("status", "--check")
	if code != 1 || !strings.Contains(out, "run it as the clone's owner") || r.count("timeout", "git fetch")+r.count("git", "fetch") != 0 {
		t.Errorf("status --check: exit %d:\n%s", code, out)
	}
	r.env = append(r.env, "FAKE_OWNER=0")
	if code, out := r.exec("status", "--check"); code != 0 || !strings.Contains(out, "upgrade: none, up to date with origin/main") ||
		r.count("timeout", "git fetch") != 1 {
		t.Errorf("status --check, control: exit %d:\n%s", code, out)
	}
}

// gitIn runs git in dir for the test, with an identity and no user or
// system configuration.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid",
		"-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// installed is an installation in a clone of a bare repository, its
// upstream; with newer, the upstream has one commit more, pushed from
// another clone, which is returned.
func installed(t *testing.T, newer bool) (*run, string) {
	t.Helper()
	r := newRun(t, []string{"SRC=v"})
	remote, other := t.TempDir(), t.TempDir()
	gitIn(t, remote, "init", "-q", "--bare")
	gitIn(t, r.dir, "init", "-q")
	gitIn(t, r.dir, "add", "setup.sh", "compose.yaml")
	gitIn(t, r.dir, "commit", "-q", "-m", "first")
	// The commit the fake container runs, as its version names it.
	gitIn(t, r.dir, "tag", "v0.6.0-fake")
	gitIn(t, r.dir, "remote", "add", "origin", remote)
	gitIn(t, r.dir, "push", "-q", "-u", "origin", "main", "v0.6.0-fake")
	if code, out := r.exec(acme(freePort(t))...); code != 0 {
		t.Fatalf("install: exit %d:\n%s", code, out)
	}
	gitIn(t, other, "clone", "-q", remote, ".")
	if newer {
		if err := os.WriteFile(filepath.Join(other, "NEWS"), []byte("news\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitIn(t, other, "add", "NEWS")
		gitIn(t, other, "commit", "-q", "-m", "second")
		gitIn(t, other, "push", "-q")
	}
	return r, other
}

// TestSetupUpgrade: upgrade refuses a detached HEAD, a branch without
// upstream, a dirty and a diverged clone, before anything changes, each with
// the clone repaired as its control; up to date, it exits 0 with no backup.
// It tags the previous image, merges, builds, takes the backup, starts and
// checks the new version, in that order, and prints the way back. Without a
// terminal it needs --yes; at one, from the menu too, it shows at most 20
// commits, never through a pager, before it asks. A build or a backup that
// fails or is never written leaves the old container running; a start or a
// check that fails prints the way back and exits 1. A CLI from before
// backup - needs --legacy-backup, asked at a terminal and quoted in the way
// back. status --check says whether an upgrade is there.
func TestSetupUpgrade(t *testing.T) {
	// changed are the calls of an upgrade that change something.
	changed := func(r *run) int {
		return r.count("docker", "docker tag") + r.count("git", "merge --ff-only") + r.count("docker", "compose build") + r.count("docker", "backup -")
	}
	t.Run("up to date", func(t *testing.T) {
		r, _ := installed(t, false)
		code, out := r.exec("upgrade", "--yes")
		if code != 0 || !strings.Contains(out, "up to date with origin/main; nothing was changed") || changed(r) != 0 {
			t.Errorf("exit %d:\n%s", code, out)
		}
		if code, out := r.exec("status", "--check"); code != 0 || !strings.Contains(out, "upgrade: none, up to date with origin/main") {
			t.Errorf("status --check: exit %d:\n%s", code, out)
		}
	})

	t.Run("upgrade", func(t *testing.T) {
		r, other := installed(t, true)
		if code, out := r.exec("status", "--check"); code != 0 || !strings.Contains(out, "upgrade: 1 new commit(s) on origin/main; ./setup.sh upgrade applies them") {
			t.Errorf("status --check: exit %d:\n%s", code, out)
		}
		want := gitIn(t, other, "describe", "--tags", "--always", "--dirty")
		ups, fetches := r.count("docker", "compose up"), r.count("timeout", "timeout -k 5 20 git fetch --quiet")
		start := len(r.calls())
		code, out := r.exec("upgrade", "--yes")
		if code != 0 || gitIn(t, r.dir, "rev-parse", "HEAD") != gitIn(t, other, "rev-parse", "HEAD") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		// The order: tag, merge, build, backup, up, then the checks.
		var order []string
		for _, c := range r.calls()[start:] {
			line := strings.Join(c.Argv, " ")
			for _, step := range []string{"docker tag sha256:0123abcd stargaite:local-previous", "git merge --ff-only",
				"docker compose build", "/stargaite backup - --json", "docker compose up -d --no-build", "/stargaite version --json", "/stargaite validate --json"} {
				if strings.Contains(line, step) && (len(order) == 0 || order[len(order)-1] != step) {
					order = append(order, step)
				}
			}
		}
		if want := []string{"/stargaite version --json", "/stargaite validate --json", "docker tag sha256:0123abcd stargaite:local-previous",
			"git merge --ff-only", "docker compose build", "/stargaite backup - --json", "docker compose up -d --no-build",
			"/stargaite version --json", "/stargaite validate --json"}; !slices.Equal(order, want) {
			t.Errorf("order:\n%q\nwant:\n%q", order, want)
		}
		if r.count("docker", "compose up") != ups+1 || r.count("timeout", "timeout -k 5 20 git fetch --quiet") != fetches+1 {
			t.Errorf("%d up, %d bounded fetches:\n%s", r.count("docker", "compose up")-ups, r.count("timeout", "git fetch")-fetches, out)
		}
		backups, err := filepath.Glob(filepath.Join(r.dir, "backups", "v0.6.0-fake-*Z.db"))
		if err != nil || len(backups) != 1 {
			t.Fatalf("backups %q, error %v:\n%s", backups, err, out)
		}
		backup := "backups/" + filepath.Base(backups[0])
		if info, err := os.Stat(backups[0]); err != nil || info.Mode().Perm() != 0o600 || r.file(backup) != "SQLite format 3\x00 the store" {
			t.Errorf("the backup: %v, error %v", info, err)
		}
		for _, s := range []string{"1 new commit(s) on origin/main:", "the running image is kept as stargaite:local-previous", "building version " + want,
			"backing up the store to " + backup, `upgraded: {"level":"INFO","msg":"ready","version":"` + want + `"}`,
			"  docker compose run --rm --no-deps -T stargaite restore --yes - < " + backup, "  docker tag stargaite:local-previous stargaite:local"} {
			if !strings.Contains(out, s) {
				t.Errorf("no %q in:\n%s", s, out)
			}
		}
		if strings.Contains(out, "the upgrade failed") {
			t.Errorf("a failure printed:\n%s", out)
		}
		if code, out := r.exec("status", "--check"); code != 0 || !strings.Contains(out, "version: "+want+" running, "+want+" in this clone") ||
			!strings.Contains(out, "upgrade: none, up to date") {
			t.Errorf("status --check after: exit %d:\n%s", code, out)
		}
		if strings.Contains(out, "a migration of the store changed") || strings.Contains(out, "the running version is unknown") {
			t.Errorf("a migration or a dev build named:\n%s", out)
		}
		if code, out := r.exec("upgrade", "--yes"); code != 0 || !strings.Contains(out, "up to date with origin/main; nothing was changed") {
			t.Errorf("upgrade again: exit %d:\n%s", code, out)
		}
	})

	// A commit that touches a migration is named before the question; a dev
	// build running is named, and its backup is named dev.
	t.Run("migration and dev build", func(t *testing.T) {
		r, other := installed(t, false)
		dir := filepath.Join(other, "internal", "store", "migrations")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "0099_x.sql"), []byte("-- x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitIn(t, other, "add", "internal")
		gitIn(t, other, "commit", "-q", "-m", "migration")
		gitIn(t, other, "push", "-q")
		want := gitIn(t, other, "describe", "--tags", "--always", "--dirty")
		r.env = append(r.env, "FAKE_VERSION=dev")
		code, out := r.exec("upgrade", "--yes")
		backups, _ := filepath.Glob(filepath.Join(r.dir, "backups", "dev-*Z.db"))
		if code != 0 || len(backups) != 1 || !strings.Contains(out, "a migration of the store changed: migrations are one-way") ||
			!strings.Contains(out, "the running version is unknown (a dev build); this clone is at v0.6.0-fake") || !strings.Contains(out, "building version "+want) ||
			strings.Contains(out, "is not this clone's") {
			t.Errorf("exit %d, backups %q:\n%s", code, backups, out)
		}
	})

	for _, c := range []struct {
		name         string
		break_, mend func(t *testing.T, r *run)
		want         string
	}{
		{"detached HEAD", func(t *testing.T, r *run) { gitIn(t, r.dir, "switch", "-q", "--detach") },
			func(t *testing.T, r *run) { gitIn(t, r.dir, "switch", "-q", "main") }, "HEAD is detached"},
		{"no upstream", func(t *testing.T, r *run) { gitIn(t, r.dir, "branch", "--unset-upstream") },
			func(t *testing.T, r *run) { gitIn(t, r.dir, "branch", "-q", "--set-upstream-to", "origin/main") }, "branch main has no upstream"},
		{"dirty", func(t *testing.T, r *run) {
			if err := os.WriteFile(filepath.Join(r.dir, "compose.yaml"), []byte("# changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, r *run) { gitIn(t, r.dir, "checkout", "-q", "--", "compose.yaml") }, "the clone has local changes to tracked files"},
		{"diverged", func(t *testing.T, r *run) { gitIn(t, r.dir, "commit", "-q", "--allow-empty", "-m", "local") },
			func(t *testing.T, r *run) { gitIn(t, r.dir, "reset", "-q", "--hard", "HEAD~1") }, "cannot fast-forward"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := installed(t, true)
			c.break_(t, r)
			head := gitIn(t, r.dir, "rev-parse", "HEAD")
			code, out := r.exec("upgrade", "--yes")
			if code != 1 || !strings.Contains(out, c.want) || changed(r) != 0 || gitIn(t, r.dir, "rev-parse", "HEAD") != head {
				t.Errorf("exit %d, %d changes:\n%s", code, changed(r), out)
			}
			c.mend(t, r)
			if code, out := r.exec("upgrade", "--yes"); code != 0 || !strings.Contains(out, "upgraded: ") {
				t.Errorf("control: exit %d:\n%s", code, out)
			}
		})
	}

	t.Run("no --yes without a terminal", func(t *testing.T) {
		r, _ := installed(t, true)
		head := gitIn(t, r.dir, "rev-parse", "HEAD")
		code, out := r.exec("upgrade")
		if code != 2 || !strings.Contains(out, "missing --yes: upgrading") || changed(r) != 0 || gitIn(t, r.dir, "rev-parse", "HEAD") != head {
			t.Errorf("exit %d, %d changes:\n%s", code, changed(r), out)
		}
	})

	t.Run("at a terminal", func(t *testing.T) {
		// Chosen from the menu, with more commits than are shown: at most
		// 20, written out rather than through a pager, then the question;
		// declined, nothing changes.
		r, other := installed(t, true)
		for i := range 25 {
			gitIn(t, other, "commit", "-q", "--allow-empty", "-m", fmt.Sprintf("change %02d", i+1))
		}
		gitIn(t, other, "push", "-q")
		head := gitIn(t, r.dir, "rev-parse", "HEAD")
		code, out := r.pty(nil, step{"what to do: a number or a name: ", "8"}, step{"continue? [y/N]: ", "n"})
		if code != 1 || !strings.Contains(out, "26 new commit(s) on origin/main:") || !strings.Contains(out, "stopped; nothing more was changed") ||
			strings.Contains(out, "pager") || strings.Contains(out, "change 05") || changed(r) != 0 || gitIn(t, r.dir, "rev-parse", "HEAD") != head {
			t.Errorf("exit %d, %d changes:\n%s", code, changed(r), out)
		}
		var missing []int
		for i := 6; i <= 25; i++ {
			if !strings.Contains(out, fmt.Sprintf("change %02d\r\n", i)) {
				missing = append(missing, i)
			}
		}
		if len(missing) > 0 {
			t.Errorf("commits %v not shown:\n%s", missing, out)
		}
	})

	t.Run("pre-phase CLI at a terminal", func(t *testing.T) {
		r, _ := installed(t, true)
		r.env = append(r.env, "FAKE_UPGRADE=legacy")
		if err := os.MkdirAll(filepath.Join(r.dir, "my backups"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.dir, "my backups", "pre.db"), []byte("by hand"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out := r.pty([]string{"upgrade"}, step{"such as backups/pre-upgrade.db: ", "my backups/pre.db"}, step{"continue? [y/N]: ", "y"})
		// The way back holds the path quoted, as it is run.
		if code != 0 || r.count("docker", "backup -") != 0 || !strings.Contains(out, `restore --yes - < my\ backups/pre.db`+"\r\n") {
			t.Errorf("exit %d:\n%s", code, out)
		}
	})

	t.Run("pre-phase CLI", func(t *testing.T) {
		r, _ := installed(t, true)
		r.env = append(r.env, "FAKE_UPGRADE=legacy")
		code, out := r.exec("upgrade", "--yes")
		if code != 2 || !strings.Contains(out, "missing --legacy-backup") || !strings.Contains(out, "  docker compose exec -T stargaite /stargaite backup /data/pre-upgrade.db") || changed(r) != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if err := os.MkdirAll(filepath.Join(r.dir, "backups"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.dir, "backups", "pre.db"), []byte("by hand"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out = r.exec("upgrade", "--yes", "--legacy-backup", "backups/pre.db")
		if code != 0 || r.count("docker", "backup -") != 0 || !strings.Contains(out, "restore --yes - < backups/pre.db") {
			t.Errorf("--legacy-backup: exit %d:\n%s", code, out)
		}
	})

	for _, c := range []struct {
		name, fake, want string
		backup, up       int // backups taken, docker compose up run by the upgrade
	}{
		{"build fails", "build-fails", "the build failed or took over 20 minutes: v0.6.0-fake still runs, the store unchanged; git reset --hard ", 0, 0},
		{"backup fails", "backup-short", "the backup failed", 0, 0},
		{"backup never written", "", "the backup failed (mkdir: cannot create directory", 0, 0},
		{"start fails", "start-fails", "the upgrade failed", 1, 1},
		{"check fails", "invalid", "validate exits 1, and 0 before: the new version refuses part of the stored configuration", 1, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := installed(t, true)
			r.env = append(r.env, "FAKE_UPGRADE="+c.fake)
			if c.fake == "" {
				// backups is a file, so the directory is never made, the
				// CLI never run, and no count is reported.
				if err := os.WriteFile(filepath.Join(r.dir, "backups"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ups := r.count("docker", "compose up")
			code, out := r.exec("upgrade", "--yes")
			files, _ := filepath.Glob(filepath.Join(r.dir, "backups", "*.db"))
			if code != 1 || !strings.Contains(out, c.want) || len(files) != c.backup || r.count("docker", "compose up")-ups != c.up {
				t.Errorf("exit %d, backups %q, %d up:\n%s", code, files, r.count("docker", "compose up")-ups, out)
			}
			if wayBack := strings.Contains(out, "restore --yes - < backups/"); wayBack != (c.backup == 1) {
				t.Errorf("the way back printed: %v:\n%s", wayBack, out)
			}
			if c.fake != "build-fails" && c.fake != "backup-short" {
				return
			}
			// The clone moved and the old version still runs: run again, it
			// is named, not up to date; reset as told, the upgrade goes on.
			code, out = r.exec("upgrade", "--yes")
			if code != 1 || !strings.Contains(out, "up to date with origin/main, but v0.6.0-fake runs, not this clone's commit: an upgrade did not finish; git reset --hard v0.6.0-fake") {
				t.Errorf("again: exit %d:\n%s", code, out)
			}
			gitIn(t, r.dir, "reset", "-q", "--hard", "v0.6.0-fake")
			r.env = r.env[:len(r.env)-1]
			if code, out := r.exec("upgrade", "--yes"); code != 0 || !strings.Contains(out, "upgraded: ") {
				t.Errorf("after the reset: exit %d:\n%s", code, out)
			}
		})
	}
}
