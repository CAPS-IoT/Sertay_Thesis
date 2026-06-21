# SIF Operator

`sif-operator` is the Kubernetes control plane for the thesis prototype. It
watches `WasmFunction` custom resources, deploys the edge Wasm host, tracks
battery telemetry from the ESP32, and publishes MQTT commands that move
execution between local ESP32/WAMR mode and edge/wasmtime mode.

This README is scoped to `edge/operator/`. For the full system workflow, use the
repository root `README.md`.

## What the Operator Manages

For each `WasmFunction`, the reconciler creates or updates:

- a Deployment running `sif-edge-host`,
- a Service exposing the host inside the cluster,
- status fields for endpoint, placement, battery, artifact digest, and commands,
- MQTT placement commands sent to the ESP32 control topic.

The operator also synchronizes Wasm artifacts. When `spec.device.operatorWasmURL`
is set, the operator probes that URL, compares the digest with the edge host
`/wasm` endpoint, and uploads the artifact to the host when needed. When local
placement is selected and `spec.device.reloadWasmURL` is set, the operator sends
a `reload` command so the ESP32 fetches the Wasm blob before rebooting into
local mode.

## Important Paths

```text
api/v1alpha1/                 WasmFunction API types
cmd/main.go                   manager entry point
internal/controller/          reconciler, artifact sync, MQTT publisher/telemetry
config/crd/                   generated CRD manifests
config/rbac/                  generated RBAC manifests
config/manager/               manager Deployment manifest
config/default/               default kustomize deployment bundle
config/samples/               example WasmFunction resource
test/                         Kubebuilder e2e test scaffolding
```

Keep `api/`, `config/`, `cmd/`, and `internal/` in git. Even the generated
`zz_generated.deepcopy.go` and `config/crd/bases/*.yaml` files are normal
Kubebuilder repository artifacts and should be committed. Do not commit
`bin/` or `dist/`; they are generated locally by `make`.

## Placement Policy

`spec.placement.mode` accepts:

- `auto`: use battery telemetry and hysteresis.
- `local`: request ESP32 execution unless the low-battery guardrail is hit.
- `edge`: request edge execution.

Battery behavior:

- `status.observedBatteryPercent` from MQTT telemetry is preferred.
- `spec.placement.batteryPercent` is only a manual fallback when telemetry is
  missing.
- `lowBatteryThreshold` forces edge placement at or below the threshold.
- `highBatteryThreshold` allows local placement at or above the threshold.
- Values inside the threshold band retain the previous desired placement.

## MQTT Configuration

The manager reads MQTT settings from environment variables:

```text
SIF_MQTT_BROKER     broker host:port, for example mqtt.caps-platform.de:1883
SIF_MQTT_USER       MQTT username
SIF_MQTT_TOKEN      MQTT password/JWT token
SIF_MQTT_CLIENT_ID  optional client id
```

The default manager manifest reads `SIF_MQTT_TOKEN` from the Kubernetes secret
`sif-mqtt` key `token`. Create it without printing the token:

```bash
read -s SIF_MQTT_TOKEN
kubectl -n sertay create secret generic sif-mqtt \
  --from-literal=token="${SIF_MQTT_TOKEN}" \
  --dry-run=client -o yaml | kubectl apply -f -
```

## Build and Test

```bash
make generate manifests fmt vet
make test
make build
```

Useful focused checks:

```bash
go test ./internal/controller/...
go test ./api/...
```

`make generate` refreshes `api/v1alpha1/zz_generated.deepcopy.go`.
`make manifests` refreshes CRD and RBAC YAML under `config/`.

## Build the Operator Image

For a local or directly reachable registry:

```bash
make docker-build IMG=localhost:30500/sif-operator:<tag>
make docker-push IMG=localhost:30500/sif-operator:<tag>
```

On macOS setups where Docker/buildx cannot reach a host SSH tunnel, use an OCI
layout and push with `crane` from the host network namespace:

```bash
export OPERATOR_TAG=<tag>
docker buildx build \
  --platform linux/amd64 \
  --output type=oci,dest=/tmp/sif-operator-oci.tar \
  .
rm -rf /tmp/sif-operator-oci
mkdir -p /tmp/sif-operator-oci
tar -xf /tmp/sif-operator-oci.tar -C /tmp/sif-operator-oci
crane push --insecure /tmp/sif-operator-oci 127.0.0.1:30500/sif-operator:${OPERATOR_TAG}
crane digest --insecure 127.0.0.1:30500/sif-operator:${OPERATOR_TAG}
```

Use unique image tags for thesis runs. Avoid relying on `latest` for repeatable
operator rollouts.

## Deploy

Install the CRD:

```bash
make install
```

Deploy the manager:

```bash
make deploy IMG=localhost:30500/sif-operator:<tag>
kubectl rollout status deploy/sif-operator -n sertay --timeout=240s
kubectl logs -n sertay deploy/sif-operator --tail=80
```

If your kustomize namespace differs from `sertay`, either patch the manifests or
pass the namespace consistently in your deployment workflow.

## Example WasmFunction

```yaml
apiVersion: edge.sif.2iot.2de/v1alpha1
kind: WasmFunction
metadata:
  name: dht-reader
  namespace: sertay
spec:
  image: localhost:30500/sif-edge-host:<edge-host-tag>
  wasmPath: /app/dht_reader.wasm
  replicas: 1
  port: 8080
  device:
    id: esp32-dht
    controlTopic: "64/199/data"
    telemetryTopic: "64/199/data/telemetry"
    operatorWasmURL: "http://dht-reader.sertay:8080/wasm"
    reloadWasmURL: "http://<dev-machine-lan-ip>:8081/wasm"
  placement:
    mode: auto
    lowBatteryThreshold: 20
    highBatteryThreshold: 80
```

Apply the sample after setting the image tag and URLs:

```bash
kubectl apply -f config/samples/edge_v1alpha1_wasmfunction.yaml
kubectl get wasmfunction dht-reader -n sertay -o yaml
kubectl get pods,svc -n sertay
```

## Status Fields to Watch

```text
status.endpoint
status.desiredPlacement
status.placementReason
status.observedBatteryPercent
status.observedMode
status.observedArtifactDigest
status.desiredArtifactDigest
status.hostArtifactDigest
status.lastCommandedPlacement
status.lastCommandedArtifactDigest
```

These fields are the fastest way to verify whether placement is blocked by
telemetry, artifact sync, MQTT publishing, or Kubernetes deployment state.

## Cleanup

```bash
kubectl delete -f config/samples/edge_v1alpha1_wasmfunction.yaml --ignore-not-found
make undeploy
make uninstall
```

`make undeploy` removes the manager resources. `make uninstall` removes the CRD.
