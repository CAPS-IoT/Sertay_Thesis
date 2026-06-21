# ESP32 SIF-Wasm Firmware

`SIF_wasm_dht` is the device tier of the SIF-Wasm continuum. It is an ESP-IDF
v5.4.1 application that keeps the ESP32 as the SIF event source while selecting
local WAMR execution or HTTP offload according to operator-controlled release
and placement state.

The generic `WasmFunction`, scheduler, dispatcher, and controls that pause new
invocations while admitted invocations finish come from the companion SIF
contribution in an external checkout. This directory contains the
thesis-specific application and runtime integration.

For the complete architecture and implementation rationale, see
[`technical/`](../technical/README.md). Current validation status is in
[`technical/03-traceability-and-status.md`](../technical/03-traceability-and-status.md).

## Implemented runtime

- `BasicWasmTrigger` emits a `BasicWasmEvent` every 15 seconds.
- SIF admission is checked before allocating a `wasmProcess` invocation.
- Local mode runs the active Wasm release through WAMR.
- Edge mode collects contract-declared inputs and calls the Go edge host.
- MQTT control and telemetry remain available in both modes.
- NVS stores active/staged release metadata, concrete runtime mode, persistent
  admission pause, battery/demo state, command IDs, and actuator state.
- SPIFFS stores active and staged Wasm bytes.
- A shared static worker serializes release download and activation.
- The RGB LED applies `actuatorCommand` and overlays deadline rejection.

Logical hybrid placement is implemented with concrete runtime mode `edge`:
device-local contract members are collected/applied by the firmware around the
remote guest call.

## Important files

```text
main/main.cpp                         boot and runtime selection
main/sif_migratingWasmFunction.cpp   local invocation telemetry/battery update
main/sif_httpForward.cpp             typed edge request and verified response
main/sif_release.cpp                 stage, drain, activate, pause, resume
main/sif_state.cpp                    NVS active/staged state
main/sif_wasmHostApi.cpp              release-constrained WAMR imports
main/sif_control.cpp                 MQTT command parser
main/sif_telemetry.cpp               state and invocation telemetry
main/sif_led.cpp                     steady actuator and blue overlay
CMakeLists.txt                       selects one SIF worker and the WAMR slot
components/wamr/core/shared/platform/esp-idf/espidf_memmap.c
                                     project-modified reusable slot allocator
partitions.csv                       2 MiB app + 512 KiB SPIFFS layout
sdkconfig.defaults                   WAMR, Wi-Fi, MQTT, I2C, and LED defaults
```

The active and staged artifact paths retain the compatibility name:

```text
/spiffs/dht_reader.wasm
/spiffs/dht_reader.staged.wasm
```

The active guest identity comes from release metadata, not from the file name.

## Prerequisites

- ESP-IDF v5.4.1.
- A SIF checkout containing the companion Wasm integration and per-function
  admission support. Set `SIF_BASE_DIR` to its repository root.
- The local WAMR submodule, initialized with
  `git submodule update --init SIF_wasm_dht/components/wamr`.
- A compatible ESP32 with 4 MiB flash for the checked-in partition table.
- The configured LC709203F-compatible battery-gauge path on I2C bus 0 if real
  battery telemetry is required.
- Wi-Fi and MQTT connectivity for control/telemetry and edge execution.

Do not commit Wi-Fi passwords, MQTT tokens, or machine-specific serial ports.
Use `idf.py menuconfig` or a private/local SDK configuration for secrets.

## Configure

```bash
source /path/to/esp-idf-v5.4.1/export.sh
export SIF_BASE_DIR=/path/to/SIF
idf.py -C SIF_wasm_dht menuconfig
```

The application menu configures:

- Wi-Fi SSID/password;
- MQTT broker, user, token, and data/control topic;
- edge-host `/process` URL;
- green/red/blue GPIOs and active-low behavior; and
- the optional labeled LED boot diagnostic.

The device-reachable artifact service and ESP32-to-edge invocation tunnel are
documented under [Development network paths](../edge/README.md#development-network-paths).

The checked-in physical demo defaults are common-anode/active-low:

| Channel | GPIO | Meaning |
|---|---:|---|
| Green | 17 | `actuatorCommand=1` |
| Red | 16 | `actuatorCommand=2` |
| Blue | 18 | temporary deadline-rejection overlay |

Set an LED GPIO to `-1` to disable that channel. Disable the boot diagnostic if
the target hardware should not pulse each channel during startup.

## Build, flash, and monitor

```bash
source /path/to/esp-idf-v5.4.1/export.sh
export SIF_BASE_DIR=/path/to/SIF
idf.py -C SIF_wasm_dht build
idf.py -C SIF_wasm_dht -p /dev/cu.usbserial-XXXX -b 460800 flash
idf.py -C SIF_wasm_dht -p /dev/cu.usbserial-XXXX monitor
```

Use the serial device and baud rate appropriate for the board. Building is a
local check; flashing and monitoring interact with physical hardware.

Expected boot evidence includes the battery-gauge reading, LED mapping, active
release state, selected runtime mode, MQTT topic, and either WAMR initialization
or the edge HTTP target.

## Runtime memory contract

| Element | Bound |
|---|---:|
| WAMR internal pool | 48 KiB static |
| Wasm linear memory | one reusable 64 KiB static page |
| WAMR stack / execution environment | 8 KiB |
| WAMR application heap | 0 |
| Wasm bytecode | maximum 8 KiB |
| SIF invocation workers | one worker on one core, 32 KiB stack |
| Release transition worker | 6 KiB static |

The firmware uses fast interpretation. AoT, libc-WASI, guest pthreads, shared
memory, and multi-module support are disabled. Guest publication must retain
the one-page initial/maximum memory declaration.

The firmware compile defines `WAMR_ESP_IDF_STATIC_LINEAR_MEMORY_SIZE=65536`.
The pinned WAMR ESP-IDF platform layer uses the resulting BSS buffer for an
exactly matching non-executable `os_mmap()` request, clears it before each use,
and releases it from `os_munmap()`. This single slot matches the one invocation
worker and avoids depending on a contiguous 64 KiB system-heap allocation after
Wi-Fi, MQTT, and HTTP activity. Other mapping sizes retain WAMR's upstream heap
path.

The firmware also defines `SIF_THREADPOOL_WORKER_CORE_COUNT=1`. Base SIF keeps
two worker cores as its default, but a second 32 KiB worker stack could not be
allocated alongside this firmware's fixed WAMR memory, networking, and control
tasks. One worker also matches the single reusable linear-memory slot and the
prototype's serialized local-invocation contract.

## Control-plane network readiness

The companion SIF Wi-Fi resource blocks wake-up until the station obtains an
IP address or exhausts its retry budget. MQTT therefore cannot begin DNS
resolution merely because a fixed connection timeout elapsed. Wi-Fi logs show
the SSID for diagnosis but redact the password.

## Release and command protocol

The operator uses the following actions:

| Action | Firmware behavior |
|---|---|
| `stage_release` | Validate generation/metadata, drain, download, hash, and persist staged state. |
| `activate_local` | Drain and promote the requested release in local WAMR mode. |
| `set_runtime_mode` | Drain and promote/select the requested release in edge mode. |
| `pause_function` | Persistently close scheduler admission. |
| `resume_function` | Reopen admission only for the exact active generation. |
| `set_thresholds` | Persist low/high battery thresholds. |
| `set_simulation` | Select real/simulated battery and drain/recovery steps. |
| `signal_deadline_rejection` | Run one decision-ID-deduplicated blue two-pulse overlay. |

`stage_release` requires a command ID, positive generation, device-reachable
artifact URL, lowercase SHA-256 digest, function identity, and resource
contract. Same-generation conflicts are rejected; exact duplicates are
idempotent.

The parser also retains `set_battery`, `set_battery_source`, and `set_drain` as
manual demonstration compatibility controls. They are not part of automatic
release activation.

## Telemetry acknowledgement

State telemetry is published to `<data-topic>/telemetry` with MQTT QoS 1. It
contains battery/source, concrete mode, admission pause, active/staged release
generation and digest, active identity, and optional per-invocation timing and
resource observations.

Release operations publish immediate state. The operator treats matching
telemetry as application acknowledgement; MQTT `PUBACK` alone is not
convergence.

## Current provider boundary

Battery values use the gauge or deterministic simulation. The current factory
does not bind a physical DHT resource and therefore uses the 22 °C/50% fallback.
Light (120 lux), occupancy (85 cm), and button (false) are fixed demo providers.
Only declared fields are exposed, but the contract cannot create a new driver
dynamically. Only `actuatorCommand:i32` has a device-side output effect.

## Host-buildable policy tests

From the repository root, compile the four tests with C++17 and include
`SIF_wasm_dht/main` plus
`${SIF_BASE_DIR}/sif_framework/sif_scheduler`:

```text
tools/tests/sif_contract_policy_test.cpp
tools/tests/sif_control_message_assembler_test.cpp
tools/tests/sif_function_admission_test.cpp
tools/tests/sif_led_policy_test.cpp
```

These tests validate pure policy and bounds logic. They do not replace ESP-IDF
tests for NVS, SPIFFS, HTTP, MQTT, GPIO, or crash recovery.

## Known firmware limitations

- File, WAMR-vector, and NVS promotion are ordered but not one atomic
  cross-subsystem transaction.
- There is no ESP-IDF-native interruption/recovery test matrix.
- WAMR has no guest instruction budget.
- String resources and arbitrary dynamic providers are not implemented.
- A completed non-200 edge response and nonzero remote guest result do not yet
  have the same SIF failure semantics as local execution.
