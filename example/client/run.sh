#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")"/../.. && pwd)"
BIN_DIR="$ROOT/example/client/bin"
RELAY_PORT="${RELAY_PORT:-29999}"
CLIENT_PORT="${CLIENT_PORT:-9999}"
UPSTREAM_PORT="${UPSTREAM_PORT:-5000}"
UPSTREAM_BASE="http://127.0.0.1:${UPSTREAM_PORT}"
RELAY_REDIS_URL="${REDIS_URL:-redis://127.0.0.1:6379}"
REDIS_CONTAINER_NAME="${REDIS_CONTAINER_NAME:-stream-relay-example-redis}"

mkdir -p "$BIN_DIR"

echo "[build] compiling upstream SSE counter"
cd "$ROOT/example/client"
go build -o "$BIN_DIR/counter" ./counter.go

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
  if [[ -n "${COUNTER_PID:-}" ]]; then kill "$COUNTER_PID" >/dev/null 2>&1 || true; fi
  if [[ -n "${CLIENT_PID:-}" ]]; then kill "$CLIENT_PID" >/dev/null 2>&1 || true; fi
  if [[ -n "${REDIS_STARTED:-}" ]]; then docker stop "$REDIS_CONTAINER_NAME" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT INT TERM

ensure_redis; REDIS_STARTED=1

echo "[run] starting upstream counter on :$UPSTREAM_PORT"
PORT="$UPSTREAM_PORT" "$BIN_DIR/counter" &
COUNTER_PID=$!

echo "[run] starting relay on :$RELAY_PORT (Redis: $RELAY_REDIS_URL)"
ALLOWED_DOMAINS="127.0.0.1,localhost" PORT="$RELAY_PORT" REDIS_URL="$RELAY_REDIS_URL" "$BIN_DIR/relay" &
RELAY_PID=$!

echo "[run] starting local proxy on :$CLIENT_PORT -> $UPSTREAM_BASE via relay"
cd "$ROOT/client"
LISTEN_PORT="$CLIENT_PORT" SERVER_BASE="http://127.0.0.1:${RELAY_PORT}" UPSTREAM_BASE="$UPSTREAM_BASE" node client.js &
CLIENT_PID=$!

sleep 1
echo "[demo] curling the proxy stream..."
curl -N "http://127.0.0.1:${CLIENT_PORT}/sse" || true

wait "$COUNTER_PID"
