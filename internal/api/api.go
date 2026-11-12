// Package api serves the data-plane listener.
package api

import (
	"io"
	"net/http"
)

// Handler returns the routes of the data-plane listener.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
	})
	return mux
}
