#!/usr/bin/env bash
set -euo pipefail
GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
API_KEY="${GATEWAY_API_KEY:-local-dev}"
MODEL="${MODEL:-gpt-4o}"
curl -N -s "$GATEWAY_URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Accept: text/event-stream" \
  -d "{
    \"model\": \"$MODEL\",
    \"messages\": [{\"role\": \"user\", \"content\": \"Count 1 to 5\"}],
    \"stream\": true
  }"
echo
# expects data: {"choices":[{"delta":{"content":"1"}}]} ... data: [DONE]
