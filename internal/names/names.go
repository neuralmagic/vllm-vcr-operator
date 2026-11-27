// Package names holds identifiers shared by the operator, the admission
// webhook, the device plugin, the simulator, and the generated manifests.
//
// These strings are part of the operator's external contract: llm-d
// configuration, profile images, and test manifests depend on them. Treat
// changes as compatibility changes.
package names

const (
	// OperatorName is the operator's workload name, used for names and labels.
	OperatorName = "vllm-vcr-operator"

	// APIGroup is the operator's API group.
	APIGroup = "vcr.opendatahub-io"

	// SyntheticResourceName is the extended resource advertised by the
	// operator-managed device plugin. It intentionally reuses the NVIDIA GPU
	// resource name so unmodified workloads request it.
	SyntheticResourceName = "nvidia.com/gpu"

	// DevicePluginName is the DaemonSet and container name of the synthetic
	// device plugin.
	DevicePluginName = "vllm-vcr-device-plugin"

	// DevicePluginServiceAccount is the dedicated ServiceAccount the device
	// plugin runs as. A dedicated account exists so a platform can grant it the
	// narrowest permission that permits the kubelet socket hostPath (on
	// OpenShift, hostmount-anyuid) without granting it to every Pod in the
	// operator namespace.
	DevicePluginServiceAccount = "vllm-vcr-device-plugin"

	// DevicePluginSocketName is the unix socket the plugin registers on with
	// kubelet, relative to the device-plugin directory.
	DevicePluginSocketName = "vllm-vcr-gpu.sock"

	// DevicePluginDir is the host directory shared with kubelet for device
	// plugin sockets and registration.
	DevicePluginDir = "/var/lib/kubelet/device-plugins"

	// FrontendContainerName is the injected Python vLLM frontend container.
	FrontendContainerName = "vllm-frontend"

	// BackendContainerName is the injected vllm-vcr simulator container.
	BackendContainerName = "vllm-vcr-backend"

	// ProvisionerContainerName is the init container that exports profile data
	// into the shared profile volume.
	ProvisionerContainerName = "vcr-profile-provisioner"

	// ProfileVolumeName is the shared emptyDir holding exported profile data.
	ProfileVolumeName = "vcr-profile"

	// ProfileMountPath is where the profile volume is mounted in the simulator
	// and provisioning containers.
	ProfileMountPath = "/var/run/vcr/profile"

	// ProfileMetadataFileName is the metadata document inside the profile image
	// and the exported profile volume.
	ProfileMetadataFileName = "metadata.json"

	// HandshakePort is the engine-core ZMQ handshake port the frontend binds and
	// the simulator connects to.
	HandshakePort = 29550

	// ControlPort is the simulator's runtime control API port.
	ControlPort = 8001

	// KVEventsPort is the KV-cache event PUB port used by llm-d discovery.
	KVEventsPort = 5556

	// DefaultInferencePort is the vLLM serving port used when the workload does
	// not specify one.
	DefaultInferencePort = 8000

	// AnnotationPrefix is prepended to every operator annotation.
	AnnotationPrefix = APIGroup + "/"

	// InjectedAnnotation records the runtime and profile identity applied at
	// admission. It never contains credentials.
	InjectedAnnotation = AnnotationPrefix + "injected"

	// ProfileAnnotation records the pinned profile id: the image is pinned
	// separately, in ProfileImageAnnotation.
	ProfileAnnotation = AnnotationPrefix + "profile"

	// ProfileImageAnnotation records the digest-pinned profile image.
	ProfileImageAnnotation = AnnotationPrefix + "profile-image"

	// ModelAnnotation records the underlying model identity used for selection.
	ModelAnnotation = AnnotationPrefix + "model"

	// VLLMVersionAnnotation records the vLLM release the translation targets.
	VLLMVersionAnnotation = AnnotationPrefix + "vllm-version"

	// OriginalGPUsAnnotation records the preserved GPU quantity.
	OriginalGPUsAnnotation = AnnotationPrefix + "original-gpus"

	// LabelPrefix is prepended to operator labels.
	LabelPrefix = APIGroup + "/"

	// ManagedByLabel marks operator-managed resources.
	ManagedByLabel = LabelPrefix + "managed-by"

	// EnvironmentLabel records which VcrGpuCluster produced a resource.
	EnvironmentLabel = LabelPrefix + "environment"

	// ProfileCatalogSchemaVersion is the profile metadata schema version the
	// operator accepts.
	ProfileCatalogSchemaVersion = "vcr.opendatahub-io/profile/v1"

	// ProfileMetadataLabel is the OCI image config label carrying profile
	// metadata so that reconciliation never downloads trace layers.
	ProfileMetadataLabel = "vcr.opendatahub-io/profile-metadata"

	// PullSecretPrefix prefixes projected pull-secret copies in workload
	// namespaces.
	PullSecretPrefix = "vcr-profile-pull-"

	// SupervisorBinary is the lifecycle supervisor entrypoint inside the
	// frontend container.
	SupervisorBinary = "/usr/local/bin/vcr-supervisor"

	// SupportedVLLMVersion is the vLLM release this operator build translates
	// against. Fixed per operator release.
	SupportedVLLMVersion = "0.33.0"

	// SupportedSimulatorTag is the release tag of the pinned vllm-vcr simulator
	// build this operator emits `play` arguments for. It is recorded in
	// hack/vllm-vcr-pin.env (VLLM_VCR_PIN_TAG) and enforced by
	// hack/check-vllm-vcr-upstream.sh plus a unit test, so the two cannot drift.
	//
	// Rejections that exist only because the pinned build lacks a feature name
	// this tag: re-pinning the simulator is then the single place that relaxes
	// them.
	SupportedSimulatorTag = "v0.2.5"
)
