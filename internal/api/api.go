// Package api serves the data-plane listener.
package api

import (
	"io"
	"net/http"
)

// Handler returns the routes of the data-plane listener.
func Handler() http.Handler {
	mux := http.NewServeMux()
	// serve binds this listener only once the store is open, so whatever
	// answers is ready; on a stop the listener closes before the store does.
	mux.HandleFunc("GET /health/live", ok)
	mux.HandleFunc("GET /health/ready", ok)
	return mux
}

func ok(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
}
