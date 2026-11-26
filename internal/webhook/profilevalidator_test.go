package webhook_test

import (
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/webhook"
)

func selection(name, namespace, id string) *vcrv1alpha1.VcrGpuProfile {
	return &vcrv1alpha1.VcrGpuProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       vcrv1alpha1.VcrGpuProfileSpec{Profile: id},
	}
}

// TestFirstSelectionInANamespaceIsAllowed is the ordinary case: a workload owner picks a
// recording.
func TestFirstSelectionInANamespaceIsAllowed(t *testing.T) {
	validator := webhook.NewProfileValidator(testClient(t))
	resp := validator.Validate(t.Context(),
		admissionRequest(t, admissionv1.Create, selection("selection", "team-a", "profile-a")))
	if !resp.Allowed {
		t.Fatalf("first selection denied: %s", messageOf(resp))
	}
}

// TestSecondSelectionInANamespaceIsDenied: admission pins every simulated workload in a
// namespace to the namespace's selection, so a second one would make the recording depend
// on listing order. The denial has to name the object that already holds the choice,
// because that is what the user has to edit.
func TestSecondSelectionInANamespaceIsDenied(t *testing.T) {
	existing := selection("chosen", "team-a", "profile-a")
	validator := webhook.NewProfileValidator(testClient(t, existing))

	resp := validator.Validate(t.Context(),
		admissionRequest(t, admissionv1.Create, selection("another", "team-a", "profile-b")))
	if resp.Allowed {
		t.Fatal("a second selection in one namespace was allowed")
	}
	message := messageOf(resp)
	for _, want := range []string{"team-a", "chosen", "exactly one VcrGpuProfile"} {
		if !strings.Contains(message, want) {
			t.Errorf("denial does not mention %q: %s", want, message)
		}
	}
}

// TestSelectionInAnotherNamespaceIsIndependent: the rule is per namespace, so two
// namespaces may choose differently.
func TestSelectionInAnotherNamespaceIsIndependent(t *testing.T) {
	validator := webhook.NewProfileValidator(testClient(t, selection("chosen", "team-a", "profile-a")))
	resp := validator.Validate(t.Context(),
		admissionRequest(t, admissionv1.Create, selection("chosen", "team-b", "profile-b")))
	if !resp.Allowed {
		t.Fatalf("selection in a second namespace denied: %s", messageOf(resp))
	}
}

// TestUpdatingASelectionIsAlwaysAllowed: a choice must be changeable, otherwise the only
// way to pick a different recording would be to delete and recreate.
func TestUpdatingASelectionIsAlwaysAllowed(t *testing.T) {
	existing := selection("chosen", "team-a", "profile-a")
	validator := webhook.NewProfileValidator(testClient(t, existing))

	updated := existing.DeepCopy()
	updated.Spec.Profile = "profile-b"
	req := admissionRequest(t, admissionv1.Update, updated)
	req.Name = existing.Name
	req.Namespace = existing.Namespace

	resp := validator.Validate(t.Context(), req)
	if !resp.Allowed {
		t.Fatalf("update of the namespace's own selection denied: %s", messageOf(resp))
	}
}

// TestDeletingASelectionIsAlwaysAllowed: removal returns the namespace to
// requirement-based selection and must never be blocked.
func TestDeletingASelectionIsAlwaysAllowed(t *testing.T) {
	validator := webhook.NewProfileValidator(testClient(t))
	resp := validator.Validate(t.Context(),
		admissionRequest(t, admissionv1.Delete, selection("chosen", "team-a", "profile-a")))
	if !resp.Allowed {
		t.Fatalf("deletion denied: %s", messageOf(resp))
	}
}
