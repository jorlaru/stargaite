package admin

import (
	"cmp"
	"encoding/json/jsontext"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/presets"
)

// The credential status a provider read shows (OPS-6): its reference
// resolves, does not resolve (the provider is disabled), or it has none.
const (
	credentialResolved   = "resolved"
	credentialUnresolved = "unresolved"
	credentialNone       = "none"
)

// providerView is a provider as a read shows it: its name, its declaration
// and its credential status, never the credential's value.
type providerView struct {
	Name string `json:"name"`
	domain.Provider
	Credential string `json:"credential"`
}

func viewProvider(p *config.Provider) providerView {
	status := credentialResolved
	switch {
	case p.Spec.APIKeyEnv == "":
		status = credentialNone
	case p.Disabled:
		status = credentialUnresolved
	}
	return providerView{p.Name, p.Spec, status}
}

func noProvider(name string) *domain.Error {
	return domain.NewError(domain.CodeNotFound, fmt.Sprintf("provider %.64q does not exist", name))
}

// listPresets answers the built-in catalog (ADR-019).
func (s *server) listPresets(w http.ResponseWriter, _ *http.Request) {
	write(w, http.StatusOK, struct {
		Presets []presets.Preset `json:"presets"`
	}{presets.Catalog})
}

func (s *server) listProviders(w http.ResponseWriter, _ *http.Request) {
	snap := s.cfg.Snapshot()
	list := []providerView{}
	for _, name := range slices.Sorted(maps.Keys(snap.Providers)) {
		list = append(list, viewProvider(snap.Providers[name]))
	}
	write(w, http.StatusOK, struct {
		Providers []providerView `json:"providers"`
	}{list})
}

func (s *server) showProvider(w http.ResponseWriter, r *http.Request) {
	p := s.cfg.Snapshot().Providers[r.PathValue("name")]
	if p == nil {
		s.fail(w, noProvider(r.PathValue("name")))
		return
	}
	write(w, http.StatusOK, viewProvider(p))
}

// changed answers an accepted change of provider name with the result and the
// provider as published.
func (s *server) changed(w http.ResponseWriter, status int, res config.Result, name string) {
	out := written{Result: res}
	if p := s.cfg.Snapshot().Providers[name]; p != nil {
		out.Resource = viewProvider(p)
	}
	write(w, status, out)
}

// addProvider is "providers add" (kick-off section 7.2): the body is the
// declaration plus name and an optional preset. The preset named by preset,
// or else the one named like the provider, is the starting point, and the
// explicit members are merged over it; only the result is stored, so from
// then on the preset no longer counts.
func (s *server) addProvider(w http.ResponseWriter, r *http.Request) {
	data, err := body(w, r, maxBody)
	var members map[string]jsontext.Value
	if err == nil {
		if err = config.Decode(data, &members); err != nil {
			err = invalid("%v", err)
		}
	}
	var name, preset string
	for member, v := range map[string]*string{"name": &name, "preset": &preset} {
		if raw, ok := members[member]; ok && err == nil {
			if config.Decode(raw, v) != nil {
				err = invalid("%s must be a string", member)
			}
			delete(members, member)
		}
	}
	if err == nil && name == "" {
		err = invalid("name is required")
	}
	var patch []byte
	if err == nil {
		patch, err = config.Encode(members)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	res, err := s.cfg.Commit(r.Context(), config.Change{Summary: "providers add " + name, Kind: config.KindProvider, Name: name, Mutate: func(st *config.State) (map[string]string, error) {
		if _, ok := st.Doc.Providers[name]; ok {
			return nil, domain.NewError(domain.CodeConflict, fmt.Sprintf("provider %.64q exists", name))
		}
		var spec domain.Provider
		from := cmp.Or(preset, name)
		switch i := slices.IndexFunc(presets.Catalog, func(p presets.Preset) bool { return p.Name == from }); {
		case i >= 0:
			spec = presets.Catalog[i].Provider
		case preset != "":
			return nil, invalid("preset %.64q does not exist; providers presets lists them", preset)
		}
		if err := merge(&spec, patch); err != nil {
			return nil, err
		}
		st.Doc.Providers[name] = spec
		return nil, nil
	}})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.changed(w, http.StatusCreated, res, name)
}

// setProvider merges the patch into the stored declaration.
func (s *server) setProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	patch, err := patchBody(w, r)
	if err != nil {
		s.fail(w, err)
		return
	}
	res, err := s.cfg.Commit(r.Context(), config.Change{Summary: "providers set " + name, Kind: config.KindProvider, Name: name, Mutate: func(st *config.State) (map[string]string, error) {
		p, ok := st.Doc.Providers[name]
		if !ok {
			return nil, noProvider(name)
		}
		if err := merge(&p, patch); err != nil {
			return nil, err
		}
		st.Doc.Providers[name] = p
		return nil, nil
	}})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.changed(w, http.StatusOK, res, name)
}

// removeProvider deletes a provider. One that a target still names is
// refused by Validate, whose findings name those targets.
func (s *server) removeProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	res, err := s.cfg.Commit(r.Context(), config.Change{Summary: "providers remove " + name, Kind: config.KindProvider, Name: name, Mutate: func(st *config.State) (map[string]string, error) {
		if _, ok := st.Doc.Providers[name]; !ok {
			return nil, noProvider(name)
		}
		delete(st.Doc.Providers, name)
		return nil, nil
	}})
	if err != nil {
		s.fail(w, err)
		return
	}
	write(w, http.StatusOK, res)
}
