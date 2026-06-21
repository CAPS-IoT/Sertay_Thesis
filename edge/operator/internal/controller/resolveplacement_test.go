package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	edgev1alpha1 "github.com/sertayy/sif-operator/api/v1alpha1"
)

func TestResolvePlacementUsesObservedBatteryPercent(t *testing.T) {
	specBattery := int32(90)
	observedBattery := int32(10)

	wf := &edgev1alpha1.WasmFunction{
		Spec: edgev1alpha1.WasmFunctionSpec{
			Device: edgev1alpha1.DeviceSpec{
				ControlTopic: "64/199/data",
			},
			Placement: edgev1alpha1.PlacementSpec{
				Mode:                 placementAuto,
				BatteryPercent:       &specBattery,
				LowBatteryThreshold:  20,
				HighBatteryThreshold: 80,
			},
		},
		Status: edgev1alpha1.WasmFunctionStatus{
			ObservedBatteryPercent: &observedBattery,
		},
	}

	decision := resolvePlacement(wf)
	if decision.Desired != placementEdge {
		t.Fatalf("expected edge placement from observed battery, got %q", decision.Desired)
	}
	if decision.BatteryPercent == nil || *decision.BatteryPercent != observedBattery {
		t.Fatalf("expected observed battery %d, got %v", observedBattery, decision.BatteryPercent)
	}
}

func TestResolvePlacementFallsBackToSpecBatteryPercent(t *testing.T) {
	specBattery := int32(90)

	wf := &edgev1alpha1.WasmFunction{
		Spec: edgev1alpha1.WasmFunctionSpec{
			Device: edgev1alpha1.DeviceSpec{
				ControlTopic: "64/199/data",
			},
			Placement: edgev1alpha1.PlacementSpec{
				Mode:                 placementAuto,
				BatteryPercent:       &specBattery,
				LowBatteryThreshold:  20,
				HighBatteryThreshold: 80,
			},
		},
	}

	decision := resolvePlacement(wf)
	if decision.Desired != placementLocal {
		t.Fatalf("expected local placement from spec battery fallback, got %q", decision.Desired)
	}
	if decision.BatteryPercent == nil || *decision.BatteryPercent != specBattery {
		t.Fatalf("expected spec battery %d, got %v", specBattery, decision.BatteryPercent)
	}
}

func TestTelemetryTopicDefaultsToControlTopicSuffix(t *testing.T) {
	topic := telemetryTopicForDevice(edgev1alpha1.DeviceSpec{ControlTopic: "64/199/data"})
	if topic != "64/199/data/telemetry" {
		t.Fatalf("expected derived telemetry topic, got %q", topic)
	}
}

func TestReconcileStatusUpdatePreservesTelemetryFields(t *testing.T) {
	observedBattery := int32(64)
	observedVoltage := int32(3711)
	status := edgev1alpha1.WasmFunctionStatus{
		ObservedBatteryPercent:    &observedBattery,
		ObservedMode:              placementEdge,
		ObservedBatterySource:     "real",
		ObservedVoltageMillivolts: &observedVoltage,
		ObservedArtifactDigest:    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}

	availableCondition := metav1.Condition{Type: "Available", Status: metav1.ConditionTrue, Reason: "Reconciled", Message: "Deployment and Service are reconciled"}
	placementCondition := metav1.Condition{Type: "PlacementCommanded", Status: metav1.ConditionTrue, Reason: "AlreadyCommanded", Message: "edge placement already commanded"}

	status.AvailableReplicas = 1
	status.Endpoint = "http://dht-reader.default:8080/process"
	status.DesiredPlacement = placementLocal
	status.PlacementReason = "battery recovered"
	metaSetStatus := func() {
		meta.SetStatusCondition(&status.Conditions, availableCondition)
		meta.SetStatusCondition(&status.Conditions, placementCondition)
	}
	metaSetStatus()

	if status.ObservedArtifactDigest == "" {
		t.Fatalf("expected observed artifact digest to be preserved")
	}
	if status.ObservedMode != placementEdge {
		t.Fatalf("expected observed mode to be preserved, got %q", status.ObservedMode)
	}
	if status.ObservedBatterySource != "real" {
		t.Fatalf("expected observed battery source to be preserved, got %q", status.ObservedBatterySource)
	}
	if status.ObservedVoltageMillivolts == nil || *status.ObservedVoltageMillivolts != observedVoltage {
		t.Fatalf("expected observed voltage to be preserved, got %v", status.ObservedVoltageMillivolts)
	}
	if status.ObservedBatteryPercent == nil || *status.ObservedBatteryPercent != observedBattery {
		t.Fatalf("expected observed battery to be preserved, got %v", status.ObservedBatteryPercent)
	}
	if len(status.Conditions) != 2 {
		t.Fatalf("expected 2 status conditions, got %d", len(status.Conditions))
	}
}
