# Contributing

Thanks for taking the time to improve the gateway. This document describes how to build, test and submit a change that fits the project. It is intentionally practical: if a command here does not work, that is a bug in the docs — open an issue.

## Prerequisites

- **Go 1.24** (`go.mod` pins the language version).
- Optional: Docker + Docker Compose for the container profile.
- Optional: Redis 7 for distributed rate limit/budget/cache.
- Optional: `golangci-lint`, `govulncheck`, `air` for the local dev loop.

No provider key is required to run the test suite: tests use `httptest` fakes. A key **is** required to run the gateway against a real provider.

## Local setup

```bash
git clone https://github.com/Francisco-cor/LLM-API-Gateway.git
cd LLM-API-Gateway
cp .env.example .env            # fill at least one provider key; do not commit .env
export OPENAI_API_KEY="sk-..."  # or ANTHROPIC_API_KEY / GEMINI_API_KEY
go run ./cmd/gateway -config config.dev.yaml
```

`config.dev.yaml` disables tenant auth and binds the admin API to loopback. Do not use it as a production profile.

Useful commands:

```bash
make build        # go build -o bin/gateway ./cmd/gateway
make run          # build + run with config.yaml
make dev          # air if installed, else go run (config.yaml)
make test         # go test ./... -count=1 -shuffle=on
make test-race    # go test ./... -race
make test-integration # REDIS_URL=... go test -tags=integration ./... -race
make cover        # go test ./... -coverpkg=./... + total
make lint         # golangci-lint run ./...
make lint-fix     # golangci-lint run --fix ./...
make bench        # go test ./... -bench=. -benchmem
make vulncheck    # govulncheck ./...
make docker       # docker build -t llm-api-gateway .
make docker-run   # docker-compose up -d
make clean
```

Contract tests and focused runs:

```bash
go test ./tests/contract -run TestContract -v
go test ./tests -run TestAdmin -v
go test ./tests -run '^$' -bench=. -benchmem -count=1
```

## What CI runs

The workflow in `.github/workflows/ci.yaml` runs, on push to `main` and pull requests:

- `go vet ./...`
- `go test ./... -race -coverpkg=./... -coverprofile=coverage.out`
- a coverage floor of **55 %** on the `-coverpkg` total
- `golangci-lint` v2 (configuration in `.golangci.yml`, includes `gosec`)
- `govulncheck` with a pinned version (advisory while `go.mod` is upgraded)
- a Redis 7 service and an opt-in `-tags=integration` job
- `docker build`, a blocking Trivy OS scan, an advisory Go-module scan and an SBOM artifact

Coverage is measured, not guessed: a local `go test ./... -coverpkg=./...` on 2026-11-13 at `f8663d0` measured 66.0 %. Do not claim a different number without the command and the result. The gate is 55 %, not the measured value.

Integration tests use a build tag and `REDIS_URL`:

```bash
REDIS_URL=redis://127.0.0.1:6379/0 go test -tags=integration ./... -count=1
```

If no integration-tagged files exist, the command still passes and runs the normal suite.

## Pull requests

Keep changes small and explain the “why”. A PR should:

- [ ] Compile and pass `go test ./... -race`, `go vet ./...` and `golangci-lint run ./...` locally when possible.
- [ ] Add or update tests for behavior changes (table-driven + `httptest` for providers; contract tests for response shape).
- [ ] Update `CHANGELOG.md` under `Unreleased` (or the next version section) for user-visible changes.
- [ ] Update docs when behavior, config, metrics, flags or endpoints change. `docs/ROADMAP.md` is the plan; `README.md` is the pitch; `docs/RUNBOOK.md` is operations. Do not duplicate the plan in three places.
- [ ] Not introduce a claim that cannot be verified (coverage, benchmark, “production-ready”, provider count, image size).
- [ ] Not include secrets, `.env`, `coverage.out`, heap profiles or generated binaries.
- [ ] Preserve the OpenAI contract unless the change is explicitly a breaking change with a version bump and migration note.
- [ ] Keep the data plane free of new synchronous dependencies in the request path. New control-plane state belongs in the roadmap design, not in a hidden global.

Guidelines:

- **Commits:** Conventional Commits (`feat(provider): ...`, `fix(security): ...`, `docs: ...`, `test: ...`, `chore: ...`). Use a scope that matches the area (`config`, `proxy`, `ratelimit`, `cache`, `auth`, `admin`, `metrics`, `deploy`, `docs`).
- **Formatting:** `gofmt`/`goimports`; the linter enforces both.
- **Errors:** wrap with `%w`; keep provider errors bounded and avoid echoing secrets.
- **Logs:** never log raw `Authorization`, API keys or prompt/response bodies by default.
- **Config:** new fields need validation in `internal/config/config.go`, a default where appropriate, and a hot-reload decision (can it change without a restart?).
- **Metrics:** bound label cardinality; do not put model names or URLs in labels without a limit.

A maintainer reviews for correctness, contract compatibility and operational impact. There is no guaranteed review SLA.

## Adding a provider

The current wiring is explicit in `cmd/gateway/main.go:buildProviders`. A new adapter needs:

1. `internal/provider/<name>.go` implementing `Provider` (`Name`, `Send`, `SendStream`, `Models`, `HealthCheck`) and, if applicable, `Embedder`.
2. Translation functions in `internal/translate/` if the provider is not OpenAI-shaped.
3. Registration in `buildProviders` and examples in `config.yaml` / a profile.
4. Tests with `httptest`: success, upstream 429/5xx, malformed body, timeout, streaming, usage.
5. A contract fixture that proves the OpenAI-shaped response and error mapping.
6. Docs: README provider table, `docs/ARCHITECTURE.md`, and a note about unsupported OpenAI fields (for example `logprobs` or `seed`).

A generic OpenAI-compatible adapter and capability metadata are planned (roadmap R-18/R-15); until then, avoid adding provider-specific branches to the core handler.

## Security

Do not open a public issue for a vulnerability. Use the repository's **private security advisory** flow described in [`SECURITY.md`](SECURITY.md). Do not include secrets in a PR, an issue, a log excerpt or a test fixture. If you need to test a secret-handling path, use a dummy value and assert that it is redacted.

## License and sign-off

The project is MIT (`LICENSE`). By contributing you agree to license your contribution under the same terms. Use the Developer Certificate of Origin sign-off:

```bash
git commit -s -m "feat(proxy): ..."
```

This adds `Signed-off-by: Your Name <you@example.com>` to the commit. Use your own identity; do not invent someone else's.

## Release (maintainers)

1. Move relevant `Unreleased` entries into a version section in `CHANGELOG.md` with the release date.
2. Update `VERSION` to the same version (SemVer).
3. Run the full suite and the contract tests; validate `api/openapi.yaml`.
4. Tag `vX.Y.Z`; `.github/workflows/release.yaml` builds and publishes.
5. Do not tag with unverified benchmark or coverage claims in the notes.

## Getting help

Open an issue with the command you ran, the expected vs actual result, the Go version, and a redacted config. For security, use the private advisory. There is no support email.
