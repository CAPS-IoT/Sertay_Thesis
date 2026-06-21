package controller

import (
	"context"
	"fmt"
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
	inboundTelemetryAt := lastClientWrite.Add(15 * time.Second)

	if shouldSendTelemetryPing(lastClientWrite, inboundTelemetryAt) {
		t.Fatalf("expected inbound telemetry not to satisfy MQTT keepalive")
	}

	timeoutAt := lastClientWrite.Add(20 * time.Second)
	if !shouldSendTelemetryPing(lastClientWrite, timeoutAt) {
		t.Fatalf("expected ping once client has been silent for %s", mqttTelemetryPingInterval)
	}

	lastClientWrite = timeoutAt
	if shouldSendTelemetryPing(lastClientWrite, timeoutAt.Add(10*time.Second)) {
		t.Fatalf("expected no ping before another %s of client silence", mqttTelemetryPingInterval)
	}
	if !shouldSendTelemetryPing(lastClientWrite, timeoutAt.Add(mqttTelemetryPingInterval)) {
		t.Fatalf("expected ping after client silence reaches %s again", mqttTelemetryPingInterval)
	}
}

func TestApplyBatteryTelemetryStoresObservedArtifactStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := edgev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	wf := &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Device: edgev1alpha1.DeviceSpec{ControlTopic: "64/199/data"},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&edgev1alpha1.WasmFunction{}).
		WithObjects(wf).
		Build()

	bridge := NewMQTTTelemetryBridge(cl)
	digest := strings.Repeat("a", 64)
	payload := []byte(fmt.Sprintf(`{"batteryPercent":42,"mode":"edge","source":"real","voltageMv":3777,"artifactDigest":"%s"}`,
		digest))
	if err := bridge.applyBatteryTelemetry(context.Background(), "64/199/data/telemetry", payload); err != nil {
		t.Fatalf("apply telemetry: %v", err)
	}

	var updated edgev1alpha1.WasmFunction
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "dht-reader", Namespace: "sertay"}, &updated); err != nil {
		t.Fatalf("get updated wasmfunction: %v", err)
	}
	if updated.Status.ObservedBatteryPercent == nil || *updated.Status.ObservedBatteryPercent != 42 {
		t.Fatalf("observed battery = %v, want 42", updated.Status.ObservedBatteryPercent)
	}
	if updated.Status.ObservedMode != "edge" {
		t.Fatalf("observed mode = %q, want edge", updated.Status.ObservedMode)
	}
	if updated.Status.ObservedBatterySource != "real" {
		t.Fatalf("observed battery source = %q, want real", updated.Status.ObservedBatterySource)
	}
	if updated.Status.ObservedVoltageMillivolts == nil || *updated.Status.ObservedVoltageMillivolts != 3777 {
		t.Fatalf("observed voltage = %v, want 3777", updated.Status.ObservedVoltageMillivolts)
	}
	if updated.Status.ObservedArtifactDigest != digest {
		t.Fatalf("observed artifact digest = %q, want %q", updated.Status.ObservedArtifactDigest, digest)
	}
	if updated.Status.LastTelemetryTime == nil {
		t.Fatalf("expected lastTelemetryTime to be set")
	}
}

func TestUpdateObservedTelemetryKeepsVoltageOnlyRefreshQuiet(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := edgev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	battery := int32(42)
	voltage := int32(3777)
	digest := strings.Repeat("b", 64)
	wf := &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Device: edgev1alpha1.DeviceSpec{ControlTopic: "64/199/data"},
		},
		Status: edgev1alpha1.WasmFunctionStatus{
			ObservedBatteryPercent:    &battery,
			ObservedMode:              "local",
			ObservedBatterySource:     "real",
			ObservedVoltageMillivolts: &voltage,
			ObservedArtifactDigest:    digest,
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&edgev1alpha1.WasmFunction{}).
		WithObjects(wf).
		Build()

	bridge := NewMQTTTelemetryBridge(cl)
	newVoltage := int32(3888)
	result, err := bridge.updateObservedTelemetry(
		context.Background(),
		types.NamespacedName{Name: "dht-reader", Namespace: "sertay"},
		batteryTelemetry{
			Mode:           "local",
			Source:         "real",
			VoltageMV:      &newVoltage,
			ArtifactDigest: digest,
		},
		battery,
	)
	if err != nil {
		t.Fatalf("update telemetry: %v", err)
	}
	if !result.statusChanged {
		t.Fatalf("expected voltage-only telemetry to update status")
	}
	if result.reportableInfo {
		t.Fatalf("expected voltage-only telemetry refresh to stay quiet")
	}

	var updated edgev1alpha1.WasmFunction
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "dht-reader", Namespace: "sertay"}, &updated); err != nil {
		t.Fatalf("get updated wasmfunction: %v", err)
	}
	if updated.Status.ObservedVoltageMillivolts == nil || *updated.Status.ObservedVoltageMillivolts != newVoltage {
		t.Fatalf("observed voltage = %v, want %d", updated.Status.ObservedVoltageMillivolts, newVoltage)
	}
	if updated.Status.LastTelemetryTime == nil {
		t.Fatalf("expected lastTelemetryTime to be set")
	}
}

func TestUpdateObservedTelemetryKeepsBatteryOnlyRefreshQuiet(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := edgev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	battery := int32(42)
	updatedBattery := int32(43)
	voltage := int32(3777)
	digest := strings.Repeat("c", 64)
	wf := &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Device: edgev1alpha1.DeviceSpec{ControlTopic: "64/199/data"},
		},
		Status: edgev1alpha1.WasmFunctionStatus{
			ObservedBatteryPercent:    &battery,
			ObservedMode:              "local",
			ObservedBatterySource:     "real",
			ObservedVoltageMillivolts: &voltage,
			ObservedArtifactDigest:    digest,
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&edgev1alpha1.WasmFunction{}).
		WithObjects(wf).
		Build()

	bridge := NewMQTTTelemetryBridge(cl)
	result, err := bridge.updateObservedTelemetry(
		context.Background(),
		types.NamespacedName{Name: "dht-reader", Namespace: "sertay"},
		batteryTelemetry{
			Mode:           "local",
			Source:         "real",
			VoltageMV:      &voltage,
			ArtifactDigest: digest,
		},
		updatedBattery,
	)
	if err != nil {
		t.Fatalf("update telemetry: %v", err)
	}
	if !result.statusChanged {
		t.Fatalf("expected battery-only telemetry to update status")
	}
	if result.reportableInfo {
		t.Fatalf("expected battery-only telemetry refresh to stay quiet")
	}

	var updated edgev1alpha1.WasmFunction
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "dht-reader", Namespace: "sertay"}, &updated); err != nil {
		t.Fatalf("get updated wasmfunction: %v", err)
	}
	if updated.Status.ObservedBatteryPercent == nil || *updated.Status.ObservedBatteryPercent != updatedBattery {
		t.Fatalf("observed battery = %v, want %d", updated.Status.ObservedBatteryPercent, updatedBattery)
	}
	if updated.Status.LastTelemetryTime == nil {
		t.Fatalf("expected lastTelemetryTime to be set")
	}
}
