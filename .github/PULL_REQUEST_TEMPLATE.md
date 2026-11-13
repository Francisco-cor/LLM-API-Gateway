## What and why

<!-- Describe the problem and the change. Link the issue/roadmap item if there is one. -->

## Type of change

- [ ] Bug fix
- [ ] New feature / provider
- [ ] Breaking change (contract, config, admin API)
- [ ] Documentation / examples only
- [ ] Refactor / performance (no behavior change)
- [ ] CI / deployment

## Verification

<!-- Paste the commands you ran and the relevant result. Do not paste secrets. -->

- [ ] `go test ./... -race`
- [ ] `go vet ./...`
- [ ] `golangci-lint run ./...` (if installed)
- [ ] `go test ./tests/contract -run TestContract -v`
- [ ] `python3 -c "import yaml; yaml.safe_load(open('api/openapi.yaml'))"` (if the contract changed)
- [ ] Manual check against a real or fake provider (describe it)

## Compatibility and operations

- [ ] Config changes are validated in `internal/config/config.go` and documented.
- [ ] I decided whether the change is hot-reloadable; if not, it is documented.
- [ ] New metrics have bounded label cardinality.
- [ ] Logs do not contain Authorization headers, API keys, prompts or responses.
- [ ] `CHANGELOG.md` updated for user-visible behavior.
- [ ] Docs updated (`README.md`, `docs/ARCHITECTURE.md`, `docs/RUNBOOK.md` as applicable).
- [ ] No unverified benchmark, coverage or “production-ready” claim was added.

## Risk

<!-- What can break? How would you roll it back? Any migration or restart required? -->

## Checklist

- [ ] No secrets, `.env`, `coverage.out`, profiles or binaries committed.
- [ ] Commits are signed off (`git commit -s`) and use Conventional Commits.
- [ ] I have read `CONTRIBUTING.md` and `SECURITY.md`.
