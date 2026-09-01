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
	go run ./cmd/gateway -config config.yaml

clean:
	rm -rf bin/ coverage.out
