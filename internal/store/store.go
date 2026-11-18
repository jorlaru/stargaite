//go:build linux || darwin

// Package store owns the data directory: its lock, the SQLite database, the
// migrations, the one writer goroutine and the read pool (ADR-003).
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// pragmas are set on every connection by the driver's shorthand keys. A
// misspelt key is ignored silently by the driver, so the tests read every
// value back.
const pragmas = "?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_busy_timeout=5000"

// queueSize bounds the writes waiting for the writer goroutine.
const queueSize = 64

//go:embed migrations/*.sql
var migrations embed.FS

// ErrClosed is returned by Write and Backup once Close has begun.
var ErrClosed = errors.New("store: closed")

// backupPrefix starts the name of every copy Backup writes in the data
// directory; leftover matches the names of those copies and of their
// rollback journals alone, which Open removes: a crash during a backup
// leaves them, the journal while VACUUM INTO runs.
const backupPrefix = ".stargaite-backup-"

var leftover = regexp.MustCompile(`^\.stargaite-backup-[A-Z2-7]{26}(-journal)?$`)

// vacuumed is a test hook, nil in production: Backup calls it after VACUUM
// INTO and before the checks, with the copy's path.
var vacuumed func(path string)

// Store is the open data directory. Write is the only write path.
type Store struct {
	dir    string // absolute
	dsn    string // of the database, with the pragmas
	lock   *os.File
	writer *sql.DB
	reader *sql.DB

	mu      sync.RWMutex // held for reading while a Write waits for a queue slot or a Backup starts
	closed  bool
	jobs    chan job
	done    chan struct{}  // closed when the writer goroutine has drained jobs
	backups sync.WaitGroup // the Backup calls running, which Close waits for
}

type job struct {
	fn     func(*sql.Tx) error
	result chan error
}

// Open creates dataDir with mode 0700 when it is missing, locks it, removes
// the copies a crashed Backup left, logging each, opens the writer, runs the
// migrations and opens the read pool, in that order. A data directory locked
// by another process is an error.
func Open(ctx context.Context, dataDir string, log *slog.Logger) (_ *Store, err error) {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, jobs: make(chan job, queueSize), done: make(chan struct{})}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.closeHandles())
		}
	}()

	// The lock file stays open for the life of the process and is never deleted.
	if s.lock, err = lockDir(dir); err != nil {
		return nil, err
	}
	// Under the lock, so no Backup of this directory is running. Only a
	// regular file whose whole name is a copy's is ever removed.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Type().IsRegular() && leftover.MatchString(e.Name()) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return nil, err
			}
			log.Warn("leftover backup copy removed", "path", filepath.Join(dir, e.Name()))
		}
	}

	// The DSN is constant but for the escaped path: no text from the
	// command line can add a parameter. The writer opens first: it is what
	// puts the file in WAL mode.
	s.dsn = "file:" + (&url.URL{Path: filepath.Join(dir, "stargaite.db")}).EscapedPath() + pragmas
	if s.writer, err = sql.Open("sqlite", s.dsn+"&_txlock=immediate"); err != nil {
		return nil, err
	}
	s.writer.SetMaxOpenConns(1)
	if err := migrate(ctx, s.writer, migrations); err != nil {
		return nil, err
	}
	// The read pool refuses writes through PRAGMA query_only, applied last
	// on each connection; it is a guard against mistakes, not a boundary:
	// a statement on the pool could turn it off again.
	if s.reader, err = sql.Open("sqlite", s.dsn+"&_query_only=1"); err != nil {
		return nil, err
	}
	if err := s.reader.PingContext(ctx); err != nil {
		return nil, err
	}

	go s.run()
	return s, nil
}

// lockDir takes the lock of the data directory dir, which holds while the
// file it returns is open: one process at a time (OPS-10).
func lockDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			err = fmt.Errorf("data directory %s is locked by another process", dir)
		} else {
			err = fmt.Errorf("lock %s: %w", f.Name(), err)
		}
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// migrate runs every migration of fsys above PRAGMA user_version, each in
// one transaction that also sets the new version. A store whose version is
// above this binary's is refused (OPS-7). Open passes the embedded files; the
// tests pass others.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	names, err := fs.Glob(fsys, "migrations/*.sql") // sorted: 001_, 002_, ...
	if err != nil {
		return err
	}
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 0 || version > len(names) {
		return fmt.Errorf("the store has schema version %d; this binary knows up to %d", version, len(names))
	}
	for i := version; i < len(names); i++ {
		text, err := fs.ReadFile(fsys, names[i])
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(text)); err != nil {
			return errors.Join(fmt.Errorf("migration %s: %w", names[i], err), tx.Rollback())
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			return errors.Join(err, tx.Rollback())
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// run is the writer goroutine: each queued fn runs in its own transaction.
func (s *Store) run() {
	for j := range s.jobs {
		j.result <- s.apply(j.fn)
	}
	close(s.done)
}

func (s *Store) apply(fn func(*sql.Tx) error) error {
	// Once queued, a write is not tied to its caller's context.
	tx, err := s.writer.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

// Write runs fn in a transaction on the writer goroutine and returns the
// commit result. ctx bounds only the wait for a slot in the queue: once fn is
// queued, Write waits for the outcome even if ctx ends, so a caller never sees
// an unknown outcome.
func (s *Store) Write(ctx context.Context, fn func(*sql.Tx) error) error {
	j := job{fn: fn, result: make(chan error, 1)}
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return ErrClosed
	}
	select {
	case s.jobs <- j:
		s.mu.RUnlock()
	case <-ctx.Done():
		s.mu.RUnlock()
		return ctx.Err()
	}
	return <-j.result
}

// Read returns the read pool. Its connections refuse writes.
func (s *Store) Read() *sql.DB {
	return s.reader
}

// Backup writes a consistent copy of the store: VACUUM INTO a new file
// <data-dir>/.stargaite-backup-<rand.Text()> on a connection of its own (no
// query_only, no transaction), which snapshots the last commit while writes
// continue. The copy, opened read-only, must give integrity_check "ok", an
// empty foreign_key_check and this binary's user_version, or it is removed
// and the error returned. Backup returns it open at offset 0, already
// unlinked, so nothing is left behind; ctx interrupts it. Backup takes
// s.mu.RLock, returns ErrClosed when s.closed, else calls s.backups.Add(1)
// (a sync.WaitGroup field) and releases the RLock at once: it holds no store
// lock during VACUUM INTO or the checks, and calls s.backups.Done() when it
// returns. Close sets closed under s.mu.Lock as today, then waits on
// s.backups before <-s.done and closeHandles, so the data-directory lock is
// released only after every copy is closed and unlinked.
func (s *Store) Backup(ctx context.Context) (_ *os.File, err error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, ErrClosed
	}
	s.backups.Add(1)
	s.mu.RUnlock()
	defer s.backups.Done()

	// The path is the server's own, bound as a parameter: never a text from
	// a request and never a URI, which the driver would parse.
	path := filepath.Join(s.dir, backupPrefix+rand.Text())
	defer func() {
		if err != nil {
			if rmErr := os.Remove(path); !errors.Is(rmErr, fs.ErrNotExist) {
				err = errors.Join(err, rmErr)
			}
		}
	}()
	if err := withDB(ctx, s.dsn, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, "VACUUM INTO ?", path)
		return err
	}); err != nil {
		return nil, fmt.Errorf("vacuum into the copy: %w", err)
	}
	if vacuumed != nil {
		vacuumed(path)
	}
	if err := withDB(ctx, "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro", func(db *sql.DB) error { return check(ctx, db, false) }); err != nil {
		return nil, fmt.Errorf("the copy failed its check: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// withDB opens the database at dsn on a handle of its own, runs fn and closes it.
func withDB(ctx context.Context, dsn string, fn func(*sql.DB) error) error {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	if err := db.PingContext(ctx); err != nil {
		return errors.Join(err, db.Close())
	}
	return errors.Join(fn(db), db.Close())
}

// check requires integrity_check "ok", an empty foreign_key_check, which
// integrity_check does not cover (ADR-018), and this binary's user_version,
// or, when older, a user_version from 1 up to it, which Open migrates.
func check(ctx context.Context, db *sql.DB, older bool) error {
	var lines []string
	if err := each(ctx, db, "PRAGMA integrity_check", func(rows *sql.Rows) error {
		var line string
		err := rows.Scan(&line)
		lines = append(lines, line)
		return err
	}); err != nil {
		return err
	}
	if len(lines) != 1 || lines[0] != "ok" {
		return fmt.Errorf("integrity_check: %s", strings.Join(lines, "; "))
	}
	var missing []string
	if err := each(ctx, db, "PRAGMA foreign_key_check", func(rows *sql.Rows) error {
		var table, parent string
		var rowid sql.NullInt64
		var fk int
		err := rows.Scan(&table, &rowid, &parent, &fk)
		missing = append(missing, fmt.Sprintf("%s rowid %d refers to a missing %s row", table, rowid.Int64, parent))
		return err
	}); err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("foreign_key_check: %s", strings.Join(missing, "; "))
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 1 || version > len(names) || !older && version != len(names) {
		return fmt.Errorf("user_version %d; this binary's is %d", version, len(names))
	}
	return nil
}

// restorePart is where Restore writes the copy before it checks it.
const restorePart = ".stargaite-restore.db"

// Restore puts the copy src holds in place of the store of dataDir while no
// process holds the directory's lock (OPS-10), which it takes. It writes the
// copy beside the store as restorePart, mode 0600, and checks it before
// anything moves: not in WAL mode, as a file alone would lose the frames of
// its -wal, integrity_check, foreign_key_check and a user_version from 1 up
// to this binary's, since Open migrates an older store and refuses a newer
// one. Then the store's files that exist move together into
// <dataDir>/replaced, emptied first, so copies never accumulate, and the copy
// is renamed to stargaite.db. It returns the bytes restored and the names of
// the files moved, none for a data directory with no store. The part is
// removed on any failure; a part a crash left is replaced by the next
// Restore.
func Restore(ctx context.Context, dataDir string, src io.Reader) (n int64, moved []string, err error) {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return 0, nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, nil, err
	}
	lock, err := lockDir(dir)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: restore runs only while serve is stopped", err)
	}
	defer func() { err = errors.Join(err, lock.Close()) }()

	part := filepath.Join(dir, restorePart)
	if err := os.Remove(part); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, nil, err
	}
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, nil, err
	}
	defer func() {
		if err != nil {
			if rmErr := os.Remove(part); !errors.Is(rmErr, fs.ErrNotExist) {
				err = errors.Join(err, rmErr)
			}
		}
	}()
	n, err = io.Copy(f, src)
	if err == nil {
		err = f.Sync()
	}
	// Bytes 18 and 19 of the header are 2 in WAL mode; a shorter file is
	// left to the check.
	header := make([]byte, 20)
	_, rErr := f.ReadAt(header, 0)
	if err = errors.Join(err, f.Close()); err != nil {
		return 0, nil, fmt.Errorf("the copy was not written beside the store: %w", err)
	}
	if rErr == nil && (header[18] == 2 || header[19] == 2) {
		return 0, nil, errors.New("the copy is in WAL mode, so it may lack the frames of its -wal file, and nothing was moved: restore a copy that backup wrote")
	}
	if err := withDB(ctx, "file:"+(&url.URL{Path: part}).EscapedPath()+"?mode=ro", func(db *sql.DB) error { return check(ctx, db, true) }); err != nil {
		return 0, nil, fmt.Errorf("the copy failed its check, so nothing was moved: %w", err)
	}

	for _, name := range []string{"stargaite.db", "stargaite.db-wal", "stargaite.db-shm"} {
		switch _, err := os.Lstat(filepath.Join(dir, name)); {
		case err == nil:
			moved = append(moved, name)
		case !errors.Is(err, fs.ErrNotExist):
			return 0, nil, err
		}
	}
	if len(moved) > 0 {
		replaced := filepath.Join(dir, "replaced")
		if err := os.RemoveAll(replaced); err != nil {
			return 0, nil, err
		}
		if err := os.Mkdir(replaced, 0o700); err != nil {
			return 0, nil, err
		}
		for _, name := range moved {
			if err := os.Rename(filepath.Join(dir, name), filepath.Join(replaced, name)); err != nil {
				return 0, nil, fmt.Errorf("the store was moved to %s only in part, and the copy not put in place: %w", replaced, err)
			}
		}
	}
	if err := os.Rename(part, filepath.Join(dir, "stargaite.db")); err != nil {
		return 0, nil, err
	}
	d, err := os.Open(dir)
	if err == nil {
		err = errors.Join(d.Sync(), d.Close())
	}
	if err != nil {
		return 0, nil, fmt.Errorf("the copy is in place, but %s was not synced: %w", dir, err)
	}
	return n, moved, nil
}

// Check is PRAGMA quick_check on the read pool: nil for "ok", else its rows
// (kick-off section 14.3, ADR-018). An answer with no row is not "ok" either.
// ctx interrupts it.
func (s *Store) Check(ctx context.Context) ([]string, error) {
	var lines []string
	if err := each(ctx, s.reader, "PRAGMA quick_check", func(rows *sql.Rows) error {
		var line string
		err := rows.Scan(&line)
		lines = append(lines, line)
		return err
	}); err != nil {
		return nil, err
	}
	switch {
	case len(lines) == 1 && lines[0] == "ok":
		return nil, nil
	case len(lines) == 0:
		return []string{"quick_check returned no row"}, nil
	}
	return lines, nil
}

// each runs query and calls scan on each row.
func each(ctx context.Context, db *sql.DB, query string, scan func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	for rows.Next() {
		if err := scan(rows); err != nil {
			return errors.Join(err, rows.Close())
		}
	}
	return errors.Join(rows.Err(), rows.Close())
}

// Close lets the queued writes finish, waits for the Backup calls running,
// closes both handles and releases the lock. Writes and backups that arrive
// after Close has begun get ErrClosed.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.jobs)
	s.mu.Unlock()
	s.backups.Wait()
	<-s.done
	return s.closeHandles()
}

// closeHandles closes whatever Open managed to open; closing the lock file
// releases the lock.
func (s *Store) closeHandles() error {
	var errs []error
	if s.reader != nil {
		errs = append(errs, s.reader.Close())
	}
	if s.writer != nil {
		errs = append(errs, s.writer.Close())
	}
	if s.lock != nil {
		errs = append(errs, s.lock.Close())
	}
	return errors.Join(errs...)
}
