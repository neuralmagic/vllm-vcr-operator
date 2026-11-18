package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
)

// devicePluginPodLabels selects the device-plugin Pods by the labels the
// operator puts on them (internal/controller). Selecting by name alone would miss
// them: the Pods are named <daemonset>-<suffix>.
func devicePluginPodLabels() map[string]string {
	return map[string]string{
		names.ManagedByLabel:            names.OperatorName,
		names.LabelPrefix + "component": "device-plugin",
	}
}

// devicePluginPodSelector renders devicePluginPodLabels as a kubectl selector.
func devicePluginPodSelector() string {
	entries := make([]string, 0, 2)
	for key, value := range devicePluginPodLabels() {
		entries = append(entries, key+"="+value)
	}
	sort.Strings(entries)
	return strings.Join(entries, ",")
}

// yamlMarshal encodes an object as YAML, matching what kubectl apply consumes.
func yamlMarshal(object any) (string, error) {
	encoded, err := yaml.Marshal(object)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// prepareWorkloadNamespace creates the namespace and ServiceAccount the modelserver
// fixtures expect. A real llm-d deployment brings both; the suite does the same so
// the workload it submits is the unchanged manifest rather than a reduced one.
func (s *suite) prepareWorkloadNamespace(t *testing.T, namespace string) {
	t.Helper()
	ctx := t.Context()

	namespaceObject := newNamespace(namespace)
	if err := s.client.Create(ctx, namespaceObject); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %s: %v", namespace, err)
	}
	serviceAccount := newServiceAccount(namespace, modelserverServiceAcc)
	if err := s.client.Create(ctx, serviceAccount); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create ServiceAccount %s/%s: %v", namespace, modelserverServiceAcc, err)
	}
}

// ensureAcceleratorRuntimeClass installs the accelerator runtime class the workload
// asks for.
//
// The API server rejects a Pod that names a RuntimeClass that does not exist, before
// any admission webhook runs, so a workload with an unresolvable runtime class never
// reaches the operator. Installing the class (with the real accelerator handler
// name) is what makes "admission removed the runtime class" an assertion rather than
// an untestable claim: if the rewrite did not happen, the Pod would fail on a node
// with no nvidia runtime handler.
func (s *suite) ensureAcceleratorRuntimeClass(t *testing.T, name string) {
	t.Helper()
	ctx := t.Context()

	runtimeClass := &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Handler:    name,
	}
	if err := s.client.Create(ctx, runtimeClass); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create RuntimeClass %s: %v", name, err)
	}
}

// deleteNamespace removes a namespace and ignores the failure, for t.Cleanup paths
// where a cleanup error must not mask the assertion failure that caused it.
func (s *suite) deleteNamespace(t *testing.T, namespace string) {
	t.Helper()
	if err := s.deleteNamespaceNow(t, namespace); err != nil {
		t.Logf("cleanup: delete namespace %s: %v", namespace, err)
	}
}

// deleteNamespaceNow deletes a namespace and waits for it to disappear, so the next
// step cannot observe a terminating workload that still holds synthetic devices.
func (s *suite) deleteNamespaceNow(t *testing.T, namespace string) error {
	t.Helper()
	// A fresh context, not t.Context(): since Go 1.24 the test context is cancelled
	// before Cleanup functions run, which is exactly when this is called on failure.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var current corev1.Namespace
	err := s.client.Get(ctx, client.ObjectKey{Name: namespace}, &current)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.client.Delete(ctx, &current); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return waitFor(ctx, 2*time.Minute, time.Second, func(ctx context.Context) (bool, string, error) {
		var namespaceObject corev1.Namespace
		err := s.client.Get(ctx, client.ObjectKey{Name: namespace}, &namespaceObject)
		if apierrors.IsNotFound(err) {
			return true, "", nil
		}
		if err != nil {
			return false, "", err
		}
		return false, "namespace " + namespace + " is still terminating", nil
	})
}

// podsOf lists the Pods matching a label selector in a namespace.
func (s *suite) podsOf(ctx context.Context, namespace, selector string) ([]corev1.Pod, error) {
	parsed, err := labels.Parse(selector)
	if err != nil {
		return nil, fmt.Errorf("parse selector %q: %w", selector, err)
	}
	var pods corev1.PodList
	if err := s.client.List(ctx, &pods, client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: parsed}); err != nil {
		return nil, err
	}
	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	return pods.Items, nil
}

// waitForPod waits until at least one Pod matches the selector and returns it.
func (s *suite) waitForPod(t *testing.T, namespace, selector string, timeout time.Duration) *corev1.Pod {
	t.Helper()
	ctx := t.Context()

	var found corev1.Pod
	err := waitFor(ctx, timeout, 2*time.Second, func(ctx context.Context) (bool, string, error) {
		if rejection, err := s.podCreationRejection(ctx, namespace); err != nil {
			return false, "", err
		} else if rejection != "" {
			return false, "", fmt.Errorf("the API server rejected the Pod the operator generated: %s", rejection)
		}
		pods, err := s.podsOf(ctx, namespace, selector)
		if err != nil {
			return false, "", err
		}
		for i := range pods {
			if pods[i].Annotations == nil {
				continue
			}
			found = pods[i]
			return true, "", nil
		}
		return false, "no admitted Pod matches " + selector, nil
	})
	if err != nil {
		s.dumpNamespace(t, "wait", namespace)
		t.Fatalf("waiting for a Pod matching %q in %s: %v", selector, namespace, err)
	}
	return &found
}

// waitForReadyPod waits for a Ready Pod matching the selector: the state in which
// admission, provisioning, and readiness have all happened.
func (s *suite) waitForReadyPod(t *testing.T, namespace, selector string) *corev1.Pod {
	t.Helper()
	ctx := t.Context()

	var ready corev1.Pod
	err := waitFor(ctx, 4*time.Minute, 2*time.Second, func(ctx context.Context) (bool, string, error) {
		if rejection, err := s.podCreationRejection(ctx, namespace); err != nil {
			return false, "", err
		} else if rejection != "" {
			return false, "", fmt.Errorf("the API server rejected the Pod the operator generated: %s", rejection)
		}
		pods, err := s.podsOf(ctx, namespace, selector)
		if err != nil {
			return false, "", err
		}
		states := make([]string, 0, len(pods))
		for i := range pods {
			pod := pods[i]
			states = append(states, fmt.Sprintf("%s:%s ready=%v", pod.Name, pod.Status.Phase, podReady(&pod)))
			if podReady(&pod) {
				ready = pod
				return true, "", nil
			}
		}
		if len(states) == 0 {
			return false, "no Pod matches " + selector, nil
		}
		return false, strings.Join(states, ", "), nil
	})
	if err != nil {
		s.dumpNamespace(t, "wait", namespace)
		t.Fatalf("waiting for a Ready Pod matching %q in %s: %v", selector, namespace, err)
	}
	return &ready
}

// podCreationRejection returns the API server's rejection message for a workload in
// the namespace, if any.
//
// A Pod that the API server rejects as invalid is never created, so polling for it
// would only ever time out. Surfacing the server's own message turns that into an
// immediate failure that names the problem: the operator's webhook produced a Pod
// spec the API server will not accept.
func (s *suite) podCreationRejection(ctx context.Context, namespace string) (string, error) {
	var events corev1.EventList
	if err := s.client.List(ctx, &events, client.InNamespace(namespace)); err != nil {
		return "", err
	}
	for _, event := range events.Items {
		if event.InvolvedObject.Kind != "ReplicaSet" || event.Reason != "FailedCreate" {
			continue
		}
		if strings.Contains(event.Message, "is invalid") || strings.Contains(event.Message, "forbidden") {
			return event.Message, nil
		}
	}
	return "", nil
}

// execInPod runs a command in a container and fails the test when it exits non-zero.
func (s *suite) execInPod(t *testing.T, pod *corev1.Pod, container string, args ...string) string {
	t.Helper()
	full := append([]string{"-n", pod.Namespace, "exec", pod.Name, "-c", container, "--"}, args...)
	out, err := s.kubectl(full...)
	if err != nil {
		t.Fatalf("kubectl exec %s/%s (%s) %v: %v", pod.Namespace, pod.Name, container, args, err)
	}
	return out
}

// dumpOnFailure writes the namespace diagnostics when the step fails, which is the
// only moment the evidence still exists.
func (s *suite) dumpOnFailure(t *testing.T, step, namespace string) {
	t.Cleanup(func() {
		if t.Failed() {
			s.dumpNamespace(t, step, namespace)
		}
	})
}

// execInPodIgnoringError runs a command in a container once and ignores the exit
// status, for hooks whose command succeeds by making its own container fail.
func (s *suite) execInPodIgnoringError(t *testing.T, pod *corev1.Pod, container string, args ...string) {
	t.Helper()
	full := append([]string{"-n", pod.Namespace, "exec", pod.Name, "-c", container, "--"}, args...)
	if _, err := s.kubectl(full...); err != nil {
		t.Logf("exec %s/%s (%s) %v reported: %v", pod.Namespace, pod.Name, container, args, err)
	}
}

// execInPodEventually runs a command in a container, retrying while the container is
// being (re)started.
//
// After the supervisor terminates the frontend, the container is briefly absent and
// kubectl exec fails with "container not found". A readiness/restart assertion must
// not turn that normal transition into a test failure, so the call is retried within a
// bounded window and still fails when the command never succeeds.
func (s *suite) execInPodEventually(t *testing.T, pod *corev1.Pod, container string, args ...string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	var last error
	for time.Now().Before(deadline) {
		full := append([]string{"-n", pod.Namespace, "exec", pod.Name, "-c", container, "--"}, args...)
		out, err := s.kubectl(full...)
		if err == nil {
			return out
		}
		last = err
		time.Sleep(time.Second)
	}
	t.Fatalf("kubectl exec %s/%s (%s) %v did not succeed: %v", pod.Namespace, pod.Name, container, args, last)
	return ""
}

// deleteDeploymentAndWait removes a Deployment and waits for its Pods to disappear.
//
// Deleting only the Pod would let the ReplicaSet recreate it and re-take the
// synthetic device the release assertion depends on, so the declaration goes too.
func (s *suite) deleteDeploymentAndWait(t *testing.T, namespace, name, selector string) error {
	t.Helper()
	ctx := t.Context()

	deployment := &appsv1.Deployment{}
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, deployment); err != nil {
		return err
	}
	if err := s.client.Delete(ctx, deployment, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		return err
	}
	return waitFor(ctx, 2*time.Minute, time.Second, func(ctx context.Context) (bool, string, error) {
		pods, err := s.podsOf(ctx, namespace, selector)
		if err != nil {
			return false, "", err
		}
		return len(pods) == 0, fmt.Sprintf("%d Pod(s) still exist", len(pods)), nil
	})
}

// assertNonGPUPodUntouched asserts admission returns a Pod without a GPU request
// unchanged: no provenance annotation, no injected container, no rewritten
// placement.
func (s *suite) assertNonGPUPodUntouched(t *testing.T, namespace string) {
	t.Helper()
	ctx := t.Context()

	pod := plainPod(namespace, "plain-workload", s.images.frontendStub)
	if err := s.client.Create(ctx, pod); err != nil {
		t.Fatalf("create the non-GPU Pod: %v", err)
	}
	if err := waitFor(ctx, 2*time.Minute, 2*time.Second, func(ctx context.Context) (bool, string, error) {
		var current corev1.Pod
		if err := s.client.Get(ctx, client.ObjectKeyFromObject(pod), &current); err != nil {
			return false, "", err
		}
		return current.Spec.NodeName != "", fmt.Sprintf("phase %s", current.Status.Phase), nil
	}); err != nil {
		t.Fatalf("the non-GPU Pod did not schedule: %v", err)
	}

	var admitted corev1.Pod
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(pod), &admitted); err != nil {
		t.Fatalf("read the non-GPU Pod: %v", err)
	}
	if value, ok := admitted.Annotations[names.InjectedAnnotation]; ok {
		t.Errorf("a non-GPU Pod carries %s=%q: Pods without a GPU request must be returned unchanged", names.InjectedAnnotation, value)
	}
	if len(admitted.Spec.Containers) != 1 {
		t.Errorf("the non-GPU Pod has %d containers, want the submitted 1: %v", len(admitted.Spec.Containers), containerNames(&admitted))
	}
	if len(admitted.Spec.InitContainers) != 0 {
		t.Errorf("the non-GPU Pod gained init containers: %v", admitted.Spec.InitContainers)
	}
	if admitted.Spec.NodeSelector != nil {
		t.Errorf("the non-GPU Pod's node selector was rewritten: %v", admitted.Spec.NodeSelector)
	}
}

// assertUnsupportedWorkloadDenied asserts an unresolvable invocation is rejected
// with an actionable message that the submitting user can see on the ReplicaSet.
func (s *suite) assertUnsupportedWorkloadDenied(t *testing.T, namespace string) {
	t.Helper()
	ctx := t.Context()

	deployment := modelserverDeployment(namespace, workloadOptions{
		name:         "modelserver-shell-wrapper",
		shellWrapper: true,
	})
	if err := s.client.Create(ctx, deployment); err != nil {
		t.Fatalf("create the unsupported Deployment: %v", err)
	}
	defer func() {
		_ = s.client.Delete(context.Background(), deployment)
	}()

	var message string
	err := waitFor(ctx, 2*time.Minute, 2*time.Second, func(ctx context.Context) (bool, string, error) {
		var events corev1.EventList
		if err := s.client.List(ctx, &events, client.InNamespace(namespace)); err != nil {
			return false, "", err
		}
		for _, event := range events.Items {
			if event.InvolvedObject.Kind != "ReplicaSet" {
				continue
			}
			if event.Reason != "FailedCreate" {
				continue
			}
			if strings.Contains(event.Message, "shell wrapper") {
				message = event.Message
				return true, "", nil
			}
		}
		return false, "no admission rejection is visible on the ReplicaSet yet", nil
	})
	if err != nil {
		s.dumpNamespace(t, "admission-denied", namespace)
		t.Fatalf("the unsupported workload was not denied with a visible rejection: %v", err)
	}
	t.Logf("unsupported workload denied: %s", truncate(message, 300))

	pods, err := s.podsOf(ctx, namespace, "app=modelserver-shell-wrapper")
	if err != nil {
		t.Fatalf("list the rejected workload's Pods: %v", err)
	}
	if len(pods) != 0 {
		t.Fatalf("%d Pod(s) were created for a denied workload; admission must reject, not admit untranslated", len(pods))
	}
}

// containerNames lists a Pod's container names, for diagnostics.
func containerNames(pod *corev1.Pod) []string {
	names := make([]string, 0, len(pod.Spec.Containers))
	for _, container := range pod.Spec.Containers {
		names = append(names, container.Name)
	}
	return names
}

// envValue reads an environment entry from a container.
func envValue(env []corev1.EnvVar, name string) (string, bool) {
	for _, entry := range env {
		if entry.Name == name {
			return entry.Value, true
		}
	}
	return "", false
}

// nodeAffinityRequires reports whether the Pod's required node affinity asks for
// key=value.
func nodeAffinityRequires(pod *corev1.Pod, key, value string) bool {
	affinity := pod.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil {
		return false
	}
	required := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if required == nil {
		return false
	}
	for _, term := range required.NodeSelectorTerms {
		for _, expression := range term.MatchExpressions {
			if expression.Key != key {
				continue
			}
			for _, candidate := range expression.Values {
				if candidate == value {
					return true
				}
			}
		}
	}
	return false
}

// hasVolume reports whether the Pod declares a volume with that name.
func hasVolume(pod *corev1.Pod, name string) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == name {
			return true
		}
	}
	return false
}

// hasPort reports whether a container declares a container port with that number.
func hasPort(container *corev1.Container, port int32) bool {
	for _, candidate := range container.Ports {
		if candidate.ContainerPort == port {
			return true
		}
	}
	return false
}

// restartCount returns a container's restart count, or 0 when it has no status yet.
func restartCount(pod *corev1.Pod, container string) int32 {
	if status := containerStatus(pod, container); status != nil {
		return status.RestartCount
	}
	return 0
}

// truncate shortens a diagnostic string for an assertion message.
func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// tagOf returns the tag of an image reference ("repo:tag" -> "tag"), or "".
func tagOf(image string) string {
	idx := strings.LastIndex(image, ":")
	if idx < 0 || strings.Contains(image[idx:], "/") {
		return ""
	}
	return image[idx+1:]
}

// artifactDir returns (and creates) the directory diagnostics for a step go into.
// Step names become directory names, so a failing step is easy to find.
func (s *suite) artifactDir(t *testing.T, step string) string {
	t.Helper()
	name := strings.NewReplacer(" ", "-", "/", "-", ",", "-").Replace(strings.ToLower(step))
	dir := filepath.Join(s.artifactsRoot, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create the artifacts directory %s: %v", dir, err)
	}
	return dir
}

// dumpClusterState writes everything a failure investigation needs: the
// SimulationEnvironment, node capacity, every test namespace's Pods, describes,
// logs, the injected container arguments, and the operator's logs.
func (s *suite) dumpClusterState(t *testing.T, step string) {
	t.Helper()
	dir := s.artifactDir(t, step)

	s.kubectlToFile(dir, "simulationenvironment.yaml", "get", "simulationenvironment", "default", "-o", "yaml")
	s.kubectlToFile(dir, "nodes.txt", "get", "nodes", "-o", "wide")
	s.kubectlToFile(dir, "node-allocatable.txt", "get", "nodes", "-o",
		`jsonpath={range .items[*]}{.metadata.name}{"\t"}{.status.allocatable.nvidia\.com/gpu}{"\t"}{.status.capacity.nvidia\.com/gpu}{"\n"}{end}`)
	s.kubectlToFile(dir, "operator-logs.txt", "-n", operatorNamespace, "logs",
		"-l", "control-plane=controller-manager", "--tail=800")
	s.kubectlToFile(dir, "device-plugin-pods.txt", "-n", operatorNamespace, "get", "pods", "-o", "wide")
	s.kubectlToFile(dir, "device-plugin-describe.txt", "-n", operatorNamespace, "describe", "pods",
		"-l", "app.kubernetes.io/name="+names.DevicePluginName)

	for _, namespace := range []string{workloadNamespace, capacityNamespace, placementNamespace} {
		s.dumpNamespace(t, step+"-"+namespace, namespace)
	}
	_ = dir
}

// dumpNamespace writes the Pod-level diagnostics for one namespace.
func (s *suite) dumpNamespace(t *testing.T, step, namespace string) {
	t.Helper()
	dir := s.artifactDir(t, step)

	s.kubectlToFile(dir, "pods.yaml", "-n", namespace, "get", "pods", "-o", "yaml")
	s.kubectlToFile(dir, "pods.txt", "-n", namespace, "get", "pods", "-o", "wide")
	s.kubectlToFile(dir, "deployments.yaml", "-n", namespace, "get", "deployments", "-o", "yaml")
	s.kubectlToFile(dir, "events.txt", "-n", namespace, "get", "events", "--sort-by=.lastTimestamp")
	s.kubectlToFile(dir, "replicasets.txt", "-n", namespace, "describe", "replicasets")
	s.kubectlToFile(dir, "injected-args.txt", "-n", namespace, "get", "pods", "-o",
		`jsonpath={range .items[*]}{.metadata.name}{"\n"}{range .spec.initContainers[*]}  init/{.name}: {.command} {.args}{"\n"}{end}{range .spec.containers[*]}  {.name}: {.command} {.args}{"\n"}{end}{end}`)

	// A dump runs after a failure, when the test context is already cancelled, so
	// it must bring its own deadline.
	dumpCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pods, err := s.podsOf(dumpCtx, namespace, "")
	if err != nil {
		s.logf("dump %s: list pods: %v", namespace, err)
		return
	}
	for _, pod := range pods {
		name := pod.Name
		s.kubectlToFile(dir, "describe-"+name+".txt", "-n", namespace, "describe", "pod", name)
		for _, container := range pod.Spec.Containers {
			s.kubectlToFile(dir, fmt.Sprintf("logs-%s-%s.txt", name, container.Name),
				"-n", namespace, "logs", name, "-c", container.Name, "--tail=300")
			s.kubectlToFile(dir, fmt.Sprintf("logs-%s-%s-previous.txt", name, container.Name),
				"-n", namespace, "logs", name, "-c", container.Name, "--previous", "--tail=300")
		}
		for _, container := range pod.Spec.InitContainers {
			s.kubectlToFile(dir, fmt.Sprintf("logs-%s-init-%s.txt", name, container.Name),
				"-n", namespace, "logs", name, "-c", container.Name, "--tail=300")
		}
	}
}
