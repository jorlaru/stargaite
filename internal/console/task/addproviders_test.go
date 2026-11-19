package task

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/admintest"
	"github.com/jorlaru/stargaite/internal/cli"
	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/rules"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
)

// The recreate's words, as every agree ends.
// writtenNote is said before a recreate that follows a write.
const writtenNote = "the files are written: if the recreate stops before the container is recreated, " +
	"docker compose up -d --no-build in this clone does it"

const recreateTail = ", and the container is then recreated to read it: requests in flight are given time to finish, and the rate-limit windows restart"

// modelList is a provider on 127.0.0.1: it answers its model list, or its
// /key, with 200 for the key it accepts and refuse (401 unless set) for any
// other, and records the Authorization and the path of each such request.
// With stall set, it calls stall instead and answers nothing until the
// request ends. It also answers readiness.
type modelList struct {
	srv    *httptest.Server
	accept string
	refuse int
	stall  func()
	mu     sync.Mutex
	auth   []string
	paths  []string
}

func serveModels(t *testing.T, accept string) *modelList {
	t.Helper()
	m := &modelList{accept: accept, refuse: http.StatusUnauthorized}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health/ready":
			return
		case "/v1/models", "/v1/key":
			m.mu.Lock()
			m.auth = append(m.auth, r.Header.Get("Authorization"))
			m.paths = append(m.paths, r.URL.Path)
			stall := m.stall
			m.mu.Unlock()
			switch {
			case stall != nil:
				stall()
				<-r.Context().Done()
			case r.Header.Get("Authorization") != "Bearer "+m.accept:
				w.WriteHeader(m.refuse)
			case r.URL.Path == "/v1/key":
				_, _ = io.WriteString(w, `{"data":{"label":"x"}}`)
			default:
				_, _ = io.WriteString(w, `{"data":[{"id":"m1"}]}`)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *modelList) port() string { u, _ := url.Parse(m.srv.URL); return u.Port() }

func (m *modelList) requests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.auth)
}

func (m *modelList) requestPaths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.paths)
}

// world is Add providers' fixture: the admin API in process through the real
// CLI, every argv recorded; a clone whose docker fake answers compose ps,
// up and logs, logging each call; and a model list whose port is the data
// plane's in .env, so it answers readiness too.
type world struct {
	t      *testing.T
	h      *host.Host
	env    map[string]string
	runner proxy.Runner
	log    string
	models *modelList
	mu     sync.Mutex
	argvs  []string
	docs   []string // the standard input of each apply
}

func newWorld(t *testing.T) *world {
	t.Helper()
	return worldOn(t, admintest.Server(t))
}

// worldOn is newWorld with the CLI pointed by env.
func worldOn(t *testing.T, env map[string]string) *world {
	t.Helper()
	bin := t.TempDir()
	w := &world{t: t, h: host.New(t.TempDir()), env: env, log: filepath.Join(bin, "log")}
	writeFile(t, filepath.Join(bin, "docker"), "#!/bin/sh\necho \"$*\" >>'"+w.log+"'\ncase \"$*\" in\n"+
		"'compose ps -q') echo c0ffee ;;\n'compose up -d --no-build') ;;\n'compose logs --tail 30 stargaite') echo 'a log line' ;;\n"+
		"*) exit 9 ;;\nesac\n", 0o755)
	writeFile(t, filepath.Join(bin, "sleep"), "#!/bin/sh\necho \"sleep $*\" >>'"+w.log+"'\n", 0o755)
	t.Setenv("PATH", bin)
	w.models = serveModels(t, "good")
	writeFile(t, filepath.Join(w.h.Dir, ".env"), "STARGAITE_PORT="+w.models.port()+"\n", 0o600)
	w.runner = cliRunner(w.env)
	return w
}

// canned makes the world's CLI answer from answers instead.
func (w *world) canned(answers map[string]reply) {
	var calls []string
	w.runner = cannedRunner(w.t, answers, &calls)
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// add runs Add providers with answers from the flags, environ the console's
// environment, and the plain front; it returns the result, the output and
// the error.
func (w *world) add(answers map[string]string, environ ...string) (string, string, error) {
	w.t.Helper()
	var out bytes.Buffer
	res, err := w.addWith(ui.New(plain.New(&out), answers).WithEnv(environ))
	return res, out.String(), err
}

func (w *world) addWith(u *ui.UI) (string, error) {
	return w.addCtx(context.Background(), u)
}

// addCtx is addWith under ctx.
func (w *world) addCtx(ctx context.Context, u *ui.UI) (string, error) {
	return u.Run(ctx, addSteps(), func(ctx context.Context) (string, error) {
		return AddProviders(ctx, u, w.proxy(), w.h)
	})
}

// proxy is the world's CLI, every argv recorded, and the document of each
// apply (a verify's standard input is a pipe left open, so only apply's).
func (w *world) proxy() *proxy.Proxy {
	return proxy.New(func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		w.mu.Lock()
		w.argvs = append(w.argvs, strings.Join(args, " "))
		w.mu.Unlock()
		if len(args) > 0 && args[0] == "apply" && stdin != nil {
			doc, _ := io.ReadAll(stdin)
			w.mu.Lock()
			w.docs = append(w.docs, string(doc))
			w.mu.Unlock()
			stdin = bytes.NewReader(doc)
		}
		return w.runner(ctx, args, stdin, stdout, stderr)
	})
}

// applied is the document of each apply the task sent.
func (w *world) applied() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.docs)
}

// cli is every argv the task gave the CLI.
func (w *world) cli() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.argvs)
}

func (w *world) dockerLog() string { b, _ := os.ReadFile(w.log); return string(b) }

func (w *world) ups() int { return strings.Count(w.dockerLog(), "compose up -d --no-build\n") }

// file is a file of the clone, "" when missing.
func (w *world) file(name string) string {
	b, _ := os.ReadFile(filepath.Join(w.h.Dir, name))
	return string(b)
}

// files is every file of the clone and its content.
func (w *world) files() map[string]string {
	w.t.Helper()
	all := map[string]string{}
	err := filepath.WalkDir(w.h.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		all[path] = string(b)
		return err
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return all
}

// listed adds a provider through the CLI before the run, on host.docker.internal
// at port, as the task stores a service on the host's loopback.
func (w *world) listed(name, variable, port string) {
	w.t.Helper()
	args := []string{"providers", "add", "--base-url", "http://host.docker.internal:" + port + "/v1", "--allow-private-network"}
	if variable != "" {
		args = append(args, "--protocol", "openai", "--api-key-env", variable, "--billing", "pay_per_use")
	}
	var out, errOut bytes.Buffer
	if code := cli.Main(append(args, "--", name), func(k string) string { return w.env[k] }, strings.NewReader(""), &out, &errOut); code != 0 {
		w.t.Fatalf("providers add %s: exit %d: %s%s", name, code, out.String(), errOut.String())
	}
}

// leaks lists where secret was found: the CLI's argvs, the docker log, the
// output, or a file of the clone other than .env.
func (w *world) leaks(secret, output string) []string {
	var where []string
	for _, a := range w.cli() {
		if strings.Contains(a, secret) {
			where = append(where, "a CLI argv")
		}
	}
	if strings.Contains(w.dockerLog(), secret) {
		where = append(where, "the docker log")
	}
	if strings.Contains(output, secret) {
		where = append(where, "the output")
	}
	for path, content := range w.files() {
		if filepath.Base(path) != ".env" && strings.Contains(content, secret) {
			where = append(where, path)
		}
	}
	return where
}

// before reports whether line is said, and before next when next is not "".
func before(out, line, next string) bool {
	lines := strings.Split(out, "\n")
	i, j := slices.Index(lines, line), slices.Index(lines, next)
	return i >= 0 && (next == "" || (j > i))
}

// acmeAnswers adds acme, a compatible service on the world's model list,
// with its key in SRC.
func acmeAnswers(w *world) map[string]string {
	return map[string]string{"provider": "acme", "protocol": "openai", "base-url": "http://127.0.0.1:" + w.models.port() + "/v1",
		"api-key-env": "ANTHROPIC_API_KEY", "key-env": "SRC", "yes": "yes"}
}

const secret = `s3cr3t $HOME #x y\z`

func TestAddProvidersFlags(t *testing.T) {
	w := newWorld(t)
	w.models.accept = secret
	res, out, err := w.add(acmeAnswers(w), "SRC="+secret)
	if err != nil || res != "Done: acme added, key checked." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if !before(out, "writing ANTHROPIC_API_KEY to .env"+recreateTail, "recreating the container") {
		t.Errorf("the write's line is not said before the recreate:\n%s", out)
	}
	if where := w.leaks(secret, out); len(where) > 0 {
		t.Errorf("the key leaked to %q", where)
	}
	if env := w.file(".env"); !strings.Contains(env, "\nANTHROPIC_API_KEY='"+secret+"'\n") {
		t.Errorf(".env = %q", env)
	}
	if info, err := os.Stat(filepath.Join(w.h.Dir, ".env")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf(".env: %v, %v", info, err)
	}
	if got := w.models.requests(); len(got) != 1 || got[0] != "Bearer "+secret {
		t.Errorf("model list requests: %q", got)
	}
	if w.ups() != 1 {
		t.Errorf("docker log:\n%s", w.dockerLog())
	}
	p := w.models.port()
	want := []string{"providers presets --json", "providers list --json",
		"providers add --json --base-url http://host.docker.internal:" + p + "/v1 --api-key-env ANTHROPIC_API_KEY --protocol openai --billing pay_per_use --allow-private-network -- acme",
		"providers show --json -- acme"}
	if got := w.cli(); !slices.Equal(got, want) {
		t.Errorf("CLI calls:\n%q\nwant:\n%q", got, want)
	}
	if !strings.Contains(w.file("compose.override.yaml"), "# setup: host_gateway=1\n") {
		t.Errorf("override: %q", w.file("compose.override.yaml"))
	}
}

func TestAddProvidersLeakControl(t *testing.T) {
	w := newWorld(t)
	if _, _, err := w.h.Capture(context.Background(), "sleep", "0", secret); err != nil {
		t.Fatal(err)
	}
	if where := w.leaks(secret, ""); !slices.Equal(where, []string{"the docker log"}) {
		t.Fatalf("leaks = %q, want the docker log", where)
	}
}

func TestAddProvidersOllama(t *testing.T) {
	w := newWorld(t)
	res, out, err := w.add(map[string]string{"provider": "ollama", "yes": "yes"})
	if err != nil || res != "Done: ollama added, no key." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	want := []string{"providers presets --json", "providers list --json",
		"providers add --json --base-url http://host.docker.internal:11434/v1 --allow-private-network -- ollama", "providers show --json -- ollama"}
	if got := w.cli(); !slices.Equal(got, want) {
		t.Errorf("CLI calls: %q", got)
	}
	if w.ups() != 1 || !strings.Contains(w.file("compose.override.yaml"), `"host.docker.internal:host-gateway"`) || len(w.models.requests()) != 0 {
		t.Errorf("ups %d, override %q, requests %q", w.ups(), w.file("compose.override.yaml"), w.models.requests())
	}
}

func TestAddProvidersNoYes(t *testing.T) {
	w := newWorld(t)
	files := w.files()
	_, out, err := w.add(map[string]string{"provider": "ollama"})
	var usage *ui.UsageError
	if !errors.As(err, &usage) || !strings.HasPrefix(err.Error(), "missing --yes: writing compose.override.yaml with host_gateway=1"+recreateTail) {
		t.Fatalf("err = %v", err)
	}
	if got := w.cli(); len(got) != 3 || !strings.HasPrefix(got[2], "providers add ") {
		t.Errorf("CLI calls: %q", got)
	}
	if w.ups() != 0 || !maps.Equal(files, w.files()) {
		t.Errorf("ups %d, files %q", w.ups(), w.files())
	}
	if !before(out, "writing compose.override.yaml with host_gateway=1"+recreateTail, "") {
		t.Errorf("the change was not said:\n%s", out)
	}

	// With a key checked and pending: .env is not written either.
	w = newWorld(t)
	files = w.files()
	a := acmeAnswers(w)
	delete(a, "yes")
	_, out, err = w.add(a, "SRC=good")
	if !errors.As(err, &usage) || !strings.HasPrefix(err.Error(), "missing --yes: writing ANTHROPIC_API_KEY to .env"+recreateTail) {
		t.Fatalf("with a key: err = %v\n%s", err, out)
	}
	if len(w.models.requests()) != 1 || w.ups() != 0 || !maps.Equal(files, w.files()) {
		t.Errorf("with a key: requests %q, ups %d, files %q", w.models.requests(), w.ups(), w.files())
	}
}

func TestAddProvidersReaderOverride(t *testing.T) {
	mine := "services:\n  stargaite:\n    environment:\n      MINE: \"1\"\n"
	w := newWorld(t)
	writeFile(t, filepath.Join(w.h.Dir, "compose.override.yaml"), mine, 0o600)
	_, out, err := w.add(map[string]string{"provider": "ollama", "yes": "yes"})
	want := `compose.override.yaml was not written by setup.sh, so it is left as it is: add "host.docker.internal:host-gateway" ` +
		`to the extra_hosts of its stargaite service yourself, or move it away and run this again`
	if err == nil || err.Error() != want || w.file("compose.override.yaml") != mine || w.ups() != 0 {
		t.Fatalf("err = %v, override %q\n%s", err, w.file("compose.override.yaml"), out)
	}

	// With a key checked and pending: .env is not written either.
	w = newWorld(t)
	writeFile(t, filepath.Join(w.h.Dir, "compose.override.yaml"), mine, 0o600)
	files := w.files()
	_, out, err = w.add(acmeAnswers(w), "SRC=good")
	if err == nil || err.Error() != want || !maps.Equal(files, w.files()) || w.ups() != 0 || len(w.models.requests()) != 1 {
		t.Fatalf("with a key: err = %v, files %q\n%s", err, w.files(), out)
	}

	theirs := mine + "    extra_hosts:\n      - host.docker.internal:host-gateway\n"
	w = newWorld(t)
	writeFile(t, filepath.Join(w.h.Dir, "compose.override.yaml"), theirs, 0o600)
	res, out, err := w.add(map[string]string{"provider": "ollama", "yes": "yes"})
	if err != nil || res != "Done: ollama added, no key." || w.file("compose.override.yaml") != theirs {
		t.Fatalf("%q, %v, override %q\n%s", res, err, w.file("compose.override.yaml"), out)
	}
}

func TestAddProvidersMarkedOverride(t *testing.T) {
	w := newWorld(t)
	// Readiness runs under TLS by the override's name: a server whose
	// certificate names example.com, trusted through tls/cert.pem.
	ready := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(ready.Close)
	u, _ := url.Parse(ready.URL)
	writeFile(t, filepath.Join(w.h.Dir, ".env"), "STARGAITE_PORT="+u.Port()+"\n", 0o600)
	writeFile(t, filepath.Join(w.h.Dir, "tls", "cert.pem"), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ready.Certificate().Raw})), 0o600)
	marker := "# Written by setup.sh from its \"# setup:\" lines below; it rewrites this file whole.\n"
	setup := "# setup: tls=self-signed\n# setup: tls_name=example.com\n# setup: tls_ca=cert.pem\n# setup: addresses=127.0.0.1\n"
	tls := "    command: [\"serve\", \"--listen\", \"0.0.0.0:4000\", \"--tls-cert\", \"/tls/cert.pem\", \"--tls-key\", \"/tls/key.pem\"]\n" +
		"    ports: !override\n      - \"127.0.0.1:${STARGAITE_PORT:-4000}:4000\"\n    volumes:\n      - ./tls:/tls:ro\n"
	writeFile(t, filepath.Join(w.h.Dir, "compose.override.yaml"),
		marker+setup+"services:\n  stargaite:\n    extra_hosts:\n      - \"example.com:127.0.0.1\"\n"+tls, 0o600)
	res, out, err := w.add(map[string]string{"provider": "ollama", "yes": "yes"})
	if err != nil || res != "Done: ollama added, no key." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	want := marker + setup + "# setup: host_gateway=1\nservices:\n  stargaite:\n    extra_hosts:\n      - \"host.docker.internal:host-gateway\"\n" +
		"      - \"example.com:127.0.0.1\"\n" + tls
	if got := w.file("compose.override.yaml"); got != want {
		t.Errorf("override:\n%s\nwant:\n%s", got, want)
	}
	if !before(out, `compose.override.yaml is rewritten with host_gateway=1, keeping its other "# setup:" lines`+recreateTail, "recreating the container") {
		t.Errorf("the rewrite was not said:\n%s", out)
	}
	if !slices.Contains(w.cli(), "providers add --json --base-url http://host.docker.internal:11434/v1 --allow-private-network -- ollama") {
		t.Errorf("CLI calls: %q", w.cli())
	}
}

func TestAddProvidersRefusedKeyFlags(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		w := newWorld(t)
		w.models.refuse = status
		res, out, err := w.add(acmeAnswers(w), "SRC=bad")
		var usage *ui.UsageError
		if err == nil || err.Error() != "acme refused the key 1 time(s); nothing was written for it" || errors.As(err, &usage) ||
			!strings.Contains(out, "\nacme refused the key ("+strconv.Itoa(status)+")\n") {
			t.Fatalf("%d: %q, %v\n%s", status, res, err, out)
		}
		if len(w.models.requests()) != 1 || strings.Contains(w.file(".env"), "ANTHROPIC_API_KEY") || w.ups() != 0 {
			t.Errorf("%d: requests %q, .env %q, docker log:\n%s", status, w.models.requests(), w.file(".env"), w.dockerLog())
		}
	}
}

// TestAddProvidersCheckEndsWithContext: a run whose context ends during the
// check (a signal) stops with the context's error: the key is not said "not
// checked", and nothing is written.
func TestAddProvidersCheckEndsWithContext(t *testing.T) {
	w := newWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.models.stall = cancel
	files := w.files()
	var out bytes.Buffer
	_, err := w.addCtx(ctx, ui.New(plain.New(&out), acmeAnswers(w)).WithEnv([]string{"SRC=good"}))
	if !errors.Is(err, context.Canceled) || strings.Contains(out.String(), "key not checked") {
		t.Fatalf("err = %v\n%s", err, out.String())
	}
	if !maps.Equal(files, w.files()) || w.ups() != 0 {
		t.Errorf("ups %d, files %q", w.ups(), w.files())
	}
}

// TestAddProvidersNo: a No at "continue?" stops the run, and every file is
// left as it was.
func TestAddProvidersNo(t *testing.T) {
	w := newWorld(t)
	w.listed("acme", "ANTHROPIC_API_KEY", w.models.port())
	files := w.files()
	s := &scripted{answers: map[string][]string{"provider": {"acme"}, "key-env": {"good"}, "yes": {"no"}}}
	_, err := w.addWith(ui.New(s, nil))
	var usage *ui.UsageError
	if err == nil || err.Error() != "stopped; nothing more was changed" || errors.As(err, &usage) {
		t.Fatalf("err = %v\n%s", err, s.output())
	}
	if !maps.Equal(files, w.files()) || w.ups() != 0 {
		t.Errorf("ups %d, files %q", w.ups(), w.files())
	}
}

// TestAddProvidersMarkedOverrideAsked: the rewrite of a marked override is
// asked even once the reader has agreed to a change, and a No to it writes
// nothing, the key included.
func TestAddProvidersMarkedOverrideAsked(t *testing.T) {
	w := newWorld(t)
	w.listed("acme", "ANTHROPIC_API_KEY", w.models.port())
	writeFile(t, filepath.Join(w.h.Dir, "compose.override.yaml"),
		"# Written by setup.sh from its \"# setup:\" lines below; it rewrites this file whole.\n# setup: addresses=127.0.0.1\nservices: {}\n", 0o600)
	files := w.files()
	s := &scripted{answers: map[string][]string{"provider": {"acme"}, "key-env": {"good"}, "yes": {"yes", "no"}}}
	_, err := w.addWith(ui.New(s, nil))
	if err == nil || err.Error() != "stopped; nothing more was changed" {
		t.Fatalf("err = %v\n%s", err, s.output())
	}
	if !maps.Equal(files, w.files()) || w.ups() != 0 {
		t.Errorf("ups %d, files %q", w.ups(), w.files())
	}
}

// TestAddProvidersUpFails: when the files were written, the run says how to
// apply them before it recreates, since the next run finds them kept; and
// nothing after the recreate claims how far it got.
func TestAddProvidersUpFails(t *testing.T) {
	w := newWorld(t)
	writeFile(t, filepath.Join(filepath.Dir(w.log), "docker"), "#!/bin/sh\necho \"$*\" >>'"+w.log+"'\ncase \"$*\" in\n"+
		"'compose ps -q') echo c0ffee ;;\n'compose up -d --no-build') exit 1 ;;\n*) exit 9 ;;\nesac\n", 0o755)
	_, out, err := w.add(acmeAnswers(w), "SRC=good")
	if err == nil || err.Error() != "docker compose up failed or was interrupted; its output is above" {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !before(out, "recreating the container", writtenNote) || strings.Contains(out, "was not recreated") {
		t.Errorf("output:\n%s", out)
	}
}

// leave, as a scripted answer, is Esc.
const leave = "<esc>"

// scripted is a front that answers each question by its ID, from a list in
// order, and records the prompts asked. A work question waits for its work,
// or answers Esc once leaveWork, polled every 10 ms, says so.
type scripted struct {
	mu        sync.Mutex
	answers   map[string][]string
	asked     []string
	questions []ui.Question
	out       strings.Builder
	leaveWork func(ui.Question) bool
	workErr   error // what leaving the work answers: ui.ErrLeave when nil
}

func (s *scripted) Ask(ctx context.Context, q ui.Question) (string, error) {
	if q.Kind == ui.KindWork {
		return s.work(ctx, q)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, q.Prompt)
	s.questions = append(s.questions, q)
	list := s.answers[q.ID]
	if len(list) == 0 {
		return "", errors.New("no answer for " + q.ID)
	}
	s.answers[q.ID] = list[1:]
	if list[0] == leave {
		return "", ui.ErrLeave
	}
	return list[0], nil
}

// work waits for the work to end, or for leaveWork to leave it.
func (s *scripted) work(ctx context.Context, q ui.Question) (string, error) {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-tick.C:
			if s.leaveWork != nil && s.leaveWork(q) {
				if s.workErr != nil {
					return "", s.workErr
				}
				return "", ui.ErrLeave
			}
		}
	}
}

func (s *scripted) Show(_ context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out.WriteString(text + "\n")
	return nil
}

func (s *scripted) output() string { s.mu.Lock(); defer s.mu.Unlock(); return s.out.String() }

func TestAddProvidersRefusedKeyFront(t *testing.T) {
	w := newWorld(t)
	w.listed("acme", "ANTHROPIC_API_KEY", w.models.port())
	s := &scripted{answers: map[string][]string{"provider": {"acme"}, "key-env": {"bad", "good"}, "yes": {"yes"}}}
	res, err := w.addWith(ui.New(s, nil))
	out := s.output()
	if err != nil || res != "Done: acme kept, key checked." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if strings.Count(out, "acme refused the key (401)\n") != 1 || !before(out, "acme refused the key (401)", "acme: key checked") ||
		len(w.models.requests()) != 2 {
		t.Errorf("requests %q:\n%s", w.models.requests(), out)
	}

	w = newWorld(t)
	w.listed("acme", "ANTHROPIC_API_KEY", w.models.port())
	env := w.file(".env")
	s = &scripted{answers: map[string][]string{"provider": {"acme"}, "key-env": {"bad", "bad", "bad"}}}
	_, err = w.addWith(ui.New(s, nil))
	if err == nil || err.Error() != "acme refused the key 3 time(s); nothing was written for it" {
		t.Fatalf("err = %v", err)
	}
	if strings.Count(s.output(), "acme refused the key (401)\n") != 3 || len(w.models.requests()) != 3 || w.file(".env") != env {
		t.Errorf("requests %q, .env %q:\n%s", w.models.requests(), w.file(".env"), s.output())
	}
}

func TestAddProvidersCheckFails(t *testing.T) {
	w := newWorld(t)
	closed := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(closed.URL)
	closed.Close()
	answers := acmeAnswers(w)
	answers["base-url"] = "http://127.0.0.1:" + u.Port() + "/v1"
	res, out, err := w.add(answers, "SRC=good")
	if err != nil || res != "Done: acme added, key not checked." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if !strings.Contains(out, "\nacme: key not checked: the model list at http://localhost:"+u.Port()+"/v1/models was not read") {
		t.Errorf("output:\n%s", out)
	}
	if !strings.Contains(w.file(".env"), "\nANTHROPIC_API_KEY='good'\n") {
		t.Errorf(".env = %q", w.file(".env"))
	}
}

func TestAddProvidersRunAgainKeepsAll(t *testing.T) {
	w := newWorld(t)
	if _, out, err := w.add(acmeAnswers(w), "SRC=good"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	files, requests, ups := w.files(), len(w.models.requests()), w.ups()
	res, out, err := w.add(acmeAnswers(w), "SRC=good")
	if err != nil || res != "Done: acme kept, key kept in .env." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if !before(out, "provider acme exists; kept", "ANTHROPIC_API_KEY is in .env; kept") {
		t.Errorf("output:\n%s", out)
	}
	if len(w.models.requests()) != requests || w.ups() != ups || !maps.Equal(files, w.files()) {
		t.Errorf("the second run changed something: requests %q, docker log:\n%s", w.models.requests(), w.dockerLog())
	}
}

func TestAddProvidersSharedVariable(t *testing.T) {
	w := newWorld(t)
	w.listed("acme", "ANTHROPIC_API_KEY", w.models.port())
	w.listed("acme2", "ANTHROPIC_API_KEY", w.models.port())
	res, out, err := w.add(map[string]string{"provider": "acme,acme2", "key-env": "SRC", "yes": "yes"}, "SRC=good")
	if err != nil || res != "Done: acme kept, key checked; acme2 kept, key kept in .env." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if len(w.models.requests()) != 1 || w.ups() != 1 || !strings.Contains(out, "\nANTHROPIC_API_KEY is asked once, for acme: acme2 uses the same key\n") {
		t.Errorf("requests %q, ups %d:\n%s", w.models.requests(), w.ups(), out)
	}
	if strings.Count(w.file(".env"), "ANTHROPIC_API_KEY=") != 1 {
		t.Errorf(".env = %q", w.file(".env"))
	}
}

// twoKeys is a world with acme and acme3 listed, on two model lists that
// accept k-one and k-two. acme3's variable is OPENROUTER_API_KEY, since the
// admin API in process resolves only the variables of config.example.yaml.
func twoKeys(t *testing.T) (*world, *modelList) {
	w := newWorld(t)
	w.models.accept = "k-one"
	second := serveModels(t, "k-two")
	w.listed("acme", "ANTHROPIC_API_KEY", w.models.port())
	w.listed("acme3", "OPENROUTER_API_KEY", second.port())
	return w, second
}

func TestAddProvidersTwoKeys(t *testing.T) {
	w, second := twoKeys(t)
	res, out, err := w.add(map[string]string{"provider": "acme,acme3", "key-env": "acme=SRC1,acme3=SRC2", "yes": "yes"}, "SRC1=k-one", "SRC2=k-two")
	if err != nil || res != "Done: acme kept, key checked; acme3 kept, key checked." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if !slices.Equal(w.models.requests(), []string{"Bearer k-one"}) || !slices.Equal(second.requests(), []string{"Bearer k-two"}) {
		t.Errorf("requests %q and %q", w.models.requests(), second.requests())
	}
	env := w.file(".env")
	if !strings.Contains(env, "\nANTHROPIC_API_KEY='k-one'\n") || !strings.Contains(env, "\nOPENROUTER_API_KEY='k-two'\n") || w.ups() != 1 {
		t.Errorf(".env %q, ups %d", env, w.ups())
	}
}

// TestAddProvidersReplaceEvery: --replace answers every provider of the run.
func TestAddProvidersReplaceEvery(t *testing.T) {
	w, _ := twoKeys(t)
	writeFile(t, filepath.Join(w.h.Dir, ".env"), w.file(".env")+"ANTHROPIC_API_KEY='old1'\nOPENROUTER_API_KEY='old2'\n", 0o600)
	res, out, err := w.add(map[string]string{"provider": "acme,acme3", "replace": "yes", "key-env": "acme=SRC1,acme3=SRC2", "yes": "yes"},
		"SRC1=k-one", "SRC2=k-two")
	if err != nil || res != "Done: acme kept, key checked; acme3 kept, key checked." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if env := w.file(".env"); !strings.Contains(env, "\nANTHROPIC_API_KEY='k-one'\nOPENROUTER_API_KEY='k-two'\n") || strings.Contains(env, "old") {
		t.Errorf(".env = %q", env)
	}
}

// TestAddProvidersBareKeyEnvTwoVariables: a bare --key-env for two variables
// is a usage error, and the first provider's checked key is not written:
// no file of the clone changed.
func TestAddProvidersBareKeyEnvTwoVariables(t *testing.T) {
	w, second := twoKeys(t)
	files := w.files()
	_, out, err := w.add(map[string]string{"provider": "acme,acme3", "key-env": "SRC", "yes": "yes"}, "SRC=k-one")
	var usage *ui.UsageError
	if !errors.As(err, &usage) || err.Error() != "--key-env: several providers need a key each: give acme=VAR,acme3=VAR" {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if len(w.models.requests()) > 1 || len(second.requests()) != 0 || w.ups() != 0 || !maps.Equal(files, w.files()) {
		t.Errorf("requests %q and %q, ups %d, files %q", w.models.requests(), second.requests(), w.ups(), w.files())
	}
}

func TestAddProvidersSeveralWithOneProviderFlag(t *testing.T) {
	for flag, value := range map[string]string{"base-url": "http://x/v1", "protocol": "openai", "api-key-env": "X", "allow-private-network": "yes"} {
		w, second := twoKeys(t)
		_, out, err := w.add(map[string]string{"provider": "acme,acme3", flag: value, "key-env": "acme=SRC1,acme3=SRC2"}, "SRC1=k-one", "SRC2=k-two")
		var usage *ui.UsageError
		if !errors.As(err, &usage) || err.Error() != "--"+flag+" answers one provider: add acme, acme3 one at a time" {
			t.Fatalf("%s: err = %v\n%s", flag, err, out)
		}
		if got := w.cli(); len(got) != 2 || len(w.models.requests())+len(second.requests()) != 0 {
			t.Errorf("%s: CLI calls %q, requests %q %q", flag, got, w.models.requests(), second.requests())
		}
	}
}

func TestAddProvidersReplace(t *testing.T) {
	w := newWorld(t)
	w.listed("acme", "ANTHROPIC_API_KEY", w.models.port())
	writeFile(t, filepath.Join(w.h.Dir, ".env"), w.file(".env")+"ANTHROPIC_API_KEY='old'\n", 0o600)
	res, out, err := w.add(map[string]string{"provider": "acme", "replace": "yes", "key-env": "SRC", "yes": "yes"}, "SRC=good")
	if err != nil || res != "Done: acme kept, key checked." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if env := w.file(".env"); !strings.Contains(env, "\nANTHROPIC_API_KEY='good'\n") || strings.Contains(env, "old") {
		t.Errorf(".env = %q", env)
	}
	if len(w.models.requests()) != 1 || w.ups() != 1 {
		t.Errorf("requests %q, ups %d", w.models.requests(), w.ups())
	}
}

// canned answers for a run on a canned CLI: the presets, an empty list and
// a show; each test adds its own.
func cannedAdd(list, show string) map[string]reply {
	return map[string]reply{
		"providers presets --json":      {`{"presets":[{"name":"ollama","protocol":"openai","base_url":"http://localhost:11434/v1"}]}`, 0},
		"providers list --json":         {`{"providers":[` + list + `]}`, 0},
		"providers show --json -- acme": {show, 0},
	}
}

func TestAddProvidersAllowPrivate(t *testing.T) {
	for _, row := range []struct {
		url   string
		allow bool
	}{{"http://models.internal.test/v1", true}, {"http://models.internal.test/v1", false}, {"https://models.internal.test/v1", true}} {
		add := "providers add --json --base-url " + row.url + " --protocol openai --billing none"
		resource := `{"findings":[],"resource":{"name":"acme","credential":"none","protocol":"openai","base_url":"` + row.url + `"}}`
		w := newWorld(t)
		answers := map[string]string{"provider": "acme", "base-url": row.url, "api-key-env": "none", "yes": "yes"}
		argv := add + " -- acme"
		if row.allow {
			answers["allow-private-network"] = "yes"
			argv = add + " --allow-private-network -- acme"
		}
		canned := cannedAdd("", `{"name":"acme","credential":"none"}`)
		canned[argv] = reply{resource, 0}
		w.canned(canned)
		res, out, err := w.add(answers)
		if err != nil || res != "Done: acme added, no key." || !slices.Contains(w.cli(), argv) || len(w.models.requests()) != 0 {
			t.Fatalf("%s allow %v: %q, %v, CLI %q\n%s", row.url, row.allow, res, err, w.cli(), out)
		}
	}
}

// acmeListed is acme in the list, with its credential's state.
func acmeListed(credential string) string {
	return `{"name":"acme","credential":"` + credential + `","protocol":"openai","base_url":"https://api.acme.test/v1","api_key_env":"ANTHROPIC_API_KEY"}`
}

func TestAddProvidersStillUnresolved(t *testing.T) {
	w := newWorld(t)
	writeFile(t, filepath.Join(w.h.Dir, ".env"), w.file(".env")+"ANTHROPIC_API_KEY='x'\n", 0o600)
	w.canned(cannedAdd(acmeListed("resolved"), `{"name":"acme","credential":"unresolved"}`))
	_, out, err := w.add(map[string]string{"provider": "acme"})
	var usage *ui.UsageError
	if err == nil || err.Error() != "the credential of acme does not resolve: ANTHROPIC_API_KEY is not in the container's environment" || errors.As(err, &usage) {
		t.Fatalf("err = %v\n%s", err, out)
	}
}

// addAcme is the canned add of acme on api.acme.test with its findings.
const addAcme = "providers add --json --base-url https://api.acme.test/v1 --api-key-env ANTHROPIC_API_KEY --protocol openai --billing pay_per_use -- acme"

func addAcmeAnswers() map[string]string {
	return map[string]string{"provider": "acme", "base-url": "https://api.acme.test/v1", "api-key-env": "ANTHROPIC_API_KEY", "yes": "yes"}
}

func TestAddProvidersRecreatesUnresolved(t *testing.T) {
	recreate := "the container is recreated to read ANTHROPIC_API_KEY: requests in flight are given time to finish, and the rate-limit windows restart"
	resolved := `{"name":"acme","credential":"resolved"}`

	w := newWorld(t)
	writeFile(t, filepath.Join(w.h.Dir, ".env"), w.file(".env")+"ANTHROPIC_API_KEY='x'\n", 0o600)
	w.canned(cannedAdd(acmeListed("unresolved"), resolved))
	res, out, err := w.add(map[string]string{"provider": "acme", "yes": "yes"})
	if err != nil || res != "Done: acme kept, key kept in .env." || !before(out, recreate, "recreating the container") || w.ups() != 1 {
		t.Fatalf("listed: %q, %v, ups %d\n%s", res, err, w.ups(), out)
	}

	w = newWorld(t)
	writeFile(t, filepath.Join(w.h.Dir, ".env"), w.file(".env")+"ANTHROPIC_API_KEY='x'\n", 0o600)
	canned := cannedAdd("", resolved)
	canned[addAcme] = reply{`{"findings":[],"resource":` + acmeListed("unresolved") + `}`, 0}
	w.canned(canned)
	res, out, err = w.add(addAcmeAnswers())
	if err != nil || res != "Done: acme added, key kept in .env." || !before(out, recreate, "recreating the container") || w.ups() != 1 {
		t.Fatalf("added: %q, %v, ups %d\n%s", res, err, w.ups(), out)
	}
}

func TestAddProvidersHeldWarnings(t *testing.T) {
	w := newWorld(t)
	writeFile(t, filepath.Join(w.h.Dir, ".env"), w.file(".env")+"ANTHROPIC_API_KEY='x'\n", 0o600)
	canned := cannedAdd("", `{"name":"acme","credential":"resolved"}`)
	canned[addAcme] = reply{`{"findings":[` +
		`{"severity":"warning","code":"credential_unresolved","resource":"providers/acme","message":"credential message"},` +
		`{"severity":"warning","code":"url_unresolved","resource":"providers/acme","message":"url message"},` +
		`{"severity":"warning","code":"unpriced","resource":"targets/x","message":"unpriced message"}],` +
		`"resource":` + acmeListed("unresolved") + `}`, 0}
	w.canned(canned)
	res, out, err := w.add(addAcmeAnswers())
	if err != nil || res != "Done: acme added, key kept in .env." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if !before(out, rules.Plain("url_unresolved", "providers/acme", "url message"), "") ||
		strings.Contains(out, "credential message") || strings.Contains(out, "its key is not in the container yet") || strings.Contains(out, "unpriced") {
		t.Errorf("output:\n%s", out)
	}
}

// TestAddProvidersCheckPathAccepts: the name does not choose where a key is
// checked: openrouter on another host is checked at its model list.
func TestAddProvidersCheckPathAccepts(t *testing.T) {
	w := newWorld(t)
	w.listed("openrouter", "", w.models.port())
	res, out, err := w.add(map[string]string{"provider": "openrouter", "key-env": "SRC", "yes": "yes"}, "SRC=good")
	if err != nil || res != "Done: openrouter kept, key checked." || !strings.Contains(out, "\nopenrouter: key checked\n") {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if got := w.models.requestPaths(); !slices.Equal(got, []string{"/v1/models"}) {
		t.Errorf("request paths %q", got)
	}
}

// TestFreeCheck: the free check is the provider's, with the key, at /key
// for OpenRouter's host whatever the provider's name, at the model list
// otherwise.
func TestFreeCheck(t *testing.T) {
	for _, row := range []struct{ name, url, path string }{
		{"openrouter", "https://openrouter.ai/api/v1", "/key"},
		{"or2", "https://OpenRouter.ai/api/v1/", "/key"},
		{"openrouter", "https://api.openai.com/v1", ""},
		{"openai", "https://api.openai.com/v1", ""},
		{"evil", "https://openrouter.ai.evil.test/v1", ""},
	} {
		p := proxy.Provider{Name: row.name, Protocol: "openai", BaseURL: row.url, AllowPrivateNetwork: true}
		want := host.Provider{Protocol: "openai", BaseURL: row.url, Key: "k", Path: row.path, AllowPrivate: true}
		if got := freeCheck(p, "k"); got != want {
			t.Errorf("%s at %s: %+v, want %+v", row.name, row.url, got, want)
		}
	}
}

func TestAddProvidersSeveralNeedPresets(t *testing.T) {
	w := newWorld(t)
	_, out, err := w.add(map[string]string{"provider": "ollama,newthing"})
	var usage *ui.UsageError
	if !errors.As(err, &usage) || err.Error() != "--provider: several providers at once are presets or providers already added; add newthing on its own" {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if got := w.cli(); len(got) != 2 {
		t.Errorf("CLI calls: %q", got)
	}

	// A provider named twice is added once, and is one provider: a flag that
	// answers one provider is not refused.
	w = newWorld(t)
	res, out, err := w.add(map[string]string{"provider": "ollama,ollama", "protocol": "openai", "yes": "yes"})
	if err != nil || res != "Done: ollama added, no key." {
		t.Fatalf("twice: %q, %v\n%s", res, err, out)
	}
	want := []string{"providers presets --json", "providers list --json",
		"providers add --json --base-url http://host.docker.internal:11434/v1 --allow-private-network -- ollama", "providers show --json -- ollama"}
	if got := w.cli(); !slices.Equal(got, want) {
		t.Errorf("twice: CLI calls %q", got)
	}

	// At the front, a pick may mix presets with one new service.
	w = newWorld(t)
	s := &scripted{answers: map[string][]string{"provider": {"ollama,+another"}, "provider-name": {leave}}}
	if _, err := w.addWith(ui.New(s, nil)); !errors.Is(err, ui.ErrLeave) {
		t.Fatalf("err = %v", err)
	}
	if !slices.Equal(s.asked, []string{"Which providers do you want to add?", "a name for the service"}) {
		t.Errorf("asked %q", s.asked)
	}

	// The name typed for the new service may be another pick: it is added once.
	w = newWorld(t)
	s = &scripted{answers: map[string][]string{"provider": {"ollama,+another"}, "provider-name": {"ollama"}, "yes": {"yes"}}}
	if res, err := w.addWith(ui.New(s, nil)); err != nil || res != "Done: ollama added, no key." {
		t.Fatalf("typed again: %q, %v\n%s", res, err, s.output())
	}
	adds := 0
	for _, c := range w.cli() {
		if strings.HasPrefix(c, "providers add ") {
			adds++
		}
	}
	if adds != 1 {
		t.Errorf("typed again: CLI calls %q", w.cli())
	}
}

func TestAddProvidersNoContainer(t *testing.T) {
	w := newWorld(t)
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "docker"), "#!/bin/sh\n[ \"$*\" = \"compose ps -q\" ] || exit 9\n", 0o755)
	t.Setenv("PATH", bin)
	_, out, err := w.add(map[string]string{"provider": "ollama", "yes": "yes"})
	if err == nil || err.Error() != "no container runs here: ./setup.sh install starts it" || len(w.cli()) != 0 {
		t.Fatalf("err = %v, CLI %q\n%s", err, w.cli(), out)
	}
}

func TestAddProvidersEscLeaves(t *testing.T) {
	w := newWorld(t)
	w.listed("acme", "ANTHROPIC_API_KEY", w.models.port())
	env := w.file(".env")
	s := &scripted{answers: map[string][]string{"provider": {"acme"}, "key-env": {leave}}}
	if _, err := w.addWith(ui.New(s, nil)); !errors.Is(err, ui.ErrLeave) {
		t.Fatalf("err = %v", err)
	}
	if w.file(".env") != env || len(w.models.requests()) != 0 {
		t.Errorf(".env %q, requests %q", w.file(".env"), w.models.requests())
	}
}

// TestAddProvidersEscDuringCheck: Esc while the free check waits for a
// model list that stalls leaves at once, and nothing is written.
func TestAddProvidersEscDuringCheck(t *testing.T) {
	w := newWorld(t)
	w.listed("acme", "ANTHROPIC_API_KEY", w.models.port())
	w.models.stall = func() {}
	files := w.files()
	s := &scripted{answers: map[string][]string{"provider": {"acme"}, "key-env": {"good"}},
		leaveWork: func(q ui.Question) bool { return strings.HasPrefix(q.Prompt, "checking the key of acme") }}
	done := make(chan error, 1)
	go func() { _, err := w.addWith(ui.New(s, nil)); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ui.ErrLeave) {
			t.Fatalf("err = %v\n%s", err, s.output())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Esc did not stop the free check within 2 s")
	}
	if !maps.Equal(files, w.files()) || w.ups() != 0 {
		t.Errorf("ups %d, files %q", w.ups(), w.files())
	}
}

func TestKeyCheckPath(t *testing.T) {
	if keyCheckPath("https://openrouter.ai/api/v1") != "/key" || keyCheckPath("https://api.openai.com/v1") != "" {
		t.Fatal(keyCheckPath("https://openrouter.ai/api/v1"), keyCheckPath("https://api.openai.com/v1"))
	}
}

func TestVariableOf(t *testing.T) {
	if got := variableOf("my-llm.v2"); got != "MY_LLM_V2_API_KEY" {
		t.Fatal(got)
	}
}
