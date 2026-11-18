// Package setup runs setup.sh (OPS-11) against shims, as the stubbed layer of
// docs/design/phase-6.md section 7: setup.sh is copied into a temporary
// directory and run with a PATH that holds only shims, links to the command
// of ./shim, which record every external command and answer for docker and
// curl. jq is the real one make tools puts in bin/; the runs at a terminal go
// through script(1). Linux only; make setup-test runs it end to end.
package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
// external command is recorded. docker, curl, id, stat and sleep answer as
// fakes; the rest are recording pass-throughs to the real command.
var shimNames = []string{"docker", "curl", "jq", "git", "id", "stat", "mktemp", "mv", "rm", "timeout", "sleep", "stty"}

// shimBin is the shim, built once for every run.
var shimBin string

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
		shimBin = filepath.Join(dir, "shim")
		if out, err := exec.Command("go", "build", "-o", shimBin, "./shim").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "the shim was not built: %v\n%s", err, out)
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
		"SETUP_STATE=" + filepath.Join(tmp, "state"), "SETUP_REAL_PATH=" + r.realPath}, env...)
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
		if len(c.Argv) > 6 && c.Argv[0] == "docker" && c.Argv[2] == "exec" {
			list = append(list, strings.Join(slices.DeleteFunc(slices.Clone(c.Argv[6:]), func(a string) bool { return a == "--json" }), " "))
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

	// --key-env names an exported variable only, never one of the script's.
	t.Run("key-env not exported", func(t *testing.T) {
		r := newRun(t, nil)
		code, out := r.exec(acme(freePort(t), "--key-env", "from")...)
		if code != 2 || !strings.Contains(out, "exported to this script") || strings.Contains(r.file(".env"), "ACME_API_KEY") {
			t.Errorf("exit %d:\n%s", code, out)
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

	r = newRun(t, nil)
	marked := "# Written by setup.sh from its \"# setup:\" lines below; it rewrites this file whole.\n# setup: tls=self-signed\nservices:\n  stargaite:\n"
	if err := os.WriteFile(filepath.Join(r.dir, "compose.override.yaml"), []byte(marked), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := r.exec(local...); code != 0 {
		t.Fatalf("control: exit %d:\n%s", code, out)
	}
	want := "# Written by setup.sh from its \"# setup:\" lines below; it rewrites this file whole.\n# setup: tls=self-signed\n# setup: host_gateway=1\n" +
		"services:\n  stargaite:\n    extra_hosts:\n      - \"host.docker.internal:host-gateway\"\n"
	if got := r.file("compose.override.yaml"); got != want {
		t.Errorf("override:\n%s\nwant:\n%s", got, want)
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
		if code != 1 || !strings.Contains(out, "not ready within 120 s; the last 30 log lines:\na log line") ||
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
// runs; control: root in its own clone goes on.
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
}
