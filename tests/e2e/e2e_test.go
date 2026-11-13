// Package e2e runs the built binary as a user does: the quick start of
// README.md line by line, then a restart, against the mock provider. It dials
// loopback only, resolves no name, binds no fixed port and leaves no process
// behind.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/tests/mockprovider"
)

const (
	// The values the quick start writes, which the test replaces with its own.
	mockDefault = "http://127.0.0.1:4010"
	tokenShown  = "sk-stargaite-..."
	dataDefault = "http://127.0.0.1:4000"
)

var (
	tokenLine  = regexp.MustCompile(`(?m)^sk-stargaite-\S+$`)
	curlURL    = regexp.MustCompile(regexp.QuoteMeta(dataDefault) + `(/\S*)`)
	curlHeader = regexp.MustCompile(`-H "([^"]+)"`)
	curlData   = regexp.MustCompile(`-d '([^']*)'`)
	traceID    = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// TestQuickStart runs the quick start of README.md with the built binary,
// then checks what it left: the first answer of the chat completion with its
// PAR-9 headers and the one call the provider got (AC-04), /v1/models with the
// new key, a second serve on the same directory refused (AC-42), and the same
// export and the same key after a restart (RES-9). The guide's commands use
// the default data directory and addresses; here the environment points them
// at a temporary directory and at ports the system chose.
func TestQuickStart(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "stargaite")
	dir := filepath.Join(t.TempDir(), "data")
	env := []string{"STARGAITE_DATA_DIR=" + dir, "STARGAITE_LISTEN=127.0.0.1:0", "STARGAITE_ADMIN_LISTEN=127.0.0.1:0"}
	var (
		mock     *mockprovider.Server
		mockURL  string
		srv      *server
		token    string
		answered bool
	)
	cli := func(args ...string) string {
		t.Helper()
		code, stdout, stderr := run(t, "", slices.Concat(env, []string{"STARGAITE_ADMIN_URL=http://" + srv.admin}), bin, args...)
		if code != 0 {
			t.Fatalf("stargaite %q: exit %d; stdout %q stderr %q", args, code, stdout, stderr)
		}
		return stdout
	}

	for _, line := range quickStart(t) {
		if mockURL != "" {
			line = strings.ReplaceAll(line, mockDefault, mockURL)
		}
		line = strings.ReplaceAll(line, tokenShown, token)
		fields := strings.Fields(line)
		switch {
		case line == "go build -o stargaite ./cmd/stargaite":
			if code, _, stderr := run(t, "../..", nil, "go", "build", "-o", bin, "./cmd/stargaite"); code != 0 {
				t.Fatalf("%s: exit %d: %s", line, code, stderr)
			}
		case line == "go run ./tests/mockprovider/cmd/mockprovider &":
			mock = mockprovider.New()
			s := httptest.NewServer(mock)
			t.Cleanup(s.Close)
			mockURL = s.URL
		case fields[0] == "./stargaite" && fields[len(fields)-1] == "&":
			srv = start(t, bin, env, fields[1:len(fields)-1]...)
		case fields[0] == "./stargaite" && srv != nil:
			if m := tokenLine.FindString(cli(fields[1:]...)); m != "" {
				token = m
			}
		case fields[0] == "curl" && srv != nil:
			m := curlURL.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("%s: no URL on %s", line, dataDefault)
			}
			var header []string
			if h := curlHeader.FindStringSubmatch(line); h != nil {
				header = []string{h[1]}
			}
			// curl -d sends a POST with a form content type, which the proxy ignores.
			method, data := http.MethodGet, ""
			if d := curlData.FindStringSubmatch(line); d != nil {
				method, data = http.MethodPost, d[1]
				header = append(header, "Content-Type: application/x-www-form-urlencoded")
			}
			status, h, body := send(t, method, "http://"+srv.data+m[1], data, header...)
			if status != http.StatusOK {
				t.Fatalf("%s: status %d: %s", line, status, body)
			}
			if method == http.MethodPost {
				answered = true
				var completion struct {
					Choices []struct{ Message struct{ Content string } }
				}
				if err := json.Unmarshal(body, &completion); err != nil || len(completion.Choices) != 1 || completion.Choices[0].Message.Content == "" {
					t.Errorf("%s: body %s", line, body)
				}
				if h.Get("X-Stargaite-Request-Id") == "" || !traceID.MatchString(h.Get("X-Stargaite-Trace-Id")) || h.Get("X-Stargaite-Target") != "mock/mock" ||
					h.Values("X-Stargaite-Reasoning-Effort") != nil || h.Values("X-Stargaite-Params-Changed") != nil {
					t.Errorf("%s: headers %v", line, h)
				}
			}
		default:
			t.Fatalf("a quick start line this test does not run: %q", line)
		}
	}
	if srv == nil || mock == nil || token == "" || !answered {
		t.Fatalf("the quick start did not start the proxy and the mock provider, create a key and get an answer")
	}

	models := func() {
		t.Helper()
		status, _, body := send(t, http.MethodGet, "http://"+srv.data+"/v1/models", "", "Authorization: Bearer "+token)
		var list struct {
			Object string
			Data   []struct{ ID string }
		}
		if err := json.Unmarshal(body, &list); err != nil || status != http.StatusOK || list.Object != "list" || len(list.Data) != 1 || list.Data[0].ID != "fast" {
			t.Errorf("/v1/models: status %d, body %s", status, body)
		}
	}
	models()
	// The one call of the quick start, with the upstream model id and no
	// credential: the mock provider declares none.
	if calls := mock.Calls(); len(calls) != 1 || calls[0].Path != "/chat/completions" ||
		string(calls[0].Body) != `{"model":"mock","messages":[{"role":"user","content":"hi"}]}` || calls[0].Header.Values("Authorization") != nil {
		t.Errorf("the mock received %+v", calls)
	}
	before := cli("export")
	if !strings.Contains(before, "my-agent") || strings.Contains(before, token) {
		t.Errorf("export lacks the key or carries its token:\n%s", before)
	}

	// A second serve on the same data directory refuses to start (AC-42).
	if code, _, stderr := run(t, "", env, bin, "serve"); code != 1 || !strings.Contains(stderr, `"msg":"store failed"`) {
		t.Errorf("second serve: exit %d, log %q", code, stderr)
	}

	code, log := srv.stop(t)
	if code != 0 || !strings.Contains(log, `"msg":"stopped"`) {
		t.Fatalf("stop: exit %d, log:\n%s", code, log)
	}
	// OPS-9: the provider call and the request each logged once, with the key and the alias.
	if strings.Count(log, `"msg":"attempt"`) != 1 || !strings.Contains(log, `"key":"my-agent","alias":"fast","attempts":1,"status":200`) {
		t.Errorf("log:\n%s", log)
	}
	srv = start(t, bin, env, "serve")
	models()
	if after := cli("export"); after != before {
		t.Errorf("export after the restart:\n%s\nbefore:\n%s", after, before)
	}
	code, restarted := srv.stop(t)
	if code != 0 {
		t.Errorf("stop after the restart: exit %d", code)
	}

	// Logs are JSON lines and never carry the admin key or a client key.
	adminKey, err := os.ReadFile(filepath.Join(dir, "admin.key"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(log+restarted), "\n") {
		if !json.Valid([]byte(line)) || strings.Contains(line, strings.TrimSpace(string(adminKey))) || strings.Contains(line, token) {
			t.Errorf("log line is not JSON or carries a key: %q", line)
		}
	}
}

// TestListenerOutsideLoopback: without TLS or --behind-tls-proxy, serve exits
// 1 before it opens the store (AC-45), and keeps its output JSON with --json.
func TestListenerOutsideLoopback(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "stargaite")
	if code, _, stderr := run(t, "../..", nil, "go", "build", "-o", bin, "./cmd/stargaite"); code != 0 {
		t.Fatalf("build: exit %d: %s", code, stderr)
	}
	dir := filepath.Join(t.TempDir(), "data")
	code, stdout, stderr := run(t, "", []string{"STARGAITE_DATA_DIR=" + dir}, bin, "serve", "--json", "--listen", "0.0.0.0:0", "--admin-listen", "127.0.0.1:0")
	var line struct{ Msg, Listener, Rule string }
	if code != 1 || stdout != "" || json.Unmarshal([]byte(stderr), &line) != nil || line != (struct{ Msg, Listener, Rule string }{"listener refused", "data", "SEC-6"}) {
		t.Errorf("exit %d, stdout %q, log %q", code, stdout, stderr)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused serve created its data directory: %v", err)
	}
}

// quickStart returns the command lines of the first sh block after the
// "## Quick start" heading of README.md, exactly as written. A line with "#"
// fails: stock interactive zsh does not treat it as a comment.
func quickStart(t *testing.T) []string {
	t.Helper()
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(readme), "\n## Quick start\n")
	_, block, ok2 := strings.Cut(section, "\n```sh\n")
	block, _, ok3 := strings.Cut(block, "\n```")
	if !ok || !ok2 || !ok3 {
		t.Fatal("README.md has no sh block under ## Quick start")
	}
	var lines []string
	for _, line := range strings.Split(block, "\n") {
		if strings.Contains(line, "#") {
			t.Fatalf("a quick start line with #, which zsh passes on as arguments: %q", line)
		}
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// run runs a command to its end, with standard input empty and no terminal,
// within a bounded time. A nil env inherits the test's environment.
func run(t *testing.T, dir string, env []string, name string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	timeout := 30 * time.Second
	if name == "go" {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env, cmd.WaitDelay = dir, env, 5*time.Second
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil && !errors.As(err, new(*exec.ExitError)) {
		t.Fatalf("%s %q: %v", name, args, err)
	}
	return cmd.ProcessState.ExitCode(), out.String(), errOut.String()
}

// send sends one request with the given body and "Name: value" headers.
func send(t *testing.T, method, url, body string, headers ...string) (int, http.Header, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range headers {
		name, value, _ := strings.Cut(h, ": ")
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, b
}

// server is a running serve, killed at the end of the test if still running.
type server struct {
	cmd         *exec.Cmd
	data, admin string // the bound addresses, from the ready line
	exited      chan struct{}
	code        int
	mu          sync.Mutex
	log         strings.Builder
}

// start runs serve and waits for its ready line.
func start(t *testing.T, bin string, env []string, args ...string) *server {
	t.Helper()
	s := &server{cmd: exec.Command(bin, args...), exited: make(chan struct{})}
	logs, w := io.Pipe()
	s.cmd.Env, s.cmd.Stdout, s.cmd.Stderr = env, w, w
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan [2]string, 1)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for lines := bufio.NewScanner(logs); lines.Scan(); {
			s.mu.Lock()
			s.log.WriteString(lines.Text() + "\n")
			s.mu.Unlock()
			var line struct {
				Msg       string
				DataAddr  string `json:"data_addr"`
				AdminAddr string `json:"admin_addr"`
			}
			if json.Unmarshal(lines.Bytes(), &line) == nil && line.Msg == "ready" {
				select {
				case ready <- [2]string{line.DataAddr, line.AdminAddr}:
				default:
				}
			}
		}
	}()
	go func() {
		_ = s.cmd.Wait() // the exit code is read from ProcessState
		_ = w.Close()
		<-drained
		s.code = s.cmd.ProcessState.ExitCode()
		close(s.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-s.exited:
		default:
			_ = s.cmd.Process.Kill()
			<-s.exited
		}
	})

	select {
	case addrs := <-ready:
		s.data, s.admin = addrs[0], addrs[1]
	case <-s.exited:
		t.Fatalf("serve exited %d before it was ready; log:\n%s", s.code, s.logs())
	case <-time.After(30 * time.Second):
		t.Fatalf("serve was not ready within 30 s; log:\n%s", s.logs())
	}
	return s
}

// stop sends SIGTERM and returns the exit code and the whole log.
func (s *server) stop(t *testing.T) (int, string) {
	t.Helper()
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.exited:
	case <-time.After(15 * time.Second):
		t.Fatalf("serve did not stop within 15 s; log:\n%s", s.logs())
	}
	return s.code, s.logs()
}

func (s *server) logs() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.log.String()
}
