package controller_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/controller"
	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
	"github.com/neuralmagic/vllm-vcr-operator/internal/registry"
)

var errRegistryUnavailable = errors.New("profile image: fetch from registry: connection refused")

// fixedTime is the reconcile clock used by every test, so ReconciledAt is
// deterministic.
var fixedTime = time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)

// fakeRegistry is an injected registry.Resolver factory: it records the
// references it was asked to resolve and the options it was built with, so tests
// can assert both that no call was made and which credentials were used.
type fakeRegistry struct {
	mu       sync.Mutex
	resolved map[string]registry.Resolved
	errs     map[string]error
	calls    []string
	options  []registry.Options
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{resolved: map[string]registry.Resolved{}, errs: map[string]error{}}
}

// resolve records what the given reference resolves to.
func (f *fakeRegistry) resolve(ref, digest string, metadata []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved[ref] = registry.Resolved{
		Image:    digestReference(ref, digest),
		Digest:   digest,
		Metadata: metadata,
		Source:   registry.SourceLabel,
	}
}

// fail makes resolving ref fail with err.
func (f *fakeRegistry) fail(ref string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[ref] = err
}

func (f *fakeRegistry) factory(opts registry.Options) registry.Resolver {
	f.mu.Lock()
	f.options = append(f.options, opts)
	f.mu.Unlock()
	return resolverFunc(f.resolveRef)
}

func (f *fakeRegistry) resolveRef(_ context.Context, ref string) (registry.Resolved, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ref)
	if err, ok := f.errs[ref]; ok {
		return registry.Resolved{}, err
	}
	resolved, ok := f.resolved[ref]
	if !ok {
		return registry.Resolved{}, fmt.Errorf("profile image %q: fetch from registry: not found", ref)
	}
	return resolved, nil
}

func (f *fakeRegistry) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeRegistry) lastOptions() registry.Options {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.options) == 0 {
		return registry.Options{}
	}
	return f.options[len(f.options)-1]
}

type resolverFunc func(ctx context.Context, ref string) (registry.Resolved, error)

func (f resolverFunc) Resolve(ctx context.Context, ref string) (registry.Resolved, error) {
	return f(ctx, ref)
}

// digestReference returns a digest-pinned reference the way a registry does.
func digestReference(ref, digest string) string {
	repository := ref
	if index := strings.LastIndex(ref, ":"); index > strings.LastIndex(ref, "/") {
		repository = ref[:index]
	}
	return repository + "@" + digest
}

// environmentReconciler builds the reconciler under test with an injected
// registry and no cluster.
func environmentReconciler(t *testing.T, reg *fakeRegistry, objects ...client.Object) (*controller.VcrGpuClusterReconciler, client.Client, *record.FakeRecorder) {
	t.Helper()
	c := testClient(t, objects...)
	recorder := record.NewFakeRecorder(32)
	reconciler, err := controller.NewVcrGpuClusterReconciler(controller.Options{
		Client:            c,
		OperatorNamespace: testOperatorNamespace,
		DevicePluginImage: testDevicePluginImage,
		NewResolver:       reg.factory,
		Now:               func() time.Time { return fixedTime },
		RetryInterval:     30 * time.Second,
		Recorder:          recorder,
	})
	if err != nil {
		t.Fatalf("NewVcrGpuClusterReconciler returned error: %v", err)
	}
	return reconciler, c, recorder
}

// reconcile runs one reconcile and returns the stored environment.
func reconcile(t *testing.T, reconciler *controller.VcrGpuClusterReconciler, c client.Client) (*vcrv1alpha1.VcrGpuCluster, ctrl.Result) {
	t.Helper()
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: vcrv1alpha1.VcrGpuClusterNameDefault}}
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	stored := &vcrv1alpha1.VcrGpuCluster{}
	if err := c.Get(context.Background(), request.NamespacedName, stored); err != nil {
		t.Fatalf("get reconciled environment: %v", err)
	}
	return stored, result
}

// makeDevicePluginReady simulates kubelet and the DaemonSet controller reporting
// a fully ready plugin, which is outside the operator's control.
func makeDevicePluginReady(t *testing.T, c client.Client, nodes int32) {
	t.Helper()
	setDaemonSetStatus(t, c, nodes, nodes)
}

func TestReconcilePublishesSupportedVersionAndConditions(t *testing.T) {
	env := testEnvironment()
	reg := newFakeRegistry()
	reconciler, c, _ := environmentReconciler(t, reg, env)

	stored, result := reconcile(t, reconciler, c)

	if got := stored.Status.SupportedVLLMVersion; got != names.SupportedVLLMVersion {
		t.Errorf("status.supportedVLLMVersion = %q, want %q", got, names.SupportedVLLMVersion)
	}
	if got := stored.Status.ObservedGeneration; got != env.Generation {
		t.Errorf("status.observedGeneration = %d, want %d", got, env.Generation)
	}

	profiles := conditionOf(t, stored, vcrv1alpha1.ConditionProfilesReconciled)
	if profiles.Status != metav1.ConditionTrue || profiles.Reason != vcrv1alpha1.ReasonAllProfilesUsable {
		t.Errorf("ProfilesReconciled = %s/%s, want True/%s", profiles.Status, profiles.Reason, vcrv1alpha1.ReasonAllProfilesUsable)
	}
	if !strings.Contains(profiles.Message, "no profile") {
		t.Errorf("ProfilesReconciled message = %q, want it to explain that no profile image is configured", profiles.Message)
	}

	devicePlugin := conditionOf(t, stored, vcrv1alpha1.ConditionDevicePluginReady)
	if devicePlugin.Status != metav1.ConditionFalse || devicePlugin.Reason != vcrv1alpha1.ReasonDaemonSetNotReady {
		t.Errorf("DevicePluginReady = %s/%s, want False/%s before kubelet reports the DaemonSet",
			devicePlugin.Status, devicePlugin.Reason, vcrv1alpha1.ReasonDaemonSetNotReady)
	}
	if !strings.Contains(devicePlugin.Message, "0 of 0") {
		t.Errorf("DevicePluginReady message = %q, want the desired and ready node counts", devicePlugin.Message)
	}

	ready := conditionOf(t, stored, vcrv1alpha1.ConditionReady)
	if ready.Status != metav1.ConditionFalse || ready.Reason != vcrv1alpha1.ReasonDevicePluginNotReady {
		t.Errorf("Ready = %s/%s, want False/%s", ready.Status, ready.Reason, vcrv1alpha1.ReasonDevicePluginNotReady)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %s, want no requeue once profiles are reconciled", result.RequeueAfter)
	}

	makeDevicePluginReady(t, c, 2)
	stored, _ = reconcile(t, reconciler, c)

	devicePlugin = conditionOf(t, stored, vcrv1alpha1.ConditionDevicePluginReady)
	if devicePlugin.Status != metav1.ConditionTrue || devicePlugin.Reason != vcrv1alpha1.ReasonDaemonSetReady {
		t.Errorf("DevicePluginReady = %s/%s, want True/%s once every node runs the plugin",
			devicePlugin.Status, devicePlugin.Reason, vcrv1alpha1.ReasonDaemonSetReady)
	}
	ready = conditionOf(t, stored, vcrv1alpha1.ConditionReady)
	if ready.Status != metav1.ConditionTrue || ready.Reason != vcrv1alpha1.ReasonEnvironmentReady {
		t.Errorf("Ready = %s/%s, want True/%s", ready.Status, ready.Reason, vcrv1alpha1.ReasonEnvironmentReady)
	}
}

func TestReconcileRejectsANonDefaultEnvironmentName(t *testing.T) {
	env := testEnvironment()
	env.Name = "not-default"
	reg := newFakeRegistry()
	reconciler, _, _ := environmentReconciler(t, reg, env)

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: env.Name}}
	_, err := reconciler.Reconcile(context.Background(), request)
	if err == nil {
		t.Fatal("Reconcile accepted a non-default environment name, want a rejection naming the accepted name")
	}
	for _, want := range []string{env.Name, vcrv1alpha1.VcrGpuClusterNameDefault} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err.Error(), want)
		}
	}
	if reg.callCount() != 0 {
		t.Errorf("registry was called %d times for a rejected environment", reg.callCount())
	}
}

func TestReconcilePublishesResolvedProfiles(t *testing.T) {
	env := testEnvironment()
	image := "ghcr.io/example/profiles/qwen3-h200:v1"
	env.Spec.Profiles = []vcrv1alpha1.ProfileSource{{Image: image}}
	metadata := contractMetadata(t, "qwen3-0.6b-h200-tp1-bf16", "Qwen/Qwen3-0.6B")

	reg := newFakeRegistry()
	reg.resolve(image, "sha256:1111111111111111111111111111111111111111111111111111111111111111", metadata)
	reconciler, c, _ := environmentReconciler(t, reg, env)

	stored, _ := reconcile(t, reconciler, c)

	if len(stored.Status.Profiles) != 1 {
		t.Fatalf("status.profiles has %d entries, want 1: %+v", len(stored.Status.Profiles), stored.Status.Profiles)
	}
	profile := stored.Status.Profiles[0]
	if profile.Image != image {
		t.Errorf("published image = %q, want the configured image %q", profile.Image, image)
	}
	if want := "sha256:1111111111111111111111111111111111111111111111111111111111111111"; profile.Digest != want {
		t.Errorf("published digest = %q, want %q", profile.Digest, want)
	}
	if profile.Metadata != string(metadata) {
		t.Errorf("published metadata = %q, want the profile document verbatim", profile.Metadata)
	}
	if profile.Message != "" {
		t.Errorf("published message = %q, want empty for a usable profile", profile.Message)
	}
	if !profile.ReconciledAt.Time.Equal(fixedTime) {
		t.Errorf("reconciledAt = %s, want %s", profile.ReconciledAt.Time, fixedTime)
	}

	profiles := conditionOf(t, stored, vcrv1alpha1.ConditionProfilesReconciled)
	if profiles.Status != metav1.ConditionTrue {
		t.Errorf("ProfilesReconciled = %s (%s: %s), want True", profiles.Status, profiles.Reason, profiles.Message)
	}
}

func TestReconcileKeepsUsableProfilesWhenOneIsUnusable(t *testing.T) {
	env := testEnvironment()
	good := "ghcr.io/example/profiles/good:v1"
	bad := "ghcr.io/example/profiles/bad:v1"
	env.Spec.Profiles = []vcrv1alpha1.ProfileSource{{Image: good}, {Image: bad}}

	reg := newFakeRegistry()
	reg.resolve(good, "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		contractMetadata(t, "good-profile", "Qwen/Qwen3-0.6B"))
	// A document missing the contract's required fields is unusable, not
	// partially trusted.
	reg.resolve(bad, "sha256:3333333333333333333333333333333333333333333333333333333333333333",
		[]byte(`{"id":"bad-profile"}`))
	reconciler, c, recorder := environmentReconciler(t, reg, env)

	stored, result := reconcile(t, reconciler, c)

	if len(stored.Status.Profiles) != 2 {
		t.Fatalf("status.profiles has %d entries, want both configured images: %+v", len(stored.Status.Profiles), stored.Status.Profiles)
	}
	if stored.Status.Profiles[0].Message != "" {
		t.Errorf("usable profile was published with message %q", stored.Status.Profiles[0].Message)
	}
	unusable := stored.Status.Profiles[1]
	if unusable.Message == "" {
		t.Error("unusable profile was published without a message")
	}
	if unusable.Digest == "" {
		t.Error("unusable profile lost the digest it resolved to, so diagnostics cannot name the artifact")
	}
	if unusable.Metadata != "" {
		t.Errorf("unusable profile published unvalidated metadata %q", unusable.Metadata)
	}

	profiles := conditionOf(t, stored, vcrv1alpha1.ConditionProfilesReconciled)
	if profiles.Status != metav1.ConditionFalse || profiles.Reason != vcrv1alpha1.ReasonProfileUnusable {
		t.Errorf("ProfilesReconciled = %s/%s, want False/%s", profiles.Status, profiles.Reason, vcrv1alpha1.ReasonProfileUnusable)
	}
	if !strings.Contains(profiles.Message, bad) {
		t.Errorf("ProfilesReconciled message %q does not name the unusable image %q", profiles.Message, bad)
	}
	if ready := conditionOf(t, stored, vcrv1alpha1.ConditionReady); ready.Status != metav1.ConditionFalse ||
		ready.Reason != vcrv1alpha1.ReasonProfilesNotReconciled {
		t.Errorf("Ready = %s/%s, want False/%s", ready.Status, ready.Reason, vcrv1alpha1.ReasonProfilesNotReconciled)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %s, want no requeue for a metadata contract violation that only a spec change can fix", result.RequeueAfter)
	}

	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, bad) {
			t.Errorf("event %q does not name the unusable image", event)
		}
	default:
		t.Error("no event was recorded for an unusable profile")
	}
}

func TestReconcileRequeuesOnRegistryFailure(t *testing.T) {
	env := testEnvironment()
	image := "ghcr.io/example/profiles/qwen3-h200:v1"
	env.Spec.Profiles = []vcrv1alpha1.ProfileSource{{Image: image}}

	reg := newFakeRegistry()
	reg.fail(image, errRegistryUnavailable)
	reconciler, c, _ := environmentReconciler(t, reg, env)

	stored, result := reconcile(t, reconciler, c)

	if len(stored.Status.Profiles) != 1 || stored.Status.Profiles[0].Message == "" {
		t.Fatalf("status.profiles = %+v, want the failed image published with a message", stored.Status.Profiles)
	}
	profiles := conditionOf(t, stored, vcrv1alpha1.ConditionProfilesReconciled)
	if profiles.Status != metav1.ConditionFalse || profiles.Reason != vcrv1alpha1.ReasonRegistryUnavailable {
		t.Errorf("ProfilesReconciled = %s/%s, want False/%s", profiles.Status, profiles.Reason, vcrv1alpha1.ReasonRegistryUnavailable)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %s, want the configured 30s retry for a transient registry failure", result.RequeueAfter)
	}
}

func TestReconcileDoesNotResolveAlreadyReconciledProfiles(t *testing.T) {
	env := testEnvironment()
	image := "ghcr.io/example/profiles/qwen3-h200:v1"
	env.Spec.Profiles = []vcrv1alpha1.ProfileSource{{Image: image}}

	reg := newFakeRegistry()
	reg.resolve(image, "sha256:4444444444444444444444444444444444444444444444444444444444444444",
		contractMetadata(t, "qwen3-0.6b-h200-tp1-bf16", "Qwen/Qwen3-0.6B"))
	reconciler, c, _ := environmentReconciler(t, reg, env)

	first, _ := reconcile(t, reconciler, c)
	second, _ := reconcile(t, reconciler, c)

	if got := reg.callCount(); got != 1 {
		t.Errorf("registry was called %d times for one unchanged profile, want 1: a resolved digest is stable", got)
	}
	if second.ResourceVersion != first.ResourceVersion {
		t.Errorf("environment resource version changed from %q to %q: an unchanged reconcile must not write status",
			first.ResourceVersion, second.ResourceVersion)
	}
	if len(second.Status.Profiles) != 1 || second.Status.Profiles[0].Digest != first.Status.Profiles[0].Digest {
		t.Errorf("published digest changed between reconciles: %+v then %+v", first.Status.Profiles, second.Status.Profiles)
	}
	if !second.Status.Profiles[0].ReconciledAt.Time.Equal(fixedTime) {
		t.Errorf("reconciledAt moved to %s on a reused entry, want the original %s",
			second.Status.Profiles[0].ReconciledAt.Time, fixedTime)
	}
}

func TestReconcileReportsACatalogConflict(t *testing.T) {
	env := testEnvironment()
	first := "ghcr.io/example/profiles/one:v1"
	second := "ghcr.io/example/profiles/two:v1"
	env.Spec.Profiles = []vcrv1alpha1.ProfileSource{{Image: first}, {Image: second}}

	// The same recorded identity published with two digests is ambiguous: the
	// recorded timing and content of one id must not depend on which image a
	// workload happened to match.
	reg := newFakeRegistry()
	reg.resolve(first, "sha256:5555555555555555555555555555555555555555555555555555555555555555",
		contractMetadata(t, "same-profile-id", "Qwen/Qwen3-0.6B"))
	reg.resolve(second, "sha256:6666666666666666666666666666666666666666666666666666666666666666",
		contractMetadata(t, "same-profile-id", "Qwen/Qwen3-0.6B"))
	reconciler, c, _ := environmentReconciler(t, reg, env)

	stored, _ := reconcile(t, reconciler, c)

	profiles := conditionOf(t, stored, vcrv1alpha1.ConditionProfilesReconciled)
	if profiles.Status != metav1.ConditionFalse {
		t.Fatalf("ProfilesReconciled = %s, want False for two digests of one profile id", profiles.Status)
	}
	if !strings.Contains(profiles.Message, "same-profile-id") {
		t.Errorf("ProfilesReconciled message %q does not name the conflicting profile id", profiles.Message)
	}
	if ready := conditionOf(t, stored, vcrv1alpha1.ConditionReady); ready.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %s, want False while the profile catalog is ambiguous", ready.Status)
	}
}

func TestReconcileUsesPullSecretCredentialsPerRegistry(t *testing.T) {
	env := testEnvironment()
	image := "ghcr.io/example/profiles/qwen3-h200:v1"
	env.Spec.Profiles = []vcrv1alpha1.ProfileSource{{Image: image}}
	env.Spec.ImagePullSecrets = []vcrv1alpha1.LocalSecretReference{{Name: "profile-pull"}}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "profile-pull", Namespace: testOperatorNamespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"ghcr.io":{"username":"profile-puller","password":"pull-token","auth":"cHJvZmlsZS1wdWxsZXI6cHVsbC10b2tlbg=="}}}`),
		},
	}

	reg := newFakeRegistry()
	reg.resolve(image, "sha256:7777777777777777777777777777777777777777777777777777777777777777",
		contractMetadata(t, "qwen3-0.6b-h200-tp1-bf16", "Qwen/Qwen3-0.6B"))
	reconciler, c, _ := environmentReconciler(t, reg, env, secret)

	reconcile(t, reconciler, c)

	options := reg.lastOptions()
	if options.Credentials == nil {
		t.Fatal("registry was built without credentials although a pull Secret is configured")
	}
	authenticator, err := options.Credentials(context.Background(), "ghcr.io")
	if err != nil {
		t.Fatalf("credentials callback returned error: %v", err)
	}
	config, err := authenticator.Authorization()
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}
	if config.Username != "profile-puller" || config.Password != "pull-token" {
		t.Errorf("credentials resolved to %q/%q, want the pull Secret values", config.Username, config.Password)
	}

	anonymous, err := options.Credentials(context.Background(), "quay.io")
	if err != nil {
		t.Fatalf("credentials callback for an unknown host returned error: %v", err)
	}
	if anonymous != authn.Anonymous {
		t.Errorf("credentials for an unknown registry = %#v, want anonymous access", anonymous)
	}
}

func TestReconcilePerProfilePullSecretOverridesEnvironmentSecrets(t *testing.T) {
	env := testEnvironment()
	image := "ghcr.io/example/profiles/qwen3-h200:v1"
	env.Spec.Profiles = []vcrv1alpha1.ProfileSource{{Image: image, PullSecretName: "profile-only"}}
	env.Spec.ImagePullSecrets = []vcrv1alpha1.LocalSecretReference{{Name: "environment-wide"}}

	environmentSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "environment-wide", Namespace: testOperatorNamespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"quay.io":{"username":"wide","password":"wide-token"}}}`),
		},
	}
	profileSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "profile-only", Namespace: testOperatorNamespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"ghcr.io":{"username":"profile","password":"profile-token"}}}`),
		},
	}

	reg := newFakeRegistry()
	reg.resolve(image, "sha256:8888888888888888888888888888888888888888888888888888888888888888",
		contractMetadata(t, "qwen3-0.6b-h200-tp1-bf16", "Qwen/Qwen3-0.6B"))
	reconciler, c, _ := environmentReconciler(t, reg, env, environmentSecret, profileSecret)

	reconcile(t, reconciler, c)

	options := reg.lastOptions()
	if options.Credentials == nil {
		t.Fatal("registry was built without credentials although a per-profile pull Secret is configured")
	}
	authenticator, err := options.Credentials(context.Background(), "ghcr.io")
	if err != nil {
		t.Fatalf("credentials callback returned error: %v", err)
	}
	config, err := authenticator.Authorization()
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}
	if config.Username != "profile" {
		t.Errorf("credentials came from %q, want the per-profile pull Secret", config.Username)
	}
}

func TestReconcileNeverPublishesPullSecretValues(t *testing.T) {
	const password = "super-secret-pull-token"
	const username = "secret-pull-user"

	env := testEnvironment()
	image := "ghcr.io/example/profiles/qwen3-h200:v1"
	env.Spec.Profiles = []vcrv1alpha1.ProfileSource{{Image: image}}
	env.Spec.ImagePullSecrets = []vcrv1alpha1.LocalSecretReference{{Name: "profile-pull"}}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "profile-pull", Namespace: testOperatorNamespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(fmt.Sprintf(
				`{"auths":{"ghcr.io":{"username":%q,"password":%q,"auth":%q}}}`,
				username, password, base64.StdEncoding.EncodeToString([]byte(username+":"+password)))),
		},
	}

	// The registry rejects the pull credential; its error must not be echoed into
	// diagnostics beyond the reference and host.
	reg := newFakeRegistry()
	reg.fail(image, errors.New("profile image \"ghcr.io/example/profiles/qwen3-h200:v1\": fetch from registry: unauthorized"))
	reconciler, c, recorder := environmentReconciler(t, reg, env, secret)

	stored, _ := reconcile(t, reconciler, c)

	diagnostics := strings.Builder{}
	for _, profile := range stored.Status.Profiles {
		diagnostics.WriteString(profile.Message)
	}
	for _, condition := range stored.Status.Conditions {
		diagnostics.WriteString(condition.Message)
	}
	for {
		select {
		case event := <-recorder.Events:
			diagnostics.WriteString(event)
			continue
		default:
		}
		break
	}
	published := diagnostics.String()
	if published == "" {
		t.Fatal("no diagnostics were published for a failing profile")
	}
	for _, value := range []string{password, username} {
		if strings.Contains(published, value) {
			t.Errorf("pull Secret value %q appears in published diagnostics: %q", value, published)
		}
	}
	if !strings.Contains(published, image) {
		t.Errorf("diagnostics %q do not name the failing image", published)
	}
}

func TestReconcileRejectsUnusablePullSecretWithoutLeakingIt(t *testing.T) {
	const password = "another-secret-token"

	env := testEnvironment()
	image := "ghcr.io/example/profiles/qwen3-h200:v1"
	env.Spec.Profiles = []vcrv1alpha1.ProfileSource{{Image: image}}
	env.Spec.ImagePullSecrets = []vcrv1alpha1.LocalSecretReference{{Name: "broken-pull"}}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "broken-pull", Namespace: testOperatorNamespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		// Not a docker config document, and it contains credential-looking data
		// that must never be echoed.
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(password)},
	}

	reg := newFakeRegistry()
	reg.resolve(image, "sha256:9999999999999999999999999999999999999999999999999999999999999999",
		contractMetadata(t, "qwen3-0.6b-h200-tp1-bf16", "Qwen/Qwen3-0.6B"))
	reconciler, c, _ := environmentReconciler(t, reg, env, secret)

	stored, result := reconcile(t, reconciler, c)

	if len(stored.Status.Profiles) != 1 || stored.Status.Profiles[0].Message == "" {
		t.Fatalf("status.profiles = %+v, want the profile published as unusable", stored.Status.Profiles)
	}
	message := stored.Status.Profiles[0].Message
	if !strings.Contains(message, "broken-pull") {
		t.Errorf("message %q does not name the unusable pull Secret", message)
	}
	if strings.Contains(message, password) {
		t.Errorf("message %q contains the Secret payload", message)
	}
	if result.RequeueAfter == 0 {
		t.Errorf("RequeueAfter = 0, want a retry: creating the missing Secret must heal the profile without a spec change")
	}
	if reg.callCount() != 0 {
		t.Errorf("registry was called %d times although no credential could be read", reg.callCount())
	}
}

func TestNewVcrGpuClusterReconcilerValidatesItsConfiguration(t *testing.T) {
	c := testClient(t)
	tests := []struct {
		name   string
		opts   controller.Options
		absent string
	}{
		{
			name:   "no client",
			opts:   controller.Options{OperatorNamespace: testOperatorNamespace, DevicePluginImage: testDevicePluginImage},
			absent: "client",
		},
		{
			name:   "no operator namespace",
			opts:   controller.Options{Client: c, DevicePluginImage: testDevicePluginImage},
			absent: "namespace",
		},
		{
			name:   "no device plugin image",
			opts:   controller.Options{Client: c, OperatorNamespace: testOperatorNamespace},
			absent: controller.EnvDevicePluginImage,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := controller.NewVcrGpuClusterReconciler(tc.opts)
			if err == nil {
				t.Fatalf("NewVcrGpuClusterReconciler accepted a configuration without %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.absent) {
				t.Errorf("error %q does not name the missing setting %q", err.Error(), tc.absent)
			}
		})
	}
}

func TestDevicePluginImageFromEnv(t *testing.T) {
	if got := controller.DevicePluginImageFromEnv(func(string) string { return "" }); got != controller.DefaultDevicePluginImage {
		t.Errorf("image = %q, want the documented default %q", got, controller.DefaultDevicePluginImage)
	}
	if got := controller.DevicePluginImageFromEnv(func(key string) string {
		if key != controller.EnvDevicePluginImage {
			t.Errorf("looked up env var %q, want %q", key, controller.EnvDevicePluginImage)
		}
		return "registry.example.test/plugin:dev"
	}); got != "registry.example.test/plugin:dev" {
		t.Errorf("image = %q, want the injected value", got)
	}
}

func TestReconcileIgnoresADeletedEnvironment(t *testing.T) {
	reg := newFakeRegistry()
	reconciler, c, _ := environmentReconciler(t, reg)

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: vcrv1alpha1.VcrGpuClusterNameDefault}}
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile of a missing environment returned error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %s, want no requeue for a deleted environment", result.RequeueAfter)
	}
	if reg.callCount() != 0 {
		t.Errorf("registry was called %d times for a missing environment", reg.callCount())
	}

	// Nothing is created without an environment: the device plugin would
	// advertise capacity nothing configured.
	daemonSets := &appsv1.DaemonSetList{}
	if err := c.List(context.Background(), daemonSets); err != nil {
		t.Fatalf("list DaemonSets: %v", err)
	}
	if len(daemonSets.Items) != 0 {
		t.Errorf("found %d DaemonSets without an environment, want none", len(daemonSets.Items))
	}
}

// conflictClient wraps a client and fails status writes on VcrGpuCluster
// objects with a conflict, the way a concurrent writer does. failures is the
// number of updates to fail: 1 models one race, 0 (or a huge number) models a
// writer that never stops.
type conflictClient struct {
	client.Client
	failures int
	mu       sync.Mutex
	attempts int
}

func (c *conflictClient) Status() client.SubResourceWriter {
	return &conflictStatusWriter{SubResourceWriter: c.Client.Status(), owner: c}
}

func (c *conflictClient) attempt() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts++
	return c.attempts <= c.failures
}

func (c *conflictClient) attemptCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts
}

type conflictStatusWriter struct {
	client.SubResourceWriter
	owner *conflictClient
}

func (w *conflictStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if _, ok := obj.(*vcrv1alpha1.VcrGpuCluster); ok && w.owner.attempt() {
		return apierrors.NewConflict(
			vcrv1alpha1.GroupVersion.WithResource("vcrgpuclusters").GroupResource(),
			obj.GetName(), errors.New("the object has been modified; apply your changes to the latest version"))
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

// TestReconcileRetriesAConflictingStatusUpdate asserts that a status conflict is
// re-read and retried inside one reconcile instead of being handed to the work
// queue: the environment must be published by the reconcile that hit the conflict.
func TestReconcileRetriesAConflictingStatusUpdate(t *testing.T) {
	env := testEnvironment()
	reg := newFakeRegistry()
	reg.resolve("registry.test/profile:1", "sha256:"+strings.Repeat("a", 64), contractMetadata(t, "profile-a", "model-a"))
	env.Spec.Profiles = []vcrv1alpha1.ProfileSource{{Image: "registry.test/profile:1"}}
	reconciler, c, _ := environmentReconciler(t, reg, env)

	conflicting := &conflictClient{Client: c, failures: 1}
	reconciler.Client = conflicting

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: vcrv1alpha1.VcrGpuClusterNameDefault}}
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile returned error for a single status conflict, want a retried write: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %s, want no requeue: the conflict retry published the status", result.RequeueAfter)
	}
	if attempts := conflicting.attemptCount(); attempts != 2 {
		t.Errorf("status update attempts = %d, want 2 (one conflict, one success)", attempts)
	}

	stored := &vcrv1alpha1.VcrGpuCluster{}
	if err := c.Get(context.Background(), request.NamespacedName, stored); err != nil {
		t.Fatalf("get the reconciled environment: %v", err)
	}
	if len(stored.Status.Profiles) != 1 {
		t.Fatalf("status.profiles = %+v, want the resolved profile", stored.Status.Profiles)
	}
	if metadata := stored.Status.Profiles[0].Metadata; !strings.Contains(metadata, `"profile-a"`) {
		t.Errorf("status.profiles[0].metadata = %q, want the resolved profile-a document", metadata)
	}
	if condition := conditionOf(t, stored, vcrv1alpha1.ConditionProfilesReconciled); condition.Status != metav1.ConditionTrue {
		t.Errorf("ProfilesReconciled = %s, want True after the retried write", condition.Status)
	}
}

// TestReconcileRequeuesWhenStatusRetriesAreExhausted asserts the fallback: when a
// concurrent writer never yields, the reconcile gives up retrying and asks the
// work queue to try again rather than reporting a hard error.
func TestReconcileRequeuesWhenStatusRetriesAreExhausted(t *testing.T) {
	env := testEnvironment()
	reg := newFakeRegistry()
	reconciler, c, _ := environmentReconciler(t, reg, env)

	conflicting := &conflictClient{Client: c, failures: 1 << 30}
	reconciler.Client = conflicting

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: vcrv1alpha1.VcrGpuClusterNameDefault}}
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile returned error after the retry budget, want a requeue: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %s, want the configured 30s retry so a persistent conflict is retried", result.RequeueAfter)
	}
	if attempts := conflicting.attemptCount(); attempts < 2 {
		t.Errorf("status update attempts = %d, want the retry budget to be spent before requeueing", attempts)
	}
}

// TestEmptyNodeSelectorSelectsEveryNode covers the relaxed API: an empty selector
// follows Kubernetes semantics and matches all nodes, so the DaemonSet carries no
// node selector. Synthetic capacity on control-plane nodes is then legitimate but
// surprising, so the reconciler says so in an event.
func TestEmptyNodeSelectorSelectsEveryNode(t *testing.T) {
	env := testEnvironment()
	env.Spec.NodeSelection.NodeSelector = nil
	reg := newFakeRegistry()
	reconciler, c, recorder := environmentReconciler(t, reg, env)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: env.Name},
	}); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	daemonSet := &appsv1.DaemonSet{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: testOperatorNamespace,
		Name:      names.DevicePluginName,
	}, daemonSet); err != nil {
		t.Fatalf("get device plugin DaemonSet: %v", err)
	}
	if len(daemonSet.Spec.Template.Spec.NodeSelector) != 0 {
		t.Errorf("DaemonSet node selector = %v, want none for an empty selection", daemonSet.Spec.Template.Spec.NodeSelector)
	}

	found := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "AllNodesSelected") {
				found = true
			}
		default:
			if !found {
				t.Error("no AllNodesSelected warning was recorded for an empty node selector")
			}
			return
		}
	}
}

// TestProfileRegistryResolvesEntriesInsideIt: with a registry configured, an entry names an
// identity inside it, so the catalog publishes the full reference and the admission side
// keeps working with absolute images.
func TestProfileRegistryResolvesEntriesInsideIt(t *testing.T) {
	const base = "ghcr.io/acme/vllm-vcr-profiles"
	env := testEnvironment()
	env.Spec.ProfileRegistry = base
	env.Spec.Profiles = []vcrv1alpha1.ProfileSource{{Image: "qwen3-0.6b-h200-tp1-bf16-0.1.0"}}

	digest := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	metadata := contractMetadata(t, "qwen3-0.6b-h200-tp1-bf16", "Qwen/Qwen3-0.6B")

	reg := newFakeRegistry()
	reg.resolve(base+":qwen3-0.6b-h200-tp1-bf16-0.1.0", digest, metadata)
	reconciler, c, _ := environmentReconciler(t, reg, env)

	stored, _ := reconcile(t, reconciler, c)

	if len(stored.Status.Profiles) != 1 {
		t.Fatalf("status.profiles = %+v", stored.Status.Profiles)
	}
	want := base + ":qwen3-0.6b-h200-tp1-bf16-0.1.0"
	if got := stored.Status.Profiles[0].Image; got != want {
		t.Errorf("published image = %q, want %q", got, want)
	}
	if got := stored.Status.Profiles[0].Digest; got != digest {
		t.Errorf("published digest = %q, want %q", got, digest)
	}
	if calls := reg.callCount(); calls != 1 {
		t.Errorf("registry was called %d times, want the joined reference resolved once", calls)
	}
}

// TestProfileRegistryRefusesAnotherRegistry: one configured registry means one source of
// profiles. An entry naming a different host is reported on the catalog instead of fetched.
func TestProfileRegistryRefusesAnotherRegistry(t *testing.T) {
	env := testEnvironment()
	env.Spec.ProfileRegistry = "ghcr.io/acme/vllm-vcr-profiles"
	env.Spec.Profiles = []vcrv1alpha1.ProfileSource{{Image: "quay.io/other/profiles:qwen3"}}

	reg := newFakeRegistry()
	reconciler, c, _ := environmentReconciler(t, reg, env)

	stored, _ := reconcile(t, reconciler, c)

	entry := stored.Status.Profiles[0]
	if !strings.Contains(entry.Message, "profileRegistry") {
		t.Fatalf("entry message %q does not explain the refusal", entry.Message)
	}
	if len(reg.calls) != 0 {
		t.Errorf("resolver was called for a reference outside the registry: %v", reg.calls)
	}
}
