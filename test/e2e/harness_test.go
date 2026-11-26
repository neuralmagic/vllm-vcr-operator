package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/controller"
	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
	"github.com/neuralmagic/vllm-vcr-operator/internal/registry"
	operatorwebhook "github.com/neuralmagic/vllm-vcr-operator/internal/webhook"
)

const (
	// operatorNamespace holds the operator-owned resources and the pull Secrets
	// the webhook projects from. It is the namespace the shipped admission
	// configuration excludes from Pod mutation.
	operatorNamespace = "vllm-vcr-operator-system"

	// devicePluginImage and the runtime images are placeholders: nothing is
	// pulled here, and the assertions only read the references back.
	devicePluginImage = "quay.io/opendatahub/vllm-vcr-device-plugin:e2e"
	frontendImage     = "quay.io/opendatahub/vllm-vcr-frontend:e2e-cpu"
	backendImage      = "quay.io/opendatahub/vllm-vcr-backend:e2e"

	// profileRepository is the repository the fake registry serves profiles
	// from. Every reference is a tag; the fake resolver pins the digest, which
	// is the same path a real registry takes.
	profileRepository = "quay.io/opendatahub/vllm-vcr-profiles"

	// privateProfileImage is a profile a workload needs credentials to pull.
	privateProfileImage = "registry.example.com/vcr-profiles:qwen3-private"

	// privatePullSecretName is the operator-namespace Secret carrying the
	// credentials for privateProfileImage.
	privatePullSecretName = "e2e-private-profile-pull"

	// privatePullSecretValue is planted in the operator-namespace Secret. It is
	// never a real credential: the suite asserts it appears nowhere in any
	// annotation, event, or status.
	privatePullSecretValue = "e2e-not-a-real-credential-7f3c"

	// goodProfileImage, twinProfileImage, and unusableProfileImage are the three
	// reconciled catalog inputs the suite configures.
	goodProfileImage     = profileRepository + ":e2e-qwen3-0.6b-h200-tp1-bf16"
	twinProfileImage     = profileRepository + ":e2e-qwen3-twin"
	unusableProfileImage = profileRepository + ":e2e-unusable-metadata"

	// profileID and model identity the metadata fixture records.
	profileID     = "e2e-qwen3-0.6b-h200-tp1-bf16"
	twinProfileID = "e2e-qwen3-twin-h200-tp1-bf16"
	modelName     = "Qwen/Qwen3-0.6B"
	modelRevision = "c9340e6f3"

	simulationNodeLabel = "vcr.opendatahub-io/simulation"
)

// Fake digests, one per profile image. They are digest-shaped so that the code
// under test treats them exactly as it treats a registry's answer.
const (
	goodProfileDigest    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	twinProfileDigest    = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	unusableDigest       = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	privateProfileDigest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
)

// skipReason is set when the suite cannot start; every test then skips with it.
var (
	suite      *harness
	skipReason string
)

// TestMain starts one API server, one manager, and the shared environment for the
// whole package, and tears them down once.
func TestMain(m *testing.M) {
	code, err := runSuite(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// runSuite starts and stops the suite around m.Run.
//
// A missing asset directory is not an error: it is reported once and every test
// skips with the same reason, so a plain `go test ./...` stays green on a machine
// without envtest assets without pretending to have run the suite.
func runSuite(m *testing.M) (int, error) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		skipReason = "KUBEBUILDER_ASSETS is not set: run `make test-e2e`, which resolves envtest assets with setup-envtest"
		fmt.Fprintf(os.Stderr, "e2e: %s\n", skipReason)
		return m.Run(), nil
	}

	started, err := startSuite()
	if err != nil {
		return 1, err
	}
	defer started.stop()
	suite = started
	return m.Run(), nil
}

// harness is the suite's shared state: the API server, the manager, the fake
// registry, and the direct (uncached) client used for setup and assertions.
type harness struct {
	root string

	env          *envtest.Environment
	cfg          *rest.Config
	scheme       *runtime.Scheme
	client       client.Client
	cachedClient client.Client

	registry *fakeRegistry

	cancelManager context.CancelFunc
	managerDone   chan error
}

// startSuite boots envtest, installs the CRDs and webhook configurations, starts
// the operator in-process, and creates the singleton environment.
func startSuite() (_ *harness, err error) {
	ctrl.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(vcrv1alpha1.AddToScheme(scheme))

	h := &harness{root: root, scheme: scheme, managerDone: make(chan error, 1)}

	h.env = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		// The shipped admission configurations are installed unchanged: their
		// rules, failure policy, side effects, and namespace selector are part
		// of what the suite accepts. envtest replaces the service reference with
		// the local serving endpoint and injects the CA bundle.
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{filepath.Join(root, "config", "webhook")},
		},
	}

	cfg, err := h.env.Start()
	if err != nil {
		return nil, fmt.Errorf("start envtest: %w", err)
	}
	h.cfg = cfg
	defer func() {
		if err != nil {
			_ = h.env.Stop()
		}
	}()

	h.client, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("build direct client: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.cancelManager = cancel

	if err := h.createNamespace(ctx, operatorNamespace); err != nil {
		return nil, err
	}
	// The API server validates a Pod's runtimeClassName before admission runs, so
	// the accelerator runtime class named by real GPU manifests must exist for the
	// rewrite to be observable. envtest has no CRI, so the handler is nominal.
	if err := h.createRuntimeClass(ctx); err != nil {
		return nil, err
	}
	if err := h.startManager(ctx); err != nil {
		return nil, err
	}
	if err := h.createBaselineEnvironment(ctx); err != nil {
		return nil, err
	}
	return h, nil
}

// createRuntimeClass registers the accelerator RuntimeClass that GPU-targeted
// manifests reference. Without the object the API server rejects the Pod before
// any webhook sees it, which would test nothing.
func (h *harness) createRuntimeClass(ctx context.Context) error {
	class := &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "nvidia"},
		Handler:    "runc",
	}
	if err := h.client.Create(ctx, class); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create RuntimeClass %s: %w", class.Name, err)
	}
	return nil
}

// stop tears the manager down before the API server.
func (h *harness) stop() {
	if h.cancelManager != nil {
		h.cancelManager()
		select {
		case <-h.managerDone:
		case <-time.After(30 * time.Second):
		}
	}
	if h.env != nil {
		_ = h.env.Stop()
	}
}

// startManager wires the operator exactly as cmd/manager/main.go does, with the
// fake profile registry injected through controller.Options.NewResolver so no
// registry is contacted.
func (h *harness) startManager(ctx context.Context) error {
	h.registry = newFakeRegistry()
	if err := h.registerProfileFixtures(); err != nil {
		return err
	}

	install := h.env.WebhookInstallOptions
	mgr, err := ctrl.NewManager(h.cfg, ctrl.Options{
		Scheme: h.scheme,
		// Nothing needs the metrics or probe endpoints, and fixed ports would
		// collide with the operator a developer may be running.
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    install.LocalServingHost,
			Port:    install.LocalServingPort,
			CertDir: install.LocalServingCertDir,
		}),
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	h.cachedClient = mgr.GetClient()

	environmentReconciler, err := controller.NewVcrGpuClusterReconciler(controller.Options{
		Client:            mgr.GetClient(),
		OperatorNamespace: operatorNamespace,
		DevicePluginImage: devicePluginImage,
		NewResolver:       h.registry.factory,
		Recorder:          mgr.GetEventRecorderFor("vcrgpucluster"),
	})
	if err != nil {
		return fmt.Errorf("create the simulation environment reconciler: %w", err)
	}
	if err := environmentReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register the simulation environment reconciler: %w", err)
	}

	// A namespace's profile selection is resolved against the reconciled catalog, so the
	// suite exercises the same wiring the manager installs.
	if err := controller.NewVcrGpuProfileReconciler(mgr.GetClient()).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register the GPU profile selection reconciler: %w", err)
	}

	if err := operatorwebhook.SetupWithManager(mgr, operatorwebhook.Options{
		Client:            mgr.GetClient(),
		Reader:            mgr.GetAPIReader(),
		OperatorNamespace: operatorNamespace,
		FrontendImage:     frontendImage,
		BackendImage:      backendImage,
		Recorder:          mgr.GetEventRecorderFor("pod-admission"),
	}); err != nil {
		return fmt.Errorf("register the admission webhooks: %w", err)
	}

	go func() {
		h.managerDone <- mgr.Start(ctx)
	}()

	// The webhook configurations exist before the server listens, and the API
	// server calls it with failurePolicy Fail, so a Pod created too early would
	// be denied for the wrong reason.
	checker := mgr.GetWebhookServer().StartedChecker()
	if err := wait.PollUntilContextTimeout(ctx, 25*time.Millisecond, 30*time.Second, true,
		func(context.Context) (bool, error) {
			return checker(nil) == nil, nil
		}); err != nil {
		return fmt.Errorf("wait for the webhook server: %w", err)
	}
	return nil
}

// helperContext returns a context for harness operations.
//
// helperContext(t) is canceled before t.Cleanup functions run (Go 1.24+), and the
// harness restores the singleton environment from cleanups, so helpers must not
// depend on the test's own context.
func helperContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// requireSuite returns the suite, or skips the test with the recorded reason.
//
// A skipped test is not a passing test: the skip message says why nothing was
// asserted.
func requireSuite(t *testing.T) *harness {
	t.Helper()
	if suite == nil {
		t.Skipf("envtest suite unavailable: %s", skipReason)
	}
	return suite
}

// createNamespace creates a namespace for one test. envtest has no namespace
// controller, so deleted namespaces stay Terminating: each test therefore uses
// its own namespace and never waits for deletion.
func (h *harness) createNamespace(ctx context.Context, name string) error {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := h.client.Create(ctx, namespace); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %s: %w", name, err)
	}
	return nil
}

// newNamespace creates a fresh workload namespace and registers its cleanup.
func (h *harness) newNamespace(t *testing.T) string {
	t.Helper()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-"}}
	if err := h.client.Create(helperContext(t), namespace); err != nil {
		t.Fatalf("create workload namespace: %v", err)
	}
	t.Cleanup(func() {
		// The namespace controller is absent, so this only marks the namespace
		// Terminating. The API server is discarded with the suite.
		_ = h.client.Delete(context.Background(), namespace)
	})
	return namespace.Name
}

// registerProfileFixtures teaches the fake registry every profile image the
// suite configures, with metadata read from testdata/.
func (h *harness) registerProfileFixtures() error {
	fixtures := []struct {
		image  string
		digest string
		file   string
	}{
		{goodProfileImage, goodProfileDigest, "qwen3-0.6b-h200-tp1-bf16.json"},
		{twinProfileImage, twinProfileDigest, "qwen3-twin-h200-tp1-bf16.json"},
		{unusableProfileImage, unusableDigest, "unusable-metadata.json"},
		{privateProfileImage, privateProfileDigest, "qwen3-0.6b-h200-tp1-bf16.json"},
	}
	for _, fixture := range fixtures {
		raw, err := os.ReadFile(filepath.Join(h.root, "test", "e2e", "testdata", fixture.file))
		if err != nil {
			return fmt.Errorf("read profile fixture %s: %w", fixture.file, err)
		}
		h.registry.register(fixture.image, fixture.digest, raw)
	}
	return nil
}

// createBaselineEnvironment publishes the singleton configuration the admission
// tests work against.
func (h *harness) createBaselineEnvironment(ctx context.Context) error {
	env := baselineEnvironment()
	return h.client.Create(ctx, env)
}

// baselineEnvironment is the singleton VcrGpuCluster configured the way
// test/fixtures/environments/h200.yaml documents it, with the fake registry's
// profile image.
func baselineEnvironment() *vcrv1alpha1.VcrGpuCluster {
	return &vcrv1alpha1.VcrGpuCluster{
		ObjectMeta: metav1.ObjectMeta{Name: vcrv1alpha1.VcrGpuClusterNameDefault},
		Spec: vcrv1alpha1.VcrGpuClusterSpec{
			NodeSelection: vcrv1alpha1.NodeSelection{
				NodeSelector: map[string]string{simulationNodeLabel: "true"},
				Tolerations: []corev1.Toleration{{
					Key:      simulationNodeLabel,
					Operator: corev1.TolerationOpExists,
					Effect:   corev1.TaintEffectNoSchedule,
				}},
			},
			DevicesPerNode: 8,
			GPUType:        "H200",
			Profiles:       []vcrv1alpha1.ProfileSource{{Image: goodProfileImage}},
			Runtime: vcrv1alpha1.RuntimeSizing{
				Frontend: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resourceQuantity("1"),
						corev1.ResourceMemory: resourceQuantity("2Gi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resourceQuantity("2"),
						corev1.ResourceMemory: resourceQuantity("4Gi"),
					},
				},
				Backend: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resourceQuantity("2"),
						corev1.ResourceMemory: resourceQuantity("2Gi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resourceQuantity("4"),
						corev1.ResourceMemory: resourceQuantity("8Gi"),
					},
				},
				Provisioner: ptr.To(corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resourceQuantity("100m"),
						corev1.ResourceMemory: resourceQuantity("64Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resourceQuantity("500m"),
						corev1.ResourceMemory: resourceQuantity("256Mi"),
					},
				}),
			},
			LogLevel: "info",
		},
	}
}

// getEnvironment reads the singleton configuration from the direct client.
func (h *harness) getEnvironment(t *testing.T) *vcrv1alpha1.VcrGpuCluster {
	t.Helper()
	env := &vcrv1alpha1.VcrGpuCluster{}
	key := types.NamespacedName{Name: vcrv1alpha1.VcrGpuClusterNameDefault}
	if err := h.client.Get(helperContext(t), key, env); err != nil {
		t.Fatalf("get VcrGpuCluster %s: %v", key, err)
	}
	return env
}

// setProfiles replaces spec.profiles for the duration of one test and restores
// the baseline when the test finishes.
//
// The environment is a singleton, so a test that needs a different catalog must
// put it back; the restore waits for the baseline catalog to be published again
// before the test is released.
func (h *harness) setProfiles(t *testing.T, sources ...vcrv1alpha1.ProfileSource) {
	t.Helper()
	h.updateProfiles(t, sources...)
	t.Cleanup(func() {
		h.updateProfiles(t, vcrv1alpha1.ProfileSource{Image: goodProfileImage})
		h.waitForProfiles(t, goodProfileImage)
	})
}

// updateProfiles writes spec.profiles and waits for the reconciler to observe it.
func (h *harness) updateProfiles(t *testing.T, sources ...vcrv1alpha1.ProfileSource) {
	t.Helper()
	env := h.getEnvironment(t)
	env.Spec.Profiles = sources
	if err := h.client.Update(helperContext(t), env); err != nil {
		t.Fatalf("update VcrGpuCluster profiles: %v", err)
	}
}

// waitForProfiles waits until a status entry for image is published.
func (h *harness) waitForProfiles(t *testing.T, image string) vcrv1alpha1.ResolvedProfile {
	t.Helper()
	var entry vcrv1alpha1.ResolvedProfile
	h.poll(t, "a published status.profiles entry for "+image, func() (bool, error) {
		env := &vcrv1alpha1.VcrGpuCluster{}
		key := types.NamespacedName{Name: vcrv1alpha1.VcrGpuClusterNameDefault}
		if err := h.client.Get(helperContext(t), key, env); err != nil {
			return false, err
		}
		for _, published := range env.Status.Profiles {
			if published.Image == image {
				entry = published
				return true, nil
			}
		}
		return false, nil
	})
	return entry
}

// setDevicePluginReady stands in for kubelet and the DaemonSet controller, which
// envtest does not run: they report how many nodes run a ready device-plugin Pod.
//
// The suite asserts the DaemonSet the operator renders; what it cannot observe is
// a plugin actually registering with kubelet (see doc.go).
func (h *harness) setDevicePluginReady(t *testing.T, nodes int32) {
	t.Helper()
	daemonSet := &appsv1.DaemonSet{}
	key := types.NamespacedName{Namespace: operatorNamespace, Name: names.DevicePluginName}
	h.poll(t, "the device plugin DaemonSet", func() (bool, error) {
		err := h.client.Get(helperContext(t), key, daemonSet)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	})

	if daemonSet.Status.ObservedGeneration == daemonSet.Generation &&
		daemonSet.Status.DesiredNumberScheduled == nodes &&
		daemonSet.Status.NumberReady == nodes {
		return
	}
	daemonSet.Status = appsv1.DaemonSetStatus{
		ObservedGeneration:     daemonSet.Generation,
		DesiredNumberScheduled: nodes,
		CurrentNumberScheduled: nodes,
		NumberReady:            nodes,
		NumberAvailable:        nodes,
		UpdatedNumberScheduled: nodes,
	}
	if err := h.client.Status().Update(helperContext(t), daemonSet); err != nil {
		t.Fatalf("report device plugin DaemonSet status: %v", err)
	}
}

// requireReadyEnvironment drives the shared environment to Ready and waits for
// the manager's cache to observe it, so admission cannot race the status write.
func (h *harness) requireReadyEnvironment(t *testing.T) *vcrv1alpha1.VcrGpuCluster {
	t.Helper()
	h.setDevicePluginReady(t, 1)
	h.waitForCondition(t, vcrv1alpha1.ConditionReady, metav1.ConditionTrue)
	h.poll(t, "the manager cache to observe a ready VcrGpuCluster", func() (bool, error) {
		env := &vcrv1alpha1.VcrGpuCluster{}
		key := types.NamespacedName{Name: vcrv1alpha1.VcrGpuClusterNameDefault}
		if err := h.cachedClient.Get(helperContext(t), key, env); err != nil {
			return false, err
		}
		ready := meta.FindStatusCondition(env.Status.Conditions, vcrv1alpha1.ConditionReady)
		return ready != nil && ready.Status == metav1.ConditionTrue, nil
	})
	return h.getEnvironment(t)
}

// condition returns a published condition, or nil.
func condition(env *vcrv1alpha1.VcrGpuCluster, conditionType string) *metav1.Condition {
	return meta.FindStatusCondition(env.Status.Conditions, conditionType)
}

// waitForCondition waits for one status condition of the singleton environment.
func (h *harness) waitForCondition(t *testing.T, conditionType string, status metav1.ConditionStatus) metav1.Condition {
	t.Helper()
	var found metav1.Condition
	h.poll(t, fmt.Sprintf("condition %s=%s", conditionType, status), func() (bool, error) {
		env := &vcrv1alpha1.VcrGpuCluster{}
		key := types.NamespacedName{Name: vcrv1alpha1.VcrGpuClusterNameDefault}
		if err := h.client.Get(helperContext(t), key, env); err != nil {
			return false, err
		}
		current := condition(env, conditionType)
		if current == nil || current.Status != status {
			return false, nil
		}
		found = *current
		return true, nil
	})
	return found
}

// poll retries observe until it reports success or the suite timeout expires.
//
// Timeouts are deliberately short: the suite asserts operator behavior, not
// scheduler latency, and a stuck reconcile must fail fast rather than run into
// the package timeout.
func (h *harness) poll(t *testing.T, what string, observe func() (bool, error)) {
	t.Helper()
	const (
		timeout  = 15 * time.Second
		interval = 20 * time.Millisecond
	)
	if err := wait.PollUntilContextTimeout(helperContext(t), interval, timeout, true,
		func(context.Context) (bool, error) { return observe() }); err != nil {
		t.Fatalf("timed out waiting for %s: %v", what, err)
	}
}

// resolverFunc adapts a function to registry.Resolver.
type resolverFunc func(ctx context.Context, ref string) (registry.Resolved, error)

// Resolve implements registry.Resolver.
func (f resolverFunc) Resolve(ctx context.Context, ref string) (registry.Resolved, error) {
	return f(ctx, ref)
}

// fakeRegistry is the injected registry.Resolver factory. It records every
// reference it was asked to resolve and every credential callback it was built
// with, so the suite can assert that reconciliation used the configured pull
// Secrets without contacting anything.
type fakeRegistry struct {
	mu       sync.Mutex
	resolved map[string]registry.Resolved
	options  []registry.Options
	calls    []string
}

// newFakeRegistry returns an empty fake registry.
func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{resolved: map[string]registry.Resolved{}}
}

// register makes ref resolve to a digest-pinned image carrying metadata.
func (f *fakeRegistry) register(ref, digest string, metadata []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved[ref] = registry.Resolved{
		Image:    pinnedReference(ref, digest),
		Digest:   digest,
		Metadata: metadata,
		Source:   registry.SourceLabel,
	}
}

// factory is the controller.ResolverFactory the reconciler is built with.
func (f *fakeRegistry) factory(opts registry.Options) registry.Resolver {
	f.mu.Lock()
	f.options = append(f.options, opts)
	f.mu.Unlock()
	return resolverFunc(f.resolve)
}

// resolve answers from the registered fixtures.
func (f *fakeRegistry) resolve(_ context.Context, ref string) (registry.Resolved, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ref)
	resolved, ok := f.resolved[ref]
	if !ok {
		return registry.Resolved{}, fmt.Errorf("profile image %q: fetch from registry: not found", ref)
	}
	return resolved, nil
}

// lastOptions returns the most recent resolver options.
func (f *fakeRegistry) lastOptions() registry.Options {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.options) == 0 {
		return registry.Options{}
	}
	return f.options[len(f.options)-1]
}

// hasCredentials reports whether any resolver was built with a credential
// callback: reconciliation only does that when pull Secrets are configured.
func (f *fakeRegistry) hasCredentials() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, opts := range f.options {
		if opts.Credentials != nil {
			return true
		}
	}
	return false
}

// pinnedReference renders the digest-pinned form of a reference.
func pinnedReference(ref, digest string) string {
	repository := ref
	if index := strings.Index(repository, "@"); index >= 0 {
		repository = repository[:index]
	}
	if index := strings.LastIndex(repository, ":"); index > strings.LastIndex(repository, "/") {
		repository = repository[:index]
	}
	return repository + "@" + digest
}
