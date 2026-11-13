# ADR-002: Sharded Token Bucket with TTL

**Status:** Accepted · **Date:** 2026-09-01

## Context
Original `Limiter.buckets` single `sync.Mutex` + never expiry → leak + contention at 1k rps. Need per-tenant/model/token limits, distributed option.

## Decision
- **Sharded 16×** `fnv` hash (`ratelimit/token_bucket.go` `shards [16]*shard`): `shardFor(key)` → `shard.mu`, not a global mutex. Microbenchmarks are recorded in `BENCH.md`; they are local reference values, not a capacity guarantee.
- **TTL 10m** with a 2m cleanup tick deletes buckets whose `lastSeen` is older than the cutoff. Fixes the original unbounded map.
- **Token-aware** `AllowN(key, n)` and `AllowWithLimits(key, n, rpm, burst)`; the handler estimates `chars/4` over message text plus tools JSON before dispatch. It is an estimate, not a tokenizer.
- **Overrides** `overrideStore.Resolve(tenant, modelPattern)` supports `tenant:"pro", model:"gpt-4*"` → per-tier RPM/burst (`config.yaml:rate_limit.overrides`). Model-scoped buckets avoid a high-limit model refilling a shared bucket.
- **Distributed** `ratelimit/redis.go` Lua token bucket if `REDIS_URL` is active; a 100 ms timeout falls back to the local bucket (availability over global correctness). The local fallback multiplies the effective limit by the number of replicas.
- **Hot-reload** `UpdateLimits(rpm, burst)` under `RWMutex` + `GetLimits()`.

## Alternatives
- `golang.org/x/time/rate` — per-key limits are not built in and need a `sync.Map`.
- `redis-cell` GCRA — needs Redis always, so no local fallback.
- A library limiter — the in-repo implementation is ~250 LOC and can be hot-reloaded without replacing the handler.

## Consequences
+ The limiter is bounded by TTL and does not need a global mutex.
+ Budget `insufficient_quota` is a separate mechanism (`internal/budget`), so rate limiting and spend control do not share semantics.
- FNV is not cryptographic; it is only used for sharding.
- Redis fallback is per-process: global limits are not guaranteed during an outage. A strict mode is roadmap R-27.

## Validation
`tests/ratelimit_v2_test.go` covers concurrency, TTL and overrides. `BENCH.md` records the microbenchmark command and the caveat that its figures were not re-measured for this release.
