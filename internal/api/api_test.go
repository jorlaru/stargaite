package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/admin"
	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/store"
	"github.com/jorlaru/stargaite/internal/transport"
)

const (
	adminKey = "test-admin-key-of-at-least-32-characters"
	testNow  = 1_790_000_000_000
	planted  = "sk-PLANTED-credential-of-"
)

// logBuffer is a log destination that concurrent requests may share.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// fixture is both listeners on one configuration service, with a clock the
// test moves: no test waits for a grace period.
type fixture struct {
	admin, data http.Handler
	now         *atomic.Int64 // Unix milliseconds
	log         *logBuffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	env := func() config.Env {
		return config.Env{
			// A reference starting with MISSING does not resolve; any other
			// resolves to a planted value that must never leave the proxy.
			Lookup: func(name string) (string, bool) { return planted + name, !strings.HasPrefix(name, "MISSING") },
			Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
				if host == "localhost" {
					return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
				}
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			},
		}
	}
	now := &atomic.Int64{}
	now.Store(testNow)
	cfg, err := config.Open(context.Background(), st, env, func() time.Time { return time.UnixMilli(now.Load()) })
	if err != nil {
		t.Fatal(err)
	}
	log := &logBuffer{}
	noDNS := func(context.Context, string) ([]netip.Addr, error) {
		return nil, errors.New("no name is resolved in tests")
	}
	return &fixture{admin.Handler(cfg, sha256.Sum256([]byte(adminKey)), slog.New(slog.NewJSONHandler(log, nil))),
		Handler(Deps{Config: cfg, Log: slog.New(slog.NewJSONHandler(log, nil)),
			Clients: [2]*http.Client{transport.NewClient(false, noDNS), transport.NewClient(true, noDNS)}}), now, log}
}

// call sends one request to h with the Authorization header given, if any.
func call(h http.Handler, authorization, method, target string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// issue runs an admin call that issues a token and returns it.
func (f *fixture) issue(t *testing.T, method, target, body, name string) string {
	t.Helper()
	w := call(f.admin, "Bearer "+adminKey, method, target, []byte(body))
	var res config.Result
	if w.Code/100 != 2 || json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Secrets[name] == "" {
		t.Fatalf("%s %s: status %d: %s", method, target, w.Code, w.Body)
	}
	return res.Secrets[name]
}

// models lists the aliases token reaches, or returns nil on a 401.
func (f *fixture) models(t *testing.T, token string) []string {
	t.Helper()
	w := call(f.data, "Bearer "+token, http.MethodGet, "/v1/models", nil)
	if w.Header().Get(domain.RequestIDHeader) == "" || w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", w.Header())
	}
	// PAR-9: the trace id on every response to an authenticated request.
	if trace := w.Header().Get(traceHeader); (w.Code == http.StatusOK) != (len(trace) == 32) {
		t.Errorf("status %d with trace id %q", w.Code, trace)
	}
	if w.Code == http.StatusUnauthorized {
		var env struct{ Error domain.Error }
		if json.Unmarshal(w.Body.Bytes(), &env) != nil || env.Error.Code != domain.CodeInvalidAPIKey || env.Error.Type != "authentication_error" {
			t.Errorf("401 body %s", w.Body)
		}
		if len(token) >= 20 && strings.Contains(w.Body.String(), token[len(token)-20:]) {
			t.Error("the 401 body echoes the token")
		}
		return nil
	}
	var list struct {
		Object string
		Data   []struct {
			ID, Object string
			Created    *int64
			OwnedBy    string `json:"owned_by"`
		}
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil || list.Object != "list" || list.Data == nil {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	ids := []string{}
	for _, m := range list.Data {
		if m.Object != "model" || m.Created == nil || m.OwnedBy == "" {
			t.Errorf("entry %+v", m)
		}
		ids = append(ids, m.ID)
	}
	return ids
}

func TestHealth(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/health/live", http.StatusOK},
		{http.MethodGet, "/health/ready", http.StatusOK},
		{http.MethodPost, "/health/live", http.StatusMethodNotAllowed},
		{http.MethodPost, "/health/ready", http.StatusMethodNotAllowed},
		{http.MethodGet, "/health/other", http.StatusNotFound},
	} {
		rec := call(f.data, "", tc.method, tc.path, nil)
		if rec.Code != tc.status || rec.Header().Get(domain.RequestIDHeader) == "" {
			t.Errorf("%s %s: status %d, want %d; headers %v", tc.method, tc.path, rec.Code, tc.status, rec.Header())
		}
		if tc.status != http.StatusOK {
			continue
		}
		var body struct{ Status string }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Status != "ok" {
			t.Errorf("body %q: status %q, error %v", rec.Body, body.Status, err)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type %q", got)
		}
	}
}

// TestModels (AC-06, SEC-2): a key lists only its allowed aliases that
// exist, a key with none lists nothing, ["*"] lists every alias; the admin
// key and anything but a client key get 401, and a client key gets 401 on
// the admin listener. The request line names the key, never the token.
func TestModels(t *testing.T) {
	f := newFixture(t)
	example, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	w := call(f.admin, "Bearer "+adminKey, http.MethodPost, "/admin/v1/apply", example)
	var applied config.Result
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &applied) != nil {
		t.Fatalf("apply: %d %s", w.Code, w.Body)
	}
	none := f.issue(t, http.MethodPost, "/admin/v1/keys", `{"name":"none","models":[]}`, "none")
	all := f.issue(t, http.MethodPost, "/admin/v1/keys", `{"name":"all","models":["*"]}`, "all")
	ghost := f.issue(t, http.MethodPost, "/admin/v1/keys", `{"name":"ghost","models":["fast","gone"]}`, "ghost")
	for _, tc := range []struct {
		token string
		want  []string
	}{
		{applied.Secrets["invoice-agent"], []string{"deep", "fast"}},
		{applied.Secrets["classifier-flow"], []string{"embed", "fast", "rerank"}},
		{none, []string{}},
		{all, []string{"deep", "embed", "fast", "rerank"}},
		{ghost, []string{"fast"}},
	} {
		if got := f.models(t, tc.token); !slices.Equal(got, tc.want) {
			t.Errorf("lists %q, want %q", got, tc.want)
		}
	}
	if f.models(t, adminKey) != nil {
		t.Error("the admin key is accepted on the data plane")
	}
	for _, authorization := range []string{"", "Bearer", "Bearer " + adminKey, "Basic " + all, "Bearer  " + all, all} {
		if w := call(f.data, authorization, http.MethodGet, "/v1/models", nil); w.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status %d", authorization, w.Code)
		}
	}
	// The scheme is case-insensitive (RFC 9110).
	if w := call(f.data, "bearer "+all, http.MethodGet, "/v1/models", nil); w.Code != http.StatusOK {
		t.Errorf("lower-case scheme: status %d", w.Code)
	}
	if w := call(f.admin, "Bearer "+all, http.MethodGet, "/admin/v1/keys", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a client key on the admin listener: status %d", w.Code)
	}
	if w := call(f.data, "Bearer "+all, http.MethodPost, "/v1/models", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/models: status %d", w.Code)
	}

	log := f.log.String()
	if !strings.Contains(log, `"msg":"request","request_id":`) || !strings.Contains(log, `"status":200,"key":"all"`) || !strings.Contains(log, `"status":401`) {
		t.Errorf("log %s", log)
	}
	for _, token := range []string{none, all, ghost, adminKey, applied.Secrets["invoice-agent"]} {
		if strings.Contains(log, token[len(token)-20:]) {
			t.Error("the log holds a token")
		}
	}
}

// TestRefusalLogRateLimited (OPS-9): a flood of wrong tokens writes one
// line per 10 s with the count of failures, never the token, and no
// per-request line.
func TestRefusalLogRateLimited(t *testing.T) {
	f := newFixture(t)
	token := f.issue(t, http.MethodPost, "/admin/v1/keys", `{"name":"agent","models":["*"]}`, "agent")
	call(f.admin, "Bearer "+adminKey, http.MethodPost, "/admin/v1/keys/agent/revoke", nil)
	for range 1000 {
		if f.models(t, token) != nil {
			t.Fatal("a revoked key is accepted")
		}
	}
	f.now.Add((10 * time.Second).Milliseconds())
	f.models(t, token)
	log := f.log.String()
	if n := strings.Count(log, `"msg":"authentication failed"`); n != 2 || !strings.Contains(log, `"failures":1000`) ||
		strings.Contains(log, `"msg":"request"`) || strings.Contains(log, token[len(token)-20:]) {
		t.Errorf("%d lines, log %s", n, log)
	}
}

// TestRotationGrace (AC-07, SEC-8): the previous secret stays valid for the
// grace, 1h by default, and not an instant longer; a rotation during a grace
// invalidates the oldest secret at once, so a key never has two old ones.
func TestRotationGrace(t *testing.T) {
	f := newFixture(t)
	first := f.issue(t, http.MethodPost, "/admin/v1/keys", `{"name":"agent","models":["*"]}`, "agent")
	second := f.issue(t, http.MethodPost, "/admin/v1/keys/agent/rotate", ``, "agent")
	valid := func(token string, after time.Duration) bool {
		t.Helper()
		f.now.Store(testNow + after.Milliseconds())
		return f.models(t, token) != nil
	}
	if !valid(first, time.Hour-time.Millisecond) || valid(first, time.Hour) || !valid(second, 1000*time.Hour) {
		t.Error("the default grace is not 1h")
	}

	f.now.Store(testNow + 2*time.Hour.Milliseconds())
	third := f.issue(t, http.MethodPost, "/admin/v1/keys/agent/rotate", `{"grace":"30m"}`, "agent")
	f.now.Store(testNow + (2*time.Hour + time.Minute).Milliseconds())
	fourth := f.issue(t, http.MethodPost, "/admin/v1/keys/agent/rotate", `{"grace":"10m"}`, "agent")
	at := 2*time.Hour + time.Minute
	if valid(second, at) || !valid(third, at+10*time.Minute-time.Millisecond) || valid(third, at+10*time.Minute) || !valid(fourth, at) {
		t.Error("a rotation during a grace")
	}
	var shown struct {
		Secrets []config.SecretView
	}
	w := call(f.admin, "Bearer "+adminKey, http.MethodGet, "/admin/v1/keys/agent", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &shown); err != nil || len(shown.Secrets) != 2 {
		t.Errorf("secrets %s", w.Body)
	}
}

// TestRevokeWithinOneSecond (AC-07): a client that keeps calling with a key
// is refused within 1 s of the revocation, and a request sent after the
// revoke answered is refused, whatever the timing.
func TestRevokeWithinOneSecond(t *testing.T) {
	f := newFixture(t)
	token := f.issue(t, http.MethodPost, "/admin/v1/keys", `{"name":"agent","models":["*"]}`, "agent")
	srv := httptest.NewServer(f.data)
	t.Cleanup(srv.Close)

	started, refused := make(chan struct{}), make(chan time.Time, 1)
	go func() {
		defer close(refused)
		deadline := time.Now().Add(10 * time.Second)
		for once := false; time.Now().Before(deadline); {
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
			if err != nil {
				return
			}
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := srv.Client().Do(req)
			if err != nil {
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				refused <- time.Now()
				return
			}
			if !once {
				once = true
				close(started)
			}
		}
	}()
	select { // a client that fails before its first answer closes refused
	case <-started:
	case <-refused:
	}
	w := call(f.admin, "Bearer "+adminKey, http.MethodPost, "/admin/v1/keys/agent/revoke", nil)
	revoked := time.Now()
	if w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if f.models(t, token) != nil {
		t.Error("a request after the revocation is accepted")
	}
	at, ok := <-refused
	if !ok || at.Sub(revoked) > time.Second {
		t.Errorf("the concurrent client was refused %v after the revocation (ok %v)", at.Sub(revoked), ok)
	}
}
