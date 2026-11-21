# Development

`make ci` runs every check (format, vet, tidy, lint, `shellcheck` on the shell scripts, vulnerability scan, four
cross-builds, `go test -race ./...`, including `tests/e2e`) with Go 1.27.4 or later and a C compiler, which the race
detector needs; `make tools` fetches the pinned linters, `shellcheck` and `jq`, each checked against its checksum.
On Linux, `tests/setup` runs `setup.sh` and the console it starts against stubs of every command they call, and
needs bash 4.4 or later, `script(1)` (util-linux), for its runs at a terminal, and `git`, for the clones of `upgrade`.
`make ci-container` runs the same inside the Go image, which has all of them, and `make image-test` builds the image
and runs it. `make setup-test`, on a Linux host with Docker and outside `make ci` like `make image-test`, runs
`setup.sh` end to end in a temporary copy of the working tree, under its own Compose project, image name and free
host port, so it runs beside a service on port 4000: an install against the mock provider, which runs from the source
in the Go image, then `key`, `budget`, `telemetry`, `remote` with a self-signed certificate (then renewed), `provider`,
`alias --fallback`, the console's terminal tests against that container, `install` again under TLS, `status`, `logs`,
`stop --yes`, `start`, `restart --yes`, `backup`, `restore --from <file> --yes`, an `upgrade` to a new commit whose
printed way back it then runs, `status --check` and `uninstall --yes`; it removes only what it created. The hosted
workflow runs `make ci-container`, `make image-test` and `make setup-test`. No check needs a secret, and the tests of
`make ci` use no network beyond loopback.
The README's images are `docs/console/*.svg`, written from the console's own frames by its tests and compared by
`go test ./...`; `go test ./internal/console/ui/guided -run TestShots -update` rewrites them.
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

## Hardening

`tests/adversarial`, part of `make ci`, sends hostile input to the data plane and the admin API on every endpoint it
applies to: malformed, invalid UTF-8, deeply nested and repeated-key bodies, oversized bodies, revoked and malformed
keys, unknown aliases, incompatible targets, fragmented and truncated streams, split and unclosed think tags, SSRF
addresses, redirects and DNS rebinding, budget and quota races, oversized and gzip-bomb answers, credentials echoed by
a provider, control bytes in logged values and a flood of bad tokens. `TestServeAdversarial` in `cmd/stargaite` checks
the server options on raw sockets: request smuggling forms, header size and count limits, slow headers, a client that
stops reading and one that disconnects. `make ci` replays every fuzz seed under `testdata/fuzz`; `make fuzz` runs
each fuzz target for `FUZZTIME` (60 s by default, `make fuzz FUZZTIME=5m`), and a finding, written under the
package's `testdata/fuzz`, is committed as a seed with its fix.

## Benchmark

`make bench` (Linux) builds the binary and the mock provider into `BENCHDIR` (`$TMPDIR/stargaite-bench` by default)
and runs `tests/benchmark` against them: both as processes on loopback, the proxy with `GOMAXPROCS=1` and pinned to
one CPU with `taskset` when present, as the container's 1000m CPU limit gives it. It measures every target of kick-off
section 18 that a benchmark can (added latency, throughput, memory, start-up, cancellation, a configuration change at
scale and the recovery time) and prints one JSON document (to the file `STARGAITE_BENCH_OUT` names, else standard
output) and a table; it exits `1` for every missed target that [docs/benchmark.md](benchmark.md) does not list as
an accepted deviation. `make bench-container` runs it inside the Go image. It is not part of `make ci`, which only
compiles and lints it. [docs/benchmark.md](benchmark.md) holds the method and the last figures.

## Release checklist

Before tagging a release, a maintainer runs each of these with their own keys and accounts, nothing private, and
fixes or documents what fails:

1. `make ci-container`, and the hosted workflow on a fork with no secret configured (PUB-5).
2. `make image-test`: the image builds, starts on a read-only root, bootstraps its admin key and stops with exit `0`.
3. `make setup-test`: `setup.sh` end to end against the mock provider: an install, the later subcommands, HTTPS with a
   generated certificate, and an upgrade with its way back (AC-54).
4. `make fuzz` (`FUZZTIME=60s` by default); a finding is committed as a seed with its fix.
5. `make bench`, then a review of [docs/benchmark.md](benchmark.md): the figures, the host and the commit
   updated, and every miss listed as an accepted deviation with its figure, reason and date (AC-52).
6. `tests/contract` with its `STARGAITE_CONTRACT_*` variables (see [Development](#development)) against each preset
   whose `untested` mark is to go, and the live-safe parts against each of the first ten presets of
   `./stargaite providers presets`, with the preset notes checked against each provider's current documentation
   (AC-53).
7. `tests/langfuse` with its variables, before a release that changes the span schema or the exporter (AC-30).
8. The official OpenAI and Anthropic client SDKs, unmodified, against the mock provider through the proxy, changing
   only the base URL, the key and the model name: a chat completion with each, and against `/v1/messages` a tool loop
   with thinking enabled (AC-01, AC-02).
9. A search of every file and every commit message, in every revision, for the name of an organisation or a person
   that develops, deploys or uses the product and for data of a particular infrastructure
   (`git grep -I -e <pattern> $(git rev-list --all)` for file contents, and `git log --all --format=%B` for commit
   messages) (PUB-2).
10. The quick start on a clean machine: clone, build, start, an answer from `/health/ready`, then every line of
    [Try it locally with Go](../README.md#try-it-locally-with-go) with the mock provider (PUB-3; `tests/e2e` runs it in
    `make ci`). On a clean Linux machine with only the prerequisites of [Install](../README.md#install), `./setup.sh` with one real
    provider, then `./setup.sh key` and `./setup.sh status`: one `targets verify`, and one test call for the install
    and one for the key, each capped at 256 output tokens and never retried (PRI-9).
11. A review of the repository and all its documentation for English (PUB-8).
