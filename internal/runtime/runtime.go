// Package runtime applies an admission plan to a submitted Pod.
//
// The package is a pure function over typed Kubernetes objects: it holds no
// client, performs no I/O, and never invents a decision. internal/translator
// decided which profile to pin, how to configure the frontend and the simulator,
// what resources the injected runtime receives, and which placement constraints are
// accelerator-specific; this package applies exactly that and nothing more.
//
// The layout it produces is documented in docs/contracts/runtime-layout.md: the
// workload's own vLLM container keeps its inference port, mounts, and GPU
// allocation, but takes the layout's container identity (names.FrontendContainerName),
// runs the supervisor instead of the engine, and is probed through that supervisor;
// the simulator and the profile provisioner are injected; a shared profile volume
// holds the exported profile data. Mutation is idempotent, so applying it twice is
// a no-op, and it never writes credential material anywhere.
package runtime

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/plan"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/supervisor"
)

// tmpVolumeName and tmpMountPath give the simulator a writable scratch directory
// while its root filesystem stays read-only. The simulator resolves remote trace
// references by downloading them into a temporary directory; the operator always
// points it at the exported profile content, so the mount exists for the process's
// other scratch needs rather than for trace fetching.
const (
	tmpVolumeName = "vcr-tmp"
	tmpMountPath  = "/tmp"

	// supervisorHealthPortName is the container port name of the supervisor's
	// liveness and readiness endpoint, so the probes are visible in the Pod's
	// port list.
	supervisorHealthPortName = "sup-health"

	// controlStatsPath is the simulator control API surface used as its readiness
	// probe (vllm-vcr docs/control-api.md: GET /stats).
	controlStatsPath = "/stats"
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

	if AlreadyInjected(pod) {
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

// AlreadyInjected reports whether this operator already mutated the Pod. The
// injected annotation alone is not enough: the annotation and the injected
// containers together identify a complete layout.
//
// It is exported because admission must apply exactly the same rule before it does
// any work: two definitions of "what an injected Pod looks like" would drift, and a
// Pod recognized by one and not the other would be translated twice.
func AlreadyInjected(pod *corev1.Pod) bool {
	if pod == nil || pod.Annotations[names.InjectedAnnotation] == "" {
		return false
	}
	return HasContainer(pod.Spec.Containers, names.BackendContainerName) &&
		HasContainer(pod.Spec.InitContainers, names.ProvisionerContainerName)
}

// HasContainer reports whether the named container is present. It is exported for
// the same reason as AlreadyInjected.
func HasContainer(containers []corev1.Container, name string) bool {
	for _, container := range containers {
		if container.Name == name {
			return true
		}
	}
	return false
}

// RequestsSyntheticDevice reports whether any container or init container requests
// or limits the synthetic device. It is the applicability rule of
// docs/contracts/admission.md, shared by admission and mutation.
func RequestsSyntheticDevice(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	return len(SyntheticDeviceOwners(pod.Spec.Containers)) > 0 ||
		len(SyntheticDeviceOwners(pod.Spec.InitContainers)) > 0
}

// SyntheticDeviceOwners returns the indexes of the containers that request or limit
// the synthetic device, in order.
//
// Exactly one entry authorizes translation: it is the container whose entrypoint is
// replaced and whose allocation is preserved. Callers that need to reject a Pod with
// several owners use the indexes to name them.
func SyntheticDeviceOwners(containers []corev1.Container) []int {
	var owners []int
	for i := range containers {
		if _, ok := SyntheticDeviceQuantity(containers[i].Resources); ok {
			owners = append(owners, i)
		}
	}
	return owners
}

// SyntheticDeviceQuantity returns a container's synthetic device allocation.
func SyntheticDeviceQuantity(resources corev1.ResourceRequirements) (resource.Quantity, bool) {
	return gpuQuantityOf(resources)
}

// findWorkloadContainer returns the index of the container whose GPU allocation is
// preserved, together with that allocation.
//
// Exactly one container may request the synthetic resource: it is the container
// whose entrypoint is replaced, and a Pod with two GPU consumers cannot be
// translated without deciding which one owns the allocation.
func findWorkloadContainer(pod *corev1.Pod) (int, resource.Quantity, error) {
	owners := SyntheticDeviceOwners(pod.Spec.Containers)
	if len(owners) == 0 {
		return 0, resource.Quantity{}, fmt.Errorf(
			"runtime: no container requests %s: the Pod is not applicable and must not be mutated",
			names.SyntheticResourceName)
	}
	quantity, _ := SyntheticDeviceQuantity(pod.Spec.Containers[owners[0]].Resources)
	if len(owners) > 1 {
		described := make([]string, 0, len(owners))
		for _, index := range owners {
			described = append(described, pod.Spec.Containers[index].Name)
		}
		return 0, quantity, fmt.Errorf(
			"runtime: containers %s all request %s: exactly one workload container may own the synthetic allocation",
			strings.Join(described, ", "), names.SyntheticResourceName)
	}
	return owners[0], quantity, nil
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

// checkInitContainers rejects any init container that requests or limits the
// synthetic device.
//
// A GPU-dependent setup step cannot be translated safely as a whole: the operator
// replaces the GPU engine with a simulator, and an init container that asks for a
// device would either hold the allocation the workload container must own
// (docs/contracts/admission.md: exactly one container requests the original
// quantity) or run accelerator code on a node that has none. Guessing from a
// container's name or command which setup steps are really GPU-free is exactly the
// kind of approximation the policy rejects, so this release refuses them all and
// says so by name. An init container that requests no device is preserved
// unchanged.
func checkInitContainers(pod *corev1.Pod) error {
	for _, container := range pod.Spec.InitContainers {
		if _, ok := gpuQuantityOf(container.Resources); !ok {
			continue
		}
		return fmt.Errorf(
			"runtime: init container %q requests %s: GPU-dependent init containers are not translated by this release, so the Pod cannot be simulated as a whole; remove the %s request or run the setup step outside the Pod",
			container.Name, names.SyntheticResourceName, names.SyntheticResourceName)
	}
	return nil
}

// rewriteFrontendContainer turns the workload's vLLM container into the frontend
// container: same identity, ports, and mounts; a different entrypoint, image,
// environment, sizing, and probes.
func rewriteFrontendContainer(container, original *corev1.Container, admission *plan.Admission) error {
	frontend := admission.Frontend
	if len(frontend.Command) == 0 {
		return fmt.Errorf("runtime: the admission plan carries no frontend command for container %q", container.Name)
	}

	copied, err := copiedArguments(*original, frontend.CopiedArguments)
	if err != nil {
		return err
	}

	// The injected frontend container carries the layout's documented identity
	// (docs/contracts/runtime-layout.md, names.FrontendContainerName); everything
	// else about the workload container is preserved.
	container.Name = names.FrontendContainerName
	container.Image = frontend.Image
	container.ImagePullPolicy = corev1.PullIfNotPresent
	container.Command = append([]string(nil), frontend.Command...)
	container.Args = append(append([]string(nil), frontend.Args...), copied...)
	container.Env = mergeEnv(original.Env, frontend.Env, frontend.RemoveEnv)
	container.Ports = mergePorts(original.Ports, frontend.Ports)
	container.Ports = mergePorts(container.Ports, []corev1.ContainerPort{{
		Name:          supervisorHealthPortName,
		ContainerPort: supervisor.DefaultHealthPort,
		Protocol:      corev1.ProtocolTCP,
	}})
	container.Resources = mergeResources(admission.Resources.Frontend, original.Resources)
	addProfileMount(&container.VolumeMounts, true)
	if err := checkReadinessInputs(container.Env); err != nil {
		return err
	}
	applyFrontendProbes(container)
	return nil
}

// readinessInputEnvVars are the environment entries that tell the supervisor which
// endpoints decide readiness. The translator derives them from the workload's
// inference port and names.ControlPort; this package only checks that the plan
// carries them, because the supervisor would otherwise fall back to the default
// ports and report a non-default inference port as unhealthy forever.
var readinessInputEnvVars = []string{"VCR_FRONTEND_HEALTH_URL", "VCR_BACKEND_HEALTH_URL"}

// checkReadinessInputs rejects a plan that cannot configure the supervisor's
// readiness inputs.
func checkReadinessInputs(env []corev1.EnvVar) error {
	present := make(map[string]struct{}, len(env))
	for _, entry := range env {
		present[entry.Name] = struct{}{}
	}
	for _, name := range readinessInputEnvVars {
		if _, ok := present[name]; !ok {
			return fmt.Errorf(
				"runtime: the admission plan sets no %s for the frontend container, so the supervisor would probe default ports instead of the workload's",
				name)
		}
	}
	return nil
}

// applyFrontendProbes points the container's probes at the supervisor instead of
// the inference port.
//
// The workload's own readiness probe only observes the frontend process: in
// external-engine mode the server answers it while the simulator is gone, so it
// would keep an engine-less server Ready. The supervisor's /readyz combines the
// frontend process, the frontend's health endpoint, and the simulator control API,
// which is exactly the contract's readiness definition.
func applyFrontendProbes(container *corev1.Container) {
	container.LivenessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path:   supervisor.HealthPath,
			Port:   intstr.FromInt32(supervisor.DefaultHealthPort),
			Scheme: corev1.URISchemeHTTP,
		}},
		TimeoutSeconds:   2,
		PeriodSeconds:    10,
		FailureThreshold: 3,
	}
	container.ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path:   supervisor.ReadyPath,
			Port:   intstr.FromInt32(supervisor.DefaultHealthPort),
			Scheme: corev1.URISchemeHTTP,
		}},
		// A single failed evaluation withdraws readiness: the supervisor has
		// already decided, and a Pod must not stay Ready with an unavailable
		// backend.
		TimeoutSeconds:   2,
		PeriodSeconds:    5,
		FailureThreshold: 1,
	}
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

// backendStartupWaitSeconds bounds how long the simulator waits for the frontend's
// engine-core handshake to exist before it starts anyway. A real Python frontend
// needs tens of seconds to import torch, resolve the model config and fetch
// tokenizer files; vllm-vcr gives up after 30s of dialing and exits, which would
// cost a container restart on every cold start. Waiting here is a cold-start
// ordering fix only: it does not assume a live session can be re-established,
// and the supervisor still treats a lost simulator as a container restart.
const backendStartupWaitSeconds = 300

// backendCommand returns the simulator's command and arguments, prefixed with a
// bounded wait for the handshake socket. The wait uses bash's /dev/tcp, which the
// pinned vllm-vcr image provides (it ships bash; it has no nc).
func backendCommand(admission *plan.Admission) ([]string, []string) {
	backend := admission.Backend
	argv := append(append([]string(nil), backend.Command...), backend.Args...)
	address := handshakeAddress(backend.Args)
	script := fmt.Sprintf(
		"for i in $(seq 1 %d); do (exec 3<>/dev/tcp/%s) 2>/dev/null && break; sleep 1; done; exec %s",
		backendStartupWaitSeconds, address, shellQuoteAll(argv))
	return []string{"/bin/bash", "-c"}, []string{script}
}

// handshakeAddress extracts host:port from the plan's --handshake-address value,
// defaulting to loopback on the contract port.
func handshakeAddress(args []string) string {
	for i, arg := range args {
		value := ""
		switch {
		case arg == "--handshake-address" && i+1 < len(args):
			value = args[i+1]
		case strings.HasPrefix(arg, "--handshake-address="):
			value = strings.TrimPrefix(arg, "--handshake-address=")
		}
		if value == "" {
			continue
		}
		if _, rest, ok := strings.Cut(value, "://"); ok {
			value = rest
		}
		if host, port, ok := strings.Cut(value, ":"); ok && host != "" && port != "" {
			return host + ":" + port
		}
	}
	return fmt.Sprintf("127.0.0.1:%d", names.HandshakePort)
}

// shellQuoteAll renders argv for a bash -c script, quoting every word so that
// flags carrying spaces or quotes survive.
func shellQuoteAll(argv []string) string {
	quoted := make([]string, len(argv))
	for i, word := range argv {
		quoted[i] = "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// backendContainer builds the simulator container.
func backendContainer(admission *plan.Admission) (corev1.Container, error) {
	backend := admission.Backend
	if len(backend.Command) == 0 {
		return corev1.Container{}, fmt.Errorf("runtime: the admission plan carries no simulator command")
	}
	command, args := backendCommand(admission)
	return corev1.Container{
		Name:            names.BackendContainerName,
		Image:           backend.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         command,
		Args:            args,
		Env:             append([]corev1.EnvVar(nil), backend.Env...),
		Ports:           append([]corev1.ContainerPort(nil), backend.Ports...),
		Resources:       *admission.Resources.Backend.DeepCopy(),
		// Readiness is the control API, which is also the supervisor's backend
		// readiness input. Liveness is a socket check on the same port: a dial
		// proves the simulator is running, which is all liveness ever claims.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path:   controlStatsPath,
				Port:   intstr.FromInt32(names.ControlPort),
				Scheme: corev1.URISchemeHTTP,
			}},
			TimeoutSeconds:   2,
			PeriodSeconds:    5,
			FailureThreshold: 2,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromInt32(names.ControlPort),
			}},
			TimeoutSeconds:   2,
			PeriodSeconds:    10,
			FailureThreshold: 3,
		},
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
