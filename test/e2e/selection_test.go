package e2e

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
)

// TestNamespaceSelectionPinsOneOfSeveralCompatibleProfiles is the feature end to end: an
// ambiguous catalog (two recordings that both fit the workload) plus a namespace selection
// that decides which one runs.
func TestNamespaceSelectionPinsOneOfSeveralCompatibleProfiles(t *testing.T) {
	h := requireSuite(t)
	h.requireReadyEnvironment(t)

	h.setProfiles(t,
		vcrv1alpha1.ProfileSource{Image: goodProfileImage},
		vcrv1alpha1.ProfileSource{Image: twinProfileImage},
	)
	h.waitForProfiles(t, twinProfileImage)
	h.requireReadyEnvironment(t)

	namespace := h.newNamespace(t)
	h.createSelection(t, namespace, twinProfileID)
	h.waitForResolvedSelection(t, namespace, twinProfileID, twinProfileDigest)

	pod := podFromTemplate(h.modelserverDeployment(t, namespace))
	pod.GenerateName = ""
	pod.Name = "selected-workload"

	created := h.createPod(t, pod)
	if got, want := created.Annotations[names.ProfileAnnotation], twinProfileID; got != want {
		t.Errorf("injected profile = %q, want the namespace's selection %q", got, want)
	}
	if got := created.Annotations[names.ProfileImageAnnotation]; !strings.HasSuffix(got, twinProfileDigest) {
		t.Errorf("injected profile image = %q, want the selected digest", got)
	}
}

// TestSecondSelectionInANamespaceIsRejected: two selections would make the recording a Pod
// runs depend on listing order, so the second one must be refused with a message naming the
// object that already holds the choice.
func TestSecondSelectionInANamespaceIsRejected(t *testing.T) {
	h := requireSuite(t)
	h.requireReadyEnvironment(t)

	namespace := h.newNamespace(t)
	h.createSelection(t, namespace, profileID)

	second := &vcrv1alpha1.VcrGpuProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: namespace},
		Spec:       vcrv1alpha1.VcrGpuProfileSpec{Profile: twinProfileID},
	}
	err := h.client.Create(helperContext(t), second)
	if err == nil {
		t.Fatal("a second selection in one namespace was accepted")
	}
	message := err.Error()
	for _, want := range []string{"exactly one VcrGpuProfile", namespace} {
		if !strings.Contains(message, want) {
			t.Errorf("rejection %q does not mention %q", message, want)
		}
	}
}

// TestSelectionNamingAnUnknownProfileIsReportedOnTheObject keeps the mistake where the
// user can see it: the object explains that the id is not offered, instead of leaving it to
// fail at Pod creation.
func TestSelectionNamingAnUnknownProfileIsReportedOnTheObject(t *testing.T) {
	h := requireSuite(t)
	h.requireReadyEnvironment(t)

	namespace := h.newNamespace(t)
	h.createSelection(t, namespace, "not-offered")

	h.poll(t, "the selection to report the unknown profile", func() (bool, error) {
		selection := &vcrv1alpha1.VcrGpuProfile{}
		key := types.NamespacedName{Namespace: namespace, Name: "selection"}
		if err := h.client.Get(helperContext(t), key, selection); err != nil {
			return false, err
		}
		for _, condition := range selection.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == metav1.ConditionFalse &&
				strings.Contains(condition.Message, profileID) {
				return true, nil
			}
		}
		return false, nil
	})
}

// createSelection creates the namespace's profile selection.
func (h *harness) createSelection(t *testing.T, namespace, profileID string) {
	t.Helper()
	selection := &vcrv1alpha1.VcrGpuProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "selection", Namespace: namespace},
		Spec:       vcrv1alpha1.VcrGpuProfileSpec{Profile: profileID},
	}
	if err := h.client.Create(helperContext(t), selection); err != nil {
		t.Fatalf("create VcrGpuProfile: %v", err)
	}
	t.Cleanup(func() {
		_ = h.client.Delete(helperContext(t), selection)
	})
}

// waitForResolvedSelection waits until the selection reports the digest it pins.
func (h *harness) waitForResolvedSelection(t *testing.T, namespace, profileID, digest string) {
	t.Helper()
	h.poll(t, "the selection to resolve "+profileID, func() (bool, error) {
		selection := &vcrv1alpha1.VcrGpuProfile{}
		key := types.NamespacedName{Namespace: namespace, Name: "selection"}
		if err := h.client.Get(helperContext(t), key, selection); err != nil {
			return false, err
		}
		resolved := selection.Status.Resolved
		return resolved != nil && resolved.ID == profileID && resolved.Digest == digest, nil
	})
}
