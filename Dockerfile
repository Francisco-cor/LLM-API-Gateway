# syntax=docker/dockerfile:1.7
# TODO(supply-chain): pin both base images by digest, e.g.
#   FROM golang:1.26-alpine@sha256:<digest> AS builder
# Digests are intentionally not invented here; resolve them with
#   docker buildx imagetools inspect golang:1.26-alpine
# and update this file (and Dependabot/digest automation) when pinning.

ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-alpine AS builder
WORKDIR /src
ENV CGO_ENABLED=0

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
# main.version/main.commit are not declared in the Go sources yet; -X is a
# no-op until they are (TODO go-owners). Build metadata still lands in the
# OCI labels below. -trimpath keeps build paths out of the binary.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -buildvcs=false \
      -ldflags="-s -w -buildid= -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/gateway ./cmd/gateway

FROM alpine:3.21
ARG VERSION=dev
ARG COMMIT=unknown
LABEL org.opencontainers.image.title="llm-api-gateway" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"

RUN apk --no-cache add ca-certificates
WORKDIR /app
RUN addgroup -S -g 65532 appgroup && adduser -S -u 65532 -G appgroup appuser
COPY --from=builder /out/gateway ./gateway
COPY config.yaml ./config.yaml
RUN chown appuser:appgroup ./gateway ./config.yaml

USER appuser
EXPOSE 8080 8081
# Readiness covers provider checks; /health is liveness-only and can stay green
# while the gateway is not ready to serve traffic.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/readyz || exit 1
ENTRYPOINT ["./gateway", "-config", "config.yaml"]
