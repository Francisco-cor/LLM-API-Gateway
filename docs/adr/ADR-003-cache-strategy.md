# ADR-003: Cache — LRU Memory + Redis + Semantic (opt-in)

**Status:** Accepted · **Date:** 2026-09-01

## Context
Same prompt pays 2×. Need exact-hit cache with low overhead; semantic optional.

## Decision
- **Interface** `cache.Cache{Get,Set,Delete,Stats}` (`cache/cache.go`) with `memory` LRU (`container/list` + `map`) 1000 TTL 5m (`cache/memory.go`), `redis` backend (`cache/redis.go`) and a `semantic` wrapper (`cache/semantic.go`, cosine 0.97 default).
- **Key** `BuildKey(req)` → `sha256(canonical JSON)` over response-shaping fields (`cache/key.go`), salted with the client identity by `BuildKeyForIdentity`. Embedding keys are separate (`cache/embedding_key.go`). See `BENCH.md` for the microbenchmark; no fixed nanosecond claim is made here.
- **Policy:** only `200` non-streaming; `X-Cache:HIT/MISS` + `X-Gateway-Provider:cache`; respect `X-Cache-Skip:true` + `X-Cache-TTL:100ms`. The gateway does **not** read upstream `Cache-Control`; it only sets `Cache-Control: no-cache` on SSE responses.
- **Wiring** `buildCache` (`cmd/gateway/main.go`): memory → Redis if the shared store is active → semantic wrapper if enabled. The backend is chosen when the cache is built; a Redis outage at runtime produces misses, not an automatic memory fallback.
- **Hot reload** `SetCache(cache, ttl)` on the chat/embeddings handlers.

## Alternatives
- Always Redis — adds a network hop and a hard dependency for a cache that is optional.
- Semantic first — embeddings cost money and need threshold tuning; it is opt-in and off by default.
- Layered memory + Redis — would need coherence/invalidation; not implemented.

## Consequences
+ A second identical prompt can be served without an upstream call.
+ `BENCH.md` records the build-key/cache microbenchmarks with their caveats.
- Memory is bounded (1000 entries by default) and must be tuned for production; `cache` knobs are hot-reloadable via `PATCH /admin/config`.
- Semantic mode requires an embedding provider and is local to a process; a shared index is roadmap work.
- Cache entries contain response bodies (potentially sensitive) in memory or Redis, unencrypted.

## Verification
`tests/cache_test.go` covers hit/miss/TTL/LRU eviction and identity isolation; `TestHandler_CacheHITMISS` covers headers; semantic tests cover identical/similar queries and namespace isolation.
