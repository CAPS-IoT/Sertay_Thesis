package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	edgev1alpha1 "github.com/sertayy/sif-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func orchestrationTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for name, add := range map[string]func(*runtime.Scheme) error{
		"edge": edgev1alpha1.AddToScheme,
		"apps": appsv1.AddToScheme,
		"core": corev1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add %s scheme: %v", name, err)
		}
	}
	return scheme
}

func TestInactiveHostStageFailureDoesNotBlockCurrentLocalActivation(t *testing.T) {
	scheme := orchestrationTestScheme(t)
	artifact := []byte("current local release")
	digest := digestBytes(artifact)
	source := newSourceArtifactServer(artifact, digest)
	defer source.Close()
	wf := &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Image: "localhost:30500/sif-edge-host:latest",
			Device: edgev1alpha1.DeviceSpec{
				ControlTopic: "device/control", OperatorWasmURL: source.URL,
				ArtifactURL: "http://device.invalid/wasm",
			},
			Release: edgev1alpha1.ReleaseSpec{
				Generation: 2, ArtifactDigest: digest, FunctionIdentity: "dht-reader",
			},
			Placement: edgev1alpha1.PlacementSpec{Mode: placementLocal, LowBatteryThreshold: 20, HighBatteryThreshold: 60},
		},
		Status: edgev1alpha1.WasmFunctionStatus{
			DesiredPlacement: placementLocal, ObservedMode: placementLocal,
			ObservedFunction: "dht-reader", StagedReleaseGeneration: 2,
			DeviceStagedArtifactDigest:     digest,
			LastAppliedLowBatteryThreshold: 20, LastAppliedHighBatteryThreshold: 60,
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&edgev1alpha1.WasmFunction{}).WithObjects(wf).Build()
	var actions []string
	r := &WasmFunctionReconciler{
		Client: client, Scheme: scheme,
		HTTPClient: &http.Client{Timeout: 100 * time.Millisecond},
		HostWasmURLResolver: func(*edgev1alpha1.WasmFunction) string {
			return "http://127.0.0.1:1/wasm"
		},
		PublishControl: func(_ context.Context, _ string, payload interface{}) error {
			actions = append(actions, payload.(map[string]interface{})["action"].(string))
			return nil
		},
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: wf.Name, Namespace: wf.Namespace}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(actions) != 1 || actions[0] != "activate_local" {
		t.Fatalf("actions = %#v, want activate_local", actions)
	}
	var updated edgev1alpha1.WasmFunction
	if err := client.Get(context.Background(), types.NamespacedName{Name: wf.Name, Namespace: wf.Namespace}, &updated); err != nil {
		t.Fatalf("get updated function: %v", err)
	}
	artifactCondition := meta.FindStatusCondition(updated.Status.Conditions, "ArtifactSynchronized")
	placementCondition := meta.FindStatusCondition(updated.Status.Conditions, "PlacementCommanded")
	if artifactCondition == nil || artifactCondition.Status != metav1.ConditionFalse {
		t.Fatalf("artifact condition = %#v", artifactCondition)
	}
	if placementCondition == nil || placementCondition.Status != metav1.ConditionTrue {
		t.Fatalf("placement condition = %#v", placementCondition)
	}
}

func TestHardTransitionPauseAndAcknowledgedResume(t *testing.T) {
	digest := strings.Repeat("a", 64)
	wf := &edgev1alpha1.WasmFunction{
		Spec:   edgev1alpha1.WasmFunctionSpec{Release: edgev1alpha1.ReleaseSpec{Generation: 3}},
		Status: edgev1alpha1.WasmFunctionStatus{},
	}
	var actions []string
	r := &WasmFunctionReconciler{PublishControl: func(_ context.Context, _ string, payload interface{}) error {
		actions = append(actions, payload.(map[string]interface{})["action"].(string))
		return nil
	}}
	if err := r.applyPauseCommand(context.Background(), wf, "device/control"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := r.applyPauseCommand(context.Background(), wf, "device/control"); err != nil {
		t.Fatalf("duplicate pause: %v", err)
	}
	if len(actions) != 1 || actions[0] != "pause_function" {
		t.Fatalf("pause actions = %#v", actions)
	}

	wf.Status.ObservedAdmissionPaused = true
	wf.Status.ObservedMode = placementEdge
	wf.Status.AcknowledgedReleaseGeneration = 3
	wf.Status.ObservedArtifactDigest = digest
	decision := placementDecision{Desired: placementHybrid}
	artifact := artifactDecision{DesiredDigest: digest}
	if err := r.applyResumeCommand(context.Background(), wf, "device/control", decision, artifact); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(actions) != 2 || actions[1] != "resume_function" {
		t.Fatalf("pause/resume actions = %#v", actions)
	}
}

func TestReadinessPendingDoesNotSignalDeadlineRejection(t *testing.T) {
	wf := &edgev1alpha1.WasmFunction{
		Spec: edgev1alpha1.WasmFunctionSpec{Release: edgev1alpha1.ReleaseSpec{
			Generation: 5, FunctionIdentity: "dht-reader",
		}},
		Status: edgev1alpha1.WasmFunctionStatus{ArtifactReadinessReason: "HostStagePending"},
	}
	decision := placementDecision{
		Current: placementLocal, Candidate: placementEdge, Transition: true,
		Deadline: deadlineEvaluation{Condition: deadlineAdmissionCondition(
			metav1.ConditionFalse, "CandidateDeadlineUnsafe", "unsafe candidate"),
		},
	}
	published := 0
	r := &WasmFunctionReconciler{PublishControl: func(context.Context, string, interface{}) error {
		published++
		return nil
	}}
	if err := r.applyDeadlineRejectionSignal(context.Background(), wf, "device/control", decision); err != nil {
		t.Fatalf("signal check: %v", err)
	}
	if published != 0 || wf.Status.LastSignaledDeadlineDecisionID != "" {
		t.Fatalf("readiness wait published=%d decisionId=%q", published, wf.Status.LastSignaledDeadlineDecisionID)
	}
}

func TestBatterySimulationUpdateIsOptInAndDeduplicated(t *testing.T) {
	wf := &edgev1alpha1.WasmFunction{}
	var published []map[string]interface{}
	now := time.Date(2026, 7, 21, 3, 0, 0, 0, time.UTC)
	r := &WasmFunctionReconciler{Now: func() time.Time { return now }, PublishControl: func(_ context.Context, topic string, payload interface{}) error {
		if topic != "device/control" {
			t.Fatalf("topic = %q", topic)
		}
		published = append(published, payload.(map[string]interface{}))
		return nil
	}}

	commanded, err := r.applyBatterySimulationUpdate(context.Background(), wf, "device/control")
	if err != nil || commanded || len(published) != 0 {
		t.Fatalf("unmanaged simulation commanded=%t published=%d err=%v", commanded, len(published), err)
	}

	wf.Spec.Device.BatterySimulation.Enabled = boolPtr(true)
	drain := int32(10)
	recover := int32(20)
	wf.Spec.Device.BatterySimulation.LocalDrainPercent = &drain
	wf.Spec.Device.BatterySimulation.EdgeRecoverPercent = &recover
	commanded, err = r.applyBatterySimulationUpdate(context.Background(), wf, "device/control")
	if err != nil || !commanded || len(published) != 1 {
		t.Fatalf("enable simulation commanded=%t published=%d err=%v", commanded, len(published), err)
	}
	if published[0]["action"] != "set_simulation" || published[0]["enabled"] != true ||
		published[0]["drain"] != int32(10) || published[0]["recover"] != int32(20) {
		t.Fatalf("simulation payload = %#v", published[0])
	}
	firstID := wf.Status.LastAppliedBatterySimulationCommandID
	if wf.Status.LastBatterySimulationCommandTime == nil ||
		!wf.Status.LastBatterySimulationCommandTime.Time.Equal(now) {
		t.Fatalf("simulation command time = %#v, want %s", wf.Status.LastBatterySimulationCommandTime, now)
	}

	// A duplicate reconcile before any fresh telemetry must stay silent even
	// though the previously observed source has not changed yet.
	wf.Status.ObservedBatterySource = "real"
	commanded, err = r.applyBatterySimulationUpdate(context.Background(), wf, "device/control")
	if err != nil || commanded || len(published) != 1 {
		t.Fatalf("duplicate simulation commanded=%t published=%d err=%v", commanded, len(published), err)
	}

	// Fresh post-command telemetry that still reports real is a negative
	// acknowledgement. Retry once, then wait for another observation.
	telemetryTime := metav1.NewTime(now.Add(time.Second))
	wf.Status.LastTelemetryTime = &telemetryTime
	now = now.Add(2 * time.Second)
	commanded, err = r.applyBatterySimulationUpdate(context.Background(), wf, "device/control")
	if err != nil || !commanded || len(published) != 2 {
		t.Fatalf("mismatch retry commanded=%t published=%d err=%v", commanded, len(published), err)
	}
	commanded, err = r.applyBatterySimulationUpdate(context.Background(), wf, "device/control")
	if err != nil || commanded || len(published) != 2 {
		t.Fatalf("retry duplicate commanded=%t published=%d err=%v", commanded, len(published), err)
	}

	// A post-command simulated observation acknowledges the desired source.
	wf.Status.ObservedBatterySource = "simulated"
	telemetryTime = metav1.NewTime(now.Add(time.Second))
	wf.Status.LastTelemetryTime = &telemetryTime
	commanded, err = r.applyBatterySimulationUpdate(context.Background(), wf, "device/control")
	if err != nil || commanded || len(published) != 2 {
		t.Fatalf("acknowledged simulation commanded=%t published=%d err=%v", commanded, len(published), err)
	}

	recover = 25
	commanded, err = r.applyBatterySimulationUpdate(context.Background(), wf, "device/control")
	if err != nil || !commanded || len(published) != 3 {
		t.Fatalf("updated simulation commanded=%t published=%d err=%v", commanded, len(published), err)
	}
	if firstID == wf.Status.LastAppliedBatterySimulationCommandID {
		t.Fatalf("simulation command ID did not change: %q", firstID)
	}

	wf.Spec.Device.BatterySimulation.Enabled = boolPtr(false)
	commanded, err = r.applyBatterySimulationUpdate(context.Background(), wf, "device/control")
	if err != nil || !commanded || len(published) != 4 || published[3]["enabled"] != false {
		t.Fatalf("disable simulation commanded=%t payload=%#v err=%v", commanded, published, err)
	}
}

func TestForcedTransitionSequencePausesCommandsAndResumes(t *testing.T) {
	digest := strings.Repeat("d", 64)
	battery := int32(10)
	target := int32(100)
	wf := admissionFixture(placementLocal, "dht-reader", target, false)
	wf.Spec.Release.Generation = 6
	wf.Spec.Release.ArtifactDigest = digest
	wf.Status.ObservedBatteryPercent = &battery
	decision := resolveForTest(wf)
	if !decision.Hard || decision.Candidate != placementEdge {
		t.Fatalf("forced proposal = %#v", decision)
	}

	decision = applyDestinationReadiness(wf, decision, artifactDecision{DesiredDigest: digest}, false)
	if decision.Deadline.Accepted || decision.ReadinessReason != "RuntimeUnavailable" {
		t.Fatalf("pending decision = %#v", decision)
	}

	var actions []string
	r := &WasmFunctionReconciler{PublishControl: func(_ context.Context, _ string, payload interface{}) error {
		actions = append(actions, payload.(map[string]interface{})["action"].(string))
		return nil
	}}
	if err := r.applyPauseCommand(context.Background(), wf, "device/control"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if len(actions) != 1 || actions[0] != "pause_function" {
		t.Fatalf("pending actions = %#v", actions)
	}

	wf.Status.ObservedAdmissionPaused = true
	wf.Status.StagedReleaseGeneration = 6
	wf.Status.DeviceStagedArtifactDigest = digest
	readyArtifact := artifactDecision{
		Enabled: true, DesiredDigest: digest, HostDigest: digest,
		HostActiveGeneration: 6, HostActiveFunction: "dht-reader",
	}
	decision = resolveForTest(wf)
	decision = applyDestinationReadiness(wf, decision, readyArtifact, true)
	if !decision.Deadline.Accepted || decision.Desired != placementEdge {
		t.Fatalf("ready decision = %#v", decision)
	}
	if _, err := r.applyPlacementCommand(context.Background(), wf, "device/control", decision, readyArtifact); err != nil {
		t.Fatalf("runtime command: %v", err)
	}
	if len(actions) != 2 || actions[1] != "set_runtime_mode" {
		t.Fatalf("ready actions = %#v", actions)
	}

	wf.Status.ObservedMode = placementEdge
	wf.Status.AcknowledgedReleaseGeneration = 6
	wf.Status.ObservedArtifactDigest = digest
	if err := r.applyResumeCommand(context.Background(), wf, "device/control", decision, readyArtifact); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(actions) != 3 || actions[2] != "resume_function" {
		t.Fatalf("sequence actions = %#v", actions)
	}
}

func TestEdgePlacementActivatesSameDigestAtNewGenerationBeforeRuntimeSwitch(t *testing.T) {
	digest := strings.Repeat("a", 64)
	wf := &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Device: edgev1alpha1.DeviceSpec{ControlTopic: "device/control"},
			Release: edgev1alpha1.ReleaseSpec{
				Generation: 27, ArtifactDigest: digest, FunctionIdentity: "dht-reader",
			},
		},
		Status: edgev1alpha1.WasmFunctionStatus{
			StagedReleaseGeneration: 27, DeviceStagedArtifactDigest: digest,
		},
	}

	activationRequests := 0
	activationMethod := ""
	activationPath := ""
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		activationRequests++
		activationMethod = r.Method
		activationPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer host.Close()

	var actions []string
	r := &WasmFunctionReconciler{
		HTTPClient: http.DefaultClient,
		HostWasmURLResolver: func(*edgev1alpha1.WasmFunction) string {
			return host.URL + "/wasm"
		},
		PublishControl: func(_ context.Context, _ string, payload interface{}) error {
			actions = append(actions, payload.(map[string]interface{})["action"].(string))
			return nil
		},
	}
	artifact := artifactDecision{
		Enabled: true, DesiredDigest: digest,
		HostDigest: digest, HostActiveGeneration: 3, HostActiveFunction: "dht-reader",
		HostStagedDigest: digest, HostStagedGeneration: 27, HostStagedFunction: "dht-reader",
	}
	decision := placementDecision{
		Enabled: true, Current: placementLocal, Candidate: placementEdge,
		Desired: placementEdge, RuntimeMode: placementEdge, Transition: true,
		Deadline: deadlineEvaluation{Accepted: true},
	}

	commanded, err := r.applyPlacementCommand(context.Background(), wf, "device/control", decision, artifact)
	if err != nil {
		t.Fatalf("applyPlacementCommand: %v", err)
	}
	if !commanded || activationRequests != 1 {
		t.Fatalf("commanded=%t activationRequests=%d, want true/1", commanded, activationRequests)
	}
	if activationMethod != http.MethodPost || activationPath != "/release" {
		t.Fatalf("host request = %s %s, want POST /release", activationMethod, activationPath)
	}
	if len(actions) != 1 || actions[0] != "set_runtime_mode" {
		t.Fatalf("actions = %#v, want set_runtime_mode after host activation", actions)
	}
}

func TestArtifactRetryBackoffIsBounded(t *testing.T) {
	wf := &edgev1alpha1.WasmFunction{}
	errSync := context.DeadlineExceeded
	for attempt := 1; attempt <= 8; attempt++ {
		updateArtifactRetryState(wf, errSync)
		result, requeue := requeueOnError(wf, errSync, nil)
		if !requeue || result.RequeueAfter > 60*time.Second {
			t.Fatalf("attempt %d requeue=%t delay=%s", attempt, requeue, result.RequeueAfter)
		}
	}
	wf.Status.ArtifactSyncStartedAt = &metav1.Time{Time: time.Now().Add(-61 * time.Second)}
	updateArtifactRetryState(wf, errSync)
	if wf.Status.ArtifactReadinessReason != "SyncFailed" {
		t.Fatalf("readiness reason = %q", wf.Status.ArtifactReadinessReason)
	}
	updateArtifactRetryState(wf, nil)
	if wf.Status.ArtifactSyncStartedAt != nil || wf.Status.ArtifactSyncRetryCount != 0 {
		t.Fatalf("retry state did not reset: %#v", wf.Status)
	}
}

func TestReleaseStageMaintainsOneInflightCommandWithBoundedBackoff(t *testing.T) {
	digest := strings.Repeat("e", 64)
	clock := time.Unix(1000, 0)
	wf := &edgev1alpha1.WasmFunction{
		Spec: edgev1alpha1.WasmFunctionSpec{
			Device: edgev1alpha1.DeviceSpec{ArtifactURL: "http://device.invalid/wasm"},
			Release: edgev1alpha1.ReleaseSpec{
				Generation: 12, ArtifactDigest: digest,
				FunctionIdentity: "hybrid-resource-demo",
			},
		},
	}
	published := 0
	r := &WasmFunctionReconciler{
		Now: func() time.Time { return clock },
		PublishControl: func(context.Context, string, interface{}) error {
			published++
			return nil
		},
	}
	artifact := artifactDecision{Enabled: true, DesiredDigest: digest}

	commanded, err := r.applyReleaseStage(context.Background(), wf, "device/control", artifact)
	if err != nil || !commanded || published != 1 || wf.Status.StageCommandAttempts != 1 {
		t.Fatalf("initial stage commanded=%t published=%d attempts=%d err=%v",
			commanded, published, wf.Status.StageCommandAttempts, err)
	}
	if state := releaseDeliveryState(wf, digest); state != "AwaitingStageAck" {
		t.Fatalf("initial delivery state = %q", state)
	}

	clock = clock.Add(29 * time.Second)
	commanded, err = r.applyReleaseStage(context.Background(), wf, "device/control", artifact)
	if err != nil || commanded || published != 1 {
		t.Fatalf("early retry commanded=%t published=%d err=%v", commanded, published, err)
	}

	clock = clock.Add(time.Second)
	commanded, err = r.applyReleaseStage(context.Background(), wf, "device/control", artifact)
	if err != nil || !commanded || published != 2 || wf.Status.StageCommandAttempts != 2 {
		t.Fatalf("first retry commanded=%t published=%d attempts=%d err=%v",
			commanded, published, wf.Status.StageCommandAttempts, err)
	}

	clock = clock.Add(59 * time.Second)
	commanded, err = r.applyReleaseStage(context.Background(), wf, "device/control", artifact)
	if err != nil || commanded || published != 2 {
		t.Fatalf("second early retry commanded=%t published=%d err=%v", commanded, published, err)
	}

	clock = clock.Add(time.Second)
	commanded, err = r.applyReleaseStage(context.Background(), wf, "device/control", artifact)
	if err != nil || !commanded || published != 3 || wf.Status.StageCommandAttempts != 3 {
		t.Fatalf("second retry commanded=%t published=%d attempts=%d err=%v",
			commanded, published, wf.Status.StageCommandAttempts, err)
	}
	if releaseStageRetryDelay(10) != releaseStageRetryMax {
		t.Fatalf("retry delay is not capped: %s", releaseStageRetryDelay(10))
	}

	wf.Status.StagedReleaseGeneration = wf.Spec.Release.Generation
	wf.Status.DeviceStagedArtifactDigest = digest
	commanded, err = r.applyReleaseStage(context.Background(), wf, "device/control", artifact)
	if err != nil || commanded || published != 3 || wf.Status.StageCommandAttempts != 0 {
		t.Fatalf("acknowledged stage commanded=%t published=%d attempts=%d err=%v",
			commanded, published, wf.Status.StageCommandAttempts, err)
	}
	if state := releaseDeliveryState(wf, digest); state != "Staged" {
		t.Fatalf("acknowledged delivery state = %q", state)
	}
}

func TestReleaseStageMemorySuppressesStaleCacheDuplicate(t *testing.T) {
	digest := strings.Repeat("d", 64)
	clock := time.Unix(2000, 0)
	newFunction := func() *edgev1alpha1.WasmFunction {
		return &edgev1alpha1.WasmFunction{
			ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
			Spec: edgev1alpha1.WasmFunctionSpec{
				Device: edgev1alpha1.DeviceSpec{ArtifactURL: "http://device.invalid/wasm"},
				Release: edgev1alpha1.ReleaseSpec{
					Generation: 23, ArtifactDigest: digest, FunctionIdentity: "dht-reader",
				},
			},
		}
	}
	published := 0
	r := &WasmFunctionReconciler{
		Now: func() time.Time { return clock },
		PublishControl: func(context.Context, string, interface{}) error {
			published++
			return nil
		},
	}
	artifact := artifactDecision{Enabled: true, DesiredDigest: digest}
	first := newFunction()
	if commanded, err := r.applyReleaseStage(context.Background(), first, "device/control", artifact); err != nil || !commanded {
		t.Fatalf("initial stage commanded=%t err=%v", commanded, err)
	}

	// Simulate a cache read that has not observed the status write from the
	// preceding reconciliation. The process-local send record must still stop a
	// second attempt-1 publish.
	clock = clock.Add(time.Second)
	stale := newFunction()
	if commanded, err := r.applyReleaseStage(context.Background(), stale, "device/control", artifact); err != nil || commanded {
		t.Fatalf("stale-cache stage commanded=%t err=%v", commanded, err)
	}
	if published != 1 {
		t.Fatalf("published=%d, want exactly one attempt", published)
	}

	clock = clock.Add(29 * time.Second)
	if commanded, err := r.applyReleaseStage(context.Background(), stale, "device/control", artifact); err != nil || !commanded {
		t.Fatalf("timeout retry commanded=%t err=%v", commanded, err)
	}
	if published != 2 || stale.Status.StageCommandAttempts != 2 {
		t.Fatalf("published=%d attempts=%d", published, stale.Status.StageCommandAttempts)
	}
}

func TestPendingReleaseSchedulesItsOwnReconciliation(t *testing.T) {
	digest := strings.Repeat("c", 64)
	commandTime := time.Unix(3000, 0)
	wf := &edgev1alpha1.WasmFunction{
		Spec: edgev1alpha1.WasmFunctionSpec{Release: edgev1alpha1.ReleaseSpec{
			Generation: 24, ArtifactDigest: digest,
		}},
		Status: edgev1alpha1.WasmFunctionStatus{
			LastStageCommandID:   "stage-24-cccccccccccc...",
			LastStageCommandTime: &metav1.Time{Time: commandTime},
			StageCommandAttempts: 1,
		},
	}
	result, requeue := requeuePendingRelease(wf, commandTime.Add(10*time.Second))
	if !requeue || result.RequeueAfter != 20*time.Second {
		t.Fatalf("stage requeue=%t delay=%s", requeue, result.RequeueAfter)
	}

	wf.Status.StagedReleaseGeneration = 24
	wf.Status.DeviceStagedArtifactDigest = digest
	wf.Status.LastCommandID = "activate-local-24"
	wf.Status.LastCommandTime = &metav1.Time{Time: commandTime}
	result, requeue = requeuePendingRelease(wf, commandTime.Add(5*time.Second))
	if !requeue || result.RequeueAfter != 25*time.Second {
		t.Fatalf("activation requeue=%t delay=%s", requeue, result.RequeueAfter)
	}
}
