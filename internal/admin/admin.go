// Package admin serves the admin listener under /admin/v1 (kick-off section
// 13.2): one wrapper authenticates every route with the admin key, gives every
// response a request id and answers every error in the envelope of the error
// model (kick-off section 15).
package admin

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/auth"
	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/telemetry"
	"github.com/jorlaru/stargaite/internal/usage"
)

const (
	// RequestIDHeader is on every response of the listener.
	RequestIDHeader = domain.RequestIDHeader
	// maxBody caps a JSON body; the apply route takes config.MaxYAMLBytes.
	maxBody = 1 << 20
	// refusalLogEvery rate-limits the log line of a failed authentication:
	// one line per interval, with the count of failures since the last one.
	refusalLogEvery = 10 * time.Second
	// lookupTimeout bounds the name lookups of a validate.
	lookupTimeout = 30 * time.Second
)

// backupWait is the write deadline of a backup, which replaces the
// listener's write timeout: the copy and its stream must end within it.
// Tests lower it.
var backupWait = 15 * time.Minute

type server struct {
	cfg     *config.Service
	keyHash [32]byte
	log     *slog.Logger
	usage   *sql.DB
	limits  *admission.State
	spans   *telemetry.Exporter
	backup  func(ctx context.Context) (*os.File, error)

	mu        sync.Mutex // guards the two fields below
	refusedAt time.Time  // when the last refusal line was logged
	refusals  int        // failures since that line
}

// route is one route of the listener; the table is what the tests walk.
type route struct {
	pattern string
	handle  func(s *server, w http.ResponseWriter, r *http.Request)
}

var routes = []route{
	{"GET /admin/v1/settings", (*server).getSettings},
	{"PATCH /admin/v1/settings", (*server).patchSettings},
	{"GET /admin/v1/export", (*server).export},
	{"POST /admin/v1/apply", (*server).apply},
	{"GET /admin/v1/validate", (*server).validate},
	{"GET /admin/v1/presets", (*server).listPresets},
	{"GET /admin/v1/providers", (*server).listProviders},
	{"POST /admin/v1/providers", (*server).addProvider},
	{"GET /admin/v1/providers/{name}", (*server).showProvider},
	{"PATCH /admin/v1/providers/{name}", (*server).setProvider},
	{"DELETE /admin/v1/providers/{name}", (*server).removeProvider},
	// A target id holds "/": the trailing wildcard takes the rest of the
	// path, unescaped, so the CLI escapes the whole id and "." or ".."
	// segments of an upstream id are never cleaned away. Sent unescaped,
	// such an id gets the mux's redirect, which the wrapper refuses.
	{"GET /admin/v1/targets", (*server).listTargets},
	{"POST /admin/v1/targets", (*server).addTarget},
	{"GET /admin/v1/targets/{id...}", (*server).showTarget},
	{"PATCH /admin/v1/targets/{id...}", (*server).setTarget},
	{"DELETE /admin/v1/targets/{id...}", (*server).removeTarget},
	{"GET /admin/v1/models", (*server).listModels},
	{"POST /admin/v1/models", (*server).addModel},
	{"GET /admin/v1/models/{name}", (*server).showModel},
	{"PATCH /admin/v1/models/{name}", (*server).setModel},
	{"DELETE /admin/v1/models/{name}", (*server).removeModel},
	{"POST /admin/v1/models/{name}/explain", (*server).explainModel},
	{"GET /admin/v1/keys", (*server).listKeys},
	{"POST /admin/v1/keys", (*server).createKey},
	{"GET /admin/v1/keys/{name}", (*server).showKey},
	{"PATCH /admin/v1/keys/{name}", (*server).setKey},
	{"POST /admin/v1/keys/{name}/rotate", (*server).rotateKey},
	{"POST /admin/v1/keys/{name}/revoke", (*server).revokeKey},
	{"GET /admin/v1/usage", (*server).showUsage},
	{"GET /admin/v1/usage/quota", (*server).showQuota},
	{"GET /admin/v1/telemetry", (*server).showTelemetry},
	{"PATCH /admin/v1/telemetry", (*server).setTelemetry},
	{"DELETE /admin/v1/telemetry", (*server).unsetTelemetry},
	{"POST /admin/v1/telemetry/test", (*server).testTelemetry},
	{"POST /admin/v1/backup", (*server).sendBackup},
}

// Deps is what the admin listener needs from serve.
type Deps struct {
	Config    *config.Service
	KeyHash   [32]byte // the SHA-256 of the admin key, the only form of it the process keeps
	Log       *slog.Logger
	Usage     *sql.DB                                     // the store's read pool, which usage show reads
	Admission *admission.State                            // the data plane's, whose quota windows usage quota shows
	Spans     *telemetry.Exporter                         // the exporter telemetry test sends through and telemetry status reads
	Backup    func(ctx context.Context) (*os.File, error) // store.Store.Backup
}

// Handler returns the admin listener's handler.
func Handler(d Deps) http.Handler {
	s := &server{cfg: d.Config, keyHash: d.KeyHash, log: d.Log, usage: d.Usage, limits: d.Admission, spans: d.Spans, backup: d.Backup}
	mux := http.NewServeMux()
	for _, rt := range routes {
		mux.HandleFunc(rt.pattern, func(w http.ResponseWriter, r *http.Request) { rt.handle(s, w, r) })
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := rand.Text()
		w.Header().Set(RequestIDHeader, id)
		// Authentication comes first, so an unknown route is 401 too and a
		// caller without the key learns nothing about the routes (SEC-2).
		scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") || !auth.Equal(token, s.keyHash) {
			s.refused(r, id)
			write(w, http.StatusUnauthorized, envelope{domain.NewError(domain.CodeInvalidAPIKey, "a valid admin key is required: Authorization: Bearer <admin key>")})
			return
		}
		// The pattern only tells whether a route matched; the request goes
		// through mux.ServeHTTP, which gives the route its wildcards
		// (r.PathValue) and r.Pattern.
		_, pattern := mux.Handler(r)
		rec := &recorder{ResponseWriter: w, mux: pattern == ""}
		mux.ServeHTTP(rec, r)
		switch {
		case rec.status/100 == 3:
			// Only the mux redirects: to the cleaned path of a path with "."
			// or ".." segments or "//", or to the path with the trailing "/"
			// of a route that ends in a wildcard.
			// A client that followed it could write to another resource
			// than the one it named, so the request is refused instead.
			w.Header().Del("Location")
			rec.status = http.StatusBadRequest // what the request line records
			write(w, rec.status, envelope{domain.NewError(domain.CodeInvalidRequest,
				`the path is not canonical: it has a "." or ".." segment or "//", or lacks a trailing "/"; escape an id whole, each "/" as %2F`)})
		case rec.mux && rec.status == http.StatusNotFound:
			write(w, rec.status, envelope{domain.NewError(domain.CodeNotFound, "no such route")})
		case rec.mux && rec.status == http.StatusMethodNotAllowed:
			// The error model has no row for it; it is a request error.
			e := domain.NewError(domain.CodeInvalidRequest, "method "+r.Method+" is not allowed here; allowed: "+w.Header().Get("Allow"))
			e.Status = rec.status
			write(w, rec.status, envelope{e})
		}
		s.log.Info("admin request", "request_id", id, "method", r.Method, "path", r.URL.Path, "status", rec.status)
	})
}

// refused logs a failed authentication, rate-limited (OPS-9). Neither the
// presented token nor anything else from the request but its address is logged.
func (s *server) refused(r *http.Request, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refusals++
	if time.Since(s.refusedAt) < refusalLogEvery {
		return
	}
	s.log.Warn("admin authentication failed", "request_id", id, "remote", r.RemoteAddr, "failures", s.refusals)
	s.refusedAt, s.refusals = time.Now(), 0
}

// recorder keeps the status for the request log line. It drops the mux's own
// answers, which the wrapper answers in the envelope instead: a redirect,
// which no route ever answers, and for a request no route matched, the
// plain-text 404 and 405.
type recorder struct {
	http.ResponseWriter
	status int
	mux    bool
}

func (rec *recorder) WriteHeader(code int) {
	rec.status = code
	if !rec.dropped() {
		rec.ResponseWriter.WriteHeader(code)
	}
}

func (rec *recorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.WriteHeader(http.StatusOK)
	}
	if rec.dropped() {
		return len(b), nil
	}
	return rec.ResponseWriter.Write(b)
}

// Unwrap lets http.NewResponseController reach the connection's deadlines.
func (rec *recorder) Unwrap() http.ResponseWriter {
	return rec.ResponseWriter
}

func (rec *recorder) dropped() bool {
	return rec.status/100 == 3 || (rec.mux && (rec.status == http.StatusNotFound || rec.status == http.StatusMethodNotAllowed))
}

type envelope struct {
	Error *domain.Error `json:"error"`
}

// write sends v as JSON with status.
func write(w http.ResponseWriter, status int, v any) {
	data, err := config.Encode(v)
	if err != nil {
		status, data = http.StatusInternalServerError, []byte(`{"error":{"message":"the response could not be encoded","type":"api_error","code":"internal_error"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

// fail answers err in the envelope. A 500 carries a fixed message: its cause
// goes to the log under the request id, never to the client (OPS-9).
func (s *server) fail(w http.ResponseWriter, err error) {
	de := (*domain.Error)(nil)
	if !errors.As(err, &de) {
		de = domain.NewError(domain.CodeInternalError, err.Error())
	}
	if de.Status >= http.StatusInternalServerError {
		id := w.Header().Get(RequestIDHeader)
		s.log.Error("internal error", "request_id", id, "error", de.Message)
		de = domain.NewError(domain.CodeInternalError, "internal error; the server log has its cause under request id "+id)
	}
	write(w, de.Status, envelope{de})
}

func invalid(format string, args ...any) *domain.Error {
	return domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf(format, args...))
}

// body reads the request body up to limit bytes.
func body(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
		return nil, domain.NewError(domain.CodeRequestTooLarge, fmt.Sprintf("the body is larger than %d bytes", limit))
	}
	if err != nil {
		return nil, invalid("the body could not be read: %v", err)
	}
	return data, nil
}

// query returns the query parameters of r, each at most once and each one of
// accepted, so that a misspelt dry_run never applies for real.
func query(r *http.Request, accepted ...string) (url.Values, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, invalid("the query is malformed")
	}
	for name, values := range q {
		if !slices.Contains(accepted, name) || len(values) > 1 {
			return nil, invalid("query parameter %.64q is unknown or repeated; this route takes %s, once each", name, strings.Join(accepted, ", "))
		}
	}
	return q, nil
}

// boolean reads a query flag: absent is false, and a flag given without a
// value is refused, so a bare ?dry_run never applies for real.
func boolean(q url.Values, name string) (bool, error) {
	if !q.Has(name) {
		return false, nil
	}
	v, err := strconv.ParseBool(q.Get(name))
	if err != nil {
		return false, invalid("query parameter %s must be true or false", name)
	}
	return v, nil
}

// merge applies an RFC 7396 merge patch to the stored declaration *v and runs
// the result through the strict decoder: null removes a member, an object
// merges member by member and anything else, a list included, replaces.
func merge[T any](v *T, patch jsontext.Value) error {
	data, err := config.Encode(*v)
	if err != nil {
		return err
	}
	if data, err = mergePatch(data, patch); err != nil {
		return err
	}
	var out T
	if err := config.Decode(data, &out); err != nil {
		return invalid("%v", err)
	}
	*v = out
	return nil
}

func mergePatch(target, patch jsontext.Value) (jsontext.Value, error) {
	if patch.Kind() != '{' {
		return patch, nil
	}
	t := map[string]jsontext.Value{}
	if target.Kind() == '{' {
		if err := json.Unmarshal(target, &t); err != nil {
			return nil, err
		}
	}
	var p map[string]jsontext.Value
	if err := json.Unmarshal(patch, &p); err != nil {
		return nil, err
	}
	for name, v := range p {
		if v.Kind() == 'n' {
			delete(t, name)
			continue
		}
		merged, err := mergePatch(t[name], v)
		if err != nil {
			return nil, err
		}
		t[name] = merged
	}
	return config.Encode(t)
}

// written is the answer to an accepted change, or a dry run: the result and,
// for a single resource, that resource as a read shows it.
type written struct {
	config.Result
	Resource any `json:"resource,omitzero"`
}

// getSettings answers the effective settings: declared values, and the
// default of every undeclared one (design section 13, point 15).
func (s *server) getSettings(w http.ResponseWriter, _ *http.Request) {
	write(w, http.StatusOK, s.cfg.Snapshot().Settings)
}

// patchBody reads the merge patch of a PATCH request: one JSON value with no
// repeated member. What it may hold is decided when it is merged.
func patchBody(w http.ResponseWriter, r *http.Request) (jsontext.Value, error) {
	data, err := body(w, r, maxBody)
	if err != nil {
		return nil, err
	}
	var patch jsontext.Value
	if err := config.Decode(data, &patch); err != nil {
		return nil, invalid("%v", err)
	}
	return patch, nil
}

func (s *server) patchSettings(w http.ResponseWriter, r *http.Request) {
	patch, err := patchBody(w, r)
	if err != nil {
		s.fail(w, err)
		return
	}
	var declared domain.Settings
	res, err := s.cfg.Commit(r.Context(), config.Change{Summary: "settings set", Kind: "settings", Mutate: func(st *config.State) (map[string]string, error) {
		err := merge(&st.Doc.Settings, patch)
		declared = st.Doc.Settings
		return nil, err
	}})
	if err != nil {
		s.fail(w, err)
		return
	}
	write(w, http.StatusOK, written{res, config.WithDefaults(declared)})
}

// export answers the published declarations as YAML, or as the canonical
// JSON with ?format=json: no secret, no revoked key, no default (OPS-3).
func (s *server) export(w http.ResponseWriter, r *http.Request) {
	q, err := query(r, "format")
	if err != nil {
		s.fail(w, err)
		return
	}
	doc := s.cfg.Snapshot().Doc
	switch q.Get("format") {
	case "json":
		write(w, http.StatusOK, doc)
	case "", "yaml":
		data, err := config.EncodeYAML(doc)
		if err != nil {
			s.fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(data)
	default:
		s.fail(w, invalid("format must be yaml or json"))
	}
}

// apply takes the YAML file as the body (OPS-3).
func (s *server) apply(w http.ResponseWriter, r *http.Request) {
	q, err := query(r, "dry_run", "prune")
	if err != nil {
		s.fail(w, err)
		return
	}
	dryRun, err := boolean(q, "dry_run")
	if err != nil {
		s.fail(w, err)
		return
	}
	prune, err := boolean(q, "prune")
	if err != nil {
		s.fail(w, err)
		return
	}
	data, err := body(w, r, config.MaxYAMLBytes)
	if err != nil {
		s.fail(w, err)
		return
	}
	var file domain.Document
	if err := config.DecodeYAML(data, &file); err != nil {
		s.fail(w, invalid("the file is not a configuration: %v", err))
		return
	}
	res, err := s.cfg.Apply(r.Context(), &file, prune, dryRun)
	if err != nil {
		s.fail(w, err)
		return
	}
	write(w, http.StatusOK, res)
}

// validate answers the findings of the published configuration, in the
// object validate -f prints; error findings do not change the status.
func (s *server) validate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), lookupTimeout)
	defer cancel()
	write(w, http.StatusOK, struct {
		Findings []domain.Finding `json:"findings"`
	}{s.cfg.Check(ctx)})
}

// showUsage answers usage show (USE-9, phase-3 design section 12) from the
// store's read pool: since is a positive duration back from now on the
// service clock, such as 24h (the default), or an RFC 3339 time before now
// and not before 1970 (completed_at is Unix milliseconds); key, model and target filter; by groups by key, model, target and
// effort-sent, comma-separated; compare_previous adds the window before.
// The published retention.usage_days decides where each window is read. A
// read: not audited.
func (s *server) showUsage(w http.ResponseWriter, r *http.Request) {
	q, err := query(r, "since", "key", "model", "target", "by", "compare_previous")
	if err != nil {
		s.fail(w, err)
		return
	}
	compare, err := boolean(q, "compare_previous")
	if err != nil {
		s.fail(w, err)
		return
	}
	now := s.cfg.Now()
	since := now.Add(-24 * time.Hour)
	if v := q.Get("since"); v != "" {
		d, errDuration := time.ParseDuration(v)
		at, errTime := time.Parse(time.RFC3339, v)
		switch {
		case errDuration == nil && d > 0:
			since = now.Add(-d)
		case errTime == nil && at.Before(now):
			since = at
		default:
			since = time.Time{}
		}
		// completed_at is Unix milliseconds: a window cannot start before 1970,
		// whichever form named it.
		if since.Before(time.Unix(0, 0)) {
			s.fail(w, invalid("since must be a positive duration such as 24h, or an RFC 3339 time before now, and not before 1970"))
			return
		}
	}
	var by []string
	if v := q.Get("by"); v != "" {
		by = strings.Split(v, ",")
	}
	rep, err := usage.Show(r.Context(), s.usage, usage.Query{Since: since, Key: q.Get("key"), Model: q.Get("model"), Target: q.Get("target"), By: by,
		ComparePrevious: compare}, now, s.cfg.Snapshot().Settings.Retention.UsageDays)
	if err != nil {
		s.fail(w, err)
		return
	}
	write(w, http.StatusOK, rep)
}

// quotaNote says what a quota window counts (phase-3 design section 9).
const quotaNote = "counts this proxy's traffic only"

// showQuota answers usage quota (phase-3 design section 9): every window of
// every subscription provider's quota, then of every key's share of one, as
// the published configuration declares them, with the requests or tokens
// used, the limit, the percentage used and, once exhausted, the seconds
// until it admits again. A window declared since the start and not used yet
// is seeded from the store first. A read: not audited.
func (s *server) showQuota(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		s.fail(w, invalid("this route takes no query parameter"))
		return
	}
	snap := s.cfg.Snapshot()
	if err := s.limits.Sync(snap.Revision, snap.Doc.Providers, snap.Doc.Keys); err != nil {
		s.fail(w, fmt.Errorf("quota windows not seeded: %w", err))
		return
	}
	type window struct {
		Provider    string  `json:"provider"`
		Key         string  `json:"key,omitempty"`
		Share       float64 `json:"share,omitzero"`
		Window      string  `json:"window"`
		Unit        string  `json:"unit"`
		Used        int64   `json:"used"`
		Limit       int64   `json:"limit"`
		UsedPct     float64 `json:"used_pct"`
		RetryAfterS int64   `json:"retry_after_s,omitzero"`
	}
	ws := []window{}
	for _, v := range s.limits.Windows() {
		pct := 100.0 // a limit of 0 or below admits nothing
		if v.Limit > 0 {
			pct = math.Round(float64(v.Used)*1000/float64(v.Limit)) / 10
		}
		ws = append(ws, window{v.Provider, v.Key, v.Share, v.Window, v.Unit, v.Used, v.Limit, pct, int64(v.RetryAfter / time.Second)})
	}
	write(w, http.StatusOK, struct {
		Note    string   `json:"note"`
		Windows []window `json:"windows"`
	}{quotaNote, ws})
}
