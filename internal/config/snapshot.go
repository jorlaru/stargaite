package config

import (
	"cmp"
	"crypto/sha256"
	"slices"
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
	secrets   map[string]secret
}

// Provider is a provider as published. Credential is set when api_key_env
// resolves to a value; a reference that does not resolve leaves the provider
// Disabled. A provider without api_key_env sends no credential.
type Provider struct {
	Name       string
	Spec       domain.Provider
	Credential transport.Secret
	Disabled   bool
}

// Target is a target as published, with its provider.
type Target struct {
	ID, Upstream string
	Spec         domain.Target
	Provider     *Provider
}

// Model is an alias as published, with its targets in order: the primary,
// then the fallbacks.
type Model struct {
	Name    string
	Spec    domain.Model
	Targets []*Target
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
		Revision:  st.Revision,
		Doc:       d,
		Settings:  WithDefaults(d.Settings),
		Providers: make(map[string]*Provider, len(d.Providers)),
		Targets:   make(map[string]*Target, len(d.Targets)),
		Models:    make(map[string]*Model, len(d.Models)),
		Keys:      make(map[string]*Key, len(d.Keys)),
		Findings:  findings,
		secrets:   make(map[string]secret, len(st.Secrets)),
	}
	for name, spec := range d.Providers {
		p := &Provider{Name: name, Spec: spec}
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
		m := &Model{Name: name, Spec: spec}
		for _, id := range spec.Targets {
			if t := s.Targets[id]; t != nil {
				m.Targets = append(m.Targets, t)
			}
		}
		s.Models[name] = m
	}
	views := secretsByKey(st.Secrets)
	for name, spec := range d.Keys {
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
// (design section 13, point 15). A declaration is never changed: export
// reads Doc.
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
	if s.Telemetry != nil {
		t := *s.Telemetry
		if t.TrustTraceparent == nil {
			t.TrustTraceparent = new(domain.DefaultTrustTraceparent)
		}
		t.ContentMaxBytes = cmp.Or(t.ContentMaxBytes, domain.DefaultContentMaxBytes)
		s.Telemetry = &t
	}
	return s
}
