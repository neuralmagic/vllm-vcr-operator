package webhook

// Webhook paths. They are part of the installation contract: the
// MutatingWebhookConfiguration and ValidatingWebhookConfiguration in
// config/webhook must register exactly these paths.
const (
	// PodMutatorPath is the mutating admission path for Pods.
	PodMutatorPath = "/mutate-v1-pod"

	// EnvironmentValidatorPath is the validating admission path for
	// VcrGpuClusters.
	EnvironmentValidatorPath = "/validate-vcr-opendatahub-io-v1alpha1-vcrgpucluster"
)
