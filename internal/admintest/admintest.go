// Package admintest is the admin API in process, for tests only: the CLI's
// tests and the console's call it through the real CLI. It imports neither
// the CLI nor the console, which both import it from their tests.
package admintest

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/admin"
	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/api"
	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/store"
	"github.com/jorlaru/stargaite/internal/telemetry"
	"github.com/jorlaru/stargaite/internal/transport"
	records "github.com/jorlaru/stargaite/internal/usage"
)

// Key is the admin key of Server.
const Key = "test-admin-key-of-at-least-32-characters"

// AnnexBEnv sets every credential config.example.yaml references.
func AnnexBEnv() map[string]string {
	return map[string]string{
		"OPENROUTER_API_KEY": "x", "ANTHROPIC_API_KEY": "x", "MINIMAX_PLAN_API_KEY": "x",
		"LANGFUSE_PUBLIC_KEY": "x", "LANGFUSE_SECRET_KEY": "x",
	}
}

// Server is the admin listener in process, on an empty store, with every
// credential of config.example.yaml resolving and no name lookup leaving the
// test. It returns the environment that points the CLI at it.
func Server(t testing.TB) map[string]string {
	t.Helper()
	env, _ := Plane(t)
	return env
}

// Plane is Server with the data plane beside it, in process on the same
// configuration: env points the CLI at the admin listener, and port is the
// data plane's on 127.0.0.1. Its clients, as verify's, resolve no name, so a
// provider must be on 127.0.0.1 with allow_private_network.
func Plane(t testing.TB) (env map[string]string, port string) {
	t.Helper()
	annex := AnnexBEnv()
	st, err := store.Open(context.Background(), t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfgEnv := func() config.Env {
		return config.Env{
			Lookup: func(n string) (string, bool) { v, ok := annex[n]; return v, ok },
			Resolve: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			},
		}
	}
	cfg, err := config.Open(context.Background(), st, cfgEnv, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// The exporter dials loopback for every name, which SEC-4 refuses without
	// allow_private_network: telemetry test never leaves the host.
	loopback := func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	spans := telemetry.NewExporter(func() *config.Exporter { return cfg.Snapshot().Exporter },
		[2]*http.Client{transport.NewClient(false, loopback), transport.NewClient(true, loopback)}, "test", log)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		spans.Close(ctx)
	})
	// targets verify and the data plane run on clients that resolve no name,
	// so no call leaves the host.
	limits, writer := admission.New(cfg.Now, records.Seeder(st.Read(), log)), records.NewWriter(st.Write, log)
	t.Cleanup(func() { writer.Close(context.Background()) }) // before the store closes
	noDNS := func(context.Context, string) ([]netip.Addr, error) {
		return nil, errors.New("no name is resolved in tests")
	}
	deps := api.Deps{Config: cfg, Log: log, Clients: [2]*http.Client{transport.NewClient(false, noDNS), transport.NewClient(true, noDNS)},
		Flight: &sync.WaitGroup{}, Usage: writer, Admission: limits, Counters: &records.Counters{}, Spanned: &records.Counters{}, Spans: spans, Version: "test"}
	srv := httptest.NewServer(admin.Handler(admin.Deps{Config: cfg, KeyHash: sha256.Sum256([]byte(Key)), Log: log, Usage: st.Read(),
		Admission: limits, Spans: spans, Backup: st.Backup, Verify: api.Verify(deps)}))
	t.Cleanup(srv.Close)
	data := httptest.NewServer(api.Handler(deps))
	t.Cleanup(data.Close)
	u, err := url.Parse(data.URL)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"STARGAITE_ADMIN_URL": srv.URL, "STARGAITE_ADMIN_KEY": Key}, u.Port()
}
