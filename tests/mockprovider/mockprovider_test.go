package mockprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

func send(t *testing.T, srv *httptest.Server, method, path string, header http.Header, body []byte) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(req.Header, header)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, got
}

// The exact bytes received are recorded: spacing, key order, a repeated key,
// escapes and a byte that is not UTF-8 all survive.
func TestRecordsExactBytes(t *testing.T) {
	m := New()
	srv := httptest.NewServer(m)
	defer srv.Close()
	body := []byte("{ \"model\":\"m\",\"model\" : \"n\",\n\t\"x\":\"\\u00e9\xff\" }")
	send(t, srv, http.MethodPost, "/chat/completions", http.Header{"Authorization": {"Bearer token"}, "X-Extra": {"a", "b"}}, body)
	calls := m.Calls()
	if len(calls) != 1 {
		t.Fatalf("%d calls", len(calls))
	}
	c := calls[0]
	if c.Method != http.MethodPost || c.Path != "/chat/completions" || !bytes.Equal(c.Body, body) {
		t.Errorf("call %s %s %q", c.Method, c.Path, c.Body)
	}
	if c.Header.Get("Authorization") != "Bearer token" || !slices.Equal(c.Header["X-Extra"], []string{"a", "b"}) {
		t.Errorf("header %v", c.Header)
	}
}

// An empty queue answers each route with well-formed JSON.
func TestDefaults(t *testing.T) {
	srv := httptest.NewServer(New())
	defer srv.Close()
	for _, path := range []string{"/chat/completions", "/embeddings", "/v1/messages"} {
		status, header, body := send(t, srv, http.MethodPost, path, nil, []byte(`{}`))
		if status != http.StatusOK || header.Get("Content-Type") != "application/json" || !json.Valid(body) {
			t.Errorf("%s: %d %q %s", path, status, header.Get("Content-Type"), body)
		}
	}
}

// Scripted replies are sent in order, exactly, and then the default again.
// Another route or method does not take a scripted reply.
func TestScript(t *testing.T) {
	m := New()
	srv := httptest.NewServer(m)
	defer srv.Close()
	m.Script(Step{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"1"}}, Body: []byte(`{"error":"slow down"}`)})
	m.Script(Step{Body: []byte("not json")})
	if status, _, _ := send(t, srv, http.MethodPost, "/unknown", nil, nil); status != http.StatusNotFound {
		t.Errorf("unknown route: %d", status)
	}
	if status, header, _ := send(t, srv, http.MethodGet, "/embeddings", nil, nil); status != http.StatusMethodNotAllowed || header.Get("Allow") != http.MethodPost {
		t.Errorf("GET: %d %v", status, header)
	}
	status, header, body := send(t, srv, http.MethodPost, "/embeddings", nil, nil)
	if status != http.StatusTooManyRequests || header.Get("Retry-After") != "1" || string(body) != `{"error":"slow down"}` {
		t.Errorf("first step: %d %v %s", status, header, body)
	}
	if status, _, body := send(t, srv, http.MethodPost, "/v1/messages", nil, nil); status != http.StatusOK || string(body) != "not json" {
		t.Errorf("second step: %d %s", status, body)
	}
	if status, _, body := send(t, srv, http.MethodPost, "/v1/messages", nil, nil); status != http.StatusOK || !json.Valid(body) {
		t.Errorf("default: %d %s", status, body)
	}
	if n := len(m.Calls()); n != 5 {
		t.Errorf("%d calls recorded, want 5", n)
	}
}

// An empty queue answers "stream": true with the route's default stream:
// chat chunks with a usage chunk and [DONE], and the Messages events.
func TestDefaultStreams(t *testing.T) {
	srv := httptest.NewServer(New())
	defer srv.Close()
	for path, last := range map[string]string{"/chat/completions": "data: [DONE]\n\n", "/v1/messages": "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"} {
		status, header, body := send(t, srv, http.MethodPost, path, nil, []byte(`{"stream":true}`))
		if status != http.StatusOK || header.Get("Content-Type") != "text/event-stream" || !bytes.HasSuffix(body, []byte(last)) {
			t.Errorf("%s: %d %q %s", path, status, header.Get("Content-Type"), body)
		}
		for event := range bytes.SplitSeq(bytes.TrimSuffix(body, []byte("\n\n")), []byte("\n\n")) {
			_, data, _ := bytes.Cut(event, []byte("data: "))
			if !json.Valid(data) && string(data) != "[DONE]" {
				t.Errorf("%s: event %q", path, event)
			}
		}
	}
	if status, header, _ := send(t, srv, http.MethodPost, "/chat/completions", nil, []byte(`{"stream":false}`)); status != http.StatusOK || header.Get("Content-Type") != "application/json" {
		t.Errorf("stream false: %d %v", status, header)
	}
	if status, header, _ := send(t, srv, http.MethodPost, "/embeddings", nil, []byte(`{"stream":true}`)); status != http.StatusOK || header.Get("Content-Type") != "application/json" {
		t.Errorf("embeddings: %d %v", status, header)
	}
}

// Chunks are flushed one by one with Gap between them, after Delay; Hang
// holds the reply until the request ends; Cut aborts it.
func TestStreamSteps(t *testing.T) {
	m := New()
	srv := httptest.NewServer(m)
	defer srv.Close()
	m.Script(Step{Chunks: [][]byte{[]byte("data: 1\n\n"), []byte("data: 2\n\n")}, Delay: 50 * time.Millisecond, Gap: 100 * time.Millisecond})
	start := time.Now()
	resp, err := srv.Client().Post(srv.URL+"/chat/completions", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	first := time.Since(start)
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil || string(buf[:n]) != "data: 1\n\n" || resp.Header.Get("Content-Type") != "text/event-stream" || first < 50*time.Millisecond {
		t.Errorf("first chunk %q after %v: %v", buf[:n], first, err)
	}
	rest, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(rest) != "data: 2\n\n" || time.Since(start) < 150*time.Millisecond {
		t.Errorf("second chunk %q after %v: %v", rest, time.Since(start), err)
	}

	m.Script(Step{Status: http.StatusTeapot, Header: http.Header{"Content-Type": {"text/plain"}}, Chunks: [][]byte{[]byte("x")}, Cut: true})
	resp, err = srv.Client().Post(srv.URL+"/chat/completions", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot || resp.Header.Get("Content-Type") != "text/plain" || string(body) != "x" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("cut: %d %q %v", resp.StatusCode, body, err)
	}

	m.Script(Step{Body: []byte("held"), Hang: true})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/chat/completions", nil)
	resp, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	n, err = resp.Body.Read(buf)
	if string(buf[:n]) != "held" {
		t.Errorf("hang: %q %v", buf[:n], err)
	}
	cancel()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Error("the held reply ended without an error")
	}
	_ = resp.Body.Close()
}
