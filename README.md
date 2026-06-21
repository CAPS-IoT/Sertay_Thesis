# Migrating Serverless Functions in the IoT-Edge Continuum

This repository contains the thesis prototype for migrating WebAssembly
functions across an IoT-edge continuum. The same Rust Wasm guest can run locally
on an ESP32 through WAMR or remotely on an edge Kubernetes node through a Go
wasmtime host. A Kubernetes control plane, `sif-operator`, selects the execution
location from battery telemetry and placement policy.

The original SIF framework is treated as external base infrastructure. This
repository contains only the thesis-specific implementation:

- `SIF_wasm_dht/`: ESP-IDF application for ESP32 local Wasm execution, HTTP
  offload, MQTT control, NVS migration state, and battery-aware mode switching.
- `wasm_guests/dht_reader/`: Rust guest compiled to `wasm32-wasip1`.
- `edge/host/`: Go HTTP host that runs the same Wasm guest with wasmtime.
- `edge/operator/`: Kubebuilder operator and `WasmFunction` CRD.
- `edge/k8s/`: Zot registry manifest for the edge cluster.

## Architecture

```text
ESP32 device tier                         Edge Kubernetes tier

SIF_wasm_dht/                             edge/
  SIF app + Scheduler                       WasmFunction CRD
  WAMR local runtime                        sif-operator
  host HAL: env.*                           Go / wasmtime host
  NVS migration state                       Zot registry
  HTTP + MQTT clients
        |                                             ^
        | HTTP /process offload                       |
        +---------------------------------------------+
        |                                             |
        +<-------------- MQTT control ----------------+

wasm_guests/dht_reader/
  one Rust Wasm guest used by both hosts
```

The guest imports a small HAL from Wasm module `env`:

- `get_temperature() -> f32`
- `get_humidity() -> f32`
- `log_message(ptr, len)`

The guest exports:

- `process_event() -> i32`

In local mode, ESP32/WAMR provides the HAL through SIF resources. In edge mode,
the ESP32 forwards the event payload to `POST /process`, and the Go host
provides the same HAL from HTTP request data.

## Prerequisites

- ESP-IDF v5.4.1.
- A compatible external SIF base framework and ESP-IDF component layout expected
  by `SIF_wasm_dht/CMakeLists.txt`.
- Rust with the `wasm32-wasip1` target.
- Go.
- Docker/buildx.
- `kubectl` access to the edge Kubernetes node.
- `crane` or `oras` for registry publishing.
- MQTT credentials stored outside the repository.

Do not commit WiFi passwords, MQTT tokens, kubeconfigs, local `sdkconfig`, or
other secrets.

## Repository Hygiene

Commit source, manifests, module files, and lock files. Do not commit generated
build outputs or local configuration:

```gitignore
SIF_wasm_dht/build/
SIF_wasm_dht/build_idf54/
SIF_wasm_dht/managed_components/
SIF_wasm_dht/sdkconfig
SIF_wasm_dht/sdkconfig.old
wasm_guests/**/target/
edge/operator/bin/
edge/host/sif-edge-host
*.kubeconfig
.env
*.log
.DS_Store
```

`SIF_wasm_dht/sdkconfig.defaults` is the shared firmware baseline. Use
`idf.py menuconfig` or a private local `sdkconfig` for machine-specific WiFi,
MQTT, edge URL, and reload URL values.

## Build the Wasm Guest

```bash
cd wasm_guests/dht_reader
rustup target add wasm32-wasip1
cargo build --release --target wasm32-wasip1
```

The compiled module is:

```text
wasm_guests/dht_reader/target/wasm32-wasip1/release/dht_reader.wasm
```

Use the Rust source as the authoritative guest. Before demos or releases,
refresh the copied artifacts from the current build:

```bash
cd wasm_guests/dht_reader
cp target/wasm32-wasip1/release/dht_reader.wasm ../../edge/host/dht_reader.wasm
xxd -i -n dht_reader_wasm target/wasm32-wasip1/release/dht_reader.wasm > dht_reader_wasm.h
```

`edge/host/dht_reader.wasm` is used by the Go host and Docker image.
`dht_reader_wasm.h` is the ESP32 embedded fallback when SPIFFS does not already
contain a downloaded Wasm module.

## Build and Run the Edge Host

```bash
cd edge/host
go test ./...
go build .
PORT=8080 WASM_PATH=dht_reader.wasm go run .
```

Check the host:

```bash
curl -sS http://localhost:8080/health

curl -sS -X POST http://localhost:8080/process \
  -H 'Content-Type: application/json' \
  -d '{"function":"WasmDhtProcess","source":"manual","temperature":22,"humidity":50}'
```

A successful `/process` request returns JSON with `result: 0`. The host also
serves the active artifact through `/wasm`:

```bash
curl -fsSI http://localhost:8080/wasm
curl -fsS http://localhost:8080/wasm -o /tmp/dht_reader.wasm
```

## Build the ESP32 Firmware

Load ESP-IDF, configure local values, then build:

```bash
source /path/to/esp-idf-v5.4.1/export.sh
cd SIF_wasm_dht
idf.py set-target esp32
idf.py menuconfig
idf.py build
```

Important menuconfig values are under `SIF Wasm DHT - Migration Config`:

- `WIFI_SSID`
- `WIFI_PASS`
- `MQTT_BROKER`
- `MQTT_USER`
- `MQTT_TOKEN`
- `DATA_TOPIC`
- `EDGE_HOST_URL`
- `WASM_PULL_URL`

Flash and monitor with your board's serial port:

```bash
idf.py -p /dev/cu.usbserial-XXXX -b 460800 flash monitor
```

Local mode initializes WAMR and runs `process_event()` on the ESP32. Edge mode
does not initialize WAMR; it starts WiFi/MQTT and forwards events to the edge
host.

## Deploy the Edge Registry

The edge manifests assume namespace `sertay` and a Zot NodePort registry on
`30500`.

```bash
kubectl create namespace sertay --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f edge/k8s/registry.yaml
kubectl get deployment,service,pod -n sertay -l app=zot
```

If the registry is reachable only from the edge node, open an SSH tunnel from
the development machine before pushing images:

```bash
ssh -N -L 127.0.0.1:30500:127.0.0.1:30500 edge-01
```

## Build and Publish the Edge Host Image

```bash
cd edge/host
export EDGE_HOST_TAG=<tag>
docker buildx build \
  --platform linux/amd64 \
  -t localhost:30500/sif-edge-host:${EDGE_HOST_TAG} \
  --output type=oci,dest=/tmp/sif-edge-host-oci.tar \
  .
rm -rf /tmp/sif-edge-host-oci
mkdir -p /tmp/sif-edge-host-oci
tar -xf /tmp/sif-edge-host-oci.tar -C /tmp/sif-edge-host-oci
crane push --insecure /tmp/sif-edge-host-oci 127.0.0.1:30500/sif-edge-host:${EDGE_HOST_TAG}
crane digest --insecure 127.0.0.1:30500/sif-edge-host:${EDGE_HOST_TAG}
```

Use immutable tags for repeatable rollouts. Avoid relying on `latest` for the
thesis demo.

## Build and Deploy the Operator

```bash
cd edge/operator
make generate manifests fmt vet
make build
```

Publish the operator image:

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

Create the MQTT token secret without printing the token:

```bash
read -s SIF_MQTT_TOKEN
kubectl -n sertay create secret generic sif-mqtt \
  --from-literal=token="${SIF_MQTT_TOKEN}" \
  --dry-run=client -o yaml | kubectl apply -f -
```

Deploy the operator with the published image:

```bash
cd edge/operator
make deploy IMG=localhost:30500/sif-operator:${OPERATOR_TAG}
kubectl rollout status deploy/sif-operator -n sertay --timeout=240s
kubectl logs -n sertay deploy/sif-operator --tail=80
```

## Apply a WasmFunction

Edit `edge/operator/config/samples/edge_v1alpha1_wasmfunction.yaml` so it points
to the current edge host image and device topics:

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

Apply and inspect status:

```bash
kubectl apply -f edge/operator/config/samples/edge_v1alpha1_wasmfunction.yaml
kubectl get wasmfunction dht-reader -n sertay -o yaml
kubectl get pods,svc -n sertay
```

The operator writes placement state to `status.desiredPlacement`,
`status.placementReason`, `status.observedBatteryPercent`, and command-tracking
fields.

## Runtime Behavior

- Low battery forces edge placement.
- High battery allows local placement.
- `auto` mode uses hysteresis and live MQTT telemetry.
- The operator publishes `set_thresholds`, `set_mode`, or `reload` commands.
- The ESP32 reboots after mode-changing commands so WAMR and networking get a
  clean memory layout.
- In local mode, the ESP32 runs the Wasm guest through WAMR.
- In edge mode, the ESP32 sends events to the Go host over HTTP and keeps the
  MQTT control channel open.

## Useful Checks

```bash
kubectl get all -n sertay
kubectl logs -n sertay deploy/sif-operator --tail=80
kubectl logs -n sertay -l app.kubernetes.io/instance=dht-reader --tail=80
kubectl get events -n sertay --sort-by=.lastTimestamp
```

For the ESP32 monitor:

- Local mode should show WAMR initialization, host API registration, and guest
  logs.
- Edge mode should show WiFi connection, HTTP offload, MQTT control
  registration, and telemetry publication.
- Reboots after placement changes are expected.

## Control Commands

The ESP32 command handler accepts JSON on the configured control topic:

```json
{"action":"set_thresholds","low":20,"high":80}
{"action":"set_mode","value":"edge"}
{"action":"set_mode","value":"local"}
{"action":"reload","url":"http://<host>:8081/wasm"}
{"action":"set_battery","value":50}
{"action":"set_battery_source","value":"real"}
{"action":"set_simulation","enabled":true,"drain":10,"recover":20}
```

The device must have MQTT connected to receive live commands.
