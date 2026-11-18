// Package runtime applies an admission plan to a submitted Pod.
//
// The package is a pure function over typed Kubernetes objects: it holds no
// client, performs no I/O, and never invents a decision. internal/translator
// decided which profile to pin, how to configure the frontend and the simulator,
// what resources the injected runtime receives, and which placement constraints are
// accelerator-specific; this package applies exactly that and nothing more.
//
// The layout it produces is documented in docs/contracts/runtime-layout.md:
// the workload's own vLLM container keeps its identity, inference port, probes,
// mounts, and GPU allocation, but runs the supervisor instead of the engine; the
// simulator and the profile provisioner are injected; a shared profile volume holds
// the exported profile data. Mutation is idempotent, so applying it twice is a
// no-op, and it never writes credential material anywhere.
package runtime

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/plan"
)

// tmpVolumeName and tmpMountPath give the simulator a writable scratch directory
// while its root filesystem stays read-only. The simulator resolves remote trace
// references by downloading them into a temporary directory; the operator always
// points it at the exported profile content, so the mount exists for the process's
// other scratch needs rather than for trace fetching.
const (
	tmpVolumeName = "vcr-tmp"
	tmpMountPath  = "/tmp"
)

// Mutate returns a copy of pod with the admission plan applied.
//
// The input Pod is never modified. A Pod that this operator already mutated is
// returned unchanged, so a repeated admission cannot duplicate containers or
// annotations.
func Mutate(pod *corev1.Pod, admission *plan.Admission) (*corev1.Pod, error) {
	if pod == nil {
		return nil, fmt.Errorf("runtime: no Pod to mutate")
	}
	if admission == nil {
		return nil, fmt.Errorf("runtime: no admission plan: a Pod is only mutated with a translation result")
	}

	if alreadyInjected(pod) {
		return pod.DeepCopy(), nil
	}

	workloadIndex, gpuQuantity, err := findWorkloadContainer(pod)
	if err != nil {
		return nil, err
	}
	if err := checkInitContainers(pod); err != nil {
		return nil, err
	}

	out := pod.DeepCopy()
	original := out.Spec.Containers[workloadIndex]

	// Accelerator devices are replaced by the synthetic device plugin, so the
	// host directories that expose real devices are removed before the runtime
	// containers are rewritten.
	removeAcceleratorVolumes(out)

	if err := rewriteFrontendContainer(&out.Spec.Containers[workloadIndex], &original, admission); err != nil {
		return nil, err
	}

	backend, err := backendContainer(admission)
	if err != nil {
		return nil, err
	}
	out.Spec.Containers = append(out.Spec.Containers, backend)

	provisioner, err := provisionerContainer(admission)
	if err != nil {
		return nil, err
	}
	out.Spec.InitContainers = append(out.Spec.InitContainers, provisioner)

	if err := addProfileVolume(out); err != nil {
		return nil, err
	}
	if err := addScratchVolume(out); err != nil {
		return nil, err
	}

	if err := rewritePlacement(out, admission.Placement); err != nil {
		return nil, err
	}
	setProvenanceAnnotations(out, admission, gpuQuantity)

	return out, nil
}

// alreadyInjected reports whether this operator already mutated the Pod. The
// injected annotation alone is not enough: the annotation and the injected
// containers together identify a complete layout.
func alreadyInjected(pod *corev1.Pod) bool {
	if pod.Annotations[names.InjectedAnnotation] == "" {
		return false
	}
	return hasContainer(pod.Spec.Containers, names.BackendContainerName) &&
		hasContainer(pod.Spec.InitContainers, names.ProvisionerContainerName)
}

func hasContainer(containers []corev1.Container, name string) bool {
	for _, container := range containers {
		if container.Name == name {
			return true
		}
	}
	return false
}

// findWorkloadContainer returns the index of the container whose GPU allocation is
// preserved, together with that allocation.
//
// Exactly one container may request the synthetic resource: it is the container
// whose entrypoint is replaced, and a Pod with two GPU consumers cannot be
// translated without deciding which one owns the allocation.
func findWorkloadContainer(pod *corev1.Pod) (int, resource.Quantity, error) {
	found := -1
	var quantity resource.Quantity
	described := make([]string, 0, 2)

	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		value, ok := gpuQuantityOf(container.Resources)
		if !ok {
			continue
		}
		described = append(described, container.Name)
		if found >= 0 {
			continue
		}
		found = i
		quantity = value
	}

	switch {
	case found < 0:
		return 0, quantity, fmt.Errorf(
			"runtime: no container requests %s: the Pod is not applicable and must not be mutated",
			names.SyntheticResourceName)
	case len(described) > 1:
		return 0, quantity, fmt.Errorf(
			"runtime: containers %s all request %s: exactly one workload container may own the synthetic allocation",
			strings.Join(described, ", "), names.SyntheticResourceName)
	}
	return found, quantity, nil
}

// gpuQuantityOf returns the container's synthetic device allocation.
func gpuQuantityOf(resources corev1.ResourceRequirements) (resource.Quantity, bool) {
	gpu := corev1.ResourceName(names.SyntheticResourceName)
	if quantity, ok := resources.Requests[gpu]; ok {
		return quantity, true
	}
	if quantity, ok := resources.Limits[gpu]; ok {
		return quantity, true
	}
	return resource.Quantity{}, false
}

// checkInitContainers rejects a GPU-dependent init container the translator does
// not recognize. An init container is never dropped silently: either it is a
// recognized setup step that can run without a GPU, or the Pod cannot be
// translated as a whole.
func checkInitContainers(pod *corev1.Pod) error {
	for _, container := range pod.Spec.InitContainers {
		if _, ok := gpuQuantityOf(container.Resources); !ok {
			continue
		}
		if recognizedGPUInitContainer(container) {
			continue
		}
		return fmt.Errorf(
			"runtime: init container %q requests %s and is not a recognized weight-download step: a GPU-dependent setup step cannot be translated",
			container.Name, names.SyntheticResourceName)
	}
	return nil
}

// gpuInitContainerNameHints are name fragments of setup steps that fetch model
// artifacts, which is the only GPU-dependent init-container pattern this operator
// understands.
var gpuInitContainerNameHints = []string{"download", "fetch", "weights", "loader"}

// gpuInitContainerCommandHints are command fragments of artifact fetchers.
var gpuInitContainerCommandHints = []string{
	"huggingface-cli", "hf", "git", "wget", "curl", "aws", "gsutil", "gs",
}

// recognizedGPUInitContainer reports whether an init container is a known
// artifact-download step. Such a step keeps its GPU allocation and is preserved
// verbatim: it schedules against the same synthetic devices and does not run the
// model server.
func recognizedGPUInitContainer(container corev1.Container) bool {
	name := strings.ToLower(container.Name)
	for _, hint := range gpuInitContainerNameHints {
		if strings.Contains(name, hint) {
			return true
		}
	}
	for _, token := range append(append([]string{}, container.Command...), container.Args...) {
		lower := strings.ToLower(token)
		for _, hint := range gpuInitContainerCommandHints {
			if lower == hint || strings.HasSuffix(lower, "/"+hint) || strings.Contains(lower, hint+" ") {
				return true
			}
		}
	}
	return false
}

// rewriteFrontendContainer turns the workload's vLLM container into the frontend
// container: same identity, ports, probes, and mounts; a different entrypoint,
// image, environment, and sizing.
func rewriteFrontendContainer(container, original *corev1.Container, admission *plan.Admission) error {
	frontend := admission.Frontend
	if len(frontend.Command) == 0 {
		return fmt.Errorf("runtime: the admission plan carries no frontend command for container %q", container.Name)
	}

	copied, err := copiedArguments(*original, frontend.CopiedArguments)
	if err != nil {
		return err
	}

	container.Image = frontend.Image
	container.ImagePullPolicy = corev1.PullIfNotPresent
	container.Command = append([]string(nil), frontend.Command...)
	container.Args = append(append([]string(nil), frontend.Args...), copied...)
	container.Env = mergeEnv(original.Env, frontend.Env, frontend.RemoveEnv)
	container.Ports = mergePorts(original.Ports, frontend.Ports)
	container.Resources = mergeResources(admission.Resources.Frontend, original.Resources)
	addProfileMount(&container.VolumeMounts, true)
	return nil
}

// copiedArguments resolves the argument fragments the plan copies from the
// workload container instead of storing them.
//
// The fragments are copied verbatim so that a credential never enters the plan,
// annotations, status, or logs. A fragment that cannot be located is an error: a
// server that silently starts without its configured API key would serve
// unauthenticated requests.
func copiedArguments(original corev1.Container, copied []plan.CopiedArgument) ([]string, error) {
	if len(copied) == 0 {
		return nil, nil
	}
	argv := append(append([]string(nil), original.Command...), original.Args...)

	var out []string
	for _, argument := range copied {
		fragment, err := findArgument(argv, argument)
		if err != nil {
			return nil, err
		}
		out = append(out, fragment...)
	}
	return out, nil
}

// findArgument locates one argument fragment in the workload's argv, accepting both
// "--flag=value" and "--flag value" forms.
func findArgument(argv []string, argument plan.CopiedArgument) ([]string, error) {
	for i, token := range argv {
		for _, spelling := range argument.Spellings {
			if strings.HasPrefix(token, spelling+"=") {
				return []string{token}, nil
			}
			if token != spelling {
				continue
			}
			if !argument.TakesValue {
				return []string{token}, nil
			}
			if i+1 >= len(argv) {
				return nil, fmt.Errorf(
					"runtime: %s: the workload argument has no value, so it cannot be preserved",
					argument.Setting)
			}
			return []string{token, argv[i+1]}, nil
		}
	}
	return nil, fmt.Errorf(
		"runtime: %s: the workload argument could not be located in the container command, so the setting would be lost",
		argument.Setting)
}

// mergeEnv forwards the workload environment minus the entries the policy replaces
// or removes, then applies the plan's entries.
func mergeEnv(inherited, set []corev1.EnvVar, remove []string) []corev1.EnvVar {
	removed := make(map[string]struct{}, len(remove))
	for _, name := range remove {
		removed[name] = struct{}{}
	}
	replaced := make(map[string]struct{}, len(set))
	for _, entry := range set {
		replaced[entry.Name] = struct{}{}
	}

	out := make([]corev1.EnvVar, 0, len(inherited)+len(set))
	for _, entry := range inherited {
		if _, drop := removed[entry.Name]; drop {
			continue
		}
		if _, replace := replaced[entry.Name]; replace {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, set...)
	return out
}

// mergePorts keeps the workload's declared ports and adds the plan's, without
// duplicating a port number or a name.
func mergePorts(inherited, add []corev1.ContainerPort) []corev1.ContainerPort {
	out := append([]corev1.ContainerPort(nil), inherited...)
	for _, candidate := range add {
		conflict := false
		for _, existing := range out {
			if existing.ContainerPort == candidate.ContainerPort || existing.Name == candidate.Name {
				conflict = true
				break
			}
		}
		if !conflict {
			out = append(out, candidate)
		}
	}
	return out
}

// mergeResources applies the environment's sizing while keeping the workload's
// synthetic device allocation exactly as requested.
func mergeResources(sizing, original corev1.ResourceRequirements) corev1.ResourceRequirements {
	out := *sizing.DeepCopy()
	gpu := corev1.ResourceName(names.SyntheticResourceName)

	if quantity, ok := original.Requests[gpu]; ok {
		if out.Requests == nil {
			out.Requests = corev1.ResourceList{}
		}
		out.Requests[gpu] = quantity
	}
	if quantity, ok := original.Limits[gpu]; ok {
		if out.Limits == nil {
			out.Limits = corev1.ResourceList{}
		}
		out.Limits[gpu] = quantity
	}
	return out
}

// backendContainer builds the simulator container.
func backendContainer(admission *plan.Admission) (corev1.Container, error) {
	backend := admission.Backend
	if len(backend.Command) == 0 {
		return corev1.Container{}, fmt.Errorf("runtime: the admission plan carries no simulator command")
	}
	return corev1.Container{
		Name:            names.BackendContainerName,
		Image:           backend.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         append([]string(nil), backend.Command...),
		Args:            append([]string(nil), backend.Args...),
		Env:             append([]corev1.EnvVar(nil), backend.Env...),
		Ports:           append([]corev1.ContainerPort(nil), backend.Ports...),
		Resources:       *admission.Resources.Backend.DeepCopy(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: names.ProfileVolumeName, MountPath: names.ProfileMountPath, ReadOnly: true},
			{Name: tmpVolumeName, MountPath: tmpMountPath},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			RunAsNonRoot:             boolPtr(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}, nil
}

// provisionerContainer builds the profile provisioning init container.
func provisionerContainer(admission *plan.Admission) (corev1.Container, error) {
	provisioner := admission.Provisioner
	if len(provisioner.Command) == 0 {
		return corev1.Container{}, fmt.Errorf("runtime: the admission plan carries no profile provisioning command")
	}
	return corev1.Container{
		Name:            names.ProvisionerContainerName,
		Image:           provisioner.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         append([]string(nil), provisioner.Command...),
		Args:            append([]string(nil), provisioner.Args...),
		Env:             append([]corev1.EnvVar(nil), provisioner.Env...),
		Resources:       *admission.Resources.Provisioner.DeepCopy(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: names.ProfileVolumeName, MountPath: names.ProfileMountPath},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			RunAsNonRoot:             boolPtr(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}, nil
}

// addScratchVolume adds the simulator's writable scratch emptyDir.
func addScratchVolume(pod *corev1.Pod) error {
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == tmpVolumeName {
			return fmt.Errorf(
				"runtime: the Pod already defines a volume named %q, which the simulated runtime requires",
				tmpVolumeName)
		}
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name:         tmpVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	return nil
}

// addProfileVolume adds the shared profile volume.
func addProfileVolume(pod *corev1.Pod) error {
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == names.ProfileVolumeName {
			return fmt.Errorf(
				"runtime: the Pod already defines a volume named %q, which the profile provisioning stage requires",
				names.ProfileVolumeName)
		}
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: names.ProfileVolumeName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})
	return nil
}

// addProfileMount mounts the shared profile volume when the container does not
// already mount it.
func addProfileMount(mounts *[]corev1.VolumeMount, readOnly bool) {
	for _, mount := range *mounts {
		if mount.Name == names.ProfileVolumeName || mount.MountPath == names.ProfileMountPath {
			return
		}
	}
	*mounts = append(*mounts, corev1.VolumeMount{
		Name:      names.ProfileVolumeName,
		MountPath: names.ProfileMountPath,
		ReadOnly:  readOnly,
	})
}

// setProvenanceAnnotations records the applied plan
// (docs/contracts/admission.md). Every value is an identity or a quantity; no
// annotation ever carries a credential.
func setProvenanceAnnotations(pod *corev1.Pod, admission *plan.Admission, gpuQuantity resource.Quantity) {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	image := admission.Provenance.ProfileImage
	if image == "" {
		image = admission.Profile.PinnedImage()
	}
	profileID := admission.Provenance.ProfileID
	if profileID == "" {
		profileID = admission.Profile.ID
	}
	model := admission.Provenance.Model
	if model == "" {
		model = admission.Model.Reference()
	}
	pod.Annotations[names.InjectedAnnotation] = plan.RuntimeLayoutVersion
	pod.Annotations[names.ProfileAnnotation] = profileID
	pod.Annotations[names.ProfileImageAnnotation] = image
	pod.Annotations[names.ModelAnnotation] = model
	pod.Annotations[names.OriginalGPUsAnnotation] = gpuQuantity.String()
	pod.Annotations[names.VLLMVersionAnnotation] = admission.Provenance.VLLMVersion
}

func boolPtr(value bool) *bool { return &value }
