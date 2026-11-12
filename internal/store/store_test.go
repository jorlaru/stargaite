//go:build linux || darwin

package store

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func pragma(t *testing.T, q querier, name string) string {
	t.Helper()
	var v string
	if err := q.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

// wantPragmas are the ADR-003 settings as SQLite reports them back.
var wantPragmas = map[string]string{"journal_mode": "wal", "synchronous": "2", "foreign_keys": "1", "busy_timeout": "5000"}

// TestPragmas reads every setting back on the writer and on several reader
// connections held at once, in a directory whose name would break an
// unescaped URI. A key the driver ignored would show its default here.
func TestPragmas(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a b?c=d&e#f%20g")
	s := open(t, dir)
	if _, err := os.Stat(filepath.Join(dir, "stargaite.db")); err != nil {
		t.Fatalf("the database is not where the path says: %v", err)
	}

	err := s.Write(context.Background(), func(tx *sql.Tx) error {
		for name, want := range wantPragmas {
			if got := pragma(t, tx, name); got != want {
				t.Errorf("writer: %s = %s, want %s", name, got, want)
			}
		}
		if got := pragma(t, tx, "query_only"); got != "0" {
			t.Errorf("writer: query_only = %s", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	for i := range 4 {
		conn, err := s.Read().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		for name, want := range wantPragmas {
			if got := pragma(t, conn, name); got != want {
				t.Errorf("reader %d: %s = %s, want %s", i, name, got, want)
			}
		}
		if got := pragma(t, conn, "query_only"); got != "1" {
			t.Errorf("reader %d: query_only = %s, want 1", i, got)
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO settings (name, value, updated_at) VALUES ('x', '1', 0)"); err == nil {
			t.Errorf("reader %d: an INSERT went through", i)
		}
	}
	if got := s.Read().Stats().OpenConnections; got < 4 {
		t.Errorf("%d reader connections open, want at least 4", got)
	}
	if _, err := s.Read().ExecContext(ctx, "DELETE FROM settings"); err == nil {
		t.Error("a DELETE through Read() went through")
	}
}

// TestDriverDSN pins what the design leans on in the pinned driver: each key
// of the DSN is accepted, a wrong value is refused, and a misspelt key is
// ignored silently, which is why TestPragmas reads every value back.
func TestDriverDSN(t *testing.T) {
	file := filepath.Join(t.TempDir(), "x.db")
	for _, tc := range []struct {
		query string
		ok    bool
	}{
		{pragmas + "&_txlock=immediate&_query_only=1", true},
		{"?_journal_mode=WALL", false},
		{"?_synchronous=FUL", false},
		{"?_foreign_keys=maybe", false},
		{"?_busy_timeout=5s", false},
		{"?_txlock=later", false},
		{"?_query_only=perhaps", false},
		{"?_journal_mod=WAL", true}, // ignored
	} {
		db, err := sql.Open("sqlite", "file:"+file+tc.query)
		if err != nil {
			t.Fatal(err)
		}
		err = db.Ping()
		_ = db.Close()
		if (err == nil) != tc.ok {
			t.Errorf("%s: error %v", tc.query, err)
		}
	}
}

// TestTxLock shows that _txlock=immediate took effect: a transaction of the
// writer holds the write lock before its first statement.
func TestTxLock(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	other, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "stargaite.db")+"?_busy_timeout=0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	other.SetMaxOpenConns(1)
	err = s.Write(context.Background(), func(*sql.Tx) error {
		_, err := other.Exec("BEGIN IMMEDIATE")
		if err == nil {
			_, _ = other.Exec("ROLLBACK")
			return errors.New("a second writer began while the writer's transaction was open")
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}
}

// TestSchema compares sqlite_master after the migrations with the golden
// file; migration 001 is annex A of the kick-off verbatim.
func TestSchema(t *testing.T) {
	s := open(t, t.TempDir())
	rows, err := s.Read().Query("SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_master ORDER BY type, name")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	fmt.Fprintf(&b, "-- user_version %s\n", pragma(t, s.Read(), "user_version"))
	for rows.Next() {
		var typ, name, table, text string
		if err := rows.Scan(&typ, &name, &table, &text); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "\n-- %s %s on %s\n%s\n", typ, name, table, text)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	const golden = "testdata/schema.golden"
	if *update {
		if err := os.WriteFile(golden, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Errorf("schema differs from %s (run the test with -update after a deliberate change):\n%s", golden, b.String())
	}
}

// TestMigrationNames holds the rule that a migration's number is its position.
func TestMigrationNames(t *testing.T) {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("migrations %v, error %v", names, err)
	}
	for i, name := range names {
		if !strings.HasPrefix(path.Base(name), fmt.Sprintf("%03d_", i+1)) {
			t.Errorf("migration %d is %s", i+1, name)
		}
	}
}

func TestReopenKeepsDataAndRefusesANewerSchema(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("data directory: %v, error %v", info.Mode(), err)
	}
	err = s.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO settings (name, value, updated_at) VALUES ('kept', '1', 0)")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open(t, dir) // the migration does not run twice
	var n int
	if err := s.Read().QueryRow("SELECT count(*) FROM settings WHERE name = 'kept'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("row after reopening: %d, error %v", n, err)
	}
	if err := s.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("PRAGMA user_version = 99")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "schema version 99") {
		t.Errorf("a newer schema: error %v", err)
	}
	// The refused Open released the lock: the second try fails the same way.
	if _, err := Open(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "schema version 99") {
		t.Errorf("second try: error %v", err)
	}

	// A negative version (a hand edit or a foreign file) is refused, not a panic.
	db, err := sql.Open("sqlite", filepath.Join(dir, "stargaite.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = -1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "schema version -1") {
		t.Errorf("a negative schema version: error %v", err)
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "locked by another process") {
		t.Errorf("second holder: error %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "lock")); err != nil {
		t.Errorf("the lock file is gone: %v", err)
	}
	open(t, dir) // released by Close
}

func TestWrite(t *testing.T) {
	s := open(t, t.TempDir())
	ctx := context.Background()
	count := func() int {
		var n int
		if err := s.Read().QueryRow("SELECT count(*) FROM settings").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	insert := func(name string) func(*sql.Tx) error {
		return func(tx *sql.Tx) error {
			_, err := tx.Exec("INSERT INTO settings (name, value, updated_at) VALUES (?, '1', 0)", name)
			return err
		}
	}

	failed := errors.New("refused")
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		if err := insert("rolled-back")(tx); err != nil {
			return err
		}
		return failed
	}); !errors.Is(err, failed) || count() != 0 {
		t.Errorf("a failing fn: error %v, %d rows", err, count())
	}
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO targets (id, provider, upstream_model, spec, updated_at) VALUES ('p/m', 'nobody', 'm', '{}', 0)")
		return err
	}); err == nil {
		t.Error("a foreign key violation committed")
	}

	// Hold the writer and fill the queue.
	hold, held := make(chan struct{}), make(chan struct{})
	go func() {
		_ = s.Write(ctx, func(*sql.Tx) error { close(held); <-hold; return nil })
	}()
	<-held
	cancelled, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	results := make(chan error, queueSize)
	for i := range queueSize {
		wg.Go(func() { results <- s.Write(cancelled, insert(fmt.Sprint("queued-", i))) })
	}
	for len(s.jobs) < queueSize {
		time.Sleep(time.Millisecond)
	}

	// With no slot free the context decides; once queued it no longer does.
	late, stop := context.WithTimeout(ctx, 10*time.Millisecond)
	defer stop()
	if err := s.Write(late, insert("late")); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("no slot: error %v", err)
	}
	cancel()
	close(hold)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("a queued write whose context ended: %v", err)
		}
	}
	if count() != queueSize {
		t.Errorf("%d rows, want %d", count(), queueSize)
	}
}

func TestCloseDrainsTheQueue(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	hold, held := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- s.Write(context.Background(), func(*sql.Tx) error { close(held); <-hold; return nil })
	}()
	<-held
	queued := make(chan error, 1)
	go func() {
		queued <- s.Write(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.Exec("INSERT INTO settings (name, value, updated_at) VALUES ('last', '1', 0)")
			return err
		})
	}()
	for len(s.jobs) == 0 {
		time.Sleep(time.Millisecond)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	for {
		s.mu.RLock()
		c := s.closed
		s.mu.RUnlock()
		if c {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := s.Write(context.Background(), func(*sql.Tx) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Errorf("a write after Close: %v", err)
	}
	close(hold)
	if err := errors.Join(<-first, <-queued, <-closed); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	var n int
	if err := open(t, dir).Read().QueryRow("SELECT count(*) FROM settings WHERE name = 'last'").Scan(&n); err != nil || n != 1 {
		t.Errorf("the queued write: %d rows, error %v", n, err)
	}
}
