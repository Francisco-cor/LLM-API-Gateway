# Benchmarks

> **These are microbenchmarks, not a capacity claim.**
> The last recorded reference was captured on **2026-09-01 on Windows AMD64 with
> Go 1.24** using the command below. It has **not** been re-measured for
> `v1.1.0`, and the repository does not commit an end-to-end load test result.
> Treat the numbers as a local reference, not a guarantee. CPU model, governor,
> background load and Go version change them.

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

## Last recorded reference (not re-measured)

Source: the previous `BENCH.md`, dated 2026-09-01, Windows AMD64, Go 1.24,
`go test ./... -bench=. -benchmem -count=1`. Formatting normalized; values are
shown as recorded, not re-validated:

```text
BenchmarkLimiter_Allow-8            210 ns/op      0 allocs/op
BenchmarkLimiter_Allow_Sharded-8    145 ns/op      0 allocs/op   # sharded path
BenchmarkLimiter_AllowN_Token-8     220 ns/op      0 allocs/op
BenchmarkBuildKey-8                1150 ns/op    512 allocs/op   # sha256(canonical JSON)
BenchmarkCache_Get_Hit-8            180 ns/op      0 allocs/op
BenchmarkCache_Get_Miss-8           150 ns/op      0 allocs/op
```

Earlier README/BENCH text also quoted `56 ns`, `75 ns` and `1.8 µs` for the same
paths. Those values conflict with the recorded table above and with each other;
they have been removed rather than presented as fact. If you need a number,
run the command on your hardware and publish the metadata.

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
- The server sets `ReadHeaderTimeout=5s`, `IdleTimeout=120s`, and clears the
  write deadline for active SSE streams.
- The limiter uses 16 shards, a 10-minute bucket TTL and a 2-minute cleanup
  tick. Those are implementation facts, not throughput claims.
- The cache is either in-memory LRU or Redis, not a layered cache. If Redis is
  configured and dies at runtime, gets return miss and sets fail silently; the
  request still succeeds. See the runbook.

## Next (roadmap)

- Add a committed fake upstream and a repeatable load test.
- Publish p50/p95/p99 with hardware, date and commit.
- Add a gateway-overhead timer separate from upstream latency.
- Only optimize after the measurement exists; do not tune nanoseconds against
  an unmeasured bottleneck.
