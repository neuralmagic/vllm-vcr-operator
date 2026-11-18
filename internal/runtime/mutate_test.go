package runtime_test

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/plan"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/runtime"
)

const (
	workloadContainerName = "modelserver"
	sidecarContainerName  = "routing-sidecar"
	apiKeyValue           = "top-secret-api-key"
	hfTokenValue          = "hf-secret-token"
)

func strPtr(value string) *string { return &value }

// testAdmission is the plan the translator produces for the contract workload.
func testAdmission() *plan.Admission {
	return &plan.Admission{
		Profile: plan.Profile{
			ID:     "qwen3-0.6b-h200-tp1-bf16",
			Image:  "quay.io/opendatahub/vllm-vcr-profiles:qwen3-0.6b-h200-tp1-bf16",
			Digest: "sha256:0123456789abcdef",
		},
		Model: plan.Model{
			Name:        "Qwen/Qwen3-0.6B",
			Revision:    "c9340e6f3",
			ServedNames: []string{"qwen3"},
		},
		GPUDevices: 1,
		Frontend: plan.Invocation{
			Image:   "quay.io/opendatahub/vllm-cpu:v0.33.0",
			Command: []string{names.SupervisorBinary, "--"},
			Args:    []string{"vllm", "serve", "Qwen/Qwen3-0.6B"},
			Env: []corev1.EnvVar{
				{Name: "VCR_HANDSHAKE_ADDRESS", Value: "tcp://127.0.0.1:29550"},
				{Name: "VCR_SHUTDOWN_TIMEOUT", Value: "30"},
			},
			RemoveEnv: []string{"CUDA_VISIBLE_DEVICES", "VLLM_TARGET_DEVICE"},
			Ports: []corev1.ContainerPort{
				{Name: "engine-handshake", ContainerPort: 29550, Protocol: corev1.ProtocolTCP},
			},
			CopiedArguments: []plan.CopiedArgument{
				{Setting: "--api-key", Spellings: []string{"--api-key"}, TakesValue: true},
			},
		},
		Backend: plan.Invocation{
			Image:   "ghcr.io/neuralmagic/vllm-vcr:vllm0.33",
			Command: []string{"vllm-vcr", "play", "--handshake-address", "tcp://127.0.0.1:29550"},
			Env:     []corev1.EnvVar{{Name: "RUST_LOG", Value: "info"}},
			Ports: []corev1.ContainerPort{
				{Name: "vcr-control", ContainerPort: 8001, Protocol: corev1.ProtocolTCP},
				{Name: "kv-events", ContainerPort: 5556, Protocol: corev1.ProtocolTCP},
			},
		},
		Provisioner: plan.Invocation{
			Image:   "quay.io/opendatahub/vllm-vcr-profiles@sha256:0123456789abcdef",
			Command: []string{"/usr/local/bin/vcr-profile-export", names.ProfileMountPath},
		},
		Resources: plan.Resources{
			Frontend: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
			Backend: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
			},
			Provisioner: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		},
		Placement: plan.Placement{
			NodeSelector: map[string]string{"vcr.opendatahub-io/simulation": "true"},
			Tolerations: []corev1.Toleration{
				{Key: "vcr.opendatahub-io/simulation", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
			},
			AcceleratorNodeLabels:        []string{names.SyntheticResourceName, "nvidia.com/gpu.product"},
			AcceleratorNodeLabelPrefixes: []string{"nvidia.com/gpu."},
			AcceleratorNodeValues:        []string{"h200"},
			AcceleratorTolerationKeys:    []string{names.SyntheticResourceName, "nvidia.com/gpu"},
			RuntimeClasses:               []string{"nvidia"},
		},
		PullSecrets: []string{"profile-pull"},
		Provenance: plan.Provenance{
			ProfileID:    "qwen3-0.6b-h200-tp1-bf16",
			ProfileImage: "quay.io/opendatahub/vllm-vcr-profiles@sha256:0123456789abcdef",
			Model:        "Qwen/Qwen3-0.6B@c9340e6f3",
			VLLMVersion:  "0.33.0",
		},
		Warnings: []string{"content replay: recorded token ids are served"},
	}
}

// workloadPod is an unmodified GPU-targeted llm-d model server.
func workloadPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "qwen3",
			Namespace: "workload",
			Labels: map[string]string{
				"llm-d.ai/inference-serving": "true",
				"llm-d.ai/model":             "qwen3",
			},
			Annotations: map[string]string{"workload.example.com/owner": "team-a"},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: "qwen-sa",
			RuntimeClassName:   strPtr("nvidia"),
			NodeSelector: map[string]string{
				"nvidia.com/gpu.product": "H200",
				"kubernetes.io/os":       "linux",
				"storage":                "fast",
			},
			Tolerations: []corev1.Toleration{
				{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
				{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "models", Effect: corev1.TaintEffectNoSchedule},
			},
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{Key: "nvidia.com/gpu.product", Operator: corev1.NodeSelectorOpIn, Values: []string{"H200"}},
							},
						}},
					},
				},
			},
			Volumes: []corev1.Volume{
				{Name: "dev-shm", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
				{Name: "models", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/models"}}},
				{Name: "device-plugins", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: names.DevicePluginDir}}},
				{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "serving-tls"}}},
			},
			InitContainers: []corev1.Container{{
				Name:    "wait-for-cache",
				Image:   "busybox:1.36",
				Command: []string{"sh", "-c", "until [ -d /models/qwen ]; do sleep 1; done"},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "models", MountPath: "/models"},
				},
			}},
			Containers: []corev1.Container{
				{
					Name:    workloadContainerName,
					Image:   "vllm/vllm-openai:v0.33.0",
					Command: []string{"vllm", "serve"},
					Args:    []string{"Qwen/Qwen3-0.6B", "--port", "8000", "--api-key", apiKeyValue},
					Env: []corev1.EnvVar{
						{Name: "HF_HUB_CACHE", Value: "/models"},
						{Name: "HF_TOKEN", Value: hfTokenValue},
						{Name: "CUDA_VISIBLE_DEVICES", Value: "0"},
						{Name: "VLLM_TARGET_DEVICE", Value: "cuda"},
						{Name: "VLLM_LOG_STATS_INTERVAL", Value: "5"},
					},
					Ports: []corev1.ContainerPort{{Name: "modelserver", ContainerPort: 8000, Protocol: corev1.ProtocolTCP}},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{Path: "/v1/models", Port: intstr.FromString("modelserver")},
						},
						PeriodSeconds: 5,
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:          resource.MustParse("8"),
							corev1.ResourceMemory:       resource.MustParse("64Gi"),
							names.SyntheticResourceName: resource.MustParse("1"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:          resource.MustParse("16"),
							corev1.ResourceMemory:       resource.MustParse("96Gi"),
							names.SyntheticResourceName: resource.MustParse("1"),
						},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "dev-shm", MountPath: "/dev/shm"},
						{Name: "models", MountPath: "/models"},
						{Name: "device-plugins", MountPath: names.DevicePluginDir},
						{Name: "tls", MountPath: "/etc/tls", ReadOnly: true},
					},
				},
				{
					Name:  sidecarContainerName,
					Image: "ghcr.io/llm-d/llm-d-routing-sidecar:v0.8.0",
					Ports: []corev1.ContainerPort{{Name: "proxy", ContainerPort: 8000, Protocol: corev1.ProtocolTCP}},
				},
			},
		},
	}
}

func mutate(t *testing.T, pod *corev1.Pod, admission *plan.Admission) *corev1.Pod {
	t.Helper()
	out, err := runtime.Mutate(pod, admission)
	if err != nil {
		t.Fatalf("Mutate() failed: %v", err)
	}
	return out
}

func containerByName(t *testing.T, containers []corev1.Container, name string) *corev1.Container {
	t.Helper()
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	t.Fatalf("container %q is missing from %v", name, containerNames(containers))
	return nil
}

func containerNames(containers []corev1.Container) []string {
	names := make([]string, len(containers))
	for i, container := range containers {
		names[i] = container.Name
	}
	return names
}

// quantityString renders a resource quantity.
func quantityString(quantity resource.Quantity) string {
	return quantity.String()
}

func envByName(env []corev1.EnvVar, name string) (string, bool) {
	for _, entry := range env {
		if entry.Name == name {
			return entry.Value, true
		}
	}
	return "", false
}

// TestMutateAppliesTheRuntimeLayout asserts the injected layout and the preserved
// workload identity in one view of the mutated Pod.
func TestMutateAppliesTheRuntimeLayout(t *testing.T) {
	pod := workloadPod()
	out := mutate(t, pod, testAdmission())

	// Containers and init containers.
	if got, want := containerNames(out.Spec.Containers), []string{workloadContainerName, sidecarContainerName, names.BackendContainerName}; !reflect.DeepEqual(got, want) {
		t.Fatalf("containers = %v, want %v", got, want)
	}
	if got, want := containerNames(out.Spec.InitContainers), []string{"wait-for-cache", names.ProvisionerContainerName}; !reflect.DeepEqual(got, want) {
		t.Fatalf("init containers = %v, want %v", got, want)
	}

	// The workload's container identity, ports, probes, and labels survive.
	frontend := containerByName(t, out.Spec.Containers, workloadContainerName)
	if got, want := frontend.Image, "quay.io/opendatahub/vllm-cpu:v0.33.0"; got != want {
		t.Errorf("frontend image = %q, want %q", got, want)
	}
	if len(frontend.Ports) != 2 || frontend.Ports[0].Name != "modelserver" || frontend.Ports[1].Name != "engine-handshake" {
		t.Errorf("frontend ports = %v, want the preserved inference port and the handshake port", frontend.Ports)
	}
	if frontend.ReadinessProbe == nil || frontend.ReadinessProbe.HTTPGet.Path != "/v1/models" {
		t.Errorf("readiness probe = %v, want the preserved /v1/models probe", frontend.ReadinessProbe)
	}
	if out.Labels["llm-d.ai/inference-serving"] != "true" {
		t.Errorf("labels = %v, want the llm-d discovery labels preserved", out.Labels)
	}
	if out.Spec.ServiceAccountName != "qwen-sa" {
		t.Errorf("service account = %q, want the workload identity preserved", out.Spec.ServiceAccountName)
	}

	// TLS material stays mounted at its original path.
	requireMount(t, frontend.VolumeMounts, "tls", "/etc/tls")

	// The synthetic allocation is preserved exactly once, and never injected.
	gpu := corev1.ResourceName(names.SyntheticResourceName)
	if got, want := quantityString(frontend.Resources.Requests[gpu]), "1"; got != want {
		t.Errorf("frontend gpu request = %q, want %q", got, want)
	}
	if got, want := quantityString(frontend.Resources.Limits[gpu]), "1"; got != want {
		t.Errorf("frontend gpu limit = %q, want %q", got, want)
	}
	count := 0
	for _, container := range append(append([]corev1.Container{}, out.Spec.Containers...), out.Spec.InitContainers...) {
		if _, ok := container.Resources.Requests[gpu]; ok {
			count++
		}
	}
	if count != 1 {
		t.Errorf("%d containers request the synthetic device, want exactly 1", count)
	}

	// The environment sizing replaces the GPU-server sizing for CPU and memory.
	if got, want := quantityString(frontend.Resources.Requests[corev1.ResourceCPU]), "2"; got != want {
		t.Errorf("frontend cpu request = %q, want %q", got, want)
	}
	backend := containerByName(t, out.Spec.Containers, names.BackendContainerName)
	if got, want := quantityString(backend.Resources.Requests[corev1.ResourceMemory]), "2Gi"; got != want {
		t.Errorf("backend memory request = %q, want %q", got, want)
	}
	if got, want := backend.Image, "ghcr.io/neuralmagic/vllm-vcr:vllm0.33"; got != want {
		t.Errorf("backend image = %q, want %q", got, want)
	}
	requireMount(t, backend.VolumeMounts, names.ProfileVolumeName, names.ProfileMountPath)

	provisioner := containerByName(t, out.Spec.InitContainers, names.ProvisionerContainerName)
	if got, want := provisioner.Image, "quay.io/opendatahub/vllm-vcr-profiles@sha256:0123456789abcdef"; got != want {
		t.Errorf("provisioner image = %q, want the digest-pinned profile image %q", got, want)
	}
	if got, want := provisioner.Command, []string{"/usr/local/bin/vcr-profile-export", names.ProfileMountPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("provisioner command = %v, want %v", got, want)
	}

	// The shared profile volume exists exactly once.
	profileVolumes := 0
	for _, volume := range out.Spec.Volumes {
		if volume.Name == names.ProfileVolumeName {
			profileVolumes++
			if volume.EmptyDir == nil {
				t.Errorf("profile volume = %v, want an emptyDir", volume.VolumeSource)
			}
		}
	}
	if profileVolumes != 1 {
		t.Errorf("%d profile volumes, want exactly 1", profileVolumes)
	}
}

func requireMount(t *testing.T, mounts []corev1.VolumeMount, volume, path string) {
	t.Helper()
	for _, mount := range mounts {
		if mount.Name == volume && mount.MountPath == path {
			return
		}
	}
	t.Fatalf("mounts %v do not contain %s at %s", mounts, volume, path)
}

// TestMutateReplacesTheEntrypointWithTheSupervisor asserts the frontend process runs
// under the supervisor with the plan's arguments and a policy-filtered environment.
func TestMutateReplacesTheEntrypointWithTheSupervisor(t *testing.T) {
	out := mutate(t, workloadPod(), testAdmission())
	frontend := containerByName(t, out.Spec.Containers, workloadContainerName)

	if got, want := frontend.Command, []string{names.SupervisorBinary, "--"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("command = %v, want %v", got, want)
	}
	wantArgs := []string{"vllm", "serve", "Qwen/Qwen3-0.6B", "--api-key", apiKeyValue}
	if got := frontend.Args; !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("args = %v, want %v", got, wantArgs)
	}

	// Preserved environment stays; replaced and accelerator-only entries do not.
	if got, ok := envByName(frontend.Env, "HF_HUB_CACHE"); !ok || got != "/models" {
		t.Errorf("HF_HUB_CACHE = %q (present %t), want it preserved", got, ok)
	}
	if got, ok := envByName(frontend.Env, "HF_TOKEN"); !ok || got != hfTokenValue {
		t.Errorf("HF_TOKEN is not preserved from the workload container")
	}
	for _, removed := range []string{"CUDA_VISIBLE_DEVICES", "VLLM_TARGET_DEVICE"} {
		if _, ok := envByName(frontend.Env, removed); ok {
			t.Errorf("%s is still present, want it removed by policy", removed)
		}
	}
	if got, want := mustEnv(t, frontend.Env, "VCR_HANDSHAKE_ADDRESS"), "tcp://127.0.0.1:29550"; got != want {
		t.Errorf("VCR_HANDSHAKE_ADDRESS = %q, want %q", got, want)
	}
}

func mustEnv(t *testing.T, env []corev1.EnvVar, name string) string {
	t.Helper()
	value, ok := envByName(env, name)
	if !ok {
		t.Fatalf("environment variable %q is missing from %v", name, env)
	}
	return value
}

// TestMutateRewritesPlacement asserts accelerator placement is replaced by the
// environment's simulation-node selection while other constraints survive.
func TestMutateRewritesPlacement(t *testing.T) {
	out := mutate(t, workloadPod(), testAdmission())

	if out.Spec.RuntimeClassName != nil {
		t.Errorf("runtime class = %q, want the NVIDIA runtime class removed", *out.Spec.RuntimeClassName)
	}
	wantSelector := map[string]string{
		"kubernetes.io/os":              "linux",
		"storage":                       "fast",
		"vcr.opendatahub-io/simulation": "true",
	}
	if !reflect.DeepEqual(out.Spec.NodeSelector, wantSelector) {
		t.Errorf("node selector = %v, want %v", out.Spec.NodeSelector, wantSelector)
	}
	if out.Spec.Affinity != nil && out.Spec.Affinity.NodeAffinity != nil &&
		out.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		t.Errorf("required node affinity = %v, want the accelerator-only requirement removed",
			out.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution)
	}

	var keys []string
	for _, toleration := range out.Spec.Tolerations {
		keys = append(keys, toleration.Key)
	}
	if !reflect.DeepEqual(keys, []string{"dedicated", "vcr.opendatahub-io/simulation"}) {
		t.Errorf("toleration keys = %v, want the workload toleration and the environment toleration", keys)
	}
}

// TestMutateKeepsNonAcceleratorAffinity asserts a storage or zone constraint is
// preserved, because simulation does not remove what it can still honor.
func TestMutateKeepsNonAcceleratorAffinity(t *testing.T) {
	pod := workloadPod()
	pod.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{
						{Key: "topology.kubernetes.io/zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"zone-a"}},
						{Key: "nvidia.com/gpu.product", Operator: corev1.NodeSelectorOpIn, Values: []string{"H200"}},
					},
				}},
			},
		},
	}
	out := mutate(t, pod, testAdmission())

	required := out.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if required == nil {
		t.Fatal("the non-accelerator affinity requirement was removed")
	}
	expressions := required.NodeSelectorTerms[0].MatchExpressions
	if len(expressions) != 1 || expressions[0].Key != "topology.kubernetes.io/zone" {
		t.Fatalf("match expressions = %v, want only the zone requirement", expressions)
	}
}

// TestMutateDropsAffinityThatOnlyDescribedTheGPUType asserts an affinity whose only
// value names the simulated GPU type is accelerator-specific.
func TestMutateDropsAffinityThatOnlyDescribedTheGPUType(t *testing.T) {
	pod := workloadPod()
	pod.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{
						{Key: "node.kubernetes.io/instance-type", Operator: corev1.NodeSelectorOpIn, Values: []string{"H200"}},
					},
				}},
			},
		},
	}
	out := mutate(t, pod, testAdmission())
	if out.Spec.Affinity != nil {
		t.Errorf("affinity = %v, want it removed: it only described the accelerator instance type", out.Spec.Affinity)
	}
}

// TestMutateRejectsContradictoryNodeSelection asserts an unsupported combination is
// reported instead of silently overriding the workload's constraint.
func TestMutateRejectsContradictoryNodeSelection(t *testing.T) {
	admission := testAdmission()
	admission.Placement.NodeSelector = map[string]string{"storage": "slow"}
	pod := workloadPod()

	_, err := runtime.Mutate(pod, admission)
	if err == nil {
		t.Fatal("Mutate() accepted contradictory node selection")
	}
	for _, want := range []string{"storage", "fast", "slow"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), apiKeyValue) || strings.Contains(err.Error(), hfTokenValue) {
		t.Errorf("error %q contains a credential value", err)
	}
}

// TestMutateRemovesAcceleratorDeviceVolumes asserts the real device-plugin host
// directory is replaced by the synthetic one, while unrelated host mounts survive.
func TestMutateRemovesAcceleratorDeviceVolumes(t *testing.T) {
	out := mutate(t, workloadPod(), testAdmission())

	volumeNames := make([]string, 0, len(out.Spec.Volumes))
	for _, volume := range out.Spec.Volumes {
		volumeNames = append(volumeNames, volume.Name)
	}
	for _, removed := range []string{"device-plugins"} {
		for _, name := range volumeNames {
			if name == removed {
				t.Errorf("volume %q is still present, want it removed", removed)
			}
		}
	}
	for _, kept := range []string{"dev-shm", "models", "tls", names.ProfileVolumeName} {
		found := false
		for _, name := range volumeNames {
			if name == kept {
				found = true
			}
		}
		if !found {
			t.Errorf("volume %q is missing from %v", kept, volumeNames)
		}
	}

	frontend := containerByName(t, out.Spec.Containers, workloadContainerName)
	for _, mount := range frontend.VolumeMounts {
		if mount.Name == "device-plugins" {
			t.Errorf("frontend still mounts the removed device-plugin volume")
		}
	}
	requireMount(t, frontend.VolumeMounts, "dev-shm", "/dev/shm")
}

// TestMutateIsIdempotent asserts a repeated application changes nothing and that the
// input Pod is never modified.
func TestMutateIsIdempotent(t *testing.T) {
	pod := workloadPod()
	original := pod.DeepCopy()

	first := mutate(t, pod, testAdmission())
	second := mutate(t, first, testAdmission())

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("second mutation changed the Pod:\nfirst  %v\nsecond %v", first, second)
	}
	if !reflect.DeepEqual(pod, original) {
		t.Fatal("Mutate() modified the submitted Pod instead of returning a copy")
	}
	if _, ok := envByName(pod.Spec.Containers[0].Env, "CUDA_VISIBLE_DEVICES"); !ok {
		t.Fatal("Mutate() stripped the input Pod's environment")
	}
}

// TestMutateRecordsProvenance asserts the annotation contract.
func TestMutateRecordsProvenance(t *testing.T) {
	out := mutate(t, workloadPod(), testAdmission())

	want := map[string]string{
		names.InjectedAnnotation:     plan.RuntimeLayoutVersion,
		names.ProfileAnnotation:      "qwen3-0.6b-h200-tp1-bf16",
		names.ProfileImageAnnotation: "quay.io/opendatahub/vllm-vcr-profiles@sha256:0123456789abcdef",
		names.ModelAnnotation:        "Qwen/Qwen3-0.6B@c9340e6f3",
		names.OriginalGPUsAnnotation: "1",
		names.VLLMVersionAnnotation:  "0.33.0",
	}
	for annotation, value := range want {
		if got := out.Annotations[annotation]; got != value {
			t.Errorf("annotation %s = %q, want %q", annotation, got, value)
		}
	}
	if got := out.Annotations["workload.example.com/owner"]; got != "team-a" {
		t.Errorf("the workload's own annotation was lost: %q", got)
	}
}

// TestMutateNeverWritesCredentials asserts no credential value reaches an
// annotation, a label, or an environment variable the operator adds.
func TestMutateNeverWritesCredentials(t *testing.T) {
	out := mutate(t, workloadPod(), testAdmission())

	for key, value := range out.Annotations {
		if strings.Contains(value, apiKeyValue) || strings.Contains(value, hfTokenValue) {
			t.Errorf("annotation %s contains a credential value", key)
		}
	}
	for key, value := range out.Labels {
		if strings.Contains(value, apiKeyValue) || strings.Contains(value, hfTokenValue) {
			t.Errorf("label %s contains a credential value", key)
		}
	}
	warnings := strings.Join(testAdmission().Warnings, " ")
	if strings.Contains(warnings, apiKeyValue) || strings.Contains(warnings, hfTokenValue) {
		t.Error("a plan warning contains a credential value")
	}
}

// TestMutateRejectsUnsupportedPods covers the Pod shapes that cannot be translated
// as a whole.
func TestMutateRejectsUnsupportedPods(t *testing.T) {
	t.Run("no device request", func(t *testing.T) {
		pod := workloadPod()
		pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
		if _, err := runtime.Mutate(pod, testAdmission()); err == nil {
			t.Fatal("Mutate() accepted a Pod that requests no synthetic device")
		}
	})
	t.Run("two device consumers", func(t *testing.T) {
		pod := workloadPod()
		pod.Spec.Containers[1].Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{names.SyntheticResourceName: resource.MustParse("1")},
		}
		_, err := runtime.Mutate(pod, testAdmission())
		if err == nil {
			t.Fatal("Mutate() accepted two containers requesting the synthetic device")
		}
		if !strings.Contains(err.Error(), sidecarContainerName) {
			t.Errorf("error %q does not name the second consumer", err)
		}
	})
	t.Run("unrecognized gpu init container", func(t *testing.T) {
		pod := workloadPod()
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
			Name:    "compile-kernels",
			Image:   "builder:1",
			Command: []string{"/usr/bin/nvcc", "kernel.cu"},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{names.SyntheticResourceName: resource.MustParse("1")},
			},
		})
		_, err := runtime.Mutate(pod, testAdmission())
		if err == nil {
			t.Fatal("Mutate() accepted a GPU-dependent init container it cannot translate")
		}
		if !strings.Contains(err.Error(), "compile-kernels") {
			t.Errorf("error %q does not name the init container", err)
		}
	})
	t.Run("missing copied argument", func(t *testing.T) {
		pod := workloadPod()
		pod.Spec.Containers[0].Args = []string{"Qwen/Qwen3-0.6B"}
		_, err := runtime.Mutate(pod, testAdmission())
		if err == nil {
			t.Fatal("Mutate() accepted a plan whose credential argument is missing from the Pod")
		}
		if !strings.Contains(err.Error(), "--api-key") {
			t.Errorf("error %q does not name the argument", err)
		}
	})
	t.Run("nil plan", func(t *testing.T) {
		if _, err := runtime.Mutate(workloadPod(), nil); err == nil {
			t.Fatal("Mutate() accepted a nil plan")
		}
	})
	t.Run("nil pod", func(t *testing.T) {
		if _, err := runtime.Mutate(nil, testAdmission()); err == nil {
			t.Fatal("Mutate() accepted a nil Pod")
		}
	})
}

// TestMutatePreservesRecognizedWeightDownloaderInitContainer asserts a GPU-dependent
// artifact fetcher is kept, never silently dropped.
func TestMutatePreservesRecognizedWeightDownloaderInitContainer(t *testing.T) {
	pod := workloadPod()
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
		Name:    "model-download",
		Image:   "python:3.12-slim",
		Command: []string{"huggingface-cli", "download", "Qwen/Qwen3-0.6B"},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{names.SyntheticResourceName: resource.MustParse("1")},
		},
	})
	out := mutate(t, pod, testAdmission())

	found := false
	for _, container := range out.Spec.InitContainers {
		if container.Name == "model-download" {
			found = true
		}
	}
	if !found {
		t.Fatalf("init containers = %v, want the weight downloader preserved", containerNames(out.Spec.InitContainers))
	}
}
