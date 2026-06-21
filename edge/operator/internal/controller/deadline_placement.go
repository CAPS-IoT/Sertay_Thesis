package controller

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	edgev1alpha1 "github.com/sertayy/sif-operator/api/v1alpha1"
)

const (
	defaultDeadlineSafetyMarginMs         int32 = 250
	defaultDeadlineEstimatorWindowSeconds int32 = 300
	defaultDeadlineEstimatorMinSamples    int32 = 5
	defaultDeadlineEstimatorPercentile    int32 = 95
)

type deadlineEvaluation struct {
	Enabled          bool
	Accepted         bool
	BlocksTransition bool
	Unsafe           bool
	Forced           bool
	Candidate        string
	CostMs           *int32
	SlackMs          *int32
	Condition        *metav1.Condition
}

func evaluateDeadlineAdmission(
	wf *edgev1alpha1.WasmFunction,
	candidate string,
	hard bool,
	estimates deadlineEstimateSnapshot,
) deadlineEvaluation {
	evaluation := deadlineEvaluation{
		Enabled:   effectiveDeadlineEnabled(wf),
		Accepted:  true,
		Candidate: candidate,
	}
	if !evaluation.Enabled {
		return evaluation
	}

	identity := strings.TrimSpace(wf.Spec.Release.FunctionIdentity)
	if identity == "" || !estimates.IdentityKnown {
		evaluation.Accepted = hard
		evaluation.BlocksTransition = !hard
		evaluation.Condition = deadlineAdmissionCondition(
			metav1.ConditionUnknown,
			"FunctionIdentityUnknown",
			fmt.Sprintf("function identity %q has no configured timing profile", identity),
		)
		return evaluation
	}

	observedIdentity := strings.TrimSpace(wf.Status.ObservedFunction)
	if observedIdentity != "" && observedIdentity != identity {
		evaluation.Accepted = hard
		evaluation.BlocksTransition = !hard
		evaluation.Condition = deadlineAdmissionCondition(
			metav1.ConditionUnknown,
			"FunctionIdentityMismatch",
			fmt.Sprintf("selected function %q does not match reported function %q", identity, observedIdentity),
		)
		return evaluation
	}

	target := wf.Spec.Placement.Deadline.TargetMs
	if target == nil || *target <= 0 {
		evaluation.Condition = deadlineAdmissionCondition(
			metav1.ConditionUnknown,
			"TargetUnavailable",
			"deadline targetMs is not configured; deadline admission abstained",
		)
		return evaluation
	}
	if !estimates.Available {
		evaluation.Condition = deadlineAdmissionCondition(
			metav1.ConditionUnknown,
			"EstimatesUnavailable",
			fmt.Sprintf("timing estimates for function %q are unavailable; deadline admission abstained", identity),
		)
		return evaluation
	}

	cost, ok := deadlineCandidateCost(candidate, estimates, effectiveDeadlineSafetyMarginMs(wf))
	if !ok {
		evaluation.Accepted = false
		evaluation.BlocksTransition = true
		evaluation.Condition = deadlineAdmissionCondition(
			metav1.ConditionUnknown,
			"CandidateUnknown",
			fmt.Sprintf("cannot evaluate unsupported candidate placement %q", candidate),
		)
		return evaluation
	}
	slack := *target - cost
	evaluation.CostMs = int32Ptr(cost)
	evaluation.SlackMs = int32Ptr(slack)

	minimum := effectiveDeadlineMinSlackMs(wf)
	if slack >= minimum {
		evaluation.Condition = deadlineAdmissionCondition(
			metav1.ConditionTrue,
			"CandidateAccepted",
			fmt.Sprintf("%s candidate cost %dms leaves %dms slack (minimum %dms)", candidate, cost, slack, minimum),
		)
		return evaluation
	}

	evaluation.Unsafe = true
	if hard {
		evaluation.Forced = true
		evaluation.Condition = deadlineAdmissionCondition(
			metav1.ConditionFalse,
			"ForcedBatteryOverride",
			fmt.Sprintf("%s candidate cost %dms leaves %dms slack below minimum %dms; low-battery guardrail overrides deadline risk", candidate, cost, slack, minimum),
		)
		return evaluation
	}

	evaluation.Accepted = false
	evaluation.BlocksTransition = true
	evaluation.Condition = deadlineAdmissionCondition(
		metav1.ConditionFalse,
		"CandidateDeadlineUnsafe",
		fmt.Sprintf("%s candidate cost %dms leaves %dms slack below minimum %dms", candidate, cost, slack, minimum),
	)
	return evaluation
}

func deadlineCandidateCost(candidate string, estimates deadlineEstimateSnapshot, safetyMargin int32) (int32, bool) {
	switch candidate {
	case placementLocal:
		return estimates.LocalQueueDelayMs + estimates.ResourceWakeMs + estimates.LocalExecutionMs + safetyMargin, true
	case placementEdge:
		return estimates.NetworkRoundTripMs + estimates.EdgeExecutionMs + safetyMargin, true
	case placementHybrid:
		return estimates.ResourceCollectionMs + estimates.NetworkRoundTripMs + estimates.EdgeExecutionMs + estimates.OutputApplicationMs + safetyMargin, true
	default:
		return 0, false
	}
}

func deadlineAdmissionCondition(status metav1.ConditionStatus, reason string, message string) *metav1.Condition {
	return &metav1.Condition{
		Type:               "DeadlineAdmission",
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}
}

func effectiveDeadlineMinSlackMs(wf *edgev1alpha1.WasmFunction) int32 {
	if wf.Spec.Placement.Deadline.MinSlackMs > 0 {
		return wf.Spec.Placement.Deadline.MinSlackMs
	}
	return defaultDeadlineMinSlackMs
}

func effectiveDeadlineSafetyMarginMs(wf *edgev1alpha1.WasmFunction) int32 {
	if wf.Spec.Placement.Deadline.SafetyMarginMs > 0 {
		return wf.Spec.Placement.Deadline.SafetyMarginMs
	}
	return defaultDeadlineSafetyMarginMs
}

func effectiveDeadlineEstimatorWindowSeconds(wf *edgev1alpha1.WasmFunction) int32 {
	if wf.Spec.Placement.Deadline.Estimator.WindowSeconds > 0 {
		return wf.Spec.Placement.Deadline.Estimator.WindowSeconds
	}
	return defaultDeadlineEstimatorWindowSeconds
}

func effectiveDeadlineEstimatorMinSamples(wf *edgev1alpha1.WasmFunction) int32 {
	if wf.Spec.Placement.Deadline.Estimator.MinSamples > 0 {
		return wf.Spec.Placement.Deadline.Estimator.MinSamples
	}
	return defaultDeadlineEstimatorMinSamples
}

func effectiveDeadlineEstimatorPercentile(wf *edgev1alpha1.WasmFunction) int32 {
	if wf.Spec.Placement.Deadline.Estimator.Percentile > 0 {
		return wf.Spec.Placement.Deadline.Estimator.Percentile
	}
	return defaultDeadlineEstimatorPercentile
}

func int32Ptr(value int32) *int32 {
	copy := value
	return &copy
}

func copyInt32Ptr(value *int32) *int32 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
