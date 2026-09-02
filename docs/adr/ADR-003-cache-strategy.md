# ADR-003: Cache — LRU Memory + Redis + Semantic (opt-in)

**Status:** Accepted · **Date:** 2026-09-01

## Context
Same prompt pays 2×. Need exact-hit cache with low overhead; semantic optional.

## Decision
- **Interface** `cache.Cache{Get,Set,Delete,Stats}` (`cache/cache.go:17`) with `memory` LRU (`container/list` + `map`) 1000 TTL 5m (`cache/memory.go`), `redis` backend (`cache/redis.go`), `semantic` wrapper (`cache/semantic.go` cosine 0.97).
- **Key** `BuildKey(model,messages,temperature,max_tokens,tools)` → `sha256(canonicalJSON)` (`cache/key.go` 1.8µs, 400B).
- **Policy:** only `200` non-streaming; `X-Cache:HIT/MISS` + `X-Gateway-Provider:cache`; respect `X-Cache-Skip:true` + `X-Cache-TTL:100ms` + `Cache-Control` upstream.
- **Wiring** `cache.NewMemory` → if `redisClient` wrap `NewRedis` → if `semantic_enabled` wrap `NewSemantic` (`cmd/gateway/main.go:95`).
- **Hot reload** `SetCache(cache, ttl)` (`proxy/handler.go:132`).

## Alternatives
- Always Redis — adds latency for miss (50ms timeout) vs 75ns memory hit.
- Semantic first — cost of embeddings, threshold tuning 0.97 default disabled.

## Consequences
+ Second identical prompt <5ms `HIT` (`tests/cache_test.go`).
+ `BENCH.md` `Cache_Get_Hit 75ns`, `BuildKey 1.8µs`.
- Memory bound 1000 needs tuning for prod; `CACHE_MAX_SIZE` hot-reloadable via `PATCH /admin/config`.
- Semantic requires embeddings model; disabled by default (`cache.semantic_enabled:false`).

## Verification
`tests/cache_test.go` hit/miss/TTL/eviction LRU 2, `TestHandler_CacheHITMISS` headers, `semantic` similarity 0.99 identical.
