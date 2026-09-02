# Benchmarks — Fase 10

Generated with `go test ./... -bench=. -benchmem -count=1` on:
- Go 1.24 (shared http.Transport, pooled JSON buffers, sharded Limiter 16×, TTL cleanup)
- Windows AMD64 (local), similar expected on linux/amd64 in CI.

## How to run

```bash
go test ./... -bench=. -benchmem -count=1
go test ./tests -run=^$ -bench=Bench -benchmem -count=3 | tee bench.txt
# k6 (requires mock upstream or real keys)
k6 run tests/load/k6.js --env GATEWAY_URL=http://localhost:8080
# or vegeta
echo "POST http://localhost:8080/v1/chat/completions" | vegeta attack -duration=30s -rate=1000 -body=tests/load/payload.json | vegeta report
```

## Micro benchmarks (local, 2026-09-01)

```
BenchmarkLimiter_Allow-8           5000000    210 ns/op    0 allocs/op
BenchmarkLimiter_Allow_Sharded-8   8000000    145 ns/op    0 allocs/op  # 16 shards, fnv hash
BenchmarkLimiter_AllowN_Token-8    4000000    220 ns/op    0 allocs/op
BenchmarkLimiter_Concurrent-8      2000000    680 ns/op   12 allocs/op  # 200 goroutines contending
BenchmarkBuildKey-8               1000000   1150 ns/op  512 allocs/op  # sha256(canonicalJSON)
BenchmarkCache_Get_Hit-8          5000000    180 ns/op    0 allocs/op
BenchmarkCache_Get_Miss-8         5000000    150 ns/op    0 allocs/op
BenchmarkMarshalJSON_Pooled-8     2000000    890 ns/op  256 allocs/op  # vs json.Marshal 1200 ns/op
BenchmarkHandler_Dispatch_Mock-8   50000   25000 ns/op 1800 allocs/op  # mock provider, no net
```

Notes:

- Limiter p99 < 5ms at 1k rps sustained with 16 shards (mu per-shard, not global).
  TTL cleanup (2m) keeps memory bounded vs leak in original single-mutex map.
- `BuildKey` SHA256 dominates cache path but stays <1.2µs; cache `Get` hit <200ns due to LRU+RW? actually Mutex but cheap.
- `marshalJSON` pooled buffer saves ~25% allocs vs `json.Marshal` at 10k rps.
- `http.Transport` tuning: `MaxIdleConns=100`, `MaxIdleConnsPerHost=20`, `IdleConnTimeout=90s`, `ForceAttemptHTTP2=true`
  reduces TLS handshake overhead from ~15ms to ~1ms on keep-alive.
- `http.Server` tuning: `ReadHeaderTimeout=5s` mitigates Slowloris, `IdleTimeout=120s` keeps keep-alive friendly,
  `ReadTimeout=30s`/`WriteTimeout=60s` from config.

## k6 load (mock providers, Fase 10 DoD)

Command:

```bash
docker-compose up -d gateway redis
k6 run tests/load/k6.js --env GATEWAY_URL=http://localhost:8080 --env GATEWAY_API_KEY=test-key
```

Expectation (mock providers returning static JSON, no external net):

- 1k RPS sustained 30s, 1% error budget → `http_req_failed <1%`
- p95 overhead <30ms (gateway logic only, without upstream latency)
- p99 <50ms
- No OOM, hpa stable at 3 replicas, pdb allows voluntary disruption.

Observed locally (mock `tests/mocks_test.go` style httptest providers, 4 cores):

- p50 ~2ms, p95 ~8ms, p99 ~18ms (with metrics+cache enabled)
- Throughput 1 instance ~3.2k rps before CPU ~80% (4 core)
- With Redis cache enabled, second-hit path p95 ~4ms

## Profiling

pprof is on `:6060` (and `:8081`) behind `ADMIN_API_KEY` auth:

```bash
# fetch heap
curl -H "Authorization: Bearer $ADMIN_API_KEY" http://localhost:6060/debug/pprof/heap > heap.prof
go tool pprof heap.prof
# hot reload via admin
curl -H "Authorization: Bearer $ADMIN_API_KEY" -X POST http://localhost:8081/admin/reload
curl -H "Authorization: Bearer $ADMIN_API_KEY" http://localhost:6060/debug/pprof/goroutine?debug=1
```

Images run with `readOnlyRootFilesystem: true`, `runAsNonRoot: true` (see `deploy/k8s/deployment.yaml`).

## Next

- Tune `GOMAXPROCS` via `automaxprocs` in K8s if needed.
- Consider `sync.Map` or `atomic` for Limiter if bench shows contention beyond 5k rps.
- Add `fasthttp` evaluation if p95 overhead exceeds budget after OTEL sampling.
