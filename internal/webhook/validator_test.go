package webhook_test

import (
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
	"github.com/neuralmagic/vllm-vcr-operator/internal/webhook"
)

const otherProfileImage = "quay.io/opendatahub/vllm-vcr-profiles:h100-8b-tp2-bf16"

// updateRequest renders an UPDATE AdmissionRequest carrying both versions.
func updateRequest(t *testing.T, previous, next *vcrv1alpha1.VcrGpuCluster) *admissionv1.AdmissionRequest {
	t.Helper()
	raw := func(object *vcrv1alpha1.VcrGpuCluster) runtime.RawExtension {
		encoded, err := json.Marshal(object)
		if err != nil {
			t.Fatalf("marshal VcrGpuCluster: %v", err)
		}
		return runtime.RawExtension{Raw: encoded}
	}
	return &admissionv1.AdmissionRequest{
		Operation: admissionv1.Update,
		Name:      next.Name,
		Object:    raw(next),
		OldObject: raw(previous),
	}
}

// TestValidatorAcceptsTheSingletonName asserts the documented name is admitted.
func TestValidatorAcceptsTheSingletonName(t *testing.T) {
	validator := webhook.NewEnvironmentValidator()

	resp := validator.Validate(t.Context(), admissionRequest(t, admissionv1.Create, testEnvironment(t)))

	if !resp.Allowed {
		t.Fatalf("VcrGpuCluster named default denied: %s", messageOf(resp))
	}
}

// TestValidatorRejectsAnotherName asserts the singleton policy is enforced even if
// the CEL rule is absent (defense in depth).
func TestValidatorRejectsAnotherName(t *testing.T) {
	other := testEnvironment(t)
	other.Name = "production"
	validator := webhook.NewEnvironmentValidator()

	resp := validator.Validate(t.Context(), admissionRequest(t, admissionv1.Create, other))

	if resp.Allowed {
		t.Fatal("VcrGpuCluster with another name admitted")
	}
	if message := messageOf(resp); !strings.Contains(message, vcrv1alpha1.VcrGpuClusterNameDefault) {
		t.Errorf("denial message %q does not name the only accepted name", message)
	}
}

// TestValidatorAllowsDeletion asserts a configuration can always be removed.
func TestValidatorAllowsDeletion(t *testing.T) {
	validator := webhook.NewEnvironmentValidator()

	resp := validator.Validate(t.Context(), admissionRequest(t, admissionv1.Delete, testEnvironment(t)))

	if !resp.Allowed {
		t.Fatalf("deletion denied: %s", messageOf(resp))
	}
}

// TestValidatorRejectsCapacityReductionWithoutAcknowledgement asserts lowering
// advertised capacity requires the documented annotation.
func TestValidatorRejectsCapacityReductionWithoutAcknowledgement(t *testing.T) {
	previous := testEnvironment(t)
	next := testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Spec.DevicesPerNode = 4
	})
	validator := webhook.NewEnvironmentValidator()

	resp := validator.Validate(t.Context(), updateRequest(t, previous, next))

	if resp.Allowed {
		t.Fatal("capacity reduction admitted without acknowledgement")
	}
	message := messageOf(resp)
	for _, want := range []string{"devicesPerNode", "8", "4", webhook.AllowCapacityReductionAnnotation} {
		if !strings.Contains(message, want) {
			t.Errorf("denial message %q does not mention %q", message, want)
		}
	}
}

// TestValidatorAllowsAcknowledgedCapacityReduction asserts the annotation is the
// explicit opt-in.
func TestValidatorAllowsAcknowledgedCapacityReduction(t *testing.T) {
	previous := testEnvironment(t)
	next := testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Spec.DevicesPerNode = 4
		e.Annotations = map[string]string{webhook.AllowCapacityReductionAnnotation: "true"}
	})
	validator := webhook.NewEnvironmentValidator()

	resp := validator.Validate(t.Context(), updateRequest(t, previous, next))

	if !resp.Allowed {
		t.Fatalf("acknowledged capacity reduction denied: %s", messageOf(resp))
	}
}

// TestValidatorAllowsCapacityIncrease asserts growing capacity needs no
// acknowledgement.
func TestValidatorAllowsCapacityIncrease(t *testing.T) {
	previous := testEnvironment(t)
	next := testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Spec.DevicesPerNode = 16
	})
	validator := webhook.NewEnvironmentValidator()

	resp := validator.Validate(t.Context(), updateRequest(t, previous, next))

	if !resp.Allowed {
		t.Fatalf("capacity increase denied: %s", messageOf(resp))
	}
}

// TestValidatorRejectsProfileRemovalWithGPUTypeChange asserts the ambiguous
// transition is refused and names both halves of it.
func TestValidatorRejectsProfileRemovalWithGPUTypeChange(t *testing.T) {
	previous := environmentWithProfiles(t, testImage, otherProfileImage)
	next := environmentWithProfiles(t, testImage)
	next.Spec.GPUType = "H100"
	validator := webhook.NewEnvironmentValidator()

	resp := validator.Validate(t.Context(), updateRequest(t, previous, next))

	if resp.Allowed {
		t.Fatal("profile removal combined with a gpuType change admitted")
	}
	message := messageOf(resp)
	for _, want := range []string{otherProfileImage, "gpuType"} {
		if !strings.Contains(message, want) {
			t.Errorf("denial message %q does not mention %q", message, want)
		}
	}
}

// TestValidatorRejectsProfileRemovalWithDeviceChange asserts the capacity half of
// the ambiguous transition.
func TestValidatorRejectsProfileRemovalWithDeviceChange(t *testing.T) {
	previous := environmentWithProfiles(t, testImage, otherProfileImage)
	next := environmentWithProfiles(t, testImage)
	next.Spec.DevicesPerNode = 16
	validator := webhook.NewEnvironmentValidator()

	resp := validator.Validate(t.Context(), updateRequest(t, previous, next))

	if resp.Allowed {
		t.Fatal("profile removal combined with a device-count change admitted")
	}
	if message := messageOf(resp); !strings.Contains(message, "devicesPerNode") || !strings.Contains(message, otherProfileImage) {
		t.Errorf("denial message %q does not describe the transition", message)
	}
}

// TestValidatorAllowsProfileRemovalAlone asserts a catalog change on its own is
// allowed: the API is otherwise mutable by design.
func TestValidatorAllowsProfileRemovalAlone(t *testing.T) {
	previous := environmentWithProfiles(t, testImage, otherProfileImage)
	next := environmentWithProfiles(t, testImage)
	validator := webhook.NewEnvironmentValidator()

	resp := validator.Validate(t.Context(), updateRequest(t, previous, next))

	if !resp.Allowed {
		t.Fatalf("profile removal denied: %s", messageOf(resp))
	}
}

// TestValidatorAllowsProfileImageChange asserts changing a profile's image
// identity is a supported update.
func TestValidatorAllowsProfileImageChange(t *testing.T) {
	previous := testEnvironment(t)
	next := testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Spec.Profiles = []vcrv1alpha1.ProfileSource{{Image: otherProfileImage}}
	})
	validator := webhook.NewEnvironmentValidator()

	resp := validator.Validate(t.Context(), updateRequest(t, previous, next))

	if !resp.Allowed {
		t.Fatalf("profile image change denied: %s", messageOf(resp))
	}
}

// TestValidatorAllowsGPUTypeChangeAlone asserts replacing the simulated
// accelerator without touching the catalog is allowed.
func TestValidatorAllowsGPUTypeChangeAlone(t *testing.T) {
	previous := testEnvironment(t)
	next := testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Spec.GPUType = "H100"
	})
	validator := webhook.NewEnvironmentValidator()

	resp := validator.Validate(t.Context(), updateRequest(t, previous, next))

	if !resp.Allowed {
		t.Fatalf("gpuType change denied: %s", messageOf(resp))
	}
}

// TestValidatorRejectsUpdateWithoutPreviousObject asserts the transition rules are
// never evaluated against a guess.
func TestValidatorRejectsUpdateWithoutPreviousObject(t *testing.T) {
	validator := webhook.NewEnvironmentValidator()
	request := admissionRequest(t, admissionv1.Update, testEnvironment(t))

	resp := validator.Validate(t.Context(), request)

	if resp.Allowed {
		t.Fatal("update without the previous object admitted")
	}
}

// TestValidatorAnnotationsAreDocumented asserts the acknowledgement annotation lives
// in the operator's annotation namespace.
func TestValidatorAnnotationsAreDocumented(t *testing.T) {
	if !strings.HasPrefix(webhook.AllowCapacityReductionAnnotation, names.AnnotationPrefix) {
		t.Errorf("%s = %q does not use the operator annotation prefix %q",
			"AllowCapacityReductionAnnotation", webhook.AllowCapacityReductionAnnotation, names.AnnotationPrefix)
	}
	if webhook.AllowCapacityReductionAnnotation != "vcr.opendatahub-io/allow-capacity-reduction" {
		t.Errorf("AllowCapacityReductionAnnotation = %q, want the documented value",
			webhook.AllowCapacityReductionAnnotation)
	}
}

// environmentWithProfiles builds an environment listing the given profile images.
func environmentWithProfiles(t *testing.T, images ...string) *vcrv1alpha1.VcrGpuCluster {
	t.Helper()
	return testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Spec.Profiles = make([]vcrv1alpha1.ProfileSource, len(images))
		for i, image := range images {
			e.Spec.Profiles[i] = vcrv1alpha1.ProfileSource{Image: image}
		}
	})
}
