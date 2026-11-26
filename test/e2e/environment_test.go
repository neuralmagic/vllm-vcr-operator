package e2e

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/controller"
	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
)

// TestSingletonConfigurationIsEnforcedByTheAPIServer asserts the singleton policy
// through the API server rather than through operator conventions: the shipiped
// CRD accepts only the name "default", and a second object cannot compete with
// the existing one.
func TestSingletonConfigurationIsEnforcedByTheAPIServer(t *testing.T) {
	h := requireSuite(t)

	other := baselineEnvironment()
	other.Name = "simulation"
	err := h.client.Create(helperContext(t), other)
	if err == nil {
		_ = h.client.Delete(helperContext(t), other)
		t.Fatal("the API server accepted a VcrGpuCluster named \"simulation\", want the singleton rule to reject it")
	}
	if !apierrors.IsInvalid(err) && !apierrors.IsForbidden(err) {
		t.Errorf("create error is %T (%v), want an API validation or admission rejection", err, err)
	}
	if !strings.Contains(err.Error(), vcrv1alpha1.VcrGpuClusterNameDefault) {
		t.Errorf("rejection %q does not name the only accepted name %q", err, vcrv1alpha1.VcrGpuClusterNameDefault)
	}
	rejected := &vcrv1alpha1.VcrGpuCluster{}
	if getErr := h.client.Get(helperContext(t), types.NamespacedName{Name: "simulation"}, rejected); !apierrors.IsNotFound(getErr) {
		t.Errorf("the rejected VcrGpuCluster exists (get returned %v)", getErr)
	}

	duplicate := baselineEnvironment()
	duplicateErr := h.client.Create(helperContext(t), duplicate)
	if duplicateErr == nil {
		_ = h.client.Delete(helperContext(t), duplicate)
		t.Fatal("the API server accepted a second VcrGpuCluster named \"default\"")
	}
	if !apierrors.IsAlreadyExists(duplicateErr) {
		t.Errorf("duplicate create error is %T (%v), want AlreadyExists", duplicateErr, duplicateErr)
	}
}

// TestProfileReconciliationPublishesTheCatalog asserts what the environment
// reconciler makes externally observable: a reconciled profile entry with the
// digest pinned, the conditions admission depends on, and the rendered
// device-plugin DaemonSet with its device count ConfigMap.
func TestProfileReconciliationPublishesTheCatalog(t *testing.T) {
	h := requireSuite(t)
	h.requireReadyEnvironment(t)

	env := h.getEnvironment(t)

	if got, want := env.Status.SupportedVLLMVersion, names.SupportedVLLMVersion; got != want {
		t.Errorf("status.supportedVLLMVersion = %q, want %q", got, want)
	}
	if len(env.Status.Profiles) != 1 {
		t.Fatalf("status.profiles = %d entries, want the one configured profile: %+v", len(env.Status.Profiles), env.Status.Profiles)
	}
	published := env.Status.Profiles[0]
	if published.Image != goodProfileImage {
		t.Errorf("status.profiles[0].image = %q, want %q", published.Image, goodProfileImage)
	}
	if published.Digest != goodProfileDigest {
		t.Errorf("status.profiles[0].digest = %q, want the resolved digest %q", published.Digest, goodProfileDigest)
	}
	if published.Message != "" {
		t.Errorf("status.profiles[0].message = %q, want empty for a usable profile", published.Message)
	}
	if !strings.Contains(published.Metadata, profileID) {
		t.Errorf("status.profiles[0].metadata does not carry the validated document: %q", published.Metadata)
	}
	if published.ReconciledAt.IsZero() {
		t.Error("status.profiles[0].reconciledAt is zero: admission would not know when the digest was verified")
	}

	ready := condition(env, vcrv1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %+v, want True", ready)
	}
	if ready.Reason != vcrv1alpha1.ReasonEnvironmentReady {
		t.Errorf("Ready reason = %q, want %q", ready.Reason, vcrv1alpha1.ReasonEnvironmentReady)
	}
	profiles := condition(env, vcrv1alpha1.ConditionProfilesReconciled)
	if profiles == nil || profiles.Status != metav1.ConditionTrue {
		t.Fatalf("ProfilesReconciled condition = %+v, want True", profiles)
	}
	plugin := condition(env, vcrv1alpha1.ConditionDevicePluginReady)
	if plugin == nil || plugin.Status != metav1.ConditionTrue {
		t.Fatalf("DevicePluginReady condition = %+v, want True", plugin)
	}

	// The device plugin DaemonSet is rendered even though nothing schedules it
	// here: its shape is what the cluster suite cannot see without an image.
	daemonSet := &appsv1.DaemonSet{}
	key := types.NamespacedName{Namespace: operatorNamespace, Name: names.DevicePluginName}
	if err := h.client.Get(helperContext(t), key, daemonSet); err != nil {
		t.Fatalf("device plugin DaemonSet %s: %v", key, err)
	}
	if len(daemonSet.OwnerReferences) == 0 || daemonSet.OwnerReferences[0].Name != vcrv1alpha1.VcrGpuClusterNameDefault {
		t.Errorf("device plugin DaemonSet owner references = %+v, want the VcrGpuCluster", daemonSet.OwnerReferences)
	}
	if got := daemonSet.Spec.Template.Spec.NodeSelector[simulationNodeLabel]; got != "true" {
		t.Errorf("device plugin node selector = %v, want the environment's simulation node selection", daemonSet.Spec.Template.Spec.NodeSelector)
	}
	devicePriceProvider := daemonSet.Spec.Template.Spec.Affinity
	if devicePriceProvider == nil || devicePriceProvider.NodeAffinity == nil {
		t.Error("device plugin DaemonSet has no node affinity: it would run on nodes where a real GPU provider advertises the resource")
	}
	if len(daemonSet.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("device plugin containers = %d, want exactly one", len(daemonSet.Spec.Template.Spec.Containers))
	}
	pluginContainer := daemonSet.Spec.Template.Spec.Containers[0]
	if pluginContainer.Image != devicePluginImage {
		t.Errorf("device plugin image = %q, want %q", pluginContainer.Image, devicePluginImage)
	}
	deviceCount := ""
	for _, variable := range pluginContainer.Env {
		if variable.Name == "VCR_DEVICE_PLUGIN_DEVICE_COUNT" {
			deviceCount = variable.Value
		}
	}
	if deviceCount != "8" {
		t.Errorf("VCR_DEVICE_PLUGIN_DEVICE_COUNT = %q, want the environment's devicesPerNode 8", deviceCount)
	}
	hostPathMounted := false
	for _, mount := range pluginContainer.VolumeMounts {
		if mount.MountPath == names.DevicePluginDir {
			hostPathMounted = true
		}
	}
	if !hostPathMounted {
		t.Errorf("the device plugin does not mount %s, so it cannot register with kubelet", names.DevicePluginDir)
	}

	configMap := &corev1.ConfigMap{}
	configMapKey := types.NamespacedName{Namespace: operatorNamespace, Name: controller.DevicePluginConfigMapName}
	if err := h.client.Get(helperContext(t), configMapKey, configMap); err != nil {
		t.Fatalf("device count ConfigMap %s: %v", configMapKey, err)
	}
	if got := configMap.Data[controller.DeviceCountFileKey]; got != "8" {
		t.Errorf("%s = %q, want the device count 8", controller.DeviceCountFileKey, got)
	}
}

// TestUnusableProfileIsReportedWithoutErasingGoodOnes asserts one broken profile
// image cannot take the working catalog away: it keeps its own published entry
// with an actionable reason, the good entry keeps its digest, and the
// environment reports it is not ready.
func TestUnusableProfileIsReportedWithoutErasingGoodOnes(t *testing.T) {
	h := requireSuite(t)
	h.requireReadyEnvironment(t)
	goodBefore := h.waitForProfiles(t, goodProfileImage)

	h.setProfiles(t,
		vcrv1alpha1.ProfileSource{Image: goodProfileImage},
		vcrv1alpha1.ProfileSource{Image: unusableProfileImage},
	)

	// The reconciler reports the unusable entry in spec order.
	h.waitForProfiles(t, unusableProfileImage)

	env := h.getEnvironment(t)
	if len(env.Status.Profiles) != 2 {
		t.Fatalf("status.profiles = %d entries, want both configured images: %+v", len(env.Status.Profiles), env.Status.Profiles)
	}

	var good, broken vcrv1alpha1.ResolvedProfile
	for _, published := range env.Status.Profiles {
		switch published.Image {
		case goodProfileImage:
			good = published
		case unusableProfileImage:
			broken = published
		}
	}
	if good.Digest != goodBefore.Digest || good.Metadata == "" || good.Message != "" {
		t.Errorf("the usable profile was damaged by an unusable sibling: %+v", good)
	}
	if broken.Message == "" {
		t.Fatalf("the unusable profile publishes no message: %+v", broken)
	}
	for _, want := range []string{unusableProfileImage, "schemaVersion"} {
		if !strings.Contains(broken.Message, want) {
			t.Errorf("unusable profile message %q does not mention %q", broken.Message, want)
		}
	}

	profiles := condition(env, vcrv1alpha1.ConditionProfilesReconciled)
	if profiles == nil || profiles.Status != metav1.ConditionFalse {
		t.Fatalf("ProfilesReconciled condition = %+v, want False", profiles)
	}
	if profiles.Reason != vcrv1alpha1.ReasonProfileUnusable {
		t.Errorf("ProfilesReconciled reason = %q, want %q", profiles.Reason, vcrv1alpha1.ReasonProfileUnusable)
	}
	if !strings.Contains(profiles.Message, unusableProfileImage) {
		t.Errorf("ProfilesReconciled message %q does not name the offending image", profiles.Message)
	}
	ready := condition(env, vcrv1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready condition = %+v, want False", ready)
	}
	if ready.Reason != vcrv1alpha1.ReasonProfilesNotReconciled {
		t.Errorf("Ready reason = %q, want %q", ready.Reason, vcrv1alpha1.ReasonProfilesNotReconciled)
	}
}
