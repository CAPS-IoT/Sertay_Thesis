package controller

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	edgev1alpha1 "github.com/sertayy/sif-operator/api/v1alpha1"
)

type DeadlineTelemetryEstimator struct {
	mu       sync.Mutex
	samples  map[string][]deadlineTelemetrySample
	profiles map[string]deadlineProfile
}

type deadlineTelemetrySample struct {
	At time.Time

	LocalExecutionMs     *int32
	LocalQueueDelayMs    *int32
	ResourceWakeMs       *int32
	EdgeExecutionMs      *int32
	NetworkRoundTripMs   *int32
	ResourceCollectionMs *int32
	OutputApplicationMs  *int32
}

type deadlineEstimateSnapshot struct {
	IdentityKnown        bool
	Available            bool
	LocalExecutionMs     int32
	LocalQueueDelayMs    int32
	ResourceWakeMs       int32
	EdgeExecutionMs      int32
	NetworkRoundTripMs   int32
	ResourceCollectionMs int32
	OutputApplicationMs  int32
	HybridRoundTripMs    int32
	SampleCount          int32
}

type deadlineProfile struct {
	LocalExecutionMs     int32 `json:"localExecutionMs"`
	LocalQueueDelayMs    int32 `json:"localQueueDelayMs"`
	ResourceWakeMs       int32 `json:"resourceWakeMs"`
	EdgeExecutionMs      int32 `json:"edgeExecutionMs"`
	NetworkRoundTripMs   int32 `json:"networkRoundTripMs"`
	ResourceCollectionMs int32 `json:"localInputCollectionMs"`
	OutputApplicationMs  int32 `json:"localOutputApplicationMs"`
}

var knownFunctionIdentities = map[string]struct{}{
	"basic-edge-demo":      {},
	"dht-reader":           {},
	"hybrid-resource-demo": {},
}

func newDeadlineTelemetryEstimator(profileSets ...map[string]deadlineProfile) *DeadlineTelemetryEstimator {
	profiles := loadDeadlineProfilesFromEnvironment()
	if len(profileSets) > 0 {
		profiles = profileSets[0]
	}
	return &DeadlineTelemetryEstimator{
		samples:  map[string][]deadlineTelemetrySample{},
		profiles: profiles,
	}
}

func NewDeadlineTelemetryEstimator() *DeadlineTelemetryEstimator {
	return newDeadlineTelemetryEstimator()
}

func loadDeadlineProfilesFromEnvironment() map[string]deadlineProfile {
	path := strings.TrimSpace(os.Getenv("SIF_DEADLINE_PROFILES_PATH"))
	if path == "" {
		path = "/etc/sif/deadline-profiles/profiles.json"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	profiles := map[string]deadlineProfile{}
	if err := json.Unmarshal(data, &profiles); err != nil {
		return nil
	}
	for identity, profile := range profiles {
		if _, known := knownFunctionIdentities[identity]; !known || !validDeadlineProfile(profile) {
			delete(profiles, identity)
		}
	}
	return profiles
}

func validDeadlineProfile(profile deadlineProfile) bool {
	return profile.LocalExecutionMs >= 0 && profile.LocalQueueDelayMs >= 0 &&
		profile.ResourceWakeMs >= 0 && profile.EdgeExecutionMs >= 0 &&
		profile.NetworkRoundTripMs >= 0 && profile.ResourceCollectionMs >= 0 &&
		profile.OutputApplicationMs >= 0
}

func (e *DeadlineTelemetryEstimator) recordAndEstimate(wf *edgev1alpha1.WasmFunction, telemetry batteryTelemetry, now time.Time) deadlineEstimateSnapshot {
	if e == nil {
		return unavailableDeadlineEstimate(wf)
	}
	function := deadlineEstimatorFunction(wf, telemetry)
	if function == "" {
		return unavailableDeadlineEstimate(wf)
	}
	prefix := deadlineEstimatorKeyPrefix(wf.Namespace, wf.Name, function)
	mode := strings.ToLower(strings.TrimSpace(telemetry.Mode))
	window := time.Duration(effectiveDeadlineEstimatorWindowSeconds(wf)) * time.Second
	cutoff := now.Add(-window)

	e.mu.Lock()
	defer e.mu.Unlock()

	if sample, ok := deadlineSampleFromTelemetry(mode, telemetry, now); ok {
		key := deadlineEstimatorKey(prefix, mode)
		e.samples[key] = append(e.samples[key], sample)
	}

	for key, samples := range e.samples {
		if !strings.HasPrefix(key, prefix+"/") {
			continue
		}
		kept := samples[:0]
		for _, sample := range samples {
			if !sample.At.Before(cutoff) {
				kept = append(kept, sample)
			}
		}
		if len(kept) == 0 {
			delete(e.samples, key)
			continue
		}
		e.samples[key] = kept
	}

	return e.snapshotLocked(wf, prefix)
}

func (e *DeadlineTelemetryEstimator) snapshotLocked(wf *edgev1alpha1.WasmFunction, prefix string) deadlineEstimateSnapshot {
	fallback := e.profileEstimate(wf)
	if !fallback.Available {
		return fallback
	}
	localSamples := e.samples[deadlineEstimatorKey(prefix, placementLocal)]
	edgeSamples := e.samples[deadlineEstimatorKey(prefix, placementEdge)]
	allCount := int32(len(localSamples) + len(edgeSamples))
	minSamples := int(effectiveDeadlineEstimatorMinSamples(wf))
	percentile := effectiveDeadlineEstimatorPercentile(wf)

	localExecution := estimateDeadlinePercentile(localSamples, func(sample deadlineTelemetrySample) *int32 { return sample.LocalExecutionMs }, minSamples, percentile, fallback.LocalExecutionMs)
	localQueue := estimateDeadlinePercentile(localSamples, func(sample deadlineTelemetrySample) *int32 { return sample.LocalQueueDelayMs }, minSamples, percentile, fallback.LocalQueueDelayMs)
	resourceWake := estimateDeadlinePercentile(localSamples, func(sample deadlineTelemetrySample) *int32 { return sample.ResourceWakeMs }, minSamples, percentile, fallback.ResourceWakeMs)
	edgeExecution := estimateDeadlinePercentile(edgeSamples, func(sample deadlineTelemetrySample) *int32 { return sample.EdgeExecutionMs }, minSamples, percentile, fallback.EdgeExecutionMs)
	networkRoundTrip := estimateDeadlinePercentile(edgeSamples, func(sample deadlineTelemetrySample) *int32 { return sample.NetworkRoundTripMs }, minSamples, percentile, fallback.NetworkRoundTripMs)
	resourceCollection := estimateDeadlinePercentile(edgeSamples, func(sample deadlineTelemetrySample) *int32 { return sample.ResourceCollectionMs }, minSamples, percentile, fallback.ResourceCollectionMs)
	outputApplication := estimateDeadlinePercentile(edgeSamples, func(sample deadlineTelemetrySample) *int32 { return sample.OutputApplicationMs }, minSamples, percentile, fallback.OutputApplicationMs)

	return deadlineEstimateSnapshot{
		IdentityKnown:        true,
		Available:            true,
		LocalExecutionMs:     localExecution,
		LocalQueueDelayMs:    localQueue,
		ResourceWakeMs:       resourceWake,
		EdgeExecutionMs:      edgeExecution,
		NetworkRoundTripMs:   networkRoundTrip,
		ResourceCollectionMs: resourceCollection,
		OutputApplicationMs:  outputApplication,
		HybridRoundTripMs:    resourceCollection + networkRoundTrip + edgeExecution + outputApplication,
		SampleCount:          allCount,
	}
}

func (e *DeadlineTelemetryEstimator) profileEstimate(wf *edgev1alpha1.WasmFunction) deadlineEstimateSnapshot {
	identity := strings.TrimSpace(wf.Spec.Release.FunctionIdentity)
	_, known := knownFunctionIdentities[identity]
	profile, ok := e.profiles[identity]
	if !ok {
		return deadlineEstimateSnapshot{IdentityKnown: known}
	}
	localExecution := profile.LocalExecutionMs
	edgeExecution := profile.EdgeExecutionMs
	networkRoundTrip := profile.NetworkRoundTripMs
	resourceCollection := profile.ResourceCollectionMs
	if wf.Spec.Placement.Deadline.Estimator.FallbackLocalExecutionMs > 0 {
		localExecution = wf.Spec.Placement.Deadline.Estimator.FallbackLocalExecutionMs
	}
	if wf.Spec.Placement.Deadline.Estimator.FallbackEdgeExecutionMs > 0 {
		edgeExecution = wf.Spec.Placement.Deadline.Estimator.FallbackEdgeExecutionMs
	}
	if wf.Spec.Placement.Deadline.Estimator.FallbackNetworkRoundTripMs > 0 {
		networkRoundTrip = wf.Spec.Placement.Deadline.Estimator.FallbackNetworkRoundTripMs
	}
	if wf.Spec.Placement.Deadline.Estimator.FallbackResourceCollectionMs > 0 {
		resourceCollection = wf.Spec.Placement.Deadline.Estimator.FallbackResourceCollectionMs
	}
	return deadlineEstimateSnapshot{
		IdentityKnown:        true,
		Available:            true,
		LocalExecutionMs:     localExecution,
		LocalQueueDelayMs:    profile.LocalQueueDelayMs,
		ResourceWakeMs:       profile.ResourceWakeMs,
		EdgeExecutionMs:      edgeExecution,
		NetworkRoundTripMs:   networkRoundTrip,
		ResourceCollectionMs: resourceCollection,
		OutputApplicationMs:  profile.OutputApplicationMs,
		HybridRoundTripMs:    resourceCollection + networkRoundTrip + edgeExecution + profile.OutputApplicationMs,
	}
}

func unavailableDeadlineEstimate(wf *edgev1alpha1.WasmFunction) deadlineEstimateSnapshot {
	_, known := knownFunctionIdentities[strings.TrimSpace(wf.Spec.Release.FunctionIdentity)]
	return deadlineEstimateSnapshot{IdentityKnown: known}
}

func deadlineEstimatorFunction(wf *edgev1alpha1.WasmFunction, telemetry batteryTelemetry) string {
	selected := strings.TrimSpace(wf.Spec.Release.FunctionIdentity)
	if selected == "" {
		return ""
	}
	if telemetry.Function != "" && telemetry.Function != selected {
		return ""
	}
	return selected
}

func deadlineEstimatorKeyPrefix(namespace string, name string, function string) string {
	return namespace + "/" + name + "/" + function
}

func deadlineEstimatorKey(prefix string, mode string) string {
	return prefix + "/" + mode
}

func deadlineSampleFromTelemetry(mode string, telemetry batteryTelemetry, now time.Time) (deadlineTelemetrySample, bool) {
	sample := deadlineTelemetrySample{At: now}
	switch mode {
	case placementLocal:
		sample.LocalExecutionMs = copyInt32Ptr(telemetry.ExecutionMs)
		sample.LocalQueueDelayMs = copyInt32Ptr(telemetry.QueueDelayMs)
		sample.ResourceWakeMs = copyInt32Ptr(telemetry.ResourceWakeMs)
	case placementEdge:
		sample.EdgeExecutionMs = copyInt32Ptr(telemetry.EdgeExecutionMs)
		if sample.EdgeExecutionMs == nil && telemetry.NetworkRoundTripMs == nil {
			sample.EdgeExecutionMs = copyInt32Ptr(telemetry.ExecutionMs)
		}
		sample.NetworkRoundTripMs = copyInt32Ptr(telemetry.NetworkRoundTripMs)
		sample.ResourceCollectionMs = copyInt32Ptr(telemetry.ResourceCollectionMs)
		sample.OutputApplicationMs = copyInt32Ptr(telemetry.OutputApplicationMs)
	default:
		return deadlineTelemetrySample{}, false
	}
	return sample, sample.hasDeadlineValue()
}

func (sample deadlineTelemetrySample) hasDeadlineValue() bool {
	return sample.LocalExecutionMs != nil ||
		sample.LocalQueueDelayMs != nil ||
		sample.ResourceWakeMs != nil ||
		sample.EdgeExecutionMs != nil ||
		sample.NetworkRoundTripMs != nil ||
		sample.ResourceCollectionMs != nil ||
		sample.OutputApplicationMs != nil
}

func (e *DeadlineTelemetryEstimator) estimate(wf *edgev1alpha1.WasmFunction) deadlineEstimateSnapshot {
	if e == nil {
		return unavailableDeadlineEstimate(wf)
	}
	identity := strings.TrimSpace(wf.Spec.Release.FunctionIdentity)
	if identity == "" {
		return unavailableDeadlineEstimate(wf)
	}
	prefix := deadlineEstimatorKeyPrefix(wf.Namespace, wf.Name, identity)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotLocked(wf, prefix)
}

func estimateDeadlinePercentile(samples []deadlineTelemetrySample, valueFor func(deadlineTelemetrySample) *int32, minSamples int, percentile int32, fallback int32) int32 {
	values := make([]int32, 0, len(samples))
	for _, sample := range samples {
		value := valueFor(sample)
		if value == nil || *value < 0 {
			continue
		}
		values = append(values, *value)
	}
	if len(values) < minSamples {
		return fallback
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	index := int((int64(percentile)*int64(len(values)) + 99) / 100)
	if index < 1 {
		index = 1
	}
	if index > len(values) {
		index = len(values)
	}
	return values[index-1]
}
