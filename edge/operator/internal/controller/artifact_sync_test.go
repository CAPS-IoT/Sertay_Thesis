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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
	var stagedDigest string
	var stagedGeneration int64
	var stagedFunction string
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/release" {
			switch r.Method {
			case http.MethodGet:
				_, _ = fmt.Fprintf(w, `{"activeGeneration":0,"activeDigest":%q,"activeFunction":"dht-reader","stagedGeneration":%d,"stagedDigest":%q,"stagedFunction":%q}`,
					*digest, stagedGeneration, stagedDigest, stagedFunction)
			case http.MethodPost:
				*digest = stagedDigest
				stagedDigest = ""
				stagedGeneration = 0
				stagedFunction = ""
				w.WriteHeader(http.StatusOK)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}
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
			if onPut != nil {
				onPut(body)
			}
			stagedDigest = digestBytes(body)
			_, _ = fmt.Sscanf(r.Header.Get("X-SIF-Release-Generation"), "%d", &stagedGeneration)
			stagedFunction = r.Header.Get("X-SIF-Function-Identity")
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
}

func TestReconcileArtifactAlwaysSynchronizesDeclaredRelease(t *testing.T) {
	desiredArtifact := []byte("declared wasm artifact")
	desiredDigest := digestBytes(desiredArtifact)
	hostDigest := strings.Repeat("b", 64)
	source := newSourceArtifactServer(desiredArtifact, desiredDigest)
	defer source.Close()
	host := newMutableHostServer(&hostDigest, func(body []byte) {
		hostDigest = digestBytes(body)
	})
	defer host.Close()

	r := &WasmFunctionReconciler{
		HTTPClient: http.DefaultClient,
		HostWasmURLResolver: func(*edgev1alpha1.WasmFunction) string {
			return host.URL
		},
	}
	wf := &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Device:  edgev1alpha1.DeviceSpec{OperatorWasmURL: source.URL},
			Release: edgev1alpha1.ReleaseSpec{Generation: 1, ArtifactDigest: desiredDigest, FunctionIdentity: "dht-reader"},
		},
	}

	decision, err := r.reconcileArtifact(context.Background(), wf)
	if err != nil {
		t.Fatalf("reconcileArtifact: %v", err)
	}
	if decision.DesiredDigest != desiredDigest || decision.HostStagedDigest != desiredDigest {
		t.Fatalf("artifact decision = desired %q staged %q, want %q", decision.DesiredDigest, decision.HostStagedDigest, desiredDigest)
	}
	if !decision.UpdatedHost {
		t.Fatal("expected host staging to run even when placement is not remote")
	}
}

func TestReconcileArtifactRejectsMutableSourceDigestMismatch(t *testing.T) {
	artifact := []byte("source wasm artifact")
	actualDigest := digestBytes(artifact)
	source := newSourceArtifactServer(artifact, actualDigest)
	defer source.Close()

	r := &WasmFunctionReconciler{HTTPClient: http.DefaultClient}
	wf := &edgev1alpha1.WasmFunction{
		ObjectMeta: metav1.ObjectMeta{Name: "dht-reader", Namespace: "sertay"},
		Spec: edgev1alpha1.WasmFunctionSpec{
			Device:  edgev1alpha1.DeviceSpec{OperatorWasmURL: source.URL},
			Release: edgev1alpha1.ReleaseSpec{ArtifactDigest: strings.Repeat("a", 64)},
		},
	}

	_, err := r.reconcileArtifact(context.Background(), wf)
	if err == nil || !strings.Contains(err.Error(), "does not match declared release digest") {
		t.Fatalf("error = %v, want declared digest mismatch", err)
	}
	if !artifactSourceDigestTransitionPending(err) {
		t.Fatalf("error = %v, want source transition classification", err)
	}
	condition := artifactStatusCondition(wf, artifactDecision{Enabled: true}, err)
	if condition.Status != metav1.ConditionUnknown || condition.Reason != "SourceUpdatePending" {
		t.Fatalf("condition = %#v, want SourceUpdatePending", condition)
	}
}

func TestArtifactURLsHaveNoLegacyFallback(t *testing.T) {
	wf := &edgev1alpha1.WasmFunction{Spec: edgev1alpha1.WasmFunctionSpec{
		Device: edgev1alpha1.DeviceSpec{
			OperatorWasmURL: "http://operator.example/wasm",
			ArtifactURL:     "http://device.example/wasm",
		},
	}}
	if got := operatorArtifactURL(wf); got != "http://operator.example/wasm" {
		t.Fatalf("operatorArtifactURL = %q", got)
	}
	if got := deviceArtifactURL(wf); got != "http://device.example/wasm" {
		t.Fatalf("deviceArtifactURL = %q", got)
	}
}

func TestArtifactStatusConditionPreserved(t *testing.T) {
	digest := strings.Repeat("c", 64)
	wf := &edgev1alpha1.WasmFunction{
		Spec: edgev1alpha1.WasmFunctionSpec{Release: edgev1alpha1.ReleaseSpec{
			Generation: 1, ArtifactDigest: digest, FunctionIdentity: "dht-reader",
		}},
		Status: edgev1alpha1.WasmFunctionStatus{
			StagedReleaseGeneration: 1, DeviceStagedArtifactDigest: digest,
		},
	}
	condition := artifactStatusCondition(wf, artifactDecision{
		Enabled: true, DesiredDigest: digest, HostDigest: digest,
		HostActiveGeneration: 1, HostActiveFunction: "dht-reader",
	}, nil)
	if condition.Type != "ArtifactSynchronized" || condition.Status != metav1.ConditionTrue {
		t.Fatalf("condition = %#v", condition)
	}
	if condition.Message != fmt.Sprintf("host active or staged release matches sha256=%s", digest) {
		t.Fatalf("message = %q", condition.Message)
	}
}
