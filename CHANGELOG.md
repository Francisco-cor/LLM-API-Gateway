# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Pending integration (other workstreams)

The following areas are being worked on in parallel branches. They are **not**
claimed as part of `v1.1.0` until their commits are merged and verified; this
section will be replaced by concrete entries when that happens.

- **Streaming** — further SSE and usage-accounting work beyond the v1.1.0 fixes.
- **Pricing** — additional price catalog and reconciliation work; the ledger
  does not exist yet, so no billing claim is made.
- **Cache** — shared/distributed semantic index and invalidation work.
- **CI** — coverage gate, OpenAPI validation and load/conformance jobs. The
  current workflow uploads coverage but does not enforce a threshold.

## [v1.1.0] - 2026-11-13

### Added

- `api/openapi.yaml`: OpenAPI 3.1 contract for chat, embeddings, models, health,
  readiness, metrics and the admin API, including SSE, tools and
  `response_format.type`.
- `docs/ROADMAP.md`: maturity matrix, phases with measurable acceptance
  criteria, a 30-item backlog and the minimum Postgres schema for a persistent
  control plane and usage ledger.
- `docs/RUNBOOK.md`: startup, health semantics, reload/rollback, Redis outage,
  provider 429/5xx, circuit breaker, exhausted budget, cut streams, key
  rotation, opt-in pprof, proposed SLOs and alert rules.
- `SECURITY.md`: private advisory reporting, scope, threat model and the
  current-versus-pending control matrix.
- `docs/adr/ADR-004-professionalization.md`: Postgres control plane, DB-less
  data plane, asynchronous ledger, capability registry and fail-closed policy.
- Provider/model pricing catalogs with input, output and embedding rates, a
  versioned currency/policy and an unknown-price policy (`fallback`/`reject`).
- Exact embedding-response caching and an opt-in embedding-backed semantic
  cache with identity and response-shape namespaces.
- Configurable readiness/provider health cache and timeouts, hot-reloadable,
  with an explicit `skipped` state for credit-bearing Anthropic checks.
- Shared, replaceable Redis connection manager across rate limit, budget and
  cache; reload pings and swaps safely and keeps the previous backend on
  failure.
- Admin config fields for `bind_address`, `allow_insecure` and
  `pprof_enabled`; pprof routes are registered inside the admin mux only when
  opted in.
- Idempotent budget reservations with Redis Lua or in-process mutex semantics,
  actual-token cost adjustment and retry/circuit/budget integration for
  embeddings.
- `CostsUSDTotal`, pricing provenance (`known`/`fallback`) and bounded
  Prometheus path cardinality.

### Changed

- Admin control plane is fail-closed: with `admin.allow_insecure: false`, an
  empty or unresolved `${ADMIN_API_KEY}` stops startup. Admin binds to
  `127.0.0.1` by default. The comparison uses constant time.
- Token-aware limiting uses a bucket measured in **tokens**, independent from
  the request bucket measured in requests. `token_burst` has a floor of 1024
  when token-aware limiting is enabled.
- Streaming preserves provider usage metadata in the final SSE chunk when
  `stream_options.include_usage` is requested, including Anthropic message
  events and Gemini usage-only terminal events.
- OpenAI tools, tool choices, tool results and tool-call responses/deltas are
  translated to Anthropic `tool_use/tool_result` and Gemini
  `functionCall/functionResponse`.
- The OpenAI-compatible chat contract covers multimodal content, tools, audio,
  logprobs, parallel tool calls and modern completion controls; those fields
  are preserved in cache namespaces, with a safe text projection for narrower
  providers.
- Rate-limit enablement is hot-reloadable; overlapping weighted patterns are
  deterministic; Redis URL updates go through the shared reconnect manager
  instead of an ineffective runtime patch.
- Provider error messages are bounded; API-key scopes and expiry are enforced;
  cache keys are isolated by client identity; trailing JSON and unknown YAML
  fields are rejected.
- Gemini API keys are sent in `x-goog-api-key`, not in the query string.
- pprof no longer has a `:6060` listener. It is opt-in under `/debug/pprof/*`
  in the admin mux, with the same admin authentication.

### Fixed

- Rate-limit token budgets no longer share the request burst capacity; a
  normal prompt can no longer be rejected forever by an undersized bucket.
- Streams no longer hang after the first chunk due to the server write
  deadline; the deadline is cleared once the stream is active.
- Readiness collapses concurrent Kubernetes probes into one provider fan-out
  and clears stale snapshots when health options change.
- Limiter cleanup goroutines are closed on shutdown.
- Provider `Retry-After` is preserved and propagated; error codes such as
  `model_not_found` are correct.

### Security

- Fail-closed admin control plane, constant-time admin-key comparison,
  loopback-by-default bind and opt-in pprof reduce the exposure of a process
  that can rewrite configuration and hold provider keys.
- Request bodies are capped at 1 MiB, upstream responses at 8 MiB, and admin
  patches at 1 MiB. Unknown JSON/YAML fields are rejected.
- `Authorization`, provider keys, admin keys and Redis URLs are redacted from
  logs and from `GET /admin/config`.
- CI runs `golangci-lint` (including `gosec`) and `govulncheck` pinned to a
  reproducible tool version. The workflows do not assert a zero-issue count;
  results depend on the linters and advisory database at build time.

### Documentation

- README rewritten around the real quickstart, a verified feature table, real
  metric and flag names, honest limitations, benchmarks with caveats and an
  explicit comparison with LiteLLM/Portkey-class gateways.
- `BENCH.md` no longer lists benchmarks that do not exist
  (`BenchmarkLimiter_Concurrent`, `BenchmarkMarshalJSON_Pooled`,
  `BenchmarkHandler_Dispatch_Mock`) and labels unverified figures as local
  reference.
- `docs/ARCHITECTURE.md` corrected: `budget/budget.go`, current admin/pprof
  topology, no upstream `Cache-Control` claim, streaming/retry/hedge limits.
- `examples/curl/*.sh` are executable and use the real admin JSON field
  (`Admin.APIKey`); the contract test command is `-run TestContract`.
- Added `CONTRIBUTING.md`, Dependabot, CODEOWNERS, issue templates and a PR
  template.

### Corrected claims from earlier documentation

- **Anthropic health check:** it is not free. It sends a 1-token Messages
  request and may incur a minimal charge. Set
  `health.skip_expensive_checks: true` to skip it; `skipped` counts as healthy
  for `/readyz` but makes `/health/providers` report `overall: degraded`.
- **Lint/vulnerability counts:** previous text claimed “0 issues” and “0 vulns”
  as permanent facts. CI runs the tools; it does not guarantee zero findings.
- **Coverage:** the 85% badge was unverified. The README no longer claims a
  coverage number or gate. A prior `-coverpkg` measurement was around 53% and
  has not been re-run for this release.
- **Mock mode:** there is no mock provider mode. Tests use `httptest`; the
  `k6` script requires a real upstream.
- **Image size/dependency count:** no `<20MB` or “5 deps” claim is made. See
  `go.mod` (8 direct modules).
- **Metrics:** only the metric names in `internal/metrics/metrics.go` are
  documented; `cache_hits` without the `_total` suffix does not exist.

## [v1.0.0] - 2026-09-01

### Added

- **Unified OpenAI-compatible API** — `POST /v1/chat/completions` +
  `POST /v1/embeddings` + `GET /v1/models` behind the `Provider` interface
  (`Name/Send/SendStream/Embed/Models/HealthCheck/DiscoverModels`).
- **Providers** — OpenAI, Anthropic (`anthropic-version: 2023-06-01`) and Gemini
  (`generateContent`/`streamGenerateContent`/`embedContent`).
- **Translate layer** — OpenAI pivot ↔ Anthropic/Gemini (ADR-001).
- **Streaming SSE** — `stream:true` with `data: {...}` + `data: [DONE]`, flush
  per chunk, Anthropic/Gemini → OpenAI translation.
- **Intelligent routing** — exact models, glob/regex patterns, weighted canary,
  auto-discovery via `GET /models` and `model_aliases` on fallback.
- **Resilience** — retry with exponential jitter, circuit breaker with half-open
  probe, hedge race and `Retry-After` propagation; `X-Gateway-Provider` header.
- **Observability** — Prometheus metrics, OTEL trace context, `slog` with
  request/tenant/provider/latency fields, optional Jaeger/Prometheus/Grafana
  compose services.
- **Health** — `/health`, `/health/providers`, `/livez`, `/readyz` with drain
  semantics.
- **Traffic control** — sharded token bucket with TTL cleanup, token-aware
  limiting, per-tenant/model overrides, monthly budget and Redis Lua buckets
  with an in-process limiter fallback.
- **Cache** — exact LRU or Redis cache, `X-Cache`/`X-Cache-TTL`/`X-Cache-Skip`,
  cache only for 200 non-streaming responses (ADR-003).
- **Security** — Bearer tenant keys with scopes/expiry, CORS allowlist, baseline
  security headers, secret redaction.
- **Control plane** — admin API with redacted config, validated reload,
  provider list and hot knobs; 1 s file watcher and `SIGHUP`.
- **Perf & ops** — hardened HTTP server timeouts, shared transport, JSON buffer
  pool, K8s Deployment/HPA/PDB/ServiceMonitor, non-root/read-only container
  posture.
- **DX & release** — examples for curl/Python/Node/LangChain/Postman, C4
  architecture and ADR-001..003, `BENCH.md`, `VERSION`, contract tests and CI.

### Changed

- Go `1.22 → 1.24`; Alpine `3.19 → 3.21` builder/runtime.
- Strict config validation (ports, fallback references, model lists, bursts).
- Handler body limit, unknown-field rejection and per-handler timeouts.
- Error mapping: no provider → `404 model_not_found`; validation → `400`;
  retryable failures → `502 provider_error`.
- Provider health fan-out runs in parallel with per-provider timeouts.

### Fixed

- `stream:true` is actually proxied instead of being accepted and ignored.
- Gemini API key is not logged or placed in URLs.
- Limiter buckets expire instead of leaking.
- Auth no longer uses the raw `Authorization` header as the rate-limit key.

### Security

- CI runs `govulncheck`, `golangci-lint` and `go vet`; see “Corrected claims”
  above for the limits of those statements.

## [v0.9.0] - 2026-08-31 (pre-release, Fases 5-10)

- Fase 5 Observability, Fase 6 RateLimit v2, Fase 7 Cache, Fase 8 Routing,
  Fase 9 Control Plane, Fase 10 Perf/Hardening.

## [v0.3.0] - 2026-08-15 (Fases 1-4)

- Fase 1 Hardening base, Fase 2 Auth, Fase 3 Streaming, Fase 4 Resilience.

## [v0.1.0] - 2026-08-01 (scaffold)

- Initial scaffold: `Provider` interface, 3 providers, naive fallback,
  in-memory limiter, `slog`, Docker multi-stage.

[Unreleased]: https://github.com/Francisco-cor/LLM-API-Gateway/compare/v1.1.0...HEAD
[v1.1.0]: https://github.com/Francisco-cor/LLM-API-Gateway/releases/tag/v1.1.0
[v1.0.0]: https://github.com/Francisco-cor/LLM-API-Gateway/releases/tag/v1.0.0
