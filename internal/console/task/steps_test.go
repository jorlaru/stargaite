package task

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

// readiness is a data plane on 127.0.0.1 whose readiness answers 503, set
// as the world's data plane; it counts the checks.
func readiness(t *testing.T, w *world) *atomic.Int32 {
	t.Helper()
	var checks atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		checks.Add(1)
		rw.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	writeFile(t, filepath.Join(w.h.Dir, ".env"), "STARGAITE_PORT="+u.Port()+"\n", 0o600)
	return &checks
}

// readyIn runs ready under a one-step table, as a task would.
func readyIn(w *world, u *ui.UI) error {
	_, err := u.Run(context.Background(), []ui.Step{{ID: "x", Title: "x"}}, func(ctx context.Context) (string, error) {
		return "", ready(ctx, u, w.h, "x")
	})
	return err
}

func TestReadyEsc(t *testing.T) {
	w := newWorld(t)
	checks := readiness(t, w)
	s := &scripted{leaveWork: func(q ui.Question) bool { return q.Prompt == "waiting until the proxy is ready" }}
	done := make(chan error, 1)
	go func() { done <- readyIn(w, ui.New(s, nil)) }()
	select {
	case err := <-done:
		if !errors.Is(err, ui.ErrLeave) {
			t.Fatalf("err = %v\n%s", err, s.output())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Esc did not stop the readiness wait within 2 s")
	}
	if checks.Load() == 0 {
		t.Error("readiness was never checked")
	}
}

func TestReadyNoContainer(t *testing.T) {
	w := newWorld(t)
	checks := readiness(t, w)
	writeFile(t, filepath.Join(filepath.Dir(w.log), "docker"), "#!/bin/sh\n[ \"$*\" = \"compose ps -q\" ] || exit 9\n", 0o755)
	err := readyIn(w, ui.New(&scripted{}, nil))
	if err == nil || err.Error() != "no container runs here: ./setup.sh install starts it" || checks.Load() != 0 {
		t.Fatalf("err = %v, %d readiness checks", err, checks.Load())
	}
}
