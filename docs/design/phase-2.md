# Phase 2 design

Status: the design phase 2 of §20 of `kick-off.md` is built from. It builds on the phase 1 code and on `docs/design/phase-1.md` (cited as "phase-1 §N"). Rule ids (PAR, RSN, RES, USE, LIM, SEC, OPS, ADR, AC, OQ) are the kick-off's and binding. Provider facts come from the public documentation as read on 2026-11-13, without live calls; "derived" marks a mapping that neither vendor documents. Where the kick-off is silent, outdated against that documentation, or contradicted by it, §15 says so with a recommendation, and the design below assumes the recommendation.

## 1. Scope and outcome

Phase 2 delivers `chat-completions`, `param-contract`, `reasoning-control`, `anthropic-adapter`, `messages-endpoint` and `resilience`. Of phase 3 it fixes only the seams of §11. At the end of phase 2:

- `POST /v1/chat/completions`, synchronous and SSE, toward `openai` targets (native) and `anthropic` targets (translated).
- `POST /v1/messages`, synchronous and SSE, toward `anthropic` targets only (native), with the Anthropic error envelope.
- The parameter contract: known-key schemas, `forward` and `drop` rules, PAR-4 capability skips, PAR-12, the three output-cap layers, the list of changes and the PAR-9 headers.
- Reasoning control: the seven dialects with fragments and maps, the two alias modes, inbound validation, the RSN-6 read-back, inline think-tag removal, and `stargaite models explain` (admin route and CLI row).
- Resilience: the RES-3 timers, fallback, the per-target circuit breaker, the RES-10 errors, cancellation within 1 s and the drain on `SIGTERM`.
- The quick start of `README.md` ends with a first answer from the mock provider, so PUB-3 is complete.
- No usage record, cost, limit, budget, quota, span or metric. Each provider call ends in `finish` and each request in `done`, which log it (OPS-9) and are where phase 3 hooks in (§11).

| Criterion | Checkable at the end of phase 2 | Left for phase 3 |
|---|---|---|
| AC-01 | Recorded OpenAI SDK requests (sync, stream, tools) through an alias answer bytes the SDK parses | Real SDK runs by maintainers (§15 point 21) |
| AC-02 | A recorded Anthropic SDK tool loop with thinking: the thinking blocks reach the mock byte-identical | As AC-01 |
| AC-04 | The README quick start, its `POST` included, run line by line by `tests/e2e` | — |
| AC-05 | A stream open across an `apply` that changes the alias's targets finishes on its snapshot | — |
| AC-06 | `403 model_not_allowed` on both inference routes (the rest landed in phase 1) | — |
| AC-08, AC-10, AC-11, AC-12 | Complete (the refusals of AC-08 and AC-10 already landed in phase 1) | — |
| AC-09 | Over a generated corpus, the inbound-outbound diff equals the header and the attempt's change list | The span and the usage record (§15 point 4) |
| AC-13, AC-14, AC-16, AC-17, AC-19, AC-20, AC-22 | Complete | — |
| AC-15 | Unchanged: the RSN-7 refusal landed in phase 1 | — |
| AC-18 | Effort and cap "sent" in the headers and in the attempt handed to `finish` equal the mock's bytes | Span and usage record |
| AC-21 | Re-translation per target on fallback; the RES-10 and §15 errors | The failed attempt as a child span |
| AC-23 | Upstream cancellation within 1 s measured at the mock; one terminal event; drain on `SIGTERM` | The usage record and the persisted batch |

## 2. Decisions

| Decision | Choice | Reason | Discarded |
|---|---|---|---|
| Request-level refusals | `plan.Check`, called once before the first `Build` by the data plane and by `explain`: PAR-1 nested types, PAR-2 `unknown_parameter`, `invalid_reasoning_effort`, `422 max_output_tokens_exceeded` | Phase-1 §13 point 28; `explain` then shares every refusal (PAR-10, ADR-009) | The PAR-2 check in the handler, which `explain` would not run |
| Target-level exclusions | `Build` returns a `Skip` (endpoint, PAR-4, PAR-12, target cap, RSN-1 budget, untranslatable content) | Phase-1 §9: a `Skip` is never an error of the request | Errors from `Build` |
| Body handling | Native paths edit the client's bytes in place with `params.Edit`; only chat toward `anthropic` builds a new body, reusing raw values | ADR-002; PRI-6 | Decoding bodies into structs |
| SSE | Reader and writer in `transport`; adapters take `(event name, data)` pairs | Annex C; the phase-1 §3 import rule holds without a new package | A separate `sse` package |
| Relay choice | One `switch` in `api` over the three fixed (endpoint, protocol) pairs, yielding a struct of method values | ADR-005 and ADR-006 fix the three pairs | An interface with three implementations |
| Phase 3 seam | Two unexported methods, `finish` per provider call and `done` per request | Least code; phase 3 fills their bodies (§11) | An `outcome` package with a `Sink` interface |
| Breaker | `routing.Breakers`, keyed by target id, outside the snapshot | RES-2 state must outlive `apply` (RES-7) | State in the snapshot |
| Timers | `context.WithCancelCause` plus `time.AfterFunc`; `httptrace` `GotConn` and `GotFirstResponseByte` stop the connect and first-byte timers | `GotConn` also covers the TLS handshake | A dialer timeout, which misses TLS |
| Server deadlines | Clear the read deadline once the body is read; a write deadline per attempt, moved after every upstream event read | The phase 1 server's 60 s `ReadTimeout` cancels the request context and would cut RES-3's 180 s (phase-1 §13 point 22) | Moving only the write deadline |
| Stream commit | The first upstream event that is neither a comment nor an error commits the client response; earlier failures go to `classify` and may fall back | RES-1 and RES-4: never buffer, never fall back after a byte | Waiting for the first content event, which buffers thinking |
| Anthropic version | `2023-06-01`, one constant | §15 point 1 | `2026-10-21` |
| OpenAI schema version | "The reference as read on 2026-11-13", a date constant in `params` | §15 point 2 | The `openai-openapi` repository, stale since 2025-04-29 |
| Both chat cap names | The larger is the requested cap; both are forwarded toward `openai` | §15 point 12 | `400`; the smaller |
| §15 error types | As written in the kick-off | §15 point 13: the owner decides | Provider-specific types per envelope |
| Think tags | Every `<think>…</think>` block, on decoded strings, on every path whose target declares `inline_think_tags` | RSN-12 literal, AC-19 | A leading block only; `openai` targets only |
| `include_usage` injection (USE-8) | Not in phase 2: usage is observed only when the client asked for it or the provider sends it unasked | USE-8 belongs to `usage-accounting` (§15 point 3) | Injecting it now |
| Body size (LIM-6) | `http.MaxBytesReader(max_body_bytes)` now | An unbounded read is a hole (§15 point 3) | Waiting for `limits` |
| Thinking on the translated path | Dropped | OQ-10 stays open (§15 point 17) | An opaque field |
| Forward rule on the translated path | The member's bytes are copied into the new body, listed `forwarded` | PAR-2: "sent to the provider unchanged" | Dropping it |
| Untranslatable chat content | `Skip` with code `unsupported_parameter` and the JSON path as `param` | PAR-12; phase-1 §9 | `400` |
| `explain` body | Sent and returned as a JSON string | Exact bytes; the strict admin decoder refuses repeated names inside a raw member | A raw JSON member |
| Dependencies | None added | Standard library covers SSE, timers, tracing | An SSE library |

## 3. Package map

| Package | Phase 2 responsibility | May import (internal) |
|---|---|---|
| `params` (new) | Scanner (phase-1 §9), `Edit` with parent creation, the dated schemas and walk lists, PAR-4 detection | — |
| `reasoning` (new) | RSN-5 resolution, RSN-4 translation, dialect forms as `params.Op`, RSN-6 read-back, the RSN-12 stripper | `domain`, `params` |
| `adapters/openai` (new) | Observation of native responses and chunks; think-tag edits of `content` | `domain`, `params`, `reasoning` |
| `adapters/anthropic` (new) | Chat-to-Messages request; Messages-to-chat response and stream; observation of native responses and events | `domain`, `params`, `reasoning` |
| `plan` (new) | `Check`, `Build`, the change header | `domain`, `params`, `reasoning`, `adapters/anthropic` |
| `routing` (new) | Circuit breakers by target id | — |
| `transport` | + SSE reader and writer; `func (s Secret) SetHeader(h http.Header, name, prefix string)` sets `name` to `prefix` plus the value, and the zero `Secret` sets nothing | — |
| `domain` | + `Endpoint`, the inference codes of §15, `Usage`, `Observed`; `Error` gains `RetryAfter string` (`json:"-"`, written as the `Retry-After` header by both envelopes); `func UpstreamError(status int, message, retryAfter string) *Error` gives `upstream_error` with the status and type of kick-off §15 for `400`, `404`, `413`, `422` and `429`, else `502` | — |
| `api` | Inference handlers, target loop, timers, deadlines, envelopes, relays, `finish` and `done` | all of the above, `config`, `transport` |
| `admin` | `POST /models/{name}/explain` | + `params`, `plan` |
| `cli` | One `models explain` row and its renderer | unchanged |
| `cmd/stargaite` | Both outbound clients, the data server's `BaseContext`, the drain wait | + `transport` |
| `tests/mockprovider` | SSE chunks, delay, gap, hang, cut; `Call.Ended`, `Call.Cancelled`; stream defaults | — |

Import rule, enforced by `internal/plan/imports_test.go` with `go/build`: `params`, `reasoning`, `adapters/...`, `plan` and `routing` never reach `store`, `transport`, `config` or `api`.

Differences from annex C and from phase 1, each with its reason:

- Target selection and RES-10 live in `api`, not `routing`: the dynamic exclusions of phase 3 (quota share, budgets) need the request's key and alias, which `routing` would reach only by importing `config`. `routing` holds the breaker alone.
- The think stripper lives in `reasoning`, so all three relays share it without one adapter importing the other.
- `adapters/openai` has no SSE and no embeddings: SSE is `transport`'s (annex C), embeddings are phase 4.
- `admission`, `usage` and `telemetry` do not exist yet; §11 names where they attach.
- Phase 1 signatures that change: `api.Handler(cfg, log)` becomes `api.Handler(Deps)` (clients, drain context and wait group); `plan.Input` gains `Client` (phase-1 §13 point 28); `params.Edit` creates missing parent objects (the effort member of `adaptive_effort` goes inside an `output_config` the client may not have sent); `mockprovider.Step` and `Call` gain fields (phase-1 §9 announced it); `config`'s list of keys a `forward` rule may not name gains the adapter's (phase-1 §13 point 6, accepted); `runServers` sets `BaseContext` and waits for handlers; `domain.Error` gains `RetryAfter` (RES-1 `429` pass-through; LIM-5 and LIM-9 in phase 3); in `cmd/stargaite` the server's `ReadTimeout` and `WriteTimeout`, `shutdownGrace` and the 5 s drain wait become package variables (`serverReadTimeout`, `serverWriteTimeout`, `shutdownGrace`, `drainWait`) so the C13 and C21 tests lower them to milliseconds, and serve tests do not run in parallel; `config.build` skips a target whose provider is not published (`if s.Providers[provider] == nil { continue }`), so `Target.Provider` is never nil, as phase-1 §9 guarantees (a stored configuration a newer binary finds invalid is still published, phase-1 §13 point 13). `transport.NewClient`, `config.Snapshot`, `Build`'s signature and `Outgoing` do not change.

## 4. Data plane and request flow

```go
package api

type Deps struct {
	Config  *config.Service
	Log     *slog.Logger
	Clients [2]*http.Client // [0] public addresses only, [1] allow_private_network; both built on systemResolver
	Stop    context.Context // the data server's BaseContext, cancelled when the RES-6 grace ends
	Flight  *sync.WaitGroup // Add(1) on the line before defer h.done(rq), after step 1 authenticates; done calls Done
}
func Handler(d Deps) http.Handler // health, GET /v1/models, POST /v1/chat/completions, POST /v1/messages

type request struct {
	id, traceID string           // traceID: 16 random bytes in hex until phase 3 reads traceparent
	ep          domain.Endpoint  // "chat" or "messages"
	snap        *config.Snapshot // loaded once (RES-7)
	key         *config.Key
	alias       string // the body's "model" string, set at step 3 before the lookup; "" when the body named none
	model       *config.Model
	body        *params.Body
	header      http.Header // anthropic-version and anthropic-beta, from /v1/messages only (PAR-14)
	client      *plan.Client
	attempts    []*attempt
	err         *domain.Error // what the client got; nil on success
	started     time.Time
}

type attempt struct {
	n                         int
	target                    *config.Target
	out                       *plan.Outgoing
	status                    int             // provider status; 0 when no response arrived
	end                       verdict         // §10
	cause                     error           // context cause: a timeout, client gone, shutdown; nil otherwise
	obs                       domain.Observed // §11
	written                   bool            // a byte reached the client: no fallback from here
	started, firstByte, ended time.Time
}
```

| Step (§14.1) | What happens | Refusal |
|---|---|---|
| 1 | Request id; key from `Authorization: Bearer`, or `x-api-key` on `/v1/messages` (ACT-1); trace id from here | `401 invalid_api_key` |
| 2 | `http.MaxBytesReader(max_body_bytes)`, read the body, clear the read deadline, `params.Scan` with the endpoint's inbound walk | `413 request_too_large`; `400 invalid_request` (PAR-1) |
| 3 | `model` must be a string; alias lookup; `key.Allows`; `plan.Check` | `400 invalid_request`; `404 model_not_found`; `403 model_not_allowed`; `400 unknown_parameter`, `400 invalid_reasoning_effort`, `422 max_output_tokens_exceeded` |
| 4 | Admission: phase 3 adds one call here (§11) | — |
| 5, 6 | Per target in order: `Build` (static `Skip`), then `exclude` (dynamic: provider `Disabled`, circuit open) | — |
| 7 | Credential header (`SetHeader(h, "Authorization", "Bearer ")` toward `openai`, `SetHeader(h, "x-api-key", "")` toward `anthropic`), timers, `Do`; relay the response; an eligible failure with nothing written goes to the next target | §10 |
| 8 | No target served: RES-10 when no provider was called, else the §15 error of the last call | §15 |

`done` is deferred right after step 1 authenticates, so it runs on every exit, a panic included. The target loop:

```go
for i, t := range rq.model.Targets {
	out, skip := plan.Build(plan.Input{Endpoint: rq.ep, Body: rq.body, Header: rq.header, Model: &rq.model.Spec,
		Upstream: t.Upstream, Target: &t.Spec, Provider: &t.Provider.Spec, Client: rq.client})
	if skip != nil { static = append(static, *skip); continue }
	if code := h.exclude(rq, t); code != "" { dynamic = append(dynamic, code); continue }
	a := h.call(ctx, rq, t, out, i+1) // timers, Do, relay; call defers finish(rq, a) as its first statement, so finish runs exactly once, a panic included
	if a.end.err == nil || a.written || !a.end.eligible { return }
}
// nothing served: §15 for the last call if one was made, else RES-10 from static and dynamic
```

RES-10 when no provider was called: every target skipped statically gives the `422` of the first target's `Skip`; otherwise the first dynamic reason of the first target that passed `Build`, which in phase 2 is always `503 no_target_available`. An alias with no published target answers `503 no_target_available`.

Response headers (PAR-9): `x-stargaite-request-id` always; `x-stargaite-trace-id` once authenticated; `x-stargaite-target`, `x-stargaite-reasoning-effort` (when `Effort.Sent` is not empty) and `x-stargaite-params-changed` (when not empty) from the attempt that served, or the last one tried. They are known after `Build`, so a stream sends them with its status line. Errors on `/v1/messages` add `x-stargaite-error-code`.

## 5. Parameter contract

**Schemas (PAR-1).** Go literals in `params`, each with its source and date:

```go
package params

type Schema struct {
	AsOf  string          // the reference read date: "2026-11-13" for both
	Known map[string]bool // top-level keys
	Walk  [][]string      // objects indexed and checked for repeats, inbound
}
var Chat, Messages Schema
const AnthropicVersion = "2023-06-01" // the header sent (PAR-14)
var ChatOut, MessagesOut [][]string // walks for reading an outgoing body back (PAR-8)
var ChatReply, MessagesReply [][]string // walks for observing a response or a chunk

type Features struct{ Tools, Structured, Strict, Vision bool } // PAR-4
func Detect(ep string, b *Body) Features
```

- `Chat.Known`: `messages, model, audio, frequency_penalty, function_call, functions, logit_bias, logprobs, max_completion_tokens, max_tokens, metadata, modalities, moderation, n, parallel_tool_calls, prediction, presence_penalty, prompt_cache_key, prompt_cache_options, prompt_cache_retention, reasoning_effort, response_format, safety_identifier, seed, service_tier, stop, store, stream, stream_options, temperature, tool_choice, tools, top_logprobs, top_p, user, verbosity, web_search_options` (37 keys).
- `Messages.Known`: `max_tokens, messages, model, cache_control, container, diagnostics, inference_geo, metadata, output_config, service_tier, speed, stop_sequences, stream, system, temperature, thinking, tool_choice, tools, top_k, top_p` (§15 point 1).
- Walks: chat `{stream_options}`, `{response_format}`, `{response_format,json_schema}`, `{tool_choice}`, `{tool_choice,function}`, `{tools,*}`, `{tools,*,function}`, `{messages,*}`, `{messages,*,content,*}`, `{messages,*,content,*,image_url}`, `{messages,*,tool_calls,*}`, `{messages,*,tool_calls,*,function}`; messages `{thinking}`, `{output_config}`, `{output_config,format}`, `{tools,*}`, `{messages,*}`, `{messages,*,content,*}`.
- Settled now (phase-1 §2, "to confirm in phase 2"): a test proves `"model"` and `"mod\u0065l"` in one object are refused, and fuzzing proves `Edit` leaves every byte outside its ops unchanged.

Settled in C11:

- **Tokenizer.** `jsontext.Decoder.ReadValue` returns the exact bytes of names and values and `InputOffset` their end, so the spans come from the tokenizer; `PreserveRawStrings` is an encoder option and is not used. The hand-written fallback of §14 is not needed.
- **Walks.** Every prefix of a walk path is indexed too. `"*"` stands for array elements only (a member named `"*"` is not walked), and an array is indexed only through `"*"`, so `Get("messages","0")` needs `{messages,*}`; an index is canonical decimal. `Error.Param` is a path such as `messages[0].role`, the message `repeated key "<path>"`.
- **`Edit`.** Ops apply in order, each to the result of the previous one, so the level member of `thinking_budget` goes inside the `thinking` the on part wrote. Each op finds its path with a fresh scan that refuses a repeated key in every object on the path, so an op may reach objects outside the body's walk (`thinking` on chat). An append goes right after the parent's last member, or right after `{`; deleting an absent member changes nothing. An empty path, a `Value` that is not valid JSON or repeats a name, or a path through a scalar, `null` or a missing array element is an error: `Edit` never replaces a parent the client sent.
- **Read walks**, which this section did not list: `ChatOut` and `MessagesOut` are `{reasoning}`, `{thinking}`, `{output_config}` (any dialect may serve either protocol; caps and fragments are top-level). `ChatReply`: `{choices,*}`, `{choices,*,message}`, `{choices,*,message,tool_calls,*}`, `{choices,*,delta}`, `{choices,*,delta,tool_calls,*}`, `{usage}`, `{usage,prompt_tokens_details}`, `{usage,completion_tokens_details}`, `{error}`. `MessagesReply`: `{content,*}`, `{usage}`, `{usage,output_tokens_details}`, `{message}`, `{message,usage}`, `{message,usage,output_tokens_details}`, `{delta}`, `{content_block}`, `{error}`.
- **Out paths in the inbound walks.** `Chat.Walk` adds `{reasoning}`, `{thinking}`, `{output_config}` and `Messages.Walk` adds `{reasoning}`, so each inbound walk holds its Out walk: a forward rule may carry these objects to the read-back, and a repeat inside one is refused at step 2 with 400 `invalid_request` (PAR-1), not left without an outcome after `Build`.
- **Index bound.** `Scan` indexes at most 100,000 members and elements per body and refuses one more with 400 `invalid_request`, an `Error` reason added to phase-1 §9: a dense 10 MiB body otherwise held about 44 times its size in memory at step 2, before admission. At the bound the index is near the default `max_body_bytes`. Settled in C12 (C11 recheck): the bound applies to that inbound `Scan` only. The proxy's own later scans of the body it accepted, the one inside each `Edit` op and `params.ReadBack(raw, walk)`, which reads the outgoing body back (PAR-8), are unbounded, so a body accepted at step 2 at exactly the bound never fails after `Build` for size; what `Build` adds is bounded by the declarations.
- **Vision inside `tool_result`.** `Messages.Walk` adds `{messages,*,content,*,content,*}`, and an `image` or `document` block inside a `tool_result` counts as `vision`: it is non-text input (PAR-4), which the list above missed.
- **`Detect`.** `tools` or `tool_choice` counts when present, whatever its value (`null` and `[]` included); string values are compared unescaped; an endpoint other than `chat` and `messages` uses no feature.

Settled in C12:

- **API.** `reasoning.Edit` carries one ordered `Changes []Change` instead of `Name`, `Action` and `Side`: the effort change, then each client member a form replaced (`thinking:forced`), then the side keys; `plan.Change` is an alias of `reasoning.Change`. `reasoning` also exports `Inbound(ep)`, `Dialect(r)`, the source and action constants and `BudgetPrefix`; `params` exports `ReadBack`.
- **Endpoint skips.** Until C17, chat toward an `anthropic` target is `Skip{endpoint_not_supported}`, as is a target whose `kind` is not `chat`.
- **A fault of the proxy.** An `Edit` or read-back failure, which `Check` and validation leave unreachable, is `Skip{internal_error, <reason>}`, never a panic.
- **`Check` types.** On messages `thinking` and `output_config` must be objects, since `Build` edits inside them, and `thinking.budget_tokens` a non-negative integer; a cap is a decimal integer, so `1.0` and `1e3` are `400 invalid_request`. `domain` gains only the inference codes phase 2 answers with; the limits of phase 3 add theirs.
- **Listing.** A client effort that leaves under its own name and level but whose dialect form adds members (messages `output_config.effort` toward `adaptive_effort` adds `thinking`) is `<inbound>:translated`; an alias effort toward a `none` target is `<inbound>:dropped` even when the client sent none; the effort change is not listed when a replaced member already carries its name; a `forced` `provider_default` deletes the client's effort, listed `dropped`.
- **Kept `thinking` (RSN-1).** A client `thinking` kept by §6 is not touched at all, a level member inside it included (`thinking_budget`). When the on part or a fragment rewrites the level member's object, the level member is always written into the new one.
- **RSN-3 refusals (§15 points 8 and 9).** They land in C12 with the fragment semantics of `Build`. An `on` fragment may not hold `reasoning_effort` (dialect `reasoning_effort`), `reasoning` (`reasoning_object`) or `output_config` (`adaptive_effort`). For `thinking_budget`, whose level lives inside its own on part, the literal rule would forbid every on fragment, so its fragment's `thinking` must instead be an object without `budget_tokens`. An alias whose `max_output_tokens` is at or below a `budgets` entry of a `thinking_budget` target is refused.
- **Read-back.** `Sent` checks a non-empty `off` fragment first, so an off fragment spelled like a level reads `none`.
- **Drops first.** `Build` applies the `drop` ops (PAR-2, PAR-12) with one `Edit` and reads the result back; the cap and the reasoning edit work on that body with a second `Edit`. So a dropped member the dialect writes is written again (`enable_thinking:dropped,enable_thinking:added`). The cap is not read from that body (owner decision, 2026-11-13, the literal reading of PAR-6): the requested cap stays the client's value even when a drop rule removed every member that held it, and it has two effects. Without an alias cap it is checked against the target's declared `max_output_tokens`, a client cap above it is `Skip{max_output_tokens_exceeded}`. And it is sent under the target's cap name (`output_cap_param`, default `max_tokens`, or `max_tokens` toward `anthropic`), listed `<name>:translated` after `<member>:dropped`, so the proxy never sends more than the client asked; the alias cap fills in only for a client that sent no cap at all. When that cap name is the dropped key itself, it is written again (`max_tokens:dropped,max_tokens:translated`), as a dialect key is. When one of two chat cap members survives the drops it is forwarded as is, and the requested cap is still the larger of the two.
- **Kept `thinking` and none.** A client `thinking` is kept only for a resolved level above `none`: `none` raised to a level by `can_disable: false` writes the on form. A client effort deleted while the kept `thinking` carries nothing the proxy wrote is `<inbound>:dropped`. An alias in `forced` mode, at any level, `provider_default` and a `none` target included, replaces any client control on messages (RSN-1): when the client sent one, its `thinking` is deleted, listed `thinking:dropped`, unless the form written writes `thinking` itself, which then replaces it, listed `thinking:forced`. A level member inside `thinking` (`thinking_budget` with an `on` fragment that writes none) goes into a new `thinking`. A `thinking` of type `adaptive` or `between_tools` with no client effort is no client control and follows §6.
- **`thinking_budget` map.** A `map` value must be `none` or a declared level, which has a budget (RSN-3 `invalid_value`).

**`Check`.**

```go
package plan

type Client struct {
	Effort   string // requested effort: a canonical level, "budget:<N>" (RSN-1), or "" for none
	Cap      *int64 // requested output cap; nil when the client sent none
	Stream   bool
	Usage    bool   // stream_options.include_usage is true (chat)
	Features params.Features
}
func Check(ep domain.Endpoint, b *params.Body, m *domain.Model) (*Client, *domain.Error)
```

In order: every top-level key outside `Known` without a rule in `m.Params` is `400 unknown_parameter` with its name; the types the proxy reads are checked (`stream` boolean, `stream_options` object with a boolean `include_usage`, each cap a non-negative integer, `messages` a non-empty array of objects with a string `role`, `thinking.type` a string and `thinking.budget_tokens` an integer when present, `response_format.type` a string), else `400 invalid_request` with the path; the effort (chat `reasoning_effort`, messages per RSN-1 and §6) outside the scale is `400 invalid_reasoning_effort` listing the accepted values; when the alias sets `max_output_tokens` (non-zero), a client cap above it is `422 max_output_tokens_exceeded`; without an alias cap, the target limit is checked per target in `Build`. The requested cap is `max_completion_tokens` or `max_tokens`, the larger when both are sent (§15 point 12); on messages it is `max_tokens`, where `0` is a cap (Anthropic's cache pre-warm).

**`Build`** applies, in this order, which is also the order of `Changes`: the protocol mapping (chat toward `anthropic` only, §7), the alias's rules, the cap, the reasoning edit (§6), and the side keys of a fragment (PAR-13). Native paths collect every op and call `Edit` once; the translated path scans the new body and then calls `Edit` once. The result is scanned with the outgoing walk, and every "sent" value is read from that scan (PAR-8).

| Rule | In `Build` |
|---|---|
| Endpoint (§7.1) | `messages` toward an `openai` target: `Skip{endpoint_not_supported}` |
| PAR-4 | A feature in `Client.Features` that the target declares `false`: `Skip{capability_mismatch, <feature>}` |
| PAR-2 | `drop`: the member is deleted, `name:dropped`; `forward`: kept (native) or copied byte for byte (translated), `name:forwarded`. A `forward` rule acts only on a key outside the endpoint's `Known` schema (known keys are forwarded under PAR-12 anyway); on the translated path only those keys are copied |
| PAR-12 | A request key in `unsupported_params`, or a key with no equivalent on the translated path (§7): deleted and `name:dropped` when the alias has a `drop` rule for it, else `Skip{unsupported_parameter, name}` |
| PAR-6 | Client cap: forwarded under its own name or names (`openai`) or as `max_tokens` (`anthropic`, inside `protocol:translated` on chat); when a drop rule removed every member that held it, sent under the target's cap name, listed `<name>:translated` ("Drops first" above). No client cap and an alias cap: the alias cap under the target's `output_cap_param` (default `max_tokens`) or `max_tokens`, listed `<name>:added`. No alias cap and a client cap above the target's declared `max_output_tokens`: `Skip{max_output_tokens_exceeded}`. Never lowered, never invented |
| RSN-1 | A client `budget:<N>` toward a target whose dialect is not `thinking_budget`: `Skip{capability_mismatch, thinking.budget_tokens}` |

PAR-4 detection: `tools` = `tools` or `tool_choice` present; `structured_output` = `response_format.type` `json_schema` (chat) or `output_config.format.type` `json_schema` (messages, §15 point 11); `strict_schema` = `strict: true` in `response_format.json_schema` or in a tool (`tools[].function.strict` on chat, `tools[].strict` on messages); `vision` = a content part of type `image_url`, `input_audio` or `file` (chat), `image` or `document` (messages).

**Change list (PAR-3).** `plan.Header(cs []Change) string` joins `name:action` with commas; the same string feeds the header, the `explain` block and, in phase 3, the usage record and the span. Names come only from schemas, alias rules and dialect tables, never from client text outside them, so the header cannot be injected. A member that `Build` would write byte-identical to what the client sent is neither rewritten nor listed.

**Validation additions (OPS-5).** For a target of protocol `anthropic`, the keys a `forward` rule may not name gain the adapter's written keys `system`, `stop_sequences`, `tool_choice`, `output_config` and `max_tokens` (phase-1 §13 point 6, accepted); the other keys it writes are in `Chat.Known`, where a `forward` rule has no effect. The new RSN-3 refusals of §15 points 8 and 9, accepted by the owner, ship in phase 2.

## 6. Reasoning control

```go
package reasoning

type Choice struct {
	Requested, Alias string // RSN-6 layers; "" when absent
	Level            string // what to send: a canonical level, "budget:<N>", or "" for nothing
	Source           string // client, alias_default, alias_forced, provider_default
}
func Resolve(client string, m domain.ModelReasoning) Choice

type Edit struct {
	Ops      []params.Op
	Name     string   // PAR-3 name of the effort change; "" when nothing is listed
	Action   string   // translated, added, forced, dropped
	Side     []string // fragment keys outside the dialect's keys (PAR-13), listed added, or forced when the client sent them
	Adjusted bool     // the level changed (RSN-4)
}
func Translate(c Choice, b *params.Body, ep domain.Endpoint, r *domain.Reasoning) Edit
func Sent(out *params.Body, r *domain.Reasoning) string // RSN-6 read-back
```

**Resolution (RSN-5).** `forced`: the alias's effort, the client's recorded as requested, source `alias_forced`. `default`: the client's effort when present (`client`), else the alias's (`alias_default`). `effort: provider_default`, or no effort anywhere: nothing is written, source `provider_default`. The source is finally set from the read-back: a body with no reasoning control is `provider_default`, whatever the cause (RSN-6).

**Translation (RSN-4)**, per target, on the resolved level `L`: an entry in `map` wins; `L` in `levels` is sent as is; `none` sends the off form unless `can_disable` is `false`, then the lowest level; any other level goes to the nearest one in `levels` by position in the canonical scale, the lower on a tie; with no `levels` it is sent as is. On/off dialects send the on form for every level but `none`. `Adjusted` is true when the level sent differs from `L`; the on form of an on/off dialect is not an adjustment.

| Dialect | Level member, written for `L` | On part, replaced by an `on` fragment | Off form, replaced by an `off` fragment | Change name |
|---|---|---|---|---|
| `none` | nothing; a client effort is deleted, listed `dropped` | — | nothing | the inbound name |
| `reasoning_effort` | `reasoning_effort: L` | — | `reasoning_effort: "none"` | `reasoning_effort` |
| `reasoning_object` | `reasoning.effort: L` | — | `reasoning.effort: "none"` | `reasoning.effort` |
| `thinking_toggle` | — | `thinking: {"type":"enabled"}` | `thinking: {"type":"disabled"}` | `thinking` |
| `enable_thinking` | — | `enable_thinking: true` | `enable_thinking: false` | `enable_thinking` |
| `thinking_budget` | `thinking.budget_tokens: budgets[L]` | `thinking: {"type":"enabled"}` | `thinking: {"type":"disabled"}` | `thinking.budget_tokens` |
| `adaptive_effort` | `output_config.effort: L` | `thinking: {"type":"adaptive"}` | `thinking: {"type":"disabled"}` and no effort | `output_config.effort` |

- **Writing.** The on part, or the `on` fragment in its place, is written first, each top-level member replaced whole; then the level member is set at its path, inside the object the on part wrote, creating missing parents. So `adaptive_effort` merges `effort` into the client's `output_config` and keeps `output_config.format`. An `off` fragment replaces the whole off form (§15 point 8). When a member of the on part, the off form or a fragment replaces a member the client sent and the bytes differ, the replacement is listed under that member's name: `thinking:forced` when an alias value replaced it, `thinking:translated` when the client's own effort did (PAR-3).
- **The client's own member.** Before writing, `Build` deletes the client's effort member (`reasoning_effort` on chat, `output_config.effort` on messages) unless the form writes that same member; when `output_config` is left empty it is deleted too. So the off form of `adaptive_effort` leaves no effort, and a respelled effort never travels twice.
- **The client's `thinking` on messages (RSN-1).** With a client effort above `none`, the dialect does not replace the client's `thinking`, which is forwarded unchanged; `none` writes the off form and lists the replaced `thinking` as `thinking:translated` (§15 point 10). Without a client effort, a client `thinking` of type `adaptive` or `between_tools` is kept toward an `adaptive_effort` target for any level but `none`, and only the level member is written (§15 point 10). A client `budget:<N>` toward a `thinking_budget` target is forwarded unchanged.
- **Actions (PAR-3).** A client effort that leaves under another name or level: `<inbound name>:translated`. An alias effort with no client effort: `<change name>:added`. A forced alias effort over a client's: `<inbound name>:forced`. Dropped toward `none`: `<inbound name>:dropped`. The inbound name is `reasoning_effort` on chat and `output_config.effort` on messages.

**Inbound (§9.1, RSN-1).** Chat reads `reasoning_effort`. Messages: `output_config.effort` on the scale is the requested level (so `none` and `minimal`, which Anthropic does not accept, pass PAR-1 and are translated per the target, §15 point 10); `thinking.type: disabled` without an effort is `none`; `adaptive` or `between_tools` without an effort is no client effort; `thinking.budget_tokens` is `budget:<N>`. An empty string, `default`, a non-string or any value off the scale is `400 invalid_reasoning_effort` before any provider is called (RSN-11).

**Read-back (RSN-6).** `Sent` reads the scanned outgoing body: the level member's value; `budget:<N>` for `thinking_budget`; `on` when the on part (or `on` fragment) is present on an on/off dialect; `none` when the off form, or every member of the `off` fragment, is present byte for byte; else empty. It never looks at the `Choice`.

**`models explain` (PAR-10).** `POST /admin/v1/models/{name}/explain` with `{"endpoint":"chat"|"messages","effort":"…","max_output_tokens":N,"body":"<exact bytes>"}`, all optional. The base body is `body`, else the fixed minimal request `{"model":"<alias>","messages":[{"role":"user","content":"hi"}]}`; `effort` becomes an `Edit` op on `reasoning_effort` (chat) or `output_config.effort` (messages), and `max_output_tokens` one on `max_completion_tokens` (chat) or `max_tokens` (messages). Then `Scan`, `Check` and `Build` per target of the live snapshot, exactly as the data plane. A `Check` refusal answers the data plane's §15 error. The answer is one block per target, in order: `target`, then either `skipped {code, param}` or `effort {requested, alias, sent, source, adjusted}`, `max_output {requested, alias, sent}`, `changes` and `body`, the string of `Outgoing.Body`. Dynamic exclusions (credential, circuit) are not shown: `explain` sends nothing. Explain is a read and is not audited (SEC-7). The CLI row sends `--body <file>` as that string and renders the layout of §3.2.

**Think tags (RSN-12).** One `reasoning.Stripper` per choice index (chat) or text block (messages), fed decoded text, so a JSON-escaped spelling such as `<think>` matches. It removes every `<think>…</think>` block; an unclosed block is removed to the end of the content; it holds back at most 7 bytes that could start or end a tag and releases them with the next text, or at the end. It reports `Stripped`. The relay re-encodes the edited string through `Edit`; a chunk whose text becomes empty is still sent, so no finish or tool-call chunk is lost. It runs for a target that declares `inline_think_tags: true`, on chat to `openai`, chat to `anthropic` and messages to `anthropic`, on synchronous and streamed text.

## 7. Adapters: openai and anthropic

**`openai` native (chat to an `openai` target).** `Build` edits in place. The relay sends the provider's body unchanged, except think-tag edits of `choices[i].message.content` or `choices[i].delta.content`, and observes: `model`, each `finish_reason`, `usage` (from the body, or from any chunk whose `usage` is not null, which covers the usage chunk and providers that send usage unasked), visible bytes and tool calls.

| OpenAI usage | `domain.Usage` (USE-3) |
|---|---|
| `prompt_tokens` (cached and cache-write tokens included) | `Input` |
| `prompt_tokens_details.cached_tokens`, `.cache_write_tokens` | `CacheRead`, `CacheWrite` |
| `completion_tokens` | `Output` |
| `completion_tokens_details.reasoning_tokens` | `Reasoning` (nil when absent) |

**Chat to `anthropic` (`protocol:translated`, listed once).** `adapters/anthropic.FromChat(b *params.Body) (body []byte, noEquivalent []string, untranslatable string)`, where `untranslatable` is the JSON path of the first content that cannot be translated (`""` when none) and `Build` turns it into `Skip{unsupported_parameter, path}`, writes members in a fixed order: `model`, `max_tokens`, `system`, `messages`, `tools`, `tool_choice`, `stop_sequences`, `stream`, `temperature`, `top_p`, `output_config`, then forwarded members in the client's order. JSON string values are copied as raw tokens; the URL is `base_url + "/v1/messages"`, with `anthropic-version: 2023-06-01`.

| Chat | Messages |
|---|---|
| `system` and `developer` messages, wherever they stand | top-level `system`, one text block each, in order |
| `user` string content; `text` parts | string content; `text` blocks |
| `image_url` with a `data:` URI of `image/jpeg`, `png`, `gif` or `webp` | `image` with a `base64` source and `media_type`; `detail` dropped |
| `image_url` with an `http(s)` URL | `image` with a `url` source |
| assistant `content` and `tool_calls[{id, function:{name, arguments}}]` | text block, then `tool_use {id, name, input}`; `arguments` must parse as a JSON object |
| consecutive `tool` messages (`tool_call_id`, string or text parts) | one `user` message whose `tool_result` blocks come first, before any text |
| `max_completion_tokens` or `max_tokens` (the larger) | `max_tokens` |
| `stop` (string or array) | `stop_sequences` (array) |
| `tools[{type:"function", function:{name, description, parameters, strict}}]` | `tools[{name, description, input_schema, strict}]`; absent `parameters` gives `{"type":"object"}` |
| `tool_choice` `none`, `auto`, `required`, named | `{type:none}`, `{type:auto}`, `{type:any}`, `{type:tool, name}` |
| `parallel_tool_calls: false` | `disable_parallel_tool_use: true` in `tool_choice` (`{type:auto}` when absent and `tools` is present); absorbed with no effect when `tool_choice` is `none` or `tools` is absent |
| `response_format` `json_schema` | `output_config.format {type:"json_schema", schema}` |
| `stream`; `stream_options.include_usage` | `stream`; the latter is consumed by the relay (usage chunk), not sent |
| `temperature`, `top_p` | copied (§15 point 6) |
| `reasoning_effort` | written by the target's dialect (§6), listed `reasoning_effort:translated` |
| `n: 1`, `response_format {type:"text"}`, `parallel_tool_calls: true`, `logprobs: false`, `store: false`, `stream: false` | absorbed by the mapping: no effect |

No equivalent, so the target is skipped unless the alias drops the key (PAR-12): `n` above 1, `logprobs: true`, `top_logprobs`, `logit_bias`, `frequency_penalty`, `presence_penalty`, `seed`, `prediction`, `audio`, `modalities`, `verbosity`, `web_search_options`, `moderation`, `prompt_cache_key`, `prompt_cache_options`, `prompt_cache_retention`, `store: true`, `metadata`, `service_tier`, `user`, `safety_identifier`, `functions`, `function_call`, `custom` tools, `allowed_tools`, `json_object`. Nested content that cannot be translated skips with its path as `param`: `input_audio` and `file` parts, other image media types, a message `name`, `tool_calls` whose `arguments` are not a JSON object, a role other than `system`, `developer`, `user`, `assistant` or `tool` (§15 point 5).

**Response back (derived mapping).** Synchronous: `{id, object:"chat.completion", created, model, choices:[{index:0, message:{role:"assistant", content, tool_calls}, finish_reason, logprobs:null}], usage}`. `content` is the text blocks joined, `null` when there is none; each `tool_use` becomes `{id, type:"function", function:{name, arguments}}` with the `input` bytes as the `arguments` string; thinking and `redacted_thinking` blocks are dropped (OQ-10); `created` is the relay's clock at the response. Streaming in §9.

| `stop_reason` | `finish_reason` |
|---|---|
| `end_turn`, `stop_sequence` | `stop` |
| `max_tokens`, `model_context_window_exceeded` | `length` |
| `tool_use` | `tool_calls` |
| `refusal` | `content_filter` |
| `pause_turn` | `stop` (§15 point 5) |

| Anthropic usage | `domain.Usage`, and the chat usage written back |
|---|---|
| `input_tokens + cache_creation_input_tokens + cache_read_input_tokens` | `Input`; `prompt_tokens` |
| `cache_read_input_tokens`, `cache_creation_input_tokens` | `CacheRead`, `CacheWrite`; `prompt_tokens_details.cached_tokens`, `.cache_write_tokens` |
| `output_tokens` | `Output`; `completion_tokens`; `total_tokens` is `prompt_tokens + completion_tokens` |
| `output_tokens_details.thinking_tokens` | `Reasoning`; `completion_tokens_details.reasoning_tokens`, only when reported |

**`anthropic` native (messages to an `anthropic` target)** is §8. The recorded `FinishReason` is always the provider's own value (USE-3): `stop_reason` on the translated path too.

## 8. The /v1/messages endpoint

- **Key.** `x-api-key` is accepted besides `Authorization: Bearer` (ACT-1); with both, `Authorization` wins.
- **Targets.** An `openai` target is `Skip{endpoint_not_supported}`; all skipped gives `422 endpoint_not_supported` (§7.1).
- **Body.** Edited in place: `model`, the cap when the alias adds it, the reasoning edit (§6), the alias's rules and PAR-12 drops. Everything else, `system`, mid-conversation `system` messages, `thinking` and `redacted_thinking` blocks with their signatures, travels byte for byte (RSN-12). The credential goes in `x-api-key`.
- **Headers (PAR-14).** The client's `anthropic-version`, else `2023-06-01`; the client's `anthropic-beta` when sent. Nothing else from the client.
- **Relay.** The synchronous body is sent unchanged; stream events are re-emitted with their own event names and data (§9). Observation: `message_start` usage, cumulative `message_delta` usage (the latest non-null value of each field wins), `stop_reason`, `model`; text blocks and `text_delta` for visible bytes and the stripper; `tool_use` blocks for the tool count.
- **Errors.** The Anthropic envelope, `{"type":"error","error":{"type":…,"message":"<code>: <message>"}}`, the §15 type, and `x-stargaite-error-code` (§15). A provider error passed through keeps the provider's message after the code.
- **AC-02.** The test scripts a mock reply with a `thinking` block, its signature and a `tool_use`; the recorded client turn sends them back with a `tool_result`; the second call received by the mock holds those blocks byte-identical.

## 9. Streaming

```go
package transport

type Event struct {
	Name    string // the event: field; "" when absent (OpenAI)
	Data    []byte // data: lines joined by "\n"
	Comment bool   // a ":" line, relayed as a keepalive
}
type EventReader struct{ /* bufio.Reader, the size bound */ }
func NewEventReader(r io.Reader, max int64) *EventReader // max: settings.max_response_bytes, per event
func (r *EventReader) Next() (Event, error)              // io.EOF at a clean end; ErrEventTooLarge
func WriteEvent(w io.Writer, e Event) error              // the caller flushes and moves the write deadline
```

**The relay** for each attempt is chosen by one `switch` in `api` over `(endpoint, protocol)`:

```go
type relay struct {
	sync  func(body []byte, now time.Time) ([]byte, domain.Observed, error)   // error: the 2xx body is not usable
	event func(name string, data []byte, emit func(name string, data []byte) error) error
	end   func(emit func(name string, data []byte) error, clean bool) (domain.Observed, error) // called exactly once per stream attempt, on every exit; clean is false on a cut, a timer, client gone or shutdown, and then end emits nothing and only returns what was observed
}
// chat/openai: openai.Stream; chat/anthropic: anthropic.ChatStream; messages/anthropic: anthropic.Stream
func openai.NewStream(t *domain.Target, p *domain.Provider) *Stream
func anthropic.NewChatStream(t *domain.Target, p *domain.Provider, includeUsage bool) *ChatStream
func anthropic.NewStream(t *domain.Target, p *domain.Provider) *Stream
```

Each adapter type keeps the stream's state and the per-choice strippers; `api` binds their methods into the struct. The synchronous observers take the same `(t, p)`; phase 2 reads only `t.InlineThinkTags` (and `p` in phase 3, §11 point 1). No adapter imports `transport`.

**Commit (RES-1, RES-4).** A `2xx` whose `Content-Type` is not `text/event-stream` is `502 upstream_error`. Otherwise the relay reads events; comments only reset the idle timer. The first non-comment upstream event that is not an error event (an `event: error`, or a `data:` object with a top-level `error`) commits the response. An error event that arrives before the commit is not relayed: it ends the attempt and `classify` decides by §15 point 15; an OpenAI-format error object, whose status is undocumented, is not eligible and gives `502 upstream_error`. The commit writes status `200`, `Content-Type: text/event-stream`, `Cache-Control: no-cache`, the PAR-9 headers, then the translated event, flushed. From that byte on, `written` is true and no fallback happens. Every event is written and flushed as soon as it is translated; nothing is held beyond the 7 bytes of a stripper (§6). After each upstream event is read, whether or not it produces a write, the write deadline moves to now plus `stream_idle_ms` plus 5 s (SEC-6). A failed write to the client ends the attempt with the cause "client gone" (RES-5).

**OpenAI native.** Each `data:` chunk is relayed as is, unless a stripper edits `delta.content`; comments are relayed; `[DONE]` is relayed and ends the stream. After the commit, a chunk with a top-level `error` object is relayed and ends the stream as an upstream error.

**Anthropic to chat** (`anthropic.ChatStream`, derived mapping):

| Upstream event | Chat chunks written |
|---|---|
| `message_start` | one chunk `delta {role:"assistant", content:""}`; `id` from the message, `created` from the relay clock, `model` from the message; usage kept |
| `content_block_start` text; `content_block_delta` `text_delta` | `delta {content}`, through the stripper when declared |
| `content_block_start` `tool_use` | `delta {tool_calls:[{index:k, id, type:"function", function:{name, arguments:""}}]}`; `k` counts tool calls only, not content blocks |
| `content_block_delta` `input_json_delta` | `delta {tool_calls:[{index:k, function:{arguments: partial_json}}]}`, in order |
| thinking, `signature_delta`, `redacted_thinking`, `content_block_stop` | nothing |
| `message_delta` | nothing yet: `stop_reason` and cumulative usage kept |
| `message_stop` | `delta {}` with the mapped `finish_reason`; the usage chunk (`choices: []`) only when the client set `stream_options.include_usage`; `data: [DONE]` |
| `ping` | the comment `: ping` |
| `error` | the terminal error event below |
| an unknown event | nothing (Anthropic may add types) |

Every chunk carries `id`, `object:"chat.completion.chunk"`, `created` and `model`. RES-4 holds by construction: each written event is one complete JSON object; tool-call deltas keep upstream order, and each `tool_use` start produces exactly one opening delta.

**Anthropic native.** Every event, `ping` included, is re-emitted with its name and data unchanged, except stripper edits of `text_delta`; an unknown event type is relayed. `message_stop` ends the stream normally.

**Terminal error (RES-4).** When a committed stream fails (upstream cut, idle timeout, upstream `error` event, oversized event, shutdown), the proxy writes one event in the inbound format and closes: on chat `data: {"error":{"message":…,"type":…,"code":…}}` with no `[DONE]` after it (§15 point 15); on messages `event: error` with the Anthropic envelope. An upstream error event already in the inbound format (OpenAI native, Anthropic native) is relayed as that event. The attempt's `obs` keeps what was observed: `end` runs with `clean` false, and `api` then writes the terminal event when a byte was already written.

**Usage in streams.** Phase 2 adds no `stream_options` (§15 point 3). Usage is observed when the client asked for it on chat, when the provider sends it unasked, and always on Anthropic streams; otherwise `Observed.Usage` stays nil and phase 3 estimates (USE-8).

## 10. Resilience

**Timers (RES-3).** Read from `rq.snap.Settings.Timeouts`, per attempt:

```go
ctx, cancel := context.WithCancelCause(rq.ctx) // rq.ctx derives from r.Context(), itself from Deps.Stop
connect := time.AfterFunc(connectTimeout, func() { cancel(errConnectTimeout) })
trace := &httptrace.ClientTrace{
	GotConn:              func(httptrace.GotConnInfo) { connect.Stop() }, // dial and TLS handshake done
	GotFirstResponseByte: func() { firstByte.Stop() },                    // streams only
}
// sync: total := time.AfterFunc(totalTimeout, func() { cancel(errTotalTimeout) }), from the attempt's start
// stream: firstByte armed at the start; idle armed at the headers and Reset after every event read
```

| Timer | Applies to | Runs | Ends with |
|---|---|---|---|
| connect, 5 s | both | until `GotConn` | `errConnectTimeout` |
| first byte, 30 s | streams | until the first byte of the response headers | `errFirstByteTimeout` |
| idle, 120 s | streams | between the headers and each event | `errIdleTimeout` |
| total, 180 s | sync | the whole attempt, body read included | `errTotalTimeout` |

The total is per attempt (§15 point 22). Each timer is eligible for fallback and, when last, gives `504 upstream_timeout` or, once committed, the terminal event.

**Server deadlines** (phase-1 §13 point 22). Once the body is read, `http.ResponseController.SetReadDeadline(time.Time{})`: otherwise the server's 60 s `ReadTimeout` fires the connection's background read, which cancels the request context of any longer request. Before each attempt the write deadline is set to now plus the total (sync) or the first-byte plus idle timeouts (stream), plus 5 s; streams then move it after every upstream event read (§9). A regression test runs a synchronous call longer than the server's `ReadTimeout` and `WriteTimeout`, with `serverReadTimeout` and `serverWriteTimeout` lowered (§3), and checks it is not cut (AC-22).

**How an attempt ended.** One private function classifies it; nothing else decides fallback, breaker or error:

```go
type verdict struct {
	eligible bool            // RES-1 kind of failure: try the next target when nothing was written
	breaker  routing.Verdict // Pass, Fail, None
	err      *domain.Error   // §15 error answered when no target is left; nil on success
}
func classify(status int, cause error, header http.Header, body []byte, relayErr error) verdict
```

| End of the attempt | Eligible | Breaker | §15 error when last |
|---|---|---|---|
| `2xx` relayed to its end | — | Pass | — |
| Dial refused, connection reset, SEC-4 dial refusal | yes | Fail | `502 upstream_error` |
| A RES-3 timer | yes | Fail | `504 upstream_timeout` |
| `429` | yes | Fail | `429 upstream_error`, the provider's `Retry-After` in `Error.RetryAfter` |
| `5xx`, `529` included | yes | Fail | `502 upstream_error` |
| Sync body cut while reading (RES-4); stream cut or idle before the first event | yes | Fail | `502 upstream_error`, `504` for the timer |
| An upstream `error` event before anything was written | Anthropic: by its error type (§15 point 15); OpenAI: no | as eligible | `502 upstream_error` |
| `400`, `404`, `413`, `422` | no | Pass | the provider's status and message, `upstream_error` |
| `401`, `403` | no | Pass | `502 upstream_auth_error`, fixed message; the provider's text only in the log, rate-limited |
| `3xx` (never followed, SEC-4), any other status | no | Pass | `502 upstream_error` |
| `2xx` over `max_response_bytes`, not JSON, failing the scan, or not SSE for a stream | no | Pass | `502 upstream_error` |
| Committed stream that fails (cut, idle, upstream error event) | no fallback possible | Fail for the eligible kinds | terminal event (§9) |
| Client gone; shutdown | no | None | client gone: nothing to answer; shutdown: §15 point 15 |

**Fallback (RES-1, RSN-10).** The next target gets a fresh `Build`, so effort and cap are resolved and read back again for it, and its attempt carries its own `Outgoing`. No retry against the same target.

**Circuit breaker (RES-2).**

```go
package routing

type Verdict int // Pass, Fail, None
type Breakers struct{ mu sync.Mutex; by map[string]*breaker; now func() time.Time }
type breaker struct{ fails [5]time.Time; next int; openUntil time.Time; trials, passed int }
func NewBreakers(now func() time.Time) *Breakers
func (b *Breakers) Acquire(target string) bool        // false: open, or half-open with its two trials out
func (b *Breakers) Release(target string, v Verdict)  // exactly once per Acquire that returned true
```

Five `Fail` within 30 s open the target for 20 s; then half-open lets two requests through and treats it as open for every other; two `Pass` close it, one `Fail` reopens it for 20 s; `None` returns the trial's permit, so a cancelled trial never leaves it half-open forever. The values are constants (RES-2). The map is keyed by target id; `Handler` builds it once with `routing.NewBreakers(d.Config.Now)`, so it lives for the life of the process and `apply` keeps it (RES-7); a removed target's entry stays until the process ends. `exclude` calls `Acquire`; `finish` calls `Release`.

**Cancellation (RES-5).** The attempt context derives from `r.Context()`; a disconnect cancels it, the transport closes the upstream connection at once, and `finish` still runs with the cause. The mock's `Call.Ended` proves the upstream side ended within 1 s.

**Shutdown (RES-6).** `runServers` creates the data server with `BaseContext` returning `Deps.Stop`. On `SIGTERM`: `Shutdown` stops accepting and waits up to 30 s; when the grace ends it cancels `Stop`, so every attempt still running ends with the cause "shutdown", committed streams write their terminal event, and each request runs `finish` and `done`; `serve` runs `Flight.Wait` in a goroutine that closes a channel, and selects on it and on a `drainWait` (5 s) timer, then (phase 3: flushes the usage writer) closes the store.

**Configuration changes (RES-7, RES-8).** A request keeps the snapshot it loaded at step 1 for every attempt; fallbacks are always within the alias's validated contract (PAR-5).

## 11. Interfaces fixed now for phase 3

Phase 3 attaches at five places, without changing a signature that phase 2 ships:

```go
package domain

type Usage struct { // USE-3: Input and Output are inclusive totals; the others are parts of them
	Input, CacheRead, CacheWrite, Output int64
	Reasoning                            *int64 // nil when not reported
}
type Observed struct {
	ResponseModel string // USE-4
	FinishReason  string // the provider's own value (USE-3)
	Usage         *Usage // nil when none arrived: phase 3 estimates (USE-8)
	VisibleBytes  int64  // visible text relayed, for the estimate
	ToolCalls     int    // with VisibleBytes 0: content_empty (USE-9)
	Stripped      bool   // RSN-12
}
```

1. **Extraction.** The adapters' observation code is the only reader of response bytes, synchronous or streamed. Phase 3 adds `Cost` to `Observed` there, read from the provider's `cost_report.path` with the same `Scan`, so cost and tokens come from one pass (USE-6): phase 3 appends the parent objects of `p.CostReport.Path` to the reply walk inside the adapter constructors (§9), with no signature change. Phase 2 reads no cost.
2. **Outcome of a call.** `func (h *handler) finish(rq *request, a *attempt)` is deferred by `call` as its first statement and runs exactly once per provider call, after its last byte, its abort or a panic, before the next attempt starts. Phase 2 logs one line (request id, trace id, key, alias, attempt, target, status, cause, `duration_ms` from `a.started` to `a.ended`, `first_byte_ms` when `a.firstByte` is set, response model, finish reason, usage when observed, visible bytes, tool calls, stripped). Phase 3 adds the usage record (USE-1, USE-5: `a.out` holds the layers and the changes, `a.target.ID` and `a.target.Upstream` the target and the upstream model id; `a.obs` the tokens and finish reason; `rq.snap.Revision` the revision), the in-memory counters (budget, `tpm_limit`, quota tokens), and the attempt span.
3. **Outcome of a request.** `func (h *handler) done(rq *request)` is deferred once per authenticated request, after its last `finish`. Phase 2 logs the request line (request id, trace id, key, alias, attempts, final status or error code, `duration_ms` from `rq.started`) and calls `Flight.Done`; `rq.alias` is the alias attribute and log field, also when `rq.model` is nil (OPS-9, TEL-4). Phase 3 releases the parallel slots there (LIM-4 and LIM-6: after the charge in `finish`, on every exit) and ends the request span (`len(rq.attempts)` is `stargaite.attempts`, 0 for a request refused before any provider).
4. **Admission and exclusions.** Admission is one call between `plan.Check` and the target loop (step 4), whose release `done` runs. `func (h *handler) exclude(rq *request, t *config.Target) string` returns a RES-10 code; phase 2 checks `Disabled` and the circuit, phase 3 inserts quota, budget and usage-store checks before them, in RES-10's order, and counts a quota request at the line before `Do` (LIM-8).
5. **Trace.** `rq.traceID` is set at one line after authentication; phase 3 replaces the random value with the `traceparent` parse (TEL-6) and reads `x-session-id` there.

## 12. Testing and CI

**Mock provider.**

```go
type Step struct {
	Status int
	Header http.Header
	Body   []byte
	Chunks [][]byte      // raw SSE bytes, each written and flushed after the headers; Body is ignored
	Delay  time.Duration // before the status line: a late first byte
	Gap    time.Duration // between chunks: silence
	Hang   bool          // after the body or the chunks, block until the request's context ends
	Cut    bool          // after the body or the chunks, abort the connection (panic(http.ErrAbortHandler))
}
type Call struct {
	Method, Path string
	Header       http.Header
	Body         []byte
	Ended        time.Time // when the handler returned
	Cancelled    bool      // the request's context ended before the reply was complete
}
```

A `Step` with `Chunks` defaults its `Content-Type` to `text/event-stream`. An empty queue answers a request with `"stream":true` with a default stream per route (chat chunks with a usage chunk and `[DONE]`; the Messages event sequence).

| Layer | What it covers |
|---|---|
| Unit, table-driven | `Check` per refusal; `Build` per `Skip` code; PAR-4 detection per feature; RES-10 order; `classify` per row of §10; the breaker on an injected clock (five failures, both trial outcomes, a `None` trial); the zero `Secret` sets no header; a snapshot whose target names an unpublished provider |
| Golden (`-update`) | `plan/testdata/<endpoint>/<dialect>/<case>.json`: seven levels × default forms and fragments, with `map`, without `levels`, `can_disable: false`, alias modes; chat-to-Messages request pairs; Messages-to-chat response pairs; the error envelopes of both formats |
| Stream fixtures | `*.up.sse` to `*.down.sse` for the three relays and the stripper, each replayed split at every byte offset with identical output |
| Fuzz | `Scan` and `Edit` (bytes outside the ops unchanged; escaped repeated names refused); the stripper and the translators under random re-chunking: same output, every event valid JSON, tool calls neither duplicated nor reordered |
| Corpus property (AC-09) | Generated chat and messages bodies through every dialect and both protocols: each top-level member that differs between `Call.Body` and the inbound body is in the header, each listed name differs or is a protocol mapping, and the same list is on the attempt |
| Byte identity (AC-17) | `explain` bodies equal `Call.Body` for the same input, per target |
| Fault matrix (AC-21 to AC-23) | Faults: refused dial, private dial, `Delay` past first byte, `Hang`, `Gap` past idle, `Cut` before and after commit, in-stream error, `429`, `500`, `529`, `400`, `401`, `302`, oversized body, client cancel, shutdown. Each runs synchronous and streaming, with one target and with a fallback, timeouts in milliseconds, and asserts the envelope or terminal event, `Calls()`, the breaker by behaviour (after five failures the next request answers `503 no_target_available` without a call to the mock), one `finish` per call and one `done` per request; settled with the owner on 2026-11-13: no test-only seam injects a panicking relay, so a panic has no row here, and that `finish` and `done` run on a panic rests on each being one deferred statement, checked in review |
| Deadlines | A sync call longer than `serverReadTimeout` and `serverWriteTimeout` set to 200 ms, through `serve`, is not cut; a stream across `apply` finishes (AC-05) |
| SDK wire fixtures (AC-01, AC-02) | Request bytes recorded from the official Python and Node SDKs (sync, stream, tools, a thinking tool loop), replayed by an HTTP client; responses checked against the SDKs' field sets |
| Imports | `go/build` test of the rule in §3 |
| End to end | `tests/e2e` learns `curl … -d '<json>'` as a `POST` and checks `200`, a `choices` array, the PAR-9 headers, and that the mock received one call with the upstream model id |

`tests/contract` arrives with C19: the per-target cases of §21's test strategy that phase 2 serves (simple, streaming, cancellation, structured output, tools when declared, each declared level, errors), against the mock in CI and against a live target when maintainers set its variables. It is where OQ-10 and the open points on provider behaviour get settled. `make ci` is unchanged; no test needs the network or a secret.

## 13. Ordered changes

Each row is one pull request that leaves `make ci` green. Reasoning sits inside `Build` before the data plane exists, so no published build sends a request without the alias's effort (RSN-5, PRI-5).

| Id | What it adds | Tests | Makes checkable | Depends on |
|---|---|---|---|---|
| C11 | `params`: scanner, `Edit` with parent creation, schemas, walks, `Detect` | Fuzz; escaped repeated name; PAR-1 tables | AC-08 (unit) | phase 1 |
| C12 | `domain.Endpoint` and the inference codes of §15 with their status and type in `domain.NewError`; `reasoning` (without the stripper); `plan.Check`; native `Build` for both protocols: rules, PAR-4, PAR-12, cap, effort, read-back; `plan.Header`; import test | Dialect goldens; four mode combinations; cap table; skip table | AC-10, AC-13, AC-14, AC-16 (unit) | C11 |
| C13 | `domain.Observed` and `Usage`, `Error.RetryAfter` and `UpstreamError`; `transport` `SetHeader`; both clients in `serve`; `api.Handler(Deps)`; chat sync to `openai`: steps 1 to 8, connect and total timers, deadlines, `MaxBytesReader`, envelopes, PAR-9, `finish` and `done`; README `POST`; e2e | End to end; header matrix; deadline regression; `403` on inference | AC-01 (sync), AC-04, AC-06 (chat), AC-08, AC-10, AC-11, AC-12, AC-14 | C12 |
| C14 | `transport` SSE; `openai.Stream`; commit rule; first-byte and idle timers; terminal events; mock `Chunks`, `Delay`, `Gap`, `Hang`, `Cut` and stream defaults | Byte-split fixtures; stream across `apply` | AC-01, AC-05, AC-12 (stream) | C13 |
| C15 | `explain` route and CLI row; corpus property for chat to `openai` targets | Bytes against `Calls()`; corpus | AC-09, AC-17, AC-18 (headers and attempt) for chat to `openai` | C13 |
| C16 | `reasoning.Stripper` on `openai` native, sync and stream | Split, unclosed, escaped, several blocks | AC-19 | C14 |
| C17 | `anthropic.FromChat`, sync response back with the stripper on text blocks; the adapter's keys in the `forward` check | Request and response goldens; skip table; think tags in a text block; corpus and `explain` byte identity extended to chat to `anthropic` | AC-19, AC-20 (sync) | C12, C13, C16 |
| C18 | `anthropic.ChatStream` | Fixtures, re-chunking fuzz | AC-20 | C14, C16, C17 |
| C19 | `/v1/messages`: `x-api-key`, RSN-1, PAR-14, Anthropic envelope, native relay and stripper; `tests/contract` | Thinking tool loop; SDK fixtures; `401` and `403 model_not_allowed` with `x-api-key` and with `Authorization` in the Anthropic envelope; corpus and `explain` byte identity extended to `/v1/messages` | AC-02, AC-06 (messages), AC-09, AC-17 | C18 |
| C20 | `classify`, fallback, `routing.Breakers`, `exclude`, RES-10 | Fault matrix (without cancel and shutdown); clocked breaker | AC-11 (fallback), AC-21, AC-22 | C19 |
| C21 | Cancellation, `BaseContext`, drain and `Flight`; mock `Ended` and `Cancelled` | Disconnect within 1 s; `SIGTERM` with an open stream | AC-23 | C20 |

## 14. Risks of phase 2 and how each step retires them

| Risk | Sign | Retired by |
|---|---|---|
| `jsontext` cannot give the byte spans or the unescaped names the scanner needs | Escaped repeats accepted; `Edit` changes other bytes | C11: fuzz and the escaped-name test; fallback is a hand-written scanner behind the same API (phase-1 §9). Settled in C11: retired, no fallback (§5) |
| A dialect form or fragment combination is spelled wrong | A live provider rejects a level | C12 goldens for every combination; R3 stays: `targets verify` (phase 4) and `tests/contract` (C19) |
| An alias effort leaves unsent in a published build | Requests without effort | C12 puts reasoning into `Build` before C13 serves any request |
| `explain` and the data plane drift | AC-17 fails | C12: one `Check` and one `Build`; C15 compares bytes with `Calls()` |
| The 60 s server read and write timeouts cut long requests | Sync calls end at 60 s | C13: read deadline cleared, write deadline per attempt, regression test |
| The SSE transformer breaks events or reorders tool calls | Clients fail to parse | C14 and C18: byte-split replay and re-chunking fuzz |
| The derived Anthropic mapping differs from what SDKs expect | AC-20 clients fail | C17 and C18 goldens against SDK field sets; live runs through `tests/contract` |
| Translated thinking tool loops fail (OQ-10) | Provider `400` on the second turn | Not retired: documented, with `/v1/messages` as the path for those loops (§15 point 17) |
| A half-open breaker never closes | A target stays excluded | C20: `None` returns the permit; clocked tests |
| Cancellation leaves upstream connections open | Provider keeps generating | C21: mock `Ended` within 1 s |
| Shutdown loses the outcome of open streams | `finish` missing for the last requests | C21: `Flight` wait before `store.Close` |
| Provider schemas move (new keys, new versions) | Ordinary requests get `400 unknown_parameter` (R7) | Dated schemas updated each release; the `forward` rule meanwhile |
| Strip state machine holds text too long | Stream stalls | C16: at most 7 bytes held, flushed on the next text or at the end |

## 15. Open points for the owner

Each has a recommendation; the design above already assumes it. The owner accepted the recommendations of points 1 to 22 on 2026-11-13, and the kick-off records those that change it.

**Scope against §20 and §21**

1. **Anthropic API version (PAR-1, PAR-14).** The kick-off pins "a published version" without naming it. Anthropic now lists `2026-10-21` (recommended) and `2023-06-01`. `2026-10-21` answers `400` to `temperature`, `top_p` and `top_k` on every model, to a `document` block in an assistant turn and to citations in user text; Bedrock offers it only from Claude 4.7, and Anthropic-compatible third parties were not checked. Recommendation: pin the header `2023-06-01` as one constant (`params.AnthropicVersion`); the key list `Messages.Known` is pinned separately, to the reference read on 2026-11-13 (its `AsOf`), sampling keys `temperature`, `top_p` and `top_k` included, because PAR-14 forwards the client's own version, so the version actually sent can differ from the pin.
2. **OpenAI has no versioned schema (PAR-1).** Chat Completions has no version header and the `openai-openapi` repository stopped on 2025-04-29, without `moderation`, `prompt_cache_*`, `safety_identifier`, `verbosity`, `web_search_options`, `reasoning_effort` `xhigh` and `max`, or `include_obfuscation`. Recommendation: the pin is "the reference as read on 2026-11-13", recorded in `params` and moved each release (R7).
3. **Phase 3 items moved or kept.** LIM-6 (body size, `413`) belongs to `limits`, but an unbounded read is a hole. USE-8's `include_usage` injection belongs to `usage-accounting`. Recommendation: `MaxBytesReader` ships in phase 2 (phase 3 adds slots and rates only); the injection stays in phase 3, which adds `stream_options` in `Build` (already in the `forward` collision list) and the swallowing of the usage chunk.
4. **Criteria that cite phase 3 artefacts.** AC-09, AC-18, AC-21 and AC-23 are assigned to phase 2 features but cite spans and usage records. Recommendation: phase 2 checks them through the headers and the attempt handed to `finish`; phase 3 extends the same tests to the record and the span, fed by the same `plan.Outgoing` and `domain.Observed`.

**Contract gaps against the provider documentation**

5. **§7.1 understates the translation.** Besides the system prompt, tool calls, usage and finish reason, the adapter must merge consecutive `tool` messages, put `tool_result` blocks first, parse `arguments`, split `data:` images, map `stop`, `tool_choice` and `parallel_tool_calls`, renumber tool-call deltas, synthesize `created`, and sum Anthropic's input tokens. Several chat features have no faithful equivalent: `n > 1`, logprobs, `json_object`, `allowed_tools`, a message `name`, refusal text, `pause_turn` (server tools only, which the translated path never sends). Recommendation: adopt §7 of this design as the mapping in §7.1, with those cases skipped under PAR-12 and `pause_turn` mapped to `stop`. Forced `tool_choice` (`any`, `tool`) is a provider `400` on several current Claude models: data, not code (the operator declares `unsupported_params: [tool_choice]` or `tools: false`).
6. **PAR-12 and sampling on the translated path.** Claude 4.7 and later answer `400` to a non-default `temperature` or `top_p` even on `2023-06-01`, and `temperature` there is 0 to 1 against OpenAI's 0 to 2. Recommendation: forward both; targets of those models declare `unsupported_params: [temperature, top_p, top_k]`, and aliases whose clients send them add `drop` rules, so PAR-12 drops and lists them instead of a provider error.
7. **`adaptive_effort` "off" form (§9.1).** `thinking: {type: disabled}` is a `400` on Opus 5.5, Fable and Mythos 5.x and Mythos Preview; Sonnet 5.5's off is `{type: between_tools}`, accepted only at effort `high` or below; Opus 5 and Haiku 5.5 accept `disabled` only at `high` or below, so `can_disable` can depend on the level, which RSN-3's boolean cannot express. Recommendation: data, no code: `can_disable: false`, or an `off` fragment such as `between_tools`; the level dependence stays inexpressible in v1. `thinking_budget` is now a `400` on Claude 4.7+ and 5.x and deprecated on 4.6: the dialect stays for the older models.
8. **RSN-3 is silent on an `on` fragment for a levelled dialect.** Recommendation: the rule of §6 (the fragment replaces the on part, the level member is still written), plus a new RSN-3 refusal of an `on` fragment key equal to the level member's top-level key, which would be overwritten. This is also how an operator sends `display: "summarized"`: Claude 5 models default to `omitted`, so thinking text comes back empty otherwise.
9. **`thinking_budget` budget against the cap.** Anthropic requires `budget_tokens` of at least 1024 and below `max_tokens`; RSN-3 does not check it. Recommendation: a new RSN-3 refusal of a budget at or above the alias's `max_output_tokens`; a client cap below a budget is left to the provider's `400`.
10. **RSN-1 gaps.** RSN-1 covers neither `thinking.type: between_tools` nor `thinking.display`, and Anthropic's effort has no `none` or `minimal`, which §9.1's scale accepts on messages. Recommendation: `between_tools` without an effort is no client effort; `display` and `between_tools` are forwarded unchanged; an inbound `none` or `minimal` is valid and translated by the target's declaration, also on the native path. This diverges from RSN-1 ("`thinking` is forwarded unchanged"): an inbound `output_config.effort: none` writes the target's off form, which replaces the client's `thinking`, listed `thinking:translated`; the owner must accept it. RSN-1 is also silent on a client `thinking` without effort when the alias supplies one: the client's `thinking` of type `adaptive` or `between_tools` is kept toward an `adaptive_effort` target and only the level member is written; toward other dialects the on part replaces it, listed `thinking:forced` (§6).
11. **PAR-4 on /v1/messages.** Structured output there is `output_config.format`, and `strict` is a tool field. Recommendation: count `output_config.format` as `structured_output` and `tools[].strict` as `strict_schema`.
12. **Both chat cap names in one body.** PAR-6 does not say which is requested. Recommendation: the larger, so no value above the alias cap passes without `422`; both forwarded unchanged toward `openai`; the larger becomes `max_tokens` toward `anthropic`.
13. **§15 types against the providers.** `503 overloaded_error` is not an OpenAI type (OpenAI uses `service_unavailable_error`; Anthropic's `overloaded_error` comes with `529`); Anthropic's `413` type is `request_too_large` and its timeout type `timeout_error`; OpenAI has no `402` and reports exhausted credit as `429 insufficient_quota`; Anthropic error bodies carry a top-level `request_id`. Recommendation: keep §15 as written until the owner decides whether to align types per envelope; SDKs map errors by status, so nothing breaks meanwhile.
14. **Upstream spend limits.** Anthropic answers an account's own spend limit with `400` (never eligible) and a tier spend cap with `429` without `Retry-After` (a fallback that will not clear); `409` and `504` become `502`. Recommendation: keep RES-1 and §15 status-based in v1 and document both cases in the guide.
15. **Stream errors without a documented format.** OpenAI documents no in-stream error for Chat Completions (its SDK treats a `data:` object with a top-level `error` as one); §15 has no code for a stream cut by shutdown; RES-1 lists statuses, not an Anthropic `error` event that arrives after `200` but before any byte reached the client. Recommendation: the chat terminal event is `data: {"error":{message,type,code}}` with no `[DONE]`; shutdown answers `api_error`/`internal_error` with the message "server stopping", as an envelope or a terminal event; an early `error` event is not relayed and is classified by Anthropic's documented status for its type (`overloaded_error` 529, `api_error` 500, `rate_limit_error` 429: eligible; others not); an early OpenAI error object, whose status is undocumented, is not eligible and gives `502 upstream_error`.
16. **AC-19 "quoted literally", and other tag shapes.** Recommendation: read it as JSON-escaped spellings of the tags, all removed as in §6 (every block, literal RSN-12). Gemini's reported `<thought>` tags and Mistral's typed `thinking` content chunks are outside RSN-12 in v1; a per-target tag name would be a later change.
17. **OQ-10.** Recommendation: phase 2 drops thinking blocks on the chat-to-`anthropic` path; chat tool loops with thinking toward such targets stay unsupported, and `/v1/messages` is the documented path for them, until `tests/contract` settles OQ-10 against a live provider.
18. **OpenAI flagship models and Chat Completions (OQ-5).** GPT-6 Astra, GPT-6.1 Sol and GPT-6.1 Terra support tool calling only on the Responses API; GPT-6 Sol and Luna only at `reasoning_effort: none`; Astra rejects `none`, 6.1 Sol rejects `none` and `minimal`. Recommendation: data, not code: those targets declare `tools: false` and their real `levels`, so PAR-4 skips them; record the evidence under OQ-5.
19. **Preset data (phase-1 §13 point 24), before the AC-53 golden freezes.** The `openai` preset sets no `target_defaults.dialect`, so new OpenAI targets get `none` and drop every effort; Groq, DashScope, MiniMax and Cerebras prefer `max_completion_tokens`, and DashScope's `max_tokens` excludes chain-of-thought on some models; xAI's cost is `usage.cost_in_usd_ticks` at 1e-10 USD per unit, in streams only with `include_usage` (`stream_usage: request`); DeepInfra's `usage.estimated_cost` was not found; OpenRouter states `cost` in credits; MiniMax needs an `on` fragment (`adaptive`, not `enabled`); Gemini's compatible endpoint is reported, unofficially, to exclude reasoning from `completion_tokens`. Recommendation: `openai` gets `dialect: reasoning_effort`; the four providers get `output_cap_param: max_completion_tokens`; the xAI cost report is declared, DeepInfra's left out, OpenRouter's kept with a note; the rest is target data. None changes phase 2 code.
20. **RSN-4 and silent provider mapping.** DeepSeek, xAI, Ollama, Fireworks and Gemini map unsupported levels themselves, and Fireworks lowers `max_tokens` to fit the context: "sent" can differ from "applied" with no error. Recommendation: no code; declare real `levels` so the proxy does the mapping visibly, and rely on R2's measures.

**Test and wording**

21. **"Unmodified client SDKs" (AC-01, AC-02, §23).** CI has no Python or Node and adds no dependency. Recommendation: CI replays recorded SDK request bytes and checks responses against the SDK field sets; maintainers run the real SDKs against the mock before each release.
22. **Scope of the RES-3 total.** "Total 180 s for non-streaming requests" can be read per request or per provider call. Recommendation: per attempt, so a fallback gets a full budget; the write deadline is reset per attempt.
