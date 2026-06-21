# SIF-Wasm Guests

This directory contains the Rust application logic shared by the ESP32 WAMR
host and the Go/wasmtime edge host. Both guests export
`process_event() -> i32`, use custom imports from module `env`, and are built as
`cdylib` modules for `wasm32-wasip1`.

The target name does not imply that the modules use WASI services. The current
guests use only the custom core-Wasm host API.

## Guests

| Crate | Release identity | Inputs/outputs | Purpose |
|---|---|---|---|
| [`basic_edge_demo/`](basic_edge_demo/) | `basic-edge-demo` | No inputs or outputs; logging only | Deterministic zero-input edge execution and embedded bootstrap. |
| [`hybrid_resource_demo/`](hybrid_resource_demo/) | `hybrid-resource-demo` | Seven typed input keys across five device resources; six numeric outputs | Resource-locality, hybrid execution, timing, and actuator demonstration. |

The stable deployment artifact is still named `dht_reader.wasm`; release
identity is carried separately in the generation-bound release tuple.

## Host API

Current imports from module `env` are:

```text
get_resource_f32(resource_ptr, resource_len, key_ptr, key_len) -> f32
get_resource_i32(resource_ptr, resource_len, key_ptr, key_len) -> i32
get_resource_bool(resource_ptr, resource_len, key_ptr, key_len) -> i32
set_output_i32(key_ptr, key_len, value)
set_output_f32(key_ptr, key_len, value)
log_message(message_ptr, message_len)
```

Guest return zero means execution succeeded. Application results cross output
setters. The ESP32 exposes only members declared by the active release contract
and only applies a declared `actuatorCommand:i32`. The edge host supplies values
from the invocation request.

## Resource contracts

`basic-edge-demo` uses:

```yaml
functionIdentity: basic-edge-demo
resourceContract:
  inputs: []
  outputs: []
```

`hybrid-resource-demo` declares:

| Resource | Typed keys | Locality |
|---|---|---|
| `BATTERY` | `percent:i32`, `voltageMv:i32` | device |
| `DHT` | `temperature:f32`, `humidity:f32` | device |
| `LIGHT` | `lux:f32` | device |
| `OCCUPANCY` | `distanceCm:f32` | device |
| `GPIO` | `buttonPressed:bool` | device |

Its outputs are `temperatureF:f32`, `heatIndexC:f32`,
`comfortScore:i32`, `occupied:i32`, `nextSampleSeconds:i32`, and the
device-local `actuatorCommand:i32`.

Because this contract contains device-local members, an edge proposal becomes
logical hybrid placement. The ESP32 collects inputs, the edge host computes,
and the ESP32 verifies release evidence before applying the actuator output.

## Memory contract

Both `.cargo/config.toml` files enforce:

```text
Rust stack:       4096 bytes
initial memory:  65536 bytes (one Wasm page)
maximum memory:  65536 bytes (one Wasm page)
```

Release profiles optimize for size, use LTO, strip output, and abort on panic.
The ESP32 additionally rejects Wasm bytecode larger than 8 KiB. Do not remove
the one-page linker settings without changing and revalidating the firmware
memory design.

## Build and inspect

Install the target once:

```bash
rustup target add wasm32-wasip1
```

Build from inside each guest directory so Cargo loads its local
`.cargo/config.toml` and applies the one-page linker settings:

```bash
(cd wasm_guests/basic_edge_demo && \
  cargo build --release --target wasm32-wasip1)
(cd wasm_guests/hybrid_resource_demo && \
  cargo build --release --target wasm32-wasip1)
```

Using `--manifest-path` from the repository root is not equivalent: Cargo does
not discover the guest-local linker configuration from that working directory.

Inspect the memory declaration:

```bash
wasm-objdump -x \
  wasm_guests/basic_edge_demo/target/wasm32-wasip1/release/basic_edge_demo.wasm \
  | sed -n '/Memory/,/Global/p'
```

The required output contains:

```text
memory[0] pages: initial=1 max=1
```

Run the pure hybrid computation tests:

```bash
cargo test --manifest-path wasm_guests/hybrid_resource_demo/Cargo.toml
```

## Demonstration behavior

`basic-edge-demo` sums squares from 1 through 32 and returns zero only when the
checksum is 11,440.

`hybrid-resource-demo` calculates Fahrenheit temperature, an NWS-style
two-stage heat index, comfort score, occupancy, next sampling interval, and
actuator choice. The current source sets
`DEMO_TEMPERATURE_OVERRIDE_C = 35.0` to exercise the red/high-heat branch. Set
it below zero to use the provided DHT temperature. The override must be
described as synthetic demonstration input in thesis measurements.

The current firmware providers are also partly demonstrative: the active
factory uses a 22 °C/50% DHT fallback, plus fixed light, occupancy, and button
values. Battery data comes from the gauge or declared simulation.

## Repository artifact copies

The workflows maintain these generated copies:

```text
edge/host/dht_reader.wasm                         stable host/source slot
wasm_guests/basic_edge_demo/basic_edge_demo.wasm
wasm_guests/basic_edge_demo/basic_edge_demo_wasm.h  ESP32 embedded fallback
wasm_guests/hybrid_resource_demo/hybrid_resource_demo.wasm
```

The copy in `edge/host` is whichever guest was most recently selected for the
deployment slot. Its file name must not be used as the logical identity.

## Publication scripts

Each guest has a `publish.sh` that:

1. builds the release module;
2. enforces the one-page memory gate;
3. updates generated repository copies;
4. computes SHA-256 over the raw module;
5. reads and increments the live `WasmFunction` release generation;
6. creates a new timestamped Kubernetes ConfigMap containing the artifact;
7. rolls the authoritative `wasm-source` Deployment; and
8. patches generation, digest, function identity, and complete contract in one
   operation.

These scripts change local generated files and live external systems. They are
not build-only commands. Defaults assume SSH host `edge-01`, namespace
`sertay`, and compatibility object `dht-reader`; override `SSH_HOST`,
`NAMESPACE`, and `FUNCTION` deliberately for another environment.

The cluster must already contain the `wasm-source` Deployment and Service.
Their role and the ConfigMap-backed update flow are documented under
[Cluster-internal Wasm source](../edge/README.md#cluster-internal-wasm-source).

Before publishing, confirm that:

- `wasm-objdump` and `jq` are installed;
- the selected cluster/context and authoritative artifact source are correct;
- the ESP32-reachable artifact server serves `edge/host/dht_reader.wasm` after
  the copy is updated; and
- the generated release contract matches every guest import/output used by the
  code.

The command for serving this slot to the ESP32, its role in release staging,
and the distinction from the invocation and container-registry tunnels are
documented under [Development network paths](../edge/README.md#development-network-paths).
Keep personal LAN addresses out of committed configuration and documentation.
