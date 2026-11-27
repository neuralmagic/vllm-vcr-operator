// Package pullsecrets reads a cluster's pull-secret configuration out of the spec
// into the decision type admission, the translator, and the reconciler share.
//
// The selection rule itself lives in internal/plan.PullSecrets. This package is the
// one place that walks spec.imagePullSecrets and spec.profiles, so admission and the
// reconciler cannot disagree about which Secret applies to which image.
package pullsecrets

import (
	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/plan"
	"github.com/neuralmagic/vllm-vcr-operator/internal/registry"
)

// FromSpec resolves the pull Secrets the cluster spec configures.
//
// Each profile image is registered under two spellings: the one the spec uses and
// the reference resolved against spec.profileRegistry. status.profiles reports the
// resolved spelling, and a lookup that only knew the spec spelling lost the
// override — the workload then pulled a private profile anonymously.
//
// An image that names another registry resolves to an error; only the spec spelling
// is registered then. Such a profile is unusable and admission refuses it, naming
// the configured registry, so there is nothing to project.
func FromSpec(spec vcrv1alpha1.VcrGpuClusterSpec) plan.PullSecrets {
	sources := plan.PullSecrets{
		Candidates: make([]string, 0, len(spec.ImagePullSecrets)),
		Overrides:  make(map[string]string, len(spec.Profiles)),
	}
	for _, reference := range spec.ImagePullSecrets {
		sources.Candidates = append(sources.Candidates, reference.Name)
	}
	for _, source := range spec.Profiles {
		if source.PullSecretName == "" {
			continue
		}
		sources.Overrides[source.Image] = source.PullSecretName
		if reference, err := registry.JoinReference(spec.ProfileRegistry, source.Image); err == nil {
			sources.Overrides[reference] = source.PullSecretName
		}
	}
	return sources
}
