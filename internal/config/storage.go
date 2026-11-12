package config

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"maps"
	"slices"

	"github.com/jorlaru/stargaite/internal/domain"
)

// The configuration SQL: what a State is in the tables of annex A. A spec
// column holds the canonical encoding of a declaration, less what has its own
// rows: an alias's targets and a key's models. settings has one row per
// top-level member.

// load reads the stored configuration. Every spec passes through the strict
// decoder, so a row this binary does not understand stops the start.
func load(ctx context.Context, db *sql.DB) (*State, error) {
	st := &State{
		Doc: domain.Document{
			Version:   1,
			Providers: map[string]domain.Provider{},
			Targets:   map[string]domain.Target{},
			Models:    map[string]domain.Model{},
			Keys:      map[string]domain.Key{},
		},
		Revoked: map[string]int64{},
	}
	d := &st.Doc
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(revision), 0) FROM revisions").Scan(&st.Revision); err != nil {
		return nil, err
	}

	settings := map[string]jsontext.Value{}
	err := each(ctx, db, "SELECT name, value FROM settings", func(rows *sql.Rows) error {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return err
		}
		settings[name] = jsontext.Value(value)
		return nil
	})
	if err != nil {
		return nil, err
	}
	data, err := Encode(settings)
	if err != nil {
		return nil, err
	}
	if err := Decode(data, &d.Settings); err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}

	err = eachSpec(ctx, db, "SELECT name, spec FROM providers", "provider", func(name string, spec []byte) error {
		var p domain.Provider
		err := Decode(spec, &p)
		d.Providers[name] = p
		return err
	})
	if err != nil {
		return nil, err
	}
	err = eachSpec(ctx, db, "SELECT id, spec FROM targets", "target", func(id string, spec []byte) error {
		var t domain.Target
		err := Decode(spec, &t)
		d.Targets[id] = t
		return err
	})
	if err != nil {
		return nil, err
	}
	err = eachSpec(ctx, db, "SELECT name, spec FROM models", "model", func(name string, spec []byte) error {
		var m domain.Model
		err := Decode(spec, &m)
		d.Models[name] = m
		return err
	})
	if err != nil {
		return nil, err
	}
	err = each(ctx, db, "SELECT model, target FROM model_targets ORDER BY model, position", func(rows *sql.Rows) error {
		var model, target string
		if err := rows.Scan(&model, &target); err != nil {
			return err
		}
		m := d.Models[model]
		m.Targets = append(m.Targets, target)
		d.Models[model] = m
		return nil
	})
	if err != nil {
		return nil, err
	}

	err = each(ctx, db, "SELECT name, spec, revoked_at FROM keys", func(rows *sql.Rows) error {
		var name, spec string
		var revokedAt sql.NullInt64
		if err := rows.Scan(&name, &spec, &revokedAt); err != nil {
			return err
		}
		if revokedAt.Valid {
			st.Revoked[name] = revokedAt.Int64
			return nil
		}
		var k domain.Key
		if err := Decode([]byte(spec), &k); err != nil {
			return fmt.Errorf("key %s: %w", name, err)
		}
		k.Models = []string{} // a declared key always has a list (OPS-5)
		d.Keys[name] = k
		return nil
	})
	if err != nil {
		return nil, err
	}
	// rowid keeps the order in which a key's aliases were declared.
	err = each(ctx, db, "SELECT key_name, model FROM key_models ORDER BY key_name, rowid", func(rows *sql.Rows) error {
		var name, model string
		if err := rows.Scan(&name, &model); err != nil {
			return err
		}
		if k, ok := d.Keys[name]; ok {
			k.Models = append(k.Models, model)
			d.Keys[name] = k
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = each(ctx, db, "SELECT public_id, key_name, secret_hash, created_at, COALESCE(valid_until, 0) FROM key_secrets ORDER BY key_name, public_id", func(rows *sql.Rows) error {
		var s KeySecret
		var hash []byte
		if err := rows.Scan(&s.PublicID, &s.KeyName, &hash, &s.CreatedAt, &s.ValidUntil); err != nil {
			return err
		}
		if len(hash) != len(s.Hash) {
			return fmt.Errorf("the secret %s of key %s has a hash of %d bytes", s.PublicID, s.KeyName, len(hash))
		}
		copy(s.Hash[:], hash)
		st.Secrets = append(st.Secrets, s)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return st, nil
}

func each(ctx context.Context, db *sql.DB, query string, scan func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// eachSpec runs fn on every (name, spec) row of query; an error names the row.
func eachSpec(ctx context.Context, db *sql.DB, query, kind string, fn func(name string, spec []byte) error) error {
	return each(ctx, db, query, func(rows *sql.Rows) error {
		var name, spec string
		if err := rows.Scan(&name, &spec); err != nil {
			return err
		}
		if err := fn(name, []byte(spec)); err != nil {
			return fmt.Errorf("%s %s: %w", kind, name, err)
		}
		return nil
	})
}

// tx runs statements until the first error, which it keeps.
type tx struct {
	*sql.Tx
	err error
}

func (t *tx) exec(query string, args ...any) {
	if t.err == nil {
		_, t.err = t.Exec(query, args...)
	}
}

// text is a JSON column value: NULL for nil, otherwise TEXT, never a BLOB.
func text(b []byte) any {
	if b == nil {
		return nil
	}
	return string(b)
}

// insertAudit writes one audit row (SEC-7). kind and name come from the
// change when the row has no diff entry; a revision of 0 is NULL.
func insertAudit(t *sql.Tx, ch Change, kind, name string, previous, next []byte, result string, revision, at int64) error {
	if kind == "" {
		kind, name = ch.Kind, ch.Name
	}
	var rev any
	if revision != 0 {
		rev = revision
	}
	_, err := t.Exec(`INSERT INTO audit (id, actor, action, resource_type, resource_name, previous_value, new_value, result, revision, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rand.Text(), Actor, ch.Summary, kind, name, text(previous), text(next), result, rev, at)
	return err
}

// writeChange writes a non-empty diff, the revision and one applied audit
// row per entry. Rows go in an order the foreign keys accept: an alias's
// rows and the removed targets and providers first, then the providers,
// targets, aliases, keys and secrets that remain.
func writeChange(sqlTx *sql.Tx, ch Change, old, next *State, diff []DiffEntry, at int64) error {
	t := &tx{Tx: sqlTx}
	byKind := map[string][]DiffEntry{}
	for _, e := range diff {
		byKind[e.Kind] = append(byKind[e.Kind], e)
	}

	// An alias is written as a whole: its row goes, with its targets by cascade.
	for _, e := range byKind[KindModel] {
		t.exec("DELETE FROM models WHERE name = ?", e.Name)
	}
	for _, e := range byKind[KindTarget] {
		if e.Action == ActionRemoved {
			t.exec("DELETE FROM targets WHERE id = ?", e.Name)
		}
	}
	for _, e := range byKind[KindProvider] {
		if e.Action == ActionRemoved {
			t.exec("DELETE FROM providers WHERE name = ?", e.Name)
		}
	}

	for _, e := range byKind[KindProvider] {
		if e.Action != ActionRemoved {
			t.exec(`INSERT INTO providers (name, spec, updated_at) VALUES (?, ?, ?)
				ON CONFLICT (name) DO UPDATE SET spec = excluded.spec, updated_at = excluded.updated_at`, e.Name, text(e.New), at)
		}
	}
	for _, e := range byKind[KindTarget] {
		if e.Action != ActionRemoved {
			provider, upstream, _ := domain.SplitTarget(e.Name)
			t.exec(`INSERT INTO targets (id, provider, upstream_model, spec, updated_at) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (id) DO UPDATE SET spec = excluded.spec, updated_at = excluded.updated_at`, e.Name, provider, upstream, text(e.New), at)
		}
	}
	for _, e := range byKind[KindModel] {
		if e.Action == ActionRemoved {
			continue
		}
		m := next.Doc.Models[e.Name]
		targets := m.Targets
		m.Targets = nil
		spec, err := Encode(m)
		if err != nil {
			return err
		}
		t.exec("INSERT INTO models (name, spec, updated_at) VALUES (?, ?, ?)", e.Name, text(spec), at)
		for i, id := range targets {
			t.exec("INSERT INTO model_targets (model, position, target) VALUES (?, ?, ?)", e.Name, i, id)
		}
	}

	for _, e := range byKind[KindKey] {
		switch e.Action {
		case ActionRevoked:
			t.exec("UPDATE keys SET revoked_at = ? WHERE name = ?", next.Revoked[e.Name], e.Name)
			continue
		case ActionRotated:
			continue
		}
		k := next.Doc.Keys[e.Name]
		models := k.Models
		k.Models = nil
		spec, err := Encode(k)
		if err != nil {
			return err
		}
		if e.Action == ActionCreated {
			t.exec("INSERT INTO keys (name, spec, created_at) VALUES (?, ?, ?)", e.Name, text(spec), at)
		} else {
			t.exec("UPDATE keys SET spec = ? WHERE name = ?", text(spec), e.Name)
			t.exec("DELETE FROM key_models WHERE key_name = ?", e.Name)
		}
		for _, model := range models {
			t.exec("INSERT INTO key_models (key_name, model) VALUES (?, ?)", e.Name, model)
		}
	}
	if len(byKind[KindKey]) > 0 {
		writeSecrets(t, old.Secrets, next.Secrets)
	}

	for _, e := range byKind[KindSetting] {
		if e.Action == ActionRemoved {
			t.exec("DELETE FROM settings WHERE name = ?", e.Name)
		} else {
			t.exec(`INSERT INTO settings (name, value, updated_at) VALUES (?, ?, ?)
				ON CONFLICT (name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, e.Name, text(e.New), at)
		}
	}

	t.exec("INSERT INTO revisions (revision, actor, summary, created_at) VALUES (?, ?, ?, ?)", next.Revision, Actor, ch.Summary, at)
	for _, e := range diff {
		if t.err == nil {
			t.err = insertAudit(sqlTx, ch, e.Kind, e.Name, e.Previous, e.New, "applied", next.Revision, at)
		}
	}
	return t.err
}

// writeSecrets turns the secrets of old into those of next, by public id.
func writeSecrets(t *tx, old, next []KeySecret) {
	before := map[string]KeySecret{}
	for _, s := range old {
		before[s.PublicID] = s
	}
	after := map[string]KeySecret{}
	for _, s := range next {
		after[s.PublicID] = s
	}
	for _, id := range slices.Sorted(maps.Keys(before)) {
		if _, ok := after[id]; !ok {
			t.exec("DELETE FROM key_secrets WHERE public_id = ?", id)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(after)) {
		s, prev := after[id], before[id]
		var validUntil any
		if s.ValidUntil != 0 {
			validUntil = s.ValidUntil
		}
		switch {
		case prev.PublicID == "":
			t.exec("INSERT INTO key_secrets (public_id, key_name, secret_hash, created_at, valid_until) VALUES (?, ?, ?, ?, ?)",
				id, s.KeyName, s.Hash[:], s.CreatedAt, validUntil)
		case prev != s: // only valid_until, which diffKeys checked
			t.exec("UPDATE key_secrets SET valid_until = ? WHERE public_id = ?", validUntil, id)
		}
	}
}
