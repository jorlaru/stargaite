# stargaite

An inference proxy for LLM providers: one OpenAI-compatible endpoint in front of many providers, with model aliases,
client keys, budgets and limits, all held in an embedded store and changed through an admin API. One static binary,
one data directory. MIT licensed.

**Status: phase 2 complete.** What works today: the store, the admin API and the CLI, providers (with eighteen
built-in presets), targets, model aliases, client keys with rotation and revocation, `export`/`apply`/`validate`,
`models explain`, `GET /v1/models`, and `POST /v1/chat/completions`, synchronous and streamed, toward providers of
the `openai` protocol and of the `anthropic` protocol (translated to the Messages API), and `POST /v1/messages`,
synchronous and streamed, toward providers of the `anthropic` protocol, with the parameter contract and reasoning
control of the alias, timeouts, fallback across an alias's targets, a circuit breaker per target, cancellation when a
client goes away and a graceful stop (see [Resilience](#resilience)). Phase 3 has begun: every provider call leaves a
usage record with its tokens and cost, and `usage show` sums them (see [Usage](#usage)); keys and aliases have rate
and parallel limits (see [Limits](#limits)) and USD budgets (see [Budgets](#budgets)); quotas and telemetry follow. The full specification is [kick-off.md](kick-off.md); the designs are
[docs/design/phase-1.md](docs/design/phase-1.md), [docs/design/phase-2.md](docs/design/phase-2.md) and
[docs/design/phase-3.md](docs/design/phase-3.md).

## Quick start

From a clone of this repository, with Go 1.27 or later. The mock provider in `tests/mockprovider` stands in for a real
one, so no account or credential is needed and no file is edited by hand. `tests/e2e` runs this block line by line.

```sh
go build -o stargaite ./cmd/stargaite
go run ./tests/mockprovider/cmd/mockprovider &
./stargaite serve &
curl -sS --retry 10 --retry-connrefused --retry-delay 1 http://127.0.0.1:4000/health/ready
./stargaite providers add mock --protocol openai --base-url http://127.0.0.1:4010 --allow-private-network --billing pay_per_use
./stargaite models add fast --target mock/mock
./stargaite targets set mock/mock --pricing input_per_million=1,output_per_million=2,as_of=2026-11-01
./stargaite keys create my-agent --model fast --rpm-limit 60
./stargaite models explain fast
curl -s http://127.0.0.1:4000/v1/models -H "Authorization: Bearer sk-stargaite-..."
curl -s http://127.0.0.1:4000/v1/chat/completions -H "Authorization: Bearer sk-stargaite-..." -d '{"model":"fast","messages":[{"role":"user","content":"hi"}]}'
./stargaite usage show --since 1h
```

The mock provider listens on `127.0.0.1:4010`; `serve` puts the data plane on `127.0.0.1:4000`, the admin API on
`127.0.0.1:4001` and the store in `./data`, and the health check waits until it is up. The mock provider is billed per
use: `models add` also creates the target `mock/mock` (with a warning that it has no prices yet), and `targets set`
gives it a price in USD per million input and output tokens. `keys create` limits the key to 60 requests a minute and
prints the client key once: paste it in place of `sk-stargaite-...` in the two `curl` calls. `models explain` shows what the alias would send, without calling
the provider: per target, the effort and output-cap layers, the list of changes, the URL, the headers without the
credential and the body, which for the minimal request it explains by default are the bytes the chat completion below
sends (`--body <file>` explains a request of your own; `--json` gives each body's exact bytes). The first `curl` lists
the alias `fast`; the second sends a chat completion to it and gets the mock provider's answer, with the response
headers `x-stargaite-request-id`, `x-stargaite-trace-id` and `x-stargaite-target` (add `-i` to `curl` to see them).
`usage show` then sums the last hour from the store: one request, the tokens the mock provider reported (1 input, 2
output) and their cost at the target's prices, 0.000005 USD. Both processes log JSON lines to the terminal;
`kill %1 %2` stops them.
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
the records within `usage_days` (90 by default; deleting older ones comes in a later phase): a window that starts
earlier is answered from the daily sums, over whole UTC days and without the counts and percentiles they do not hold
(shown as `-`), and cannot be grouped by effort sent.
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

## Resilience

An alias lists up to three targets. A connection failure, a timeout, a `429` or a `5xx` from the provider moves the
request to the next target, as long as nothing was sent to the client yet; the `x-stargaite-*` headers then describe
the target that answered. Five such failures of a target within 30 s open its circuit for 20 s, during which it is
passed over. A stream that fails after its first event ends with one error event in the format of its endpoint. When
a client goes away, the provider call is cancelled at once. On `SIGTERM` (or `Ctrl-C`) `serve` stops accepting
connections and gives the requests in flight up to 30 s to finish; any still running then gets `500 internal_error`
with the message `server stopping`, as an error answer or as the stream's error event, and `serve` writes the pending
usage records, then closes the store and exits `0`. A supervisor must allow it more than 50 s to
stop (the 30 s, up to 5 s for the requests stopped to end, and up to 10 s for the records plus the store's 5 s busy
timeout for the write in progress) before it kills it: `docker stop` waits 10 s unless given `-t`.

## The admin key and the data directory

`serve` keeps everything in its data directory (`--data-dir`, else `STARGAITE_DATA_DIR`, else `./data`): the store,
its lock and, on the first start, a generated admin key in `admin.key` (mode `0600`). The CLI reads the key from that
file, or from `STARGAITE_ADMIN_KEY` when it is set; a key given to `serve` that way must have at least 32 characters.
The CLI finds the admin API at `--admin-url`, else `STARGAITE_ADMIN_URL`, else `http://127.0.0.1:4001`.

One `serve` per data directory: a second one exits `1`. Keep the directory on a local disk, not a network file system.
`./stargaite export` prints the whole configuration as YAML, and `./stargaite apply -f <file>` applies one;
`config.example.yaml` is an example input (`./stargaite validate -f config.example.yaml` checks it without a server).

## Provider credentials

A provider names the variable that holds its credential (`--api-key-env`; a preset names it for you, see
`./stargaite providers presets`), never the value. `serve` resolves it from its own environment, then from the file
given with `--env-file`: plain `NAME=value` lines, read again on every accepted admin change, so a credential added
there works without a restart. Keep that file readable only by the user that runs `serve`, and keep credentials out
of shell history and of the repository. A provider whose credential does not resolve is disabled, with a warning, and
`./stargaite providers show <name>` says `credential: unresolved`. No response, log or export ever carries the value.

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
which copies the variable from your shell without writing its value, or mount a file readable by uid `65532` and add
`--env-file <path>` after `serve`. To have stargaite terminate TLS itself, mount the certificate and its key and give
`--tls-cert` and `--tls-key` in place of `--behind-tls-proxy`; the admin API then speaks TLS too, so the CLI needs
`--admin-url https://localhost:4001` and must trust the certificate (`docker exec -e SSL_CERT_FILE=<ca file> ...`),
which must then also be valid for `localhost`. If it is not, add `--add-host <cert name>:127.0.0.1` to `docker run`
and use `--admin-url https://<cert name>:4001`; a certificate from a public CA needs no `SSL_CERT_FILE`.

## Development

`make ci` runs every check (format, vet, tidy, lint, vulnerability scan, four cross-builds, `go test -race ./...`,
including `tests/e2e`) with Go 1.27 or later; `make ci-container` runs the same inside the Go image, and
`make image-test` builds the image and runs it. No check needs a secret, and the tests use no network beyond loopback.
`tests/contract` runs the per-target cases against a live provider only when `STARGAITE_CONTRACT_BASE_URL` and
`STARGAITE_CONTRACT_API_KEY` are set (its package comment lists the variables); it costs tokens, and without them it
is skipped.
