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

	// release atomically binds the Wasm artifact, logical function identity, and
	// resource contract under one monotonically increasing generation.
	// +required
	Release ReleaseSpec `json:"release"`
}

// ReleaseSpec identifies one operator-authoritative function release.
type ReleaseSpec struct {
	// generation is monotonically increased whenever any release field changes.
	// +kubebuilder:validation:Minimum=1
	// +required
	Generation int64 `json:"generation"`

	// artifactDigest is the lowercase SHA-256 digest of the raw Wasm bytes.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	// +required
	ArtifactDigest string `json:"artifactDigest"`

	// functionIdentity is the stable logical guest name used for telemetry and
	// timing-profile selection.
	// +kubebuilder:validation:MinLength=1
	// +required
	FunctionIdentity string `json:"functionIdentity"`

	// resourceContract declares the inputs collected and outputs applied for this
	// release.
	// +required
	ResourceContract ResourceContractSpec `json:"resourceContract"`
}

// ResourceContractSpec describes the complete host/guest data contract.
type ResourceContractSpec struct {
	// +optional
	Inputs []ResourceInputSpec `json:"inputs,omitempty"`
	// +optional
	Outputs []ResourceOutputSpec `json:"outputs,omitempty"`
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
	// artifact source. The operator resolves the desired digest from this URL.
	// Edge host /wasm is synchronized from this source only when edge runtime
	// execution needs the artifact.
	// +optional
	OperatorWasmURL string `json:"operatorWasmURL,omitempty"`

	// artifactURL is the ESP32-reachable HTTP URL sent in stage_release metadata.
	// Prefer pointing this at the same operator-authoritative artifact as
	// operatorWasmURL through a device-reachable address.
	// +optional
	ArtifactURL string `json:"artifactURL,omitempty"`

	// batterySimulation configures the ESP32's deterministic demo-only battery
	// source. When enabled is omitted, the operator leaves the device battery
	// source unmanaged and real gauge telemetry remains authoritative.
	// +optional
	BatterySimulation BatterySimulationSpec `json:"batterySimulation,omitempty"`
}

// BatterySimulationSpec configures deterministic battery movement per Wasm
// invocation for short placement demonstrations.
type BatterySimulationSpec struct {
	// enabled selects simulated battery values when true and the real battery
	// gauge when false. Omission leaves the current device setting unmanaged.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// localDrainPercent is subtracted after every local Wasm invocation.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=10
	// +optional
	LocalDrainPercent *int32 `json:"localDrainPercent,omitempty"`

	// edgeRecoverPercent is added after every edge/hybrid Wasm invocation.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=10
	// +optional
	EdgeRecoverPercent *int32 `json:"edgeRecoverPercent,omitempty"`
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
	// The value 101 is reserved for validation runs that must retain remote
	// placement while a real battery gauge reports 100 percent.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=101
	// +optional
	HighBatteryThreshold int32 `json:"highBatteryThreshold,omitempty"`

	// batteryThreshold configures the classic low/high battery guardrail and
	// hysteresis policy. It is enabled by default for backwards compatibility.
	// +optional
	BatteryThreshold BatteryThresholdPolicySpec `json:"batteryThreshold,omitempty"`

	// deadline configures deadline-aware placement decisions.
	// +optional
	Deadline DeadlinePolicySpec `json:"deadline,omitempty"`

	// batteryDelta configures fast-drain placement decisions.
	// +optional
	BatteryDelta BatteryDeltaPolicySpec `json:"batteryDelta,omitempty"`
}

// BatteryThresholdPolicySpec configures battery threshold placement decisions.
type BatteryThresholdPolicySpec struct {
	// enabled turns on low/high battery threshold placement. Omitted means
	// enabled so existing resources keep their previous behavior.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

// DeadlinePolicySpec configures deadline-aware placement decisions.
type DeadlinePolicySpec struct {
	// enabled turns on deadline-aware placement. Omitted means enabled.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// targetMs is the application deadline budget for one function invocation.
	// When omitted, the operator uses the latest deadlineTargetMs reported by
	// device telemetry. If neither value is available, deadline placement waits.
	// +kubebuilder:validation:Minimum=1
	// +optional
	TargetMs *int32 `json:"targetMs,omitempty"`

	// minSlackMs is the minimum predicted slack required to consider a placement
	// deadline-safe.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinSlackMs int32 `json:"minSlackMs,omitempty"`

	// safetyMarginMs is added to each predicted finish time before slack is
	// evaluated.
	// +kubebuilder:validation:Minimum=0
	// +optional
	SafetyMarginMs int32 `json:"safetyMarginMs,omitempty"`

	// estimator configures the rolling telemetry estimator used for deadline
	// predictions.
	// +optional
	Estimator DeadlineEstimatorSpec `json:"estimator,omitempty"`
}

// DeadlineEstimatorSpec configures deadline telemetry estimates.
type DeadlineEstimatorSpec struct {
	// windowSeconds is the age limit for samples retained in the in-memory
	// estimator.
	// +kubebuilder:validation:Minimum=1
	// +optional
	WindowSeconds int32 `json:"windowSeconds,omitempty"`

	// minSamples is the number of samples required before using the configured
	// percentile instead of fallback values.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinSamples int32 `json:"minSamples,omitempty"`

	// percentile selects the sample percentile used for runtime estimates.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	Percentile int32 `json:"percentile,omitempty"`

	// fallbackLocalExecutionMs is used until enough local execution samples exist.
	// +kubebuilder:validation:Minimum=0
	// +optional
	FallbackLocalExecutionMs int32 `json:"fallbackLocalExecutionMs,omitempty"`

	// fallbackEdgeExecutionMs is used until enough edge execution samples exist.
	// +kubebuilder:validation:Minimum=0
	// +optional
	FallbackEdgeExecutionMs int32 `json:"fallbackEdgeExecutionMs,omitempty"`

	// fallbackNetworkRoundTripMs is used until enough network samples exist.
	// +kubebuilder:validation:Minimum=0
	// +optional
	FallbackNetworkRoundTripMs int32 `json:"fallbackNetworkRoundTripMs,omitempty"`

	// fallbackResourceCollectionMs is used until enough device resource collection
	// samples exist.
	// +kubebuilder:validation:Minimum=0
	// +optional
	FallbackResourceCollectionMs int32 `json:"fallbackResourceCollectionMs,omitempty"`
}

// BatteryDeltaPolicySpec configures battery-drain placement decisions.
type BatteryDeltaPolicySpec struct {
	// enabled turns on fast-drain placement. Omitted means enabled.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// windowSeconds is the telemetry window used to compare battery samples.
	// +kubebuilder:validation:Minimum=1
	// +optional
	WindowSeconds int32 `json:"windowSeconds,omitempty"`

	// maxDrainPercent forces edge placement when battery drops by at least this
	// percentage inside windowSeconds while the device is in local mode.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	MaxDrainPercent int32 `json:"maxDrainPercent,omitempty"`

	// riskyWindowsToOffload is the number of consecutive risky observations
	// required before proposing remote placement.
	// +kubebuilder:validation:Minimum=1
	// +optional
	RiskyWindowsToOffload int32 `json:"riskyWindowsToOffload,omitempty"`
}

// ResourceInputSpec describes a required input for Wasm execution.
type ResourceInputSpec struct {
	// name is the resource name, for example DHT.
	Name string `json:"name"`

	// locality declares where the input is available: device, edge, or portable.
	// +kubebuilder:validation:Enum=device;edge;portable
	// +optional
	Locality string `json:"locality,omitempty"`

	// keys are typed values collected from this resource.
	// +optional
	Keys []ResourceInputKeySpec `json:"keys,omitempty"`
}

// ResourceInputKeySpec describes one typed value collected from a resource.
type ResourceInputKeySpec struct {
	// name is the input key, for example temperature.
	Name string `json:"name"`

	// type is the value type exposed through the Wasm HAL.
	// +kubebuilder:validation:Enum=f32;i32;string;bool
	// +optional
	Type string `json:"type,omitempty"`
}

// ResourceOutputSpec describes a guest output and where it is applied.
type ResourceOutputSpec struct {
	// name is the guest output key, for example actuatorCommand.
	Name string `json:"name"`

	// type is the guest output value type.
	// +kubebuilder:validation:Enum=f32;i32;string;bool
	// +optional
	Type string `json:"type,omitempty"`

	// locality declares where the output is applied: device, edge, or portable.
	// +kubebuilder:validation:Enum=device;edge;portable
	// +optional
	Locality string `json:"locality,omitempty"`
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

	// hostStagedArtifactDigest is the release staged on the edge host but not
	// necessarily active yet.
	// +optional
	HostStagedArtifactDigest string `json:"hostStagedArtifactDigest,omitempty"`

	// deviceStagedArtifactDigest is the release staged on the ESP32 but not
	// necessarily active yet.
	// +optional
	DeviceStagedArtifactDigest string `json:"deviceStagedArtifactDigest,omitempty"`

	// selectedFunctionIdentity is the logical identity declared by spec.release.
	// +optional
	SelectedFunctionIdentity string `json:"selectedFunctionIdentity,omitempty"`

	// desiredReleaseGeneration is the generation selected by spec.release.
	// +optional
	DesiredReleaseGeneration int64 `json:"desiredReleaseGeneration,omitempty"`

	// acknowledgedReleaseGeneration is the latest active generation reported by
	// the device runtime.
	// +optional
	AcknowledgedReleaseGeneration int64 `json:"acknowledgedReleaseGeneration,omitempty"`

	// stagedReleaseGeneration is the latest staged generation reported by the
	// device runtime.
	// +optional
	StagedReleaseGeneration int64 `json:"stagedReleaseGeneration,omitempty"`

	// desiredPlacement is the placement selected by the control plane.
	// +optional
	DesiredPlacement string `json:"desiredPlacement,omitempty"`

	// placementReason explains why the current desired placement was selected.
	// +optional
	PlacementReason string `json:"placementReason,omitempty"`

	// proposalSource identifies the policy that proposed the candidate.
	// +optional
	ProposalSource string `json:"proposalSource,omitempty"`

	// proposedPlacement is the candidate after resource-locality transformation.
	// +optional
	ProposedPlacement string `json:"proposedPlacement,omitempty"`

	// predictedCandidateCostMs is the destination cost used for admission.
	// +optional
	PredictedCandidateCostMs *int32 `json:"predictedCandidateCostMs,omitempty"`

	// predictedCandidateSlackMs is the destination slack used for admission.
	// +optional
	PredictedCandidateSlackMs *int32 `json:"predictedCandidateSlackMs,omitempty"`

	// artifactReadinessReason summarizes current active and background staging.
	// +optional
	ArtifactReadinessReason string `json:"artifactReadinessReason,omitempty"`

	// artifactSyncStartedAt records when the current synchronization episode began.
	// +optional
	ArtifactSyncStartedAt *metav1.Time `json:"artifactSyncStartedAt,omitempty"`

	// artifactSyncRetryCount counts transient failures in the current episode.
	// +optional
	ArtifactSyncRetryCount int32 `json:"artifactSyncRetryCount,omitempty"`

	// observedBatteryPercent records the battery value used for placement.
	// +optional
	ObservedBatteryPercent *int32 `json:"observedBatteryPercent,omitempty"`

	// observedMode is the latest runtime mode reported by device telemetry.
	// +optional
	ObservedMode string `json:"observedMode,omitempty"`

	// observedAdmissionPaused reports whether the device scheduler is admitting
	// new invocations for this function.
	// +optional
	ObservedAdmissionPaused bool `json:"observedAdmissionPaused,omitempty"`

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

	// observedFunction is the latest function name reported by device telemetry.
	// +optional
	ObservedFunction string `json:"observedFunction,omitempty"`

	// observedBatteryDeltaPercent is the latest local-mode battery drop computed
	// by the operator inside the configured batteryDelta window.
	// +optional
	ObservedBatteryDeltaPercent *int32 `json:"observedBatteryDeltaPercent,omitempty"`

	// observedBatteryDeltaWindowSeconds is the window used for the latest battery
	// delta calculation.
	// +optional
	ObservedBatteryDeltaWindowSeconds *int32 `json:"observedBatteryDeltaWindowSeconds,omitempty"`

	// consecutiveRiskyWindows records confirmed drain observations.
	// +optional
	ConsecutiveRiskyWindows int32 `json:"consecutiveRiskyWindows,omitempty"`

	// lastTelemetryTime is when the latest device telemetry sample was observed.
	// +optional
	LastTelemetryTime *metav1.Time `json:"lastTelemetryTime,omitempty"`

	// lastCommandedPlacement is the last placement command sent to the device.
	// +optional
	LastCommandedPlacement string `json:"lastCommandedPlacement,omitempty"`

	// lastCommandedRuntimeMode is the concrete ESP32 runtime mode sent to the
	// device. For hybrid placement this is edge.
	// +optional
	LastCommandedRuntimeMode string `json:"lastCommandedRuntimeMode,omitempty"`

	// lastCommandTime is when the latest placement command was sent.
	// +optional
	LastCommandTime *metav1.Time `json:"lastCommandTime,omitempty"`

	// lastCommandID identifies the last idempotent runtime command.
	// +optional
	LastCommandID string `json:"lastCommandId,omitempty"`

	// lastStageCommandID identifies the last stage_release command.
	// +optional
	LastStageCommandID string `json:"lastStageCommandId,omitempty"`

	// lastStageCommandTime is when the latest stage_release command was sent.
	// +optional
	LastStageCommandTime *metav1.Time `json:"lastStageCommandTime,omitempty"`

	// stageCommandAttempts counts sends of the current idempotent stage command.
	// The first send is attempt one; retries use bounded exponential backoff.
	// +optional
	StageCommandAttempts int32 `json:"stageCommandAttempts,omitempty"`

	// releaseDeliveryState summarizes the end-to-end device release handshake.
	// Values are Pending, AwaitingStageAck, Staged, AwaitingActivationAck, and Active.
	// +optional
	ReleaseDeliveryState string `json:"releaseDeliveryState,omitempty"`

	// lastDeadlineDecisionID identifies the latest unsafe deadline observation.
	// +optional
	LastDeadlineDecisionID string `json:"lastDeadlineDecisionId,omitempty"`

	// lastSignaledDeadlineDecisionID is the last observation signalled on the device.
	// +optional
	LastSignaledDeadlineDecisionID string `json:"lastSignaledDeadlineDecisionId,omitempty"`

	// lastAppliedBatterySimulationCommandID identifies the latest demo battery
	// configuration published to the device. observedBatterySource is the
	// acknowledgement that the requested source is active.
	// +optional
	LastAppliedBatterySimulationCommandID string `json:"lastAppliedBatterySimulationCommandId,omitempty"`

	// lastBatterySimulationCommandTime is when the latest battery simulation
	// configuration was published. Fresh telemetry after this time is used to
	// acknowledge the command or trigger a bounded one-per-observation retry.
	// +optional
	LastBatterySimulationCommandTime *metav1.Time `json:"lastBatterySimulationCommandTime,omitempty"`

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
