package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// TestVerifyUntilStdinCloses: with --until-stdin-closes, closing standard
// input ends targets verify's request, and the server, against the mock in
// process, cancels the call in flight and starts none of the run's other two.
// The CLI answers admin_unreachable, exit 1, one JSON value. Until the close,
// the run goes on (the control).
func TestVerifyUntilStdinCloses(t *testing.T) {
	vars := adminServer(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	for _, args := range [][]string{
		{"providers", "add", "mock", "--protocol", "openai", "--base-url", srv.URL, "--allow-private-network", "--billing", "none"},
		{"targets", "add", "mock/m", "--kind", "chat", "--level", "low", "--level", "high", "--can-disable"},
	} {
		if r := runMain(vars, args...); r.code != 0 {
			t.Fatalf("%q: exit %d, stderr %q", args, r.code, r.stderr)
		}
	}
	mock.Script(mockprovider.Step{Hang: true})

	pr, pw := io.Pipe()
	var out, errOut bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Main([]string{"targets", "verify", "mock/m", "--until-stdin-closes", "--json"}, getenv(vars), pr, &out, &errOut)
	}()
	t.Cleanup(func() { _ = pw.Close() })
	for deadline := time.Now().Add(5 * time.Second); len(mock.Calls()) != 1; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the run never reached the mock")
		}
	}
	select {
	case code := <-done:
		t.Fatalf("Main returned %d before standard input closed; stderr %q", code, errOut.String())
	case <-time.After(300 * time.Millisecond):
	}

	_ = pw.Close()
	select {
	case code := <-done:
		if code != exitFailure {
			t.Errorf("exit %d, want 1; stderr %q", code, errOut.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Main did not return within 5 s of the close")
	}
	var answer struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if !oneJSONValue(out.String()) || json.Unmarshal(out.Bytes(), &answer) != nil || answer.Error.Code != "admin_unreachable" {
		t.Errorf("stdout %q, want one JSON value with error.code admin_unreachable", out.String())
	}
	for deadline := time.Now().Add(5 * time.Second); !mock.Calls()[0].Cancelled; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the call in flight was not cancelled at the mock")
		}
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(mock.Calls()); n != 1 {
		t.Errorf("%d mock calls after the cancel, want 1: the server went on", n)
	}
}

// TestUntilStdinClosesNeedsPipe: on a terminal, --until-stdin-closes is a
// usage error, before any request and without reading standard input.
func TestUntilStdinClosesNeedsPipe(t *testing.T) {
	got, vars := recorder(t, http.StatusOK, "{}\n")
	r := withStdin(vars, terminal{}, "targets", "verify", "p/m", "--until-stdin-closes")
	want := "stargaite targets verify: --until-stdin-closes needs standard input to be a pipe that stays open\n"
	if r.code != exitUsage || r.stderr != want || r.stdout != "" || *got != (recorded{}) {
		t.Errorf("exit %d, stderr %q, stdout %q, request %+v", r.code, r.stderr, r.stdout, *got)
	}
}
