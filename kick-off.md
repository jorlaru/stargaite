# stargaite — kick-off

A minimal LLM inference proxy. Clients call one endpoint with a model alias; the proxy resolves the alias to a
provider and model, translates the parameters that providers name differently, tells the truth about what it sent,
measures cost and enforces limits. Nothing else.

## 0. How to read this document

This is the kick-off document of the project and the only file in its first commit. It says what the product is, why
it exists and how it will be built. There is no code yet, so nothing described here has been tested by this project.

Where a `Status` appears, it is one of:

- `decided`: a firm decision of the project.
- `proposed`: the design baseline. Good enough to start building; may be adjusted with evidence.
- `open`: not decided; it has a row in §26.

Priorities: `P0`, required for the first usable build; `P1`, required for v1; `P2`, later and only with confirmed
demand.

Rules carry an id by area: `PAR` request parameters, `RSN` reasoning control, `TEL` telemetry, `USE` usage and cost,
`LIM` budgets, limits and quotas, `RES` resilience, `SEC` security, `OPS` operation, `PUB` publication. Other ids:
`PRI` principles, `ACT` actors, `ADR` architecture decisions, `AC` acceptance criteria, `R` risks, `ASM` assumptions,
`OQ` open questions. The acceptance criteria in §21 cite the rules.

Provider names, model ids, prices, quotas, reasoning levels and key names in examples are illustrative: they show the
shape of a declaration, not the behaviour of a named product. Performance figures are targets, not measurements.

## 1. Identity

| Field | Value |
|---|---|
| Name | `stargaite`, written StargAIte |
| Module path | `github.com/jorlaru/stargaite` |
| Kind | LLM inference proxy: a data plane (the inference endpoints), an admin API with its CLI, and an embedded usage store |
| License | MIT |
| Language | Go 1.27 or later |
| Supported systems | Linux and macOS, amd64 and arm64. Windows is outside v1 |
| Binary and CLI | `stargaite` (`proposed`, OQ-8) |
| Environment variable prefix | `STARGAITE_` (`proposed`, OQ-8) |
| Issued key prefix | `sk-stargaite-` (`proposed`, OQ-8) |
| Kick-off date | 2026-11-11 |
| State | Design. No code |

## 2. What it is

| Field | Value |
|---|---|
| Problem | Agents and automations that consume LLMs end up coupled to a provider, an endpoint and an upstream model: changing any of the three means touching every consumer. General-purpose gateways remove that coupling, but they usually bring a UI, an external database and features many deployments never use, and they can make it hard to see what is actually sent to the provider (§3) |
| Goal and audience | Give technical consumers (agents in code, workflow tools, microservices) a stable inference contract, so that the provider, the endpoint, the model or the proxy itself can be replaced without modifying them. Give the operator certainty about what each request sent and what it cost |
| The usual alternative | Consumers call a general-purpose gateway by alias, or call each provider directly |
| What changes | One binary with no UI and no external database that only authenticates, routes, translates parameters, limits and measures. Every change it makes to a request is visible. Configuration lives in one place and changes without a restart |
| Vision | Replacing a provider, a model or the whole proxy needs no change in agent logic, and running it costs little: at most 20,000 lines of production code and 0.2 FTE once stable (§23) |
| Neighbours | Consumers (OpenAI and Anthropic SDK clients, HTTP calls from workflow tools), inference providers, observability backends that ingest OTLP (Langfuse, LangSmith, an OpenTelemetry Collector) and existing gateways (LiteLLM, Bifrost), whose code may be studied for ideas. This project is built from scratch |

The chain the product preserves: `agent → inference client → proxy → provider`. The agent only sees "inference";
everything behind it can change.

## 3. Why it exists

A gateway in front of several providers removes the coupling, and in our experience it brings two problems of its
own: connecting it to an observability backend can be fragile, and it can be hard to tell what reasoning effort
actually reaches the provider. This product exists to remove both. This section describes what was observed while
operating one such gateway in front of several providers and an OTLP-capable observability backend; it is not a claim
about every gateway or every version.

### 3.1 What went wrong

| # | Failure mode | What it caused |
|---|---|---|
| 1 | The gateway rewrote the request in silence: it dropped parameters the provider "did not support" and injected an output cap nobody had asked for | Reasoning controls never reached the provider. Reasoning consumed the injected cap, so answers came back empty with `finish_reason=length` |
| 2 | The trace recorded what the client asked, not what was sent | Wrong conclusions ("the model ignores the parameter") about a parameter that had never left the gateway |
| 3 | Precedence between the client's value and the alias's value was not a contract: it changed between versions, and `none` ended up as maximum effort | A reasoning model left without an effort used several times the tokens and the wall-clock time of the same request at medium effort |
| 4 | Capabilities were keyed by upstream model name, not by alias or target | One alias per effort level: several aliases and two endpoints for the same model |
| 5 | Telemetry went through an embedded vendor SDK with several export paths, each emitting different attributes | An upgrade could rename fields or lose the key attribution or the session. Errors were traced only with a second callback. Traces were duplicated |
| 6 | The alias, the serving model, the finish reason and the reasoning tokens were missing from the trace or buried in free-form metadata | Nobody could tell which model answered, or whether a change of effort had any effect |
| 7 | A subscription plan was emulated as a pay-per-use API with a nominal price | Invented money exhausted a real budget and made cost reports meaningless |
| 8 | Configuration lived in two places (a file and a database) and every change restarted the process | Each change cut the requests in flight |
| 9 | Turning tracing off for a noisy client also turned off its spend and error records | A choice between noise and accounting |

### 3.2 The two promises

**Promise A — connect an observability backend in two commands and know that it works.** The proxy composes the
authentication header, sends a real span through the same exporter that real traffic uses, and says what failed.

```console
$ stargaite telemetry set --preset langfuse --otlp-endpoint https://langfuse.example.com/api/public/otel
$ stargaite telemetry test
credentials: resolved · reachable: yes · authorized: yes · span: accepted (200) · trace_id=4f0c9a…
```

If it fails, it names the cause: `credential_unresolved` (a referenced variable is not visible to the proxy
process), `unreachable`, `unauthorized` (401 or 403), `not_found` (wrong OTLP path) or `rejected` (any other status,
or a span refused inside a `200`, with the backend's message). From then on every span carries the key, the model
alias whenever the request named one, and the target once one is selected, on successes, streams and errors alike. `stargaite telemetry status`
shows spans sent, rejected and dropped.

**Promise B — know and control the reasoning effort that reaches the provider.** One alias, one declared effort, a
dry run that shows the outgoing body, and a query that shows the effect.

```console
$ stargaite models set fast --effort medium --effort-mode default --max-output-tokens 32000
$ stargaite models explain fast --effort none
target      minimax-plan/MiniMax-M3.1-Flash
effort      requested=none · alias=medium · sent=low · source=client · adjusted (the target cannot turn reasoning off)
max output  requested=- · alias=32000 · sent=32000
changes     protocol:translated, reasoning_effort:translated, max_tokens:added
body        {"model":"MiniMax-M3.1-Flash","max_tokens":32000,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"},…}

target      openrouter/qwen/qwen-3.6-flash
…
$ stargaite usage show --model fast --by effort-sent --since 24h --compare-previous
```

A real response carries the same facts in headers (`x-stargaite-reasoning-effort: low`, `x-stargaite-target`,
`x-stargaite-trace-id`), and the trace carries them as scalar attributes. If an alias points to a target that always
reasons and sets no effort, the change is refused and the error names the alias and the target.

How each failure mode is closed:

| # | Rule of the product | Rules |
|---|---|---|
| 1 | Nothing is removed or added, and no value is changed, without being listed in the span, the usage record and a response header. The proxy never sends an output cap that does not come from the client or the alias | PAR-2, PAR-3, PAR-6, PAR-7 |
| 2 | Every observable value is read from the final serialized outgoing body, per attempt. Requested, alias and sent values are recorded separately | PAR-8, RSN-6, TEL-9 |
| 3 | Each alias declares its effort as `default` (the client may override it) or `forced`. A deterministic, tested table translates each level for each target | RSN-4, RSN-5, RSN-11 |
| 4 | Each target declares its own reasoning controls; one target reaches every level; changing an alias never changes another alias | RSN-2, RSN-3, RSN-8, PAR-11 |
| 5 | One OTLP/HTTP exporter, no vendor SDK, one span schema for every backend, fixed by a golden test | TEL-1, TEL-4 |
| 6 | Alias, target, returned model, finish reason and reasoning tokens are first-class in spans and usage records | TEL-4, TEL-9, USE-3, USE-4 |
| 7 | Subscription plans are a billing mode of their own, metered in requests and tokens per window, never in USD | USE-7, LIM-8 |
| 8 | One source of truth changed through the CLI, published atomically, without a restart | OPS-1, OPS-3, OPS-4, RES-7 |
| 9 | Tracing can be sampled or turned off per alias and per key without touching usage, budgets or error records | TEL-7 |

### 3.3 What a proxy cannot promise

- **Sent is not applied.** The proxy shows which effort left; it cannot prove the provider honoured it, and providers
  change defaults without notice. The only evidence of the effect is the reasoning tokens, which is why the usage
  query (USE-9) and the live target check (RSN-9) exist.
- **The backend decides which attributes it shows as filterable fields.** The proxy fixes its span schema; where each
  attribute lands in a given backend has to be verified with a real trace (OQ-1).
- **Joining the agent's own trace needs the agent to send `traceparent`.** Without it there are two traces.

## 4. Principles

All `decided`.

| Id | Principle |
|---|---|
| PRI-1 | Minimal: whatever is not needed is left out |
| PRI-2 | LLM proxy only: no MCP, no agents, no guardrails, no UI, no external database |
| PRI-3 | Drop-in: an OpenAI or Anthropic client works by changing only the base URL, the key and the model name |
| PRI-4 | The alias is a contract: clients call a model alias, and what the alias guarantees does not change when its targets do |
| PRI-5 | No silent changes: every change the proxy makes to a request is listed where the operator and the client can see it |
| PRI-6 | What was sent is the truth: observability reads the final outgoing request, never the intent |
| PRI-7 | One source of truth for configuration, managed through the CLI and the admin API, exported and applied as YAML, changed without a restart. No configuration file is edited by hand |
| PRI-8 | Operable by an AI agent: no CLI command ever prompts; every one has JSON output and deterministic exit codes |
| PRI-9 | Frictionless onboarding: creating a key for an agent is one command, for a person at a console or for an AI running the CLI |
| PRI-10 | Vendor-neutral telemetry: one OTLP exporter. Usage accounting never depends on it |
| PRI-11 | Honest cost: USD only, never an invented price. Subscription plans are metered in their own units |
| PRI-12 | Limits by key and by model alias, independent of each other. The proxy knows nothing about end users |
| PRI-13 | Universal and open source: MIT, nothing from any particular organization, runnable by anyone who clones it (§19) |
| PRI-14 | Fast, small and cheap to maintain |
| PRI-15 | Replaceable: the proxy can be swapped without touching its consumers |

Membership test: a new feature that does not directly improve authentication, authorization, routing, parameter
compatibility, resilience, capacity control, accounting or the visibility of those belongs outside the proxy.

## 5. Actors

There are no accounts for people and no end users. Identity comes only from the authenticated key, never from
metadata the client declares.

| Id | Actor | Who | How it gets in | What it can do |
|---|---|---|---|---|
| ACT-1 | client | An agent, a workflow or a service. One key per agent | `Authorization: Bearer sk-stargaite-…` on the data plane (`x-api-key` is also accepted on `/v1/messages`) | Request inference through the model aliases its key allows, and list them |
| ACT-2 | operator | Whoever administers the proxy from a console | The admin key, on a separate listener | Everything in the CLI |
| ACT-3 | admin agent | An AI running the CLI or calling the admin API | The same admin key | The same as the operator |

The admin key cannot request inference, and a client key cannot reach the admin API. The admin key is full control:
whoever holds it decides where requests and credentials go, within the bounds of SEC-4 and with every change audited.

## 6. Resource model

The names follow what the market already uses.

| Resource | What it is | Example, or where it is set |
|---|---|---|
| provider | A connection to an inference service: protocol, base URL, credential reference and billing mode | `openrouter` |
| target | One upstream model at one provider, with its declared capabilities, reasoning controls and prices | `provider/upstream-model`, e.g. `openrouter/qwen/qwen-3.6-flash`. The provider is everything before the first `/` |
| model (in prose, model alias) | The name clients put in the `model` field: an ordered list of `targets` (the first is the primary, the rest are fallbacks) plus the alias's own settings | `fast` |
| key | A named credential, one per agent. The name is what appears in usage and traces | name `invoice-agent`, secret `sk-stargaite-…` |
| budget | A USD spend limit: `limit_usd` and `reset_interval` (`daily`, `monthly` or `never`) | On a key, on a model, and globally |
| limits | `rpm_limit`, `tpm_limit` and `max_parallel_requests` | On a key and on a model; `max_parallel_requests` also globally |
| quota | For subscription providers: requests or tokens per time window | On the provider; an optional `quota_share` per key |
| usage | The durable record of every request, and the queries over it | — |
| settings | Instance-wide values: global budget, global `max_parallel_requests`, `max_body_bytes`, `max_response_bytes`, timeouts, retention, telemetry | — |

Fields of each resource (complete example in annex B):

| Resource | Fields |
|---|---|
| provider | `protocol` (`openai` or `anthropic`), `base_url`, `api_key_env`, `billing` (`pay_per_use`, `subscription` or `none`), `quota`, `cost_report` (where the provider reports the cost of a request: `path` and `usd_per_unit`), `stream_usage`, `rerank_url`, `gen_ai_provider` (the well-known provider name for telemetry), `target_defaults` (what a new target of this provider starts with: the reasoning `dialect` and `output_cap_param`), `allow_private_network` |
| target | `kind` (`chat`, `embeddings` or `rerank`), `capabilities` (`tools`, `structured_output`, `strict_schema`, `vision`), `max_output_tokens` (the model's own limit), `output_cap_param`, `reasoning` (§9), `pricing` (USD per million tokens, with an `as_of` date), `unsupported_params`, `response_models`, `inline_think_tags` |
| model | `targets` (1 to 3), `requires`, `reasoning` (`effort`, `mode`), `max_output_tokens`, `params` (per-parameter rules), `budget`, `rpm_limit`, `tpm_limit`, `max_parallel_requests`, `trace_sample_rate` |
| key | `models` (an explicit list, or `["*"]` for all), `budget`, `rpm_limit`, `tpm_limit`, `max_parallel_requests`, `quota_share`, `trace_sample_rate`, `content_until` |

A capability has three states: `true`, `false` or undeclared. Undeclared means "forward and let the provider answer";
only `false` blocks a request, and only `true` satisfies an alias's `requires`. A key stops working by
`keys revoke` or by emptying its `models` list; an alias stops being served by `models remove`. There is no separate
"disabled" state.

Provider, alias and key names match `[A-Za-z0-9][A-Za-z0-9._-]*`, up to 64 characters; an upstream model id is any
text without spaces or control characters. The store keeps what was declared: defaults are applied when the
configuration is published, so `export` shows declared values and `settings show` the effective ones, and a release
that changes a default changes it for running instances.

## 7. Providers

The proxy depends on no particular provider: each deployment declares its own.

### 7.1 Protocols and formats

Two upstream protocols cover the providers of §7.2. With `openai` the proxy appends `/chat/completions` and
`/embeddings` to `base_url`; with `anthropic` it appends `/v1/messages`.

| Inbound endpoint | To an `openai` target | To an `anthropic` target |
|---|---|---|
| `POST /v1/chat/completions` | Native | Translated by the adapter: the system prompt moves to the top level, tool calls become `tool_use` and `tool_result` blocks, images, `stop`, `tool_choice` and `parallel_tool_calls` are mapped, usage and finish reason are mapped back (`pause_turn` as `stop`). A chat feature with no faithful equivalent, such as `n` above 1, logprobs, `json_object` or a message `name`, skips the target (PAR-12) |
| `POST /v1/messages` | Not supported: the target is skipped | Native |
| `POST /v1/embeddings` | Native | Not supported: the target is skipped |
| `POST /v1/rerank` | Forwarded to the provider's `rerank_url` with only `model` replaced by the upstream model id | Not supported: the target is skipped |

When every target of the alias is skipped for this reason the request gets `422 endpoint_not_supported`. There is no
translation from the Messages format to an `openai` target, and no Responses API, in v1 (§24).

The proxy defines no rerank schema: the client sends `model` (the alias) plus the provider's own fields. PAR-2 does
not apply, and of PAR-1 only the rule on repeated keys applies, on the top-level object. The request is metered as
one request, plus tokens when the response reports them.

### 7.2 Built-in presets

`stargaite providers add <name> [--preset <preset>]` creates a provider. A preset fills in the protocol, the base
URL, the name of the key variable and the provider's other fields; when `--preset` is omitted and `<name>` is a preset
name, that preset is used; explicit flags override it. A preset is data copied into the configuration when the
provider is created: afterwards the stored declaration is the only truth and shows in `export`. The catalog holds
providers, not models, and no preset sets `allow_private_network`.

| Preset | Protocol | Base URL | Key variable | Notes |
|---|---|---|---|---|
| `openai` | `openai` | `https://api.openai.com/v1` | `OPENAI_API_KEY` | New targets send the effort as `reasoning_effort` and the output cap as `max_completion_tokens` |
| `anthropic` | `anthropic` | `https://api.anthropic.com` | `ANTHROPIC_API_KEY` | — |
| `openrouter` | `openai` | `https://openrouter.ai/api/v1` | `OPENROUTER_API_KEY` | Reports the cost of each request in `usage.cost`, stated in credits |
| `deepinfra` | `openai` | `https://api.deepinfra.com/v1/openai` | `DEEPINFRA_API_KEY` | — |
| `minimax` | `openai` | `https://api.minimax.io/v1` | `MINIMAX_API_KEY` | Its Anthropic-format endpoint is `https://api.minimax.io/anthropic`. New targets send the output cap as `max_completion_tokens` |
| `zai` | `openai` | `https://api.z.ai/api/paas/v4` | `ZAI_API_KEY` | GLM models |
| `moonshot` | `openai` | `https://api.moonshot.ai/v1` | `MOONSHOT_API_KEY` | Kimi models |
| `dashscope` | `openai` | set by the operator | `DASHSCOPE_API_KEY` | Qwen models. The compatible-mode URL depends on the region and the workspace, for example `https://dashscope-intl.aliyuncs.com/compatible-mode/v1`. New targets send the output cap as `max_completion_tokens` |
| `ollama` | `openai` | `http://localhost:11434/v1` | none | `billing: none`. Needs `--allow-private-network` |
| `tei` | `openai` | set by the operator | none | Text Embeddings Inference: embeddings under `/v1`, rerank at `/rerank`; one instance serves one model. `billing: none`. Usually needs `--allow-private-network` |
| `deepseek` | `openai` | `https://api.deepseek.com` | `DEEPSEEK_API_KEY` | — |
| `gemini` | `openai` | `https://generativelanguage.googleapis.com/v1beta/openai` | `GEMINI_API_KEY` | — |
| `xai` | `openai` | `https://api.x.ai/v1` | `XAI_API_KEY` | Reports the cost of each request in `usage.cost_in_usd_ticks`, in units of 1e-10 USD; in streams only when `include_usage` is set |
| `mistral` | `openai` | `https://api.mistral.ai/v1` | `MISTRAL_API_KEY` | — |
| `groq` | `openai` | `https://api.groq.com/openai/v1` | `GROQ_API_KEY` | New targets send the output cap as `max_completion_tokens` |
| `together` | `openai` | `https://api.together.ai/v1` | `TOGETHER_API_KEY` | Has a rerank endpoint |
| `fireworks` | `openai` | `https://api.fireworks.ai/inference/v1` | `FIREWORKS_API_KEY` | — |
| `cerebras` | `openai` | `https://api.cerebras.ai/v1` | `CEREBRAS_API_KEY` | New targets send the output cap as `max_completion_tokens` |

Any other OpenAI-compatible or Anthropic-compatible service is added with explicit flags:
`stargaite providers add my-vllm --protocol openai --base-url http://vllm.internal:8000/v1 --allow-private-network --billing none`.

Before v1 is released, the first ten presets of the table are validated against the live service. The other eight
ship marked `untested` in the catalog, and the mark is removed in a release once a maintainer has run the contract
suite against them. The table shows only part of each preset's data; the rest (`billing`, `target_defaults`,
`stream_usage`, `cost_report`, `gen_ai_provider`) is completed from each provider's public documentation before the
golden test of the catalog is frozen (AC-53).

### 7.3 Billing modes

| `billing` | Meaning | Cost | Subject to USD budgets |
|---|---|---|---|
| `pay_per_use` | Billed per token or per request | USD, from the provider's own report or from the target's declared prices (USE-6) | Yes |
| `subscription` | A flat-fee plan with a usage allowance | None. Metered in requests and tokens per window against the provider's declared `quota` (LIM-8) | No |
| `none` | Self-hosted or free | None. Tokens are still recorded | No |

A subscription plan is a provider of its own, with its own key, even when it shares a base URL with the pay-per-use
API of the same vendor. It is added with explicit flags, including the `quota` windows of the tier the operator
bought; there are no plan presets:

```console
$ stargaite providers add minimax-plan --preset minimax --protocol anthropic \
    --base-url https://api.minimax.io/anthropic --api-key-env MINIMAX_PLAN_API_KEY \
    --target-defaults dialect=adaptive_effort,output_cap_param= \
    --billing subscription --quota 5h:requests=1500 --quota 7d:requests=15000
```

Some plans' published terms restrict them to the vendor's own tools. Complying with a provider's terms is the
operator's responsibility (R17).

### 7.4 Adding providers, targets and models through the API

Everything can be created through the CLI and the admin API, by a person or by an AI: providers, targets, model
aliases and keys. The guarded resources are the two that carry a URL and a credential reference, the provider and the
telemetry exporter (SEC-4, SEC-5):

- The URL must use HTTPS and resolve to public addresses.
- Plain HTTP, loopback and private networks need the explicit `allow_private_network` flag (the case of Ollama, TEI,
  a self-hosted server or a self-hosted observability backend). Link-local and cloud metadata addresses are always
  refused.
- The credential is given as the name of an environment variable, never as a value.
- Every change is audited with the previous and the new value.

## 8. Request parameters

All `proposed`.

| Id | Rule |
|---|---|
| PAR-1 | Each inbound endpoint has a schema of known body parameters, pinned to the provider's public reference of the format it implements (OpenAI Chat Completions, Anthropic Messages, OpenAI Embeddings) as read on a recorded date, which each release moves (R7). `max_tokens` and `max_completion_tokens` are both known on chat completions. The schema covers the top-level keys of the body; nested objects are validated only where the proxy reads or translates them (`thinking`, `output_config`, `stream_options`, the type of `response_format`), and anything else inside a known parameter is forwarded as is. A body that repeats a key in any object the proxy reads or writes, or that is not valid UTF-8, gets `400 invalid_request` |
| PAR-2 | An unknown top-level parameter is rejected with `400 unknown_parameter` and its name, unless the model alias has a rule for it in `params`: `forward` (sent to the provider unchanged) or `drop` (removed). There is no global "drop what is not supported" switch. A `forward` rule is refused as a change when its name is a key the proxy itself can write toward a target of the alias (the reasoning dialect and its fragments, the output-cap parameter, `stream_options`, a field of the protocol adapter): those values are set only through the client's standard field and the alias's own settings |
| PAR-3 | The proxy never removes or adds a parameter, or changes its value, without listing it as `name:action` in three places: the span attribute `stargaite.params.changed`, the usage record and the response header `x-stargaite-params-changed`. `name` is the inbound parameter name, or the outbound name when the client sent no such parameter. Actions: `dropped`; `added` (a value the client did not send, such as the alias's effort or cap); `translated` (the client's parameter left under another name or with another value, such as an effort respelled by the target's dialect or adjusted to another level); `forced` (an alias value replaced the client's); `forwarded` (an unknown parameter let through by a rule). The fixed field mapping of a protocol adapter (§7.1) is listed once, as `protocol:translated`. Replacing the alias with the upstream model id and the client key with the provider credential are not listed: they have attributes of their own |
| PAR-4 | Contractual features never degrade. A request uses a feature when it carries `tools` or `tool_choice` (`tools`), a JSON schema in `response_format` or, on `/v1/messages`, in `output_config.format` (`structured_output`), `strict: true` in that schema or in a tool (`strict_schema`) or non-text input (`vision`). A target that declares a used feature `false` is skipped; if none is left the request gets `422 capability_mismatch` before any provider is called. The proxy does not count input tokens: a request that exceeds the provider's context length gets the provider's own error (§15) |
| PAR-5 | An alias's `requires` list is checked on every change: each target, primary or fallback, must declare each required capability `true`, or the change is refused |
| PAR-6 | The output cap has three recorded layers: requested by the client (the larger of `max_tokens` and `max_completion_tokens` when a chat body sends both), set by the alias (`max_output_tokens`) and sent. The proxy only sends a cap that comes from the client or from the alias; there is no internal constant and a cap is never lowered to fit. The alias's value is used when the client sends none, and a client value above it gets `422 max_output_tokens_exceeded`. An alias cap above the declared `max_output_tokens` of one of its targets is refused as a change; with no alias cap, a client cap above a target's declared limit makes that target ineligible. Toward an `openai` target a client cap keeps the parameter name or names the client used and an alias cap is sent under the target's `output_cap_param` (`max_tokens` by default); toward an `anthropic` target every cap is sent as `max_tokens` |
| PAR-7 | If the upstream protocol requires a cap (Anthropic Messages does) and the alias sets none, the change is refused. The proxy never fills one in |
| PAR-8 | Every "sent" value in spans, usage records, headers and `explain` is read back from the final serialized outgoing body, after the last transformation, for each attempt, by the same single-pass scanner that enforces PAR-1 |
| PAR-9 | Every response carries `x-stargaite-request-id`. Every response to an authenticated request also carries `x-stargaite-trace-id`, also when its span is sampled out. A response for which a target was selected carries `x-stargaite-target` (the target that served, or the last one tried), `x-stargaite-reasoning-effort` (the value sent, when there is one) and `x-stargaite-params-changed` (when not empty) |
| PAR-10 | `stargaite models explain <alias> [--endpoint chat\|messages] [--effort <level>] [--max-output-tokens <n>] [--body <file>]` shows, without sending traffic and through the same code path the data plane uses, one block per target of the alias, in order: the layers, the list of changes and the outgoing body. `--endpoint` defaults to `chat`; a target that cannot serve the endpoint is shown as skipped. Without `--body` it uses a fixed minimal request. The admin API returns each body as a string holding its exact bytes |
| PAR-11 | Changing the settings of one alias never changes the body sent by another alias, even when they share a target |
| PAR-12 | Known parameters are forwarded unchanged. A target that lists a parameter of the request in `unsupported_params`, or whose protocol has no equivalent for it, is skipped, unless the alias has a `drop` rule for that parameter: then it is removed for that target and listed. If no target is left the request gets `422 unsupported_parameter` with the parameter's name |
| PAR-13 | A change that a reasoning dialect imposes on other parameters (for example, a provider that only accepts a fixed `temperature` while reasoning) is a change like any other and is listed |
| PAR-14 | No client header reaches the provider except `anthropic-version` and `anthropic-beta`, and those only from `/v1/messages` to an `anthropic` target. Never `Authorization`, `x-api-key`, `traceparent`, `x-session-id` or any `x-stargaite-*`. When the client sends no `anthropic-version`, the proxy sends `2023-06-01`, one pinned constant. Unknown client headers are ignored, not errors |

## 9. Reasoning control

All `proposed`. This section is the centre of the product.

### 9.1 Effort levels and dialects

The proxy has one canonical scale, in this order: `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`. The
client expresses the effort it wants in the standard field of the endpoint it calls: `reasoning_effort` on chat
completions, `output_config.effort` on messages. An empty string, `default` or any other value outside the scale gets
`400 invalid_reasoning_effort` with the accepted values, and no provider is called.

Each provider spells the control differently. The proxy knows seven dialects; adding one is a code change with its
own tests. A target may override the default "on" and "off" forms with its own fragments (RSN-3).

| `dialect` | Outgoing form for a level | Default form for `none` | Typical of |
|---|---|---|---|
| `none` | Nothing is sent; an effort is dropped and listed (PAR-3) | Nothing is sent | Models that do not reason |
| `reasoning_effort` | `"reasoning_effort": "<level>"` | `"reasoning_effort": "none"` | OpenAI Chat Completions and many compatible services |
| `reasoning_object` | `"reasoning": {"effort": "<level>"}` | `"reasoning": {"effort": "none"}` | OpenRouter |
| `thinking_toggle` | `"thinking": {"type": "enabled"}` | `"thinking": {"type": "disabled"}` | Services with an on/off switch |
| `enable_thinking` | `"enable_thinking": true` | `"enable_thinking": false` | Services with a boolean switch |
| `thinking_budget` | `"thinking": {"type": "enabled", "budget_tokens": N}` | `"thinking": {"type": "disabled"}` | Anthropic Messages, the older token-budget form |
| `adaptive_effort` | `"thinking": {"type": "adaptive"}` with `"output_config": {"effort": "<level>"}` | `"thinking": {"type": "disabled"}` and no effort | Anthropic Messages, the effort form, and compatible endpoints |

Some models refuse the default "off" form of `adaptive_effort`, or accept it only at some levels: such a target
declares `can_disable: false` or its own `off` fragment, and an ability to turn reasoning off that depends on the
level cannot be declared in v1. The newer Anthropic models refuse `thinking_budget`, which stays for the older ones.

### 9.2 Rules

| Id | Rule |
|---|---|
| RSN-1 | The canonical scale and the inbound fields are those of §9.1. On `/v1/messages`, where `none` and `minimal` are valid and translated like any level: `thinking.type: disabled` with no `output_config.effort` is requested effort `none`; with an effort above `none`, the effort is the requested level and `thinking` is forwarded unchanged, while an effort `none` sends the target's "off" form, which replaces the client's `thinking`, listed `thinking:translated`; `thinking.type: adaptive` or `between_tools` with no effort counts as no client effort, and when the alias supplies a level other than `none` that `thinking` is kept toward an `adaptive_effort` target, with only the level written, and replaced by the "on" form toward other dialects, listed `thinking:forced`; `thinking.display` is forwarded unchanged. A client `thinking.budget_tokens` is recorded as `budget:<N>` and forwarded unchanged to a `thinking_budget` target; other targets are skipped, and if none is left the request gets `422 capability_mismatch`. An alias in `forced` mode replaces any of these with its own effort |
| RSN-2 | Reasoning controls are declared per target, never looked up by model name in a table inside the code. A new target starts with the `dialect` of its provider's `target_defaults`; levels and the ability to turn reasoning off are per model and are never guessed. A provider without `target_defaults` gives its new targets the dialect `none`. Afterwards only the stored declaration counts |
| RSN-3 | A target's `reasoning` block holds: `dialect`; `levels`, the levels the provider accepts natively; `can_disable`, whether reasoning can be turned off; `budgets`, the tokens per level for `thinking_budget`; `on` and `off`, optional exact outgoing fragments that replace the default forms of §9.1 when the provider's spelling differs; and an optional `map` from a canonical level to the level sent. On a dialect with levels, an `on` fragment replaces the "on" part and the level is still written. On/off dialects have the single level `on`. `levels` and `can_disable` may be undeclared, like a capability (§6): with no `levels`, a level is sent as is in the dialect's form and nothing is adjusted; with `can_disable` undeclared, `none` is sent in the "off" form and RSN-7 does not apply. A change is refused for a target that declares `can_disable: false` and no `levels`, for a `thinking_budget` target without `levels` and a `budgets` entry for each, for an `on` fragment with a top-level key that holds the dialect's level (which would be overwritten), and for an alias whose `max_output_tokens` is at or below a `budgets` entry of one of its targets. `validate` warns about every chat target whose dialect is not `none` and that leaves `levels` or `can_disable` undeclared |
| RSN-4 | Translation is deterministic, per target, in this order: an entry in `map` wins; a level in `levels` is sent as is; `none` sends the "off" form unless `can_disable` is `false`, and then becomes the target's lowest level; any other level becomes the nearest one in `levels` by position in the canonical scale, the lower one on a tie; with an on/off dialect every level except `none` means on. A client effort that leaves under another name or at another level is listed as `translated` (PAR-3); it is recorded as adjusted only when the level changed |
| RSN-5 | Each alias declares `reasoning.effort` and `reasoning.mode`. With `default`, the client's value wins when present and the alias's value is used otherwise. With `forced`, the alias's value always wins and the client's is recorded as requested. An alias that sets an effort never lets a request leave without one toward a target whose dialect is not `none`; toward a `none` target the effort is dropped and listed, and `validate` warns about every alias that sets an effort and has such a target. `effort: provider_default` is the explicit way to send nothing |
| RSN-6 | Every request records the effort requested by the client, the effort set by the alias, the effort sent, its source and whether translation adjusted it. The effort sent is read from the outgoing body (PAR-8) in the dialect's own terms: the level; `on` for an on/off dialect; `budget:<N>` for `thinking_budget`; `none` when the body turns reasoning off; empty when the body carries no reasoning control. The source is `client`, `alias_default`, `alias_forced`, or `provider_default` when the body carries no reasoning control, whatever the cause |
| RSN-7 | A change is refused when an alias points, as primary or as fallback, to a target that declares `can_disable: false` and sets no `reasoning.effort`; an explicit `effort: provider_default` counts as setting one. An output cap does not replace the effort. The error names the alias and the target |
| RSN-8 | One target reaches every level its model supports. No alias, provider or endpoint has to be duplicated to express a different effort |
| RSN-9 | `stargaite targets verify <target> [--max-output-tokens <n>]` sends one fixed short prompt to a chat target at each level it declares, and at `none` when `can_disable` is true; when that gives no call, it sends the prompt once with no reasoning control. It uses the operator's own credential and reports the outgoing body, the provider's status and the reasoning tokens observed. The only output cap it sends is the operator's `--max-output-tokens`, and the command is refused for an `anthropic` target without it (PAR-7). It succeeds when every call returns `2xx`; the result is stored with its date and the proxy version, and `validate` warns about chat targets never verified. It costs tokens and only runs on demand. Its calls are recorded in usage under the reserved key name `_verify` (no key name may start with `_`); they pass no key or alias limit, count toward the global budget and the provider's quota, and are refused when either is exhausted |
| RSN-10 | On a fallback, the effort and the output cap are resolved and translated again for the new target, and that attempt records its own "sent" values |
| RSN-11 | Each dialect has a golden test of its translation table for the seven levels of the scale. A separate inbound test checks that an empty string, `default` and an unknown value get `400 invalid_reasoning_effort` and that no provider is called. A regression test covers the four combinations of client with or without an effort and alias in `default` or `forced` mode |
| RSN-12 | Reasoning that a provider returns in a structured field is relayed unchanged on the native paths (chat completions to an `openai` target, messages to an `anthropic` target): the proxy does not parse, move or remove it, and on `/v1/messages` `thinking` blocks with their signatures are protocol state that the client sends back. How thinking blocks survive a tool loop on the translated path is OQ-10. For a target that declares `inline_think_tags: true`, a streaming state machine removes `<think>…</think>` blocks from the visible content, also when a tag is split across SSE chunks, and the removal is recorded in the span |

## 10. Telemetry

All `proposed`. Telemetry is how the operator sees requests in an external backend. It is never the official record of
usage: that is §11.

| Id | Rule |
|---|---|
| TEL-1 | One exporter: OTLP over HTTP with JSON encoding, written with the standard library. No vendor SDK, no native adapter per backend, no second export path. A `2xx` is success for the spans the backend did not reject: the exporter reads `partialSuccess.rejectedSpans` from the response and counts those as rejected, with the backend's message |
| TEL-2 | `stargaite telemetry set` configures the exporter: the OTLP endpoint, always given in full (spans go to `<endpoint>/v1/traces`), and the authentication, given as references (`--basic-auth-env USER_VAR:PASSWORD_VAR` or `--header-env NAME=VAR`). Static headers that carry no secret are given with `--header NAME=value`; one named `Authorization`, `Proxy-Authorization`, `Cookie` or `x-api-key` is refused, because a credential is only ever a reference (SEC-5); `--allow-private-network` is needed for a plain-HTTP, loopback or private endpoint (SEC-4). The proxy composes the header; nobody encodes it by hand. `stargaite telemetry test` sends one real span through the exporter of TEL-1 with the stored configuration and reports one of `credential_unresolved`, `unreachable`, `unauthorized`, `not_found`, `rejected` or `accepted`, as defined in §3.2, with the trace id to look up |
| TEL-3 | `stargaite telemetry status` and the metrics endpoint show spans sent, spans rejected by HTTP status or by the backend, spans dropped and the last error. After ten consecutive failed batches the proxy writes a warning to the log, rate-limited |
| TEL-4 | The span name and the set of attributes are one versioned schema for every backend (annex D), fixed by a golden test. The key is an attribute of every span, request and attempt spans alike, on successes, streams and errors; the model alias is an attribute of every span whose request body named one (a body that was not read or could not be parsed names none); the target is an attribute of every span for which a target was selected. No setting removes or renames an attribute |
| TEL-5 | Every authenticated request that tracing keeps (TEL-7) produces exactly one request span, including those rejected before reaching a provider (budget, limits, capability mismatch, unknown parameter). A failure sets the span status, `error.type` and the HTTP status. Unauthenticated requests produce a metric, not a span |
| TEL-6 | If the request carries a valid W3C `traceparent`, the request span is a child of that trace instead of opening another; a malformed one is ignored, and the setting `trust_traceparent: false` ignores it always. Every request span and every attempt span gets a freshly generated span id. The header `x-session-id` is the only source of `session.id` and `gen_ai.conversation.id`; no body field sets them. Each failed attempt that precedes a fallback is a child span with its own target and error |
| TEL-7 | Tracing can be sampled or turned off per model alias and per key with `trace_sample_rate` (0 to 1); when both are set the lower one applies. The proxy decides once per request, for the request span and its attempt spans, ignoring the sampled flag of `traceparent`; failed requests are sampled like successful ones. Sampling never changes usage records, budgets, quotas or error counters; a test compares them with and without tracing |
| TEL-8 | No span carries prompt or response content by default. Content is enabled per key, with an expiry (`stargaite keys set <name> --content 24h`) and an audit record; each content attribute is truncated to `content_max_bytes` (32 KiB by default) and the truncation is flagged. Request attributes are a closed list of scalars. A test plants a fake secret in a prompt and in a header and checks that it is exported nowhere |
| TEL-9 | Attributes follow the OpenTelemetry GenAI semantic conventions at a version or commit pinned in the schema, plus `stargaite.*` for what they do not cover. `gen_ai.request.model` is the alias the client asked for; `gen_ai.response.model` is the model the provider returned; `stargaite.target` is the target that served. The alias is also emitted as `stargaite.model`, because some backends prefer the response model for their own model field. Effort and output cap appear as separate scalar attributes for requested, alias and sent (RSN-6, PAR-6). Token attributes are the inclusive totals the conventions define (USE-3) |
| TEL-10 | The cost goes in `stargaite.cost.usd`, only when it is known (USE-6); the conventions define no cost attribute. Usage and cost attributes sit on exactly one span per provider call, so a backend that adds them up never counts twice |
| TEL-11 | Nothing in the exporter is specific to a backend. Where a backend only reads a value under a name of its own, that name is added to the one schema as a compatibility attribute, documented with the backend that reads it, in a new schema version (annex D). `--preset` on `telemetry set` is only a shortcut that fills in flags (static headers and the conventional variable names of the credentials) and stores the result; the only one in v1 is `langfuse` |
| TEL-12 | Export is best effort and never blocks inference: an in-memory queue of 10,000 spans or 32 MB, whichever is reached first, batches every 2 s or 200 spans, three retries with exponential backoff. When the queue is full the oldest spans are dropped and counted |
| TEL-13 | Every span carries the proxy version and the configuration revision that served the request |
| TEL-14 | The default CI tests the exporter against an in-process OTLP receiver. An integration job outside the default CI sends spans to a self-hosted Langfuse at the version pinned in the job, with and without the ingestion-version header of the preset, and checks that the trace is readable and that key, alias and target can be filtered. It is run before any release that changes the span schema or the exporter |

Budget warnings, quota warnings and configuration changes go to the log, the metrics and the audit, not to the
telemetry backend.

## 11. Usage and cost

All `proposed`. The usage store is the official record. It is local, durable and independent of telemetry.

| Id | Rule |
|---|---|
| USE-1 | Every provider call leaves one immutable usage record, whether it succeeded, failed or was aborted. A request that fell back leaves one record per attempt, under the same request id and with its own attempt number. Requests rejected before reaching a provider are counted in metrics and spans, not in usage records |
| USE-2 | Writing is idempotent: each record has a unique id, it is inserted with insert-or-ignore, and the daily aggregate is updated in the same transaction only for rows actually inserted. Corrections are new records, never updates. Aggregates can be rebuilt from the records for the days still within the usage retention; older aggregates are never rebuilt or overwritten |
| USE-3 | Tokens are always recorded, with or without a price. `input_tokens` is the total input, cache tokens included; `cache_read_tokens` and `cache_write_tokens` are parts of it. `output_tokens` is the total output the provider bills, reasoning included; `reasoning_tokens` is the part spent on reasoning, empty when the provider does not report it. No total adds a part twice. The provider's `finish_reason` is recorded too |
| USE-4 | Each record carries the key name, the model alias, the target, the upstream model id sent and the `model` the provider returned. A returned model that starts with neither the upstream id nor one of the target's `response_models` is flagged as a mismatch and counted |
| USE-5 | Each record carries the requested, alias and sent values of effort and output cap, the list of parameter changes, the proxy version and the configuration revision |
| USE-6 | Cost is in USD and carries its source: `provider` (read from the provider's response at its `cost_report` path) or `price_table` (computed from the target's declared prices and the recorded tokens). The provider's figure wins over the price table; when a response carries no provider figure, the price table is used, also over estimated tokens. Without a report and without prices the cost is empty and the source is `none`: it is never guessed. A separate field says whether the tokens came from the provider or were estimated (USE-8) |
| USE-7 | Records of `subscription` and `none` providers have no cost. They never enter a USD total or a USD budget, and are reported separately in requests and tokens |
| USE-8 | In streaming, the usage block is obtained as each provider needs it (`stream_usage`): `request` sets `stream_options.include_usage` in the outgoing body, listed as added, and the proxy consumes the final usage chunk when the client did not ask for it; `native` expects usage without asking; `none` means it is not available. If no usage arrives (for example, the client disconnected), tokens are estimated as the bytes of the input and of the visible output divided by four, rounded up, and the record is marked `tokens_source: estimated` |
| USE-9 | `stargaite usage show` answers from the local store alone. It filters by `--key`, `--model` and `--target`, groups by any combination of key, model, target and effort sent (`--by key,model,effort-sent`), over a time window, with `--compare-previous` for the window before it. For each group: requests (provider calls: a request that fell back counts once per attempt), tokens by type, cost, median and 90th percentile of reasoning and output tokens over the successful records that reported them, responses with `finish_reason=length` and responses with empty visible content (no text and no tool call). Percentiles and grouping by effort come from the records, so they exist only within the usage retention; for older windows it returns the sums of the daily aggregates. `stargaite usage quota` shows each subscription window |
| USE-10 | Records go through a single writer in batches (100 records or 50 ms, whichever comes first), each batch committed with `synchronous = FULL`. After three consecutive failed commits the store is unavailable and the writer retries every 5 s. Meanwhile `pay_per_use` targets are ineligible (`503 usage_store_unavailable` when no other target is left), `subscription` and `none` targets keep serving, quota counters keep counting in memory, and records wait in the writer's bounded queue. A record that cannot wait (the queue is full, or the process is stopping) is written as one structured log line at critical level, with no content: it is never lost silently |
| USE-11 | Retention is finite by default and enforced by the process itself, once a day, in small batches, with no external scheduler: usage records 90 days, daily aggregates 36 months, audit 24 months. The values are settings: `settings show` prints them, and `export` when they were declared. The running total of a `never` budget is kept in its own counter and is not subject to retention |
| USE-12 | A `pay_per_use` target with neither declared prices nor a provider `cost_report` produces records without cost, which no budget can see. A change is refused when such a target can be reached from a scope that has a budget (the instance, a key or an alias); otherwise `validate` warns. A target priced only by its provider's `cost_report` cannot price a response whose report never arrived (an aborted stream): `validate` warns that it has no prices to fall back on. Every `pay_per_use` record left without a cost is counted in a metric of unpriced requests |

## 12. Budgets, limits and quotas

All `proposed`. Three independent mechanisms; a request passes only if none of them blocks it.

| Id | Rule |
|---|---|
| LIM-1 | Budgets are in USD, per key and per model alias, independent of each other, plus an optional global one. There is no limit on the combination of key and alias, and none per end user. Only scopes with a budget are evaluated |
| LIM-2 | A budget is `limit_usd` and `reset_interval`: `daily` (resets at 00:00 UTC), `monthly` (day 1 at 00:00 UTC) or `never`. The counter of a `daily` or `monthly` budget is the spend of its scope in the current period, whenever the budget was set or changed, so the figure is the same before and after a restart. The counter of a `never` budget starts at zero when it is set, is kept when `limit_usd` changes, and is deleted when the budget is removed or its interval changes |
| LIM-3 | At 80 % of a budget the proxy writes a warning to the log and raises a metric. At 100 % the `pay_per_use` targets are unavailable to the requests in that budget's scope: the next target of the alias is used, and if none is left the request gets the error RES-10 states, `402 budget_exceeded` when the budget is what excluded the first able target. A USD budget never blocks a `subscription` or `none` target. The error never reveals another key's figures |
| LIM-4 | Spend is charged to in-memory counters when a response ends, before its parallel slot is released; the durable write follows (USE-10). There is no prior reservation: the overshoot of a budget is at most the cost of the requests in flight in its scope when the limit is crossed. That is bounded where the scope has a parallel limit (a key has one by default, LIM-7) and each request's output is bounded by a cap (PAR-6); an alias that needs a hard bound sets both |
| LIM-5 | Rate limits are per key and per model alias, independent of each other and of budgets. `rpm_limit`: requests that passed admission (step 4 of §14.1) in the last 60 s, a sliding window. Admission is all or nothing: a request refused by any limit of the key, the alias or the instance counts in none of them and takes no slot; one that passed admission and is refused at a later step still counts. `tpm_limit`: the recorded input plus output tokens, reported or estimated, of the provider calls completed in the last 60 s; a request is refused while the sum is at or above the limit. `max_parallel_requests`: requests in flight. A global `max_parallel_requests` protects the process. Exceeding any of them gets `429` with `Retry-After` |
| LIM-6 | Parallel slots are always released: on completion, error, client cancellation or panic. The body size is checked against `max_body_bytes` (10 MiB by default) before the body is read in full (`413`) |
| LIM-7 | A new key gets `max_parallel_requests: 5` unless told otherwise (OQ-7); nothing else is limited by default |
| LIM-8 | A `subscription` provider declares its `quota` as a list of windows, each with a limit in requests or in tokens (for example `5h` and `7d`). The proxy counts its own traffic per sliding window, the only kind in v1 (OQ-2), and rebuilds the counters from the usage store at start. Each provider call counts when it is sent, whatever its result, and leaves its own usage record (USE-1), which is what the rebuild reads; its tokens count when it ends |
| LIM-9 | When a window is exhausted the target is unavailable: the next target of the alias is used, and if none is left the request gets the error RES-10 states, `429 quota_exceeded` with `Retry-After` when the quota is what excluded the first able target. At 80 % of a window the proxy logs a warning and raises a metric |
| LIM-10 | A key may have a `quota_share` per subscription provider: the fraction of each window that key may use. A key that reaches its share is treated as in LIM-9, for that key only. It stops one agent from taking the whole plan |

## 13. API and CLI

All `proposed`.

### 13.1 Data plane

Listens on `127.0.0.1:4000` by default (SEC-6). Everything except the health probes needs a client key.

| Route | What it does |
|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions, synchronous or SSE streaming. `model` is the alias |
| `POST /v1/messages` | Anthropic Messages, synchronous or SSE streaming, toward `anthropic` targets only. Errors use the Anthropic envelope |
| `POST /v1/embeddings` | OpenAI Embeddings |
| `POST /v1/rerank` | Forwarded to the provider's rerank endpoint (§7.1); metered and attributed like any other request |
| `GET /v1/models` | Lists the aliases the key may use |
| `GET /health/live`, `GET /health/ready` | `live`: the process answers. `ready`: the store is open, the stored configuration is published and requests can be accepted. A stored configuration that a newer binary finds invalid is still published, with every finding logged. A provider being down does not make the proxy unready |

### 13.2 Admin API

Listens on `127.0.0.1:4001` by default, under `/admin/v1/`, with the admin key. Each CLI command that talks to a
running server maps to one call.

| Resource | Operations |
|---|---|
| `providers`, `targets`, `models` | Create, read, update, delete |
| `keys` | Create, read, update, rotate, revoke. A revoked key is kept, so that usage stays attributable and its name is not reused |
| `settings` | Read, update |
| `explain` | Build the outgoing request for an alias without sending it (PAR-10) |
| `verify` | Run the live check of a target (RSN-9) |
| `usage` | Query records, aggregates and quota windows (USE-9) |
| `telemetry` | Set, unset, test and read the exporter's status (TEL-2, TEL-3) |
| `export`, `apply`, `validate` | The whole configuration as one document (OPS-3) |
| `secrets` | Resolve credential references again (OPS-6) |
| `presets` | List the built-in provider presets |
| `backup` | Write a consistent copy of the store |
| `metrics` | Prometheus text format (OPS-8) |

A key's secret is shown once, when it is created or rotated. The API never returns or accepts the value of a provider
credential. In a container, the proxy is administered by running the CLI inside it or by exposing the admin listener
explicitly to a trusted network, never next to the data plane.

### 13.3 CLI

The same binary. Shape: `stargaite <resource> <verb>`. It is a client of the admin API and holds no rules of its
own; three commands run without a server: `serve`, `version`, and `validate -f`, which calls the same validation
package in process.

| Command | Purpose |
|---|---|
| `stargaite serve [--data-dir ./data] [--env-file <file>] [--listen 127.0.0.1:4000] [--admin-listen 127.0.0.1:4001] [--tls-cert <file> --tls-key <file>] [--behind-tls-proxy]` | Run the proxy |
| `stargaite providers add\|set\|show\|list\|remove`, `providers presets` | Manage providers; list the built-in presets |
| `stargaite targets add\|set\|show\|list\|remove\|verify` | Manage targets; check one against the live provider |
| `stargaite models add\|set\|show\|list\|remove\|explain` | Manage model aliases; show what an alias would send |
| `stargaite keys create\|set\|show\|list\|rotate\|revoke` | Manage keys |
| `stargaite usage show`, `usage quota` | Query usage; show subscription windows |
| `stargaite telemetry set\|test\|status\|unset` | Manage the OTLP exporter |
| `stargaite settings show\|set` | Global budget and limits, timeouts, retention |
| `stargaite export`, `apply -f <file>`, `validate [-f <file>]` | Configuration as YAML |
| `stargaite secrets reload` | Resolve credential references again |
| `stargaite backup <file>`, `stargaite version` | Copy the store; print the version |

Flags follow one convention. Every scalar field of §6 is a flag of `add` (`create` for keys) and `set`, named after
the field with dashes (`--rpm-limit`, `--trace-sample-rate`); the alias's `reasoning.effort`, `reasoning.mode` and
`max_output_tokens` are `--effort`, `--effort-mode` and `--max-output-tokens`, and a key's `content_until` is given as
a duration from now with `--content <duration>`. List fields repeat a flag named after the singular of the field, in
order (`--target a --target b`, `--model fast --model deep`, `--require tools`); `--all-models` stands for a key's
`models: ["*"]`. Structured fields take `name=value` pairs (`--budget limit_usd=100,reset_interval=monthly`,
`--param enable_search=drop`, `--target-defaults dialect=adaptive_effort`, `--quota 5h:requests=1500`). A target's
`reasoning` block has flags of its own: `--dialect <name>`, `--level <level>` (repeated, in scale order),
`--can-disable=<true|false>`, `--thinking-budget <level>=<tokens>` and `--effort-map <level>=<level>` (repeated), and
`--reasoning-on <json>` and `--reasoning-off <json>`, each one JSON fragment taken verbatim. `set` changes only the
fields it names: `--flag=` clears a scalar, a structured flag changes one entry, and a list is replaced whole. The flags of
`telemetry set` are those of TEL-2. The flags of `serve` can also be given as `STARGAITE_<FLAG>` variables; they are the
only values that live outside the store. The other commands reach the admin API at `--admin-url`, else
`STARGAITE_ADMIN_URL`, else `http://127.0.0.1:4001`, and take the admin key from `STARGAITE_ADMIN_KEY`, else from
`<data-dir>/admin.key`, where `<data-dir>` is `--data-dir`, else `STARGAITE_DATA_DIR`, else `./data`.

From a fresh clone to a first answer, with no file edited by hand:

```console
$ OPENROUTER_API_KEY=… stargaite serve &
$ stargaite providers add openrouter
$ stargaite models add fast --target openrouter/qwen/qwen-3.6-flash
$ stargaite keys create my-agent --model fast
sk-stargaite-…          # shown once
$ curl -s localhost:4000/v1/chat/completions -H "Authorization: Bearer sk-stargaite-…" \
    -d '{"model":"fast","messages":[{"role":"user","content":"hi"}]}'
```

`models add` creates a target that does not exist yet and says so: `kind: chat`, no declared capabilities, no prices,
and the provider's `target_defaults`; the operator completes it with `targets set`. `keys create` needs
at least one `--model <alias>` or an explicit `--all-models`: a key never gets access by omission.

| Id | Rule |
|---|---|
| OPS-1 | All configuration lives in the embedded store and changes only through the admin API. There is no configuration file to edit and no second store. `config.example.yaml` in the repository is an input for `apply`, not something the proxy reads |
| OPS-2 | Nothing ever prompts. Every command accepts `--json` and returns `0` on success, `1` on failure and `2` on wrong usage. A command that deletes or revokes (`remove`, `keys revoke`, `telemetry unset`, and `apply --prune` unless it is a `--dry-run`) fails with `2` unless `--yes` is given. `apply` accepts `--dry-run`: it validates and shows the difference without publishing |
| OPS-3 | `export` writes the whole configuration as YAML, without secrets. `apply -f` validates the file, shows the difference and publishes it in one step; it creates and updates: a resource named in the file is replaced by the file's declaration as a whole, so a field the file omits returns to its default. What is absent from the file is left untouched unless `--prune` is given: then absent providers, targets and aliases are removed, absent keys are revoked (not deleted) and absent settings return to their defaults. An invalid file leaves the active configuration untouched. A key in the file that does not exist yet is created with a new secret, which `apply` prints once; a key that exists keeps its secret. `export` omits revoked keys, and a file that names a revoked key makes `apply` fail with `409 conflict`: a revoked name is never reused. Exporting from one instance and applying to an empty one therefore reproduces the configuration with new key secrets; cloning an instance with its secrets is what `backup` is for. A resource created by `apply` takes only what the file declares: it does not inherit a provider's `target_defaults` |
| OPS-4 | Every change publishes a new immutable snapshot atomically and increments the configuration revision. Requests in flight finish with the snapshot they started with. Nothing needs a restart. A change with an empty difference, and the publication at start, keep the revision: only a real difference increments it |
| OPS-5 | Every change, from a single command or from `apply`, is validated against the whole resulting configuration before it is published. A failed check refuses the change with exit code `1`: unknown providers and targets, aliases without targets, `requires` not met (PAR-5), missing or excessive output caps (PAR-6, PAR-7), `forward` rules that collide with a parameter the proxy writes (PAR-2), reasoning rules (RSN-3, RSN-7), unpriced targets under a budget (USE-12), outbound URLs (SEC-4). Warnings never block: chat targets never verified (RSN-9) or with undeclared reasoning fields (RSN-3), credentials that do not resolve, an effort toward a `none` dialect (RSN-5), targets without prices to fall back on (USE-12), aliases that cannot serve an endpoint, a key that names an unknown alias. `validate` runs the same checks on the active configuration, or on a file with no server running |
| OPS-6 | Credential references resolve from the process environment first and then from the file given with `--env-file` (no file is read unless the flag is given): plain `NAME=value` lines, no quoting, no interpolation, `#` for comments. They are resolved again on every published change and on `secrets reload`, so a credential added to that file works without a restart; a variable set in the process environment cannot change while the process runs, so a credential meant to rotate without a restart goes in the file. A provider whose credential does not resolve is loaded disabled, with a warning, and does not prevent the proxy from starting |
| OPS-7 | `stargaite version` prints the version, the same string that spans and usage records carry. Upgrading means replacing the binary: migrations of the embedded store run at start and are tested, and no external database is involved. `backup` writes a copy that a test restores |
| OPS-8 | The metrics endpoint exposes, by key and model alias: requests by status, tokens by type, cost, spans emitted, responses with `finish_reason=length`, responses with empty visible content, model mismatches, parameter changes by action and unpriced requests; and for the instance: budget and quota usage and the exporter's counters. The counters exist from the feature that produces them; the `metrics` feature adds the endpoint |
| OPS-9 | Logs are structured JSON. Each request line carries the request id, the trace id, the key name and, when the body named one, the alias. Logs never contain bearer tokens, provider credentials, sensitive headers, prompts or responses. Repeated errors are rate-limited, except the usage records of USE-10 |
| OPS-10 | `serve` takes an exclusive lock on the data directory before opening the store and exits with an error when another process holds it |

## 14. Request flow and resilience

### 14.1 Hot path

| Step | What happens |
|---|---|
| 1 | Authenticate the key (`401`) |
| 2 | Check the body size (`413`) and parse the body (`400`) |
| 3 | Resolve the alias in `model` (`404`), check that the key may use it (`403`) and validate the body against the known parameters of the endpoint (PAR-1, PAR-2) |
| 4 | Admission, for the key and the alias: parallel slots and rate limits; for the instance: parallel slots |
| 5 | Select a target (RES-10): first by what cannot change between requests (kind and protocol against the endpoint, capabilities, unsupported parameters, output cap), then by what can (circuit closed, credential resolved, quota available, budget available and usage store available for `pay_per_use`) |
| 6 | Build the outgoing request for that target: resolve effort and output cap, translate to the protocol and the dialect, list the changes, read back what will be sent (PAR-8) |
| 7 | Call the provider and relay the response or the SSE stream. On an eligible failure before anything was sent to the client, go back to step 5 with the next target |
| 8 | Answer with the proxy's headers (PAR-9) |

Errors are returned in the order of the steps. Cold path, asynchronous: `result → in-memory counters → usage record →
durable batch and daily aggregates`, and separately `span → queue → OTLP exporter`. Neither waits for the other.

### 14.2 Resilience

All `proposed`.

| Id | Rule |
|---|---|
| RES-1 | An alias has one primary target and at most two fallbacks. A fallback is tried on a connection error, a timeout, `429` or any `5xx` from the provider, or a stream `error` event whose type the provider documents with one of those statuses, as long as nothing has been sent to the client; a target excluded at step 5 of §14.1 is passed over the same way. Never on a `3xx` or on any `4xx` other than `429`, and never after the first SSE chunk has been sent. There are no automatic retries against the same target |
| RES-2 | One circuit breaker per target. Only failures eligible for fallback count. Five of them within 30 s open it for 20 s. Then it is half-open: it lets two requests through and treats the target as open for all others; if both succeed it closes, and if either fails it opens for another 20 s. These values are constants in v1 |
| RES-3 | Timeouts: connection 5 s; first byte 30 s, for streaming requests only; silence between chunks 120 s; total 180 s per provider call for non-streaming requests, which have no first-byte timer. They are settings |
| RES-4 | The proxy never buffers a streaming response. A non-streaming response is read in full, up to `max_response_bytes`, before anything is sent to the client, so a failure while reading it is still eligible for fallback. When a stream fails after its first chunk was sent, the proxy emits one terminal error event in the format of the inbound endpoint and closes (on chat completions a `data:` object with a top-level `error` and no `[DONE]` after it; on `/v1/messages` an `error` event); the usage record is written with what was observed. When it transforms SSE chunks it preserves valid events, keeps JSON well formed, duplicates nothing and does not reorder tool calls |
| RES-5 | When the client disconnects, the upstream call is cancelled within 1 s and the usage record is still written |
| RES-6 | On `SIGTERM` the proxy stops accepting requests, waits up to 30 s for those in flight, persists pending records, closes the store and exits. A request still running when the wait ends gets `internal_error` with the message "server stopping", as an envelope or as the terminal event of RES-4 |
| RES-7 | A configuration change never cuts a request in flight; a test keeps a stream open across an `apply` |
| RES-8 | A fallback only uses targets that satisfy the alias's contract, which is checked on every change (PAR-5) |
| RES-9 | A restart recovers configuration, keys, budgets, quota counters, usage records and aggregates from the store |
| RES-10 | When no target is left, the error says why. If every target was excluded by what cannot change between requests, the request gets `422` with the reason of the first target in order. Otherwise it gets the reason that excluded the first target that was otherwise able to serve: `402 budget_exceeded`, `429 quota_exceeded`, `503 usage_store_unavailable`, or `503 no_target_available` for an open circuit or an unresolved credential; when several reasons exclude that target, the first in this order is reported. This rule applies only when no provider was called: once a provider was called and failed, the client gets the error §15 gives for the last provider call, whatever excluded the remaining targets |

### 14.3 Background work

| Task | What it does | When |
|---|---|---|
| write usage | Commits pending records in one transaction | Every 100 records or 50 ms |
| reset budgets | Opens a new period for `daily` and `monthly` budgets | 00:00 UTC |
| enforce retention | Deletes what exceeds USE-11, in small batches | Once a day |
| export spans | Sends queued spans | Every 2 s or 200 spans |
| check the store | `PRAGMA quick_check`; a failure is a critical log entry and a metric | In the background, after the proxy is ready |
| back up | `VACUUM INTO` a file, then `PRAGMA integrity_check` on the copy | On demand |

## 15. Error model

`proposed`. On the OpenAI-format endpoints every error uses the OpenAI envelope, with the `type` and `code` of the
table. On `/v1/messages` it uses the Anthropic envelope, `{"type":"error","error":{"type":…,"message":…}}`, with the
same `type`; that envelope has no `code` field, so the code leads the message and is repeated in the
`x-stargaite-error-code` header. The request id is always in the `x-stargaite-request-id` header.

```json
{"error": {"message": "Unknown parameter: 'enable_search'.", "type": "invalid_request_error", "param": "enable_search", "code": "unknown_parameter"}}
```

| Situation | HTTP | `type` | `code` |
|---|---|---|---|
| Malformed body, unknown parameter, invalid effort | 400 | `invalid_request_error` | `invalid_request`, `unknown_parameter`, `invalid_reasoning_effort` |
| Missing or invalid key | 401 | `authentication_error` | `invalid_api_key` |
| Budget exhausted | 402 | `billing_error` | `budget_exceeded` |
| Alias not allowed for the key | 403 | `permission_error` | `model_not_allowed` |
| Unknown alias | 404 | `not_found_error` | `model_not_found` |
| Refused configuration change (admin API), with the list of findings | 400 | `invalid_request_error` | `invalid_request` |
| Unknown admin resource | 404 | `not_found_error` | `not_found` |
| Admin conflict | 409 | `invalid_request_error` | `conflict` |
| Body too large | 413 | `invalid_request_error` | `request_too_large` |
| Valid request that no target of the alias can serve | 422 | `invalid_request_error` | `endpoint_not_supported`, `capability_mismatch`, `max_output_tokens_exceeded`, `unsupported_parameter` |
| Rate, parallel or quota limit reached | 429 | `rate_limit_error` | `rate_limit_exceeded`, `parallel_limit_exceeded`, `quota_exceeded` |
| Internal error | 500 | `api_error` | `internal_error` |
| Provider error with no fallback left | 502 | `api_error` | `upstream_error`, `upstream_auth_error` |
| The provider's own `400`, `404`, `413`, `422` or `429` with no fallback left | The provider's status | The `type` this table gives for that status | `upstream_error` |
| No target available; usage store unavailable | 503 | `overloaded_error` | `no_target_available`, `usage_store_unavailable` |
| Provider timeout | 504 | `api_error` | `upstream_timeout` |

What a provider's own error becomes, once no fallback is left: a `400`, `404`, `413` or `422` is returned with the
provider's status and message, with `code: upstream_error`. A `401` or `403` means the operator's credential is
wrong, not the client's: it is returned as `502 upstream_auth_error` with a fixed message, and the provider's text
goes only to the log. A `429` is returned as `429 upstream_error` with the provider's `Retry-After`. A `5xx`, a `3xx`,
any other status, or a stream `error` event before anything was sent to the client, is returned as `502 upstream_error`.

## 16. Security

All `proposed`.

| Id | Rule |
|---|---|
| SEC-1 | A key is opaque and random: `sk-stargaite-<public id>-<secret>`. The public id is 12 lowercase base32 characters and the secret at least 256 bits in base32 without padding, so neither contains `-`. It carries no identity. Only a SHA-256 hash of the full token is stored; the key is located by its public id in the in-memory snapshot and compared in constant time |
| SEC-2 | Denial by default: a key reaches only the aliases in its `models` list. The admin key cannot request inference and a client key cannot administer |
| SEC-3 | The admin API has its own listener, on loopback by default. The admin key comes from `STARGAITE_ADMIN_KEY`; when that is unset, from `<data-dir>/admin.key`, which the proxy creates at start if it is missing, atomically and with mode `0600`, before the admin listener opens. A generated admin key is 52 lowercase base32 characters; a supplied one shorter than 32 characters, or starting with `sk-stargaite-`, stops `serve`. An optional read-only token from `STARGAITE_METRICS_TOKEN` reads the metrics endpoint and nothing else |
| SEC-4 | Every outbound URL set through the admin API (a provider's `base_url` and `rerank_url`, and the telemetry endpoint) is validated when it is set and again at connection time: HTTPS and public addresses unless `allow_private_network` is set on that provider or on the exporter; link-local and cloud metadata addresses never; no scheme other than HTTP(S). The check is made on the resolved address at connection time, so a name that later resolves elsewhere is refused. Redirects are never followed: a `3xx` is an upstream error. When a change sets a URL its name is resolved: a forbidden address refuses the change and a failed lookup only warns. Outbound proxy variables (`HTTP_PROXY` and the like) are ignored in v1, because the check would see the proxy's address instead of the destination's |
| SEC-5 | Secrets are referenced, never stored: provider and telemetry credentials are a syntactically valid environment variable name, which may not start with `STARGAITE_` in any letter case. Static telemetry headers (TEL-2) are not credentials: the reserved names are refused, and any header that carries a secret is given with `--header-env`. Credential values never appear in the store, `export`, logs, spans, usage records or API responses |
| SEC-6 | A listener outside loopback needs TLS: either the proxy terminates it (`--tls-cert`, `--tls-key`) or the operator states that a trusted front end does (`--behind-tls-proxy`); otherwise `serve` refuses to start. The TLS flags cover both listeners and the rule is evaluated for each. Both servers set read, write and idle timeouts and a header size limit; a streaming response extends its own write deadline on each chunk. Provider certificates are always verified |
| SEC-7 | Every admin change, whether applied, refused or without effect, leaves an audit record: who, what, the previous value, the new value and the result. Reads and dry runs are not audited, and a failed authentication goes to the log |
| SEC-8 | `keys rotate` issues a new secret and keeps the previous one valid for a grace period (`--grace`, 1 h by default); a rotation during a grace invalidates the oldest secret at once, so a key never has more than two. `keys revoke` invalidates every secret of the key: new requests are refused within 1 s, and requests in flight finish |
| SEC-9 | Prompts and responses are not persisted or logged. Capturing content in spans is the explicit, temporary, audited opt-in of TEL-8 |
| SEC-10 | The process runs without root. The container image has a read-only file system except the data directory |

## 17. Architecture decisions

`proposed` unless stated.

| ADR | Decision | Chosen | Discarded |
|---|---|---|---|
| ADR-001 | Language | Go 1.27 or later, one static binary for Linux and macOS. `decided` | Rust, C++; Windows in v1 |
| ADR-002 | HTTP and JSON | Standard `net/http`. Bodies are read and edited in place by a single-pass scanner that rejects duplicate keys (PAR-1), built on the JSON tokenizer of the standard library (`encoding/json/jsontext`); message content stays raw JSON whenever the format does not change. `decided` | Web frameworks; decoding every body into structs; `gjson` and `sjson`, which do not detect duplicate keys |
| ADR-003 | Storage | Embedded SQLite through `modernc.org/sqlite` (no CGO), a single writer goroutine fed by a bounded channel and a separate read pool. Every connection is opened with `journal_mode=WAL`, `synchronous=FULL`, `foreign_keys=ON` and `busy_timeout=5000`; none of these is part of a migration | A CGO driver; an external PostgreSQL; a key-value store |
| ADR-004 | Single source of truth | The embedded store, changed only through the admin API, published as immutable in-memory snapshots, with `export` and `apply` as YAML. `decided` | A hand-edited file plus database overrides; two stores |
| ADR-005 | Upstream protocols | Two adapters: `openai` and native `anthropic`. What differs between providers of one protocol is declared as data on the provider or the target | One adapter per provider; code keyed by provider name |
| ADR-006 | Inbound formats in v1 | Chat Completions, Messages (toward `anthropic` targets only), Embeddings and a rerank passthrough. `decided` | The Responses API; translating Messages to `openai` targets |
| ADR-007 | Unknown parameters | Rejected by default, with explicit per-alias `forward` or `drop` rules. `decided` | Dropping silently; forwarding everything |
| ADR-008 | Reasoning control | Declared per target, translated by a deterministic table, set per alias as `default` or `forced`. `decided` | A capability table keyed by model name inside the code; one alias per effort level |
| ADR-009 | Source of observable values | The final serialized outgoing body, built by one function shared by the data plane and `explain` | Attributes computed from the request's intent |
| ADR-010 | Telemetry | One OTLP/HTTP JSON exporter on the standard library, GenAI semantic conventions, one span schema for every backend. `decided` | Vendor SDKs; a custom webhook format; the full OpenTelemetry SDK as a dependency; per-backend attribute sets |
| ADR-011 | Keys | Opaque token with a public id and a secret of at least 256 bits, SHA-256 and constant-time comparison | A slow hash on every request; identity packed into the key; JWT |
| ADR-012 | Cost | USD only. The provider's report first, then the target's declared prices; empty when unknown. `decided` | Several currencies; invented prices; fetching prices from the internet at run time |
| ADR-013 | Subscription plans | A billing mode of its own, metered in requests and tokens per window. `decided` | Emulating a pay-per-use API with a nominal price |
| ADR-014 | Budget enforcement | Charged after the response, with the overshoot bounded by parallelism and output cap | Reserving funds before each request |
| ADR-015 | Usage store failure | `pay_per_use` targets become ineligible; `subscription` and `none` keep serving; no record is lost silently (USE-10) | Failing open with silent loss; a second on-disk journal |
| ADR-016 | Durability | `synchronous=FULL` on each usage batch. What a process crash can lose is what was not yet committed: the open batch and the records waiting in the writer's queue | `synchronous=NORMAL` with a claim of zero loss |
| ADR-017 | Retention | Finite and enforced by the process | Unlimited growth; an external scheduler |
| ADR-018 | Backup and integrity | `VACUUM INTO` while running, with `PRAGMA integrity_check` on the copy; `PRAGMA quick_check` in the background after start | Copying the database file while it is open; a full check before becoming ready |
| ADR-019 | Provider presets | A built-in catalog of providers, copied into the configuration when used | Looking presets up at run time; a model catalog; a catalog synchronised from the internet |
| ADR-020 | Reasoning text | Relayed unchanged on native paths; inline `<think>` blocks removed only for targets that declare them | Parsing and moving every provider's reasoning field |
| ADR-021 | Topology | One writer instance with a persistent data directory and an exclusive lock on it, one instance per environment, and a separate admin listener | Active-active; environments inside one instance |

Distribution: a static binary and an OCI image on a minimal base. The image runs plain `serve`, on loopback like the
binary; the guide shows the explicit flags that open it (SEC-6). Indicative container resources, to be adjusted with
the benchmark: request 50m CPU and 64 Mi, limit 1000m CPU and 256 Mi. The proposed package layout is in annex C.

## 18. Non-functional targets

Targets, not accepted requirements, until an automated, versioned and reproducible benchmark validates them. The mock
provider is the local stand-in for a provider that the repository ships for tests.

| Aspect | Target |
|---|---|
| Memory at rest | ≤ 40 MB RSS |
| Memory with 100 concurrent streams | ≤ 100 MB |
| Added latency, P50 | ≤ 0.5 ms, on a local network against the mock provider |
| Added latency, P99 | ≤ 2 ms when the stream is not transformed; ≤ 5 ms when SSE is inspected or adapted |
| Start-up | ≤ 2 s until ready |
| Throughput | ≥ 1,000 requests per second without streaming |
| Concurrency | 500 concurrent streams |
| Configuration change | ≤ 1 s to publish with up to 100 providers, 1,000 targets, 1,000 aliases and 10,000 keys |
| Availability | 99.9 % monthly, not counting the total outage of every provider of an alias |
| Recovery | Loss bounded as in ADR-016; RTO ≤ 15 minutes |
| Cancellation | Upstream call cancelled ≤ 1 s after the client disconnects |
| Image size | ≤ 30 MB |

## 19. Publication and packaging

The project is published as open source and has to work for anyone. All `decided`.

| Id | Requirement | How it is checked |
|---|---|---|
| PUB-1 | MIT license | The repository carries the license file from its second commit; until then this document declares it (§1) |
| PUB-2 | Universal: neither file contents nor commit messages, in any revision, carry the name of an organization or a person that develops, deploys or uses the product, nor data of a particular infrastructure. Public products and providers may be named as examples. Three identities are outside this rule because publishing needs them: the author field of commits, the copyright holder in the license, and the hosting namespace in the module path and the image name (OQ-9) | Search of the repository and of its commit messages |
| PUB-3 | Clone and run: whoever clones the repository builds and starts the proxy with their own keys, with no access to anything private | On a clean machine: clone, build, start, get an answer from `/health/ready`, and complete the quick start of §13.3 with the mock provider in place of the preset |
| PUB-4 | No secrets and no mandatory secret manager: credentials arrive through environment variables that the deployer provides | The repository contains no secrets and the proxy starts with environment variables alone |
| PUB-5 | Reproducible continuous integration: nothing in it depends on private runners, actions, templates or credentials. Every check runs locally with one command (`make ci`), and the hosted CI only calls it | The CI passes on a fork with no secret configured |
| PUB-6 | Public dependencies only | The build asks for no credentials |
| PUB-7 | The first commit contains only this `kick-off.md` | History of the repository |
| PUB-8 | The repository and all its documentation are in English | Review of the repository |

Consequences, `proposed`:

- The default CI runs only against the local mock provider and an in-process OTLP receiver. Checks against live
  providers (`targets verify`, the contract suite) and against a real observability backend (TEL-14) are run by
  maintainers with their own keys.
- The proxy starts and answers `/health/ready` with no provider configured and with no credential present (OPS-6).
- The configuration of each deployment (its providers, aliases, budgets and keys) lives in that deployment's data
  directory, never in this repository. The repository ships only `config.example.yaml`, an input for `apply` that
  uses no absolute path and needs no privilege.

## 20. Versions, features and phases

| Version | Goal | Closes with | Status |
|---|---|---|---|
| v1 | A proxy fit for production use: chat completions, messages, embeddings and rerank over model aliases; honest parameters and reasoning control; keys, limits, budgets and quotas; durable usage; OTLP telemetry; admin API and CLI; published as §19 requires | The criteria of §21 and the requirements of §19 | proposed |
| Later | Only with confirmed demand: the Responses API, more protocols, a configuration import helper | — | proposed |

Each feature is meant to be an independent change.

| Id | What | Priority | Phase | Depends on |
|---|---|---|---|---|
| project-scaffold | License, quick-start guide, `config.example.yaml`, container image, local mock provider, `make ci` and the public CI (§19) | P0 | 1 | — |
| state-store | SQLite store with migrations, validation, atomic snapshots and revisions, the export and apply logic; `serve` with the health probes and the data-directory lock | P0 | 1 | — |
| admin-cli | Admin API on its own listener, admin key bootstrap, audit, and the CLI as its client: `--json`, exit codes, `export`, `apply`, `validate` | P0 | 1 | state-store |
| keys-auth | Keys (create, rotate, revoke), authentication, the alias allow-list and `/v1/models`, the first authenticated route | P0 | 1 | admin-cli |
| providers-targets | Providers with presets, URL guardrails and credential references; targets with capabilities and prices | P0 | 1 | admin-cli |
| chat-completions | `/v1/chat/completions` synchronous and SSE, the `openai` adapter, body size (LIM-6) | P0 | 2 | keys-auth, providers-targets |
| param-contract | Known-parameter schemas, rules for unknown parameters, capability checks, the list of changes, response headers, output-cap layers | P0 | 2 | chat-completions |
| reasoning-control | Dialects, per-target declaration, translation table, alias modes, the validation rule, `models explain`, removal of inline think tags | P0 | 2 | param-contract |
| anthropic-adapter | The native `anthropic` adapter for chat completions | P0 | 2 | reasoning-control |
| messages-endpoint | `/v1/messages`, native toward `anthropic` targets | P0 | 2 | anthropic-adapter |
| resilience | Timeouts, fallback, circuit breaker, cancellation, graceful shutdown | P0 | 2 | chat-completions |
| usage-accounting | Usage records, cost with its source, the batch writer and its failure policy, daily aggregates, `usage show` | P0 | 3 | chat-completions |
| limits | Parallel slots, `rpm_limit`, `tpm_limit` | P0 | 3 | chat-completions |
| budgets | USD budgets per key, per alias and global | P0 | 3 | usage-accounting |
| subscription-quotas | The `subscription` billing mode, quota windows, `quota_share`, `usage quota` | P0 | 3 | usage-accounting |
| telemetry-otlp | Span schema, OTLP exporter and queue, `telemetry set`, `test` and `status`, `traceparent`, sampling, content opt-in | P0 | 3 | usage-accounting |
| embeddings | `/v1/embeddings` | P1 | 4 | usage-accounting |
| rerank | `/v1/rerank` passthrough | P1 | 4 | embeddings |
| targets-verify | The live check of a target | P1 | 4 | reasoning-control, usage-accounting |
| metrics | The metrics endpoint | P1 | 4 | usage-accounting |
| operations | Backup, retention, `secrets reload`, tested migrations and restore | P1 | 4 | usage-accounting |
| hardening | The adversarial suite and the benchmark | P1 | 4 | everything else |
| responses-api | `/v1/responses`, as an independent interface | P2 | — | — |
| config-import | Convert the configuration of an existing gateway into a file for `apply` | P2 | — | — |

| Phase | Priority | What it leaves usable | Features |
|---|---|---|---|
| 1 | P0 | A repository anyone clones, builds and starts, and configures entirely through the CLI | `project-scaffold`, `state-store`, `admin-cli`, `keys-auth`, `providers-targets` |
| 2 | P0 | Inference end to end, with visible parameters, controlled reasoning effort, two protocols and resilience | `chat-completions`, `param-contract`, `reasoning-control`, `anthropic-adapter`, `messages-endpoint`, `resilience` |
| 3 | P0 | Spend measured and limited, and every request visible in an observability backend | `usage-accounting`, `limits`, `budgets`, `subscription-quotas`, `telemetry-otlp` |
| 4 | P1 | Complete and verified operation | `embeddings`, `rerank`, `targets-verify`, `metrics`, `operations`, `hardening` |

## 21. Acceptance criteria and test strategy

v1 is accepted when all of these hold, together with the requirements of §19. A criterion is checked once the last
feature it needs exists; the column names the feature that owns it. A criterion that also cites a span or a usage
record is checked in phase 2 through the response headers and what the proxy observed of each provider call, and
extended to the span and the usage record in phase 3.

| Id | Criterion | Rules | Feature |
|---|---|---|---|
| AC-01 | An OpenAI client works by changing only the base URL, the key and the model name | PRI-3 | chat-completions |
| AC-02 | An Anthropic client works against `/v1/messages` by changing only the base URL, the key and the model name, including a tool loop with thinking enabled | PRI-3, RSN-12 | messages-endpoint |
| AC-03 | `make ci` passes on a fork with no secret configured, and the image builds | PUB-5, PUB-6 | project-scaffold |
| AC-04 | A fresh clone reaches a first answer with the quick start of §13.3 against the mock provider, with no file edited by hand | PUB-3, OPS-1 | chat-completions |
| AC-05 | Changing the targets of an alias needs no change and no restart in the clients, and a stream open during the change is not cut | OPS-4, RES-7 | chat-completions |
| AC-06 | A key reaches only its allowed aliases; a key with none reaches nothing; the admin key cannot request inference | SEC-2 | keys-auth |
| AC-07 | A rotated key keeps its previous secret for the grace period and never has more than two; a revoked key is refused within 1 s | SEC-8 | keys-auth |
| AC-08 | An unknown body parameter gets `400` with its name; with a `forward` or `drop` rule, the behaviour and the listing match the rule; a `forward` rule for a name the proxy writes is refused as a change; a body with a repeated key gets `400` | PAR-1, PAR-2 | param-contract |
| AC-09 | Comparing inbound and outbound bodies over a generated corpus, no parameter is removed or added and no value is changed without appearing in the span, the usage record and the response header | PAR-3, PAR-8 | param-contract |
| AC-10 | The proxy never sends an output cap that the client or the alias did not set and never lowers one; a change is refused for an alias with an `anthropic` target and no cap, or with a cap above a target's limit | PAR-6, PAR-7 | param-contract |
| AC-11 | A contractual feature or a parameter that the target cannot take leads to the next target or to `422` before any provider is called | PAR-4, PAR-5, PAR-12 | param-contract |
| AC-12 | Responses carry the headers of PAR-9, each under its stated condition, with the values of the attempt that served | PAR-9 | param-contract |
| AC-13 | For every dialect, the seven levels produce the documented outgoing body, including the "off" form and a target's own `on` and `off` fragments; `none` on a target that cannot turn reasoning off is sent as its lowest level; the three invalid inputs get `400` before any provider is called | RSN-3, RSN-4, RSN-11 | reasoning-control |
| AC-14 | The four combinations of client with or without an effort and alias in `default` or `forced` mode give the documented result; no request leaves without an effort when the alias sets one, except toward a target whose dialect is `none`, where it is listed as dropped | RSN-5 | reasoning-control |
| AC-15 | A change is refused for an alias that points, as primary or fallback, to a target that always reasons and sets no effort | RSN-7 | reasoning-control |
| AC-16 | With two aliases on one target, changing one does not change the body the other sends, and every level of the model is reached through that one target | PAR-11, RSN-8 | reasoning-control |
| AC-17 | For the same input, the body returned by `models explain` is byte-identical to the one the mock provider receives | PAR-10 | reasoning-control |
| AC-18 | For every dialect and every attempt, the "sent" values of effort and output cap in the span, the usage record and the headers equal what the mock provider received | RSN-6, PAR-8, USE-5, TEL-9 | reasoning-control |
| AC-19 | A target that declares inline think tags has them removed from the visible content, also when split across chunks, unclosed or quoted literally (a JSON-escaped spelling of a tag), and the removal is recorded | RSN-12 | reasoning-control |
| AC-20 | A chat completions request to an `anthropic` target returns an OpenAI-shaped response, synchronous and streaming, with the system prompt, tool calls, usage and finish reason mapped | PRI-3, ADR-005 | anthropic-adapter |
| AC-21 | On a fallback the effort is translated again for the new target and the failed attempt is a child span of the same trace; when no target is left the error is the one RES-10 states | RES-1, RES-10, RSN-10, TEL-6 | resilience |
| AC-22 | Five eligible failures in 30 s open a target's circuit for 20 s and the two trial requests close or reopen it; each timeout of RES-3 fires at its value, and a non-streaming request longer than 30 s is not cut | RES-2, RES-3 | resilience |
| AC-23 | A client disconnect cancels the upstream call within 1 s and still leaves a usage record; a stream that fails midway ends with one terminal error event; `SIGTERM` drains requests in flight and persists pending records | RES-4, RES-5, RES-6 | resilience |
| AC-24 | `telemetry test` tells apart `credential_unresolved`, `unreachable`, `unauthorized`, `not_found`, `rejected` (by status and inside a `200`) and `accepted` against a mock OTLP endpoint, and the mock receives the header composed from the referenced variables | TEL-1, TEL-2 | telemetry-otlp |
| AC-25 | The golden test of the span schema passes: name and attribute set are stable; the key is present on success, streaming, error and attempt spans, the alias on each of them whose request body named one, and the target on every span for which one was selected | TEL-4, TEL-9 | telemetry-otlp |
| AC-26 | With full tracing, every authenticated request that fails, including those rejected before a provider, produces exactly one request span with error status that keeps the key and, when the request carried them, the alias and the session | TEL-5 | telemetry-otlp |
| AC-27 | With a valid `traceparent` the span joins the client's trace under a new span id; without it, or with a malformed one, a new trace starts; `x-session-id` sets the session | TEL-6 | telemetry-otlp |
| AC-28 | With tracing off or sampled, for an alias and for a key, usage records, budgets, quotas and error counters are identical to those with full tracing | TEL-7 | telemetry-otlp |
| AC-29 | A fake secret planted in a prompt and in a header appears in no span, log or usage record; content is exported, truncated, only for a key with an unexpired opt-in | TEL-8, SEC-9, OPS-9 | telemetry-otlp |
| AC-30 | In the integration job of TEL-14, a trace arrives at a real Langfuse and can be filtered by key, alias and target | TEL-14 | telemetry-otlp |
| AC-31 | The exporter being down or slow does not affect inference; rejected and dropped spans are counted and show in `telemetry status` with the last error | TEL-3, TEL-12 | telemetry-otlp |
| AC-32 | Successful, failed and aborted requests all leave a usage record with the reasoning tokens as a part of the output total, the finish reason and the returned model; a mismatch is flagged | USE-1, USE-3, USE-4 | usage-accounting |
| AC-33 | Tokens are recorded without a price; each cost carries its source; the provider's figure wins over the price table; estimated tokens are marked as such | USE-6, USE-8 | usage-accounting |
| AC-34 | A failing usage store triggers the policy of USE-10, and every record produced meanwhile ends in the store or in the log | USE-10 | usage-accounting |
| AC-35 | `usage show --by key,model,effort-sent --compare-previous` returns the medians of two windows from the local store alone | USE-9 | usage-accounting |
| AC-36 | Traffic through a `subscription` provider changes no USD total; its exhausted window causes a fallback or `429`; a key's `quota_share` is enforced for that key only | USE-7, LIM-8, LIM-9, LIM-10 | subscription-quotas |
| AC-37 | An exhausted budget makes `pay_per_use` targets unavailable and gets `402` when no other target is left, before any provider is contacted; it never blocks a `subscription` or `none` target; key and alias budgets are independent; the overshoot stays within the bound of LIM-4 | LIM-1, LIM-3, LIM-4 | budgets |
| AC-38 | `rpm_limit`, `tpm_limit` and `max_parallel_requests` act independently of each other and of budgets, and answer `429` with `Retry-After` | LIM-5, LIM-6 | limits |
| AC-39 | `export` followed by `apply` on an empty instance reproduces the configuration with new key secrets; an invalid file or a refused change leaves the active configuration untouched | OPS-3, OPS-5 | admin-cli |
| AC-40 | Every admin change is audited with the previous and the new value and increments the revision, and spans and usage records carry the version and the revision | SEC-7, OPS-4, TEL-13, USE-5 | admin-cli |
| AC-41 | No command prompts, with or without a terminal; every command honours `--json` and the exit codes of OPS-2 | OPS-2 | admin-cli |
| AC-42 | The proxy runs from one binary and one data directory, with no external service, and a second `serve` on the same directory refuses to start | PRI-2, OPS-10 | state-store |
| AC-43 | Outbound URLs follow SEC-4 for providers and for the telemetry endpoint, including a name that resolves to a private address after validation and a redirect | SEC-4 | providers-targets |
| AC-44 | A credential added to the env file works without a restart, and a provider whose credential does not resolve is disabled without stopping the proxy | OPS-6, SEC-5 | providers-targets |
| AC-45 | A listener outside loopback without TLS and without `--behind-tls-proxy` refuses to start | SEC-6 | state-store |
| AC-46 | Embeddings and rerank requests are served, metered and attributed to a key and an alias | USE-1 | embeddings, rerank |
| AC-47 | Against the mock provider, `targets verify` reports the outgoing body and the reasoning tokens per level, stores its result and records its calls under `_verify` | RSN-9 | targets-verify |
| AC-48 | After a scripted run, the figures of the metrics endpoint equal those computed from the usage store and the scripted counts | OPS-8 | metrics |
| AC-49 | Retention runs inside the process and the store stops growing under constant traffic | USE-11 | operations |
| AC-50 | Migrations and the restore of a backup are tested | OPS-7, ADR-018 | operations |
| AC-51 | The adversarial suite passes | — | hardening |
| AC-52 | The benchmark meets the targets of §18 or documents the accepted deviations | — | hardening |
| AC-53 | `providers presets` lists the eighteen presets of §7.2 and none sets `allow_private_network`; a golden test fixes the data of every preset, and `providers add` with a preset stores exactly that data plus the explicit flags; before the v1 release a maintainer has run the live-safe parts of the contract suite against each of the first ten | ADR-019, SEC-4 | providers-targets |

Test strategy, `proposed`:

| Kind | What it covers |
|---|---|
| Unit | Authentication, authorization, parameter validation, translation tables, routing, limits, prices, budgets, quotas, circuit breaker, errors |
| Golden | The outgoing body per dialect and per protocol; the span schema; the exported YAML; the preset catalog |
| Contract, per target | Simple request, streaming, cancellation, usage, structured output, tools if declared, each effort level, errors, timeout and rate limit. In CI against the mock provider; maintainers run the parts that are safe against a live provider when validating a preset |
| Differential | The same request sent directly and through the proxy gives an equivalent response within the declared contract |
| Adversarial | Malformed JSON, duplicate JSON keys, oversized body, revoked key, unknown alias, incompatible target, fragmented SSE chunks, think tags split, unclosed or quoted literally, truncated streams, SSRF, budget races |
| Benchmark | Latency, memory and throughput of §18, automated and reproducible, against the mock provider |

## 22. Risks

| Id | Risk | Sign that it is happening | Response |
|---|---|---|---|
| R1 | Scope drift: rebuilding a general-purpose gateway with a smaller community | Requests for policies per organization, complex key rotation, routing by latency, semantic caching or several observability modes | The membership test of §4 and an ADR for each new capability |
| R2 | Sent is not applied: the provider ignores the effort or changes its default | Reasoning tokens do not move when the effort sent does | `usage show --by effort-sent` and `targets verify` (USE-9, RSN-9) |
| R3 | A dialect is declared wrongly; the mock provider checks the body, not the effect | A live provider rejects a level, or reasoning tokens are the same at every level | `targets verify` against the live provider, and the warning for unverified targets |
| R4 | The observability backend changes its ingestion, or shows attributes elsewhere; an accepted span is not necessarily a visible trace | A trace is accepted and cannot be found or filtered | The integration job (TEL-14), compatibility attributes in the one schema (TEL-11), and the trace id printed by `telemetry test` |
| R5 | The GenAI semantic conventions are still in development and rename attributes | A backend stops recognising a field | A pinned version in the span schema; a change is a new schema version with its golden test |
| R6 | Strict rejection breaks existing clients during a migration, and the easy way out is a permissive switch | Requests for a global "drop unsupported" option | Per-alias `forward` and `drop` rules, always listed (PAR-2). No global switch |
| R7 | Known-parameter schemas fall behind the formats they implement | A new standard parameter gets `400` | Schemas pinned and updated with each release; the `forward` rule as the way through meanwhile |
| R8 | Budget overshoot from parallel requests | Spend above the limit | Parallel limits and output caps (LIM-4) |
| R9 | Stale prices | Computed cost drifts from the invoice | Prices carry an `as_of` date; the provider's own figure is preferred (USE-6) |
| R10 | The provider does not report usage | Records with `tokens_source: estimated` | Estimates are marked as such (USE-8) |
| R11 | SSRF through the admin API | A request toward an unexpected host | SEC-4 |
| R12 | Loss of usage records | Spend and records do not match | ADR-015 and ADR-016 |
| R13 | Telemetry gaps under bursts, because export is best effort | Spans dropped in `telemetry status` | The usage store is the record |
| R14 | Streaming saturation | Memory grows without bound | Backpressure and parallel limits |
| R15 | Two processes or two instances spend the same budget twice | A second replica is deployed | The lock on the data directory (OPS-10); v1 is single-writer and says so (ADR-021) |
| R16 | Each protocol and each dialect is maintenance surface | Their number grows | A new one needs a real use case and its tests |
| R17 | A subscription plan's terms forbid the way it is being used | A provider's notice or a blocked key | Stated in §7.3; the operator's responsibility |
| R18 | Something private slips into the public repository: a name, a secret or a dependency others cannot use | The CI fails on a fork, or a proper name shows up in a file or a commit message | The requirements of §19, checked before each publication |

## 23. Hypotheses

Success and abandonment thresholds with a deadline are `open` (OQ-3).

| Category | We believe that | Will lead to | Measured with |
|---|---|---|---|
| use | an OpenAI or Anthropic client moves to the proxy by changing only the base URL, the key and the model name | replacing a general-purpose gateway without touching agent logic | AC-01 and AC-02: in CI with request bytes recorded from the official SDKs; with unmodified client SDKs, run by maintainers against the mock provider before each release |
| visibility | recording requested, alias and sent values, read from the outgoing body, ends the misdiagnosis of reasoning effort | answering "what did this request send, and did it matter?" with one command | Whether `models explain` and `usage show` answer it without reading code or provider logs |
| telemetry | one OTLP exporter with one fixed schema survives upgrades of the proxy and of the backend | no reconfiguration and no lost attribution after an upgrade | The golden test and the integration job across versions |
| data | a local SQLite store is enough as the official record of usage | reliable budgets without an external database | Committed records lost: 0 |
| operation | the core stays small | a low maintenance cost | Lines of production code and effort once stable: at most 20,000 lines and 0.2 FTE |
| adoption | anyone can clone it and run it | usefulness without adapting the code | PUB-3 and PUB-5 on a clean machine and on a fork |

## 24. Out of scope

Never in the core:

| What | Why |
|---|---|
| Running agents, reasoning loops or tools | It is a proxy, not an agent platform |
| Hosting MCP servers or clients | Outside the request path. An external MCP server can drive the admin API |
| Guardrails, moderation, PII detection, evaluation of responses | Outside the request path |
| Conversation memory; prompt management or versioning | The agent's or the observability backend's job |
| RAG, vector stores, semantic caching | They need external dependencies |
| A graphical interface or web panel | PRI-2 |
| Authentication of people (SSO, OIDC) | Identity is a technical key |
| Any notion of end user: identity, limits, budgets or metadata | The proxy governs keys and aliases, not people |
| Currencies other than USD | PRI-11 |
| Vendor SDKs for telemetry | PRI-10 |
| A catalog of models, or prices fetched from the internet | ADR-012, ADR-019 |
| Executable plugins loaded at run time | Maintenance |
| Routing decided by another LLM, by cost or by latency | Selection is deterministic: the order of `targets` |
| An inventory of agents, organizations or tenants | Governed from outside the product |
| Names of organizations or organisational rules in the code | PRI-13 |

Outside v1 only, each with confirmed demand and an ADR (R1): the Responses API; translating Messages to `openai`
targets; more protocols; a configuration import helper; audio and image generation endpoints; active-active with
distributed accounting; rolling budget windows.

## 25. Assumptions

| Id | Assumption | Affects | If it turns out false |
|---|---|---|---|
| ASM-1 | Two upstream protocols cover the providers that matter | chat-completions, anthropic-adapter | More adapters and more maintenance (R16) |
| ASM-2 | With spend charged after the response, a small overshoot in parallel bursts is acceptable | budgets | A prior reservation would be needed |
| ASM-3 | One instance is enough for v1 | usage-accounting | An external budget store or a central control plane |
| ASM-4 | The targets of §18 are reachable | hardening | They are documented as accepted deviations (AC-52) |
| ASM-5 | Each consumer can carry its own bearer key, including the HTTP nodes of workflow tools | keys-auth | Another way of identifying the consumer would be needed |
| ASM-6 | The observability backends of interest accept OTLP over HTTP with JSON encoding | telemetry-otlp | A protobuf encoder, still without a vendor SDK |
| ASM-7 | Providers report reasoning tokens for the models where effort matters | usage-accounting | The effect of an effort change can only be inferred from output tokens and latency |
| ASM-8 | During a migration, non-standard parameters can be removed from clients or covered by per-alias rules | param-contract | Pressure for a permissive mode (R6) |
| ASM-9 | Rerank services accept or ignore the `model` field the client must send to name the alias | rerank | The field is removed for targets that declare so |

## 26. Open questions

None blocks the start of phase 1. OQ-9 was the only blocking one, because its three parts cannot be taken back once
published, and it is settled; so is the CI host of OQ-4.

| Id | To decide | Options or proposal |
|---|---|---|
| OQ-1 | Which span attributes each backend shows as filterable fields or tags. If key, alias or target cannot be filtered somewhere, a compatibility attribute is added to annex D as a new schema version (TEL-11) | Verified with a real trace while building `telemetry-otlp` |
| OQ-2 | The unit and the kind of window of each subscription plan's allowance (requests or tokens; sliding or fixed), which fixes the `quota` an operator declares and whether LIM-8 needs fixed windows besides sliding ones | Checked against each provider's published terms |
| OQ-3 | Success and abandonment thresholds, with a deadline, for the hypotheses of §23 | — |
| OQ-4 | The CI host and how releases are distributed (binaries, a public image registry), within PUB-5 | Settled for the CI host: GitHub Actions, with a workflow that only calls make targets. Proposal for releases: binaries on the repository's release page and the image at `ghcr.io/jorlaru/stargaite` |
| OQ-5 | Whether any consumer needs the Responses API | Out of v1 unless confirmed. Evidence so far: some OpenAI models offer tool calling, or some effort levels, only on the Responses API; their targets declare `tools: false` and their real `levels`, so PAR-4 and RSN-4 handle them |
| OQ-6 | Whether to ship `config-import` for people coming from another gateway | Out of v1 unless confirmed |
| OQ-7 | The default `max_parallel_requests` of a new key | Proposal: 5 |
| OQ-8 | Whether the binary, the variable prefix and the key prefix keep the full project name | Proposal: yes |
| OQ-9 | The identity the project is published under: commit author, copyright holder in the license, hosting namespace in the module path and the image name (PUB-2) | Settled: the commit author is the project identity `StargAIte`, the license names `StargAIte contributors`, and the hosting namespace is `github.com/jorlaru` |
| OQ-10 | How thinking blocks survive a tool loop on the translated path (chat completions to an `anthropic` target), where the OpenAI format has no field for them | Settled for phase 2: thinking blocks are dropped on the translated path, a chat tool loop with thinking toward an `anthropic` target is unsupported, and `/v1/messages` is the documented path for it. Proposal: return them in an opaque field of the assistant message that the client echoes back, and omit them when it does not; to be settled with the contract suite against a live provider |

## 27. Glossary

| Term | Meaning here | Avoid |
|---|---|---|
| model, model alias | The logical name a client puts in `model`; a contract with an ordered list of targets | route, deployment |
| target | One upstream model at one provider, written `provider/upstream-model` | model (for the upstream one) |
| provider | The connection to an inference service | connector, endpoint |
| key | A named, opaque credential; one per agent | user, principal, virtual key |
| admin key | The credential of the admin API | master key |
| data plane | The listener that serves inference: the routes of §13.1 | — |
| budget | A USD spend limit with a reset interval | — |
| quota | The allowance of a subscription plan, in requests or tokens per window | budget (for plans) |
| usage | The durable local record of requests, tokens and cost | ledger, spend log, telemetry |
| telemetry | Best-effort spans toward an external backend; never the official record | usage |
| dialect | The way a provider spells its reasoning control | — |
| effort | The reasoning level, on the canonical scale of §9.1 | — |
| requested, alias, sent | The three recorded layers of a value: what the client asked, what the alias sets, what left for the provider | — |
| contractual feature | One the client relies on: if a target lacks it, fallback or rejection, never degradation | — |
| preset | Built-in data copied into the configuration when a provider is added, or flags filled in for `telemetry set` | plugin, adapter |
| compatibility attribute | A span attribute emitted for every backend because one backend only reads the value under that name | — |
| mock provider | The local stand-in for a provider that the repository ships for tests: both protocols, scripted failures | — |
| snapshot | An immutable version of the active configuration in memory, published atomically | — |
| revision | The counter that identifies each published configuration | — |

## Annex A. Data model (SQLite)

Proposed base schema of the store. Configuration resources keep their declaration as validated JSON, so adding a field
does not need a migration. Times are Unix milliseconds in UTC. Every connection is opened with `journal_mode=WAL`,
`synchronous=FULL`, `foreign_keys=ON` and `busy_timeout=5000` (ADR-003); those settings are per connection and are
not part of any migration. Migrations become immutable from the first tagged release; until then a later phase may
still amend migration 001.

```sql
-- Migration 001: base schema

CREATE TABLE providers (
    name        TEXT PRIMARY KEY,
    spec        TEXT NOT NULL,        -- JSON: protocol, base_url, api_key_env, billing, quota, cost_report, ...
    updated_at  INTEGER NOT NULL
);

CREATE TABLE targets (
    id                TEXT PRIMARY KEY,   -- 'provider/upstream-model'
    provider          TEXT NOT NULL REFERENCES providers(name),
    upstream_model    TEXT NOT NULL,
    spec              TEXT NOT NULL,      -- JSON: kind, capabilities, max_output_tokens, reasoning, pricing, ...
    verified_at       INTEGER,            -- last successful `targets verify`
    verified_version  TEXT,
    updated_at        INTEGER NOT NULL
);

CREATE TABLE models (
    name        TEXT PRIMARY KEY,         -- the alias
    spec        TEXT NOT NULL,            -- JSON: requires, reasoning, max_output_tokens, params, budget, limits, ...
    updated_at  INTEGER NOT NULL
);

CREATE TABLE model_targets (
    model     TEXT NOT NULL REFERENCES models(name) ON DELETE CASCADE,
    position  INTEGER NOT NULL,           -- 0 = primary, 1 and 2 = fallbacks
    target    TEXT NOT NULL REFERENCES targets(id),
    PRIMARY KEY (model, position)
);

CREATE TABLE keys (                       -- never deleted: a revoked key keeps its name and its usage
    name        TEXT PRIMARY KEY,
    spec        TEXT NOT NULL,            -- JSON: budget, limits, quota_share, trace_sample_rate, content_until
    created_at  INTEGER NOT NULL,
    revoked_at  INTEGER
);

CREATE TABLE key_models (
    key_name  TEXT NOT NULL REFERENCES keys(name) ON DELETE CASCADE,
    model     TEXT NOT NULL,              -- an alias, or '*'
    PRIMARY KEY (key_name, model)
);

CREATE TABLE key_secrets (                -- at most two rows per key, and two only during a rotation
    public_id    TEXT PRIMARY KEY,
    key_name     TEXT NOT NULL REFERENCES keys(name) ON DELETE CASCADE,
    secret_hash  BLOB NOT NULL,           -- SHA-256 of the full token; never the token
    created_at   INTEGER NOT NULL,
    valid_until  INTEGER                  -- set on the previous secret when the key is rotated
);

CREATE TABLE settings (
    name        TEXT PRIMARY KEY,
    value       TEXT NOT NULL,            -- JSON
    updated_at  INTEGER NOT NULL
);

CREATE TABLE revisions (
    revision    INTEGER PRIMARY KEY,
    actor       TEXT NOT NULL,
    summary     TEXT NOT NULL,
    created_at  INTEGER NOT NULL
);

CREATE TABLE usage (
    id                    TEXT PRIMARY KEY,   -- unique per provider call; makes the write idempotent
    request_id            TEXT NOT NULL,
    trace_id              TEXT,
    attempt               INTEGER NOT NULL DEFAULT 1,
    endpoint              TEXT NOT NULL,      -- 'chat' | 'messages' | 'embeddings' | 'rerank'
    key_name              TEXT NOT NULL,      -- a key name, or '_verify'
    model                 TEXT NOT NULL,      -- the alias requested; '' for '_verify'
    target                TEXT NOT NULL,      -- 'provider/upstream-model' this attempt was sent to
    billing               TEXT NOT NULL,      -- 'pay_per_use' | 'subscription' | 'none'
    response_model        TEXT,               -- as returned by the provider
    model_mismatch        INTEGER NOT NULL DEFAULT 0,
    status_code           INTEGER NOT NULL,
    error_type            TEXT,
    finish_reason         TEXT,
    stream                INTEGER NOT NULL DEFAULT 0,
    input_tokens          INTEGER NOT NULL DEFAULT 0,   -- total input, cache tokens included
    cache_read_tokens     INTEGER NOT NULL DEFAULT 0,   -- part of input_tokens
    cache_write_tokens    INTEGER NOT NULL DEFAULT 0,   -- part of input_tokens
    output_tokens         INTEGER NOT NULL DEFAULT 0,   -- total output, reasoning included
    reasoning_tokens      INTEGER,                      -- part of output_tokens; NULL when not reported
    tokens_source         TEXT NOT NULL,      -- 'provider' | 'estimated'
    cost_usd              REAL,               -- NULL when unknown or not applicable
    cost_source           TEXT NOT NULL,      -- 'provider' | 'price_table' | 'none'
    effort_requested      TEXT,
    effort_alias          TEXT,
    effort_sent           TEXT,               -- level | 'on' | 'budget:<N>' | 'none' | NULL
    effort_source         TEXT,               -- 'client' | 'alias_default' | 'alias_forced' | 'provider_default'
    effort_adjusted       INTEGER NOT NULL DEFAULT 0,
    max_output_requested  INTEGER,
    max_output_alias      INTEGER,
    max_output_sent       INTEGER,
    params_changed        TEXT,               -- 'name:action,name:action'
    content_empty         INTEGER NOT NULL DEFAULT 0,   -- no text and no tool call
    ttft_ms               INTEGER,
    started_at            INTEGER NOT NULL,
    completed_at          INTEGER NOT NULL,
    config_revision       INTEGER NOT NULL,
    proxy_version         TEXT NOT NULL
);
CREATE INDEX idx_usage_time       ON usage(completed_at);
CREATE INDEX idx_usage_key_time   ON usage(key_name, completed_at);
CREATE INDEX idx_usage_model_time ON usage(model, completed_at);

CREATE TABLE usage_daily (                -- updated in the same transaction as `usage`; source of budget counters
    day                 TEXT NOT NULL,    -- '2026-11-11', UTC
    key_name            TEXT NOT NULL,
    model               TEXT NOT NULL,
    target              TEXT NOT NULL,
    billing             TEXT NOT NULL,
    requests            INTEGER NOT NULL DEFAULT 0,
    errors              INTEGER NOT NULL DEFAULT 0,
    input_tokens        INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens       INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens    INTEGER NOT NULL DEFAULT 0,
    cost_usd            REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (day, key_name, model, target)
);

CREATE TABLE budget_totals (              -- running totals of `never` budgets; not subject to retention
    scope_type  TEXT NOT NULL,            -- 'global' | 'key' | 'model'
    scope_name  TEXT NOT NULL,
    cost_usd    REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (scope_type, scope_name)
);

CREATE TABLE audit (
    id              TEXT PRIMARY KEY,
    actor           TEXT NOT NULL,
    action          TEXT NOT NULL,
    resource_type   TEXT NOT NULL,
    resource_name   TEXT NOT NULL,
    previous_value  TEXT,
    new_value       TEXT,
    result          TEXT NOT NULL,
    revision        INTEGER,
    created_at      INTEGER NOT NULL
);
```

## Annex B. Example configuration

What `stargaite export` produces and `stargaite apply -f` accepts; proposed content of `config.example.yaml`.
Every value is illustrative, including the reasoning levels and the quotas attributed to named models and plans.

```yaml
version: 1

settings:
  budget: { limit_usd: 2000, reset_interval: monthly }
  max_parallel_requests: 500
  max_body_bytes: 10485760
  max_response_bytes: 33554432
  timeouts: { connect_ms: 5000, first_byte_ms: 30000, stream_idle_ms: 120000, total_ms: 180000 }
  retention: { usage_days: 90, usage_daily_months: 36, audit_months: 24 }
  telemetry:
    otlp_endpoint: https://langfuse.example.com/api/public/otel
    basic_auth_env: [LANGFUSE_PUBLIC_KEY, LANGFUSE_SECRET_KEY]
    headers: { x-langfuse-ingestion-version: "4" }   # filled in by `--preset langfuse`
    trust_traceparent: true
    content_max_bytes: 32768

providers:
  openrouter:
    protocol: openai
    base_url: https://openrouter.ai/api/v1
    api_key_env: OPENROUTER_API_KEY
    billing: pay_per_use
    cost_report: { path: usage.cost, usd_per_unit: 1 }
    stream_usage: native
    gen_ai_provider: openrouter
    target_defaults: { dialect: reasoning_object, output_cap_param: max_tokens }
  anthropic:
    protocol: anthropic
    base_url: https://api.anthropic.com
    api_key_env: ANTHROPIC_API_KEY
    billing: pay_per_use
    gen_ai_provider: anthropic
    target_defaults: { dialect: adaptive_effort }
  minimax-plan:                         # a subscription plan: no prices, a quota per window
    protocol: anthropic
    base_url: https://api.minimax.io/anthropic
    api_key_env: MINIMAX_PLAN_API_KEY
    billing: subscription
    quota:
      - { window: 5h, requests: 1500 }
      - { window: 7d, requests: 15000 }
    target_defaults: { dialect: adaptive_effort }
  tei-embed:                            # self-hosted; one instance serves one model
    protocol: openai
    base_url: http://localhost:8080/v1
    billing: none
    allow_private_network: true
  tei-rerank:
    protocol: openai
    base_url: http://localhost:8081/v1
    rerank_url: http://localhost:8081/rerank
    billing: none
    allow_private_network: true

targets:
  minimax-plan/MiniMax-M3.1-Flash:
    kind: chat
    capabilities: { tools: true, structured_output: true }
    max_output_tokens: 64000
    reasoning:
      dialect: adaptive_effort
      levels: [low, medium, high, xhigh, max]
      can_disable: false                # it always reasons: an alias that uses it must set an effort (RSN-7)
  openrouter/qwen/qwen-3.6-flash:
    kind: chat
    capabilities: { tools: true, structured_output: true, strict_schema: true }
    max_output_tokens: 32000
    reasoning:
      dialect: reasoning_object
      levels: [minimal, low, medium, high]
      can_disable: true                 # `none` is sent in the default "off" form of the dialect
    pricing: { input_per_million: 0.15, output_per_million: 0.60, cache_read_per_million: 0.05, as_of: 2026-11-01 }
  anthropic/claude-sonnet-5-5:
    kind: chat
    capabilities: { tools: true, structured_output: true, strict_schema: true, vision: true }
    max_output_tokens: 64000
    reasoning:
      dialect: adaptive_effort
      levels: [low, medium, high, xhigh, max]
      can_disable: false                # set to true, with an `off` fragment if needed, where the model allows it
    pricing: { input_per_million: 3.00, output_per_million: 15.00, cache_read_per_million: 0.30, cache_write_per_million: 3.75, as_of: 2026-11-01 }
  tei-embed/bge-m3:
    kind: embeddings
  tei-rerank/bge-reranker-v2-m3:
    kind: rerank

models:
  fast:
    targets: [minimax-plan/MiniMax-M3.1-Flash, openrouter/qwen/qwen-3.6-flash]
    requires: [tools, structured_output]
    reasoning: { effort: medium, mode: default }    # the client may override it
    max_output_tokens: 32000
    params: { enable_search: drop }                 # an explicit rule for a non-standard parameter (PAR-2)
  deep:
    targets: [anthropic/claude-sonnet-5-5]
    requires: [tools, structured_output, strict_schema]
    reasoning: { effort: high, mode: forced }       # the alias always wins
    max_output_tokens: 32000
    budget: { limit_usd: 500, reset_interval: monthly }
  embed:
    targets: [tei-embed/bge-m3]
    trace_sample_rate: 0                            # no spans; usage is still recorded (TEL-7)
  rerank:
    targets: [tei-rerank/bge-reranker-v2-m3]
    trace_sample_rate: 0

keys:
  invoice-agent:
    models: [fast, deep]
    budget: { limit_usd: 100, reset_interval: monthly }
    rpm_limit: 60
    max_parallel_requests: 5
    quota_share: { minimax-plan: 0.4 }              # at most 40 % of each window of the plan (LIM-10)
  classifier-flow:
    models: [fast, embed, rerank]
    budget: { limit_usd: 30, reset_interval: monthly }   # blocks only the pay-per-use target of `fast` (LIM-3)
    max_parallel_requests: 10
    trace_sample_rate: 0.1
```

## Annex C. Proposed package layout

```text
stargaite/
├── cmd/
│   └── stargaite/
│       └── main.go          # single entry point: `serve` and the CLI
├── internal/
│   ├── domain/              # Provider, Target, Model, Key, Usage; the catalog of error codes
│   ├── store/               # SQLite (modernc), embedded migrations, single writer, backup, retention
│   ├── config/              # validation, snapshot (atomic.Pointer), revisions, export and apply
│   ├── presets/             # embedded data: the provider catalog
│   ├── auth/                # key parsing, constant-time comparison, alias allow-list
│   ├── params/              # known-parameter schemas per endpoint, the body scanner, rules, list of changes
│   ├── reasoning/           # dialects, translation table, effort and output-cap layers
│   ├── plan/                # builds the outgoing request; shared by the data plane and `explain`
│   ├── adapters/
│   │   ├── openai/          # chat completions, embeddings, SSE, usage extraction
│   │   └── anthropic/       # native messages; translation from chat completions
│   ├── routing/             # target selection, circuit breaker
│   ├── admission/           # parallel slots, rate limits, budgets, quota windows
│   ├── transport/           # HTTP client: pooling, TLS, SSRF-safe dialer; SSE reader and writer
│   ├── usage/               # cost, batch writer, aggregates, queries
│   ├── telemetry/           # span schema, OTLP/HTTP JSON exporter, queue
│   ├── api/                 # data plane handlers
│   ├── admin/               # admin API and audit
│   └── cli/                 # the CLI, a client of the admin API
├── tests/
│   ├── mockprovider/        # local provider for CI: both protocols, scripted failures
│   ├── contract/            # per-target suite
│   ├── adversarial/
│   └── benchmark/
├── Dockerfile               # multi-stage build to a minimal non-root image
├── Makefile                 # `make ci` runs every check
├── config.example.yaml
└── go.mod
```

## Annex D. Span schema

Proposed schema, version 1, the same for every backend. The request span is named `{operation} {alias}`, for example
`chat fast`; when the body could not be parsed it is named `{operation}` and carries no alias. Resource attributes:
`service.name = stargaite`, `service.version`.

The `gen_ai.*` names are those of the OpenTelemetry GenAI semantic conventions at the version or commit recorded in
the schema file. They are still in development upstream (R5), and at the time of writing two names need care:
`gen_ai.request.reasoning.level` exists only in the unreleased conventions, and the cache-write attribute is being
renamed from `cache_creation` to `cache_write`. The golden test fixes whichever the pinned version defines.

| Attribute | Value | Present |
|---|---|---|
| `stargaite.schema.version` | `1` | Always |
| `stargaite.version`, `stargaite.config.revision` | Proxy version; configuration revision | Always |
| `stargaite.key` | Key name | Always |
| `stargaite.model` | Model alias (TEL-9) | When the body named one |
| `stargaite.target` | `provider/upstream-model` that served, or the last one tried | When a target was selected |
| `stargaite.endpoint` | `chat`, `messages`, `embeddings` or `rerank` | Always |
| `session.id`, `gen_ai.conversation.id` | From `x-session-id` | When sent |
| `gen_ai.operation.name` | `chat`, `embeddings`, or the custom value `rerank` | Always |
| `gen_ai.provider.name` | The provider's `gen_ai_provider` when set, otherwise its name | When a target was selected |
| `gen_ai.request.model` | The alias the client asked for | When the body named one |
| `gen_ai.response.model` | The model the provider returned | When returned |
| `stargaite.upstream.model` | The model id sent to the provider | When a provider was called |
| `stargaite.model_mismatch` | `true` when the returned model is not the expected one | When a provider answered |
| `gen_ai.usage.input_tokens` | Total input tokens, cache tokens included | When reported or estimated |
| `gen_ai.usage.cache_read.input_tokens`, `gen_ai.usage.cache_creation.input_tokens` | The cached parts of the input | When reported |
| `gen_ai.usage.output_tokens` | Total output tokens, reasoning included | When reported or estimated |
| `gen_ai.usage.reasoning.output_tokens` | The reasoning part of the output | When reported |
| `stargaite.usage.estimated` | `true` when tokens were estimated | When it happened |
| `stargaite.cost.usd` | Cost in USD | Only when known (USE-6) |
| `gen_ai.usage.cost` | The same value. Compatibility attribute: Langfuse reads the cost under this name | Only when known |
| `stargaite.cost.source`, `stargaite.billing` | Source of the cost; billing mode | When a provider was called |
| `gen_ai.response.finish_reasons` | The provider's finish reason | When returned |
| `stargaite.response.empty` | `true` when there is no text and no tool call | When a provider answered |
| `stargaite.reasoning.effort.requested` | Effort asked by the client | When sent by the client |
| `stargaite.reasoning.effort.alias` | Effort set by the alias | When set |
| `stargaite.reasoning.effort.sent` | Effort read from the outgoing body, in the terms of RSN-6 | When a provider was called |
| `gen_ai.request.reasoning.level` | The same value as the effort sent | When an effort was sent |
| `stargaite.reasoning.effort.source` | `client`, `alias_default`, `alias_forced` or `provider_default` | When a provider was called |
| `stargaite.reasoning.effort.adjusted` | `true` when translation changed the level | When a provider was called |
| `stargaite.reasoning.dialect` | Dialect of the target | When a provider was called |
| `stargaite.reasoning.stripped` | `true` when inline think tags were removed | When it happened |
| `stargaite.max_output_tokens.requested`, `.alias`, `.sent` | The three layers of the output cap | When present |
| `gen_ai.request.max_tokens` | The same value as the cap sent | When a cap was sent |
| `stargaite.params.changed` | List of `name:action` | When not empty |
| `stargaite.attempts` | Number of provider calls; `0` when the request was rejected before any | Always |
| `gen_ai.request.stream`, `gen_ai.response.time_to_first_chunk` | Streaming flag; seconds to the first chunk | Always; when streaming |
| `error.type`, `http.response.status_code` | Error code of §15; status returned to the client | On errors; always |
| `server.address` | Host of the provider | When a provider was called |
| `gen_ai.input.messages`, `gen_ai.output.messages`, `stargaite.content.truncated` | Content; whether it was cut | Only with the opt-in of TEL-8 |

A failed attempt that precedes a fallback is a child span named `attempt {target}` with `stargaite.schema.version`,
`stargaite.version`, `stargaite.config.revision`, `stargaite.key`, `stargaite.model`, `stargaite.target`, `stargaite.upstream.model`, the effort and cap sent, `error.type` and the
provider's status. It carries usage and cost only when that attempt was billed, so no backend counts tokens twice
(TEL-10).
