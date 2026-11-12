// Package auth is the format of a client key (SEC-1, ADR-011):
// sk-stargaite-<public id>-<secret>, both parts in lowercase base32 without
// padding, so neither holds a "-". Only the SHA-256 of a token is kept.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"io"
	"strings"
)

// Prefix starts every client key. An admin key may not start with it (SEC-3),
// so the two credentials never share a shape.
const Prefix = "sk-stargaite-"

// encoding is base32 with the lowercase alphabet a-z2-7; the standard
// alphabet is upper case.
var encoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Secret returns 32 bytes of r as 52 lowercase base32 characters: the secret
// part of a token, and a generated admin key.
func Secret(r io.Reader) (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return encoding.EncodeToString(b), nil
}

// Issue returns a new token, its public id (12 characters, 60 bits of r) and
// the SHA-256 of the whole token, which is all the store keeps. r is
// crypto/rand.Reader outside tests.
func Issue(r io.Reader) (token, publicID string, hash [32]byte, err error) {
	id := make([]byte, 8)
	if _, err = io.ReadFull(r, id); err != nil {
		return "", "", hash, err
	}
	secret, err := Secret(r)
	if err != nil {
		return "", "", hash, err
	}
	publicID = encoding.EncodeToString(id)[:12]
	token = Prefix + publicID + "-" + secret
	return token, publicID, sha256.Sum256([]byte(token)), nil
}

// Parse returns the public id of a token that has the shape of a client key:
// the prefix, a 12-character public id, "-" and a 52-character secret, both
// in the lowercase alphabet. It checks the shape only, never the secret, so
// the admin key, which may not start with the prefix (SEC-3), never passes.
func Parse(token string) (publicID string, ok bool) {
	rest, ok := strings.CutPrefix(token, Prefix)
	if !ok || len(rest) != 12+1+52 || rest[12] != '-' {
		return "", false
	}
	for i, c := range rest {
		if i != 12 && (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return "", false
		}
	}
	return rest[:12], true
}

// Equal reports whether the SHA-256 of token is hash, in constant time.
func Equal(token string, hash [32]byte) bool {
	h := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(h[:], hash[:]) == 1
}
