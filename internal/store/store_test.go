//go:build linux || darwin

package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(context.Background(), dir, slog.New(slog.DiscardHandler))
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

// schema dumps sqlite_master with user_version, as the golden file holds it.
func schema(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "-- user_version %s\n", pragma(t, db, "user_version"))
	rows, err := db.Query("SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_master ORDER BY type, name")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
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
	return b.String()
}

const golden = "testdata/schema.golden"

// TestSchema compares sqlite_master after the migrations with the golden
// file; migrations 001 to 003 are annex A of the kick-off verbatim.
func TestSchema(t *testing.T) {
	s := open(t, t.TempDir())
	got := schema(t, s.Read())
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("schema differs from %s (run the test with -update after a deliberate change):\n%s", golden, got)
	}
}

// checkIntegrity requires integrity_check ok and an empty foreign_key_check:
// integrity_check does not see foreign keys (ADR-018).
func checkIntegrity(t *testing.T, db *sql.DB) {
	t.Helper()
	if got := pragma(t, db, "integrity_check"); got != "ok" {
		t.Errorf("integrity_check: %s", got)
	}
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fk int
		if err := rows.Scan(&table, &rowid, &parent, &fk); err != nil {
			t.Fatal(err)
		}
		t.Errorf("foreign_key_check: %s rowid %v refers to a missing %s row (constraint %d)", table, rowid, parent, fk)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// columns returns the columns of every table but the sqlite_ ones.
func columns(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	rows, err := db.Query("SELECT m.name, p.name FROM sqlite_master m, pragma_table_info(m.name) p WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' ORDER BY m.name, p.cid")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols := map[string][]string{}
	for rows.Next() {
		var table, col string
		if err := rows.Scan(&table, &col); err != nil {
			t.Fatal(err)
		}
		cols[table] = append(cols[table], col)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return cols
}

// tableRows returns the rows of each table of cols, reading those columns,
// sorted; each value is written with its Go type, so a changed storage class
// shows.
func tableRows(t *testing.T, db *sql.DB, cols map[string][]string) map[string][]string {
	t.Helper()
	all := map[string][]string{}
	for table, names := range cols {
		rows, err := db.Query(fmt.Sprintf("SELECT %s FROM %s", strings.Join(names, ", "), table))
		if err != nil {
			t.Fatal(err)
		}
		values := make([]any, len(names))
		for i := range values {
			values[i] = new(any)
		}
		all[table] = []string{}
		for rows.Next() {
			if err := rows.Scan(values...); err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			for i, v := range values {
				fmt.Fprintf(&b, "%s=%T:%v ", names[i], *v.(*any), *v.(*any))
			}
			all[table] = append(all[table], b.String())
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		slices.Sort(all[table])
	}
	return all
}

// firstMigrations returns the first n embedded migrations, plus extra.
func firstMigrations(t *testing.T, n int, extra fstest.MapFS) fstest.MapFS {
	t.Helper()
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil || n > len(names) {
		t.Fatalf("%d migrations wanted of %v, error %v", n, names, err)
	}
	fsys := fstest.MapFS{}
	for _, name := range names[:n] {
		text, err := migrations.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		fsys[name] = &fstest.MapFile{Data: text}
	}
	maps.Copy(fsys, extra)
	return fsys
}

// TestMigrations seeds a store at every version from 0 to head with rows in
// each table (testdata/seed), and requires that a failing migration changes
// nothing, that the real migrations before a failing one stay committed with
// their versions, that Open migrates to the golden schema keeping every row and every
// foreign key, and that a newer version is refused without touching the file
// (OPS-7, AC-50).
func TestMigrations(t *testing.T) {
	ctx := context.Background()
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	head := len(names)
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	failingSQL := []byte("CREATE TABLE injected (x INTEGER);\nINSERT INTO missing VALUES (1);\n")
	for version := 0; version <= head; version++ {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "stargaite.db")
			// The seed connection has the pragmas of a real store, so the
			// file is in WAL mode as a binary at this version leaves it.
			db, err := sql.Open("sqlite", "file:"+file+pragmas)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			db.SetMaxOpenConns(1)
			if err := migrate(ctx, db, firstMigrations(t, version, nil)); err != nil {
				t.Fatal(err)
			}
			if version > 0 {
				text, err := os.ReadFile(fmt.Sprintf("testdata/seed/v%d.sql", version))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(string(text)); err != nil {
					t.Fatalf("seed v%d: %v", version, err)
				}
			}
			checkIntegrity(t, db)
			cols := columns(t, db)
			seeded := tableRows(t, db, cols)
			for table, rows := range seeded {
				if len(rows) == 0 {
					t.Errorf("testdata/seed/v%d.sql has no row in %s", version, table)
				}
			}
			before := schema(t, db)

			// A migration that fails at its second statement leaves the
			// version, the schema and the rows as they were.
			failing := fmt.Sprintf("migrations/%03d_failing.sql", version+1)
			fsys := firstMigrations(t, version, fstest.MapFS{failing: {Data: failingSQL}})
			if err := migrate(ctx, db, fsys); err == nil || !strings.Contains(err.Error(), failing) {
				t.Errorf("a failing migration: error %v", err)
			}
			if got := schema(t, db); got != before {
				t.Errorf("the failing migration changed the schema:\n%s", got)
			}
			if got := tableRows(t, db, columns(t, db)); !maps.EqualFunc(got, seeded, slices.Equal) {
				t.Errorf("the failing migration changed rows: %v", got)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			// A copy of the seeded file, for the real migrations followed by a failing one.
			copied := filepath.Join(t.TempDir(), "stargaite.db")
			if data, err := os.ReadFile(file); err != nil {
				t.Fatal(err)
			} else if err := os.WriteFile(copied, data, 0o600); err != nil {
				t.Fatal(err)
			}

			s := open(t, dir)
			if got := schema(t, s.Read()); got != string(want) {
				t.Errorf("schema after migrating differs from %s:\n%s", golden, got)
			}
			checkIntegrity(t, s.Read())
			kept := seeded
			if version == 1 {
				// Migration 002 drops usage_daily, which no binary at version
				// 1 wrote (phase-3 design section 5): its seeded row is the one
				// expected loss. budget_totals rows start at since_revision 0.
				kept = maps.Clone(seeded)
				kept["usage_daily"] = []string{}
				var n int
				if err := s.Read().QueryRow("SELECT count(*) FROM budget_totals WHERE since_revision <> 0").Scan(&n); err != nil || n != 0 {
					t.Errorf("budget_totals since_revision: %d rows not 0, error %v", n, err)
				}
			}
			if version == 2 {
				// Migration 003 copies an old 0 of usage_daily's reasoning
				// tokens or cost as NULL, since it cannot be told from an
				// unknown; every other value is kept (phase-5 design C49).
				kept = maps.Clone(seeded)
				kept["usage_daily"] = nil
				for _, r := range seeded["usage_daily"] {
					r = strings.Replace(r, " reasoning_tokens=int64:0 ", " reasoning_tokens=<nil>:<nil> ", 1)
					kept["usage_daily"] = append(kept["usage_daily"], strings.Replace(r, " cost_usd=float64:0 ", " cost_usd=<nil>:<nil> ", 1))
				}
				slices.Sort(kept["usage_daily"])
			}
			if got := tableRows(t, s.Read(), cols); !maps.EqualFunc(got, kept, slices.Equal) {
				t.Errorf("rows after migrating:\n%v\nwant:\n%v", got, kept)
			}

			// When a later migration fails, the real ones before it stay
			// committed, each with its own version: the copy ends at head.
			if version < head {
				c, err := sql.Open("sqlite", "file:"+copied+pragmas)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.Close() }()
				c.SetMaxOpenConns(1)
				failing := fmt.Sprintf("migrations/%03d_failing.sql", head+1)
				if err := migrate(ctx, c, firstMigrations(t, head, fstest.MapFS{failing: {Data: failingSQL}})); err == nil || !strings.Contains(err.Error(), failing) {
					t.Errorf("a failing migration after the real ones: error %v", err)
				}
				if got := schema(t, c); got != string(want) {
					t.Errorf("schema after a failing migration %d differs from %s:\n%s", head+1, golden, got)
				}
				if got := tableRows(t, c, cols); !maps.EqualFunc(got, kept, slices.Equal) {
					t.Errorf("rows after a failing migration %d:\n%v\nwant:\n%v", head+1, got, kept)
				}
			}
			if err := s.Write(ctx, func(tx *sql.Tx) error {
				_, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", head+1))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			newer, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Open(ctx, dir, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("schema version %d", head+1)) {
				t.Errorf("a newer schema: error %v", err)
			}
			if got, err := os.ReadFile(file); err != nil || !bytes.Equal(got, newer) {
				t.Errorf("the refused Open changed the file (error %v)", err)
			}
			if info, err := os.Stat(file + "-wal"); err == nil && info.Size() > 0 {
				t.Errorf("the refused Open left %d bytes in the WAL", info.Size())
			}
		})
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

// TestMigrationFromVersion1 opens a store that the phase 1 and 2 binaries
// left at version 1, with a budget_totals row: migration 002 keeps the row
// with since_revision 0 and recreates usage_daily with billing in its key
// (phase-3 design section 5).
func TestMigrationFromVersion1(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "stargaite.db"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := migrations.ReadFile("migrations/001_base.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{string(first), "PRAGMA user_version = 1",
		"INSERT INTO budget_totals (scope_type, scope_name, cost_usd) VALUES ('key', 'k', 2.5)"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s := open(t, dir)
	if v := pragma(t, s.Read(), "user_version"); v != "3" {
		t.Errorf("user_version %s", v)
	}
	checkIntegrity(t, s.Read())
	var cost float64
	var since int64
	if err := s.Read().QueryRow("SELECT cost_usd, since_revision FROM budget_totals WHERE scope_type = 'key' AND scope_name = 'k'").Scan(&cost, &since); err != nil || cost != 2.5 || since != 0 {
		t.Errorf("budget_totals row: %v %d, error %v", cost, since, err)
	}
	err = s.Write(context.Background(), func(tx *sql.Tx) error {
		for _, billing := range []string{"pay_per_use", "subscription"} {
			if _, err := tx.Exec("INSERT INTO usage_daily (day, key_name, model, target, billing) VALUES ('2026-11-14', 'k', 'fast', 'p/m', ?)", billing); err != nil {
				return err
			}
		}
		_, err := tx.Exec("INSERT INTO usage_daily (day, key_name, model, target, billing) VALUES ('2026-11-14', 'k', 'fast', 'p/m', 'subscription')")
		if err == nil {
			return errors.New("a row repeating the whole key was inserted")
		}
		return nil
	})
	if err != nil {
		t.Errorf("usage_daily by billing: %v", err)
	}
}

func TestReopenKeepsDataAndRefusesANewerSchema(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(context.Background(), dir, slog.New(slog.DiscardHandler))
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
	checkIntegrity(t, s.Read())
	if err := s.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("PRAGMA user_version = 99")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), dir, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "schema version 99") {
		t.Errorf("a newer schema: error %v", err)
	}
	// The refused Open released the lock: the second try fails the same way.
	if _, err := Open(context.Background(), dir, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "schema version 99") {
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
	if _, err := Open(context.Background(), dir, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "schema version -1") {
		t.Errorf("a negative schema version: error %v", err)
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), dir, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "locked by another process") {
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

// TestCheck: quick_check on a healthy store is nil; its context interrupts
// it; with the root page of an index overwritten the store still opens, and
// Check returns the rows quick_check gives, not an error.
func TestCheck(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if rows, err := s.Check(context.Background()); rows != nil || err != nil {
		t.Fatalf("healthy store: %q, %v", rows, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if rows, err := s.Check(cancelled); rows != nil || !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %q, %v", rows, err)
	}
	var page, size int64
	if err := s.Read().QueryRow("SELECT rootpage FROM sqlite_schema WHERE name = 'idx_usage_key_time'").Scan(&page); err != nil {
		t.Fatal(err)
	}
	if err := s.Read().QueryRow("PRAGMA page_size").Scan(&size); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "stargaite.db"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xA5}, int(size)), (page-1)*size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir)
	if rows, err := s.Check(context.Background()); err != nil || !strings.Contains(strings.Join(rows, "\n"), fmt.Sprintf("Tree %d page %d: ", page, page)) {
		t.Errorf("page %d overwritten: %q, %v", page, rows, err)
	}
}

func TestCloseDrainsTheQueue(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, slog.New(slog.DiscardHandler))
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

// copyOf writes what f holds, from where it stands, to a new file and opens
// that file read-only.
func copyOf(t *testing.T, f *os.File) *sql.DB {
	t.Helper()
	file := filepath.Join(t.TempDir(), "copy.db")
	out, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, f); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+file+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// copies lists the files of dir that Backup names.
func copies(t *testing.T, dir string) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, backupPrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// TestBackup is ADR-018 on the store: a copy taken under concurrent writes
// holds whole transactions, comes back open at offset 0 and already
// unlinked, and passes the checks; a copy that fails integrity_check,
// foreign_key_check or the user_version, or whose context ends, is an error
// and leaves no file; Close waits for a Backup in progress, and a Backup
// after Close has begun gets ErrClosed; Open removes the leftover copies of a
// crash, by exact name, and nothing else.
func TestBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	planted := filepath.Join(dir, backupPrefix+rand.Text())
	kept := []string{filepath.Join(dir, "backup-x.db"), filepath.Join(dir, backupPrefix+rand.Text()[:25]),
		filepath.Join(dir, backupPrefix+rand.Text()+".part"), filepath.Join(dir, backupPrefix+strings.ToLower(rand.Text())),
		filepath.Join(dir, backupPrefix+rand.Text()+"-wal")}
	for _, file := range append([]string{planted, planted + "-journal"}, kept...) {
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A directory with a copy's name is not a copy.
	keptDir := filepath.Join(dir, backupPrefix+rand.Text())
	if err := os.Mkdir(keptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	s, err := Open(ctx, dir, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, file := range []string{planted, planted + "-journal"} {
		if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the leftover %s is still there: %v", file, err)
		}
	}
	for _, file := range append(kept, keptDir) {
		if _, err := os.Stat(file); err != nil {
			t.Errorf("Open removed %s: %v", file, err)
		}
	}
	if lines := strings.Split(strings.TrimSpace(logs.String()), "\n"); len(lines) != 2 ||
		!strings.Contains(lines[0], `"level":"WARN","msg":"leftover backup copy removed","path":"`+planted+`"`) ||
		!strings.Contains(lines[1], `"level":"WARN","msg":"leftover backup copy removed","path":"`+planted+`-journal"`) {
		t.Errorf("log %q", &logs)
	}
	for _, file := range append(kept, keptDir) {
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
	}

	// Batches of 10 records, each one transaction with its usage_daily
	// upsert, as the usage writer commits them.
	stop := make(chan struct{})
	written := make(chan error, 1)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				written <- nil
				return
			default:
			}
			if err := s.Write(ctx, func(tx *sql.Tx) error {
				for j := range 10 {
					if _, err := tx.Exec(`INSERT INTO usage (id, request_id, endpoint, key_name, model, target, billing, status_code,
						input_tokens, output_tokens, tokens_source, cost_usd, cost_source, started_at, completed_at, config_revision, proxy_version)
						VALUES (?, ?, 'chat', 'k', 'm', 'p/m', 'pay_per_use', 200, 3, 5, 'provider', 0.25, 'price_table', 0, 0, 1, 'test')`,
						fmt.Sprint(i, "-", j), fmt.Sprint(i)); err != nil {
						return err
					}
					if _, err := tx.Exec(`INSERT INTO usage_daily (day, key_name, model, target, billing, requests, input_tokens, output_tokens, cost_usd)
						VALUES ('1970-01-01', 'k', 'm', 'p/m', 'pay_per_use', 1, 3, 5, 0.25)
						ON CONFLICT DO UPDATE SET requests = requests + 1, input_tokens = input_tokens + 3, output_tokens = output_tokens + 5,
						cost_usd = cost_usd + 0.25`); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				written <- err
				return
			}
		}
	}()
	last := 0 // records in the previous copy
	for range 3 {
		// At least one more batch committed before each copy.
		for n := last; n <= last; time.Sleep(time.Millisecond) {
			select {
			case err := <-written:
				t.Fatalf("the writes stopped: %v", err)
			default:
			}
			if err := s.Read().QueryRow("SELECT COUNT(*) FROM usage").Scan(&n); err != nil {
				t.Fatal(err)
			}
		}
		f, err := s.Backup(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(f.Name()); !errors.Is(err, fs.ErrNotExist) || !strings.HasPrefix(filepath.Base(f.Name()), backupPrefix) {
			t.Errorf("the copy %s is not unlinked: %v", f.Name(), err)
		}
		header := make([]byte, 16)
		if _, err := f.ReadAt(header, 0); err != nil || string(header) != "SQLite format 3\x00" {
			t.Errorf("header %q, error %v", header, err)
		}
		db := copyOf(t, f)
		_ = f.Close()
		checkIntegrity(t, db)
		var records, input, output int
		var cost float64
		if err := db.QueryRow("SELECT COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cost_usd), 0) FROM usage").
			Scan(&records, &input, &output, &cost); err != nil {
			t.Fatal(err)
		}
		var daily [3]int
		var dailyCost float64
		if err := db.QueryRow("SELECT COALESCE(SUM(requests), 0), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cost_usd), 0) FROM usage_daily").
			Scan(&daily[0], &daily[1], &daily[2], &dailyCost); err != nil {
			t.Fatal(err)
		}
		if records <= last || records%10 != 0 || daily != [3]int{records, input, output} || dailyCost != cost {
			t.Errorf("the copy holds %d records (%d, %d, %v) and usage_daily %v %v", records, input, output, cost, daily, dailyCost)
		}
		last = records
		if v := pragma(t, db, "user_version"); v != "3" {
			t.Errorf("the copy's user_version %s", v)
		}
	}
	close(stop)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if left := copies(t, dir); len(left) != 0 {
		t.Errorf("files left: %v", left)
	}

	// A copy that fails a check, or whose context ends, is an error and is
	// removed.
	t.Cleanup(func() { vacuumed = nil })
	for name, tc := range map[string]struct {
		damage func(t *testing.T, path string, cancel context.CancelFunc)
		want   string
	}{
		"page 2 overwritten": {func(t *testing.T, path string, _ context.CancelFunc) {
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			header := make([]byte, 100)
			if _, err := f.ReadAt(header, 0); err != nil {
				t.Fatal(err)
			}
			size := int64(header[16])<<8 | int64(header[17])
			if size == 1 {
				size = 65536
			}
			if _, err := f.WriteAt(bytes.Repeat([]byte{0xA5}, int(size)), size); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}, "the copy failed its check"},
		// Rows and an index that disagree, which only integrity_check sees.
		"index redefined": {func(t *testing.T, path string, _ context.CancelFunc) {
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.Exec("PRAGMA writable_schema = ON; UPDATE sqlite_schema SET sql = 'CREATE INDEX idx_usage_key_time ON usage(model, completed_at)' WHERE name = 'idx_usage_key_time'"); err != nil {
				t.Fatal(err)
			}
		}, "integrity_check: row 1 missing from index idx_usage_key_time"},
		"foreign key broken": {func(t *testing.T, path string, _ context.CancelFunc) {
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.Exec("INSERT INTO targets (id, provider, upstream_model, spec, updated_at) VALUES ('p/m', 'nobody', 'm', '{}', 0)"); err != nil {
				t.Fatal(err)
			}
		}, "foreign_key_check: targets rowid 1 refers to a missing providers row"},
		"newer user_version": {func(t *testing.T, path string, _ context.CancelFunc) {
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.Exec("PRAGMA user_version = 4"); err != nil {
				t.Fatal(err)
			}
		}, "user_version 4; this binary's is 3"},
		"context ended": {func(_ *testing.T, _ string, cancel context.CancelFunc) { cancel() }, "context canceled"},
	} {
		cancelled, cancel := context.WithCancel(ctx)
		vacuumed = func(path string) { tc.damage(t, path, cancel) }
		f, err := s.Backup(cancelled)
		cancel()
		if f != nil {
			_ = f.Close()
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want %q", name, err, tc.want)
		}
		if left := copies(t, dir); len(left) != 0 {
			t.Errorf("%s: files left: %v", name, left)
		}
	}

	// Close waits for a Backup in progress; one asked after Close has begun
	// gets ErrClosed.
	entered, release := make(chan struct{}), make(chan struct{})
	vacuumed = func(string) { close(entered); <-release }
	type result struct {
		f   *os.File
		err error
	}
	backedUp := make(chan result, 1)
	go func() { f, err := s.Backup(ctx); backedUp <- result{f, err} }()
	<-entered
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
	if f, err := s.Backup(ctx); !errors.Is(err, ErrClosed) || f != nil {
		t.Errorf("a Backup after Close: %v", err)
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned while a Backup was running: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	r := <-backedUp
	if r.err != nil {
		t.Fatal(r.err)
	}
	checkIntegrity(t, copyOf(t, r.f))
	_ = r.f.Close()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if left := copies(t, dir); len(left) != 0 {
		t.Errorf("files left: %v", left)
	}
}
