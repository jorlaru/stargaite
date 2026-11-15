# Phase 4 design

Status: the design phase 4 of §20 of `kick-off.md` is built from, the last phase of v1. It builds on the phase 1 to 3 code and on `docs/design/phase-1.md`, `docs/design/phase-2.md` and `docs/design/phase-3.md` (cited as "phase-N §M"). Rule ids (PAR, RSN, TEL, USE, LIM, RES, SEC, OPS, PUB, ADR, AC, OQ) are the kick-off's and binding. Provider, OpenAI, Prometheus, SQLite, Go and OWASP facts come from the public documentation and source as read on 2026-11-15, without live calls; "measured" marks a fact from Go programs run in the pinned `golang:1.27.4-trixie` image against `modernc.org/sqlite` v1.62.0 (SQLite 3.54.1) and the repository's migrations; "derived" marks a mapping that no vendor documents. Where the kick-off is silent, outdated against that documentation, or contradicted by it, §15 says so with a recommendation, and the design below assumes the recommendation unless the point says otherwise. A bare "§N" is a section of this document; the kick-off's sections are cited as "kick-off §N".

## 1. Scope and outcome

Phase 4 delivers `embeddings`, `rerank`, `targets-verify`, `metrics`, `operations` and `hardening`. It starts from the interfaces of phase-3 §13 and its Settled notes, and from the seams of phase-2 §11; where the shipped code does not keep those promises, §3 names the change and §15 point 27 lists them. No exported signature that phases 1 to 3 ship changes; exported additions and unexported changes are named in §3. At the end of phase 4:

- `POST /v1/embeddings` and `POST /v1/rerank` go through the same handler as chat (authentication, admission, budgets, quotas, fallback, records, counters, spans); embeddings is checked against the OpenAI Embeddings schema, rerank is a passthrough to the provider's `rerank_url` with only `model` replaced.
- `stargaite targets verify <target> [--max-output-tokens n]` calls a chat target once per declared level, reports the outgoing body, the status and the reasoning tokens of each call, records the calls under `_verify`, stores a successful result with its date and version, and `validate` warns about chat targets never verified.
- `GET /admin/v1/metrics` renders the OPS-8 counters, span counts, the exporter's counters and last error, and the budget and quota figures as Prometheus text, to the admin key or the read-only `STARGAITE_METRICS_TOKEN`.
- `stargaite backup <file>` writes a checked `VACUUM INTO` copy; restoring it is a documented, tested procedure; retention runs inside the process once a day in small batches; `stargaite secrets reload` resolves credential references again; every migration is tested from every earlier version.
- `tests/adversarial` runs in `make ci`; `make fuzz` and `make bench` run outside it, the benchmark against the targets of kick-off §18, its figures and accepted deviations in `docs/benchmark.md`.
- The quick start of `README.md` adds an embeddings alias and call, `targets verify`, a metrics scrape and `backup`.

| Criterion | Checkable at the end of phase 4 | How (§11) |
|---|---|---|
| AC-44 | The second half: a credential renamed into the env file works after `secrets reload`, with no other write | `TestSecretsReload` |
| AC-46 | Embeddings and rerank served, fallen back, refused, recorded per key and alias, spanned | `TestEmbeddings`, `TestRerank`, e2e |
| AC-47 | Bodies byte-equal to the mock's, reasoning tokens per level, stored result, `_verify` records | `TestVerify` |
| AC-48 | Every family equals SQL over `usage` and the scripted counts, exactly | `TestMetricsMatchStore` |
| AC-49 | Retention deletes inside the process; `page_count` plateaus under constant simulated traffic | `TestRetentionPlateau`, `TestServeRetention` |
| AC-50 | Every version migrated to head with data; a backup restored by the documented procedure | `TestMigrations`, `TestBackupRestore` |
| AC-51 | The adversarial suite, every row citing its rule | `tests/adversarial`, `TestServeAdversarial` |
| AC-52 | Figures against kick-off §18, misses listed as accepted deviations | `make bench`, `docs/benchmark.md` (maintainers) |
| TEL-3 | Exporter counters and last error on the metrics endpoint | `TestMetricsMatchStore` |
| OPS-5 | The warnings "chat targets never verified" and "aliases that cannot serve an endpoint" | `config` validation tables |

## 2. Decisions

| Decision | Choice | Reason | Discarded |
|---|---|---|---|
| Where embeddings and rerank run | `h.infer(ep)`, unchanged from authentication to `done`; `plan.Check`, `plan.Build`, `relayFor` and `NewRecord` branch by endpoint | Every protection of chat applies with no second path (LIM-*, RES-*, SEC-4, USE-1) | A separate handler per endpoint |
| Embeddings schema | `params.Embeddings`: `input`, `model`, `dimensions`, `encoding_format`, `user`, as of 2026-11-15; `stream` is unknown | PAR-1 names OpenAI Embeddings; the reference read today lists exactly these | A wider list with provider fields (they take `forward` rules, PAR-2) |
| Rerank schema | None: top-level repeated keys only, no PAR-2, only `model` replaced, no alias rule applied | kick-off §7.1 | A `params.Rerank` value |
| Embeddings and rerank tokens | Input `usage.prompt_tokens`, else `usage.total_tokens`; output 0 (derived) | Voyage, Jina and vLLM report only the total; the openai output rule would book it as output (§15 point 1) | USE-3's openai rule unchanged |
| Rerank without reported usage | The USE-8 estimate, as shipped: input from the bytes sent, output 0, `tokens_source: estimated`, priced by the target's table when it has one (USE-6) | A priced rerank target whose provider reports no `usage` object (Cohere, DeepInfra, TEI's bare array) would otherwise escape every budget (§15 point 4) | 0 tokens and no table price, a literal reading of kick-off §7.1 |
| Effort fields off chat | `EffortSource` `provider_default`, other layers NULL; spans keep every annex D attribute, only `gen_ai.operation.name` and the span name change | RSN-6 ("whatever the cause"), TEL-4 | Dropping reasoning attributes from these spans |
| `content_empty` | Chat and messages only | Every 2xx embeddings record would otherwise count as empty | A per-endpoint visible-bytes count |
| Content capture | Off on embeddings and rerank (§15 point 10) | The pinned embeddings span defines no content attribute; vectors are not messages | Capturing vectors as `gen_ai.output.messages` |
| Verify path | `api.Verify(Deps)` drives the data plane's own `exclude`, `call`, `finish` and `done` with key `_verify` and no alias | One path for SEC-4, SEC-5, RES-3, RES-4, records and spans | Rebuilding the call in `admin` |
| Verify and the circuit | A fresh `routing.Breakers` per call: it neither opens nor obeys a circuit | An operator check must not open a breaker for live traffic, nor be blocked by one | The data plane's shared breakers |
| Verify cap | The operator's value as an alias cap: `Check` and `Build` get `Model: &domain.Model{MaxOutputTokens: n}` (nil without `--max-output-tokens`), so `Build` writes it under the target's `output_cap_param` toward `openai` and as `max_tokens` toward `anthropic`, listed `added` (PAR-3); the record's `max_output_alias` and `max_output_sent` hold it | A client cap member must be a name the chat schema knows, and `output_cap_param` is free text; Build's alias-cap path names it; a target refusing `max_tokens` would fail verify (R3) | A cap member in the verify body |
| Verify result | Stored only for an all-2xx run and an unchanged target and provider, under `Commit`'s mutex; every run audited; a change of the target or its provider clears it | Annex A's column comment ("last successful"); SEC-7 | Storing failures; a migration for per-level rows |
| Metrics format | `text/plain; version=0.0.4; charset=utf-8`, no negotiation, standard library | Prometheus 3's last-resort format, valid for any scraper; label values carry any UTF-8 | `client_golang` v1.25.0 (twelve direct modules); OpenMetrics |
| Span counts by key and alias | A second `usage.Counters`, `Deps.Spanned`, fed by `done` | `TestSamplingInvariant` compares every entry of `Deps.Counters`; a span counter there breaks AC-28 | A `spans` counter in the shared set |
| Exporter last error | One info series, value 1, its label the status `last_error` (already redacted, 200 bytes) | TEL-3 names "the metrics endpoint" | Leaving it in `telemetry status` only |
| Quota figures on a scrape | `Windows()` as held; a scrape never calls `Sync` | A read must not seed from the store or drop windows | `Sync` per scrape |
| Backup | `VACUUM INTO` on a connection of its own to a server-named file in the data directory, checked, unlinked and streamed; the CLI writes the local file | The read pool refuses it and the writer would stall every batch (measured); no file-write primitive in the admin API | A server path given by the operator; the backup API (it may restart forever under writes) |
| Restore | A documented, tested procedure; no command | kick-off §13.3 has three serverless commands; a stale `-wal` is the hazard (measured) | A fourth local command (§15 point 20) |
| Retention | One goroutine in `serve`, once a day, batches of 500 rows by `rowid IN (SELECT … LIMIT 500)`, one writer job each | USE-11, ADR-017; `DELETE … LIMIT` is not compiled in (measured) | An external scheduler; `VACUUM` |
| Audit index | None: no migration 003 | The audit table holds configuration changes and warnings; the batch scan is short (§15 point 23) | `idx_audit_time` |
| Secrets reload | `Commit` with a no-op `Mutate`: fresh env file, republished snapshot, an `unchanged` audit row; a change with an empty diff is never refused for error findings | phase 1 already re-reads the file on every commit; a stored configuration with errors is still published (kick-off §13.1), so it must still reload | A new `Service` method; `SIGHUP` |
| Adversarial suite | In process in `make ci`; raw-socket cases in `cmd/stargaite` at lowered timeouts | Fast, no flake from a built binary; the server options live in `serve.go` | The built binary on every endpoint; an HTTP/2 flood in CI |
| Benchmark | `TestBenchmark` in `tests/benchmark`, skipped unless `STARGAITE_BENCH=1`; open loop; `make bench` | Compiled, vetted and linted by `make ci`, so it does not rot; no coordinated omission | A build tag; a third-party load tool |
| Toolchain floor | `go.mod` says `go 1.27.4` | GO-2026-6859 is a crash in `MaxBytesReader`, which LIM-6 uses; nine net/http advisories were fixed in 1.27.2 | `go 1.27.0` |
| Dependencies | None added | ADR-002, PRI-14 | — |

## 3. Package map

| Package | Phase 4 change | May import (internal) |
|---|---|---|
| `domain` | `EndpointEmbeddings`, `EndpointRerank`; `VerifyKey = "_verify"` | — |
| `params` | `Embeddings` schema | unchanged |
| `plan` | `Check` and `Build` serve the two endpoints (`passthrough`); Build's rule loop moves into an unexported `rules`, shared | unchanged |
| `adapters/openai` | `ObserveUsage` | unchanged |
| `usage` | `NewRecord` rules by endpoint; `Cutoffs`, `CutoffsAt`, `Retain` | `domain`, `plan`, `admission` |
| `admission` | `SeedSpan` exported; the seed reads it | `domain` |
| `telemetry` | `operation(ep)` replaces the constant | unchanged |
| `store` | `Backup`, `Check`; `migrate` takes an `fs.FS`; `Close` waits for a backup; `Open` removes leftover copies | none internal |
| `config` | `State.Verified`, `Snapshot.Verified`, `Service.Verified`, `Service.Verification`, `Unverified`; columns cleared by `writeChange`; `endpoint_unservable`; PAR-7, RSN-5 and RSN-7 read chat targets only; the `quota_beyond_retention` text; `Commit` never refuses an empty diff for error findings (behaviour only, §8) | unchanged |
| `api` | Two routes; `Verify`, `Verification`, `VerifyCall`; `Deps.Spanned`; the unauthenticated counter; `scopeNames` and `budgets` nil-safe; `server.address` from `out.URL` | unchanged |
| `admin` | `verify`, `metrics`, `backup`, `secrets/reload` routes; the metrics token in the wrapper; `Deps` gains five fields; `recorder` gains `Unwrap() http.ResponseWriter`, so `http.NewResponseController` reaches the connection's deadlines (unexported, needed by verify and backup) | + `api` |
| `cli` | `targets verify`, `secrets reload`, `backup`; long requests without the client timeout; `targets show` prints the verification | unchanged |
| `cmd/stargaite` | Shared counters and `api.Deps`; the metrics token; retention and check goroutines; stop order; `runServers` gains a `ready func()`; the `ReadHeaderTimeout` literal becomes the package variable `serverReadHeaderTimeout = 10 * time.Second`, which `TestServeAdversarial` lowers to 200 ms | unchanged |
| `tests/mockprovider` | `POST /rerank` | — |
| `tests/adversarial`, `tests/benchmark` (new) | §9 | any |

Import rule: unchanged. `admin → api` is a new edge the rule allows (`api` never imports `admin`); `admin` reaches the store only through a `Backup` function value, so it does not import `store`.

Differences from annex C and from phases 1 to 3, each with its reason:

- Retention lives in `usage`, not `store`: its cutoffs read settings and quota windows, and `store` imports no internal package (phase-1 §3). Annex C puts "backup, retention" in `store`; backup is there.
- Phase-3 §13 point 1 ("`usage.NewRecord` and the spans need nothing more") does not hold: `telemetry/spans.go` fixes `gen_ai.operation.name` to `chat`, `NewRecord` sets `content_empty` on every 2xx with no visible bytes, and `spanRequest` takes `server.address` from `base_url`, which `rerank_url` may not share. §4 changes all three, without a signature change.
- Phase-3 §13 point 2 names `Exhausted`, `Admit`, `Take` and `Writer.Add`; verify also needs `Spend` (global) and `Ended`, which it gets by reusing `finish`; `scopeNames` and `budgets` dereference `rq.model` and become nil-safe (§6).
- Phase-3 §13 point 3: `serve.go` builds the counters inline inside `api.Deps`; they are hoisted and `admin.Deps` gains them. The OPS-8 "spans emitted" by key and alias and the TEL-5 unauthenticated metric (phase-3 §4, step 1) were never counted; §7 adds both.
- Phase-3 §13 point 4 names `usage` and `usage_daily`; USE-11 also sets audit at 24 months (§8).
- Phase-3 §13 point 5 says `VACUUM INTO` is store-level, but `Write` runs inside a transaction and the read pool is `query_only`: both refuse it (measured). `Backup` opens its own connection (§8).

```go
package admin

type Deps struct {
	Config      *config.Service
	KeyHash     [32]byte
	Log         *slog.Logger
	Usage       *sql.DB
	Admission   *admission.State
	Spans       *telemetry.Exporter
	Counters    *usage.Counters // the data plane's (OPS-8)
	Spanned     *usage.Counters // spans emitted, by key and alias
	MetricsHash *[32]byte       // SHA-256 of STARGAITE_METRICS_TOKEN; nil when unset (SEC-3)
	Verify      func(ctx context.Context, target string, maxOutput *int64) (api.Verification, *domain.Error)
	Backup      func(ctx context.Context) (*os.File, error) // store.Store.Backup
}
```

`serve` builds `counters`, `spanned` and one `api.Deps` value, and passes `api.Handler(d)`, `api.Verify(d)` and `st.Backup` to the two listeners.

## 4. Embeddings

**Inbound.** `POST /v1/embeddings` is `h.infer(domain.EndpointEmbeddings)`. `authenticate` takes `Authorization: Bearer` only, as on chat. `check` reads the body under `max_body_bytes` (LIM-6, `413`) and scans it with the walk `params.Embeddings.Walk` (nil): only the top level is indexed, so a repeated top-level key or invalid UTF-8 is `400 invalid_request` (PAR-1); the alias is resolved (`404`) and allowed (`403`, SEC-2). `plan.Check` refuses an unknown top-level key without a rule (`400 unknown_parameter`, PAR-2) and returns `&Client{}`: no stream, no effort, no cap, so `rq.client` is never nil. `stream` is not in the schema, so `"stream": true` is `400 unknown_parameter`.

```go
package params

// Embeddings is OpenAI Embeddings, the reference as read on AsOf (PAR-1, R7).
var Embeddings = Schema{AsOf: "2026-11-15", Known: set("input", "model", "dimensions", "encoding_format", "user")}
```

**Build.** `plan.Build` calls `passthrough` first for both new endpoints; nothing else of `Build` runs for them.

```go
package plan

// passthrough is Build for embeddings and rerank (kick-off section 7.1): the
// target's kind and protocol decide; embeddings applies the alias's rules and
// PAR-12 as chat does; rerank applies none. Only "model" is replaced, which
// PAR-3 does not list. No cap, effort or include_usage is ever written, so the
// source is provider_default (RSN-6) and every other layer is nil.
func passthrough(in Input) (*Outgoing, *Skip) {
	t, p := in.Target, in.Provider
	out := &Outgoing{Header: http.Header{"Content-Type": {"application/json"}}, EffortSource: reasoning.SourceProviderDefault}
	switch {
	case p.Protocol != domain.ProtocolOpenAI:
		return nil, &Skip{Code: domain.CodeEndpointNotSupported}
	case in.Endpoint == domain.EndpointEmbeddings && t.Kind == domain.KindEmbeddings:
		out.URL = strings.TrimSuffix(p.BaseURL, "/") + "/embeddings"
	case in.Endpoint == domain.EndpointRerank && t.Kind == domain.KindRerank && p.RerankURL != "":
		out.URL = p.RerankURL
	default:
		return nil, &Skip{Code: domain.CodeEndpointNotSupported}
	}
	model, err := jsontext.AppendQuote(nil, in.Upstream)
	if err != nil {
		return nil, &Skip{Code: domain.CodeInternalError, Param: err.Error()}
	}
	ops := []params.Op{{Path: []string{"model"}, Value: model}}
	if in.Endpoint == domain.EndpointEmbeddings {
		drops, changes, skip := rules(in.Body, cmp.Or(in.Model, &domain.Model{}), t, params.Embeddings, nil)
		if skip != nil {
			return nil, skip // 422 unsupported_parameter when no target is left
		}
		ops, out.Changes = append(drops, ops...), changes
	}
	if out.Body, err = in.Body.Edit(ops...); err != nil {
		return nil, &Skip{Code: domain.CodeInternalError, Param: err.Error()}
	}
	return out, nil
}
```

`rules(b, m, t, schema, noEquivalent) (drops []params.Op, changes []Change, skip *Skip)` is Build's existing loop over `b.Keys()` (unsupported without `drop`: skip; `drop`: removed, listed `dropped`; `forward` on an unknown key: listed `forwarded`), moved unchanged so chat and embeddings share it. A chat target, an `anthropic` target or a rerank target behind an embeddings alias is a static skip; every target skipped gives `422 endpoint_not_supported` (RES-10). `exclude` then applies budget, quota, store, credential and circuit as for chat.

**Call and observation.** `call` is unchanged: SEC-4 at dial time through the same two clients, no redirect, the RES-3 total timer (embeddings never stream), `max_response_bytes` (RES-4, §15 point 8). `relayFor` gains one case:

```go
case ep == domain.EndpointEmbeddings || ep == domain.EndpointRerank:
	return relay{sync: func(body []byte, _ time.Time) ([]byte, domain.Observed, error) {
		if ep == domain.EndpointRerank && !object(body) {
			return body, domain.Observed{}, nil // TEI's bare array: relayed, not observed (section 5)
		}
		return openai.ObserveUsage(p, body)
	}}
```

```go
package openai

// ObserveUsage reads an embeddings or rerank answer and relays it unchanged:
// the returned model, the usage and the cost at cost_report.path, in one scan
// whose walk is {usage} plus the parents of the cost path. Input is
// usage.prompt_tokens, else usage.total_tokens (derived); Output is 0;
// Reasoning nil; Usage nil when the usage object is absent. The error says the
// body is not a JSON object or repeats a key in an object it reads: a 2xx that
// is not relayed, eligible for fallback (phase-2 section 10).
func ObserveUsage(p *domain.Provider, body []byte) ([]byte, domain.Observed, error)
```

The answer is relayed with the bytes the provider sent: `base64` or `float` vectors alike, never decoded; `usage` is the same object in both encodings.

**Record.** `NewRecord` keeps its signature and gains rules by endpoint:

```go
chatLike := c.Endpoint == domain.EndpointChat || c.Endpoint == domain.EndpointMessages
if c.Status/100 == 2 {
	// as today: reported usage, or the USE-8 estimate, rerank included (section 5)
	r.ContentEmpty = chatLike && c.Obs.VisibleBytes == 0 && c.Obs.ToolCalls == 0
}
```

An embeddings record has endpoint `embeddings`, `stream` false, `finish_reason` empty, input tokens, output 0, `reasoning_tokens` NULL, the cost by USE-6 (a priced embeddings target declares `output_per_million: 0`; validation is unchanged), `effort_source` `provider_default` and every other effort and cap column NULL. A 2xx without usage is estimated as USE-8 says: input from the bytes sent, output 0.

**Spans.** `telemetry.operation(ep)` is `embeddings`, `rerank` (the custom value annex D names) or `chat`; it names the request span (`embeddings embed`) and fills `gen_ai.operation.name`. Every other annex D attribute stays as built: the effort source `provider_default`, `adjusted` false and the dialect, because annex D sets them "when a provider was called" and TEL-4 removes nothing; `gen_ai.usage.output_tokens` is the recorded 0 (§15 point 9). `spanRequest` takes `server.address` and `server.port` from `a.out.URL`, the URL actually called, instead of the provider's `base_url`; for chat both give the same host. `admit` sets `rq.content` only on chat and messages (§15 point 10).

**Validation.** A new warning `endpoint_unservable` (OPS-5) names an alias none of whose targets can serve any endpoint: a chat target serves chat (and messages toward `anthropic`); an embeddings target on `openai` serves embeddings; a rerank target on `openai` whose provider has `rerank_url` serves rerank. PAR-7 `output_cap_required`, RSN-7 `effort_required` and RSN-5 `effort_dropped` read chat targets only, since `passthrough` writes no cap and no effort (§15 point 12).

**Refused and fallen back.** An unknown key on embeddings: `400`, one error span with `stargaite.attempts: 0`, no record, `requests{status="400"}`. A first target's `503`: record 1 and a child span `attempt {target}`, then the next target built afresh, record 2 under the same request id.

## 5. Rerank

`POST /v1/rerank` is `h.infer(domain.EndpointRerank)`. `check` scans with walk nil, so only the top-level object is indexed: a repeated top-level key is `400` (kick-off §7.1); a body that is not valid JSON or not valid UTF-8 anywhere is also `400 invalid_request`, because the tokenizer cannot read it (§15 point 32); `model` must be a string naming an allowed alias. `plan.Check` returns `&Client{}` at once for rerank: no PAR-2, no type check. `passthrough` sends the body to the provider's `rerank_url` with `model` replaced by the upstream id and nothing else touched, not even an alias rule. A `stream` member is forwarded; the answer is read whole as a synchronous one.

**Answers.** An answer whose top level is an object is observed by `ObserveUsage`: `results[]` or `data[]` are never read, so Cohere, Jina, Voyage, vLLM, Infinity, Together, Fireworks and OpenRouter shapes all relay unchanged. A 2xx whose top level is not an object (TEI's bare array, which reports tokens only in `x-compute-tokens`) is relayed unchanged and unobserved. A body that is not JSON is relayed as the provider sent it, unobserved, for the same reason: the proxy defines no rerank schema.

**Metering.** One request, plus tokens when the response reports them (kick-off §7.1): `usage.prompt_tokens`, else `usage.total_tokens`, as input. A 2xx without usage, an unobserved one included, is estimated as USE-8 says: input from the bytes sent, output 0, `tokens_source: estimated`; it is priced by the target's table when there is one (USE-6), so a budget always moves (§15 point 4). Unpriced and counted so (USE-12) is only the case of a target with neither prices nor a `cost_report`, which USE-12 already refuses under a budget; a provider `cost_report` (OpenRouter's `usage.cost`) wins over the table. Search units are not recorded (§15 point 2).

**Validation.** `endpoint_unservable` covers a rerank target whose provider has no `rerank_url`. `rerank_url` is already a SEC-4 outbound URL (phase 1).

**Mock.** `POST /rerank` joins the routes, default `{"object":"list","model":"mock","results":[{"index":0,"relevance_score":0.5}],"usage":{"total_tokens":3}}`; bare arrays and usage-free bodies are scripted `Step`s.

## 6. Targets verify

```go
package api

// VerifyCall is one call of a verify run: the level asked ("" for the call
// without reasoning control), what Build sent and what the provider answered.
type VerifyCall struct {
	Effort          string        `json:"effort"`
	EffortSent      *string       `json:"effort_sent"`
	Status          int           `json:"status"` // the provider's; 0 without a response
	ReasoningTokens *int64        `json:"reasoning_tokens"`
	OutputTokens    int64         `json:"output_tokens"`
	Body            string        `json:"body"` // the exact bytes sent (PAR-8)
	RequestID       string        `json:"request_id"`
	Error           *domain.Error `json:"error,omitempty"`
}
type Verification struct {
	Target   string          `json:"target"`
	Version  string          `json:"version"` // Deps.Version
	OK       bool            `json:"ok"`      // every call answered 2xx
	At       time.Time       `json:"at"`      // the service clock at the end of the run
	Calls    []VerifyCall    `json:"calls"`
	Spec     domain.Target   `json:"-"` // what was verified, for config.Service.Verified
	Provider domain.Provider `json:"-"`
}

// Verify returns the live check of RSN-9 over d's data plane: the snapshot is
// read once per run; each call is its own request through exclude, call,
// finish and done.
func Verify(d Deps) func(ctx context.Context, target string, maxOutput *int64) (Verification, *domain.Error)
```

**Refusals before any call.** An unknown target is `404 not_found`; a target whose kind is not `chat` is `400 invalid_request`; an `anthropic` target without `--max-output-tokens` is `400 invalid_request` naming PAR-7.

**Calls.** One per declared `levels` entry, in scale order, then `none` when `can_disable` is `true`; with neither, one call without reasoning control. A level `on` (an on/off dialect) is sent as `medium`, which RSN-4 maps to on. Each body is `{"model":"_verify","messages":[{"role":"user","content":"What is 17 × 23? Answer with the number only."}]}`, plus `"reasoning_effort":"<level>"`, and no cap member. Each goes through `params.Scan(body, params.Chat.Walk)`, `plan.Check(chat, b, m)` and `plan.Build` with that `m`, where `m` is `&domain.Model{MaxOutputTokens: n}` for `--max-output-tokens n` and nil without it (phase-1 §9's nil Model). `Build` then writes `n` under `cmp.Or(t.OutputCapParam, domain.DefaultOutputCapParam)` toward `openai` and as `max_tokens` toward `anthropic`, listed `<name>:added` (PAR-3). An `n` above the target's `max_output_tokens` is `400 invalid_request` naming PAR-6 before any call. So the report's body is the byte-exact one sent, translated toward `anthropic`.

**Per call, in `Verify`:** a `request` with `id: rand.Text()`, `ep: chat`, the run's snapshot, `key: &config.Key{Name: domain.VerifyKey}`, no alias, one trace shared by the run; `Flight.Add(1)` and `defer h.done(rq)`; `Admit` with the global scope only; `Build`; `exclude`; `call` with a discarding `http.ResponseWriter` and `http.NewRequestWithContext(ctx, …)`, since `call` reads only `r.Context()`; `rq.err` set to the call's error so the request line and span carry it. `Verify` also holds one `Flight.Add(1)` for the whole run, so the counter never reaches zero between calls while `serve` waits on it.

- `scopeNames(rq)` returns only `global` and `budgets(rq)` only the instance budget when `rq.model` is nil (a shipped unexported function, named). So no key or alias limit applies; the global budget (`Exhausted`, then `Spend` in `finish`) and the provider's quota (`Quota`, `Take` with `Plan{Key: "_verify", Share: 0}`, then `Ended` in `finish`) do, with the store and credential checks (RSN-9).
- Each call gets a fresh `routing.Breakers`: verify neither opens a circuit for live traffic nor is blocked by one (§15 point 13).
- A refusal before the provider (`402`, `429`, `503`) ends the run with that error on the call; a provider error does not, so later levels still report. Before each call the run ends when `ctx.Err() != nil`, and a call that ended because its context did (shutdown or the client gone) also ends it; the remaining levels are not attempted and leave no record.
- `finish` writes the record (`key_name` `_verify`, `model` `''`, attempt 1, `effort_source` `client`, or `provider_default` without control), counts it and adds it to the writer; `done` counts `requests` under `_verify`, emits one span per call when an exporter is published (key `_verify`, no alias), and writes the request line.

**Admin route.** `POST /admin/v1/verify/{id...}` with `{"max_output_tokens": n}`: the target routes end in `{id...}`, which must be the last segment, so verify is its own resource, as kick-off §13.2 lists it. The handler clears its read deadline once the body is read, then sets its write deadline once to `now + k × (total_ms + 5 s)`, where k is the number of calls the run will make; `call`'s own `writeDeadline` acts on the discarding writer and has no effect. It then calls `Config.Verified` and answers the `Verification` plus `stored`.

```go
package config

type Verification struct {
	At      int64  `json:"verified_at"` // Unix ms: targets.verified_at
	Version string `json:"verified_version"`
}
// State gains Verified map[string]Verification, loaded from the targets
// columns (load selects them); Snapshot gains the same map, copied by build.
// Verified runs under s.mu, writes the columns, then stores a shallow copy of
// the current snapshot with the new map and findings through s.snap.Store.
// Verification(id) is s.Snapshot().Verified[id] and Check passes
// s.Snapshot().Verified to Unverified, so no read takes s.mu.

// Verified records a verify run under Commit's mutex: one audit row always
// (action "targets verify <id>", resource target, result verified, failed or
// stale, new_value the summary of each call without bodies); the columns only
// when ok and the target and its provider still equal t and p. No revision.
func (s *Service) Verified(ctx context.Context, id string, t domain.Target, p domain.Provider, ok bool, at time.Time, version, summary string) (stored bool, err error)
func (s *Service) Verification(id string) (Verification, bool)

// Unverified is the RSN-9 warning target_unverified for each chat target
// without a stored result; Open, Commit and Check append it, validate -f
// (no store) does not (section 15 point 33).
func Unverified(d *domain.Document, v map[string]Verification) []domain.Finding
```

`writeChange` clears `verified_at` and `verified_version` on a target update (`ON CONFLICT … DO UPDATE SET …, verified_at = NULL, verified_version = NULL`) and on every target of an updated provider; `Commit` drops the same entries from `next.Verified`. `export` emits the document only, so the columns never reach a file. `targets show` and `targets list` add `verified_at` and `verified_version`.

**AC-47.** `TestVerify` (`internal/admin`, with `api` in process): a target with `dialect: reasoning_effort`, `levels: [low, high]`, `can_disable: true`; the mock scripts `completion_tokens_details.reasoning_tokens` 10, 50 and 0. Checked: three calls; each `Body` byte-equal to the mock's `Calls()`; the reasoning tokens per level; three `usage` rows with `key_name = '_verify'` and `model = ''`; the global spend moved and no key budget; `verified_at` at the test clock and the version stored; the warning gone from `validate`. Then: a `400` on one level stores nothing and audits `failed`; an exhausted global budget gives `402` before any mock call; an exhausted window `429`; an `anthropic` target without a cap `400`; targets whose `output_cap_param` is `max_completion_tokens` and `max_output_tokens` each receive that member, listed `added`; a target changed during the run audits `stale`; a target update clears the stored result.

## 7. Metrics

`GET /admin/v1/metrics` on the admin listener (kick-off §13.2). The wrapper accepts the admin key on every route and the metrics token on this route alone:

```go
ok := strings.EqualFold(scheme, "Bearer") && (auth.Equal(token, s.keyHash) ||
	s.metricsHash != nil && r.Method == http.MethodGet && r.URL.Path == "/admin/v1/metrics" && auth.Equal(token, *s.metricsHash))
```

`serve` reads `STARGAITE_METRICS_TOKEN` once at start; a token shorter than 32 characters, starting with `sk-stargaite-`, or equal to the admin key stops `serve` with exit 1, rule SEC-3 (§15 point 17); only its SHA-256 is kept. The answer is always `200` with `Content-Type: text/plain; version=0.0.4; charset=utf-8`, whatever `Accept` asks. Each family has one `# HELP` and one `# TYPE` line before its samples, which are contiguous; HELP escapes `\` and line feed, label values escape `\`, `"` and line feed; values are `strconv.FormatFloat(v, 'g', -1, 64)`; the body ends with a line feed. A read: not audited.

| Family | Type | Labels | Source |
|---|---|---|---|
| `stargaite_requests_total` | counter | key, model, status | `requests` |
| `stargaite_tokens_total` | counter | key, model, type | `tokens` |
| `stargaite_cost_usd_total` | counter | key, model | `cost_usd` |
| `stargaite_unpriced_requests_total`, `stargaite_length_finishes_total`, `stargaite_empty_responses_total`, `stargaite_model_mismatches_total` | counter | key, model | `unpriced`, `length`, `empty`, `model_mismatch` |
| `stargaite_params_changed_total` | counter | key, model, action | `params_changed` |
| `stargaite_budget_warnings_total`, `stargaite_budget_exhausted_total`, `stargaite_quota_warnings_total` | counter | key, model | the phase-3 warning counters (LIM-3, LIM-9) |
| `stargaite_spans_emitted_total` | counter | key, model | `Deps.Spanned` |
| `stargaite_unauthenticated_requests_total` | counter | — | `unauthenticated` (new, TEL-5) |
| `stargaite_store_check_failures_total` | counter | — | `store_check_failed` (new, kick-off §14.3) |
| `stargaite_spans_sent_total`, `stargaite_spans_dropped_total` | counter | — | `Exporter.Status` |
| `stargaite_spans_rejected_total` | counter | reason (`status`, `backend`), status | `Exporter.Status` |
| `stargaite_spans_queued` | gauge | — | `Exporter.Status` |
| `stargaite_exporter_last_error_info` | gauge, value 1 | message | `Status.LastError` when not empty (TEL-3) |
| `stargaite_budget_spent_usd`, `stargaite_budget_limit_usd` | gauge | scope, name, interval | `State.Spent`, `limit_usd` of each published budget |
| `stargaite_quota_used`, `stargaite_quota_limit` | gauge | provider, key, window, unit | `State.Windows()` as held |

```go
package admin

// render writes the metrics of section 7. names maps each usage.Counter name
// to its family, type, help and label name; a counter name missing from it is
// a test failure, never a silent omission.
func render(w io.Writer, snap *config.Snapshot, counters, spanned *usage.Counters, a *admission.State, st telemetry.Status) error
```

**Feeding.** `authenticate` adds `Counter{Name: "unauthenticated"}` on every `401`, `GET /v1/models` included. `done` adds `Counter{Name: "spans", Key, Model}` with the number of spans it enqueued to `Deps.Spanned`; the shared set keeps no span count, so `TestSamplingInvariant` (AC-28) stays as it is. The store check adds `store_check_failed` (§8). Budget gauges are built from the snapshot as `budgets` does: `global`, every key and every alias with a budget, `Since` from `snap.NeverSince`; scope is `global`, `key` or `model`, name the key or alias (empty for the instance), interval the `reset_interval`. A scrape reads `Windows()` as held and never calls `Sync`, so a read neither seeds from the store nor drops a window; `serve` already syncs at start and every use keeps them.

**Cardinality.** Series exist only for the combinations seen: keys are authenticated names, the model label is `""` for an alias that does not exist (phase-3 C22), statuses, token types, actions and reasons are closed sets, budgets and windows are those declared (§15 point 16). Counters live in memory and start at zero on a restart, which Prometheus reads as a reset.

**AC-48.** `TestMetricsMatchStore` (`internal/admin`, `api` in process, a receiver for spans): a scripted run of successes, a stream, a fallback, a cut stream, a refusal at admission, a budget refusal, an unpriced call, a length finish, an empty answer, a mismatch, a `drop` rule, embeddings, rerank with and without usage, a verify and three bad tokens. Provider costs are dyadic (0.25, 0.125 through `cost_report`), so float sums are exact in any order. After `Writer.Close` returns 0 the test parses the body, checks the format rules above, and requires equality, with no tolerance, of: each per key and alias family with its `SUM` or `COUNT` over `usage` grouped by `key_name`, `model`; `requests_total` and `unauthenticated_requests_total` with the script; `spans_emitted_total` and `spans_sent_total` with the receiver; budget gauges with `usage_daily` sums; quota gauges with the window rows; the last error with `telemetry status`.

## 8. Operations

**Migrations (OPS-7, AC-50).** `migrate(ctx, db, fsys fs.FS)` takes the migrations as an unexported argument; `Open` passes the embedded files. `TestMigrations` seeds rows valid at each `user_version` from 0 to head (`testdata/seed/v1.sql`, `v2.sql`), opens the store, and requires: the schema golden; the seeded rows kept; `PRAGMA integrity_check` `ok` and `PRAGMA foreign_key_check` empty (`integrity_check` does not see foreign keys, and `PRAGMA foreign_keys` is a no-op inside a transaction); a migration injected to fail at its second statement leaves `user_version` and the schema unchanged; a newer `user_version` is refused and the file's bytes are unchanged. The existing `TestMigrationFromVersion1` and `TestReopenKeepsDataAndRefusesANewerSchema` stay. No migration 003 (§15 point 23).

Settled in C33:

- **Atomicity.** `migrate` was already atomic per migration: the migration's text and its `PRAGMA user_version` run in one transaction, rolled back on any error. No code change beyond the `fsys` argument. `TestMigrations` injects, at each version, a next migration whose first statement creates a table and whose second fails: nothing changes. Below head it also runs the real migrations on a copy with a failing one after them: the copy ends at the golden schema, `user_version` head included, with every row. So each version update is in its migration's transaction; one written right after `Commit` would need fault injection to catch.
- **Seeds.** `testdata/seed/v1.sql` and `v2.sql` put rows in every table of their version, with every foreign key, `NULL`s, a `BLOB`, `REAL`s and timestamps. The test requires a row in each table at each version, so a migration 003 cannot land without a `v3.sql` that fills its tables. Version 0 is the empty file. Rows are compared on the columns that existed at the seeded version, each value with its Go type.
- **The one expected loss.** Migration 002 drops `usage_daily` (nothing wrote it at version 1, phase-3 §5), so the row `v1.sql` puts there is the only one the test expects gone from a version 1 store; every other row is kept, and the `budget_totals` rows read `since_revision` 0.
- **Newer version.** The refused `Open` leaves the database file's bytes unchanged and no bytes in a `-wal` file. The seeded file is in WAL mode, as every binary leaves it.
- **Existing tests.** No store test asserted integrity before C33; `TestMigrationFromVersion1` and `TestReopenKeepsDataAndRefusesANewerSchema` now also require `integrity_check` `ok` and an empty `foreign_key_check`.

**Backup (ADR-018, kick-off §14.3).**

```go
package store

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
func (s *Store) Backup(ctx context.Context) (*os.File, error)

// Check is PRAGMA quick_check on the read pool: nil for "ok", else its rows.
func (s *Store) Check(ctx context.Context) ([]string, error)
```

The path is built by the server and bound as a parameter (`VACUUM INTO ?`), so it is never an operator text and never a `file:` URI, which the driver would parse (measured). The copy holds only whole transactions: a usage batch, with its `usage_daily` upserts, is one transaction, and records still in the writer's queue are absent, as ADR-016 allows. `Open` removes, under the lock, only the files whose name matches `^\.stargaite-backup-[A-Z2-7]{26}(-journal)?$` (a crash during a backup), logging each; no other file of the data directory is ever removed. `store` has one unexported test hook, `var vacuumed func(path string)`, nil in production, called after `VACUUM INTO` and before the checks. `POST /admin/v1/backup` streams the file with `Content-Type: application/vnd.sqlite3` and `Content-Length`, after clearing its read deadline and setting its write deadline to `backupWait` (15 minutes, a package variable); an error before the first byte is the JSON envelope, one after it cuts the connection. `stargaite backup <file>` refuses an existing `<file>` or one whose base name starts with `.stargaite-backup-`, writes `<file>.part` with `O_EXCL` and mode `0600` (the copy holds key hashes), checks the byte count against `Content-Length`, fsyncs, then `os.Link`s it to `<file>` and removes the part, so it never replaces a file; on any error it removes the part. A read: not audited (SEC-7); the server logs one line with the size.

**Restore (OPS-7).** A documented procedure in the README, with no command (§15 point 20): stop `serve` (it releases the lock); move `stargaite.db`, `stargaite.db-wal` and `stargaite.db-shm` aside together; copy the backup in as `stargaite.db` with mode `0600`; start `serve`, which converts the copy to WAL and runs any newer migration. Keeping a stale `-wal` beside the copy silently replays it (measured: the "restored" store held the 3,000 rows written after the backup). `TestBackupRestore` (`cmd/stargaite`, `STARGAITE_TEST_RUN`): traffic committing, a backup through the CLI, more traffic, `SIGKILL` (leaving a `-wal`), the procedure, `serve`; then records equal the backup's, `usage_daily` equals `GROUP BY usage`, `integrity_check` is `ok`, budgets equal a fresh `Restore`, an old key token still authenticates, and no row written after the backup exists. Its negative control keeps the `-wal` and finds the newer rows.

Settled in C34:

- **Open's logger.** `store` had no logger, and `Open` must log each leftover it removes, so it is `Open(ctx, dataDir, log *slog.Logger)`; `serve` passes its JSON logger, tests a discarding one. The line is `WARN` `leftover backup copy removed` with the path. Only a regular file whose whole name matches is removed; one that cannot be removed fails `Open`, so a copy holding key hashes never stays unnoticed.
- **The copy.** Each `Backup` opens a `sql.DB` of its own on the store's DSN (its pragmas, the 5 s busy timeout) and closes it after `VACUUM INTO ?`; the check opens the copy with `mode=ro`. Measured: the copy is in rollback-journal mode (header bytes 18 and 19 are 1); SQLite keeps `<copy>-journal` while `VACUUM INTO` runs and deletes it at the end, so a crash can leave it, and `Open`'s pattern takes an optional `-journal` (C34 review); the check leaves no sidecar file. `TestBackup` also fails a copy on `foreign_key_check`, on `user_version` and on its context ending in the check, each leaving no file.
- **Route errors.** Every error before the first byte (`ErrClosed`, a stop, a failed check, a deadline that cannot be set) is `500 internal_error` through the existing `fail`, its cause in the log under the request id; deadlines are set before the copy is made, so a copy is never streamed under the 60 s write timeout. The route takes no query parameter (`400`) and does not read a body, as `telemetry test`. A stop during the stream is a `context.AfterFunc` on the request context that moves the write deadline to now (`TestBackupRouteStop`). The log lines are `INFO` `backup sent` with `bytes`, and `ERROR` `backup cut` with `bytes`, `size` and the error.
- **CLI.** The command table gains `save`: a row with it is sent through `untimed` (the client without its timeout, same transport) and takes a `2xx` body itself; `local` does the checks before the request. A `<file>` that exists, whose existence cannot be told, or whose base name is reserved is a wrong usage (exit `2`, stderr, nothing sent), as every refusal decided before the call is. A `.part` already there is not the CLI's: exit `1`, left untouched. An answer that is not `application/vnd.sqlite3` with a `Content-Length` is exit `1` with nothing written. After the link the directory is synced, so the name survives a crash. During a `save` call `SIGINT` and `SIGTERM` cancel the request (`signal.NotifyContext`), so an interrupted transfer is an error and the part is removed (C34 review). Output: `<file>: <n> bytes`, or `{"path","bytes"}` with `--json`; a failure is the `internal_error` envelope with `--json`.
- **Negative control.** The stale `-wal` is replayed, but the measured outcome depends on page layout: `VACUUM INTO` writes the indexes after the tables, so frames meant for the live file land on other pages, and in `TestBackupRestore` the store fails `integrity_check` instead of showing the newer rows. The control therefore requires that the store with the stale `-wal` is not the backup: it fails `integrity_check` or holds the 7 records. The README says a kept `-wal` brings back rows or corrupts the store.
- **Restore checks.** Records, `usage_daily`, `budget_totals` and `key_secrets` equal the backup's; budgets: `usage.Restore` over the restored store and over the backup file give equal `Spent` per scope and period (0.12 USD each).
- **SIGKILL mid-stream.** In `TestServeKilled` a loopback proxy between the CLI and `serve` passes the headers and 4 KiB at once and holds the rest; a 24 MiB filler table makes the copy larger than the socket buffers, so `serve` is still writing when killed; the CLI's `.part` appearing marks that it has the headers.
- **Quick start.** Row 12 extends the quick start per command: `./stargaite backup backup.db`, which `tests/e2e` writes under a temporary directory and opens read-only (`integrity_check` `ok`, no foreign key violation, the one record, mode `0600`). `/backup.db*` is in `.gitignore` and `.dockerignore`, since the copy holds key hashes.

**Retention (USE-11, ADR-017, AC-49).**

```go
package admission

// SeedSpan is how far back a seed reads for a window of span w: w plus one
// bucket (phase-3 C28). hold uses it; retention keeps that span.
func SeedSpan(w time.Duration) time.Duration

package usage

// Cutoffs are the retention bounds at one instant: rows before them go.
type Cutoffs struct {
	Usage time.Time // usage.completed_at: now − max(usage_days, the longest SeedSpan of any declared window)
	Day   string    // usage_daily.day: the earlier of now − usage_daily_months and the UTC day of Usage
	Audit time.Time // audit.created_at: now − audit_months
}
func CutoffsAt(now time.Time, r domain.Retention, providers map[string]domain.Provider) Cutoffs
type Deleted struct{ Usage, Daily, Audit int64 }

// Retain deletes, per table, batches of at most 500 rows before c, each one
// write job, until a batch is short. budget_totals, revisions and
// configuration rows are never touched.
func Retain(ctx context.Context, write func(context.Context, func(*sql.Tx) error) error, c Cutoffs) (Deleted, error)
```

Each batch is `DELETE FROM usage WHERE rowid IN (SELECT rowid FROM usage WHERE completed_at < ? ORDER BY completed_at LIMIT 500)` (a covering search of `idx_usage_time`, measured), the same by `day` on `usage_daily` (its primary key starts with `day`) and by `created_at` on `audit`, in rowid order, which is insertion order. Usage batches interleave with retention jobs on the single writer queue, so the writer is held for one batch at most. The usage cutoff keeps every record the quota seed reads (USE-11, phase-3 §17 point 12); the daily cutoff keeps every aggregate whose records are kept (USE-2) and always the current month, which budget `Restore` reads. A declared value of 0 is the default (phase 3). A forward wall-clock step of D deletes up to D early, the bound quota windows already accept. No `VACUUM` and `auto_vacuum` stays NONE: freed pages go to the freelist and are reused, so the file plateaus; checkpoints stay the automatic PASSIVE ones, since a `TRUNCATE` on the writer waits up to the 5 s busy timeout while any read is open (measured). The `quota_beyond_retention` warning keeps its rule (LIM-8) and changes its text to "records of this window are kept beyond retention.usage_days" (§15 point 24).

`runServers`' new `ready` callback starts two goroutines and returns at once: retention and the store check, both on `bg, stopBg := context.WithCancel(base)`, counted by a `sync.WaitGroup` named `background`. Retention runs `Retain` after `retainFirst` (1 minute), then every `retainEvery` (24 h), both package variables, with cutoffs computed once per run from the published snapshot on the service clock; it logs one line per run with the counts, and an error line on failure, retried at the next run.

`TestRetentionPlateau` (`internal/usage`, a real store, the test clock): 500 records per simulated day in 100-record batches, 120 days, `usage_days: 10`, a `7d` window; `Retain` once per day. Checked: usage rows never exceed 11 days of traffic (the `7d` seed span is shorter than `usage_days`); the maximum `PRAGMA page_count` over days 80 to 120 is at most 2 % above its maximum over days 40 to 80 (measured plateau: a tail of 7 pages over 100 cycles), and the `-wal` stays under 8 MiB; seeded quota windows and `Restore` budgets equal before and after each run; `budget_totals` is unchanged. The run also declares `usage_daily_months: 1` and `audit_months: 1` and inserts one audit row per simulated day through the store's `Write`, as the existing `usage` tests use the store. Checked: no `usage_daily` row is older than the `Day` cutoff and the current month is always present; no audit row is older than 1 month plus one day; the plateau bound covers all three tables. `TestServeRetention` (`cmd/stargaite`): a 200-day-old row, a 25-month-old audit row and a 37-month-old `usage_daily` row are gone after the lowered first run, a `usage_daily` row of the current month survives, a `7d` window's rows survive `usage_days: 1`, and the goroutine has ended before the store closes.

**Store check (kick-off §14.3, ADR-018).** The store check goroutine runs `st.Check(bg)` once. A failure is one `CRITICAL` log line with the rows and `store_check_failed` in the counters; the proxy keeps serving.

**Secrets reload (OPS-6, AC-44).** `POST /admin/v1/secrets/reload` is `Commit(Change{Summary: "secrets reload", Kind: "secrets", Mutate: noop})`. Commit's step 5 changes in one way: a change whose diff is empty is never refused for error findings; it is published with them, because it changes nothing that validation judges, and a stored configuration a newer binary finds invalid is still published (kick-off §13.1). The env file is read again, every credential and `Snapshot.Exporter` re-resolved, the snapshot republished at the same revision, an `unchanged` audit row written (phase-1 §13 point 8 already does all this on every accepted write). It answers `Result`, whose findings name each unresolved credential by reference only. Requests in flight keep their snapshot (RES-7); the exporter reads the new one at its next batch. No signal triggers it (§15 point 25). `TestSecretsReload` (`cmd/stargaite`): a provider whose variable is absent answers `503 no_target_available`; a new env file is renamed into place; after `secrets reload` the mock receives the new `Authorization` and a telemetry header changes at the next batch; the planted value is absent from logs, audit rows and responses; a stored configuration with an error finding (written through the store, as an upgrade would leave it) still reloads.

**Stop order (RES-6)**, phase-3 §11 extended:

1. `runServers`: `Shutdown`, `shutdownGrace` for both listeners.
2. `halt(api.ErrStopping)`: also interrupts a verify call and a backup.
3. `Flight.Wait` against `drainWait`; verify runs are inside `Flight`.
4. `stopBg()`; `background.Wait()`: retention returns between batches and the quick_check is interrupted through its context.
5. `records.Close` within `flushWait`.
6. `st.Close()`, which waits for a `Backup` in progress.
7. `spans.Close` within `exportWait`; the `stopped` line.

## 9. Hardening

**Adversarial suite (AC-51).** `tests/adversarial` runs `api.Handler` and `admin.Handler` in process behind `httptest`, with the mock as provider and a resolver for the SSRF cases; request cases run on every endpoint they apply to. Cases that depend on the server options in `serve.go` run in `cmd/stargaite` (`TestServeAdversarial`) on raw sockets at lowered timeouts. Every row of the kick-off §21 adversarial list is a row here, not a citation of an earlier test.

| Case | Expected | Rule |
|---|---|---|
| Malformed JSON, invalid UTF-8, a non-object body, 10,001 levels of nesting within `max_body_bytes` | `400`, no provider call, process alive | PAR-1 (nesting: §15 point 26) |
| A repeated key and an escaped repeat (`"model"`, `"model"`), top level and walked objects | `400 invalid_request` | PAR-1, AC-08 |
| A body one byte over `max_body_bytes`, with and without `Content-Length`, chunked | `413` before the full read; slots back to zero | LIM-6 |
| A revoked key; the wrong secret of a real public id; malformed Bearer forms; the admin key on the data plane; a client key and the metrics token on every other admin route | `401` within 1 s of the revoke | SEC-1, SEC-2, SEC-3, SEC-8 |
| An unknown alias; random unknown aliases | `404`; `Counters` does not grow; the alias cut to 200 characters in logs | OPS-8, OPS-9 |
| An incompatible target: `tools` toward `tools: false`, embeddings to a chat alias, rerank without `rerank_url` | `422` with zero mock calls | PAR-4, PAR-12, kick-off §7.1 |
| SSE split into one-byte chunks; think tags split, unclosed and JSON-escaped | events intact; tags removed | RES-4, RSN-12 |
| A stream cut midway | one terminal error event; one record | RES-4, USE-1 |
| `base_url` and `rerank_url` at `169.254.169.254`, `0x7f.1`, `2130706433`, `017700000001`, `[::ffff:127.0.0.1]`, a NAT64 form, a zoned address, userinfo; a mock `302`; a name that resolves private after validation | refused at set time or at dial; `502`, no second dial | SEC-4, AC-43 |
| 50 concurrent requests at an exhausted budget; at a 5-request window | overshoot within LIM-4's bound; exactly 5 provider calls | LIM-4, LIM-8, AC-37 |
| An upstream answer over `max_response_bytes`; a gzip bomb | `502`, memory bounded | RES-4 |
| A provider error echoing the credential | absent from response, log, span and record | SEC-5, TEL-8 |
| CR, LF and control bytes in the alias, `traceparent` and `x-session-id` | one escaped JSON log line; headers ignored | OPS-9, TEL-6 |
| A flood of bad tokens | log rate-limited; `unauthenticated_requests_total` exact | TEL-5, OPS-9 |
| `TestServeAdversarial`: `Content-Length` with `Transfer-Encoding: chunked`; two differing `Content-Length` values; a bare LF in a chunk size | chunked wins and `Content-Length` is removed (net/http's choice), and a second pipelined request on the same connection is parsed as its own request (no desync); differing lengths give `400` and the connection closed; the bare LF gives `400 invalid_request` and the connection closed | SEC-6 (as amended, §15 point 26) |
| `TestServeAdversarial`: over 68 KiB of headers (64 KiB `MaxHeaderBytes` plus net/http's 4,096-byte slack); 501 header values | `431` | SEC-6 |
| `TestServeAdversarial`: slow headers; a stream reader that stops reading; a client that disconnects | closed within `serverReadHeaderTimeout` + 1 s; `client_gone`, slot released, mock cancelled within 1 s | SEC-6, LIM-6, RES-5 |

Key comparison rests on `crypto/subtle.ConstantTimeCompare` over two fixed 32-byte digests (SEC-1), checked by review, not by a timing test; HTTP/2 frame abuse is net/http's, checked by `govulncheck` on the pinned toolchain.

**Fuzzing.** New targets, one per parser of untrusted bytes that has none, each with committed seeds under `testdata/fuzz`: `FuzzEventReader` (`transport`, upstream SSE framing: never more than its bound), `FuzzCheckURL` (`transport`: nothing accepted classifies private, link-local or metadata), `FuzzParse` (`telemetry`, `traceparent`), `FuzzToken` (`auth.Parse`), `FuzzEnvFile` (`config`: findings never hold a line's content), `FuzzObserveUsage` (`openai`), `FuzzLabel` (`admin`: an escaped label value parses back to itself). `make ci` replays every seed (plain `go test` runs them). `make fuzz FUZZTIME=60s` runs `go test -run '^$' -fuzz '^Name$' -fuzztime $(FUZZTIME)` once per (package, target) pair, the anchors needed because `-fuzz` is unanchored and `anthropic` holds both `FuzzStream` and `FuzzChatStream`; a finding becomes a seed. YAML `apply` is not fuzzed here: the dependency fuzzes itself.

**Benchmark (AC-52, kick-off §18).** `tests/benchmark.TestBenchmark`, skipped unless `STARGAITE_BENCH=1`, Linux only (RSS from `/proc`). `make bench` builds the binary and the mock into `$TMPDIR` and runs it; the harness starts both as processes on loopback, the proxy with `GOMAXPROCS=1` (kick-off §17's 1000m limit), pinned with `taskset` when present.

- Load is open loop: each request has an intended send time at the fixed rate, and latency is measured from it, so a stall is not hidden (no coordinated omission).
- Added latency P50 and P99: the same rate and payload straight to the mock and through the proxy, ten alternating runs, the median of the per-run difference of percentiles; for a sync chat call, a native stream, a translated stream and an inline-think stream.
- Throughput: 1,000 requests per second without streaming for 30 s, no error.
- RSS at rest, with 100 and with 500 open streams; start-up from exec to the `ready` line; cancellation from client close to the mock's `Ended`; `BenchmarkCommitScale18` (configuration change); the image size when `docker` is present; RTO: backup, restore procedure and start to ready on a store of kick-off §18 scale.
- Output: one JSON document and a table. The harness exits 1 for every missed target that `docs/benchmark.md` does not list as an accepted deviation (figure, reason, date). `docs/benchmark.md` also records the host, CPU, Go version and commit. Availability (99.9 %) is not measurable here and is stated so (§15 point 28).

## 10. Admin API and CLI

| Route | CLI | Notes |
|---|---|---|
| `POST /admin/v1/verify/{id...}` `{"max_output_tokens": n}` | `targets verify <id> [--max-output-tokens n]` | `Verification` plus `stored`; exit 1 unless `ok`; audited, no revision |
| `GET /admin/v1/metrics` | — (a scraper or `curl`) | Admin key or metrics token; text 0.0.4; not audited |
| `POST /admin/v1/backup` | `backup <file>` | Streams the checked copy; the CLI writes it locally; not audited |
| `POST /admin/v1/secrets/reload` | `secrets reload` | `Result`; an `unchanged` audit row, as every commit without a diff |
| `GET /admin/v1/targets`, `GET /admin/v1/targets/{id...}` | `targets list`, `targets show` | Add `verified_at`, `verified_version` |

`targets verify` escapes the whole id, as the target routes do. `targets verify` and `backup` are sent without the CLI's 90 s client timeout: the server bounds each by its write deadline (`k × (total_ms + 5 s)` for k calls; `backupWait`). Without `--json`, `targets verify` prints one block per call (effort, sent, status, reasoning and output tokens, the body) then `verified` or `failed`; `backup` prints the path and size; `secrets reload` the generic renderer. `README.md` gains: the embeddings alias and call, `targets verify mock/mock`, a metrics `curl` with the admin key, `backup`, the restore procedure, a Prometheus scrape example, writing the env file by rename, and the R9 price shapes (phase-3 §17 point 11).

## 11. Testing and CI

**Mock provider.** `POST /rerank` (§5). Per-level reasoning tokens, base64 vectors, total-only usage and bare arrays are scripted bodies; the mock already records each call's exact bytes.

**Clocks.** `Config.Now` drives retention cutoffs, verify dates and the budget gauges; `retainFirst`, `retainEvery`, `backupWait` and the phase 3 intervals are lowered in tests; no test asserts a wall-clock latency except an upper bound.

| Layer | What it covers |
|---|---|
| Unit, table-driven | `passthrough` per kind, protocol and `rerank_url`; `rules` shared with chat (the chat goldens unchanged); `ObserveUsage` on OpenAI, Mistral, Voyage, Jina, vLLM, OpenRouter and Cohere shapes, base64 and float; `NewRecord` per endpoint (no `content_empty`; a rerank 2xx without usage estimated and priced by the table); `CutoffsAt` with and without windows; `render` format rules; the metrics token rule; `Unverified`; `endpoint_unservable` |
| `internal/api` | `TestEmbeddings`, `TestRerank`: records, counters, span golden cases `embeddings` and `rerank` in `testdata/spans.json`, `server.address` of a `rerank_url` on another host, fallback, `422`, PAR-2 |
| `internal/admin` | `TestVerify`, `TestMetricsMatchStore`, `TestEveryRouteNeedsTheKey` extended with the metrics token; `TestVerify` and the backup route test run on an `httptest.Server` whose `Config.WriteTimeout` is 200 ms, with a mock `Delay` (verify) or a blocking `Backup` function value (backup) longer than that, and the answer completes |
| `internal/config` | The expected findings of `TestCommitRows` (`service_test.go:192`), `TestCredentialResolution` (`:619`, `:627`, `:635`) and any refusal test that counts `e.Findings` with a chat target in its fixture gain `warning RSN-9 target_unverified targets/p/m`; each list is extended by that finding and nothing else, and no assertion is relaxed |
| `internal/store` | `TestMigrations`; `TestBackup`: a copy under concurrent writes holds whole transactions (`usage_daily` equals `GROUP BY usage`); `vacuumed` overwrites page 2 of the copy, and `Backup` returns an error and leaves no file; `vacuumed` blocks on a channel while `Close` is called from another goroutine, and `Close` has not returned until the channel is released; a `.stargaite-backup-<26 base32>` file planted before `Open` is gone after it, and a `backup-x.db` beside it survives |
| `internal/usage` | `TestRetentionPlateau`; retention against the writer under `-race` |
| Restart and crash (`cmd/stargaite`) | `TestBackupRestore`; `TestServeKilled` gains a `SIGKILL` of `serve` once the CLI has received the backup's response headers: no file at the CLI's final name, no `.part` left, `integrity_check` `ok` at the next start; `TestServeRetention`; `TestSecretsReload`; `TestServeAdversarial` |
| Race (`-race`) | Verify alongside traffic; a reload alongside open streams (RES-7); backup and retention alongside `TestLedger`'s traffic |
| Imports | The rule of §3 |
| End to end | `tests/e2e` runs the extended quick start: an embeddings target and alias on the mock, the key allowed both aliases, one `/v1/embeddings` call, `targets verify mock/mock` (`ok`), a metrics scrape holding `stargaite_requests_total{key="my-agent",model="fast",status="200"} 1`, `backup` to a file that opens with `integrity_check` `ok`; plus, outside the README, a rerank alias on the mock's `/rerank` |

**Outside the default CI (PUB-5, TEL-14 precedent).** Each is a make target or a test skipped without its variables, run by maintainers with their own keys and nothing private:

- `make fuzz FUZZTIME=60s`.
- `make bench` (`STARGAITE_BENCH=1`, Linux), then `docs/benchmark.md` updated and reviewed.
- `tests/contract` gains `TestEmbeddings` (float and base64, total-only usage) and `TestRerank`, against `STARGAITE_CONTRACT_BASE_URL` with an embeddings and a rerank model given by variables, and a live `targets verify` of the chat target under test.
- `tests/langfuse` as phase 3, run before a release that changes the span schema: phase 4 adds the `embeddings` and `rerank` cases.

## 12. Ordered changes

Each row is one pull request that leaves `make ci` green; each extends the quick start where it adds a command. The order retires the data risks first.

| Id | What it adds | Tests | Makes checkable | Depends on |
|---|---|---|---|---|
| C33 | `migrate(ctx, db, fsys)`; seed fixtures per version; `foreign_key_check` in the store tests | `TestMigrations` (golden, rows, FK, failing migration, newer version) | AC-50 (migrations) | phase 3 |
| C34 | `Store.Backup`, reserved-name leftovers at `Open`, `Close` waits, the `vacuumed` hook; `POST /admin/v1/backup`; `recorder.Unwrap`; `backup <file>`; `admin.Deps.Backup`; README: backup and restore | `TestBackup`, `TestBackupRestore`, `SIGKILL` mid-stream, the 200 ms write-timeout case | AC-50 | C33 |
| C35 | `admission.SeedSpan`; `usage.CutoffsAt`, `Retain`; the `ready` callback, the retention goroutine and `Store.Check`; the stop order; `store_check_failed`; `serve` hoists the data plane's `usage.Counters` into a variable shared with the store check; the `quota_beyond_retention` text | `TestRetentionPlateau`, `TestServeRetention`, a corrupt page logs CRITICAL | AC-49 | C33 |
| C36 | `POST /admin/v1/secrets/reload`, `secrets reload`; `Commit` never refuses an empty diff for error findings; README: env file written by rename | `TestSecretsReload` | AC-44 | phase 3 |
| C37 | `EndpointEmbeddings`; `params.Embeddings`; `passthrough` and `rules`; `ObserveUsage`; `NewRecord` rules; `operation(ep)`; `server.address` from `out.URL`; content off; `endpoint_unservable`; PAR-7, RSN-5, RSN-7 for chat targets; `go 1.27.4`; README: embeddings | Plan goldens, PAR-2, `422`, fallback, records, span golden, e2e | AC-46 (embeddings) | phase 3 |
| C38 | `EndpointRerank`; the rerank branch; non-object answers; mock `/rerank` | Passthrough bytes, top-level repeat, TEI array, the estimate priced by the table, e2e | AC-46 | C37 |
| C39 | `domain.VerifyKey`; `api.Verify`; nil-safe `scopeNames` and `budgets`; `config.State.Verified`, `Snapshot.Verified`, `Service.Verified`, `Verification`, `Unverified`; columns cleared; the route, `targets verify`, long requests; `targets show` fields; README: `targets verify` | `TestVerify`; shipped tests that list exact findings gain `target_unverified` (§11) | AC-47 | C37 |
| C40 | The unauthenticated counter; `Deps.Spanned`; `render`; the metrics token; `admin.Deps.Counters` and `Spanned` wired from the variable C35 hoisted, and the other `admin.Deps` fields; README: scrape | `TestMetricsMatchStore`, `FuzzLabel`, the token cases | AC-48; TEL-3 | C35, C38, C39 |
| C41 | `tests/adversarial`; `TestServeAdversarial`; `serverReadHeaderTimeout`; the fuzz targets and seeds; `make fuzz` | The suite | AC-51 | C40 |
| C42 | `tests/benchmark`, `make bench`, `docs/benchmark.md`; README: container resources from the figures, R9 price shapes, a release checklist (§13) | Run by maintainers | AC-52 | C41 |

## 13. v1 closure

Every criterion of kick-off §21 and requirement of kick-off §19 that v1 needs and that phases 1 to 3 did not make checkable or deliver. PUB-1, PUB-4, PUB-6 and PUB-7 are delivered (phase 1); AC-03 to AC-45 except AC-30 and the second half of AC-44 are checkable in `make ci` today.

| Item | What is missing after phase 3 | Closed by |
|---|---|---|
| AC-44 | `secrets reload` | C36 |
| AC-46 | Both endpoints | C37, C38 |
| AC-47; RSN-9; R3 | The live check, its stored result, `_verify` records | C39 |
| OPS-5 warnings | "chat targets never verified"; "aliases that cannot serve an endpoint" | C39; C37 |
| AC-48; OPS-8 | The endpoint; spans emitted by key and alias | C40 |
| TEL-3 | Counters and last error on the metrics endpoint | C40 |
| TEL-5 | The unauthenticated metric (phase-3 §4 promised a counter) | C40 |
| SEC-3 | The metrics token | C40 (rule: §15 point 17) |
| LIM-3, LIM-9, USE-12 | "raises a metric": counted since phase 3, rendered now | C40 |
| AC-49; USE-11; ADR-017 | Retention, audit included | C35 |
| kick-off §14.3 store check; ADR-018 | `quick_check` after ready; the checked backup | C35; C34 |
| AC-50; OPS-7 | Every-version migration test; backup and a tested restore | C33, C34 (procedure: §15 point 20) |
| AC-51 | The suite | C41 |
| AC-52; kick-off §18 | The benchmark, RTO included | C42; availability: §15 point 28 |
| kick-off §17 container resources | "to be adjusted with the benchmark" | C42 |
| R9 guide sections | The price shapes one table cannot express (phase-3 §17 point 11) | C42 |
| AC-01, AC-02 | CI replays recorded SDK bytes (phase 2); the run with unmodified SDKs against the mock | Release checklist, maintainers (§15 point 30) |
| AC-30 | The TEL-14 job exists (C32); run before a release that changes spans | Release checklist |
| AC-53 | The golden exists; the live run of the first ten presets; the kick-off §7.2 notes against today's documentation | Release checklist; §15 point 5 |
| PUB-2 | Search of every file and commit message before publication | Release checklist |
| PUB-3 | `tests/e2e` runs the quick start in CI; a clean machine run | e2e (C37 extends it); release checklist |
| PUB-5 | `make ci` passes on a fork with no secret | Release checklist |
| PUB-8 | English review of the repository | Release checklist |
| OQ-4 | Release binaries and the image registry: no phase designs them | §15 point 30 |

The release checklist is a README section listing those maintainer runs, each with the make target or test that performs it.

## 14. Risks of phase 4 and how each step retires them

| Risk | Sign | Retired by |
|---|---|---|
| A migration corrupts or half-applies on an upgrade | A store that opens with a different schema or loses rows (OPS-7) | C33: every version with data, FK check, injected failure |
| A backup that cannot be restored, or a restore that replays a stale WAL | Restored figures differ from the backup's | C34: checks before streaming, `TestBackupRestore` with its negative control |
| Retention deletes what a restart needs | A quota or budget figure changes across a restart | C35: cutoffs from `SeedSpan` and the current month; plateau test compares seeds and `Restore` before and after |
| Retention or a backup stalls the writer | Usage batch latency rises; `Available` flips | C35: one batch per job; C34: its own connection, measured 146 ms for 22 MB under writes |
| The store grows without bound (USE-11, ADR-017, AC-49) | `page_count` keeps rising | C35: plateau test; no `VACUUM`, PASSIVE checkpoints |
| A long read starves checkpoints | `-wal` grows during `quick_check` or a big `usage show` | Not retired: one check at start, bounded by the store size; documented |
| A new endpoint skips a protection chat has | A limit, SSRF check or record missing on embeddings or rerank | C37, C38: the same `infer`; the adversarial rows per endpoint (C41) |
| Embeddings answers exceed `max_response_bytes` | `502` on a valid large batch | Not retired by code: §15 point 8, documented |
| Verify opens a breaker or charges a client's budget | Live traffic refused after a verify | C39: fresh breakers, global scope only, `TestVerify` checks key budgets unmoved |
| Verify records land after the store closes | Critical log lines at shutdown | C39: verify inside `Flight`, held for the whole run |
| Metrics drift from the store | AC-48 fails | C40: one feeding point per counter, exact equality with dyadic costs |
| Metrics cardinality explodes | Scrapes grow with traffic | C40: configured names and closed sets only; unknown aliases under `""` |
| A metrics token opens more than metrics | An admin route answers it | C40: one-route check in the wrapper; `TestEveryRouteNeedsTheKey` |
| Request smuggling or an HTTP/2 flaw in net/http | A desync or crash advisory | C37: `go 1.27.4`; `govulncheck` in `make ci`; C41 smuggling rows |
| Kick-off §18 targets missed | Benchmark misses | C42: measured, each miss listed as a deviation (ASM-4) |

## 15. Open points for the owner

Each has a recommendation; the design above already assumes it. The owner accepted the recommendations of points 1 to 33 on 2026-11-15, and the kick-off records those that change it.

**Embeddings and rerank against the current documentation**

1. **USE-3 off chat.** Voyage, Jina (some models), vLLM rerank and Alibaba rerank report only `usage.total_tokens`; the openai rule (`output = max(completion, total − prompt)`) would book all of it as output and the input as 0. Recommendation: for embeddings and rerank, input is `prompt_tokens`, else `total_tokens`, output 0 (derived); amend USE-3.
2. **Rerank billing units (USE-3, USE-6, annex A).** Cohere bills search units (one query with up to 100 documents, longer documents chunked) and OpenRouter reports `usage.search_units` with its `cost`; annex A has no column and prices are per million tokens. Recommendation: tokens only in v1; a search-unit target is priced by `cost_report`, or by its token prices over the reported or estimated tokens (point 4), which only approximate the invoice; one with neither is unpriced, which USE-12 refuses under a budget; document it.
3. **Rerank shapes (kick-off §7.1, ASM-9).** TEI takes `texts`, ignores `model`, returns a bare array and reports tokens only in `x-compute-tokens`; DeepInfra puts the model in the URL path and takes `{queries, documents}`; Voyage and Fireworks answer `data[]`. Recommendation: relay as §5 does (a non-object answer is estimated, no header read); DeepInfra rerank is outside v1, since its body is not the common shape; no per-target "drop model" flag, since the surveyed services accept or ignore `model`; amend kick-off §7.1 and ASM-9.
4. **Rerank without reported usage (kick-off §7.1, USE-6, USE-8).** Kick-off §7.1 meters rerank as "one request, plus tokens when the response reports them". Read literally, a priced `pay_per_use` rerank target whose provider reports no `usage` object (Cohere's `meta`, DeepInfra's top-level `input_tokens`, TEI's bare array) records 0 tokens and no cost and escapes every budget, since USE-12 refuses only a target with neither prices nor a `cost_report`. Recommendation: keep the shipped USE-8 estimate for rerank, priced by the table (§5); amend kick-off §7.1 to "plus tokens when the response reports them, else the USE-8 estimate".
5. **Kick-off §7.2 notes are outdated.** OpenRouter now serves `/embeddings` and `/rerank` with `usage.cost`; Together has no serverless embedding model and runs rerank on dedicated endpoints only; DashScope documents workspace hosts and an OpenAI-style rerank at `compatible-api/v1/reranks`, not under `base_url`; DeepInfra embeddings are float only and its rerank is at `/v1/inference/{model}`; Fireworks' embeddings schema refuses unknown fields and lists no `encoding_format`; Gemini's compatibility layer documents only `input` and `model`; Mistral uses `output_dimension` and accepts no token arrays; Ollama accepts no token arrays and has no rerank. Recommendation: update the notes before the AC-53 golden freezes; no code.
6. **SDK default `encoding_format: base64`.** The OpenAI Python and Node SDKs send it when the caller sets none; DeepInfra documents float only and Fireworks likely refuses it. Recommendation: data, not code: such a target declares `unsupported_params: [encoding_format]` and its aliases a `drop` rule (listed, PAR-3); whether each SDK accepts a float answer to its implicit base64 request is unverified, and `tests/contract` checks it.
7. **The PAR-1 pin for embeddings.** Recommendation: `input`, `model`, `dimensions`, `encoding_format`, `user` as of 2026-11-15; provider fields (`input_type`, `task`, `output_dimension`, `truncate_prompt_tokens`) pass only by `forward` rules (PAR-2), as R7 intends.
8. **Annex B `max_response_bytes` (32 MiB).** One valid request of 2,048 inputs at 3,072 dimensions returns 33,554,432 bytes of base64 vectors alone, and several times more as floats, so it gets `502`. Recommendation: keep the default; the guide says when to raise it; the `502` message already names the limit.
9. **Annex D against the pinned embeddings span.** At commit `e6658f1…` the embeddings span recommends `gen_ai.embeddings.dimension.count` and `gen_ai.request.encoding_formats`, which annex D lacks, and defines no output-token attribute, while annex D emits `gen_ai.usage.output_tokens`; no `rerank` value exists (OpenTelemetry semantic-conventions issue #253 proposes one reusing `gen_ai.retrieval.client`). Recommendation: schema v1 unchanged but for the operation name, output tokens emitted as the recorded 0; the two attributes wait for a v2. Phase-3 §17 point 1's "schema URL of TODO" is outdated: the pinned commit, still the repository's head, declares `…/gen-ai-dev/1.42.0-dev`, which answers 404, and core is v1.45.0; the conclusion (no `schemaUrl`) holds.
10. **Content capture off chat (TEL-8).** TEL-8 enables content per key without an endpoint exception; the shipped capture reads the outgoing `messages` member and the relayed bytes, which on embeddings would export vectors as `gen_ai.output.messages`. Recommendation: no content on embeddings and rerank; amend TEL-8 to "chat and messages".
11. **Rerank `stream`.** Kick-off §7.1 is silent; a rerank body with `"stream": true` is forwarded and its answer read whole. Recommendation: accept; no surveyed rerank API streams.
12. **Reasoning and cap rules off chat (RSN-5, RSN-7, PAR-7).** A non-chat target created with its provider's `target_defaults` carries a dialect, and PAR-7 refuses an alias with an `anthropic` target of any kind; `passthrough` writes neither effort nor cap. Recommendation: those three checks read chat targets only; an `anthropic` embeddings target gets `endpoint_unservable` instead.

**Targets verify**

13. **RSN-9 silences.** RSN-9 does not say whether verify shares the circuit, emits spans, how `on` is sent, whether a failure or a stale run is stored, what clears a result, or the prompt. Recommendation: fresh breakers per call; one span per call keyed `_verify` when an exporter is published; `on` as `medium`; store only an all-2xx run on an unchanged target and provider, audit every run; a change of the target or its provider clears it; the prompt of §6. Phase-3 §13 point 2 omits `Spend` and `Ended`, which verify gets through `finish`. Phase-1 §9's `Model: nil` for verify becomes a Model carrying only the operator's cap, so RSN-9's "only output cap it sends" is written under the target's `output_cap_param`; the record's `max_output_alias` then holds the operator's value although no alias exists.

**Metrics**

14. **Format (OPS-8, kick-off §13.2).** "Prometheus text format" names no version; Prometheus 3 fails a scrape without a valid `Content-Type`; OpenMetrics 1.0 needs `# EOF` and other TYPE naming; OpenMetrics 2.0 is a release candidate. Recommendation: exactly `text/plain; version=0.0.4; charset=utf-8`, no negotiation, no OTLP metrics (TEL-1 allows one exporter).
15. **Exporter last error as a label (TEL-3).** Recommendation: one `_info` series whose label is `telemetry status`'s redacted, 200-byte `last_error`: one series at a time, which TEL-3 asks for.
16. **Cardinality.** Key × alias × status can pass Prometheus's guidance at kick-off §18 scale (10,000 keys, 1,000 aliases). Recommendation: only combinations seen, names bounded by the published configuration; document the bound; no aggregation switch.
17. **The metrics token rule (SEC-3).** SEC-3 gives none. Recommendation: the admin key's rule (at least 32 characters, not starting with `sk-stargaite-`) and different from the admin key, or `serve` refuses to start.

**Operations**

18. **Which connection runs `VACUUM INTO` (ADR-018).** Unstated. Recommendation: a connection of its own; the read pool refuses it and the single writer would stall every batch for its duration.
19. **ADR-018 and foreign keys.** `integrity_check` does not find foreign-key violations. Recommendation: `foreign_key_check` on every backup and in the migration tests; amend ADR-018.
20. **Restore (OPS-7, kick-off §13.3).** No restore command exists, and kick-off §13.3 allows three serverless commands. Recommendation: the documented procedure of §8, tested with its stale-WAL control; amend OPS-7 to "`backup` writes a copy that a test restores by the documented procedure". The alternative is a fourth local command, `stargaite restore <file> --yes`, taking the lock.
21. **Who writes the backup file (kick-off §13.3, SEC-7).** Recommendation: the server streams a server-named copy and the CLI writes `<file>` locally (never replacing one, mode `0600`, since it holds key hashes); a backup is a read and is not audited.
22. **AC-49 wording and `auto_vacuum`.** The kick-off is silent on `auto_vacuum`; under NONE the file plateaus but never shrinks, and switching later needs a full `VACUUM` (twice the disk, the writer held). Recommendation: keep NONE, no `VACUUM`; read "stops growing" as `page_count` plateauing within 2 % with the `-wal` bounded.
23. **USE-11 "small batches" and the audit index.** `DELETE … LIMIT` is not compiled in; annex A has no index on `audit.created_at` (a scan, measured). Recommendation: `rowid IN (SELECT … LIMIT 500)` batches; no migration 003, since audit rows are configuration changes and warnings, and rowid order is insertion order, so only the last batch of a run scans the table; add the index if the benchmark ever shows the batch holding the writer.
24. **USE-11 against LIM-8.** Retention now keeps the longest quota window, so the `quota_beyond_retention` message ("its count after a restart can only read the records kept") describes a loss that no longer happens. Recommendation: keep the warning LIM-8 requires, with the text of §8.
25. **Reload trigger (OPS-6).** No signal is named, and Go exits on `SIGHUP` by default. Recommendation: no signal in v1; the admin call and every published change are the triggers; the guide says to write the env file by rename, which readers see whole.

**Hardening**

26. **Unclaimed threats (AC-51).** AC-51 cites no rule, and no rule claims request smuggling, header count or JSON nesting depth; LIM-6's `MaxBytesReader` was the subject of GO-2026-6859, fixed in Go 1.27.4. Recommendation: amend SEC-6 to name net/http's framing checks and its 500-value header limit, PAR-1 to name the tokenizer's 10,000-level depth, and ADR-001 to "Go 1.27.4 or later", with `govulncheck` under PUB-5. No extra limit in v1: net/http and jsontext as built (64 KiB plus 4 KiB of headers, 500 values, depth 10,000 within `max_body_bytes`), checked by the adversarial rows. Go 1.27.4 accepts a request with both `Content-Length` and chunked `Transfer-Encoding` as chunked and keeps the connection open, which RFC 9112 §6.1 asks a server to close; the proxy never forwards inbound framing (it reads the body and builds a new request), so no desync follows, and the suite checks that the next pipelined request parses on its own.
27. **Earlier promises the code does not keep.** Phase-3 §13 point 1 (operation constant, `content_empty`, `server.address` from `base_url`); point 2 (`Spend`, `Ended`, `scopeNames` and `budgets` dereference the alias); point 3 (counters unreachable from `admin`, span counts instance-wide only); point 4 (audit retention omitted); point 5 (`VACUUM INTO` impossible on `Write` or `Read`); phase-3 §4 step 1 (the unauthenticated counter); `targets.verified_at` and `verified_version` never read or written. Recommendation: accept the resolutions of §3 to §8; no exported signature changes.
28. **Kick-off §18 measurement.** "On a local network", the rate, the payload, which percentile difference, the CPU budget of "≥ 1,000 requests per second", whether "memory with 100 streams" is RSS, and a target for 500 streams are unstated; availability cannot be measured by a benchmark; the image size is a target, not a gate. Recommendation: §9's method (loopback, open loop, the median of ten per-run differences of percentiles, `GOMAXPROCS=1`, RSS for both memory figures), availability stated as not measured, the image size reported; amend kick-off §18.
29. **Outbound connection reuse.** The shipped transport keeps `DefaultMaxIdleConnsPerHost = 2`, so throughput and P99 against one mock host include connection churn. Recommendation: set `MaxIdleConnsPerHost` in `transport.NewClient` only if C42 shows churn, with the figure in `docs/benchmark.md`.
30. **Release (OQ-4, kick-off §19).** No phase designs the release workflow, the image registry, or the pre-release runs (AC-01, AC-02 with unmodified SDKs, AC-30, AC-53's live presets, PUB-2, PUB-3, PUB-5, PUB-8). Recommendation: the README release checklist of §13 now; a tag-triggered workflow that only calls make targets, with no secret beyond the platform token, decided with OQ-4.
31. **Credentials in URL text (SEC-5).** `*url.Error` prints the whole outbound URL, query included; `CheckURL` refuses userinfo, not queries. Recommendation: no new refusal (it would refuse stored providers); credentials are references by SEC-5, and every logged cause is already redacted with the provider's credential.

**Rules this design applies differently from their wording**

32. **Rerank and PAR-1 (kick-off §7.1).** Kick-off §7.1 keeps only the repeated-key rule, on the top-level object, but the scanner must tokenize the whole body to find `model` and the top-level keys, and it refuses malformed JSON and invalid UTF-8 anywhere. Recommendation: accept the refusal (§5); amend kick-off §7.1 to "of PAR-1 only the rules on repeated top-level keys and on well-formed UTF-8 JSON apply".
33. **`validate -f` and RSN-9 (OPS-5).** OPS-5 runs "the same checks … on a file with no server running", and RSN-9 says `validate` warns about chat targets never verified; but a file carries no verification (`export` never writes the columns) and `validate -f` has no store. Recommendation: `validate -f` emits no `target_unverified`; the server-side `validate`, `Open`, `Commit` and `Check` do; amend OPS-5 to "… or on a file with no server running, except the checks that need the store (chat targets never verified)".
