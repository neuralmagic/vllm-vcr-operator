package controller_test

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/controller"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/deviceplugin"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
)

const testDevicePluginImage = "registry.example.test/vllm-vcr-device-plugin:test"

// devicePluginReconciler returns a reconciler wired only to the fake client, so
// no registry, cluster, or network is involved.
func devicePluginReconciler(t *testing.T, objects ...client.Object) (*controller.DevicePluginReconciler, client.Client) {
	t.Helper()
	c := testClient(t, objects...)
	return &controller.DevicePluginReconciler{
		Client:            c,
		OperatorNamespace: testOperatorNamespace,
		Image:             testDevicePluginImage,
	}, c
}

func daemonSetOf(t *testing.T, c client.Client) *appsv1.DaemonSet {
	t.Helper()
	daemonSet := &appsv1.DaemonSet{}
	key := types.NamespacedName{Namespace: testOperatorNamespace, Name: names.DevicePluginName}
	if err := c.Get(context.Background(), key, daemonSet); err != nil {
		t.Fatalf("get device plugin DaemonSet %s: %v", key, err)
	}
	return daemonSet
}

func deviceCountConfigMapOf(t *testing.T, c client.Client) *corev1.ConfigMap {
	t.Helper()
	configMap := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: testOperatorNamespace, Name: controller.DevicePluginConfigMapName}
	if err := c.Get(context.Background(), key, configMap); err != nil {
		t.Fatalf("get device count ConfigMap %s: %v", key, err)
	}
	return configMap
}

// setDaemonSetStatus simulates kubelet and the DaemonSet controller, which are
// absent in unit tests: nothing here is asserted about real scheduling.
func setDaemonSetStatus(t *testing.T, c client.Client, desired, ready int32) {
	t.Helper()
	daemonSet := daemonSetOf(t, c)
	daemonSet.Status = appsv1.DaemonSetStatus{
		ObservedGeneration:     daemonSet.Generation,
		DesiredNumberScheduled: desired,
		NumberReady:            ready,
	}
	if err := c.Status().Update(context.Background(), daemonSet); err != nil {
		t.Fatalf("update DaemonSet status: %v", err)
	}
}

func TestDevicePluginReconcilerCreatesDaemonSetAndDeviceCountConfigMap(t *testing.T) {
	env := testEnvironment()
	reconciler, c := devicePluginReconciler(t, env)

	state, err := reconciler.Reconcile(context.Background(), env)
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	daemonSet := daemonSetOf(t, c)
	if daemonSet.Namespace != testOperatorNamespace {
		t.Errorf("DaemonSet namespace = %q, want the operator namespace %q", daemonSet.Namespace, testOperatorNamespace)
	}

	owner := metav1.GetControllerOf(daemonSet)
	if owner == nil {
		t.Fatal("DaemonSet has no controller owner reference: deleting the environment would leak it")
	}
	if owner.Kind != "SimulationEnvironment" || owner.Name != env.Name {
		t.Errorf("DaemonSet controller owner = %s/%s, want SimulationEnvironment/%s", owner.Kind, owner.Name, env.Name)
	}
	if daemonSet.Labels[names.ManagedByLabel] != names.OperatorName {
		t.Errorf("DaemonSet label %q = %q, want %q", names.ManagedByLabel, daemonSet.Labels[names.ManagedByLabel], names.OperatorName)
	}
	if daemonSet.Labels[names.EnvironmentLabel] != env.Name {
		t.Errorf("DaemonSet label %q = %q, want %q", names.EnvironmentLabel, daemonSet.Labels[names.EnvironmentLabel], env.Name)
	}

	podSpec := daemonSet.Spec.Template.Spec
	if got := podSpec.NodeSelector["vcr.opendatahub-io/simulation"]; got != "true" {
		t.Errorf("DaemonSet node selector = %v, want the simulation node selector", podSpec.NodeSelector)
	}

	if len(podSpec.Containers) != 1 {
		t.Fatalf("DaemonSet has %d containers, want exactly the device plugin", len(podSpec.Containers))
	}
	container := podSpec.Containers[0]
	if container.Name != names.DevicePluginName {
		t.Errorf("container name = %q, want %q", container.Name, names.DevicePluginName)
	}
	if container.Image != testDevicePluginImage {
		t.Errorf("container image = %q, want the injected image %q", container.Image, testDevicePluginImage)
	}

	envVars := map[string]string{}
	for _, envVar := range container.Env {
		envVars[envVar.Name] = envVar.Value
	}
	wantEnv := map[string]string{
		"VCR_DEVICE_PLUGIN_RESOURCE_NAME":     names.SyntheticResourceName,
		"VCR_DEVICE_PLUGIN_DEVICE_COUNT":      "2",
		"VCR_DEVICE_PLUGIN_DEVICE_COUNT_FILE": deviceplugin.DefaultDeviceCountFile,
		"VCR_DEVICE_PLUGIN_SOCKET_DIR":        names.DevicePluginDir,
		"VCR_DEVICE_PLUGIN_ENDPOINT":          names.DevicePluginSocketName,
		"VCR_DEVICE_PLUGIN_HEALTH_PORT":       "8080",
		"VCR_DEVICE_PLUGIN_LOG_LEVEL":         "info",
	}
	for name, want := range wantEnv {
		if envVars[name] != want {
			t.Errorf("container env %s = %q, want %q", name, envVars[name], want)
		}
	}

	if container.ReadinessProbe == nil || container.LivenessProbe == nil {
		t.Fatal("container has no readiness/liveness probe: a registered plugin that stops serving would stay Ready")
	}
	if got := container.ReadinessProbe.HTTPGet.Path; got != deviceplugin.ReadyPath {
		t.Errorf("readiness probe path = %q, want %q", got, deviceplugin.ReadyPath)
	}
	if got := container.LivenessProbe.HTTPGet.Path; got != deviceplugin.HealthPath {
		t.Errorf("liveness probe path = %q, want %q", got, deviceplugin.HealthPath)
	}
	for probe, port := range map[string]int32{
		"readiness": container.ReadinessProbe.HTTPGet.Port.IntVal,
		"liveness":  container.LivenessProbe.HTTPGet.Port.IntVal,
	} {
		if port != int32(deviceplugin.DefaultHealthPort) {
			t.Errorf("%s probe port = %d, want %d", probe, port, deviceplugin.DefaultHealthPort)
		}
	}

	// The plugin only needs to write its socket into kubelet's device-plugin
	// directory: no host devices, no NVIDIA runtime, no privileges.
	if container.SecurityContext == nil {
		t.Fatal("container has no security context")
	}
	if container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
		t.Error("container is privileged: the synthetic plugin needs no host devices")
	}
	if container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
		t.Error("container allows privilege escalation, want it disabled")
	}
	if len(container.SecurityContext.Capabilities.Drop) != 1 || container.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("container drops capabilities %v, want [ALL]", container.SecurityContext.Capabilities.Drop)
	}
	socketVolume := devicePluginVolumeName(t, podSpec, names.DevicePluginDir)
	mountPath, mounted := volumeMountPath(container, socketVolume)
	if !mounted {
		t.Errorf("container does not mount the device plugin socket volume %q", socketVolume)
	} else if mountPath != names.DevicePluginDir {
		t.Errorf("device plugin socket mount path = %q, want %q", mountPath, names.DevicePluginDir)
	}

	configMap := deviceCountConfigMapOf(t, c)
	if got, want := configMap.Data[controller.DeviceCountFileKey], "2"; got != want {
		t.Errorf("device count file content = %q, want %q from spec.devicesPerNode", got, want)
	}
	if owner := metav1.GetControllerOf(configMap); owner == nil || owner.Name != env.Name {
		t.Errorf("device count ConfigMap owner = %+v, want the SimulationEnvironment", owner)
	}

	if state.Desired != 0 || state.Ready != 0 {
		t.Errorf("state = %+v, want an unscheduled DaemonSet to report 0 of 0", state)
	}
}

// devicePluginVolumeName returns the name of the hostPath volume for mountPath.
func devicePluginVolumeName(t *testing.T, podSpec corev1.PodSpec, mountPath string) string {
	t.Helper()
	for _, volume := range podSpec.Volumes {
		if volume.HostPath != nil && volume.HostPath.Path == mountPath {
			return volume.Name
		}
	}
	t.Fatalf("no hostPath volume for %q in %+v", mountPath, podSpec.Volumes)
	return ""
}

func TestDevicePluginReconcilerUpdatesDaemonSetOnSpecChange(t *testing.T) {
	env := testEnvironment()
	reconciler, c := devicePluginReconciler(t, env)

	if _, err := reconciler.Reconcile(context.Background(), env); err != nil {
		t.Fatalf("first Reconcile returned error: %v", err)
	}

	changed := testEnvironment()
	changed.Spec.DevicesPerNode = 8
	changed.Spec.NodeSelection.NodeSelector = map[string]string{"vcr.opendatahub-io/simulation": "true", "kubernetes.io/os": "linux"}
	changed.Spec.NodeSelection.Tolerations = []corev1.Toleration{{
		Key:      "simulation",
		Operator: corev1.TolerationOpEqual,
		Value:    "true",
		Effect:   corev1.TaintEffectNoSchedule,
	}}
	changed.Spec.Runtime.Frontend = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
	}

	state, err := reconciler.Reconcile(context.Background(), changed)
	if err != nil {
		t.Fatalf("second Reconcile returned error: %v", err)
	}
	if state.Desired != 0 {
		t.Errorf("state.Desired = %d after an unscheduled recreate, want 0", state.Desired)
	}

	daemonSets := &appsv1.DaemonSetList{}
	if err := c.List(context.Background(), daemonSets, client.InNamespace(testOperatorNamespace)); err != nil {
		t.Fatalf("list DaemonSets: %v", err)
	}
	if len(daemonSets.Items) != 1 {
		t.Fatalf("found %d DaemonSets, want exactly one device plugin DaemonSet", len(daemonSets.Items))
	}

	daemonSet := daemonSets.Items[0]
	if got := daemonSet.Spec.Template.Spec.NodeSelector; len(got) != 2 || got["kubernetes.io/os"] != "linux" {
		t.Errorf("DaemonSet node selector = %v, want the updated simulation node selector", got)
	}
	if len(daemonSet.Spec.Template.Spec.Tolerations) != 1 || daemonSet.Spec.Template.Spec.Tolerations[0].Key != "simulation" {
		t.Errorf("DaemonSet tolerations = %+v, want the updated tolerations", daemonSet.Spec.Template.Spec.Tolerations)
	}

	container := daemonSet.Spec.Template.Spec.Containers[0]
	if got := envVarValue(container, "VCR_DEVICE_PLUGIN_DEVICE_COUNT"); got != "8" {
		t.Errorf("device count env = %q, want 8 from the updated spec", got)
	}

	if got, want := deviceCountConfigMapOf(t, c).Data[controller.DeviceCountFileKey], "8"; got != want {
		t.Errorf("device count file content = %q, want %q after the spec change", got, want)
	}
}

func volumeMountPath(container corev1.Container, volumeName string) (string, bool) {
	for _, mount := range container.VolumeMounts {
		if mount.Name == volumeName {
			return mount.MountPath, true
		}
	}
	return "", false
}

func envVarValue(container corev1.Container, name string) string {
	for _, envVar := range container.Env {
		if envVar.Name == name {
			return envVar.Value
		}
	}
	return ""
}

func TestDevicePluginReconcilerIsIdempotent(t *testing.T) {
	env := testEnvironment()
	reconciler, c := devicePluginReconciler(t, env)

	if _, err := reconciler.Reconcile(context.Background(), env); err != nil {
		t.Fatalf("first Reconcile returned error: %v", err)
	}
	firstDaemonSet := daemonSetOf(t, c)
	firstConfigMap := deviceCountConfigMapOf(t, c)

	if _, err := reconciler.Reconcile(context.Background(), env); err != nil {
		t.Fatalf("second Reconcile returned error: %v", err)
	}

	secondDaemonSet := daemonSetOf(t, c)
	if secondDaemonSet.ResourceVersion != firstDaemonSet.ResourceVersion {
		t.Errorf("DaemonSet resource version changed from %q to %q: an unchanged spec must not rewrite it",
			firstDaemonSet.ResourceVersion, secondDaemonSet.ResourceVersion)
	}
	secondConfigMap := deviceCountConfigMapOf(t, c)
	if secondConfigMap.ResourceVersion != firstConfigMap.ResourceVersion {
		t.Errorf("ConfigMap resource version changed from %q to %q: an unchanged device count must not rewrite it",
			firstConfigMap.ResourceVersion, secondConfigMap.ResourceVersion)
	}
}

func TestDevicePluginReconcilerExcludesNodesWithARealGPUProvider(t *testing.T) {
	env := testEnvironment()
	simulated := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "simulation-node",
		Labels: map[string]string{"vcr.opendatahub-io/simulation": "true"},
	}}
	realGPU := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "gpu-node",
		Labels: map[string]string{
			"vcr.opendatahub-io/simulation": "true",
			"nvidia.com/gpu.present":        "true",
		},
	}}
	unrelated := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "other-node"}}

	reconciler, c := devicePluginReconciler(t, env, simulated, realGPU, unrelated)

	state, err := reconciler.Reconcile(context.Background(), env)
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if len(state.ExcludedNodes) != 1 || state.ExcludedNodes[0] != "gpu-node" {
		t.Errorf("excluded nodes = %v, want [gpu-node]", state.ExcludedNodes)
	}

	// The exclusion must be enforced by the Pod spec, not only reported: the
	// operator never writes node labels.
	affinity := daemonSetOf(t, c).Spec.Template.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil || affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("DaemonSet has no required node affinity, so it would run on a node with a real GPU: %+v", affinity)
	}
	terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 {
		t.Fatalf("node affinity has %d terms, want exactly one", len(terms))
	}
	excluded := map[string]bool{}
	for _, expression := range terms[0].MatchExpressions {
		if expression.Operator == corev1.NodeSelectorOpDoesNotExist {
			excluded[expression.Key] = true
		}
	}
	for _, label := range controller.RealGPUProviderLabels {
		if !excluded[label] {
			t.Errorf("node affinity does not exclude nodes labelled %q (expressions: %+v)", label, terms[0].MatchExpressions)
		}
	}
}

func TestDevicePluginReconcilerReportsReadiness(t *testing.T) {
	env := testEnvironment()
	reconciler, c := devicePluginReconciler(t, env)

	if _, err := reconciler.Reconcile(context.Background(), env); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	setDaemonSetStatus(t, c, 2, 1)
	state, err := reconciler.Reconcile(context.Background(), env)
	if err != nil {
		t.Fatalf("Reconcile with a partially ready DaemonSet returned error: %v", err)
	}
	if state.Desired != 2 || state.Ready != 1 {
		t.Errorf("state = %+v, want 1 of 2 ready", state)
	}

	setDaemonSetStatus(t, c, 2, 2)
	state, err = reconciler.Reconcile(context.Background(), env)
	if err != nil {
		t.Fatalf("Reconcile with a ready DaemonSet returned error: %v", err)
	}
	if state.Desired != 2 || state.Ready != 2 {
		t.Errorf("state = %+v, want 2 of 2 ready", state)
	}
}

func TestDevicePluginReconcilerRejectsMissingImage(t *testing.T) {
	reconciler, _ := devicePluginReconciler(t)
	reconciler.Image = ""

	if _, err := reconciler.Reconcile(context.Background(), testEnvironment()); err == nil {
		t.Fatal("Reconcile with no image configured returned nil, want an error naming the image setting")
	}
}

func TestDevicePluginReconcilerPreservesServerDefaultedFields(t *testing.T) {
	env := testEnvironment()
	reconciler, c := devicePluginReconciler(t, env)

	if _, err := reconciler.Reconcile(context.Background(), env); err != nil {
		t.Fatalf("first Reconcile returned error: %v", err)
	}

	// The API server defaults a Pod template. Replacing the template wholesale
	// would strip those defaults, so the next comparison would differ again and
	// the operator would rewrite the DaemonSet on every reconcile.
	daemonSet := daemonSetOf(t, c)
	defaulted := daemonSet.DeepCopy()
	defaulted.Spec.Template.Spec.DNSPolicy = corev1.DNSClusterFirst
	defaulted.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyAlways
	defaulted.Spec.Template.Spec.Containers[0].TerminationMessagePath = "/dev/termination-log"
	defaulted.Spec.Template.Spec.Containers[0].TerminationMessagePolicy = corev1.TerminationMessageReadFile
	if err := c.Update(context.Background(), defaulted); err != nil {
		t.Fatalf("simulate API server defaulting: %v", err)
	}
	defaultedVersion := defaulted.ResourceVersion

	if _, err := reconciler.Reconcile(context.Background(), env); err != nil {
		t.Fatalf("Reconcile over a defaulted DaemonSet returned error: %v", err)
	}

	reconciled := daemonSetOf(t, c)
	if reconciled.ResourceVersion != defaultedVersion {
		t.Errorf("defaulted DaemonSet was rewritten (resource version %q -> %q): the operator must not fight API server defaulting",
			defaultedVersion, reconciled.ResourceVersion)
	}
	podSpec := reconciled.Spec.Template.Spec
	if podSpec.DNSPolicy != corev1.DNSClusterFirst {
		t.Errorf("pod dns policy = %q, want the defaulted %q to survive reconciliation", podSpec.DNSPolicy, corev1.DNSClusterFirst)
	}
	if got := podSpec.Containers[0].TerminationMessagePath; got != "/dev/termination-log" {
		t.Errorf("termination message path = %q, want the defaulted value to survive reconciliation", got)
	}
}

func TestDevicePluginReconcilerIgnoresStatusFromAnOlderGeneration(t *testing.T) {
	env := testEnvironment()
	reconciler, c := devicePluginReconciler(t, env)

	if _, err := reconciler.Reconcile(context.Background(), env); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	// A DaemonSet that has just been respecified still reports the previous
	// template's ready count until the rollout finishes.
	daemonSet := daemonSetOf(t, c)
	daemonSet.Generation = 2
	if err := c.Update(context.Background(), daemonSet); err != nil {
		t.Fatalf("advance the DaemonSet generation: %v", err)
	}
	stale := daemonSetOf(t, c)
	stale.Status = appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: 2, NumberReady: 2}
	if err := c.Status().Update(context.Background(), stale); err != nil {
		t.Fatalf("set a stale DaemonSet status: %v", err)
	}
	if got := daemonSetOf(t, c).Status.NumberReady; got != 2 {
		t.Fatalf("stale status was not stored: ready = %d", got)
	}

	state, err := reconciler.Reconcile(context.Background(), env)
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if state.Ready != 0 {
		t.Errorf("state.Ready = %d for a status written before the current generation, want 0", state.Ready)
	}
}
