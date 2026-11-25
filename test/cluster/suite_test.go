package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
)

// Namespaces the workload steps create. Each step tears its own namespace down, so
// a failure cannot leave a namespace behind that silently changes the next step's
// placement or capacity results.
const (
	workloadNamespace     = "vcr-accept-workload"
	capacityNamespace     = "vcr-accept-capacity"
	placementNamespace    = "vcr-accept-placement"
	modelserverServiceAcc = "mock-pc-sa"
)

// suite carries the cluster and the installation state between steps.
type suite struct {
	*environment
	artifactsRoot string
	install       installState
	logf          func(format string, args ...any)
}

// TestClusterAcceptance is the ordered acceptance run.
//
// The steps share one installation and one simulation node, and they share it in a
// deliberate order: capacity exhaustion must see the GPU a previous step allocated,
// plugin restart must see the allocations that exhaustion left, and readiness
// withdrawal must observe a Pod that is currently Ready. Failures are reported at
// the step that caused them, and the steps that depended on it are skipped with the
// reason instead of failing again for the same cause.
func TestClusterAcceptance(t *testing.T) {
	environment := loadEnvironment(t)
	s := &suite{
		environment:   environment,
		artifactsRoot: artifactsRoot(t, environment),
		logf:          t.Logf,
	}
	t.Logf("cluster context %q: simulation node %s, other node %s, runtime %s",
		s.context, s.simulationNode, s.otherNode, s.runtime)

	steps := []struct {
		name string
		run  func(*testing.T)
	}{
		{"install operator and test registry", s.stepInstall},
		{"device plugin advertises synthetic GPUs", s.stepDevicePlugin},
		{"admission on a real API server", s.stepAdmission},
		{"placement rewrite", s.stepPlacement},
		{"provisioning and serving readiness", s.stepProvisioning},
		{"allocation, exhaustion, and release", s.stepCapacity},
		{"plugin restart recovery", s.stepPluginRestart},
		{"readiness withdrawal when the backend dies", s.stepReadinessWithdrawal},
		{"cleanup leaves no residue", s.stepCleanup},
	}

	blocked := ""
	for _, step := range steps {
		step := step
		if blocked != "" {
			t.Run(step.name, func(t *testing.T) {
				t.Skipf("skipped because an earlier step failed: %s", blocked)
			})
			continue
		}
		if !t.Run(step.name, step.run) {
			blocked = step.name
		}
	}

	t.Cleanup(func() { s.teardown(t) })
}

// stepInstall applies the SimulationEnvironment and waits for the operator to
// reconcile it: the profile catalog must be published and the device plugin must
// report ready, which is what makes the rest of the run meaningful.
func (s *suite) stepInstall(t *testing.T) {
	t.Helper()
	ctx := t.Context()

	s.installOperator(t)

	if _, err := s.kubectl("label", "node", s.simulationNode,
		"vcr.opendatahub-io/simulation=true", "--overwrite"); err != nil {
		t.Fatalf("label %s as a simulation node: %v", s.simulationNode, err)
	}

	environment := newSimulationEnvironment(s.install.profileImage)
	err := s.client.Create(ctx, environment)
	if apierrors.IsAlreadyExists(err) {
		var existing vcrv1alpha1.SimulationEnvironment
		if err := s.client.Get(ctx, client.ObjectKey{Name: environment.Name}, &existing); err != nil {
			t.Fatalf("read the existing SimulationEnvironment: %v", err)
		}
		existing.Spec = environment.Spec
		err = s.client.Update(ctx, &existing)
	}
	if err != nil {
		t.Fatalf("apply the SimulationEnvironment: %v", err)
	}

	t.Logf("waiting for the SimulationEnvironment to reconcile profile %s", s.install.profileImage)
	err = waitFor(ctx, 4*time.Minute, 2*time.Second, func(ctx context.Context) (bool, string, error) {
		var current vcrv1alpha1.SimulationEnvironment
		if err := s.client.Get(ctx, client.ObjectKey{Name: environment.Name}, &current); err != nil {
			return false, "", err
		}
		for _, condition := range current.Status.Conditions {
			if condition.Type == vcrv1alpha1.ConditionReady {
				return condition.Status == "True", fmt.Sprintf("Ready=%s (%s: %s)",
					condition.Status, condition.Reason, condition.Message), nil
			}
		}
		return false, "no Ready condition yet", nil
	})
	if err != nil {
		s.dumpClusterState(t, "install")
		t.Fatalf("the SimulationEnvironment did not become Ready: %v", err)
	}

	var current vcrv1alpha1.SimulationEnvironment
	if err := s.client.Get(ctx, client.ObjectKey{Name: environment.Name}, &current); err != nil {
		t.Fatalf("read the reconciled SimulationEnvironment: %v", err)
	}

	// The published catalog is what admission pins, so assert it resolved the test
	// registry's image rather than leaving a placeholder.
	if len(current.Status.Profiles) != 1 {
		t.Fatalf("status.profiles has %d entries, want exactly the configured profile", len(current.Status.Profiles))
	}
	published := current.Status.Profiles[0]
	if published.Image != s.install.profileImage {
		t.Errorf("status.profiles[0].image = %q, want the configured %q", published.Image, s.install.profileImage)
	}
	if !strings.HasPrefix(published.Digest, "sha256:") {
		t.Fatalf("status.profiles[0].digest = %q: the operator did not resolve the profile image over the test registry", published.Digest)
	}
	if !strings.Contains(published.Metadata, "stub-qwen3-0.6b-h200-tp1-bf16") {
		t.Errorf("status.profiles[0].metadata does not contain the fixture profile id: %s", published.Metadata)
	}
	t.Logf("profile resolved: digest %s, metadata %d bytes", published.Digest, len(published.Metadata))
}

// stepDevicePlugin asserts the synthetic advertisement and the DaemonSet's shape:
// the capacity exists on selected nodes only, from a plain hostPath mount, with no
// NVIDIA runtime, device file, or privileged container.
func (s *suite) stepDevicePlugin(t *testing.T) {
	ctx := t.Context()

	capacity, err := s.nodeAllocatableGPU(ctx, s.simulationNode)
	if err != nil {
		t.Fatalf("read the simulation node's allocatable capacity: %v", err)
	}
	if capacity != 8 {
		t.Fatalf("node %s advertises %d %s, want 8 (the SimulationEnvironment's devicesPerNode)",
			s.simulationNode, capacity, names.SyntheticResourceName)
	}

	other, err := s.nodeAllocatableGPU(ctx, s.otherNode)
	if err != nil {
		t.Fatalf("read %s's allocatable capacity: %v", s.otherNode, err)
	}
	if other != 0 {
		t.Errorf("node %s advertises %d %s but is not selected by the SimulationEnvironment",
			s.otherNode, other, names.SyntheticResourceName)
	}

	var daemonSet struct {
		Desired int32
		Ready   int32
	}
	err = waitFor(ctx, 2*time.Minute, 2*time.Second, func(ctx context.Context) (bool, string, error) {
		out, err := s.kubectl("-n", operatorNamespace, "get", "daemonset", names.DevicePluginName,
			"-o", "jsonpath={.status.desiredNumberScheduled} {.status.numberReady}")
		if err != nil {
			return false, "the device plugin DaemonSet is not readable yet", nil
		}
		fields := strings.Fields(out)
		if len(fields) != 2 {
			return false, "unexpected DaemonSet status: " + out, nil
		}
		fmt.Sscanf(fields[0], "%d", &daemonSet.Desired)
		fmt.Sscanf(fields[1], "%d", &daemonSet.Ready)
		return daemonSet.Desired == 1 && daemonSet.Ready == 1, "DaemonSet status: " + out, nil
	})
	if err != nil {
		s.dumpClusterState(t, "device-plugin")
		t.Fatalf("the device plugin DaemonSet is not ready on exactly one node: %v", err)
	}

	var pods corev1.PodList
	if err := s.client.List(ctx, &pods, client.InNamespace(operatorNamespace),
		client.MatchingLabels(devicePluginPodLabels())); err != nil {
		t.Fatalf("list the device plugin Pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("%d device plugin Pod(s) exist, want one (only simulation nodes are selected)", len(pods.Items))
	}
	plugin := pods.Items[0]
	if plugin.Spec.NodeName != s.simulationNode {
		t.Errorf("the device plugin runs on %q, want the simulation node %q", plugin.Spec.NodeName, s.simulationNode)
	}
	if plugin.Spec.RuntimeClassName != nil {
		t.Errorf("the device plugin Pod uses runtime class %q; no accelerator runtime may be required", *plugin.Spec.RuntimeClassName)
	}
	container := plugin.Spec.Containers[0]
	if container.SecurityContext != nil && container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
		t.Error("the device plugin container is privileged: a plain hostPath mount is all it needs")
	}
	var devicePluginDirMounted bool
	for _, volume := range plugin.Spec.Volumes {
		if volume.HostPath != nil && volume.HostPath.Path == names.DevicePluginDir {
			devicePluginDirMounted = true
		}
	}
	if !devicePluginDirMounted {
		t.Errorf("the device plugin Pod does not mount %s: kubelet registration needs it", names.DevicePluginDir)
	}
}

// stepAdmission asserts the mutation on a real API server, including the parts the
// contract calls invariants: exactly one container keeps the original GPU quantity,
// the profile image is digest-pinned, the layout's containers are injected, the
// accelerator placement is gone, the workload's own identity is preserved, non-GPU
// Pods are untouched, and an unsupported invocation is denied with an actionable
// message visible to whoever submitted it.
func (s *suite) stepAdmission(t *testing.T) {
	ctx := t.Context()
	namespace := workloadNamespace
	s.prepareWorkloadNamespace(t, namespace)
	s.ensureAcceleratorRuntimeClass(t, "nvidia")
	s.dumpOnFailure(t, "admission", namespace)
	// The namespace deliberately outlives this step: the provisioning step observes the
	// same workload's profile volume and readiness, and the capacity step releases the
	// device this step allocates. The suite's teardown deletes it, including on failure.

	deployment := modelserverDeployment(namespace, workloadOptions{
		name:                  "modelserver-admission",
		kvEvents:              true,
		runtimeClass:          "nvidia",
		gpuNodeAffinity:       true,
		preservedNodeAffinity: true,
	})
	if err := s.client.Create(ctx, deployment); err != nil {
		t.Fatalf("create the modelserver Deployment: %v", err)
	}
	pod := s.waitForReadyPod(t, namespace, "app=modelserver-admission")

	annotations := pod.Annotations
	expected := map[string]string{
		names.InjectedAnnotation:     "v1",
		names.ProfileAnnotation:      "stub-qwen3-0.6b-h200-tp1-bf16",
		names.ModelAnnotation:        "Qwen/Qwen3-0.6B@c9340e6f3",
		names.OriginalGPUsAnnotation: "1",
		names.VLLMVersionAnnotation:  names.SupportedVLLMVersion,
	}
	for key, want := range expected {
		if got := annotations[key]; got != want {
			t.Errorf("annotation %s = %q, want %q", key, got, want)
		}
	}

	// The pinned profile image must be the digest the operator published, not the
	// tag the SimulationEnvironment names.
	var environment vcrv1alpha1.SimulationEnvironment
	if err := s.client.Get(ctx, client.ObjectKey{Name: vcrv1alpha1.SimulationEnvironmentNameDefault}, &environment); err != nil {
		t.Fatalf("read the SimulationEnvironment: %v", err)
	}
	published := environment.Status.Profiles[0]
	pinned := strings.TrimSuffix(published.Image, ":"+tagOf(published.Image)) + "@" + published.Digest
	if got := annotations[names.ProfileImageAnnotation]; got != pinned {
		t.Errorf("annotation %s = %q, want the digest-pinned %q", names.ProfileImageAnnotation, got, pinned)
	}

	frontend := containerByName(pod, names.FrontendContainerName)
	if frontend == nil {
		t.Fatalf("the admitted Pod has no %q container: %v", names.FrontendContainerName, containerNames(pod))
	}
	if frontend.Image != s.images.frontendStub {
		t.Errorf("frontend image = %q, want the operator's configured %q", frontend.Image, s.images.frontendStub)
	}
	if len(frontend.Command) == 0 || frontend.Command[0] != names.SupervisorBinary {
		t.Errorf("frontend command = %v, want the supervisor as entrypoint (%s)", frontend.Command, names.SupervisorBinary)
	}
	if _, kept := envValue(frontend.Env, "CUDA_VISIBLE_DEVICES"); kept {
		t.Error("the frontend container still sets CUDA_VISIBLE_DEVICES: a GPU-only variable must be removed on simulation nodes")
	}
	if _, kept := envValue(frontend.Env, "POD_NAME"); !kept {
		t.Error("the frontend container lost the workload's POD_NAME environment: unrelated settings must be preserved")
	}

	backend := containerByName(pod, names.BackendContainerName)
	if backend == nil {
		t.Fatalf("the admitted Pod has no %q container: %v", names.BackendContainerName, containerNames(pod))
	}
	if backend.Image != s.images.backendStub {
		t.Errorf("backend image = %q, want the operator's configured %q", backend.Image, s.images.backendStub)
	}
	// The simulator runs inside a POSIX-shell restart loop, so its command is the
	// wrapper and the vllm-vcr invocation lives in the script. A cold start makes
	// vllm-vcr give up after 30s; restarting in place avoids kubelet backoff.
	if len(backend.Command) != 2 || backend.Command[0] != "/bin/sh" || backend.Command[1] != "-c" {
		t.Fatalf("backend command = %v, want the sh -c restart wrapper", backend.Command)
	}
	if script := strings.Join(backend.Args, " "); !strings.Contains(script, "'vllm-vcr' 'play'") {
		t.Errorf("backend wrapper does not run vllm-vcr play: %s", script)
	}

	provisioner := initContainerByName(pod, names.ProvisionerContainerName)
	if provisioner == nil {
		t.Fatalf("the admitted Pod has no %q init container", names.ProvisionerContainerName)
	}
	if provisioner.Image != pinned {
		t.Errorf("provisioner image = %q, want the digest-pinned profile image %q", provisioner.Image, pinned)
	}

	// Allocation invariant: exactly one container keeps the workload's GPU request.
	gpu := corev1.ResourceName(names.SyntheticResourceName)
	var gpuContainers []string
	for _, container := range pod.Spec.Containers {
		quantity, ok := container.Resources.Requests[gpu]
		if !ok {
			quantity, ok = container.Resources.Limits[gpu]
		}
		if ok {
			gpuContainers = append(gpuContainers, fmt.Sprintf("%s=%s", container.Name, quantity.String()))
		}
	}
	if len(gpuContainers) != 1 || !strings.HasPrefix(gpuContainers[0], names.FrontendContainerName+"=") {
		t.Fatalf("containers requesting %s: %v, want exactly %s with the original quantity", gpu, gpuContainers, names.FrontendContainerName)
	}
	if got := gpuContainers[0]; got != names.FrontendContainerName+"=1" {
		t.Errorf("%s requests %q, want the workload's original quantity 1", names.FrontendContainerName, got)
	}
	for _, container := range []corev1.Container{*backend, *provisioner} {
		if _, ok := container.Resources.Requests[gpu]; ok {
			t.Errorf("injected container %q requests %s: injected containers never take synthetic devices", container.Name, gpu)
		}
	}

	// Placement rewrite: accelerator-specific placement is gone, the environment's
	// simulation selection is applied, and non-accelerator constraints survive.
	if pod.Spec.RuntimeClassName != nil {
		t.Errorf("runtime class %q survived admission: an accelerator runtime cannot run on a simulation node", *pod.Spec.RuntimeClassName)
	}
	if got := pod.Spec.NodeSelector["vcr.opendatahub-io/simulation"]; got != "true" {
		t.Errorf("node selector = %v, want the environment's simulation node selection", pod.Spec.NodeSelector)
	}
	if affinity := pod.Spec.Affinity; affinity != nil && affinity.NodeAffinity != nil &&
		affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		for _, term := range affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			for _, expression := range term.MatchExpressions {
				if expression.Key == "nvidia.com/gpu.product" {
					t.Errorf("accelerator node affinity %s survived admission", expression.Key)
				}
			}
		}
		if !nodeAffinityRequires(pod, "kubernetes.io/os", "linux") {
			t.Error("the non-accelerator node affinity (kubernetes.io/os=linux) was dropped: unrelated placement constraints must be preserved")
		}
	} else {
		t.Error("the admitted Pod lost its node affinity entirely: the non-accelerator constraint should have survived")
	}

	// The workload's own identity is preserved.
	for _, label := range []string{"llm-d.ai/guide", "llm-d.ai/model", "llm-d.ai/inference-serving"} {
		if pod.Labels[label] == "" {
			t.Errorf("label %s was dropped from the workload's Pod", label)
		}
	}
	if pod.Spec.ServiceAccountName != modelserverServiceAcc {
		t.Errorf("service account = %q, want the workload's %q", pod.Spec.ServiceAccountName, modelserverServiceAcc)
	}
	if !hasVolume(pod, "model-cache") {
		t.Error("the workload's model-cache volume was dropped")
	}
	if !hasPort(frontend, names.DefaultInferencePort) {
		t.Errorf("the frontend container does not keep the workload's inference port: %v", frontend.Ports)
	}

	s.assertNonGPUPodUntouched(t, namespace)
	s.assertUnsupportedWorkloadDenied(t, namespace)
}

// stepPlacement asserts the documented behavior for a non-accelerator constraint no
// node satisfies: admission preserves it (it is not accelerator-specific), so the
// Pod is admitted and the scheduler leaves it Pending with a FailedScheduling event.
func (s *suite) stepPlacement(t *testing.T) {
	ctx := t.Context()
	namespace := placementNamespace
	s.prepareWorkloadNamespace(t, namespace)
	t.Cleanup(func() { s.deleteNamespace(t, namespace) })
	s.dumpOnFailure(t, "placement", namespace)

	deployment := modelserverDeployment(namespace, workloadOptions{
		name:                      "modelserver-unschedulable",
		preservedNodeAffinity:     true,
		unsatisfiableNodeAffinity: true,
	})
	if err := s.client.Create(ctx, deployment); err != nil {
		t.Fatalf("create the unschedulable modelserver Deployment: %v", err)
	}

	pod := s.waitForPod(t, namespace, "app=modelserver-unschedulable", 2*time.Minute)
	if pod.Annotations[names.InjectedAnnotation] != "v1" {
		t.Fatalf("the Pod was not admitted (no %s annotation): admission must preserve non-accelerator constraints, not reject them", names.InjectedAnnotation)
	}
	if pod.Spec.NodeName != "" {
		t.Fatalf("the Pod scheduled onto %q even though its non-accelerator node affinity is unsatisfiable", pod.Spec.NodeName)
	}
	if !nodeAffinityRequires(pod, "vcr.opendatahub-io/rack", "rack-that-does-not-exist") {
		t.Fatal("the unsatisfiable non-accelerator node affinity was dropped: admission must not silently make a Pod schedulable")
	}

	var message string
	err := waitFor(ctx, 90*time.Second, 2*time.Second, func(ctx context.Context) (bool, string, error) {
		var events corev1.EventList
		if err := s.client.List(ctx, &events, client.InNamespace(namespace)); err != nil {
			return false, "", err
		}
		for _, event := range events.Items {
			if event.InvolvedObject.Kind != "Pod" || event.InvolvedObject.Name != pod.Name {
				continue
			}
			if event.Reason == "FailedScheduling" {
				message = event.Message
				return true, "", nil
			}
		}
		return false, "no FailedScheduling event for the Pod yet", nil
	})
	if err != nil {
		s.dumpClusterState(t, "placement")
		t.Fatalf("the scheduler recorded no FailedScheduling event for the preserved constraint: %v", err)
	}
	if !strings.Contains(message, "node affinity") && !strings.Contains(message, "node(s) didn't match") {
		t.Errorf("FailedScheduling message does not explain the node affinity constraint: %q", message)
	}
	t.Logf("documented behavior: admission preserved the unsatisfiable non-accelerator constraint and the scheduler reported: %s", message)
}

// stepProvisioning asserts the runtime layout end to end: the profile init
// container exported the pinned profile into the shared volume, the simulator read
// it, the supervisor published readiness only once the engine registered, and the
// frontend serves the workload's inference port.
func (s *suite) stepProvisioning(t *testing.T) {
	s.dumpOnFailure(t, "provisioning", workloadNamespace)
	pod := s.waitForReadyPod(t, workloadNamespace, "app=modelserver-admission")

	var initStatus *corev1.ContainerStatus
	for i := range pod.Status.InitContainerStatuses {
		if pod.Status.InitContainerStatuses[i].Name == names.ProvisionerContainerName {
			initStatus = &pod.Status.InitContainerStatuses[i]
		}
	}
	if initStatus == nil {
		t.Fatalf("no status for the %q init container", names.ProvisionerContainerName)
	}
	if initStatus.State.Terminated == nil || initStatus.State.Terminated.ExitCode != 0 {
		t.Fatalf("the profile provisioner did not complete successfully: %+v", initStatus.State)
	}
	if initStatus.State.Terminated.Reason != "Completed" {
		t.Errorf("provisioner termination reason = %q, want Completed", initStatus.State.Terminated.Reason)
	}

	metadata := s.execInPod(t, pod, names.FrontendContainerName,
		"cat", names.ProfileMountPath+"/"+names.ProfileMetadataFileName)
	if !strings.Contains(metadata, "stub-qwen3-0.6b-h200-tp1-bf16") {
		t.Errorf("the profile volume's metadata.json is not the fixture's metadata: %s", truncate(metadata, 200))
	}
	if !strings.Contains(metadata, `"blockSize": 64`) {
		t.Errorf("the exported metadata lost the fixture's cache shape: %s", truncate(metadata, 200))
	}
	s.execInPod(t, pod, names.FrontendContainerName,
		"sh", "-c", "test -s "+names.ProfileMountPath+"/trace.jsonl.gz")

	// The provisioner is the shipped exporter (no wrapper): it stages inside the
	// profile volume and removes its staging directory, so the volume holds the two
	// published documents and nothing else.
	entries := strings.Fields(s.execInPod(t, pod, names.FrontendContainerName,
		"sh", "-c", "ls -A "+names.ProfileMountPath))
	sort.Strings(entries)
	if want := []string{names.ProfileMetadataFileName, "trace.jsonl.gz"}; !slices.Equal(entries, want) {
		t.Errorf("the profile volume holds %v, want exactly %v (leftover staging directories or a partial export)",
			entries, want)
	}

	// The supervisor writes its ready file only while every readiness input holds.
	s.execInPod(t, pod, names.FrontendContainerName, "sh", "-c", "test -f /tmp/vcr-runtime/ready")

	// The simulator reads the provisioned trace, which proves the ordering the
	// contract requires: provisioning completed before the simulator started.
	logs, err := s.kubectl("-n", pod.Namespace, "logs", pod.Name, "-c", names.BackendContainerName, "--tail=100")
	if err != nil {
		t.Fatalf("read the backend logs: %v", err)
	}
	if !strings.Contains(logs, names.ProfileMountPath+"/trace.jsonl.gz") {
		t.Errorf("the simulator logs do not show the provisioned trace path %s: %s",
			names.ProfileMountPath, truncate(logs, 500))
	}
	if !strings.Contains(logs, "registered with the engine-core handshake") {
		t.Errorf("the simulator logs do not show the engine-core registration: %s", truncate(logs, 500))
	}

	// The inference surface only answers once the engine registered.
	models := s.execInPod(t, pod, names.FrontendContainerName,
		"wget", "-qO-", fmt.Sprintf("http://127.0.0.1:%d/v1/models", names.DefaultInferencePort))
	if !strings.Contains(models, "Qwen/Qwen3-0.6B") {
		t.Errorf("/v1/models does not report the served model: %s", truncate(models, 300))
	}

	stream := s.execInPod(t, pod, names.FrontendContainerName, "sh", "-c",
		fmt.Sprintf(`wget -qO- --header='Content-Type: application/json' --post-data='{"model":"Qwen/Qwen3-0.6B","messages":[{"role":"user","content":"hello"}],"stream":true}' http://127.0.0.1:%d/v1/chat/completions`, names.DefaultInferencePort))
	if !strings.Contains(stream, "data: ") || !strings.Contains(stream, "[DONE]") {
		t.Errorf("/v1/chat/completions did not stream server-sent events: %s", truncate(stream, 300))
	}

	// The KV-event port the workload configures is served by the simulator.
	kvPort := fmt.Sprintf("%d", names.KVEventsPort)
	s.execInPod(t, pod, names.BackendContainerName, "sh", "-c",
		"test -n \"$(netstat -ltn | grep :"+kvPort+")\"")
}

// stepCapacity asserts allocation, exhaustion, and release: eight replicas each
// request one synthetic device while the workloads step already holds one, so
// exactly one replica stays Pending with an insufficient-resource event; deleting
// the earlier Pod releases the device and the pending replica schedules.
func (s *suite) stepCapacity(t *testing.T) {
	ctx := t.Context()
	namespace := capacityNamespace
	s.prepareWorkloadNamespace(t, namespace)
	s.dumpOnFailure(t, "capacity", namespace)

	deployment := modelserverDeployment(namespace, workloadOptions{
		name:     "modelserver-capacity",
		replicas: 8,
	})
	if err := s.client.Create(ctx, deployment); err != nil {
		t.Fatalf("create the capacity Deployment: %v", err)
	}

	// Capacity is 8 devices and the workload step holds one, so seven replicas can
	// schedule and one must wait.
	err := waitFor(ctx, 4*time.Minute, 3*time.Second, func(ctx context.Context) (bool, string, error) {
		pods, err := s.podsOf(ctx, namespace, "app=modelserver-capacity")
		if err != nil {
			return false, "", err
		}
		running, pending := 0, 0
		for i := range pods {
			if pods[i].Spec.NodeName != "" {
				running++
				continue
			}
			pending++
		}
		return running == 7 && pending == 1, fmt.Sprintf("%d scheduled, %d pending", running, pending), nil
	})
	if err != nil {
		s.dumpClusterState(t, "capacity")
		t.Fatalf("capacity exhaustion did not converge: %v", err)
	}

	var pendingPod *corev1.Pod
	pods, err := s.podsOf(ctx, namespace, "app=modelserver-capacity")
	if err != nil {
		t.Fatalf("list the capacity Pods: %v", err)
	}
	for i := range pods {
		if pods[i].Spec.NodeName == "" {
			pendingPod = pods[i].DeepCopy()
			break
		}
	}
	if pendingPod == nil {
		t.Fatal("no Pending Pod found although the two counts said one was pending")
	}
	var event string
	err = waitFor(ctx, 90*time.Second, 2*time.Second, func(ctx context.Context) (bool, string, error) {
		var events corev1.EventList
		if err := s.client.List(ctx, &events, client.InNamespace(namespace)); err != nil {
			return false, "", err
		}
		for _, item := range events.Items {
			if item.InvolvedObject.Kind != "Pod" || item.InvolvedObject.Name != pendingPod.Name {
				continue
			}
			if item.Reason == "FailedScheduling" && strings.Contains(item.Message, names.SyntheticResourceName) {
				event = item.Message
				return true, "", nil
			}
		}
		return false, "no insufficient-resource event for the pending Pod yet", nil
	})
	if err != nil {
		s.dumpClusterState(t, "capacity")
		t.Fatalf("the Pending Pod has no insufficient-resource event: %v", err)
	}
	if !strings.Contains(event, "Insufficient "+names.SyntheticResourceName) {
		t.Errorf("scheduler message does not name the exhausted synthetic resource: %q", event)
	}
	t.Logf("exhaustion: %s", event)

	// Release: the workload step's Pod holds the eighth device. Its Deployment is
	// removed as well, otherwise the ReplicaSet would immediately recreate the Pod and
	// race the pending replica for the same device.
	if err := s.deleteDeploymentAndWait(t, workloadNamespace, "modelserver-admission", "app=modelserver-admission"); err != nil {
		t.Fatalf("delete the workload holding the eighth synthetic device: %v", err)
	}
	err = waitFor(ctx, 3*time.Minute, 3*time.Second, func(ctx context.Context) (bool, string, error) {
		var current corev1.Pod
		if err := s.client.Get(ctx, client.ObjectKeyFromObject(pendingPod), &current); err != nil {
			return false, "", err
		}
		if current.Spec.NodeName == "" {
			return false, "the previously pending Pod is still unscheduled", nil
		}
		return podReady(&current), fmt.Sprintf("scheduled on %s, Ready=%v", current.Spec.NodeName, podReady(&current)), nil
	})
	if err != nil {
		s.dumpClusterState(t, "capacity")
		t.Fatalf("releasing a synthetic device did not let the pending Pod schedule: %v", err)
	}

	allocated, err := s.nodeAllocatableGPU(ctx, s.simulationNode)
	if err != nil {
		t.Fatalf("read allocatable capacity after release: %v", err)
	}
	if allocated != 8 {
		t.Errorf("node allocatable %s = %d, want 8: releasing a device must not change capacity",
			names.SyntheticResourceName, allocated)
	}
}

// stepPluginRestart asserts kubelet owns the allocation state: restarting the
// device plugin restores the advertisement and leaves existing allocations alone.
func (s *suite) stepPluginRestart(t *testing.T) {
	ctx := t.Context()
	s.dumpOnFailure(t, "plugin-restart", capacityNamespace)

	before, err := s.podsOf(ctx, capacityNamespace, "app=modelserver-capacity")
	if err != nil {
		t.Fatalf("list the running capacity Pods: %v", err)
	}
	var subject *corev1.Pod
	for i := range before {
		if before[i].Spec.NodeName != "" && podReady(&before[i]) {
			subject = before[i].DeepCopy()
			break
		}
	}
	if subject == nil {
		t.Fatal("no Ready capacity Pod to observe across a plugin restart")
	}
	frontendRestarts := containerStatus(subject, names.FrontendContainerName)

	var oldPlugin corev1.PodList
	if err := s.client.List(ctx, &oldPlugin, client.InNamespace(operatorNamespace),
		client.MatchingLabels(devicePluginPodLabels())); err != nil {
		t.Fatalf("list the device plugin Pods: %v", err)
	}
	if len(oldPlugin.Items) != 1 {
		t.Fatalf("%d device plugin Pods before the restart, want 1", len(oldPlugin.Items))
	}
	oldUID := oldPlugin.Items[0].UID
	if _, err := s.kubectl("-n", operatorNamespace, "delete", "pod", oldPlugin.Items[0].Name, "--wait=false"); err != nil {
		t.Fatalf("delete the device plugin Pod: %v", err)
	}

	var newPlugin corev1.Pod
	err = waitFor(ctx, 3*time.Minute, 2*time.Second, func(ctx context.Context) (bool, string, error) {
		var pods corev1.PodList
		if err := s.client.List(ctx, &pods, client.InNamespace(operatorNamespace),
			client.MatchingLabels(devicePluginPodLabels())); err != nil {
			return false, "", err
		}
		for i := range pods.Items {
			candidate := pods.Items[i]
			if candidate.UID == oldUID {
				continue
			}
			if podReady(&candidate) {
				newPlugin = candidate
				return true, "", nil
			}
			return false, "the replacement device plugin Pod is not Ready yet", nil
		}
		return false, "the replacement device plugin Pod has not been created yet", nil
	})
	if err != nil {
		s.dumpClusterState(t, "plugin-restart")
		t.Fatalf("the device plugin did not recover after its Pod was deleted: %v", err)
	}
	t.Logf("device plugin recovered as Pod %s on %s", newPlugin.Name, newPlugin.Spec.NodeName)

	err = waitFor(ctx, 2*time.Minute, 2*time.Second, func(ctx context.Context) (bool, string, error) {
		capacity, err := s.nodeAllocatableGPU(ctx, s.simulationNode)
		if err != nil {
			return false, "", err
		}
		return capacity == 8, fmt.Sprintf("allocatable %s = %d", names.SyntheticResourceName, capacity), nil
	})
	if err != nil {
		t.Fatalf("allocatable capacity did not return after the plugin restart: %v", err)
	}

	var after corev1.Pod
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(subject), &after); err != nil {
		t.Fatalf("read the observed Pod after the restart: %v", err)
	}
	if !podReady(&after) {
		t.Errorf("Pod %s lost readiness across a device-plugin restart: kubelet owns the allocation, not the plugin", after.Name)
	}
	if status := containerStatus(&after, names.FrontendContainerName); status != nil && frontendRestarts != nil {
		if status.RestartCount != frontendRestarts.RestartCount {
			t.Errorf("the frontend container restarted (%d -> %d) across a device-plugin restart: existing allocations must be unaffected",
				frontendRestarts.RestartCount, status.RestartCount)
		}
	}
}

// stepReadinessWithdrawal asserts the failure behavior the contract requires: when
// the simulator stops answering, the Pod becomes not ready instead of staying
// healthy, the supervisor terminates the frontend rather than leaving an engine-less
// server running, kubelet's restart policy is what recovers it, and readiness returns
// only once an engine is registered again.
//
// The failure is injected through the simulator double's test hooks (a simulator that
// stays alive but no longer serves). That is deliberately not a Pod deletion: the
// contract's readiness input is the control API, and deleting the container would
// test kubelet rather than the supervisor. The simulator container's own restart
// policy is asserted at the end by crashing it through its hook.
func (s *suite) stepReadinessWithdrawal(t *testing.T) {
	ctx := t.Context()
	s.dumpOnFailure(t, "readiness-withdrawal", capacityNamespace)

	pods, err := s.podsOf(ctx, capacityNamespace, "app=modelserver-capacity")
	if err != nil {
		t.Fatalf("list the capacity Pods: %v", err)
	}
	var subject *corev1.Pod
	for i := range pods {
		if pods[i].Spec.NodeName != "" && podReady(&pods[i]) {
			subject = pods[i].DeepCopy()
			break
		}
	}
	if subject == nil {
		t.Fatal("no Ready capacity Pod to withdraw readiness from")
	}
	t.Logf("withdrawing readiness of Pod %s on %s by making its simulator unavailable", subject.Name, subject.Spec.NodeName)

	// Both containers share the Pod network namespace, so the simulator's control API
	// is reachable from the frontend container on loopback.
	controlURL := fmt.Sprintf("http://127.0.0.1:%d", names.ControlPort)
	s.execInPod(t, subject, names.FrontendContainerName, "wget", "-qO-", controlURL+"/stub/unavailable")

	err = waitFor(ctx, 60*time.Second, 2*time.Second, func(ctx context.Context) (bool, string, error) {
		var current corev1.Pod
		if err := s.client.Get(ctx, client.ObjectKeyFromObject(subject), &current); err != nil {
			return false, "", err
		}
		condition := podCondition(&current, corev1.PodReady)
		if condition == nil {
			return false, "no Ready condition", nil
		}
		return condition.Status == corev1.ConditionFalse, fmt.Sprintf("Ready=%s (%s: %s)",
			condition.Status, condition.Reason, condition.Message), nil
	})
	if err != nil {
		s.dumpClusterState(t, "readiness-withdrawal")
		t.Fatalf("the Pod stayed Ready after its simulator stopped answering: %v", err)
	}

	// It stays not ready: the simulator is still unavailable, so the supervisor must
	// never report an engine-less server as ready again.
	time.Sleep(8 * time.Second)
	var duringOutage corev1.Pod
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(subject), &duringOutage); err != nil {
		t.Fatalf("read the Pod during the outage: %v", err)
	}
	if condition := podCondition(&duringOutage, corev1.PodReady); condition == nil || condition.Status == corev1.ConditionTrue {
		t.Errorf("the Pod became Ready again while its simulator is still unavailable: %+v", condition)
	}

	// The supervisor terminates the frontend once the backend loss exceeds its grace
	// period, and kubelet's restart policy is what brings the frontend container back.
	// The simulator is never restarted by the supervisor: it is kubelet's container,
	// not the supervisor's child.
	err = waitFor(ctx, 120*time.Second, 2*time.Second, func(ctx context.Context) (bool, string, error) {
		var current corev1.Pod
		if err := s.client.Get(ctx, client.ObjectKeyFromObject(subject), &current); err != nil {
			return false, "", err
		}
		return restartCount(&current, names.FrontendContainerName) > 0, fmt.Sprintf(
			"frontend restarts %d, simulator restarts %d",
			restartCount(&current, names.FrontendContainerName),
			restartCount(&current, names.BackendContainerName)), nil
	})
	if err != nil {
		s.dumpClusterState(t, "readiness-withdrawal")
		t.Fatalf("the supervisor did not terminate the frontend after the simulator was lost: %v", err)
	}
	if got := restartCount(&duringOutage, names.BackendContainerName); got != 0 {
		t.Errorf("the simulator container restarted %d time(s) during the outage: the supervisor must not resurrect the simulator", got)
	}
	t.Logf("frontend restart count %d, simulator restart count %d: the supervisor terminated the frontend and kubelet restarted it",
		restartCount(&duringOutage, names.FrontendContainerName), restartCount(&duringOutage, names.BackendContainerName))

	// Recovery is restart-based: with the engine serving again, the restarted frontend
	// registers and readiness returns.
	s.execInPodEventually(t, subject, names.FrontendContainerName, "wget", "-qO-", controlURL+"/stub/available")
	err = waitFor(ctx, 2*time.Minute, 2*time.Second, func(ctx context.Context) (bool, string, error) {
		var current corev1.Pod
		if err := s.client.Get(ctx, client.ObjectKeyFromObject(subject), &current); err != nil {
			return false, "", err
		}
		return podReady(&current), fmt.Sprintf("phase %s, Ready=%v", current.Status.Phase, podReady(&current)), nil
	})
	if err != nil {
		s.dumpClusterState(t, "readiness-withdrawal")
		t.Fatalf("the Pod did not become Ready again after the simulator recovered: %v", err)
	}

	// Finally, crash the simulator and observe the documented recovery: the
	// simulator process is restarted in place by its POSIX-shell wrapper, so the
	// container (and the Pod) are not replaced. This is what absorbs a cold start
	// without paying kubelet's restart backoff; the cost is that a crash does not
	// show up as a container restart count.
	var beforeCrash corev1.Pod
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(subject), &beforeCrash); err != nil {
		t.Fatalf("read the Pod before crashing the simulator: %v", err)
	}
	uidBefore := beforeCrash.UID
	restartsBefore := restartCount(&beforeCrash, names.BackendContainerName)
	s.execInPodIgnoringError(t, subject, names.FrontendContainerName, "wget", "-qO-", controlURL+"/stub/exit")

	// The restarted simulator serves its control API again: in-place recovery is
	// observable without the container being recreated.
	s.execInPodEventually(t, subject, names.BackendContainerName, "wget", "-qO-", controlURL+"/stats")
	var afterCrash corev1.Pod
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(subject), &afterCrash); err != nil {
		t.Fatalf("read the Pod after the crash: %v", err)
	}
	if afterCrash.UID != uidBefore {
		t.Errorf("the Pod was replaced (%s -> %s): the wrapper restarts the simulator process, not the Pod", uidBefore, afterCrash.UID)
	}
	if got := restartCount(&afterCrash, names.BackendContainerName); got != restartsBefore {
		t.Errorf("simulator container restarts %d (was %d): the wrapper restarts the process in place", got, restartsBefore)
	}

	if err := s.deleteNamespaceNow(t, capacityNamespace); err != nil {
		t.Errorf("delete the capacity namespace: %v", err)
	}
}

// stepCleanup removes everything the run created and asserts no cluster-scoped
// residue remains, which is what makes the suite re-runnable on the same cluster.
func (s *suite) stepCleanup(t *testing.T) {
	t.Helper()
	ctx := t.Context()

	for _, namespace := range []string{workloadNamespace, capacityNamespace, placementNamespace} {
		if err := s.deleteNamespaceNow(t, namespace); err != nil {
			t.Errorf("delete namespace %s: %v", namespace, err)
		}
	}

	// The SimulationEnvironment is cluster scoped; deleting it removes the device
	// plugin DaemonSet and its ConfigMap through ownership.
	if err := s.client.Delete(ctx, newSimulationEnvironment("")); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("delete the SimulationEnvironment: %v", err)
	}
	if err := waitFor(ctx, 2*time.Minute, 2*time.Second, func(ctx context.Context) (bool, string, error) {
		var list vcrv1alpha1.SimulationEnvironmentList
		if err := s.client.List(ctx, &list); err != nil {
			return false, "", err
		}
		return len(list.Items) == 0, fmt.Sprintf("%d SimulationEnvironment(s) remain", len(list.Items)), nil
	}); err != nil {
		t.Errorf("the SimulationEnvironment was not removed: %v", err)
	}

	documents, err := s.kustomizeBuild("config/default")
	if err != nil {
		t.Errorf("rebuild config/default for the uninstall: %v", err)
	} else {
		var stream strings.Builder
		for _, document := range documents {
			switch document.GetKind() {
			case "Certificate", "Issuer":
				continue
			}
			encoded, err := yamlMarshal(document.Object)
			if err != nil {
				t.Errorf("encode %s/%s: %v", document.GetKind(), document.GetName(), err)
				continue
			}
			stream.WriteString("---\n")
			stream.WriteString(encoded)
		}
		if _, err := s.kubectlInput([]byte(stream.String()), "delete", "-f", "-", "--ignore-not-found"); err != nil {
			t.Errorf("uninstall the operator: %v", err)
		}
	}
	if err := s.deleteNamespaceNow(t, registryNamespace); err != nil {
		t.Errorf("delete the registry namespace: %v", err)
	}

	// Residue: nothing cluster scoped may survive the run.
	if _, err := s.kubectl("get", "mutatingwebhookconfiguration", "vllm-vcr-operator-mutating-webhook"); err == nil {
		t.Error("the mutating webhook configuration survived the uninstall")
	}
	if _, err := s.kubectl("get", "validatingwebhookconfiguration", "vllm-vcr-operator-validating-webhook"); err == nil {
		t.Error("the validating webhook configuration survived the uninstall")
	}
	if _, err := s.kubectl("get", "crd", "simulationenvironments.vcr.opendatahub-io"); err == nil {
		t.Error("the SimulationEnvironment CRD survived the uninstall")
	}
	for _, namespace := range []string{operatorNamespace, registryNamespace, workloadNamespace, capacityNamespace, placementNamespace} {
		var current corev1.Namespace
		err := s.client.Get(ctx, client.ObjectKey{Name: namespace}, &current)
		if err == nil {
			t.Errorf("namespace %s survived the uninstall", namespace)
		} else if !apierrors.IsNotFound(err) {
			t.Errorf("read namespace %s: %v", namespace, err)
		}
	}
	if s.runtimeEndpointLeft(t) {
		t.Errorf("the test registry is still reachable on node %s after teardown", s.otherNode)
	}
}

// runtimeEndpointLeft reports whether the removed registry still accepts
// connections, which would mean a Pod outlived its namespace.
func (s *suite) runtimeEndpointLeft(t *testing.T) bool {
	t.Helper()
	if s.install.registryHost == "" {
		return false
	}
	out, err := s.nodeExec(s.otherNode, "sh", "-c",
		fmt.Sprintf("curl -sS --max-time 2 -o /dev/null https://%s/v2/ >/dev/null 2>&1 && echo up || echo down", s.install.registryHost))
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "up"
}

// teardown is the failure-safe cleanup: it runs even when a step failed, so a run
// never leaves workloads, allocations, or admission registrations behind.
func (s *suite) teardown(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	for _, namespace := range []string{workloadNamespace, capacityNamespace, placementNamespace, registryNamespace} {
		var current corev1.Namespace
		if err := s.client.Get(ctx, client.ObjectKey{Name: namespace}, &current); err == nil {
			_ = s.client.Delete(ctx, &current)
		}
	}
	_ = s.client.Delete(ctx, newSimulationEnvironment(""))
	for _, configuration := range []string{
		"mutatingwebhookconfiguration/vllm-vcr-operator-mutating-webhook",
		"validatingwebhookconfiguration/vllm-vcr-operator-validating-webhook",
	} {
		_, _ = s.kubectl("delete", configuration, "--ignore-not-found")
	}
	_, _ = s.kubectl("delete", "runtimeclass", "nvidia", "--ignore-not-found")
	if _, err := s.kubectl("delete", "-n", operatorNamespace, "deployment",
		"vllm-vcr-operator-controller-manager", "--ignore-not-found"); err != nil {
		t.Logf("teardown: %v", err)
	}
	if _, err := s.kubectl("label", "node", s.simulationNode, "vcr.opendatahub-io/simulation-"); err != nil {
		t.Logf("teardown: remove the simulation node label: %v", err)
	}
}

// artifactsRoot resolves where diagnostics are written and makes the failure mode
// visible: the path is inside the repository and gitignored by a nested .gitignore.
func artifactsRoot(t *testing.T, environment *environment) string {
	t.Helper()
	root := os.Getenv(EnvArtifactsDir)
	if root == "" {
		root = defaultArtifacts
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(environment.root, root)
	}
	return root
}
