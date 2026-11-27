package webhook

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/neuralmagic/vllm-vcr-operator/internal/credentials"
	"github.com/neuralmagic/vllm-vcr-operator/internal/plan"
	"github.com/neuralmagic/vllm-vcr-operator/internal/projection"
)

// projectPullSecrets copies the pull credentials the selected profile image needs
// into the workload namespace and returns the name of the copy to reference.
//
// Sources are Secret names in the operator namespace. What is projected depends on
// how the environment configured them (spec.imagePullSecrets and
// spec.profiles[].pullSecretName):
//
//   - a profile's explicit pullSecretName is projected as configured. The operator
//     chose it for exactly this profile, so it is not second-guessed;
//   - otherwise the environment-wide list is a candidate list. Only entries whose
//     docker config covers the selected image's registry host are projected, and
//     when no candidate does, nothing is projected: the pull proceeds anonymously
//     rather than copying unrelated registry credentials into the namespace.
//
// The merged payload is one document, because a Pod may only reference whole
// Secrets. The copy is named after the set of sources, not after its content:
// rotating a credential then updates one copy instead of leaving the old one
// behind, and every Pod that needs the same credentials references the same Secret.
// An existing copy is only written when its payload differs.
//
// The payload never leaves this function: the returned name and every error name
// Secrets and registry hosts only. dryRun skips the write but still reads and merges
// the sources, so a dry-run request fails or succeeds for exactly the same reasons
// as a real one and produces the same response.
func (m *PodMutator) projectPullSecrets(ctx context.Context, pod *corev1.Pod, admission *plan.Admission, dryRun bool) (string, error) {
	if admission == nil || len(admission.PullSecrets) == 0 {
		return "", nil
	}
	required := strings.TrimSpace(admission.ProfilePullSecret) != ""
	merged, err := projection.Merge(ctx, m.reader, m.operatorNamespace, admission.PullSecrets,
		credentials.ImageHost(admission.Profile.Image), required)
	if err != nil {
		return "", err
	}
	if merged == nil {
		// No configured Secret can authenticate a pull of this image's registry.
		return "", nil
	}
	// The copy is named after the sources that actually contributed an entry, not
	// after every candidate: two profiles served from different registries must not
	// share one copy whose payload then depends on which Pod was admitted last.
	name := projection.Name(merged.Sources)
	if dryRun {
		return name, nil
	}
	if err := projection.Ensure(ctx, m.client, m.reader, projection.Copy{
		Merged:    *merged,
		Namespace: pod.Namespace,
		Name:      name,
		AppliedTo: pod.Namespace + "/" + pod.Name,
		UpdatedAt: m.now().UTC().Format(time.RFC3339),
	}); err != nil {
		return "", err
	}
	return name, nil
}
