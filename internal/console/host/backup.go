package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Copy is a copy of the store: its path, relative to the clone for one in
// backups/, its size, and when it was taken, its modification time.
type Copy struct {
	Path string
	Size int64
	Time time.Time
}

// Backup writes a copy of the store through write into backups/, made 0700
// when missing, as <version>-<UTC time>.db, mode 0600 (design section 5,
// file mode): written as <name>.db.part, opened O_EXCL, then linked to
// <name>.db, never replacing a file, only when write returned no error and
// the part holds the bytes write said, one at least. The part is removed in
// every case, so a failure, even a kill, leaves nothing that is listed as a
// copy. The error is the bare reason, for the caller's own sentence.
func (h *Host) Backup(ctx context.Context, say func(string), version string, write func(io.Writer) (int64, error)) (Copy, error) {
	if err := os.Mkdir(filepath.Join(h.Dir, "backups"), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return Copy{}, err
	}
	name := regexp.MustCompile(`[^A-Za-z0-9._-]`).ReplaceAllString(version, "_") + "-" + time.Now().UTC().Format("20060102T150405Z") + ".db"
	rel := filepath.Join("backups", name)
	final := filepath.Join(h.Dir, rel)
	if _, err := os.Lstat(final); !errors.Is(err, os.ErrNotExist) {
		return Copy{}, fmt.Errorf("%s is there already", rel)
	}
	part := final + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Copy{}, err
	}
	defer func() { _ = os.Remove(part) }()
	defer func() { _ = f.Close() }()
	say("backing up the store to " + rel)
	n, err := write(f)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return Copy{}, err
	}
	info, err := f.Stat()
	switch {
	case err != nil:
		return Copy{}, err
	case info.Size() == 0:
		return Copy{}, errors.New("the copy is empty")
	case info.Size() != n:
		return Copy{}, fmt.Errorf("the copy is %d bytes, the proxy said %d", info.Size(), n)
	}
	if err := f.Sync(); err != nil {
		return Copy{}, err
	}
	if err := os.Link(part, final); err != nil {
		return Copy{}, err
	}
	return Copy{Path: rel, Size: n, Time: info.ModTime()}, nil
}

// Copies is the copies in backups/, newest first: each *.db that CopyAt
// takes. None, or no backups/, is an empty list. It reads the directory, not
// a pattern, since the clone's path may hold a pattern's characters.
func (h *Host) Copies() ([]Copy, error) {
	entries, err := os.ReadDir(filepath.Join(h.Dir, "backups"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var out []Copy
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		if c, err := h.CopyAt(filepath.Join("backups", e.Name())); err == nil {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b Copy) int { return b.Time.Compare(a.Time) })
	return out, nil
}

// CopyAt is the copy at path, relative to the clone or absolute: a regular
// file, a link to one followed, of one byte at least, that can be read.
func (h *Host) CopyAt(path string) (Copy, error) {
	full := path
	if !filepath.IsAbs(path) {
		full = filepath.Join(h.Dir, path)
	}
	info, err := os.Stat(full)
	if err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		var f *os.File
		if f, err = os.Open(full); err == nil {
			_ = f.Close()
			return Copy{Path: path, Size: info.Size(), Time: info.ModTime()}, nil
		}
	}
	return Copy{}, fmt.Errorf("%s is not a file with a copy in it; nothing was changed", path)
}

// Restore puts copy c back with the current image, the container stopped
// (OPS-10): docker compose run --rm --no-deps -T stargaite restore --yes -,
// its standard input the copy, checked a regular file again once open, its
// lines said, within upBound. It returns restore's exit code; the error is
// a put-back that did not start or did not end.
func (h *Host) Restore(ctx context.Context, say func(string), c Copy) (int, error) {
	path := c.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(h.Dir, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a file with a copy in it", c.Path)
	}
	bounded, cancel := context.WithTimeout(ctx, h.upBound)
	defer cancel()
	code, err := h.display(bounded, nil, f, say, "docker", "compose", "run", "--rm", "--no-deps", "-T", "stargaite", "restore", "--yes", "-")
	if ctx.Err() == nil && bounded.Err() != nil {
		return 0, errors.New("docker compose run was not done within 20 minutes")
	}
	return code, err
}
