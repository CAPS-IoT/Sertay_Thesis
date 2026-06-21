#!/usr/bin/env bash
# Build dht_reader.wasm and push it as an OCI artifact to the edge zot
# registry. Prints the resulting blob digest — that is the URL fragment
# the ESP32 will GET (zot serves blobs via /v2/<repo>/blobs/<digest>).
#
# Usage: ./publish.sh [tag]            (default tag: v1)
#   REGISTRY=host:port  override registry endpoint (default 172.24.65.48:30500)
set -euo pipefail

TAG="${1:-v1}"
REGISTRY="${REGISTRY:-192.168.178.86:5050}"
REPO="dht_reader"
HERE="$(cd "$(dirname "$0")" && pwd)"

echo "[publish] cargo build --release --target wasm32-wasip1"
( cd "$HERE" && cargo build --release --target wasm32-wasip1 )

WASM="$HERE/target/wasm32-wasip1/release/dht_reader.wasm"
[[ -f "$WASM" ]] || { echo "ERROR: $WASM not found"; exit 1; }

SIZE=$(wc -c <"$WASM" | tr -d ' ')
DIGEST="sha256:$(shasum -a 256 "$WASM" | awk '{print $1}')"
echo "[publish] artifact size=${SIZE}  digest=${DIGEST}"

if ! command -v oras >/dev/null 2>&1; then
  echo "ERROR: oras CLI not found. Install: https://oras.land/docs/installation"
  exit 1
fi

echo "[publish] oras push --plain-http ${REGISTRY}/${REPO}:${TAG}"
( cd "$(dirname "$WASM")" && \
  oras push --plain-http \
    "${REGISTRY}/${REPO}:${TAG}" \
    "$(basename "$WASM"):application/wasm" \
    --artifact-type "application/vnd.wasm.module.v1+wasm" )

echo
echo "==== PUBLISHED ===="
echo "Tag:    ${REGISTRY}/${REPO}:${TAG}"
echo "Digest: ${DIGEST}"
echo "Blob URL (for ESP32):"
echo "  http://${REGISTRY}/v2/${REPO}/blobs/${DIGEST}"
