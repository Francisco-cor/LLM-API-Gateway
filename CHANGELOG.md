# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Capability registry and capability-aware fallback (R-15/R-16/R-17):**
  `internal/capability` defines per-model `chat`, `stream`, `tools`,
  `parallel_tools`, `vision`, `audio`, `json_object`, `json_schema` and
  `embeddings`, with provider-kind defaults refined by
  `providers.<name>.capabilities` (model name, glob or `default`).
  `Registry.ResolveForChat` rejects an incompatible model with
  `400 model_capability_mismatch` and an actionable `param` **before** any
  provider call. Normal fallback, hedge and streaming fallback skip models that
  would drop a required capability instead of degrading silently; skips are
  logged and counted in `gateway_capability_mismatches_total`. Weighted routing
  filters incompatible providers, and `GET /admin/providers` exposes the
  effective set per model.
- **Idempotency-Key (R-03):** non-streaming chat and embeddings requests can
  carry `Idempotency-Key`. A successful response is stored for
  `idempotency.ttl` (bounded LRU, optional Redis for replicas) and replayed
  with `Idempotent-Replayed: true`; reusing the key with a different body
  returns `409 idempotency_key_reuse`, and a duplicate that cannot wait for the
  in-flight leader returns `409 idempotency_in_progress` with `Retry-After: 1`.
  Concurrent duplicates are coalesced with a local lock plus a Redis `SET NX`
  lock; Redis failures degrade to per-process dedupe. Streaming bypasses the
  store. New metric: `gateway_idempotency_requests_total`.
- **Official SDK conformance (R-02):** a new CI job builds the real gateway
  and drives it with pinned `openai==3.51.0` (Python) and `openai@7.57.0`
  (Node) against a stdlib fake upstream. It covers chat, streaming with usage,
  tools, embeddings, `/v1/models`, 404 mapping and idempotency replay. Local
  entry point: `make conformance`.
- **OpenAPI contract gate in CI:** Redocly lints `api/openapi.yaml` with zero
  warnings allowed, and `oasdiff breaking --fail-on ERR` compares the spec with
  the PR base branch (or `HEAD^` on direct pushes). Breaking changes require the
  explicit `[openapi-breaking-approved]` marker. Local equivalents:
  `make openapi-lint` and `make openapi-breaking`.
- **Spec fixes for the stricter lint:** health probes declare `security: []`,
  tags have descriptions, the SSE schema references `StreamChunk`, and
  `unsupported_parameter` is documented as a `BadRequest` example instead of an
  unused response component.

Nothing else yet. The next milestone is the Postgres control plane and the usage
ledger; see `docs/ROADMAP.md`.

## [v1.1.1] - 2026-11-13

### Added

- **Strict production profile:** `--strict` or `GATEWAY_ENV=prod` applies a
  second validation pass at startup and on every reload. It requires auth, an
  admin key, pprof off, no wildcard CORS, https provider URLs (unless
  `allow_private: true`) and a shared Redis store for limits/budgets.
- **Provider `base_url` validation:** strict mode rejects `http`, localhost,
  private/loopback/link-local addresses and cloud metadata endpoints
  (`169.254.169.254`, `100.100.100.200`, `fd00:ec2::254`). `allow_private`
  is the explicit opt-in for local Ollama/vLLM backends.
- **Admin hardening:** the admin key rotates live on reload; `GET
  /admin/config` returns an `ETag` and `PATCH` honours `If-Match` with `409` on
  conflict. Every authenticated admin operation emits an `admin_audit` record.
- **Auth hardening:** mandatory scopes with an allowlist, fail-closed route →
  scope map, keys ≥ 32 characters in production, optional HMAC-SHA256 pepper
  (`auth.key_pepper` / `AUTH_KEY_PEPPER`) and a per-IP 401 throttle
  (20 failures/minute → 429 with `Retry-After`).
- **Token bucket in real units:** `rate_limit.tokens_per_minute` and
  `token_burst` size a bucket measured in tokens, independent from the request
  bucket. Overrides get isolated per-model buckets, and authenticated tenant
  identity is authoritative for the limiter key.
- **Honest streaming:** a truncated upstream or a mid-stream provider failure
  emits `event: error` and **no `data: [DONE]`**; a silent close reports
  `code: stream_truncated`. Real provider usage is preserved in the terminal
  chunk, `choices[].index` and `tool_calls[].index` are stable, and the SSE
  write deadline is rolling (60 s).
- **OpenAI error mapping:** upstream 400/422 → 400, 404 → 404, 413 → 413,
  429 → 429, 401/403 → 502, 503/504 → 503, other 5xx → 502. `Retry-After` is
  rounded up and floored at 1; `invalid_request_error` /
  `unsupported_parameter` errors include `param`.
- **Tolerant data-plane contract:** unknown request/message fields are ignored,
  `response_format.json_schema` is passed through verbatim, and the request
  cap is 8 MiB for vision/audio. Config and admin remain strict.
- **Budget correctness:** prices are per million tokens with the per-token
  fallback converted once; reservations are leases with `reservation_ttl` and
  a per-minute reconciler; months are UTC; cached prompt tokens are priced at
  the catalog cache rate; `require_shared_store` fails closed instead of
  silently multiplying the quota per replica.
- **Cache correctness:** byte budget (`cache.max_bytes`), TTL cap
  (`cache.max_ttl`), single-flight coalescing for chat misses (the leader
  bills once), semantic lookup reusing its embedding when storing the miss, and
  real Redis stats (SCAN count cached for 30 s).
- **Hot reload, actually:** `auth.enabled`, CORS, `logging.level`, the admin
  key and the pepper are reloadable. Bind/port/pprof and `logging.format`
  require a restart with a clear error. The watcher retries with backoff and a
  Redis outage no longer aborts the rest of the reload.
- **CI and release:** golangci-lint v2 config, coverage measured with
  `-coverpkg=./...` and enforced at 55 %, a Redis service plus an opt-in
  `-tags=integration` job, multi-arch images, SPDX SBOM and keyless cosign
  signing.
- **Deployment:** NetworkPolicy, ServiceAccount without a mounted token,
  hostname topology spread, `GATEWAY_ENV=prod` and `GATEWAY_REPLICAS` in the
  manifests/config, `redis_url` in the ConfigMap, and a read-only `prod`
  Compose profile.
- **OpenAPI 1.1.1:** `param` on errors, `index` on tool calls, prompt/completion
  token details, `json_schema`, tolerant request schemas, 8 MiB 413 and the
  400/404/413/429/502/503/504 mapping.

### Changed

- Benchmark documentation now publishes a measured local run with hardware,
  date, commit and command instead of old unverified figures.
- README, RUNBOOK, ARCHITECTURE, SECURITY, CONTRIBUTING and ROADMAP were
  reconciled with the code: pprof is only in the admin mux, auth is not
  optional in production, the admin key rotates live, and the data plane no
  longer rejects unknown fields.

### Fixed

- Price unit bug: a `cost_per_token_usd` fallback was interpreted as USD per
  million tokens, under-pricing unknown models by 1,000,000×.
- `Retry-After` no longer becomes `0` for sub-second upstream hints.
- Retry/hedge losers settle circuit breakers honestly; readiness survives
  client disconnects.
- Cache stats no longer report a fake `-1` size for Redis, and a single large
  response cannot flush the whole memory cache.
- Rate-limit overrides no longer share a bucket with the global limit.

### Security

- Fail-closed scopes and route allowlist; unknown scopes are a startup error.
- HMAC-SHA256 key pepper, 32-character key floor in production, 401 throttling.
- Strict mode blocks SSRF-shaped provider URLs by configuration (literal
  scheme/host/IP); DNS rebinding and redirect allowlists remain future work.
- Admin ETag/If-Match, audit trail, live key rotation and userinfo/query
  redaction in `base_url`.

### Tests

- `go test ./... -coverpkg=./...` measured **66.0 %** on 2026-11-13 at commit
  `f8663d0` (Go 1.24 in Docker). CI enforces a 55 % floor, not the measured
  number.
- Redis integration tests are opt-in with `-tags=integration` and require
  `REDIS_URL`; CI provides a Redis 7 service and skips the job cleanly when no
  tagged files exist.

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

[Unreleased]: https://github.com/Francisco-cor/LLM-API-Gateway/compare/v1.1.1...HEAD
[v1.1.1]: https://github.com/Francisco-cor/LLM-API-Gateway/releases/tag/v1.1.1
[v1.1.0]: https://github.com/Francisco-cor/LLM-API-Gateway/releases/tag/v1.1.0
[v1.0.0]: https://github.com/Francisco-cor/LLM-API-Gateway/releases/tag/v1.0.0
