package webhook_test

import (
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/types"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
	"github.com/neuralmagic/vllm-vcr-operator/internal/profile"
)

// twoProfiles returns an environment whose catalog offers two recordings with the same
// recorded requirements — exactly the case where selection has to come from the namespace
// rather than from the workload.
func twoProfiles(t *testing.T) *vcrv1alpha1.VcrGpuCluster {
	t.Helper()
	second := testMetadata(func(m *profile.Metadata) { m.ID = "second-profile" })
	return testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Status.Profiles = []vcrv1alpha1.ResolvedProfile{
			resolvedProfile(t, testMetadata()),
			resolvedProfile(t, second),
		}
	})
}

// TestTwoCompatibleProfilesAreAmbiguousWithoutASelection: without a namespace choice the
// catalog cannot decide, and guessing would make the recording depend on ordering.
func TestTwoCompatibleProfilesAreAmbiguousWithoutASelection(t *testing.T) {
	mutator := testMutator(t, testClient(t, twoProfiles(t)))

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, testPod()))

	if resp.Allowed {
		t.Fatal("an ambiguous catalog admitted a Pod without a selection")
	}
	if message := messageOf(resp); !strings.Contains(message, "ambiguous") {
		t.Errorf("denial %q does not report ambiguity", message)
	}
}

// TestSelectionPinsTheProfileTheNamespaceChose is the feature: the namespace's
// VcrGpuProfile decides which recording runs, out of a catalog that cannot decide alone.
func TestSelectionPinsTheProfileTheNamespaceChose(t *testing.T) {
	chosen := selection("selection", testPod().Namespace, "second-profile")
	mutator := testMutator(t, testClient(t, twoProfiles(t), chosen))
	pod := testPod()

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
	if !resp.Allowed {
		t.Fatalf("Pod denied with a namespace selection: %s", messageOf(resp))
	}
	injected := applyPatch(t, pod, resp.Patch)
	if got := injected.Annotations[names.ProfileAnnotation]; !strings.Contains(got, "second-profile") {
		t.Errorf("injected profile = %q, want the selected identity", got)
	}
}

// TestSelectionNamingAnUnknownProfileIsDeniedWithWhatIsOffered: the user picked an
// identity the cluster does not carry, so the denial has to list what it does carry.
func TestSelectionNamingAnUnknownProfileIsDeniedWithWhatIsOffered(t *testing.T) {
	unknown := selection("selection", testPod().Namespace, "not-a-profile")
	env := testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Status.Profiles = []vcrv1alpha1.ResolvedProfile{resolvedProfile(t, testMetadata())}
	})
	mutator := testMutator(t, testClient(t, env, unknown))

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, testPod()))

	if resp.Allowed {
		t.Fatal("a Pod admitted against a selection naming an unknown profile")
	}
	message := messageOf(resp)
	for _, want := range []string{"not-a-profile", testProfileID} {
		if !strings.Contains(message, want) {
			t.Errorf("denial does not mention %q: %s", want, message)
		}
	}
}

// TestSelectionDoesNotRelaxTheWorkloadsOwnRequirements: a selection narrows the catalog
// to one recording; it never makes an incompatible recording serve the workload, and the
// denial names both the workload's mismatch and the profile the namespace asked for.
func TestSelectionDoesNotRelaxTheWorkloadsOwnRequirements(t *testing.T) {
	// The selected recording serves a different model, so it cannot serve this workload
	// however the namespace votes.
	otherModel := testMetadata(func(m *profile.Metadata) {
		m.ID = "other-model-profile"
		m.Model.Name = "another/model"
	})
	env := testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Status.Profiles = []vcrv1alpha1.ResolvedProfile{
			resolvedProfile(t, testMetadata()),
			resolvedProfile(t, otherModel),
		}
	})
	chosen := selection("selection", testPod().Namespace, "other-model-profile")
	mutator := testMutator(t, testClient(t, env, chosen))

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, testPod()))

	if resp.Allowed {
		t.Fatal("a selection made an incompatible recording serve the workload")
	}
	message := messageOf(resp)
	if !strings.Contains(message, "other-model-profile") {
		t.Errorf("denial %q does not name the profile the namespace asked for", message)
	}
	if !strings.Contains(message, "model.name") {
		t.Errorf("denial %q does not name the requirement that failed", message)
	}
}

// TestSelectionIsIgnoredForWorkloadsThatDoNotRequestTheDevice keeps ordinary Pods in a
// namespace with a selection untouched.
func TestSelectionIsIgnoredForWorkloadsThatDoNotRequestTheDevice(t *testing.T) {
	chosen := selection("selection", testPod().Namespace, "not-a-profile")
	mutator := testMutator(t, testClient(t, testEnvironment(t), chosen))

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, cpuOnlyPod()))

	if !resp.Allowed || len(resp.Patch) != 0 {
		t.Fatalf("a CPU-only Pod was affected by the namespace selection: allowed=%v patch=%s",
			resp.Allowed, resp.Patch)
	}
}

// TestMoreThanOneSelectionIsAnOperatorProblem covers a namespace that carries two
// selections anyway (the validator bypassed, or objects created before it): the operator
// cannot choose, and the denial must say so rather than pick one.
func TestMoreThanOneSelectionIsAnOperatorProblem(t *testing.T) {
	first := selection("first", testPod().Namespace, testProfileID)
	second := selection("second", testPod().Namespace, "second-profile")
	second.UID = types.UID("uid-second")
	mutator := testMutator(t, testClient(t, twoProfiles(t), first, second))

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, testPod()))

	if resp.Allowed {
		t.Fatal("a Pod admitted in a namespace with two selections")
	}
	message := messageOf(resp)
	if !strings.Contains(message, "operator problem") || !strings.Contains(message, "exactly one profile selection") {
		t.Errorf("denial %q does not report the duplicate choices", message)
	}
}
