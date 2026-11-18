package e2e

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vcrv1alpha1 "github.com/opendatahub-io/vllm-vcr-operator/api/v1alpha1"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
)

// TestControllerCreatedPodIsTranslated submits the Pod a ReplicaSet would create
// from the unchanged llm-d modelserver Deployment and asserts admission applied
// the simulated runtime layout.
//
// envtest runs no ReplicaSet controller, so the Pod is submitted here exactly as
// that controller submits it: the fixture's PodTemplate, a generateName, and a
// controller owner reference.
func TestControllerCreatedPodIsTranslated(t *testing.T) {
	h := requireSuite(t)
	h.requireReadyEnvironment(t)
	namespace := h.newNamespace(t)

	deployment := h.modelserverDeployment(t, namespace)
	if err := h.client.Create(helperContext(t), deployment); err != nil {
		t.Fatalf("create modelserver Deployment: %v", err)
	}
	stored := &appsv1.Deployment{}
	if err := h.client.Get(helperContext(t), client.ObjectKeyFromObject(deployment), stored); err != nil {
		t.Fatalf("read back modelserver Deployment: %v", err)
	}

	// Admission never mutates the controller manifest: the Pod template stays the
	// unchanged GPU workload it was.
	template := stored.Spec.Template
	if len(template.Spec.Containers) != 1 || template.Spec.Containers[0].Name != "modelserver" {
		t.Fatalf("Deployment PodTemplate containers = %+v, want the single modelserver container", template.Spec.Containers)
	}
	for _, annotation := range provenanceAnnotations {
		if _, ok := template.Annotations[annotation]; ok {
			t.Errorf("Deployment PodTemplate carries the operator annotation %q: admission must only mutate Pods", annotation)
		}
	}

	pod := h.createPod(t, podFromTemplate(stored))
	assertSimulatedLayout(t, pod, pinnedReference(goodProfileImage, goodProfileDigest))

	if pod.Name == "" || !strings.HasPrefix(pod.Name, stored.Name+"-") {
		t.Errorf("Pod name = %q, want the generated %q prefix the template asks for", pod.Name, stored.Name+"-")
	}
	for _, label := range []string{"llm-d.ai/guide", "llm-d.ai/model", "llm-d.ai/inference-serving"} {
		if pod.Labels[label] != template.Labels[label] {
			t.Errorf("Pod label %q = %q, want the template's %q", label, pod.Labels[label], template.Labels[label])
		}
	}
	if len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].Name != stored.Name {
		t.Errorf("Pod owner references = %+v, want the Deployment", pod.OwnerReferences)
	}
	if pod.Spec.ServiceAccountName != template.Spec.ServiceAccountName {
		t.Errorf("serviceAccountName = %q, want the workload's %q", pod.Spec.ServiceAccountName, template.Spec.ServiceAccountName)
	}
}

// TestDirectlySubmittedPodIsTranslated asserts a bare Pod submitted by a user gets
// the same treatment as a controller-created one.
func TestDirectlySubmittedPodIsTranslated(t *testing.T) {
	h := requireSuite(t)
	h.requireReadyEnvironment(t)
	namespace := h.newNamespace(t)

	deployment := h.modelserverDeployment(t, namespace)
	pod := podFromTemplate(deployment)
	pod.GenerateName = ""
	pod.Name = "qwen3-modelserver"
	pod.Namespace = namespace
	pod.OwnerReferences = nil

	created := h.createPod(t, pod)
	assertSimulatedLayout(t, created, pinnedReference(goodProfileImage, goodProfileDigest))
}

// TestNonGPUWorkloadIsReturnedUnchanged asserts a Pod that requests no synthetic
// device is not touched at all.
//
// The comparison object is the same Pod submitted to the operator namespace,
// which the shipped admission configuration excludes: the API server applies the
// same defaulting to both, so any difference is admission's doing.
func TestNonGPUWorkloadIsReturnedUnchanged(t *testing.T) {
	h := requireSuite(t)
	h.requireReadyEnvironment(t)
	namespace := h.newNamespace(t)

	deployment := h.modelserverDeployment(t, namespace)

	subject := withoutGPU(podFromTemplate(deployment))
	subject.Namespace = namespace
	created := h.createPod(t, subject)

	reference := withoutGPU(podFromTemplate(deployment))
	reference.Namespace = operatorNamespace
	reference.OwnerReferences = nil
	reference.GenerateName = "e2e-untouched-"
	untouched := h.createPod(t, reference)

	if !equality.Semantic.DeepEqual(created.Spec, untouched.Spec) {
		t.Errorf("admission changed a CPU-only Pod's spec:\nadmitted: %+v\nuntouched: %+v", created.Spec, untouched.Spec)
	}
	if len(created.Annotations) != 0 {
		t.Errorf("admission annotated a CPU-only Pod: %v", created.Annotations)
	}
	if got := containerOf(created.Spec.Containers, names.FrontendContainerName); got != nil {
		t.Error("a CPU-only Pod received the injected frontend container")
	}
	if len(created.Spec.Containers) != 1 || created.Spec.Containers[0].Name != "modelserver" {
		t.Errorf("containers = %+v, want the single untouched modelserver container", created.Spec.Containers)
	}
	if len(created.Spec.InitContainers) != 0 {
		t.Errorf("init containers = %+v, want none", created.Spec.InitContainers)
	}
}

// TestAlreadyInjectedPodIsNotInjectedAgain asserts idempotency: resubmitting a Pod
// that already carries the layout changes nothing, so a re-admission can never
// duplicate containers or annotations.
func TestAlreadyInjectedPodIsNotInjectedAgain(t *testing.T) {
	h := requireSuite(t)
	h.requireReadyEnvironment(t)
	namespace := h.newNamespace(t)

	deployment := h.modelserverDeployment(t, namespace)
	injected := h.createPod(t, podFromTemplate(deployment))
	assertSimulatedLayout(t, injected, pinnedReference(goodProfileImage, goodProfileDigest))

	// Re-submit the admitted object, as a controller recreating a Pod or a user
	// applying a captured manifest would.
	resubmitted := sanitizeForCreation(injected, h.newNamespace(t))
	created := h.createPod(t, resubmitted)

	reference := sanitizeForCreation(injected, operatorNamespace)
	reference.OwnerReferences = nil
	reference.GenerateName = "e2e-injected-reference-"
	untouched := h.createPod(t, reference)

	if !equality.Semantic.DeepEqual(created.Spec, untouched.Spec) {
		t.Errorf("re-submitting an injected Pod changed it:\nresubmitted: %+v\nuntouched: %+v", created.Spec, untouched.Spec)
	}
	if !equality.Semantic.DeepEqual(created.Annotations, injected.Annotations) {
		t.Errorf("annotations changed: %v, want %v", created.Annotations, injected.Annotations)
	}
	backend, provisioners := 0, 0
	for _, container := range created.Spec.Containers {
		if container.Name == names.BackendContainerName {
			backend++
		}
	}
	for _, container := range created.Spec.InitContainers {
		if container.Name == names.ProvisionerContainerName {
			provisioners++
		}
	}
	if backend != 1 {
		t.Errorf("%d %q containers, want exactly one", backend, names.BackendContainerName)
	}
	if provisioners != 1 {
		t.Errorf("%d %q init containers, want exactly one", provisioners, names.ProvisionerContainerName)
	}
}

// TestUnsupportedSettingsAreRejected asserts the admission decision for each
// unsupported or unprovable workload shape: the API server denies the Pod, the
// message names the setting an operator has to change, and nothing is created.
func TestUnsupportedSettingsAreRejected(t *testing.T) {
	h := requireSuite(t)
	h.requireReadyEnvironment(t)

	cases := []struct {
		name   string
		mutate func(*corev1.Pod)
		want   []string
	}{
		{
			name:   "pipeline parallelism",
			mutate: func(pod *corev1.Pod) { withArgs(pod, "--pipeline-parallel-size=2") },
			want:   []string{"--pipeline-parallel-size"},
		},
		{
			name:   "LoRA adapters",
			mutate: func(pod *corev1.Pod) { withArgs(pod, "--enable-lora") },
			want:   []string{"--enable-lora", "capability"},
		},
		{
			name:   "unknown flag",
			mutate: func(pod *corev1.Pod) { withArgs(pod, "--frobnicate=rapidly") },
			want:   []string{"--frobnicate", "unknown flag"},
		},
		{
			name: "shell-wrapped command",
			mutate: func(pod *corev1.Pod) {
				container := &pod.Spec.Containers[0]
				container.Command = []string{"bash", "-c", "vllm serve Qwen/Qwen3-0.6B"}
				container.Args = nil
			},
			want: []string{"command", "shell wrapper"},
		},
		{
			name:   "setting outside the profile's coverage",
			mutate: func(pod *corev1.Pod) { withArgs(pod, "--max-num-seqs=32") },
			want:   []string{"--max-num-seqs", "coverage"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h.requireReadyEnvironment(t)
			namespace := h.newNamespace(t)

			deployment := h.modelserverDeployment(t, namespace)
			pod := podFromTemplate(deployment)
			pod.GenerateName = ""
			pod.Name = "rejected-workload"
			testCase.mutate(pod)

			message := h.createPodExpectingDenial(t, pod)
			for _, want := range testCase.want {
				if !strings.Contains(strings.ToLower(message), strings.ToLower(want)) {
					t.Errorf("denial message %q does not mention %q", message, want)
				}
			}
		})
	}
}

// TestAmbiguousProfileSelectionIsRejected asserts that two recorded profiles
// compatible with the workload produce a denial rather than an arbitrary pick.
func TestAmbiguousProfileSelectionIsRejected(t *testing.T) {
	h := requireSuite(t)
	h.requireReadyEnvironment(t)

	h.setProfiles(t,
		vcrv1alpha1.ProfileSource{Image: goodProfileImage},
		vcrv1alpha1.ProfileSource{Image: twinProfileImage},
	)
	h.waitForProfiles(t, twinProfileImage)
	h.requireReadyEnvironment(t)

	namespace := h.newNamespace(t)
	pod := podFromTemplate(h.modelserverDeployment(t, namespace))
	pod.GenerateName = ""
	pod.Name = "ambiguous-workload"

	message := h.createPodExpectingDenial(t, pod)
	for _, want := range []string{"ambiguous", profileID, twinProfileID} {
		if !strings.Contains(message, want) {
			t.Errorf("denial message %q does not mention %q", message, want)
		}
	}
}

// TestServingIdentityIsPreserved asserts everything that identifies the workload
// survives translation: served-model aliases, inference port name and number, the
// KV-event port, llm-d labels, TLS and API-key material, storage, and unrelated
// sidecars and init containers.
func TestServingIdentityIsPreserved(t *testing.T) {
	h := requireSuite(t)
	h.requireReadyEnvironment(t)
	namespace := h.newNamespace(t)

	deployment := h.modelserverDeployment(t, namespace)
	pod := podFromTemplate(deployment)
	pod.GenerateName = ""
	pod.Name = "preserved-workload"
	withSidecar(pod)
	withCredentialMounts(pod)

	created := h.createPod(t, pod)
	assertSimulatedLayout(t, created, pinnedReference(goodProfileImage, goodProfileDigest))

	for _, label := range []string{"llm-d.ai/guide", "llm-d.ai/model", "llm-d.ai/inference-serving"} {
		if created.Labels[label] != deployment.Spec.Template.Labels[label] {
			t.Errorf("Pod label %q = %q, want %q", label, created.Labels[label], deployment.Spec.Template.Labels[label])
		}
	}

	frontend := containerOf(created.Spec.Containers, names.FrontendContainerName)
	if frontend == nil {
		t.Fatal("no frontend container in the admitted Pod")
	}
	for _, port := range []struct {
		name   string
		number int32
	}{
		{"modelserver", 8000},
		{"kv-events", 5556},
	} {
		found := false
		for _, declared := range frontend.Ports {
			if declared.Name == port.name && declared.ContainerPort == port.number {
				found = true
			}
		}
		if !found {
			t.Errorf("frontend ports %+v do not preserve %q on %d", frontend.Ports, port.name, port.number)
		}
	}
	if !hasFlagValue(frontend.Args, "--served-model-name", modelName) {
		t.Errorf("frontend args %v do not preserve the served-model alias %q", frontend.Args, modelName)
	}

	mounts := map[string]string{}
	for _, mount := range frontend.VolumeMounts {
		mounts[mount.Name] = mount.MountPath
	}
	for _, want := range []struct {
		volume string
		path   string
	}{
		{"tls", "/etc/tls"},
		{"api-key", "/etc/api-key"},
		{"model-cache", "/models"},
	} {
		if mounts[want.volume] != want.path {
			t.Errorf("frontend volume mount %q = %q, want %q (mounts: %v)", want.volume, mounts[want.volume], want.path, frontend.VolumeMounts)
		}
	}

	apiKeyPreserved := false
	for _, variable := range frontend.Env {
		if variable.Name != "VLLM_API_KEY" {
			continue
		}
		if variable.ValueFrom == nil || variable.ValueFrom.SecretKeyRef == nil ||
			variable.ValueFrom.SecretKeyRef.Name != "serving-api-key" {
			t.Errorf("VLLM_API_KEY = %+v, want the workload's Secret reference preserved", variable)
		}
		apiKeyPreserved = true
	}
	if !apiKeyPreserved {
		t.Error("the frontend container lost the workload's VLLM_API_KEY")
	}

	if sidecar := containerOf(created.Spec.Containers, "metrics-sidecar"); sidecar == nil {
		t.Errorf("the unrelated sidecar was dropped (containers: %+v)", created.Spec.Containers)
	}
	if certInit := containerOf(created.Spec.InitContainers, "cert-init"); certInit == nil {
		t.Errorf("the unrelated init container was dropped (init containers: %+v)", created.Spec.InitContainers)
	}

	// Storage constraints survive: the workload's own emptyDir and its sizeLimit.
	modelCache := false
	for _, volume := range created.Spec.Volumes {
		if volume.Name != "model-cache" {
			continue
		}
		modelCache = true
		if volume.EmptyDir == nil || volume.EmptyDir.SizeLimit == nil || volume.EmptyDir.SizeLimit.String() != "20Gi" {
			t.Errorf("model-cache volume = %+v, want the workload's 20Gi emptyDir", volume.VolumeSource)
		}
	}
	if !modelCache {
		t.Error("the workload's model-cache volume was dropped")
	}
}

// TestAcceleratorPlacementIsReplaced asserts GPU-specific scheduling is removed
// and simulation node selection is applied, while non-accelerator constraints are
// kept.
func TestAcceleratorPlacementIsReplaced(t *testing.T) {
	h := requireSuite(t)
	h.requireReadyEnvironment(t)
	namespace := h.newNamespace(t)

	pod := podFromTemplate(h.modelserverDeployment(t, namespace))
	pod.GenerateName = ""
	pod.Name = "placed-workload"
	withGPUPlacement(pod)

	created := h.createPod(t, pod)
	assertSimulatedLayout(t, created, pinnedReference(goodProfileImage, goodProfileDigest))

	if value, ok := created.Spec.NodeSelector["nvidia.com/gpu.product"]; ok {
		t.Errorf("the GPU product node selector survived as %q: no simulation node carries it", value)
	}
	if got := created.Spec.NodeSelector[simulationNodeLabel]; got != "true" {
		t.Errorf("node selector = %v, want the environment's simulation selection", created.Spec.NodeSelector)
	}
	if got := created.Spec.NodeSelector["topology.kubernetes.io/zone"]; got != "zone-a" {
		t.Errorf("the workload's zone constraint was dropped (node selector: %v)", created.Spec.NodeSelector)
	}
	if created.Spec.RuntimeClassName != nil {
		t.Errorf("runtimeClassName = %q, want the accelerator runtime class removed", *created.Spec.RuntimeClassName)
	}
	for _, toleration := range created.Spec.Tolerations {
		if toleration.Key == "nvidia.com/gpu" {
			t.Errorf("the accelerator toleration survived: %+v", toleration)
		}
	}
	simulationToleration := false
	for _, toleration := range created.Spec.Tolerations {
		if toleration.Key == simulationNodeLabel {
			simulationToleration = true
		}
	}
	if !simulationToleration {
		t.Errorf("tolerations = %+v, want the environment's simulation toleration", created.Spec.Tolerations)
	}
	if affinity := created.Spec.Affinity; affinity != nil && affinity.NodeAffinity != nil &&
		affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		t.Errorf("GPU-only node affinity survived: %+v", affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution)
	}

	frontend := containerOf(created.Spec.Containers, names.FrontendContainerName)
	if frontend == nil {
		t.Fatal("no frontend container in the admitted Pod")
	}
	weightsMounted := false
	for _, mount := range frontend.VolumeMounts {
		if mount.Name == "model-weights" && mount.MountPath == "/weights" {
			weightsMounted = true
		}
	}
	if !weightsMounted {
		t.Errorf("the workload's storage mount was dropped (mounts: %v)", frontend.VolumeMounts)
	}
}

// hasFlagValue reports whether argv carries flag with the given value, in either
// the "--flag value" or the "--flag=value" spelling.
func hasFlagValue(argv []string, flag, value string) bool {
	for i, token := range argv {
		if token == flag && i+1 < len(argv) && argv[i+1] == value {
			return true
		}
		if token == flag+"="+value {
			return true
		}
	}
	return false
}

// eventMessages returns every event message and annotation in a namespace.
func eventMessages(t *testing.T, h *harness, namespace string) []string {
	t.Helper()
	events := &corev1.EventList{}
	if err := h.client.List(helperContext(t), events, client.InNamespace(namespace)); err != nil {
		t.Fatalf("list events in %s: %v", namespace, err)
	}
	var messages []string
	for _, event := range events.Items {
		messages = append(messages, event.Message, labels.Set(event.Labels).String(), labels.Set(event.Annotations).String())
	}
	return messages
}

// objectMeta returns the object's metadata for annotation assertions.
func objectMeta(t *testing.T, object metav1.Object) metav1.ObjectMeta {
	t.Helper()
	return metav1.ObjectMeta{
		Name:        object.GetName(),
		Namespace:   object.GetNamespace(),
		Annotations: object.GetAnnotations(),
	}
}
