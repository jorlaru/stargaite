package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/jorlaru/stargaite/internal/admin"
	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/api"
	"github.com/jorlaru/stargaite/internal/auth"
	"github.com/jorlaru/stargaite/internal/cli"
	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/store"
	"github.com/jorlaru/stargaite/internal/telemetry"
	"github.com/jorlaru/stargaite/internal/transport"
	"github.com/jorlaru/stargaite/internal/usage"
)

// The servers' read and write timeouts (SEC-6). A data-plane request clears
// its read deadline once its body is read and moves its write deadline per
// provider call (phase-2 design section 10). On a stop signal, requests in
// flight get shutdownGrace to finish (RES-6); those still running are then
// stopped, drainWait bounds the wait for them to end, and flushWait the
// usage writer's flush, before the store closes, and exportWait the
// exporter's last sends (phase-3 design section 11). Tests lower them.
var (
	serverReadTimeout  = 60 * time.Second
	serverWriteTimeout = 60 * time.Second
	shutdownGrace      = 30 * time.Second
	drainWait          = 5 * time.Second
	flushWait          = 10 * time.Second
	exportWait         = 5 * time.Second
)

// Retention runs retainRun retainFirst after the ready line, then every
// retainEvery (USE-11, phase-4 design section 8). Tests lower the intervals
// and replace retainRun to hold a run across the stop.
var (
	retainFirst = time.Minute
	retainEvery = 24 * time.Hour
	retainRun   = usage.Retain
)

// serve runs the proxy until SIGINT or SIGTERM. Each flag falls back to its
// STARGAITE_<FLAG> variable; the flag wins. Logs are JSON lines on stderr.
func serve(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stargaite serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dataDir := fs.String("data-dir", "./data", "the data directory: the store and its lock")
	dataAddr := fs.String("listen", "127.0.0.1:4000", "address of the data-plane listener")
	adminAddr := fs.String("admin-listen", "127.0.0.1:4001", "address of the admin listener")
	certFile := fs.String("tls-cert", "", "PEM certificate chain: both listeners serve TLS")
	keyFile := fs.String("tls-key", "", "PEM private key of --tls-cert")
	behindProxy := fs.Bool("behind-tls-proxy", false, "a trusted front end terminates TLS for a listener outside loopback")
	envFile := fs.String("env-file", "", "a file of NAME=value lines that credential references also resolve from, read again on every change")
	fs.Bool("json", false, "accepted like on every command: the logs are always JSON")

	var err error
	fs.VisitAll(func(f *flag.Flag) {
		name := "STARGAITE_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		if v := getenv(name); v != "" && err == nil {
			if err = fs.Set(f.Name, v); err != nil {
				err = fmt.Errorf("%s: %w", name, err)
			}
		}
	})
	if err == nil {
		err = fs.Parse(args)
	}
	switch {
	case errors.Is(err, flag.ErrHelp):
		fs.SetOutput(stdout)
		fs.Usage()
		return 0
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "stargaite serve: %v\n", err)
		return 2
	case fs.NArg() > 0:
		_, _ = fmt.Fprintf(stderr, "stargaite serve: unexpected argument %q\n", fs.Arg(0))
		return 2
	case *dataDir == "":
		_, _ = fmt.Fprintln(stderr, "stargaite serve: --data-dir is empty")
		return 2
	case (*certFile == "") != (*keyFile == ""):
		_, _ = fmt.Fprintln(stderr, "stargaite serve: --tls-cert and --tls-key go together")
		return 2
	}

	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.LevelKey && a.Value.Any() == usage.LevelCritical {
			a.Value = slog.StringValue("CRITICAL")
		}
		return a
	}}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The listener rule (SEC-6), for both listeners, before anything opens.
	for _, l := range []struct{ name, addr string }{{"admin", *adminAddr}, {"data", *dataAddr}} {
		if err := checkListener(l.addr, *certFile != "" || *behindProxy); err != nil {
			log.Error("listener refused", "listener", l.name, "rule", "SEC-6", "error", err.Error())
			return 1
		}
	}
	var tlsConfig *tls.Config
	if *certFile != "" {
		cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			log.Error("tls certificate refused", "error", err.Error())
			return 1
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	st, err := store.Open(ctx, *dataDir, log)
	if err != nil {
		log.Error("store failed", "data_dir", *dataDir, "error", err.Error())
		return 1
	}
	// The process environment cannot tell an empty variable from an unset
	// one through getenv; an empty one does not resolve anyway.
	environ := func(name string) (string, bool) {
		v := getenv(name)
		return v, v != ""
	}
	cfg, err := config.Open(ctx, st, func() config.Env { return config.NewEnv(environ, *envFile, systemResolver) }, time.Now)
	if err != nil {
		log.Error("configuration failed", "error", err.Error())
		if err := st.Close(); err != nil {
			log.Error("store close failed", "error", err.Error())
		}
		return 1
	}
	snap := cfg.Snapshot()
	// A stored configuration with findings is still published (design
	// section 13, point 13). A finding never carries a credential value.
	for _, f := range snap.Findings {
		level := slog.LevelWarn
		if f.Severity == domain.SeverityError {
			level = slog.LevelError
		}
		log.Log(ctx, level, "configuration finding", "severity", f.Severity, "rule", f.Rule, "code", f.Code,
			"resource", f.Resource, "field", f.Field, "message", f.Message)
	}

	// The budgets' counters and every declared quota window are rebuilt from
	// the store before the listeners bind, so a restart changes no figure
	// (RES-9, LIM-8). A window declared later is seeded at its first use.
	limits := admission.New(cfg.Now, usage.Seeder(st.Read(), log))
	for _, step := range []struct {
		failed string
		run    func() error
	}{
		{"budgets not restored", func() error { return usage.Restore(ctx, st.Read(), limits, cfg.Now()) }},
		{"quotas not restored", func() error { return limits.Sync(snap.Revision, snap.Doc.Providers, snap.Doc.Keys) }},
	} {
		if err := step.run(); err != nil {
			log.Error(step.failed, "error", err.Error())
			if err := st.Close(); err != nil {
				log.Error("store close failed", "error", err.Error())
			}
			return 1
		}
	}
	// Every record goes through one writer on the store's write path, which
	// config commits share (ADR-003).
	records := usage.NewWriter(st.Write, log)
	// Both outbound clients apply SEC-4 at dial time; the exporter reads the
	// published exporter per batch (phase-3 design section 10).
	clients := [2]*http.Client{transport.NewClient(false, systemResolver), transport.NewClient(true, systemResolver)}
	spans := telemetry.NewExporter(func() *config.Exporter { return cfg.Snapshot().Exporter }, clients, cli.Version, log)
	code := 0
	// The admin key, under the lock and before the admin listener binds (SEC-3).
	keyHash, created, err := admin.Key(*dataDir, getenv)
	switch {
	case err != nil:
		log.Error("admin key refused", "rule", "SEC-3", "error", err.Error())
		code = 1
	case created:
		log.Info("admin key created", "path", filepath.Join(*dataDir, admin.KeyFile))
	}
	// The metrics token, read once; only its SHA-256 is kept (SEC-3).
	var metricsHash *[32]byte
	if code == 0 {
		if metricsHash, err = metricsToken(getenv("STARGAITE_METRICS_TOKEN"), keyHash); err != nil {
			log.Error("metrics token refused", "rule", "SEC-3", "error", err.Error())
			code = 1
		}
	}
	attrs := []any{"data_dir", *dataDir, "tls", tlsConfig != nil, "version", cli.Version, "revision", snap.Revision}
	// The data plane's OPS-8 counters, which the store check shares, and its
	// span counts by key and alias; the metrics route renders both.
	counters, spanned := &usage.Counters{}, &usage.Counters{}
	if code == 0 {
		// Every request context derives from base: cancelling it stops the
		// requests still running when the grace ends, and flight counts the
		// inference requests whose done has not run yet and the verify runs
		// (phase-4 design section 8, stop order). Retention and the
		// store check run on bg once the proxy is ready, counted by
		// background (kick-off section 14.3).
		base, halt := context.WithCancelCause(context.Background())
		bg, stopBg := context.WithCancel(base)
		var flight, background sync.WaitGroup
		ready := func() {
			background.Go(func() { retain(bg, st, cfg, log) })
			background.Go(func() { checkStore(bg, st, log, counters) })
		}
		// One data plane: a verify run goes through the same Flight, writer,
		// admission state, counters and exporter as traffic (RSN-9), and
		// halt stops its call like any other.
		d := api.Deps{Config: cfg, Log: log, Clients: clients, Flight: &flight, Usage: records, Admission: limits, Counters: counters, Spanned: spanned,
			Spans: spans, Version: cli.Version}
		if err := runServers(ctx, base, log, tlsConfig, attrs, ready, []listener{
			{"admin", *adminAddr, admin.Handler(admin.Deps{Config: cfg, KeyHash: keyHash, Log: log, Usage: st.Read(), Admission: limits, Spans: spans,
				Counters: counters, Spanned: spanned, MetricsHash: metricsHash, Backup: st.Backup, Verify: api.Verify(d)})},
			{"data", *dataAddr, api.Handler(d)},
		}); err != nil {
			log.Error("server failed", "error", err.Error())
			code = 1
		}
		halt(api.ErrStopping)
		drained := make(chan struct{})
		go func() { flight.Wait(); close(drained) }()
		select {
		case <-drained:
		case <-time.After(drainWait):
			// Their records, added after the flush below, go to the
			// critical log.
			log.Error("requests still running after the drain wait", "drain_wait", drainWait.String())
			code = 1
		}
		// Retention returns between batches, the quick_check is interrupted,
		// and neither touches the store after this (RES-6).
		stopBg()
		background.Wait()
	}
	// Every finish that ran has added its record: each is stored before the
	// store closes, or logged at critical level (USE-10).
	flush, cancel := context.WithTimeout(context.Background(), flushWait)
	if n := records.Close(flush); n > 0 {
		log.Error("usage records logged instead of stored", "records", n)
		code = 1
	}
	cancel()
	if err := st.Close(); err != nil {
		log.Error("store close failed", "error", err.Error())
		code = 1
	}
	// Spans are best effort (TEL-12): those dropped only log.
	export, cancel := context.WithTimeout(context.Background(), exportWait)
	if n := spans.Close(export); n > 0 {
		log.Warn("spans dropped at the stop", "spans", n)
	}
	cancel()
	if code == 0 {
		log.Info("stopped")
	}
	return code
}

// retain runs usage.Retain retainFirst after the ready line, then every
// retainEvery, until ctx ends, each run with the cutoffs of the published
// snapshot on the service clock (USE-11, ADR-017). Each run logs one line
// with the rows deleted per table; a failed run logs an error line and is
// retried at the next run.
func retain(ctx context.Context, st *store.Store, cfg *config.Service, log *slog.Logger) {
	next := time.NewTimer(retainFirst)
	defer next.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-next.C:
		}
		snap := cfg.Snapshot()
		c := usage.CutoffsAt(cfg.Now(), *snap.Settings.Retention, snap.Doc.Providers)
		d, err := retainRun(ctx, st.Write, c)
		attrs := []any{"usage", d.Usage, "usage_daily", d.Daily, "audit", d.Audit,
			"usage_before", c.Usage.UTC().Format(time.RFC3339), "usage_daily_before", c.Day, "audit_before", c.Audit.UTC().Format(time.RFC3339)}
		switch {
		case err != nil && ctx.Err() != nil:
			log.Info("retention stopped between batches", append(attrs, "error", err.Error())...)
			return
		case err != nil:
			log.Error("retention failed: retried at the next run", append(attrs, "next_run", retainEvery.String(), "error", err.Error())...)
		default:
			log.Info("retention run", attrs...)
		}
		next.Reset(retainEvery)
	}
}

// checkStore runs the store check once, after the ready line (kick-off
// section 14.3, ADR-018): quick_check's rows, or an error that is not the
// stop's, are one CRITICAL line and add store_check_failed to counters; the
// proxy keeps serving.
func checkStore(ctx context.Context, st *store.Store, log *slog.Logger, counters *usage.Counters) {
	rows, err := st.Check(ctx)
	switch {
	case err != nil && ctx.Err() != nil:
		log.Info("store check stopped", "error", err.Error())
	case err != nil:
		log.Log(ctx, usage.LevelCritical, "store check failed", "error", err.Error())
		counters.Add(usage.Counter{Name: "store_check_failed"}, 1)
	case rows != nil:
		log.Log(ctx, usage.LevelCritical, "store check failed", "rows", rows)
		counters.Add(usage.Counter{Name: "store_check_failed"}, 1)
	default:
		log.Info("store check passed")
	}
}

// metricsToken is the SHA-256 of STARGAITE_METRICS_TOKEN, nil when it is
// unset (SEC-3, phase-4 design section 15 point 17). The admin key's rule
// applies, and the token may not be the admin key, which opens every route:
// at least 32 characters, no leading or trailing whitespace, not the client
// key prefix. No error carries the token.
func metricsToken(token string, adminHash [32]byte) (*[32]byte, error) {
	if token == "" {
		return nil, nil
	}
	hash := sha256.Sum256([]byte(token))
	switch {
	case utf8.RuneCountInString(token) < 32:
		return nil, errors.New("STARGAITE_METRICS_TOKEN has fewer than 32 characters")
	case token != strings.TrimSpace(token):
		return nil, errors.New("STARGAITE_METRICS_TOKEN has leading or trailing whitespace, which no client can present")
	case strings.HasPrefix(token, auth.Prefix):
		return nil, errors.New("STARGAITE_METRICS_TOKEN starts with " + auth.Prefix + ", the prefix of a client key")
	case hash == adminHash:
		return nil, errors.New("STARGAITE_METRICS_TOKEN is the admin key; it must differ, since it opens the metrics alone")
	}
	return &hash, nil
}

// systemResolver is the resolver of set time (CheckResolved) and of dial
// time, in both outbound clients: both ask the same source.
func systemResolver(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// checkListener applies SEC-6 to one address: a host other than a loopback
// literal or localhost needs TLS, from the certificate pair or a trusted front
// end. An empty host listens everywhere, so it is not loopback.
func checkListener(addr string, secured bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); secured || host == "localhost" || ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("%s is outside loopback: it needs --tls-cert and --tls-key, or --behind-tls-proxy", addr)
}

type listener struct {
	name, addr string
	handler    http.Handler
}

// runServers binds the listeners in order, logs the ready line with attrs
// and the bound addresses, calls ready, which must return at once, and
// serves until ctx is done or a server fails; then it shuts every server
// down: the listeners close at once and requests in flight get
// shutdownGrace to finish. Every request
// context derives from base. A grace that ends with requests still running
// is logged, not an error: the caller stops them by cancelling base.
func runServers(ctx, base context.Context, log *slog.Logger, tlsConfig *tls.Config, attrs []any, ready func(), ls []listener) error {
	lns := make([]net.Listener, 0, len(ls))
	defer func() {
		for _, ln := range lns {
			_ = ln.Close() // already closed by Shutdown, except on a failed bind
		}
	}()
	for _, l := range ls {
		ln, err := net.Listen("tcp", l.addr)
		if err != nil {
			return fmt.Errorf("listener %s: %w", l.name, err)
		}
		lns = append(lns, ln)
		attrs = append(attrs, l.name+"_addr", ln.Addr().String())
	}

	failed := make(chan error, len(ls))
	srvs := make([]*http.Server, len(ls))
	for i, l := range ls {
		srvs[i] = &http.Server{
			Handler:           l.handler,
			TLSConfig:         tlsConfig,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       serverReadTimeout,
			WriteTimeout:      serverWriteTimeout,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    64 << 10,
			ErrorLog:          slog.NewLogLogger(log.With("listener", l.name).Handler(), slog.LevelWarn),
			BaseContext:       func(net.Listener) context.Context { return base },
		}
		go func() {
			var err error
			if tlsConfig != nil {
				err = srvs[i].ServeTLS(lns[i], "", "")
			} else {
				err = srvs[i].Serve(lns[i])
			}
			failed <- fmt.Errorf("listener %s: %w", l.name, err)
		}()
	}
	log.Info("ready", attrs...)
	ready()

	var err error
	select {
	case err = <-failed:
	case <-ctx.Done():
	}
	grace, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	done := make(chan error, len(srvs))
	for i, srv := range srvs {
		go func() {
			err := srv.Shutdown(grace)
			if errors.Is(err, context.DeadlineExceeded) {
				log.Warn("requests still running when the grace ended: they are stopped", "listener", ls[i].name, "grace", shutdownGrace.String())
				err = nil
			}
			done <- err
		}()
	}
	for range srvs {
		err = errors.Join(err, <-done)
	}
	return err
}
