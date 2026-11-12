package config

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"maps"
	"slices"

	"github.com/jorlaru/stargaite/internal/domain"
)

// Apply is "apply -f" (OPS-3), through Commit. Every resource the file names
// is replaced by the file's declaration as a whole; each top-level member of
// settings is one resource. A key the file creates gets a new secret, which
// the result shows once; an existing key keeps its secrets; a revoked name is
// 409 conflict. With prune, absent providers, targets and aliases are
// removed, absent keys revoked and absent settings cleared. The file's
// version goes into the checked document, so Validate refuses any but 1.
func (s *Service) Apply(ctx context.Context, file *domain.Document, prune, dryRun bool) (Result, error) {
	summary := "apply"
	if prune {
		summary = "apply --prune"
	}
	return s.Commit(ctx, Change{Summary: summary, Kind: "configuration", DryRun: dryRun, Mutate: func(st *State) (map[string]string, error) {
		d := &st.Doc
		d.Version = file.Version

		settings := map[string]jsontext.Value{}
		for i, from := range []domain.Settings{d.Settings, file.Settings} {
			if i == 0 && prune {
				continue // the stored members are cleared
			}
			m, err := members(from)
			if err != nil {
				return nil, err
			}
			maps.Copy(settings, m)
		}
		data, err := Encode(settings)
		if err != nil {
			return nil, err
		}
		d.Settings = domain.Settings{}
		if err := Decode(data, &d.Settings); err != nil {
			return nil, err
		}

		if prune {
			clear(d.Providers)
			clear(d.Targets)
			clear(d.Models)
		}
		maps.Copy(d.Providers, file.Providers)
		maps.Copy(d.Targets, file.Targets)
		maps.Copy(d.Models, file.Models)

		now := s.now().UnixMilli()
		issued := map[string]string{}
		for _, name := range slices.Sorted(maps.Keys(file.Keys)) {
			if _, revoked := st.Revoked[name]; revoked {
				return nil, domain.NewError(domain.CodeConflict, fmt.Sprintf("key %.64q is revoked, and a revoked name is never reused", name))
			}
			if _, exists := d.Keys[name]; !exists {
				token, err := st.NewSecret(name, now)
				if err != nil {
					return nil, err
				}
				issued[name] = token
			}
			d.Keys[name] = file.Keys[name]
		}
		if prune {
			for name := range d.Keys {
				if _, named := file.Keys[name]; !named {
					st.Revoke(name, now)
				}
			}
		}
		return issued, nil
	}})
}

// Check is "validate" on the published configuration: the checks of a change,
// with every outbound URL resolved, so it gives the verdict validate -f gives
// on the exported file (design section 13, point 14).
func (s *Service) Check(ctx context.Context) []domain.Finding {
	doc := s.Snapshot().Doc
	return append(Validate(doc), CheckResolved(ctx, doc, OutboundURLs(doc), s.env())...)
}
