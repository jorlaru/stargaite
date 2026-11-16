package config

import (
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jorlaru/stargaite/internal/auth"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/transport"
)

// Snapshot is one published configuration (OPS-4). Only build makes one, and
// nothing changes it afterwards: a request loads it once and keeps it.
type Snapshot struct {
	Revision  int64
	Doc       *domain.Document // the committed declarations, for export and reads
	Settings  domain.Settings  // defaults applied
	Providers map[string]*Provider
	Targets   map[string]*Target
	Models    map[string]*Model
	Keys      map[string]*Key  // by name; revoked keys are absent
	Findings  []domain.Finding // the findings of the published configuration
	// NeverSince is, by scope, the revision the counter of a never budget
	// started at (State.NeverSince): admission.Budget.Since.
	NeverSince map[string]int64
	// Verified is State.Verified: by target id, the last successful
	// targets verify (RSN-9).
	Verified map[string]Verification
	// Exporter is the OTLP exporter as published, nil without an
	// otlp_endpoint; the exporter reads it per batch (phase-3 design section 10).
	Exporter *Exporter
	secrets  map[string]secret
}

// Exporter is the telemetry exporter as published: credentials re-resolve on
// every publish, like provider ones (OPS-6, SEC-5).
type Exporter struct {
	URL      string      // otlp_endpoint + "/v1/traces" (TEL-2)
	Header   http.Header // the static headers
	Revision int64       // the revision that published it, which a test span carries (TEL-13)
	// Secrets are by canonical header name: Authorization "Basic
	// base64(user:password)" from basic_auth_env, and each header_env.
	Secrets map[string]transport.Secret
	// Redact holds every secret value, longest first: each of Secrets and the
	// parts of the composed Authorization a backend's message may echo on
	// their own (the base64 text, the user and the password). Longest first,
	// a secret that contains another is never left half redacted (SEC-5).
	Redact       []transport.Secret
	AllowPrivate bool
	Unresolved   []string // the references that do not resolve, by name, sorted
}

// exporter composes the Exporter of t. A reference resolves as for a
// provider (the test of CheckResolved); one that does not is listed in
// Unresolved and sets no header.
func exporter(t *domain.Telemetry, env Env, revision int64) *Exporter {
	if t == nil || t.OTLPEndpoint == "" {
		return nil
	}
	e := &Exporter{URL: strings.TrimSuffix(t.OTLPEndpoint, "/") + "/v1/traces", Header: http.Header{}, Revision: revision,
		Secrets: map[string]transport.Secret{}, AllowPrivate: t.AllowPrivateNetwork}
	for name, v := range t.Headers {
		e.Header.Set(name, v)
	}
	lookup := func(ref string) (string, bool) {
		v, ok := "", false
		if validReference(ref) {
			v, ok = env.Lookup(ref)
		}
		if !ok || v == "" {
			e.Unresolved = append(e.Unresolved, ref)
			return "", false
		}
		return v, true
	}
	var values []string
	if len(t.BasicAuthEnv) == 2 {
		user, okUser := lookup(t.BasicAuthEnv[0])
		password, okPassword := lookup(t.BasicAuthEnv[1])
		if okUser && okPassword {
			encoded := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
			e.Secrets["Authorization"] = transport.NewSecret("Basic " + encoded)
			values = append(values, "Basic "+encoded, encoded, user, password)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(t.HeaderEnv)) {
		if v, ok := lookup(t.HeaderEnv[name]); ok {
			e.Secrets[http.CanonicalHeaderKey(name)] = transport.NewSecret(v)
			values = append(values, v)
		}
	}
	slices.SortStableFunc(values, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, v := range values {
		e.Redact = append(e.Redact, transport.NewSecret(v))
	}
	slices.Sort(e.Unresolved)
	e.Unresolved = slices.Compact(e.Unresolved)
	return e
}

// Provider is a provider as published. Credential is set when api_key_env
// resolves to a value; a reference that does not resolve leaves the provider
// Disabled. A provider without api_key_env sends no credential. Timeouts
// are the timers of a call to one of its targets: each one the provider
// declares, else the published setting (RES-3).
type Provider struct {
	Name       string
	Spec       domain.Provider
	Credential transport.Secret
	Disabled   bool
	Timeouts   domain.Timeouts
}

// Target is a target as published, with its provider.
type Target struct {
	ID, Upstream string
	Spec         domain.Target
	Provider     *Provider
}

// Model is an alias as published, with its targets in order: the primary,
// then the fallbacks. Declared holds every parameter a rule of those targets
// or of their providers names, which plan.Check lets through (PAR-2).
type Model struct {
	Name     string
	Spec     domain.Model
	Targets  []*Target
	Declared map[string]bool
}

// Key is a client key as published. Its secrets are not reachable from it:
// Secrets shows their public ids and ends, which a read lists, never a hash.
type Key struct {
	Name    string
	Spec    domain.Key
	Secrets []SecretView
}

// Allows reports whether the key may use alias: it is listed, or the list is
// ["*"] (SEC-2). An empty list reaches nothing.
func (k *Key) Allows(alias string) bool {
	return slices.Equal(k.Spec.Models, []string{"*"}) || slices.Contains(k.Spec.Models, alias)
}

// dummyHash is what a token with an unknown public id is compared against, so
// that an unknown id costs the same comparison as a known one.
var dummyHash = sha256.Sum256([]byte("stargaite: no secret has this public id"))

// Authenticate returns the key a client token belongs to. The token must have
// the shape of a client key, its public id must be published and its hash
// must match, and the secret must not have reached its valid_until: a secret
// in its grace is valid until then, and not at that instant (SEC-8). Any
// failure is the same false, which the caller answers 401 invalid_api_key.
func (s *Snapshot) Authenticate(token string, now time.Time) (*Key, bool) {
	id, ok := auth.Parse(token)
	if !ok {
		return nil, false
	}
	sec, found := s.secrets[id]
	hash := sec.hash
	if !found {
		hash = dummyHash
	}
	if !auth.Equal(token, hash) || !found || (sec.validUntil != 0 && now.UnixMilli() >= sec.validUntil) {
		return nil, false
	}
	return sec.key, true
}

// secret is one secret of a key, found by its public id.
type secret struct {
	hash       [32]byte
	validUntil int64 // Unix milliseconds; 0 is no end
	key        *Key
}

// build publishes st. A stored configuration that a newer binary finds
// invalid is still published (design section 13, point 13), so a reference to
// something missing is skipped here rather than trusted.
func build(st *State, env Env, findings []domain.Finding) *Snapshot {
	d := &st.Doc
	s := &Snapshot{
		Revision:   st.Revision,
		Doc:        d,
		Settings:   WithDefaults(d.Settings),
		Providers:  make(map[string]*Provider, len(d.Providers)),
		Targets:    make(map[string]*Target, len(d.Targets)),
		Models:     make(map[string]*Model, len(d.Models)),
		Keys:       make(map[string]*Key, len(d.Keys)),
		Findings:   findings,
		NeverSince: st.NeverSince,
		Verified:   st.Verified,
		Exporter:   exporter(d.Settings.Telemetry, env, st.Revision),
		secrets:    make(map[string]secret, len(st.Secrets)),
	}
	set := s.Settings.Timeouts
	for name, spec := range d.Providers {
		t := spec.Timeouts
		p := &Provider{Name: name, Spec: spec, Timeouts: domain.Timeouts{
			ConnectMS:    cmp.Or(t.ConnectMS, set.ConnectMS),
			FirstByteMS:  cmp.Or(t.FirstByteMS, set.FirstByteMS),
			StreamIdleMS: cmp.Or(t.StreamIdleMS, set.StreamIdleMS),
			TotalMS:      cmp.Or(t.TotalMS, set.TotalMS),
		}}
		if ref := spec.APIKeyEnv; ref != "" {
			// The same test as the credential_unresolved warning of CheckResolved.
			v, ok := "", false
			if validReference(ref) {
				v, ok = env.Lookup(ref)
			}
			if ok && v != "" {
				p.Credential = transport.NewSecret(v)
			} else {
				p.Disabled = true
			}
		}
		s.Providers[name] = p
	}
	for id, spec := range d.Targets {
		provider, upstream, _ := domain.SplitTarget(id)
		if s.Providers[provider] == nil {
			continue // so Target.Provider is never nil (phase-1 design section 9)
		}
		s.Targets[id] = &Target{ID: id, Upstream: upstream, Spec: spec, Provider: s.Providers[provider]}
	}
	for name, spec := range d.Models {
		m := &Model{Name: name, Spec: spec, Declared: map[string]bool{}}
		for _, id := range spec.Targets {
			if t := s.Targets[id]; t != nil {
				m.Targets = append(m.Targets, t)
				for k := range t.Spec.Params {
					m.Declared[k] = true
				}
				for k := range t.Provider.Spec.Params {
					m.Declared[k] = true
				}
			}
		}
		s.Models[name] = m
	}
	views := secretsByKey(st.Secrets)
	for name, spec := range d.Keys {
		// LIM-7: a key that declares no parallel limit gets the default at
		// publish; Doc keeps the declaration for export.
		spec.MaxParallelRequests = cmp.Or(spec.MaxParallelRequests, domain.DefaultKeyMaxParallelRequests)
		s.Keys[name] = &Key{Name: name, Spec: spec, Secrets: views[name]}
	}
	for _, sec := range st.Secrets {
		if k := s.Keys[sec.KeyName]; k != nil {
			s.secrets[sec.PublicID] = secret{hash: sec.Hash, validUntil: sec.ValidUntil, key: k}
		}
	}
	return s
}

// WithDefaults gives every undeclared setting that has a default its default
// (design section 13, point 15), the telemetry ones with or without a block
// (phase-3 design section 2). A declaration is never changed: export reads
// Doc.
func WithDefaults(s domain.Settings) domain.Settings {
	s.MaxBodyBytes = cmp.Or(s.MaxBodyBytes, domain.DefaultMaxBodyBytes)
	s.MaxResponseBytes = cmp.Or(s.MaxResponseBytes, domain.DefaultMaxResponseBytes)
	t := cmp.Or(s.Timeouts, &domain.Timeouts{})
	s.Timeouts = &domain.Timeouts{
		ConnectMS:    cmp.Or(t.ConnectMS, domain.DefaultConnectMS),
		FirstByteMS:  cmp.Or(t.FirstByteMS, domain.DefaultFirstByteMS),
		StreamIdleMS: cmp.Or(t.StreamIdleMS, domain.DefaultStreamIdleMS),
		TotalMS:      cmp.Or(t.TotalMS, domain.DefaultTotalMS),
	}
	r := cmp.Or(s.Retention, &domain.Retention{})
	s.Retention = &domain.Retention{
		UsageDays:        cmp.Or(r.UsageDays, domain.DefaultUsageDays),
		UsageDailyMonths: cmp.Or(r.UsageDailyMonths, domain.DefaultUsageDailyMonths),
		AuditMonths:      cmp.Or(r.AuditMonths, domain.DefaultAuditMonths),
	}
	tel := *cmp.Or(s.Telemetry, &domain.Telemetry{})
	if tel.TrustTraceparent == nil {
		tel.TrustTraceparent = new(domain.DefaultTrustTraceparent)
	}
	tel.ContentMaxBytes = cmp.Or(tel.ContentMaxBytes, domain.DefaultContentMaxBytes)
	s.Telemetry = &tel
	return s
}
