package webhook_test

import (
	"encoding/json"
	"strings"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
	"github.com/neuralmagic/vllm-vcr-operator/internal/plan"
)

// TestPodWithoutGPURequestIsUntouched asserts admission leaves workloads that do
// not request the synthetic device exactly as submitted.
func TestPodWithoutGPURequestIsUntouched(t *testing.T) {
	mutator := testMutator(t, testClient(t, testEnvironment(t)))
	pod := cpuOnlyPod()

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))

	if !resp.Allowed {
		t.Fatalf("non-GPU Pod denied: %s", messageOf(resp))
	}
	if len(resp.Patch) != 0 {
		t.Errorf("non-GPU Pod received patch %s, want no patch", resp.Patch)
	}
}

// TestAlreadyInjectedPodIsUntouched asserts admission is idempotent: a Pod this
// operator already translated is allowed unchanged, even when the environment is
// not available any more.
func TestAlreadyInjectedPodIsUntouched(t *testing.T) {
	mutator := testMutator(t, testClient(t))
	pod := testPod()
	pod.Annotations = map[string]string{names.InjectedAnnotation: plan.RuntimeLayoutVersion}
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: names.BackendContainerName})
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{Name: names.ProvisionerContainerName})

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))

	if !resp.Allowed {
		t.Fatalf("already injected Pod denied: %s", messageOf(resp))
	}
	if len(resp.Patch) != 0 {
		t.Errorf("already injected Pod received patch %s, want no patch", resp.Patch)
	}
}

// TestNonCreateOperationIsUntouched asserts only Pod creation is handled.
func TestNonCreateOperationIsUntouched(t *testing.T) {
	mutator := testMutator(t, testClient(t))
	pod := testPod()

	for _, operation := range []admissionv1.Operation{admissionv1.Update, admissionv1.Delete, admissionv1.Connect} {
		resp := mutator.Admit(t.Context(), admissionRequest(t, operation, pod))
		if !resp.Allowed || len(resp.Patch) != 0 {
			t.Errorf("%s: Allowed=%t patch=%s, want allowed without a patch", operation, resp.Allowed, resp.Patch)
		}
	}
}

// TestApplicablePodDeniedWithoutEnvironment asserts an applicable Pod is rejected
// with an actionable message when the singleton configuration is absent.
func TestApplicablePodDeniedWithoutEnvironment(t *testing.T) {
	mutator := testMutator(t, testClient(t))

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, testPod()))

	if resp.Allowed {
		t.Fatal("GPU Pod admitted without a SimulationEnvironment")
	}
	message := messageOf(resp)
	for _, want := range []string{"SimulationEnvironment", vcrv1alpha1.SimulationEnvironmentNameDefault} {
		if !strings.Contains(message, want) {
			t.Errorf("denial message %q does not mention %q", message, want)
		}
	}
}

// TestApplicablePodDeniedWhenEnvironmentIsNotReady asserts a not-ready
// environment fails closed and reports its own condition message.
func TestApplicablePodDeniedWhenEnvironmentIsNotReady(t *testing.T) {
	env := testEnvironment(t, func(e *vcrv1alpha1.SimulationEnvironment) {
		e.Status.Conditions = []metav1.Condition{{
			Type:    vcrv1alpha1.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  vcrv1alpha1.ReasonDevicePluginNotReady,
			Message: "the synthetic device plugin is not ready on simulation nodes",
		}}
	})
	mutator := testMutator(t, testClient(t, env))

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, testPod()))

	if resp.Allowed {
		t.Fatal("GPU Pod admitted by a not-ready SimulationEnvironment")
	}
	message := messageOf(resp)
	if !strings.Contains(message, "not ready") && !strings.Contains(message, "not Ready") {
		t.Errorf("denial message %q does not say the environment is not ready", message)
	}
	if !strings.Contains(message, "synthetic device plugin is not ready") {
		t.Errorf("denial message %q does not relay the environment condition", message)
	}
}

// TestApplicablePodDeniedWithoutReconciledProfiles asserts an environment that is
// ready but publishes no profile cannot admit anything.
func TestApplicablePodDeniedWithoutReconciledProfiles(t *testing.T) {
	env := testEnvironment(t, func(e *vcrv1alpha1.SimulationEnvironment) {
		e.Status.Profiles = nil
		e.Spec.Profiles = nil
	})
	mutator := testMutator(t, testClient(t, env))

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, testPod()))

	if resp.Allowed {
		t.Fatal("GPU Pod admitted without a reconciled profile catalog")
	}
	if message := messageOf(resp); !strings.Contains(message, "profile") {
		t.Errorf("denial message %q does not mention profiles", message)
	}
}

// TestUnsupportedSettingDeniedWithSettingNamed asserts a policy rejection names
// the offending setting and the reason.
func TestUnsupportedSettingDeniedWithSettingNamed(t *testing.T) {
	mutator := testMutator(t, testClient(t, testEnvironment(t)))
	pod := testPod("--pipeline-parallel-size", "2")

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))

	if resp.Allowed {
		t.Fatal("GPU Pod with pipeline parallelism admitted")
	}
	message := messageOf(resp)
	if !strings.Contains(message, "--pipeline-parallel-size") {
		t.Errorf("denial message %q does not name the setting", message)
	}
	if !strings.Contains(message, "pipeline") {
		t.Errorf("denial message %q does not explain the reason", message)
	}
}

// TestUnsupportedEntrypointDeniedAsPolicy asserts a workload the operator cannot
// interpret is a policy denial, not an operator problem.
func TestUnsupportedEntrypointDeniedAsPolicy(t *testing.T) {
	mutator := testMutator(t, testClient(t, testEnvironment(t)))
	pod := testPod()
	pod.Spec.Containers[0].Command = []string{"/bin/sh", "-c", "vllm serve " + testModel}

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))

	if resp.Allowed {
		t.Fatal("shell-wrapped GPU Pod admitted")
	}
	if message := messageOf(resp); strings.Contains(message, "operator problem") {
		t.Errorf("denial message %q reports a workload problem as an operator problem", message)
	}
}

// TestMultipleGPUContainersDenied asserts the Pod as a whole is rejected when the
// synthetic allocation has no single owner.
func TestMultipleGPUContainersDenied(t *testing.T) {
	mutator := testMutator(t, testClient(t, testEnvironment(t)))
	pod := testPod()
	second := pod.Spec.Containers[0].DeepCopy()
	second.Name = "second-modelserver"
	pod.Spec.Containers = append(pod.Spec.Containers, *second)

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))

	if resp.Allowed {
		t.Fatal("GPU Pod with two GPU containers admitted")
	}
	message := messageOf(resp)
	for _, want := range []string{"modelserver", "second-modelserver", names.SyntheticResourceName} {
		if !strings.Contains(message, want) {
			t.Errorf("denial message %q does not mention %q", message, want)
		}
	}
}

// TestAcceptedPodIsTranslated asserts the admitted Pod carries the translated
// runtime layout and the provenance annotations while keeping its own device
// allocation.
func TestAcceptedPodIsTranslated(t *testing.T) {
	mutator := testMutator(t, testClient(t, testEnvironment(t)))
	pod := testPod()

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))

	if !resp.Allowed {
		t.Fatalf("GPU Pod denied: %s", messageOf(resp))
	}
	if resp.PatchType == nil || *resp.PatchType != admissionv1.PatchTypeJSONPatch {
		t.Fatalf("patchType = %v, want JSONPatch", resp.PatchType)
	}
	mutated := applyPatch(t, pod, resp.Patch)

	gpu := corev1.ResourceName(names.SyntheticResourceName)
	frontend, ok := containerByName(mutated.Spec.Containers, names.FrontendContainerName)
	if !ok {
		t.Fatalf("containers %v do not include %q", containerNames(mutated.Spec.Containers), names.FrontendContainerName)
	}
	if got := frontend.Resources.Requests[gpu]; got.String() != "1" {
		t.Errorf("frontend %s request = %s, want 1 (the workload's allocation)", gpu, got.String())
	}
	if got := frontend.Resources.Limits[gpu]; got.String() != "1" {
		t.Errorf("frontend %s limit = %s, want 1 (the workload's allocation)", gpu, got.String())
	}

	backend, ok := containerByName(mutated.Spec.Containers, names.BackendContainerName)
	if !ok {
		t.Fatalf("containers %v do not include %q", containerNames(mutated.Spec.Containers), names.BackendContainerName)
	}
	provisioner, ok := containerByName(mutated.Spec.InitContainers, names.ProvisionerContainerName)
	if !ok {
		t.Fatalf("init containers %v do not include %q", containerNames(mutated.Spec.InitContainers), names.ProvisionerContainerName)
	}
	for name, container := range map[string]corev1.Container{"backend": backend, "provisioner": provisioner} {
		if _, found := container.Resources.Requests[gpu]; found {
			t.Errorf("injected %s container requests %s", name, gpu)
		}
		if _, found := container.Resources.Limits[gpu]; found {
			t.Errorf("injected %s container limits %s", name, gpu)
		}
	}

	if got, want := frontend.Image, testFrontend; got != want {
		t.Errorf("frontend image = %q, want %q", got, want)
	}
	if got, want := backend.Image, testBackend; got != want {
		t.Errorf("backend image = %q, want %q", got, want)
	}
	pinned := testImage[:strings.Index(testImage, ":")] + "@" + testDigest
	if got := provisioner.Image; got != pinned {
		t.Errorf("provisioner image = %q, want the digest-pinned profile %q", got, pinned)
	}

	annotations := mutated.Annotations
	wantAnnotations := map[string]string{
		names.InjectedAnnotation:     plan.RuntimeLayoutVersion,
		names.ProfileAnnotation:      testProfileID,
		names.ProfileImageAnnotation: pinned,
		names.ModelAnnotation:        testModel + "@c9340e6f3",
		names.VLLMVersionAnnotation:  names.SupportedVLLMVersion,
		names.OriginalGPUsAnnotation: "1",
	}
	for key, want := range wantAnnotations {
		if got := annotations[key]; got != want {
			t.Errorf("annotation %s = %q, want %q", key, got, want)
		}
	}

	// Accepted-but-not-reproduced aspects are surfaced to the user as warnings,
	// never as silent approximations.
	if len(resp.Warnings) == 0 {
		t.Error("admission carries no warnings, but the plan records replay observations")
	}

	// The workload's own pull secret is preserved next to the projected one.
	if !hasPullSecret(mutated, "workload-owned-pull") {
		t.Errorf("imagePullSecrets %v dropped the workload's own secret", mutated.Spec.ImagePullSecrets)
	}

	// The submitted Pod is never modified.
	if len(pod.Spec.Containers) != 1 || pod.Annotations != nil {
		t.Errorf("admission mutated the submitted Pod: containers=%v annotations=%v",
			containerNames(pod.Spec.Containers), pod.Annotations)
	}
}

// TestPatchIsDeterministic asserts two admissions of the same Pod produce the same
// patch, so retries and dry-run requests cannot disagree.
func TestPatchIsDeterministic(t *testing.T) {
	env := testEnvironment(t)
	pod := testPod()
	first := testMutator(t, testClient(t, env)).Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
	second := testMutator(t, testClient(t, env)).Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))

	if !first.Allowed || !second.Allowed {
		t.Fatalf("admission denied: %s / %s", messageOf(first), messageOf(second))
	}
	if string(first.Patch) != string(second.Patch) {
		t.Errorf("patch is not deterministic:\n%s\n%s", first.Patch, second.Patch)
	}
}

// TestMutatedPodPreservesModelArguments asserts the workload's own settings reach
// the process that enforces them: served-model aliases and rendering settings go to
// the frontend, recorded-shape settings go to the simulator.
func TestMutatedPodPreservesModelArguments(t *testing.T) {
	mutator := testMutator(t, testClient(t, testEnvironment(t)))
	pod := testPod("--max-model-len", "4096", "--served-model-name", "qwen-alias")

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
	if !resp.Allowed {
		t.Fatalf("GPU Pod denied: %s", messageOf(resp))
	}
	mutated := applyPatch(t, pod, resp.Patch)
	frontend, ok := containerByName(mutated.Spec.Containers, names.FrontendContainerName)
	if !ok {
		t.Fatalf("containers %v do not include the frontend", containerNames(mutated.Spec.Containers))
	}
	backend, ok := containerByName(mutated.Spec.Containers, names.BackendContainerName)
	if !ok {
		t.Fatalf("containers %v do not include the backend", containerNames(mutated.Spec.Containers))
	}
	if argv := strings.Join(append(frontend.Command, frontend.Args...), " "); !strings.Contains(argv, "--served-model-name qwen-alias") {
		t.Errorf("frontend argv %q does not preserve the workload's served-model alias", argv)
	}
	// The simulator runs under a cold-start wait wrapper, so its argv is the
	// wrapper script: the planned flags appear quoted inside it.
	if argv := strings.Join(backend.Command, " ") + " " + strings.Join(backend.Args, " "); !strings.Contains(argv, "--max-model-len") || !strings.Contains(argv, "4096") {
		t.Errorf("backend argv %q does not carry the workload's --max-model-len", argv)
	}
}

// applyPatch returns the Pod the API server would store after applying the patch
// to the submitted object.
func applyPatch(t *testing.T, pod *corev1.Pod, patch []byte) *corev1.Pod {
	t.Helper()
	original, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal Pod: %v", err)
	}
	decoded, err := jsonpatch.DecodePatch(patch)
	if err != nil {
		t.Fatalf("decode patch %s: %v", patch, err)
	}
	mutatedJSON, err := decoded.Apply(original)
	if err != nil {
		t.Fatalf("apply patch %s: %v", patch, err)
	}
	var mutated corev1.Pod
	if err := json.Unmarshal(mutatedJSON, &mutated); err != nil {
		t.Fatalf("unmarshal mutated Pod: %v", err)
	}
	return &mutated
}

// messageOf returns the denial message of a response.
func messageOf(resp *admissionv1.AdmissionResponse) string {
	if resp == nil || resp.Result == nil {
		return ""
	}
	return resp.Result.Message
}

// containerByName returns the named container.
func containerByName(containers []corev1.Container, name string) (corev1.Container, bool) {
	for _, container := range containers {
		if container.Name == name {
			return container, true
		}
	}
	return corev1.Container{}, false
}

// containerNames lists container names for failure messages.
func containerNames(containers []corev1.Container) []string {
	out := make([]string, len(containers))
	for i, container := range containers {
		out[i] = container.Name
	}
	return out
}

// hasPullSecret reports whether the Pod references the named pull secret.
func hasPullSecret(pod *corev1.Pod, name string) bool {
	for _, reference := range pod.Spec.ImagePullSecrets {
		if reference.Name == name {
			return true
		}
	}
	return false
}

// TestAcceleratorPlacementIsRewritten asserts the patch removes accelerator
// placement while preserving everything that does not describe a GPU: the patch is
// a real JSON patch that must apply cleanly, including object removals and array
// shrinks.
func TestAcceleratorPlacementIsRewritten(t *testing.T) {
	mutator := testMutator(t, testClient(t, testEnvironment(t)))
	pod := testPod()
	pod.Spec.NodeSelector = map[string]string{
		"nvidia.com/gpu.product": "NVIDIA-H200",
		"kubernetes.io/os":       "linux",
	}
	runtimeClass := "nvidia"
	pod.Spec.RuntimeClassName = &runtimeClass
	pod.Spec.Tolerations = []corev1.Toleration{
		{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "llm", Effect: corev1.TaintEffectNoSchedule},
	}
	pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{
				Key: "nvidia.com/gpu.product", Operator: corev1.NodeSelectorOpIn, Values: []string{"NVIDIA-H200"},
			}},
		}}},
	}}
	pod.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "CUDA_VISIBLE_DEVICES", Value: "0"},
		{Name: "HF_HOME", Value: "/models"},
	}
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name:  "log-shipper",
		Image: "busybox:1.36",
	})

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
	if !resp.Allowed {
		t.Fatalf("GPU Pod denied: %s", messageOf(resp))
	}
	mutated := applyPatch(t, pod, resp.Patch)

	wantSelector := map[string]string{
		"vcr.opendatahub-io/simulation": "true",
		"kubernetes.io/os":              "linux",
	}
	if len(mutated.Spec.NodeSelector) != len(wantSelector) {
		t.Errorf("nodeSelector = %v, want %v", mutated.Spec.NodeSelector, wantSelector)
	}
	for key, want := range wantSelector {
		if got := mutated.Spec.NodeSelector[key]; got != want {
			t.Errorf("nodeSelector[%s] = %q, want %q", key, got, want)
		}
	}
	if mutated.Spec.RuntimeClassName != nil {
		t.Errorf("runtimeClassName = %q, want it removed", *mutated.Spec.RuntimeClassName)
	}
	if mutated.Spec.Affinity != nil && mutated.Spec.Affinity.NodeAffinity != nil {
		t.Errorf("node affinity %v survived, want the accelerator-only requirement removed", mutated.Spec.Affinity.NodeAffinity)
	}
	if len(mutated.Spec.Tolerations) != 2 {
		t.Errorf("tolerations = %v, want the non-accelerator one and the simulation toleration", mutated.Spec.Tolerations)
	}
	for _, toleration := range mutated.Spec.Tolerations {
		if toleration.Key == "nvidia.com/gpu" {
			t.Errorf("accelerator toleration survived: %v", mutated.Spec.Tolerations)
		}
	}

	frontend, ok := containerByName(mutated.Spec.Containers, names.FrontendContainerName)
	if !ok {
		t.Fatalf("containers %v do not include the frontend", containerNames(mutated.Spec.Containers))
	}
	if _, ok := containerByName(mutated.Spec.Containers, "log-shipper"); !ok {
		t.Errorf("containers %v dropped the unrelated sidecar", containerNames(mutated.Spec.Containers))
	}
	var haveHome bool
	for _, entry := range frontend.Env {
		if entry.Name == "CUDA_VISIBLE_DEVICES" {
			t.Errorf("GPU-only environment variable survived: %v", frontend.Env)
		}
		if entry.Name == "HF_HOME" && entry.Value == "/models" {
			haveHome = true
		}
	}
	if !haveHome {
		t.Errorf("frontend env %v lost the workload's HF_HOME", frontend.Env)
	}
}

// TestGPUOnlyInInitContainerDenied asserts a Pod with no translatable workload
// container is rejected rather than admitted against synthetic devices unmodified.
func TestGPUOnlyInInitContainerDenied(t *testing.T) {
	mutator := testMutator(t, testClient(t, testEnvironment(t)))
	pod := cpuOnlyPod()
	pod.Spec.InitContainers = []corev1.Container{{
		Name:    "prepare-weights",
		Image:   "busybox:1.36",
		Command: []string{"wget", "https://example.com/weights"},
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceName(names.SyntheticResourceName): resource.MustParse("1"),
			},
		},
	}}

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))

	if resp.Allowed {
		t.Fatal("Pod requesting devices only in an init container admitted")
	}
	if message := messageOf(resp); !strings.Contains(message, names.SyntheticResourceName) {
		t.Errorf("denial message %q does not name the resource", message)
	}
}

// TestGPUInitContainerDeniedAsWorkloadProblem asserts a GPU-dependent setup step is
// a workload problem, not an operator problem, and that the message names the
// container: this release translates no GPU-requesting init container.
func TestGPUInitContainerDeniedAsWorkloadProblem(t *testing.T) {
	mutator := testMutator(t, testClient(t, testEnvironment(t)))
	pod := testPod()
	pod.Spec.InitContainers = []corev1.Container{{
		Name:    "warm-cache",
		Image:   "vllm/vllm-openai:v0.33.0",
		Command: []string{"python", "-c", "import torch"},
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceName(names.SyntheticResourceName): resource.MustParse("1"),
			},
		},
	}}

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))

	if resp.Allowed {
		t.Fatal("Pod with a GPU-requesting init container admitted")
	}
	message := messageOf(resp)
	if strings.Contains(message, "operator problem") {
		t.Errorf("denial message %q reports a workload problem as an operator problem", message)
	}
	for _, want := range []string{"warm-cache", names.SyntheticResourceName} {
		if !strings.Contains(message, want) {
			t.Errorf("denial message %q does not mention %q", message, want)
		}
	}
}
