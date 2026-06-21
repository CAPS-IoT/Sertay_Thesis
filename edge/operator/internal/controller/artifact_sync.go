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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	edgev1alpha1 "github.com/sertayy/sif-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	maxArtifactDownloadBytes  = 8 << 20
	maxArtifactErrorBodyBytes = 4096
	artifactHTTPTimeout       = 15 * time.Second
	artifactCallerHeader      = "X-SIF-Artifact-Caller"
	operatorArtifactCaller    = "operator-artifact-sync"
)

var errArtifactSourceDigestTransition = errors.New("artifact source digest transition pending")

type artifactDecision struct {
	Enabled              bool
	SourceURL            string
	HostURL              string
	DesiredDigest        string
	HostDigest           string
	HostStagedDigest     string
	HostActiveGeneration int64
	HostStagedGeneration int64
	HostActiveFunction   string
	HostStagedFunction   string
	UpdatedHost          bool
}

type hostReleaseStatus struct {
	ActiveGeneration int64  `json:"activeGeneration"`
	ActiveDigest     string `json:"activeDigest"`
	ActiveFunction   string `json:"activeFunction"`
	StagedGeneration int64  `json:"stagedGeneration"`
	StagedDigest     string `json:"stagedDigest"`
	StagedFunction   string `json:"stagedFunction"`
}

func (r *WasmFunctionReconciler) reconcileArtifact(ctx context.Context, wf *edgev1alpha1.WasmFunction) (artifactDecision, error) {
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
	declaredDigest := normalizeArtifactDigest(wf.Spec.Release.ArtifactDigest)
	if declaredDigest == "" {
		return decision, fmt.Errorf("spec.release.artifactDigest is required when device.operatorWasmURL is configured")
	}
	if desiredDigest != declaredDigest {
		return decision, fmt.Errorf("%w: authoritative source digest %s does not match declared release digest %s", errArtifactSourceDigestTransition, desiredDigest, declaredDigest)
	}
	decision.DesiredDigest = desiredDigest

	hostStatus, err := r.fetchHostReleaseStatus(ctx, decision.HostURL)
	if err != nil {
		return decision, fmt.Errorf("probe host release status from %s: %w", decision.HostURL, err)
	}
	decision.HostDigest = normalizeArtifactDigest(hostStatus.ActiveDigest)
	decision.HostStagedDigest = normalizeArtifactDigest(hostStatus.StagedDigest)
	decision.HostActiveGeneration = hostStatus.ActiveGeneration
	decision.HostStagedGeneration = hostStatus.StagedGeneration
	decision.HostActiveFunction = strings.TrimSpace(hostStatus.ActiveFunction)
	decision.HostStagedFunction = strings.TrimSpace(hostStatus.StagedFunction)
	if hostReleaseMatches(decision.HostDigest, decision.HostActiveGeneration, decision.HostActiveFunction, wf) ||
		hostReleaseMatches(decision.HostStagedDigest, decision.HostStagedGeneration, decision.HostStagedFunction, wf) {
		return decision, nil
	}

	artifactBytes, actualDigest, err := r.fetchArtifact(ctx, sourceURL)
	if err != nil {
		return decision, fmt.Errorf("download desired artifact from %s: %w", sourceURL, err)
	}
	if actualDigest != desiredDigest {
		return decision, fmt.Errorf("desired artifact digest changed during sync: resolved %s, downloaded %s", desiredDigest, actualDigest)
	}

	if err := r.uploadArtifact(ctx, decision.HostURL, artifactBytes, wf); err != nil {
		return decision, fmt.Errorf("upload desired artifact to host %s: %w", decision.HostURL, err)
	}

	decision.HostStagedDigest = actualDigest
	decision.HostStagedGeneration = wf.Spec.Release.Generation
	decision.HostStagedFunction = wf.Spec.Release.FunctionIdentity
	decision.UpdatedHost = true
	logf.FromContext(ctx).Info(
		"Synced edge host artifact",
		"hostURL", decision.HostURL,
		"hostStagedArtifactDigest", artifactDigestForLog(actualDigest),
		"desiredArtifactDigest", artifactDigestForLog(desiredDigest),
		"sourceURL", sourceURL,
	)
	return decision, nil
}

func artifactSourceDigestTransitionPending(err error) bool {
	return errors.Is(err, errArtifactSourceDigestTransition)
}

func hostReleaseMatches(digest string, generation int64, function string, wf *edgev1alpha1.WasmFunction) bool {
	return normalizeArtifactDigest(digest) == normalizeArtifactDigest(wf.Spec.Release.ArtifactDigest) &&
		generation == wf.Spec.Release.Generation &&
		strings.TrimSpace(function) == strings.TrimSpace(wf.Spec.Release.FunctionIdentity)
}

func (r *WasmFunctionReconciler) fetchHostReleaseStatus(ctx context.Context, wasmURL string) (hostReleaseStatus, error) {
	releaseURL := strings.TrimSuffix(wasmURL, "/wasm") + "/release"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL, nil)
	if err != nil {
		return hostReleaseStatus{}, fmt.Errorf("build GET release request: %w", err)
	}
	request.Header.Set(artifactCallerHeader, operatorArtifactCaller)
	response, err := r.httpClient().Do(request)
	if err != nil {
		return hostReleaseStatus{}, fmt.Errorf("GET %s failed: %w", releaseURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return hostReleaseStatus{}, fmt.Errorf("GET %s returned %s", releaseURL, response.Status)
	}
	var status hostReleaseStatus
	if err := json.NewDecoder(io.LimitReader(response.Body, maxArtifactErrorBodyBytes)).Decode(&status); err != nil {
		return hostReleaseStatus{}, fmt.Errorf("decode GET %s: %w", releaseURL, err)
	}
	return status, nil
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

func (r *WasmFunctionReconciler) uploadArtifact(ctx context.Context, url string, artifact []byte, wf *edgev1alpha1.WasmFunction) error {
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(artifact))
	if err != nil {
		return fmt.Errorf("build PUT request: %w", err)
	}
	putReq.Header.Set(artifactCallerHeader, operatorArtifactCaller)
	putReq.Header.Set("Content-Type", "application/wasm")
	putReq.Header.Set("X-Wasm-Sha256", wf.Spec.Release.ArtifactDigest)
	putReq.Header.Set("X-SIF-Release-Generation", fmt.Sprintf("%d", wf.Spec.Release.Generation))
	putReq.Header.Set("X-SIF-Function-Identity", wf.Spec.Release.FunctionIdentity)

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

func (r *WasmFunctionReconciler) activateHostRelease(ctx context.Context, wf *edgev1alpha1.WasmFunction) error {
	releaseURL := strings.TrimSuffix(r.hostWasmURL(wf), "/wasm") + "/release"
	body, err := json.Marshal(map[string]int64{"releaseGeneration": wf.Spec.Release.Generation})
	if err != nil {
		return fmt.Errorf("marshal host activation request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, releaseURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build POST release request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(artifactCallerHeader, operatorArtifactCaller)
	response, err := r.httpClient().Do(request)
	if err != nil {
		return fmt.Errorf("POST %s failed: %w", releaseURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, maxArtifactErrorBodyBytes))
		return fmt.Errorf("POST %s returned %s: %s", releaseURL, response.Status, strings.TrimSpace(string(message)))
	}
	return nil
}

func (r *WasmFunctionReconciler) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return &http.Client{Timeout: artifactHTTPTimeout}
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

func artifactDigestForLog(digest string) string {
	normalized := normalizeArtifactDigest(digest)
	if normalized == "" {
		return strings.TrimSpace(digest)
	}
	if len(normalized) <= 12 {
		return normalized
	}
	return normalized[:12] + "..."
}

func operatorArtifactURL(wf *edgev1alpha1.WasmFunction) string {
	return strings.TrimSpace(wf.Spec.Device.OperatorWasmURL)
}

func deviceArtifactURL(wf *edgev1alpha1.WasmFunction) string {
	return strings.TrimSpace(wf.Spec.Device.ArtifactURL)
}

func deviceReleaseReady(wf *edgev1alpha1.WasmFunction, digest string) bool {
	return deviceReleaseStaged(wf, digest) || deviceReleaseActive(wf, digest)
}

func deviceReleaseStaged(wf *edgev1alpha1.WasmFunction, digest string) bool {
	return wf.Status.StagedReleaseGeneration == wf.Spec.Release.Generation &&
		normalizeArtifactDigest(wf.Status.DeviceStagedArtifactDigest) == digest
}

func deviceReleaseActive(wf *edgev1alpha1.WasmFunction, digest string) bool {
	return wf.Status.AcknowledgedReleaseGeneration == wf.Spec.Release.Generation &&
		normalizeArtifactDigest(wf.Status.ObservedArtifactDigest) == digest &&
		strings.TrimSpace(wf.Status.ObservedFunction) == strings.TrimSpace(wf.Spec.Release.FunctionIdentity)
}

func artifactStatusCondition(wf *edgev1alpha1.WasmFunction, artifact artifactDecision, err error) metav1.Condition {
	condition := metav1.Condition{
		Type:               "ArtifactSynchronized",
		LastTransitionTime: metav1.Now(),
	}
	if !artifact.Enabled {
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "ArtifactSourceMissing"
		condition.Message = "device.operatorWasmURL is not configured"
		return condition
	}
	if err != nil {
		if artifactSourceDigestTransitionPending(err) {
			condition.Status = metav1.ConditionUnknown
			condition.Reason = "SourceUpdatePending"
		} else {
			condition.Status = metav1.ConditionFalse
			condition.Reason = "SyncFailed"
		}
		condition.Message = err.Error()
		return condition
	}
	hostReady := hostReleaseMatches(artifact.HostDigest, artifact.HostActiveGeneration, artifact.HostActiveFunction, wf) ||
		hostReleaseMatches(artifact.HostStagedDigest, artifact.HostStagedGeneration, artifact.HostStagedFunction, wf)
	deviceReady := deviceReleaseReady(wf, artifact.DesiredDigest)
	if !hostReady || !deviceReady {
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "StagingPending"
		condition.Message = fmt.Sprintf("release staging pending: hostReady=%t deviceReady=%t sha256=%s", hostReady, deviceReady, artifact.DesiredDigest)
		return condition
	}
	condition.Status = metav1.ConditionTrue
	if artifact.UpdatedHost {
		condition.Reason = "HostStaged"
		condition.Message = fmt.Sprintf("host staged release sha256=%s", artifact.DesiredDigest)
		return condition
	}
	condition.Reason = "HostReady"
	condition.Message = fmt.Sprintf("host active or staged release matches sha256=%s", artifact.DesiredDigest)
	return condition
}
