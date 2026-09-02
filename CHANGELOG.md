# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v1.0.0] - 2026-09-01

### Added
- **Unified OpenAI-compatible API** — `POST /v1/chat/completions` + `POST /v1/embeddings` + `GET /v1/models` behind `Provider` interface (`Name/Send/SendStream/Embed/Models/HealthCheck/DiscoverModels`).
- **Providers** — OpenAI (`openai.go`), Anthropic (`anthropic.go` + `anthropic-version:2023-06-01`), Gemini (`gemini.go` `generateContent`/`streamGenerateContent`/`embedContent`).
- **Translate layer** — `internal/translate/` pivot OpenAI ↔ Anthropic/Gemini (Fase 8, ADR-001).
- **Streaming SSE** — `stream:true` `text/event-stream` with `data: {...}` + `data: [DONE]`, flush per chunk, Anthropic/Gemini → OpenAI translation (Fase 3).
- **Intelligent routing** — `providers.<name>.models` wildcards `gpt-4*`/`claude-*`/`gemini-*` and regex, `routing.weighted` canary `90/10 ±5%` in 1k reqs, auto-discovery `models: []` → `GET /v1/models` (Fase 8).
- **Resilience** — retry exponential jitter 3× `200ms` (`internal/resilience/retry.go`), circuit breaker 5 → open 30s half-open 1 (`circuit.go`), hedge race 300ms (`hedge.go`), `model_aliases` remap, `Retry-After` propagation, `X-Gateway-Provider` header (Fase 4).
- **Observability** — Prometheus `/metrics` (`gateway_requests_total`, `gateway_request_duration_seconds`, `gateway_tokens_total`, `gateway_cache_hits_total`, `gateway_provider_errors_total`, `gateway_circuit_state`), OTEL `traceparent` span per provider, `slog` with `request_id/tenant/provider/latency_ms`, Jaeger/Prometheus/Grafana via `docker-compose` (Fase 5).
- **Health** — `GET /health` (uptime), `/health/providers` fan-out parallel 3s `degraded`, `/livez`/`/readyz` K8s with `SetReady(false)` draining (Fase 1 & 5).
- **Traffic control** — sharded 16× `fnv` + TTL 10m limiter (56ns `Allow`), token-aware `AllowN`, per-tenant/model `overrides`, monthly `budget` `insufficient_quota`, Redis Lua `INCR+EXPIRE` with memory fallback (Fase 6).
- **Cache** — `X-Cache:HIT/MISS` LRU 1000 TTL 5m + Redis + semantic 0.97, `X-Cache-TTL`/`X-Cache-Skip`, only 200 non-stream, `sha256` key 1.8µs, hit 75ns (Fase 7, ADR-003).
- **Security** — Bearer multi-tenant `auth` with `scopes`/`expires_at`, CORS allowlist, `X-Content-Type-Options nosniff` etc, secrets redacted `***` (logger + admin) (Fase 2).
- **Control plane** — Admin `:8081` `GET /admin/config` redacted, `POST /admin/reload` 400 rollback, `GET /admin/providers`, `PATCH /admin/config` hot knobs (`rate_limit/cache/circuit/hedge/routing/logging`), file watcher 1s poll + `SIGHUP` with `Validate` rollback (Fase 9).
- **Perf & Ops** — `ReadHeaderTimeout 5s` anti-Slowloris, `IdleTimeout 120s`, shared `Transport MaxIdleConns100/PerHost20/KeepAlive30s/ForceAttemptHTTP2`, `sync.Pool` JSON buffers, `BENCH.md` + `k6` 1k RPS p95<30ms, K8s `Deployment/HPA 3→10/PDB minAvailable:2/ServiceMonitor`, `pprof :6060` behind admin auth, `readOnlyRootFilesystem` + `runAsNonRoot` (Fase 10).
- **DX & Release** — `examples/` (curl/python/node/langchain/postman), `docs/ARCHITECTURE.md` C4 + ADR-001..003, `CONTRIBUTING.md` (<10 min), `BENCH.md`, `VERSION` SemVer, `make dev` air live-reload, contract suite `tests/contract/openai_compat_test.go` (Fase 11).
- **CI/CD** — `ci.yaml` (test, lint, vulncheck, docker), `release.yaml` on tag `v*` (Fase 11).

### Changed
- Go bump `1.22 → 1.24`, `alpine:3.19 → 3.21`, `golang:1.24-alpine` builder.
- `config.Load` strict validation: `port∈[1024,65535]`, `fallback_chain` subset, `models` non-empty, `burst>=1` etc.
- `proxy/handler.go` adds `MaxBytesReader 1MiB` + `DisallowUnknownFields` + per-handler timeout.
- Error mapping: `ErrNoProvider` → `404 model_not_found`, validation → `400`, retryable → `502 fallback`.
- `HealthProvidersHandler` now parallel fan-out with per-provider 3s timeout.

### Fixed
- `Stream=true` no longer hangs (was accepted but never proxied, G1).
- `Gemini API key` not logged; `Anthropic` healthcheck no longer spends credits.
- `Limiter buckets` leak fixed via TTL 10m cleanup + sharded 16× mutex (G6).
- Auth no longer uses raw `Authorization` header as rate-limit key.

### Security
- `govulncheck` 0 vulns, `golangci-lint` 0 issues, `docker scout` clean, `go vet` clean.

## [v0.9.0] - 2026-08-31 (pre-release, Fases 5-10)

- Fase 5 Observability, Fase 6 RateLimit v2, Fase 7 Cache, Fase 8 Routing, Fase 9 Control Plane, Fase 10 Perf/Hardening.

## [v0.3.0] - 2026-08-15 (Fases 1-4)

- Fase 1 Hardening base, Fase 2 Auth, Fase 3 Streaming, Fase 4 Resilience.

## [v0.1.0] - 2026-08-01 (scaffold)

- Initial scaffold: `Provider` interface, 3 providers, naive fallback, in-memory limiter, `slog`, Docker multi-stage.

[v1.0.0]: https://github.com/Francisco-cor/LLM-API-Gateway/releases/tag/v1.0.0
