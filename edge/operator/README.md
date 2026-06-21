# SIF Operator

`sif-operator` is the Kubernetes control plane for SIF-Wasm. It reconciles a
`WasmFunction`, owns the edge-host Deployment and NodePort Service, synchronizes
a generation-bound Wasm release to the host and ESP32, evaluates placement
transitions, publishes idempotent MQTT commands, and records application-level
acknowledgement in status.

The current protocol is live validated for multiple local, edge, and hybrid
releases. Exact evidence and remaining limitations are in
[Traceability and Status](../../technical/03-traceability-and-status.md).

## Project layout

```text
api/v1alpha1/                     WasmFunction API and generated DeepCopy
cmd/main.go                       manager entry point
internal/controller/              reconciliation, placement, MQTT, telemetry
config/crd/bases/                 generated CRD
config/manager/                   manager Deployment and deadline profiles
config/samples/                   example WasmFunction
test/e2e/                         generic Kubebuilder E2E scaffold
```

Generated API output must be regenerated through the Makefile rather than
edited by hand.

## Managed resources and dependencies

For each `WasmFunction`, the controller owns:

- one edge-host Deployment;
- one NodePort Service; and
- release, placement, command, and observability status.

It also depends on:

- an operator-reachable authoritative raw-Wasm URL;
- a device-reachable URL serving the same bytes;
- edge-host `/wasm` and `/release` endpoints;
- MQTT control and telemetry topics; and
- a mounted `deadline-profiles` ConfigMap.

The registry tunnel used to publish new operator and edge-host images is
documented under [Development network paths](../README.md#development-network-paths).

The host stays deployed during local placement so a remote destination can be
staged before it is selected.

## Required release tuple

`spec.release` is mandatory:

```yaml
release:
  generation: 1
  artifactDigest: <lowercase-sha256-of-raw-wasm>
  functionIdentity: basic-edge-demo
  resourceContract:
    inputs: []
    outputs: []
```

Generation binds the raw bytes, logical identity, and complete resource
contract. Any change requires a larger generation. Same-generation conflicts
are rejected; exact duplicate staging and activation are idempotent.

`device.operatorWasmURL` is authoritative for the operator.
`device.artifactURL` is embedded in `stage_release` for the ESP32. Both URLs
must resolve to identical bytes, although they may use different network
addresses.

The Kubernetes object and Service may remain named `dht-reader`; the selected
guest is `spec.release.functionIdentity`.

## Reconciliation

The main reconciliation order is:

1. reconcile the owned Deployment and Service;
2. resolve and verify the authoritative artifact;
3. read host active/staged state and stage a missing tuple;
4. determine edge-host availability;
5. resolve proposal, resource locality, candidate deadline result, and
   destination readiness;
6. publish release/policy/placement commands as needed;
7. update conditions and scalar status; and
8. schedule bounded retry for synchronization or missing acknowledgement.

Host staging is independent of placement. An inactive-host staging failure is
reported and retried, but does not prevent a ready desired release from
activating at the retained local placement.

HTTP artifact operations use a 15-second client timeout. Operator downloads are
bounded to 8 MiB, and synchronization retry delay is capped at 60 seconds.

## Placement and admission

`spec.placement.mode` accepts `auto`, `local`, or `edge`. Logical `hybrid` is a
computed status value; it uses concrete ESP32 runtime mode `edge`.

Proposal priority is:

1. hard low-battery transition from current local placement;
2. explicit local/edge request;
3. high-battery local return in auto mode;
4. confirmed rolling battery-drain remote proposal; and
5. retain the accepted placement.

Observed MQTT battery is preferred over the optional
`spec.placement.batteryPercent` fallback. Low/high thresholds provide
hysteresis. Drain requires the configured number of consecutive risky windows
and resets when battery source, selected function, runtime mode, or telemetry
continuity changes.

Any resource input or output with `locality: device` transforms a remote edge
candidate into hybrid. Removing the final device-local member normalizes a
retained hybrid placement to edge without sending a redundant runtime-mode
command.

Deadline admission evaluates only a destination proposed by another policy:

```text
local  = queue + wake + local execution + safety margin
edge   = network + edge execution + safety margin
hybrid = input collection + network + edge execution
         + output application + safety margin
```

Missing target/estimates causes an abstention. Unknown or mismatched identity
blocks a soft transition. An unsafe soft candidate is retained/rejected with no
destination activation. Hard low battery can override deadline risk only after
destination readiness.

Policy `enabled` pointers default to enabled when omitted and preserve an
explicit false. `batterySimulation.enabled` is different: omission leaves the
device battery source unmanaged.

`highBatteryThreshold: 101` is reserved for controlled validation that must
retain remote placement while telemetry is capped at 100. Normal deployments
should use at most 100.

## Deadline profiles

Fallbacks are loaded from
[`config/manager/deadline-profiles.yaml`](config/manager/deadline-profiles.yaml):

| Identity | Local exec | Queue | Wake | Edge exec | Network | Input | Output |
|---|---:|---:|---:|---:|---:|---:|---:|
| `basic-edge-demo` | 20 ms | 200 ms | 0 ms | 2 ms | 100 ms | 0 ms | 0 ms |
| `dht-reader` (compatibility) | 50 ms | 200 ms | 0 ms | 5 ms | 100 ms | 20 ms | 0 ms |
| `hybrid-resource-demo` | 100 ms | 200 ms | 0 ms | 5 ms | 100 ms | 50 ms | 2 ms |

The in-memory estimator is isolated by namespace, `WasmFunction`, selected
identity, and runtime mode. After `minSamples`, each metric uses the configured
nearest-rank percentile; otherwise the profile/fallback value is used.

## Command protocol

The operator publishes MQTT QoS-1 messages with stable identifiers:

| Action | Purpose |
|---|---|
| `stage_release` | Deliver the complete inactive tuple and device artifact URL. |
| `activate_local` | Activate the generation in WAMR/local mode. |
| `set_runtime_mode` | Activate/select the generation in concrete edge mode. |
| `pause_function` | Close admission for a hard transition waiting on readiness. |
| `resume_function` | Reopen after mode/generation/digest acknowledgement. |
| `set_thresholds` | Synchronize low/high threshold compatibility state. |
| `set_simulation` | Apply the optional declarative battery demo source. |
| `signal_deadline_rejection` | Request one blue overlay for a new rejected decision. |

Broker `PUBACK` proves broker acceptance only. Release convergence requires
later device telemetry with matching mode, generation, digest, and identity.
The operator claims sends in memory/status before publication to suppress
duplicate reconciles, uses 30/60-second acknowledgement-timeout retry, and
keeps the same command ID across retries.

Battery simulation acknowledgement is the observed source. A fresh
post-command telemetry sample that still disagrees permits one retry; unchanged
reconciles before new telemetry remain silent.

Deadline decision IDs include release generation, Kubernetes spec generation,
current/candidate placement, and telemetry timestamp. Every fresh rejected
invocation observation may signal once, while duplicate reconciliation and
QoS-1 redelivery remain idempotent.

## Status

Conditions are:

| Condition | Question answered |
|---|---|
| `Available` | Were the Deployment and Service reconciled? |
| `ArtifactSynchronized` | Do host and device hold the desired release active or staged? |
| `DeadlineAdmission` | Was the candidate accepted, rejected, blocked, overridden, or not evaluated? |
| `PlacementCommanded` | Did placement converge, send a command, remain pending, or get rejected? |

`releaseDeliveryState` progresses through:

```text
Pending -> AwaitingStageAck -> Staged -> AwaitingActivationAck -> Active
```

Scalar status includes desired/active/staged digests and generations,
selected/observed function, logical placement and concrete mode, proposal
source/candidate, predicted cost/slack, readiness/retry state, battery source
and drain observations, admission pause, command identifiers, and timestamps.

The controller removes obsolete `DeadlineReady` and `DeadlineRisk` conditions
when reconciling upgraded resources.

## MQTT configuration

The manager reads:

```text
SIF_MQTT_BROKER
SIF_MQTT_USER
SIF_MQTT_TOKEN
SIF_MQTT_CLIENT_ID
```

The default manager manifest reads `SIF_MQTT_TOKEN` from the `sif-mqtt` Secret.
Create/update that Secret without printing the token. The leader-elected
telemetry bridge subscribes to configured topics and uses Kubernetes conflict
retry when patching status.

## Build and test

```bash
cd edge/operator
make generate manifests fmt vet
make test
make build
```

Or from the repository root:

```bash
GOCACHE="$PWD/.go-cache" make -C edge/operator test
```

The tests cover artifact matching/staging, same-digest newer-generation host
activation, all destination cost directions, resource-locality transformation,
rolling drain, identity mismatch, rejection deduplication, battery-simulation
acknowledgement, pause/resume orchestration, MQTT QoS 1, and status updates.

The generic Kind E2E scaffold is not yet a feature-specific acceptance suite.

## Example resource

Use the checked-in
[`edge_v1alpha1_wasmfunction.yaml`](config/samples/edge_v1alpha1_wasmfunction.yaml)
as the schema reference. A minimal shape is:

```yaml
apiVersion: edge.sif.2iot.2de/v1alpha1
kind: WasmFunction
metadata:
  name: dht-reader
  namespace: <namespace>
spec:
  image: <registry>/sif-edge-host:<immutable-tag>
  wasmPath: /app/dht_reader.wasm
  replicas: 1
  port: 8080
  device:
    id: <device-id>
    controlTopic: <control-topic>
    telemetryTopic: <telemetry-topic>
    operatorWasmURL: http://<operator-reachable-source>/wasm
    artifactURL: http://<device-reachable-source>/wasm
  release:
    generation: 1
    artifactDigest: <raw-wasm-sha256>
    functionIdentity: basic-edge-demo
    resourceContract:
      inputs: []
      outputs: []
  placement:
    mode: auto
    lowBatteryThreshold: 20
    highBatteryThreshold: 60
    batteryThreshold: {enabled: true}
    deadline:
      enabled: true
      targetMs: 1000
      minSlackMs: 100
      safetyMarginMs: 50
    batteryDelta:
      enabled: true
      windowSeconds: 60
      maxDrainPercent: 3
      riskyWindowsToOffload: 2
```

Replace every placeholder and the sample zero digest before applying it.

## Deployment boundary

Installing the CRD, publishing an image, rolling the manager, applying a
`WasmFunction`, running a guest publication script, or sending MQTT traffic
changes external state. Use immutable image references, keep
`spec.release` atomic, and inspect device telemetry plus host `/release` after
each rollout.

Do not commit MQTT tokens, live device topics, personal addresses, or
kubeconfigs.
