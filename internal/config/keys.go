package config

import (
	"crypto/rand"
	"slices"

	"github.com/jorlaru/stargaite/internal/auth"
)

// NewSecret issues a secret for key name, created at (Unix milliseconds), and
// returns its token. Only the hash is kept; the token goes back to the client
// once, in Result.Secrets (SEC-1, OPS-3).
func (st *State) NewSecret(name string, at int64) (string, error) {
	token, id, hash, err := auth.Issue(rand.Reader)
	if err != nil {
		return "", err
	}
	st.Secrets = append(st.Secrets, KeySecret{PublicID: id, KeyName: name, Hash: hash, CreatedAt: at})
	return token, nil
}

// Rotate issues a new secret for key name (SEC-8). The secret in use, the one
// without an end, stays valid until validUntil; every other secret of the key
// is already in its grace or expired, and is deleted at once, so a key never
// has more than two.
func (st *State) Rotate(name string, at, validUntil int64) (string, error) {
	st.Secrets = slices.DeleteFunc(st.Secrets, func(s KeySecret) bool { return s.KeyName == name && s.ValidUntil != 0 })
	for i := range st.Secrets {
		if st.Secrets[i].KeyName == name {
			st.Secrets[i].ValidUntil = validUntil
		}
	}
	return st.NewSecret(name, at)
}

// Revoke deletes every secret of key name and moves the name from Doc to
// Revoked, revoked at at: the row stays, so the name is never reused.
func (st *State) Revoke(name string, at int64) {
	delete(st.Doc.Keys, name)
	st.Revoked[name] = at
	st.Secrets = slices.DeleteFunc(st.Secrets, func(s KeySecret) bool { return s.KeyName == name })
}
