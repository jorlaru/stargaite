package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/store"
	"github.com/jorlaru/stargaite/internal/transport"
)

// testNow is the fixed clock of these tests, in Unix milliseconds.
const testNow = 1_790_000_000_000

// secretValue is a credential value that must appear in no row, result,
// finding or formatted snapshot.
const secretValue = "sk-" + planted + "-credential"

// answers is what the injected resolver knows; any other name fails the test.
var answers = map[string][]string{
	"api.example.com":   {"93.184.216.34"},
	"llm.internal.test": {"10.0.0.5"},
	"gone.example.com":  nil, // no such host
}

func testEnv(t *testing.T, vars map[string]string) func() Env {
	return func() Env {
		return Env{
			Lookup:  func(name string) (string, bool) { v, ok := vars[name]; return v, ok },
			Resolve: fakeResolver(t, answers),
		}
	}
}

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func openService(t *testing.T, st *store.Store, env func() Env) *Service {
	t.Helper()
	s, err := Open(context.Background(), st, env, func() time.Time { return time.UnixMilli(testNow) })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// newService opens a service on an empty store with P_API_KEY resolving.
func newService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st := openStore(t, t.TempDir())
	return openService(t, st, testEnv(t, map[string]string{"P_API_KEY": secretValue})), st
}

func change(summary string, f func(*State)) Change {
	return Change{Summary: summary, Kind: "configuration", Mutate: func(s *State) (map[string]string, error) {
		f(s)
		return nil, nil
	}}
}

var hash1, hash2 = sha256.Sum256([]byte("token-1")), sha256.Sum256([]byte("token-2"))

// seed declares one of each resource: provider p, target p/m, alias fast,
// key k with one secret.
func seed(s *State) {
	s.Doc.Providers["p"] = domain.Provider{Protocol: domain.ProtocolOpenAI, BaseURL: "https://api.example.com/v1", APIKeyEnv: "P_API_KEY", Billing: domain.BillingNone}
	s.Doc.Targets["p/m"] = domain.Target{Kind: domain.KindChat}
	s.Doc.Models["fast"] = domain.Model{Targets: []string{"p/m"}}
	s.Doc.Keys["k"] = domain.Key{Models: []string{"fast"}}
	s.Secrets = append(s.Secrets, KeySecret{PublicID: "aaaaaaaaaaaa", KeyName: "k", Hash: hash1, CreatedAt: testNow})
}

func commit(t *testing.T, s *Service, ch Change) Result {
	t.Helper()
	res, err := s.Commit(context.Background(), ch)
	if err != nil {
		t.Fatalf("%s: %v", ch.Summary, err)
	}
	return res
}

// rows renders every row of a query, one string per row, NULL as "-".
func rows(t *testing.T, st *store.Store, query string) []string {
	t.Helper()
	r, err := st.Read().Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	cols, err := r.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for r.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := r.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		fields := make([]string, len(vals))
		for i, v := range vals {
			switch v := v.(type) {
			case nil:
				fields[i] = "-"
			case []byte:
				fields[i] = fmt.Sprintf("%x", v)
			default:
				fields[i] = fmt.Sprint(v)
			}
		}
		out = append(out, strings.Join(fields, " "))
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

const auditQuery = "SELECT actor, action, resource_type, resource_name, previous_value, new_value, result, revision, created_at FROM audit ORDER BY rowid"

// everything renders every row of every table, for the planted-value check.
func everything(t *testing.T, st *store.Store) string {
	t.Helper()
	var b strings.Builder
	for _, table := range rows(t, st, "SELECT name FROM sqlite_master WHERE type = 'table'") {
		b.WriteString(strings.Join(rows(t, st, "SELECT * FROM "+table), "\n"))
	}
	return b.String()
}

func wantRows(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n%s\nwant\n%s", what, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func entries(diff []DiffEntry) []string {
	out := []string{}
	for _, e := range diff {
		out = append(out, strings.Join([]string{e.Kind, e.Name, e.Action, string(e.Previous), string(e.New)}, " "))
	}
	return out
}

// TestCommitRows follows the audit and revision rows through every kind of
// change (SEC-7, OPS-4, AC-40).
func TestCommitRows(t *testing.T) {
	s, st := newService(t)
	first := s.Snapshot()
	if first.Revision != 0 || len(first.Providers) != 0 || first.Doc.Version != 1 {
		t.Fatalf("empty store: %+v", first)
	}

	res := commit(t, s, Change{Summary: "apply", Kind: "configuration", Mutate: func(st *State) (map[string]string, error) {
		seed(st)
		return map[string]string{"k": "the-token"}, nil
	}})
	wantRows(t, "diff of the seed", entries(res.Diff), []string{
		`provider p created  {"protocol":"openai","base_url":"https://api.example.com/v1","api_key_env":"P_API_KEY","billing":"none"}`,
		`target p/m created  {"kind":"chat"}`,
		`model fast created  {"targets":["p/m"]}`,
		`key k created  {"models":["fast"]}`,
	})
	if res.Revision != 1 || !res.Changed || len(res.Findings) != 0 || res.Secrets["k"] != "the-token" {
		t.Errorf("result %+v", res)
	}
	snap := s.Snapshot()
	if snap.Revision != 1 || snap.Providers["p"].Disabled || snap.Models["fast"].Targets[0] != snap.Targets["p/m"] || snap.Targets["p/m"].Provider != snap.Providers["p"] {
		t.Errorf("snapshot %+v", snap)
	}
	at := fmt.Sprint(testNow)
	wantRows(t, "revisions", rows(t, st, "SELECT * FROM revisions"), []string{"1 admin apply " + at})
	wantRows(t, "audit", rows(t, st, auditQuery), []string{
		`admin apply provider p - {"protocol":"openai","base_url":"https://api.example.com/v1","api_key_env":"P_API_KEY","billing":"none"} applied 1 ` + at,
		`admin apply target p/m - {"kind":"chat"} applied 1 ` + at,
		`admin apply model fast - {"targets":["p/m"]} applied 1 ` + at,
		`admin apply key k - {"models":["fast"]} applied 1 ` + at,
	})
	wantRows(t, "configuration rows", slices.Concat(
		rows(t, st, "SELECT * FROM providers"),
		rows(t, st, "SELECT * FROM targets"),
		rows(t, st, "SELECT * FROM models"),
		rows(t, st, "SELECT * FROM model_targets"),
		rows(t, st, "SELECT * FROM keys"),
		rows(t, st, "SELECT * FROM key_models"),
		rows(t, st, "SELECT * FROM key_secrets"),
	), []string{
		`p {"protocol":"openai","base_url":"https://api.example.com/v1","api_key_env":"P_API_KEY","billing":"none"} ` + at,
		`p/m p m {"kind":"chat"} - - ` + at,
		`fast {} ` + at,
		`fast 0 p/m`,
		`k {} ` + at + ` -`,
		`k fast`,
		fmt.Sprintf("aaaaaaaaaaaa k %x %s -", hash1, at),
	})

	// An empty diff: one unchanged row under the change's own kind and name,
	// no revision, and a new snapshot under the same revision.
	res = commit(t, s, Change{Summary: "providers set p", Kind: KindProvider, Name: "p", Mutate: func(*State) (map[string]string, error) { return nil, nil }})
	if res.Revision != 1 || res.Changed || len(res.Diff) != 0 || s.Snapshot() == snap || s.Snapshot().Revision != 1 {
		t.Errorf("unchanged: %+v", res)
	}

	// Refusals: 404 and 409 from Mutate, 400 from validation; each audited
	// once, and a dry run writes nothing.
	refusals := []struct {
		ch   Change
		code string
	}{
		{Change{Summary: "providers remove nope", Kind: KindProvider, Name: "nope", Mutate: func(*State) (map[string]string, error) {
			return nil, domain.NewError(domain.CodeNotFound, "provider nope does not exist")
		}}, domain.CodeNotFound},
		{Change{Summary: "keys create k", Kind: KindKey, Name: "k", Mutate: func(s *State) (map[string]string, error) {
			delete(s.Doc.Providers, "p") // changes to the copy are thrown away
			return nil, domain.NewError(domain.CodeConflict, "key k exists")
		}}, domain.CodeConflict},
		{change("models add slow", func(s *State) { s.Doc.Models["slow"] = domain.Model{Targets: []string{"p/none"}} }), domain.CodeInvalidRequest},
	}
	for _, dry := range []bool{true, false} {
		for _, r := range refusals {
			r.ch.DryRun = dry
			_, err := s.Commit(context.Background(), r.ch)
			var de *domain.Error
			if !errors.As(err, &de) || de.Code != r.code || de.Status == http.StatusInternalServerError {
				t.Errorf("%s (dry run %v): error %v", r.ch.Summary, dry, err)
			}
			if r.code == domain.CodeInvalidRequest && (len(de.Findings) != 1 || de.Findings[0].Code != "unknown_target") {
				t.Errorf("findings %+v", de.Findings)
			}
		}
	}
	wantRows(t, "audit after the refusals", rows(t, st, auditQuery+" LIMIT -1 OFFSET 4"), []string{
		`admin providers set p provider p - - unchanged - ` + at,
		`admin providers remove nope provider nope - - refused:not_found - ` + at,
		`admin keys create k key k - - refused:conflict - ` + at,
		`admin models add slow configuration  - - refused:invalid_request - ` + at,
	})

	// A dry run that would succeed answers the diff and writes nothing.
	before := s.Snapshot()
	res = commit(t, s, Change{Summary: "providers set p", DryRun: true, Mutate: func(st *State) (map[string]string, error) {
		edit(st.Doc.Providers, "p", func(p *domain.Provider) { p.Billing = domain.BillingSubscription })
		return map[string]string{"k": "never-shown"}, nil
	}})
	if res.Revision != 1 || !res.Changed || len(res.Diff) != 1 || res.Secrets != nil || s.Snapshot() != before {
		t.Errorf("dry run: %+v", res)
	}
	if n := len(rows(t, st, "SELECT * FROM audit")); n != 8 {
		t.Errorf("%d audit rows after the dry runs, want 8", n)
	}

	// Update, rotate, then revoke, remove and settings.
	res = commit(t, s, change("providers set p", func(st *State) {
		edit(st.Doc.Providers, "p", func(p *domain.Provider) { p.Billing = domain.BillingSubscription })
		edit(st.Doc.Keys, "k", func(k *domain.Key) { k.RPMLimit = 10 })
		st.Secrets[0].ValidUntil = testNow + 3_600_000
		st.Secrets = append(st.Secrets, KeySecret{PublicID: "bbbbbbbbbbbb", KeyName: "k", Hash: hash2, CreatedAt: testNow})
		st.Doc.Settings.Timeouts.TotalMS = 60_000
	}))
	wantRows(t, "diff of the update", entries(res.Diff), []string{
		`provider p updated {"protocol":"openai","base_url":"https://api.example.com/v1","api_key_env":"P_API_KEY","billing":"none"} {"protocol":"openai","base_url":"https://api.example.com/v1","api_key_env":"P_API_KEY","billing":"subscription"}`,
		`key k updated {"models":["fast"]} {"models":["fast"],"rpm_limit":10}`,
		`key k rotated [{"public_id":"aaaaaaaaaaaa"}] [{"public_id":"aaaaaaaaaaaa","valid_until":1790003600000},{"public_id":"bbbbbbbbbbbb"}]`,
		`setting timeouts created  {"total_ms":60000}`,
	})
	if res.Revision != 2 {
		t.Errorf("revision %d", res.Revision)
	}
	wantRows(t, "secrets after the rotation", rows(t, st, "SELECT public_id, valid_until FROM key_secrets ORDER BY public_id"),
		[]string{"aaaaaaaaaaaa 1790003600000", "bbbbbbbbbbbb -"})

	res = commit(t, s, change("apply --prune", func(st *State) {
		delete(st.Doc.Keys, "k")
		st.Revoked["k"] = testNow
		st.Secrets = nil
		delete(st.Doc.Models, "fast")
		delete(st.Doc.Targets, "p/m")
		delete(st.Doc.Providers, "p")
		st.Doc.Settings.Timeouts = domain.Timeouts{}
	}))
	wantRows(t, "diff of the prune", entries(res.Diff), []string{
		`provider p removed {"protocol":"openai","base_url":"https://api.example.com/v1","api_key_env":"P_API_KEY","billing":"subscription"} `,
		`target p/m removed {"kind":"chat"} `,
		`model fast removed {"targets":["p/m"]} `,
		`key k revoked {"models":["fast"],"rpm_limit":10} `,
		`setting timeouts removed {"total_ms":60000} `,
	})
	wantRows(t, "rows after the prune", slices.Concat(
		rows(t, st, "SELECT name, revoked_at FROM keys"),
		rows(t, st, "SELECT * FROM key_secrets"),
		rows(t, st, "SELECT * FROM providers"),
		rows(t, st, "SELECT * FROM settings"),
		rows(t, st, "SELECT revision, summary FROM revisions"),
	), []string{"k " + at, "1 apply", "2 providers set p", "3 apply --prune"})
	if snap := s.Snapshot(); snap.Revision != 3 || len(snap.Keys) != 0 || len(snap.secrets) != 0 {
		t.Errorf("snapshot after the prune: %+v", snap)
	}
	if n := len(rows(t, st, "SELECT * FROM audit WHERE revision = 3 AND result = 'applied'")); n != 5 {
		t.Errorf("%d audit rows of revision 3, want 5", n)
	}

	// A revoked name is never reused or revived.
	for _, f := range []func(*State){
		func(st *State) { st.Doc.Keys["k"] = domain.Key{Models: []string{}} },
		func(st *State) { delete(st.Revoked, "k") },
	} {
		if _, err := s.Commit(context.Background(), change("keys create k", f)); err == nil || err.(*domain.Error).Status != http.StatusInternalServerError {
			t.Errorf("a revoked key revived: %v", err)
		}
	}
}

// TestCommitRefusesKeyChangesNoCommandMakes: a key is never deleted, and a
// secret always belongs to a declared key.
func TestCommitRefusesKeyChangesNoCommandMakes(t *testing.T) {
	s, st := newService(t)
	commit(t, s, change("apply", func(st *State) {
		seed(st)
		st.Doc.Keys["old"] = domain.Key{Models: []string{"fast"}}
	}))
	commit(t, s, change("keys revoke old", func(st *State) {
		delete(st.Doc.Keys, "old")
		st.Revoked["old"] = testNow
	}))
	before := everything(t, st)
	for name, f := range map[string]func(*State){
		"deleted key":        func(st *State) { delete(st.Doc.Keys, "k") },
		"revoked unknown":    func(st *State) { st.Revoked["other"] = testNow },
		"revoked_at changed": func(st *State) { st.Revoked["old"] = testNow + 999 },
		"orphan secret":      func(st *State) { st.Secrets[0].KeyName = "other" },
		"secret moved hash":  func(st *State) { st.Secrets[0].Hash = hash2 },
		"revision set":       func(st *State) { st.Revision += 5 },
	} {
		_, err := s.Commit(context.Background(), change(name, f))
		if de, ok := err.(*domain.Error); !ok || de.Status != http.StatusInternalServerError {
			t.Errorf("%s: error %v", name, err)
		}
	}
	if s.Snapshot().Revision != 2 || s.state.Revoked["old"] != testNow || everything(t, st) != before {
		t.Error("a refused key change left a trace")
	}
}

// TestRefusedChangeLeavesStateUntouched: a 400 publishes nothing and a later
// change starts from the committed state (OPS-5, AC-39).
func TestRefusedChangeLeavesStateUntouched(t *testing.T) {
	s, _ := newService(t)
	commit(t, s, change("apply", seed))
	before := s.Snapshot()
	beforeDoc, _ := Encode(before.Doc)
	_, err := s.Commit(context.Background(), change("break everything", func(st *State) {
		delete(st.Doc.Targets, "p/m") // alias fast is left without its target
		st.Doc.Providers["q"] = domain.Provider{Protocol: domain.ProtocolOpenAI, BaseURL: "https://api.example.com"}
	}))
	if de, ok := err.(*domain.Error); !ok || de.Status != http.StatusBadRequest {
		t.Fatalf("error %v", err)
	}
	afterDoc, _ := Encode(s.Snapshot().Doc)
	if s.Snapshot() != before || !bytes.Equal(beforeDoc, afterDoc) {
		t.Error("the refused change was published")
	}
	res := commit(t, s, change("models set fast", func(st *State) {
		edit(st.Doc.Models, "fast", func(m *domain.Model) { m.MaxOutputTokens = 100 })
	}))
	if len(res.Diff) != 1 || res.Diff[0].Kind != KindModel {
		t.Errorf("the next change saw the refused one: %v", entries(res.Diff))
	}
}

// TestCommitStoreFailure injects a failing transaction: nothing is published,
// no row is left, and the next change gets the revision the failed one would
// have had.
func TestCommitStoreFailure(t *testing.T) {
	s, st := newService(t)
	ctx := context.Background()
	trigger := func(stmt string) {
		t.Helper()
		if err := st.Write(ctx, func(tx *sql.Tx) error { _, err := tx.Exec(stmt); return err }); err != nil {
			t.Fatal(err)
		}
	}
	trigger("CREATE TRIGGER fail_revision BEFORE INSERT ON revisions BEGIN SELECT RAISE(ABORT, 'injected failure'); END")
	before := s.Snapshot()
	_, err := s.Commit(ctx, change("apply", seed))
	if de, ok := err.(*domain.Error); !ok || de.Status != http.StatusInternalServerError || !strings.Contains(de.Message, "injected failure") {
		t.Fatalf("error %v", err)
	}
	if s.Snapshot() != before || len(s.state.Doc.Providers) != 0 {
		t.Error("a failed change was published")
	}
	for _, table := range []string{"providers", "targets", "models", "keys", "key_secrets", "revisions", "audit"} {
		if n := len(rows(t, st, "SELECT * FROM "+table)); n != 0 {
			t.Errorf("%d rows in %s after a failed change", n, table)
		}
	}

	// An audit row that cannot be written turns a refusal into a 500.
	trigger("CREATE TRIGGER fail_audit BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT, 'injected failure'); END")
	_, err = s.Commit(ctx, Change{Summary: "providers remove nope", Mutate: func(*State) (map[string]string, error) {
		return nil, domain.NewError(domain.CodeNotFound, "nope")
	}})
	if de, ok := err.(*domain.Error); !ok || de.Status != http.StatusInternalServerError {
		t.Errorf("refusal with a failing audit write: %v", err)
	}

	trigger("DROP TRIGGER fail_revision")
	trigger("DROP TRIGGER fail_audit")
	if res := commit(t, s, change("apply", seed)); res.Revision != 1 || s.Snapshot().Revision != 1 {
		t.Errorf("after the failure: %+v", res)
	}
}

// TestCommitCancelledWhileQueued: once the write is queued the request's
// context no longer matters; the change is committed and published.
func TestCommitCancelledWhileQueued(t *testing.T) {
	st := openStore(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	inTx := false
	// The clock is read inside the transaction, on the writer goroutine: the
	// write is queued and running when the context ends.
	s, err := Open(context.Background(), st, testEnv(t, nil), func() time.Time {
		inTx = true
		cancel()
		return time.UnixMilli(testNow)
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Commit(ctx, change("apply", seed))
	if err != nil || !inTx || ctx.Err() == nil {
		t.Fatalf("result %+v, error %v, clock read %v, context %v", res, err, inTx, ctx.Err())
	}
	if res.Revision != 1 || s.Snapshot().Revision != 1 || len(rows(t, st, "SELECT * FROM revisions")) != 1 {
		t.Errorf("result %+v; snapshot revision %d", res, s.Snapshot().Revision)
	}
}

// TestCommitConcurrent runs changes and snapshot loads at once (-race): every
// change gets its own revision, and a published snapshot is always whole.
func TestCommitConcurrent(t *testing.T) {
	s, st := newService(t)
	const writers = 16
	var stop atomic.Bool
	var wg, readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			last := int64(0)
			for !stop.Load() {
				snap := s.Snapshot()
				if snap.Revision < last || int64(len(snap.Providers)) != snap.Revision || int64(len(snap.Doc.Providers)) != snap.Revision {
					t.Errorf("snapshot of revision %d after %d, with %d providers", snap.Revision, last, len(snap.Providers))
					return
				}
				last = snap.Revision
			}
		})
	}
	revisions := make(chan int64, writers)
	for i := range writers {
		wg.Go(func() {
			name := fmt.Sprintf("p%02d", i)
			res, err := s.Commit(context.Background(), change("providers add "+name, func(st *State) {
				st.Doc.Providers[name] = domain.Provider{Protocol: domain.ProtocolOpenAI, BaseURL: "https://api.example.com", Billing: domain.BillingNone}
			}))
			if err != nil {
				t.Error(err)
			}
			revisions <- res.Revision
		})
	}
	wg.Wait()
	stop.Store(true)
	readers.Wait()
	close(revisions)
	var got []int64
	for r := range revisions {
		got = append(got, r)
	}
	slices.Sort(got)
	for i, r := range got {
		if r != int64(i+1) {
			t.Fatalf("revisions %v", got)
		}
	}
	if n := len(rows(t, st, "SELECT * FROM revisions")); n != writers || s.Snapshot().Revision != writers {
		t.Errorf("%d revision rows, snapshot revision %d", n, s.Snapshot().Revision)
	}
}

// TestReload: a restart rebuilds the same State and snapshot from the store
// under the stored revision (RES-9), which also repairs a crash between the
// commit and the publication.
func TestReload(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	// No resolver: the hosts of annex B are left unresolved, with a warning.
	env := func() Env { return Env{Lookup: noEnviron} }
	s := openService(t, st, env)
	doc := annexBJSON(t)
	commit(t, s, change("apply", func(st *State) {
		st.Doc = doc
		st.Doc.Keys["old"] = domain.Key{Models: []string{"*"}}
		seed(st)
		st.Secrets = append(st.Secrets, KeySecret{PublicID: "cccccccccccc", KeyName: "old", Hash: hash2, CreatedAt: 5, ValidUntil: 9})
	}))
	commit(t, s, change("keys revoke old", func(st *State) {
		delete(st.Doc.Keys, "old")
		st.Revoked["old"] = testNow
		st.Secrets = slices.DeleteFunc(st.Secrets, func(k KeySecret) bool { return k.KeyName == "old" })
	}))
	want, err := Encode(s.state)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openService(t, openStore(t, dir), env)
	got, err := Encode(reopened.state)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("reloaded state\n%s\nwant\n%s", got, want)
	}
	if snap := reopened.Snapshot(); snap.Revision != 2 || len(snap.Keys) != 3 || snap.Keys["old"] != nil {
		t.Errorf("reloaded snapshot: revision %d, %d keys", snap.Revision, len(snap.Keys))
	}
	// A change after the restart continues the revisions.
	if res := commit(t, reopened, change("keys set k", func(st *State) {
		edit(st.Doc.Keys, "k", func(k *domain.Key) { k.RPMLimit = 1 })
	})); res.Revision != 3 {
		t.Errorf("revision after the restart %d", res.Revision)
	}
}

// TestOpenPublishesAnInvalidStoredConfiguration: a stored configuration that
// this binary finds invalid is still published, with its findings (design
// section 13, point 13); a row it cannot decode stops the start.
func TestOpenPublishesAnInvalidStoredConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, spec string
		ok         bool
	}{
		{"invalid", `{"protocol":"gopher","base_url":"https://api.example.com","billing":"none"}`, true},
		{"undecodable", `{"protocol":"openai","colour":"blue"}`, false},
	} {
		st := openStore(t, t.TempDir())
		if err := st.Write(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.Exec("INSERT INTO providers (name, spec, updated_at) VALUES ('p', ?, 0)", tc.spec)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		s, err := Open(context.Background(), st, testEnv(t, nil), time.Now)
		if !tc.ok {
			if err == nil || !strings.Contains(err.Error(), `provider p: unknown member "/colour"`) {
				t.Errorf("%s: error %v", tc.name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := summary(s.Snapshot().Findings); !slices.Equal(got, []string{"error OPS-5 invalid_value providers/p protocol"}) {
			t.Errorf("%s: findings %q", tc.name, got)
		}
	}
}

// TestDryRunReportsAnUnresolvedCredential, and a credential added to the env
// file resolves on the next accepted change, with no restart (OPS-6, AC-44).
func TestCredentialResolution(t *testing.T) {
	st := openStore(t, t.TempDir())
	envFile := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(envFile, []byte("# nothing yet\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := openService(t, st, func() Env { return NewEnv(noEnviron, envFile, fakeResolver(t, answers)) })

	res := commit(t, s, Change{Summary: "providers add p", DryRun: true, Mutate: func(st *State) (map[string]string, error) {
		seed(st)
		return nil, nil
	}})
	if got := summary(res.Findings); !slices.Equal(got, []string{"warning OPS-6 credential_unresolved providers/p api_key_env"}) {
		t.Errorf("dry run findings %q", got)
	}
	if n := len(rows(t, st, "SELECT * FROM audit")); n != 0 {
		t.Errorf("the dry run wrote %d audit rows", n)
	}

	commit(t, s, change("providers add p", seed))
	if p := s.Snapshot().Providers["p"]; !p.Disabled || len(s.Snapshot().Findings) != 1 {
		t.Errorf("unresolved credential: provider %+v, findings %v", p, s.Snapshot().Findings)
	}
	if err := os.WriteFile(envFile, []byte("P_API_KEY="+secretValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := commit(t, s, change("settings set", func(st *State) { st.Doc.Settings.MaxBodyBytes = 1 << 20 })); res.Revision != 2 {
		t.Errorf("revision %d", res.Revision)
	}
	p := s.Snapshot().Providers["p"]
	if p.Disabled || p.Credential == (transport.Secret{}) || len(s.Snapshot().Findings) != 0 {
		t.Errorf("after the env file changed: provider %+v, findings %v", p, s.Snapshot().Findings)
	}
}

// TestSetTimeResolution: a change resolves the URLs it sets, and only those
// (SEC-4, design section 13, point 14).
func TestSetTimeResolution(t *testing.T) {
	st := openStore(t, t.TempDir())
	var lookups atomic.Int32
	s := openService(t, st, func() Env {
		resolve := fakeResolver(t, answers)
		return Env{Lookup: noEnviron, Resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			lookups.Add(1)
			return resolve(ctx, host)
		}}
	})
	private := domain.Provider{Protocol: domain.ProtocolOpenAI, BaseURL: "http://llm.internal.test:8080/v1", Billing: domain.BillingNone}
	_, err := s.Commit(context.Background(), change("providers add local", func(st *State) { st.Doc.Providers["local"] = private }))
	if de, ok := err.(*domain.Error); !ok || !slices.Equal(summary(de.Findings), []string{
		"error SEC-4 url_refused providers/local base_url", // plain http without the flag, by CheckURL
	}) {
		t.Fatalf("error %v", err)
	}
	private.BaseURL = "https://llm.internal.test/v1"
	_, err = s.Commit(context.Background(), change("providers add local", func(st *State) { st.Doc.Providers["local"] = private }))
	if de, ok := err.(*domain.Error); !ok || !slices.Equal(summary(de.Findings), []string{"error SEC-4 url_refused providers/local base_url"}) || lookups.Load() != 1 {
		t.Fatalf("a name resolving to a private address: %v, %d lookups", err, lookups.Load())
	}
	private.AllowPrivateNetwork = true
	commit(t, s, change("providers add local", func(st *State) { st.Doc.Providers["local"] = private }))
	res := commit(t, s, change("providers add gone", func(st *State) {
		st.Doc.Providers["gone"] = domain.Provider{Protocol: domain.ProtocolOpenAI, BaseURL: "https://gone.example.com", Billing: domain.BillingNone}
	}))
	if got := summary(res.Findings); !slices.Equal(got, []string{"warning SEC-4 url_unresolved providers/gone base_url"}) || lookups.Load() != 3 {
		t.Errorf("findings %q, %d lookups", got, lookups.Load())
	}
	commit(t, s, change("settings set", func(st *State) { st.Doc.Settings.MaxBodyBytes = 1 }))
	if lookups.Load() != 3 {
		t.Errorf("a change that sets no URL resolved one: %d lookups", lookups.Load())
	}
}

// TestPlantedCredential: a credential value reaches the snapshot as a
// transport.Secret and nothing else: no row, result, finding or format.
func TestPlantedCredential(t *testing.T) {
	s, st := newService(t)
	res := commit(t, s, change("apply", seed))
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	p := snap.Providers["p"]
	if p.Disabled || p.Credential == (transport.Secret{}) {
		t.Fatal("the credential did not resolve")
	}
	texts := []string{string(out), everything(t, st), fmt.Sprint(snap.Findings)}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		texts = append(texts, fmt.Sprintf(verb, p), fmt.Sprintf(verb, *p), fmt.Sprintf(verb, snap.Targets["p/m"]))
	}
	for _, text := range texts {
		if strings.Contains(text, planted) || strings.Contains(text, fmt.Sprintf("%x", planted)) {
			t.Errorf("the planted credential leaked: %.300s", text)
		}
	}
	if !strings.Contains(fmt.Sprintf("%+v", *p), "Credential:[redacted]") {
		t.Errorf("%+v", *p)
	}
}

func TestSnapshotBuild(t *testing.T) {
	doc := annexBJSON(t)
	st := &State{Doc: doc, Revision: 7, Secrets: []KeySecret{
		{PublicID: "aaaaaaaaaaaa", KeyName: "invoice-agent", Hash: hash1, ValidUntil: 9},
		{PublicID: "bbbbbbbbbbbb", KeyName: "invoice-agent", Hash: hash2},
	}}
	snap := build(st, Env{Lookup: func(name string) (string, bool) { return "v", name == "ANTHROPIC_API_KEY" }}, nil)
	if snap.Revision != 7 || snap.Doc != &st.Doc || len(snap.Keys) != len(doc.Keys) {
		t.Errorf("snapshot %+v", snap)
	}
	if snap.Providers["anthropic"].Disabled || !snap.Providers["openrouter"].Disabled || snap.Providers["tei-embed"].Disabled {
		t.Error("disabled follows the credential")
	}
	for name, m := range snap.Models {
		var ids []string
		for _, tg := range m.Targets {
			ids = append(ids, tg.ID)
			if tg.Provider == nil || tg.Provider != snap.Providers[strings.Split(tg.ID, "/")[0]] || tg.Upstream != strings.SplitN(tg.ID, "/", 2)[1] {
				t.Errorf("target %+v", tg)
			}
		}
		if !slices.Equal(ids, doc.Models[name].Targets) {
			t.Errorf("alias %s targets %v", name, ids)
		}
	}
	k := snap.Keys["invoice-agent"]
	if a, b := snap.secrets["aaaaaaaaaaaa"], snap.secrets["bbbbbbbbbbbb"]; len(snap.secrets) != 2 || a.key != k || b.key != k || a.hash != hash1 || a.validUntil != 9 || b.hash != hash2 || b.validUntil != 0 {
		t.Errorf("secrets %+v", snap.secrets)
	}

	// Defaults are applied to Settings, never to the declaration. Annex B
	// declares every setting that has a default.
	declared, _ := Encode(doc.Settings)
	if published, _ := Encode(snap.Settings); !bytes.Equal(published, declared) {
		t.Errorf("settings %s, want %s", published, declared)
	}
	empty := build(&State{}, Env{}, nil).Settings
	if empty.MaxBodyBytes != domain.DefaultMaxBodyBytes || empty.MaxResponseBytes != domain.DefaultMaxResponseBytes ||
		empty.Timeouts != (domain.Timeouts{ConnectMS: domain.DefaultConnectMS, FirstByteMS: domain.DefaultFirstByteMS, StreamIdleMS: domain.DefaultStreamIdleMS, TotalMS: domain.DefaultTotalMS}) ||
		empty.Retention != (domain.Retention{UsageDays: domain.DefaultUsageDays, UsageDailyMonths: domain.DefaultUsageDailyMonths, AuditMonths: domain.DefaultAuditMonths}) || empty.Telemetry != nil {
		t.Errorf("defaults %+v", empty)
	}
	withTelemetry := &State{Doc: domain.Document{Settings: domain.Settings{Telemetry: &domain.Telemetry{OTLPEndpoint: "https://otel.example.com"}}}}
	tel := build(withTelemetry, Env{}, nil).Settings.Telemetry
	if tel == withTelemetry.Doc.Settings.Telemetry || tel.TrustTraceparent == nil || !*tel.TrustTraceparent || tel.ContentMaxBytes != domain.DefaultContentMaxBytes ||
		withTelemetry.Doc.Settings.Telemetry.TrustTraceparent != nil {
		t.Errorf("telemetry %+v", tel)
	}
}

// BenchmarkCommitScale18 changes one key of a configuration at the sizes of
// kick-off section 18 (100 providers, 1,000 targets, 1,000 aliases, 10,000
// keys with a secret each), which must publish within 1 s.
func BenchmarkCommitScale18(b *testing.B) {
	st, err := store.Open(context.Background(), b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	s, err := Open(context.Background(), st, func() Env { return Env{Lookup: noEnviron} }, time.Now)
	if err != nil {
		b.Fatal(err)
	}
	doc := scale18()
	doc.Keys["k00000"] = domain.Key{Models: []string{"a0000"}}
	for i := 1; i < 100; i++ {
		doc.Providers[fmt.Sprintf("p%02d", i)] = domain.Provider{Protocol: domain.ProtocolOpenAI, BaseURL: "https://198.51.100.1/v1", Billing: domain.BillingNone, AllowPrivateNetwork: true}
	}
	start := time.Now()
	if _, err := s.Commit(context.Background(), change("apply", func(st *State) {
		st.Doc = doc
		for name := range doc.Keys {
			st.Secrets = append(st.Secrets, KeySecret{PublicID: name + "xxxxxx", KeyName: name, Hash: hash1, CreatedAt: 1})
		}
	})); err != nil {
		b.Fatal(err)
	}
	b.Logf("the first apply at this scale took %v", time.Since(start))
	i := 0
	for b.Loop() {
		i++
		if _, err := s.Commit(context.Background(), change("keys set k00001", func(st *State) {
			edit(st.Doc.Keys, "k00001", func(k *domain.Key) { k.RPMLimit = int64(i) })
		})); err != nil {
			b.Fatal(err)
		}
	}
}
