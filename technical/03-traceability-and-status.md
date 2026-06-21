# Traceability and Status

## 1. Snapshot

This ledger describes the combined thesis repository, companion SIF
contribution, and validation baseline as of **2026-07-22**.

- The thesis demonstrator is implemented across the ESP32, SIF runtime, WAMR
  host, Rust guests, Go edge host, and Kubernetes operator.
- The current release and placement protocol has live hardware/cluster evidence
  through `hybrid-resource-demo` generation 32 and `basic-edge-demo`
  generation 33.
- The stable Kubernetes object, Service, image path, and artifact slot remain
  named `dht-reader`; they are compatibility deployment identifiers, not the
  active guest identity.
- The full release delivery state has converged to `Active` with matching host,
  device, and desired evidence for multiple releases.
- Core local/edge/hybrid placement, safe and rejected deadline decisions,
  battery simulation, and RGB behavior have been exercised live.
- A control firmware built with the unmodified upstream WAMR memory map
  repeatedly failed local hybrid instantiation with `allocate linear memory
  failed` after networking and release activity. The restored forked static
  slot has clean-build and linked-symbol evidence but still requires a repeat
  physical-device run.
- Recovery, security, generality, and edge error-semantics gaps remain and
  constrain the thesis claims.

## 2. Evidence method

### 2.1 Precedence

1. Current source establishes what is implemented.
2. Current automated checks establish local validation.
3. Dated serial, operator, host, and Kubernetes observations establish live
   validation of the current protocol.
4. This ledger summarizes that evidence but does not override current source or
   establish runtime behavior without the corresponding observation.

### 2.2 Promotion rules

| Status | Evidence required |
|---|---|
| **Implemented** | A controlling runtime path exists. |
| **Locally validated** | A relevant current test/build passed. |
| **Live validated** | The current path was observed on physical hardware or the deployed cluster. |
| **Partially validated** | Only part of the path, an emulated provider, or a policy abstraction was exercised. |
| **Future work** | The implementation boundary is absent or intentionally outside scope. |

Live evidence is phrased narrowly. For example, a successful release activation
does not prove crash recovery at every file/NVS boundary.

## 3. Current live baseline

The following observations are the current evidence boundary recorded during
the 2026-07-21 live validation.

### 3.1 Release delivery and identity

- Generation 21 demonstrated one stable `stage_release`, one staged telemetry
  acknowledgement, one activation command, and one active acknowledgement,
  followed by repeated successful invocations without the former dispatcher
  retry storm or MQTT outbox exhaustion.
- Reused Wasm bytes at generation 27 demonstrated that host activation now
  compares digest, generation, and function identity rather than digest alone.
  The host activated the new generation before the device entered remote mode.
- Generation 32 ran `hybrid-resource-demo` remotely with seven typed inputs.
- Generation 33 activated `basic-edge-demo` while the ESP32 remained in edge
  runtime mode. Its empty contract normalized logical placement from hybrid to
  edge, the device sent `resourceInputs:{}`, and the host repeatedly executed
  with zero inputs in approximately 1 ms.
- Final generation-33 status recorded `releaseDeliveryState=Active`, matching
  desired/host/device digests, acknowledged generation 33, and observed
  function `basic-edge-demo`.
- Stable command IDs, MQTT QoS 1, acknowledgement-timeout requeues, and
  duplicate delivery were observed without duplicate activation effects.

### 3.2 Placement and admission

- `hybrid-resource-demo` executed locally and remotely with contract-driven
  collection and actuator output.
- Simulated low/high battery cycles produced local-to-hybrid/edge and
  edge-to-local runtime transitions while keeping `placement.mode=auto`.
- Rolling battery-drain confirmation proposed a soft remote transition only
  after the configured risky windows.
- With an unsafe deadline target, fresh device invocation telemetry produced a
  distinct rejection decision and one two-pulse blue episode per observation.
  Repeated reconciles of the same observation remained deduplicated.
- Editing the deadline target while it remained unsafe produced a new decision
  through the Kubernetes spec generation, while same-generation reconciles
  remained silent.
- A hard low-battery threshold overrode unsafe deadline admission and permitted
  remote placement after readiness. Subsequent battery recovery allowed the
  return to local execution.
- The operator's persistent hard-pending `pause_function`/`resume_function`
  orchestration has focused automated coverage; a deliberately unavailable
  destination recovery sequence is not separately claimed as live validated.

### 3.3 Battery simulation and acknowledgement

- `spec.device.batterySimulation` was deployed with explicit enablement and
  drain/recovery steps.
- A successful MQTT publish did not count as device acknowledgement. When fresh
  post-command telemetry still reported `real`, the operator retried the stable
  configuration once; telemetry then reported `simulated` and retries stopped.
- Local invocation telemetry subsequently showed the requested deterministic
  battery drain.

### 3.4 RGB and actuator state

- Labeled physical diagnostics established a common-anode, active-low LED with
  green GPIO 17, red GPIO 16, and blue GPIO 18.
- `actuatorCommand=1` drove only green and `actuatorCommand=2` drove red.
- Deadline rejection produced two blue on/off cycles and restored the prior
  steady actuator state.
- The active guest's latest accepted green/red command survived local-to-edge
  and edge-to-local software resets through NVS restoration.
- Activating output-free `basic-edge-demo` cleared the live and persisted
  actuator state.

## 4. Claim ledger

### 4.1 SIF and device runtime

| ID | Claim | Status | Evidence and boundary |
|---|---|---|---|
| SIF-01 | Events are matched to generic Wasm subscriptions and scheduled by deadline/resource readiness. | **Implemented** | `Event`, `BasicWasmTrigger`, `Scheduler::createInvocations()`, and dispatcher source. |
| SIF-02 | Admission is checked before invocation allocation and one count follows an invocation through one retry. | **Locally validated** | `FunctionAdmissionGate` plus current host-policy test. |
| SIF-03 | Paused staging/activation drains admitted work and terminal cleanup releases resources without recursive dispatcher locking or use-after-delete. | **Live validated** | Current cleanup source; release activation on hardware exposed and confirmed the corrected path. |
| SIF-04 | A trigger observed while paused creates no invocation or invocation telemetry. | **Partially validated** | Scheduler source and policy test; no isolated on-device trigger-count experiment is recorded. |
| SIF-05 | The thesis firmware selects one 32 KiB invocation worker to match its single reusable WAMR slot. | **Locally validated** | Compile definition and clean firmware build; the preceding two-worker build produced `ENOMEM` for the second worker while core 0 remained operational. Repeat boot validation is pending. |
| DEV-01 | Concrete mode selects WAMR locally and HTTP forwarding remotely while MQTT control remains present in both. | **Live validated** | Current boot paths and observed local/edge cycles. |
| DEV-02 | Wi-Fi wake-up does not report readiness before an IP address, and connection logs redact the password. | **Locally validated** | Companion SIF source and clean firmware build. A repeat live connection test is pending. |

### 4.2 WAMR, memory, and ABI

| ID | Claim | Status | Evidence and boundary |
|---|---|---|---|
| WAMR-01 | Each local invocation loads, instantiates, calls, and tears down WAMR around `process_event`. | **Live validated** | `WasmFunction::run()` and repeated on-device guest execution. |
| WAMR-02 | Bytecode replacement and invocation copying share a mutex; each call executes a scratch snapshot. | **Implemented** | `reloadFromPath()` and `WasmFunction::run()`. |
| WAMR-03 | The active device configuration uses a 48 KiB WAMR pool, one reusable static 64 KiB linear-memory slot, 8 KiB WAMR stack, zero app heap, and 8 KiB bytecode maximum. | **Live validated** | Clean-build symbol evidence, one-page guest checks, and repeated physical-device hybrid invocations without linear-memory allocation failure. |
| ABI-01 | Current guests use the generic typed getters/setters and logging ABI on both hosts. | **Live validated** | Hybrid input/output runs and zero-input basic edge runs. |
| ABI-02 | ESP32 input/output calls are constrained by the active exact typed contract. | **Live validated** | Cached policy source, host-policy test, successful hybrid release after the former undeclared-input retry storm was removed. |
| ABI-03 | Guest pointer/length pairs are bounds-checked by WAMR signatures and explicit wasmtime memory checks. | **Implemented** | Native signature table and `wasmBytes()`. |
| ABI-04 | Arbitrary string resources work end to end. | **Future work** | CRD/edge decoding can represent strings; device/guest ABI cannot. |

### 4.3 Release lifecycle

| ID | Claim | Status | Evidence and boundary |
|---|---|---|---|
| REL-01 | Generation binds digest, function identity, and the complete resource contract in active/staged slots. | **Live validated** | NVS/host state plus multiple staged and active release acknowledgements. |
| REL-02 | Device and host reject stale generations and same-generation conflicts and accept exact duplicates idempotently. | **Partially validated** | Host tests cover conflict/idempotency; device enforcement is implemented and firmware-build validated but not directly covered by an ESP-IDF test. |
| REL-03 | The device hashes completed bytes before persisting staged metadata. | **Live validated** | Serial staging logs and matching device acknowledgement. |
| REL-04 | Host activation and execution share an invocation boundary. | **Locally validated** | Shared mutex and concurrency-focused host tests. |
| REL-05 | Same bytes at a newer generation still require full host activation. | **Live validated** | Generation-27 incident and correction. |
| REL-06 | Same-mode activation changes the active guest identity without retaining the boot subscriber identity. | **Live validated** | Generation-33 edge-mode activation and converged telemetry. |
| REL-07 | Power-loss recovery is correct at every SPIFFS/NVS promotion step. | **Future work** | Ordered backup/rename and NVS operations exist, but no complete interruption matrix is tested. |

### 4.4 Edge data plane

| ID | Claim | Status | Evidence and boundary |
|---|---|---|---|
| EDGE-01 | `/process` requires active function/generation and reports the exact executed function/generation/digest. | **Live validated** | Same-digest generation fix and successful remote invocations. |
| EDGE-02 | The device sends only declared supported inputs and verifies all release evidence before applying a declared output. | **Live validated** | Seven-input hybrid and zero-input basic release observations. |
| EDGE-03 | Completed HTTP errors and nonzero remote guest results have the same SIF failure semantics as local execution. | **Future work** | The forwarder currently returns success after a completed non-200 response and ignores nonzero guest result for SIF retry. |
| EDGE-04 | Host APIs are authenticated, request bodies are bounded, and nonterminating guests are interrupted. | **Future work** | Not implemented. |

### 4.5 Operator and placement

| ID | Claim | Status | Evidence and boundary |
|---|---|---|---|
| K8S-01 | `spec.release` requires generation, raw digest, identity, and complete contract. | **Locally validated** | API, generated CRD/DeepCopy, sample, and operator tests. |
| K8S-02 | The operator owns the host Deployment/Service and stages the host independently of placement. | **Live validated** | Deployed controller behavior and active/staged host status. |
| MQTT-01 | QoS-1 release commands use stable IDs and telemetry, not PUBACK, for application acknowledgement. | **Live validated** | Release and battery-simulation retry observations. |
| PLC-01 | Battery threshold and rolling drain propose placement; mandatory resource locality converts remote to hybrid when needed. | **Live validated** | Auto-mode local/remote cycles and generation-33 hybrid-to-edge normalization. |
| DL-01 | Deadline policy admits only a destination proposed by another policy and uses candidate-specific local/edge/hybrid cost. | **Locally validated** | Direction/cost table tests and controller source. |
| DL-02 | Unsafe soft proposals are retained/rejected without destination activation and are signaled once per fresh observation/spec decision. | **Live validated** | Multiple physical blue episodes and rejected edge-to-local state. |
| DL-03 | Hard low battery can override unsafe admission after readiness. | **Live validated** | Simulated battery cycle crossing the hard threshold. |
| PLC-02 | A hard transition waiting on an unavailable destination persists pause and resumes only after mode/generation/digest acknowledgement. | **Locally validated** | Focused transition-orchestration tests; no isolated live unavailable-destination run. |

### 4.6 Guests and actuation

| ID | Claim | Status | Evidence and boundary |
|---|---|---|---|
| GST-01 | `basic-edge-demo` is one-page, logging-only, and has an empty contract. | **Live validated** | Generation-33 zero-input edge execution. |
| GST-02 | `hybrid-resource-demo` is one-page and consumes seven typed inputs across five resources with six outputs. | **Live validated** | Generation-32 remote inputs and earlier local/remote actuator runs. Some providers remain emulated. |
| PUB-01 | Publication changes generation, digest, identity, and contract together. | **Live validated** | Generation-bound scripts and multiple live publications. |
| LED-01 | Green/red steady ownership, blue rejection overlay, and reboot restoration are synchronized. | **Live validated** | Physical diagnostics, rejection episodes, and both placement reboot directions. |

## 5. Local validation on 2026-07-22

The documentation cleanup reran the following source-only checks without
publishing artifacts or changing the cluster/device:

| Check | Result |
|---|---|
| `go test ./...` in `edge/host` | Passed |
| `go test ./...` in `edge/operator` | Passed |
| `cargo test` in `hybrid_resource_demo` | Passed, 2 tests |
| Release Wasm builds for both guests | Passed |
| `wasm-objdump` one-page gate for both guests | Passed: initial=1, max=1 |
| `sif_contract_policy_test.cpp` | Passed with `-Wall -Wextra -Werror` |
| `sif_control_message_assembler_test.cpp` | Passed with `-Wall -Wextra -Werror` |
| `sif_function_admission_test.cpp` | Passed with `-Wall -Wextra -Werror` |
| `sif_led_policy_test.cpp` | Passed with `-Wall -Wextra -Werror` |

The Go tests required normal localhost listener/build-cache access outside the
restricted command sandbox. No production or live external state was changed.

The firmware was not rebuilt during this documentation-only pass. The latest
recorded ESP-IDF v5.4.1 build and physical flash passed after the actuator-state
preservation changes; the recorded application retained approximately 41%
headroom in its 2 MiB app partition.

No feature-specific Kind E2E suite has been added; the checked-in E2E files are
generic Kubebuilder scaffolding.

## 6. Known implementation limitations

### 6.1 Device and runtime

1. SPIFFS file promotion, WAMR byte-vector replacement, NVS tuple promotion,
   and reset are not one atomic transaction.
2. ESP-IDF-native tests do not cover stale/conflicting staging, NVS corruption,
   interrupted download/promotion, or every invocation-drain race.
3. WAMR has no guest instruction budget or preemption for a nonterminating
   module.
4. The active factory does not bind a physical DHT resource; DHT values use the
   documented 22 °C/50% fallback. Light, occupancy, and button providers are
   also fixed demonstration values.
5. Only `actuatorCommand:i32` is applied as a device output. String resources
   and arbitrary dynamic collectors are not end-to-end capabilities.

### 6.2 Edge and control plane

1. The edge forwarder does not convert all HTTP/guest failures into local-like
   SIF failure semantics.
2. Edge endpoints lack complete authentication, body limits, TLS policy, and
   guest interruption/fuel.
3. Host active/staged files and metadata are not persisted independently of the
   container filesystem; restart recovery relies on operator reconciliation.
4. The deadline estimator window is process memory; ConfigMap profiles are the
   restart fallback.
5. Device telemetry binds the complete contract through release generation but
   does not echo every contract field independently.
6. There is no feature-specific cluster E2E acceptance suite.

### 6.3 Distribution and scope

1. SHA-256 verifies equality, not artifact provenance or publisher identity.
2. Guest distribution is raw Wasm over HTTP, not OCI guest manifest/layer
   consumption on the operator or ESP32.
3. Kubernetes runs a normal Go/wasmtime container, not a Wasm CRI workload.
4. The device uses interpretation, not Xtensa AoT.
5. The continuum does not include a cloud execution tier.

## 7. Thesis claim boundary

The implementation supports the following thesis-level claims:

- SIF can schedule a portable generation-bound guest through either a bounded
  device sandbox or a Kubernetes-managed edge host.
- A release-constrained host API separates guest logic from device hardware and
  binds remote inputs/outputs to the same release identity.
- Battery, drain, resource locality, readiness, and candidate-only deadline
  admission can coordinate execution placement without moving in-flight state.
- Staged/active release state, idempotent commands, and telemetry
  acknowledgement provide application-level convergence across MQTT and HTTP.
- Logical hybrid placement preserves device data/actuator locality around edge
  computation.

The implementation does **not** support claims of arbitrary hardware support,
zero-trust artifact delivery, failure-atomic recovery, cloud placement, live
process migration, native Kubernetes Wasm orchestration, OCI guest delivery to
the ESP32, or AoT execution.

## 8. Documentation consistency checklist

Future changes should preserve these rules:

1. A release example includes generation, raw digest, identity, inputs, and
   outputs together.
2. `dht-reader` is labeled as a compatibility slot, not the active guest.
3. Logical hybrid is distinct from concrete device mode edge.
4. Deadline logic admits another policy's candidate; it does not search for or
   propose placement by itself.
5. Rejected destinations receive no activation command.
6. Release staging and activation remain separate on both runtimes.
7. Status lists `Available` plus the three control-state conditions.
8. Local tests and live observations remain separate evidence categories.
9. Emulated providers, error-semantics gaps, recovery gaps, and security scope
   remain explicit until the controlling source changes.
10. Examples contain placeholders rather than secrets, personal endpoints, or
    machine-specific device identifiers.
