#!/usr/bin/env bash
# Run the official OpenAI SDKs against the real gateway and a fake upstream.
# Pins both SDK versions so a released SDK change cannot silently redefine the
# contract; updating a pin is a deliberate review.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GATEWAY_BIN="${GATEWAY_BIN:-$ROOT/bin/gateway}"
PYTHON="${PYTHON:-python3}"
PYTHON_SDK_VERSION="${PYTHON_SDK_VERSION:-3.51.0}"
NODE_SDK_VERSION="${NODE_SDK_VERSION:-7.57.0}"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/llmgw-conformance.XXXXXX")"

if [[ ! -x "$GATEWAY_BIN" ]]; then
  echo "gateway binary not found at $GATEWAY_BIN; run 'make build' first" >&2
  exit 1
fi

UPSTREAM_PID=""
GATEWAY_PID=""
cleanup() {
  [[ -n "$GATEWAY_PID" ]] && kill "$GATEWAY_PID" 2>/dev/null || true
  [[ -n "$UPSTREAM_PID" ]] && kill "$UPSTREAM_PID" 2>/dev/null || true
  [[ -n "$GATEWAY_PID" ]] && wait "$GATEWAY_PID" 2>/dev/null || true
  [[ -n "$UPSTREAM_PID" ]] && wait "$UPSTREAM_PID" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

echo "conformance: starting fake upstream"
"$PYTHON" "$ROOT/tests/conformance/fake_upstream.py" --port 18081 >"$TMP/upstream.log" 2>&1 &
UPSTREAM_PID=$!

echo "conformance: starting gateway"
"$GATEWAY_BIN" -config "$ROOT/tests/conformance/config.yaml" >"$TMP/gateway.log" 2>&1 &
GATEWAY_PID=$!

for _ in $(seq 1 100); do
  if curl -fsS http://127.0.0.1:18080/health >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$GATEWAY_PID" 2>/dev/null; then
    echo "gateway exited during startup:" >&2
    cat "$TMP/gateway.log" >&2
    exit 1
  fi
  sleep 0.1
done
if ! curl -fsS http://127.0.0.1:18080/health >/dev/null 2>&1; then
  echo "gateway did not become healthy:" >&2
  cat "$TMP/gateway.log" >&2
  exit 1
fi

echo "conformance: installing openai==$PYTHON_SDK_VERSION (Python)"
"$PYTHON" -m venv "$TMP/venv"
"$TMP/venv/bin/pip" install --quiet --disable-pip-version-check "openai==$PYTHON_SDK_VERSION"

echo "conformance: installing openai@$NODE_SDK_VERSION (Node)"
mkdir -p "$TMP/node"
npm install --silent --no-audit --no-fund --prefix "$TMP/node" "openai@$NODE_SDK_VERSION"
cp "$ROOT/tests/conformance/node_sdk_test.mjs" "$TMP/node/"

run_test() {
  local label="$1"
  shift
  if ! "$@"; then
    echo "$label failed; gateway log:" >&2
    cat "$TMP/gateway.log" >&2
    exit 1
  fi
}

run_test "python SDK conformance" "$TMP/venv/bin/python" "$ROOT/tests/conformance/python_sdk_test.py"
run_test "node SDK conformance" node "$TMP/node/node_sdk_test.mjs"

echo "conformance: OK"
