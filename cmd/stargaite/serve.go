package main

import (
	"context"
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
	"syscall"
	"time"

	"github.com/jorlaru/stargaite/internal/admin"
	"github.com/jorlaru/stargaite/internal/api"
	"github.com/jorlaru/stargaite/internal/cli"
	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/store"
)

// shutdownGrace bounds how long in-flight requests may take to finish on a stop signal.
const shutdownGrace = 30 * time.Second

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

	log := slog.New(slog.NewJSONHandler(stderr, nil))
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

	st, err := store.Open(ctx, *dataDir)
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
	ready := []any{"data_dir", *dataDir, "tls", tlsConfig != nil, "version", cli.Version, "revision", snap.Revision}
	if code == 0 {
		if err := runServers(ctx, log, tlsConfig, ready, []listener{
			{"admin", *adminAddr, admin.Handler(cfg, keyHash, log)},
			{"data", *dataAddr, api.Handler()},
		}); err != nil {
			log.Error("server failed", "error", err.Error())
			code = 1
		}
	}
	if err := st.Close(); err != nil {
		log.Error("store close failed", "error", err.Error())
		code = 1
	}
	if code == 0 {
		log.Info("stopped")
	}
	return code
}

// systemResolver is the resolver of set time (CheckResolved) and, from phase
// 2, of dial time: both ask the same source.
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

// runServers binds the listeners in order, logs the ready line with the
// attributes of ready and the bound addresses, and serves until ctx is done or
// a server fails; then it shuts every server down within shutdownGrace.
func runServers(ctx context.Context, log *slog.Logger, tlsConfig *tls.Config, ready []any, ls []listener) error {
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
		ready = append(ready, l.name+"_addr", ln.Addr().String())
	}

	failed := make(chan error, len(ls))
	srvs := make([]*http.Server, len(ls))
	for i, l := range ls {
		srvs[i] = &http.Server{
			Handler:           l.handler,
			TLSConfig:         tlsConfig,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    64 << 10,
			ErrorLog:          slog.NewLogLogger(log.With("listener", l.name).Handler(), slog.LevelWarn),
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
	log.Info("ready", ready...)

	var err error
	select {
	case err = <-failed:
	case <-ctx.Done():
	}
	grace, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	done := make(chan error, len(srvs))
	for _, srv := range srvs {
		go func() { done <- srv.Shutdown(grace) }()
	}
	for range srvs {
		err = errors.Join(err, <-done)
	}
	return err
}
