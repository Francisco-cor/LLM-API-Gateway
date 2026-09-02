#!/usr/bin/env bash
set -euo pipefail
GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
API_KEY="${GATEWAY_API_KEY:-local-dev}"
curl -s "$GATEWAY_URL/v1/models" -H "Authorization: Bearer $API_KEY" | jq .
curl -s "$GATEWAY_URL/health" | jq .
curl -s "$GATEWAY_URL/health/providers" -H "Authorization: Bearer $API_KEY" | jq .
curl -s "$GATEWAY_URL/metrics" | grep -E "gateway_requests_total|gateway_cache_hits" | head -20
echo "admin (needs ADMIN_API_KEY):"
curl -s http://localhost:8081/admin/config -H "Authorization: Bearer ${ADMIN_API_KEY:-dummy}" | jq '.admin.api_key' || echo "admin auth required"
