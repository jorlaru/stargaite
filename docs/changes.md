# Changes

## Since phase 6

`./setup.sh` is now the bootstrap of a guided console, `bin/stargaite-console`, which runs on the host: it checks
bash, the root rule and Docker with Compose and buildx, builds the console from the clone when it is missing or of
another version, and starts it ([Install](../README.md#install)). The console's first run goes from the machine to a
first answer under a step bar ([The first run](../README.md#the-first-run)), and its home offers every later change
as a task picked from a list ([The home](../README.md#the-home)); each subcommand runs the same task by flags, without
a terminal ([Scripts and agents](../README.md#scripts-and-agents)). Docker's buildx component is now needed, to build
the console, and curl and jq no longer are; Open to the network makes its self-signed certificate itself, without
openssl. Upgrade gains `--rebuild`, which rebuilds the clone's own version without fetching, and at a terminal the
console restarts as the new version after an upgrade. Esc or Ctrl-C during a paid check stops it at the server.

For the proxy: `stargaite targets verify --until-stdin-closes`, with standard input a pipe, ends its request when
standard input closes, and the server then starts no further call of the run
([Targets verify](reference.md#targets-verify)); a verify run whose request ended before its calls did is no longer
stored as verified.

## Since phase 5

`./setup.sh` installs, configures and upgrades a Docker deployment by asking questions
([Install](../README.md#install)); the README starts with it, the Go quick start is now
[Try it locally with Go](../README.md#try-it-locally-with-go), and the manual path,
[Install with Docker by hand](reference.md#install-with-docker-by-hand), writes `.env` without an editor. `compose.yaml` passes
`STARGAITE_VERSION` to the build as its version, so a Compose deployment no longer always reports `dev`; it names its
image from `STARGAITE_IMAGE` (`stargaite:local` by default, as before) and publishes the host port `STARGAITE_PORT`
(`4000` by default). `stargaite backup -` writes the copy to standard output and its summary to standard error, so a
copy leaves the container without staying in the volume, and `stargaite restore <file|-> --yes` puts a copy back while
`serve` is stopped, after checking it, for the binary and the container alike; it replaces the manual procedure, which
a container, with no shell, could not run ([Backup and restore](reference.md#backup-and-restore)). Git and the image build also
ignore `.env.*`, `compose.override.yaml`, `tls/` and `backups/`.

## Since phase 4

Providers and targets take `--param` rules ([Parameters](reference.md#parameters)); like any provider
change, a change of a provider's rules clears the stored verification of its targets. A client's output cap toward a
target that declares `output_cap_param` is now sent under that name: an OpenAI SDK's `max_tokens` toward a target of
the `openai` preset arrives as `max_completion_tokens`, listed `max_tokens:translated`, and a client's
`max_completion_tokens` toward one of the `openrouter` preset as `max_tokens`. A `mistral` provider created earlier
lacks the preset's new rule: `providers set <name> --param output_dimension=forward` adds it. The log line of a refused
request names the parameter at fault in `param`. Providers take the four timeout flags of `settings set`
([Resilience](reference.md#resilience)); a provider that declares none behaves as before, and like any provider change a change
of its timeouts clears the stored verification of its targets.

A provider's `401`, `402`, `403` or `404`, and an error object that states one of them or a `429` or `5xx` inside a
`2xx` answer or before a stream's first event, now move the request to the alias's next target, so an alias whose
first provider is out of credit or refuses the key is served by its fallback ([Resilience](reference.md#resilience)). With no
target left, a provider's `404` is `502 upstream_error` with a fixed message instead of the provider's `404` and text,
and a `2xx` that only holds an error object is answered by its status instead of being relayed as a success; its
record carries that status and no tokens. The provider's text in an error answer is cut to 2,000 characters; the
warning for a refused credential is now `provider refused the target`, with the status alone, and the text is on the
attempt's log line as `provider_message`. An answer that a retry would only repeat carries `x-should-retry: false`.

Toward an `anthropic` target, a chat member at the value that turns its feature off is now left out instead of
skipping the target: `frequency_penalty` and `presence_penalty` 0, `service_tier` `"auto"`, `modalities` `["text"]`
and `logit_bias` `{}`, as `n` 1 and `logprobs` and `store` false already were, so an OpenAI SDK's defaults reach
the target. `cache_control` on a content part, a tool or a tool message is copied onto the block it becomes, so prompt
caching works through chat; a tool message may carry images and a `name`, and a tool call with empty `arguments` is
sent as a call without arguments. `image_url.detail` is still not sent: the image goes whole and the provider chooses
its resolution. Values with meaning, such as `seed`, a non-zero penalty, `n` above 1, `json_object`, another
`service_tier` or a non-empty `logit_bias`, still skip the target unless a `drop` rule reaches them, and a `name` on
a user, assistant or system message still makes the request untranslatable for it.

What a stream sends for the provider's last event (`[DONE]` or `message_stop`, or the error event that ends it, and
toward a chat client of an `anthropic` target also the finish-reason and usage chunks) now leaves once the request has
ended for the proxy, after its usage is charged and its slots are released, as the end of a non-streaming answer
already did: a client that starts its next request on reading it no longer meets its own `max_parallel_requests` as a
`429 parallel_limit_exceeded`. Every earlier event is still sent as it arrives, so a client that starts on a native
finish-reason chunk, before the terminator, can still meet it.

Outbound connections, to providers and to the telemetry endpoint, are now kept for reuse up to 100 per host, each
closed after 90 s idle, instead of Go's default of 2: parallel calls to one provider reuse the connections earlier
calls opened rather than paying a lookup, a connection and a TLS handshake each
([docs/benchmark.md](benchmark.md#outbound-connection-reuse)). A kept connection that the provider has closed
fails as any connection failure does ([Resilience](reference.md#resilience)).

A figure no record reported is now absent at every level of `usage show` instead of 0 ([Usage](reference.md#usage)): reasoning
tokens where the provider reported none, in the records and in the daily sums alike, so a reported 0 (the evidence that
effort `none` works) is told apart from no figure; and a daily cost where no call was priced, while a priced 0 is now
shown. The store's migration 003 makes the daily sums' reasoning tokens and cost nullable and copies each old 0 as
absent, since it cannot be told from an unknown. A `pay_per_use` call whose request reached the provider and got no
answer, such as one cut by a timer or by the client leaving before it, now has no cost and counts as unpriced instead of
costing 0 from the price table (its record has `status_code` 0); as before, it charges no budget. One that never reached
the provider, such as a refused connection, keeps its cost of 0.

A request body is now read in place each time the proxy scans it: a scan no longer copies the values it reads past, so
scanning a body whose megabyte is one long message allocates a few KB where it allocated about 4 MB. The index of the
members the proxy reads costs what it did, so a body of many short messages can still cost several times its size
per scan.

`models explain` now serves `--endpoint embeddings` and `--endpoint rerank`, through the rules the alias, its targets
and their providers declare; `--effort` and `--max-output-tokens` apply to chat and messages only. Where a command
takes a file (`apply -f`, `validate -f`, `models explain --body`), `-` reads standard input, so a file of the host
reaches the CLI inside the container; the Docker alias above gains `-T` for that, and `apply -f - --prune` needs
`--dry-run`. On `/v1/messages`, `context_management` and `mcp_servers` are no longer `400 unknown_parameter`;
`fallbacks` and `thread` still are, since one lets a body choose another model and the other expires into a `404`,
and `providers set <provider> --param fallbacks=forward` lets one through; so are the beta `compaction`,
`fallback_credit_token` and `output_format`. The reference was read on 2026-11-17: the API version `2026-10-21`
removes `temperature`, `top_p` and `top_k`, which stay known because the proxy sends `anthropic-version: 2023-06-01`
when the client sends none; a client that sends `2026-10-21` with one of them gets the provider's own `400`. A body with `mcp_servers` uses tools,
so an alias whose targets declare `capabilities.tools` false skips them for it, and `validate` refuses a provider's
or a target's `drop` on `mcp_servers`; on chat toward an `anthropic` target, `mcp_servers` that a rule lets through
uses tools the same way. Since both keys are now known on `/v1/messages`, a stored alias `drop` on
`context_management` or `mcp_servers` acts only toward a target that refuses the key, as for every known key: to keep
removing it, list it in the target's `unsupported_params`, or declare a provider's or a target's `drop` for
`context_management`; to keep MCP servers out, declare `capabilities.tools` false.

`export` now ends its YAML with the end marker `...`, and `apply -f -` publishes only a document that ends with it, so a
pipe cut short no longer resets the resource at the cut; what you add to an exported file goes before the marker, since
text after it is a second document, which `apply` refuses. Toward a target that declares `output_cap_param`, a client
that sends both cap names with different values now finds the declared member listed `translated` too when its value
changed. `targets verify` prints the provider's own text for a call it refused. `validate` now refuses a timer above one
day (86,400,000 ms), a provider's or a target's rule on a member the proxy itself reads or writes (`model`, `messages`,
`system`, `stream`, `input`, `query`, `documents`), and an `output_cap_param` naming one, a key that carries a
contractual feature (such as `tools`), a key of any reasoning dialect (such as `reasoning_effort`), another key the
proxy writes toward the target (`stream_options`, its `on` and `off` keys) or a known chat parameter other than
`max_tokens` and `max_completion_tokens` (such as `temperature`); a model's own name such as `max_new_tokens` stays
valid. It also refuses an alias's rule, or a target's `unsupported_params` entry, on one of those members. A stored
configuration with one keeps serving, and the next change must remove it. A target that lists a member in
`unsupported_params` now lets the alias decide even when its provider declares that member `drop`: it is skipped unless
the alias drops it.
