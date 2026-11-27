package pullsecrets_test

import (
	"testing"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/pullsecrets"
)

// TestFromSpecRegistersEverySpellingOfAProfileImage: status.profiles reports the
// reference resolved against spec.profileRegistry, while the spec may name only the
// tag. A lookup that knew one spelling and not the other lost the override, and the
// workload pulled a private profile anonymously.
func TestFromSpecRegistersEverySpellingOfAProfileImage(t *testing.T) {
	spec := vcrv1alpha1.VcrGpuClusterSpec{
		ProfileRegistry: "ghcr.io/acme/vllm-vcr-profiles",
		Profiles: []vcrv1alpha1.ProfileSource{{
			Image:          "qwen3-0.6b-h200",
			PullSecretName: "profile-pull",
		}},
	}

	sources := pullsecrets.FromSpec(spec)
	for _, image := range []string{
		"qwen3-0.6b-h200",
		"ghcr.io/acme/vllm-vcr-profiles:qwen3-0.6b-h200",
	} {
		if got := sources.For(image).Override; got != "profile-pull" {
			t.Errorf("override for %q = %q, want the profile's Secret", image, got)
		}
	}
}

// TestFromSpecKeepsTheEnvironmentWideCandidates: the candidates are what admission
// projects when a profile names no Secret of its own.
func TestFromSpecKeepsTheEnvironmentWideCandidates(t *testing.T) {
	spec := vcrv1alpha1.VcrGpuClusterSpec{
		ImagePullSecrets: []vcrv1alpha1.LocalSecretReference{{Name: "one"}, {Name: "two"}},
		Profiles:         []vcrv1alpha1.ProfileSource{{Image: "quay.io/a/profile"}},
	}

	choice := pullsecrets.FromSpec(spec).For("quay.io/a/profile")
	if len(choice.Names) != 2 || choice.Names[0] != "one" || choice.Names[1] != "two" {
		t.Errorf("Names = %v, want the environment-wide candidates in spec order", choice.Names)
	}
	if choice.Override != "" {
		t.Errorf("Override = %q, want it empty when the profile names no Secret", choice.Override)
	}
}

// TestFromSpecLeavesAnUnresolvableProfileToAdmission: an image naming another
// registry cannot be resolved, and admission refuses that profile by name, so only
// the spec spelling is registered and nothing is projected for it.
func TestFromSpecLeavesAnUnresolvableProfileToAdmission(t *testing.T) {
	spec := vcrv1alpha1.VcrGpuClusterSpec{
		ProfileRegistry: "ghcr.io/acme/vllm-vcr-profiles",
		Profiles: []vcrv1alpha1.ProfileSource{{
			Image:          "quay.io/elsewhere/profile:tag",
			PullSecretName: "profile-pull",
		}},
	}

	sources := pullsecrets.FromSpec(spec)
	if got := sources.For("quay.io/elsewhere/profile:tag").Override; got != "profile-pull" {
		t.Errorf("override = %q, want the spec spelling to keep its Secret", got)
	}
	if got := sources.For("ghcr.io/acme/vllm-vcr-profiles:quay.io/elsewhere/profile:tag").Override; got != "" {
		t.Errorf("override = %q, want nothing registered for a reference that cannot exist", got)
	}
}
