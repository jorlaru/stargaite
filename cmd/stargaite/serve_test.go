package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/cli"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// TestServe drives serve as the process does: it reads the address from the
// first log line, calls the listener, and stops it with a signal.
func TestServe(t *testing.T) {
	logs, stderr := io.Pipe()
	exit := make(chan int, 1)
	go func() {
		exit <- serve(nil, "127.0.0.1:0", io.Discard, stderr)
		_ = stderr.Close()
	}()

	lines := bufio.NewScanner(logs)
	if !lines.Scan() {
		t.Fatal("no log line")
	}
	var first struct{ Msg, Listener, Addr, Version string }
	if err := json.Unmarshal(lines.Bytes(), &first); err != nil {
		t.Fatalf("log line %q: %v", lines.Text(), err)
	}
	if first.Msg != "listening" || first.Listener != "data" || first.Version != cli.Version {
		t.Fatalf("log line %+v", first)
	}
	rest := make(chan string, 1)
	go func() {
		var b strings.Builder
		for lines.Scan() {
			b.WriteString(lines.Text() + "\n")
		}
		rest <- b.String()
	}()

	client := &http.Client{Timeout: 5 * time.Second}
	get := func(header string) (int, error) {
		req, err := http.NewRequest(http.MethodGet, "http://"+first.Addr+"/health/live", nil)
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
	if code, err := get(""); err != nil || code != http.StatusOK {
		t.Errorf("health probe: status %d, error %v", code, err)
	}
	if code, err := get(strings.Repeat("x", 128<<10)); err != nil || code != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("oversized header: status %d, error %v", code, err)
	}

	// serve listens for the signal before it logs the first line.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-exit:
		if code != 0 {
			t.Errorf("exit %d after SIGTERM", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	if got := <-rest; !strings.Contains(got, `"msg":"stopped"`) {
		t.Errorf("no stop line in the log: %q", got)
	}
	if _, err := get(""); err == nil {
		t.Error("the listener still answers after the stop")
	}
}

func TestRunServerReportsAFailedListener(t *testing.T) {
	ln := listen(t)
	_ = ln.Close()
	if err := runServer(context.Background(), ln, slog.New(slog.NewJSONHandler(io.Discard, nil))); err == nil {
		t.Error("no error from a listener that cannot accept")
	}
}

func TestServeExitCodes(t *testing.T) {
	taken := listen(t)
	defer func() { _ = taken.Close() }()

	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"help", []string{"--help"}, 0},
		{"unknown flag", []string{"--nope"}, 2},
		{"unexpected argument", []string{"extra"}, 2},
		{"address in use", []string{"--json"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := serve(tc.args, taken.Addr().String(), &stdout, &stderr); code != tc.code {
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
				if err := json.Unmarshal(stderr.Bytes(), &line); err != nil || line.Level != "ERROR" || line.Msg != "listen failed" {
					t.Errorf("log line %q: %+v, error %v", &stderr, line, err)
				}
			}
		})
	}
}

func TestDefaultAddressIsLoopback(t *testing.T) {
	host, _, err := net.SplitHostPort(dataAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		t.Errorf("default address %q: host %q, error %v", dataAddr, host, err)
	}
}

func TestRunDispatch(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, func(string) string { return "" }, &stdout, &stderr); code != 0 || stdout.String() != cli.Version+"\n" {
		t.Errorf("version: exit %d stdout %q stderr %q", code, &stdout, &stderr)
	}
	// Only serve answers its own help with 0: the CLI has no such command.
	if code := run([]string{"serve", "--help"}, nil, &stdout, &stderr); code != 0 {
		t.Errorf("serve --help: exit %d", code)
	}
}
