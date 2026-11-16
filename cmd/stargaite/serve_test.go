package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/auth"
	"github.com/jorlaru/stargaite/internal/cli"
	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/store"
	"github.com/jorlaru/stargaite/internal/usage"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// adminKey is an admin key given through STARGAITE_ADMIN_KEY.
const (
	adminKey         = "test-admin-key-of-at-least-32-characters"
	testMetricsToken = "test-metrics-token-of-40-characters-long"
)

// TestMain runs the command in place of the tests when STARGAITE_TEST_RUN is
// set, so a test can start serve as a process of its own and kill it.
func TestMain(m *testing.M) {
	if os.Getenv("STARGAITE_TEST_RUN") != "" {
		os.Exit(run(os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

type readyLine struct {
	Msg, Version string
	DataDir      string `json:"data_dir"`
	DataAddr     string `json:"data_addr"`
	AdminAddr    string `json:"admin_addr"`
	TLS          bool
	Revision     int64
	before       string // the log lines before the ready line
}

// start runs serve as the process does and returns its ready line. stop
// sends SIGTERM and returns the exit code and the log lines after the ready
// line. serve listens for the signal before it logs that line.
func start(t *testing.T, getenv func(string) string, args ...string) (ready readyLine, stop func() (int, string)) {
	t.Helper()
	logs, stderr := io.Pipe()
	exit := make(chan int, 1)
	go func() {
		exit <- serve(args, getenv, io.Discard, stderr)
		_ = stderr.Close()
	}()

	lines := bufio.NewScanner(logs)
	var before strings.Builder
	for ready.Msg != "ready" {
		if !lines.Scan() {
			t.Fatalf("no ready line; log %q", &before)
		}
		var line struct{ Level, Msg string }
		if err := json.Unmarshal(lines.Bytes(), &line); err != nil || line.Level == "ERROR" {
			t.Fatalf("log line %q: %+v, error %v", lines.Text(), line, err)
		}
		if line.Msg != "ready" {
			before.WriteString(lines.Text() + "\n")
		} else if err := json.Unmarshal(lines.Bytes(), &ready); err != nil {
			t.Fatal(err)
		}
	}
	ready.before = before.String()
	rest := make(chan string, 1)
	go func() {
		var b strings.Builder
		for lines.Scan() {
			b.WriteString(lines.Text() + "\n")
		}
		rest <- b.String()
	}()
	return ready, func() (int, string) {
		t.Helper()
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case code := <-exit:
			return code, <-rest
		case <-time.After(10 * time.Second):
			t.Fatal("serve did not stop")
			return 0, ""
		}
	}
}

func get(t *testing.T, client *http.Client, url, header string) (int, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Padding", header)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// TestServe starts serve with its addresses from a flag and from the
// environment, probes both listeners, checks that a second serve on the same
// data directory refuses to start (AC-42), and stops it with a signal.
func TestServe(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	ready, stop := start(t, env(map[string]string{
		"STARGAITE_DATA_DIR":     dir,
		"STARGAITE_ADMIN_LISTEN": "127.0.0.1:0",
		"STARGAITE_LISTEN":       "0.0.0.0:4000", // the flag wins, or the rule would refuse it
	}), "--listen", "127.0.0.1:0")
	if ready.Version != cli.Version || ready.DataDir != dir || ready.TLS {
		t.Errorf("ready line %+v", ready)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	data := "http://" + ready.DataAddr
	for _, tc := range []struct {
		url, header string
		status      int
	}{
		{data + "/health/live", "", http.StatusOK},
		{data + "/health/ready", "", http.StatusOK},
		{data + "/health/ready", strings.Repeat("x", 128<<10), http.StatusRequestHeaderFieldsTooLarge},
		{"http://" + ready.AdminAddr + "/admin/v1/settings", "", http.StatusUnauthorized},
	} {
		if code, err := get(t, client, tc.url, tc.header); err != nil || code != tc.status {
			t.Errorf("GET %s: status %d, error %v; want %d", tc.url, code, err, tc.status)
		}
	}
	for _, name := range []string{"lock", "stargaite.db", "admin.key"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("data directory: %v", err)
		}
	}
	// The CLI finds the generated key in the data directory (SEC-3).
	var stdout bytes.Buffer
	if code := cli.Main([]string{"settings", "show", "--json", "--admin-url", "http://" + ready.AdminAddr}, env(map[string]string{"STARGAITE_DATA_DIR": dir}), nil, &stdout, io.Discard); code != 0 || !json.Valid(stdout.Bytes()) {
		t.Errorf("settings show: exit %d, stdout %q", code, &stdout)
	}

	var stderr bytes.Buffer
	if code := serve([]string{"--data-dir", dir, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0"}, env(nil), io.Discard, &stderr); code != 1 {
		t.Errorf("second serve on the same directory: exit %d", code)
	}
	var line struct{ Level, Msg, Error string }
	if err := json.Unmarshal(stderr.Bytes(), &line); err != nil || line.Msg != "store failed" || !strings.Contains(line.Error, "locked by another process") {
		t.Errorf("second serve log %q: %+v, error %v", &stderr, line, err)
	}

	code, rest := stop()
	if code != 0 || !strings.Contains(rest, `"msg":"stopped"`) {
		t.Errorf("exit %d after SIGTERM; log %q", code, rest)
	}
	if _, err := get(t, client, data+"/health/live", ""); err == nil {
		t.Error("the data listener still answers after the stop")
	}
}

// TestServeLoadsTheConfiguration: serve publishes the stored configuration
// before it is ready, under the stored revision, resolves credentials from
// --env-file and logs the findings, never a credential value.
func TestServeLoadsTheConfiguration(t *testing.T) {
	const planted = "sk-PLANTED-credential"
	dir := t.TempDir()
	st, err := store.Open(context.Background(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Open(context.Background(), st, func() config.Env { return config.Env{Lookup: func(string) (string, bool) { return "", false }} }, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Commit(context.Background(), config.Change{Summary: "apply", Mutate: func(s *config.State) (map[string]string, error) {
		for _, name := range []string{"p", "q"} {
			// A literal address: a change resolves host names, and no test does.
			s.Doc.Providers[name] = domain.Provider{Protocol: domain.ProtocolOpenAI, BaseURL: "https://93.184.216.34/v1", Billing: domain.BillingNone, APIKeyEnv: strings.ToUpper(name) + "_KEY"}
		}
		return nil, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(envFile, []byte("P_KEY="+planted+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The admin key from the environment: no file is created, so the
	// finding is the only line before the ready line.
	ready, stop := start(t, env(map[string]string{"STARGAITE_ENV_FILE": envFile, "STARGAITE_ADMIN_KEY": adminKey}), "--data-dir", dir, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0")
	var finding struct{ Level, Msg, Severity, Rule, Code, Resource, Field string }
	if err := json.Unmarshal([]byte(ready.before), &finding); err != nil || ready.Revision != 1 || finding != (struct{ Level, Msg, Severity, Rule, Code, Resource, Field string }{
		"WARN", "configuration finding", "warning", "OPS-6", "credential_unresolved", "providers/q", "api_key_env",
	}) {
		t.Errorf("ready line %+v; finding %+v, error %v", ready, finding, err)
	}
	code, rest := stop()
	if code != 0 || strings.Contains(ready.before+rest, "PLANTED") || strings.Contains(ready.before+rest, adminKey) {
		t.Errorf("exit %d; log %q %q", code, ready.before, rest)
	}
	if _, err := os.Stat(filepath.Join(dir, "admin.key")); !os.IsNotExist(err) {
		t.Errorf("admin.key was written although STARGAITE_ADMIN_KEY is set: %v", err)
	}

	// A row this binary cannot decode stops the start, and the lock is released.
	if st, err = store.Open(context.Background(), dir, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE providers SET spec = '{"colour":"blue"}' WHERE name = 'q'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := serve([]string{"--data-dir", dir, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0"}, env(nil), io.Discard, &stderr); code != 1 {
		t.Errorf("undecodable configuration: exit %d", code)
	}
	var line struct{ Level, Msg, Error string }
	if err := json.Unmarshal(stderr.Bytes(), &line); err != nil || line.Msg != "configuration failed" || !strings.Contains(line.Error, `provider q: unknown member "/colour"`) {
		t.Errorf("log %q: %+v, error %v", &stderr, line, err)
	}
	if st, err = store.Open(context.Background(), dir, slog.New(slog.DiscardHandler)); err != nil {
		t.Errorf("the lock was not released: %v", err)
	} else {
		_ = st.Close()
	}
}

// TestServeDeadlines (phase-2 design section 10, AC-22): a synchronous chat
// completion and a stream that last longer than both servers' read and write
// timeouts are not cut, because the data plane clears its read deadline once
// the body is read and sets its write deadline per provider call, which a
// stream moves per event (SEC-6). The configuration is stored before serve
// starts, so no admin call races the lowered timeouts.
func TestServeDeadlines(t *testing.T) {
	const reply = `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"late"},"finish_reason":"stop"}]}`
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(600 * time.Millisecond):
			_, _ = io.WriteString(w, reply)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)
	mock := mockprovider.New()
	mock.Script(mockprovider.Step{Chunks: [][]byte{[]byte("data: {\"choices\":[]}\n\n"), []byte("data: {\"choices\":[]}\n\n"),
		[]byte("data: {\"choices\":[]}\n\n"), []byte("data: [DONE]\n\n")}, Gap: 200 * time.Millisecond})
	sse := httptest.NewServer(mock)
	t.Cleanup(sse.Close)
	dir, token := seed(t, map[string]string{"fast": slow.URL, "stream": sse.URL})

	serverReadTimeout, serverWriteTimeout = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { serverReadTimeout, serverWriteTimeout = 60*time.Second, 60*time.Second })
	ready, stop := start(t, env(map[string]string{"STARGAITE_ADMIN_KEY": adminKey}), "--data-dir", dir, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0")
	for alias, want := range map[string]string{"fast": reply, "stream": "data: {\"choices\":[]}\n\ndata: {\"choices\":[]}\n\ndata: {\"choices\":[]}\n\ndata: [DONE]\n\n"} {
		req, err := http.NewRequest(http.MethodPost, "http://"+ready.DataAddr+"/v1/chat/completions",
			strings.NewReader(`{"model":"`+alias+`","messages":[{"role":"user","content":"hi"}],"stream":`+map[string]string{"fast": "false", "stream": "true"}[alias]+`}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		began := time.Now()
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("%s: the call was cut after %v: %v", alias, time.Since(began), err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || string(body) != want || time.Since(began) < 600*time.Millisecond {
			t.Errorf("%s: status %d after %v: %q, error %v", alias, resp.StatusCode, time.Since(began), body, err)
		}
	}
	if code, log := stop(); code != 0 || strings.Count(log, `"msg":"attempt"`) != 2 || strings.Count(log, `"msg":"request"`) != 2 {
		t.Errorf("exit %d; log %s", code, log)
	}
}

// seed stores, in a new data directory, an alias per entry of providers,
// each with its own openai provider at that base URL and its target
// <alias>/m, and the key agent that reaches them all; it returns the
// directory and the key's token. serve then starts with it, so no admin call
// races the test.
func seed(t *testing.T, providers map[string]string) (dir, token string) {
	t.Helper()
	dir = t.TempDir()
	st, err := store.Open(context.Background(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Open(context.Background(), st, func() config.Env { return config.Env{Lookup: func(string) (string, bool) { return "", false }} }, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	token, id, hash, err := auth.Issue(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Commit(context.Background(), config.Change{Summary: "apply", Mutate: func(s *config.State) (map[string]string, error) {
		key := domain.Key{}
		for alias, url := range providers {
			s.Doc.Providers[alias] = domain.Provider{Protocol: domain.ProtocolOpenAI, BaseURL: url, Billing: domain.BillingNone, AllowPrivateNetwork: true}
			s.Doc.Targets[alias+"/m"] = domain.Target{Kind: domain.KindChat}
			s.Doc.Models[alias] = domain.Model{Targets: []string{alias + "/m"}}
			key.Models = append(key.Models, alias)
		}
		s.Doc.Keys["agent"] = key
		s.Secrets = append(s.Secrets, config.KeySecret{PublicID: id, KeyName: "agent", Hash: hash})
		return nil, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, token
}

// TestServeDrain is RES-6 and AC-23 (drain) through serve, with the grace
// lowered: on SIGTERM the listeners stop accepting at once; a stream that
// ends within the grace gets its whole answer; when the grace ends, a
// synchronous call and a stream still open are stopped, within 1 s at the
// mock, and get 500 internal_error "server stopping" as an envelope and as
// the stream's terminal event; so does a request whose body is still
// arriving. A stream whose client stopped reading ends 1 s later, as client
// gone (499). Every request runs finish and done before the store closes,
// that last one included, and serve exits 0 with the stop line. Every
// request line has its usage records in the store, one per attempt, read
// after the stop: the writer flushed them before the store closed (AC-23).
func TestServeDrain(t *testing.T) {
	shutdownGrace = 1500 * time.Millisecond
	t.Cleanup(func() { shutdownGrace = 30 * time.Second })
	chunk := []byte("data: {\"choices\":[]}\n\n")
	big := []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + strings.Repeat("x", 16<<10) + "\"}}]}\n\n")
	mocks := map[string]*mockprovider.Server{"held": mockprovider.New(), "open": mockprovider.New(), "ends": mockprovider.New(), "stall": mockprovider.New()}
	mocks["held"].Script(mockprovider.Step{Delay: time.Minute})
	mocks["open"].Script(mockprovider.Step{Chunks: [][]byte{chunk}, Hang: true})
	mocks["ends"].Script(mockprovider.Step{Chunks: [][]byte{chunk, chunk, chunk, []byte("data: [DONE]\n\n")}, Gap: 150 * time.Millisecond})
	// 32 MiB, more than the socket buffers of a client that reads nothing hold.
	mocks["stall"].Script(mockprovider.Step{Chunks: slices.Repeat([][]byte{big}, 2000), Hang: true})
	urls := map[string]string{}
	for alias, m := range mocks {
		srv := httptest.NewServer(m)
		t.Cleanup(srv.Close)
		urls[alias] = srv.URL
	}
	dir, token := seed(t, urls)
	ready, stop := start(t, env(map[string]string{"STARGAITE_ADMIN_KEY": adminKey}), "--data-dir", dir, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0")

	// Raw clients: one authenticated whose body is still arriving, and a
	// stream client with a small receive buffer that reads only the status line.
	raw := func(body string, n int) *bufio.Reader {
		conn, err := net.Dial("tcp", ready.DataAddr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.(*net.TCPConn).SetReadBuffer(4096)
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second)) // a failing run ends sooner than the server ReadTimeout
		if _, err := fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s",
			token, len(body), body[:n]); err != nil {
			t.Fatal(err)
		}
		return bufio.NewReader(conn)
	}
	slow := raw(`{"model":"held","messages":[{"role":"user","content":"hi"}]}`, 10)
	stall := `{"model":"stall","messages":[{"role":"user","content":"hi"}],"stream":true}`
	stalled := raw(stall, len(stall))
	if line, err := stalled.ReadString('\n'); line != "HTTP/1.1 200 OK\r\n" {
		t.Fatalf("the stalled stream: %q %v", line, err)
	}

	type answer struct {
		status int
		body   string
		err    error
	}
	answers := map[string]chan answer{}
	for _, alias := range []string{"held", "open", "ends"} {
		stream := map[bool]string{false: "false", true: "true"}[alias != "held"]
		req, err := http.NewRequest(http.MethodPost, "http://"+ready.DataAddr+"/v1/chat/completions",
			strings.NewReader(`{"model":"`+alias+`","messages":[{"role":"user","content":"hi"}],"stream":`+stream+`}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		started, out := make(chan struct{}), make(chan answer, 1)
		answers[alias] = out
		go func() {
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				close(started)
				out <- answer{err: err}
				return
			}
			defer func() { _ = resp.Body.Close() }()
			r := bufio.NewReader(resp.Body)
			first, err := r.ReadString('\n')
			close(started)
			rest, err2 := io.ReadAll(r)
			out <- answer{resp.StatusCode, first + string(rest), errors.Join(err, err2)}
		}()
		if alias == "held" {
			for deadline := time.Now().Add(2 * time.Second); len(mocks["held"].Calls()) == 0; time.Sleep(5 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the synchronous call did not reach the mock")
				}
			}
		} else {
			<-started
		}
	}

	// The listener refuses new connections while the open stream still waits.
	refused := make(chan string, 1)
	go func() {
		for deadline := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
			conn, err := net.Dial("tcp", ready.DataAddr)
			if err != nil {
				refused <- map[bool]string{true: "the open stream ended before the listener refused"}[len(answers["open"]) > 0]
				return
			}
			_ = conn.Close()
			if time.Now().After(deadline) {
				refused <- "the data listener still accepts after SIGTERM"
				return
			}
		}
	}()
	signalled := time.Now()
	code, log := stop()
	if msg := <-refused; msg != "" {
		t.Error(msg)
	}

	if a := <-answers["ends"]; a.err != nil || a.status != 200 || a.body != strings.Repeat(string(chunk), 3)+"data: [DONE]\n\n" {
		t.Errorf("the stream that ends within the grace: %d %q: %v", a.status, a.body, a.err)
	}
	if a := <-answers["held"]; a.err != nil || a.status != 500 || a.body != `{"error":{"message":"server stopping","type":"api_error","code":"internal_error"}}`+"\n" {
		t.Errorf("the held call: %d %q: %v", a.status, a.body, a.err)
	}
	if a := <-answers["open"]; a.err != nil || a.status != 200 ||
		a.body != string(chunk)+`data: {"error":{"message":"server stopping","type":"api_error","code":"internal_error"}}`+"\n\n" {
		t.Errorf("the open stream: %d %q: %v", a.status, a.body, a.err)
	}
	if resp, err := http.ReadResponse(slow, nil); err != nil {
		t.Errorf("the request still sending its body: %v", err)
	} else if b, err := io.ReadAll(resp.Body); err != nil || resp.StatusCode != 500 ||
		string(b) != `{"error":{"message":"server stopping","type":"api_error","code":"internal_error"}}`+"\n" {
		t.Errorf("the request still sending its body: %d %q: %v", resp.StatusCode, b, err)
	}
	if c := mocks["stall"].Calls(); len(c) != 1 || c[0].Ended.IsZero() || c[0].Ended.Sub(signalled) > shutdownGrace+time.Second {
		t.Errorf("stall: calls %+v", c)
	}
	for alias, cancelled := range map[string]bool{"held": true, "open": true, "ends": false} {
		c := mocks[alias].Calls()
		if len(c) != 1 || c[0].Cancelled != cancelled || c[0].Ended.IsZero() {
			t.Fatalf("%s: calls %+v", alias, c)
		}
		if cancelled && c[0].Ended.Sub(signalled) > shutdownGrace+time.Second {
			t.Errorf("%s: the provider call ended %v after SIGTERM", alias, c[0].Ended.Sub(signalled))
		}
	}
	stopped := strings.Index(log, `"msg":"stopped"`)
	if code != 0 || stopped < 0 || strings.Count(log, `"msg":"attempt"`) != 4 || strings.Count(log, `"msg":"request"`) != 5 ||
		strings.Count(log, `"status":500,"code":"internal_error"`) != 3 || strings.Count(log, `"status":499,"code":"client_gone"`) != 1 ||
		strings.LastIndex(log, `"msg":"request"`) > stopped ||
		!strings.Contains(log, `"msg":"requests still running when the grace ended: they are stopped","listener":"data"`) {
		t.Errorf("exit %d; log %s", code, log)
	}

	st, err := store.Open(context.Background(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	records := 0
	for _, l := range strings.Split(strings.TrimSpace(log), "\n") {
		var line struct {
			Msg       string
			RequestID string `json:"request_id"`
			Attempts  int
		}
		if json.Unmarshal([]byte(l), &line) != nil || line.Msg != "request" {
			continue
		}
		var n int
		if err := st.Read().QueryRow("SELECT COUNT(*) FROM usage WHERE request_id = ?", line.RequestID).Scan(&n); err != nil || n != line.Attempts {
			t.Errorf("request %s: %d attempts, %d records, error %v", line.RequestID, line.Attempts, n, err)
		}
		records += n
	}
	if records != 4 || strings.Contains(log, "usage record not stored") {
		t.Errorf("%d records; log %s", records, log)
	}
}

// TestServeLostRecords is USE-10 and the exit code of section 11 (phase-3
// design): a record the writer cannot store before flushWait ends is one log
// line at level CRITICAL, and serve then exits 1. A trigger the test adds
// after the ready line refuses every usage insert.
func TestServeLostRecords(t *testing.T) {
	flushWait = 200 * time.Millisecond
	t.Cleanup(func() { flushWait = 10 * time.Second })
	srv := httptest.NewServer(mockprovider.New())
	t.Cleanup(srv.Close)
	dir, token := seed(t, map[string]string{"m": srv.URL})
	ready, stop := start(t, env(map[string]string{"STARGAITE_ADMIN_KEY": adminKey}), "--data-dir", dir, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0")
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "stargaite.db")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER refuse BEFORE INSERT ON usage BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+ready.DataAddr+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if code, log := stop(); code != 1 || strings.Count(log, `"level":"CRITICAL","msg":"usage record not stored"`) != 1 ||
		!strings.Contains(log, `"msg":"usage records logged instead of stored","records":1`) || strings.Contains(log, `"msg":"stopped"`) {
		t.Errorf("exit %d; log %s", code, log)
	}
}

// budgets prices the provider m of a seeded directory at 0.01 USD per
// token, so each mock call costs 0.03 USD (1 input and 2 output tokens), gives
// the key agent a never budget and the alias m a monthly one of limit, and
// makes the provider q a subscription of 2 requests an hour.
func budgets(t *testing.T, dir string, limit float64) {
	t.Helper()
	st, err := store.Open(context.Background(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Open(context.Background(), st, func() config.Env { return config.Env{Lookup: func(string) (string, bool) { return "", false }} }, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	price := 10000.0
	if _, err := cfg.Commit(context.Background(), config.Change{Summary: "budgets", Mutate: func(s *config.State) (map[string]string, error) {
		p := s.Doc.Providers["m"]
		p.Billing = domain.BillingPayPerUse
		s.Doc.Providers["m"] = p
		s.Doc.Targets["m/m"] = domain.Target{Kind: domain.KindChat, Pricing: &domain.Pricing{InputPerMillion: &price, OutputPerMillion: &price, AsOf: "2026-11-01"}}
		k := s.Doc.Keys["agent"]
		k.Budget = &domain.Budget{LimitUSD: &limit, ResetInterval: domain.ResetNever}
		s.Doc.Keys["agent"] = k
		m := s.Doc.Models["m"]
		m.Budget = &domain.Budget{LimitUSD: &limit, ResetInterval: domain.ResetMonthly}
		s.Doc.Models["m"] = m
		q := s.Doc.Providers["q"]
		q.Billing, q.Quota = domain.BillingSubscription, []domain.QuotaWindow{{Window: "1h", Requests: 2}}
		s.Doc.Providers["q"] = q
		return nil, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// spawn runs serve on dir as a process of its own, as TestMain allows, and
// returns its bound addresses; kill sends it SIGKILL and waits for its end.
func spawn(t *testing.T, dir string) (data, adminAddr string, kill func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "serve", "--data-dir", dir, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0")
	logs, stderr := io.Pipe()
	cmd.Env, cmd.Stderr = []string{"STARGAITE_TEST_RUN=1", "STARGAITE_ADMIN_KEY=" + adminKey}, stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait(); _ = stderr.Close() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	addrs := make(chan [2]string, 1)
	go func() {
		for lines := bufio.NewScanner(logs); lines.Scan(); { // read to the end, so serve never blocks on its log
			var line struct {
				Msg       string
				DataAddr  string `json:"data_addr"`
				AdminAddr string `json:"admin_addr"`
			}
			if json.Unmarshal(lines.Bytes(), &line) == nil && line.Msg == "ready" {
				addrs <- [2]string{line.DataAddr, line.AdminAddr}
			}
		}
	}()
	select {
	case a := <-addrs:
		data, adminAddr = a[0], a[1]
	case err := <-exited:
		t.Fatalf("serve exited before it was ready: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("serve was not ready within 30 s")
	}
	return data, adminAddr, func() {
		t.Helper()
		if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		if err := <-exited; err == nil || !strings.Contains(err.Error(), "killed") {
			t.Fatalf("serve after SIGKILL: %v", err)
		}
	}
}

// chat sends one chat completion to alias with token and requires a 200.
func chat(t *testing.T, data, token, alias string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+data+"/v1/chat/completions", strings.NewReader(`{"model":"`+alias+`","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// committed waits until another connection reads n usage rows in dir.
func committed(t *testing.T, dir string, n int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "stargaite.db")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var got int
		if err := db.QueryRow("SELECT COUNT(*) FROM usage").Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d records committed after 5 s, want %d", got, n)
		}
	}
}

// checkIntegrity requires integrity_check ok and an empty foreign_key_check.
func checkIntegrity(t *testing.T, db *sql.DB) {
	t.Helper()
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil || result != "ok" {
		t.Errorf("integrity_check %q, error %v", result, err)
	}
	if got := dump(t, db, "PRAGMA foreign_key_check"); len(got) != 0 {
		t.Errorf("foreign_key_check %v", got)
	}
}

// dump returns the rows of query, each as one line of text.
func dump(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		values := make([]any, len(cols))
		for i := range values {
			values[i] = new(any)
		}
		if err := rows.Scan(values...); err != nil {
			t.Fatal(err)
		}
		line := make([]string, len(values))
		for i, v := range values {
			line[i] = fmt.Sprintf("%v", *v.(*any))
		}
		out = append(out, strings.Join(line, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// copies lists the server's own backup copies in dir.
func copies(t *testing.T, dir string) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, ".stargaite-backup-*"))
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// TestServeKilled is ADR-016 and RES-9 through serve: serve runs as a
// process of its own, commits the records of three priced requests, which
// exhaust the key's never budget and the alias's monthly one, and of two
// requests that exhaust a subscription's 1h window, and is killed with
// SIGKILL while it streams a backup whose response headers have reached the
// CLI; the CLI then exits 1 and leaves neither its file nor its part. serve
// starts again on the same data directory, whose lock the kernel released:
// the key still gets 402, and once its budget is removed the alias's still
// answers 402, and the plan's alias 429 quota_exceeded with Retry-After,
// before any provider call; usage show and the store hold the five records,
// and the store passes integrity_check.
func TestServeKilled(t *testing.T) {
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	dir, token := seed(t, map[string]string{"m": srv.URL, "q": srv.URL})
	budgets(t, dir, 0.08)
	// 24 MiB, more than the socket buffers between serve and the CLI hold:
	// serve is still streaming the copy when it is killed.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "stargaite.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE filler (b BLOB); INSERT INTO filler VALUES (zeroblob(24 << 20))"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	data, adminAddr, kill := spawn(t, dir)
	for _, alias := range []string{"m", "m", "m", "q", "q"} {
		chat(t, data, token, alias)
	}
	committed(t, dir, 5)

	// Between the CLI and serve, a proxy passes the response headers and
	// the first 4 KiB on at once, and holds the rest until serve is killed.
	killed := make(chan struct{})
	// Set only by a real kill: a cleanup after an early Fatal also closes
	// killed while serve is alive, and the rest of the copy then arrives.
	var wasKilled atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, "http://"+adminAddr+r.URL.RequestURI(), nil)
		if err != nil {
			t.Error(err)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := (&http.Transport{}).RoundTrip(req)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		maps.Copy(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.CopyN(w, resp.Body, 4096)
		w.(http.Flusher).Flush()
		<-killed
		if _, err := io.Copy(io.Discard, resp.Body); err == nil && wasKilled.Load() {
			t.Error("the whole copy arrived although serve was killed")
		}
		panic(http.ErrAbortHandler) // the CLI's connection is cut as serve's was
	}))
	t.Cleanup(proxy.Close)
	// Run before proxy.Close (cleanups run last first), so a Fatal below
	// does not leave the handler, and so the test, blocked.
	release := sync.OnceFunc(func() { close(killed) })
	t.Cleanup(release)
	file := filepath.Join(t.TempDir(), "killed.db")
	backedUp := make(chan int, 1)
	var cliOut, cliErr bytes.Buffer
	go func() {
		backedUp <- cli.Main([]string{"backup", file, "--admin-url", proxy.URL}, env(map[string]string{"STARGAITE_ADMIN_KEY": adminKey}), nil, &cliOut, &cliErr)
	}()
	// The CLI creates its part once it has the response headers.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(file + ".part"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the CLI did not start writing the backup within 10 s")
		}
	}
	kill()
	wasKilled.Store(true)
	release()
	if code := <-backedUp; code != 1 || !strings.Contains(cliErr.String(), "the backup was not written") {
		t.Errorf("backup cut by the kill: exit %d, stdout %q, stderr %q", code, &cliOut, &cliErr)
	}
	for _, name := range []string{file, file + ".part"} {
		if _, err := os.Lstat(name); !os.IsNotExist(err) {
			t.Errorf("%s is there after the cut backup: %v", name, err)
		}
	}

	ready, stop := start(t, env(map[string]string{"STARGAITE_ADMIN_KEY": adminKey}), "--data-dir", dir, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0")
	post := func(alias string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "http://"+ready.DataAddr+"/v1/chat/completions", strings.NewReader(`{"model":"`+alias+`","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, string(body)
	}
	refused := func(message string) {
		t.Helper()
		if resp, body := post("m"); resp.StatusCode != http.StatusPaymentRequired || !strings.Contains(body, `"code":"budget_exceeded"`) || !strings.Contains(body, message) {
			t.Errorf("after the restart: %d %s, want 402 %s", resp.StatusCode, body, message)
		}
	}
	refused("the budget of this key is exhausted")
	// The window was seeded before the listeners bound: a Retry-After of
	// at most an hour and a 6 s bucket.
	if resp, body := post("q"); resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, `"code":"quota_exceeded"`) ||
		resp.Header.Get("Retry-After") == "" || len(resp.Header.Get("Retry-After")) > 4 {
		t.Errorf("the plan after the restart: %d %s, Retry-After %q", resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
	if code := cli.Main([]string{"keys", "set", "agent", "--budget=", "--admin-url", "http://" + ready.AdminAddr},
		env(map[string]string{"STARGAITE_ADMIN_KEY": adminKey}), nil, io.Discard, io.Discard); code != 0 {
		t.Fatalf("keys set: exit %d", code)
	}
	refused(`the budget of model \"m\" is exhausted`)
	if n := len(mock.Calls()); n != 5 {
		t.Errorf("the mock got %d calls, want 5", n)
	}
	var stdout bytes.Buffer
	if code := cli.Main([]string{"usage", "show", "--json", "--since", "1h", "--admin-url", "http://" + ready.AdminAddr},
		env(map[string]string{"STARGAITE_ADMIN_KEY": adminKey}), nil, &stdout, io.Discard); code != 0 {
		t.Fatalf("usage show: exit %d", code)
	}
	var report struct {
		Groups []struct {
			Billing  string
			Requests int64
		}
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || fmt.Sprint(report.Groups) != "[{pay_per_use 3} {subscription 2}]" {
		t.Errorf("usage show after the restart: %s, error %v", &stdout, err)
	}
	stdout.Reset()
	if code := cli.Main([]string{"usage", "quota", "--admin-url", "http://" + ready.AdminAddr},
		env(map[string]string{"STARGAITE_ADMIN_KEY": adminKey}), nil, &stdout, io.Discard); code != 0 ||
		!strings.Contains(stdout.String(), "counts this proxy's traffic only") || !strings.Contains(stdout.String(), "q         -    -      1h      requests  2     2      100") {
		t.Errorf("usage quota after the restart: exit %d\n%s", code, &stdout)
	}
	if code, log := stop(); code != 0 {
		t.Errorf("exit %d; log %s", code, log)
	}
	st, err := store.Open(context.Background(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var records, daily int
	if err := st.Read().QueryRow("SELECT (SELECT COUNT(*) FROM usage), (SELECT SUM(requests) FROM usage_daily)").Scan(&records, &daily); err != nil ||
		records != 5 || daily != 5 {
		t.Errorf("%d records, usage_daily counts %d, error %v", records, daily, err)
	}
	checkIntegrity(t, st.Read())
	if left := copies(t, dir); len(left) != 0 {
		t.Errorf("copies left in the data directory: %v", left)
	}
}

// TestBackupRestore is AC-50 and OPS-7: serve commits traffic, the CLI takes
// a backup, more traffic follows, and serve is killed with SIGKILL, leaving a
// -wal. The README's procedure (stargaite.db, -wal and -shm moved aside
// together, the backup copied in as stargaite.db with mode 0600) and a start
// give a store whose records, daily sums and budget totals are the backup's,
// with no row written after it, whose usage_daily equals the GROUP BY over
// usage, which passes integrity_check, whose budgets a fresh Restore reads as
// it reads the backup's, and where a key token of before the backup still
// authenticates. The negative control keeps the stale -wal beside the copy
// and finds the newer rows.
func TestBackupRestore(t *testing.T) {
	srv := httptest.NewServer(mockprovider.New())
	t.Cleanup(srv.Close)
	dir, token := seed(t, map[string]string{"m": srv.URL, "q": srv.URL})
	budgets(t, dir, 1000)
	data, adminAddr, kill := spawn(t, dir)
	for range 4 {
		chat(t, data, token, "m")
	}
	committed(t, dir, 4)
	backup := filepath.Join(t.TempDir(), "backup.db")
	var stdout, stderr bytes.Buffer
	if code := cli.Main([]string{"backup", backup, "--admin-url", "http://" + adminAddr}, env(map[string]string{"STARGAITE_ADMIN_KEY": adminKey}), nil, &stdout, &stderr); code != 0 ||
		!strings.HasPrefix(stdout.String(), backup+": ") {
		t.Fatalf("backup: exit %d, stdout %q, stderr %q", code, &stdout, &stderr)
	}
	if info, err := os.Stat(backup); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the backup: %v, error %v", info, err)
	}
	if left := copies(t, dir); len(left) != 0 {
		t.Errorf("copies left in the data directory: %v", left)
	}
	for range 3 {
		chat(t, data, token, "m")
	}
	committed(t, dir, 7)
	kill()
	if info, err := os.Stat(filepath.Join(dir, "stargaite.db-wal")); err != nil || info.Size() == 0 {
		t.Fatalf("no -wal after the kill: %v, error %v", info, err)
	}
	copyFile := func(from, to string) {
		t.Helper()
		in, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, in, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The negative control: the stale -wal beside the copy is replayed onto
	// it, so the store is no longer the backup. VACUUM INTO lays the pages
	// out afresh (the indexes after the tables), so the frames meant for the
	// live file land on other pages: the store holds the newer rows or, as
	// measured with this layout, fails integrity_check.
	control := t.TempDir()
	copyFile(backup, filepath.Join(control, "stargaite.db"))
	for _, suffix := range []string{"-wal", "-shm"} {
		copyFile(filepath.Join(dir, "stargaite.db"+suffix), filepath.Join(control, "stargaite.db"+suffix))
	}
	if ctl, err := store.Open(context.Background(), control, slog.New(slog.DiscardHandler)); err == nil {
		var integrity string
		var records int
		errIntegrity := ctl.Read().QueryRow("PRAGMA integrity_check").Scan(&integrity)
		errRecords := ctl.Read().QueryRow("SELECT COUNT(*) FROM usage").Scan(&records)
		if errIntegrity == nil && integrity == "ok" && errRecords == nil && records != 7 {
			t.Errorf("the copy with the stale -wal passes integrity_check with %d records: the -wal was not replayed", records)
		}
		_ = ctl.Close()
	}

	// The procedure of the README.
	aside := t.TempDir()
	for _, name := range []string{"stargaite.db", "stargaite.db-wal", "stargaite.db-shm"} {
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(aside, name)); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(backup, filepath.Join(dir, "stargaite.db"))
	ready, stop := start(t, env(map[string]string{"STARGAITE_ADMIN_KEY": adminKey}), "--data-dir", dir, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0")
	if code, err := get(t, &http.Client{Timeout: 5 * time.Second}, "http://"+ready.DataAddr+"/v1/models", ""); err != nil || code != http.StatusUnauthorized {
		t.Errorf("/v1/models without a key: %d, error %v", code, err)
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+ready.DataAddr+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the key token of before the backup: status %d", resp.StatusCode)
	}
	if code, log := stop(); code != 0 {
		t.Errorf("exit %d; log %s", code, log)
	}

	st, err := store.Open(context.Background(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	copied, err := sql.Open("sqlite", "file:"+backup+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = copied.Close() }()
	for _, q := range []string{"SELECT * FROM usage ORDER BY id", "SELECT * FROM usage_daily ORDER BY 1, 2, 3, 4, 5",
		"SELECT * FROM budget_totals ORDER BY 1, 2", "SELECT * FROM key_secrets ORDER BY 1"} {
		if got, want := dump(t, st.Read(), q), dump(t, copied, q); fmt.Sprint(got) != fmt.Sprint(want) || len(want) == 0 {
			t.Errorf("%s:\n%v\nthe backup's:\n%v", q, got, want)
		}
	}
	if n := dump(t, st.Read(), "SELECT COUNT(*) FROM usage"); fmt.Sprint(n) != "[4]" {
		t.Errorf("%v records after the restore, want the 4 of the backup", n)
	}
	daily := `SELECT day, key_name, model, target, billing, requests, errors, input_tokens, output_tokens, printf('%.9f', cost_usd) FROM usage_daily ORDER BY 1, 2, 3, 4, 5`
	grouped := `SELECT strftime('%Y-%m-%d', completed_at / 1000, 'unixepoch'), key_name, model, target, billing, COUNT(*), COUNT(error_type),
		SUM(input_tokens), SUM(output_tokens), printf('%.9f', SUM(COALESCE(cost_usd, 0))) FROM usage GROUP BY 1, 2, 3, 4, 5 ORDER BY 1, 2, 3, 4, 5`
	if a, b := dump(t, st.Read(), daily), dump(t, st.Read(), grouped); fmt.Sprint(a) != fmt.Sprint(b) {
		t.Errorf("usage_daily\n%v\nGROUP BY usage\n%v", a, b)
	}
	checkIntegrity(t, st.Read())
	restored, fresh := admission.New(time.Now, nil), admission.New(time.Now, nil)
	for state, db := range map[*admission.State]*sql.DB{restored: st.Read(), fresh: copied} {
		if err := usage.Restore(context.Background(), db, state, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	var since int64
	if err := copied.QueryRow("SELECT since_revision FROM budget_totals WHERE scope_type = 'key' AND scope_name = 'agent'").Scan(&since); err != nil {
		t.Fatal(err)
	}
	bs := []admission.Budget{{Scope: "key:agent", Spec: domain.Budget{ResetInterval: domain.ResetNever}, Since: since}}
	for _, scope := range []string{"global", "key:agent", "model:m"} {
		for _, r := range []string{domain.ResetDaily, domain.ResetMonthly} {
			bs = append(bs, admission.Budget{Scope: scope, Spec: domain.Budget{ResetInterval: r}})
		}
	}
	for _, b := range bs {
		if got, want := restored.Spent(b), fresh.Spent(b); math.Abs(got-want) > 1e-9 || math.Abs(want-0.12) > 1e-9 {
			t.Errorf("%s %s: %v restored, %v from the backup, want 0.12", b.Scope, b.Spec.ResetInterval, got, want)
		}
	}
}

// TestServeTLS checks that the certificate pair covers both listeners.
func TestServeTLS(t *testing.T) {
	certFile, keyFile, roots := writeCert(t)
	ready, stop := start(t, env(nil), "--data-dir", t.TempDir(), "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0",
		"--tls-cert", certFile, "--tls-key", keyFile)
	if !ready.TLS {
		t.Errorf("ready line %+v", ready)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	for url, want := range map[string]int{
		"https://" + ready.DataAddr + "/health/ready":       http.StatusOK,
		"https://" + ready.AdminAddr + "/admin/v1/settings": http.StatusUnauthorized,
		"http://" + ready.DataAddr + "/health/ready":        http.StatusBadRequest, // plain HTTP to a TLS listener
		"http://" + ready.AdminAddr + "/admin/v1/settings":  http.StatusBadRequest,
	} {
		if code, err := get(t, client, url, ""); err != nil || code != want {
			t.Errorf("GET %s: status %d, error %v; want %d", url, code, err, want)
		}
	}
	if code, _ := stop(); code != 0 {
		t.Errorf("exit %d after SIGTERM", code)
	}
}

// writeCert writes a self-signed certificate for 127.0.0.1 and its key.
func writeCert(t *testing.T) (certFile, keyFile string, roots *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	for file, block := range map[string]*pem.Block{certFile: {Type: "CERTIFICATE", Bytes: der}, keyFile: {Type: "PRIVATE KEY", Bytes: pkcs8}} {
		if err := os.WriteFile(file, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	roots = x509.NewCertPool()
	roots.AddCert(cert)
	return certFile, keyFile, roots
}

func TestServeExitCodes(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	loopback := []string{"--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0"}

	for _, tc := range []struct {
		name    string
		vars    map[string]string
		args    []string
		code    int
		msg     string // the log line of an exit 1
		created bool   // the data directory may exist afterwards
	}{
		{name: "help", args: []string{"--help"}, code: 0},
		{name: "unknown flag", args: []string{"--nope"}, code: 2},
		{name: "unexpected argument", args: []string{"extra"}, code: 2},
		{name: "certificate without key", args: append([]string{"--tls-cert", "c.pem"}, loopback...), code: 2},
		// Given last, it overrides the --data-dir the loop prepends; the taken
		// address makes a serve without the check exit instead of running.
		{name: "empty data directory", args: []string{"--listen", taken.Addr().String(), "--admin-listen", "127.0.0.1:0", "--data-dir="}, code: 2},
		{name: "malformed variable", vars: map[string]string{"STARGAITE_BEHIND_TLS_PROXY": "maybe"}, args: loopback, code: 2},
		{name: "data listener outside loopback", args: []string{"--listen", "0.0.0.0:0", "--admin-listen", "127.0.0.1:0"}, code: 1, msg: "listener refused"},
		{name: "admin listener on every address", args: []string{"--listen", "127.0.0.1:0", "--admin-listen", ":0"}, code: 1, msg: "listener refused"},
		{name: "default admin address with a public data address", args: []string{"--listen", "192.0.2.1:4000"}, code: 1, msg: "listener refused"},
		{name: "address without port", args: []string{"--listen", "127.0.0.1", "--admin-listen", "127.0.0.1:0"}, code: 1, msg: "listener refused"},
		{name: "unreadable certificate", args: append([]string{"--tls-cert", "missing.pem", "--tls-key", "missing.pem"}, loopback...), code: 1, msg: "tls certificate refused"},
		{name: "address in use", vars: map[string]string{"STARGAITE_ADMIN_KEY": adminKey}, args: []string{"--json", "--listen", taken.Addr().String(), "--admin-listen", "127.0.0.1:0"}, code: 1, msg: "server failed", created: true},
		{name: "short admin key", vars: map[string]string{"STARGAITE_ADMIN_KEY": adminKey[:31]}, args: loopback, code: 1, msg: "admin key refused", created: true},
		{name: "admin key with the client key prefix", vars: map[string]string{"STARGAITE_ADMIN_KEY": "sk-stargaite-" + adminKey}, args: loopback, code: 1, msg: "admin key refused", created: true},
		// SEC-3, phase-4 design section 15 point 17.
		{name: "short metrics token", vars: map[string]string{"STARGAITE_ADMIN_KEY": adminKey, "STARGAITE_METRICS_TOKEN": testMetricsToken[:31]}, args: loopback, code: 1, msg: "metrics token refused", created: true},
		{name: "metrics token with the client key prefix", vars: map[string]string{"STARGAITE_ADMIN_KEY": adminKey, "STARGAITE_METRICS_TOKEN": "sk-stargaite-" + testMetricsToken}, args: loopback, code: 1, msg: "metrics token refused", created: true},
		{name: "metrics token equal to the admin key", vars: map[string]string{"STARGAITE_ADMIN_KEY": adminKey, "STARGAITE_METRICS_TOKEN": adminKey}, args: loopback, code: 1, msg: "metrics token refused", created: true},
		{name: "metrics token with whitespace", vars: map[string]string{"STARGAITE_ADMIN_KEY": adminKey, "STARGAITE_METRICS_TOKEN": testMetricsToken + " "}, args: loopback, code: 1, msg: "metrics token refused", created: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			var stdout, stderr bytes.Buffer
			if code := serve(append([]string{"--data-dir", dir}, tc.args...), env(tc.vars), &stdout, &stderr); code != tc.code {
				t.Fatalf("exit %d, want %d; stderr %q", code, tc.code, &stderr)
			}
			if tc.code == 0 && (stdout.Len() == 0 || stderr.Len() != 0) {
				t.Errorf("success must write to stdout only; stdout %q stderr %q", &stdout, &stderr)
			}
			if tc.code != 0 && (stdout.Len() != 0 || stderr.Len() == 0) {
				t.Errorf("a failure must write to stderr only; stdout %q stderr %q", &stdout, &stderr)
			}
			if tc.code == 1 {
				var line struct{ Level, Msg, Rule string }
				if err := json.Unmarshal(stderr.Bytes(), &line); err != nil || line.Level != "ERROR" || line.Msg != tc.msg {
					t.Errorf("log line %q: %+v, error %v", &stderr, line, err)
				}
				if token := tc.vars["STARGAITE_METRICS_TOKEN"]; token != "" && (line.Rule != "SEC-3" || strings.Contains(stderr.String(), strings.TrimSpace(token))) {
					t.Errorf("log line %q: the rule SEC-3, never the token", &stderr)
				}
			}
			if _, err := os.Stat(dir); err == nil && !tc.created {
				t.Error("the data directory was created before the refusal")
			}
		})
	}
	// The serve that failed to bind closed the store and released the lock.
	dir := t.TempDir()
	_, stop := start(t, env(nil), append([]string{"--data-dir", dir}, loopback...)...)
	if code, _ := stop(); code != 0 {
		t.Errorf("exit %d", code)
	}
}

// TestServeRestoreFails: budgets that cannot be restored from the store stop
// serve with exit 1 before the listeners bind (RES-9, phase-3 design section
// 11), and the store is closed, so its lock is released.
func TestServeRestoreFails(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO usage_daily (day, key_name, model, target, billing, cost_usd) VALUES ('9999-99', 'k', 'm', 'p/m', 'pay_per_use', 1)")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := serve([]string{"--data-dir", dir, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0"}, env(nil), io.Discard, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), `"level":"ERROR","msg":"budgets not restored"`) || strings.Contains(stderr.String(), `"msg":"ready"`) {
		t.Fatalf("exit %d; log %s", code, &stderr)
	}
	st, err = store.Open(context.Background(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("the lock is still held: %v", err)
	}
	_ = st.Close()
}

// TestServeQuotaSeedFails: quota windows that cannot be seeded from the store
// stop serve with exit 1 before the listeners bind (RES-9, LIM-8, phase-3
// design section 11), with the cause logged, and the store is closed.
func TestServeQuotaSeedFails(t *testing.T) {
	dir, _ := seed(t, map[string]string{"q": "http://127.0.0.1:1"})
	st, err := store.Open(context.Background(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Open(context.Background(), st, func() config.Env { return config.Env{Lookup: func(string) (string, bool) { return "", false }} }, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Commit(context.Background(), config.Change{Summary: "quota", Mutate: func(s *config.State) (map[string]string, error) {
		q := s.Doc.Providers["q"]
		q.Billing, q.Quota = domain.BillingSubscription, []domain.QuotaWindow{{Window: "5h", Requests: 10}}
		s.Doc.Providers["q"] = q
		return nil, nil
	}}); err != nil {
		t.Fatal(err)
	}
	// A stored row the seed cannot read: its started_at is not a number.
	if err := st.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO usage (id, request_id, endpoint, key_name, model, target, billing, status_code, tokens_source, cost_source,
			started_at, completed_at, config_revision, proxy_version)
			VALUES ('bad-1', 'bad', 'chat', 'agent', 'q', 'q/m', 'subscription', 200, 'provider', 'none', 'not a time', ?, 1, 'v')`, time.Now().UnixMilli())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := serve([]string{"--data-dir", dir, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0"}, env(nil), io.Discard, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), `"level":"ERROR","msg":"quota windows not seeded","provider":"q"`) ||
		!strings.Contains(stderr.String(), `"level":"ERROR","msg":"quotas not restored"`) || strings.Contains(stderr.String(), `"msg":"ready"`) {
		t.Fatalf("exit %d; log %s", code, &stderr)
	}
	st, err = store.Open(context.Background(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("the lock is still held: %v", err)
	}
	_ = st.Close()
}

// TestServeRetention is AC-49 through serve (phase-4 design section 8): with
// retainFirst lowered, the first run deletes a 200-day-old record, a
// 25-month-old audit row and a 37-month-old aggregate, while the current
// month's aggregate, the configuration's audit rows and the record of a 7d
// window survive usage_days 1. The second run, a few milliseconds later, is
// held across the stop and writes once its context has ended: the store is
// still open, since the run ends before the store closes. No run fails,
// nothing is logged at CRITICAL, and the stopped line is the last.
func TestServeRetention(t *testing.T) {
	holding, held := make(chan struct{}), make(chan error, 1)
	runs := 0
	retainFirst, retainEvery = 50*time.Millisecond, 5*time.Millisecond
	retainRun = func(ctx context.Context, write func(context.Context, func(*sql.Tx) error) error, c usage.Cutoffs) (usage.Deleted, error) {
		if runs++; runs == 1 {
			return usage.Retain(ctx, write, c)
		}
		close(holding)
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond) // time for a store closed too early to close
		held <- write(context.Background(), func(*sql.Tx) error { return nil })
		return usage.Deleted{}, ctx.Err()
	}
	t.Cleanup(func() { retainFirst, retainEvery, retainRun = time.Minute, 24*time.Hour, usage.Retain })
	dir, _ := seed(t, map[string]string{"q": "http://127.0.0.1:1"})
	st, err := store.Open(context.Background(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Open(context.Background(), st, func() config.Env { return config.Env{Lookup: func(string) (string, bool) { return "", false }} }, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Commit(context.Background(), config.Change{Summary: "retention", Mutate: func(s *config.State) (map[string]string, error) {
		q := s.Doc.Providers["q"]
		q.Billing, q.Quota = domain.BillingSubscription, []domain.QuotaWindow{{Window: "7d", Requests: 100}}
		s.Doc.Providers["q"] = q
		s.Doc.Settings.Retention = &domain.Retention{UsageDays: 1}
		return nil, nil
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.Write(context.Background(), func(tx *sql.Tx) error {
		for id, at := range map[string]time.Time{"old": now.AddDate(0, 0, -200), "window": now.AddDate(0, 0, -3)} {
			if _, err := tx.Exec(`INSERT INTO usage (id, request_id, endpoint, key_name, model, target, billing, status_code, tokens_source, cost_source,
				started_at, completed_at, config_revision, proxy_version)
				VALUES (?, ?, 'chat', 'agent', 'q', 'q/m', 'subscription', 200, 'provider', 'none', ?, ?, 1, 'v')`, id, id, at.UnixMilli()-1000, at.UnixMilli()); err != nil {
				return err
			}
		}
		for _, day := range []time.Time{now.AddDate(0, -37, 0), now} {
			if _, err := tx.Exec(`INSERT INTO usage_daily (day, key_name, model, target, billing, requests) VALUES (?, 'agent', 'q', 'q/m', 'subscription', 1)`,
				day.Format(time.DateOnly)); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`INSERT INTO audit (id, actor, action, resource_type, resource_name, result, created_at)
			VALUES ('old', 'operator', 'test', 'settings', 'retention', 'applied', ?)`, now.AddDate(0, -25, 0).UnixMilli())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var audits int
	if err := st.Read().QueryRow("SELECT COUNT(*) FROM audit").Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	_, stop := start(t, env(map[string]string{"STARGAITE_ADMIN_KEY": adminKey}), "--data-dir", dir, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0")
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "stargaite.db")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows := func(q string) []string { t.Helper(); return dump(t, db, q) }
	for deadline := time.Now().Add(10 * time.Second); len(rows("SELECT id FROM usage WHERE id = 'old' UNION ALL SELECT id FROM audit WHERE id = 'old'")) != 0 ||
		len(rows("SELECT day FROM usage_daily")) != 1; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the old rows are still stored after 10 s")
		}
	}
	select {
	case <-holding:
	case <-time.After(10 * time.Second):
		t.Fatal("no second run after 10 s")
	}
	code, log := stop()
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if code != 0 || !strings.Contains(log, `"msg":"retention run","usage":1,"usage_daily":1,"audit":1,`) || strings.Contains(log, "retention failed") ||
		strings.Contains(log, `"level":"CRITICAL"`) || !strings.Contains(lines[len(lines)-1], `"msg":"stopped"`) {
		t.Errorf("exit %d; log %s", code, log)
	}
	select {
	case err := <-held:
		if err != nil {
			t.Errorf("a write of the run held across the stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the run held across the stop never wrote")
	}
	if got := rows("SELECT id FROM usage"); !slices.Equal(got, []string{"window"}) {
		t.Errorf("records %q, want the 7d window's", got)
	}
	if got := rows("SELECT day FROM usage_daily"); !slices.Equal(got, []string{now.Format(time.DateOnly)}) {
		t.Errorf("aggregates %q, want this month's", got)
	}
	if got := rows("SELECT id FROM audit WHERE id = 'old'"); len(got) != 0 || len(rows("SELECT id FROM audit")) != audits-1 {
		t.Errorf("audit rows: the old one %q, %d of %d kept", got, len(rows("SELECT id FROM audit")), audits-1)
	}
}

// TestServeStoreCheck is the store check of kick-off section 14.3 through
// serve: with the root page of an index overwritten, serve starts, logs one
// CRITICAL line with quick_check's rows after the ready line, keeps serving
// and stops as usual; checkStore counts store_check_failed. On a healthy
// store it logs that the check passed and counts nothing; when quick_check
// itself fails, on a closed store, it logs CRITICAL with the error and counts.
func TestServeStoreCheck(t *testing.T) {
	dir, token := seed(t, map[string]string{"m": "http://127.0.0.1:1"})
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "stargaite.db"))
	if err != nil {
		t.Fatal(err)
	}
	var page, size int64
	if err := db.QueryRow("SELECT rootpage, (SELECT page_size FROM pragma_page_size) FROM sqlite_schema WHERE name = 'idx_usage_key_time'").Scan(&page, &size); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "stargaite.db"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xA5}, int(size)), (page-1)*size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	logs, stderr := io.Pipe()
	exit := make(chan int, 1)
	go func() {
		exit <- serve([]string{"--data-dir", dir, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0"},
			env(map[string]string{"STARGAITE_ADMIN_KEY": adminKey, "STARGAITE_METRICS_TOKEN": testMetricsToken}), io.Discard, stderr)
		_ = stderr.Close()
	}()
	lines := bufio.NewScanner(logs)
	var log strings.Builder
	var data, adminAddr string
	for !strings.Contains(log.String(), `"msg":"store check`) {
		if !lines.Scan() {
			t.Fatalf("serve ended before its store check; log %s", &log)
		}
		log.WriteString(lines.Text() + "\n")
		var line readyLine
		if json.Unmarshal(lines.Bytes(), &line) == nil && line.Msg == "ready" {
			data, adminAddr = line.DataAddr, line.AdminAddr
		}
	}
	rest := make(chan string, 1)
	go func() { // read to the end, so serve never blocks on its log
		var b strings.Builder
		for lines.Scan() {
			b.WriteString(lines.Text() + "\n")
		}
		rest <- b.String()
	}()
	if code, err := get(t, &http.Client{Timeout: 5 * time.Second}, "http://"+data+"/v1/models", ""); err != nil || code != http.StatusUnauthorized {
		t.Errorf("GET /v1/models without a key: %d, %v", code, err)
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+data+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/models after the failed check: %d", resp.StatusCode)
	}
	// The failure is counted on the metrics endpoint, which the metrics
	// token opens; so is the unauthenticated GET /v1/models above.
	req, err = http.NewRequest(http.MethodGet, "http://"+adminAddr+"/admin/v1/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testMetricsToken)
	resp, err = (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	scraped, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(scraped), "\nstargaite_store_check_failures_total 1\n") ||
		!strings.Contains(string(scraped), "\nstargaite_unauthenticated_requests_total 1\n") {
		t.Errorf("metrics: status %d, error %v:\n%s", resp.StatusCode, err, scraped)
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	log.WriteString(<-rest)
	all := strings.Split(strings.TrimSpace(log.String()), "\n")
	if code := <-exit; code != 0 || strings.Count(log.String(), `"level":"CRITICAL"`) != 1 ||
		!strings.Contains(log.String(), `"level":"CRITICAL","msg":"store check failed","rows":["*** in database main ***\nTree `+fmt.Sprintf("%d page %d: ", page, page)) ||
		!strings.Contains(all[len(all)-1], `"msg":"stopped"`) {
		t.Errorf("exit %d; log %s", code, &log)
	}

	healthy, _ := seed(t, map[string]string{"m": "http://127.0.0.1:1"})
	for _, tc := range []struct {
		dir, want string
		closed    bool // quick_check itself fails, not by the stop
		failed    float64
	}{
		{dir, `"level":"ERROR+4","msg":"store check failed","rows":[`, false, 1},
		{healthy, `"level":"INFO","msg":"store check passed"`, false, 0},
		{healthy, `"level":"ERROR+4","msg":"store check failed","error":"sql: database is closed"`, true, 1},
	} {
		st, err := store.Open(context.Background(), tc.dir, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		if tc.closed {
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
		}
		var buf bytes.Buffer
		var counters usage.Counters
		checkStore(context.Background(), st, slog.New(slog.NewJSONHandler(&buf, nil)), &counters)
		var failed float64
		counters.Each(func(c usage.Counter, v float64) {
			if c == (usage.Counter{Name: "store_check_failed"}) {
				failed = v
			}
		})
		if failed != tc.failed || !strings.Contains(buf.String(), tc.want) {
			t.Errorf("%s: store_check_failed %v, log %s", tc.dir, failed, &buf)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestMetricsToken is the rule of STARGAITE_METRICS_TOKEN (SEC-3, phase-4
// design section 15 point 17): the admin key's rule, counted in characters,
// and not the admin key; only its SHA-256 is kept, and no error carries it.
func TestMetricsToken(t *testing.T) {
	adminHash := sha256.Sum256([]byte(adminKey))
	for _, tc := range []struct {
		token string
		ok    bool
	}{
		{testMetricsToken, true},
		{testMetricsToken[:32], true},
		{testMetricsToken[:31], false},
		{strings.Repeat("é", 31), false}, // 62 bytes, 31 characters
		{strings.Repeat("é", 32), true},
		{" " + testMetricsToken, false},
		{testMetricsToken + "\n", false},
		{"sk-stargaite-" + testMetricsToken, false},
		{adminKey, false},
	} {
		hash, err := metricsToken(tc.token, adminHash)
		switch {
		case tc.ok && (err != nil || hash == nil || *hash != sha256.Sum256([]byte(tc.token))):
			t.Errorf("%q: hash %v, error %v", tc.token, hash, err)
		case !tc.ok && (err == nil || hash != nil || strings.Contains(err.Error(), strings.TrimSpace(tc.token))):
			t.Errorf("%q: hash %v, error %v: refused, never naming the token", tc.token, hash, err)
		}
	}
	if hash, err := metricsToken("", adminHash); hash != nil || err != nil {
		t.Errorf("unset: hash %v, error %v", hash, err)
	}
}

// TestCheckListener is the listener table of SEC-6 and AC-45.
func TestCheckListener(t *testing.T) {
	for _, tc := range []struct {
		addr     string
		loopback bool // allowed without TLS
	}{
		{"127.0.0.1:4000", true},
		{"127.0.0.2:4000", true},
		{"[::1]:4000", true},
		{"localhost:4000", true},
		{":4000", false},
		{"0.0.0.0:4000", false},
		{"[::]:4000", false},
		{"192.0.2.10:4000", false},
		{"[2001:db8::1]:4000", false},
		{"[fe80::1%lo]:4000", false},
		{"example.com:4000", false},
		{"LOCALHOST:4000", false},
		{"localhost.:4000", false},
	} {
		if err := checkListener(tc.addr, false); (err == nil) != tc.loopback {
			t.Errorf("%s without TLS: error %v", tc.addr, err)
		}
		if err := checkListener(tc.addr, true); err != nil {
			t.Errorf("%s with TLS: error %v", tc.addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1", "", "[::1]"} {
		if err := checkListener(addr, true); err == nil {
			t.Errorf("%q: no error", addr)
		}
	}
}

func TestDefaultsAreLoopback(t *testing.T) {
	var stdout bytes.Buffer
	if code := serve([]string{"--help"}, env(nil), &stdout, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{`(default "127.0.0.1:4000")`, `(default "127.0.0.1:4001")`, `(default "./data")`} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("help lacks %s:\n%s", want, &stdout)
		}
	}
}

func TestRunDispatch(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, env(nil), nil, &stdout, &stderr); code != 0 || stdout.String() != cli.Version+"\n" {
		t.Errorf("version: exit %d stdout %q stderr %q", code, &stdout, &stderr)
	}
	// Only serve answers its own help with 0: the CLI has no such command.
	if code := run([]string{"serve", "--help"}, env(nil), nil, &stdout, &stderr); code != 0 {
		t.Errorf("serve --help: exit %d", code)
	}
}

// change commits mutate to the configuration stored in dir, with no serve
// running and no credential resolving.
func change(t *testing.T, dir string, mutate func(*config.State)) {
	t.Helper()
	st, err := store.Open(context.Background(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Open(context.Background(), st, func() config.Env { return config.Env{Lookup: func(string) (string, bool) { return "", false }} }, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Commit(context.Background(), config.Change{Summary: "apply", Mutate: func(s *config.State) (map[string]string, error) {
		mutate(s)
		return nil, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// renameInto writes an env file as the README says: a temporary file in the
// same directory, mode 0600, renamed over the old one, so a reader sees the
// old file or the new one whole.
func renameInto(t *testing.T, path, content string) {
	t.Helper()
	tmp := filepath.Join(filepath.Dir(path), ".env.new")
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("env file %v, error %v", info, err)
	}
}

// reloadSecrets runs stargaite secrets reload against admin and returns its
// exit code, stdout and stderr.
func reloadSecrets(admin string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := cli.Main(append([]string{"secrets", "reload", "--admin-url", "http://" + admin}, args...), env(map[string]string{"STARGAITE_ADMIN_KEY": adminKey}), nil, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// waitFor polls cond every 10 ms for up to 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 10 s", what)
		}
	}
}

// TestSecretsReload is AC-44 through serve (phase-4 design section 8). The
// store holds, as an upgrade would leave it, a value this binary refuses, so
// serve starts with an error finding; the provider's variable is absent from
// the env file, so its alias answers 503 no_target_available. A reload then
// answers the unresolved variable by name. A new env file renamed into place
// and secrets reload, with no other write: the next call reaches the mock
// with the new credential, the first telemetry batch after the reload
// carries the new header value, the revision is unchanged and each reload
// left an unchanged audit row. Neither value is in the log, the audit rows,
// the CLI's output or the data plane's answers.
func TestSecretsReload(t *testing.T) {
	const planted, oldToken, newToken = "sk-PLANTED-reloaded-credential", "PLANTED-old-ingest-token", "PLANTED-new-ingest-token"
	mock, receiver := mockprovider.New(), mockprovider.New()
	srv, rsrv := httptest.NewServer(mock), httptest.NewServer(receiver)
	t.Cleanup(srv.Close)
	t.Cleanup(rsrv.Close)
	dir, token := seed(t, map[string]string{"m": srv.URL})
	change(t, dir, func(s *config.State) {
		p := s.Doc.Providers["m"]
		p.APIKeyEnv = "M_KEY"
		s.Doc.Providers["m"] = p
		s.Doc.Settings.Telemetry = &domain.Telemetry{OTLPEndpoint: rsrv.URL, AllowPrivateNetwork: true, HeaderEnv: map[string]string{"X-Ingest-Token": "T_KEY"}}
	})
	st, err := store.Open(context.Background(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO settings (name, value, updated_at) VALUES ('retention', '{"audit_months":-1}', 0)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(t.TempDir(), "env")
	renameInto(t, envFile, "T_KEY="+oldToken+"\n")

	// serve logs the stored error finding as an ERROR line before it is
	// ready, which start refuses: its log is read here.
	logs, stderr := io.Pipe()
	exit := make(chan int, 1)
	go func() {
		exit <- serve([]string{"--data-dir", dir, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0", "--env-file", envFile},
			env(map[string]string{"STARGAITE_ADMIN_KEY": adminKey}), io.Discard, stderr)
		_ = stderr.Close()
	}()
	var log strings.Builder // read once ended is closed
	readyc, ended := make(chan readyLine, 1), make(chan struct{})
	go func() { // read to the end, so serve never blocks on its log
		defer close(ended)
		for lines := bufio.NewScanner(logs); lines.Scan(); {
			log.WriteString(lines.Text() + "\n")
			var line readyLine
			if json.Unmarshal(lines.Bytes(), &line) == nil && line.Msg == "ready" {
				readyc <- line
			}
		}
	}()
	var ready readyLine
	select {
	case ready = <-readyc:
	case <-ended:
		t.Fatalf("serve ended before it was ready; log %s", &log)
	case <-time.After(30 * time.Second):
		t.Fatal("serve was not ready within 30 s")
	}

	var answers strings.Builder // every answer, for the planted check
	post := func() int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "http://"+ready.DataAddr+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&answers, "%v\n%s\n", resp.Header, body)
		if resp.StatusCode != http.StatusOK && !strings.Contains(string(body), `"code":"no_target_available"`) {
			t.Errorf("status %d: %s", resp.StatusCode, body)
		}
		return resp.StatusCode
	}
	reload := func(args ...string) (int, string) {
		t.Helper()
		code, stdout, stderr := reloadSecrets(ready.AdminAddr, args...)
		answers.WriteString(stdout + stderr)
		return code, stdout
	}
	traces := func() []mockprovider.Call {
		var out []mockprovider.Call
		for _, c := range receiver.Calls() {
			if c.Path == "/v1/traces" {
				out = append(out, c)
			}
		}
		return out
	}
	const invalid = "retention.audit_months"
	// The chat target m/m was never verified (RSN-9).
	const unverified = "warning target_unverified  this chat target was never checked against its live provider: run targets verify on it"
	findings := func(stdout string) (config.Result, []string) {
		t.Helper()
		var res config.Result
		if err := json.Unmarshal([]byte(stdout), &res); err != nil {
			t.Fatalf("secrets reload --json: %q: %v", stdout, err)
		}
		var out []string
		for _, f := range res.Findings {
			out = append(out, f.Severity+" "+f.Code+" "+f.Field+" "+f.Message)
		}
		return res, out
	}

	if code := post(); code != http.StatusServiceUnavailable || len(mock.Calls()) != 0 {
		t.Fatalf("without the variable: status %d, %d provider calls", code, len(mock.Calls()))
	}
	waitFor(t, "a telemetry batch before the reload", func() bool { return len(traces()) > 0 })

	// The variable is still absent: the finding names it, never a value.
	code, stdout := reload("--json")
	res, got := findings(stdout)
	if code != 1 || res.Revision != ready.Revision || res.Changed || len(res.Diff) != 0 || !slices.Equal(got, []string{
		"error invalid_value " + invalid + " " + invalid + " must not be negative",
		"warning credential_unresolved api_key_env M_KEY does not resolve",
		unverified,
	}) {
		t.Errorf("reload without the variable: exit %d, %+v, findings %q", code, res, got)
	}

	renameInto(t, envFile, "M_KEY="+planted+"\nT_KEY="+newToken+"\n")
	batches := len(traces())
	for _, c := range traces() {
		if c.Header.Get("X-Ingest-Token") != oldToken {
			t.Errorf("a batch before the reload carries %q", c.Header.Get("X-Ingest-Token"))
		}
	}
	// The stored error finding still fails the exit code, as on every command.
	if code, stdout := reload(); code != 1 || !strings.Contains(stdout, fmt.Sprintf("revision: %d\n", ready.Revision)) ||
		!strings.Contains(stdout, "changed: false\n") || !strings.Contains(stdout, invalid) || strings.Contains(stdout, "M_KEY") {
		t.Errorf("secrets reload: exit %d, stdout %q", code, stdout)
	}
	code, stdout = reload("--json")
	res, got = findings(stdout)
	if code != 1 || res.Revision != ready.Revision || res.Changed || !slices.Equal(got, []string{"error invalid_value " + invalid + " " + invalid + " must not be negative", unverified}) {
		t.Errorf("reload with the variable: exit %d, %+v, findings %q", code, res, got)
	}
	if code := post(); code != http.StatusOK {
		t.Fatalf("after the reload: status %d", code)
	}
	if calls := mock.Calls(); len(calls) != 1 || calls[0].Header.Get("Authorization") != "Bearer "+planted {
		t.Errorf("provider calls %d; the credential sent is not the new one", len(calls))
	}
	waitFor(t, "a telemetry batch after the reload", func() bool { return len(traces()) > batches })
	for _, c := range traces()[batches:] {
		if c.Header.Get("X-Ingest-Token") != newToken {
			t.Errorf("a batch after the reload carries %q", c.Header.Get("X-Ingest-Token"))
		}
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := <-exit; code != 0 {
		t.Errorf("exit %d", code)
	}
	<-ended
	if !strings.Contains(log.String(), `"level":"ERROR","msg":"configuration finding","severity":"error","rule":"USE-11","code":"invalid_value","resource":"settings","field":"`+invalid+`"`) {
		t.Errorf("no error finding at the start; log %s", &log)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "stargaite.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if got := dump(t, db, "SELECT action, resource_type, resource_name, result, revision FROM audit WHERE action = 'secrets reload' ORDER BY rowid"); !slices.Equal(got,
		slices.Repeat([]string{"secrets reload|secrets||unchanged|<nil>"}, 3)) {
		t.Errorf("audit rows %q", got)
	}
	var revision int64
	if err := db.QueryRow("SELECT MAX(revision) FROM revisions").Scan(&revision); err != nil || revision != ready.Revision {
		t.Errorf("revision %d after the reloads, want %d: %v", revision, ready.Revision, err)
	}
	everything := log.String() + answers.String() + strings.Join(dump(t, db, "SELECT * FROM audit"), "\n")
	for _, v := range []string{planted, oldToken, newToken, "PLANTED"} {
		if strings.Contains(everything, v) {
			t.Errorf("%q is in the log, an answer or an audit row", v)
		}
	}
}

// TestSecretsReloadAlongsideStreams is RES-7 for secrets reload, under
// -race: three streams are opened one after another, each after a new env
// file and a reload, while reloads run without pause; each stream reaches the
// mock with the credential of the snapshot it started on and ends whole.
func TestSecretsReloadAlongsideStreams(t *testing.T) {
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	dir, token := seed(t, map[string]string{"m": srv.URL})
	change(t, dir, func(s *config.State) {
		p := s.Doc.Providers["m"]
		p.APIKeyEnv = "M_KEY"
		s.Doc.Providers["m"] = p
	})
	const streams = 3
	var chunks [][]byte
	var want strings.Builder
	for _, c := range []string{"a", "b", "c", "d"} {
		chunks = append(chunks, []byte(`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"`+c+`"},"finish_reason":null}]}`+"\n\n"))
	}
	chunks = append(chunks, []byte("data: [DONE]\n\n"))
	for _, c := range chunks {
		want.Write(c)
	}
	for range streams {
		mock.Script(mockprovider.Step{Chunks: chunks, Gap: 100 * time.Millisecond})
	}
	envFile := filepath.Join(t.TempDir(), "env")
	renameInto(t, envFile, "M_KEY=credential-0\n")
	ready, stop := start(t, env(map[string]string{"STARGAITE_ADMIN_KEY": adminKey}), "--data-dir", dir, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0", "--env-file", envFile)

	halt, reloaded := make(chan struct{}), make(chan int, 1)
	reloads := sync.OnceValue(func() int { close(halt); return <-reloaded })
	defer reloads() // no reload outlives the test, a failed one included
	go func() {
		n := 0
		defer func() { reloaded <- n }()
		for {
			select {
			case <-halt:
				return
			default:
			}
			if code, stdout, stderr := reloadSecrets(ready.AdminAddr, "--json"); code != 0 {
				t.Errorf("reload alongside the streams: exit %d, %s %s", code, stdout, stderr)
				return
			}
			n++
		}
	}()
	var open []*bufio.Reader
	for i := range streams {
		renameInto(t, envFile, fmt.Sprintf("M_KEY=credential-%d\n", i))
		if code, stdout, stderr := reloadSecrets(ready.AdminAddr); code != 0 {
			t.Fatalf("reload %d: exit %d, %s %s", i, code, stdout, stderr)
		}
		req, err := http.NewRequest(http.MethodPost, "http://"+ready.DataAddr+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		r := bufio.NewReader(resp.Body)
		if line, err := r.ReadString('\n'); resp.StatusCode != http.StatusOK || err != nil || line != string(chunks[0][:len(chunks[0])-1]) {
			t.Fatalf("stream %d: status %d, first line %q: %v", i, resp.StatusCode, line, err)
		}
		open = append(open, r)
	}
	for i, r := range open {
		rest, err := io.ReadAll(r)
		if got := string(chunks[0][:len(chunks[0])-1]) + string(rest); err != nil || got != want.String() {
			t.Errorf("stream %d: %q: %v", i, got, err)
		}
	}
	if n := reloads(); n == 0 {
		t.Error("no reload ran alongside the streams")
	}
	calls := mock.Calls()
	if len(calls) != streams {
		t.Fatalf("%d provider calls", len(calls))
	}
	for i, c := range calls {
		if got := c.Header.Get("Authorization"); got != fmt.Sprintf("Bearer credential-%d", i) || c.Cancelled {
			t.Errorf("stream %d: Authorization %q, cancelled %v", i, got, c.Cancelled)
		}
	}
	if code, log := stop(); code != 0 || strings.Contains(log, "credential-") {
		t.Errorf("exit %d; log %s", code, log)
	}
}
