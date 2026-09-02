#!/usr/bin/env bash
set -euo pipefail
GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
API_KEY="${GATEWAY_API_KEY:-local-dev}"
MODEL="${MODEL:-text-embedding-3-small}"
curl -s "$GATEWAY_URL/v1/embeddings" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $API_KEY" \
  -d "{\"model\":\"$MODEL\",\"input\":\"hello world\"}" | jq .
# also array input:
curl -s "$GATEWAY_URL/v1/embeddings" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $API_KEY" \
  -d "{\"model\":\"$MODEL\",\"input\":[\"hello\",\"world\"]}" | jq .
