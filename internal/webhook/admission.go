package webhook

import (
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
)

// allowed admits the object unchanged.
func allowed() *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{Allowed: true}
}

// allowedWithPatch admits the object with a JSON patch.
func allowedWithPatch(patch []byte, patchType admissionv1.PatchType) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{Allowed: true, Patch: patch, PatchType: &patchType}
}

// denied rejects the object with a message the user can act on.
//
// The message never carries credential material: every producer of a message in
// this package reports settings, Secret names, and registry hosts only.
func denied(message string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: false,
		Result: &metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    http.StatusForbidden,
			Reason:  metav1.StatusReasonForbidden,
			Message: message,
		},
	}
}

// workloadDenied rejects a Pod because of its own configuration.
func workloadDenied(detail string) *admissionv1.AdmissionResponse {
	return denied("the Pod cannot be simulated: " + detail +
		". Remove the " + names.SyntheticResourceName +
		" request to run the workload unmodified, or adjust the workload configuration named above.")
}

// operatorDenied rejects a request because of operator or cluster configuration.
//
// It is deliberately distinguishable from workloadDenied: a user must not be told
// to change a valid workload when the operator is the broken part.
func operatorDenied(detail string) *admissionv1.AdmissionResponse {
	return denied("the Pod cannot be admitted: operator problem: " + detail +
		". This is a cluster configuration or operator failure, not a workload problem; " +
		"check the default SimulationEnvironment and the operator logs.")
}

// decodeObject unmarshals an admission object into target.
func decodeObject(raw []byte, target any, kind string) error {
	if len(raw) == 0 {
		return fmt.Errorf("the admission request carries no %s object", kind)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		// The decoder error quotes input bytes but no object this webhook handles
		// is a Secret, so it is safe to keep the position information.
		return fmt.Errorf("the admission request does not carry a valid %s: %w", kind, err)
	}
	return nil
}
