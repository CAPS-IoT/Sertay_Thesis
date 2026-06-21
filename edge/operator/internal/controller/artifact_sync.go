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

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"

	edgev1alpha1 "github.com/sertayy/sif-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	maxArtifactDownloadBytes  = 8 << 20
	maxArtifactErrorBodyBytes = 4096
	artifactCallerHeader      = "X-SIF-Artifact-Caller"
	operatorArtifactCaller    = "operator-artifact-sync"
)

type artifactDecision struct {
	Enabled       bool
	SourceURL     string
	HostURL       string
	DesiredDigest string
	HostDigest    string
	UpdatedHost   bool
	DeferredHost  bool
}

func (r *WasmFunctionReconciler) reconcileArtifact(ctx context.Context, wf *edgev1alpha1.WasmFunction, syncHost bool) (artifactDecision, error) {
	sourceURL := operatorArtifactURL(wf)
	if sourceURL == "" {
		return artifactDecision{}, nil
	}

	decision := artifactDecision{
		Enabled:    true,
		SourceURL:  sourceURL,
		HostURL:    r.hostWasmURL(wf),
		HostDigest: normalizeArtifactDigest(wf.Status.HostArtifactDigest),
	}

	desiredDigest, err := r.fetchArtifactDigest(ctx, sourceURL)
	if err != nil {
		return decision, fmt.Errorf("resolve desired artifact digest from %s: %w", sourceURL, err)
	}
	decision.DesiredDigest = desiredDigest
	if !syncHost {
		decision.DeferredHost = true
		return decision, nil
	}

	hostDigest, err := r.fetchArtifactDigest(ctx, decision.HostURL)
	if err != nil {
		return decision, fmt.Errorf("probe host artifact digest from %s: %w", decision.HostURL, err)
	}
	decision.HostDigest = hostDigest
	if hostDigest == desiredDigest {
		return decision, nil
	}

	artifactBytes, actualDigest, err := r.fetchArtifact(ctx, sourceURL)
	if err != nil {
		return decision, fmt.Errorf("download desired artifact from %s: %w", sourceURL, err)
	}
	if actualDigest != desiredDigest {
		return decision, fmt.Errorf("desired artifact digest changed during sync: resolved %s, downloaded %s", desiredDigest, actualDigest)
	}

	if err := r.uploadArtifact(ctx, decision.HostURL, artifactBytes); err != nil {
		return decision, fmt.Errorf("upload desired artifact to host %s: %w", decision.HostURL, err)
	}

	decision.HostDigest = actualDigest
	decision.UpdatedHost = true
	return decision, nil
}

func (r *WasmFunctionReconciler) fetchArtifactDigest(ctx context.Context, url string) (string, error) {
	headReq, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", fmt.Errorf("build HEAD request: %w", err)
	}
	headReq.Header.Set(artifactCallerHeader, operatorArtifactCaller)

	headResp, headErr := r.httpClient().Do(headReq)
	if headErr == nil {
		defer headResp.Body.Close()
		if headResp.StatusCode >= 200 && headResp.StatusCode < 300 {
			if digest := artifactDigestFromHeaders(headResp.Header); digest != "" {
				return digest, nil
			}
		} else if headResp.StatusCode != http.StatusMethodNotAllowed && headResp.StatusCode != http.StatusNotImplemented {
			return "", fmt.Errorf("HEAD %s returned %s", url, headResp.Status)
		}
	}

	_, digest, getErr := r.fetchArtifact(ctx, url)
	if getErr != nil {
		if headErr != nil {
			return "", fmt.Errorf("HEAD %s failed: %v; GET fallback failed: %w", url, headErr, getErr)
		}
		return "", getErr
	}
	return digest, nil
}

func (r *WasmFunctionReconciler) fetchArtifact(ctx context.Context, url string) ([]byte, string, error) {
	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build GET request: %w", err)
	}
	getReq.Header.Set(artifactCallerHeader, operatorArtifactCaller)

	resp, err := r.httpClient().Do(getReq)
	if err != nil {
		return nil, "", fmt.Errorf("GET %s failed: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxArtifactErrorBodyBytes))
		message := strings.TrimSpace(string(body))
		if message == "" {
			return nil, "", fmt.Errorf("GET %s returned %s", url, resp.Status)
		}
		return nil, "", fmt.Errorf("GET %s returned %s: %s", url, resp.Status, message)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxArtifactDownloadBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read GET %s body: %w", url, err)
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("GET %s returned an empty artifact", url)
	}
	if len(data) > maxArtifactDownloadBytes {
		return nil, "", fmt.Errorf("GET %s exceeded %d bytes", url, maxArtifactDownloadBytes)
	}

	headerDigest := artifactDigestFromHeaders(resp.Header)
	actualDigest := digestBytes(data)
	if headerDigest != "" && headerDigest != actualDigest {
		return nil, "", fmt.Errorf("GET %s digest header %s did not match body digest %s", url, headerDigest, actualDigest)
	}
	if headerDigest == "" {
		headerDigest = actualDigest
	}

	return data, headerDigest, nil
}

func (r *WasmFunctionReconciler) uploadArtifact(ctx context.Context, url string, artifact []byte) error {
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(artifact))
	if err != nil {
		return fmt.Errorf("build PUT request: %w", err)
	}
	putReq.Header.Set(artifactCallerHeader, operatorArtifactCaller)
	putReq.Header.Set("Content-Type", "application/wasm")

	resp, err := r.httpClient().Do(putReq)
	if err != nil {
		return fmt.Errorf("PUT %s failed: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxArtifactErrorBodyBytes))
		message := strings.TrimSpace(string(body))
		if message == "" {
			return fmt.Errorf("PUT %s returned %s", url, resp.Status)
		}
		return fmt.Errorf("PUT %s returned %s: %s", url, resp.Status, message)
	}

	return nil
}

func (r *WasmFunctionReconciler) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return http.DefaultClient
}

func (r *WasmFunctionReconciler) hostWasmURL(wf *edgev1alpha1.WasmFunction) string {
	if r.HostWasmURLResolver != nil {
		return r.HostWasmURLResolver(wf)
	}
	return fmt.Sprintf("http://%s.%s:%d/wasm", wf.Name, wf.Namespace, effectivePort(wf))
}

func artifactDigestFromHeaders(header http.Header) string {
	if digest := normalizeArtifactDigest(header.Get("X-Wasm-Sha256")); digest != "" {
		return digest
	}

	etag := strings.TrimSpace(header.Get("ETag"))
	etag = strings.TrimPrefix(etag, "W/")
	etag = strings.Trim(etag, `"`)
	etag = strings.TrimPrefix(strings.ToLower(etag), "sha256:")
	return normalizeArtifactDigest(etag)
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func operatorArtifactURL(wf *edgev1alpha1.WasmFunction) string {
	if url := strings.TrimSpace(wf.Spec.Device.OperatorWasmURL); url != "" {
		return url
	}
	return strings.TrimSpace(wf.Spec.Device.WasmURL)
}

func deviceReloadURL(wf *edgev1alpha1.WasmFunction) string {
	if url := strings.TrimSpace(wf.Spec.Device.ReloadWasmURL); url != "" {
		return url
	}
	return strings.TrimSpace(wf.Spec.Device.WasmURL)
}

func shouldSyncHostArtifact(decision placementDecision) bool {
	return !decision.Enabled || decision.Desired != placementLocal
}

func shouldReloadLocalArtifact(wf *edgev1alpha1.WasmFunction, decision placementDecision, artifact artifactDecision) bool {
	if decision.Desired != placementLocal || !artifact.Enabled || artifact.DesiredDigest == "" || deviceReloadURL(wf) == "" {
		return false
	}

	if normalizeArtifactDigest(wf.Status.ObservedArtifactDigest) == artifact.DesiredDigest {
		return false
	}

	if wf.Status.LastCommandedPlacement != placementLocal ||
		normalizeArtifactDigest(wf.Status.LastCommandedArtifactDigest) != artifact.DesiredDigest {
		return true
	}

	if wf.Status.LastTelemetryTime == nil || wf.Status.LastCommandTime == nil {
		return false
	}

	return wf.Status.LastTelemetryTime.After(wf.Status.LastCommandTime.Time)
}

func localArtifactReloadReason(wf *edgev1alpha1.WasmFunction, artifact artifactDecision) string {
	desiredDigest := artifact.DesiredDigest
	if desiredDigest == "" {
		return ""
	}

	observedDigest := normalizeArtifactDigest(wf.Status.ObservedArtifactDigest)
	if observedDigest == desiredDigest {
		return ""
	}
	if observedDigest == "" {
		observedDigest = "unknown"
	}

	if wf.Status.LastCommandedPlacement != placementLocal ||
		normalizeArtifactDigest(wf.Status.LastCommandedArtifactDigest) != desiredDigest {
		return fmt.Sprintf("desired local artifact sha256=%s differs from observed sha256=%s", desiredDigest, observedDigest)
	}

	if wf.Status.LastTelemetryTime != nil && wf.Status.LastCommandTime != nil &&
		wf.Status.LastTelemetryTime.After(wf.Status.LastCommandTime.Time) {
		return fmt.Sprintf("newer telemetry still reports observed sha256=%s after local reload to sha256=%s", observedDigest, desiredDigest)
	}

	return ""
}

func artifactStatusCondition(artifact artifactDecision, err error) metav1.Condition {
	condition := metav1.Condition{
		Type:               "ArtifactSynchronized",
		LastTransitionTime: metav1.Now(),
	}
	if !artifact.Enabled {
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "ArtifactSourceMissing"
		condition.Message = "device.operatorWasmURL (or legacy device.wasmURL) is not configured"
		return condition
	}
	if err != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "SyncFailed"
		condition.Message = err.Error()
		return condition
	}
	if artifact.DeferredHost {
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "HostSyncDeferred"
		condition.Message = fmt.Sprintf("authoritative artifact sha256=%s resolved; host sync deferred while local placement is desired", artifact.DesiredDigest)
		return condition
	}
	condition.Status = metav1.ConditionTrue
	if artifact.UpdatedHost {
		condition.Reason = "HostUpdated"
		condition.Message = fmt.Sprintf("host /wasm synchronized to sha256=%s", artifact.DesiredDigest)
		return condition
	}
	condition.Reason = "AlreadyCurrent"
	condition.Message = fmt.Sprintf("host /wasm already serving sha256=%s", artifact.DesiredDigest)
	return condition
}
