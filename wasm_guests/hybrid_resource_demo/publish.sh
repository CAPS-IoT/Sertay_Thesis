#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="${NAMESPACE:-sertay}"
FUNCTION="${FUNCTION:-dht-reader}"
SSH_HOST="${SSH_HOST:-edge-01}"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
WASM="$HERE/target/wasm32-wasip1/release/hybrid_resource_demo.wasm"
CM_NAME="wasm-source-hybrid-$(date +%H%M%S)"

command -v wasm-objdump >/dev/null 2>&1 || { echo "ERROR: wasm-objdump not found"; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "ERROR: jq not found"; exit 1; }

echo "[publish] cargo build --release --target wasm32-wasip1"
(cd "$HERE" && cargo build --release --target wasm32-wasip1)

MEMORY_SECTION="$(wasm-objdump -x "$WASM" | sed -n '/Memory/,/Global/p')"
echo "$MEMORY_SECTION"
grep -q 'memory\[0\] pages: initial=1 max=1' <<<"$MEMORY_SECTION" || {
  echo "ERROR: generated Wasm exceeds the ESP32 one-page memory contract"
  exit 1
}

cp "$WASM" "$ROOT/edge/host/dht_reader.wasm"
cp "$WASM" "$HERE/hybrid_resource_demo.wasm"

DIGEST="$(shasum -a 256 "$WASM" | awk '{print $1}')"
CURRENT_GENERATION="$(ssh "$SSH_HOST" "kubectl -n ${NAMESPACE} get wasmfunction ${FUNCTION} -o jsonpath='{.spec.release.generation}'")"
CURRENT_GENERATION="${CURRENT_GENERATION:-0}"
NEXT_GENERATION="$((CURRENT_GENERATION + 1))"

PATCH="$(jq -cn \
  --arg digest "$DIGEST" \
  --argjson generation "$NEXT_GENERATION" \
  '{spec:{release:{generation:$generation,artifactDigest:$digest,functionIdentity:"hybrid-resource-demo",resourceContract:{inputs:[{name:"BATTERY",locality:"device",keys:[{name:"percent",type:"i32"},{name:"voltageMv",type:"i32"}]},{name:"DHT",locality:"device",keys:[{name:"temperature",type:"f32"},{name:"humidity",type:"f32"}]},{name:"LIGHT",locality:"device",keys:[{name:"lux",type:"f32"}]},{name:"OCCUPANCY",locality:"device",keys:[{name:"distanceCm",type:"f32"}]},{name:"GPIO",locality:"device",keys:[{name:"buttonPressed",type:"bool"}]}],outputs:[{name:"temperatureF",type:"f32",locality:"portable"},{name:"heatIndexC",type:"f32",locality:"portable"},{name:"comfortScore",type:"i32",locality:"portable"},{name:"occupied",type:"i32",locality:"portable"},{name:"nextSampleSeconds",type:"i32",locality:"portable"},{name:"actuatorCommand",type:"i32",locality:"device"}]}}}}')"

echo "[publish] updating authoritative Wasm source ${NAMESPACE}/${CM_NAME}"
ssh "$SSH_HOST" "kubectl -n ${NAMESPACE} create configmap ${CM_NAME} --from-file=wasm=/dev/stdin" < "$WASM"
ssh "$SSH_HOST" "kubectl -n ${NAMESPACE} patch deploy wasm-source --type=json -p='[{\"op\":\"replace\",\"path\":\"/spec/template/spec/volumes/0/configMap/name\",\"value\":\"${CM_NAME}\"}]'"
ssh "$SSH_HOST" "kubectl -n ${NAMESPACE} rollout status deploy/wasm-source --timeout=120s"
printf '%s' "$PATCH" | ssh "$SSH_HOST" "kubectl -n ${NAMESPACE} patch wasmfunction ${FUNCTION} --type=merge --patch-file=/dev/stdin"

echo "Published hybrid-resource-demo release generation=${NEXT_GENERATION} sha256=${DIGEST}"
