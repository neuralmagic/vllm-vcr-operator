package webhook

import (
	"context"
	"fmt"
	"sort"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
)

// ProfileValidator keeps a namespace to one profile selection.
//
// The rule exists because admission reads the namespace's VcrGpuProfile to pin every
// qualifying workload to one recording. With two of them, which recording a Pod runs
// would depend on listing order — a decision that must be made by a person, not by an
// implementation detail.
//
// It holds a client, unlike the VcrGpuCluster validator, because the rule is a property of
// the namespace rather than of the submitted object.
type ProfileValidator struct {
	client client.Reader
}

// NewProfileValidator builds the validator.
func NewProfileValidator(reader client.Reader) *ProfileValidator {
	return &ProfileValidator{client: reader}
}

// Handle implements the controller-runtime admission handler interface.
func (v *ProfileValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	return admission.Response{AdmissionResponse: *v.Validate(ctx, &req.AdmissionRequest)}
}

// Validate returns the validation decision for one VcrGpuProfile write.
//
// It is exported so tests and the handler share exactly one code path.
func (v *ProfileValidator) Validate(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if req == nil {
		return denied("the admission request is empty")
	}
	switch req.Operation {
	case admissionv1.Delete:
		// Removing a selection is always allowed: a choice must be replaceable.
		return allowed()
	case admissionv1.Create, admissionv1.Update:
	default:
		return allowed()
	}
	if req.Kind.Kind != "" && req.Kind.Kind != "VcrGpuProfile" {
		return denied(fmt.Sprintf("this webhook validates VcrGpuProfile objects, not %q", req.Kind.Kind))
	}

	submitted := &vcrv1alpha1.VcrGpuProfile{}
	if err := decodeObject(req.Object.Raw, submitted, "VcrGpuProfile"); err != nil {
		return denied(err.Error())
	}
	resolveIdentity(&submitted.ObjectMeta, req)

	existing := &vcrv1alpha1.VcrGpuProfileList{}
	if err := v.client.List(ctx, existing, client.InNamespace(submitted.Namespace)); err != nil {
		// Fail closed: without the list, the uniqueness rule cannot be established.
		return operatorDenied(fmt.Sprintf("cannot list VcrGpuProfile objects in namespace %q: %v", submitted.Namespace, err))
	}

	others := make([]string, 0, len(existing.Items))
	for _, item := range existing.Items {
		// The object being updated is not a competing selection.
		if item.UID == submitted.UID && item.UID != types.UID("") {
			continue
		}
		if item.Name == submitted.Name {
			continue
		}
		others = append(others, item.Name)
	}
	if len(others) > 0 {
		sort.Strings(others)
		return denied(fmt.Sprintf(
			"namespace %q already has a profile selection (%s): exactly one VcrGpuProfile is allowed per namespace, "+
				"because admission pins every simulated workload in the namespace to one recording. "+
				"Change %q instead of adding another selection",
			submitted.Namespace, strings.Join(others, ", "), others[0]))
	}
	return allowed()
}
