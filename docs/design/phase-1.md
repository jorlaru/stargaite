# Phase 1 design

Status: the build record for phase 1 of §20 of `kick-off.md`. Step C1 is built and its checks pass; nothing after it has been built or run by this project. "Verified" means the dependency survey of 2026-11-11 ran it on linux/amd64; "to confirm" means the named step must prove it with a test. Rule ids (PAR, RSN, OPS, SEC, ADR, AC, OQ) are the kick-off's and are binding. Every gap or divergence is listed in §13.

## 1. Scope and outcome

Phase 1 delivers `project-scaffold`, `state-store`, `admin-cli`, `keys-auth` and `providers-targets`. Of phase 2 it fixes only four seams: the snapshot type, the inputs of the outgoing-request builder, the body scanner contract and the mock provider (§9). At the end of phase 1 the repository holds:

- One static binary `stargaite` (linux and darwin, amd64 and arm64, `CGO_ENABLED=0`) with `serve`, `version`, `validate [-f]`, `export`, `apply -f`, `settings show/set`, `providers add/set/show/list/remove/presets`, `targets add/set/show/list/remove`, `models add/set/show/list/remove` and `keys create/set/show/list/rotate/revoke`.
- A data directory (`lock`, `stargaite.db`, `admin.key`) holding all configuration (OPS-1, ADR-004), changed only through the admin API on its own listener (SEC-3).
- A data-plane listener with `/health/live`, `/health/ready` and, pending §13 point 7, `GET /v1/models`.
- The outbound URL guard for set time and dial time (SEC-4), credential references with the env file (SEC-5, OPS-6), and the catalog of eighteen presets (ADR-019).
- `config.example.yaml` (annex B), the quick-start guide, `tests/mockprovider`, `make ci`, the Dockerfile and the hosted workflow (`.github/workflows/ci.yml`).
- No code for inference, the scanner, the builder, usage, limits, telemetry export, `explain`, `verify`, `backup`, `metrics` or `secrets reload`. Commands not shipped are not registered and exit `2` as unknown.

| Criterion | Checkable at the end of phase 1 | Left for later |
|---|---|---|
| AC-03 | `make ci` passes with an empty environment and no secret; the image builds | The run on a fork needs the repository published with its hosted workflow |
| AC-06 | A key lists only its allowed aliases; a key with none lists nothing; the admin key gets `401` on the data plane | `403` on an inference route (phase 2) |
| AC-07 | Grace period, never more than two secrets, revoked key refused within 1 s | — |
| AC-39 | `export` then `apply` on an empty instance; invalid file and refused change leave the configuration untouched | — |
| AC-40 | Audit row with previous and new value, and revision increment, per change | Revision in spans and usage records (phase 3) |
| AC-41 | Every shipped command, with standard input closed | Commands of later phases |
| AC-42 | One binary, one directory; a second `serve` exits `1` | — |
| AC-43 | Providers in full; telemetry endpoint at set time | Telemetry dial time (the exporter, phase 3, reuses the same client) |
| AC-44 | Credential added to the env file resolves on the next accepted admin write; unresolved credential disables the provider only | `secrets reload` (phase 4) |
| AC-45 | Non-loopback listener without TLS or `--behind-tls-proxy` exits `1` | — |
| AC-53 | Catalog lists eighteen presets, none with `allow_private_network`; golden; `providers add` stores preset plus flags | Golden frozen only after §13 point 24; live runs before v1 |

PUB-3 is checkable up to `keys create` of the quick start, with the mock provider in place of the preset; the first answer (AC-04) is phase 2.

## 2. Decisions

Versions are stated only where the survey verified them.

| Decision | Choice | Reason | Discarded |
|---|---|---|---|
| Toolchain | Go 1.27.4; `go 1.27.0` in `go.mod`; builder image `golang:1.27.4-trixie`, pinned by digest in the Makefile and the Dockerfile | The standard JSON tokenizer needs 1.27; the SQLite driver needs 1.26 | Go 1.26 with a JSON dependency |
| SQLite | `modernc.org/sqlite` v1.62.0 (SQLite 3.54.1), with the `modernc.org/libc` v1.80.1 it pins | ADR-003; builds without CGO (verified) | A CGO driver |
| libc pin | libc is never bumped on its own; no dependency-update bot | The driver's documentation requires its exact libc | Automated updates |
| YAML | `go.yaml.in/yaml/v3` v3.0.6, used in one file through `yaml.Node` | Maintained, frozen API, rejects alias bombs (verified) | `gopkg.in/yaml.v3` (archived); v4 (one week old); `goccy/go-yaml` (alias bomb accepted) |
| Declaration codec | `encoding/json/v2`: duplicate names rejected, unknown members rejected by option (to confirm, C2; the survey ran only `jsontext.Decoder`) | One strict decoder for store rows, admin bodies and YAML; no dependency | `encoding/json` v1 plus a hand check (kept as fallback, step C2) |
| Body scanner, phase 2 | `encoding/json/jsontext` behind the `params` API | ADR-002 names it. The survey verified escaped-equivalent rejection only in the decoder's default mode; with duplicate names allowed, the scanner compares unescaped names itself (to confirm in phase 2 with `"model"` against `"mod\u0065l"`) | gjson and sjson (no duplicate detection); a hand-written scanner (fallback only) |
| CLI | Standard `flag`, one `FlagSet` per command, a command table | About 35 commands, no completion or man pages required | cobra, kong |
| HTTP routing | `net/http.ServeMux` method patterns, one mux per listener, a JSON wrapper for its plain-text 404 and 405 | Verified on 1.27.4 | A router dependency |
| Logging | `log/slog`, JSON handler | OPS-9 | A logging library |
| Data-directory lock | `syscall.Flock(LOCK_EX or LOCK_NB)` on `<data-dir>/lock`, build constraint for `linux` or `darwin` | Standard library only (verified on Linux) | `gofrs/flock`; `x/sys/windows`; the `unix` tag (does not compile everywhere) |
| DSN | A constant `file:` DSN: the path escaped, every other character a literal of the source, with the shorthand keys of §4. Confirmed in C3: the pinned driver accepts each key and refuses a wrong value, but ignores a misspelt key silently, so the read-back test is the guard. `StrictPragmas` is a process-wide switch that only rejects a multi-statement `_pragma` value; the DSN carries no `_pragma`, so it is not called | `_pragma` text can carry extra SQL; a plain path drops URI parameters (both verified) | `_pragma=` parameters; a plain file name |
| Migrations | Embedded `NNN_name.sql`, position in `PRAGMA user_version` | About 40 lines | A migration library |
| Lint and vulnerability scan | `golangci-lint` v2.15.0 default set, `go vet`, `govulncheck` v1.9.0, all outside `go.mod` | The linter has about 223 requirements | `go tool` |
| Base image | `gcr.io/distroless/static-debian13:nonroot`, pinned by the digest current at implementation time | CA bundle, non-root user, about 2 MiB | `scratch` |
| Supported systems | linux and darwin, amd64 and arm64 | The lock; the container is Linux. `make ci` builds all four, darwin/amd64 with the SQLite driver included (confirmed in C3) | Windows in v1 (§13 point 3) |
| Wire shape of a resource | The flat declaration with its `json` tags plus `name` (or `id`) | One declaration shape for API, YAML and store | A `{name, spec}` wrapper |
| Update semantics | JSON Merge Patch (RFC 7396) on the stored declaration, then the strict decoder | Defined semantics; can remove one `params` entry | Decoding the body over a copy |
| Defaults | Stored value = declared value; one table in `domain` applied when the snapshot is built | Annex B shows declared fields only; export is trivially a fixed point | A write-time normalize step (§13 point 15) |
| Diff | Resource level: previous and new declaration | One structure for audit, `--dry-run` and the response | A field-level diff |
| Refused change | `400 invalid_request` with `findings` | §15 reserves `422` for inference | A new `422` code (§13 point 10) |
| Failed admin authentication | Rate-limited log line, no store write | An unauthenticated caller must not reach the single writer | An audit row |
| Phase 2 seams | Signatures in §9 only | A package nothing calls is a file "just in case" | Stub packages |
| Tests | Standard `testing`, `httptest`, golden files with `-update` | — | Assertion libraries |

## 3. Package map

| Package | Responsibility in phase 1 | May import (internal) |
|---|---|---|
| `cmd/stargaite` | `main.go` dispatches; `serve.go` wires flags, lock, store, config, both listeners and shutdown | everything below |
| `internal/domain` | `Document` and every field of §6 and annex B; `Finding`; `Error` and the §15 codes; the defaults table; the table of top-level keys each dialect writes in its default forms (§9.1) | — |
| `internal/store` | Data-directory lock, connections, migrations, writer goroutine, read pool | — |
| `internal/transport` | Address classifier, URL check, `Resolver`, `Secret`, guarded HTTP clients | — |
| `internal/auth` | Token format: issue, parse, hash, constant-time compare | — |
| `internal/presets` | The provider catalog as a Go literal | `domain` |
| `internal/config` | Strict codec, YAML conversion, `Validate`, `CheckResolved`, `State`, `Service`, diff, configuration SQL, audit rows, `Snapshot`, `Env` and the env file parser | `domain`, `store`, `transport`, `auth` |
| `internal/admin` | Admin listener: key bootstrap, authentication, envelope, handlers | `domain`, `config`, `auth`, `presets` |
| `internal/api` | Data listener: health probes, `GET /v1/models` | `domain`, `config` |
| `internal/cli` | Command table, flag parsing, admin client, output, offline `validate -f` | `domain`, `config` |
| `tests/mockprovider` | Scriptable provider handler, package `mockprovider`; its `main` is the single file of `tests/mockprovider/cmd/mockprovider` | — |
| `tests/e2e` | Tests that drive the built binary | `tests/mockprovider` |

The compiler forbids cycles; no import-rule test ships in phase 1. Phase 2 adds one with its packages: `params` imports nothing internal; `plan` imports `domain`, `params`, `reasoning` and the request translation of `adapters`; none of them may reach `store`, `transport`, `config` or `api`.

Differences from annex C:

- Only the packages above are created. `params`, `reasoning`, `plan`, `adapters`, `routing`, `admission`, `usage` and `telemetry` arrive with their features.
- `store` holds no configuration SQL and imports no internal package, so phase 3 usage brings its own SQL through the same `Write`. Backup and retention join it in phase 4 as annex C says.
- Audit rows are written by `config`, not `admin`: SEC-7 and AC-40 need them in the change's transaction.
- `auth` holds only the token format; the allow-list is a method on the snapshot's key, which avoids a cycle with `config`. Settled in C5: `admin` imports `auth` for the admin key's alphabet (`auth.Secret`), the client-key prefix it refuses and the constant-time comparison (`auth.Equal`).
- `transport` has no SSE code yet. `serve` wiring lives in `cmd/stargaite` so `cli` never imports `store`. `tests/e2e` is new.

## 4. Configuration: store, validation, snapshot

**Schema.** Migration `001` is annex A verbatim (§13 point 9); a golden test dumps `sqlite_master` after migrating. Phase 1 writes `providers`, `targets`, `models`, `model_targets`, `keys`, `key_models`, `key_secrets`, `settings`, `revisions` and `audit`. A `spec` column holds the declaration in the canonical JSON encoding, minus what has columns of its own (an alias's `targets`, a key's `models`); `settings` has one row per top-level member. Each migration runs in one transaction that also sets `PRAGMA user_version`; a store whose version is above the binary's refuses to start (OPS-7).

**Connections (ADR-003).** The DSN is `file:<escaped path>?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_busy_timeout=5000`. The C3 read-back test is what proves each key took effect; a key the driver ignores silently fails that test. The writer handle adds `_txlock=immediate` and `SetMaxOpenConns(1)`, and opens first, since it is what puts the file in WAL mode. The read pool adds `_query_only=1` and does not use `mode=ro`; confirmed in C3 that `_query_only=1` alone refuses writes. It guards against mistakes, not against a statement that sets the pragma back to `0`. The database file is `<data-dir>/stargaite.db`. Tests: the four pragmas read back on the writer and on several reader connections held at once, in a directory whose name holds a space, `?`, `&`, `#` and `%`; `PRAGMA query_only` reads `1` on each and an `INSERT` through `Read()` fails; a second connection cannot `BEGIN IMMEDIATE` while the writer's transaction is open, which shows `_txlock` took effect.

```go
package store

func Open(ctx context.Context, dataDir string) (*Store, error)             // mkdir 0700, lock, writer, migrations, read pool, in that order
func (s *Store) Write(ctx context.Context, fn func(*sql.Tx) error) error // the only write path; ctx bounds only the wait for a queue slot
func (s *Store) Read() *sql.DB
func (s *Store) Close() error                                            // drains the queue, closes both handles, releases the lock
```

`Write` sends `fn` on a bounded channel to the one writer goroutine, which runs each `fn` in its own transaction; `ctx` bounds only the wait for a slot in the queue. Once `fn` is queued, `Write` returns nothing but the commit result, even if `ctx` ends, so a caller never sees an unknown outcome. Phase 3's usage batcher is another producer.

**State and change.**

```go
package config

type State struct { // all durable configuration; a committed State is never mutated
	Doc      domain.Document  // what export emits; revoked keys are absent
	Secrets  []KeySecret      // PublicID, KeyName, Hash [32]byte, CreatedAt, ValidUntil (ms, 0 = none)
	Revoked  map[string]int64 // key name -> revoked_at
	Revision int64
}

type Change struct {
	Summary    string // "providers add openrouter"; becomes revisions.summary and audit.action
	Kind, Name string // the resource the request names ("provider", "openrouter"; "configuration" and "" for apply): audit.resource_type and resource_name on a row that has no diff entry
	DryRun     bool
	Mutate     func(*State) (issued map[string]string, err error) // gets a deep copy; issued = key name -> new token
}

type DiffEntry struct {
	Kind     string          `json:"kind"`   // provider, target, model, key, setting
	Name     string          `json:"name"`
	Action   string          `json:"action"` // created, updated, removed, rotated, revoked
	Previous json.RawMessage `json:"previous,omitempty"` // declarations; for rotated, public ids and valid_until only
	New      json.RawMessage `json:"new,omitempty"`
}

type Result struct {
	Revision int64
	Changed  bool
	Diff     []DiffEntry
	Findings []domain.Finding  // warnings
	Secrets  map[string]string // shown once (OPS-3, SEC-8); empty on a dry run
}

func Open(ctx context.Context, st *store.Store, env func() Env, now func() time.Time) (*Service, error) // load, validate, publish
func (s *Service) Commit(ctx context.Context, ch Change) (Result, error)           // error is *domain.Error
func (s *Service) Snapshot() *Snapshot                                             // atomic.Pointer load; once per request
```

**The change pipeline.** `Commit` is the only way configuration changes, for single commands and `apply` alike. Under one mutex:

1. Deep-copy the committed `State` (encode and decode with the canonical codec).
2. Run `Mutate`. A `404 not_found` or `409 conflict` ends here; unless `DryRun`, it first writes one audit row for `Kind` and `Name` with `result=refused:<code>` (`refused:not_found` or `refused:conflict`), no revision. Settled in C4: any `*domain.Error` below `500` that `Mutate` returns is such a refusal, audited with its own code; any other error is a `500` with no audit row, because nothing was decided.
3. Compute the resource-level diff against the committed `State`. Added in C4: a key that leaves `Doc` without entering `Revoked`, a revoked name that is declared again, leaves `Revoked` or changes its `revoked_at`, a `Mutate` that sets `State.Revision`, a revocation of a key that did not exist, a secret of an undeclared key, and a public id whose key, hash or creation time changes are refused here with `500`: no command may do them, so only a faulty `Mutate` can.
4. Call `env()` once, which re-reads the env file; run `Validate` on the whole resulting document, then `CheckResolved` with that `Env` on every credential reference and on the outbound URLs this diff sets.
5. On any finding of severity `error`: return `400` with the findings; unless `DryRun`, first write one audit row for `Kind` and `Name` with `result=refused:invalid_request`, no revision. The pointer is untouched (OPS-5, AC-39).
6. On `DryRun` without errors: return diff and findings. A dry run never writes or publishes anything, an audit row included.
7. Build the snapshot from the copy with the same `Env`. The revision is `Revision+1` when the diff is not empty.
8. Non-empty diff: one `store.Write` with the changed rows, the `revisions` row and one `audit` row per diff entry (`previous_value`, `new_value`, `result=applied`, `revision`). Empty diff: one audit row for `Kind` and `Name` with `result=unchanged`, no revision (§13 point 12).
9. Make the copy the committed `State` and store the snapshot pointer, still under the mutex. Commit order equals publication order (OPS-4).

An audit row with no diff entry (the refused rows of steps 2 and 5 and the unchanged row of step 8) takes `resource_type` and `resource_name` from `Change.Kind` and `Change.Name` and leaves `previous_value` and `new_value` null. `result` is `applied`, `unchanged`, or `refused:<code>` with the §15 code, for example `refused:not_found`.

Failure analysis: the snapshot exists before the transaction, and `Write` returns nothing but the commit result once `fn` is queued, so nothing can fail between commit and publication. A failed transaction returns `500` and publishes nothing. A request cancelled after its write was queued is still committed and published; the client only loses the response. A crash after the commit is repaired by the start-up load, which rebuilds the snapshot from the store under the stored revision. Requests in flight keep the pointer they loaded (RES-7).

**Snapshot (OPS-4).** Built only by one unexported function; read-only afterwards.

```go
type Snapshot struct {
	Revision  int64
	Doc       *domain.Document // the committed declarations, for export and reads
	Settings  domain.Settings  // defaults applied
	Providers map[string]*Provider
	Targets   map[string]*Target
	Models    map[string]*Model
	Keys      map[string]*Key  // by name; revoked keys are absent
	Findings  []domain.Finding // findings of the published configuration: warnings after a change; at start, errors too (§13 point 13)
	secrets   map[string]secret // by public id: hash, valid_until, *Key
}
type Provider struct { Name string; Spec domain.Provider; Credential transport.Secret; Disabled bool }
type Target   struct { ID, Upstream string; Spec domain.Target; Provider *Provider }
type Model    struct { Name string; Spec domain.Model; Targets []*Target } // in order: primary, then fallbacks
type Key      struct { Name string; Spec domain.Key }

func (k *Key) Allows(alias string) bool
func (s *Snapshot) Authenticate(token string, now time.Time) (*Key, bool)
```

The admin key is never in the snapshot. Secret hashes and revocations live in `State`, outside `Doc`, so nothing that serializes a declaration can reach them (OPS-3, SEC-1).

**Validation (OPS-5).**

```go
func Validate(doc *domain.Document) []domain.Finding // pure: no clock, I/O, DNS or environment
type OutboundURL struct { Resource, Field, URL string; AllowPrivate bool }
func CheckResolved(ctx context.Context, doc *domain.Document, urls []OutboundURL, env Env) []domain.Finding
// every credential reference through env.Lookup (warning); every host in urls through env.Resolve
```

- Errors landed in phase 1 (§13 point 5): unknown providers and targets; aliases with no target or more than three; resource-name syntax; RSN-9 (no key name starts with `_`); an alias listed twice in a key's `models` (added in C4: `key_models` holds one row per alias, so the repeat could not be stored and reloaded); PAR-5; PAR-6 and PAR-7 on alias caps; the PAR-2 collision check: a `forward` rule is refused when its name is, for any target of the alias, a top-level key that the dialect table writes for that target's dialect, a top-level key of that target's own `on` or `off` fragment (RSN-3), that target's `output_cap_param` (its default included), or `stream_options`; adapter fields remain §13 point 6; RSN-3; RSN-7, naming the alias and the target; USE-12; SEC-4 on `base_url`, `rerank_url` and `settings.telemetry.otlp_endpoint`; SEC-5 on every credential reference; the reserved header names of TEL-2.
- Warnings landed in phase 1: undeclared `levels` or `can_disable` (RSN-3), an effort toward a `none` dialect (RSN-5), no prices to fall back on (USE-12), a key naming an unknown alias. Warnings tied to later features ("never verified", "cannot serve an endpoint") arrive with them; adding a warning never invalidates a stored configuration.
- `CheckResolved` holds the two checks that need the environment, so a real change, a dry run, `validate` and `validate -f` report the same findings (OPS-5). It looks every credential reference of the document up through `env.Lookup`: one that does not resolve, or resolves to an empty value, is the `credential_unresolved` warning. It resolves each host in `urls` through `env.Resolve` and classifies every address (§8): an address that is `Forbidden`, or `Private` while that URL's `AllowPrivate` is false, is an error; a failed lookup, or an answer with no address, is a warning. `Commit` passes the URLs the diff sets; `validate` and `validate -f` pass every URL of the document, so a file gets the verdict `apply` would give on an empty instance (§13 point 14).
- At start, findings on the stored configuration are logged and shown by `validate`; the configuration is still published (§13 point 13).

**Codec and YAML.** `domain.Document{Version, Settings, Providers, Targets, Models, Keys}` has `json` tags only. Tri-state fields (`capabilities.*`, `reasoning.can_disable`) are `*bool`, and `levels` is omitted when empty; no default is ever applied to them (§6, RSN-3). YAML is read into a `yaml.Node` and converted to JSON for the strict decoder: `!!int`, `!!float`, `!!bool` and `!!null` scalars keep their type when their text is valid JSON, and every other scalar, timestamps included, becomes a string, so `as_of: 2026-11-01` stays text. The converter explicitly refuses aliases, anchors, merge keys, repeated mapping keys, non-string keys and a second document; decoding into a `Node` may bypass the library's own guards (to confirm, C2). Export walks the canonical JSON into block-style nodes with strings tagged `!!str`.

**Export and apply (OPS-3).**

- `export` emits `Doc`: no secrets, no revoked keys, no derived fields; maps sorted by name. Comments and flow style of a hand-written file do not survive, and date-like strings are emitted quoted. The golden file is "export after applying `config.example.yaml`".
- `apply` replaces each named resource by the file's declaration as a whole; each top-level member of `settings` counts as one resource, and one declared empty (`timeouts: {}`) is named, so its fields return to their defaults (settled in C5: every member of `domain.Settings` is a pointer). Presets and `target_defaults` are not consulted: the file is the declaration.
- A key not present is created with a new secret, returned once in `Result.Secrets`; an existing key keeps its secrets; a key named in `Revoked` is `409 conflict`.
- `--prune` removes absent providers, targets and aliases, revokes absent keys and clears absent settings. `--dry-run` goes no further than step 6 and writes nothing. `version` must be `1`: settled in C5, the file's `version` goes into the checked document, so `Validate` refuses any other value as a finding (`unsupported_version`) and the refusal is audited like any other.
- Settled in C5: `apply` is `Service.Apply(ctx, file, prune, dryRun)`, which builds the `Change` (summary `apply` or `apply --prune`, kind `configuration`) and calls `Commit`; `GET /validate` is `Service.Check(ctx)`. A file that does not decode (YAML, unknown member) is `400` before `Commit` and is not audited.
- Property tests: `apply(export(S))` on S gives an empty diff; on an empty instance it gives a document equal to S (AC-39).

**Start-up order of `serve`.** (1) Flags, then `STARGAITE_<FLAG>` variables. (2) The listener rule for both listeners (§5). (3) `store.Open`: directory `0700`, lock, SQLite; a held lock exits `1` (OPS-10, AC-42). The lock file stays open for the life of the process and is never deleted. (4) `config.Open`: load, validate, build, publish; it runs `CheckResolved` with no URLs, so start-up resolves credentials but no host name, and `serve` logs every finding of the published configuration (§13 point 13), then names its revision in the `ready` line. A row the strict decoder refuses stops the start with exit `1`. (5) Admin key. (6) Bind the admin listener, then the data listener; `/health/ready` answers `200` from here, and one `ready` log line names both bound addresses. On `SIGTERM`: `Shutdown` on both servers with a 30 s bound, then `store.Close`.

**Admin key bootstrap (SEC-3).** `STARGAITE_ADMIN_KEY` wins. Otherwise `<data-dir>/admin.key` is read; when missing, 32 random bytes in lowercase base32 are written to a temporary file opened `O_EXCL` with mode `0600`, then `fsync`, `rename`, and `fsync` of the directory, all before the admin listener binds and under the lock. Only the SHA-256 of the key is kept in memory. `serve` exits `1` if the key has fewer than 32 characters, has leading or trailing whitespace (HTTP trims a header value, so no client could present it; settled in C5) or starts with `sk-stargaite-` (§13 point 17).

**Credential resolution (SEC-5, OPS-6).**

```go
type Env struct {
	Lookup   func(name string) (string, bool) // process environment first, then the env file
	Resolve  transport.Resolver               // injected in tests
	Findings []domain.Finding                 // warnings from this read of the env file
}
```

The env file is read only when `--env-file` is given, and again each time `env()` is called: once per `Commit`, at step 4, so every publication uses a fresh read. Its format: `NAME=value` lines, no quoting, no interpolation, `#` comments; a malformed line is a warning with its line number and no content, and a file that cannot be read is one warning; both travel in `Env.Findings`, which `CheckResolved` returns ahead of its own, so a change, a dry run, `validate` and `validate -f` all show them. Neither blocks a change or the start. The resolver that `serve` passes to `config.Open`, inside `Env`, is the one both clients of §8 receive.

A resolved value is wrapped at once with `transport.NewSecret`: `config.Provider.Credential` is a `transport.Secret` (§8). Only `transport` reads the value, when it adds the credential header (phase 2); no exported method returns it. A provider whose reference does not resolve, or resolves to an empty value, is `Disabled` in the snapshot, carries the `credential_unresolved` warning of `CheckResolved`, and never blocks a change or the start. A provider without `api_key_env` has credential status `none`. No field of any declaration accepts a credential value.

## 5. Admin API

Listens on `127.0.0.1:4001` under `/admin/v1`. Every CLI command that talks to a server makes exactly one of these calls (§13.2).

| Method and path | Body | CLI |
|---|---|---|
| `GET /providers`, `/targets`, `/models`, `/keys` | — | `list` |
| `POST /providers` | declaration, `name`, optional `preset` | `providers add` |
| `POST /targets`, `POST /models` | declaration and `id` or `name` | `targets add`, `models add` |
| `GET`, `PATCH`, `DELETE /providers/{name}`, `/models/{name}`, `/targets/{id...}` | merge patch on `PATCH` | `show`, `set`, `remove` |
| `POST /keys` | declaration and `name`; `models` required | `keys create` |
| `GET`, `PATCH /keys/{name}` | merge patch | `keys show`, `keys set` |
| `POST /keys/{name}/rotate` | `{"grace":"1h"}`, optional | `keys rotate` |
| `POST /keys/{name}/revoke` | — | `keys revoke` |
| `GET`, `PATCH /settings` | merge patch | `settings show`, `settings set` |
| `GET /presets` | — | `providers presets` |
| `GET /export` | — (`?format=json` returns the canonical JSON) | `export` |
| `POST /apply?dry_run=&prune=` | the YAML file, `application/yaml` | `apply -f` |
| `GET /validate` | — | `validate` |

Rules of the listener:

- **Authentication.** `Authorization: Bearer <admin key>`; SHA-256 of the presented token compared in constant time. Failure is `401 invalid_api_key` and a rate-limited log line (settled in C5: at most one line per 10 s, with the request id, the remote address and the count of failures since the previous line; never the presented token). The scheme is matched in any letter case. An authenticated request logs one `admin request` line: request id, method, path, status. A client key fails the same way (SEC-2). One wrapper around the admin mux enforces it for every route.
- **Metrics token.** Out of phase 1: no metrics route is registered and `STARGAITE_METRICS_TOKEN` is not read; SEC-5 already forbids that name as a credential reference.
- **Bodies.** JSON through the strict decoder; a repeated or unknown member is `400 invalid_request` naming it. Cap of 1 MiB, 16 MiB on `/apply` (sized for §18); above it `413 request_too_large`.
- **Create and update.** `POST` on an existing or revoked name is `409 conflict`. `PATCH` merges, so `null` removes a member and lists are replaced whole, and the result goes through the pipeline. `preset` and `grace` are request members, never stored.
- **Reads** come from the snapshot and add derived, read-only members: a provider's `credential` (`resolved`, `unresolved` or `none`), a key's `secrets` (public ids and `valid_until`). A credential value is never accepted or returned.
- **Envelope.** Every response carries `x-stargaite-request-id`. The mux's plain-text 404 and 405 are rewritten into the JSON envelope. Settled in C5: §15 has no row for `405`, so it answers status `405` with `invalid_request_error` and `invalid_request`, keeping the mux's `Allow` header. A `500` answers a fixed message naming the request id; its cause goes only to the log under that id (OPS-9). A route that takes query parameters (`/export`, `/apply`) refuses an unknown or repeated one, and a flag is `true` or `false`: absent is `false`, a flag without a value is refused, so neither a misspelt nor a bare `dry_run` ever applies for real.
- **Settings (settled in C5).** `GET /settings` answers the effective values (§13 point 15); `PATCH /settings` merges into the stored declaration inside `Mutate`, so a member the strict decoder refuses is an audited refusal, and answers the `Result` plus `resource`, the effective settings after the change. `GET /validate` answers `200` with `{"findings":[…]}` whatever their severity.
- **Listener rule (SEC-6, AC-45).** Evaluated per listener before binding: a host other than a loopback literal or `localhost` needs the certificate pair or `--behind-tls-proxy`; an empty host (`:4000`) is not loopback. Both servers set `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout` and `MaxHeaderBytes`. No listener ever runs without a write timeout (§13 point 22).

```text
POST /admin/v1/providers
{"name":"openrouter"}

201
{"revision":3,"changed":true,
 "resource":{"name":"openrouter","protocol":"openai","base_url":"https://openrouter.ai/api/v1",
             "api_key_env":"OPENROUTER_API_KEY","billing":"pay_per_use","credential":"unresolved"},
 "diff":[{"kind":"provider","name":"openrouter","action":"created","new":{"protocol":"openai"}}],
 "findings":[{"severity":"warning","rule":"OPS-6","code":"credential_unresolved",
              "resource":"providers/openrouter","field":"api_key_env","message":"OPENROUTER_API_KEY does not resolve"}]}
```

```text
POST /admin/v1/keys
{"name":"my-agent","models":["fast"]}

201
{"revision":6,"changed":true,"resource":{"name":"my-agent","models":["fast"]},
 "secrets":{"my-agent":"sk-stargaite-<12>-<52>"}}
```

Errors use the §15 envelope; a refused change adds the findings inside the error object:

```json
{"error":{"message":"change refused: 1 check failed","type":"invalid_request_error","code":"invalid_request",
  "findings":[{"severity":"error","rule":"RSN-7","code":"effort_required","resource":"models/fast",
    "field":"reasoning.effort","message":"target minimax-plan/MiniMax-M3.1-Flash always reasons and the alias sets no effort"}]}}
```

```go
package domain

type Finding struct { // Rule is a kick-off rule id
	Severity string `json:"severity"`
	Rule     string `json:"rule"`
	Code     string `json:"code"`
	Resource string `json:"resource"`
	Field    string `json:"field,omitempty"`
	Message  string `json:"message"`
}
type Error struct {
	Status   int       `json:"-"`
	Type     string    `json:"type"`
	Code     string    `json:"code"`
	Message  string    `json:"message"`
	Param    string    `json:"param,omitempty"`
	Findings []Finding `json:"findings,omitempty"`
}
```

Codes used by the admin API: `invalid_request` (400), `invalid_api_key` (401), `conflict` (409), `request_too_large` (413), `internal_error` (500), and one new code, `not_found` (404, `not_found_error`), for an unknown resource (§13 point 10).

## 6. CLI

```go
package cli

type flagSpec struct {
	name string // "rpm-limit", "target", "budget"
	kind int    // string, number, boolean, string list (repeated, in order), string pairs, typed pairs, quota window, raw JSON
	path string // member of the request body it fills: "rpm_limit", "targets", "reasoning.effort"
}

type command struct {
	words    []string                                                 // {"providers", "add"}
	args     []string                                                 // positional arguments, all required: {"name"}
	flags    []flagSpec
	needsYes func(in input) bool                                      // nil = never; exit 2 without --yes (OPS-2)
	request  func(in input) (method, path string, body any)           // exactly one admin call
	local    func(ctx context.Context, in input, out io.Writer) error // only version and validate -f
}

var commands = []command{ /* one row per shipped command of §13.3 */ }

func Main(args []string, getenv func(string) string, stdout, stderr io.Writer) int
```

- **Settled in C5.** Kinds shipped so far: string and boolean (read by the row itself, no `path`), number and typed pairs; the others arrive with the commands that need them. A row may have both `local` and `request`: `local` returns a sentinel to hand over to the request, which is how `validate` without `-f` asks the server. A request body of the `yamlFile` type is read by `Main` (regular files only) and sent as `application/yaml`. Rows with a request get `--admin-url` and `--data-dir`; rows with `needsYes` get `--yes`. A `2xx` answer that holds an error finding exits `1`, so `validate` exits as `validate -f` does. A missing admin key and an unreachable API are both the local `admin_unreachable` envelope; an `-f` file that cannot be read is exit `1` with a local `invalid_request` envelope. The client never follows a redirect.
- **Registration.** A command is one row. `Main` matches the leading words, builds one `FlagSet` from the row, parses, and either calls `local` or sends the one request. There is no per-command handler code and no rule: presets, "name equals a preset name", defaults and validation are the server's.
- **Flags after positionals.** Standard `flag` stops at the first positional (verified), and the kick-off writes positionals first. `Main` therefore parses in a loop: parse, take one positional, parse the rest, until the arguments are exhausted; `--` ends flag parsing. Tested with flags before, after and between positionals.
- **Flag convention (§13.3)**, written out as table rows, not derived by reflection:
  - Scalars are named after the field with dashes; `--effort`, `--effort-mode` and `--max-output-tokens` map to the alias's `reasoning.effort`, `reasoning.mode` and `max_output_tokens`.
  - Lists repeat the singular (`--target`, `--model`, `--require`, `--level`); `--all-models` sends `models: ["*"]`.
  - Pairs: `--budget`, `--param`, `--target-defaults`, `--quota`, `--thinking-budget`, `--effort-map`.
  - The row, never the text of a value, fixes the JSON type of a scalar or a list: `--model 4` sends the string `"4"`, and `--rpm-limit x` is exit `2`. String pairs (`--param`, `--effort-map`, `--target-defaults`) send every value as a string. Typed pairs (`--budget`, `--thinking-budget`) send a value that is a JSON number, `true` or `false` as that literal and anything else as a string. `--quota <window>:<unit>=<n>` is its own kind and appends one `{window, <unit>: n}` entry.
  - `--reasoning-on` and `--reasoning-off` take one JSON fragment verbatim.
  - `set` sends only the flags given, as a merge patch; `--flag=` with an empty value sends `null` (§13 point 20).
  - `--content` ships with phase 3; `content_until` already round-trips.
- **Exit codes (OPS-2).** `0` on a `2xx`; `1` on any other response or when the admin API cannot be reached; `2` for anything decided before the call: unknown command or flag, missing positional, malformed pair, missing `--yes`. `--yes` is needed by `remove`, `keys revoke` and `apply --prune` without `--dry-run`.
- **Output.** `--json` prints the response body unchanged, including error envelopes; errors go to standard output with `--json`, to standard error without. Without `--json` one generic renderer prints objects as `field: value` lines and lists as tables; `keys create` and `keys rotate` print the token on its own line, `export` prints the YAML. Standard input is never read (PRI-8).
- **Finding the admin API.** `--admin-url`, else `STARGAITE_ADMIN_URL`, else `http://127.0.0.1:4001`. The key comes from `STARGAITE_ADMIN_KEY`, else `<data-dir>/admin.key`, with `<data-dir>` from `--data-dir`, else `STARGAITE_DATA_DIR`, else `./data`.
- **Without a server.** `serve` (wired in `cmd/stargaite`; each flag falls back to its `STARGAITE_<FLAG>` variable, the flag wins; `--json` is accepted and changes nothing, since its only output is the JSON log). `version` prints the string of OPS-7, set at link time; `--json` gives `{"version":"…"}`. `validate -f <file> [--env-file <file>]` runs the YAML conversion, `Validate` and `CheckResolved` in process, with an `Env` built from the process environment and `--env-file`, so it reports unresolved credentials too, and prints the same findings object as `GET /validate`: exit `1` on any error finding, `0` with warnings.
- **AC-41.** A test in `cli` walks `commands` against an in-process admin handler with standard input closed, asserting for every row valid JSON under `--json`, the exit code, and exit `2` without `--yes` where required.

## 7. Keys and authentication

- **Format (SEC-1, ADR-011).** `sk-stargaite-<public id>-<secret>`, 78 characters (corrected in C5: 13 + 12 + 1 + 52). The public id is 12 characters and the secret 52 (32 random bytes), both in base32 with the lowercase alphabet `a-z2-7` and no padding, from `crypto/rand`. The standard encoding is upper case, so the alphabet is set explicitly and tested.
- **Storage.** `key_secrets` holds the public id and the SHA-256 of the full token. The token leaves the server once, in `Result.Secrets`.

```go
package auth

func Issue(rand io.Reader) (token, publicID string, hash [32]byte, err error)
func Parse(token string) (publicID string, ok bool) // prefix, length and alphabet only
func Equal(token string, hash [32]byte) bool        // SHA-256, then subtle.ConstantTimeCompare
```

- **Authentication.** `Snapshot.Authenticate` checks the shape, finds the public id in the snapshot, compares against a fixed dummy hash when the id is unknown, and refuses a secret whose `valid_until` has passed. Any failure is `401 invalid_api_key`. `Key.Allows` is true for a listed alias or when the list is `["*"]`.
- **The two credentials never cross (SEC-2, AC-06).** The admin key is not in the snapshot and cannot have the issued shape (§4), so the data plane refuses it at the shape check. A client key fails the hash comparison on the admin listener.
- **Create.** `POST /keys` without `models` is `400`: no access by omission (§13.3). An explicit empty list is valid and reaches nothing. A name starting with `_` is refused (RSN-9). A new key has the default `max_parallel_requests` of LIM-7 (§13 point 15).
- **Rotate (SEC-8, AC-07).** A new secret is issued; the previous one gets `valid_until = now + grace` (1 h by default). If a secret is already in its grace, the oldest is deleted in the same change, so a key never has more than two. Expired rows are deleted on the next rotation or revocation; the check at request time is the authority.
- **Revoke.** Deletes every secret, sets `revoked_at`, moves the name to `State.Revoked` and publishes. The next request loads a snapshot without the key, well within 1 s; requests in flight finish. The row stays, so the name is never reused: `POST /keys` and `apply` answer `409 conflict`, and `export` omits it (OPS-3).
- **Data-plane route in phase 1.** `GET /v1/models` authenticates and lists the aliases the key may use, in the OpenAI list shape. It is the one authenticated route of phase 1 and what makes AC-06 and AC-07 checkable over HTTP (§13 point 7).
- **Tests.** Token shape and alphabet; unknown id and wrong secret; grace expiry with an injected clock; a third rotation during a grace; revoke observed by a concurrent client within 1 s; admin key on the data plane and client key on the admin API.

## 8. Providers, presets and outbound URL guard

**Presets (ADR-019, AC-53).** `presets.Catalog` is a Go literal of eighteen entries `{Name, Untested, Provider domain.Provider}`. The last eight of §7.2 carry `Untested: true`; the mark is catalog metadata, shown by `providers presets`, fixed by the golden file and never copied into a provider. The catalog holds only what §7.2 and annex B state; everything else stays undeclared until §13 point 24 is settled. `POST /providers` picks `preset`, or the preset named like the provider, merges the explicit members over it and stores the result. From then on only the stored declaration counts. `dashscope` and `tei` have no base URL, and `ollama` is plain HTTP on loopback, so each is refused without the explicit flag the kick-off names.

**Targets and aliases.** A target id is `provider/upstream-model`, split at the first `/`; the provider must exist. `targets add` and `models add` copy the provider's `target_defaults` into a new target (RSN-2); `models add` creates a missing target as `kind: chat` in the same change and reports it in `diff` (§13.3). Removing a provider or target that is still referenced is refused by `Validate` with a finding naming the dependents; there is no second refusal path.

**One classifier, two moments (SEC-4, AC-43).**

```go
package transport

type Class int // Public, Private, Forbidden
type Resolver func(ctx context.Context, host string) ([]netip.Addr, error)

func Classify(addr netip.Addr) Class                             // after Unmap
func CheckURL(raw string, allowPrivate bool) error               // literal checks, no network
func NewClient(allowPrivate bool, resolve Resolver) *http.Client // phase 2 picks one of two by the provider's flag

type Secret struct{ /* unexported value */ } // Format (fmt.Formatter, so every verb, %#v included), MarshalJSON and LogValue write "[redacted]"
func NewSecret(value string) Secret         // called only by config, when a reference resolves
```

Corrected in C4: `fmt` calls no method on an unexported struct field, and none on any field while it reports a wrong verb (`%s` on a pointer to a struct that holds a `Secret` prints that struct with its methods off), so a value held as a plain string leaks through both paths. The value is therefore held behind a pointer, which `fmt` prints below the top level only as an address; exported fields are no longer what keeps it out.

- `Classify`: always `Forbidden`, checked first: unspecified, multicast, broadcast, link-local (`169.254.0.0/16`, `fe80::/10`), and the cloud metadata addresses that sit outside link-local, kept in one explicit list of prefixes that starts with `fd00:ec2::/32`, `100.100.100.200/32` and `168.63.129.16/32` (the last one is in public address space, so only the list refuses it). Then `Private`: loopback, RFC 1918, carrier-grade NAT and unique-local. Everything else is `Public`. An address in the NAT64 prefix `64:ff9b::/96` is classified by the IPv4 address in its last four bytes, because a translating gateway delivers it there. A table test fixes every range and every listed prefix, IPv4-mapped IPv6 included, and `64:ff9b::` followed by a loopback, an RFC 1918 and a link-local IPv4 address, and checks that each `Forbidden` entry stays refused with `allow_private_network` set.
- `CheckURL`, called by `Validate`: scheme `http` or `https` only; no userinfo; a host is required; HTTPS unless `allowPrivate`; a literal address is classified; `localhost` counts as loopback.
- `CheckResolved` (§4) applies `Classify` to the addresses a name resolves to when the URL is set.
- `NewClient` resolves the host through `resolve` in its `DialContext`, applies `Classify` to every address, refuses the dial when any address is `Forbidden`, or `Private` while `allowPrivate` is false, and otherwise dials an address literal; `net.Dialer.Control` applies the same verdict to the address actually connected to, so a name that later resolves elsewhere is refused. `Proxy` is nil, so proxy variables are ignored (§13 point 23). `CheckRedirect` returns `http.ErrUseLastResponse`. Certificates are always verified. The two clients have separate connection pools.
- The telemetry block that `apply` stores gets the same treatment in phase 1: `otlp_endpoint` under its own `allow_private_network`, `basic_auth_env` and header references as credential references, and static headers named `Authorization`, `Proxy-Authorization`, `Cookie` or `x-api-key` refused (TEL-2).

**Credential references (SEC-5).** A reference is a variable name: letters in either case, digits and `_`, not starting with a digit, and not starting with `STARGAITE_` in any letter case (`strings.EqualFold` on its first ten characters), so `stargaite_admin_key` is refused too (§13 point 18). Resolution is in §4. The resolved value lives in a `Secret` (the block above): the value is readable only inside `transport`, and no exported method returns it.

**Tests.** Preset golden; `providers add` with and without flags stores exactly preset plus flags; an injected resolver that answers a public address to `CheckResolved` at set time and a private one to `NewClient` at dial time; a name that resolves to a private address, refused at set time without `allow_private_network` and accepted with it; a mock provider answering `302`; a metadata address, link-local and listed, with `allow_private_network` set; a credential added to the env file resolving after the next accepted admin write (AC-44); a planted fake credential that must appear in no export, response, log line or audit row; `fmt.Sprintf` of a `Secret`, and of a `config.Provider` that holds one, with `%v`, `%+v`, `%#v`, `%s`, `%q`, `%x` and `%d`, never contains the planted value.

## 9. Interfaces fixed now for phase 2

No file is created for `params` or `plan` in phase 1. These signatures are the contract phase 1 code must already satisfy.

What phase 1 guarantees to them:

- `Service.Snapshot()` is loaded once per request and never changes afterwards.
- `Snapshot.Models[alias].Targets` gives, in order, each target's declaration and its provider's declaration.
- Every refusal rule that reads only declarations already holds for any stored configuration.
- `domain` carries the §15 codes and the table of keys each dialect writes.
- The credential value is readable only inside `transport`; the builder receives declarations only.

**Body scanner (PAR-1, PAR-8, ADR-002).**

```go
package params

type Error struct{ Code, Param, Message string } // always invalid_request: syntax, or a repeated key

type Body struct{ /* the raw bytes and the byte spans of indexed members */ }

// walk: every nested object the proxy reads or writes for the endpoint; "*" stands for each element of an array, e.g.
// {"thinking"}, {"output_config"}, {"stream_options"}, {"response_format"},
// {"tools","*"}, {"tools","*","function"}, {"messages","*"}, {"messages","*","content","*"},
// {"messages","*","tool_calls","*"}, {"messages","*","tool_calls","*","function"}
func Scan(raw []byte, walk [][]string) (*Body, *Error)
func (b *Body) Keys() []string                           // top-level names, in order of appearance
func (b *Body) Get(path ...string) (raw []byte, ok bool) // exact bytes of the value; an element is a member name or a decimal array index

type Op struct{ Path []string; Value []byte } // nil Value deletes; otherwise replace, or append when absent
func (b *Body) Edit(ops ...Op) ([]byte, error)
```

Contract:

- One pass over the bytes, on `encoding/json/jsontext` with duplicate names allowed in the decoder and `PreserveRawStrings(true)`.
- The top-level object and every object named in `walk` are indexed and refuse a repeated key. The scanner compares the names after unescaping them itself, because the decoder's own check is off (§2). Everything else is syntax-checked and skipped, so a repeat inside forwarded content is not an error. This is the literal PAR-1 (§13 point 16).
- The PAR-4 detector and the adapters read the body only through `Body.Get`, and a `Get` outside the indexed set fails, so nothing can be read from an object that was not checked for repeats.
- `Edit` output is byte-identical outside its ops: a replace swaps the value span, an append goes before the closing brace in op order, a delete removes the member and one separator.
- The `400 unknown_parameter` rejection (PAR-2) belongs to the caller, at step 3 of §14.1, which compares `Body.Keys()` with the endpoint's list of known parameters and the alias's rules. Applying the alias's `forward` and `drop` rules belongs to `Build`, which finds the same list by `Input.Endpoint` and lists each rule in `Changes` (PAR-3), so `models explain` shows them through the same code (PAR-10). The per-endpoint lists of known parameters and of walked objects arrive with the phase 2 code in `params`; no type for them is fixed now.
- The same `Scan` reads the outgoing body back. If the tokenizer proves unfit on the pinned toolchain, a hand-written scanner replaces it behind this API without touching callers.

**Outgoing-request builder (ADR-009, PAR-10).**

```go
package plan

type Input struct {
	Endpoint domain.Endpoint  // chat, messages, embeddings, rerank
	Body     *params.Body     // scanned once, at step 3 of §14.1
	Header   http.Header      // only anthropic-version and anthropic-beta (PAR-14)
	Model    *domain.Model    // the alias's settings; nil for targets verify (RSN-9)
	Upstream string           // upstream model id
	Target   *domain.Target
	Provider *domain.Provider // declarations only: no credential is reachable
}

type Layers[T any] struct{ Requested, Alias, Sent *T }
type Change struct{ Name, Action string } // PAR-3

type Outgoing struct {
	URL            string
	Header         http.Header // without the credential; transport adds it afterwards
	Body           []byte      // the exact bytes sent, and the bytes explain returns
	Changes        []Change
	Effort         Layers[string]
	EffortSource   string // client, alias_default, alias_forced, provider_default (RSN-6)
	EffortAdjusted bool
	MaxOutput      Layers[int64]
}

type Skip struct{ Code, Param string } // static ineligibility, with its §15 code

func Build(in Input) (*Outgoing, *Skip)
```

Contract:

- `Build` depends on no clock, randomness, I/O or map iteration order: the same `Input` gives the same bytes (AC-17).
- It is called per target, at step 6 of §14.1, and by `models explain` with the same arguments.
- `Effort.Sent` and `MaxOutput.Sent` are assigned only by running `params.Scan` on `Outgoing.Body` (PAR-8).
- A `Skip` means "target skipped", never an error of the request.

**Mock provider.** Ships in phase 1 as code, because `project-scaffold` owns it and the transport tests need it.

```go
package mockprovider // tests/mockprovider; standard library only

type Step struct { Status int; Header http.Header; Body []byte }
type Call struct { Method, Path string; Header http.Header; Body []byte } // exact bytes received

func New() *Server
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) // POST /chat/completions, /embeddings, /v1/messages
func (s *Server) Script(steps ...Step)                             // queued replies; an empty queue answers a well-formed default
func (s *Server) Calls() []Call
```

A `main` of about fifteen lines, in `tests/mockprovider/cmd/mockprovider` because one directory holds one package and `tests/e2e` imports the library, serves it on a loopback port for the quick start (`go run ./tests/mockprovider/cmd/mockprovider`). Phase 2 adds fields to `Step` (SSE chunks, delay, hang, cut); none exists now because nothing in phase 1 streams.

## 10. Testing and CI

| Layer | What it covers in phase 1 |
|---|---|
| Unit, table-driven | Each validation rule by rule id; address classifier; token format; YAML conversion pitfalls; env file parser; flag loop; listener rule; merge patch per field kind |
| Golden (`testdata/`, `-update`) | Preset catalog; YAML exported after applying `config.example.yaml`; schema after migrations; the error envelope |
| Property | `apply(export(S))` gives an empty diff on S and an equal document on an empty instance |
| Failure injection and `-race` | A failing `Write` publishes nothing and leaves the state untouched; a request context cancelled while its write is queued is still committed and published; concurrent `Commit` and `Snapshot` loads; reload after a commit without publication |
| Store | Pragma read-back on every connection; `query_only` on every reader connection and a refused `INSERT` through `Read()`; a newer `user_version` refused; second lock holder refused |
| In-process CLI | The walk over `commands` (AC-41) |
| End to end (`tests/e2e`) | The built binary on `127.0.0.1:0` (ports read from its start-up log line) |

The end-to-end test builds the binary, starts it, runs the quick start by CLI against the mock provider up to `keys create`, lists `/v1/models` with the new key, restarts and compares `export` (RES-9). It also checks that a second `serve` on the same directory and a non-loopback listener without TLS both exit `1`.

**`make ci` (PUB-5).** One target, run under `env -i` with only `PATH` and `HOME`, so a check that needs an ambient credential fails for maintainers too (PUB-4):

1. `gofmt -l`, `go vet ./...`, `go mod tidy -diff`.
2. `golangci-lint run` (v2.15.0) and `govulncheck ./...` (v1.9.0), installed into a git-ignored `bin/` by `make tools`: the lint release binary by version and checksum, `govulncheck` by `go install …@v1.9.0`.
3. `CGO_ENABLED=0 go build -trimpath ./...` for linux and darwin, amd64 and arm64.
4. `go test -race ./...`, which includes `tests/e2e`.

`make ci` requires Go 1.27 or later and says so in one line when it is missing. `make ci-container` runs the same target inside `golang:1.27.4-trixie`, for machines without Go 1.27. Confirmed in C1: `-race` needs cgo and a C compiler at test time; the builder image has both, and `make ci-container` passes. Measured in C4 (`BenchmarkCommitScale18`, AMD Ryzen 5 5500): at §18 scale, with a secret per key, one change publishes in about 72 ms and the first apply of the whole configuration in about 0.4 s, so the encode-and-decode deep copy stays.

**Fork-safe CI (AC-03).** Tests dial loopback only and DNS is injected, so no test needs the network or a secret. The hosted workflow only calls `make ci-container` and `make image-test` (which runs `make image` first), on `pull_request` and on pushes to the default branch, with read-only permissions, no secret reference and third-party steps pinned by commit. The host is GitHub Actions (OQ-4), so the file is `.github/workflows/ci.yml`. No PUB-2 search ships in the repository: its patterns would be the very names PUB-2 forbids, so maintainers run the search with their own patterns before publishing.

**Container image (SEC-10).** A multi-stage Dockerfile: the builder compiles with `CGO_ENABLED=0 -trimpath` and creates an empty `/data`; the final stage is the pinned distroless image with the binary, `/data` copied with the non-root owner, `STARGAITE_DATA_DIR=/data`, `ENTRYPOINT ["/stargaite"]` and `CMD ["serve"]`. The image never bakes in `--behind-tls-proxy` or a non-loopback listener (§13 point 21). `make image-test` runs it with a read-only root and a volume on `/data` and waits for the ready line in the log, which proves the non-root user can write the data directory; then `docker stop` must end it with exit `0` and the stop line (confirmed in C3). From C5 (planned for C10, brought forward because it was cheap) it also runs `stargaite settings show --json` inside the container: exit `0` proves the key was bootstrapped and the admin API answers. Confirmed by hand in C1: the image runs as uid 65532 and `--chown=65532:65532` gives it `/data`, created with mode `0700`.

## 11. Ordered changes

Each row is one pull request that leaves `make ci` green.

| Id | What it adds | Tests | Makes checkable | Depends on |
|---|---|---|---|---|
| C1 | License, `go.mod`, Makefile (`ci`, `ci-container`, `tools`, `image`), Dockerfile, `cli.Main` with the flag loop and exit codes, `version`, `serve` on the default loopback addresses only, with `/health/live`, JSON logs. The hosted workflow `.github/workflows/ci.yml` | Flag loop; exit codes; `make ci` under `env -i`, building the four platforms; image builds and runs `version` | AC-03 locally; PUB-4, PUB-6 | OQ-9 |
| C2 | `domain` complete; strict codec; YAML conversion; `transport.Classify`, `CheckURL` and `Resolver`; `Env` and the env file parser; `Validate` with every phase 1 rule; `CheckResolved`; offline `validate -f`; `config.example.yaml` | Rule tables, including a `forward` rule naming a key of a target's `off` fragment; the classifier table, with every `Forbidden` entry refused under `allow_private_network`; YAML pitfalls; env file parser; a credential reference with the reserved prefix in mixed case; a name that resolves to a private address, with and without the flag; an unresolved credential reported by `validate -f`; annex B validates clean; codec options confirmed on the toolchain | OPS-5 offline; groundwork for AC-10, AC-15, AC-43 | C1 |
| C3 | `store`: lock, DSN, writer goroutine, read pool, migration 001; `serve` lifecycle with `--listen`, `--admin-listen`, the TLS flags and the listener rule together; `/health/ready`; shutdown; `make image-test` (read-only root, a volume on `/data`, the ready line in the log) | Pragma read-back; `query_only` read-back and a refused `INSERT` through `Read()`; schema golden; second `serve`; listener table; the four-platform build with the SQLite driver; image writes `/data` | AC-42, AC-45 | C1 |
| C4 | `config.Service`: load, `Commit` (set-time resolution included, step 4), diff, configuration SQL, audit rows, snapshot; `transport.Secret`; `serve --env-file` and the `Env` (env file and system resolver) it passes to `config.Open` | Failure injection; `-race`; a request context cancelled while its write is queued; refused change untouched; audit and revision rows, including a `404` or `409` audited as refused and a dry run that writes no row; an unresolved credential reported by a dry run; the `fmt` verbs of §8 on a `Secret`; a benchmark at §18 scale | AC-40 (store half) | C2, C3 |
| C5 | Admin listener: key bootstrap, authentication, envelope, 404 and 405 wrapper; settings, `export`, `apply`, `validate`; CLI client and those commands; the key mutations `apply` needs, with `auth.Issue`: create with a new secret, revoke on `--prune`, `409` on a revoked name | Bootstrap mode and atomicity; `401` on every route; export golden; fixed-point properties; `--dry-run`, `--prune` revoking an absent key, `409` for a revoked name in the file, `--yes`; command walk; token shape and alphabet (§7) | AC-39, AC-40, AC-41 (commands so far) | C4 |
| C6 | `presets`; provider routes and commands; credential status | Preset golden; preset plus flags; env file picked up without restart; planted secret | AC-53, AC-44, AC-43 (set time) | C5 |
| C7 | `transport.NewClient` with its injected resolver (two clients); `tests/mockprovider` | Rebinding through the injected resolver; `302`; metadata address; exact bytes recorded | AC-43 (dial time, redirect) | C2 |
| C8 | Target and alias routes and commands; `models add` creating targets; reasoning flags | Rules through the API; the providers, targets and aliases of annex B built command by command equal those of the applied file | AC-39 over annex B | C6 |
| C9 | Key routes and commands; rotate; `Snapshot.Authenticate`; `GET /v1/models` | The other tests of §7; the keys of annex B created by command equal those of the applied file, secrets aside | AC-06, AC-07 | C5, C8 |
| C10 | Quick-start guide; `tests/e2e` (the `make image-test` check of `settings show --json` landed in C5) | End-to-end flow; image smoke test | PUB-3 as far as phase 1 reaches; AC-41 complete | C7, C9 |

## 12. Risks of phase 1 and how each step retires them

| Risk | Sign | Retired by |
|---|---|---|
| A contributor's machine has no Go, or one below the 1.27 floor | `make ci` cannot start | C1: `make ci-container` on the pinned builder image |
| The flag package stops at the first positional | `models set fast --effort medium` loses the flag | C1: the parse loop and its tests |
| `-race` under `env -i` finds no C compiler | Tests fail only in the clean environment | C1: confirmed on the builder image; documented for bare hosts |
| The distroless non-root user cannot write `/data` | `serve` fails in the image | C1 builds the image, C3 proves the write, C10 smoke-tests the admin API |
| `encoding/json/v2` options behave differently than assumed (unknown members, case, duplicates) | A typo in a field name is accepted | C2: codec tests; fallback is v1 `DisallowUnknownFields` plus a `jsontext` duplicate pass |
| `yaml.Node` bypasses the library's duplicate-key and alias guards; a bare date becomes a timestamp | Annex B golden differs; an alias bomb is accepted | C2: the converter refuses them explicitly, with tests |
| DSN parameters do not reach every pooled connection; a path with special characters breaks the URI | A reader without `foreign_keys` or `busy_timeout` | C3: read-back test on every connection; a test path with spaces and `?` |
| `_query_only=1` without `mode=ro` does not refuse writes on the read pool | An `INSERT` through `Read()` succeeds | C3: `query_only` read-back on every reader connection and the refused `INSERT` |
| The SQLite driver does not build for darwin/amd64, which the survey did not cross-compile | Step 3 of `make ci` fails | C3: the four-platform build, once the driver is a dependency |
| Lock semantics on macOS were only cross-compiled | Two `serve` processes on one directory | C3 on Linux; macOS stays unverified until a maintainer runs the suite there |
| A change is committed but cannot be published | Store and memory disagree | C4: snapshot built before the commit; a queued write always returns its commit result; failure injection; cancellation and reload tests |
| Deep copy by encoding is too slow at 10,000 keys (§18: 1 s) | Slow `apply` | C4: benchmark at §18 scale; if it fails, copy only the changed resources |
| A configuration accepted by phase 1 is refused by a later phase | An upgrade leaves findings on stored data | C2: every declaration-only refusal lands now; the adapter's names remain open (§13 point 6) |
| A credential value leaks through a response, log, export or audit row | The planted secret is found | C4 (`transport.Secret`: no exported way to read the value, and every `fmt` verb redacted), C6 (planted-secret test) |
| DNS rebinding or a redirect reaches a private address, or `allow_private_network` reaches a cloud metadata address outside link-local | An outbound call to an unexpected host (R11) | C2 (set time; the classifier table with the listed metadata addresses), C7 (dial time and redirect, through the injected resolver) |
| Preset data is incomplete when the golden is frozen | A preset changes after release | C6 freezes only what the kick-off states; §13 point 24 |
| Phase 2 forces a rewrite of the snapshot, the builder inputs or the scanner seam | `plan.Build` needs data the snapshot lacks, or the proxy reads an object the scanner did not check | §9 fixed now, with array elements in `walk` and the alias's rules applied inside `Build`; the request-level refusals stay open (§13 point 28); C8 stores every field `Build` reads; C4 exposes declarations per target |
| A dependency update breaks the libc pin | `go mod tidy -diff` fails or SQLite misbehaves | C1: no update bot; the pin rule is written in the contributor notes |
| SQLite WAL or advisory locks on a network file system | Corruption or a lock that does not exclude | Not retired: documented as "local disk only" in the quick start |

## 13. Open points for the owner

Each has a recommendation; the design above already assumes it. The owner accepted the recommendations of points 3 to 28, and the kick-off records those that change it. Points 1 and 2 are settled as written below.

**Blocking**

1. **OQ-9** is settled: the commit author is the project identity `StargAIte`, the license names `StargAIte contributors`, the module path is `github.com/jorlaru/stargaite` and the image name `ghcr.io/jorlaru/stargaite`.
2. **OQ-4** is settled for the CI host: GitHub Actions, with a workflow that only calls the two make targets. How releases are distributed stays a proposal in the kick-off.

**Platform and dependencies**

3. **Supported systems are not stated.** Recommendation: linux and darwin; `serve` on Windows is out of v1, which keeps the lock on the standard library.
4. **ADR-002 and the Go floor.** Settled by the kick-off: ADR-002 names `encoding/json/jsontext` and ADR-001 sets Go 1.27 or later. Nothing to decide.

**Scope against §20**

5. **Declaration-only refusal rules** (PAR-2, PAR-5, PAR-6, PAR-7, RSN-3, RSN-7, USE-12) belong to phase 2 and 3 features, but OPS-5 lists them for every change and phase 1 stores the declarations. Recommendation: land them in phase 1, as designed.
6. **PAR-2's collision list includes "a field of the protocol adapter"**, known only when the adapter is written. Recommendation: add the adapter's written top-level names to §7.1 now, or accept that phase 2 may refuse a stored `forward` rule.
7. **AC-06 and AC-07 have no authenticated route before phase 2.** Recommendation: move `GET /v1/models` from `chat-completions` to `keys-auth`. If refused, both criteria are checked only at unit level and C9 drops the route.
8. **AC-44 is phase 1, `secrets reload` is phase 4.** Recommendation: keep the command in phase 4; in phase 1 any accepted admin write re-reads the env file (OPS-6's first trigger).
9. **Annex A as migration 001 creates the usage tables two phases early.** Recommendation: apply it verbatim, and declare migrations immutable only from the first tagged release, so phase 3 can still amend 001.

**Contract gaps and contradictions**

10. **§15 has no row for a refused change, an unknown admin resource or an unreachable admin API.** Recommendation: `400 invalid_request` with `findings`; one new code `not_found` (404); a CLI-local `admin_unreachable` (`api_error`) so `--json` stays valid JSON.
11. **SEC-7 says "every admin operation", AC-40 says "every admin change".** Recommendation: audit writes (applied, refused, unchanged), a write that ends in `404` or `409` included; do not audit reads or dry runs; failed authentication goes to the log. The actor is the constant `admin`.
12. **Revision semantics (OPS-4, glossary).** Undefined for a change with an empty diff and for a republication at start. Recommendation: both rebuild and publish the snapshot under the same revision; only a non-empty diff increments it.
13. **§13.1 "ready: a valid configuration is published"** against a stored configuration that a newer binary finds invalid. Recommendation: publish it, log every finding, stay ready; a release that tightens a rule says so in its notes.
14. **SEC-4 "validated when it is set"** does not say whether names are resolved. Recommendation: resolve only the URLs a change sets, and every URL for `validate`; an address the URL may not use (`Forbidden`, or `Private` without the flag) refuses, a failed lookup warns; dial time is the authority.
15. **Defaults: stored or applied at read time?** OPS-3 ("returns to its default"), USE-11 ("show in export"), LIM-7 and RSN-2 pull in different directions.
    - Recommendation: store what is declared and apply defaults when the snapshot is built; `settings show` prints effective values.
    - Consequences to accept: a release that changes a default changes running instances; a key cannot be declared without a parallel limit; a target created by `apply` does not inherit `target_defaults`; `max_response_bytes` has no stated default (annex B's 32 MiB is used).
16. **PAR-1 is narrower than the tokenizer's default.** Recommendation: literal PAR-1 (repeats refused only in read or written objects); invalid UTF-8 is already `400 invalid_request` under PAR-1. The alternative, for the owner to weigh: keeping the decoder's default rejection for the whole body removes the scanner's own tracking of indexed objects and unescaped names (§9), at the price of a contract stricter than literal PAR-1.
17. **The admin key has no stated format or strength (SEC-3).** Recommendation: generated as 52 lowercase base32 characters with no prefix; an operator-supplied key under 32 characters, or starting with `sk-stargaite-`, stops `serve`.
18. **SEC-5 only forbids the `STARGAITE_` prefix.** Recommendation: also require a syntactically valid variable name; letters may be in either case, and the forbidden prefix is matched in any letter case.
19. **Resource-name syntax is unspecified**, and so is the spelling of `models: *` in YAML, where a bare `*` is an alias marker. Recommendation: providers, aliases and keys match `[A-Za-z0-9][A-Za-z0-9._-]*` up to 64 characters; an upstream model id is any text without spaces or control characters; "all models" is `models: ["*"]`.
20. **`set` semantics** for clearing a scalar and for structured fields are unspecified. Recommendation: merge-patch semantics: `--flag=` clears, `--param a=drop` changes one entry, lists are replaced whole.
21. **SEC-6 against the container:** a container must listen outside loopback, which `serve` refuses without flags; the kick-off also does not say which listener the TLS flags cover. Recommendation: the image runs plain `serve` and the guide shows the explicit flags; the TLS flags apply to both listeners, with the rule evaluated per listener.
22. **SEC-6's server-wide write timeout would cut unbuffered streams (RES-3, RES-4).** Recommendation: keep the server-wide write timeout on both listeners. In phase 2 each inference handler sets its own deadlines with `http.ResponseController`: the RES-3 total for a non-streaming request, and a write deadline moved forward on every chunk for a stream.
23. **Outbound proxies are not mentioned.** Honouring `HTTP_PROXY` would make the dial-time check see the proxy's address. Recommendation: ignore proxy variables in v1 and say so in the guide.
24. **§7.2 lacks most per-preset data** needed by the AC-53 golden: `target_defaults`, `stream_usage`, `cost_report` path and unit, `gen_ai_provider`, and `billing` for the hosted presets. Recommendation: complete the table from public documentation before the golden is frozen; until then hosted presets carry `billing: pay_per_use` and the rest stays undeclared.
25. **A key naming an unknown alias** is not in OPS-5 and annex A has no foreign key for it. Recommendation: a warning, so `models remove` and `--prune` never require editing keys first.
26. **PUB-2's search cannot ship its own deny-list.** Recommendation: ship no search code; maintainers run the search over files and commit messages with their own patterns before each publication.
27. **Annex B puts `settings.telemetry` in the document phase 1 stores.** Recommendation: validate it in phase 1 under SEC-4, SEC-5 and TEL-2, as designed, although `telemetry set` ships in phase 3.
28. **Where the request-level refusals that `explain` must share are raised** (`400 invalid_reasoning_effort`, and `422 max_output_tokens_exceeded` for a client cap above the alias cap) is not fixed: `Build` returns only a per-target `Skip` (§9), and PAR-10 wants `explain` on the data plane's code path. Recommendation: one request-level check in `plan`, called before the first `Build` by the data plane and by `explain` alike; its signature is settled in phase 2 with `param-contract` and `reasoning-control`, and nothing phase 1 builds depends on it.
