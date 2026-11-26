package webhook

import (
	"context"
	"fmt"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
)

// AllowCapacityReductionAnnotation lets an update lower spec.devicesPerNode
// deliberately. It is the explicit acknowledgement the validator requires, because
// a capacity reduction can leave admitted Pods unschedulable.
const AllowCapacityReductionAnnotation = names.AnnotationPrefix + "allow-capacity-reduction"

// EnvironmentValidator validates VcrGpuCluster writes.
//
// The CRD enforces the singleton name through a CEL rule. This webhook exists for
// defense in depth and for the transition rules CEL cannot express; everything
// else about the API is mutable by design, and deletion is always allowed.
type EnvironmentValidator struct{}

// NewEnvironmentValidator builds the validator. It holds no client: every rule is
// a function of the submitted object and its previous version.
func NewEnvironmentValidator() *EnvironmentValidator {
	return &EnvironmentValidator{}
}

// Handle implements the controller-runtime admission handler interface.
func (v *EnvironmentValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	return admission.Response{AdmissionResponse: *v.Validate(ctx, &req.AdmissionRequest)}
}

// Validate returns the validation decision for one VcrGpuCluster write.
//
// It is exported so tests and the handler share exactly one code path.
func (v *EnvironmentValidator) Validate(_ context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if req == nil {
		return denied("the admission request is empty")
	}
	switch req.Operation {
	case admissionv1.Delete:
		// A configuration the operator must be able to replace must never be
		// blocked; the configurations only list DELETE implicitly, but a direct
		// request is handled the same way.
		return allowed()
	case admissionv1.Create, admissionv1.Update:
	default:
		return allowed()
	}

	next := &vcrv1alpha1.VcrGpuCluster{}
	if err := decodeObject(req.Object.Raw, next, "VcrGpuCluster"); err != nil {
		return denied(err.Error())
	}
	if next.Name != vcrv1alpha1.VcrGpuClusterNameDefault {
		return denied(fmt.Sprintf(
			"name %q is not accepted: a VcrGpuCluster is a singleton cluster configuration and must be named %q",
			next.Name, vcrv1alpha1.VcrGpuClusterNameDefault))
	}
	if req.Operation != admissionv1.Update {
		return allowed()
	}

	previous := &vcrv1alpha1.VcrGpuCluster{}
	if err := decodeObject(req.OldObject.Raw, previous, "VcrGpuCluster"); err != nil {
		// Without the previous version no transition rule can be established, and
		// admitting on a guess would defeat the validator.
		return denied(fmt.Sprintf("the previous VcrGpuCluster cannot be read, so the update cannot be validated: %v", err))
	}

	if violations := transitionViolations(previous, next); len(violations) > 0 {
		return denied("the update is rejected: " + strings.Join(violations, "; "))
	}
	return allowed()
}

// transitionViolations reports the documented rules an update breaks.
//
// Order is stable so the message is deterministic.
func transitionViolations(previous, next *vcrv1alpha1.VcrGpuCluster) []string {
	var violations []string

	removed := removedProfiles(previous, next)
	gpuChanged := previous.Spec.GPUType != next.Spec.GPUType
	devicesChanged := previous.Spec.DevicesPerNode != next.Spec.DevicesPerNode
	if len(removed) > 0 && (gpuChanged || devicesChanged) {
		changed := make([]string, 0, 2)
		if gpuChanged {
			changed = append(changed, fmt.Sprintf("gpuType %q -> %q", previous.Spec.GPUType, next.Spec.GPUType))
		}
		if devicesChanged {
			changed = append(changed, fmt.Sprintf("devicesPerNode %d -> %d",
				previous.Spec.DevicesPerNode, next.Spec.DevicesPerNode))
		}
		violations = append(violations, fmt.Sprintf(
			"profiles %s are removed while %s changes in the same update: admitted Pods pinned a profile this transition no longer explains, so the catalog and capacity transitions must be separate updates",
			quoteAll(removed), strings.Join(changed, " and ")))
	}

	if next.Spec.DevicesPerNode < previous.Spec.DevicesPerNode &&
		next.Annotations[AllowCapacityReductionAnnotation] != "true" {
		violations = append(violations, fmt.Sprintf(
			"devicesPerNode decreases from %d to %d: admitted Pods requesting more than %d device(s) become unschedulable, so set annotation %s=%q on the object to acknowledge the reduction",
			previous.Spec.DevicesPerNode, next.Spec.DevicesPerNode, next.Spec.DevicesPerNode,
			AllowCapacityReductionAnnotation, "true"))
	}

	return violations
}

// removedProfiles lists the configured profile images the update drops, in spec
// order of the previous object.
func removedProfiles(previous, next *vcrv1alpha1.VcrGpuCluster) []string {
	kept := make(map[string]struct{}, len(next.Spec.Profiles))
	for _, source := range next.Spec.Profiles {
		kept[source.Image] = struct{}{}
	}
	var removed []string
	for _, source := range previous.Spec.Profiles {
		if _, ok := kept[source.Image]; !ok {
			removed = append(removed, source.Image)
		}
	}
	return removed
}

// quoteAll renders image references for a message.
func quoteAll(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = fmt.Sprintf("%q", value)
	}
	return strings.Join(quoted, ", ")
}
