package controller

import (
	"context"
	"fmt"
	"path"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/deviceplugin"
	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
)

const (
	// DevicePluginConfigMapName is the operator-owned ConfigMap that carries the
	// device count file polled by a running device plugin, so the advertised
	// count can change without a new image.
	DevicePluginConfigMapName = "vllm-vcr-device-plugin-config"

	// DeviceCountFileKey is the ConfigMap key whose content is the advertised
	// device count. The key name is the file name the device plugin reads, so
	// the ConfigMap can be mounted as a directory.
	DeviceCountFileKey = "device-count"

	// deviceComponentLabel distinguishes operator-managed workloads that share
	// the operator namespace.
	deviceComponentLabel = names.LabelPrefix + "component"
	// devicePluginComponent is the component label value of the device plugin.
	devicePluginComponent = "device-plugin"

	// devicePluginContainerPort is the health endpoint port name.
	devicePluginContainerPort = "health"

	// devicePluginsVolumeName is the kubelet device-plugin directory volume.
	devicePluginsVolumeName = "device-plugins"
	// deviceCountVolumeName is the device count ConfigMap volume.
	deviceCountVolumeName = "device-count"

	// devicePluginTerminationGracePeriodSeconds leaves the plugin time to stop
	// its gRPC server and release the kubelet registration cleanly.
	devicePluginTerminationGracePeriodSeconds = 30
)

// deviceCountDir is the directory the device count ConfigMap is mounted into. It
// is derived from the device plugin's default file path so the two cannot drift
// apart: the plugin reads <deviceCountDir>/<DeviceCountFileKey>.
var deviceCountDir = path.Dir(deviceplugin.DefaultDeviceCountFile)

// RealGPUProviderLabels are node labels that a real GPU device provider sets,
// for example NVIDIA GPU feature discovery ("nvidia.com/gpu.present") and the
// NVIDIA device plugin ("nvidia.com/gpu.product"). A node carrying any of them
// already advertises nvidia.com/gpu from another provider, so the synthetic
// plugin must not run there: two providers registering the same resource for one
// node corrupts kubelet's allocation bookkeeping.
//
// The exclusion is expressed as a required node affinity in the rendered Pod, so
// the operator never writes node labels and needs no node write permission.
var RealGPUProviderLabels = []string{
	"nvidia.com/gpu.present",
	"nvidia.com/gpu.product",
}

// DevicePluginState is the observed state of the device-plugin DaemonSet after
// reconciliation. The environment reconciler reports it as the DevicePluginReady
// condition.
type DevicePluginState struct {
	// Desired is the number of nodes the DaemonSet should run on.
	Desired int32
	// Ready is the number of nodes running a ready plugin Pod.
	Ready int32
	// ExcludedNodes names the nodes that match the simulation node selector but
	// carry a RealGPUProviderLabels marker, so the DaemonSet does not run there.
	ExcludedNodes []string
}

// DevicePluginReconciler renders and reconciles the synthetic GPU device-plugin
// DaemonSet and its device count ConfigMap from a VcrGpuCluster.
//
// Both objects live in OperatorNamespace and are controlled by the cluster-scoped
// VcrGpuCluster, so deleting the environment deletes them.
type DevicePluginReconciler struct {
	// Client reads and writes the DaemonSet, the ConfigMap, and Nodes.
	Client client.Client

	// OperatorNamespace is the namespace holding the DaemonSet and the device
	// count ConfigMap.
	OperatorNamespace string

	// Image is the device-plugin container image. It is injected (see
	// EnvDevicePluginImage) so one operator build serves every registry and tag.
	Image string
}

// Reconcile creates or updates the device-plugin DaemonSet and its device count
// ConfigMap, and reports the observed DaemonSet readiness.
//
// The DaemonSet and its ConfigMap are operator-owned; Nodes are only read, to
// report which simulation nodes are excluded because a real GPU provider already
// advertises nvidia.com/gpu there. The operator never writes node labels.
func (r *DevicePluginReconciler) Reconcile(ctx context.Context, env *vcrv1alpha1.VcrGpuCluster) (DevicePluginState, error) {
	if r.Image == "" {
		return DevicePluginState{}, fmt.Errorf(
			"device plugin image is not configured: set %s to the device-plugin image reference", EnvDevicePluginImage)
	}
	if r.OperatorNamespace == "" {
		return DevicePluginState{}, fmt.Errorf(
			"operator namespace is not configured: the device plugin DaemonSet and its device count ConfigMap need a namespace")
	}
	if env == nil {
		return DevicePluginState{}, fmt.Errorf("device plugin reconciliation requires a VcrGpuCluster")
	}

	if err := r.reconcileServiceAccount(ctx, env); err != nil {
		return DevicePluginState{}, err
	}
	daemonSet, err := r.reconcileDaemonSet(ctx, env)
	if err != nil {
		return DevicePluginState{}, err
	}
	if err := r.reconcileDeviceCountConfigMap(ctx, env); err != nil {
		return DevicePluginState{}, err
	}
	excluded, err := r.excludedNodes(ctx, env)
	if err != nil {
		return DevicePluginState{}, err
	}

	state := DevicePluginState{
		Desired:       daemonSet.Status.DesiredNumberScheduled,
		Ready:         daemonSet.Status.NumberReady,
		ExcludedNodes: excluded,
	}
	// Status written for an older generation describes a different pod template:
	// reporting it as ready would claim a rollout that has not happened yet.
	if daemonSet.Status.ObservedGeneration < daemonSet.Generation {
		state.Ready = 0
	}
	return state, nil
}

// reconcileServiceAccount ensures the dedicated device-plugin ServiceAccount
// exists. The DaemonSet runs as this account so a platform can bind the narrowest
// permission that permits kubelet's socket directory, instead of granting it to
// every Pod in the operator namespace.
func (r *DevicePluginReconciler) reconcileServiceAccount(ctx context.Context, env *vcrv1alpha1.VcrGpuCluster) error {
	desired := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.DevicePluginServiceAccount,
			Namespace: r.OperatorNamespace,
			Labels:    devicePluginLabels(env),
		},
		AutomountServiceAccountToken: ptr.To(false),
	}
	if err := setEnvironmentOwner(env, desired, r.Client.Scheme()); err != nil {
		return fmt.Errorf("set the device plugin ServiceAccount owner: %w", err)
	}

	existing := &corev1.ServiceAccount{}
	key := types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}
	if err := r.Client.Get(ctx, key, existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get device plugin ServiceAccount %s: %w", key, err)
		}
		if err := r.Client.Create(ctx, desired); err != nil {
			return fmt.Errorf("create device plugin ServiceAccount %s: %w", key, err)
		}
		return nil
	}

	before := existing.DeepCopy()
	for k, v := range desired.Labels {
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		existing.Labels[k] = v
	}
	existing.AutomountServiceAccountToken = desired.AutomountServiceAccountToken
	if err := setEnvironmentOwner(env, existing, r.Client.Scheme()); err != nil {
		return fmt.Errorf("set the device plugin ServiceAccount owner: %w", err)
	}
	if equality.Semantic.DeepEqual(before, existing) {
		return nil
	}
	if err := r.Client.Update(ctx, existing); err != nil {
		return fmt.Errorf("update device plugin ServiceAccount %s: %w", key, err)
	}
	return nil
}

// reconcileDaemonSet creates the DaemonSet when absent and otherwise merges the
// operator-managed fields into the existing object, so server-defaulted fields
// are preserved and an unchanged spec produces no write.
func (r *DevicePluginReconciler) reconcileDaemonSet(ctx context.Context, env *vcrv1alpha1.VcrGpuCluster) (*appsv1.DaemonSet, error) {
	desired, err := r.desiredDaemonSet(env)
	if err != nil {
		return nil, err
	}

	existing := &appsv1.DaemonSet{}
	key := types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}
	if err := r.Client.Get(ctx, key, existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get device plugin DaemonSet %s: %w", key, err)
		}
		if err := r.Client.Create(ctx, desired); err != nil {
			return nil, fmt.Errorf("create device plugin DaemonSet %s: %w", key, err)
		}
		return desired, nil
	}

	before := existing.DeepCopy()
	if err := mergeDaemonSet(existing, desired, env, r.Client.Scheme()); err != nil {
		return nil, fmt.Errorf("reconcile device plugin DaemonSet %s: %w", key, err)
	}
	if equality.Semantic.DeepEqual(before, existing) {
		return existing, nil
	}
	if err := r.Client.Update(ctx, existing); err != nil {
		return nil, fmt.Errorf("update device plugin DaemonSet %s: %w", key, err)
	}
	return existing, nil
}

// desiredDaemonSet renders the DaemonSet for env.
func (r *DevicePluginReconciler) desiredDaemonSet(env *vcrv1alpha1.VcrGpuCluster) (*appsv1.DaemonSet, error) {
	labels := devicePluginLabels(env)

	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.DevicePluginName,
			Namespace: r.OperatorNamespace,
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: devicePluginSelector()},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName:            names.DevicePluginServiceAccount,
					NodeSelector:                  env.Spec.NodeSelection.NodeSelector,
					Tolerations:                   env.Spec.NodeSelection.Tolerations,
					Affinity:                      realGPUProviderExclusion(),
					AutomountServiceAccountToken:  ptr.To(false),
					TerminationGracePeriodSeconds: ptr.To(int64(devicePluginTerminationGracePeriodSeconds)),
					Containers:                    []corev1.Container{r.desiredContainer(env)},
					Volumes:                       devicePluginVolumes(),
				},
			},
		},
	}
	if err := setEnvironmentOwner(env, daemonSet, r.Client.Scheme()); err != nil {
		return nil, err
	}
	return daemonSet, nil
}

// desiredContainer renders the single device-plugin container.
func (r *DevicePluginReconciler) desiredContainer(env *vcrv1alpha1.VcrGpuCluster) corev1.Container {
	logLevel := env.Spec.LogLevel
	if logLevel == "" {
		logLevel = "info"
	}

	return corev1.Container{
		Name:            names.DevicePluginName,
		Image:           r.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env: []corev1.EnvVar{
			{Name: "VCR_DEVICE_PLUGIN_RESOURCE_NAME", Value: names.SyntheticResourceName},
			{Name: "VCR_DEVICE_PLUGIN_DEVICE_COUNT", Value: strconv.Itoa(int(env.Spec.DevicesPerNode))},
			{Name: "VCR_DEVICE_PLUGIN_DEVICE_COUNT_FILE", Value: deviceplugin.DefaultDeviceCountFile},
			{Name: "VCR_DEVICE_PLUGIN_SOCKET_DIR", Value: names.DevicePluginDir},
			{Name: "VCR_DEVICE_PLUGIN_ENDPOINT", Value: names.DevicePluginSocketName},
			{Name: "VCR_DEVICE_PLUGIN_HEALTH_PORT", Value: strconv.Itoa(deviceplugin.DefaultHealthPort)},
			{Name: "VCR_DEVICE_PLUGIN_LOG_LEVEL", Value: logLevel},
		},
		Ports: []corev1.ContainerPort{{
			Name:          devicePluginContainerPort,
			ContainerPort: deviceplugin.DefaultHealthPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		VolumeMounts: []corev1.VolumeMount{
			// The kubelet registration socket is only addressable at its fixed
			// host path, so the mount path must equal it.
			{Name: devicePluginsVolumeName, MountPath: names.DevicePluginDir},
			{Name: deviceCountVolumeName, MountPath: deviceCountDir, ReadOnly: true},
		},
		LivenessProbe:  devicePluginProbe(deviceplugin.HealthPath, 10),
		ReadinessProbe: devicePluginProbe(deviceplugin.ReadyPath, 5),
		SecurityContext: &corev1.SecurityContext{
			// The plugin creates its socket in kubelet's root-owned device-plugin
			// directory, which is its only privileged-looking requirement. It
			// needs no host device, no NVIDIA runtime class, and no capability:
			// kubelet owns allocation and the plugin only reports capacity.
			RunAsUser:                ptr.To(int64(0)),
			RunAsNonRoot:             ptr.To(false),
			Privileged:               ptr.To(false),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			// No explicit seccompProfile: some SCCs refuse one, and an unset profile
			// is the platform default.
			//
			// spc_t is required on an SELinux-enforcing node: kubelet's socket lives
			// in kubelet_var_lib_t, which the default container_t type cannot connect
			// to. The NVIDIA device plugin needs the same permission. The plugin
			// still requests no device, no NVIDIA runtime, and no capability: this
			// container-level label is about reaching kubelet's socket, not about
			// GPUs. On clusters without SELinux the label is inert.
			SELinuxOptions: &corev1.SELinuxOptions{Type: "spc_t"},
		},
	}
}

// devicePluginProbe builds a health probe against the plugin's health endpoint.
func devicePluginProbe(path string, initialDelaySeconds int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   path,
				Port:   intstr.FromInt32(deviceplugin.DefaultHealthPort),
				Scheme: corev1.URISchemeHTTP,
			},
		},
		InitialDelaySeconds: initialDelaySeconds,
		PeriodSeconds:       10,
		TimeoutSeconds:      5,
		FailureThreshold:    3,
		SuccessThreshold:    1,
	}
}

// devicePluginVolumes renders the kubelet socket directory and the device count
// ConfigMap volumes.
func devicePluginVolumes() []corev1.Volume {
	return []corev1.Volume{
		{
			Name: devicePluginsVolumeName,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: names.DevicePluginDir,
					Type: ptr.To(corev1.HostPathDirectory),
				},
			},
		},
		{
			Name: deviceCountVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: DevicePluginConfigMapName},
				},
			},
		},
	}
}

// realGPUProviderExclusion renders the required node affinity that keeps the
// synthetic plugin off nodes where a real GPU provider already advertises
// nvidia.com/gpu.
func realGPUProviderExclusion() *corev1.Affinity {
	expressions := make([]corev1.NodeSelectorRequirement, 0, len(RealGPUProviderLabels))
	for _, label := range RealGPUProviderLabels {
		expressions = append(expressions, corev1.NodeSelectorRequirement{
			Key:      label,
			Operator: corev1.NodeSelectorOpDoesNotExist,
		})
	}
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: expressions}},
			},
		},
	}
}

// devicePluginSelector is the immutable label selector shared by the DaemonSet
// and its Pods. It must not depend on the VcrGpuCluster name: a
// DaemonSet selector cannot be updated in place.
func devicePluginSelector() map[string]string {
	return map[string]string{
		names.ManagedByLabel: names.OperatorName,
		deviceComponentLabel: devicePluginComponent,
	}
}

// devicePluginLabels are the labels put on the DaemonSet and its Pods.
func devicePluginLabels(env *vcrv1alpha1.VcrGpuCluster) map[string]string {
	labels := devicePluginSelector()
	labels[names.EnvironmentLabel] = env.Name
	return labels
}

// reconcileDeviceCountConfigMap publishes the device count the plugin polls. The
// file content is the decimal count, which is what the plugin parses after
// trimming whitespace.
func (r *DevicePluginReconciler) reconcileDeviceCountConfigMap(ctx context.Context, env *vcrv1alpha1.VcrGpuCluster) error {
	key := types.NamespacedName{Namespace: r.OperatorNamespace, Name: DevicePluginConfigMapName}
	existing := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, key, existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get device count ConfigMap %s: %w", key, err)
		}
		desired := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: devicePluginLabels(env)},
			Data:       map[string]string{DeviceCountFileKey: strconv.Itoa(int(env.Spec.DevicesPerNode))},
		}
		if err := setEnvironmentOwner(env, desired, r.Client.Scheme()); err != nil {
			return err
		}
		if err := r.Client.Create(ctx, desired); err != nil {
			return fmt.Errorf("create device count ConfigMap %s: %w", key, err)
		}
		return nil
	}

	before := existing.DeepCopy()
	if err := mergeDeviceCountConfigMap(existing, env, r.Client.Scheme()); err != nil {
		return fmt.Errorf("reconcile device count ConfigMap %s: %w", key, err)
	}
	if equality.Semantic.DeepEqual(before, existing) {
		return nil
	}
	if err := r.Client.Update(ctx, existing); err != nil {
		return fmt.Errorf("update device count ConfigMap %s: %w", key, err)
	}
	return nil
}

// excludedNodes lists the nodes that match the simulation node selector but
// carry a real GPU provider marker. Listing nodes is read-only: the exclusion
// itself is enforced by the DaemonSet's node affinity.
func (r *DevicePluginReconciler) excludedNodes(ctx context.Context, env *vcrv1alpha1.VcrGpuCluster) ([]string, error) {
	nodeList := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodeList, client.MatchingLabels(env.Spec.NodeSelection.NodeSelector)); err != nil {
		return nil, fmt.Errorf("list simulation nodes: %w", err)
	}
	var excluded []string
	for _, node := range nodeList.Items {
		for _, label := range RealGPUProviderLabels {
			if _, ok := node.Labels[label]; ok {
				excluded = append(excluded, node.Name)
				break
			}
		}
	}
	return excluded, nil
}

// mergeDeviceCountConfigMap writes the operator-managed fields of the device
// count ConfigMap. The caller compares the object before and after the call.
func mergeDeviceCountConfigMap(configMap *corev1.ConfigMap, env *vcrv1alpha1.VcrGpuCluster, scheme *runtime.Scheme) error {
	if configMap.Data == nil {
		configMap.Data = map[string]string{}
	}
	configMap.Data[DeviceCountFileKey] = strconv.Itoa(int(env.Spec.DevicesPerNode))
	if configMap.Labels == nil {
		configMap.Labels = map[string]string{}
	}
	for label, value := range devicePluginLabels(env) {
		configMap.Labels[label] = value
	}
	return setEnvironmentOwner(env, configMap, scheme)
}

// setEnvironmentOwner makes the cluster-scoped VcrGpuCluster the
// controller of an operator-managed object so it is garbage collected with the
// environment. A cluster-scoped owner of a namespaced object is allowed; the
// reverse is not, and this operator never relies on that.
func setEnvironmentOwner(env *vcrv1alpha1.VcrGpuCluster, object metav1.Object, scheme *runtime.Scheme) error {
	if err := controllerutil.SetControllerReference(env, object, scheme); err != nil {
		return fmt.Errorf("set owner reference on %T %s: %w", object, object.GetName(), err)
	}
	return nil
}

// mergeDaemonSet writes the operator-managed fields of desired into existing.
//
// Only managed fields are assigned: the API server defaults a Pod template
// (dns policy, restart policy, probe defaults, termination message paths, and so
// on), and replacing the whole template would strip those defaults and make the
// next comparison differ again. Merging keeps an unchanged spec a no-op.
func mergeDaemonSet(existing, desired *appsv1.DaemonSet, env *vcrv1alpha1.VcrGpuCluster, scheme *runtime.Scheme) error {
	existing.Labels = mergeStringMap(existing.Labels, desired.Labels)
	if existing.Spec.Selector == nil {
		// The selector is immutable, so it is only ever set on creation. A
		// pre-existing DaemonSet with a foreign selector is rejected by the API
		// server on update rather than silently adopted.
		existing.Spec.Selector = desired.Spec.Selector
	}

	template := &existing.Spec.Template
	template.Labels = mergeStringMap(template.Labels, desired.Spec.Template.Labels)

	pod, desiredPod := &template.Spec, &desired.Spec.Template.Spec
	// The ServiceAccount is part of the operator-managed identity: without it the
	// pod falls back to "default", which on OpenShift is not bound to the SCC that
	// permits kubelet's socket directory.
	pod.ServiceAccountName = desiredPod.ServiceAccountName
	pod.NodeSelector = desiredPod.NodeSelector
	pod.Tolerations = desiredPod.Tolerations
	pod.Affinity = desiredPod.Affinity
	pod.AutomountServiceAccountToken = desiredPod.AutomountServiceAccountToken
	pod.TerminationGracePeriodSeconds = desiredPod.TerminationGracePeriodSeconds
	pod.Volumes = mergeVolumes(pod.Volumes, desiredPod.Volumes, []string{devicePluginsVolumeName, deviceCountVolumeName})
	pod.Containers = mergeContainers(pod.Containers, desiredPod.Containers, []string{names.DevicePluginName})

	return setEnvironmentOwner(env, existing, scheme)
}

// mergeStringMap copies desired into existing, allocating existing when needed.
func mergeStringMap(existing, desired map[string]string) map[string]string {
	if len(desired) == 0 {
		return existing
	}
	if existing == nil {
		existing = make(map[string]string, len(desired))
	}
	for key, value := range desired {
		existing[key] = value
	}
	return existing
}

// mergeVolumes replaces the managed volumes of existing with desired and keeps
// unmanaged volumes, so unrelated sidecar or storage volumes introduced later
// are not removed.
func mergeVolumes(existing, desired []corev1.Volume, managed []string) []corev1.Volume {
	managedSet := make(map[string]struct{}, len(managed))
	for _, name := range managed {
		managedSet[name] = struct{}{}
	}
	merged := make([]corev1.Volume, 0, len(existing)+len(desired))
	for _, volume := range existing {
		if _, operatorManaged := managedSet[volume.Name]; !operatorManaged {
			merged = append(merged, volume)
		}
	}
	return append(merged, desired...)
}

// mergeContainers replaces the managed containers of existing with desired and
// keeps unmanaged containers. A managed container keeps every field the operator
// does not set (termination message path, for example) so that server-defaulted
// values survive.
func mergeContainers(existing, desired []corev1.Container, managed []string) []corev1.Container {
	managedSet := make(map[string]struct{}, len(managed))
	for _, name := range managed {
		managedSet[name] = struct{}{}
	}
	existingByName := make(map[string]corev1.Container, len(existing))
	merged := make([]corev1.Container, 0, len(existing)+len(desired))
	for _, container := range existing {
		if _, operatorManaged := managedSet[container.Name]; operatorManaged {
			existingByName[container.Name] = container
			continue
		}
		merged = append(merged, container)
	}
	for _, container := range desired {
		if prior, ok := existingByName[container.Name]; ok {
			merged = append(merged, mergeContainer(prior, container))
			continue
		}
		merged = append(merged, container)
	}
	return merged
}

// mergeContainer applies the operator-managed container fields to a container
// that already exists in the cluster.
func mergeContainer(existing, desired corev1.Container) corev1.Container {
	merged := existing
	merged.Image = desired.Image
	merged.ImagePullPolicy = desired.ImagePullPolicy
	merged.Command = desired.Command
	merged.Args = desired.Args
	merged.Env = desired.Env
	merged.EnvFrom = desired.EnvFrom
	merged.Ports = desired.Ports
	merged.VolumeMounts = desired.VolumeMounts
	merged.Resources = desired.Resources
	merged.SecurityContext = desired.SecurityContext
	merged.LivenessProbe = desired.LivenessProbe
	merged.ReadinessProbe = desired.ReadinessProbe
	merged.StartupProbe = desired.StartupProbe
	return merged
}
