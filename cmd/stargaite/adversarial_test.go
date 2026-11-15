package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// TestServeAdversarial is the part of the AC-51 suite that depends on the
// server options of serve (phase-4 design section 9), on raw sockets with
// serverReadHeaderTimeout lowered to 200 ms (SEC-6 as amended, section 15
// point 26; LIM-6; RES-5):
//
//   - Content-Length with Transfer-Encoding: chunked: the body is read as
//     chunked, as net/http chooses, the provider gets the proxy's own body,
//     and a second request pipelined on the connection is parsed as its own
//     (no desync); net/http keeps the connection open;
//   - two differing Content-Length values: net/http answers 400 and closes;
//   - a bare LF after a chunk size: net/http hands the handler a body read
//     error, so the proxy answers 400 invalid_request and net/http closes the
//     connection, the pipelined request unanswered;
//   - over 68 KiB of headers and 501 header values: 431 on both listeners,
//     while 60 KiB and 500 values pass;
//   - slow headers: closed within serverReadHeaderTimeout + 1 s on both
//     listeners, with no answer;
//   - a stream reader that stops reading, then a client that disconnects:
//     each ends as client_gone with its slot released (the key has one) and
//     the provider call cancelled within 1 s.
func TestServeAdversarial(t *testing.T) {
	serverReadHeaderTimeout = 200 * time.Millisecond
	t.Cleanup(func() { serverReadHeaderTimeout = 10 * time.Second })
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	dir, token := seed(t, map[string]string{"m": srv.URL})
	ready, stop := start(t, env(map[string]string{"STARGAITE_ADMIN_KEY": adminKey}), "--data-dir", dir, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0")
	for path, body := range map[string]string{"/admin/v1/keys/agent": `{"max_parallel_requests":1}`, "/admin/v1/settings": `{"timeouts":{"stream_idle_ms":500}}`} {
		req, err := http.NewRequest(http.MethodPatch, "http://"+ready.AdminAddr+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+adminKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("PATCH %s: %v %v", path, resp, err)
		}
		_ = resp.Body.Close()
	}

	dial := func(addr string) (net.Conn, *bufio.Reader) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second)) // a failing run ends
		return conn, bufio.NewReader(conn)
	}
	send := func(conn net.Conn, raw string) {
		if _, err := io.WriteString(conn, raw); err != nil {
			t.Fatal(err)
		}
	}
	// read reads one answer: its status, whether it announced the close
	// of the connection, and its body.
	read := func(r *bufio.Reader) (int, bool, string) {
		t.Helper()
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Fatalf("no answer: %v", err)
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Close, string(b)
	}
	// closed reports whether the server closed the connection after its
	// answer: nothing more arrives, not even an answer to a pipelined request.
	closed := func(conn net.Conn, r *bufio.Reader) bool {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err := r.ReadByte()
		return err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
	}
	const chat = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	auth := "Host: x\r\nAuthorization: Bearer " + token + "\r\n"
	models := "GET /v1/models HTTP/1.1\r\n" + auth + "\r\n"

	// Content-Length with Transfer-Encoding: chunked. Read by Content-Length,
	// the body would end inside the chunk and the rest be read as a request.
	conn, r := dial(ready.DataAddr)
	send(conn, "POST /v1/chat/completions HTTP/1.1\r\n"+auth+"Content-Length: 5\r\nTransfer-Encoding: chunked\r\n\r\n"+
		fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(chat), chat)+models)
	if status, closing, body := read(r); status != http.StatusOK || closing || !strings.Contains(body, `"content":"mock reply"`) {
		t.Errorf("Content-Length with chunked: %d, close %v, %s", status, closing, body)
	}
	if status, _, body := read(r); status != http.StatusOK || body != `{"object":"list","data":[{"id":"m","object":"model","created":0,"owned_by":"stargaite"}]}`+"\n" {
		t.Errorf("the pipelined request: %d %s", status, body)
	}
	if c := mock.Calls(); len(c) != 1 || strings.Contains(string(c[0].Body), "\r\n") || !strings.HasPrefix(string(c[0].Body), `{"model":"m","messages":[`) ||
		c[0].Header.Get("Transfer-Encoding") != "" {
		t.Errorf("the provider got %+v", c)
	}

	// Two differing Content-Length values.
	conn, r = dial(ready.DataAddr)
	send(conn, "POST /v1/chat/completions HTTP/1.1\r\n"+auth+"Content-Length: 3\r\nContent-Length: 4\r\n\r\nabcd"+models)
	if status, closing, body := read(r); status != http.StatusBadRequest || !closing || body != "400 Bad Request" || !closed(conn, r) {
		t.Errorf("two Content-Length values: %d, close %v, %q", status, closing, body)
	}

	// A bare LF after a chunk size.
	conn, r = dial(ready.DataAddr)
	send(conn, "POST /v1/chat/completions HTTP/1.1\r\n"+auth+"Transfer-Encoding: chunked\r\n\r\n"+
		fmt.Sprintf("%x\n%s\r\n0\r\n\r\n", len(chat), chat)+models)
	if status, closing, body := read(r); status != http.StatusBadRequest || !closing ||
		body != `{"error":{"message":"the request body could not be read","type":"invalid_request_error","code":"invalid_request"}}`+"\n" || !closed(conn, r) {
		t.Errorf("a bare LF in a chunk size: %d, close %v, %q", status, closing, body)
	}
	if n := len(mock.Calls()); n != 1 {
		t.Errorf("%d provider calls after the framing cases", n)
	}

	// Header size and count, on both listeners.
	values := func(n int) string {
		var b strings.Builder
		for i := range n - 1 { // and Host
			fmt.Fprintf(&b, "X-V%d: v\r\n", i)
		}
		return b.String()
	}
	for _, l := range []struct {
		addr, path string
		status     int
	}{{ready.DataAddr, "/health/live", http.StatusOK}, {ready.AdminAddr, "/admin/v1/settings", http.StatusUnauthorized}} {
		for _, tc := range []struct {
			name, headers string
			status        int
		}{
			{"60 KiB", "X-Pad: " + strings.Repeat("x", 60<<10) + "\r\n", l.status},
			{"69 KiB", "X-Pad: " + strings.Repeat("x", 69<<10) + "\r\n", http.StatusRequestHeaderFieldsTooLarge},
			{"500 values", values(500), l.status},
			{"501 values", values(501), http.StatusRequestHeaderFieldsTooLarge},
		} {
			conn, r := dial(l.addr)
			send(conn, "GET "+l.path+" HTTP/1.1\r\nHost: x\r\n"+tc.headers+"\r\n")
			if status, _, body := read(r); status != tc.status {
				t.Errorf("%s %s: %d %q", l.addr, tc.name, status, body)
			}
		}
	}

	// Slow headers, on both listeners.
	for _, addr := range []string{ready.DataAddr, ready.AdminAddr} {
		conn, r := dial(addr)
		began := time.Now()
		send(conn, "GET /health/live HTTP/1.1\r\nHost: x\r\n")
		got, err := io.ReadAll(r)
		if took := time.Since(began); err != nil || len(got) != 0 || took > serverReadHeaderTimeout+time.Second || took < serverReadHeaderTimeout/2 {
			t.Errorf("%s slow headers: closed after %v with %q, error %v", addr, took, got, err)
		}
	}

	// held waits until the key's one slot is free, which a synchronous call
	// shows by being served, and returns when it was first seen free.
	held := func(what string) time.Time {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			req, err := http.NewRequest(http.MethodPost, "http://"+ready.DataAddr+"/v1/chat/completions", strings.NewReader(chat))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK:
				return time.Now()
			case http.StatusTooManyRequests:
			default:
				t.Fatalf("%s: %d while the slot is held", what, resp.StatusCode)
			}
		}
		t.Fatalf("%s: the slot was not released", what)
		return time.Time{}
	}
	stream := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`
	big := []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + strings.Repeat("x", 16<<10) + "\"}}]}\n\n")

	// A stream reader that stops reading: the write deadline (stream_idle_ms
	// + 5 s) ends it.
	mock.Script(mockprovider.Step{Chunks: slices.Repeat([][]byte{big}, 2000), Hang: true})
	conn, r = dial(ready.DataAddr)
	_ = conn.(*net.TCPConn).SetReadBuffer(4096)
	send(conn, "POST /v1/chat/completions HTTP/1.1\r\n"+auth+fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(stream), stream))
	if line, err := r.ReadString('\n'); line != "HTTP/1.1 200 OK\r\n" {
		t.Fatalf("the stalled stream: %q %v", line, err)
	}
	free := held("stalled reader")
	c := mock.Calls() // the stalled stream's, then the one that found the slot free
	if call := c[len(c)-2]; !strings.Contains(string(call.Body), `"stream":true`) || !call.Cancelled || call.Ended.IsZero() || call.Ended.After(free.Add(time.Second)) {
		t.Errorf("the stalled stream's provider call: cancelled %v, ended %v after the slot was seen free", call.Cancelled, call.Ended.Sub(free))
	}

	// A client that disconnects after its first event.
	calls := len(mock.Calls())
	mock.Script(mockprovider.Step{Chunks: [][]byte{big}, Hang: true})
	conn, r = dial(ready.DataAddr)
	send(conn, "POST /v1/chat/completions HTTP/1.1\r\n"+auth+fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(stream), stream))
	if resp, err := http.ReadResponse(r, nil); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("the stream: %v", err)
	} else if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	gone := time.Now()
	_ = conn.Close()
	held("gone client")
	if call := mock.Calls()[calls]; !call.Cancelled || call.Ended.IsZero() || call.Ended.Sub(gone) > time.Second {
		t.Errorf("the gone client's provider call: cancelled %v, ended %v after the close", call.Cancelled, call.Ended.Sub(gone))
	}

	code, log := stop()
	var requests []string
	for _, l := range strings.Split(strings.TrimSpace(log), "\n") {
		var line struct {
			Msg, Code string
			Status    int
		}
		if err := json.Unmarshal([]byte(l), &line); err != nil {
			t.Errorf("log line %q: %v", l, err)
		}
		if line.Msg == "request" && line.Status != http.StatusOK {
			requests = append(requests, fmt.Sprintf("%d %s", line.Status, line.Code))
		}
	}
	slices.Sort(requests)
	requests = slices.DeleteFunc(requests, func(s string) bool { return s == "429 parallel_limit_exceeded" })
	if code != 0 || !slices.Equal(requests, []string{"400 invalid_request", "499 client_gone", "499 client_gone"}) {
		t.Errorf("exit %d; requests %q; log %s", code, requests, log)
	}
}
