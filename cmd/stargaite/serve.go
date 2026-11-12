package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/jorlaru/stargaite/internal/api"
	"github.com/jorlaru/stargaite/internal/cli"
)

// dataAddr is the default address of the data-plane listener: loopback only.
const dataAddr = "127.0.0.1:4000"

// shutdownGrace bounds how long in-flight requests may take to finish on a stop signal.
const shutdownGrace = 30 * time.Second

// serve runs the proxy until SIGINT or SIGTERM. Logs are JSON lines on stderr.
func serve(args []string, addr string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stargaite serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Bool("json", false, "accepted like on every command: the logs are always JSON")
	switch err := fs.Parse(args); {
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
	}

	log := slog.New(slog.NewJSONHandler(stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error("listen failed", "listener", "data", "error", err.Error())
		return 1
	}
	if err := runServer(ctx, ln, log); err != nil {
		log.Error("server failed", "listener", "data", "error", err.Error())
		return 1
	}
	return 0
}

// runServer serves the data plane on ln until ctx is done, then shuts down gracefully.
func runServer(ctx context.Context, ln net.Listener, log *slog.Logger) error {
	srv := &http.Server{
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	log.Info("listening", "listener", "data", "addr", ln.Addr().String(), "version", cli.Version)

	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}

	grace, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(grace); err != nil {
		return err
	}
	log.Info("stopped", "listener", "data")
	return nil
}
