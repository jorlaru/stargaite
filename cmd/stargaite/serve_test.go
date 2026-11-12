package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/cli"
)

func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

type readyLine struct {
	Msg, Version string
	DataDir      string `json:"data_dir"`
	DataAddr     string `json:"data_addr"`
	AdminAddr    string `json:"admin_addr"`
	TLS          bool
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
	if !lines.Scan() {
		t.Fatal("no log line")
	}
	if err := json.Unmarshal(lines.Bytes(), &ready); err != nil || ready.Msg != "ready" {
		t.Fatalf("first log line %q: %+v, error %v", lines.Text(), ready, err)
	}
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
		{"http://" + ready.AdminAddr + "/admin/v1/settings", "", http.StatusNotFound}, // bound, no route yet
	} {
		if code, err := get(t, client, tc.url, tc.header); err != nil || code != tc.status {
			t.Errorf("GET %s: status %d, error %v; want %d", tc.url, code, err, tc.status)
		}
	}
	for _, name := range []string{"lock", "stargaite.db"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("data directory: %v", err)
		}
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
		"https://" + ready.AdminAddr + "/admin/v1/settings": http.StatusNotFound,
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
		{name: "address in use", args: []string{"--json", "--listen", taken.Addr().String(), "--admin-listen", "127.0.0.1:0"}, code: 1, msg: "server failed", created: true},
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
