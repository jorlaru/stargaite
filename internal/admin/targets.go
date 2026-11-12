package admin

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
)

// targetView is a target as a read shows it: its id and its declaration.
type targetView struct {
	ID string `json:"id"`
	domain.Target
}

// modelView is a model alias as a read shows it: its name and its declaration.
type modelView struct {
	Name string `json:"name"`
	domain.Model
}

func noTarget(id string) *domain.Error {
	return domain.NewError(domain.CodeNotFound, fmt.Sprintf("target %.200q does not exist", id))
}

func noModel(name string) *domain.Error {
	return domain.NewError(domain.CodeNotFound, fmt.Sprintf("alias %.64q does not exist", name))
}

// newTarget is t as a new target of id starts: with the dialect and the
// output-cap parameter of its provider's target_defaults (RSN-2). A provider
// without them gives nothing, so the dialect stays undeclared, which is none.
// Only targets add and models add call it; apply takes the file as it is.
func newTarget(doc *domain.Document, id string, t domain.Target) domain.Target {
	provider, _, _ := domain.SplitTarget(id)
	d := doc.Providers[provider].TargetDefaults
	t.OutputCapParam = d.OutputCapParam
	if d.Dialect != "" {
		t.Reasoning = &domain.Reasoning{Dialect: d.Dialect}
	}
	return t
}

// mergeTarget merges patch into t, except that a reasoning on or off fragment
// the patch carries as an object replaces the stored one as given: a fragment
// is sent verbatim (kick-off section 13.3), so it is neither joined with the
// stored one nor stripped of the nulls inside it, and {} declares "send
// nothing". A null fragment is left to the merge, which removes it.
func mergeTarget(t *domain.Target, patch jsontext.Value) error {
	if err := merge(t, patch); err != nil {
		return err
	}
	var given struct {
		Reasoning domain.Reasoning `json:"reasoning"`
	}
	if err := json.Unmarshal(patch, &given); err != nil {
		return invalid("%v", err)
	}
	// The merge succeeded, so t.Reasoning is set when the patch has a fragment.
	if given.Reasoning.On != nil {
		t.Reasoning.On = given.Reasoning.On
	}
	if given.Reasoning.Off != nil {
		t.Reasoning.Off = given.Reasoning.Off
	}
	return nil
}

func (s *server) listTargets(w http.ResponseWriter, _ *http.Request) {
	doc := s.cfg.Snapshot().Doc
	list := []targetView{}
	for _, id := range slices.Sorted(maps.Keys(doc.Targets)) {
		list = append(list, targetView{id, doc.Targets[id]})
	}
	write(w, http.StatusOK, struct {
		Targets []targetView `json:"targets"`
	}{list})
}

// showTarget reads {id...}: a target id holds "/", sent as is or escaped.
func (s *server) showTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, ok := s.cfg.Snapshot().Doc.Targets[id]
	if !ok {
		s.fail(w, noTarget(id))
		return
	}
	write(w, http.StatusOK, targetView{id, t})
}

// addTarget is "targets add": the body is the declaration plus id. The new
// target starts from its provider's target_defaults, and the explicit
// members are merged over them, so null removes a default.
func (s *server) addTarget(w http.ResponseWriter, r *http.Request) {
	var id string
	patch, err := addBody(w, r, "id", &id, nil)
	if err != nil {
		s.fail(w, err)
		return
	}
	var spec domain.Target
	res, err := s.cfg.Commit(r.Context(), config.Change{Summary: "targets add " + id, Kind: config.KindTarget, Name: id, Mutate: func(st *config.State) (map[string]string, error) {
		if _, ok := st.Doc.Targets[id]; ok {
			return nil, domain.NewError(domain.CodeConflict, fmt.Sprintf("target %.200q exists", id))
		}
		spec = newTarget(&st.Doc, id, domain.Target{})
		if err := mergeTarget(&spec, patch); err != nil {
			return nil, err
		}
		st.Doc.Targets[id] = spec
		return nil, nil
	}})
	if err != nil {
		s.fail(w, err)
		return
	}
	write(w, http.StatusCreated, written{res, targetView{id, spec}})
}

// setTarget merges the patch into the stored declaration, but for the
// reasoning on and off fragments (see mergeTarget).
func (s *server) setTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	patch, err := patchBody(w, r)
	if err != nil {
		s.fail(w, err)
		return
	}
	var spec domain.Target
	res, err := s.cfg.Commit(r.Context(), config.Change{Summary: "targets set " + id, Kind: config.KindTarget, Name: id, Mutate: func(st *config.State) (map[string]string, error) {
		var ok bool
		if spec, ok = st.Doc.Targets[id]; !ok {
			return nil, noTarget(id)
		}
		if err := mergeTarget(&spec, patch); err != nil {
			return nil, err
		}
		st.Doc.Targets[id] = spec
		return nil, nil
	}})
	if err != nil {
		s.fail(w, err)
		return
	}
	write(w, http.StatusOK, written{res, targetView{id, spec}})
}

// removeTarget deletes a target. One that an alias still lists is refused by
// Validate, whose findings name those aliases.
func (s *server) removeTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, err := s.cfg.Commit(r.Context(), config.Change{Summary: "targets remove " + id, Kind: config.KindTarget, Name: id, Mutate: func(st *config.State) (map[string]string, error) {
		if _, ok := st.Doc.Targets[id]; !ok {
			return nil, noTarget(id)
		}
		delete(st.Doc.Targets, id)
		return nil, nil
	}})
	if err != nil {
		s.fail(w, err)
		return
	}
	write(w, http.StatusOK, res)
}

func (s *server) listModels(w http.ResponseWriter, _ *http.Request) {
	doc := s.cfg.Snapshot().Doc
	list := []modelView{}
	for _, name := range slices.Sorted(maps.Keys(doc.Models)) {
		list = append(list, modelView{name, doc.Models[name]})
	}
	write(w, http.StatusOK, struct {
		Models []modelView `json:"models"`
	}{list})
}

func (s *server) showModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	m, ok := s.cfg.Snapshot().Doc.Models[name]
	if !ok {
		s.fail(w, noModel(name))
		return
	}
	write(w, http.StatusOK, modelView{name, m})
}

// addModel is "models add": the body is the declaration plus name. A target
// it lists that does not exist yet is created in the same change, as kind
// chat with its provider's target_defaults and nothing else, and the diff
// reports it (kick-off section 13.3).
func (s *server) addModel(w http.ResponseWriter, r *http.Request) {
	var name string
	patch, err := addBody(w, r, "name", &name, nil)
	if err != nil {
		s.fail(w, err)
		return
	}
	var spec domain.Model
	res, err := s.cfg.Commit(r.Context(), config.Change{Summary: "models add " + name, Kind: config.KindModel, Name: name, Mutate: func(st *config.State) (map[string]string, error) {
		if _, ok := st.Doc.Models[name]; ok {
			return nil, domain.NewError(domain.CodeConflict, fmt.Sprintf("alias %.64q exists", name))
		}
		if err := merge(&spec, patch); err != nil {
			return nil, err
		}
		for _, id := range spec.Targets {
			if _, ok := st.Doc.Targets[id]; !ok {
				st.Doc.Targets[id] = newTarget(&st.Doc, id, domain.Target{Kind: domain.KindChat})
			}
		}
		st.Doc.Models[name] = spec
		return nil, nil
	}})
	if err != nil {
		s.fail(w, err)
		return
	}
	write(w, http.StatusCreated, written{res, modelView{name, spec}})
}

// setModel merges the patch into the stored declaration; a target it names
// must exist.
func (s *server) setModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	patch, err := patchBody(w, r)
	if err != nil {
		s.fail(w, err)
		return
	}
	var spec domain.Model
	res, err := s.cfg.Commit(r.Context(), config.Change{Summary: "models set " + name, Kind: config.KindModel, Name: name, Mutate: func(st *config.State) (map[string]string, error) {
		var ok bool
		if spec, ok = st.Doc.Models[name]; !ok {
			return nil, noModel(name)
		}
		if err := merge(&spec, patch); err != nil {
			return nil, err
		}
		st.Doc.Models[name] = spec
		return nil, nil
	}})
	if err != nil {
		s.fail(w, err)
		return
	}
	write(w, http.StatusOK, written{res, modelView{name, spec}})
}

// removeModel deletes an alias. A key that still lists it only gets a
// warning (design section 13, point 25).
func (s *server) removeModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	res, err := s.cfg.Commit(r.Context(), config.Change{Summary: "models remove " + name, Kind: config.KindModel, Name: name, Mutate: func(st *config.State) (map[string]string, error) {
		if _, ok := st.Doc.Models[name]; !ok {
			return nil, noModel(name)
		}
		delete(st.Doc.Models, name)
		return nil, nil
	}})
	if err != nil {
		s.fail(w, err)
		return
	}
	write(w, http.StatusOK, res)
}
