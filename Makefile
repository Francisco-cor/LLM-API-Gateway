.PHONY: build run test lint docker docker-run clean

build:
	go build -o bin/gateway ./cmd/gateway

run: build
	./bin/gateway -config config.yaml

test:
	go test ./... -v -cover

lint:
	golangci-lint run

docker:
	docker build -t llm-api-gateway .

docker-run: docker
	docker-compose up -d

clean:
	rm -rf bin/
