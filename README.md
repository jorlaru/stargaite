# stargaite

An inference proxy for LLM providers: one OpenAI-compatible endpoint in front of many providers, with model aliases,
client keys, budgets and limits, all held in an embedded store and changed through an admin API. One static binary,
one data directory. MIT licensed.

**Status: phase 2 complete.** What works today: the store, the admin API and the CLI, providers (with eighteen
built-in presets), targets, model aliases, client keys with rotation and revocation, `export`/`apply`/`validate`,
`models explain`, `GET /v1/models`, and `POST /v1/chat/completions`, synchronous and streamed, toward providers of
the `openai` protocol and of the `anthropic` protocol (translated to the Messages API), and `POST /v1/messages`,
synchronous and streamed, toward providers of the `anthropic` protocol, `POST /v1/embeddings` toward embeddings
targets of the `openai` protocol (see [Embeddings](#embeddings)) and `POST /v1/rerank` toward a provider's
`rerank_url` (see [Rerank](#rerank)), with the parameter contract and reasoning
control of the alias, timeouts, fallback across an alias's targets, a circuit breaker per target, cancellation when a
client goes away and a graceful stop (see [Resilience](#resilience)). Phase 3 has begun: every provider call leaves a
usage record with its tokens and cost, and `usage show` sums them (see [Usage](#usage)); keys and aliases have rate
and parallel limits (see [Limits](#limits)), USD budgets (see [Budgets](#budgets)) and subscription quotas (see
[Quotas](#quotas)); every request a key sends leaves spans in the configured OTLP backend (see [Telemetry](#telemetry)). The full specification is [kick-off.md](kick-off.md); the designs are
[docs/design/phase-1.md](docs/design/phase-1.md), [docs/design/phase-2.md](docs/design/phase-2.md) and
[docs/design/phase-3.md](docs/design/phase-3.md).

## Quick start

From a clone of this repository, with Go 1.27.4 or later. The mock provider in `tests/mockprovider` stands in for a real
one, so no account or credential is needed and no file is edited by hand. `tests/e2e` runs this block line by line.

```sh
go build -o stargaite ./cmd/stargaite
go run ./tests/mockprovider/cmd/mockprovider &
./stargaite serve &
curl -sS --retry 10 --retry-connrefused --retry-delay 1 http://127.0.0.1:4000/health/ready
./stargaite providers add mock --protocol openai --base-url http://127.0.0.1:4010 --allow-private-network --billing pay_per_use
./stargaite models add fast --target mock/mock
./stargaite targets set mock/mock --pricing input_per_million=1,output_per_million=2,as_of=2026-11-01
./stargaite targets add mock/mock-embed --kind embeddings --pricing input_per_million=1,output_per_million=0,as_of=2026-11-01
./stargaite models add embed --target mock/mock-embed
./stargaite keys create my-agent --model fast --model embed --rpm-limit 60
./stargaite models explain fast
./stargaite targets verify mock/mock
./stargaite telemetry set --otlp-endpoint http://127.0.0.1:4010 --allow-private-network
./stargaite telemetry test
curl -s http://127.0.0.1:4000/v1/models -H "Authorization: Bearer sk-stargaite-..."
curl -s http://127.0.0.1:4000/v1/chat/completions -H "Authorization: Bearer sk-stargaite-..." -d '{"model":"fast","messages":[{"role":"user","content":"hi"}]}'
curl -s http://127.0.0.1:4000/v1/embeddings -H "Authorization: Bearer sk-stargaite-..." -d '{"model":"embed","input":"hi"}'
curl -s http://127.0.0.1:4001/admin/v1/metrics -H "Authorization: Bearer $(cat data/admin.key)"
./stargaite usage show --since 1h --by model
./stargaite backup backup.db
```

The mock provider listens on `127.0.0.1:4010`; `serve` puts the data plane on `127.0.0.1:4000`, the admin API on
`127.0.0.1:4001` and the store in `./data`, and the health check waits until it is up. The mock provider is billed per
use: `models add` also creates the target `mock/mock` (with a warning that it has no prices yet), and `targets set`
gives it a price in USD per million input and output tokens. `targets add` declares a second model of the mock
provider, `mock/mock-embed`, as an embeddings target priced per million input tokens, and `models add embed` makes it
an alias. `keys create` limits the key to 60 requests a minute, allows it both aliases and
prints the client key once: paste it in place of `sk-stargaite-...` in the three `curl` calls. `models explain` shows what the alias would send, without calling
the provider: per target, the effort and output-cap layers, the list of changes, the URL, the headers without the
credential and the body, which for the minimal request it explains by default are the bytes the chat completion below
sends (`--body <file>` explains a request of your own; `--json` gives each body's exact bytes). `targets verify` then
calls the target for real, once per reasoning level it declares (once with no reasoning control for `mock/mock`, which
declares none), prints for each call the effort sent, the provider's status, the reasoning and output tokens and the
exact body sent, then `verified: mock/mock, the result is stored`; until a chat target is verified, `validate` warns
about it (see [Targets verify](#targets-verify)). `telemetry set` points
the OTLP exporter at the mock provider, which also accepts trace exports, and `telemetry test` sends it one span and
prints `credentials: resolved · reachable: yes · authorized: yes · span: accepted (200) · trace_id=…` (see
[Telemetry](#telemetry)). The first `curl` lists
the aliases `embed` and `fast`; the second sends a chat completion to `fast` and gets the mock provider's answer, with
the response headers `x-stargaite-request-id`, `x-stargaite-trace-id` and `x-stargaite-target` (add `-i` to `curl` to
see them), and its span reaches the mock provider within the 2 s of a batch; the third sends an embeddings request to
`embed` and gets the mock provider's vector, byte for byte. The fourth `curl` reads the metrics from the admin API
with the admin key, in the Prometheus text format: among them `stargaite_requests_total{key="my-agent",model="fast",status="200"} 1`
(see [Metrics](#metrics)).
`usage show --by model` then sums the last hour from the store per alias: for `fast` one request, the tokens the mock
provider reported (1 input, 2 output) and their cost at the target's prices, 0.000005 USD; for `embed` one request,
1 input token, 0 output and 0.000001 USD; and on the first row, under the alias `-` (none), the verify call, recorded
under the key `_verify` with the same tokens and cost as `fast`. `backup` writes a checked copy of the store, the three
records included, to `backup.db` with mode `0600` and prints its size (see [Backup and restore](#backup-and-restore)).
Both processes log JSON lines to the terminal; `kill %1 %2` stops them.
`./stargaite --help` lists every command, and each one accepts `--json`, never prompts, and exits `0` on success, `1`
on failure and `2` on wrong usage; `remove`, `keys revoke` and `apply --prune` (unless `--dry-run`) need `--yes`.

## Usage

Every provider call leaves one usage record in the store, whether it succeeded, failed or was cut short, and a request
that fell back leaves one per attempt: the key, the alias, the target, the model the provider answered with (flagged
when it is not the one sent), the finish reason, the tokens (input with its cache parts, output with its reasoning
part), the requested, alias and sent effort and output cap, the parameter changes, the configuration revision and the
version. Tokens are the provider's own figures; when a `2xx` answer brought none, or was cut, they are estimated from
the bytes (a quarter of them, rounded up) and marked `estimated`. Toward a provider whose `stream_usage` is `request`
(OpenAI's preset among others), a streamed chat asks for the usage chunk with `stream_options.include_usage`, listed in
`x-stargaite-params-changed`, and relays that chunk only to a client that asked for it. A provider is billed
`pay_per_use`, `subscription` or `none`; only `pay_per_use` calls have a cost, read from the response at the provider's
`cost_report` path when it declares one (OpenRouter and xAI do), else computed from the target's `--pricing`, else left
empty and counted as unpriced: it is never guessed. Records reach the store in batches within 50 ms; on `SIGTERM` the pending ones are
written before the store closes, and a record that cannot be stored is written to the log as one `CRITICAL` line
instead of being lost. A crash loses at most the records not yet committed. When the store fails three commits in a
row it is unavailable: `pay_per_use` targets are passed over (`503 usage_store_unavailable` when the alias has no
other target), `subscription` and `none` targets keep serving, and records wait in a queue of 10,000 while the
writer retries every 5 s; a record that finds the queue full goes to the log as one `CRITICAL` line.

`./stargaite usage show` sums the records of a window (`--since 24h` by default, or an RFC 3339 time) per billing mode,
filtered with `--key`, `--model` and `--target` and grouped with `--by key,model,target,effort-sent`: requests,
errors, each kind of token, estimated calls, the cost of `pay_per_use` calls and the unpriced ones, answers cut at a
limit (`finish_reason` `length` or the like), empty answers, and the median and 90th percentile of output and
reasoning tokens over the successful calls whose provider reported them. `--compare-previous` adds the window of the
same length before it; `--by effort-sent` with it shows whether reasoning tokens move with the effort sent. It reads
the records within `usage_days` (90 by default; `serve` deletes older ones once a day, keeping those the longest quota
window reads): a window that starts earlier is answered from the daily sums, over whole UTC days and without the
counts and percentiles they do not hold (shown as `-`), and cannot be grouped by effort sent.
`--json` gives the same as JSON, with each window's `source` (`records` or `daily`), `since` and `until`.

## Limits

A key and an alias can each limit requests per minute (`--rpm-limit`), tokens per minute (`--tpm-limit`: the input
plus output tokens of the provider calls that ended in the last minute, as recorded) and requests in flight
(`--max-parallel-requests`), on `keys create|set` and `models add|set`; `settings set --max-parallel-requests` limits
the requests in flight of the whole instance. Each limit acts on its own. A key that declares no parallel limit gets
5 when the configuration is published: `keys show` shows the 5 and `export` keeps what was declared, so a key can be
given a large limit but not none. Nothing else is limited by default.

A request beyond any limit gets `429` before any provider is called: `parallel_limit_exceeded` with `Retry-After: 1`,
or `rate_limit_exceeded` with `Retry-After` set to the seconds until the minute admits it again. Such a request counts
in no limit and leaves no usage record; one that passed the limits and failed later counts. A request holds its slots
until it ends, a stream until its last event, and gives them back on an error, a client gone or a stop alike. Minutes
are counted in steps of one second, so a window may hold up to a second more than 60 s of traffic, never less, and
counts start empty after a restart.

## Budgets

A key, an alias and the instance can each have a USD budget: `--budget limit_usd=5,reset_interval=daily` on
`keys create|set`, `models add|set` and `settings set` (`daily` and `monthly` start again at 00:00 UTC, on day 1 for
`monthly`; `never` keeps a running total). Budgets count only `pay_per_use` calls with a cost. Once a budget's spend
reaches its limit, the `pay_per_use` targets of the requests in its scope are passed over: the alias's next target of
another billing serves, and with none left the request gets `402 budget_exceeded`, naming only the kind of budget,
before any provider is called. A budget never blocks a `subscription` or `none` target. Spend is charged when a call
ends, so the calls in flight when a limit is crossed may pass it by their cost: at most the cost of the calls in flight
in the budget's scope; for a key, its parallel slots (5 by default) times one capped answer; an alias or the instance
is bounded only when it sets `max_parallel_requests` and its targets cap output. Reaching 80 % and 100 % of a budget writes one `WARN` log line, one audit row and one counter
each per period. Spend survives a restart, as it is rebuilt from the stored records; a `never` budget's total starts
at zero when it is set or its interval changes, and is kept when only its limit changes.

## Quotas

A `subscription` provider (a flat-fee plan) declares the windows of its plan, each limited in requests, tokens or both:
`providers add|set <name> --billing subscription --quota 5h:requests=1000 --quota 7d:tokens=5000000`. A key may use a
fraction of each window of a plan: `keys create|set <key> --quota-share <provider>=0.4` (rounded down: 0.4 of 1001
requests is 400). Each provider call through the plan counts one request when its target is chosen, whatever its
result, and its input plus output tokens when it ends. Once a window is used up, the plan's targets are passed over:
the alias's next target serves, and with none left the request gets `429 quota_exceeded` with `Retry-After` set to the
seconds until the window admits again, rounded up and uncapped, before any provider is called; a key at its share is
refused the same way while other keys are still served. A quota never touches a USD budget. Reaching 80 % of a window
writes one `WARN` log line, one audit row and one counter, again only after the window has dropped below 80 %.

Windows slide: a `5h` window counts the last five hours, in steps of a six-hundredth of the window (30 s for `5h`, about
17 minutes for `7d`), so it may count up to one step more of the past, never less. Windows are rebuilt from the stored
records when `serve` starts, before it accepts requests, and a window or share added while running is rebuilt at its
first use, so the use made before counts, calls still running included. `./stargaite usage quota` shows each window of
each plan and of each key's share: used, limit, `used_pct` and, once used up, `retry_after_s`. It counts this proxy's
traffic only: a plan also used by other clients, or whose provider resets on a calendar or from the first use, makes the
provider's own `429` authoritative, and that `429` moves the request to the next target like any other. `validate` warns
when a window is longer than `retention.usage_days`, whose records the rebuild reads.

## Telemetry

stargaite exports spans over OTLP/HTTP with the JSON encoding to any backend that accepts it; there is no vendor SDK
and no second export path. `./stargaite telemetry set --otlp-endpoint <url>` sets the endpoint, always in full: spans go
to `<url>/v1/traces`. Credentials are references, never values: `--basic-auth-env USER_VAR:PASSWORD_VAR` composes the
`Authorization: Basic` header from two variables and `--header-env NAME=VAR` sends a header read from a variable, both
resolved by `serve` like provider credentials (see [Provider credentials](#provider-credentials)); `--header NAME=value`
sends a static header that carries no secret, and `Authorization`, `Proxy-Authorization`, `Cookie` and `x-api-key` are
refused there. A plain HTTP, loopback or private endpoint needs `--allow-private-network`; the address is checked again
at every connection and redirects are never followed. `--preset langfuse` fills in
`--basic-auth-env LANGFUSE_PUBLIC_KEY:LANGFUSE_SECRET_KEY` and the header `x-langfuse-ingestion-version: 4`, which
explicit flags override; give the endpoint as `https://<host>/api/public/otel`. Langfuse shows the cost stargaite
sends; for a call without one (`subscription`, `none` or unpriced) it shows a cost of its own, from its price table and
the returned model, which no record has.

`./stargaite telemetry test` sends one span, `stargaite telemetry test` under the reserved key `_telemetry`, and names
the first thing that failed: `credential_unresolved` (a referenced variable is not visible to `serve`; nothing is
sent), `unreachable`, `unauthorized` (401 or 403), `not_found` (404: the path is wrong) or `rejected` (any other status,
or a span refused inside a `2xx`, with the backend's message), else `accepted` with the trace id to look up; it exits
`1` unless the span was accepted. `./stargaite telemetry status` shows the effective block, the references that do not
resolve, and the spans sent, rejected by HTTP status and by the backend, dropped and queued, with the last error.
`./stargaite telemetry unset --yes` removes the exporter. Export never slows inference: spans wait in a queue of 10,000
spans or 32 MB, whichever comes first (the oldest are dropped and counted), and go in batches every 2 s or of 200;
`429`, `502`, `503`, `504` and connection failures are retried three times, honouring a `Retry-After` up to 30 s, and
any other status rejects the batch. Ten failed batches in a row write one `WARN` line, at most once a minute. On
`SIGTERM` queued spans get 5 s to leave after the store closes; any left are dropped and logged, without changing the
exit code. No usage record, budget or quota ever reads telemetry.

Every request to `/v1/chat/completions` or `/v1/messages` with a valid key leaves one request span (kind `SERVER`,
named `chat <alias>`), refusals included, and each provider call that failed before a fallback leaves a child span
`attempt <target>` (kind `CLIENT`) with its error; a failed authentication and `GET /v1/models` leave none. The
attributes are schema v1 of [kick-off.md](kick-off.md) annex D: the OpenTelemetry GenAI names of
`semantic-conventions-genai` at commit `e6658f14c9d9ea0760825f029cb82e7673f48fab`, the `stargaite.*` ones (key, alias,
target, efforts and caps requested, set by the alias and sent, the change list, billing, cost when known, version and
configuration revision), and `langfuse.observation.metadata.stargaite_key`, `_model` and `_target`, under which
Langfuse filters. Tokens and cost sit on one span per provider call. A request with a valid W3C `traceparent` joins the
client's trace (`telemetry set --trust-traceparent=false` turns that off); `x-session-id`, 1 to 128 printable ASCII
bytes, sets `session.id`. `--trace-sample-rate` on a key or an alias (0 to 1; the lower one applies) keeps that share of
requests, decided once per request; it never changes records, budgets, quotas or counters. Unless a key opts in to
content (below), spans carry no prompt or answer, and never a header value other than `x-session-id`.

Content is an opt-in per key, with an expiry: `./stargaite keys set <key> --content 24h` (a positive duration from now,
also on `keys create`) sets the key's `content_until`, which `keys show` displays and the audit records like any key
change; `--content=` stops it at once. Until then the request spans of that key's sampled requests carry
`gen_ai.input.messages`, the `messages` member of the body sent to the provider that answered, and
`gen_ai.output.messages`, the answer relayed to the client (a stream's event data joined by newlines), each a string
cut at a UTF-8 boundary to `content_max_bytes` (32768 by default; `telemetry set --content-max-bytes <n>`) and flagged
by `stargaite.content.truncated` when cut, with the provider credential redacted. A provider's own error text, which
may quote the prompt, never enters a span status, opt-in or not, and nothing of the content reaches a log or a usage
record.

## Metrics

`GET /admin/v1/metrics` on the admin listener answers the Prometheus text format, always as
`Content-Type: text/plain; version=0.0.4; charset=utf-8` whatever `Accept` asks, which any Prometheus scrapes. It
takes the admin key, or a read-only token that opens this route and no other: set `STARGAITE_METRICS_TOKEN` in the
environment of `serve`, which reads it once at start and keeps only its SHA-256. The token follows the admin key's
rule: at least 32 characters, no leading or trailing whitespace, not starting with `sk-stargaite-`, and not the admin
key itself; otherwise `serve` refuses to start (exit `1`, a log line naming `SEC-3`, never the token). A scrape is a
read: it writes no audit record.

| Family | Type | Labels |
|---|---|---|
| `stargaite_requests_total` | counter | `key`, `model`, `status` |
| `stargaite_tokens_total` | counter | `key`, `model`, `type` (`input`, `cache_read`, `cache_write`, `output`, `reasoning`) |
| `stargaite_cost_usd_total` | counter | `key`, `model` |
| `stargaite_unpriced_requests_total`, `stargaite_length_finishes_total`, `stargaite_empty_responses_total`, `stargaite_model_mismatches_total` | counter | `key`, `model` |
| `stargaite_params_changed_total` | counter | `key`, `model`, `action` |
| `stargaite_budget_warnings_total`, `stargaite_budget_exhausted_total`, `stargaite_quota_warnings_total` | counter | `key`, `model` |
| `stargaite_spans_emitted_total` | counter | `key`, `model` |
| `stargaite_unauthenticated_requests_total`, `stargaite_store_check_failures_total` | counter | none |
| `stargaite_spans_sent_total`, `stargaite_spans_dropped_total` | counter | none |
| `stargaite_spans_rejected_total` | counter | `reason` (`status`, `backend`), `status` |
| `stargaite_spans_queued` | gauge | none |
| `stargaite_exporter_last_error_info` | gauge, value 1 | `message`: the exporter's last error as `telemetry status` shows it, redacted |
| `stargaite_budget_spent_usd`, `stargaite_budget_limit_usd` | gauge | `scope` (`global`, `key`, `model`), `name`, `interval` |
| `stargaite_quota_used`, `stargaite_quota_limit` | gauge | `provider`, `key` (empty for the provider's own window), `window`, `unit` |

`model` is the alias a request named; a request whose alias does not exist is counted under `model=""`, and the calls
of `targets verify` under `key="_verify"`, `model=""`. Request, token, cost and span counts are those of the usage
records and spans since the start: counters live in memory and start again from zero on a restart, which Prometheus
reads as a counter reset. The budget gauges are each declared budget's spend in its current period; the quota gauges
are the windows held since the start or the last use, read as they are, so a scrape never seeds a window from the
store. A series exists only for a combination seen: keys and aliases are names the configuration declares, and
statuses, token types, actions and reasons are closed sets, so the number of series is bounded by the declared keys
times the aliases each may use times the statuses they got. At the scale of 10,000 keys and 1,000 aliases that bound
can pass what one Prometheus should hold; drop or aggregate labels there with `metric_relabel_configs`. A Prometheus
scrape job, with the token in a file only Prometheus reads:

```yaml
scrape_configs:
  - job_name: stargaite
    metrics_path: /admin/v1/metrics
    authorization:
      credentials_file: /etc/prometheus/stargaite-metrics-token
    static_configs:
      - targets: ["127.0.0.1:4001"]
```

## Anthropic clients

`POST /v1/messages` is the Anthropic Messages API, served by targets of the `anthropic` protocol: an Anthropic SDK
works with its base URL set to the data plane (`http://127.0.0.1:4000`), a client key as its API key and an alias as
the model. The key is read from `x-api-key` or `Authorization: Bearer`; with both, `Authorization` wins. Of the
client's headers only `anthropic-version` (else `2023-06-01`) and `anthropic-beta` reach the provider. The answer and
the stream are relayed as the provider sent them, thinking blocks and their signatures included, so a tool loop with
thinking works; a target that declares `inline_think_tags` has its `<think>` blocks removed from text blocks. Errors
use the Anthropic envelope, `{"type":"error","error":{"type":...,"message":"<code>: ..."}}`, with the code also in
the `x-stargaite-error-code` header. A target of the `openai` protocol is skipped, so an alias with no other answers
`422 endpoint_not_supported`. `./stargaite models explain <alias> --endpoint messages` shows the bytes it would send.

## Embeddings

`POST /v1/embeddings` is the OpenAI Embeddings API, served by targets of kind `embeddings` on providers of the `openai`
protocol, at the provider's `base_url` plus `/embeddings`: an OpenAI SDK works with its base URL set to
`http://127.0.0.1:4000/v1`, a client key and an alias as the model. It has every protection of chat: the key and the
alias, `max_body_bytes`, the limits, budgets and quotas, fallback and the circuit, records and spans. The known body
members are `input`, `model`, `dimensions`, `encoding_format` and `user`; any other, `stream` included, is refused with
`400 unknown_parameter` unless the alias has a rule for it (`models set <alias> --param input_type=forward`, for a
provider field such as `input_type` or `output_dimension`). Only `model` is replaced by the upstream id and the
alias's rules applied; no output cap or effort is ever sent, whatever the alias declares. The answer is relayed as the
provider sent it, `float` or `base64` vectors alike. A record's input tokens are the provider's `usage.prompt_tokens`,
else its `usage.total_tokens`, and its output tokens 0; an answer without `usage` is estimated from the bytes sent, so
a priced embeddings target (`output_per_million=0`) always moves its budgets. A chat, anthropic or rerank target
behind an embeddings alias is skipped, and an alias with no other answers `422 endpoint_not_supported`; `validate`
warns about an alias none of whose targets can serve any endpoint.

The OpenAI SDKs ask for `encoding_format: base64` when the caller sets none. A provider that refuses it is declared
with `targets set <target> --unsupported-param encoding_format` and its aliases with
`--param encoding_format=drop`, so the member is removed toward it and listed in `x-stargaite-params-changed`. Large
batches can pass `max_response_bytes` (32 MiB by default): 2,048 inputs at 3,072 dimensions return 32 MiB of base64
vectors alone, and several times more as floats, which answers `502 upstream_error` (the attempt's log line names the
limit); raise it with `settings set --max-response-bytes <n>`, or send smaller batches.

## Rerank

`POST /v1/rerank` forwards the body to the `rerank_url` of a rerank target's provider (`providers add <name>
--protocol openai --rerank-url <url>`, then `targets add <provider>/<model> --kind rerank`), with only `model` replaced
by the upstream id: the proxy defines no rerank schema, so Cohere, Jina, Voyage, vLLM or TEI shapes pass unchanged,
no alias rule, output cap or effort is applied, and the answer is relayed as the provider sent it. Of the body checks
only these apply: well-formed UTF-8 JSON, no repeated top-level key, and `model` naming an alias the key may use;
every other protection of chat applies as well. A target whose provider has no `rerank_url`, or of the `anthropic`
protocol, is skipped (`422 endpoint_not_supported` when no other is left); `validate` warns about an alias none of
whose targets can serve any endpoint. A record's
input tokens are `usage.prompt_tokens`, else `usage.total_tokens`, and its output 0; an answer without them (a bare
array, as TEI sends, or Cohere's `meta`) is estimated from the bytes sent and priced by the target's table, and a
provider `cost_report` wins over the table. Search units are not recorded: a target billed by them is priced by its
`cost_report`, or approximately by its token prices.

## Targets verify

`./stargaite targets verify <target> [--max-output-tokens n]` checks a chat target against its live provider, with
the provider's own credential: one call per level the target declares (`--level`, in order), then one at `none` when
it declares `--can-disable=true`, or a single call with no reasoning control when it declares neither; an `on` level
is sent as `medium`. Each call sends the fixed prompt "What is 17 × 23? Answer with the number only." through the same
build as live traffic, so the body printed is the exact one sent, translated toward an `anthropic` target. The only
output cap sent is `--max-output-tokens`, written under the target's `output_cap_param` (`max_tokens` toward
`anthropic`) and listed as added; it is required for an `anthropic` target and may not exceed the target's
`max_output_tokens`. For each call it prints the level asked, the effort sent, the provider's status, the reasoning
and output tokens and the body, then `verified` or `failed`; it exits `0` only when every call answered `2xx`, and
`--json` gives every body's exact bytes. The calls cost tokens: they are recorded under the key `_verify` with an
empty alias, count toward the global budget and the provider's quota (and are refused, `402` or `429`, when either is
exhausted), and pass no key or alias limit; they neither open nor obey a target's circuit. A successful run is stored
with its date and the proxy version, which `targets show` and `targets list` print as `verified_at` (Unix
milliseconds) and `verified_version`, and `validate` warns about every chat target without one (`validate -f`, with no
store, does not); any change of the target or of its provider clears it. Every run is audited, `verified`, `failed`,
or `stale` when the target changed while it ran, and none writes a revision. The server bounds the run by its own
write deadline, the number of calls times `total_ms` plus 5 s, so the CLI waits without its 90 s timeout.

## Resilience

An alias lists up to three targets. A connection failure, a timeout, a `429` or a `5xx` from the provider moves the
request to the next target, as long as nothing was sent to the client yet; the `x-stargaite-*` headers then describe
the target that answered. Five such failures of a target within 30 s open its circuit for 20 s, during which it is
passed over. A stream that fails after its first event ends with one error event in the format of its endpoint. When
a client goes away, the provider call is cancelled at once. On `SIGTERM` (or `Ctrl-C`) `serve` stops accepting
connections and gives the requests in flight up to 30 s to finish; any still running then gets `500 internal_error`
with the message `server stopping`, as an error answer or as the stream's error event, and `serve` writes the pending
usage records, then closes the store, gives queued spans up to 5 s to leave and exits `0`. A supervisor must allow it
more than 55 s to stop (the 30 s, up to 5 s for the requests stopped to end, up to 10 s for the records plus the store's
5 s busy timeout for the write in progress, and the 5 s of the spans) before it kills it: `docker stop` waits 10 s
unless given `-t`.

## The admin key and the data directory

`serve` keeps everything in its data directory (`--data-dir`, else `STARGAITE_DATA_DIR`, else `./data`): the store,
its lock and, on the first start, a generated admin key in `admin.key` (mode `0600`). The CLI reads the key from that
file, or from `STARGAITE_ADMIN_KEY` when it is set; a key given to `serve` that way must have at least 32 characters.
The CLI finds the admin API at `--admin-url`, else `STARGAITE_ADMIN_URL`, else `http://127.0.0.1:4001`.

One `serve` per data directory: a second one exits `1`. Keep the directory on a local disk, not a network file system.
`./stargaite export` prints the whole configuration as YAML, and `./stargaite apply -f <file>` applies one;
`config.example.yaml` is an example input (`./stargaite validate -f config.example.yaml` checks it without a server).

## Backup and restore

`./stargaite backup <file>` asks the running `serve` for a copy of the store and writes it to `<file>`. The copy is
taken while `serve` keeps serving (`VACUUM INTO`, on a connection of its own) and holds every committed transaction;
usage records still waiting for their batch (at most 50 ms of traffic) are not in it. Before sending it, `serve`
checks it (`PRAGMA integrity_check` and `PRAGMA foreign_key_check`) and refuses one that fails; the copy never stays
in the data directory. The CLI writes `<file>.part` first, with mode `0600`, since the copy holds the key hashes and
every usage record: keep it as private as the data directory. Once the size matches the announced one and the part is
synced, it is linked to `<file>`; `backup` never replaces a file, refuses a `<file>` that exists, and removes the part
on any failure, `Ctrl-C` and `SIGTERM` included. `serve` gives a backup up to 15 minutes. A backup is a read: one
log line with its size, no audit record. The admin key (`admin.key`) is not in the store, so a restored store keeps
the key of its data directory.

To restore a backup:

1. Stop `serve` (`SIGTERM`, or `Ctrl-C`) and wait for it to exit: it releases the data directory.
2. Move `stargaite.db`, `stargaite.db-wal` and `stargaite.db-shm` out of the data directory, all three together. A
   `-wal` left beside the restored copy is replayed onto it, which brings back rows the backup does not hold or
   corrupts the store.
3. Copy the backup in as `stargaite.db` with mode `0600`, owned by the user that runs `serve`:
   `install -m 600 backup.db data/stargaite.db`.
4. Start `serve`. It turns the copy back to WAL mode and runs any newer migration, and budgets and quota windows are
   rebuilt from it, as at every start. Key tokens, aliases and usage are those of the backup.

## Provider credentials

A provider names the variable that holds its credential (`--api-key-env`; a preset names it for you, see
`./stargaite providers presets`), never the value. `serve` resolves it from its own environment, then from the file
given with `--env-file`: plain `NAME=value` lines, read again on every accepted admin change, so a credential added
there works without a restart. Keep that file readable only by the user that runs `serve`, and keep credentials out
of shell history and of the repository. A provider whose credential does not resolve is disabled, with a warning, and
`./stargaite providers show <name>` says `credential: unresolved`. No response, log or export ever carries the value.

`./stargaite secrets reload` reads the env file again with no other change: every provider credential and the
telemetry exporter's are resolved again and published at the same revision, with one `unchanged` audit record. Its
findings name each variable that still does not resolve, never a value. Requests in flight finish with the
credentials they started with; the exporter uses the new ones from its next batch. No signal triggers a reload. A
variable set in `serve`'s own environment wins over the file and cannot change while it runs, so a credential meant
to rotate without a restart goes in the file. Write the file by rename, so `serve` never reads it half written: write
a temporary file in the same directory with mode `0600`, then move it over the old one:

```sh
cp -p /etc/stargaite/env /etc/stargaite/env.new   # keeps mode 0600; edit env.new with your editor
mv /etc/stargaite/env.new /etc/stargaite/env
./stargaite secrets reload
```

As every command whose answer holds an error finding, it exits `1` when the stored configuration has one (such as a
value an upgraded binary refuses); the reload is still published.

stargaite ignores `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` in v1: `serve` connects to providers directly, so the
check of each outbound address sees the real destination, and the CLI sends the admin key to the admin URL only.

## Listeners, TLS and the container

Both listeners default to loopback. A listener outside loopback needs TLS, either `--tls-cert <file> --tls-key <file>`
(both listeners then serve TLS) or `--behind-tls-proxy`, which states that a trusted front end terminates it;
otherwise `serve` exits `1`. Every `serve` flag can also be given as `STARGAITE_<FLAG>` (`STARGAITE_LISTEN`); the flag
wins.

The image runs plain `serve` on loopback, like the binary, so its listener flags are always explicit. It runs as uid
`65532` with its data directory on the `/data` volume:

```sh
docker build -t stargaite .
docker run -d --name stargaite --read-only -v stargaite-data:/data -p 127.0.0.1:4000:4000 \
  stargaite serve --listen 0.0.0.0:4000 --behind-tls-proxy
curl -sS --retry 10 --retry-all-errors --retry-delay 1 http://127.0.0.1:4000/health/ready
docker exec stargaite /stargaite settings show
docker stop -t 60 stargaite
```

Only the data plane leaves the container. The admin API stays on its loopback, and the CLI inside the container
administers it with the key in `/data/admin.key`. Pass a credential with `-e OPENROUTER_API_KEY` on `docker run`,
which copies the variable from your shell without writing its value, or mount the directory that holds the env file,
readable by uid `65532`, and add `--env-file` after `serve` (`-v /etc/stargaite:/run/stargaite:ro` with
`--env-file /run/stargaite/env`): mount the directory, not the file, because a file replaced by rename on the host is
not seen through a mount of the file itself. To have stargaite terminate TLS itself, mount the certificate and its key and give
`--tls-cert` and `--tls-key` in place of `--behind-tls-proxy`; the admin API then speaks TLS too, so the CLI needs
`--admin-url https://localhost:4001` and must trust the certificate (`docker exec -e SSL_CERT_FILE=<ca file> ...`),
which must then also be valid for `localhost`. If it is not, add `--add-host <cert name>:127.0.0.1` to `docker run`
and use `--admin-url https://<cert name>:4001`; a certificate from a public CA needs no `SSL_CERT_FILE`.

## Development

`make ci` runs every check (format, vet, tidy, lint, vulnerability scan, four cross-builds, `go test -race ./...`,
including `tests/e2e`) with Go 1.27.4 or later; `make ci-container` runs the same inside the Go image, and
`make image-test` builds the image and runs it. No check needs a secret, and the tests use no network beyond loopback.
`tests/contract` runs the per-target cases against a live provider only when `STARGAITE_CONTRACT_BASE_URL` and
`STARGAITE_CONTRACT_API_KEY` are set (its package comment lists the variables); it costs tokens, and without them it
is skipped. Its usage cases (`-run TestUsage`) check the declared `stream_usage` and `cost_report` of the preset under
test, given in `STARGAITE_CONTRACT_PROVIDER`, and the token totals of the records:

```sh
STARGAITE_CONTRACT_BASE_URL=https://api.x.ai/v1 STARGAITE_CONTRACT_API_KEY=... STARGAITE_CONTRACT_MODEL=<model> \
  STARGAITE_CONTRACT_PROTOCOL=openai \
  STARGAITE_CONTRACT_PROVIDER='stream_usage: request, cost_report: { path: usage.cost_in_usd_ticks, usd_per_unit: 1e-10 }' \
  go test -count=1 -v ./tests/contract
```

`tests/langfuse` sends the proxy's spans, with the `langfuse` preset, to a self-hosted Langfuse (v4.80.0, started with
its own `docker compose`; use a disposable project) and reads them back through its public API: each trace readable,
filtered by key, alias and target, the cost of each `pay_per_use` trace equal to its records. It runs only when
`STARGAITE_LANGFUSE_URL` (the base URL, such as `http://localhost:3000`), `LANGFUSE_PUBLIC_KEY` and
`LANGFUSE_SECRET_KEY` are set: `go test -count=1 -timeout 30m -v ./tests/langfuse` (its wait for traces sent without
the ingestion version header can pass go test's default 10 minutes). Neither suite is part of `make ci`;
maintainers run both before a release that changes the span schema or the exporter, and the contract suite before a
preset loses its `untested` mark.

### Hardening

`tests/adversarial`, part of `make ci`, sends hostile input to the data plane and the admin API on every endpoint it
applies to: malformed, invalid UTF-8, deeply nested and repeated-key bodies, oversized bodies, revoked and malformed
keys, unknown aliases, incompatible targets, fragmented and truncated streams, split and unclosed think tags, SSRF
addresses, redirects and DNS rebinding, budget and quota races, oversized and gzip-bomb answers, credentials echoed by
a provider, control bytes in logged values and a flood of bad tokens. `TestServeAdversarial` in `cmd/stargaite` checks
the server options on raw sockets: request smuggling forms, header size and count limits, slow headers, a client that
stops reading and one that disconnects. `make ci` replays every fuzz seed under `testdata/fuzz`; `make fuzz` runs
each fuzz target for `FUZZTIME` (60 s by default, `make fuzz FUZZTIME=5m`), and a finding, written under the
package's `testdata/fuzz`, is committed as a seed with its fix.
