# Contributing — LLM API Gateway

Thanks for considering a contribution! This is `v1.0.0` production-ready; first external PR simulation is DoD for Fase 11.

## Quick start for contributors (<10 min)

```bash
git clone https://github.com/Francisco-cor/LLM-API-Gateway.git && cd LLM-API-Gateway
cp .env.example .env  # fill OPENAI_API_KEY etc, or use mock providers in tests
go vet ./... && go test ./... -race -cover -count=1
docker-compose up --build -d   # gateway :8080 admin :8081 pprof :6060 + jaeger 16686 prometheus 9090 grafana 3000
curl -s http://localhost:8080/health | jq
curl -s http://localhost:8080/v1/models -H "Authorization: Bearer local-dev" | jq
make bench   # limiter 56ns BuildKey 1.8µs
```

## Branching & commits

- **Branches:** `main` protected, PR with CI green (test, lint, vulncheck, docker), 1 review, squash merge.
- **Conventional Commits:** `feat(scope):` `fix:` `perf:` `chore:` `docs:` `test:` `refactor:` + scope `provider|proxy|config|ratelimit|cache|auth|metrics|admin|deploy`.
  Example: `feat(provider): add Azure OpenAI Embed`.
- **DoD per commit (see `PLAN.md` 7):**
  - `go test ./... -race -cover` passes (>85% target)
  - `golangci-lint run` 0 warnings (`make lint`)
  - `go vet ./...` ok
  - `govulncheck ./...` 0 (or `make vulncheck`)
  - README/CONFIG docs updated if config changes
  - Contract tests pass (`tests/contract/`)

## Architecture overview

Read `docs/ARCHITECTURE.md` (C4) + `docs/adr/ADR-001..003`. Key contracts:

- `Provider` interface (`internal/provider/provider.go:33`): `Name, Send, SendStream, Models, HealthCheck`; optional `Embedder`.
- `Registry` (`proxy/router.go:20`): weighted exact → exact → weighted pattern → pattern → 404; `Reload` atomic.
- `Translate` pivot OpenAI (`internal/translate/`).
- `Resilience` decorators: `retry.go` `circuit.go` `hedge.go`.
- Config validated `config/config.go:160 Validate()` → hot-reload via `admin` `:8081` + `SIGHUP` + `Watch` 1s.

## Adding a provider (1 PR)

1. Create `internal/provider/<name>.go` implementing `Provider` (+ `Embedder` if needed) using `newHTTPClient(timeout)` sharedTransport.
2. Add translate if needed in `internal/translate/`.
3. Register in `cmd/gateway/main.go:buildProviders`.
4. Add `config.yaml` example `providers.<name>`.
5. Tests: `tests/` table-driven + `httptest` mock upstream + contract `openai_compat`.
6. Docs: `README.md` provider list + `docs/ARCHITECTURE.md` update.

## Testing

```bash
make test          # -v -cover
make test-race     # -race -coverprofile
make test-cover    # html
make bench         # limiter/cache/handler benchmarks (see BENCH.md)
go test ./tests -run TestAdmin -v        # admin hot-reload
go test ./tests -bench=. -benchmem -count=1
k6 run tests/load/k6.js --env GATEWAY_URL=http://localhost:8080  # 1k RPS p95<30ms
```

Contract suite (`tests/contract/openai_compat_test.go`) verifies OpenAI SDK compat without external deps: `POST /v1/chat/completions` 200 shape `id/object/choices/usage`, `stream:true` `data: [DONE]`, `GET /v1/models`, `POST /v1/embeddings`, error mapping `404 model_not_found`.

## Lint & security

```bash
make lint          # golangci-lint (errcheck, govet, staticcheck, gosec)
make lint-fix      # --fix
make vulncheck     # govulncheck ./...
docker scout cves llm-api-gateway:ci   # if docker scout installed
```

## Dev loop

- `make dev` runs `air` if installed (`go install github.com/air-verse/air@latest`), else `go run ./cmd/gateway`. Live-reload on `**/*.go, config.yaml`.
- Env: `OPENAI_API_KEY` etc from `.env` or `config.yaml ${ENV}`. Admin `ADMIN_API_KEY` for `:8081` (`Bearer` or `X-Admin-API-Key`). Redis `REDIS_URL`.
- Hot reload test: `kill -HUP $(pidof gateway)` or `curl -X POST -H "Authorization: Bearer $ADMIN_API_KEY" http://localhost:8081/admin/reload`.

## PR checklist (for reviewers)

- [ ] CI green: `test` `lint` `vulncheck` `docker`
- [ ] No secrets in logs (`Authorization`/`apiKey` redacted `***`)
- [ ] Config `Validate` covers new fields; `Clone` round-trip ok
- [ ] Metrics added if new path: `gateway_requests_total` etc
- [ ] Docs: `README.md` + `config.yaml` example + `CHANGELOG.md` entry
- [ ] K8s `deploy/k8s/` still `readOnlyRootFilesystem` if image changes

## Release (maintainers)

`VERSION` file + `git tag v1.0.0` triggers `.github/workflows/release.yaml` (goreleaser/docker). See `CHANGELOG.md` Keep a Changelog. Versioning SemVer: `v0.x` (Fase 1-4), `v0.9.0-rc` (5-10), `v1.0.0` (11).

## License

MIT — `LICENSE`. By contributing, you agree to same license + DCO.

## Getting help

Open issue with `go test -v` log + `config.yaml` redacted. For security reports, email `security@` (not public issue).
