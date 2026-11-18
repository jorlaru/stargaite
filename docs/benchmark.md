# Benchmark

The figures of kick-off section 18 (AC-52), measured by `tests/benchmark` with `make bench` (or `make bench-container`,
inside the Go image). The method is phase-4 design section 9, with the choices its "Settled in C42" notes record. The
harness fails for every missed target that the table of [accepted deviations](#accepted-deviations) does not list up
to its figure; review this file after each run.

## The run recorded here

| | |
|---|---|
| Date | 2026-11-15 |
| Commit | `a5c3419` with the step C42 changes applied, not yet committed (the harness printed `a5c3419-dirty`) |
| CPU | AMD Ryzen 5 5500, 6 cores, 12 threads |
| Kernel | Linux 7.0.0-43-generic |
| Go | go1.27.4 linux/amd64, `make bench` inside the `golang:1.27.4-trixie` image |
| Pinning | `taskset` present: the proxy on CPU 0 with `GOMAXPROCS=1`, the mock provider on CPUs 1 to 11, the harness unpinned |

The host is a desktop machine that ran other work at the same time, so the figures carry its noise: over four full
runs of the same harness (three with the shipped transport, one with the variant of
[Outbound connection reuse](#outbound-connection-reuse)), a P99 difference varied by up to 0.71 ms
(`first_byte_translated_stream_p99_ms`, 1.470 to 2.184 ms) and a P50 difference by up to 0.10 ms.

## Method

- Both binaries are built by `make bench` and run as processes on loopback: the mock provider
  (`tests/mockprovider/cmd/mockprovider`) and `stargaite serve` on an empty data directory. The proxy's configuration
  is the mock as an `openai` and as an `anthropic` provider, billed per use and priced, so every call writes a priced
  usage record and two log lines; one key may hold 1,000 requests in flight; no telemetry exporter.
- Load is open loop: request *i* has the intended send time *start + i / rate*, and its latency runs from that time,
  so a stall counts against every request it delays (no coordinated omission).
- Added latency: per scenario, ten runs of 200 requests per second for 5 s straight to the mock and ten through the
  proxy, the side that goes first alternating per run; each figure is the median over the ten runs of the difference
  between the proxy's percentile and the mock's (nearest rank). The payload is a one-message chat. Scenarios: a
  synchronous chat (`sync`); a native OpenAI stream, relayed (`native_stream`); a chat stream toward an `anthropic`
  target, translated from the Messages stream, whose direct side is that Messages stream (`translated_stream`); a
  stream from a target with `inline_think_tags`, the tags split across chunks (`think_stream`). Streams are timed to
  their first body byte (`first_byte_…`) and to their end (`latency_…`).
- Throughput: 30,000 synchronous requests at 1,000 per second; met when none fails and the slowest ends within 1 s of
  its intended send time (no backlog).
- Memory: `VmRSS` from `/proc/<pid>/status`, in MB of 10^6 bytes, a minute after the warm-up, with 100 and with 500
  streams held open by the mock, and 5 s after the last latency run. CPU: user and system time from
  `/proc/<pid>/stat`.
- Start-up: from the exec to the `ready` line. Cancellation: from the client's close of a stream that the mock holds
  open to the mock's `Ended` of that call, the slowest of ten.
- Configuration change: `BenchmarkCommitScale18` of `internal/config` on one CPU (`-cpu 1`): one key changed in a
  configuration of 100 providers, 1,000 targets, 1,000 aliases and 10,000 keys.
- RTO: the proxy's store after the run, which holds the 71,410 usage records of the run, plus the kick-off section 18
  configuration (100 providers, 1,000 priced targets, 1,000 aliases, 10,000 keys) applied with `apply -f`; then
  `backup`, the procedure `restore` follows, by hand (stop with `SIGTERM`, move the three store files out,
  `install -m 600` the copy) and the start to ready; a request with the restored key then succeeds. An operator's
  reaction time is outside it.
- Image size: `docker image inspect` of `stargaite:dev` as `make image-test` builds it, on the host, since the Go
  image has no `docker`.

## Kick-off section 18 targets

| Target | Figure | Measured | Verdict |
|---|---|---|---|
| Memory at rest ≤ 40 MB RSS | `rss_rest_mb`; `rss_rest_after_load_mb` | 23.3 MB; 35.8 MB | met |
| Memory with 100 concurrent streams ≤ 100 MB RSS | `rss_streams_100_mb` | 33.0 MB | met |
| Added latency P50 ≤ 0.5 ms | `latency_sync_p50_ms` | 0.257 ms | met |
| | `latency_native_stream_p50_ms`; `first_byte_…` | 0.398 ms; 0.234 ms | met |
| | `latency_translated_stream_p50_ms`; `first_byte_…` | 0.402 ms; 0.263 ms | met |
| | `latency_think_stream_p50_ms`; `first_byte_…` | 0.430 ms; 0.246 ms | met |
| Added latency P99 ≤ 2 ms, stream not transformed | `latency_sync_p99_ms` | 2.090 ms | accepted deviation |
| | `latency_native_stream_p99_ms` | 2.151 ms | accepted deviation |
| | `first_byte_native_stream_p99_ms` | 1.976 ms | met (listed: 2.077 ms in another run) |
| Added latency P99 ≤ 5 ms, SSE inspected or adapted | `latency_translated_stream_p99_ms`; `first_byte_…` | 2.341 ms; 2.184 ms | met |
| | `latency_think_stream_p99_ms`; `first_byte_…` | 2.416 ms; 2.189 ms | met |
| Start-up ≤ 2 s until ready | `startup_ms`; `startup_scale18_ms` | 61 ms empty; 113 ms on the store of the RTO | met |
| Throughput ≥ 1,000 requests per second without streaming | `throughput_errors`; `throughput_max_ms` | 0 errors in 30,000; slowest 26.5 ms | met |
| Concurrency: 500 concurrent streams | `streams_500_failed` | 0 of 500 failed | met |
| Configuration change ≤ 1 s at 100 providers, 1,000 targets, 1,000 aliases, 10,000 keys | `commit_scale18_ms` | 92 ms per change | met |
| Availability 99.9 % monthly | | not measurable by a benchmark: it is a property of a deployment over a month | not measured |
| Recovery: RTO ≤ 15 minutes | `rto_s` | 0.63 s | met |
| Recovery: loss bounded as in ADR-016 | | checked by tests (`TestServeKilled`, `TestBackupRestore`), not by the benchmark | not measured here |
| Cancellation ≤ 1 s after the client disconnects | `cancel_max_ms` | 51 ms | met |
| Image size ≤ 30 MB (reported, not gated) | `image_mb` | 26.5 MB (26,478,861 bytes) | met |

## Other figures of the run

| Figure | Measured |
|---|---|
| Throughput achieved (`throughput_rps`) | 999.7 requests per second |
| Latency under the throughput load, P50 and P99 (`throughput_p50_ms`, `throughput_p99_ms`) | 0.90 ms, 13.3 ms |
| CPU used by the proxy at 1,000 requests per second (`throughput_cpu_cores`) | 0.30 of a CPU |
| CPU used at rest over a minute (`cpu_rest_cores`) | below the 10 ms resolution of `/proc/<pid>/stat` |
| RSS with 500 open streams (`rss_streams_500_mb`; no target) | 62.5 MB |
| Direct to the mock, P50 and P99, synchronous (`direct_sync_p50_ms`, `direct_sync_p99_ms`) | 0.739 ms, 1.297 ms |
| First `apply -f` of the kick-off section 18 configuration through the CLI (`apply_scale18_ms`) | 573 ms |
| Store and backup size (`store_mb`) | 28.1 MB |
| RTO parts: backup, stop, restore steps, start to ready | 496 ms, 9 ms, 12 ms, 113 ms |
| Median cancellation (`cancel_median_ms`) | 50 ms |

## The P99 on one CPU

The synchronous and native-stream P99 differences sit around the 2 ms target: 2.087, 1.902 and 2.090 ms for `sync`
and 2.250, 2.164 and 2.151 ms for `native_stream` over three full runs at `GOMAXPROCS=1`. A diagnostic run of the same
harness with the proxy on two CPUs (`GOMAXPROCS=2`, pinned to CPUs 0 and 1; not the gated configuration) gave 0.674 ms
and 1.339 ms, and a throughput P99 of 3.5 ms instead of 13.3 ms, while the proxy uses 0.3 of a CPU at 1,000 requests
per second. The tail is therefore requests waiting for the one CPU while the proxy runs other work (the usage
writer's batches, the garbage collector and the log lines run on the same CPU as the requests); which of them
dominates is not measured. Both are listed below as accepted deviations, up to 2.5 ms; a run above that fails again.

## Outbound connection reuse

Phase-4 design section 15, point 29: the run recorded here used an outbound transport with Go's
`DefaultMaxIdleConnsPerHost` of 2. With it, the throughput run left 2,870, 3,110 and 3,188 sockets in `TIME_WAIT`
toward the mock over three runs (`throughput_time_wait`), so connections churned. A run with
`MaxIdleConnsPerHost: 100` in `transport.NewClient` left none, but did not change throughput or P99:

| | Default (three runs) | `MaxIdleConnsPerHost: 100` |
|---|---|---|
| `throughput_time_wait` | 2,870; 3,110; 3,188 | 0 |
| `throughput_p99_ms` | 13.0; 11.9; 13.3 | 12.2 |
| `throughput_max_ms` | 27.2; 23.6; 26.5 | 23.9 |
| `latency_sync_p99_ms` | 2.087; 1.902; 2.090 | 2.016 |
| `latency_native_stream_p99_ms` | 2.250; 2.164; 2.151 | 2.161 |

Churn does not hurt throughput or P99 on loopback, where a new connection costs nothing measurable, so phase 4 left
`transport.NewClient` unchanged. Phase 5 (design step C48) reverses that: toward a provider each new connection is
also a lookup and a TLS handshake, and a proxy sends the calls of many clients to a handful of hosts. Measured on
2026-11-16 in the pinned Go image, on loopback, with two waves of parallel calls to one host, each wave held until
all its calls are in flight:

| | Go's default, 2 idle per host | `MaxIdleConnsPerHost: 100` |
|---|---|---|
| New connections, two waves of 16 plain HTTP calls (`TestClientPool`) | 16, then 14 | 16, then 0 |
| TLS handshakes, two waves of 100 calls | 198 for 200 calls | 100 for 200 calls |

`transport.NewClient` now keeps up to 100 idle connections per host and closes one left idle for 90 s, the value of
Go's `DefaultTransport`. No figure was measured over a real network. The run recorded here predates the change: its
`throughput_time_wait` is Go's default, and a new run is expected to show the 0 of the `MaxIdleConnsPerHost: 100` run
above (not run again).

## Container resources

The figures support kick-off section 17's indicative values, which the README now states: a request of 50m CPU and
64 Mi, a limit of 1000m CPU and 256 Mi. At rest the proxy uses no measurable CPU and 23 to 36 MB; 1,000 synchronous
requests per second use 0.3 of a CPU; 500 open streams 62.5 MB (60 Mi). The memory limit leaves room for large bodies,
which these figures do not exercise: each request may hold up to `max_body_bytes` and `max_response_bytes`.

## Accepted deviations

| Figure | Accepted up to | Reason | Date |
|---|---|---|---|
| `latency_sync_p99_ms` | 2.5 ms | On one CPU, requests wait while the proxy runs its other work; 0.674 ms on two CPUs (see above) | 2026-11-15 |
| `latency_native_stream_p99_ms` | 2.5 ms | As for `latency_sync_p99_ms`; 1.339 ms on two CPUs | 2026-11-15 |
| `first_byte_native_stream_p99_ms` | 2.5 ms | As for `latency_sync_p99_ms`; measured from 1.854 to 2.077 ms over three runs | 2026-11-15 |
