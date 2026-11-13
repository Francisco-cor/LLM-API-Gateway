#!/usr/bin/env bash
# examples/curl/chat.sh — non-stream chat
set -euo pipefail
GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
API_KEY="${GATEWAY_API_KEY:-local-dev}"
MODEL="${MODEL:-claude-sonnet-4-6}"
curl -s "$GATEWAY_URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $API_KEY" \
  -d "{
    \"model\": \"$MODEL\",
    \"messages\": [
      {\"role\": \"system\", \"content\": \"You are concise.\"},
      {\"role\": \"user\", \"content\": \"What is a token bucket?\"}
    ],
    \"max_tokens\": 64,
    \"temperature\": 0.2
  }" | jq .
