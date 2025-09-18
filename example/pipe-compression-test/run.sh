#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")"/../../ && pwd)"
TEST_DIR="$ROOT/example/pipe-compression-test"
BIN_DIR="$TEST_DIR/bin"
RELAY_PORT="${RELAY_PORT:-29998}"
ECHO_PORT="${ECHO_PORT:-3333}"
CLIENT_PORT="${CLIENT_PORT:-9999}"
STATS_PORT="${STATS_PORT:-9998}"
RELAY_REDIS_URL="${REDIS_URL:-redis://127.0.0.1:6379}"
REDIS_CONTAINER_NAME="${REDIS_CONTAINER_NAME:-stream-relay-pipe-test-redis}"

mkdir -p "$BIN_DIR"

echo "[build] compiling echo server"
cd "$TEST_DIR"
go build -o "$BIN_DIR/echo-server" echo-server.go

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
  if [[ -n "${CLIENT_PID:-}" ]]; then kill "$CLIENT_PID" >/dev/null 2>&1 || true; fi
  if [[ -n "${RELAY_PID:-}" ]]; then kill "$RELAY_PID" >/dev/null 2>&1 || true; fi
  if [[ -n "${ECHO_PID:-}" ]]; then kill "$ECHO_PID" >/dev/null 2>&1 || true; fi
  if [[ -n "${REDIS_STARTED:-}" ]]; then docker stop "$REDIS_CONTAINER_NAME" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT INT TERM

ensure_redis; REDIS_STARTED=1

echo "[run] starting echo server on :$ECHO_PORT"
PORT="$ECHO_PORT" "$BIN_DIR/echo-server" &
ECHO_PID=$!
sleep 1

echo "[run] starting relay on :$RELAY_PORT (Redis: $RELAY_REDIS_URL)"
ALLOWED_DOMAINS="127.0.0.1,localhost" PORT="$RELAY_PORT" REDIS_URL="$RELAY_REDIS_URL" "$BIN_DIR/relay" &
RELAY_PID=$!
sleep 1

echo "[run] starting client proxy on :$CLIENT_PORT (stats dashboard on :$STATS_PORT)"
LISTEN_PORT="$CLIENT_PORT" STATS_PORT="$STATS_PORT" SERVER_BASE="http://localhost:$RELAY_PORT" UPSTREAM_BASE="http://localhost:$ECHO_PORT" \
  PIPE_BODYDICT=1 PIPE_DICT_MIN_SIZE=1024 \
  node "$ROOT/client/client.js" &
CLIENT_PID=$!
sleep 1

echo ""
echo "========================================"
echo "Running BodyDict compression test"
echo "========================================"
echo ""
echo "Stats dashboard: http://127.0.0.1:$STATS_PORT"
echo ""

cd "$TEST_DIR"
PIPE_BODYDICT=1 PIPE_DICT_MIN_SIZE=1024 node test-compression.js "http://localhost:$RELAY_PORT" "http://localhost:$ECHO_PORT" "http://localhost:$CLIENT_PORT"
