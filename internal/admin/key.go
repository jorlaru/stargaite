package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jorlaru/stargaite/internal/auth"
)

// KeyFile is the name of the generated admin key inside the data directory.
const KeyFile = "admin.key"

// Key returns the SHA-256 of the admin key (SEC-3): STARGAITE_ADMIN_KEY, else
// <dataDir>/admin.key, which it creates when missing: 52 lowercase base32
// characters written to a temporary file of mode 0600, synced, renamed into
// place and the directory synced, so the file is whole or absent. serve calls
// it under the data-directory lock, before the admin listener binds. A key of
// fewer than 32 characters, with leading or trailing whitespace, or with the
// shape of a client key, is refused
// (design section 13, point 17). No error carries the key.
func Key(dataDir string, getenv func(string) string) (hash [32]byte, created bool, err error) {
	key := getenv("STARGAITE_ADMIN_KEY")
	if key == "" {
		path := filepath.Join(dataDir, KeyFile)
		data, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			key, err = auth.Secret(rand.Reader)
			if err == nil {
				err = writeKey(path, key)
			}
			created = true
		case err == nil:
			key = strings.TrimSpace(string(data))
		}
		if err != nil {
			return hash, false, err
		}
	}
	switch {
	case len(key) < 32:
		return hash, false, errors.New("the admin key has fewer than 32 characters")
	case key != strings.TrimSpace(key):
		// No client can present it: HTTP trims a header value.
		return hash, false, errors.New("the admin key has leading or trailing whitespace")
	case strings.HasPrefix(key, auth.Prefix):
		return hash, false, errors.New("the admin key starts with " + auth.Prefix + ", the prefix of a client key")
	}
	return sha256.Sum256([]byte(key)), created, nil
}

// writeKey writes key to path atomically. os.CreateTemp opens the file with
// O_EXCL and mode 0600.
func writeKey(path, key string) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+KeyFile+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }() // gone after the rename; removes a failed attempt
	_, err = f.WriteString(key + "\n")
	if err = errors.Join(err, f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
