/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WasmFunctionSpec defines the desired state of WasmFunction.
type WasmFunctionSpec struct {
	// image is the container image running the sif-edge-host binary.
	// +kubebuilder:default="localhost:30500/sif-edge-host:latest"
	Image string `json:"image"`

	// wasmPath is the path to the .wasm module inside the container.
	// +kubebuilder:default="/app/dht_reader.wasm"
	// +optional
	WasmPath string `json:"wasmPath,omitempty"`

	// replicas is the desired number of host pods.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// port is the HTTP port the host listens on.
	// +kubebuilder:default=8080
	// +optional
	Port int32 `json:"port,omitempty"`

	// env is additional environment variables passed to the host container.
	// +optional
	Env []EnvVar `json:"env,omitempty"`

	// device describes the IoT endpoint that can run the same Wasm function.
	// +optional
	Device DeviceSpec `json:"device,omitempty"`

	// placement describes the desired execution locality and battery guardrails.
	// +optional
	Placement PlacementSpec `json:"placement,omitempty"`
}

// EnvVar is a simplified key-value environment variable.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// DeviceSpec identifies the controlled IoT runtime.
type DeviceSpec struct {
	// id is a human-readable device identifier used in status and logs.
	// +optional
	ID string `json:"id,omitempty"`

	// controlTopic is the MQTT topic where placement commands are published.
	// +optional
	ControlTopic string `json:"controlTopic,omitempty"`

	// telemetryTopic is the MQTT topic where the device publishes live battery
	// telemetry for operator-driven auto placement. When omitted, the operator
	// derives it as <controlTopic>/telemetry.
	// +optional
	TelemetryTopic string `json:"telemetryTopic,omitempty"`

	// operatorWasmURL is the HTTP URL the operator uses as the authoritative
	// artifact source. The operator resolves the desired digest from this URL and
	// keeps the edge host /wasm endpoint synchronized to it.
	// +optional
	OperatorWasmURL string `json:"operatorWasmURL,omitempty"`

	// reloadWasmURL is the HTTP URL sent to the device in a "reload" command
	// when local execution must fetch or refresh its artifact.
	// +optional
	ReloadWasmURL string `json:"reloadWasmURL,omitempty"`

	// wasmURL is a deprecated compatibility fallback used as both the operator
	// artifact source URL and the device reload URL when the newer split fields
	// are not set.
	// +optional
	WasmURL string `json:"wasmURL,omitempty"`
}

// PlacementSpec defines the control-plane placement policy.
type PlacementSpec struct {
	// mode is the requested placement policy: auto, local, or edge.
	// +kubebuilder:validation:Enum=auto;local;edge
	// +kubebuilder:default=auto
	// +optional
	Mode string `json:"mode,omitempty"`

	// batteryPercent is an optional manual fallback used when live device
	// telemetry is unavailable.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	BatteryPercent *int32 `json:"batteryPercent,omitempty"`

	// lowBatteryThreshold forces edge placement at or below this battery level.
	// +kubebuilder:default=20
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	LowBatteryThreshold int32 `json:"lowBatteryThreshold,omitempty"`

	// highBatteryThreshold allows local placement at or above this battery level.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	HighBatteryThreshold int32 `json:"highBatteryThreshold,omitempty"`
}

// WasmFunctionStatus defines the observed state of WasmFunction.
type WasmFunctionStatus struct {
	// availableReplicas is the number of ready pods.
	// +optional
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// endpoint is the in-cluster service URL (e.g. http://<name>.<ns>:8080/process).
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// desiredArtifactDigest is the SHA-256 digest selected by the operator from
	// the configured operator artifact source URL.
	// +optional
	DesiredArtifactDigest string `json:"desiredArtifactDigest,omitempty"`

	// hostArtifactDigest is the SHA-256 digest currently served by the edge host
	// /wasm endpoint.
	// +optional
	HostArtifactDigest string `json:"hostArtifactDigest,omitempty"`

	// desiredPlacement is the placement selected by the control plane.
	// +optional
	DesiredPlacement string `json:"desiredPlacement,omitempty"`

	// placementReason explains why the current desired placement was selected.
	// +optional
	PlacementReason string `json:"placementReason,omitempty"`

	// observedBatteryPercent records the battery value used for placement.
	// +optional
	ObservedBatteryPercent *int32 `json:"observedBatteryPercent,omitempty"`

	// observedMode is the latest runtime mode reported by device telemetry.
	// +optional
	ObservedMode string `json:"observedMode,omitempty"`

	// observedBatterySource records whether the reported battery values are real
	// gauge readings or simulated values.
	// +optional
	ObservedBatterySource string `json:"observedBatterySource,omitempty"`

	// observedVoltageMillivolts is the latest battery voltage reported by the device.
	// +optional
	ObservedVoltageMillivolts *int32 `json:"observedVoltageMillivolts,omitempty"`

	// observedArtifactDigest is the SHA-256 digest of the currently active wasm
	// artifact reported by device telemetry.
	// +optional
	ObservedArtifactDigest string `json:"observedArtifactDigest,omitempty"`

	// lastTelemetryTime is when the latest device telemetry sample was observed.
	// +optional
	LastTelemetryTime *metav1.Time `json:"lastTelemetryTime,omitempty"`

	// lastCommandedPlacement is the last placement command sent to the device.
	// +optional
	LastCommandedPlacement string `json:"lastCommandedPlacement,omitempty"`

	// lastCommandedArtifactDigest is the artifact digest tied to the latest
	// placement or reload command sent by the operator.
	// +optional
	LastCommandedArtifactDigest string `json:"lastCommandedArtifactDigest,omitempty"`

	// lastCommandTime is when the latest placement command was sent.
	// +optional
	LastCommandTime *metav1.Time `json:"lastCommandTime,omitempty"`

	// lastAppliedLowBatteryThreshold is the latest low threshold sent to the device.
	// +optional
	LastAppliedLowBatteryThreshold int32 `json:"lastAppliedLowBatteryThreshold,omitempty"`

	// lastAppliedHighBatteryThreshold is the latest high threshold sent to the device.
	// +optional
	LastAppliedHighBatteryThreshold int32 `json:"lastAppliedHighBatteryThreshold,omitempty"`

	// conditions represent the current state of the WasmFunction resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// WasmFunction is the Schema for the wasmfunctions API
type WasmFunction struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of WasmFunction
	// +required
	Spec WasmFunctionSpec `json:"spec"`

	// status defines the observed state of WasmFunction
	// +optional
	Status WasmFunctionStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WasmFunctionList contains a list of WasmFunction
type WasmFunctionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []WasmFunction `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WasmFunction{}, &WasmFunctionList{})
}
