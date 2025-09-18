#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")"/.. && pwd)"
NAMESPACE="stream-relay"
CONFIG_FILE="$ROOT_DIR/config.json"
IMAGE_TAG="${1:-$(git -C "$ROOT_DIR" rev-parse --short HEAD 2>/dev/null || date +%s)}"
IMAGE_NAME="stream-relay/server:${IMAGE_TAG}"

echo "[deploy] Building Go server image ${IMAGE_NAME}"
docker build -t "${IMAGE_NAME}" -f "$ROOT_DIR/server-go/Dockerfile" "$ROOT_DIR/server-go"

echo "[deploy] Importing image into containerd"
docker save "${IMAGE_NAME}" | k3s ctr images import -

echo "[deploy] Applying base Kubernetes manifests"
kubectl apply -f "$ROOT_DIR/k8s/stream-relay.yaml"

if [[ -f "$CONFIG_FILE" ]]; then
  echo "[deploy] Syncing config.json into ConfigMap"
  kubectl create configmap stream-relay-config \
    --from-file=config.json="$CONFIG_FILE" \
    --namespace "$NAMESPACE" \
    --dry-run=client -o yaml | kubectl apply -f -
else
  echo "[deploy] config.json not found, applying empty config"
  kubectl create configmap stream-relay-config \
    --from-literal=config.json="{}" \
    --namespace "$NAMESPACE" \
    --dry-run=client -o yaml | kubectl apply -f -
fi

echo "[deploy] Updating deployment image"
kubectl -n "$NAMESPACE" set image deployment/stream-relay-server server="${IMAGE_NAME}"

echo "[deploy] Waiting for rollout"
kubectl -n "$NAMESPACE" rollout status deployment/stream-relay-server

echo "[deploy] Done"