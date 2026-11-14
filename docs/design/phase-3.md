# Phase 3 design

Status: the design phase 3 of §20 of `kick-off.md` is built from. It builds on the phase 1 and phase 2 code and on `docs/design/phase-1.md` and `docs/design/phase-2.md` (cited as "phase-1 §N" and "phase-2 §N"). Rule ids (PAR, RSN, TEL, USE, LIM, RES, SEC, OPS, ADR, AC, OQ) are the kick-off's and binding. Provider, OTLP, OpenTelemetry, Langfuse and W3C facts come from the public documentation and source as read on 2026-11-14, without live calls; "derived" marks a mapping that no vendor documents. Where the kick-off is silent, outdated against that documentation, or contradicted by it, §17 says so with a recommendation, and the design below assumes the recommendation. A bare "§N" is a section of this document; the kick-off's sections are cited as "kick-off §N".

## 1. Scope and outcome

Phase 3 delivers `usage-accounting`, `limits`, `budgets`, `subscription-quotas` and `telemetry-otlp`. Of phase 4 it fixes only the interfaces of §13. It starts from the seams of phase-2 §11; where the shipped code differs from that section, §4 and §17 point 19 say how. At the end of phase 3:

- Every provider call leaves one usage record with tokens, cost and its source, the requested, alias and sent layers, the version and the revision; records reach SQLite in batches through one writer with the USE-10 failure policy, and the daily aggregates move in the same transaction.
- `stargaite usage show` answers sums, percentiles, length and empty counts per group from the local store, with `--compare-previous`; `stargaite usage quota` shows each subscription window.
- Admission enforces parallel slots, `rpm_limit` and `tpm_limit` per key, per alias and (slots) per instance, all or nothing; a new key has 5 slots (LIM-7).
- USD budgets per key, alias and instance exclude `pay_per_use` targets, with `402 budget_exceeded` when nothing is left; counters survive a restart.
- Subscription windows and `quota_share` exclude a plan's targets, with `429 quota_exceeded` and `Retry-After`.
- One OTLP/HTTP JSON exporter sends the request span and the failed-attempt spans of annex D, joins a valid `traceparent`, samples per key and alias, exports content only under an unexpired opt-in, and `stargaite telemetry set|test|status|unset` manage it.
- The OPS-8 counters exist; the metrics endpoint that reads them is phase 4.
- The quick start of `README.md` declares the mock provider `pay_per_use`, adds prices to the target, `usage show`, `telemetry set` and `telemetry test` against the mock provider, which gains an OTLP route.

| Criterion | Checkable at the end of phase 3 | How (§14) |
|---|---|---|
| AC-09 | The C15 corpus also checks the record's `params_changed` and the span's `stargaite.params.changed` against the header | Corpus property |
| AC-18 | Effort and cap "sent" in the record and the span equal the mock's bytes, per attempt | Corpus property |
| AC-21 | The failed attempt before a fallback is a child span of the request span, with its own target and error | Fault matrix and receiver |
| AC-23 | A client gone and a cut stream leave a record; `SIGTERM` persists pending records before the store closes | `TestClientGone`, `TestServeDrain` read the store |
| AC-24 | Every `telemetry test` outcome against the mock receiver, which receives the composed `Authorization` | `TestTelemetryTest` |
| AC-25 | The span golden; key on every span, alias when named, target when selected | Golden |
| AC-26, AC-27 | Refused requests give one error span with key, alias and session; `traceparent` cases | Receiver tables |
| AC-28 | Rates 0, 0.5 and 1 give identical records, budgets, quotas and counters | `TestSamplingInvariant` |
| AC-29 | A planted secret appears in no span, log or record; content only under an unexpired opt-in | `TestPlantedSecret` |
| AC-30 | Filtering by key, alias and target in a real Langfuse | The TEL-14 job, outside the default CI (C32) |
| AC-31 | Exporter down or slow: inference unaffected; counters and last error in `telemetry status` | Traffic with the receiver on `Hang` and `500` |
| AC-32, AC-33, AC-34, AC-35 | Records per outcome; cost source and precedence; the failing writer; medians of two windows | `usage` and `api` tests |
| AC-36, AC-37, AC-38 | Quotas and shares; budgets and the LIM-4 bound; limits with `Retry-After` | Clocked tables, `-race` |
| AC-40 | Records and spans carry the version and the revision (the audit half landed in phase 1) | Record and span tests |
| AC-43 | The telemetry endpoint: a name that resolves private after validation, and a redirect, are refused | Exporter test with the test resolver |
| AC-53 | The preset golden carries the `stream_usage` and `gen_ai_provider` data of §17 point 9 | Golden, updated when the owner's settled points were recorded |

## 2. Decisions

| Decision | Choice | Reason | Discarded |
|---|---|---|---|
| Where a record is made | `finish` builds it from a `usage.Call`; `done` builds the spans from the attempts' records | One source for record, span, log and counters (USE-5, TEL-9); `targets verify` fills the same `Call` (§13) | The attempt span in `finish` (phase-2 §11 point 2, a named move) |
| Writer | `usage.Writer` batches on `store.Write`: one transaction per batch, 100 records or 50 ms, queue of 10,000 | The store already has one writer goroutine shared with config commits (ADR-003); USE-10 numbers | A second writer connection; one transaction per record |
| Record id | `<request id>-<attempt>` | Unique per call, so a retried batch inserts nothing twice (USE-2) | A random id per record |
| Error column | `usage.error_type` holds the kick-off §15 code, as annex D's `error.type` does; the type follows from the code (`domain.NewError`) | Tells `upstream_timeout` from `upstream_error` without a column | An `error_code` column (§17 point 15) |
| Upstream model id | Derived: the target id after its first `/` (USE-4) | Exactly the id sent; no column | An `upstream_model` column |
| USE-3 on the openai protocol | Output is `max(completion_tokens, total_tokens − prompt_tokens)`; a `null` reasoning count stays nil | xAI counts reasoning outside `completion_tokens` (official example); the Gemini compatible endpoint is reported to, unofficially (§17 point 8); ADR-005 keeps it data-free | A per-provider normalization field |
| Estimates (USE-8) | Only after a `2xx`: a call with no usage, or a stream that did not end cleanly | A refused call bills nothing (§17 point 10) | Estimating every call |
| `include_usage` | `Build` writes it for `stream_usage: request`; `openai.Stream` swallows the usage chunk it asked for and the client did not | USE-8; `Build` already has `Provider` and `Client` | Filtering chunks in `api` |
| Counting windows | One `window` type: buckets of `max(1 s, w/600)` aligned to Unix time, never admitting more than an exact window | Bounded memory; a restart rebuilds the same buckets (RES-9, LIM-8) | Exact sliding logs (memory grows with traffic); two-bucket estimates (can admit more) |
| Admission | `Admit(scopes ...Scope)` between `check` and `route`, all or nothing; its release deferred in `done` | LIM-5, LIM-6; explicit scope names let `targets verify` pass only `global` (§13) | Admission inside `check` |
| Budget periods | A period is a map key (`d:2026-11-14`, `m:2026-11`); no reset task | The figure is the same before and after a restart (LIM-2) | The `reset budgets` task of kick-off §14.3 (§17 point 17) |
| `never` budgets | `budget_totals.since_revision`; a record counts when its revision is at or after it, in memory and in the writer's `UPDATE` | Memory and store agree when a commit races a charge (LIM-2) | Rows matched by scope alone |
| Quota count | `Take` checks and counts atomically at the end of `exclude`, after the circuit permit | Concurrent requests cannot over-admit; every counted call has an attempt, hence a record | Counting at the line before `Do` without the check (phase-2 §11 point 4, §17 point 20) |
| Quota rebuild | Every declared window is seeded from the store before the listeners bind; one declared while running, at its first use, outside the lock | RES-9 and LIM-8 at start; one `Seed` for both cases | Seeding only at first use (a failed or slow seed on the hot path) |
| Conventions pin | `semantic-conventions-genai` at commit `e6658f14c9d9ea0760825f029cb82e7673f48fab`, no `schemaUrl` | Keeps every annex D name, as annex D expected (§17 point 1) | Core v1.41.1, which lacks `gen_ai.request.reasoning.level` |
| Span kinds | Request span `SERVER`, attempt span `CLIENT` | The request span serves the client's call; an attempt is the provider call | `CLIENT` for both (§17 point 2) |
| Unknown cost | No cost attribute | Annex D "only when known"; USE-7, PRI-11 | `gen_ai.usage.cost: 0` against Langfuse's guess (§17 point 4) |
| Content | Strings: the outgoing `messages` and the relayed answer bytes, captured in `api` up to `content_max_bytes` | TEL-8 truncation is in bytes; Langfuse decodes no structured values (§17 point 7) | `Observed.Content` with a capture bound on every adapter constructor |
| Exporter configuration | `config.build` composes `Snapshot.Exporter`: URL, static headers, the Basic header and `header_env` values as `transport.Secret`, unresolved references | Telemetry credentials re-resolve on every publish like provider ones (OPS-6, SEC-5) | The exporter reading the environment |
| Export rules | OTLP's: retry 429, 502, 503, 504 and transport failures only, `Retry-After` up to 30 s, 10 s timeout, 4 MiB response cap | TEL-12 is silent on statuses (§17 point 5) | Retrying every failure |
| Queue bound | Spans encoded at `Enqueue`; the 32 MB bound counts those bytes | TEL-12 counts real bytes | Estimating sizes |
| Sampling | Once per request, from a 64-bit `crypto/rand` draw stored in `rq` after authentication, against the lower declared rate; content capture and `done` read the same draw | TEL-7; a client's trace id need not be random (TEL-6) | The low bits of the trace id |
| OPS-8 counters | `usage.Counters`, fed by `finish`, `done` and admission; span counts stay in `Exporter.Status`; phase 4 renders both | "The counters exist from the feature that produces them"; AC-28 compares error counters | Deferring them to `metrics` |
| Warnings (80 %) | A log line, an audit row through the writer and a counter, once per period or crossing | kick-off §10 last paragraph, LIM-3, LIM-9 | Log only |
| LIM-7 | `build` gives a published key without `max_parallel_requests` the default 5 | kick-off §6: defaults apply at publish; LIM-4 relies on it | A default written at `keys create` only |
| `trust_traceparent` | `WithDefaults` fills the telemetry defaults with or without a block | TEL-6 default true is visible in `settings show` | A default only when a block exists |
| Upstream limit headers | Not read; the IETF `RateLimit` fields are not emitted | The draft has no token unit and no provider sends it; §17 points 13 and 14 give the cost | Parsing `x-ratelimit-*` per provider |
| Dependencies | None added: `encoding/json/v2` tagged structs encode OTLP | ADR-010 | The OpenTelemetry SDK |

## 3. Package map

| Package | Phase 3 responsibility | May import (internal) |
|---|---|---|
| `admission` (new) | `window`; slots, rates, budget spend, quota windows and shares, warnings, `Retry-After`; on an injected clock | `domain` |
| `usage` (new) | `Call`, `NewRecord`, `PriceCost`, `Writer`, `Restore` and the quota `Seed`, `Show`, `Counters` | `domain`, `plan`, `admission` |
| `telemetry` (new) | `Context`, `Parse`, `Sampled`, span assembly from records, the OTLP encoder, `Exporter` with `Test` and `Status` | `domain`, `config`, `transport`, `usage` |
| `domain` | + the five codes of kick-off §15 in `NewError`; `Observed.Cost` | — |
| `adapters/openai` | Cost read at `cost_report.path`; the USE-3 output rule; Mistral's `prompt_token_details`; `null` reasoning as nil; `NewStream(t, p, includeUsage)` swallows the usage chunk it asked for | unchanged |
| `adapters/anthropic` | Cost read at `cost_report.path` (no preset sets one) | unchanged |
| `params` | `ChatReply` gains `{usage,prompt_token_details}` | unchanged |
| `plan` | `Build` writes `stream_options.include_usage` (USE-8) | unchanged |
| `config` | LIM-7 at publish; `WithDefaults` telemetry defaults; `Snapshot.Exporter` and `Snapshot.NeverSince`; `budget_totals` rows in `writeChange`; `State.NeverSince` loaded and cloned | unchanged |
| `store` | Migration 002 (§5) | — |
| `api` | Admission, exclusions, records, counters, trace context, session, spans, content capture | + the three new packages |
| `admin` | `usage` and `telemetry` routes; `keys` accept `content` | + `usage`, `admission`, `telemetry` |
| `cli` | `usage show`, `usage quota`, `telemetry set\|test\|status\|unset`, `--content` | unchanged |
| `cmd/stargaite` | Start and stop order of §11 | + the three new packages |
| `tests/mockprovider` | `UsageOnRequest`; `POST /v1/traces` | — |

Import rule, added to `internal/plan/imports_test.go`: `admission` reaches only `domain`; `usage` never reaches `config`, `store`, `transport` or `api`; `telemetry` never reaches `api`, `admin` or `cli` (it reaches `store` only through `config`, for `config.Exporter`).

Differences from annex C and from phases 1 and 2, each with its reason:

- Batching lives in `usage`, on top of `store.Write`: the store keeps its single writer goroutine (ADR-003), which config commits share, so `budget_totals` rows written by a commit and by the writer are serialized.
- `usage` holds the OPS-8 counters until `metrics` adds the endpoint; annex C has no other home for them before phase 4.
- Spans are assembled in `telemetry` from `usage.Record`, so the record is the one source of observable values; `api` only collects them.
- Shipped signatures that change: `exclude(rq, t) (gen uint64, e *domain.Error)`, because `quota_exceeded` carries `Retry-After` (LIM-9) and `budget_exceeded` its scope; `authenticate` returns a `telemetry.Context` in place of the trace string (TEL-6); `openai.NewStream(t, p, includeUsage bool)`, as `anthropic.NewChatStream` already takes it, to swallow the usage chunk (USE-8); `admin.Handler(admin.Deps)`, because the new routes need the read pool, the admission state and the exporter; `config.WithDefaults` fills telemetry defaults without a block. `api.Deps` gains fields; `attempt` and `request` gain fields; `a.started`, `a.firstByte`, `a.firstEvent`, `a.ended` and `rq.started` all read `h.Config.Now()` (chat.go:163, :321, :384), and `done`'s duration is `h.Config.Now().Sub(rq.started)` (chat.go:685), so every duration, record time and budget period is on one clock, the test clock in tests; timers, write deadlines and `AfterFunc` stay on the wall clock.

## 4. Request flow with phase 3

```go
package api

type Deps struct {
	Config    *config.Service
	Log       *slog.Logger
	Clients   [2]*http.Client
	Flight    *sync.WaitGroup
	Usage     *usage.Writer
	Admission *admission.State
	Spans     *telemetry.Exporter
	Counters  *usage.Counters
	Version   string // cli.Version: records and spans (USE-5, TEL-13)
}

// request gains:
//	trace   telemetry.Context // replaces traceID; set by authenticate
//	session string            // x-session-id when valid, else ""
//	sample  uint64            // a crypto/rand draw taken in infer after authenticate (TEL-7)
//	release func()            // admission's; nil until admitted
//	content bool              // sampled and the key's content_until is ahead (TEL-8)
// attempt gains:
//	firstEvent time.Time   // the first upstream event of a stream (ttft_ms)
//	complete   bool        // the answer was read to its end: a sync body observed, or a stream's last event
//	rec        usage.Record
//	answer     []byte      // captured answer bytes, at most content_max_bytes, when rq.content
```

| Step (kick-off §14.1) | Phase 3 addition | Refusal |
|---|---|---|
| 1 | `authenticate` parses `traceparent` when `trust_traceparent` (§10) and sets `x-stargaite-trace-id`; `infer` reads `x-session-id` | `401`, a counter, no span (TEL-5) |
| 2, 3 | Unchanged | As phase 2 |
| 4 | `h.admit(rq)`: `Admit` over `key:<name>`, `model:<alias>`, `global`; `rq.content` decided | `429 parallel_limit_exceeded`, `429 rate_limit_exceeded` |
| 5, 6 | `Build` (USE-8 injection), then `exclude` in RES-10 order | §8, §9; RES-10 |
| 7 | `call`: `firstEvent`, `complete`, capture; `finish` records and charges | kick-off §15 |
| 8 | `done`: release, counters, spans, request line | — |

```go
if rq.err = h.check(w, r, rq); rq.err == nil {
	if rq.err = h.admit(rq); rq.err == nil { // step 4; rq.release set
		rq.err = h.route(w, r, rq)
	}
}

// exclude, RES-10 order (budget, quota, store, then no_target_available):
func (h *handler) exclude(rq *request, t *config.Target) (uint64, *domain.Error) {
	p := &t.Provider.Spec
	if p.Billing == domain.BillingPayPerUse {
		if e := h.Admission.Exhausted(budgets(rq)...); e != nil {
			return 0, e // 402 budget_exceeded, naming the scope kind only (LIM-3)
		}
	}
	qp := quotaPlan(rq, t) // nil unless subscription with a quota
	if qp != nil {
		if e := h.Admission.Quota(*qp); e != nil {
			return 0, e // 429 quota_exceeded with Retry-After (LIM-9)
		}
	}
	if p.Billing == domain.BillingPayPerUse && !h.Usage.Available() {
		return 0, domain.NewError(domain.CodeUsageStoreUnavailable, "the usage store is unavailable") // USE-10
	}
	if t.Provider.Disabled {
		return 0, noTarget
	}
	gen, ok := h.breakers.Acquire(t.ID)
	if !ok {
		return 0, noTarget
	}
	if qp != nil {
		if e := h.Admission.Take(*qp); e != nil { // lost a race since Quota
			h.breakers.Release(t.ID, gen, routing.None)
			return 0, e
		}
	}
	return gen, nil
}
```

`route` keeps the first dynamic `*domain.Error` (`cmp.Or` on the code today) and answers it when no provider was called, so its `Retry-After` and message reach the client; `no_target_available` keeps phase 2's message. RES-10 applies as phase 2 built it: once a provider was called, the last call's kick-off §15 error wins.

```go
func (h *handler) finish(rq *request, a *attempt) {
	a.ended = h.Config.Now()
	h.breakers.Release(a.target.ID, a.gen, a.end.breaker)
	a.rec = usage.NewRecord(h.usageCall(rq, a))
	t := a.rec.InputTokens + a.rec.OutputTokens
	h.Admission.Tokens(t, a.ended, scopeNames(rq)...) // tpm (LIM-5)
	var warns []admission.Warning
	switch a.rec.Billing {
	case domain.BillingPayPerUse:
		if a.rec.CostUSD != nil { // before done releases the slot (LIM-4)
			warns = h.Admission.Spend(*a.rec.CostUSD, a.ended, scopeNames(rq), budgets(rq))
		}
	case domain.BillingSubscription:
		if qp := quotaPlan(rq, a.target); qp != nil {
			warns = h.Admission.Ended(*qp, t, a.ended)
		}
	}
	h.warn(warns)            // log line, Writer.Audit, counter
	h.count(rq, &a.rec)      // usage.Counters (OPS-8)
	h.Usage.Add(a.rec)       // durable, in a batch (USE-10)
	h.Log.Info("attempt", …) // phase 2's line, unchanged
}

func (h *handler) done(rq *request) {
	defer h.Flight.Done()
	if rq.release != nil {
		defer rq.release() // after every finish, before Flight, also on a panic (LIM-6)
	}
	status, code := outcome(rq) // phase 2's request-line status and code
	h.Counters.Add(usage.Counter{Name: "requests", Key: rq.key.Name, Model: rq.alias, Label: strconv.Itoa(status)}, 1)
	if rq.snap.Exporter != nil && telemetry.Sampled(rq.sample, sampleRate(rq)) { // no exporter: no span is built
		h.Spans.Enqueue(telemetry.Build(h.spanRequest(rq, status, code))...)
	}
	h.Log.Info("request", …)
}
```

`finish` keeps phase 2's first statement and order (`a.ended`, then `Release`, then the line), so a panic in the record code still returns the circuit permit; the warnings go to the log, the writer's audit rows and the counters. `done` releases the slots after every `finish` and before `Flight`, a panic included (LIM-4, LIM-6), then counts, builds the spans when sampled (§10) and writes the request line.

**Refused at admission.** No record (USE-1), the counters, and one request span with `stargaite.attempts: 0` (TEL-5). **Fell back.** Records 1 and 2 under one request id; attempt 1 is a child span `attempt {target}` with its error; the request span carries attempt 2's usage (TEL-10). **Client gone.** The record of the open call with `error_type: client_gone`, status as received, tokens estimated when a `2xx` had begun (§6).

The sync error envelope is still written by `infer` after the last `finish` (phase-2 §11 point 2 says "after its last byte"); nothing in the record depends on it.

Settled in C22:

- **Staging.** `Deps` gains `Usage`, `Counters` and `Version` only; `Admission` joins with C26 and `Spans` with C29, and `request` keeps `traceID string` until C30. `attempt` gains `firstEvent`, `complete` and `rec`; `answer` waits for C31. `finish` runs as written without the admission, budget and quota lines; `done` adds the `requests` counter before its line.
- **Clocks.** `rq.started`, `a.started`, `a.firstByte`, `a.ended` and `done`'s duration read `h.Config.Now()`; `a.firstEvent` is the first event `transport.EventReader` returns that is not an SSE comment, which carries no content. `a.complete` is set when the synchronous adapter observed the body (before the write to the client) or when the stream's last event was relayed.
- **Counter keys.** The `requests` counter names the alias only when it exists (`rq.model` set), else `""`: `rq.alias` is the client's text, and a key that sends random unknown aliases would otherwise grow the counters without bound. `finish`'s counters, keyed by the record's key and alias, are `tokens` (labels `input`, `cache_read`, `cache_write`, `output`, `reasoning`), `cost_usd`, `unpriced`, `length` (`usage.Length`), `empty`, `model_mismatch` and `params_changed` (label: the action); a zero is not added.
- **Error code.** `usage.Call.ErrorCode` is `a.end.err.Code` when the attempt ended with an error, `client_gone` when its cause is the client gone, else empty; a shutdown is `internal_error`.

## 5. Usage records and the writer

**Migration 002** (`internal/store/migrations/002_usage.sql`; 001 stays as shipped, because `migrate` skips every file at or below `user_version` and data directories made by the phase 1 and 2 binaries are already at 1): `ALTER TABLE budget_totals ADD COLUMN since_revision INTEGER NOT NULL DEFAULT 0`, the revision its counter started at (§8); `usage_daily` is dropped and recreated with primary key `(day, key_name, model, target, billing)`, so a provider whose billing changes keeps its rows apart (nothing wrote it before phase 3). Nothing else changes; no index is added (the quota seed filters on `idx_usage_time`, §9). A test opens a store at version 1 and checks both changes.

```go
package usage

// Call is one provider call as finish saw it. targets verify (phase 4) fills it
// with Key "_verify" and Model "".
type Call struct {
	RequestID, TraceID, Key, Model, Version string
	Endpoint                                domain.Endpoint
	Attempt, Status                         int // Status: the provider's; 0 without a response
	TargetID                                string
	Target                                  *domain.Target
	Provider                                *domain.Provider
	Out                                     *plan.Outgoing
	Stream, Complete                        bool
	ErrorCode                               string // the kick-off §15 code, "client_gone", or ""
	Obs                                     domain.Observed
	Started, FirstEvent, Ended              time.Time // the service clock
	Revision                                int64
}

type Record struct { /* one field per column of usage, in annex A order */ }

func NewRecord(c Call) Record
func PriceCost(p *domain.Pricing, u domain.Usage) float64

func NewWriter(write func(context.Context, func(*sql.Tx) error) error, log *slog.Logger) *Writer
func (w *Writer) Add(r Record)                 // never blocks: a full queue or a closed writer logs it (USE-10)
func (w *Writer) Audit(a admission.Warning)    // an audit row in the next batch (kick-off §10)
func (w *Writer) Available() bool              // false from the third consecutive failed commit
func (w *Writer) Close(ctx context.Context) int // see below; the count logged instead of stored
```

`Close` stops accepting entries and the retry loop, and always waits for the `store.Write` in progress to return: a queued write always ends (within `busy_timeout` plus the commit), and a committed batch counts as stored. It then commits the rest of the queue while `ctx` allows, and returns only after the writer goroutine has exited, so no write starts after it. It logs at critical level, and counts, only the entries never handed to `store.Write`. A test whose write blocks past the `Close` deadline and then succeeds sees a count of 0 for that batch.

| Column | Value |
|---|---|
| `id`, `request_id`, `trace_id`, `attempt` | `<request id>-<attempt>`, the request id, the trace id in lowercase hex, `a.n` |
| `endpoint`, `key_name`, `model`, `target`, `billing` | `rq.ep`, the key name, `rq.alias`, `a.target.ID`, the provider's `billing` |
| `response_model`, `model_mismatch` | `Obs.ResponseModel`; set when it is non-empty and starts with neither the upstream id nor a `response_models` entry (USE-4) |
| `status_code`, `error_type` | The provider's status, `0` without a response; the attempt's kick-off §15 code, `client_gone`, or NULL |
| `finish_reason`, `stream` | The provider's own value (USE-3); `rq.client.Stream` |
| tokens, `reasoning_tokens`, `tokens_source` | §6 |
| `cost_usd`, `cost_source` | §6 |
| effort and cap columns, `params_changed` | `a.out.Effort`, `EffortSource`, `EffortAdjusted`, `MaxOutput`, `plan.Header(a.out.Changes)` (USE-5) |
| `content_empty` | A `2xx` with `VisibleBytes == 0` and `ToolCalls == 0` |
| `ttft_ms` | Streams: `firstEvent − started`, the first upstream event read (annex A), not the headers as `firstByte` is; NULL for sync |
| `started_at`, `completed_at`, `config_revision`, `proxy_version` | `a.started`, `a.ended` (service clock), `rq.snap.Revision`, `Deps.Version` |

**Writer (USE-2, USE-10).** One goroutine reads a channel of 10,000 entries (records and audit rows) and commits a batch at 100 entries or 50 ms after the first, through `store.Write` (`synchronous=FULL` on every connection, ADR-003). In the batch's transaction each record is `INSERT OR IGNORE INTO usage`; only when `RowsAffected() == 1`: an upsert of `usage_daily` for the UTC day of `completed_at` (requests `+1`, errors `+1` when `error_type` is not NULL, each token type, `cost_usd` when not NULL), and, for a `pay_per_use` record with a cost, `UPDATE budget_totals SET cost_usd = cost_usd + ? WHERE scope_type = ? AND scope_name = ? AND since_revision <= ?` for `global`, the key and the alias, with the record's `config_revision`. A failed commit is retried after 50 ms with the same batch; from the third consecutive failure `Available` is false and the retry runs every 5 s; a success makes it true again. While the batch waits, entries keep queueing; an `Add` that finds the queue full, or comes after `Close`, writes the record as one `CRITICAL` JSON log line (`"usage record not stored"`, every column, no content) and counts it: never lost silently, never rate-limited (OPS-9). The intervals are package variables, as `serve`'s `drainWait` is, which tests lower: `batchWait` 50 ms, `retryWait` 50 ms, `unavailableRetry` 5 s.

```go
type Query struct {
	Since                  time.Time // the window is [Since, now)
	Key, Model, Target     string   // filters; "" for any
	By                     []string // of key, model, target, effort-sent
	ComparePrevious        bool
}
type Group struct {
	Billing, Key, Model, Target, EffortSent                        string // the grouped ones; the others empty
	Requests, Errors, Estimated, Unpriced, Length, Empty            int64
	Input, CacheRead, CacheWrite, Output, Reasoning                 int64
	CostUSD                                                         *float64 // pay_per_use groups only
	OutputP50, OutputP90, ReasoningP50, ReasoningP90                *int64   // records only
}
type Report struct {
	Source   string // "records" or "daily"
	Groups   []Group
	Previous []Group // with ComparePrevious
}
func Show(ctx context.Context, db *sql.DB, q Query, now time.Time, usageDays int64) (Report, error)
```

**`usage show` (USE-9).** `Show` reads the read pool. Within `usage_days` it answers from records: one statement over `usage` filtered by window (`completed_at`), key, alias and target, grouped by `billing` and the `--by` columns (`key_name`, `model`, `target`, `effort_sent`). Per group: requests (provider calls), errors (records whose `error_type` is not NULL, the rule the writer applies to `usage_daily`), each token type, estimated calls, cost (`pay_per_use` only; `subscription` and `none` groups show requests and tokens, USE-7), unpriced calls, `length` finishes (`finish_reason` in `length`, `max_tokens`, `model_context_window_exceeded`, the provider's own values that mean it), empty responses, and nearest-rank p50 and p90 of `output_tokens` and `reasoning_tokens` over the `2xx` records with `tokens_source = 'provider'` that reported the value, by `ROW_NUMBER() OVER (PARTITION BY …)`. A window that starts before the retention reads `usage_daily` sums for all of it, without percentiles or `effort-sent`, and says so (`"source": "daily"`). `--compare-previous` runs the same query on the window of equal length before it.

Settled in C22:

- **Close.** `Close` keeps retrying a failed commit, every `retryWait`, while its context allows, and commits the rest of the queue the same way. A batch whose write failed and whose retries the context ended is logged at critical level and counted too: it was handed to `store.Write` but is not stored, and the sentence "only the entries never handed to `store.Write`" would otherwise lose it silently. `Close` returns the count over the writer's life, `Add`'s overflow and late records included, so `serve` exits `1` for any record logged instead of stored; a second `Close` returns the same count.
- **Failures in C22.** Without `Available` (C23), a failed commit is retried every `retryWait` without bound while records keep queueing; the log gets one `ERROR` line when a run of failures starts (`usage records not committed: retrying`) and one `INFO` line when the batch commits again, not one per retry.
- **Critical level.** `usage.LevelCritical` is `slog.LevelError + 4`; `serve`'s handler writes it as `"level":"CRITICAL"`. The line is `"msg":"usage record not stored"` with the record under `record`, every column by its annex A name.
- **Show in C22.** `Show(ctx, db, q, now)` takes no `usageDays` until the daily fallback (C25); `Query` has `Since`, `Key`, `Model`, `Target` and `By` (key, model and target; `effort-sent` is refused until C25), `Group` the sums without `EffortSent`, `Length`, `Empty` and the percentiles, and `Report` no `Previous`. JSON names: `billing`, `key`, `model`, `target` (only when grouped), `requests`, `errors`, `estimated`, `unpriced`, `input_tokens`, `cache_read_tokens`, `cache_write_tokens`, `output_tokens`, `reasoning_tokens` and `cost_usd` (only for a `pay_per_use` group with a priced record).

## 6. Tokens and cost

**Normalization (USE-3)**, in the adapters' single observation pass:

| Path | `Input` | `CacheRead`, `CacheWrite` | `Output` | `Reasoning` |
|---|---|---|---|---|
| openai protocol | `prompt_tokens` | `prompt_tokens_details.cached_tokens` (else Mistral's `prompt_token_details.cached_tokens`), `.cache_write_tokens` | `max(completion_tokens, total_tokens − prompt_tokens)` | `completion_tokens_details.reasoning_tokens` when an integer; nil when absent or `null` |
| anthropic protocol | `input_tokens + cache_read_input_tokens + cache_creation_input_tokens` (phase 2) | as phase 2 | `output_tokens` | `output_tokens_details.thinking_tokens` (phase 2) |

The output rule fixes xAI (prompt 32, completion 9, reasoning 94, total 135: output 103), would fix the Gemini compatible endpoint if the unofficial report holds (`tests/contract` checks it), and leaves every provider whose total is prompt plus completion unchanged (§17 point 8). Groq's `x_groq.usage` is not read (unverified; `tests/contract` settles it).

**Estimates (USE-8).** For a call with a `2xx`: when `Obs.Usage` is nil, or the answer is not `complete` (a cut stream, a timer, client gone mid-stream), `tokens_source` is `estimated`, input is the reported input when usage arrived, else `ceil(len(a.out.Body)/4)`, and output is `max(reported output, ceil(VisibleBytes/4))`; cache parts and reasoning keep what was reported. A sync answer observed whole and then lost on a write to a gone client is `complete`: its usage is the provider's. Without a `2xx` (no response, `4xx`, `5xx`) every count is 0 and `tokens_source` is `provider`, the only other value the column has (§17 point 10).

**Cost (USE-6, USE-7, USE-12)**, in order: `subscription` and `none` records have NULL with source `none`; else `Obs.Cost` when not nil (`provider`); else, with prices, `PriceCost` (`price_table`), also over estimated tokens and also `0` for a call with zero tokens; else NULL with source `none`, counted as unpriced.

```go
// PriceCost is USD for u at p: each part at its own price, a missing cache
// price at the input price, reasoning at the output price (it is part of it).
func PriceCost(p *domain.Pricing, u domain.Usage) float64 {
	in, out := *p.InputPerMillion, *p.OutputPerMillion
	read, write := cmp.Or(p.CacheReadPerMillion, &in), cmp.Or(p.CacheWritePerMillion, &in)
	plain := max(u.Input-u.CacheRead-u.CacheWrite, 0)
	return (float64(plain)*in + float64(u.CacheRead)**read + float64(u.CacheWrite)**write + float64(u.Output)*out) / 1e6
}
```

**Provider cost.** `Observed.Cost *float64` is the number at `cost_report.path` times `usd_per_unit`; nil when the member is absent, `null`, not a number or negative. The adapters append the parent objects of the path to their reply walk (`usage.cost` needs none beyond `{usage}`), in `Observe`, `ToChat` and the stream constructors; in a stream the latest non-null value wins, as for usage.

**Injection (USE-8).** `Build`, for chat toward an `openai` target with `stream: true`, `Provider.StreamUsage == request` and `Client.Usage` false, writes `stream_options.include_usage: true` (parents created), listed `stream_options:added` unless the client sent `include_usage: false`, which is listed `stream_options:translated` (§17 point 21); `explain` shows it. `stream_options` is already a key a `forward` rule may not name. `openai.NewStream(t, p, includeUsage)` drops a chunk whose `choices` is empty and whose `usage` is not null, after observing it, only when `p.StreamUsage == request` and `includeUsage` is false, that is, exactly when `Build` injected `include_usage`; toward `native`, `none` or an undeclared `stream_usage` every chunk is relayed as in phase 2, and the golden `openai-stream.down.sse` keeps its usage chunk. The `"usage": null` OpenAI adds to every other chunk is relayed as is (SDKs read the member as optional). An undeclared `stream_usage` injects nothing, as `native` and `none` do. Toward `anthropic`, usage always arrives (phase-2 §9).

**Price shapes not represented.** One price table cannot express cache-write tiers by TTL, long-context tiers, service-tier and residency multipliers, peak hours, per-call tool fees or dated price changes; the guide documents them and prefers `cost_report` where a provider has one (§17 point 11).

Settled in C22:

- **Cost path.** `params.Reply(walk, path)` adds the parents of a dotted `cost_report.path` to a reply walk without modifying it, and `params.Cost(b, path, perUnit)` reads the number there (absent, `null`, a string, any non-number or a negative number is nil); both adapters use them in every observer, and a repeated key on the way to the cost refuses the body like any key the proxy reads. `openai.Stream` now holds that walk, so its zero value is no longer ready: `NewStream` makes it.
- **Output rule.** A `total_tokens` below `prompt_tokens + completion_tokens` leaves `completion_tokens`; a missing total counts as 0. The Mistral fallback applies only when `prompt_tokens_details.cached_tokens` is not an integer.
- **Estimates.** A `2xx` stream whose relay failed before any event (not an event stream) and an oversized synchronous `2xx` are estimated like a cut stream: input from the bytes sent, output from the visible bytes, which are 0, so `content_empty` is set.

## 7. Limits

```go
package admission

type Scope struct {
	Name               string // "global", "key:<name>", "model:<alias>"
	Parallel, RPM, TPM int64  // 0: no limit
}

func New(now func() time.Time, seed Seed) *State
func (s *State) Admit(scopes ...Scope) (release func(), e *domain.Error) // all or nothing; release is idempotent
func (s *State) Tokens(n int64, at time.Time, scopes ...string)          // tpm, at finish

// window counts over a sliding span w in buckets of g = max(1 s, w/600),
// aligned to Unix time: an entry stays counted until its whole bucket is older
// than w, so the sum is never below an exact window's (it may count up to g
// more of the past, §17 point 13).
type window struct {
	w, g    time.Duration
	first   int64   // Unix index of the oldest bucket held
	buckets []int64 // ring of w/g + 1
}
func (x *window) add(at time.Time, n int64)
func (x *window) sum(now time.Time) int64
func (x *window) wait(now time.Time, limit float64) time.Duration // until sum < limit with no new entry, rounded up to whole seconds, at least 1 s
```

**Admit (LIM-5, LIM-6, LIM-7).** Under one mutex, for each scope in order key, alias, instance: in-flight `>= Parallel`, then `rpm.sum >= RPM`, then `tpm.sum >= TPM`. The first refusal answers, with nothing counted anywhere: `429 parallel_limit_exceeded` with `Retry-After: 1`, or `429 rate_limit_exceeded` with the binding window's `wait` (§17 point 13). Otherwise every scope takes a slot and its `rpm` window adds 1 at that instant. The instance scope carries only `settings.max_parallel_requests`; a scope without limits holds no window. `tpm` adds each call's recorded input plus output, reported or estimated, at `finish` (LIM-5). Windows of a scope whose limits are cleared are dropped at the next `Admit`; rpm and tpm are not rebuilt after a restart (RES-9 names budgets and quotas).

**LIM-7.** `config.build` publishes every key with `Spec.MaxParallelRequests = cmp.Or(declared, domain.DefaultKeyMaxParallelRequests)`; `export` keeps the declared value, `keys show` the effective one, and the CLI help "(5 when undeclared)" becomes true. A key cannot then be unlimited, only large (§17 point 16).

## 8. Budgets

```go
type Budget struct {
	Scope string        // as Scope.Name
	Spec  domain.Budget
	Since int64         // reset_interval never: snap.NeverSince[Scope]
}

func (s *State) Exhausted(bs ...Budget) *domain.Error
func (s *State) Spend(usd float64, at time.Time, scopes []string, bs []Budget) []Warning
func (s *State) SeedSpend(scope, period string, usd float64)          // Restore
func (s *State) SeedNever(scope string, since int64, usd float64)     // Restore

// Warning is one crossing that goes to the log, the audit and the counters.
// Event: budget_warning, budget_exhausted or quota_warning; Period: the budget's
// period key, or a quota's window and the Unix second of the crossing; At: the charge's a.ended.
type Warning struct {
	Event, Scope, Period string
	At                   time.Time
}
```

**Counters (LIM-2).** Spend of every `pay_per_use` record with a cost is added, at `finish` and before the slot is released (LIM-4), to `d:<UTC day>` and `m:<UTC month>` of `completed_at` for `global`, `key:<name>` and `model:<alias>`, whether or not the scope has a budget, so a budget set mid-period sees the period's spend. Keys of older periods are dropped on the first charge of a new one. `daily` reads today's key, `monthly` the month's; LIM-1: only scopes with a budget are checked.

**`never` budgets.** `config` keeps `State.NeverSince` (scope to revision), loaded from `budget_totals` and published as `Snapshot.NeverSince`. In `Commit`, after the revision increments: a scope whose budget stays `never` keeps its `since` (a `limit_usd` change keeps the counter); one whose budget becomes `never` (new, or from another interval) gets the new revision and `writeChange` replaces its row with `(scope, since, 0)`; one whose `never` budget is removed or changes interval loses its row. In memory, a `Budget` whose `Since` is above the stored one resets the counter to zero at that `since`; a charge whose `Since` is below it, or a record whose request started under an older snapshot without the budget, is not counted; the writer's `since_revision <= config_revision` applies the same rule in the store, so memory and store agree when a commit races a charge. Phases 1 and 2 already stored `never` budgets without a row, so `config.Open`, before the first publish, runs one `store.Write` of `INSERT OR IGNORE INTO budget_totals` with `(scope, current revision, 0)` for every declared `never` budget; every published `never` budget then has a row and a `NeverSince` entry, and a test of `Open` checks it.

**Exclusion (LIM-3).** In `exclude`, for `pay_per_use` targets only: each budget of the request's key, alias and instance whose counter is at or above `limit_usd` excludes the target. The error names the kind of scope, never a figure: `402 budget_exceeded` "the budget of this key is exhausted" (or "of model %q", "of the instance"), with no `Retry-After`. A `subscription` or `none` target is never blocked by a USD budget.

**Warnings.** The charge that moves a counter from below 80 % to 80 % or above returns a `Warning{Event: "budget_warning", Scope, Period, At}` once per period: a `WARN` log line, an audit row through `Writer.Audit`, and a counter (kick-off §10). Crossing 100 % adds the same with `budget_exhausted`. `Writer.Audit` writes `INSERT OR IGNORE INTO audit (id, actor, action, resource_type, resource_name, result, created_at)` with id `<event>:<scope>:<period>`, actor `system`, action the event, `resource_type` `budget` or `quota`, `resource_name` the scope, result `warning` and `created_at` = `At` in Unix milliseconds, so a retried batch adds no row twice.

**Overshoot (LIM-4).** No reservation: spend can pass the limit by the cost of the calls in flight in the scope when it is crossed, at most its slots (5 per key by default) times the cost of one capped answer.

**Restore (RES-9).** Before the listeners bind, `usage.Restore` reads `SELECT key_name, model, day, SUM(cost_usd) FROM usage_daily WHERE billing = 'pay_per_use' AND day >= <first day of the month> GROUP BY 1, 2, 3` into `SeedSpend` for the three scopes, and every `budget_totals` row into `SeedNever`. Records written to the critical log are missing from both after a restart (§17 point 15).

## 9. Subscription quotas

```go
type Plan struct {
	Provider string
	Windows  []domain.QuotaWindow
	Key      string
	Share    float64 // the key's quota_share of this provider; 0 when none
}
type Event struct {
	Key         string
	Sent, Ended time.Time // started_at, completed_at
	Tokens      int64     // input + output
}
type Seed func(provider string, from time.Time) ([]Event, error)

func (s *State) Quota(p Plan) *domain.Error                      // check only
func (s *State) Take(p Plan) *domain.Error                       // check again and count one request, under the same lock
func (s *State) Ended(p Plan, tokens int64, at time.Time) []Warning // tokens, at finish
func (s *State) Windows() []WindowView                           // usage quota
```

**Windows (LIM-8).** Each declared window of a subscription provider has a `window` for requests and one for tokens (as its limits declare), for the plan and for each key whose `quota_share` names that provider; when a share is added, that key's windows are seeded from the store by the same `Seed`, keeping its own rows by `key_name`, so earlier use counts. A request counts when `Take` admits it, at the end of `exclude`, an instant before `call` sets `a.started`; every counted call therefore leaves a record (USE-1), whatever its result. Tokens count at `finish`, at `completed_at`. A window is exhausted at `sum >= limit`; for a key with a share, its own windows at `sum >= share × limit` exclude that key only (LIM-10). `429 quota_exceeded` carries the largest `wait` of the exhausted windows (LIM-9). Crossing 80 % returns a `Warning{Event: "quota_warning"}`: log, audit row, counter.

**Seed.** `serve` seeds every quota window of the published snapshot before the listeners bind, next to `usage.Restore`, and exits `1` when that fails, as for budgets (RES-9). A window declared or lengthened while running, or a key's windows when its share is added, is seeded at the first `Quota` or `Take` that needs it, outside the `State` mutex, with one seed in flight per provider. A seed calls `Seed(provider, now − longest window)`: `usage.Seeder(db)` reads `SELECT key_name, started_at, completed_at, input_tokens + output_tokens FROM usage WHERE billing = 'subscription' AND completed_at >= ? AND target >= ? AND target < ?` (the provider's prefix range, `"<name>/"` to `"<name>0"`) on `idx_usage_time`, and buckets requests by `started_at` and tokens by `completed_at`. Buckets are Unix-aligned, so a seeded window equals the live one for every committed record. A seed that fails while running logs an error and leaves the window unheld, so the next use tries again; until one succeeds the provider's targets are excluded with `503 usage_store_unavailable` (RES-10), so no window admits on a partial count. Records still in the writer's queue at that instant are not seen, which is possible only when a window or share is declared within a batch's 50 ms or during a store outage.

**What the proxy cannot know.** It counts its own traffic only; a plan shared with other clients, or windows the provider anchors or fixes to the calendar, make the provider's own `429` authoritative (§17 point 12). A plan's hard `429` passes through RES-1 like any `429`: it is eligible and fails the breaker (§17 point 14).

**`usage quota`.** `GET /admin/v1/usage/quota` lists, per subscription provider and per window, requests and tokens used, the limits, `used_pct` and `retry_after_s` when exhausted, then the same per key with a share against its fraction, with the note "counts this proxy's traffic only".

## 10. Telemetry

**Schema v1.** Annex D, the request span named `{operation} {alias}` (or `{operation}`), with these choices, each in §17: the names of `semantic-conventions-genai` at commit `e6658f14c9d9ea0760825f029cb82e7673f48fab` (`gen_ai.usage.cache_write.input_tokens`, `gen_ai.request.reasoning.level`), no `schemaUrl`; `gen_ai.operation.name` `chat` for both endpoints; `gen_ai.request.stream` only when true; `gen_ai.response.finish_reasons` a one-element array; `server.port` with `server.address`; cache and reasoning parts only when reported and non-zero; `gen_ai.request.reasoning.level` only when the outgoing body holds the dialect's level member (`reasoning_effort`, `reasoning.effort` or `output_config.effort`), with that member's string as the PAR-8 scanner reads it back, while a body that turns reasoning off without such a member carries only `stargaite.reasoning.effort.sent: none`; `gen_ai.provider.name` once a target is selected. Compatibility attributes for Langfuse, on every span: `langfuse.observation.metadata.stargaite_key`, `.stargaite_model` (when named), `.stargaite_target` (when selected), and `session.id` on attempt spans too. Resource: `service.name = stargaite`, `service.version`; scope `stargaite` with the version. A golden test fixes name and attributes for success, stream, error, refusal and attempt spans (AC-25).

| Attribute | Source |
|---|---|
| `stargaite.key`, `stargaite.model`, `gen_ai.request.model`, `stargaite.endpoint` | `rq.key.Name`, `rq.alias`, `rq.ep` |
| `stargaite.target`, `stargaite.upstream.model`, `gen_ai.provider.name` | The record's target and the id after its `/`; the provider's `gen_ai_provider`, else its name |
| `server.address`, `server.port` | Host and port of the provider's `base_url` (443 or 80 by scheme when absent) |
| `gen_ai.response.model`, `stargaite.model_mismatch`, `gen_ai.response.finish_reasons` | The record |
| `gen_ai.usage.*`, `stargaite.usage.estimated`, `stargaite.cost.usd`, `gen_ai.usage.cost`, `stargaite.cost.source`, `stargaite.billing` | The record; cost attributes only when `cost_usd` is not NULL |
| effort, cap and `stargaite.params.changed` attributes, `gen_ai.request.max_tokens` | The record's columns (PAR-8, RSN-6) |
| `stargaite.reasoning.dialect`, `stargaite.reasoning.stripped` | The target's dialect; `Obs.Stripped` |
| `stargaite.response.empty` | `content_empty` |
| `gen_ai.response.time_to_first_chunk` | Streams: `firstEvent − rq.started` in seconds on the request span (the operation, retries included); `− a.started` on an attempt span |
| `stargaite.attempts`, `error.type`, `http.response.status_code` | `len(rq.attempts)`; the kick-off §15 code; the status the request line logs (`499` for a client gone) |
| `session.id`, `gen_ai.conversation.id` | `rq.session` |
| `stargaite.schema.version`, `stargaite.version`, `stargaite.config.revision` | `1`, `Deps.Version`, `rq.snap.Revision` (TEL-13) |

```go
package telemetry

type Context struct {
	Trace  [16]byte
	Parent [8]byte // zero for a root
	Remote bool    // Parent came from a traceparent
}
func Parse(values []string) (Context, bool) // the traceparent header's values: exactly one, valid
func NewContext() Context                   // 16 random bytes, never all zero
func Sampled(draw uint64, rate float64) bool

type Attr struct {
	Key   string
	Value any // string, bool, int64, float64 or []string
}
type Span struct {
	Trace      [16]byte
	ID, Parent [8]byte
	Remote     bool // flags 0x200
	Name       string
	Kind       int // 2 SERVER, 3 CLIENT
	Start, End time.Time
	Attrs      []Attr
	Failed     bool
	Message    string
}

// Request is what done hands Build: the request's own facts and one Attempt
// per provider call, in order.
type Request struct {
	Context             Context
	Key, Alias, Session string
	Endpoint            domain.Endpoint
	Stream              bool
	Version             string
	Revision            int64
	Status              int    // what the request line logs
	Code, Message       string // the kick-off section 15 error; "" on success
	Started, Ended      time.Time
	Attempts            []Attempt
	Input, Output       []byte // content, set only under the opt-in (TEL-8)
	ContentMax          int64
}
type Attempt struct {
	Record   usage.Record
	Provider string // gen_ai.provider.name
	Host     string
	Port     int
	Dialect  string
	Stripped bool
}
func Build(r Request) []Span
```

`authenticate` becomes `(w, r, snap, ep) (*config.Key, telemetry.Context)`: the parsed context when `trust_traceparent` holds and the header is valid, else `NewContext()`; it writes the trace id to `x-stargaite-trace-id` as before, for `GET /v1/models` too, which emits no span.

**Spans.** `telemetry.Build(r Request) []Span` returns the request span (`SERVER`, child of the remote parent when there is one, else a root) and one `CLIENT` child `attempt {target}` for each attempt but the last, each with a fresh 8-byte id from `crypto/rand` (TEL-6). The request span takes the last attempt's attributes; usage and cost sit on an attempt span only when its record's status is `2xx`, and on the request span only for the last attempt's, so a backend that adds them up never counts a call twice (TEL-10). A failure sets status `ERROR` with the kick-off §15 message (already redacted), `error.type` and the HTTP status (TEL-5). Timestamps are `rq.started` to `done`'s clock, and `a.started` to `a.ended`.

**Trace context (TEL-6).** `Parse` accepts W3C Trace Context Level 1: exactly one `traceparent` header; version two lowercase hex digits, not `ff`; for `00`, exactly 55 characters; for a higher version, at least 55 and either exactly 55 or a `-` at offset 55, fields read at fixed offsets; trace id 32 and parent id 16 lowercase hex digits, neither all zero; flags two lowercase hex digits, read for nothing (TEL-7 ignores the sampled bit). Anything else, or `trust_traceparent: false`, starts a new trace of 16 random bytes; `tracestate` is never read. The span `flags` field is `0x01 | 0x100`, plus `0x200` on a request span whose parent is remote. `x-session-id` is used when it has 1 to 128 bytes of printable ASCII (`0x20` to `0x7E`), else ignored like any unknown header; it never comes from the body.

**Sampling (TEL-7).** `rate` is the lower of the key's and the alias's declared `trace_sample_rate` (the key's alone before the alias is known), `1` when neither is declared; `Sampled(draw, rate)` is `rate >= 1`, or `draw` below `rate × 2^64`, where `draw` is `rq.sample`; it is never taken from the trace id, which a client may set (TEL-6). Records, budgets, quotas, counters and logs never read it.

**Content (TEL-8, SEC-9).** `rq.content` is set at admission when the request is sampled and `key.Spec.ContentUntil` is after the service clock. Then the request span carries `gen_ai.input.messages`, the raw `messages` member of the serving attempt's outgoing body, and `gen_ai.output.messages`, the answer bytes relayed to the client: the sync body, or the data of each relayed event joined by `\n`, captured in `call` into `a.answer` up to `content_max_bytes` and no further. Each is a string cut at a UTF-8 boundary to `content_max_bytes`, with `stargaite.content.truncated: true` when cut, and the provider credential redacted as every relayed byte already is. No header value enters a span except a valid `x-session-id`. `keys create|set --content 24h` sends `{"content":"24h"}`; `admin` turns it into `content_until` on the service clock, and the change is audited as any key change; `--content=` clears it.

**Encoding.** `ExportTraceServiceRequest` per OTLP 1.11.1 JSON, from tagged structs on `encoding/json/v2`: lowerCamelCase keys, ids as lowercase hex, enums as integers, `startTimeUnixNano`, `endTimeUnixNano` and `intValue` as decimal strings, `[]string` as `arrayValue`. `Enqueue` encodes each span at once, so the queue's 32 MB bound counts real bytes.

```go
package config

type Exporter struct {
	URL          string                      // otlp_endpoint + "/v1/traces" (TEL-2)
	Header       http.Header                 // the static headers
	Secrets      map[string]transport.Secret // by header name: Authorization "Basic base64(user:password)" from basic_auth_env, and each header_env
	AllowPrivate bool
	Unresolved   []string // references that do not resolve
}
// Snapshot gains Exporter *Exporter (nil without otlp_endpoint) and NeverSince map[string]int64.

package telemetry

func NewExporter(cfg func() *config.Exporter, clients [2]*http.Client, version string, log *slog.Logger) *Exporter
func (e *Exporter) Enqueue(spans ...Span)             // never blocks; past 10,000 spans or 32 MB the oldest are dropped and counted
func (e *Exporter) Test(ctx context.Context) Result   // Outcome, Status, Message, TraceID
func (e *Exporter) Status() Status                    // sent, rejected by status, rejected by the backend, dropped, queued, last error
func (e *Exporter) Close(ctx context.Context) int     // flushes; the count dropped
```

**Exporter (TEL-1, TEL-3, TEL-12).** One goroutine sends a batch every 2 s or at 200 spans: `POST` to `Exporter.URL`, read from `cfg()` per batch so a publish changes it without a restart, with `Content-Type: application/json`, the static headers and each secret set by `SetHeader`, through `Clients[0]` or `Clients[1]` by `AllowPrivate` (SEC-4 at dial time, redirects never followed: AC-43). Timeout 10 s per request; at most 4 MiB of the answer is read, more is a non-retryable failure. A `2xx` is success for every span but `partialSuccess.rejectedSpans`, read as a number or a decimal string, which counts as rejected by the backend with its message; a `2xx` body of another shape (Langfuse's) is plain success. `429`, `502`, `503`, `504` and transport failures are retried three times at 1, 2 and 4 s with ±20 % jitter, a `Retry-After` in seconds or as an HTTP date honoured up to 30 s; any other status rejects the batch at once, counted by status. A batch whose credentials do not resolve is dropped and counted, with last error `credential_unresolved`. Ten consecutive failed batches log a warning, at most once a minute (TEL-3). Spans already queued when an exporter is unset are dropped and counted. Messages from the backend are cut to 200 bytes and redacted with every secret. The timings are package variables that tests lower: `batchEvery` 2 s, `backoff` 1, 2 and 4 s, `retryAfterCap` 30 s, `requestTimeout` 10 s, `warnEvery` 1 minute.

**`telemetry test` (TEL-2, kick-off §3.2).** Without a published exporter: `400 invalid_request` "no exporter is configured". `credential_unresolved` when `Unresolved` is not empty, without sending. Otherwise one span `stargaite telemetry test` under the reserved key `_telemetry` (no key name may start with `_`), through the same send function, without retries: `unreachable` for a transport failure (dial, DNS, TLS, the SEC-4 refusal, the timeout), `unauthorized` for `401` and `403`, `not_found` for `404`, `rejected` for any other status or rejected spans inside a `2xx` (with the backend's message), else `accepted` with the status and the trace id. The test span counts in `Status`.

**Langfuse preset (TEL-11).** `telemetry set --preset langfuse` fills `basic_auth_env: [LANGFUSE_PUBLIC_KEY, LANGFUSE_SECRET_KEY]` and `headers: {x-langfuse-ingestion-version: "4"}`; explicit flags override it. The endpoint is given in full, `https://<host>/api/public/otel`.

## 11. Failure policy and shutdown

**Store unavailable (USE-10, ADR-015).** From the third consecutive failed commit, `exclude` passes over `pay_per_use` targets (`503 usage_store_unavailable` when none is left, RES-10), `subscription` and `none` targets keep serving and counting in memory, and records wait in the queue; one that cannot wait goes to the critical log. A config commit fails meanwhile with `500`, as in phase 1.

**Exporter.** Export failures never reach inference: `Enqueue` never waits, the queue is bounded, and accounting never reads telemetry (PRI-10, AC-31).

**Start**, in `serve`: listener checks, TLS, `store.Open`, `config.Open`, `admission.New(cfg.Now, usage.Seeder(st.Read()))`, `usage.Restore` and the seed of every declared quota window (exit `1` when either fails), `usage.NewWriter(st.Write, log)`, `telemetry.NewExporter`, the admin key, `runServers`.

**Stop (RES-6)**, in order:

1. `runServers`: `Shutdown` stops accepting, `shutdownGrace` (30 s) for the requests in flight.
2. `halt(api.ErrStopping)`.
3. `Flight.Wait` against `drainWait` (5 s); every `finish` and `done` that ran has added its record.
4. `writer.Close` within `flushWait` (10 s, a package variable tests lower), as §5 defines it: the write in progress always ends and counts as stored; only entries never handed to `store.Write` are logged at critical level.
5. `st.Close()`.
6. `exporter.Close` within `exportWait` (5 s); dropped spans are logged once, with the count.
7. The `stopped` line.

The exit code is `1` for a drain timeout, records logged instead of stored, or a failed store close; dropped spans only log. A request still running after the drain wait whose `finish` runs after step 4 logs its record at critical level, so it is never lost silently.

## 12. Admin API and CLI

| Route | CLI | Notes |
|---|---|---|
| `GET /admin/v1/usage?since&key&model&target&by&compare_previous` | `usage show [--since 24h] [--key] [--model] [--target] [--by key,model,target,effort-sent] [--compare-previous]` | `since` a duration back from now or an RFC 3339 time; a read, not audited |
| `GET /admin/v1/usage/quota` | `usage quota` | §9 |
| `PATCH /admin/v1/telemetry` | `telemetry set [--preset langfuse] [--otlp-endpoint <url>] [--basic-auth-env U:P] [--header-env N=VAR] [--header N=v] [--allow-private-network] [--trust-traceparent=<bool>] [--content-max-bytes <n>]` | Merges into `settings.telemetry` through `Commit`: validated (TEL-2, SEC-4, SEC-5), audited, a new revision |
| `DELETE /admin/v1/telemetry` | `telemetry unset --yes` | Removes the block; trust and content size return to their defaults |
| `POST /admin/v1/telemetry/test` | `telemetry test` | §10; not audited (it changes nothing) |
| `GET /admin/v1/telemetry` | `telemetry status` | `Exporter.Status()` and the effective block |
| `POST /admin/v1/keys`, `PATCH /admin/v1/keys/{name}` | `keys create\|set <name> --content <duration>` | `content` member (§10) |

`admin.Handler(admin.Deps{Config, KeyHash, Log, Usage *sql.DB, Admission, Spans})` replaces the three arguments. `telemetry set` without a running server is not offered, as for every command but `serve`, `version` and `validate -f`. The renderer of `usage show` without `--json` is a table per window, one row per group; `telemetry test` prints the kick-off §3.2 line. Validation additions: a `content` duration must be positive; a quota window longer than `usage_days` warns (`quota_beyond_retention`, §17 point 12).

Settled in C22:

- **Admin.** `admin.Deps` is `{Config, KeyHash, Log, Usage}` in C22; `Admission` and `Spans` join with C26 and C29. `GET /admin/v1/usage` takes `since`, `key`, `model`, `target` and `by`, each at most once (`compare_previous` comes with C25); without `since` the window is the last 24 h on the service clock, and a duration that is not positive is a `400`.
- **CLI.** `usage show` sends only the flags given; without `--json` it prints one table, a header and a row per group, with `-` for a cost that does not apply or is unknown, and `no usage in the window` when there is none.
- **Stop.** With `flushWait`, a supervisor must allow `serve` more than 50 s to stop: 30 s grace, up to 5 s drain, and the 10 s flush plus the store's 5 s busy timeout, since `Close` waits for the write in progress; the README's `docker stop` example now gives `-t 60`.

## 13. Interfaces fixed now for phase 4

Phase 4 attaches without changing a signature that phase 3 ships:

1. **Embeddings and rerank.** `domain.Endpoint` gains `embeddings` and `rerank`; `relayFor` and the adapters gain their observers, which fill `Observed.Usage` (input only) and `Cost`; `usage.NewRecord` and the spans need nothing more, and `gen_ai.operation.name` takes `embeddings` and the custom `rerank` (annex D). The record's `stream` is false and `finish_reason` empty.
2. **`targets verify` (RSN-9).** It fills a `usage.Call` with `Key: "_verify"`, `Model: ""`, calls `Admission.Exhausted` with the global `Budget` only, `Admit` with the global `Scope` only (running its release when done) and `Take` with a `Plan` whose `Key` is `_verify` and `Share` 0, and hands the record to `Writer.Add`; its spans, if any, come from the same `telemetry.Build`.
3. **Metrics (OPS-8).** The endpoint renders `usage.Counters.Each` (by key and alias: requests by status, tokens by type, cost, length, empty, mismatches, parameter changes by action, unpriced), `admission.State.Windows` and budget figures (instance: budget and quota use) and `Exporter.Status` (exporter counters, spans included); `SEC-3`'s metrics token is its own concern.
4. **Retention (USE-11).** Deletes run as `store.Write` jobs in small batches, from `usage` and `usage_daily` by `completed_at` and `day`, never from `budget_totals`; it must keep the records of the longest quota window, which the seed reads (§17 point 12).
5. **Backup and secrets reload.** `VACUUM INTO` is store-level; `secrets reload` republishes the snapshot, which recomposes `Snapshot.Exporter`, and the exporter reads it per batch.

```go
package usage

type Counter struct{ Name, Key, Model, Label string } // Label: a status, a token type, an action
type Counters struct{ /* mutex, map[Counter]float64 */ }
func (c *Counters) Add(k Counter, v float64)
func (c *Counters) Each(fn func(Counter, float64)) // sorted by key
```

## 14. Testing and CI

**Mock provider.** `Server.UsageOnRequest bool`: when true the default chat stream sends its usage chunk only when the body's `stream_options.include_usage` is true (the `request` behaviour); false keeps today's `native` behaviour. `POST /v1/traces` joins the routes, default reply `200 {}`; `httptest` serves it in process, so the in-process OTLP receiver of TEL-14 is a mock. A test that scripts `Step`s and exports spans uses a second `mockprovider.Server` as receiver, separate from the provider mock, so the shared queue of phase-1 §9 never mixes provider steps with export steps (status, `Retry-After`, a `partialSuccess` body, `Delay`, `Hang`); that server's `Calls()` holds each export's exact bytes and headers. Costs, cache parts and odd usage shapes are scripted bodies.

**Clock.** `Config.Now` is a settable test clock; windows, periods, the 00:00 UTC rollover, quota windows of `5h` and `7d` and `content_until` are driven by it, never by sleeping. The writer's and the exporter's intervals (§5, §10) are lowered to milliseconds; no test asserts a wall-clock latency other than an upper bound.

| Layer | What it covers |
|---|---|
| Unit, table-driven | `NewRecord` per provider family and outcome (success, `429`, `5xx`, timer, client gone, cut stream, oversized); USE-3 output rule (xAI's example, Gemini, a plain provider); estimates; cost precedence and `PriceCost`; `window` against an exact sliding log on random events (never below it, at most one bucket above, `wait` never early); `Admit` all or nothing; `Exhausted`, `Quota`, `Take`, shares; warnings once per period; `Parse` over the W3C cases; `Sampled` at 0, 0.5, 1; the encoder's JSON rules |
| Writer | An injected failing `write`: three failures make `Available` false, the lowered `unavailableRetry`, recovery, queue overflow to the critical log; a write that outlasts the `Close` deadline and succeeds is not logged; the same batch committed twice inserts nothing twice and moves no aggregate (USE-2); `Add` racing `Close` under `-race` (AC-34) |
| `TestLedger` (`-race`) | Concurrent requests with fallbacks, cancels, cut streams and an `apply` that sets, changes and removes a `never` budget; afterwards: records = mock calls, `usage_daily` = `GROUP BY` over `usage`, memory budgets and quota windows = a fresh `Restore` and `Seed` from the store, slots back to zero |
| Restart | In process: close and reopen; budgets and windows equal. `cmd/stargaite`: `SIGKILL` after records are committed, restart, and the exhausted budget still answers `402`, the exhausted window `429` (RES-9, ADR-016) |
| `TestServeDrain` (extended) | After `SIGTERM`, every request line has its row in the store before `stopped` (AC-23) |
| Limits, budgets, quotas | AC-36, AC-37 (overshoot with five parallel requests within LIM-4's bound), AC-38 on the test clock |
| Corpus (C15, extended) | Record and span carry the same change list and "sent" values as the header and the mock's bytes (AC-09, AC-18) |
| Fault matrix (extended) | One record per call, the failed attempt's child span and its error (AC-21) |
| Spans | Golden per span shape; refusals at each step give one error span (AC-26); `traceparent` valid, malformed, uppercase, version `ff`, trust off (AC-27); `TestSamplingInvariant` runs one script at rates 0, 0.5 and 1 and compares records, `usage_daily`, budgets, windows and `Counters`, which hold no span count (AC-28) |
| `TestPlantedSecret` | A fake secret in a prompt and in an unknown header: absent from spans, logs and the store without opt-in; with an unexpired opt-in, only the prompt's appears, in `gen_ai.input.messages`, truncated at `content_max_bytes`; after expiry, none (AC-29) |
| Exporter | Every `telemetry test` outcome and the composed header (AC-24); traffic through the proxy with the receiver down, on `Hang` and on `500`: response latency within an upper bound of the no-exporter baseline, spans counted as rejected or dropped in `telemetry status` (AC-31, from C30); retries only on the OTLP statuses, `Retry-After` honoured, at the lowered timings; queue bounds by count and bytes; a resolver that answers private after validation, and a `302`, are refused (AC-43) |
| Imports | The rule of §3 |
| End to end | `tests/e2e` runs the extended quick start: `providers add mock … --billing pay_per_use` (replacing `--billing none` in `README.md`), `targets set mock/mock --pricing input_per_million=1,output_per_million=2,as_of=2026-11-01`, `telemetry set --otlp-endpoint http://127.0.0.1:4010 --allow-private-network`, `telemetry test` (`accepted`), then the `POST`, then `usage show --since 1h` (one request, its tokens and a `price_table` cost); after the stop, which flushes the exporter, it filters the mock's calls by path: exactly one `/chat/completions` call, and an export to `/v1/traces` holding a request span with `stargaite.key = my-agent` |

**Outside the default CI (TEL-14, PUB-5).** `tests/langfuse`, skipped unless `STARGAITE_LANGFUSE_URL`, `LANGFUSE_PUBLIC_KEY` and `LANGFUSE_SECRET_KEY` are set, runs the proxy in process against a self-hosted Langfuse at the version pinned in the job (v4.80.0 as read on 2026-11-14): with and without `x-langfuse-ingestion-version: 4`, one success, one stream, one fallback and one refusal; then reads the traces through Langfuse's public API and checks that each is readable, that key, alias and target filter, that the cost shown for a `pay_per_use` record equals its `cost_usd`, whether Langfuse shows an inferred cost for a `subscription` record without a cost attribute (expected when its catalogue matches the response model, §17 point 4, so the guide can state it), and how a trace whose root is an external `traceparent` renders (AC-30). It runs before any release that changes the span schema or the exporter. `tests/contract` gains the usage cases per target: stream usage arrives as the declared `stream_usage` says, the USE-3 total rule holds, and the cost report path is present. Neither needs anything in `make ci`.

## 15. Ordered changes

Each row is one pull request that leaves `make ci` green; each extends the quick start where it adds a command.

| Id | What it adds | Tests | Makes checkable | Depends on |
|---|---|---|---|---|
| C22 | The five codes in `domain.NewError`; `Observed.Cost` and adapter cost reading; the USE-3 output rule, `null` reasoning, Mistral fallback; clocks on `Config.Now`, `firstEvent`, `complete`; migration 002; `usage.Call`, `NewRecord`, `PriceCost`, `Counters`; a `Writer` with batches, insert-or-ignore, `usage_daily`, flush and the critical log line; `finish` records; `serve` starts and flushes it; `admin.Deps`; `usage show` sums; README: `pay_per_use`, prices and `usage show` | `NewRecord` tables; migration from a version 1 store; `Writer.Close` with a slow write; drain persists records; e2e | AC-23, AC-32, AC-33 (without injection), AC-40 (record), AC-09 and AC-18 (record) | phase 2 |
| C23 | USE-10: `Available`, the 5 s retry, overflow; `exclude` returns `*domain.Error`; `usage_store_unavailable`; `SIGKILL` restart test of records | Failing writer; RES-10 order with the store | AC-34 | C22 |
| C24 | `stream_options` injection in `Build`; `openai.NewStream(t, p, includeUsage)`; mock `UsageOnRequest` | Goldens; `explain` shows the change; the same upstream stream loses its usage chunk toward `stream_usage: request` and keeps it toward an undeclared one | AC-33 | C22 |
| C25 | `usage show` complete: percentiles, `effort-sent`, length and empty, `--compare-previous`, the daily fallback | Seeded windows | AC-35 | C22 |
| C26 | `admission`: `window`, `Admit`, slots, rpm, tpm; `h.admit`; release in `done`; LIM-7 at publish; README: `--rpm-limit` | Clocked tables, `-race` | AC-38 (without budgets) | C22 |
| C27 | Budgets: `NeverSince` in `config`, `budget_totals` in `writeChange` and the writer, `Exhausted`, `Spend`, `Restore`, warnings with audit rows | Overshoot; rollover; `never` across `apply`; a `never` budget stored before phase 3 survives a restart; restart | AC-37, AC-38 (an exhausted budget changes no rpm, tpm or slot count, and a limit refusal charges no budget) | C23, C26 |
| C28 | Quotas: windows, `Quota`, `Take`, shares, `Seed`, warnings; `usage quota` | Exhaustion, fallback, share, restart (seeded before the listeners bind), window and share declared while running, a failing seed | AC-36 | C23, C26 |
| C29 | `Snapshot.Exporter`; `telemetry` encoder, `Exporter`, `Test`, `Status`; `telemetry` routes and CLI with the preset; mock `/v1/traces`; `serve` order; README: `telemetry set` and `test` | Receiver tables; resolver and redirect | AC-24, AC-43, AC-31 (`telemetry status` counters and last error only) | C22 |
| C30 | Traffic spans: `Parse`, `x-session-id`, `Sampled`, `telemetry.Build` in `done`, compatibility attributes, the golden | Golden; refusal spans; sampling invariant; traffic with the receiver on `Hang` and `500` | AC-21, AC-25, AC-26, AC-27, AC-28, AC-31, AC-40 (span), AC-09 and AC-18 (span) | C26, C27, C28, C29 |
| C31 | Content opt-in: `--content`, capture in `call`, truncation | `TestPlantedSecret` | AC-29 | C30 |
| C32 | `tests/langfuse` and the `tests/contract` usage cases | Run by maintainers | AC-30 | C30 |

## 16. Risks of phase 3 and how each step retires them

| Risk | Sign | Retired by |
|---|---|---|
| Records lost at shutdown or under load | Spend and records disagree (R12) | C22: flush before `st.Close`, drain test reads the store; C23: overflow to the critical log, `SIGKILL` test |
| Memory and store counters drift | A restart changes a budget's figure | C27: `since_revision`, the same rule on both sides; `TestLedger` compares memory with a fresh restore |
| Double counting in aggregates | `usage_daily` above `GROUP BY usage` | C22: aggregates move only for inserted rows; the same batch committed twice |
| Wrong token totals for one provider family | tpm, quotas and percentiles off (xAI, Gemini) | C22: the total rule; `tests/contract` usage cases; §17 point 8 |
| Stream usage never arrives | Records `estimated` for a provider that would report (R10) | The preset data of §17 point 9; C24: injection; the contract cases |
| Budget overshoot from parallel requests | Spend above the limit (R8) | C26 and C27: LIM-7 at publish, charge before release, the overshoot test |
| Over-admission past a plan's window | More calls than the quota | C28: `Take` atomic with its check; the window never below an exact one |
| A plan exhausted upstream cycles the breaker | `503` and `429` alternate until the plan resets | Not retired: documented, §17 point 14 |
| The exporter slows inference | Latency rises with the backend down (R13) | C29: non-blocking `Enqueue`, bounded queue; C30: the AC-31 test with traffic |
| An accepted span is not a visible or filterable trace | Langfuse shows nothing, or cannot filter (R4) | C30 compatibility attributes; C32 against a real Langfuse |
| The conventions rename attributes again (R5) | A backend stops recognising a field | The pinned commit and the golden; a change is a new schema version |
| Content or a secret leaks into a span | A secret in the backend (SEC-9) | C31: opt-in, closed attribute list, planted secret test |
| Sampling changes accounting | Records differ with tracing off | C30: `TestSamplingInvariant`; nothing in accounting reads it |
| Stale or unrepresentable prices | Cost drifts from the invoice (R9) | Not retired by code: `cost_report` preferred, the guide documents the shapes (§17 point 11) |

## 17. Open points for the owner

Each has a recommendation; the design above already assumes it. The owner accepted the recommendations of points 1 to 21 on 2026-11-14, and the kick-off records those that change it.

**Telemetry against the current documentation**

1. **Conventions pin (TEL-9, annex D, R5).** Since core semantic conventions v1.42.0 the `gen_ai.*` attributes are deprecated in core and live in `semantic-conventions-genai`, which has no release, no tag and a schema URL of "TODO". Core v1.41.1 (schema `https://opentelemetry.io/schemas/1.41.1`) has `cache_creation.input_tokens` but no `gen_ai.request.reasoning.level`; the GenAI repository's main has `cache_write` and `reasoning.level` but no schema URL. Recommendation: pin commit `e6658f14c9d9ea0760825f029cb82e7673f48fab` without `schemaUrl`, as annex D anticipated, so no annex D row is dropped; record the commit in the schema file and move it only with a new schema version.
2. **Annex D against the pinned conventions.** `gen_ai.request.stream` is "Always" in annex D and "if and only if streaming" upstream; `gen_ai.response.finish_reasons` is a string array; `server.port` is required with `server.address`, which annex D omits; cache and reasoning parts are recommended only when non-zero; `gen_ai.provider.name` is Required upstream but annex D sets it only once a target is selected, so refused requests lack it; `gen_ai.request.reasoning.level` "SHOULD be the exact string sent", which RSN-6's `on`, `budget:<N>` and an off form are not; neither annex D nor TEL-* gives span kinds; `gen_ai.system_instructions` holds a separate system prompt, which annex D omits; `GET /v1/models` is authenticated, so TEL-5 reads as giving it a span, but annex D has no operation for it. Recommendation: the choices of §10 (stream only when true, a one-element array, `server.port`, non-zero parts, the provider name only once known, `reasoning.level` only when the body holds a level member, `SERVER` and `CLIENT`, no `system_instructions`, no span for `/v1/models`), recorded as annex D v1. The array departs from TEL-8 ("request attributes are a closed list of scalars"): it is the only non-scalar attribute, so amend TEL-8 to "a closed list of scalars and the conventions' string arrays"; the alternative is a scalar `stargaite.finish_reason` alone.
3. **AC-30 fails as annex D stands (TEL-11, OQ-1).** Langfuse puts unmapped attributes under `metadata.attributes`, which its documentation says cannot be filtered, and v4 needs trace-level values copied to every span. TEL-11 asks for compatibility attributes "in a new schema version". Recommendation: since v1 is not published, fold `langfuse.observation.metadata.stargaite_key`, `_model`, `_target` and `session.id` on every span into v1, and let C32 confirm; a trace-level form would come in v2 if the job shows it is needed.
4. **Langfuse guesses a cost when none is sent.** With no cost attribute, Langfuse infers one from its own price table by the response model, also for `subscription`, `none` and unpriced records, against USE-6 and USE-7. Any provided cost key, `0` included, disables the guess. Recommendation: keep annex D ("only when known") and document the guess; sending `gen_ai.usage.cost: 0` for `subscription` and `none` would show an invented $0, which PRI-11 forbids.
5. **TEL-1, TEL-12 and Langfuse's answers.** Langfuse never returns `partialSuccess`, rejects a whole batch with `400 {"error":…}` (not `google.rpc.Status`), and its `2xx` body is not an `ExportTraceServiceResponse`; so AC-24's "rejected inside a 200" is exercised against the mock only. TEL-12 says "three retries" without statuses; OTLP retries only `429`, `502`, `503`, `504`, honours `Retry-After`, adds jitter and caps the response at 4 MiB. Recommendation: the OTLP rules of §10, `Retry-After` capped at 30 s.
6. **"Valid traceparent" (TEL-6).** The kick-off does not define it; Level 1 is a Recommendation, Level 2 a draft that adds the random flag, and Level 1 is loose on trailing data. Langfuse expects a root span, so a request span under an external parent is not a root there (unverified, C32 checks). Recommendation: the Level 1 rules of §10, strictly, flags ignored.
7. **Content form (TEL-8).** The GenAI main branch now requires structured values for `gen_ai.input.messages` and `gen_ai.output.messages`; Langfuse stores a structured value as the JSON of its OTLP wrapper, and TEL-8 truncates bytes of a string. Recommendation: strings, as §10 builds them, a recorded deviation from the pinned commit while content stays opt-in.

**Usage and cost against the current documentation**

8. **USE-3 differs by provider on one protocol (ADR-005).** xAI's `completion_tokens` excludes reasoning (official example 32 + 9 + 94 = 135); the Gemini compatible endpoint is reported, unofficially, to do the same; Mistral reports cache reads at `prompt_token_details` (singular); Groq may stream usage only under `x_groq.usage` (unverified). Recommendation: output = `max(completion_tokens, total_tokens − prompt_tokens)`, reasoning only as reported (no derived remainder, which USE-3's "empty when not reported" forbids), the Mistral fallback, and Groq left to `tests/contract`.
9. **Preset data before the AC-53 golden moves (kick-off §7.2).** OpenAI streams usage only with `include_usage` (`request`); Anthropic, DeepSeek, DeepInfra and Mistral send it unasked (`native`); Moonshot, MiniMax, DashScope and Ollama need it (`request`), Gemini likely too; Groq, Together, Cerebras and Z.ai are unverified. DeepInfra's examples carry `usage.estimated_cost`, absent from its schema and named an estimate. `gen_ai_provider` well-known values at the pinned commit: `openai`, `anthropic`, `deepseek`, `gcp.gemini`, `x_ai`, `mistral`, `groq`, `ollama`, `moonshot_ai`; OpenRouter and MiniMax have none, so their custom names stay (`openrouter`, as the preset already sets). Recommendation: set those `stream_usage` and `gen_ai_provider` values, leave the unverified ones undeclared (an undeclared value injects nothing), and give DeepInfra no `cost_report`: an estimate is not the provider's billed figure USE-6 trusts.
10. **USE-8 scope.** USE-8 says "if no usage arrives" without a status condition and is silent on partial usage (an Anthropic stream cut after `message_start` has the input but not the output). Recommendation: estimate only after a `2xx`, keep reported parts and take the larger output (§6); a call without a `2xx` records zero tokens with `tokens_source: provider`, since the column has no third value.
11. **Prices annex A cannot express.** Cache writes priced by TTL (Anthropic 5 m and 1 h, OpenAI 30 m and 12 h, DeepInfra), long-context tiers that reprice the whole request (xAI, Gemini, OpenAI, Anthropic Haiku, Qwen, MiniMax: 33 to 80 % under), service tiers and fast modes the response reports (up to 2×), residency (1.1×), DeepSeek's peak hours (2×), per-call tool fees, and dated future prices (Gemini). Annex B's Sonnet prices are stale, as illustrative values may be. Recommendation: no new price fields in v1; a missing cache price falls back to the input price; the guide documents each case under R9 and prefers `cost_report`.

**Limits and quotas against the current documentation**

12. **Subscription windows (LIM-8, OQ-2, kick-off §7.3, R17).** No plan surveyed counts in a purely sliding window: Alibaba's week follows the calendar, Z.ai's and Kimi's weeks are anchored at subscription, MiniMax anchors both windows at first use; units are credits with multipliers or unpublished; plans are shared with other devices; the kick-off §7.3 MiniMax example (`5h:requests=1500`, `7d:requests=15000`) describes a plan no longer sold; several plans' terms now bar backends and non-interactive use, and the proxy sends Go's default `User-Agent` (PAR-14). A quota window longer than `usage_days` would lose its seed to retention in phase 4. Recommendation: keep sliding windows in v1 (they never admit more than the provider would, but may refuse up to one window after the provider resets); update the kick-off §7.3 example; state the terms plainly under R17; warn at validation when a window exceeds `usage_days`, and have phase 4 retention keep the longest window.
13. **`Retry-After` values (LIM-5, LIM-9).** No honest value exists for a parallel limit; quota values can be hours or days; the Anthropic SDK sleeps for any positive value, the OpenAI SDK refuses to retry above 120 s. The bucketed window may count up to one bucket of older traffic (1 s for rpm and tpm, 30 s for `5h`, about 17 minutes for `7d`). The IETF `RateLimit` fields remain a draft without a token unit. Recommendation: `1` for slots; for rates and quotas the window's `wait`, rounded up and uncapped; none on `402`; accept the bucket bound as "never more permissive"; emit no `RateLimit` fields; document SDK behaviour.
14. **Hard upstream `429`s.** Anthropic's spend cap, OpenAI's billing codes, and exhausted plans (Z.ai 1308 and 1310, MiniMax 2056, Kimi's monthly limit) look like any `429` and cycle the breaker until the reset (extends phase-2 §15 point 14); an upstream `503` with `Retry-After` becomes `502` without it (kick-off §15). Recommendation: keep classification status-based in v1 and document it; reading upstream limit headers is a later change with demand.

**Kick-off wording, schema and earlier promises**

15. **Annex A changes, shipped as migration 002.** Annex A lets a phase amend 001 until the first tag, but stores made by the phase 1 and 2 binaries are already at version 1 and would never run an amended 001, so the changes go in 002 and annex A should show them there: `billing` joins `usage_daily`'s key; `budget_totals` gains `since_revision`, and `config.Open` writes the row of every `never` budget declared before phase 3; `usage.error_type` holds the kick-off §15 code, as annex D's `error.type` does (the type follows from the code); no `upstream_model` column (derived from `target`) and no `session_id` column (spans carry it). Records sent to the critical log are absent from counters after a restart; cost stays `REAL`. Recommendation: accept.
16. **Defaults at publish.** LIM-7 applied at publish means a key cannot declare "unlimited", only a large number (OQ-7); `trust_traceparent` and `content_max_bytes` now default without a telemetry block, so `settings show` always shows them. Recommendation: accept both.
17. **Order and background work.** Kick-off §14.1 step 5 lists the dynamic checks as circuit, credential, quota, budget, store, while RES-10 reports budget, quota, store, then no target; kick-off §14.3 lists a "reset budgets" task at 00:00 UTC, which period keys make unnecessary. Recommendation: follow RES-10's order and amend kick-off §14.1 and §14.3.
18. **Estimated input of a translated request.** USE-8's byte-count estimate of a translated Anthropic request counts the Messages body the provider received, not the client's chat body. Recommendation: accept; it is what the provider read.
19. **Phase 2 promises the code does not keep.** Phase-2 §11 promised no shipped signature change, but `exclude` already returns `(gen, code)` and changes again here, and `authenticate`, `openai.NewStream` and `admin.Handler` change (§3); phase-2 §11 point 1 put cost walks "inside the adapter constructors", but the synchronous observers are plain functions that receive `p` (no change needed); phase-2 §11 point 2 put the attempt span in `finish`, which moves to `done`; phase-2 §11 point 4 promised an admission call "between `plan.Check` and the target loop", but `plan.Check` sits inside `check`, so the call goes between `check` and `route`; phase-2 §11 point 5 placed the trace "one line after authentication", but it is set inside `authenticate`, which `GET /v1/models` shares; request and attempt times used `time.Now`, not the service clock; `firstByte` is the time of the response headers, not of a first token; a `null` `reasoning_tokens` gave `&0`, not nil; phase-2 §9 promised the usage chunk swallowed without saying `NewStream` cannot know the client's flag; the CLI help states LIM-7's default, which nothing applied. Recommendation: accept the resolutions of §3, §4 and §6.
20. **Where a quota request counts.** LIM-8 says "when it is sent" and phase-2 §11 point 4 "at the line before `Do`"; `Take` counts at the end of `exclude`, an instant earlier, so the check and the count are atomic and every counted call leaves a record. Recommendation: accept.
21. **How the injected `include_usage` is listed (USE-8, PAR-3).** USE-8 says "listed as added"; a client's explicit `include_usage: false` that the proxy turns to `true` is a changed value, `translated` under PAR-3. Recommendation: `added` when the member was absent, `translated` only for an explicit `false` (§6); amend USE-8 to match.
