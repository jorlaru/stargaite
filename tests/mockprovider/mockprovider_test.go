package mockprovider

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
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
