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
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	edgev1alpha1 "github.com/sertayy/sif-operator/api/v1alpha1"
)

// WasmFunctionReconciler reconciles a WasmFunction object
type WasmFunctionReconciler struct {
	client.Client
	Scheme                 *runtime.Scheme
	HTTPClient             *http.Client
	HostWasmURLResolver    func(*edgev1alpha1.WasmFunction) string
	DeadlineEstimator      *DeadlineTelemetryEstimator
	PublishControl         func(context.Context, string, interface{}) error
	EdgeRuntimeAvailable   func(context.Context, *edgev1alpha1.WasmFunction) bool
	Now                    func() time.Time
	releaseCommandMu       sync.Mutex
	releaseStageSends      map[types.NamespacedName]releaseStageSendState
	releaseActivationSends map[types.NamespacedName]releaseActivationSendState
}

type releaseStageSendState struct {
	commandID string
	sentAt    time.Time
	attempts  int32
}

type releaseActivationSendState struct {
	commandID string
	sentAt    time.Time
}

const (
	releaseStageRetryBase = 30 * time.Second
	releaseStageRetryMax  = 60 * time.Second
)

func (r *WasmFunctionReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *WasmFunctionReconciler) publishControl(ctx context.Context, topic string, payload interface{}) error {
	if r.PublishControl != nil {
		return r.PublishControl(ctx, topic, payload)
	}
	return publishControlMessage(ctx, topic, payload)
}

// +kubebuilder:rbac:groups=edge.sif.2iot.2de,resources=wasmfunctions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=edge.sif.2iot.2de,resources=wasmfunctions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=edge.sif.2iot.2de,resources=wasmfunctions/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

func (r *WasmFunctionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Fetch the WasmFunction CR.
	var wf edgev1alpha1.WasmFunction
	if err := r.Get(ctx, req.NamespacedName, &wf); err != nil {
		if errors.IsNotFound(err) {
			log.Info("WasmFunction deleted, nothing to do")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 2. Reconcile the Deployment.
	deploy, err := r.ensureDeployment(ctx, &wf)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 3. Reconcile the Service.
	if err := r.ensureService(ctx, &wf); err != nil {
		return ctrl.Result{}, err
	}

	// 4. Stage the desired release independently of placement.
	artifact, artifactErr := r.reconcileArtifact(ctx, &wf)
	wf.Status.DesiredArtifactDigest = artifact.DesiredDigest
	wf.Status.HostArtifactDigest = artifact.HostDigest
	wf.Status.HostStagedArtifactDigest = artifact.HostStagedDigest
	if artifactErr != nil {
		if artifactSourceDigestTransitionPending(artifactErr) {
			log.V(1).Info("Artifact source update not yet consistent with declared release",
				"sourceURL", artifact.SourceURL, "hostURL", artifact.HostURL,
				"error", artifactErr.Error())
		} else {
			log.Error(artifactErr, "Failed to reconcile artifact", "sourceURL", artifact.SourceURL, "hostURL", artifact.HostURL)
		}
	}
	availableReplicas := r.availableReplicasForDeployment(ctx, deploy)
	edgeRuntimeAvailable := availableReplicas > 0
	if r.EdgeRuntimeAvailable != nil {
		edgeRuntimeAvailable = r.EdgeRuntimeAvailable(ctx, &wf)
	}

	// 5. Resolve the proposal, locality, readiness, and destination admission.
	decision := resolvePlacementWithEstimates(&wf, r.deadlineEstimate(&wf))
	decision = applyDestinationReadiness(&wf, decision, artifact, edgeRuntimeAvailable)
	updateArtifactRetryState(&wf, artifactErr)

	placementErr := error(nil)
	placementCommanded := false
	if decision.Enabled {
		placementCommanded, placementErr = r.applyPlacement(ctx, &wf, decision, artifact)
		if placementErr != nil {
			log.Error(placementErr, "Failed to publish placement command", "topic", wf.Spec.Device.ControlTopic)
		}
	}

	// 6. Update status.
	endpoint := fmt.Sprintf("http://%s.%s:%d/process", wf.Name, wf.Namespace, effectivePort(&wf))

	availableCondition := availableStatusCondition()
	artifactCondition := artifactStatusCondition(&wf, artifact, artifactErr)
	placementCondition := placementStatusCondition(decision, placementCommanded, placementErr)

	statusInput := reconcileStatusUpdate{
		availableReplicas: availableReplicas,
		endpoint:          endpoint,
		decision:          decision,
		available:         availableCondition,
		artifact:          artifactCondition,
		placement:         placementCondition,
	}
	if err := r.updateStatus(ctx, req.NamespacedName, &wf, statusInput); err != nil {
		return ctrl.Result{}, err
	}
	if result, shouldRequeue := requeueOnError(&wf, artifactErr, placementErr); shouldRequeue {
		return result, nil
	}
	if result, shouldRequeue := requeuePendingRelease(&wf, r.now()); shouldRequeue {
		return result, nil
	}

	return ctrl.Result{}, nil
}

func (r *WasmFunctionReconciler) ensureDeployment(ctx context.Context, wf *edgev1alpha1.WasmFunction) (*appsv1.Deployment, error) {
	deploy := r.desiredDeployment(wf)
	if err := ctrl.SetControllerReference(wf, deploy, r.Scheme); err != nil {
		return nil, err
	}

	var foundDeploy appsv1.Deployment
	key := types.NamespacedName{Name: deploy.Name, Namespace: deploy.Namespace}
	err := r.Get(ctx, key, &foundDeploy)
	switch {
	case errors.IsNotFound(err):
		logf.FromContext(ctx).Info("Creating Deployment", "name", deploy.Name)
		if err := r.Create(ctx, deploy); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case deploymentNeedsUpdate(&foundDeploy, deploy):
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, key, &foundDeploy); err != nil {
				return err
			}
			foundDeploy.Spec = deploy.Spec
			return r.Update(ctx, &foundDeploy)
		}); err != nil {
			return nil, err
		}
	}

	return deploy, nil
}

func deploymentNeedsUpdate(found *appsv1.Deployment, desired *appsv1.Deployment) bool {
	return !reflect.DeepEqual(found.Spec.Template, desired.Spec.Template) ||
		!reflect.DeepEqual(found.Spec.Replicas, desired.Spec.Replicas)
}

func (r *WasmFunctionReconciler) ensureService(ctx context.Context, wf *edgev1alpha1.WasmFunction) error {
	svc := r.desiredService(wf)
	if err := ctrl.SetControllerReference(wf, svc, r.Scheme); err != nil {
		return err
	}

	var foundSvc corev1.Service
	key := types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}
	err := r.Get(ctx, key, &foundSvc)
	switch {
	case errors.IsNotFound(err):
		logf.FromContext(ctx).Info("Creating Service", "name", svc.Name)
		return r.Create(ctx, svc)
	case err != nil:
		return err
	case serviceNeedsUpdate(&foundSvc, svc):
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, key, &foundSvc); err != nil {
				return err
			}
			foundSvc.Spec = svc.Spec
			return r.Update(ctx, &foundSvc)
		})
	default:
		return nil
	}
}

func serviceNeedsUpdate(found *corev1.Service, desired *corev1.Service) bool {
	return !reflect.DeepEqual(found.Spec.Ports, desired.Spec.Ports) ||
		!reflect.DeepEqual(found.Spec.Selector, desired.Spec.Selector)
}

func (r *WasmFunctionReconciler) availableReplicasForDeployment(ctx context.Context, deploy *appsv1.Deployment) int32 {
	var current appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: deploy.Name, Namespace: deploy.Namespace}, &current); err == nil {
		return current.Status.AvailableReplicas
	}
	return 0
}

func availableStatusCondition() metav1.Condition {
	return metav1.Condition{
		Type:               "Available",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            "Deployment and Service are reconciled",
		LastTransitionTime: metav1.Now(),
	}
}

type reconcileStatusUpdate struct {
	availableReplicas int32
	endpoint          string
	decision          placementDecision
	available         metav1.Condition
	artifact          metav1.Condition
	placement         metav1.Condition
}

func (r *WasmFunctionReconciler) updateStatus(ctx context.Context, key types.NamespacedName, wf *edgev1alpha1.WasmFunction, update reconcileStatusUpdate) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current edgev1alpha1.WasmFunction
		if err := r.Get(ctx, key, &current); err != nil {
			return err
		}

		current.Status.AvailableReplicas = update.availableReplicas
		current.Status.Endpoint = update.endpoint
		current.Status.DesiredArtifactDigest = wf.Status.DesiredArtifactDigest
		current.Status.HostArtifactDigest = wf.Status.HostArtifactDigest
		current.Status.HostStagedArtifactDigest = wf.Status.HostStagedArtifactDigest
		current.Status.ArtifactSyncStartedAt = wf.Status.ArtifactSyncStartedAt
		current.Status.ArtifactSyncRetryCount = wf.Status.ArtifactSyncRetryCount
		current.Status.ArtifactReadinessReason = wf.Status.ArtifactReadinessReason
		current.Status.SelectedFunctionIdentity = strings.TrimSpace(current.Spec.Release.FunctionIdentity)
		current.Status.DesiredReleaseGeneration = current.Spec.Release.Generation
		current.Status.DesiredPlacement = update.decision.Desired
		current.Status.PlacementReason = update.decision.Reason
		current.Status.ProposalSource = update.decision.ProposalSource
		current.Status.ProposedPlacement = update.decision.Candidate
		current.Status.PredictedCandidateCostMs = copyInt32Ptr(update.decision.Deadline.CostMs)
		current.Status.PredictedCandidateSlackMs = copyInt32Ptr(update.decision.Deadline.SlackMs)
		current.Status.LastAppliedLowBatteryThreshold = wf.Status.LastAppliedLowBatteryThreshold
		current.Status.LastAppliedHighBatteryThreshold = wf.Status.LastAppliedHighBatteryThreshold
		current.Status.LastCommandedPlacement = wf.Status.LastCommandedPlacement
		current.Status.LastCommandedRuntimeMode = wf.Status.LastCommandedRuntimeMode
		current.Status.LastCommandID = wf.Status.LastCommandID
		current.Status.LastCommandTime = wf.Status.LastCommandTime
		current.Status.LastStageCommandID = wf.Status.LastStageCommandID
		current.Status.LastStageCommandTime = wf.Status.LastStageCommandTime
		current.Status.StageCommandAttempts = wf.Status.StageCommandAttempts
		current.Status.LastDeadlineDecisionID = wf.Status.LastDeadlineDecisionID
		current.Status.LastSignaledDeadlineDecisionID = wf.Status.LastSignaledDeadlineDecisionID
		current.Status.LastAppliedBatterySimulationCommandID = wf.Status.LastAppliedBatterySimulationCommandID
		current.Status.LastBatterySimulationCommandTime = wf.Status.LastBatterySimulationCommandTime
		current.Status.ObservedBatteryPercent = copyBatteryPercent(update.decision.BatteryPercent)
		current.Status.ReleaseDeliveryState = releaseDeliveryState(
			&current, current.Status.DesiredArtifactDigest)
		meta.SetStatusCondition(&current.Status.Conditions, update.available)
		meta.SetStatusCondition(&current.Status.Conditions, update.artifact)
		meta.SetStatusCondition(&current.Status.Conditions, update.placement)
		meta.RemoveStatusCondition(&current.Status.Conditions, "DeadlineReady")
		meta.RemoveStatusCondition(&current.Status.Conditions, "DeadlineRisk")
		if update.decision.Deadline.Condition != nil {
			meta.SetStatusCondition(&current.Status.Conditions, *update.decision.Deadline.Condition)
		} else {
			meta.RemoveStatusCondition(&current.Status.Conditions, "DeadlineAdmission")
		}
		return r.Status().Update(ctx, &current)
	})
}

func (r *WasmFunctionReconciler) deadlineEstimate(wf *edgev1alpha1.WasmFunction) deadlineEstimateSnapshot {
	if r.DeadlineEstimator == nil {
		return unavailableDeadlineEstimate(wf)
	}
	return r.DeadlineEstimator.estimate(wf)
}

func copyBatteryPercent(value *int32) *int32 {
	if value == nil {
		return nil
	}
	observedBattery := *value
	return &observedBattery
}

func updateArtifactRetryState(wf *edgev1alpha1.WasmFunction, artifactErr error) {
	if artifactErr == nil {
		wf.Status.ArtifactSyncStartedAt = nil
		wf.Status.ArtifactSyncRetryCount = 0
		return
	}
	if wf.Status.ArtifactSyncStartedAt == nil {
		now := metav1.Now()
		wf.Status.ArtifactSyncStartedAt = &now
	}
	wf.Status.ArtifactSyncRetryCount++
	if time.Since(wf.Status.ArtifactSyncStartedAt.Time) >= 60*time.Second {
		wf.Status.ArtifactReadinessReason = "SyncFailed"
	}
}

func requeueOnError(wf *edgev1alpha1.WasmFunction, artifactErr error, placementErr error) (ctrl.Result, bool) {
	if artifactErr != nil || placementErr != nil {
		retries := wf.Status.ArtifactSyncRetryCount
		if retries < 1 {
			retries = 1
		}
		delay := 5 * time.Second * time.Duration(1<<min(retries-1, 3))
		if delay > 60*time.Second {
			delay = 60 * time.Second
		}
		return ctrl.Result{RequeueAfter: delay}, true
	}
	return ctrl.Result{}, false
}

func requeuePendingRelease(wf *edgev1alpha1.WasmFunction, now time.Time) (ctrl.Result, bool) {
	var retryAt time.Time
	switch releaseDeliveryState(wf, normalizeArtifactDigest(wf.Spec.Release.ArtifactDigest)) {
	case "AwaitingStageAck":
		if wf.Status.LastStageCommandTime == nil {
			return ctrl.Result{RequeueAfter: time.Second}, true
		}
		attempts := wf.Status.StageCommandAttempts
		if attempts < 1 {
			attempts = 1
		}
		retryAt = wf.Status.LastStageCommandTime.Add(releaseStageRetryDelay(attempts))
	case "AwaitingActivationAck":
		if wf.Status.LastCommandTime == nil {
			return ctrl.Result{RequeueAfter: time.Second}, true
		}
		retryAt = wf.Status.LastCommandTime.Add(releaseStageRetryBase)
	default:
		return ctrl.Result{}, false
	}
	delay := retryAt.Sub(now)
	if delay <= 0 {
		delay = time.Second
	}
	return ctrl.Result{RequeueAfter: delay}, true
}

type placementDecision struct {
	Enabled                 bool
	Current                 string
	Proposal                string
	Candidate               string
	Desired                 string
	RuntimeMode             string
	Reason                  string
	ReadinessReason         string
	ProposalSource          string
	Hard                    bool
	Transition              bool
	BatteryPercent          *int32
	LowThreshold            int32
	HighThreshold           int32
	BatteryThresholdEnabled bool
	Deadline                deadlineEvaluation
}

const (
	placementAuto   = "auto"
	placementLocal  = "local"
	placementEdge   = "edge"
	placementHybrid = "hybrid"

	defaultLowBatteryThreshold         int32 = 20
	defaultHighBatteryThreshold        int32 = 60
	defaultDeadlineMinSlackMs          int32 = 500
	defaultBatteryDeltaWindowSeconds   int32 = 60
	defaultBatteryDeltaMaxDrainPercent int32 = 3
	defaultBatteryDeltaRiskyWindows    int32 = 2
	defaultEdgeHostImage                     = "localhost:30500/sif-edge-host:latest"
)

func resolvePlacementWithEstimates(wf *edgev1alpha1.WasmFunction, estimates deadlineEstimateSnapshot) placementDecision {
	low := wf.Spec.Placement.LowBatteryThreshold
	if low == 0 {
		low = defaultLowBatteryThreshold
	}
	high := wf.Spec.Placement.HighBatteryThreshold
	if high == 0 {
		high = defaultHighBatteryThreshold
	}
	if high < low {
		high = low
	}

	decision := placementDecision{
		Enabled:                 wf.Spec.Device.ControlTopic != "",
		Current:                 currentPlacement(wf),
		BatteryPercent:          batteryPercentForPlacement(wf),
		LowThreshold:            low,
		HighThreshold:           high,
		BatteryThresholdEnabled: effectiveBatteryThresholdEnabled(wf),
	}
	decision.Desired = decision.Current
	decision.RuntimeMode = runtimeModeForPlacement(decision.Current)
	if !decision.Enabled {
		decision.Reason = "device control topic not configured"
		return decision
	}

	mode := strings.ToLower(wf.Spec.Placement.Mode)
	if mode == "" {
		mode = placementAuto
	}
	if decision.BatteryThresholdEnabled && decision.Current == placementLocal &&
		decision.BatteryPercent != nil && *decision.BatteryPercent <= low {
		decision.Proposal = placementEdge
		decision.ProposalSource = "LowBatteryThreshold"
		decision.Hard = true
		decision.Reason = fmt.Sprintf("battery %d%% <= low threshold %d%%", *decision.BatteryPercent, low)
	} else {
		switch mode {
		case placementEdge:
			decision.Proposal = placementEdge
			decision.ProposalSource = "ExplicitPlacement"
			decision.Reason = "operator requested edge placement"
		case placementLocal:
			decision.Proposal = placementLocal
			decision.ProposalSource = "ExplicitPlacement"
			decision.Reason = "operator requested local placement"
		case placementAuto:
			if decision.BatteryPercent == nil {
				decision.Reason = "auto placement waiting for battery telemetry"
				break
			}
			if decision.BatteryThresholdEnabled && decision.Current != placementLocal && *decision.BatteryPercent >= high {
				decision.Proposal = placementLocal
				decision.ProposalSource = "HighBatteryThreshold"
				decision.Reason = fmt.Sprintf("battery %d%% >= high threshold %d%%", *decision.BatteryPercent, high)
			} else if localBatteryDeltaForcesEdge(wf, high) {
				decision.Proposal = placementEdge
				decision.ProposalSource = "BatteryDrain"
				decision.Reason = fmt.Sprintf("local battery drain %d%% confirmed for %d windows over %ds",
					*wf.Status.ObservedBatteryDeltaPercent,
					wf.Status.ConsecutiveRiskyWindows,
					effectiveBatteryDeltaWindowSeconds(wf))
			} else {
				decision.Reason = fmt.Sprintf("retaining accepted %s placement", decision.Current)
			}
		default:
			decision.Reason = fmt.Sprintf("unsupported placement mode %q", wf.Spec.Placement.Mode)
		}
	}

	if decision.Proposal == "" {
		decision.Candidate = decision.Current
		decision.Deadline = evaluateDeadlineAdmission(wf, decision.Current, false, estimates)
		return decision
	}

	decision.Candidate = applyResourceLocality(wf, decision.Proposal)
	decision.Transition = decision.Candidate != decision.Current
	decision.Deadline = evaluateDeadlineAdmission(wf, decision.Candidate, decision.Hard, estimates)
	if !decision.Transition || decision.Deadline.Accepted {
		decision.Desired = decision.Candidate
		decision.RuntimeMode = runtimeModeForPlacement(decision.Candidate)
	} else {
		decision.Desired = decision.Current
		decision.RuntimeMode = runtimeModeForPlacement(decision.Current)
		if decision.Deadline.Condition != nil {
			decision.Reason = decision.Deadline.Condition.Message
		}
	}
	return decision
}

func currentPlacement(wf *edgev1alpha1.WasmFunction) string {
	if wf.Status.DesiredPlacement != "" {
		retained := normalizeRetainedPlacement(wf.Status.DesiredPlacement)
		if retained == placementHybrid &&
			!hasDeviceLocalResources(wf.Spec.Release.ResourceContract) {
			return placementEdge
		}
		return retained
	}
	if strings.EqualFold(wf.Status.ObservedMode, placementEdge) {
		return applyResourceLocality(wf, placementEdge)
	}
	return placementLocal
}

func applyDestinationReadiness(wf *edgev1alpha1.WasmFunction, decision placementDecision, artifact artifactDecision, edgeRuntimeAvailable bool) placementDecision {
	deviceReady := deviceReleaseReady(wf, artifact.DesiredDigest)
	hostReady := hostReleaseMatches(artifact.HostDigest, artifact.HostActiveGeneration, artifact.HostActiveFunction, wf) ||
		hostReleaseMatches(artifact.HostStagedDigest, artifact.HostStagedGeneration, artifact.HostStagedFunction, wf)

	if !decision.Transition {
		wf.Status.ArtifactReadinessReason = "CurrentPlacementActivation"
		decision.ReadinessReason = wf.Status.ArtifactReadinessReason
		return decision
	}

	ready := deviceReady
	reason := "DeviceStagePending"
	if decision.Candidate == placementEdge || decision.Candidate == placementHybrid {
		ready = deviceReady && hostReady && edgeRuntimeAvailable
		switch {
		case !edgeRuntimeAvailable:
			reason = "RuntimeUnavailable"
		case !hostReady:
			reason = "HostStagePending"
		case !deviceReady:
			reason = "DeviceStagePending"
		}
	}
	if ready {
		wf.Status.ArtifactReadinessReason = "DestinationReady"
		decision.ReadinessReason = wf.Status.ArtifactReadinessReason
		return decision
	}

	wf.Status.ArtifactReadinessReason = reason
	decision.ReadinessReason = reason
	decision.Desired = decision.Current
	decision.RuntimeMode = runtimeModeForPlacement(decision.Current)
	decision.Deadline.Accepted = false
	if decision.Hard {
		decision.Reason = "ForcedRemotePendingArtifact: " + reason
	} else {
		decision.Reason = "candidate readiness pending: " + reason
	}
	return decision
}

func effectiveBatteryThresholdEnabled(wf *edgev1alpha1.WasmFunction) bool {
	if wf.Spec.Placement.BatteryThreshold.Enabled == nil {
		return true
	}
	return *wf.Spec.Placement.BatteryThreshold.Enabled
}

func effectiveDeadlineEnabled(wf *edgev1alpha1.WasmFunction) bool {
	if wf.Spec.Placement.Deadline.Enabled == nil {
		return true
	}
	return *wf.Spec.Placement.Deadline.Enabled
}

func effectiveBatteryDeltaEnabled(wf *edgev1alpha1.WasmFunction) bool {
	if wf.Spec.Placement.BatteryDelta.Enabled == nil {
		return true
	}
	return *wf.Spec.Placement.BatteryDelta.Enabled
}

func batteryPercentForPlacement(wf *edgev1alpha1.WasmFunction) *int32 {
	if wf.Status.ObservedBatteryPercent != nil {
		return wf.Status.ObservedBatteryPercent
	}
	return wf.Spec.Placement.BatteryPercent
}

func localBatteryDeltaForcesEdge(wf *edgev1alpha1.WasmFunction, high int32) bool {
	if !effectiveBatteryDeltaEnabled(wf) || wf.Status.ObservedMode != placementLocal ||
		wf.Status.ObservedBatteryDeltaPercent == nil {
		return false
	}
	if wf.Status.ObservedBatteryPercent != nil && *wf.Status.ObservedBatteryPercent >= high {
		return false
	}
	return *wf.Status.ObservedBatteryDeltaPercent >= effectiveBatteryDeltaMaxDrainPercent(wf) &&
		wf.Status.ConsecutiveRiskyWindows >= effectiveBatteryDeltaRiskyWindows(wf)
}

func effectiveBatteryDeltaWindowSeconds(wf *edgev1alpha1.WasmFunction) int32 {
	if wf.Spec.Placement.BatteryDelta.WindowSeconds > 0 {
		return wf.Spec.Placement.BatteryDelta.WindowSeconds
	}
	return defaultBatteryDeltaWindowSeconds
}

func effectiveBatteryDeltaMaxDrainPercent(wf *edgev1alpha1.WasmFunction) int32 {
	if wf.Spec.Placement.BatteryDelta.MaxDrainPercent > 0 {
		return wf.Spec.Placement.BatteryDelta.MaxDrainPercent
	}
	return defaultBatteryDeltaMaxDrainPercent
}

func effectiveBatteryDeltaRiskyWindows(wf *edgev1alpha1.WasmFunction) int32 {
	if wf.Spec.Placement.BatteryDelta.RiskyWindowsToOffload > 0 {
		return wf.Spec.Placement.BatteryDelta.RiskyWindowsToOffload
	}
	return defaultBatteryDeltaRiskyWindows
}

func applyResourceLocality(wf *edgev1alpha1.WasmFunction, placement string) string {
	if placement == placementEdge && hasDeviceLocalResources(wf.Spec.Release.ResourceContract) {
		return placementHybrid
	}
	return placement
}

func hasDeviceLocalResources(contract edgev1alpha1.ResourceContractSpec) bool {
	for _, input := range contract.Inputs {
		if strings.EqualFold(strings.TrimSpace(input.Locality), "device") {
			return true
		}
	}
	for _, output := range contract.Outputs {
		if strings.EqualFold(strings.TrimSpace(output.Locality), "device") {
			return true
		}
	}
	return false
}

func runtimeModeForPlacement(placement string) string {
	if placement == placementHybrid {
		return placementEdge
	}
	return placement
}

func normalizeRetainedPlacement(placement string) string {
	switch placement {
	case placementLocal, placementEdge, placementHybrid:
		return placement
	default:
		return placementLocal
	}
}

func (r *WasmFunctionReconciler) applyPlacement(ctx context.Context, wf *edgev1alpha1.WasmFunction, decision placementDecision, artifact artifactDecision) (bool, error) {
	topic := wf.Spec.Device.ControlTopic
	if topic == "" {
		return false, nil
	}

	if _, err := r.applyReleaseStage(ctx, wf, topic, artifact); err != nil {
		return false, err
	}

	if _, err := r.applyBatterySimulationUpdate(ctx, wf, topic); err != nil {
		return false, err
	}

	thresholdsCommanded, err := r.applyThresholdUpdate(ctx, wf, topic, decision)
	if err != nil {
		return false, err
	}
	_ = thresholdsCommanded

	if err := r.applyDeadlineRejectionSignal(ctx, wf, topic, decision); err != nil {
		return false, err
	}
	if decision.Hard && decision.Transition && !decision.Deadline.Accepted {
		if err := r.applyPauseCommand(ctx, wf, topic); err != nil {
			return false, err
		}
	}

	placementCommanded, err := r.applyPlacementCommand(ctx, wf, topic, decision, artifact)
	if err != nil {
		return placementCommanded, err
	}

	if placementCommanded {
		now := metav1.NewTime(r.now())
		wf.Status.LastCommandTime = &now
	}
	if err := r.applyResumeCommand(ctx, wf, topic, decision, artifact); err != nil {
		return placementCommanded, err
	}

	return placementCommanded, nil
}

func effectiveSimulationStep(value *int32) int32 {
	if value == nil {
		return 10
	}
	return *value
}

func batterySimulationSourceAcknowledged(wf *edgev1alpha1.WasmFunction, enabled bool) bool {
	source := strings.ToLower(strings.TrimSpace(wf.Status.ObservedBatterySource))
	if enabled {
		return source == "simulated"
	}
	return source == "real"
}

func (r *WasmFunctionReconciler) applyBatterySimulationUpdate(ctx context.Context, wf *edgev1alpha1.WasmFunction, topic string) (bool, error) {
	simulation := wf.Spec.Device.BatterySimulation
	if simulation.Enabled == nil {
		return false, nil
	}
	drain := effectiveSimulationStep(simulation.LocalDrainPercent)
	recover := effectiveSimulationStep(simulation.EdgeRecoverPercent)
	commandID := fmt.Sprintf("battery-simulation-%t-%d-%d", *simulation.Enabled, drain, recover)
	sameConfiguration := wf.Status.LastAppliedBatterySimulationCommandID == commandID
	acknowledged := batterySimulationSourceAcknowledged(wf, *simulation.Enabled)
	if sameConfiguration && acknowledged {
		return false, nil
	}
	if sameConfiguration && wf.Status.LastBatterySimulationCommandTime != nil {
		// Do not publish again merely because status reconciliation ran. Wait for
		// a telemetry observation newer than the last send; that observation is
		// either the acknowledgement or evidence that the device still disagrees.
		if wf.Status.LastTelemetryTime == nil ||
			!wf.Status.LastTelemetryTime.After(wf.Status.LastBatterySimulationCommandTime.Time) {
			return false, nil
		}
	}
	payload := map[string]interface{}{
		"action":    "set_simulation",
		"commandId": commandID,
		"enabled":   *simulation.Enabled,
		"drain":     drain,
		"recover":   recover,
	}
	if err := r.publishControl(ctx, topic, payload); err != nil {
		return false, err
	}
	retry := sameConfiguration
	wf.Status.LastAppliedBatterySimulationCommandID = commandID
	commandTime := metav1.NewTime(r.now())
	wf.Status.LastBatterySimulationCommandTime = &commandTime
	log := logf.FromContext(ctx)
	values := []interface{}{
		"topic", topic,
		"enabled", *simulation.Enabled,
		"localDrainPercent", drain,
		"edgeRecoverPercent", recover,
		"observedBatterySource", wf.Status.ObservedBatterySource,
		"commandId", commandID,
	}
	if retry {
		log.V(1).Info("Retried battery simulation update after telemetry mismatch", values...)
	} else {
		log.Info("Sent battery simulation update", values...)
	}
	return true, nil
}

func commandAwaitingNewTelemetry(wf *edgev1alpha1.WasmFunction, commandID string) bool {
	if wf.Status.LastCommandID != commandID {
		return false
	}
	if wf.Status.LastCommandTime == nil || wf.Status.LastTelemetryTime == nil {
		return true
	}
	return !wf.Status.LastTelemetryTime.After(wf.Status.LastCommandTime.Time)
}

func (r *WasmFunctionReconciler) applyPauseCommand(ctx context.Context, wf *edgev1alpha1.WasmFunction, topic string) error {
	if wf.Status.ObservedAdmissionPaused {
		return nil
	}
	commandID := fmt.Sprintf("pause-%d", wf.Spec.Release.Generation)
	if commandAwaitingNewTelemetry(wf, commandID) {
		return nil
	}
	payload := map[string]interface{}{
		"action": "pause_function", "commandId": commandID,
		"releaseGeneration": wf.Spec.Release.Generation,
	}
	if err := r.publishControl(ctx, topic, payload); err != nil {
		return err
	}
	now := metav1.Now()
	wf.Status.LastCommandID = commandID
	wf.Status.LastCommandTime = &now
	logf.FromContext(ctx).Info("Paused function admission for forced transition",
		"releaseGeneration", wf.Spec.Release.Generation,
		"candidatePlacement", placementEdge)
	return nil
}

func (r *WasmFunctionReconciler) applyResumeCommand(ctx context.Context, wf *edgev1alpha1.WasmFunction, topic string, decision placementDecision, artifact artifactDecision) error {
	if !wf.Status.ObservedAdmissionPaused ||
		wf.Status.ObservedMode != runtimeModeForPlacement(decision.Desired) ||
		wf.Status.AcknowledgedReleaseGeneration != wf.Spec.Release.Generation ||
		normalizeArtifactDigest(wf.Status.ObservedArtifactDigest) != artifact.DesiredDigest {
		return nil
	}
	commandID := fmt.Sprintf("resume-%d", wf.Spec.Release.Generation)
	if commandAwaitingNewTelemetry(wf, commandID) {
		return nil
	}
	payload := map[string]interface{}{
		"action": "resume_function", "commandId": commandID,
		"releaseGeneration": wf.Spec.Release.Generation,
	}
	if err := r.publishControl(ctx, topic, payload); err != nil {
		return err
	}
	now := metav1.Now()
	wf.Status.LastCommandID = commandID
	wf.Status.LastCommandTime = &now
	logf.FromContext(ctx).Info("Resumed function admission after destination acknowledgement",
		"releaseGeneration", wf.Spec.Release.Generation,
		"placement", decision.Desired)
	return nil
}

func (r *WasmFunctionReconciler) applyThresholdUpdate(ctx context.Context, wf *edgev1alpha1.WasmFunction, topic string, decision placementDecision) (bool, error) {
	if !decision.BatteryThresholdEnabled {
		return false, nil
	}

	thresholdsChanged := wf.Status.LastAppliedLowBatteryThreshold != decision.LowThreshold ||
		wf.Status.LastAppliedHighBatteryThreshold != decision.HighThreshold
	if !thresholdsChanged {
		return false, nil
	}

	payload := map[string]interface{}{
		"action":    "set_thresholds",
		"commandId": fmt.Sprintf("thresholds-%d-%d", decision.LowThreshold, decision.HighThreshold),
		"low":       decision.LowThreshold,
		"high":      decision.HighThreshold,
	}
	if err := r.publishControl(ctx, topic, payload); err != nil {
		return false, err
	}
	logf.FromContext(ctx).Info("Sent threshold update", "topic", topic, "low", decision.LowThreshold, "high", decision.HighThreshold)
	wf.Status.LastAppliedLowBatteryThreshold = decision.LowThreshold
	wf.Status.LastAppliedHighBatteryThreshold = decision.HighThreshold
	return true, nil
}

func (r *WasmFunctionReconciler) applyReleaseStage(ctx context.Context, wf *edgev1alpha1.WasmFunction, topic string, artifact artifactDecision) (bool, error) {
	if !artifact.Enabled || artifact.DesiredDigest == "" || wf.Spec.Release.Generation <= 0 {
		return false, nil
	}
	artifactURL := deviceArtifactURL(wf)
	if artifactURL == "" {
		return false, fmt.Errorf("device.artifactURL is required to stage release %d", wf.Spec.Release.Generation)
	}
	if deviceReleaseReady(wf, artifact.DesiredDigest) {
		wf.Status.StageCommandAttempts = 0
		r.releaseCommandMu.Lock()
		delete(r.releaseStageSends, types.NamespacedName{Namespace: wf.Namespace, Name: wf.Name})
		r.releaseCommandMu.Unlock()
		return false, nil
	}
	commandID := fmt.Sprintf("stage-%d-%s", wf.Spec.Release.Generation, artifactDigestForLog(artifact.DesiredDigest))
	now := r.now()
	attempt, claimed := r.claimReleaseStageAttempt(wf, commandID, now)
	if !claimed {
		return false, nil
	}
	payload := map[string]interface{}{
		"action":            "stage_release",
		"commandId":         commandID,
		"releaseGeneration": wf.Spec.Release.Generation,
		"artifactURL":       artifactURL,
		"artifactDigest":    artifact.DesiredDigest,
		"functionIdentity":  wf.Spec.Release.FunctionIdentity,
		"resourceContract":  wf.Spec.Release.ResourceContract,
	}
	if err := r.publishControl(ctx, topic, payload); err != nil {
		return false, err
	}
	log := logf.FromContext(ctx)
	values := []interface{}{
		"topic", topic,
		"releaseGeneration", wf.Spec.Release.Generation,
		"functionIdentity", wf.Spec.Release.FunctionIdentity,
		"artifactDigest", artifactDigestForLog(artifact.DesiredDigest),
		"commandId", commandID,
		"attempt", attempt,
	}
	if attempt == 1 {
		log.Info("Release staging requested; awaiting device acknowledgement", values...)
	} else {
		log.V(1).Info("Retried release staging after acknowledgement timeout", values...)
	}
	return true, nil
}

func (r *WasmFunctionReconciler) claimReleaseStageAttempt(wf *edgev1alpha1.WasmFunction, commandID string, now time.Time) (int32, bool) {
	key := types.NamespacedName{Namespace: wf.Namespace, Name: wf.Name}
	r.releaseCommandMu.Lock()
	defer r.releaseCommandMu.Unlock()
	if r.releaseStageSends == nil {
		r.releaseStageSends = make(map[types.NamespacedName]releaseStageSendState)
	}

	lastSent := time.Time{}
	attempts := int32(0)
	if wf.Status.LastStageCommandID == commandID && wf.Status.LastStageCommandTime != nil {
		lastSent = wf.Status.LastStageCommandTime.Time
		attempts = wf.Status.StageCommandAttempts
	}
	if remembered, ok := r.releaseStageSends[key]; ok &&
		remembered.commandID == commandID && remembered.sentAt.After(lastSent) {
		lastSent = remembered.sentAt
		attempts = remembered.attempts
	}
	if !lastSent.IsZero() {
		if attempts < 1 {
			attempts = 1
		}
		if now.Before(lastSent.Add(releaseStageRetryDelay(attempts))) {
			return 0, false
		}
	}

	attempt := int32(1)
	if !lastSent.IsZero() {
		attempt = attempts + 1
		if attempt < 2 {
			attempt = 2
		}
	}
	commandTime := metav1.NewTime(now)
	wf.Status.LastStageCommandID = commandID
	wf.Status.LastStageCommandTime = &commandTime
	wf.Status.StageCommandAttempts = attempt
	r.releaseStageSends[key] = releaseStageSendState{
		commandID: commandID, sentAt: now, attempts: attempt,
	}
	return attempt, true
}

func releaseStageRetryDelay(attempts int32) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := releaseStageRetryBase
	for n := int32(1); n < attempts && delay < releaseStageRetryMax; n++ {
		delay *= 2
		if delay > releaseStageRetryMax {
			delay = releaseStageRetryMax
		}
	}
	return delay
}

func releaseDeliveryState(wf *edgev1alpha1.WasmFunction, digest string) string {
	if digest == "" || wf.Spec.Release.Generation <= 0 {
		return "Pending"
	}
	if deviceReleaseActive(wf, digest) {
		return "Active"
	}
	if deviceReleaseStaged(wf, digest) {
		generation := wf.Spec.Release.Generation
		if wf.Status.LastCommandID == fmt.Sprintf("activate-local-%d", generation) ||
			wf.Status.LastCommandID == fmt.Sprintf("runtime-edge-%d", generation) {
			return "AwaitingActivationAck"
		}
		return "Staged"
	}
	expectedCommand := fmt.Sprintf("stage-%d-%s", wf.Spec.Release.Generation, artifactDigestForLog(digest))
	if wf.Status.LastStageCommandID == expectedCommand {
		return "AwaitingStageAck"
	}
	return "Pending"
}

func (r *WasmFunctionReconciler) applyDeadlineRejectionSignal(ctx context.Context, wf *edgev1alpha1.WasmFunction, topic string, decision placementDecision) error {
	if !decision.Transition || decision.Deadline.Condition == nil ||
		decision.Deadline.Condition.Reason != "CandidateDeadlineUnsafe" ||
		wf.Status.ArtifactReadinessReason != "DestinationReady" {
		return nil
	}
	// The decision ID is an idempotency key, not just a user-facing label. A
	// fresh telemetry observation represents a fresh rejected invocation and
	// must blink once; repeated reconciles and QoS-1 redelivery of that same
	// observation must not blink again. The spec generation also preserves the
	// existing behavior where editing the deadline policy re-signals a rejection.
	observation := int64(0)
	if wf.Status.LastTelemetryTime != nil {
		observation = wf.Status.LastTelemetryTime.UnixNano()
	}
	decisionID := fmt.Sprintf("deadline-%d-%d-%s-%s-%d",
		wf.Spec.Release.Generation, wf.Generation, decision.Current,
		decision.Candidate, observation)
	wf.Status.LastDeadlineDecisionID = decisionID
	if wf.Status.LastSignaledDeadlineDecisionID == decisionID {
		return nil
	}
	payload := map[string]interface{}{
		"action":           "signal_deadline_rejection",
		"decisionId":       decisionID,
		"functionIdentity": wf.Spec.Release.FunctionIdentity,
	}
	if err := r.publishControl(ctx, topic, payload); err != nil {
		return err
	}
	wf.Status.LastSignaledDeadlineDecisionID = decisionID
	targetMs := int32(0)
	if wf.Spec.Placement.Deadline.TargetMs != nil {
		targetMs = *wf.Spec.Placement.Deadline.TargetMs
	}
	logf.FromContext(ctx).Info("Signalled deadline rejection",
		"decisionId", decisionID,
		"specGeneration", wf.Generation,
		"targetMs", targetMs,
		"currentPlacement", decision.Current,
		"candidatePlacement", decision.Candidate,
		"candidateCostMs", decision.Deadline.CostMs,
		"candidateSlackMs", decision.Deadline.SlackMs)
	return nil
}

func (r *WasmFunctionReconciler) applyPlacementCommand(ctx context.Context, wf *edgev1alpha1.WasmFunction, topic string, decision placementDecision, artifact artifactDecision) (bool, error) {
	trackPlacement := !decision.Transition || decision.Deadline.Accepted
	if !artifact.Enabled || artifact.DesiredDigest == "" || wf.Spec.Release.Generation <= 0 {
		return false, nil
	}
	deviceReady := deviceReleaseReady(wf, artifact.DesiredDigest)
	if !deviceReady {
		return false, nil
	}
	if decision.Desired == placementLocal {
		commandID := fmt.Sprintf("activate-local-%d", wf.Spec.Release.Generation)
		if r.shouldSkipPlacementCommand(wf, commandID, decision.Desired, placementLocal, wf.Spec.Release.Generation, artifact.DesiredDigest, r.now()) {
			return false, nil
		}
		return r.sendActivateLocalCommand(ctx, wf, topic, decision, artifact.DesiredDigest, trackPlacement)
	}
	if !hostReleaseMatches(artifact.HostDigest, artifact.HostActiveGeneration, artifact.HostActiveFunction, wf) {
		if !hostReleaseMatches(artifact.HostStagedDigest, artifact.HostStagedGeneration, artifact.HostStagedFunction, wf) {
			return false, nil
		}
		if err := r.activateHostRelease(ctx, wf); err != nil {
			return false, err
		}
		artifact.HostDigest = artifact.DesiredDigest
		artifact.HostActiveGeneration = wf.Spec.Release.Generation
		artifact.HostActiveFunction = wf.Spec.Release.FunctionIdentity
		artifact.HostStagedDigest = ""
		artifact.HostStagedGeneration = 0
		artifact.HostStagedFunction = ""
		wf.Status.HostArtifactDigest = artifact.DesiredDigest
		wf.Status.HostStagedArtifactDigest = ""
		logf.FromContext(ctx).Info("Activated edge host release",
			"releaseGeneration", wf.Spec.Release.Generation,
			"functionIdentity", wf.Spec.Release.FunctionIdentity,
			"artifactDigest", artifactDigestForLog(artifact.DesiredDigest))
	}
	if !hostReleaseMatches(artifact.HostDigest, artifact.HostActiveGeneration, artifact.HostActiveFunction, wf) {
		return false, nil
	}
	commandID := fmt.Sprintf("runtime-edge-%d", wf.Spec.Release.Generation)
	if r.shouldSkipPlacementCommand(wf, commandID, decision.Desired, placementEdge, wf.Spec.Release.Generation, artifact.DesiredDigest, r.now()) {
		return false, nil
	}
	return r.sendRuntimeModeCommand(ctx, wf, topic, decision, artifact.DesiredDigest, trackPlacement)
}

func (r *WasmFunctionReconciler) shouldSkipPlacementCommand(wf *edgev1alpha1.WasmFunction, commandID string, desired string, runtimeMode string, generation int64, digest string, now time.Time) bool {
	if desired == "" || runtimeMode == "" {
		return true
	}
	key := types.NamespacedName{Namespace: wf.Namespace, Name: wf.Name}
	if wf.Status.ObservedMode == runtimeMode &&
		wf.Status.AcknowledgedReleaseGeneration == generation &&
		normalizeArtifactDigest(wf.Status.ObservedArtifactDigest) == digest {
		r.releaseCommandMu.Lock()
		delete(r.releaseActivationSends, key)
		r.releaseCommandMu.Unlock()
		return true
	}
	if wf.Status.LastCommandID != commandID {
		r.releaseCommandMu.Lock()
		remembered, ok := r.releaseActivationSends[key]
		r.releaseCommandMu.Unlock()
		return ok && remembered.commandID == commandID &&
			now.Before(remembered.sentAt.Add(releaseStageRetryBase))
	}
	lastRuntimeMode := wf.Status.LastCommandedRuntimeMode
	if lastRuntimeMode == "" {
		lastRuntimeMode = runtimeModeForPlacement(wf.Status.LastCommandedPlacement)
	}
	if wf.Status.LastCommandedPlacement != desired || lastRuntimeMode != runtimeMode {
		return false
	}
	lastSent := time.Time{}
	if wf.Status.LastCommandTime != nil {
		lastSent = wf.Status.LastCommandTime.Time
	}
	r.releaseCommandMu.Lock()
	remembered, ok := r.releaseActivationSends[key]
	r.releaseCommandMu.Unlock()
	if ok && remembered.commandID == commandID && remembered.sentAt.After(lastSent) {
		lastSent = remembered.sentAt
	}
	return !lastSent.IsZero() && now.Before(lastSent.Add(releaseStageRetryBase))
}

func (r *WasmFunctionReconciler) rememberReleaseActivationSend(wf *edgev1alpha1.WasmFunction, commandID string, now time.Time) {
	key := types.NamespacedName{Namespace: wf.Namespace, Name: wf.Name}
	r.releaseCommandMu.Lock()
	defer r.releaseCommandMu.Unlock()
	if r.releaseActivationSends == nil {
		r.releaseActivationSends = make(map[types.NamespacedName]releaseActivationSendState)
	}
	r.releaseActivationSends[key] = releaseActivationSendState{commandID: commandID, sentAt: now}
}

func (r *WasmFunctionReconciler) sendActivateLocalCommand(ctx context.Context, wf *edgev1alpha1.WasmFunction, topic string, decision placementDecision, artifactDigest string, trackPlacement bool) (bool, error) {
	commandID := fmt.Sprintf("activate-local-%d", wf.Spec.Release.Generation)
	retryCommand := wf.Status.LastCommandID == commandID
	payload := map[string]interface{}{
		"action":            "activate_local",
		"commandId":         commandID,
		"releaseGeneration": wf.Spec.Release.Generation,
	}
	r.rememberReleaseActivationSend(wf, commandID, r.now())
	if err := r.publishControl(ctx, topic, payload); err != nil {
		return false, err
	}
	log := logf.FromContext(ctx)
	values := []interface{}{"topic", topic,
		"releaseGeneration", wf.Spec.Release.Generation,
		"artifactDigest", artifactDigestForLog(artifactDigest),
		"reason", decision.Reason}
	if retryCommand {
		log.V(1).Info("Retried local release activation after acknowledgement timeout", values...)
	} else {
		log.Info("Sent local release activation command", values...)
	}
	if trackPlacement {
		wf.Status.LastCommandedPlacement = placementLocal
		wf.Status.LastCommandedRuntimeMode = placementLocal
	}
	wf.Status.LastCommandID = commandID
	return true, nil
}

func (r *WasmFunctionReconciler) sendRuntimeModeCommand(ctx context.Context, wf *edgev1alpha1.WasmFunction, topic string, decision placementDecision, artifactDigest string, trackPlacement bool) (bool, error) {
	commandID := fmt.Sprintf("runtime-edge-%d", wf.Spec.Release.Generation)
	payload := map[string]interface{}{
		"action":            "set_runtime_mode",
		"commandId":         commandID,
		"value":             placementEdge,
		"releaseGeneration": wf.Spec.Release.Generation,
	}
	r.rememberReleaseActivationSend(wf, commandID, r.now())
	if err := r.publishControl(ctx, topic, payload); err != nil {
		return false, err
	}
	logf.FromContext(ctx).Info("Sent placement command", "topic", topic,
		"placement", decision.Desired, "runtimeMode", placementEdge,
		"releaseGeneration", wf.Spec.Release.Generation,
		"artifactDigest", artifactDigestForLog(artifactDigest),
		"reason", decision.Reason)
	if trackPlacement {
		wf.Status.LastCommandedPlacement = decision.Desired
		wf.Status.LastCommandedRuntimeMode = placementEdge
	}
	wf.Status.LastCommandID = commandID
	return true, nil
}

func placementStatusCondition(decision placementDecision, commanded bool, err error) metav1.Condition {
	condition := metav1.Condition{
		Type:               "PlacementCommanded",
		LastTransitionTime: metav1.Now(),
	}
	if !decision.Enabled {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "ControlTopicMissing"
		condition.Message = decision.Reason
		return condition
	}
	if err != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "PublishFailed"
		condition.Message = err.Error()
		return condition
	}
	if decision.Desired == "" {
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "WaitingForTelemetry"
		condition.Message = decision.Reason
		return condition
	}
	if decision.Transition && decision.ReadinessReason != "DestinationReady" {
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "ArtifactSyncPending"
		condition.Message = decision.Reason
		return condition
	}
	if decision.Transition && !decision.Deadline.Accepted {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "TransitionRejected"
		condition.Message = fmt.Sprintf("retained %s placement: %s", decision.Current, decision.Reason)
		return condition
	}
	condition.Status = metav1.ConditionTrue
	if commanded {
		condition.Reason = "CommandSent"
		condition.Message = fmt.Sprintf("commanded %s placement: %s", decision.Desired, decision.Reason)
	} else {
		condition.Reason = "AlreadyCommanded"
		condition.Message = fmt.Sprintf("%s placement already commanded: %s", decision.Desired, decision.Reason)
	}
	return condition
}

func effectivePort(wf *edgev1alpha1.WasmFunction) int32 {
	if wf.Spec.Port != 0 {
		return wf.Spec.Port
	}
	return 8080
}

func effectiveImage(wf *edgev1alpha1.WasmFunction) string {
	if strings.TrimSpace(wf.Spec.Image) != "" {
		return wf.Spec.Image
	}
	return defaultEdgeHostImage
}

func (r *WasmFunctionReconciler) desiredDeployment(wf *edgev1alpha1.WasmFunction) *appsv1.Deployment {
	labels := map[string]string{
		"app.kubernetes.io/name":       "sif-edge-host",
		"app.kubernetes.io/instance":   wf.Name,
		"app.kubernetes.io/managed-by": "sif-operator",
	}

	replicas := int32(1)
	if wf.Spec.Replicas != nil {
		replicas = *wf.Spec.Replicas
	}

	port := effectivePort(wf)

	wasmPath := "/app/dht_reader.wasm"
	if wf.Spec.WasmPath != "" {
		wasmPath = wf.Spec.WasmPath
	}

	env := []corev1.EnvVar{
		{Name: "WASM_PATH", Value: wasmPath},
		{Name: "PORT", Value: fmt.Sprintf("%d", port)},
	}
	for _, e := range wf.Spec.Env {
		env = append(env, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      wf.Name,
			Namespace: wf.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "wasm-host",
						Image: effectiveImage(wf),
						Ports: []corev1.ContainerPort{{
							ContainerPort: port,
							Protocol:      corev1.ProtocolTCP,
						}},
						Env: env,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("64Mi"),
								corev1.ResourceCPU:    resource.MustParse("50m"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("128Mi"),
								corev1.ResourceCPU:    resource.MustParse("200m"),
							},
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/health",
									Port: intstr.FromInt32(port),
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/health",
									Port: intstr.FromInt32(port),
								},
							},
							InitialDelaySeconds: 3,
							PeriodSeconds:       5,
						},
					}},
				},
			},
		},
	}
}

func (r *WasmFunctionReconciler) desiredService(wf *edgev1alpha1.WasmFunction) *corev1.Service {
	port := effectivePort(wf)

	labels := map[string]string{
		"app.kubernetes.io/name":       "sif-edge-host",
		"app.kubernetes.io/instance":   wf.Name,
		"app.kubernetes.io/managed-by": "sif-operator",
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      wf.Name,
			Namespace: wf.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Port:       port,
				TargetPort: intstr.FromInt32(port),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *WasmFunctionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&edgev1alpha1.WasmFunction{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Named("wasmfunction").
		Complete(r)
}
