package webhook

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/plan"
	"github.com/neuralmagic/vllm-vcr-operator/internal/profile"
	"github.com/neuralmagic/vllm-vcr-operator/internal/translator"
)

// admissionDenial is one reason admission refuses a request;
// operatorProblem selects the message shape.
type admissionDenial struct {
	detail string
	// operatorProblem distinguishes a cluster/operator failure from a workload
	// problem.
	operatorProblem bool
}

// response renders the denial.
func (d *admissionDenial) response() *admissionv1.AdmissionResponse {
	if d.operatorProblem {
		return operatorDenied(d.detail)
	}
	return workloadDenied(d.detail)
}

// operatorFailure builds an operator-problem denial.
func operatorFailure(format string, args ...any) *admissionDenial {
	return &admissionDenial{detail: fmt.Sprintf(format, args...), operatorProblem: true}
}

// workloadFailure builds a workload-problem denial.
func workloadFailure(format string, args ...any) *admissionDenial {
	return &admissionDenial{detail: fmt.Sprintf(format, args...)}
}

// environment reads the singleton VcrGpuCluster admission works against.
//
// A missing, unready, or empty-catalog environment is an operator problem: the
// workload may be perfectly valid, so the denial says so.
func (m *PodMutator) environment(ctx context.Context) (*vcrv1alpha1.VcrGpuCluster, *admissionDenial) {
	env := &vcrv1alpha1.VcrGpuCluster{}
	key := types.NamespacedName{Name: vcrv1alpha1.VcrGpuClusterNameDefault}
	if err := m.client.Get(ctx, key, env); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, operatorFailure(
				"no VcrGpuCluster named %q exists: create the cluster-scoped VcrGpuCluster before submitting GPU workloads",
				key.Name)
		}
		return nil, operatorFailure("the VcrGpuCluster %q cannot be read: %v", key.Name, err)
	}

	ready := meta.FindStatusCondition(env.Status.Conditions, vcrv1alpha1.ConditionReady)
	switch {
	case ready == nil:
		return nil, operatorFailure(
			"the VcrGpuCluster %q publishes no %s condition: reconciliation has not completed",
			env.Name, vcrv1alpha1.ConditionReady)
	case ready.Status != metav1.ConditionTrue:
		return nil, operatorFailure(
			"the VcrGpuCluster %q is not ready (%s): %s",
			env.Name, ready.Reason, ready.Message)
	case len(env.Status.Profiles) == 0:
		return nil, operatorFailure(
			"the VcrGpuCluster %q publishes no reconciled profile: spec.profiles must reference a profile image before GPU workloads can be admitted",
			env.Name)
	}
	return env, nil
}

// catalogFor rebuilds the selection catalog from the reconciled status entries.
//
// Admission never contacts a registry (docs/contracts/admission.md): the digests
// and metadata were resolved during reconciliation and are reused verbatim. An
// entry that cannot be parsed keeps its place as a rejected candidate so that a
// selection failure explains what was considered.
func catalogFor(env *vcrv1alpha1.VcrGpuCluster) (*profile.Catalog, error) {
	candidates := make([]profile.Candidate, 0, len(env.Status.Profiles))
	for _, published := range env.Status.Profiles {
		candidate := profile.Candidate{
			Image:   published.Image,
			Digest:  published.Digest,
			Message: published.Message,
		}
		if candidate.Message == "" {
			metadata, err := profile.Parse([]byte(published.Metadata))
			if err != nil {
				candidate.Message = err.Error()
			} else {
				candidate.Metadata = metadata
			}
		}
		candidates = append(candidates, candidate)
	}
	catalog, err := profile.NewCatalog(candidates)
	if err != nil {
		return nil, fmt.Errorf("the published profile catalog is inconsistent: %w", err)
	}
	return catalog, nil
}

// translatorEnvironment resolves the environment configuration admission hands to
// the translator.
//
// The provisioner needs no sizing of its own when the environment omits it: the
// profile export is a short copy job, and spec.runtime.provisioner is optional.
func translatorEnvironment(env *vcrv1alpha1.VcrGpuCluster, images translator.Images) translator.Environment {
	sizing := plan.Resources{
		Frontend: env.Spec.Runtime.Frontend,
		Backend:  env.Spec.Runtime.Backend,
	}
	if env.Spec.Runtime.Provisioner != nil {
		sizing.Provisioner = *env.Spec.Runtime.Provisioner
	}

	pullSecrets := make([]string, 0, len(env.Spec.ImagePullSecrets))
	for _, reference := range env.Spec.ImagePullSecrets {
		pullSecrets = append(pullSecrets, reference.Name)
	}
	profilePullSecrets := make(map[string]string, len(env.Spec.Profiles))
	for _, source := range env.Spec.Profiles {
		if source.PullSecretName != "" {
			profilePullSecrets[source.Image] = source.PullSecretName
		}
	}

	return translator.Environment{
		GPUType:              env.Spec.GPUType,
		DevicesPerNode:       env.Spec.DevicesPerNode,
		SupportedVLLMVersion: env.Status.SupportedVLLMVersion,
		LogLevel:             env.Spec.LogLevel,
		Images:               images,
		Sizing:               sizing,
		NodeSelector:         env.Spec.NodeSelection.NodeSelector,
		Tolerations:          env.Spec.NodeSelection.Tolerations,
		PullSecrets:          pullSecrets,
		ProfilePullSecrets:   profilePullSecrets,
	}
}

// volumeMap indexes the Pod's volumes by name, which is how internal/vllm
// resolves a mounted configuration file.
func volumeMap(pod *corev1.Pod) map[string]corev1.Volume {
	volumes := make(map[string]corev1.Volume, len(pod.Spec.Volumes))
	for _, volume := range pod.Spec.Volumes {
		volumes[volume.Name] = volume
	}
	return volumes
}

// recordOperatorProblem emits one warning event per operator-caused denial so the
// failure is visible with kubectl describe on the environment, and records it in
// the admission log.
//
// The recorder is optional, and the message never contains Secret material.
func (m *PodMutator) recordOperatorProblem(env *vcrv1alpha1.VcrGpuCluster, log logr.Logger, detail string) {
	name := ""
	if env != nil {
		name = env.Name
	}
	log.V(1).Info("denied a GPU Pod because of an operator problem", "environment", name, "reason", detail)
	if m.recorder == nil || env == nil {
		return
	}
	m.recorder.Event(env, corev1.EventTypeWarning, "AdmissionDenied", detail)
}
