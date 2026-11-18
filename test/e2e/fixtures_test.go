package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/plan"
)

// resourceQuantity parses a resource quantity or fails the test.
func resourceQuantity(value string) resource.Quantity {
	return resource.MustParse(value)
}

// modelserverDeployment loads the checked-in, unchanged llm-d modelserver
// fixture (test/fixtures/llmd/mock-prefix-cache-deployment.yaml) into the given
// namespace.
//
// Using the fixture verbatim is the point: the operator's primary acceptance
// interface is a real GPU-requesting llm-d workload that carries no simulator
// opt-in at all, so admission has to trigger on the GPU request alone.
func (h *harness) modelserverDeployment(t *testing.T, namespace string) *appsv1.Deployment {
	t.Helper()
	path := filepath.Join(h.root, "test", "fixtures", "llmd", "mock-prefix-cache-deployment.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read modelserver fixture %s: %v", path, err)
	}
	deployment := &appsv1.Deployment{}
	if err := yaml.UnmarshalStrict(raw, deployment); err != nil {
		t.Fatalf("decode modelserver fixture %s: %v", path, err)
	}
	deployment.Namespace = namespace
	return deployment
}

// podFromTemplate renders the Pod a ReplicaSet controller would create from the
// deployment's template: the template's labels and annotations, a generated
// name, and a controller owner reference to the Deployment.
//
// envtest runs no ReplicaSet controller, so the suite submits this Pod itself;
// admission sees exactly what it sees for a controller-created Pod (generateName
// resolution, template metadata, owner reference).
func podFromTemplate(deployment *appsv1.Deployment) *corev1.Pod {
	template := deployment.Spec.Template
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: deployment.Name + "-",
			Namespace:    deployment.Namespace,
			Labels:       copyStringMap(template.Labels),
			Annotations:  copyStringMap(template.Annotations),
		},
		Spec: *template.Spec.DeepCopy(),
	}
	// A Deployment that was never submitted has no UID, and the API server
	// rejects an owner reference with an empty UID. Only mirror the controller
	// owner when the object really exists.
	if deployment.UID != "" {
		pod.OwnerReferences = []metav1.OwnerReference{{
			APIVersion:         "apps/v1",
			Kind:               "Deployment",
			Name:               deployment.Name,
			UID:                deployment.UID,
			Controller:         ptr.To(true),
			BlockOwnerDeletion: ptr.To(true),
		}}
	}
	return pod
}

// copyStringMap returns a copy so a test cannot mutate the fixture through the
// object it created.
func copyStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

// workloadContainer returns the first container of a Pod.
func workloadContainer(t *testing.T, pod *corev1.Pod) *corev1.Container {
	t.Helper()
	if len(pod.Spec.Containers) == 0 {
		t.Fatal("the Pod fixture has no containers")
	}
	return &pod.Spec.Containers[0]
}

// withArgs appends vLLM arguments to the fixture's workload container.
func withArgs(pod *corev1.Pod, args ...string) *corev1.Pod {
	pod.Spec.Containers[0].Args = append(pod.Spec.Containers[0].Args, args...)
	return pod
}

// withoutGPU removes the synthetic device request, leaving a CPU-only workload.
func withoutGPU(pod *corev1.Pod) *corev1.Pod {
	resources := &pod.Spec.Containers[0].Resources
	delete(resources.Requests, corev1.ResourceName(names.SyntheticResourceName))
	delete(resources.Limits, corev1.ResourceName(names.SyntheticResourceName))
	return pod
}

// withGPUPlacement adds the accelerator-specific placement a real GPU workload
// carries: a device-product node selector, a GPU label in required node affinity,
// the NVIDIA runtime class, a GPU toleration, and a storage constraint that must
// survive.
func withGPUPlacement(pod *corev1.Pod) *corev1.Pod {
	pod.Spec.NodeSelector = map[string]string{
		"nvidia.com/gpu.product":      "NVIDIA-H200-141GB",
		"topology.kubernetes.io/zone": "zone-a",
	}
	pod.Spec.RuntimeClassName = ptr.To("nvidia")
	pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{
		Key:      "nvidia.com/gpu",
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	})
	pod.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      "nvidia.com/gpu.memory",
						Operator: corev1.NodeSelectorOpGt,
						Values:   []string{"80000"},
					}},
				}},
			},
		},
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: "model-weights",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "model-weights"},
		},
	})
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{Name: "model-weights", MountPath: "/weights", ReadOnly: true})
	return pod
}

// withSidecar adds an unrelated sidecar and init container that admission must
// leave alone.
func withSidecar(pod *corev1.Pod) *corev1.Pod {
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name:    "metrics-sidecar",
		Image:   "quay.io/example/metrics-sidecar:e2e",
		Command: []string{"/sidecar"},
		Args:    []string{"--listen=:9100"},
	})
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
		Name:    "cert-init",
		Image:   "quay.io/example/cert-init:e2e",
		Command: []string{"/init-certs"},
	})
	return pod
}

// withCredentialMounts adds the TLS and API-key material a served model server
// may carry, which must survive translation untouched.
func withCredentialMounts(pod *corev1.Pod) *corev1.Pod {
	pod.Spec.Volumes = append(pod.Spec.Volumes,
		corev1.Volume{Name: "tls", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: "serving-tls"},
		}},
		corev1.Volume{Name: "api-key", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: "serving-api-key"},
		}},
	)
	container := &pod.Spec.Containers[0]
	container.VolumeMounts = append(container.VolumeMounts,
		corev1.VolumeMount{Name: "tls", MountPath: "/etc/tls", ReadOnly: true},
		corev1.VolumeMount{Name: "api-key", MountPath: "/etc/api-key", ReadOnly: true},
	)
	container.Env = append(container.Env, corev1.EnvVar{
		Name: "VLLM_API_KEY",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "serving-api-key"},
				Key:                  "api-key",
			},
		},
	})
	return pod
}

// createPod submits a Pod through the API server (and therefore through
// admission) and returns the stored object.
func (h *harness) createPod(t *testing.T, pod *corev1.Pod) *corev1.Pod {
	t.Helper()
	if err := h.client.Create(helperContext(t), pod); err != nil {
		t.Fatalf("create Pod %s/%s: %v", pod.Namespace, pod.Name, err)
	}
	stored := &corev1.Pod{}
	if err := h.client.Get(helperContext(t), client.ObjectKeyFromObject(pod), stored); err != nil {
		t.Fatalf("read back Pod %s/%s: %v", pod.Namespace, pod.Name, err)
	}
	return stored
}

// createPodExpectingDenial submits a Pod and requires the API server to reject it
// because admission denied it, and requires that no Pod was created.
func (h *harness) createPodExpectingDenial(t *testing.T, pod *corev1.Pod) string {
	t.Helper()
	err := h.client.Create(helperContext(t), pod)
	if err == nil {
		// A created Pod cannot be taken back: deleting it would leave the
		// assertion meaningless, so fail with the object that was admitted.
		_ = h.client.Delete(helperContext(t), pod)
		t.Fatalf("Pod %s/%s was admitted, want an admission denial", pod.Namespace, pod.Name)
	}
	if !apierrors.IsForbidden(err) {
		t.Fatalf("Pod creation failed with %T (%v), want an admission denial (Forbidden)", err, err)
	}

	key := client.ObjectKeyFromObject(pod)
	if key.Name != "" {
		if getErr := h.client.Get(helperContext(t), key, &corev1.Pod{}); !apierrors.IsNotFound(getErr) {
			t.Fatalf("denied Pod %s still exists (get returned %v)", key, getErr)
		}
	} else {
		// A generateName Pod is rejected before a name is assigned; list instead.
		pods := &corev1.PodList{}
		if listErr := h.client.List(helperContext(t), pods, client.InNamespace(pod.Namespace)); listErr != nil {
			t.Fatalf("list Pods after a denial: %v", listErr)
		}
		if len(pods.Items) != 0 {
			t.Fatalf("denied Pod left %d Pod(s) behind in %s", len(pods.Items), pod.Namespace)
		}
	}
	return err.Error()
}

// sanitizeForCreation strips the server-owned fields from an object read from the
// API server so it can be submitted again as a new object.
func sanitizeForCreation(pod *corev1.Pod, namespace string) *corev1.Pod {
	out := pod.DeepCopy()
	out.Namespace = namespace
	out.Name = ""
	out.GenerateName = "resubmitted-"
	out.UID = ""
	out.ResourceVersion = ""
	out.Generation = 0
	out.CreationTimestamp = metav1.Time{}
	out.ManagedFields = nil
	out.DeletionTimestamp = nil
	out.DeletionGracePeriodSeconds = nil
	out.OwnerReferences = nil
	out.Status = corev1.PodStatus{}
	out.Spec.NodeName = ""
	return out
}

// containerOf returns the named container, or nil.
func containerOf(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

// gpuRequestingContainers returns the names of the containers that request or
// limit the synthetic device, with the quantities as strings.
func gpuRequestingContainers(pod *corev1.Pod) map[string]string {
	requesters := map[string]string{}
	record := func(container corev1.Container) {
		gpu := corev1.ResourceName(names.SyntheticResourceName)
		if quantity, ok := container.Resources.Requests[gpu]; ok {
			requesters[container.Name] = quantity.String()
			return
		}
		if quantity, ok := container.Resources.Limits[gpu]; ok {
			requesters[container.Name] = quantity.String()
		}
	}
	for _, container := range pod.Spec.Containers {
		record(container)
	}
	for _, container := range pod.Spec.InitContainers {
		record(container)
	}
	return requesters
}

// provenanceAnnotations are the annotations docs/contracts/admission.md requires
// on every mutated Pod.
var provenanceAnnotations = []string{
	names.InjectedAnnotation,
	names.ProfileAnnotation,
	names.ProfileImageAnnotation,
	names.ModelAnnotation,
	names.OriginalGPUsAnnotation,
	names.VLLMVersionAnnotation,
}

// assertSimulatedLayout asserts the observable outcome of admission for an
// applicable Pod: the pinned profile, the injected containers, the preserved
// allocation, and the provenance annotations.
func assertSimulatedLayout(t *testing.T, pod *corev1.Pod, wantProfileImage string) {
	t.Helper()

	for _, annotation := range provenanceAnnotations {
		if pod.Annotations[annotation] == "" {
			t.Errorf("Pod is missing provenance annotation %q (annotations: %v)", annotation, pod.Annotations)
		}
	}
	if got, want := pod.Annotations[names.InjectedAnnotation], plan.RuntimeLayoutVersion; got != want {
		t.Errorf("%s = %q, want %q", names.InjectedAnnotation, got, want)
	}
	if got, want := pod.Annotations[names.ProfileAnnotation], profileID; got != want {
		t.Errorf("%s = %q, want the selected profile id %q", names.ProfileAnnotation, got, want)
	}
	if got, want := pod.Annotations[names.ProfileImageAnnotation], wantProfileImage; got != want {
		t.Errorf("%s = %q, want the digest-pinned profile image %q", names.ProfileImageAnnotation, got, want)
	}
	if got, want := pod.Annotations[names.ModelAnnotation], modelName+"@"+modelRevision; got != want {
		t.Errorf("%s = %q, want the resolved model identity %q", names.ModelAnnotation, got, want)
	}
	if got, want := pod.Annotations[names.OriginalGPUsAnnotation], "1"; got != want {
		t.Errorf("%s = %q, want the preserved GPU quantity %q", names.OriginalGPUsAnnotation, got, want)
	}
	if got, want := pod.Annotations[names.VLLMVersionAnnotation], names.SupportedVLLMVersion; got != want {
		t.Errorf("%s = %q, want the translation target %q", names.VLLMVersionAnnotation, got, want)
	}

	// Exactly one container keeps the original allocation, and it is the
	// workload container that became the frontend.
	requesters := gpuRequestingContainers(pod)
	if len(requesters) != 1 {
		t.Fatalf("containers requesting %s = %v, want exactly one", names.SyntheticResourceName, requesters)
	}
	if got, ok := requesters[names.FrontendContainerName]; !ok || got != "1" {
		t.Errorf("%s requests %v, want the preserved quantity 1", names.FrontendContainerName, requesters)
	}

	frontend := containerOf(pod.Spec.Containers, names.FrontendContainerName)
	if frontend == nil {
		t.Fatalf("Pod has no %q container (containers: %v)", names.FrontendContainerName, pod.Spec.Containers)
	}
	backend := containerOf(pod.Spec.Containers, names.BackendContainerName)
	if backend == nil {
		t.Fatalf("Pod has no %q container (containers: %v)", names.BackendContainerName, pod.Spec.Containers)
	}
	if containerOf(pod.Spec.InitContainers, names.ProvisionerContainerName) == nil {
		t.Errorf("Pod has no %q init container (init containers: %v)", names.ProvisionerContainerName, pod.Spec.InitContainers)
	}
	if got := containerOf(pod.Spec.InitContainers, names.ProvisionerContainerName); got != nil {
		if got.Image != pinnedReference(goodProfileImage, goodProfileDigest) && !strings.Contains(got.Image, "@"+goodProfileDigest) {
			t.Errorf("provisioner image = %q, want the profile image pinned by digest", got.Image)
		}
	}

	// The simulator is sized for CPU only; the frontend keeps the allocation.
	for _, container := range []corev1.Container{*backend} {
		if _, ok := container.Resources.Requests[corev1.ResourceName(names.SyntheticResourceName)]; ok {
			t.Errorf("injected container %q requests %s: only the workload container may hold the allocation",
				container.Name, names.SyntheticResourceName)
		}
	}

	// The frontend runs the supervisor and is probed through it.
	if len(frontend.Command) == 0 || frontend.Command[0] != names.SupervisorBinary {
		t.Errorf("%s command = %v, want the supervisor %q", names.FrontendContainerName, frontend.Command, names.SupervisorBinary)
	}
	if frontend.ReadinessProbe == nil || frontend.ReadinessProbe.HTTPGet == nil ||
		frontend.ReadinessProbe.HTTPGet.Path == "/v1/models" {
		t.Errorf("%s readiness probe = %+v, want the supervisor's readiness endpoint", names.FrontendContainerName, frontend.ReadinessProbe)
	}

	// The shared profile volume is mounted into the containers that read it.
	for _, container := range []corev1.Container{*frontend, *backend} {
		mounted := false
		for _, mount := range container.VolumeMounts {
			if mount.Name == names.ProfileVolumeName {
				mounted = true
			}
		}
		if !mounted {
			t.Errorf("container %q does not mount the %q volume", container.Name, names.ProfileVolumeName)
		}
	}
	if containerOf(pod.Spec.InitContainers, names.ProvisionerContainerName) != nil {
		provisioner := containerOf(pod.Spec.InitContainers, names.ProvisionerContainerName)
		mounted := false
		for _, mount := range provisioner.VolumeMounts {
			if mount.Name == names.ProfileVolumeName {
				mounted = true
			}
		}
		if !mounted {
			t.Errorf("the provisioner does not mount the %q volume", names.ProfileVolumeName)
		}
	}
	profileVolume := false
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == names.ProfileVolumeName {
			profileVolume = true
		}
	}
	if !profileVolume {
		t.Errorf("Pod has no %q volume (volumes: %v)", names.ProfileVolumeName, pod.Spec.Volumes)
	}
}
