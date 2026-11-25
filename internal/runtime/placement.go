package runtime

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/neuralmagic/vllm-vcr-operator/internal/plan"
)

// rewritePlacement replaces accelerator-specific placement with eligibility for
// simulation nodes.
//
// Simulation does not reproduce accelerator topology, so constraints that describe
// GPUs are removed and the environment's simulation-node selection is applied. Every
// other constraint is preserved: storage, zone, sidecar, and scheduling
// requirements still describe what the workload needs. A workload constraint that
// contradicts the environment's node selection cannot be satisfied on any
// simulation node and is reported instead of being dropped.
func rewritePlacement(pod *corev1.Pod, placement plan.Placement) error {
	if err := rewriteRuntimeClass(pod, placement); err != nil {
		return err
	}
	if err := rewriteNodeSelector(pod, placement); err != nil {
		return err
	}
	rewriteTolerations(pod, placement)
	rewriteAffinity(pod, placement)
	return nil
}

// rewriteRuntimeClass removes a container runtime class that installs an
// accelerator runtime.
func rewriteRuntimeClass(pod *corev1.Pod, placement plan.Placement) error {
	if pod.Spec.RuntimeClassName == nil {
		return nil
	}
	className := strings.ToLower(strings.TrimSpace(*pod.Spec.RuntimeClassName))
	if className == "" {
		return nil
	}
	for _, candidate := range placement.RuntimeClasses {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if candidate == "" {
			continue
		}
		if className == candidate || strings.HasPrefix(className, candidate) {
			pod.Spec.RuntimeClassName = nil
			return nil
		}
	}
	return nil
}

// rewriteNodeSelector removes accelerator label selection, applies the
// environment's node selection, and rejects a surviving workload constraint that
// contradicts it.
func rewriteNodeSelector(pod *corev1.Pod, placement plan.Placement) error {
	selector := pod.Spec.NodeSelector
	if len(selector) > 0 {
		for key, value := range selector {
			if isAcceleratorLabel(key, value, placement) {
				delete(selector, key)
			}
		}
	}

	for key, value := range placement.NodeSelector {
		if existing, present := selector[key]; present && existing != value {
			return fmt.Errorf(
				"runtime: the workload node selector requires %s=%q but the simulation nodes provide %s=%q: "+
					"the constraint cannot be satisfied on a simulation node",
				key, existing, key, value)
		}
		if selector == nil {
			selector = map[string]string{}
		}
		selector[key] = value
	}
	if len(selector) == 0 {
		pod.Spec.NodeSelector = nil
		return nil
	}
	pod.Spec.NodeSelector = selector
	return nil
}

// rewriteTolerations removes accelerator-only tolerations and adds the
// environment's tolerations.
func rewriteTolerations(pod *corev1.Pod, placement plan.Placement) {
	kept := make([]corev1.Toleration, 0, len(pod.Spec.Tolerations))
	for _, toleration := range pod.Spec.Tolerations {
		if isAcceleratorToleration(toleration, placement) {
			continue
		}
		kept = append(kept, toleration)
	}
	for _, toleration := range placement.Tolerations {
		if containsToleration(kept, toleration) {
			continue
		}
		kept = append(kept, toleration)
	}
	if len(kept) == 0 {
		pod.Spec.Tolerations = nil
		return
	}
	pod.Spec.Tolerations = kept
}

// rewriteAffinity removes accelerator expressions from node affinity. A required
// term that becomes unconstrained makes the whole required selector
// non-constraining: node selector terms are a disjunction, so one term that matches
// every node means the Pod may run on any node.
func rewriteAffinity(pod *corev1.Pod, placement plan.Placement) {
	affinity := pod.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil {
		return
	}
	nodeAffinity := affinity.NodeAffinity

	if required := nodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution; required != nil {
		terms := make([]corev1.NodeSelectorTerm, 0, len(required.NodeSelectorTerms))
		unconstrained := false
		for _, term := range required.NodeSelectorTerms {
			rewritten, droppedAll := rewriteNodeSelectorTerm(term, placement)
			if droppedAll {
				unconstrained = true
				break
			}
			terms = append(terms, rewritten)
		}
		if unconstrained || len(terms) == 0 {
			nodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = nil
		} else {
			required.NodeSelectorTerms = terms
		}
	}

	if preferred := nodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution; preferred != nil {
		kept := make([]corev1.PreferredSchedulingTerm, 0, len(preferred))
		for _, term := range preferred {
			rewritten, droppedAll := rewriteNodeSelectorTerm(term.Preference, placement)
			if droppedAll {
				continue
			}
			term.Preference = rewritten
			kept = append(kept, term)
		}
		nodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = kept
	}

	if nodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil &&
		len(nodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) == 0 {
		affinity.NodeAffinity = nil
	}
	if affinity.NodeAffinity == nil && affinity.PodAffinity == nil && affinity.PodAntiAffinity == nil {
		pod.Spec.Affinity = nil
	}
}

// rewriteNodeSelectorTerm removes accelerator expressions from one term. It reports
// whether the term constrained nothing but accelerator labels, in which case it
// matches every node and carries no placement intent.
func rewriteNodeSelectorTerm(term corev1.NodeSelectorTerm, placement plan.Placement) (corev1.NodeSelectorTerm, bool) {
	hadExpressions := len(term.MatchExpressions) > 0 || len(term.MatchFields) > 0

	expressions := make([]corev1.NodeSelectorRequirement, 0, len(term.MatchExpressions))
	for _, requirement := range term.MatchExpressions {
		if isAcceleratorRequirement(requirement, placement) {
			continue
		}
		expressions = append(expressions, requirement)
	}
	fields := make([]corev1.NodeSelectorRequirement, 0, len(term.MatchFields))
	for _, requirement := range term.MatchFields {
		if isAcceleratorRequirement(requirement, placement) {
			continue
		}
		fields = append(fields, requirement)
	}
	term.MatchExpressions = expressions
	term.MatchFields = fields

	if hadExpressions && len(expressions) == 0 && len(fields) == 0 {
		return corev1.NodeSelectorTerm{}, true
	}
	return term, false
}

// isAcceleratorRequirement reports whether a node selector requirement describes
// accelerator hardware.
func isAcceleratorRequirement(requirement corev1.NodeSelectorRequirement, placement plan.Placement) bool {
	if isAcceleratorLabel(requirement.Key, "", placement) {
		return true
	}
	for _, value := range requirement.Values {
		if isAcceleratorLabel(requirement.Key, value, placement) {
			return true
		}
	}
	return false
}

// isAcceleratorLabel reports whether a node label key, or one of its values, names
// accelerator hardware.
func isAcceleratorLabel(key, value string, placement plan.Placement) bool {
	lowerKey := strings.ToLower(key)
	for _, label := range placement.AcceleratorNodeLabels {
		if lowerKey == strings.ToLower(label) {
			return true
		}
	}
	for _, prefix := range placement.AcceleratorNodeLabelPrefixes {
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(lowerKey, strings.ToLower(prefix)) {
			return true
		}
	}
	if value != "" {
		lowerValue := strings.ToLower(value)
		for _, candidate := range placement.AcceleratorNodeValues {
			if candidate == "" {
				continue
			}
			if lowerValue == strings.ToLower(candidate) {
				return true
			}
		}
	}
	return false
}

// isAcceleratorToleration reports whether a toleration exists only for accelerator
// node taints.
func isAcceleratorToleration(toleration corev1.Toleration, placement plan.Placement) bool {
	key := strings.ToLower(strings.TrimSpace(toleration.Key))
	if key == "" {
		// A wildcard toleration tolerates every taint, which is not GPU-specific.
		return false
	}
	for _, candidate := range placement.AcceleratorTolerationKeys {
		if key == strings.ToLower(candidate) {
			return true
		}
	}
	return false
}

// containsToleration reports whether the toleration is already present.
func containsToleration(tolerations []corev1.Toleration, candidate corev1.Toleration) bool {
	for _, toleration := range tolerations {
		if toleration.Key == candidate.Key &&
			toleration.Operator == candidate.Operator &&
			toleration.Value == candidate.Value &&
			toleration.Effect == candidate.Effect {
			return true
		}
	}
	return false
}
