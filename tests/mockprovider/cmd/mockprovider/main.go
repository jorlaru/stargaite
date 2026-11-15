// Command mockprovider serves the mock provider for the quick start:
// go run ./tests/mockprovider/cmd/mockprovider [-listen 127.0.0.1:4010]
//
// For the benchmark (tests/benchmark), which runs it as a process, it also
// takes a JSON array of mockprovider.Step on POST /mock/script, queued as
// Script queues them, and lists the calls received, with their Ended and
// Cancelled, on GET /mock/calls.
package main

import (
	"encoding/json"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jorlaru/stargaite/tests/mockprovider"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:4010", "address to serve on")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	mock := mockprovider.New()
	mux := http.NewServeMux()
	mux.Handle("/", mock)
	mux.HandleFunc("POST /mock/script", func(w http.ResponseWriter, r *http.Request) {
		var steps []mockprovider.Step
		if err := json.NewDecoder(r.Body).Decode(&steps); err != nil {
			http.Error(w, "the body is not a JSON array of steps: "+err.Error(), http.StatusBadRequest)
			return
		}
		mock.Script(steps...)
	})
	mux.HandleFunc("GET /mock/calls", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mock.Calls())
	})
	ln, err := net.Listen("tcp", *listen)
	if err == nil {
		log.Info("ready", "addr", ln.Addr().String())
		err = (&http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
	}
	log.Error("mock provider stopped", "error", err.Error())
	os.Exit(1)
}
