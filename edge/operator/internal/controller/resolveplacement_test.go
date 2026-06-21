package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	edgev1alpha1 "github.com/sertayy/sif-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func boolPtr(value bool) *bool { return &value }

func testDeadlineProfiles() map[string]deadlineProfile {
	return map[string]deadlineProfile{
		"dht-reader": {
			LocalExecutionMs: 50, LocalQueueDelayMs: 200, ResourceWakeMs: 0,
			EdgeExecutionMs: 5, NetworkRoundTripMs: 100,
			ResourceCollectionMs: 20, OutputApplicationMs: 0,
		},
		"hybrid-resource-demo": {
			LocalExecutionMs: 100, LocalQueueDelayMs: 200, ResourceWakeMs: 0,
			EdgeExecutionMs: 5, NetworkRoundTripMs: 100,
			ResourceCollectionMs: 50, OutputApplicationMs: 2,
		},
	}
}

func resolveForTest(wf *edgev1alpha1.WasmFunction) placementDecision {
	estimator := newDeadlineTelemetryEstimator(testDeadlineProfiles())
	return resolvePlacementWithEstimates(wf, estimator.estimate(wf))
}

func admissionFixture(current, identity string, target int32, deviceLocal bool) *edgev1alpha1.WasmFunction {
	battery := int32(70)
	contract := edgev1alpha1.ResourceContractSpec{}
	if deviceLocal {
		contract.Inputs = []edgev1alpha1.ResourceInputSpec{{
			Name: "DHT", Locality: "device",
			Keys: []edgev1alpha1.ResourceInputKeySpec{{Name: "temperature", Type: "f32"}},
		}}
		contract.Outputs = []edgev1alpha1.ResourceOutputSpec{{Name: "actuatorCommand", Type: "i32", Locality: "device"}}
	}
	return &edgev1alpha1.WasmFunction{
		Spec: edgev1alpha1.WasmFunctionSpec{
			Device: edgev1alpha1.DeviceSpec{ControlTopic: "device/control"},
			Release: edgev1alpha1.ReleaseSpec{
				Generation: 1, FunctionIdentity: identity,
				ResourceContract: contract,
			},
			Placement: edgev1alpha1.PlacementSpec{
				Mode: placementAuto, LowBatteryThreshold: 20, HighBatteryThreshold: 80,
				Deadline: edgev1alpha1.DeadlinePolicySpec{
					Enabled: boolPtr(true), TargetMs: &target, MinSlackMs: 100, SafetyMarginMs: 1,
				},
				BatteryDelta: edgev1alpha1.BatteryDeltaPolicySpec{
					Enabled: boolPtr(true), WindowSeconds: 60, MaxDrainPercent: 3, RiskyWindowsToOffload: 2,
				},
			},
		},
		Status: edgev1alpha1.WasmFunctionStatus{
			DesiredPlacement: current, ObservedMode: runtimeModeForPlacement(current),
			ObservedBatteryPercent: &battery, ObservedFunction: identity,
		},
	}
}

func TestDestinationOnlyDeadlineAdmissionDirections(t *testing.T) {
	tests := []struct {
		name        string
		current     string
		identity    string
		deviceLocal bool
		target      int32
		battery     int32
		drain       bool
		candidate   string
		accepted    bool
	}{
		{name: "hybrid to local rejected", current: placementHybrid, identity: "hybrid-resource-demo", deviceLocal: true, target: 350, battery: 90, candidate: placementLocal},
		{name: "edge to local rejected", current: placementEdge, identity: "dht-reader", target: 300, battery: 90, candidate: placementLocal},
		{name: "local to edge rejected", current: placementLocal, identity: "dht-reader", target: 150, battery: 70, drain: true, candidate: placementEdge},
		{name: "local to hybrid rejected", current: placementLocal, identity: "hybrid-resource-demo", deviceLocal: true, target: 200, battery: 70, drain: true, candidate: placementHybrid},
		{name: "hybrid to local accepted", current: placementHybrid, identity: "hybrid-resource-demo", deviceLocal: true, target: 1000, battery: 90, candidate: placementLocal, accepted: true},
		{name: "edge to local accepted", current: placementEdge, identity: "dht-reader", target: 1000, battery: 90, candidate: placementLocal, accepted: true},
		{name: "local to edge accepted", current: placementLocal, identity: "dht-reader", target: 1000, battery: 70, drain: true, candidate: placementEdge, accepted: true},
		{name: "local to hybrid accepted", current: placementLocal, identity: "hybrid-resource-demo", deviceLocal: true, target: 1000, battery: 70, drain: true, candidate: placementHybrid, accepted: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wf := admissionFixture(test.current, test.identity, test.target, test.deviceLocal)
			*wf.Status.ObservedBatteryPercent = test.battery
			if test.drain {
				delta := int32(4)
				wf.Status.ObservedBatteryDeltaPercent = &delta
				wf.Status.ConsecutiveRiskyWindows = 2
			}
			decision := resolveForTest(wf)
			if decision.Candidate != test.candidate {
				t.Fatalf("candidate = %q, want %q", decision.Candidate, test.candidate)
			}
			if decision.Deadline.Accepted != test.accepted {
				t.Fatalf("accepted = %t, want %t (%#v)", decision.Deadline.Accepted, test.accepted, decision.Deadline.Condition)
			}
			wantDesired := test.current
			if test.accepted {
				wantDesired = test.candidate
			}
			if decision.Desired != wantDesired {
				t.Fatalf("desired = %q, want %q", decision.Desired, wantDesired)
			}
		})
	}
}

func TestDeadlineAbstainsWithoutTargetOrEstimates(t *testing.T) {
	target := int32(1000)
	wf := admissionFixture(placementLocal, "dht-reader", target, false)
	wf.Spec.Placement.Mode = placementEdge

	wf.Spec.Placement.Deadline.TargetMs = nil
	decision := resolveForTest(wf)
	if !decision.Deadline.Accepted || decision.Deadline.Condition == nil || decision.Deadline.Condition.Reason != "TargetUnavailable" {
		t.Fatalf("target abstention = %#v", decision.Deadline)
	}

	wf.Spec.Placement.Deadline.TargetMs = &target
	evaluation := evaluateDeadlineAdmission(wf, placementEdge, false, deadlineEstimateSnapshot{IdentityKnown: true})
	if !evaluation.Accepted || evaluation.Condition == nil || evaluation.Condition.Reason != "EstimatesUnavailable" {
		t.Fatalf("estimate abstention = %#v", evaluation)
	}
}

func TestUnknownAndMismatchedIdentityBlockSoftTransitions(t *testing.T) {
	target := int32(1000)
	wf := admissionFixture(placementLocal, "unknown-function", target, false)
	wf.Spec.Placement.Mode = placementEdge
	decision := resolveForTest(wf)
	if decision.Deadline.Accepted || decision.Deadline.Condition == nil || decision.Deadline.Condition.Reason != "FunctionIdentityUnknown" {
		t.Fatalf("unknown identity = %#v", decision.Deadline)
	}

	wf = admissionFixture(placementLocal, "dht-reader", target, false)
	wf.Spec.Placement.Mode = placementEdge
	wf.Status.ObservedFunction = "hybrid-resource-demo"
	decision = resolveForTest(wf)
	if decision.Deadline.Accepted || decision.Deadline.Condition == nil || decision.Deadline.Condition.Reason != "FunctionIdentityMismatch" {
		t.Fatalf("identity mismatch = %#v", decision.Deadline)
	}
}

func TestLowBatteryOverridesUnsafeDeadlineAfterLocality(t *testing.T) {
	target := int32(100)
	wf := admissionFixture(placementLocal, "hybrid-resource-demo", target, true)
	battery := int32(10)
	wf.Status.ObservedBatteryPercent = &battery
	decision := resolveForTest(wf)
	if !decision.Hard || decision.Candidate != placementHybrid || decision.Desired != placementHybrid {
		t.Fatalf("forced decision = %#v", decision)
	}
	if decision.Deadline.Condition == nil || decision.Deadline.Condition.Reason != "ForcedBatteryOverride" {
		t.Fatalf("deadline override = %#v", decision.Deadline)
	}
}

func TestHighThreshold101RetainsEdgeAtFullBattery(t *testing.T) {
	target := int32(1000)
	wf := admissionFixture(placementLocal, "dht-reader", target, false)
	battery := int32(100)
	wf.Status.ObservedBatteryPercent = &battery
	wf.Spec.Placement.LowBatteryThreshold = 100
	wf.Spec.Placement.HighBatteryThreshold = 101

	decision := resolveForTest(wf)
	if !decision.Hard || decision.ProposalSource != "LowBatteryThreshold" ||
		decision.Desired != placementEdge {
		t.Fatalf("local decision = %#v, want forced edge", decision)
	}

	wf.Status.DesiredPlacement = placementEdge
	wf.Status.ObservedMode = placementEdge
	decision = resolveForTest(wf)
	if decision.Proposal != "" || decision.Desired != placementEdge ||
		decision.Reason != "retaining accepted edge placement" {
		t.Fatalf("edge decision = %#v, want retained edge", decision)
	}
}

func TestOutputLocalityAloneRequiresHybrid(t *testing.T) {
	wf := &edgev1alpha1.WasmFunction{Spec: edgev1alpha1.WasmFunctionSpec{
		Release: edgev1alpha1.ReleaseSpec{ResourceContract: edgev1alpha1.ResourceContractSpec{
			Outputs: []edgev1alpha1.ResourceOutputSpec{{Name: "actuatorCommand", Type: "i32", Locality: "device"}},
		}},
	}}
	if got := applyResourceLocality(wf, placementEdge); got != placementHybrid {
		t.Fatalf("placement = %q, want hybrid", got)
	}
}

func TestReleaseWithoutDeviceResourcesNormalizesHybridToEdge(t *testing.T) {
	target := int32(1000)
	wf := admissionFixture(placementHybrid, "basic-edge-demo", target, false)
	wf.Status.ObservedMode = placementEdge
	decision := resolveForTest(wf)
	if decision.Current != placementEdge || decision.Desired != placementEdge {
		t.Fatalf("placement = current %q desired %q, want edge", decision.Current, decision.Desired)
	}
	if decision.RuntimeMode != placementEdge || decision.Transition {
		t.Fatalf("runtime=%q transition=%t, want unchanged edge runtime", decision.RuntimeMode, decision.Transition)
	}
}

func TestPoliciesDefaultEnabledButExplicitFalseIsPreserved(t *testing.T) {
	wf := &edgev1alpha1.WasmFunction{}
	if !effectiveBatteryThresholdEnabled(wf) || !effectiveDeadlineEnabled(wf) || !effectiveBatteryDeltaEnabled(wf) {
		t.Fatal("omitted policies must default enabled")
	}
	wf.Spec.Placement.BatteryThreshold.Enabled = boolPtr(false)
	wf.Spec.Placement.Deadline.Enabled = boolPtr(false)
	wf.Spec.Placement.BatteryDelta.Enabled = boolPtr(false)
	if effectiveBatteryThresholdEnabled(wf) || effectiveDeadlineEnabled(wf) || effectiveBatteryDeltaEnabled(wf) {
		t.Fatal("explicit false must disable each policy")
	}
}

func TestDeadlineRejectionSignalIsDeduplicatedWithoutPlacementMutation(t *testing.T) {
	target := int32(150)
	wf := admissionFixture(placementLocal, "dht-reader", target, false)
	wf.Spec.Release.Generation = 4
	wf.Spec.Release.ArtifactDigest = strings.Repeat("a", 64)
	wf.Status.LastCommandedPlacement = placementEdge
	wf.Status.LastCommandedRuntimeMode = placementEdge
	wf.Status.StagedReleaseGeneration = 4
	wf.Status.DeviceStagedArtifactDigest = wf.Spec.Release.ArtifactDigest
	wf.Status.ArtifactReadinessReason = "DestinationReady"
	telemetryTime := metav1.NewTime(time.Unix(123, 0))
	wf.Status.LastTelemetryTime = &telemetryTime
	delta := int32(4)
	wf.Status.ObservedBatteryDeltaPercent = &delta
	wf.Status.ConsecutiveRiskyWindows = 2
	decision := resolveForTest(wf)
	if decision.Deadline.Condition == nil || decision.Deadline.Condition.Reason != "CandidateDeadlineUnsafe" {
		t.Fatalf("decision = %#v", decision.Deadline)
	}

	var published []map[string]interface{}
	reconciler := &WasmFunctionReconciler{PublishControl: func(_ context.Context, topic string, payload interface{}) error {
		if topic != "device/control" {
			t.Fatalf("topic = %q", topic)
		}
		published = append(published, payload.(map[string]interface{}))
		return nil
	}}
	if err := reconciler.applyDeadlineRejectionSignal(context.Background(), wf, "device/control", decision); err != nil {
		t.Fatalf("first signal: %v", err)
	}
	if err := reconciler.applyDeadlineRejectionSignal(context.Background(), wf, "device/control", decision); err != nil {
		t.Fatalf("duplicate signal: %v", err)
	}
	if len(published) != 1 {
		t.Fatalf("published %d signals, want 1", len(published))
	}
	if published[0]["action"] != "signal_deadline_rejection" || published[0]["decisionId"] == "" {
		t.Fatalf("payload = %#v", published[0])
	}
	commanded, err := reconciler.applyPlacementCommand(context.Background(), wf, "device/control", decision, artifactDecision{
		Enabled: true, DesiredDigest: wf.Spec.Release.ArtifactDigest,
	})
	if err != nil || !commanded {
		t.Fatalf("current release activation commanded=%t err=%v", commanded, err)
	}
	if len(published) != 2 || published[1]["action"] != "activate_local" {
		t.Fatalf("published commands = %#v", published)
	}
	if wf.Status.LastCommandedPlacement != placementEdge || wf.Status.LastCommandedRuntimeMode != placementEdge {
		t.Fatalf("placement command fields changed to %q/%q", wf.Status.LastCommandedPlacement, wf.Status.LastCommandedRuntimeMode)
	}
}

func TestDeadlineRejectionSignalRepeatsOnceAfterSpecChange(t *testing.T) {
	target := int32(1)
	wf := admissionFixture(placementEdge, "dht-reader", target, false)
	wf.Generation = 12
	wf.Spec.Release.Generation = 38
	wf.Status.ArtifactReadinessReason = "DestinationReady"
	telemetryTime := metav1.NewTime(time.Unix(1784598912, 0))
	wf.Status.LastTelemetryTime = &telemetryTime
	*wf.Status.ObservedBatteryPercent = 90

	var published []map[string]interface{}
	reconciler := &WasmFunctionReconciler{PublishControl: func(_ context.Context, _ string, payload interface{}) error {
		published = append(published, payload.(map[string]interface{}))
		return nil
	}}

	decision := resolveForTest(wf)
	if decision.Deadline.Condition == nil || decision.Deadline.Condition.Reason != "CandidateDeadlineUnsafe" {
		t.Fatalf("first decision = %#v", decision.Deadline)
	}
	if err := reconciler.applyDeadlineRejectionSignal(context.Background(), wf, "device/control", decision); err != nil {
		t.Fatalf("first signal: %v", err)
	}
	firstID := wf.Status.LastSignaledDeadlineDecisionID
	if err := reconciler.applyDeadlineRejectionSignal(context.Background(), wf, "device/control", decision); err != nil {
		t.Fatalf("duplicate signal: %v", err)
	}
	if len(published) != 1 {
		t.Fatalf("same-generation signals = %d, want 1", len(published))
	}

	// Kubernetes increments metadata.generation when targetMs changes. The
	// candidate is still unsafe, but this is a distinct admission decision and
	// must produce one fresh device indication and operator log.
	updatedTarget := int32(2)
	wf.Spec.Placement.Deadline.TargetMs = &updatedTarget
	wf.Generation++
	decision = resolveForTest(wf)
	if decision.Deadline.Condition == nil || decision.Deadline.Condition.Reason != "CandidateDeadlineUnsafe" {
		t.Fatalf("updated decision = %#v", decision.Deadline)
	}
	if err := reconciler.applyDeadlineRejectionSignal(context.Background(), wf, "device/control", decision); err != nil {
		t.Fatalf("updated signal: %v", err)
	}
	secondID := wf.Status.LastSignaledDeadlineDecisionID
	if len(published) != 2 {
		t.Fatalf("signals after spec update = %d, want 2", len(published))
	}
	if firstID == secondID {
		t.Fatalf("decision ID was reused across spec generations: %q", firstID)
	}
	if err := reconciler.applyDeadlineRejectionSignal(context.Background(), wf, "device/control", decision); err != nil {
		t.Fatalf("updated duplicate signal: %v", err)
	}
	if len(published) != 2 {
		t.Fatalf("same updated-generation signals = %d, want 2", len(published))
	}
}

func TestDeadlineRejectionSignalRepeatsOncePerTelemetryObservation(t *testing.T) {
	target := int32(1)
	wf := admissionFixture(placementLocal, "dht-reader", target, false)
	wf.Generation = 20
	wf.Spec.Release.Generation = 40
	wf.Status.ArtifactReadinessReason = "DestinationReady"
	delta := int32(4)
	wf.Status.ObservedBatteryDeltaPercent = &delta
	wf.Status.ConsecutiveRiskyWindows = 2
	firstTelemetry := metav1.NewTime(time.Unix(200, 0))
	wf.Status.LastTelemetryTime = &firstTelemetry

	var decisionIDs []string
	reconciler := &WasmFunctionReconciler{PublishControl: func(_ context.Context, _ string, payload interface{}) error {
		decisionIDs = append(decisionIDs, payload.(map[string]interface{})["decisionId"].(string))
		return nil
	}}
	decision := resolveForTest(wf)
	if decision.Deadline.Condition == nil || decision.Deadline.Condition.Reason != "CandidateDeadlineUnsafe" {
		t.Fatalf("decision = %#v", decision.Deadline)
	}

	if err := reconciler.applyDeadlineRejectionSignal(context.Background(), wf, "device/control", decision); err != nil {
		t.Fatalf("first observation: %v", err)
	}
	if err := reconciler.applyDeadlineRejectionSignal(context.Background(), wf, "device/control", decision); err != nil {
		t.Fatalf("first observation duplicate: %v", err)
	}
	if len(decisionIDs) != 1 {
		t.Fatalf("first observation signals = %d, want 1", len(decisionIDs))
	}

	secondTelemetry := metav1.NewTime(time.Unix(215, 0))
	wf.Status.LastTelemetryTime = &secondTelemetry
	if err := reconciler.applyDeadlineRejectionSignal(context.Background(), wf, "device/control", decision); err != nil {
		t.Fatalf("second observation: %v", err)
	}
	if err := reconciler.applyDeadlineRejectionSignal(context.Background(), wf, "device/control", decision); err != nil {
		t.Fatalf("second observation duplicate: %v", err)
	}
	if len(decisionIDs) != 2 {
		t.Fatalf("two observations signals = %d, want 2", len(decisionIDs))
	}
	if decisionIDs[0] == decisionIDs[1] {
		t.Fatalf("decision ID was reused across telemetry observations: %q", decisionIDs[0])
	}
}
