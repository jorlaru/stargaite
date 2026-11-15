package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
)

// lockedBuffer is a log the server's goroutines write while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestBackupRoute: POST /admin/v1/backup on a server whose read and write
// timeouts are 200 ms streams a copy that takes longer than both to make,
// whole, as application/vnd.sqlite3 with its Content-Length, because the
// route clears its read deadline and sets its write deadline to backupWait
// through the wrapper's recorder; the server logs one line with the size. A
// Backup error is the envelope, its cause in the log only; a query is refused.
func TestBackupRoute(t *testing.T) {
	content := make([]byte, 1<<20)
	_, _ = rand.Read(content)
	file := filepath.Join(t.TempDir(), "copy")
	if err := os.WriteFile(file, content, 0o600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("planted cause")
	var fail atomic.Bool
	logs := &lockedBuffer{}
	h := Handler(Deps{KeyHash: sha256.Sum256([]byte(testKey)), Log: slog.New(slog.NewJSONHandler(logs, nil)),
		Backup: func(ctx context.Context) (*os.File, error) {
			select {
			case <-time.After(400 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if fail.Load() {
				return nil, failure
			}
			return os.Open(file)
		}})
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ReadTimeout, srv.Config.WriteTimeout = 200*time.Millisecond, 200*time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)

	post := func(path string) (*http.Response, []byte, error) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testKey)
		resp, err := srv.Client().Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		return resp, body, err
	}
	began := time.Now()
	resp, body, err := post("/admin/v1/backup")
	if err != nil {
		t.Fatalf("the backup was cut after %v: %v", time.Since(began), err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/vnd.sqlite3" ||
		resp.ContentLength != int64(len(content)) || !bytes.Equal(body, content) || time.Since(began) < 400*time.Millisecond {
		t.Errorf("status %d, Content-Type %q, Content-Length %d, %d bytes after %v", resp.StatusCode, resp.Header.Get("Content-Type"),
			resp.ContentLength, len(body), time.Since(began))
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), `"msg":"admin request"`); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no request line: %s", logs)
		}
	}
	if log := logs.String(); strings.Count(log, `"msg":"backup sent"`) != 1 || !strings.Contains(log, `"bytes":1048576`) {
		t.Errorf("log %s", log)
	}

	fail.Store(true)
	resp, body, err = post("/admin/v1/backup")
	var env struct{ Error domain.Error }
	if err != nil || resp.StatusCode != http.StatusInternalServerError || json.Unmarshal(body, &env) != nil ||
		env.Error.Code != domain.CodeInternalError || strings.Contains(string(body), failure.Error()) {
		t.Errorf("a failed backup: %v %s, error %v", resp, body, err)
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), failure.Error()); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the cause is not in the log: %s", logs)
		}
	}
	if resp, body, err = post("/admin/v1/backup?to=/etc/passwd"); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a query: %v %s, error %v", resp, body, err)
	}
}

// TestBackupRouteStop: a stop (the request context ending, as halt does)
// cuts a backup stream in progress, so the client sees the body fall short
// of its Content-Length, and the server logs the cut.
func TestBackupRouteStop(t *testing.T) {
	// Sparse, 32 MiB: more than the socket buffers hold while the client waits.
	file := filepath.Join(t.TempDir(), "copy")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(file, 32<<20); err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	base, halt := context.WithCancel(context.Background())
	defer halt()
	srv := httptest.NewUnstartedServer(Handler(Deps{KeyHash: sha256.Sum256([]byte(testKey)), Log: slog.New(slog.NewJSONHandler(logs, nil)),
		Backup: func(context.Context) (*os.File, error) { return os.Open(file) }}))
	srv.Config.BaseContext = func(net.Listener) context.Context { return base }
	srv.Start()
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/admin/v1/backup", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.ReadFull(resp.Body, make([]byte, 4096)); err != nil || resp.ContentLength != 32<<20 {
		t.Fatalf("Content-Length %d, error %v", resp.ContentLength, err)
	}
	halt()
	if n, err := io.Copy(io.Discard, resp.Body); err == nil {
		t.Errorf("the stream ran to its end after the stop: %d more bytes", n)
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), `"msg":"backup cut"`); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no cut line: %s", logs)
		}
	}
}
