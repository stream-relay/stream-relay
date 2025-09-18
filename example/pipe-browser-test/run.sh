#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")"/../../ && pwd)"
TEST_DIR="$ROOT/example/pipe-browser-test"
BIN_DIR="$TEST_DIR/bin"
PUBLIC_DIR="$TEST_DIR/public"
RELAY_PORT="${RELAY_PORT:-29998}"
TEST_PORT="${TEST_PORT:-4001}"
RELAY_REDIS_URL="${REDIS_URL:-redis://127.0.0.1:6379}"
REDIS_CONTAINER_NAME="${REDIS_CONTAINER_NAME:-stream-relay-pipe-browser-test-redis}"

mkdir -p "$BIN_DIR" "$PUBLIC_DIR"

echo "[build] copying browser SDK pipe-client"
cp "$ROOT/browser-sdk/pipe-client.js" "$PUBLIC_DIR/pipe-client.js"

echo "[build] compiling test server"
cd "$TEST_DIR"
go build -o "$BIN_DIR/test-server" server.go

echo "[build] compiling relay server"
cd "$ROOT/server-go"
go build -o "$BIN_DIR/relay" .

ensure_redis() {
  if redis-cli -u "$RELAY_REDIS_URL" ping >/dev/null 2>&1; then
    echo "[redis] already reachable at $RELAY_REDIS_URL"
    return
  fi
  echo "[redis] starting docker redis container ($REDIS_CONTAINER_NAME)"
  if ! docker ps -a --format '{{.Names}}' | grep -q "^${REDIS_CONTAINER_NAME}\$"; then
    docker run -d --name "$REDIS_CONTAINER_NAME" -p 6379:6379 redis:7-alpine >/dev/null
  fi
  if ! docker ps --format '{{.Names}}' | grep -q "^${REDIS_CONTAINER_NAME}\$"; then
    docker start "$REDIS_CONTAINER_NAME" >/dev/null
  fi
  for i in {1..20}; do
    if redis-cli -u "$RELAY_REDIS_URL" ping >/dev/null 2>&1; then
      echo "[redis] ready at $RELAY_REDIS_URL"
      return
    fi
    sleep 0.5
  done
  echo "[redis] failed to start at $RELAY_REDIS_URL" >&2
  exit 1
}

cleanup() {
  if [[ -n "${RELAY_PID:-}" ]]; then kill "$RELAY_PID" >/dev/null 2>&1 || true; fi
  if [[ -n "${TEST_PID:-}" ]]; then kill "$TEST_PID" >/dev/null 2>&1 || true; fi
  if [[ -n "${REDIS_STARTED:-}" ]]; then docker stop "$REDIS_CONTAINER_NAME" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT INT TERM

ensure_redis; REDIS_STARTED=1

echo ""
echo "========================================"
echo "Pipe Browser BodyDict Compression Test"
echo "========================================"
echo ""
echo "[run] starting test server on :$TEST_PORT"
echo "[run] starting relay on :$RELAY_PORT (Redis: $RELAY_REDIS_URL)"
echo ""
echo "Open http://127.0.0.1:$TEST_PORT/ in your browser"
echo ""

ALLOWED_DOMAINS="127.0.0.1,localhost" PORT="$RELAY_PORT" REDIS_URL="$RELAY_REDIS_URL" "$BIN_DIR/relay" &
RELAY_PID=$!

PORT="$TEST_PORT" "$BIN_DIR/test-server" &
TEST_PID=$!

wait "$TEST_PID"
