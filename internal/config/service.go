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
	Findings []domain.Finding  `json:"findings"`          // warnings
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
func Open(ctx context.Context, st *store.Store, env func() Env, now func() time.Time) (*Service, error) {
	state, err := load(ctx, st.Read())
	if err != nil {
		return nil, fmt.Errorf("load the configuration: %w", err)
	}
	e := env()
	findings := append(Validate(&state.Doc), CheckResolved(ctx, &state.Doc, nil, e)...)
	s := &Service{st: st, env: env, now: now, state: state}
	s.snap.Store(build(state, e, findings))
	return s, nil
}

// Snapshot returns the published configuration. A request loads it once and
// keeps it to the end, across any change (RES-7).
func (s *Service) Snapshot() *Snapshot { return s.snap.Load() }

// Commit runs the change pipeline of design section 4. The error is always a
// *domain.Error: 400 with findings, the refusal Mutate returned, or 500 when
// the store fails, and then nothing is published.
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

	// 4. Validate the whole document; resolve every credential and the URLs
	// this change sets, with a fresh read of the env file.
	env := s.env()
	findings := append(Validate(&next.Doc), CheckResolved(ctx, &next.Doc, setURLs(old, next), env)...)

	// 5. An error finding refuses the change.
	if slices.ContainsFunc(findings, func(f domain.Finding) bool { return f.Severity == domain.SeverityError }) {
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

// secretView is what a diff and an audit row show of a key's secrets.
type secretView struct {
	PublicID   string `json:"public_id"`
	ValidUntil int64  `json:"valid_until,omitzero"`
}

func secretsByKey(secrets []KeySecret) map[string][]secretView {
	m := map[string][]secretView{}
	for _, s := range secrets {
		m[s.KeyName] = append(m[s.KeyName], secretView{s.PublicID, s.ValidUntil})
	}
	for _, list := range m {
		slices.SortFunc(list, func(a, b secretView) int { return cmp.Compare(a.PublicID, b.PublicID) })
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
