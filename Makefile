.PHONY: build run test test-race test-cover lint lint-fix bench vulncheck docker docker-run clean dev

build:
	go build -o bin/gateway ./cmd/gateway

run: build
	./bin/gateway -config config.yaml

test:
	go test ./... -v -cover

test-race:
	go test ./... -race -coverprofile=coverage.out -v

test-cover: test-race
	go tool cover -html=coverage.out

lint:
	golangci-lint run ./...

lint-fix:
	golangci-lint run --fix ./...

bench:
	go test ./... -bench=. -benchmem

vulncheck:
	govulncheck ./...

docker:
	docker build -t llm-api-gateway .

docker-run: docker
	docker-compose up -d

dev:
	@if command -v air >/dev/null 2>&1; then \
		air; \
	else \
		echo "air not found, falling back to go run (install: go install github.com/air-verse/air@latest)"; \
		go run ./cmd/gateway -config config.yaml; \
	fi

clean:
	rm -rf bin/ coverage.out tmp/
