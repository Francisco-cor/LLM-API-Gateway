# ADR-002: Sharded Token Bucket with TTL

**Status:** Accepted · **Date:** 2026-09-01

## Context
Original `Limiter.buckets` single `sync.Mutex` + never expiry → leak + contention at 1k rps. Need per-tenant/model/token limits, distributed option.

## Decision
- **Sharded 16×** `fnv` hash (`ratelimit/token_bucket.go:11` `shards [16]*shard`): `shardFor(key)` → `shard.mu` not global. Bench `56ns Allow` vs 210ns global (see `BENCH.md`).
- **TTL 10m** `cleanupLoop 2m` deletes `lastSeen < cutoff`. Fixes leak (`fix(ratelimit): sharded 16x + TTL`).
- **Token-aware** `AllowN(key, n)` where `n = EstimateTokens(chars/4)` (`handler.go:237`); before dispatch check `limiter.AllowN(key+"_tokens", est)`.
- **Overrides** `overrideStore.Resolve(tenant, modelPattern)` supports `tenant:"pro", model:"gpt-4*"` → per-tier RPM/burst (`config.yaml:rate_limit.overrides`).
- **Distributed** `ratelimit/redis.go` Lua `INCR+EXPIRE` if `REDIS_URL` set; 50ms timeout → fallback memory (no SPOF).
- **Hot-reload** `UpdateLimits(rpm,burst)` `RWMutex` + `GetLimits()` (Fase 9).

## Alternatives
- `golang.org/x/time/rate` — per-key not built-in, needs `sync.Map`.
- `redis-cell` GCRA — needs Redis always, not local fallback.
- `sony/gobreaker` style limiter — overkill, we own 182 LOC.

## Consequences
+ p99 <5ms at 1k rps 16 shards, TTL bounded.
+ Budget `insufficient_quota` separate (`internal/budget`).
- FNV hash not crypto; fine for sharding.
- `sync.Map` alternative not needed until >5k rps contention (see `BENCH.md` Next).

## Validation
`tests/ratelimit_v2_test.go` concurrency 200 goroutines burst 100 exactly, TTL, bench <200ms.
