package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/auth"
	"github.com/jorlaru/stargaite/internal/cli"
	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/store"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// adminKey is an admin key given through STARGAITE_ADMIN_KEY.
const adminKey = "test-admin-key-of-at-least-32-characters"

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
	if code := cli.Main([]string{"settings", "show", "--json", "--admin-url", "http://" + ready.AdminAddr}, env(map[string]string{"STARGAITE_DATA_DIR": dir}), &stdout, io.Discard); code != 0 || !json.Valid(stdout.Bytes()) {
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
	st, err := store.Open(context.Background(), dir)
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
	if st, err = store.Open(context.Background(), dir); err != nil {
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
	if st, err = store.Open(context.Background(), dir); err != nil {
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
	st, err := store.Open(context.Background(), dir)
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
// that last one included, and serve exits 0 with the stop line.
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
				var line struct{ Level, Msg string }
				if err := json.Unmarshal(stderr.Bytes(), &line); err != nil || line.Level != "ERROR" || line.Msg != tc.msg {
					t.Errorf("log line %q: %+v, error %v", &stderr, line, err)
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
	if code := run([]string{"version"}, env(nil), &stdout, &stderr); code != 0 || stdout.String() != cli.Version+"\n" {
		t.Errorf("version: exit %d stdout %q stderr %q", code, &stdout, &stderr)
	}
	// Only serve answers its own help with 0: the CLI has no such command.
	if code := run([]string{"serve", "--help"}, env(nil), &stdout, &stderr); code != 0 {
		t.Errorf("serve --help: exit %d", code)
	}
}
