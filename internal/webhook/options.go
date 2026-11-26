package webhook

import (
	"fmt"
	"time"

	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
)

const (
	// EnvFrontendImage names the environment variable holding the Python vLLM
	// frontend image injected for an admitted workload.
	EnvFrontendImage = "VCR_FRONTEND_IMAGE"

	// EnvBackendImage names the environment variable holding the vllm-vcr
	// simulator image injected for an admitted workload.
	EnvBackendImage = "VCR_BACKEND_IMAGE"

	// DefaultFrontendImage is the placeholder frontend image used when
	// EnvFrontendImage is unset. It is a public placeholder, never a private
	// registry, and every deployment is expected to override it.
	DefaultFrontendImage = "quay.io/opendatahub/vllm-vcr-frontend:v0.33.0-cpu"

	// DefaultBackendImage is the placeholder simulator image used when
	// EnvBackendImage is unset.
	DefaultBackendImage = "quay.io/opendatahub/vllm-vcr-backend:v0.33.0"
)

// FrontendImageFromEnv resolves the frontend image reference from the
// environment, falling back to DefaultFrontendImage.
func FrontendImageFromEnv(getenv func(string) string) string {
	if value := getenv(EnvFrontendImage); value != "" {
		return value
	}
	return DefaultFrontendImage
}

// BackendImageFromEnv resolves the simulator image reference from the
// environment, falling back to DefaultBackendImage.
func BackendImageFromEnv(getenv func(string) string) string {
	if value := getenv(EnvBackendImage); value != "" {
		return value
	}
	return DefaultBackendImage
}

// Options configures the admission webhooks.
type Options struct {
	// Client is the manager's client. It reads the singleton
	// VcrGpuCluster (from the cache, so admission never waits on the API
	// server for it) and writes projected pull Secrets.
	Client client.Client

	// Reader performs direct reads of pull Secrets. Default: Client.
	//
	// The manager's cached client would start a cluster-wide Secret informer and
	// hold every Secret value in memory just to project one copy, so the operator
	// binary should pass mgr.GetAPIReader(). Admission reads Secrets only when an
	// applicable Pod selects a profile that needs them.
	Reader client.Reader

	// OperatorNamespace holds the Secrets referenced by
	// spec.imagePullSecrets and spec.profiles[].pullSecretName.
	OperatorNamespace string

	// FrontendImage is the Python vLLM frontend image. Resolve it with
	// FrontendImageFromEnv so the image stays deploy-time configurable.
	FrontendImage string

	// BackendImage is the vllm-vcr simulator image. Resolve it with
	// BackendImageFromEnv so the image stays deploy-time configurable.
	//
	// The profile provisioner needs no operator image: it runs the selected
	// profile image's own exported entrypoint (docs/contracts/profile-image.md).
	BackendImage string

	// Now is the clock used for the applied-at annotation of a projected pull
	// Secret. Default: time.Now.
	Now func() time.Time

	// Recorder emits a warning event on the VcrGpuCluster when an
	// applicable Pod is denied because of an operator problem. It never carries
	// Secret values. Default: no events.
	Recorder record.EventRecorder
}

// validate rejects settings that cannot be defaulted.
func (o Options) validate() error {
	switch {
	case o.Client == nil:
		return fmt.Errorf("admission webhook: a client is required")
	case o.OperatorNamespace == "":
		return fmt.Errorf("admission webhook: an operator namespace is required: it holds the Secrets projected into workload namespaces")
	case o.FrontendImage == "":
		return fmt.Errorf("admission webhook: a frontend image is required: set %s", EnvFrontendImage)
	case o.BackendImage == "":
		return fmt.Errorf("admission webhook: a backend image is required: set %s", EnvBackendImage)
	}
	return nil
}

// resolved returns opts with the documented defaults applied.
func (o Options) resolved() Options {
	if o.Reader == nil {
		o.Reader = o.Client
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// SetupWithManager registers both admission webhooks on the manager's webhook
// server. config/webhook installs the matching configurations.
func SetupWithManager(mgr ctrl.Manager, opts Options) error {
	mutator, err := NewPodMutator(opts)
	if err != nil {
		return err
	}
	if err := mutator.SetupWithManager(mgr); err != nil {
		return err
	}
	return NewEnvironmentValidator().SetupWithManager(mgr)
}

// SetupWithManager registers the Pod mutator at PodMutatorPath.
func (m *PodMutator) SetupWithManager(mgr ctrl.Manager) error {
	mgr.GetWebhookServer().Register(PodMutatorPath, &crwebhook.Admission{Handler: m})
	return nil
}

// SetupWithManager registers the VcrGpuCluster validator at
// EnvironmentValidatorPath.
func (v *EnvironmentValidator) SetupWithManager(mgr ctrl.Manager) error {
	mgr.GetWebhookServer().Register(EnvironmentValidatorPath, &crwebhook.Admission{Handler: v})
	return nil
}
