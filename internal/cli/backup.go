package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// backupPrefix starts the names of the server's own copies, which serve
// removes from its data directory at start.
const backupPrefix = ".stargaite-backup-"

// backupFile refuses, before anything is sent and so as a wrong usage, a
// <file> that exists or cannot be looked up, and one whose base name starts
// with the prefix of the server's own copies; then the request goes to the
// server.
func backupFile(_ context.Context, in input, _ io.Writer) error {
	file := in.args[0]
	if strings.HasPrefix(filepath.Base(file), backupPrefix) {
		return fmt.Errorf("%w: %s: a name starting with %s is reserved for the server's own copies", errUsage, file, backupPrefix)
	}
	switch _, err := os.Lstat(file); {
	case err == nil:
		return fmt.Errorf("%w: %s exists; backup never replaces a file", errUsage, file)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	return errRemote
}

// saveBackup writes the copy the server streams to <file> through
// <file>.part, created with O_EXCL and mode 0600 since the copy holds key
// hashes. The byte count must equal Content-Length; the part is synced, then
// linked to <file>, so an existing file is never replaced, and the directory
// synced. The part is removed in every case.
func saveBackup(in input, resp *http.Response, stdout io.Writer) error {
	file := in.args[0]
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "application/vnd.sqlite3" || resp.ContentLength < 0 {
		return fmt.Errorf("the answer is not a store copy of known length: Content-Type %q, Content-Length %d", resp.Header.Get("Content-Type"), resp.ContentLength)
	}
	part := file + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	if err == nil && n != resp.ContentLength {
		err = fmt.Errorf("%d bytes received, %d announced", n, resp.ContentLength)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err == nil {
		err = os.Link(part, file)
	}
	if err = errors.Join(err, os.Remove(part)); err != nil {
		return fmt.Errorf("the backup was not written: %w", err)
	}
	dir, err := os.Open(filepath.Dir(file))
	if err == nil {
		err = errors.Join(dir.Sync(), dir.Close())
	}
	if err != nil {
		return fmt.Errorf("%s is written but its directory was not synced: %w", file, err)
	}
	if in.json {
		return json.NewEncoder(stdout).Encode(map[string]any{"path": file, "bytes": n})
	}
	_, err = fmt.Fprintf(stdout, "%s: %d bytes\n", file, n)
	return err
}
