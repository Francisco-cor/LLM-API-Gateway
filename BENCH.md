# Benchmarks

> **These are microbenchmarks, not a capacity claim.**
> The figures below were captured on **2026-11-13 at commit `f8663d0`**
> inside `golang:1.24-alpine` on an **AMD Ryzen 5 3600X (6 cores / 12
> threads)**, using the command shown. They measure individual components, not
> gateway throughput, and the repository still does not commit an end-to-end
> load-test result. CPU model, governor, background load, container limits and
> Go version change them; re-run before quoting.

## What is actually benchmarked

Only these benchmarks exist (`tests/bench_test.go`):

| Benchmark | What it measures |
|---|---|
| `BenchmarkLimiter_Allow` | Token bucket `Allow` on the sharded limiter |
| `BenchmarkLimiter_Allow_Sharded` | Parallel `Allow` (`b.RunParallel`), exercising shard contention |
| `BenchmarkLimiter_AllowN_Token` | `AllowN` for a token count |
| `BenchmarkBuildKey` | SHA-256 canonical JSON cache key |
| `BenchmarkCache_Get_Hit` | In-memory LRU hit |
| `BenchmarkCache_Get_Miss` | In-memory LRU miss |

Historical documentation listed `BenchmarkLimiter_Concurrent`,
`BenchmarkMarshalJSON_Pooled` and `BenchmarkHandler_Dispatch_Mock`. **Those do
not exist** and have been removed. Do not cite them.

## How to run

```bash
# from the repository root
go test ./tests -run '^$' -bench=. -benchmem -count=3

# a single family, with a stable machine and no other load
go test ./tests -run '^$' -bench='Benchmark(Limiter|Cache|BuildKey)' \
  -benchmem -count=5 -benchtime=2s

# capture CPU and memory profiles for one benchmark
go test ./tests -run '^$' -bench='BenchmarkBuildKey' \
  -benchmem -cpuprofile=/tmp/cpu.prof -memprofile=/tmp/mem.prof
go tool pprof -http=:9091 /tmp/cpu.prof
```

Report the result with **hardware, OS, Go version, commit and command**:

```text
CPU:        <model>, <cores>, <RAM>
OS:         <linux/amd64 | darwin/arm64 | ...>
Go:         go version
Commit:     git rev-parse --short HEAD
Command:    go test ./tests -run '^$' -bench=. -benchmem -count=3
```

## Measured reference (2026-11-13)

Metadata:

```text
CPU:        AMD Ryzen 5 3600X 6-Core Processor (12 threads visible)
OS:         linux/amd64 (golang:1.24-alpine container)
Go:         go1.24 (toolchain in the image)
Commit:     f8663d0
Command:    go test ./tests -run '^$' -bench=. -benchmem -count=3
GOMAXPROCS: 12
```

Results, three runs per benchmark (not cherry-picked):

```text
BenchmarkLimiter_Allow-12            254.0 / 299.0 / 424.4 ns/op     40 B/op   3 allocs/op
BenchmarkLimiter_Allow_Sharded-12    353.0 / 343.6 / 334.2 ns/op     48 B/op   3 allocs/op
BenchmarkLimiter_AllowN_Token-12     305.5 / 231.7 / 278.8 ns/op     40 B/op   3 allocs/op
BenchmarkBuildKey-12                2610 / 2249 / 2145 ns/op         753 B/op   7 allocs/op
BenchmarkCache_Get_Hit-12             97.64 / 112.7 / 98.73 ns/op    16 B/op   1 alloc/op
BenchmarkCache_Get_Miss-12            16.36 / 16.46 / 17.09 ns/op     0 B/op   0 allocs/op
```

The spread is real: shared CI machines and background load move these by
30–60 %. Prefer the median of repeated runs over a single number.

## How to run

```bash
# from the repository root
go test ./tests -run '^$' -bench=. -benchmem -count=3

# a single family, with a stable machine and no other load
go test ./tests -run '^$' -bench='Benchmark(Limiter|Cache|BuildKey)' \
  -benchmem -count=5 -benchtime=2s

# capture CPU and memory profiles for one benchmark
go test ./tests -run '^$' -bench='BenchmarkBuildKey' \
  -benchmem -cpuprofile=/tmp/cpu.prof -memprofile=/tmp/mem.prof
go tool pprof -http=:9091 /tmp/cpu.prof
```

Report the result with **hardware, OS, Go version, commit and command**:

```text
CPU:        <model>, <cores>, <RAM>
OS:         <linux/amd64 | darwin/arm64 | ...>
Go:         go version
Commit:     git rev-parse --short HEAD
Command:    go test ./tests -run '^$' -bench=. -benchmem -count=3
```

## Load testing: what is missing

`tests/load/k6.js` exists and targets `/v1/chat/completions`, but:

- the gateway has **no mock provider mode**; the script requires a real
  upstream or a separately deployed fake upstream;
- its thresholds (`p95 < 30 ms`, `p99 < 50 ms`) assume a fake upstream with
  near-zero latency; against a real LLM they measure the provider, not the
  gateway;
- no result from this script is committed, so there is no reproducible
  end-to-end throughput number in this repository;
- the old “1 instance ~3.2k RPS at 80% CPU” claim has been removed: there is no
  committed evidence for it.

To run a meaningful load test, deploy a local OpenAI-compatible stub with a
fixed 200 response, point a provider `base_url` at it, disable auth or use a
test key, then:

```bash
k6 run tests/load/k6.js \
  --env GATEWAY_URL=http://localhost:8080 \
  --env GATEWAY_API_KEY=test-key
```

Publish the stub implementation, its latency, the hardware and the commit
alongside the result. Without those, a p95 is not comparable.

The roadmap tracks a versioned fake-upstream harness and a capacity test:
see [`docs/ROADMAP.md`](docs/ROADMAP.md) (R-29, Fase 7).

## Profiling

pprof is **opt-in** and is served by the admin mux, not on `:6060`:

```yaml
admin:
  pprof_enabled: true
  api_key: ${ADMIN_API_KEY}
  bind_address: 127.0.0.1
```

Restart the process (pprof routes are registered when the admin handler is
built), then:

```bash
curl -sS -H "Authorization: Bearer $ADMIN_API_KEY" \
  http://127.0.0.1:8081/debug/pprof/heap -o /tmp/heap.prof
go tool pprof -http=:9091 /tmp/heap.prof
```

Heap dumps can contain provider keys and prompt data. Treat them as secrets,
do not attach them to tickets, and disable pprof afterwards.

## Notes that are still true

- `http.Transport` is shared with `MaxIdleConns=100`,
  `MaxIdleConnsPerHost=20`, `MaxConnsPerHost=100`, `IdleConnTimeout=90s`,
  `TLSHandshakeTimeout=5s` and `ForceAttemptHTTP2=true`.
- The server sets `ReadHeaderTimeout=5s`, `IdleTimeout=120s`, and renews the
  SSE write deadline by 60 s before every chunk (a rolling deadline, not a
  cleared one).
- The limiter uses 16 shards, a 10-minute bucket TTL and a 2-minute cleanup
  tick. The token bucket capacity is `token_burst` (tokens), independent from
  the request burst.
- The cache is either in-memory LRU or Redis, not a layered cache, and enforces
  `max_bytes`/`max_ttl`. If Redis is configured and dies at runtime, gets
  return miss and sets fail silently; the request still succeeds. See the
  runbook.
- Chat cache misses are coalesced with single-flight; a benchmark of the
  provider call would measure the provider, not the coalescer.

## Next (roadmap)

- Add a committed fake upstream and a repeatable load test.
- Publish p50/p95/p99 with hardware, date and commit.
- Add a gateway-overhead timer separate from upstream latency.
- Only optimize after the measurement exists; do not tune nanoseconds against
  an unmeasured bottleneck.
