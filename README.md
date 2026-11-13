# stargaite

An inference proxy for LLM providers: one OpenAI-compatible endpoint in front of many providers, with model aliases,
client keys, budgets and limits, all held in an embedded store and changed through an admin API. One static binary,
one data directory. MIT licensed.

**Status: phase 2 in progress.** What works today: the store, the admin API and the CLI, providers (with eighteen
built-in presets), targets, model aliases, client keys with rotation and revocation, `export`/`apply`/`validate`,
`GET /v1/models`, and `POST /v1/chat/completions`, synchronous and streamed, toward providers of the `openai`
protocol, with the parameter contract and reasoning control of the alias. Anthropic targets, `POST /v1/messages` and
fallback arrive in the next steps of phase 2. The full specification is [kick-off.md](kick-off.md); the designs are
[docs/design/phase-1.md](docs/design/phase-1.md) and [docs/design/phase-2.md](docs/design/phase-2.md).

## Quick start

From a clone of this repository, with Go 1.27 or later. The mock provider in `tests/mockprovider` stands in for a real
one, so no account or credential is needed and no file is edited by hand. `tests/e2e` runs this block line by line.

```sh
go build -o stargaite ./cmd/stargaite
go run ./tests/mockprovider/cmd/mockprovider &
./stargaite serve &
curl -sS --retry 10 --retry-connrefused --retry-delay 1 http://127.0.0.1:4000/health/ready
./stargaite providers add mock --protocol openai --base-url http://127.0.0.1:4010 --allow-private-network --billing none
./stargaite models add fast --target mock/mock
./stargaite keys create my-agent --model fast
curl -s http://127.0.0.1:4000/v1/models -H "Authorization: Bearer sk-stargaite-..."
curl -s http://127.0.0.1:4000/v1/chat/completions -H "Authorization: Bearer sk-stargaite-..." -d '{"model":"fast","messages":[{"role":"user","content":"hi"}]}'
```

The mock provider listens on `127.0.0.1:4010`; `serve` puts the data plane on `127.0.0.1:4000`, the admin API on
`127.0.0.1:4001` and the store in `./data`, and the health check waits until it is up. `models add` also creates the
target `mock/mock`. `keys create` prints the client key once: paste it in place of `sk-stargaite-...` in the last two
calls. The first lists the alias `fast`; the second sends a chat completion to it and gets the mock provider's answer,
with the response headers `x-stargaite-request-id`, `x-stargaite-trace-id` and `x-stargaite-target` (add `-i` to
`curl` to see them). Both processes log JSON lines to the terminal; `kill %1 %2` stops them.
`./stargaite --help` lists every command, and each one accepts `--json`, never prompts, and exits `0` on success, `1`
on failure and `2` on wrong usage; `remove`, `keys revoke` and `apply --prune` (unless `--dry-run`) need `--yes`.

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
docker stop stargaite
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
