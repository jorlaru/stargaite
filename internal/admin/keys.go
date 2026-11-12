package admin

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
)

// defaultGrace is how long a rotated secret stays valid (SEC-8).
const defaultGrace = "1h"

// keyView is a key as a read shows it: its name, its declaration and the
// public id and end of each secret, never a secret or its hash.
type keyView struct {
	Name string `json:"name"`
	domain.Key
	Secrets []config.SecretView `json:"secrets"`
}

func viewKey(k *config.Key) keyView { return keyView{k.Name, k.Spec, k.Secrets} }

func noKey(name string) *domain.Error {
	return domain.NewError(domain.CodeNotFound, fmt.Sprintf("key %.64q does not exist", name))
}

// findKey returns the stored declaration of key name: 404 for a name never
// declared, 409 for a revoked one, which no command can change again.
func findKey(st *config.State, name string) (domain.Key, error) {
	if _, revoked := st.Revoked[name]; revoked {
		return domain.Key{}, domain.NewError(domain.CodeConflict, fmt.Sprintf("key %.64q is revoked", name))
	}
	k, ok := st.Doc.Keys[name]
	if !ok {
		return k, noKey(name)
	}
	return k, nil
}

func (s *server) listKeys(w http.ResponseWriter, _ *http.Request) {
	snap := s.cfg.Snapshot()
	list := []keyView{}
	for _, name := range slices.Sorted(maps.Keys(snap.Keys)) {
		list = append(list, viewKey(snap.Keys[name]))
	}
	write(w, http.StatusOK, struct {
		Keys []keyView `json:"keys"`
	}{list})
}

// showKey reads a key; a revoked one is absent from the snapshot, so 404.
func (s *server) showKey(w http.ResponseWriter, r *http.Request) {
	k := s.cfg.Snapshot().Keys[r.PathValue("name")]
	if k == nil {
		s.fail(w, noKey(r.PathValue("name")))
		return
	}
	write(w, http.StatusOK, viewKey(k))
}

// keyChanged answers an accepted change of key name with the result and the
// key as published; a revoked key has no resource.
func (s *server) keyChanged(w http.ResponseWriter, status int, res config.Result, name string) {
	out := written{Result: res}
	if k := s.cfg.Snapshot().Keys[name]; k != nil {
		out.Resource = viewKey(k)
	}
	write(w, status, out)
}

// createKey is "keys create": the body is the declaration plus name, and
// models is required (Validate refuses a key without it: no access by
// omission). The key gets its first secret, whose token the result shows once.
func (s *server) createKey(w http.ResponseWriter, r *http.Request) {
	var name string
	patch, err := addBody(w, r, "name", &name, nil)
	if err != nil {
		s.fail(w, err)
		return
	}
	res, err := s.cfg.Commit(r.Context(), config.Change{Summary: "keys create " + name, Kind: config.KindKey, Name: name, Mutate: func(st *config.State) (map[string]string, error) {
		if _, revoked := st.Revoked[name]; revoked {
			return nil, domain.NewError(domain.CodeConflict, fmt.Sprintf("key %.64q is revoked, and a revoked name is never reused", name))
		}
		if _, ok := st.Doc.Keys[name]; ok {
			return nil, domain.NewError(domain.CodeConflict, fmt.Sprintf("key %.64q exists", name))
		}
		var spec domain.Key
		if err := merge(&spec, patch); err != nil {
			return nil, err
		}
		st.Doc.Keys[name] = spec
		token, err := st.NewSecret(name, s.cfg.Now().UnixMilli())
		return map[string]string{name: token}, err
	}})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.keyChanged(w, http.StatusCreated, res, name)
}

// setKey merges the patch into the stored declaration; the secrets stay.
func (s *server) setKey(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	patch, err := patchBody(w, r)
	if err != nil {
		s.fail(w, err)
		return
	}
	res, err := s.cfg.Commit(r.Context(), config.Change{Summary: "keys set " + name, Kind: config.KindKey, Name: name, Mutate: func(st *config.State) (map[string]string, error) {
		k, err := findKey(st, name)
		if err != nil {
			return nil, err
		}
		if err := merge(&k, patch); err != nil {
			return nil, err
		}
		st.Doc.Keys[name] = k
		return nil, nil
	}})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.keyChanged(w, http.StatusOK, res, name)
}

// actionBody reads the optional body of a key action into v: nothing, or a
// JSON object whose members the strict decoder accepts. What it refuses is
// refused before the pipeline, so it is not audited.
func actionBody(w http.ResponseWriter, r *http.Request, v any) error {
	data, err := body(w, r, maxBody)
	if err != nil || len(data) == 0 {
		return err
	}
	if err := config.Decode(data, v); err != nil {
		return invalid("%v", err)
	}
	return nil
}

// rotateKey is "keys rotate" (SEC-8): a new secret, shown once, and the
// secret in use valid for the grace period, {"grace":"1h"} by default.
func (s *server) rotateKey(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Grace string `json:"grace"`
	}
	if err := actionBody(w, r, &req); err != nil {
		s.fail(w, err)
		return
	}
	grace, err := time.ParseDuration(cmp.Or(req.Grace, defaultGrace))
	if err != nil || grace < 0 {
		s.fail(w, invalid("grace must be a duration of 0 or more, such as 1h or 30m"))
		return
	}
	res, err := s.cfg.Commit(r.Context(), config.Change{Summary: "keys rotate " + name, Kind: config.KindKey, Name: name, Mutate: func(st *config.State) (map[string]string, error) {
		if _, err := findKey(st, name); err != nil {
			return nil, err
		}
		now := s.cfg.Now().UnixMilli()
		token, err := st.Rotate(name, now, now+grace.Milliseconds())
		return map[string]string{name: token}, err
	}})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.keyChanged(w, http.StatusOK, res, name)
}

// revokeKey is "keys revoke": every secret is deleted and the next request
// loads a snapshot without the key; the name is never reused.
func (s *server) revokeKey(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := actionBody(w, r, &struct{}{}); err != nil {
		s.fail(w, err)
		return
	}
	res, err := s.cfg.Commit(r.Context(), config.Change{Summary: "keys revoke " + name, Kind: config.KindKey, Name: name, Mutate: func(st *config.State) (map[string]string, error) {
		if _, err := findKey(st, name); err != nil {
			return nil, err
		}
		st.Revoke(name, s.cfg.Now().UnixMilli())
		return nil, nil
	}})
	if err != nil {
		s.fail(w, err)
		return
	}
	write(w, http.StatusOK, res)
}
