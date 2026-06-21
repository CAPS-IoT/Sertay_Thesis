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
	Scheme              *runtime.Scheme
	HTTPClient          *http.Client
	HostWasmURLResolver func(*edgev1alpha1.WasmFunction) string
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

	// 4. Resolve placement first so host artifact sync can follow the same
	// placement policy as the device side.
	decision := resolvePlacement(&wf)
	artifact, artifactErr := r.reconcileArtifact(ctx, &wf, shouldSyncHostArtifact(decision))
	wf.Status.DesiredArtifactDigest = artifact.DesiredDigest
	wf.Status.HostArtifactDigest = artifact.HostDigest
	if artifactErr != nil {
		log.Error(artifactErr, "Failed to reconcile artifact", "sourceURL", artifact.SourceURL, "hostURL", artifact.HostURL)
	}

	placementErr := error(nil)
	placementCommanded := false
	if decision.Enabled && artifactErr == nil {
		placementCommanded, placementErr = r.applyPlacement(ctx, &wf, decision, artifact)
		if placementErr != nil {
			log.Error(placementErr, "Failed to publish placement command", "topic", wf.Spec.Device.ControlTopic)
		}
	}

	// 5. Update status.
	availableReplicas := r.availableReplicasForDeployment(ctx, deploy)
	endpoint := fmt.Sprintf("http://%s.%s:%d/process", wf.Name, wf.Namespace, effectivePort(&wf))

	availableCondition := availableStatusCondition()
	artifactCondition := artifactStatusCondition(artifact, artifactErr)
	placementCondition := placementStatusCondition(decision, placementCommanded, placementErr)
	if artifactErr != nil {
		placementCondition = blockedPlacementStatusCondition(decision, artifactErr)
	}

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
	if result, shouldRequeue := requeueOnError(artifactErr, placementErr); shouldRequeue {
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
		current.Status.DesiredPlacement = update.decision.Desired
		current.Status.PlacementReason = update.decision.Reason
		current.Status.LastAppliedLowBatteryThreshold = wf.Status.LastAppliedLowBatteryThreshold
		current.Status.LastAppliedHighBatteryThreshold = wf.Status.LastAppliedHighBatteryThreshold
		current.Status.LastCommandedPlacement = wf.Status.LastCommandedPlacement
		current.Status.LastCommandedArtifactDigest = wf.Status.LastCommandedArtifactDigest
		current.Status.LastCommandTime = wf.Status.LastCommandTime
		current.Status.ObservedBatteryPercent = copyBatteryPercent(update.decision.BatteryPercent)
		meta.SetStatusCondition(&current.Status.Conditions, update.available)
		meta.SetStatusCondition(&current.Status.Conditions, update.artifact)
		meta.SetStatusCondition(&current.Status.Conditions, update.placement)
		return r.Status().Update(ctx, &current)
	})
}

func copyBatteryPercent(value *int32) *int32 {
	if value == nil {
		return nil
	}
	observedBattery := *value
	return &observedBattery
}

func requeueOnError(artifactErr error, placementErr error) (ctrl.Result, bool) {
	if artifactErr != nil || placementErr != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, true
	}
	return ctrl.Result{}, false
}

type placementDecision struct {
	Enabled        bool
	Desired        string
	Reason         string
	BatteryPercent *int32
	LowThreshold   int32
	HighThreshold  int32
}

const (
	placementAuto  = "auto"
	placementLocal = "local"
	placementEdge  = "edge"

	defaultLowBatteryThreshold  int32 = 20
	defaultHighBatteryThreshold int32 = 60
)

func resolvePlacement(wf *edgev1alpha1.WasmFunction) placementDecision {
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
		Enabled:        wf.Spec.Device.ControlTopic != "",
		BatteryPercent: batteryPercentForPlacement(wf),
		LowThreshold:   low,
		HighThreshold:  high,
	}
	if !decision.Enabled {
		decision.Reason = "device control topic not configured"
		return decision
	}

	mode := strings.ToLower(wf.Spec.Placement.Mode)
	if mode == "" {
		mode = placementAuto
	}

	// Low battery is a hard guardrail: it overrides explicit local requests.
	if decision.BatteryPercent != nil && *decision.BatteryPercent <= low {
		decision.Desired = placementEdge
		decision.Reason = fmt.Sprintf("battery %d%% <= low threshold %d%%", *decision.BatteryPercent, low)
		return decision
	}

	switch mode {
	case placementEdge:
		decision.Desired = placementEdge
		decision.Reason = "operator requested edge placement"
	case placementLocal:
		decision.Desired = placementLocal
		decision.Reason = "operator requested local placement"
	case placementAuto:
		if decision.BatteryPercent == nil {
			decision.Reason = "auto placement waiting for battery telemetry"
			return decision
		}
		if *decision.BatteryPercent >= high {
			decision.Desired = placementLocal
			decision.Reason = fmt.Sprintf("battery %d%% >= high threshold %d%%", *decision.BatteryPercent, high)
			return decision
		}
		if wf.Status.DesiredPlacement != "" {
			decision.Desired = wf.Status.DesiredPlacement
			decision.Reason = fmt.Sprintf("battery %d%% inside hysteresis band [%d%%,%d%%], retaining %s", *decision.BatteryPercent, low, high, wf.Status.DesiredPlacement)
			return decision
		}
		decision.Desired = placementLocal
		decision.Reason = fmt.Sprintf("battery %d%% inside hysteresis band [%d%%,%d%%], defaulting local", *decision.BatteryPercent, low, high)
	default:
		decision.Reason = fmt.Sprintf("unsupported placement mode %q", wf.Spec.Placement.Mode)
	}

	return decision
}

func batteryPercentForPlacement(wf *edgev1alpha1.WasmFunction) *int32 {
	if wf.Status.ObservedBatteryPercent != nil {
		return wf.Status.ObservedBatteryPercent
	}
	return wf.Spec.Placement.BatteryPercent
}

func (r *WasmFunctionReconciler) applyPlacement(ctx context.Context, wf *edgev1alpha1.WasmFunction, decision placementDecision, artifact artifactDecision) (bool, error) {
	topic := wf.Spec.Device.ControlTopic
	if topic == "" {
		return false, nil
	}

	thresholdsCommanded, err := r.applyThresholdUpdate(ctx, wf, topic, decision)
	if err != nil {
		return thresholdsCommanded, err
	}

	placementCommanded, err := r.applyPlacementCommand(ctx, wf, topic, decision, artifact)
	if err != nil {
		return thresholdsCommanded || placementCommanded, err
	}

	if thresholdsCommanded || placementCommanded {
		now := metav1.Now()
		wf.Status.LastCommandTime = &now
	}

	return thresholdsCommanded || placementCommanded, nil
}

func (r *WasmFunctionReconciler) applyThresholdUpdate(ctx context.Context, wf *edgev1alpha1.WasmFunction, topic string, decision placementDecision) (bool, error) {
	thresholdsChanged := wf.Status.LastAppliedLowBatteryThreshold != decision.LowThreshold ||
		wf.Status.LastAppliedHighBatteryThreshold != decision.HighThreshold
	if !thresholdsChanged {
		return false, nil
	}

	payload := map[string]interface{}{
		"action": "set_thresholds",
		"low":    decision.LowThreshold,
		"high":   decision.HighThreshold,
	}
	if err := publishControlMessage(ctx, topic, payload); err != nil {
		return false, err
	}
	logf.FromContext(ctx).Info("Sent threshold update", "topic", topic, "low", decision.LowThreshold, "high", decision.HighThreshold)
	wf.Status.LastAppliedLowBatteryThreshold = decision.LowThreshold
	wf.Status.LastAppliedHighBatteryThreshold = decision.HighThreshold
	return true, nil
}

func (r *WasmFunctionReconciler) applyPlacementCommand(ctx context.Context, wf *edgev1alpha1.WasmFunction, topic string, decision placementDecision, artifact artifactDecision) (bool, error) {
	reloadURL := deviceReloadURL(wf)
	if shouldReloadLocalArtifact(wf, decision, artifact) {
		return r.sendReloadCommand(ctx, wf, topic, artifact.DesiredDigest, decision.Reason, localArtifactReloadReason(wf, artifact), "Sent artifact reload command")
	}
	if shouldSkipPlacementCommand(wf, decision.Desired) {
		return false, nil
	}
	if decision.Desired == placementLocal && reloadURL != "" {
		return r.sendReloadCommand(ctx, wf, topic, artifact.DesiredDigest, decision.Reason, "", "Sent binary migration command")
	}
	return r.sendSetModeCommand(ctx, wf, topic, decision, artifact.DesiredDigest)
}

func shouldSkipPlacementCommand(wf *edgev1alpha1.WasmFunction, desired string) bool {
	if desired == "" {
		return true
	}
	if wf.Status.LastCommandedPlacement != desired {
		return false
	}
	if wf.Status.ObservedMode == desired {
		return true
	}
	if wf.Status.LastCommandTime == nil || wf.Status.LastTelemetryTime == nil {
		return false
	}
	return !wf.Status.LastTelemetryTime.After(wf.Status.LastCommandTime.Time)
}

func (r *WasmFunctionReconciler) sendReloadCommand(ctx context.Context, wf *edgev1alpha1.WasmFunction, topic string, artifactDigest string, placementReason string, reloadReason string, message string) (bool, error) {
	reloadURL := deviceReloadURL(wf)
	payload := map[string]string{
		"action": "reload",
		"url":    reloadURL,
	}
	if err := publishControlMessage(ctx, topic, payload); err != nil {
		return false, err
	}
	keysAndValues := []interface{}{"topic", topic, "url", reloadURL, "digest", artifactDigest}
	if placementReason != "" {
		keysAndValues = append(keysAndValues, "placementReason", placementReason)
	}
	if reloadReason != "" {
		keysAndValues = append(keysAndValues, "reloadReason", reloadReason)
	}
	logf.FromContext(ctx).Info(message, keysAndValues...)
	wf.Status.LastCommandedPlacement = placementLocal
	if artifactDigest != "" {
		wf.Status.LastCommandedArtifactDigest = artifactDigest
	}
	return true, nil
}

func (r *WasmFunctionReconciler) sendSetModeCommand(ctx context.Context, wf *edgev1alpha1.WasmFunction, topic string, decision placementDecision, artifactDigest string) (bool, error) {
	payload := map[string]string{
		"action": "set_mode",
		"value":  decision.Desired,
	}
	if err := publishControlMessage(ctx, topic, payload); err != nil {
		return false, err
	}
	logf.FromContext(ctx).Info("Sent placement command", "topic", topic, "placement", decision.Desired, "reason", decision.Reason)
	wf.Status.LastCommandedPlacement = decision.Desired
	if artifactDigest != "" {
		wf.Status.LastCommandedArtifactDigest = artifactDigest
	}
	return true, nil
}

func blockedPlacementStatusCondition(decision placementDecision, artifactErr error) metav1.Condition {
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
	condition.Status = metav1.ConditionFalse
	condition.Reason = "ArtifactSyncPending"
	condition.Message = fmt.Sprintf("placement deferred until artifact synchronization succeeds: %v", artifactErr)
	return condition
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
						Image: wf.Spec.Image,
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
