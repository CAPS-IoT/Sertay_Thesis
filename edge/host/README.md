# SIF Edge Host

`sif-edge-host` is the remote Wasm runtime for SIF-Wasm. It is a Go HTTP
service using `wasmtime-go`. It maintains active and staged release slots,
implements the same custom `env` ABI as the ESP32, and binds each invocation to
the active function identity and release generation.

The host runs in a standard Linux container; it is not a Kubernetes Wasm CRI
workload. See [System Design](../../technical/01-system-design.md) for its role
in the continuum.

## Environment

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | HTTP listen port. |
| `WASM_PATH` | `dht_reader.wasm` | Stable active artifact slot. |
| `FUNCTION_IDENTITY` | `dht-reader` | Startup compatibility identity. |
| `RELEASE_GENERATION` | `0` | Startup generation before operator staging. |

The operator normally stages and activates the desired positive generation
after the pod starts. The file name `dht_reader.wasm` is a compatibility slot;
the active identity is release metadata.

## HTTP API

| Method and path | Behavior |
|---|---|
| `GET /health` | Returns runtime and architecture for probes. |
| `POST /process` | Executes `process_event` only when request identity/generation match active state. |
| `GET /wasm` | Serves active bytes with `X-Wasm-Sha256` and `ETag`. |
| `HEAD /wasm` | Returns active digest metadata. |
| `PUT /wasm` | Hashes, compiles, validates, and stages a release. |
| `GET /release` | Reports active and optional staged generation/digest/identity. |
| `POST /release` | Activates the requested staged generation. |

### Invocation request

```json
{
  "function": "basic-edge-demo",
  "releaseGeneration": 1,
  "source": "manual-test",
  "resourceInputs": {}
}
```

Each resource input uses a typed shape:

```json
{
  "DHT": {
    "temperature": {"type": "f32", "value": 22.0},
    "humidity": {"type": "f32", "value": 50.0}
  },
  "BATTERY": {
    "percent": {"type": "i32", "value": 80}
  },
  "GPIO": {
    "buttonPressed": {"type": "bool", "value": false}
  }
}
```

The successful response includes:

```json
{
  "result": 0,
  "outputs": {},
  "timing": {"edgeExecutionMs": 1},
  "function": "basic-edge-demo",
  "releaseGeneration": 1,
  "artifactDigest": "<sha256>"
}
```

The ESP32 verifies the returned function, generation, and digest before applying
any declared output.

### Staging request

`PUT /wasm` requires these headers:

```text
X-Wasm-Sha256
X-SIF-Release-Generation
X-SIF-Function-Identity
X-SIF-Artifact-Caller
```

The handler streams to a temporary file, hashes the body, verifies the declared
digest, compiles and instantiates the module, checks for `process_event`, and
then installs the staged slot. It rejects stale generations and
same-generation conflicts. An exact active/staged duplicate is an idempotent
success.

`POST /release` accepts:

```json
{"releaseGeneration": 2}
```

Activation and invocation share one mutex, so the compiled module, file, and
release metadata change between invocations.

## Host API

The current guest ABI under module `env` is:

```text
get_resource_f32(resource, key) -> f32
get_resource_i32(resource, key) -> i32
get_resource_bool(resource, key) -> i32
set_output_i32(key, value)
set_output_f32(key, value)
log_message(ptr, len)
```

The actual Wasm signatures pass pointer/length pairs. `wasmBytes()` validates
negative values, overflow, memory existence, and range bounds before copying a
guest string. The host retains `get_temperature` and `get_humidity` only as
compatibility imports; current guests use the generic resource getters.

Request and output maps are process-global but execution is serialized. A new
wasmtime Store and Instance are created for every request; Engine, Module, and
Linker are reused for the active release.

## Build and test

```bash
cd edge/host
go test ./...
go build -o sif-edge-host .
```

For a local basic guest run:

```bash
FUNCTION_IDENTITY=basic-edge-demo \
RELEASE_GENERATION=1 \
WASM_PATH=dht_reader.wasm \
PORT=8080 \
go run .
```

In another shell:

```bash
curl -fsS http://127.0.0.1:8080/health
curl -fsS http://127.0.0.1:8080/release
curl -fsS -X POST http://127.0.0.1:8080/process \
  -H 'Content-Type: application/json' \
  -d '{"function":"basic-edge-demo","releaseGeneration":1,"source":"manual-test","resourceInputs":{}}'
```

Tests use localhost listeners and require loopback socket access.

## Container image

```bash
docker build -t <registry>/sif-edge-host:<tag> edge/host
```

The multi-stage image builds the Go binary and copies it with
`dht_reader.wasm` into `/app`. Use a unique tag or digest for a reproducible
operator rollout.

## Known limitations

- HTTP endpoints have no complete authentication or request-body limits.
- Guest execution has no fuel, epoch interruption, or timeout.
- Active/staged state is not persisted outside the container filesystem.
- Execution is serialized; the current implementation is not horizontally
  state-coordinated across replicas.
- Output values are represented as numeric `float32` values in the host
  response, even when a guest used the i32 setter.
