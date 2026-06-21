# Implementation

## 1. Source map

This chapter maps the design to this repository and the companion SIF
contribution supplied through `SIF_BASE_DIR`. Validation status is kept
separately in [Traceability and Status](03-traceability-and-status.md).

| Area | Primary source |
|---|---|
| ESP32 application | [`SIF_wasm_dht/main/`](../SIF_wasm_dht/main/) |
| Companion SIF integration | `${SIF_BASE_DIR}/sif_framework/` |
| WAMR integration | `${SIF_BASE_DIR}/sif_framework/sif_function/sif_wasmFunction.cpp`, [`sif_wasmHostApi.cpp`](../SIF_wasm_dht/main/sif_wasmHostApi.cpp), firmware [`CMakeLists.txt`](../SIF_wasm_dht/CMakeLists.txt), and forked [`espidf_memmap.c`](../SIF_wasm_dht/components/wamr/core/shared/platform/esp-idf/espidf_memmap.c) |
| Device release lifecycle | [`sif_release.cpp`](../SIF_wasm_dht/main/sif_release.cpp), [`sif_state.cpp`](../SIF_wasm_dht/main/sif_state.cpp) |
| Device HTTP data plane | [`sif_httpForward.cpp`](../SIF_wasm_dht/main/sif_httpForward.cpp) |
| Device MQTT and telemetry | [`sif_control.cpp`](../SIF_wasm_dht/main/sif_control.cpp), [`sif_telemetry.cpp`](../SIF_wasm_dht/main/sif_telemetry.cpp) |
| Edge execution host | [`edge/host/`](../edge/host/) |
| Kubernetes API and operator | [`edge/operator/`](../edge/operator/) |
| Rust guests | [`wasm_guests/`](../wasm_guests/) |
| Host-buildable firmware policies | [`tools/tests/`](../tools/tests/) |

## 2. Firmware boot and runtime selection

[`app_main()`](../SIF_wasm_dht/main/main.cpp) performs the following sequence:

1. initialize SIF and the RGB LED;
2. initialize and wake the LC709203F battery gauge;
3. mount SPIFFS;
4. initialize power-on defaults or load preserved NVS state;
5. create a generation-zero bootstrap release when no active identity exists;
6. compile the active resource contract and restore actuator ownership; and
7. start either the local WAMR subscriber or the edge HTTP subscriber.

The power-on defaults select local mode, thresholds 20/80, the real battery
source, and zero simulation steps. Software resets preserve the selected mode,
release, battery simulation, thresholds, and actuator command.

The bootstrap artifact comes from
[`basic_edge_demo_wasm.h`](../wasm_guests/basic_edge_demo/basic_edge_demo_wasm.h)
when `/spiffs/dht_reader.wasm` does not exist. Managed releases use positive
generations from `WasmFunction.spec.release`.

### 2.1 Generic SIF names

The firmware no longer exposes the former DHT-specific event/function names.
The active path uses:

```text
EventType::wasmTimer       -> "wasmTimer"
FunctionType::wasmProcess -> "wasmProcess"
BasicWasmTrigger
BasicWasmEvent
```

The Kubernetes object, Service, SPIFFS path, and edge-host artifact slot remain
`dht-reader`/`dht_reader.wasm` for deployment compatibility. The subscriber's
runtime name is updated from the active release identity.

### 2.2 Local and edge boot paths

Local mode initializes WAMR and registers `MigratingWasmFunction`. Edge mode
does not initialize WAMR; it registers `HttpForwardFunction`. Both paths add
Wi-Fi and MQTT resources and start a background control-channel task, so the
operator can deliver commands and receive telemetry in either runtime mode.

The scheduler subscribes `wasmProcess` to `wasmTimer` and installs one
15-second timer trigger. Runtime-mode changes are persisted and completed with
`esp_restart()` after release activation.

## 3. SIF admission and dispatch

### 3.1 Admission before allocation

`FunctionAdmissionGate`
(`${SIF_BASE_DIR}/sif_framework/sif_scheduler/sif_functionAdmission.hpp`)
stores one enabled flag and admitted count per `FunctionType`. The scheduler
exposes methods to enable/disable a function, inspect its count, and complete an
admitted invocation.

`Scheduler::createInvocations()`
(`${SIF_BASE_DIR}/sif_framework/sif_scheduler/sif_scheduler.cpp`)
calls `admit()` before allocating either a waiting or immediately available
invocation. A closed gate consumes the event without creating an invocation.
It therefore also suppresses execution-result and invocation-timing telemetry.

An invocation retains one admission count while it moves through waiting,
available, scheduled, dispatched, executing, and one optional retry. Only
terminal deletion decrements the count.

### 3.2 Resource and retry cleanup

`Dispatcher::enqueue()`
(`${SIF_BASE_DIR}/sif_framework/sif_dispatcher/sif_dispatcher.cpp`)
reserves the dependency-expanded SIF resource set before dispatch. Finished
entries are removed from the terminated queue while its mutex is held, then
processed after the mutex is released. This prevents failure requeue from
recursively locking the dispatcher.

The terminal paths:

- abandon resources before requeue or deletion;
- retry a failed invocation at most once while admission remains open;
- delete a failed invocation immediately when admission has been paused; and
- obtain the function type and resource list before deletion, avoiding the
  former use-after-delete cleanup order.

The thread pool uses 16 KiB stacks for scheduler/dispatcher threads and a
32 KiB stack for each invocation worker. Base SIF defaults to one worker on
each ESP32 core. This firmware defines
`SIF_THREADPOOL_WORKER_CORE_COUNT=1`, leaving one invocation worker on core 0.
The configuration matches the single reusable WAMR linear-memory slot and
avoids allocating a second 32 KiB worker stack. Before this override was
applied, the second `pthread_create()` returned `ENOMEM` on the physical
device while the first worker continued to execute successfully.

## 4. Local WAMR execution

### 4.1 Runtime configuration

Local mode calls `wasm_runtime_full_init()` once with a static 48 KiB WAMR
allocator pool. The current build enables the fast interpreter and disables
AoT, libc-WASI, guest pthreads, shared memory, and multi-module support.

The SIF firmware build defines `WAMR_ESP_IDF_STATIC_LINEAR_MEMORY_SIZE=65536`.
The project-modified WAMR ESP-IDF memory map consequently reserves one aligned
64 KiB BSS buffer. `os_mmap()` lends that slot only to a matching
non-executable allocation, clears it before use, and rejects a second matching
allocation while the slot is occupied. `os_munmap()` releases the slot after
the invocation tears down; allocations of other sizes retain the upstream
heap path. The single slot matches the configured one-worker invocation path
and avoids requiring a contiguous 64 KiB system-heap block after Wi-Fi, MQTT,
or HTTP activity has fragmented that heap.

| Bound | Value | Controlling source |
|---|---:|---|
| WAMR allocator pool | 48 KiB | `run_local_mode()` |
| Wasm linear memory | exactly one reusable 64 KiB static page | firmware compile definition, guest linker flags, and `espidf_memmap.c` |
| Module instantiate stack | 8 KiB | `WasmFunction::WASM_STACK_SIZE` |
| WAMR application heap | 0 | `WasmFunction::WASM_HEAP_SIZE` |
| Maximum Wasm bytecode | 8 KiB | `WasmFunction::MAX_WASM_BYTECODE_SIZE` |
| Rust auxiliary stack | 4 KiB | guest `.cargo/config.toml` |
| SIF invocation workers | one worker, 32 KiB stack | firmware compile definition and `sif_threadpool.cpp` |
| Release transition stack | 6 KiB static | `sif_release.cpp` |

### 4.2 Per-invocation lifecycle

`WasmFunction::run()`
(`${SIF_BASE_DIR}/sif_framework/sif_function/sif_wasmFunction.cpp`):

1. locks the canonical bytecode and copies at most 8 KiB into a stack scratch
   buffer;
2. initializes WAMR thread-local state for the SIF worker;
3. loads the scratch module with `wasm_runtime_load()`;
4. instantiates it with an 8 KiB stack and zero application heap;
5. creates an 8 KiB execution environment;
6. stores the `WasmFunction*` as execution-environment user data;
7. resolves and calls `process_event`;
8. translates guest zero/nonzero to `ESP_OK`/`ESP_FAIL`; and
9. destroys the execution environment, instance, module, and WAMR thread state.

WAMR may modify its load buffer, so the canonical vector or embedded array is
never passed directly. `reloadFromPath()` builds a new vector and swaps it under
the same mutex used for per-invocation copying. An invocation therefore runs an
independent byte snapshot even if activation replaces the canonical module.

## 5. Device release lifecycle

### 5.1 Persistent state

[`sif_state::ReleaseMetadata`](../SIF_wasm_dht/main/sif_state.hpp) contains:

```text
generation
artifact_digest
function_identity
resource_contract_json
```

NVS stores active and staged tuples under separate prefixes. It also stores the
concrete mode, battery state, simulation configuration, thresholds, actuator
command, persistent admission pause, last command ID, and last deadline
decision ID. `load_summary()` omits large contracts and diagnostic strings for
high-frequency telemetry and input paths.

The active file is `/spiffs/dht_reader.wasm`; the inactive file is
`/spiffs/dht_reader.staged.wasm`.

### 5.2 Contract compilation

The release manager parses the active contract once into
[`SifContractPolicy`](../SIF_wasm_dht/main/sif_contractPolicy.hpp), an exact
bitset of supported typed capabilities. Empty input/output arrays may be
omitted by Kubernetes JSON serialization and are treated as empty sets. A
present non-array is rejected.

Host imports use this cache rather than allocating and parsing JSON on every
call. Activation compiles the staged contract before promotion and swaps the
cached policy under the release mutex.

### 5.3 Staging

`sif_release_stage_async()` validates the command and tuple before waking the
shared transition worker:

- a generation lower than active or staged state is stale;
- an equal generation must match digest, identity, and full contract;
- an exact duplicate is an idempotent success; and
- another non-duplicate stage/activation cannot overlap the active worker.

The device temporarily closes scheduler admission and drains admitted work
before HTTP download. This gives the constrained heap to the HTTP client and
prevents guest failures/retry storms during staging. It downloads to the
staged SPIFFS path, calculates SHA-256 over the completed file, rejects and
removes a mismatch, persists staged metadata, publishes state telemetry, and
restores admission unless a persistent hard-transition pause is active.

Staging and activation share one statically allocated FreeRTOS task and stack.
The design avoids allocating a late task stack from a fragmented heap and
avoids reserving two stacks for mutually exclusive work.

### 5.4 Activation

`activate_local` and `set_runtime_mode` enter the same asynchronous activation
path with a target mode. The worker:

1. closes admission and waits up to five seconds for the admitted count to
   reach zero;
2. accepts either the matching staged generation or an already active
   generation used for a mode-only change;
3. compiles the staged contract and, for local activation, reloads WAMR bytes;
4. promotes the staged file with a backup/rename sequence;
5. promotes staged NVS metadata to active in one NVS commit;
6. updates the long-lived subscriber identity and active capability cache;
7. transfers or clears LED ownership according to the new contract;
8. persists the target mode and command ID and publishes telemetry; and
9. restores admission or reboots if the concrete mode changed.

File promotion, canonical-vector replacement, and NVS promotion are ordered but
are not one hardware transaction. Interrupted-promotion recovery remains a
firmware test and hardening concern.

### 5.5 Explicit pause and resume

`pause_function` accepts the active or a future generation, disables admission,
persists `admission_paused=true`, and publishes state immediately.
`resume_function` requires the exact active generation before reopening the
gate. Immediate publication is required because invocation telemetry stops
while no new invocation is admitted.

## 6. Device host API and providers

### 6.1 Native registration and pointer safety

[`register_wasm_native_apis()`](../SIF_wasm_dht/main/sif_wasmHostApi.cpp)
registers six imports in module `env`. Signatures such as `(*~*~)f` and
`(*~i)` instruct WAMR to translate and bounds-check each guest pointer/length
pair before calling C++.

Every input getter checks the active typed contract. Undeclared numeric inputs
return the error sentinel; undeclared outputs are ignored. Currently only
`actuatorCommand:i32` has a device-side effect. Other declared numeric outputs
are logged but not applied to a device service.

### 6.2 Current provider boundary

The compiled firmware recognizes the following members:

| Resource | Keys | Source in current demonstrator |
|---|---|---|
| `BATTERY` | `percent:i32`, `voltageMv:i32` | NVS/simulation state and LC709203F gauge |
| `DHT` | `temperature:f32`, `humidity:f32` | SIF DHT resource if bound; current factory supplies no DHT resource and uses 22 °C/50% fallback |
| `LIGHT` | `lux:f32` | fixed 120 lux demonstration provider |
| `OCCUPANCY` | `distanceCm:f32` | fixed 85 cm demonstration provider |
| `GPIO` | `buttonPressed:bool` | fixed false demonstration provider |

The release contract constrains this set; it does not dynamically instantiate
new drivers. Although the CRD schema permits `string`, the guest ABI and
firmware forwarder do not implement end-to-end string input.

## 7. Edge invocation data plane

### 7.1 Request construction

[`HttpForwardFunction::run()`](../SIF_wasm_dht/main/sif_httpForward.cpp) resolves
the active identity and generation for every invocation. It constructs:

```json
{
  "function": "<active identity>",
  "releaseGeneration": 1,
  "source": "esp32-edgemode",
  "resourceInputs": {},
  "events": []
}
```

Only supported keys declared by the active contract are included. Event
metadata is diagnostic; the edge host dispatches the single `process_event`
export rather than routing by event type.

The HTTP client uses a 15-second timeout and at most three transport attempts
with a one-second delay. Timing telemetry separates local collection, total
HTTP duration, edge execution reported by the host, derived network time, local
output application, queue delay, and total execution.

### 7.2 Response binding

A successful response contains result, numeric outputs, edge execution time,
function identity, release generation, and artifact digest. Before applying an
output, the ESP32 requires all release fields to match its active tuple and
checks the output declaration again.

The forwarder returns failure for transport and release-metadata errors. A
completed non-200 response is currently logged but still reaches the final
`ESP_OK`, and a nonzero remote guest result is not converted into a SIF failure.
This asymmetry with local execution is a known correctness limitation.

## 8. MQTT control, telemetry, and LED state

### 8.1 Operator-controlled actions

The device parser accepts the current operator actions:

| Action | Effect |
|---|---|
| `stage_release` | Download, hash, and persist a complete staged tuple. |
| `activate_local` | Activate the specified generation in local mode. |
| `set_runtime_mode` | Activate the specified generation in edge mode. |
| `pause_function` / `resume_function` | Persistently close/open invocation admission. |
| `set_thresholds` | Persist low/high battery thresholds. |
| `set_simulation` | Select real/simulated source and configure drain/recovery steps. |
| `signal_deadline_rejection` | Start one decision-ID-deduplicated blue overlay. |

`set_battery`, `set_battery_source`, and `set_drain` remain as manual demo
compatibility controls; the operator does not use them for release placement.

The MQTT control callback reassembles fragmented payloads with a fixed-bounds
assembler before JSON parsing. Release commands release the cJSON tree before
waking the download worker to reduce peak heap pressure.

The companion SIF Wi-Fi resource waits for either `IP_EVENT_STA_GOT_IP` or
exhaustion of its retry budget before wake-up returns. The control task starts
MQTT only after that wake-up path reports the resource idle. This ordering
prevents DNS lookup from starting against a network interface that has not yet
received an address. The connection log includes the SSID but replaces the
password with a redaction marker.

### 8.2 Telemetry

State telemetry is published with MQTT QoS 1 and contains:

- battery percent, voltage, and source (`real` or `simulated`);
- concrete runtime mode and admission pause;
- active digest, generation, and function identity;
- staged digest and generation; and
- optional invocation timing and only those observed resources enabled by the
  active contract.

Stage, activation, pause, and resume paths publish state without waiting for a
new invocation. The active persisted identity is authoritative; telemetry does
not use a stale boot-time subscriber name after hot release activation.

### 8.3 RGB ownership

[`sif_led.cpp`](../SIF_wasm_dht/main/sif_led.cpp) serializes all channels with
one mutex. The verified board configuration is common-anode/active-low:

```text
green GPIO 17
red   GPIO 16
blue  GPIO 18
```

Actuator values 0/1/2 mean off/green/red. Accepted values are persisted only
when changed and restored after a placement reboot if the active release owns
`actuatorCommand:i32`. Activating a guest without that output clears live and
stored actuator state.

Blue is exclusively a two-pulse, 250 ms on/off deadline-rejection overlay. A
persisted decision ID suppresses duplicate QoS-1 delivery. Completing or
cancelling the overlay restores the newest steady actuator state.

## 9. Edge host

### 9.1 Startup and execution

[`edge/host/main.go`](../edge/host/main.go) loads `WASM_PATH` (default
`dht_reader.wasm`) and initializes the active identity/generation from
`FUNCTION_IDENTITY` and `RELEASE_GENERATION`, defaulting to compatibility
identity `dht-reader` and generation zero.

`runWasm()` holds one mutex across active-release matching, wasmtime Store and
Instance construction, `process_event`, and output copying. Engine, Module, and
Linker are reused for the active release; Store and Instance are new per
request. Process-global request/output maps are safe because execution is
serialized.

The edge host implements the six current imports. It also retains
`get_temperature` and `get_humidity` compatibility imports, which neither
current guest uses. [`wasmBytes()`](../edge/host/hal.go) rejects negative ranges,
overflow, missing memory, and out-of-bounds access.

### 9.2 HTTP API and release slots

| Endpoint | Purpose |
|---|---|
| `GET /health` | Liveness/readiness. |
| `POST /process` | Execute only when function and generation match the active release. |
| `GET`/`HEAD /wasm` | Serve active bytes and SHA-256 metadata. |
| `PUT /wasm` | Hash, compile, validate, and stage a release. |
| `GET /release` | Report active/staged generation, digest, and identity. |
| `POST /release` | Activate one staged generation. |

Host staging rejects stale generations and same-generation metadata conflicts.
An exact active/staged duplicate succeeds idempotently. Host activation holds
the execution mutex, promotes the staged file with backup/rename, swaps the
compiled runtime objects and metadata, and therefore occurs between
invocations. `/process` responds with an `executionRelease` captured inside the
same critical section.

The host currently lacks authentication, request-body limits, durable release
state outside the container filesystem, and guest fuel/interruption.

## 10. Kubernetes API and operator

### 10.1 `WasmFunction` API

[`wasmfunction_types.go`](../edge/operator/api/v1alpha1/wasmfunction_types.go)
defines:

- host image/path/replicas/port/environment;
- device identity, MQTT topics, operator artifact URL, device artifact URL,
  and optional battery simulation;
- placement mode and battery/deadline/drain policies; and
- required `release` generation, raw-Wasm digest, identity, and resource
  contract.

`placement.mode` accepts only `auto`, `local`, or `edge`; `hybrid` is computed.
Policy `enabled` members are pointers so omission defaults to enabled while an
explicit false remains distinguishable. `batterySimulation.enabled` behaves
differently: omission leaves the device source unmanaged.

### 10.2 Reconciliation order

[`WasmFunctionReconciler.Reconcile()`](../edge/operator/internal/controller/wasmfunction_controller.go):

1. reads the custom resource;
2. reconciles the owned Deployment and NodePort Service;
3. resolves and stages the desired host release independently of placement;
4. obtains host availability;
5. resolves proposal, resource locality, candidate deadline result, and
   destination readiness;
6. sends release, simulation, threshold, rejection, pause, placement, and
   resume operations as applicable;
7. updates scalar status and conditions; and
8. schedules bounded retries for synchronization or missing acknowledgement.

### 10.3 Artifact synchronization

[`reconcileArtifact()`](../edge/operator/internal/controller/artifact_sync.go)
requires `device.operatorWasmURL`, obtains or calculates the source digest,
compares it with `spec.release.artifactDigest`, reads `/release`, and stages a
missing host tuple through `PUT /wasm`. HTTP operations use a 15-second client
timeout and artifact bodies are bounded to 8 MiB in the operator.

Host staging runs even during local placement. A failed inactive host stage is
reported through `ArtifactSynchronized=False/SyncFailed` and retried, but it
does not block activation of an already staged desired release at the current
local placement.

Device `stage_release` carries the complete tuple and a device-reachable URL.
The stable command ID is derived from release generation and digest. The first
send and final device acknowledgement are INFO events; acknowledgement-timeout
retries use 30/60-second backoff and debug-level logs.

### 10.4 Placement implementation

`resolvePlacementWithEstimates()` starts from `status.desiredPlacement` or
derives an initial placement from observed mode and the selected contract. It
implements low-battery, explicit, high-battery, and confirmed-drain proposal
priority, then applies mandatory resource locality and candidate-only deadline
evaluation.

`applyDestinationReadiness()` requires the device tuple for local placement and
device tuple + host tuple + available host for edge/hybrid placement. It
retains the current placement while readiness is pending.

[`DeadlineTelemetryEstimator`](../edge/operator/internal/controller/deadline_estimator.go)
is keyed by namespace, resource, function identity, and runtime mode. It loads
fallback profiles from `deadline-profiles.yaml`, retains a bounded in-memory
sample window, and uses the configured nearest-rank percentile after the
minimum sample count.

`applyPlacement()` sends operations in this order:

```text
stage_release
set_simulation (when managed)
set_thresholds
signal_deadline_rejection (when unsafe and ready)
pause_function (hard transition waiting for readiness)
activate_local or host activation + set_runtime_mode
resume_function (after telemetry acknowledgement)
```

Rejection decision IDs contain release generation, Kubernetes spec generation,
current/candidate placement, and telemetry timestamp. A policy edit or fresh
invocation observation can therefore produce one new signal, while duplicate
reconciliation and MQTT redelivery remain silent.

### 10.5 Status and MQTT bridge

The leader-elected telemetry bridge subscribes to every configured telemetry
topic, patches status with Kubernetes conflict retry, and updates the rolling
battery drain and deadline estimators. Observed mode, active/staged release,
function identity, and battery source form the application acknowledgement.

The operator records four conditions (`Available`, `ArtifactSynchronized`,
`DeadlineAdmission`, and `PlacementCommanded`) plus scalar evidence. It also
removes the obsolete `DeadlineReady` and `DeadlineRisk` conditions from
upgraded resources.

## 11. Guests and publication

### 11.1 `basic-edge-demo`

[`basic_edge_demo`](../wasm_guests/basic_edge_demo/) imports only
`env.log_message`, performs a deterministic no-input computation, and returns
zero when its checksum matches. Its release contract is empty, making it the
zero-input edge-placement demonstration and embedded bootstrap.

### 11.2 `hybrid-resource-demo`

[`hybrid_resource_demo`](../wasm_guests/hybrid_resource_demo/) is `no_std` and
uses all typed getters/setters. Its contract declares seven keys across five
device-local resources and six outputs, including device-local
`actuatorCommand:i32`. It calculates Fahrenheit temperature, NWS-style heat
index, a comfort score, occupancy, sampling interval, and actuator choice.

The current source has a 35 °C demonstration temperature override, which
exercises the red/high-heat branch independently of the DHT fallback. This is a
demonstration setting, not a claim of measured ambient temperature.

### 11.3 Build and publication invariants

Both crates target `wasm32-wasip1`, use a 4 KiB stack, declare exactly one
64 KiB initial/maximum memory page, optimize for size, enable LTO, strip output,
and abort on panic. They use custom core-Wasm imports and make no WASI calls.

Each `publish.sh` builds the guest, checks the one-page declaration with
`wasm-objdump`, updates repository artifact copies, computes SHA-256, increments
the live release generation, creates a new timestamped source ConfigMap, rolls
the source Deployment, and patches the complete release tuple. The scripts are
live deployment operations, not build-only helpers.

## 12. Local development checks

Component READMEs contain the operational workflow. The principal source-only
checks are:

```bash
go -C edge/host test ./...
GOCACHE="$PWD/.go-cache" make -C edge/operator test

cargo test --manifest-path wasm_guests/hybrid_resource_demo/Cargo.toml
(cd wasm_guests/basic_edge_demo && \
  cargo build --release --target wasm32-wasip1)
(cd wasm_guests/hybrid_resource_demo && \
  cargo build --release --target wasm32-wasip1)

source /path/to/esp-idf-v5.4.1/export.sh
export SIF_BASE_DIR=/path/to/SIF
idf.py -C SIF_wasm_dht build
```

The four C++ host-policy tests cover admission accounting, contract capability
mapping, control-message reassembly, and LED ownership. Exact commands and the
latest results are recorded in the status chapter.
