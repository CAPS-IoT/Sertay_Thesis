package controller

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	edgev1alpha1 "github.com/sertayy/sif-operator/api/v1alpha1"
)

func TestReconcilePreservesObservedArtifactStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := edgev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add edge scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	observedBattery := int32(55)
	observedVoltage := int32(3701)
	now := metav1.Now()
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	wf := &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Image: "localhost:30500/sif-edge-host:latest",
		},
		Status: edgev1alpha1.WasmFunctionStatus{
			ObservedBatteryPercent:    &observedBattery,
			ObservedMode:              "local",
			ObservedBatterySource:     "real",
			ObservedVoltageMillivolts: &observedVoltage,
			ObservedArtifactDigest:    digest,
			LastTelemetryTime:         &now,
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&edgev1alpha1.WasmFunction{}).
		WithObjects(wf).
		Build()

	r := &WasmFunctionReconciler{Client: cl, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "dht-reader", Namespace: "sertay"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated edgev1alpha1.WasmFunction
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "dht-reader", Namespace: "sertay"}, &updated); err != nil {
		t.Fatalf("get updated wasmfunction: %v", err)
	}
	if updated.Status.ObservedArtifactDigest != digest {
		t.Fatalf("observed artifact digest = %q, want %q", updated.Status.ObservedArtifactDigest, digest)
	}
	if updated.Status.ObservedMode != "local" {
		t.Fatalf("observed mode = %q, want local", updated.Status.ObservedMode)
	}
	if updated.Status.ObservedBatterySource != "real" {
		t.Fatalf("observed battery source = %q, want real", updated.Status.ObservedBatterySource)
	}
	if updated.Status.ObservedVoltageMillivolts == nil || *updated.Status.ObservedVoltageMillivolts != observedVoltage {
		t.Fatalf("observed voltage = %v, want %d", updated.Status.ObservedVoltageMillivolts, observedVoltage)
	}
}

func TestReconcilePersistsPlacementCommandStatus(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen mqtt stub: %v", err)
	}
	defer listener.Close()

	go serveMQTTStub(t, listener)
	t.Setenv("SIF_MQTT_BROKER", listener.Addr().String())
	t.Setenv("SIF_MQTT_USER", "")
	t.Setenv("SIF_MQTT_TOKEN", "")
	t.Setenv("SIF_MQTT_CLIENT_ID", "test-operator")

	scheme := runtime.NewScheme()
	if err := edgev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add edge scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	observedBattery := int32(100)
	artifact := []byte("placement command artifact")
	digest := digestBytes(artifact)
	source := newSourceArtifactServer(artifact, digest)
	defer source.Close()
	hostDigest := strings.Repeat("a", 64)
	host := newMutableHostServer(&hostDigest, func(body []byte) { hostDigest = digestBytes(body) })
	defer host.Close()
	wf := &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Image:   "localhost:30500/sif-edge-host:latest",
			Device:  edgev1alpha1.DeviceSpec{ControlTopic: "64/199/data", OperatorWasmURL: source.URL, ArtifactURL: "http://device.invalid/wasm"},
			Release: edgev1alpha1.ReleaseSpec{Generation: 1, ArtifactDigest: digest, FunctionIdentity: "dht-reader"},
			Placement: edgev1alpha1.PlacementSpec{
				Mode:                 "auto",
				LowBatteryThreshold:  100,
				HighBatteryThreshold: 100,
			},
		},
		Status: edgev1alpha1.WasmFunctionStatus{
			ObservedBatteryPercent:     &observedBattery,
			StagedReleaseGeneration:    1,
			DeviceStagedArtifactDigest: digest,
			ObservedFunction:           "dht-reader",
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&edgev1alpha1.WasmFunction{}).
		WithObjects(wf).
		Build()

	r := &WasmFunctionReconciler{Client: cl, Scheme: scheme, HTTPClient: http.DefaultClient,
		HostWasmURLResolver:  func(*edgev1alpha1.WasmFunction) string { return host.URL },
		EdgeRuntimeAvailable: func(context.Context, *edgev1alpha1.WasmFunction) bool { return true }}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "dht-reader", Namespace: "sertay"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated edgev1alpha1.WasmFunction
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "dht-reader", Namespace: "sertay"}, &updated); err != nil {
		t.Fatalf("get updated wasmfunction: %v", err)
	}
	if updated.Status.LastAppliedLowBatteryThreshold != 100 {
		t.Fatalf("lastAppliedLowBatteryThreshold = %d, want 100", updated.Status.LastAppliedLowBatteryThreshold)
	}
	if updated.Status.LastAppliedHighBatteryThreshold != 100 {
		t.Fatalf("lastAppliedHighBatteryThreshold = %d, want 100", updated.Status.LastAppliedHighBatteryThreshold)
	}
	if updated.Status.LastCommandedPlacement != "edge" {
		t.Fatalf("lastCommandedPlacement = %q, want edge", updated.Status.LastCommandedPlacement)
	}
	if updated.Status.LastCommandTime == nil {
		t.Fatalf("expected lastCommandTime to be set")
	}
	if updated.Status.DesiredPlacement != "edge" {
		t.Fatalf("desiredPlacement = %q, want edge", updated.Status.DesiredPlacement)
	}
}

func TestReconcileDoesNotApplyThresholdsWhenBatteryThresholdDisabled(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen mqtt stub: %v", err)
	}
	defer listener.Close()

	go serveMQTTStub(t, listener)
	t.Setenv("SIF_MQTT_BROKER", listener.Addr().String())
	t.Setenv("SIF_MQTT_USER", "")
	t.Setenv("SIF_MQTT_TOKEN", "")
	t.Setenv("SIF_MQTT_CLIENT_ID", "test-operator")

	scheme := runtime.NewScheme()
	if err := edgev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add edge scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	observedBattery := int32(5)
	wf := &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Image:  "localhost:30500/sif-edge-host:latest",
			Device: edgev1alpha1.DeviceSpec{ControlTopic: "64/199/data"},
			Placement: edgev1alpha1.PlacementSpec{
				Mode:                 placementAuto,
				LowBatteryThreshold:  99,
				HighBatteryThreshold: 100,
				BatteryThreshold: edgev1alpha1.BatteryThresholdPolicySpec{
					Enabled: boolPtr(false),
				},
			},
		},
		Status: edgev1alpha1.WasmFunctionStatus{
			ObservedBatteryPercent: &observedBattery,
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&edgev1alpha1.WasmFunction{}).
		WithObjects(wf).
		Build()

	r := &WasmFunctionReconciler{Client: cl, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "dht-reader", Namespace: "sertay"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated edgev1alpha1.WasmFunction
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "dht-reader", Namespace: "sertay"}, &updated); err != nil {
		t.Fatalf("get updated wasmfunction: %v", err)
	}
	if updated.Status.LastAppliedLowBatteryThreshold != 0 {
		t.Fatalf("lastAppliedLowBatteryThreshold = %d, want 0", updated.Status.LastAppliedLowBatteryThreshold)
	}
	if updated.Status.LastAppliedHighBatteryThreshold != 0 {
		t.Fatalf("lastAppliedHighBatteryThreshold = %d, want 0", updated.Status.LastAppliedHighBatteryThreshold)
	}
	if updated.Status.DesiredPlacement != placementLocal {
		t.Fatalf("desiredPlacement = %q, want %q", updated.Status.DesiredPlacement, placementLocal)
	}
}

func TestReconcileRetriesPlacementWhenObservedModeStillDiffers(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen mqtt stub: %v", err)
	}
	defer listener.Close()

	go serveMQTTStub(t, listener)
	t.Setenv("SIF_MQTT_BROKER", listener.Addr().String())
	t.Setenv("SIF_MQTT_USER", "")
	t.Setenv("SIF_MQTT_TOKEN", "")
	t.Setenv("SIF_MQTT_CLIENT_ID", "test-operator")

	scheme := runtime.NewScheme()
	if err := edgev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add edge scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	commandTime := metav1.NewTime(time.Unix(100, 0))
	telemetryTime := metav1.NewTime(time.Unix(200, 0))
	artifact := []byte("retry placement artifact")
	digest := digestBytes(artifact)
	source := newSourceArtifactServer(artifact, digest)
	defer source.Close()
	hostDigest := digest
	host := newMutableHostServer(&hostDigest, func(body []byte) { hostDigest = digestBytes(body) })
	defer host.Close()
	wf := &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Image:   "localhost:30500/sif-edge-host:latest",
			Device:  edgev1alpha1.DeviceSpec{ControlTopic: "64/199/data", OperatorWasmURL: source.URL, ArtifactURL: "http://device.invalid/wasm"},
			Release: edgev1alpha1.ReleaseSpec{Generation: 2, ArtifactDigest: digest, FunctionIdentity: "dht-reader"},
			Placement: edgev1alpha1.PlacementSpec{
				Mode:                 placementEdge,
				LowBatteryThreshold:  20,
				HighBatteryThreshold: 60,
			},
		},
		Status: edgev1alpha1.WasmFunctionStatus{
			ObservedMode:                    placementLocal,
			LastAppliedLowBatteryThreshold:  20,
			LastAppliedHighBatteryThreshold: 60,
			LastCommandedPlacement:          placementEdge,
			LastCommandedRuntimeMode:        placementEdge,
			LastCommandID:                   "runtime-edge-2",
			LastCommandTime:                 &commandTime,
			LastTelemetryTime:               &telemetryTime,
			StagedReleaseGeneration:         2,
			DeviceStagedArtifactDigest:      digest,
			ObservedFunction:                "dht-reader",
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&edgev1alpha1.WasmFunction{}).
		WithObjects(wf).
		Build()

	r := &WasmFunctionReconciler{Client: cl, Scheme: scheme, HTTPClient: http.DefaultClient,
		HostWasmURLResolver:  func(*edgev1alpha1.WasmFunction) string { return host.URL },
		EdgeRuntimeAvailable: func(context.Context, *edgev1alpha1.WasmFunction) bool { return true }}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "dht-reader", Namespace: "sertay"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated edgev1alpha1.WasmFunction
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "dht-reader", Namespace: "sertay"}, &updated); err != nil {
		t.Fatalf("get updated wasmfunction: %v", err)
	}
	if updated.Status.LastCommandTime == nil {
		t.Fatalf("expected lastCommandTime to be set")
	}
	if !updated.Status.LastCommandTime.After(commandTime.Time) {
		t.Fatalf("expected lastCommandTime %v to be newer than %v", updated.Status.LastCommandTime, commandTime)
	}
	if updated.Status.LastCommandedPlacement != placementEdge {
		t.Fatalf("lastCommandedPlacement = %q, want %q", updated.Status.LastCommandedPlacement, placementEdge)
	}
	if updated.Status.DesiredPlacement != placementEdge {
		t.Fatalf("desiredPlacement = %q, want %q", updated.Status.DesiredPlacement, placementEdge)
	}
}

func TestReconcileSkipsDuplicatePlacementWithoutNewTelemetry(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen mqtt stub: %v", err)
	}
	defer listener.Close()

	go serveMQTTStub(t, listener)
	t.Setenv("SIF_MQTT_BROKER", listener.Addr().String())
	t.Setenv("SIF_MQTT_USER", "")
	t.Setenv("SIF_MQTT_TOKEN", "")
	t.Setenv("SIF_MQTT_CLIENT_ID", "test-operator")

	scheme := runtime.NewScheme()
	if err := edgev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add edge scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	commandTime := metav1.NewTime(time.Unix(300, 0))
	artifact := []byte("duplicate placement artifact")
	digest := digestBytes(artifact)
	source := newSourceArtifactServer(artifact, digest)
	defer source.Close()
	hostDigest := digest
	host := newMutableHostServer(&hostDigest, func(body []byte) { hostDigest = digestBytes(body) })
	defer host.Close()
	wf := &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Image:   "localhost:30500/sif-edge-host:latest",
			Device:  edgev1alpha1.DeviceSpec{ControlTopic: "64/199/data", OperatorWasmURL: source.URL, ArtifactURL: "http://device.invalid/wasm"},
			Release: edgev1alpha1.ReleaseSpec{Generation: 3, ArtifactDigest: digest, FunctionIdentity: "dht-reader"},
			Placement: edgev1alpha1.PlacementSpec{
				Mode:                 placementEdge,
				LowBatteryThreshold:  20,
				HighBatteryThreshold: 60,
			},
		},
		Status: edgev1alpha1.WasmFunctionStatus{
			ObservedMode:                    placementLocal,
			LastAppliedLowBatteryThreshold:  20,
			LastAppliedHighBatteryThreshold: 60,
			LastCommandedPlacement:          placementEdge,
			LastCommandedRuntimeMode:        placementEdge,
			LastCommandID:                   "runtime-edge-3",
			LastCommandTime:                 &commandTime,
			LastTelemetryTime:               &commandTime,
			StagedReleaseGeneration:         3,
			DeviceStagedArtifactDigest:      digest,
			ObservedFunction:                "dht-reader",
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&edgev1alpha1.WasmFunction{}).
		WithObjects(wf).
		Build()

	r := &WasmFunctionReconciler{Client: cl, Scheme: scheme, HTTPClient: http.DefaultClient,
		HostWasmURLResolver:  func(*edgev1alpha1.WasmFunction) string { return host.URL },
		EdgeRuntimeAvailable: func(context.Context, *edgev1alpha1.WasmFunction) bool { return true },
		Now:                  func() time.Time { return commandTime.Add(10 * time.Second) }}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "dht-reader", Namespace: "sertay"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated edgev1alpha1.WasmFunction
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "dht-reader", Namespace: "sertay"}, &updated); err != nil {
		t.Fatalf("get updated wasmfunction: %v", err)
	}
	if updated.Status.LastCommandTime == nil {
		t.Fatalf("expected lastCommandTime to be preserved")
	}
	if !updated.Status.LastCommandTime.Equal(&commandTime) {
		t.Fatalf("lastCommandTime = %v, want %v", updated.Status.LastCommandTime, commandTime)
	}
}

func serveMQTTStub(t *testing.T, listener net.Listener) {
	t.Helper()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go handleMQTTStubConn(t, conn)
	}
}

func handleMQTTStubConn(t *testing.T, conn net.Conn) {
	t.Helper()
	defer conn.Close()

	if err := drainMQTTPacket(conn); err != nil {
		return
	}
	if _, err := conn.Write([]byte{0x20, 0x02, 0x00, 0x00}); err != nil {
		return
	}
	_ = drainMQTTPacket(conn)
	if _, err := conn.Write([]byte{0x40, 0x02, 0x00, 0x01}); err != nil {
		return
	}
	_, _ = io.CopyN(io.Discard, conn, 2)
}

func drainMQTTPacket(conn net.Conn) error {
	header := make([]byte, 1)
	if _, err := io.ReadFull(conn, header); err != nil {
		return err
	}
	remaining, err := mqttReadRemainingLength(conn)
	if err != nil {
		return err
	}
	if remaining == 0 {
		return nil
	}
	_, err = io.CopyN(io.Discard, conn, int64(remaining))
	return err
}
