package config

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/store"
)

// State is all durable configuration. A committed State is never mutated:
// Commit hands Mutate a deep copy.
type State struct {
	Doc      domain.Document  // what export emits; revoked keys are absent
	Secrets  []KeySecret      // the secrets of the keys in Doc
	Revoked  map[string]int64 // key name -> revoked_at, in Unix milliseconds
	Revision int64
	// NeverSince is, for every scope ("global", "key:<name>",
	// "model:<alias>") whose budget is never, the revision its counter
	// started at: its budget_totals row (LIM-2). Only Commit sets it.
	NeverSince map[string]int64
	// Verified is, by target id, the last successful targets verify: the
	// targets columns verified_at and verified_version (RSN-9). Only
	// Service.Verified adds an entry; Commit drops the entry of a target it
	// changes, as writeChange clears its columns.
	Verified map[string]Verification
}

// Verification is the stored result of a successful targets verify (RSN-9).
type Verification struct {
	At      int64  `json:"verified_at"` // Unix ms: targets.verified_at
	Version string `json:"verified_version"`
}

// KeySecret is one secret of a key: the SHA-256 of the full token, never the
// token. Times are Unix milliseconds; a ValidUntil of 0 is no end.
type KeySecret struct {
	PublicID, KeyName     string
	Hash                  [32]byte
	CreatedAt, ValidUntil int64
}

// Change is one admin write: a single command or an apply.
type Change struct {
	Summary    string // "providers add openrouter": revisions.summary and audit.action
	Kind, Name string // the resource the request names ("provider", "openrouter"; "configuration" and "" for apply): the audit row of a change with no diff entry
	DryRun     bool
	// Mutate changes a deep copy of the committed State. It returns the
	// tokens it issued, by key name, or a *domain.Error that refuses the
	// change, such as 404 not_found or 409 conflict.
	Mutate func(*State) (issued map[string]string, err error)
}

// DiffEntry is one resource a change creates, updates, removes, rotates or
// revokes.
type DiffEntry struct {
	Kind     string          `json:"kind"`               // provider, target, model, key, setting
	Name     string          `json:"name"`               // a setting is named by its member of settings
	Action   string          `json:"action"`             // created, updated, removed, rotated, revoked
	Previous json.RawMessage `json:"previous,omitempty"` // declarations; for rotated, public ids and valid_until only
	New      json.RawMessage `json:"new,omitempty"`
}

// Result is what an accepted change, or a dry run, answers.
type Result struct {
	Revision int64             `json:"revision"`
	Changed  bool              `json:"changed"`
	Diff     []DiffEntry       `json:"diff"`
	Findings []domain.Finding  `json:"findings"`          // warnings; errors too when the diff is empty
	Secrets  map[string]string `json:"secrets,omitempty"` // tokens by key name, shown once (OPS-3, SEC-8); empty on a dry run
}

// Actor is the actor of every revision and audit row: the admin key is the
// only identity of phase 1.
const Actor = "admin"

// Service holds the committed State and publishes its Snapshot. Commit is the
// only way the configuration changes.
type Service struct {
	st    *store.Store
	env   func() Env
	now   func() time.Time
	mu    sync.Mutex // serializes Commit: commit order is publication order (OPS-4)
	state *State
	snap  atomic.Pointer[Snapshot]
}

// Open loads the stored configuration, checks it and publishes it under the
// stored revision. Findings do not stop it: they are in the snapshot, for the
// caller to log (design section 13, point 13). Only a store that cannot be
// read or a row that cannot be decoded is an error. Start-up resolves the
// credentials through env but no host name: SEC-4 resolves only what a change
// sets, and dial time is the authority.
//
// A never budget declared before migration 002 has no budget_totals row:
// before the first publish, Open writes one at the current revision with a
// total of 0 for each, so every published never budget has a row and a
// NeverSince entry (phase-3 design section 8).
func Open(ctx context.Context, st *store.Store, env func() Env, now func() time.Time) (*Service, error) {
	state, err := load(ctx, st.Read())
	if err != nil {
		return nil, fmt.Errorf("load the configuration: %w", err)
	}
	var missing []string
	for _, scope := range neverScopes(&state.Doc) {
		if _, ok := state.NeverSince[scope]; !ok {
			missing = append(missing, scope)
		}
	}
	if len(missing) > 0 {
		err := st.Write(ctx, func(tx *sql.Tx) error {
			for _, scope := range missing {
				kind, name := scopeRow(scope)
				if _, err := tx.Exec("INSERT OR IGNORE INTO budget_totals (scope_type, scope_name, cost_usd, since_revision) VALUES (?, ?, 0, ?)",
					kind, name, state.Revision); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("write the rows of never budgets: %w", err)
		}
		for _, scope := range missing {
			state.NeverSince[scope] = state.Revision
		}
	}
	e := env()
	findings := slices.Concat(Validate(&state.Doc), CheckResolved(ctx, &state.Doc, nil, e), Unverified(&state.Doc, state.Verified))
	s := &Service{st: st, env: env, now: now, state: state}
	s.snap.Store(build(state, e, findings))
	return s, nil
}

// Snapshot returns the published configuration. A request loads it once and
// keeps it to the end, across any change (RES-7).
func (s *Service) Snapshot() *Snapshot { return s.snap.Load() }

// Now is the clock the service was opened with: what a change records and
// what a key's valid_until is measured against, so tests inject one clock.
func (s *Service) Now() time.Time { return s.now() }

// Commit runs the change pipeline of design section 4. The error is always a
// *domain.Error: 400 with findings, the refusal Mutate returned, or 500 when
// the store fails, and then nothing is published. Every accepted change, one
// with an empty diff included, reads the env file again and republishes, so
// a Mutate that changes nothing is secrets reload (OPS-6).
func (s *Service) Commit(ctx context.Context, ch Change) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.state

	// 1-2. Mutate a deep copy.
	next, err := clone(old)
	if err != nil {
		return Result{}, internal(err)
	}
	issued, err := ch.Mutate(next)
	if de := (*domain.Error)(nil); errors.As(err, &de) && de.Status < 500 {
		return Result{}, s.refuse(ch, de)
	} else if err != nil {
		return Result{}, internal(err)
	}

	// 3. The resource-level diff.
	if next.Revision != old.Revision {
		return Result{}, internal(errors.New("the change sets the revision, which only Commit may do"))
	}
	diff, err := diffStates(old, next)
	if err != nil {
		return Result{}, internal(err)
	}
	// A stored verification goes with any change of its target or of the
	// target's provider, as writeChange clears the columns (RSN-9).
	changed := map[string]bool{}
	for _, e := range diff {
		changed[e.Kind+" "+e.Name] = true
	}
	next.Verified = maps.Clone(old.Verified)
	for id := range next.Verified {
		if provider, _, _ := domain.SplitTarget(id); changed[KindTarget+" "+id] || changed[KindProvider+" "+provider] {
			delete(next.Verified, id)
		}
	}

	// 4. Validate the whole document; resolve every credential and the URLs
	// this change sets, with a fresh read of the env file; then the chat
	// targets never verified (RSN-9).
	env := s.env()
	findings := slices.Concat(Validate(&next.Doc), CheckResolved(ctx, &next.Doc, setURLs(old, next), env), Unverified(&next.Doc, next.Verified))

	// 5. An error finding refuses a change. A change with an empty diff that
	// keeps the document's version (which no diff entry shows) alters
	// nothing validation judges, and a stored configuration a newer binary
	// finds invalid is still published (design section 13, point 13), so it
	// is published with its findings: secrets reload works on such a store
	// (phase-4 design section 8).
	same := len(diff) == 0 && next.Doc.Version == old.Doc.Version
	if !same && slices.ContainsFunc(findings, func(f domain.Finding) bool { return f.Severity == domain.SeverityError }) {
		de := domain.NewError(domain.CodeInvalidRequest, "the change is refused: the configuration it leads to has errors")
		de.Findings = findings
		return Result{}, s.refuse(ch, de)
	}

	res := Result{Revision: old.Revision, Changed: len(diff) > 0, Diff: diff, Findings: findings}
	// 6. A dry run stops here and writes nothing.
	if ch.DryRun {
		return res, nil
	}

	// 7. The snapshot exists before the transaction, so nothing can fail
	// between the commit and the publication.
	if res.Changed {
		next.Revision++
		next.NeverSince = neverSince(old, next)
	}
	snap := build(next, env, findings)

	// 8. One transaction: the changed rows, the revision and the audit rows.
	err = s.st.Write(ctx, func(tx *sql.Tx) error {
		at := s.now().UnixMilli()
		if !res.Changed {
			return insertAudit(tx, ch, "", "", nil, nil, "unchanged", 0, at)
		}
		return writeChange(tx, ch, old, next, diff, at)
	})
	if err != nil {
		return Result{}, internal(err)
	}

	// 9. Publish, still under the mutex.
	s.state = next
	s.snap.Store(snap)
	res.Revision = next.Revision
	if res.Changed {
		res.Secrets = issued
	}
	return res, nil
}

// Verified records a verify run under Commit's mutex (RSN-9, SEC-7): one
// audit row always, action "targets verify <id>", resource the target, result
// verified, failed (a run that was not all 2xx) or stale (a successful run
// whose target or provider no longer equals t and p), new_value summary, the
// calls without their bodies; the columns only when verified. No revision: a
// stored result changes no declaration, so the snapshot is republished as a
// shallow copy with the new map and the target_unverified findings again.
func (s *Service) Verified(ctx context.Context, id string, t domain.Target, p domain.Provider, ok bool, at time.Time, version, summary string) (stored bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	provider, _, _ := domain.SplitTarget(id)
	same := func(a, b any) bool {
		x, errX := Encode(a)
		y, errY := Encode(b)
		return errX == nil && errY == nil && bytes.Equal(x, y)
	}
	cur, okT := s.state.Doc.Targets[id]
	curP, okP := s.state.Doc.Providers[provider]
	result := "failed"
	switch {
	case ok && okT && okP && same(cur, t) && same(curP, p):
		result, stored = "verified", true
	case ok:
		result = "stale"
	}
	err = s.st.Write(ctx, func(tx *sql.Tx) error {
		if stored {
			if _, err := tx.Exec("UPDATE targets SET verified_at = ?, verified_version = ? WHERE id = ?", at.UnixMilli(), version, id); err != nil {
				return err
			}
		}
		return insertAudit(tx, Change{Summary: "targets verify " + id, Kind: KindTarget, Name: id}, "", "", nil, []byte(summary), result, 0, s.now().UnixMilli())
	})
	if err != nil || !stored {
		return false, err
	}
	state, snap := *s.state, *s.Snapshot()
	state.Verified = maps.Clone(s.state.Verified)
	if state.Verified == nil {
		state.Verified = map[string]Verification{}
	}
	state.Verified[id] = Verification{At: at.UnixMilli(), Version: version}
	snap.Verified = state.Verified
	snap.Findings = append(slices.DeleteFunc(slices.Clone(snap.Findings), func(f domain.Finding) bool { return f.Code == codeUnverified }),
		Unverified(snap.Doc, snap.Verified)...)
	s.state = &state
	s.snap.Store(&snap)
	return true, nil
}

// Verification is the stored result of target id's last successful verify.
func (s *Service) Verification(id string) (Verification, bool) {
	v, ok := s.Snapshot().Verified[id]
	return v, ok
}

// codeUnverified is the code of the RSN-9 warning.
const codeUnverified = "target_unverified"

// Unverified is the RSN-9 warning target_unverified for each chat target of
// d without a stored result in v, by id. Open, Commit and Check append it;
// validate -f, which has no store, does not (phase-4 design section 15 point
// 33).
func Unverified(d *domain.Document, v map[string]Verification) []domain.Finding {
	var r report
	for _, id := range slices.Sorted(maps.Keys(d.Targets)) {
		if _, ok := v[id]; !ok && d.Targets[id].Kind == domain.KindChat {
			r.warn("RSN-9", codeUnverified, "targets/"+id, "", "this chat target was never checked against its live provider: run targets verify on it")
		}
	}
	return r
}

// neverSince is the NeverSince of next, committed after old at its new
// revision (LIM-2): a scope whose budget stays never keeps its revision, so
// a limit_usd change keeps the counter; one whose budget becomes never, new
// or from another interval, starts at next's revision; one whose never
// budget is removed or changes interval has none.
func neverSince(old, next *State) map[string]int64 {
	was := map[string]bool{}
	for _, scope := range neverScopes(&old.Doc) {
		was[scope] = true
	}
	m := map[string]int64{}
	for _, scope := range neverScopes(&next.Doc) {
		since, ok := old.NeverSince[scope]
		if !ok || !was[scope] {
			since = next.Revision
		}
		m[scope] = since
	}
	return m
}

// neverScopes are the scopes of d whose budget is never, sorted.
func neverScopes(d *domain.Document) []string {
	never := func(b *domain.Budget) bool { return b != nil && b.ResetInterval == domain.ResetNever }
	var scopes []string
	if never(d.Settings.Budget) {
		scopes = append(scopes, "global")
	}
	for name, k := range d.Keys {
		if never(k.Budget) {
			scopes = append(scopes, "key:"+name)
		}
	}
	for name, m := range d.Models {
		if never(m.Budget) {
			scopes = append(scopes, "model:"+name)
		}
	}
	slices.Sort(scopes)
	return scopes
}

// scopeRow is the scope_type and scope_name of a budget_totals row for
// scope: "global" and "", "key" and the key's name, "model" and the alias.
func scopeRow(scope string) (kind, name string) {
	kind, name, _ = strings.Cut(scope, ":")
	return kind, name
}

// refuse writes the audit row of a refused change, unless it is a dry run,
// and returns the refusal. The row does not wait on the request's context: a
// client that went away does not erase its refused attempt (SEC-7).
func (s *Service) refuse(ch Change, de *domain.Error) error {
	if ch.DryRun {
		return de
	}
	err := s.st.Write(context.Background(), func(tx *sql.Tx) error {
		return insertAudit(tx, ch, "", "", nil, nil, "refused:"+de.Code, 0, s.now().UnixMilli())
	})
	if err != nil {
		return internal(err)
	}
	return de
}

func internal(err error) *domain.Error {
	return domain.NewError(domain.CodeInternalError, "the change was not saved: "+err.Error())
}

// clone deep-copies a State through the canonical codec.
func clone(st *State) (*State, error) {
	data, err := Encode(st)
	if err != nil {
		return nil, err
	}
	var out State
	if err := Decode(data, &out); err != nil {
		return nil, err
	}
	// An empty map is omitted by the encoding; Mutate gets every map ready to write.
	if d := &out.Doc; d.Providers == nil {
		d.Providers = map[string]domain.Provider{}
	}
	if d := &out.Doc; d.Targets == nil {
		d.Targets = map[string]domain.Target{}
	}
	if d := &out.Doc; d.Models == nil {
		d.Models = map[string]domain.Model{}
	}
	if d := &out.Doc; d.Keys == nil {
		d.Keys = map[string]domain.Key{}
	}
	if out.Revoked == nil {
		out.Revoked = map[string]int64{}
	}
	return &out, nil
}

// setURLs lists the outbound URLs of next that old does not have with the same
// value and flag: the URLs this change sets (design section 13, point 14).
func setURLs(old, next *State) []OutboundURL {
	had := map[OutboundURL]bool{}
	for _, u := range OutboundURLs(&old.Doc) {
		had[u] = true
	}
	var urls []OutboundURL
	for _, u := range OutboundURLs(&next.Doc) {
		if !had[u] {
			urls = append(urls, u)
		}
	}
	return urls
}

// The kinds of a diff entry, in the order a diff lists them.
const (
	KindProvider = "provider"
	KindTarget   = "target"
	KindModel    = "model"
	KindKey      = "key"
	KindSetting  = "setting"
)

// The actions of a diff entry.
const (
	ActionCreated = "created"
	ActionUpdated = "updated"
	ActionRemoved = "removed"
	ActionRotated = "rotated"
	ActionRevoked = "revoked"
)

// diffStates lists what changes from old to next: by kind in the order of the
// constants above, then by name. Two declarations are equal when their
// canonical encodings are. It refuses what no command may do to keys: delete
// one, revive a revoked one or revoke one that does not exist.
func diffStates(old, next *State) ([]DiffEntry, error) {
	var diff []DiffEntry
	for _, part := range []struct {
		kind string
		diff func() ([]DiffEntry, error)
	}{
		{KindProvider, func() ([]DiffEntry, error) { return diffMap(old.Doc.Providers, next.Doc.Providers) }},
		{KindTarget, func() ([]DiffEntry, error) { return diffMap(old.Doc.Targets, next.Doc.Targets) }},
		{KindModel, func() ([]DiffEntry, error) { return diffMap(old.Doc.Models, next.Doc.Models) }},
		{KindKey, func() ([]DiffEntry, error) { return diffKeys(old, next) }},
		{KindSetting, func() ([]DiffEntry, error) {
			prev, err := members(old.Doc.Settings)
			if err != nil {
				return nil, err
			}
			cur, err := members(next.Doc.Settings)
			if err != nil {
				return nil, err
			}
			return diffMap(prev, cur)
		}},
	} {
		entries, err := part.diff()
		if err != nil {
			return nil, err
		}
		for i := range entries {
			entries[i].Kind = part.kind
		}
		diff = append(diff, entries...)
	}
	return diff, nil
}

// members splits settings into its top-level members, each one resource.
func members(s domain.Settings) (map[string]jsontext.Value, error) {
	data, err := Encode(s)
	if err != nil {
		return nil, err
	}
	var m map[string]jsontext.Value
	return m, jsonv2.Unmarshal(data, &m)
}

func diffMap[V any](old, next map[string]V) ([]DiffEntry, error) {
	var diff []DiffEntry
	for _, name := range slices.Sorted(maps.Keys(union(old, next))) {
		prev, err := encodeIn(old, name)
		if err != nil {
			return nil, err
		}
		cur, err := encodeIn(next, name)
		if err != nil {
			return nil, err
		}
		switch {
		case prev == nil:
			diff = append(diff, DiffEntry{Name: name, Action: ActionCreated, New: cur})
		case cur == nil:
			diff = append(diff, DiffEntry{Name: name, Action: ActionRemoved, Previous: prev})
		case !bytes.Equal(prev, cur):
			diff = append(diff, DiffEntry{Name: name, Action: ActionUpdated, Previous: prev, New: cur})
		}
	}
	return diff, nil
}

func union[V any](a, b map[string]V) map[string]bool {
	u := make(map[string]bool, max(len(a), len(b)))
	for k := range a {
		u[k] = true
	}
	for k := range b {
		u[k] = true
	}
	return u
}

// encodeIn encodes m[name], or returns nil when m has no such entry.
func encodeIn[V any](m map[string]V, name string) (json.RawMessage, error) {
	v, ok := m[name]
	if !ok {
		return nil, nil
	}
	return Encode(v)
}

// SecretView is what a diff, an audit row and a read show of a key's secret:
// its public id and its end, never its hash.
type SecretView struct {
	PublicID   string `json:"public_id"`
	ValidUntil int64  `json:"valid_until,omitzero"`
}

func secretsByKey(secrets []KeySecret) map[string][]SecretView {
	m := map[string][]SecretView{}
	for _, s := range secrets {
		m[s.KeyName] = append(m[s.KeyName], SecretView{s.PublicID, s.ValidUntil})
	}
	for _, list := range m {
		slices.SortFunc(list, func(a, b SecretView) int { return cmp.Compare(a.PublicID, b.PublicID) })
	}
	return m
}

func diffKeys(old, next *State) ([]DiffEntry, error) {
	entries, err := diffMap(old.Doc.Keys, next.Doc.Keys)
	if err != nil {
		return nil, err
	}
	var diff []DiffEntry
	oldSecrets, newSecrets := secretsByKey(old.Secrets), secretsByKey(next.Secrets)
	for _, e := range entries {
		_, revoked := next.Revoked[e.Name]
		switch {
		case e.Action == ActionRemoved && revoked:
			e.Action = ActionRevoked
		case e.Action == ActionRemoved:
			return nil, fmt.Errorf("key %s is removed without being revoked: a key is never deleted", e.Name)
		case revoked:
			return nil, fmt.Errorf("key %s is both declared and revoked", e.Name)
		}
		diff = append(diff, e)
	}
	for name, was := range old.Revoked {
		if at, ok := next.Revoked[name]; !ok || at != was {
			return nil, fmt.Errorf("key %s is revoked and cannot change", name)
		}
	}
	// A public id keeps its key and its hash; only valid_until may change.
	byID := map[string]KeySecret{}
	for _, sec := range old.Secrets {
		byID[sec.PublicID] = sec
	}
	for _, sec := range next.Secrets {
		if prev, ok := byID[sec.PublicID]; ok && (prev.KeyName != sec.KeyName || prev.Hash != sec.Hash || prev.CreatedAt != sec.CreatedAt) {
			return nil, fmt.Errorf("secret %s changed other than in valid_until", sec.PublicID)
		}
	}
	for name := range newSecrets {
		if _, ok := next.Doc.Keys[name]; !ok {
			return nil, fmt.Errorf("a secret belongs to key %s, which is not declared", name)
		}
	}
	for name := range next.Revoked {
		if _, wasRevoked := old.Revoked[name]; !wasRevoked {
			if _, existed := old.Doc.Keys[name]; !existed {
				return nil, fmt.Errorf("key %s is revoked without having existed", name)
			}
		}
	}

	// A key that exists before and after with other secrets is rotated.
	for _, name := range slices.Sorted(maps.Keys(next.Doc.Keys)) {
		if _, existed := old.Doc.Keys[name]; !existed {
			continue
		}
		prev, cur := oldSecrets[name], newSecrets[name]
		if slices.Equal(prev, cur) {
			continue
		}
		p, err := Encode(prev)
		if err != nil {
			return nil, err
		}
		c, err := Encode(cur)
		if err != nil {
			return nil, err
		}
		i, _ := slices.BinarySearchFunc(diff, name, func(e DiffEntry, n string) int {
			// After the updated entry of the same name.
			return cmp.Or(cmp.Compare(e.Name, n), -1)
		})
		diff = slices.Insert(diff, i, DiffEntry{Name: name, Action: ActionRotated, Previous: p, New: c})
	}
	return diff, nil
}
