package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func newControllerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
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
	return scheme
}

func newSourceArtifactServer(artifact []byte, digest string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Wasm-Sha256", digest)
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(artifact)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
}

func newMutableHostServer(digest *string, onPut func([]byte)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("X-Wasm-Sha256", *digest)
			w.WriteHeader(http.StatusOK)
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			onPut(body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"status":"ok","digest":"%s"}`, *digest)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
}

func TestReconcileArtifactSynchronizesHostToDesiredDigest(t *testing.T) {
	desiredArtifact := []byte("desired wasm artifact")
	desiredDigest := digestBytes(desiredArtifact)
	hostArtifact := []byte("stale host artifact")
	hostDigest := digestBytes(hostArtifact)

	source := newSourceArtifactServer(desiredArtifact, desiredDigest)
	defer source.Close()

	host := newMutableHostServer(&hostDigest, func(body []byte) {
		hostArtifact = append([]byte(nil), body...)
		hostDigest = digestBytes(hostArtifact)
	})
	defer host.Close()

	r := &WasmFunctionReconciler{
		HTTPClient: http.DefaultClient,
		HostWasmURLResolver: func(*edgev1alpha1.WasmFunction) string {
			return host.URL
		},
	}

	decision, err := r.reconcileArtifact(context.Background(), &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Device: edgev1alpha1.DeviceSpec{OperatorWasmURL: source.URL},
		},
	}, true)
	if err != nil {
		t.Fatalf("reconcileArtifact: %v", err)
	}
	if decision.DesiredDigest != desiredDigest {
		t.Fatalf("desired digest = %q, want %q", decision.DesiredDigest, desiredDigest)
	}
	if decision.HostDigest != desiredDigest {
		t.Fatalf("host digest = %q, want %q", decision.HostDigest, desiredDigest)
	}
	if !decision.UpdatedHost {
		t.Fatalf("expected host artifact to be updated")
	}
	if got := digestBytes(hostArtifact); got != desiredDigest {
		t.Fatalf("host artifact digest after sync = %q, want %q", got, desiredDigest)
	}
}

func TestReconcileArtifactSendsCallerHeader(t *testing.T) {
	desiredArtifact := []byte("desired wasm artifact")
	desiredDigest := digestBytes(desiredArtifact)
	hostDigest := strings.Repeat("a", 64)
	var sourceCallers []string
	var hostHeadCallers []string
	var hostPutCallers []string

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceCallers = append(sourceCallers, r.Header.Get(artifactCallerHeader))
		w.Header().Set("X-Wasm-Sha256", desiredDigest)
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(desiredArtifact)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer source.Close()

	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			hostHeadCallers = append(hostHeadCallers, r.Header.Get(artifactCallerHeader))
			w.Header().Set("X-Wasm-Sha256", hostDigest)
			w.WriteHeader(http.StatusOK)
		case http.MethodPut:
			hostPutCallers = append(hostPutCallers, r.Header.Get(artifactCallerHeader))
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			hostDigest = digestBytes(body)
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer host.Close()

	r := &WasmFunctionReconciler{
		HTTPClient: http.DefaultClient,
		HostWasmURLResolver: func(*edgev1alpha1.WasmFunction) string {
			return host.URL
		},
	}

	if _, err := r.reconcileArtifact(context.Background(), &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Device: edgev1alpha1.DeviceSpec{OperatorWasmURL: source.URL},
		},
	}, true); err != nil {
		t.Fatalf("reconcileArtifact: %v", err)
	}

	for i, caller := range sourceCallers {
		if caller != operatorArtifactCaller {
			t.Fatalf("source caller[%d] = %q, want %q", i, caller, operatorArtifactCaller)
		}
	}
	for i, caller := range hostHeadCallers {
		if caller != operatorArtifactCaller {
			t.Fatalf("host HEAD caller[%d] = %q, want %q", i, caller, operatorArtifactCaller)
		}
	}
	for i, caller := range hostPutCallers {
		if caller != operatorArtifactCaller {
			t.Fatalf("host PUT caller[%d] = %q, want %q", i, caller, operatorArtifactCaller)
		}
	}
	if len(sourceCallers) < 2 {
		t.Fatalf("expected HEAD and GET against source artifact, saw %d calls", len(sourceCallers))
	}
	if len(hostHeadCallers) != 1 {
		t.Fatalf("host HEAD calls = %d, want 1", len(hostHeadCallers))
	}
	if len(hostPutCallers) != 1 {
		t.Fatalf("host PUT calls = %d, want 1", len(hostPutCallers))
	}
}

func TestReconcileSkipsHostArtifactSyncWhenLocalPlacementDesired(t *testing.T) {
	scheme := newControllerTestScheme(t)

	desiredArtifact := []byte("fresh wasm artifact")
	desiredDigest := digestBytes(desiredArtifact)
	staleHostDigest := strings.Repeat("c", 64)
	hostHeadCalls := 0
	hostPutCalls := 0

	source := newSourceArtifactServer(desiredArtifact, desiredDigest)
	defer source.Close()

	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			hostHeadCalls++
			w.Header().Set("X-Wasm-Sha256", staleHostDigest)
			w.WriteHeader(http.StatusOK)
		case http.MethodPut:
			hostPutCalls++
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer host.Close()

	wf := &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Image:     "localhost:30500/sif-edge-host:latest",
			Placement: edgev1alpha1.PlacementSpec{Mode: placementLocal},
			Device: edgev1alpha1.DeviceSpec{
				ControlTopic:    "64/199/data",
				OperatorWasmURL: source.URL,
			},
		},
		Status: edgev1alpha1.WasmFunctionStatus{
			ObservedArtifactDigest:      desiredDigest,
			LastCommandedPlacement:      placementLocal,
			LastCommandedArtifactDigest: desiredDigest,
			HostArtifactDigest:          staleHostDigest,
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&edgev1alpha1.WasmFunction{}).
		WithObjects(wf).
		Build()

	r := &WasmFunctionReconciler{
		Client:     cl,
		Scheme:     scheme,
		HTTPClient: http.DefaultClient,
		HostWasmURLResolver: func(*edgev1alpha1.WasmFunction) string {
			return host.URL
		},
	}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "dht-reader", Namespace: "sertay"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if hostHeadCalls != 0 {
		t.Fatalf("host HEAD calls = %d, want 0", hostHeadCalls)
	}
	if hostPutCalls != 0 {
		t.Fatalf("host PUT calls = %d, want 0", hostPutCalls)
	}

	var updated edgev1alpha1.WasmFunction
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "dht-reader", Namespace: "sertay"}, &updated); err != nil {
		t.Fatalf("get updated wasmfunction: %v", err)
	}
	if updated.Status.DesiredArtifactDigest != desiredDigest {
		t.Fatalf("desiredArtifactDigest = %q, want %q", updated.Status.DesiredArtifactDigest, desiredDigest)
	}
	if updated.Status.HostArtifactDigest != staleHostDigest {
		t.Fatalf("hostArtifactDigest = %q, want preserved stale digest %q", updated.Status.HostArtifactDigest, staleHostDigest)
	}
	condition := meta.FindStatusCondition(updated.Status.Conditions, "ArtifactSynchronized")
	if condition == nil {
		t.Fatalf("expected ArtifactSynchronized condition")
	}
	if condition.Status != metav1.ConditionUnknown {
		t.Fatalf("ArtifactSynchronized status = %s, want Unknown", condition.Status)
	}
	if condition.Reason != "HostSyncDeferred" {
		t.Fatalf("ArtifactSynchronized reason = %q, want HostSyncDeferred", condition.Reason)
	}
}

func TestShouldReloadLocalArtifactWhenDesiredDigestDiffers(t *testing.T) {
	desiredDigest := strings.Repeat("a", 64)
	staleDigest := strings.Repeat("b", 64)

	wf := &edgev1alpha1.WasmFunction{
		Spec: edgev1alpha1.WasmFunctionSpec{
			Device: edgev1alpha1.DeviceSpec{ReloadWasmURL: "http://example.invalid/wasm"},
		},
		Status: edgev1alpha1.WasmFunctionStatus{
			ObservedArtifactDigest:      staleDigest,
			LastCommandedPlacement:      placementEdge,
			LastCommandedArtifactDigest: staleDigest,
		},
	}

	if !shouldReloadLocalArtifact(wf, placementDecision{Desired: placementLocal}, artifactDecision{Enabled: true, DesiredDigest: desiredDigest}) {
		t.Fatalf("expected reload when desired local digest differs")
	}

	wf.Status.LastCommandedPlacement = placementLocal
	wf.Status.LastCommandedArtifactDigest = desiredDigest
	if shouldReloadLocalArtifact(wf, placementDecision{Desired: placementLocal}, artifactDecision{Enabled: true, DesiredDigest: desiredDigest}) {
		t.Fatalf("expected repeated reload to be suppressed once the digest was already commanded")
	}

	wf.Status.ObservedArtifactDigest = desiredDigest
	if shouldReloadLocalArtifact(wf, placementDecision{Desired: placementLocal}, artifactDecision{Enabled: true, DesiredDigest: desiredDigest}) {
		t.Fatalf("expected no reload once observed digest already matches")
	}
}

func TestShouldReloadLocalArtifactWhenNewTelemetryStillStale(t *testing.T) {
	staleDigest := strings.Repeat("1", 64)
	desiredDigest := strings.Repeat("2", 64)
	commandTime := metav1.Unix(100, 0)
	telemetryTime := metav1.Unix(110, 0)
	wf := &edgev1alpha1.WasmFunction{
		Spec: edgev1alpha1.WasmFunctionSpec{
			Device: edgev1alpha1.DeviceSpec{ReloadWasmURL: "http://example.invalid/wasm"},
		},
		Status: edgev1alpha1.WasmFunctionStatus{
			ObservedArtifactDigest:      staleDigest,
			LastCommandedPlacement:      placementLocal,
			LastCommandedArtifactDigest: desiredDigest,
			LastCommandTime:             &commandTime,
			LastTelemetryTime:           &telemetryTime,
		},
	}

	if !shouldReloadLocalArtifact(wf, placementDecision{Desired: placementLocal}, artifactDecision{Enabled: true, DesiredDigest: desiredDigest}) {
		t.Fatalf("expected reload retry when newer telemetry still reports a stale digest")
	}

	wf.Status.LastTelemetryTime = &commandTime
	if shouldReloadLocalArtifact(wf, placementDecision{Desired: placementLocal}, artifactDecision{Enabled: true, DesiredDigest: desiredDigest}) {
		t.Fatalf("expected reload retry to stay suppressed until newer telemetry arrives")
	}
}

func TestLocalArtifactReloadReasonWhenDesiredDigestDiffers(t *testing.T) {
	desiredDigest := strings.Repeat("a", 64)
	staleDigest := strings.Repeat("b", 64)
	wf := &edgev1alpha1.WasmFunction{
		Status: edgev1alpha1.WasmFunctionStatus{
			ObservedArtifactDigest:      staleDigest,
			LastCommandedPlacement:      placementEdge,
			LastCommandedArtifactDigest: staleDigest,
		},
	}

	reason := localArtifactReloadReason(wf, artifactDecision{Enabled: true, DesiredDigest: desiredDigest})
	want := fmt.Sprintf("desired local artifact sha256=%s differs from observed sha256=%s", desiredDigest, staleDigest)
	if reason != want {
		t.Fatalf("localArtifactReloadReason = %q, want %q", reason, want)
	}
}

func TestLocalArtifactReloadReasonWhenNewTelemetryStillStale(t *testing.T) {
	staleDigest := strings.Repeat("1", 64)
	desiredDigest := strings.Repeat("2", 64)
	commandTime := metav1.Unix(100, 0)
	telemetryTime := metav1.Unix(110, 0)
	wf := &edgev1alpha1.WasmFunction{
		Status: edgev1alpha1.WasmFunctionStatus{
			ObservedArtifactDigest:      staleDigest,
			LastCommandedPlacement:      placementLocal,
			LastCommandedArtifactDigest: desiredDigest,
			LastCommandTime:             &commandTime,
			LastTelemetryTime:           &telemetryTime,
		},
	}

	reason := localArtifactReloadReason(wf, artifactDecision{Enabled: true, DesiredDigest: desiredDigest})
	want := fmt.Sprintf("newer telemetry still reports observed sha256=%s after local reload to sha256=%s", staleDigest, desiredDigest)
	if reason != want {
		t.Fatalf("localArtifactReloadReason = %q, want %q", reason, want)
	}
}

func TestReconcilePersistsArtifactDigestsInStatus(t *testing.T) {
	scheme := newControllerTestScheme(t)

	desiredArtifact := []byte("fresh wasm artifact")
	desiredDigest := digestBytes(desiredArtifact)

	source := newSourceArtifactServer(desiredArtifact, desiredDigest)
	defer source.Close()

	hostDigest := strings.Repeat("c", 64)
	host := newMutableHostServer(&hostDigest, func(body []byte) {
		hostDigest = digestBytes(body)
	})
	defer host.Close()

	wf := &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Image:  "localhost:30500/sif-edge-host:latest",
			Device: edgev1alpha1.DeviceSpec{OperatorWasmURL: source.URL},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&edgev1alpha1.WasmFunction{}).
		WithObjects(wf).
		Build()

	r := &WasmFunctionReconciler{
		Client:     cl,
		Scheme:     scheme,
		HTTPClient: http.DefaultClient,
		HostWasmURLResolver: func(*edgev1alpha1.WasmFunction) string {
			return host.URL
		},
	}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "dht-reader", Namespace: "sertay"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated edgev1alpha1.WasmFunction
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "dht-reader", Namespace: "sertay"}, &updated); err != nil {
		t.Fatalf("get updated wasmfunction: %v", err)
	}
	if updated.Status.DesiredArtifactDigest != desiredDigest {
		t.Fatalf("desiredArtifactDigest = %q, want %q", updated.Status.DesiredArtifactDigest, desiredDigest)
	}
	if updated.Status.HostArtifactDigest != desiredDigest {
		t.Fatalf("hostArtifactDigest = %q, want %q", updated.Status.HostArtifactDigest, desiredDigest)
	}
	condition := meta.FindStatusCondition(updated.Status.Conditions, "ArtifactSynchronized")
	if condition == nil {
		t.Fatalf("expected ArtifactSynchronized condition")
	}
	if condition.Status != metav1.ConditionTrue {
		t.Fatalf("ArtifactSynchronized status = %s, want True", condition.Status)
	}
}

func TestOperatorArtifactURLPrefersSplitField(t *testing.T) {
	wf := &edgev1alpha1.WasmFunction{
		Spec: edgev1alpha1.WasmFunctionSpec{
			Device: edgev1alpha1.DeviceSpec{
				OperatorWasmURL: "http://operator.example/wasm",
				WasmURL:         "http://legacy.example/wasm",
			},
		},
	}

	if got := operatorArtifactURL(wf); got != "http://operator.example/wasm" {
		t.Fatalf("operatorArtifactURL = %q, want split operator URL", got)
	}
}

func TestDeviceReloadURLFallsBackToLegacyField(t *testing.T) {
	wf := &edgev1alpha1.WasmFunction{
		Spec: edgev1alpha1.WasmFunctionSpec{
			Device: edgev1alpha1.DeviceSpec{WasmURL: "http://legacy.example/wasm"},
		},
	}

	if got := deviceReloadURL(wf); got != "http://legacy.example/wasm" {
		t.Fatalf("deviceReloadURL = %q, want legacy fallback URL", got)
	}
}
