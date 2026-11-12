// Command mockprovider serves the mock provider for the quick start:
// go run ./tests/mockprovider/cmd/mockprovider [-listen 127.0.0.1:4010]
package main

import (
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
	ln, err := net.Listen("tcp", *listen)
	if err == nil {
		log.Info("ready", "addr", ln.Addr().String())
		err = (&http.Server{Handler: mockprovider.New(), ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
	}
	log.Error("mock provider stopped", "error", err.Error())
	os.Exit(1)
}
