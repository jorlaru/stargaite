// Package api serves the data-plane listener: the health probes, GET
// /v1/models and the inference routes (phase-2 design section 4).
package api

import (
	"crypto/rand"
	"encoding/hex"
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
	"github.com/jorlaru/stargaite/internal/routing"
)

// refusalLogEvery rate-limits the log line of a repeated refusal, as on the
// admin listener: one line per interval, with the count since the last one.
const refusalLogEvery = 10 * time.Second

// refusals counts one kind of refusal since the last line logged.
type refusals struct {
	mu sync.Mutex
	at time.Time // when the last line was logged, on the service clock
	n  int
}

// tick counts one refusal and reports whether a line may be logged now, with
// the count since the previous line.
func (l *refusals) tick(now time.Time) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n++; now.Sub(l.at) < refusalLogEvery {
		return 0, false
	}
	n := l.n
	l.at, l.n = now, 0
	return n, true
}

// Deps is what the data plane needs from serve.
type Deps struct {
	Config  *config.Service
	Log     *slog.Logger
	Clients [2]*http.Client // [0] public addresses only, [1] allow_private_network (SEC-4)
}

type handler struct {
	Deps
	refused  refusals // failed authentications
	upstream refusals // a provider refusing the operator's credential (401, 403)
	breakers *routing.Breakers
}

// Handler returns the routes of the data-plane listener. Every response
// carries a request id. The circuit breakers live as long as the handler,
// on the service clock: every apply keeps them (RES-7).
func Handler(d Deps) http.Handler {
	h := &handler{Deps: d, breakers: routing.NewBreakers(d.Config.Now)}
	mux := http.NewServeMux()
	// serve binds this listener only once the store is open and the stored
	// configuration is published (config.Open returned), so whatever answers
	// is ready (kick-off section 13.1); on a stop the listener closes before
	// the store does.
	mux.HandleFunc("GET /health/live", ok)
	mux.HandleFunc("GET /health/ready", ok)
	mux.HandleFunc("GET /v1/models", h.models)
	mux.HandleFunc("POST /v1/chat/completions", h.infer(domain.EndpointChat))
	mux.HandleFunc("POST /v1/messages", h.infer(domain.EndpointMessages))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(domain.RequestIDHeader, rand.Text())
		mux.ServeHTTP(w, r)
	})
}

// authenticate returns the key of the client token (step 1), checked
// against snap, and the trace id it sets on the response (PAR-9). The token
// is in Authorization: Bearer or, on /v1/messages, in x-api-key (ACT-1);
// when both are sent, Authorization wins. The admin key never has the shape
// of a client key, so it is refused like any other wrong token (SEC-2). A
// failure answers 401 invalid_api_key in the envelope of ep and is logged
// rate-limited, never with the token, so a flood of wrong tokens cannot grow
// the log (OPS-9).
func (h *handler) authenticate(w http.ResponseWriter, r *http.Request, snap *config.Snapshot, ep domain.Endpoint) (*config.Key, string) {
	now := h.Config.Now()
	var key *config.Key
	want := "a valid client key is required: Authorization: Bearer sk-stargaite-..."
	if ep == domain.EndpointMessages {
		want = "a valid client key is required: x-api-key: sk-stargaite-... or Authorization: Bearer sk-stargaite-..."
	}
	if v := r.Header.Get("Authorization"); v != "" || ep != domain.EndpointMessages {
		scheme, token, _ := strings.Cut(v, " ")
		if strings.EqualFold(scheme, "Bearer") {
			key, _ = snap.Authenticate(token, now)
		}
	} else {
		key, _ = snap.Authenticate(r.Header.Get("X-Api-Key"), now)
	}
	if key == nil {
		fail(w, ep, domain.NewError(domain.CodeInvalidAPIKey, want))
		if n, ok := h.refused.tick(now); ok {
			h.Log.Warn("authentication failed", "request_id", w.Header().Get(domain.RequestIDHeader), "method", r.Method, "path", r.URL.Path,
				"status", http.StatusUnauthorized, "remote", r.RemoteAddr, "failures", n)
		}
		return nil, ""
	}
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails
	trace := hex.EncodeToString(b[:])
	w.Header().Set(traceHeader, trace)
	return key, trace
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
// request after its revocation is published (SEC-8). The request line names
// the key, never the token (OPS-9).
func (h *handler) models(w http.ResponseWriter, r *http.Request) {
	snap := h.Config.Snapshot()
	key, trace := h.authenticate(w, r, snap, domain.EndpointChat)
	if key == nil {
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
	h.Log.Info("request", "request_id", w.Header().Get(domain.RequestIDHeader), "trace_id", trace, "method", r.Method, "path", r.URL.Path,
		"status", http.StatusOK, "key", key.Name)
}

// fail writes e in the envelope of ep (kick-off section 15), with its
// Retry-After header when it has one: the OpenAI envelope, or on
// /v1/messages the Anthropic one with the code in x-stargaite-error-code.
func fail(w http.ResponseWriter, ep domain.Endpoint, e *domain.Error) {
	if e.RetryAfter != "" {
		w.Header().Set("Retry-After", e.RetryAfter)
	}
	if ep == domain.EndpointMessages {
		w.Header().Set(errorCodeHeader, e.Code)
	}
	write(w, e.Status, envelope(ep, e))
}

// anthropicError is the Anthropic envelope (kick-off section 15), which has
// no code member: the code leads the message.
type anthropicError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// envelope is e in the error envelope of ep: {"type":"error","error":
// {"type","message":"<code>: <message>"}} on /v1/messages, else {"error":e}.
func envelope(ep domain.Endpoint, e *domain.Error) any {
	if ep == domain.EndpointMessages {
		a := anthropicError{Type: "error"}
		a.Error.Type, a.Error.Message = e.Type, e.Code+": "+e.Message
		return a
	}
	return struct {
		Error *domain.Error `json:"error"`
	}{e}
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
