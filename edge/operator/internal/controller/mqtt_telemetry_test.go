package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	edgev1alpha1 "github.com/sertayy/sif-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTelemetryBridgeKeepaliveTracksClientWrites(t *testing.T) {
	lastClientWrite := time.Unix(0, 0)
	if shouldSendTelemetryPing(lastClientWrite, lastClientWrite.Add(15*time.Second)) {
		t.Fatal("inbound telemetry must not reset the client-write keepalive")
	}
	if !shouldSendTelemetryPing(lastClientWrite, lastClientWrite.Add(mqttTelemetryPingInterval)) {
		t.Fatal("expected a ping after the client-write interval")
	}
}

func int64Ptr(value int64) *int64    { return &value }
func stringPtr(value string) *string { return &value }

func telemetryTestClient(t *testing.T, wf *edgev1alpha1.WasmFunction) (*mqttTelemetryBridge, types.NamespacedName) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := edgev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&edgev1alpha1.WasmFunction{}).
		WithObjects(wf).Build()
	return NewMQTTTelemetryBridge(client), types.NamespacedName{Name: wf.Name, Namespace: wf.Namespace}
}

func TestStateTelemetryAcknowledgesActiveAndStagedRelease(t *testing.T) {
	activeDigest := strings.Repeat("a", 64)
	stagedDigest := strings.Repeat("b", 64)
	wf := &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Release: edgev1alpha1.ReleaseSpec{
				Generation: 8, ArtifactDigest: stagedDigest,
				FunctionIdentity: "dht-reader",
			},
		},
	}
	bridge, key := telemetryTestClient(t, wf)
	battery := int32(42)
	result, err := bridge.updateObservedTelemetry(context.Background(), key, batteryTelemetry{
		Mode: placementEdge, Source: "real", ArtifactDigest: activeDigest,
		ReleaseGeneration: int64Ptr(7), StagedArtifactDigest: stringPtr(stagedDigest),
		StagedGeneration: int64Ptr(8), Function: "dht-reader",
	}, battery)
	if err != nil {
		t.Fatalf("update telemetry: %v", err)
	}
	if !result.stagedReleaseAcknowledged || result.activatedReleaseAcknowledged {
		t.Fatalf("acknowledgement result = %#v", result)
	}

	var updated edgev1alpha1.WasmFunction
	if err := bridge.client.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get updated function: %v", err)
	}
	if updated.Status.AcknowledgedReleaseGeneration != 7 || updated.Status.StagedReleaseGeneration != 8 {
		t.Fatalf("release generations = active %d staged %d", updated.Status.AcknowledgedReleaseGeneration, updated.Status.StagedReleaseGeneration)
	}
	if updated.Status.ObservedArtifactDigest != activeDigest || updated.Status.DeviceStagedArtifactDigest != stagedDigest {
		t.Fatalf("release digests = active %q staged %q", updated.Status.ObservedArtifactDigest, updated.Status.DeviceStagedArtifactDigest)
	}
}

func TestStateTelemetryClearsStagedReleaseAfterActivation(t *testing.T) {
	digest := strings.Repeat("c", 64)
	wf := &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{Release: edgev1alpha1.ReleaseSpec{
			Generation: 9, ArtifactDigest: digest, FunctionIdentity: "dht-reader",
		}},
		Status: edgev1alpha1.WasmFunctionStatus{
			StagedReleaseGeneration: 9, DeviceStagedArtifactDigest: digest,
		},
	}
	bridge, key := telemetryTestClient(t, wf)
	battery := int32(50)
	result, err := bridge.updateObservedTelemetry(context.Background(), key, batteryTelemetry{
		Mode: placementLocal, ArtifactDigest: digest, Function: "dht-reader",
		ReleaseGeneration: int64Ptr(9), StagedGeneration: int64Ptr(0),
		StagedArtifactDigest: stringPtr(""),
	}, battery)
	if err != nil {
		t.Fatalf("update telemetry: %v", err)
	}
	if !result.activatedReleaseAcknowledged {
		t.Fatalf("active acknowledgement result = %#v", result)
	}
	var updated edgev1alpha1.WasmFunction
	if err := bridge.client.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get updated function: %v", err)
	}
	if updated.Status.StagedReleaseGeneration != 0 || updated.Status.DeviceStagedArtifactDigest != "" {
		t.Fatalf("staged state not cleared: generation=%d digest=%q", updated.Status.StagedReleaseGeneration, updated.Status.DeviceStagedArtifactDigest)
	}
	if !deviceReleaseReady(&updated, digest) {
		t.Fatal("active release should satisfy device readiness after staged state clears")
	}
}

func drainTestFunction() *edgev1alpha1.WasmFunction {
	battery := int32(70)
	return &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Release: edgev1alpha1.ReleaseSpec{FunctionIdentity: "dht-reader"},
			Placement: edgev1alpha1.PlacementSpec{
				LowBatteryThreshold: 20, HighBatteryThreshold: 80,
				BatteryDelta: edgev1alpha1.BatteryDeltaPolicySpec{
					Enabled: boolPtr(true), WindowSeconds: 60,
					MaxDrainPercent: 3, RiskyWindowsToOffload: 2,
				},
			},
		},
		Status: edgev1alpha1.WasmFunctionStatus{
			ObservedMode: placementLocal, ObservedBatterySource: "real",
			ObservedBatteryPercent: &battery, ObservedFunction: "dht-reader",
		},
	}
}

func TestRollingDrainRequiresTwoConsecutiveRiskyWindows(t *testing.T) {
	bridge := &mqttTelemetryBridge{drainTracker: newBatteryDrainTracker()}
	wf := drainTestFunction()
	key := types.NamespacedName{Name: wf.Name, Namespace: wf.Namespace}
	base := time.Unix(100, 0)
	result := telemetryUpdateResult{}

	bridge.updateBatteryDeltaStatus(key, wf, batteryTelemetry{Mode: placementLocal, Source: "real", Function: "dht-reader"}, 70, base, &result)
	bridge.updateBatteryDeltaStatus(key, wf, batteryTelemetry{Mode: placementLocal, Source: "real", Function: "dht-reader"}, 66, base.Add(10*time.Second), &result)
	if wf.Status.ConsecutiveRiskyWindows != 1 {
		t.Fatalf("risky windows = %d, want 1", wf.Status.ConsecutiveRiskyWindows)
	}
	if localBatteryDeltaForcesEdge(wf, 80) {
		t.Fatal("one risky window must not propose remote")
	}
	bridge.updateBatteryDeltaStatus(key, wf, batteryTelemetry{Mode: placementLocal, Source: "real", Function: "dht-reader"}, 62, base.Add(20*time.Second), &result)
	if wf.Status.ConsecutiveRiskyWindows != 2 || !localBatteryDeltaForcesEdge(wf, 80) {
		t.Fatalf("confirmed drain = windows %d delta %v", wf.Status.ConsecutiveRiskyWindows, wf.Status.ObservedBatteryDeltaPercent)
	}

	resolveForTest(wf)
	resolveForTest(wf)
	if wf.Status.ConsecutiveRiskyWindows != 2 {
		t.Fatal("reconciliation must not advance drain observations")
	}
}

func TestRollingDrainResetsOnSourceModeFunctionAndGap(t *testing.T) {
	resets := []struct {
		name      string
		telemetry batteryTelemetry
		at        time.Duration
	}{
		{name: "source", telemetry: batteryTelemetry{Mode: placementLocal, Source: "simulated", Function: "dht-reader"}, at: 20 * time.Second},
		{name: "mode", telemetry: batteryTelemetry{Mode: placementEdge, Source: "real", Function: "dht-reader"}, at: 20 * time.Second},
		{name: "function", telemetry: batteryTelemetry{Mode: placementLocal, Source: "real", Function: "hybrid-resource-demo"}, at: 20 * time.Second},
		{name: "gap", telemetry: batteryTelemetry{Mode: placementLocal, Source: "real", Function: "dht-reader"}, at: 80 * time.Second},
	}
	for _, test := range resets {
		t.Run(test.name, func(t *testing.T) {
			bridge := &mqttTelemetryBridge{drainTracker: newBatteryDrainTracker()}
			wf := drainTestFunction()
			key := types.NamespacedName{Name: wf.Name, Namespace: wf.Namespace}
			base := time.Unix(100, 0)
			result := telemetryUpdateResult{}
			baseTelemetry := batteryTelemetry{Mode: placementLocal, Source: "real", Function: "dht-reader"}
			bridge.updateBatteryDeltaStatus(key, wf, baseTelemetry, 70, base, &result)
			bridge.updateBatteryDeltaStatus(key, wf, baseTelemetry, 66, base.Add(10*time.Second), &result)
			bridge.updateBatteryDeltaStatus(key, wf, test.telemetry, 62, base.Add(test.at), &result)
			if wf.Status.ConsecutiveRiskyWindows != 0 || wf.Status.ObservedBatteryDeltaPercent != nil {
				t.Fatalf("status after reset = windows %d delta %v", wf.Status.ConsecutiveRiskyWindows, wf.Status.ObservedBatteryDeltaPercent)
			}
		})
	}
}

func TestRollingDrainSuppressedAtThresholds(t *testing.T) {
	bridge := &mqttTelemetryBridge{drainTracker: newBatteryDrainTracker()}
	wf := drainTestFunction()
	key := types.NamespacedName{Name: wf.Name, Namespace: wf.Namespace}
	result := telemetryUpdateResult{}
	telemetry := batteryTelemetry{Mode: placementLocal, Source: "real", Function: "dht-reader"}
	bridge.updateBatteryDeltaStatus(key, wf, telemetry, 90, time.Unix(100, 0), &result)
	bridge.updateBatteryDeltaStatus(key, wf, telemetry, 10, time.Unix(110, 0), &result)
	if wf.Status.ConsecutiveRiskyWindows != 0 || wf.Status.ObservedBatteryDeltaPercent != nil {
		t.Fatalf("threshold-suppressed drain = windows %d delta %v", wf.Status.ConsecutiveRiskyWindows, wf.Status.ObservedBatteryDeltaPercent)
	}
}

func TestDeadlineEstimatorKeepsFunctionsSeparate(t *testing.T) {
	estimator := newDeadlineTelemetryEstimator(testDeadlineProfiles())
	wf := drainTestFunction()
	wf.Spec.Placement.Deadline.Estimator.MinSamples = 1
	execution := int32(12)
	estimator.recordAndEstimate(wf, batteryTelemetry{Mode: placementLocal, Function: "dht-reader", ExecutionMs: &execution}, time.Unix(100, 0))
	snapshot := estimator.estimate(wf)
	if snapshot.LocalExecutionMs != 12 {
		t.Fatalf("dht estimate = %d, want 12", snapshot.LocalExecutionMs)
	}
	wf.Spec.Release.FunctionIdentity = "hybrid-resource-demo"
	snapshot = estimator.estimate(wf)
	if snapshot.LocalExecutionMs != 100 {
		t.Fatalf("hybrid estimate = %d, want profile 100", snapshot.LocalExecutionMs)
	}
}

func TestDeadlineProfilesLoadFromConfiguredFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	data := []byte(`{"dht-reader":{"localExecutionMs":51,"localQueueDelayMs":201,"resourceWakeMs":1,"edgeExecutionMs":6,"networkRoundTripMs":101,"localInputCollectionMs":21,"localOutputApplicationMs":2}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write profiles: %v", err)
	}
	t.Setenv("SIF_DEADLINE_PROFILES_PATH", path)
	estimator := NewDeadlineTelemetryEstimator()
	wf := drainTestFunction()
	snapshot := estimator.estimate(wf)
	if !snapshot.Available || snapshot.LocalExecutionMs != 51 ||
		snapshot.OutputApplicationMs != 2 {
		t.Fatalf("loaded snapshot = %#v", snapshot)
	}
}
