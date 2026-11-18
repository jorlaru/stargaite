package cli

import (
	"cmp"
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

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/store"
)

// backupPrefix starts the names of the server's own copies, which serve
// removes from its data directory at start.
const backupPrefix = ".stargaite-backup-"

// backupFile refuses, before anything is sent and so as a wrong usage, a
// <file> that exists or cannot be looked up, one whose base name starts with
// the prefix of the server's own copies, and "-" when standard output is a
// terminal or another device (OPS-3); then the request goes to the server.
func backupFile(_ context.Context, in input, _ io.Writer) error {
	file := in.args[0]
	if file == "-" {
		if device(in.stdout) {
			return fmt.Errorf("%w: backup - writes the copy to standard output, which is a terminal or a device; redirect it to a file", errUsage)
		}
		return errRemote
	}
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

// saveBackup writes the copy the server streams, to standard output for
// "-", and then its summary, the path and the byte count, to out, which for
// "-" is standard error. The byte count must equal Content-Length.
func saveBackup(in input, resp *http.Response, out io.Writer) error {
	file := in.args[0]
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "application/vnd.sqlite3" || resp.ContentLength < 0 {
		return fmt.Errorf("the answer is not a store copy of known length: Content-Type %q, Content-Length %d", resp.Header.Get("Content-Type"), resp.ContentLength)
	}
	var n int64
	var err error
	if file == "-" {
		if n, err = io.Copy(in.stdout, resp.Body); err == nil && n != resp.ContentLength {
			err = fmt.Errorf("%d bytes received, %d announced", n, resp.ContentLength)
		}
		if err != nil {
			return fmt.Errorf("the copy on standard output is not whole: %w", err)
		}
	} else if n, err = saveFile(file, resp); err != nil {
		return err
	}
	if in.json {
		return json.NewEncoder(out).Encode(map[string]any{"path": file, "bytes": n})
	}
	_, err = fmt.Fprintf(out, "%s: %d bytes\n", file, n)
	return err
}

// saveFile writes the copy to <file> through <file>.part, created with
// O_EXCL and mode 0600 since the copy holds key hashes. The byte count must
// equal Content-Length; the part is synced, then linked to <file>, so an
// existing file is never replaced, and the directory synced. The part is
// removed in every case.
func saveFile(file string, resp *http.Response) (int64, error) {
	part := file + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
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
		return 0, fmt.Errorf("the backup was not written: %w", err)
	}
	dir, err := os.Open(filepath.Dir(file))
	if err == nil {
		err = errors.Join(dir.Sync(), dir.Close())
	}
	if err != nil {
		return 0, fmt.Errorf("%s is written but its directory was not synced: %w", file, err)
	}
	return n, nil
}

// restore puts a copy of the store in place while serve is stopped
// (store.Restore), from <file> or, for "-", standard input, never a terminal
// (OPS-3), and prints what it moved. It runs without a server and needs
// --yes: every key, the configuration and the usage are replaced.
func restore(ctx context.Context, in input, stdout io.Writer) error {
	if !*in.bools["yes"] {
		return fmt.Errorf("%w: this replaces the whole store, keys and configuration included; give --yes to confirm", errUsage)
	}
	if in.args[0] == "-" && device(in.stdin) {
		return fmt.Errorf("%w: - reads standard input, which is a terminal or a device; pipe the copy in, through docker compose run -T", errUsage)
	}
	dir := cmp.Or(*in.flags["data-dir"], in.getenv("STARGAITE_DATA_DIR"), "./data")
	var n int64
	var moved []string
	var err error
	src := in.stdin
	if in.args[0] != "-" {
		var f *os.File
		if f, err = os.Open(in.args[0]); err == nil {
			defer func() { _ = f.Close() }()
			src = f
		}
	}
	if err == nil {
		n, moved, err = store.Restore(ctx, dir, src)
	}
	if err != nil {
		if in.json { // the output stays one JSON value; Main prints err as well
			_ = json.NewEncoder(stdout).Encode(map[string]any{"error": domain.NewError(domain.CodeInvalidRequest, err.Error())})
		}
		return err
	}
	if in.json {
		return json.NewEncoder(stdout).Encode(map[string]any{"path": in.args[0], "bytes": n, "data_dir": dir, "replaced": moved})
	}
	if len(moved) == 0 {
		_, err = fmt.Fprintf(stdout, "%s: %d bytes restored into %s, which held no store\n", in.args[0], n, dir)
	} else {
		_, err = fmt.Fprintf(stdout, "%s: %d bytes restored into %s; the store it replaced, %s, is in %s\n",
			in.args[0], n, dir, strings.Join(moved, ", "), filepath.Join(dir, "replaced"))
	}
	return err
}

// device reports whether f is a terminal or another device, which "-" never
// reads from or writes to (OPS-3).
func device(f any) bool {
	s, ok := f.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		return false
	}
	info, err := s.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
