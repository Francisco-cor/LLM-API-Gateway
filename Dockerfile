FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o gateway ./cmd/gateway

FROM alpine:3.21
RUN apk --no-cache add ca-certificates wget
WORKDIR /app
RUN addgroup -S appgroup && adduser -S appuser -G appgroup
COPY --from=builder /app/gateway ./gateway
COPY config.yaml ./config.yaml
RUN chown appuser:appgroup ./gateway ./config.yaml
USER appuser
EXPOSE 8080 8081 6060
HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD wget -qO- http://localhost:8080/health || exit 1
ENTRYPOINT ["./gateway", "-config", "config.yaml"]
