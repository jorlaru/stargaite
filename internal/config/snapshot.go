package config

import (
	"cmp"

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

// Key is a client key as published. Its secrets are not reachable from it.
type Key struct {
	Name string
	Spec domain.Key
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
		Settings:  withDefaults(d.Settings),
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
	for name, spec := range d.Keys {
		s.Keys[name] = &Key{Name: name, Spec: spec}
	}
	for _, sec := range st.Secrets {
		if k := s.Keys[sec.KeyName]; k != nil {
			s.secrets[sec.PublicID] = secret{hash: sec.Hash, validUntil: sec.ValidUntil, key: k}
		}
	}
	return s
}

// withDefaults gives every undeclared setting that has a default its default
// (design section 13, point 15). A declaration is never changed: export
// reads Doc.
func withDefaults(s domain.Settings) domain.Settings {
	s.MaxBodyBytes = cmp.Or(s.MaxBodyBytes, domain.DefaultMaxBodyBytes)
	s.MaxResponseBytes = cmp.Or(s.MaxResponseBytes, domain.DefaultMaxResponseBytes)
	s.Timeouts.ConnectMS = cmp.Or(s.Timeouts.ConnectMS, domain.DefaultConnectMS)
	s.Timeouts.FirstByteMS = cmp.Or(s.Timeouts.FirstByteMS, domain.DefaultFirstByteMS)
	s.Timeouts.StreamIdleMS = cmp.Or(s.Timeouts.StreamIdleMS, domain.DefaultStreamIdleMS)
	s.Timeouts.TotalMS = cmp.Or(s.Timeouts.TotalMS, domain.DefaultTotalMS)
	s.Retention.UsageDays = cmp.Or(s.Retention.UsageDays, domain.DefaultUsageDays)
	s.Retention.UsageDailyMonths = cmp.Or(s.Retention.UsageDailyMonths, domain.DefaultUsageDailyMonths)
	s.Retention.AuditMonths = cmp.Or(s.Retention.AuditMonths, domain.DefaultAuditMonths)
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
