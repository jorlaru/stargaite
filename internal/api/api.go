// Package api serves the data-plane listener. In phase 1 its one
// authenticated route is GET /v1/models (design section 7).
package api

import (
	"crypto/rand"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
)

// refusalLogEvery rate-limits the log line of a failed authentication, as on
// the admin listener: one line per interval, with the count of failures.
const refusalLogEvery = 10 * time.Second

// refusals counts the failed authentications since the last line logged.
type refusals struct {
	mu sync.Mutex
	at time.Time // when the last line was logged, on the service clock
	n  int
}

// Handler returns the routes of the data-plane listener. Every response
// carries a request id.
func Handler(cfg *config.Service, log *slog.Logger) http.Handler {
	refused := &refusals{}
	mux := http.NewServeMux()
	// serve binds this listener only once the store is open and the stored
	// configuration is published (config.Open returned), so whatever answers
	// is ready (kick-off section 13.1); on a stop the listener closes before
	// the store does.
	mux.HandleFunc("GET /health/live", ok)
	mux.HandleFunc("GET /health/ready", ok)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) { models(cfg, log, refused, w, r) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(domain.RequestIDHeader, rand.Text())
		mux.ServeHTTP(w, r)
	})
}

func ok(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
}

// model is one entry of GET /v1/models, in the OpenAI list shape. An alias
// has no creation time of its own, so created is 0.
type model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// models lists the aliases the key may use (SEC-2, AC-06). It authenticates
// against one snapshot, loaded once: a revoked key is refused from the first
// request after its revocation is published (SEC-8). The admin key never has
// the shape of a client key, so it is refused like any other wrong token. The
// request line names the key, never the token; a failure is logged
// rate-limited, so a flood of wrong tokens cannot grow the log (OPS-9).
func models(cfg *config.Service, log *slog.Logger, refused *refusals, w http.ResponseWriter, r *http.Request) {
	snap, now := cfg.Snapshot(), cfg.Now()
	var key *config.Key
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if strings.EqualFold(scheme, "Bearer") {
		key, _ = snap.Authenticate(token, now)
	}
	attrs := []any{"request_id", w.Header().Get(domain.RequestIDHeader), "method", r.Method, "path", r.URL.Path}
	if key == nil {
		write(w, http.StatusUnauthorized, struct {
			Error *domain.Error `json:"error"`
		}{domain.NewError(domain.CodeInvalidAPIKey, "a valid client key is required: Authorization: Bearer sk-stargaite-...")})
		refused.mu.Lock()
		defer refused.mu.Unlock()
		if refused.n++; now.Sub(refused.at) >= refusalLogEvery {
			log.Warn("authentication failed", append(attrs, "status", http.StatusUnauthorized, "remote", r.RemoteAddr, "failures", refused.n)...)
			refused.at, refused.n = now, 0
		}
		return
	}
	list := []model{}
	for _, alias := range slices.Sorted(maps.Keys(snap.Models)) {
		if key.Allows(alias) {
			list = append(list, model{alias, "model", 0, "stargaite"})
		}
	}
	write(w, http.StatusOK, struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{"list", list})
	log.Info("request", append(attrs, "status", http.StatusOK, "key", key.Name)...)
}

func write(w http.ResponseWriter, status int, v any) {
	data, err := config.Encode(v)
	if err != nil {
		status, data = http.StatusInternalServerError, []byte(`{"error":{"message":"the response could not be encoded","type":"api_error","code":"internal_error"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
