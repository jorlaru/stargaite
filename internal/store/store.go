//go:build linux || darwin

// Package store owns the data directory: its lock, the SQLite database, the
// migrations, the one writer goroutine and the read pool (ADR-003).
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
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

// ErrClosed is returned by Write once Close has begun.
var ErrClosed = errors.New("store: closed")

// Store is the open data directory. Write is the only write path.
type Store struct {
	lock   *os.File
	writer *sql.DB
	reader *sql.DB

	mu     sync.RWMutex // held for reading while a Write waits for a queue slot
	closed bool
	jobs   chan job
	done   chan struct{} // closed when the writer goroutine has drained jobs
}

type job struct {
	fn     func(*sql.Tx) error
	result chan error
}

// Open creates dataDir with mode 0700 when it is missing, locks it, opens the
// writer, runs the migrations and opens the read pool, in that order. A data
// directory locked by another process is an error.
func Open(ctx context.Context, dataDir string) (_ *Store, err error) {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{jobs: make(chan job, queueSize), done: make(chan struct{})}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.closeHandles())
		}
	}()

	// The lock file stays open for the life of the process and is never deleted.
	s.lock, err = os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("data directory %s is locked by another process", dir)
		}
		return nil, fmt.Errorf("lock %s: %w", s.lock.Name(), err)
	}

	// The DSN is constant but for the escaped path: no text from the
	// command line can add a parameter. The writer opens first: it is what
	// puts the file in WAL mode.
	dsn := "file:" + (&url.URL{Path: filepath.Join(dir, "stargaite.db")}).EscapedPath() + pragmas
	if s.writer, err = sql.Open("sqlite", dsn+"&_txlock=immediate"); err != nil {
		return nil, err
	}
	s.writer.SetMaxOpenConns(1)
	if err := migrate(ctx, s.writer); err != nil {
		return nil, err
	}
	// The read pool refuses writes through PRAGMA query_only, applied last
	// on each connection; it is a guard against mistakes, not a boundary:
	// a statement on the pool could turn it off again.
	if s.reader, err = sql.Open("sqlite", dsn+"&_query_only=1"); err != nil {
		return nil, err
	}
	if err := s.reader.PingContext(ctx); err != nil {
		return nil, err
	}

	go s.run()
	return s, nil
}

// migrate runs every embedded migration above PRAGMA user_version, each in
// one transaction that also sets the new version. A store whose version is
// above this binary's is refused (OPS-7).
func migrate(ctx context.Context, db *sql.DB) error {
	names, err := fs.Glob(migrations, "migrations/*.sql") // sorted: 001_, 002_, ...
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
		text, err := migrations.ReadFile(names[i])
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

// Close lets the queued writes finish, closes both handles and releases the
// lock. Writes that arrive after Close has begun get ErrClosed.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.jobs)
	s.mu.Unlock()
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
