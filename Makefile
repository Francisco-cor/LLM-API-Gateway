.PHONY: build run test test-race test-integration cover lint lint-fix bench vulncheck sbom docker-build docker docker-run clean dev

GO ?= go
LINT_VERSION ?= v2.15.0

build:
	$(GO) build -trimpath -o bin/gateway ./cmd/gateway

run: build
	./bin/gateway -config config.yaml

test:
	$(GO) test ./... -count=1 -shuffle=on

test-race:
	$(GO) test ./... -count=1 -race

# Integration tests opt in with a `//go:build integration` tag and expect a
# Redis server at $REDIS_URL (defaults to the compose instance). With no tagged
# files this is equivalent to `test` plus the integration tag.
test-integration:
	REDIS_URL=$${REDIS_URL:-redis://127.0.0.1:6379/0} $(GO) test -tags=integration ./... -count=1 -race

cover:
	$(GO) test ./... -count=1 -race -coverpkg=./... -coverprofile=coverage.out
	$(GO) tool cover -func=coverage.out | tail -1

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found: install $(LINT_VERSION) (https://golangci-lint.run/welcome/install/)"; \
		exit 1; \
	}
	golangci-lint run --timeout 5m ./...

lint-fix:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found: install $(LINT_VERSION) (https://golangci-lint.run/welcome/install/)"; \
		exit 1; \
	}
	golangci-lint run --fix --timeout 5m ./...

bench:
	$(GO) test ./... -bench=. -benchmem

vulncheck:
	@command -v govulncheck >/dev/null 2>&1 || { \
		echo "govulncheck not found: go install golang.org/x/vuln/cmd/govulncheck@v1.1.4"; \
		exit 1; \
	}
	govulncheck ./...

sbom:
	@command -v syft >/dev/null 2>&1 || { \
		echo "syft not found: https://github.com/anchore/syft#installation"; \
		exit 1; \
	}
	syft dir:. -o spdx-json=sbom.spdx.json

docker-build:
	docker build -t llm-api-gateway:local .

docker: docker-build

docker-run: docker-build
	docker compose up -d

dev:
	@if command -v air >/dev/null 2>&1; then \
		air; \
	else \
		echo "air not found, falling back to go run (install: go install github.com/air-verse/air@latest)"; \
		go run ./cmd/gateway -config config.yaml; \
	fi

clean:
	rm -rf bin/ dist/ coverage.out coverage.html sbom.spdx.json tmp/
