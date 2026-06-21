# SIF-Wasm Technical Documentation

This directory is the canonical technical description of the SIF-Wasm
prototype. It is organized for direct use while writing the system-design and
implementation chapters of the thesis.

The documented system combines the implementation in this repository with the
companion contribution to SIF as of 2026-07-22. SIF on an ESP32 schedules a
generation-bound Wasm function, WAMR executes it locally, a Go/wasmtime host
executes it remotely, and a Kubernetes operator coordinates release delivery
and placement admission.

## Document ownership

1. [System Design](01-system-design.md) owns architectural responsibilities,
   stable interfaces, state models, decision order, and trust boundaries.
2. [Implementation](02-implementation.md) owns the source-level realization,
   concrete memory bounds, protocols, persistence, and development checks.
3. [Traceability and Status](03-traceability-and-status.md) owns implementation
   status, dated validation evidence, limitations, and the thesis claim
   boundary.

Component-specific build and operating instructions live in README files next
to the relevant code. They should link here instead of repeating the system
design.

## Current scope

- Device: ESP32, ESP-IDF/FreeRTOS, SIF, WAMR, SPIFFS, NVS, MQTT, and HTTP.
- Edge execution: a standard Linux container embedding Go and wasmtime.
- Control plane: a Kubebuilder operator managing `WasmFunction`, Deployment,
  Service, release synchronization, placement admission, and telemetry status.
- Guests: `basic-edge-demo` and `hybrid-resource-demo`, both compiled from Rust
  to `wasm32-wasip1` and constrained to one 64 KiB memory page.
- Distribution: verified raw Wasm over HTTP; OCI is used for operator and host
  container images, not as the ESP32 guest-consumption format.
- Placement: logical local, edge, and hybrid across two concrete device runtime
  modes (`local` and `edge`).

Cloud execution, a Wasm CRI shim, ESP32 OCI manifest/layer handling, live stack
migration, and Xtensa AoT are outside the implemented scope.

## Evidence vocabulary

| Term | Meaning |
|---|---|
| **Implemented** | The controlling source path realizes the stated behavior. |
| **Locally validated** | A relevant automated test, host-policy test, guest build, or firmware build passed. |
| **Live validated** | The current protocol was observed on the physical ESP32 and/or deployed edge cluster. |
| **Partially validated** | Only a subset, build boundary, emulated provider, or host-only policy was exercised. |
| **Compatibility behavior** | Retained for the existing deployment slot or older artifact, but not part of the active guest contract. |
| **Future work** | Not implemented in the current prototype. |

Source establishes implementation. Tests establish local validation. Dated
hardware/cluster observations establish live validation. One category must not
be inferred from another.

## Naming rules

The stable Kubernetes deployment slot is still named `dht-reader`, and the
container/firmware artifact path remains `dht_reader.wasm`. Those names do not
identify the executing guest. The authoritative logical identity comes from the
active release tuple and is currently `basic-edge-demo` or
`hybrid-resource-demo`.

## Documentation lifecycle

The former `docs/implementation-guide.md`, `docs/operations-guide.md`,
`project_state.md`, implementation plans, requirements notes, and handoff files
were removed during the 2026-07-22 consolidation because they duplicated this
directory or described superseded behavior.
