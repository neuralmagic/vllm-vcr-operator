package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SimulationEnvironmentNameDefault is the only accepted name for a
// SimulationEnvironment. A single cluster-scoped configuration keeps admission,
// device advertisement, and profile selection consistent.
const SimulationEnvironmentNameDefault = "default"

// SimulationEnvironment configures how the operator simulates GPU-backed vLLM
// model servers on CPU-only nodes.
//
// A SimulationEnvironment is cluster scoped and its name must be "default". The
// name restriction enforces the singleton policy through API validation rather
// than through operator conventions.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,path=simulationenvironments,singular=simulationenvironment,shortName=sime
// +kubebuilder:printcolumn:name="GPU",type=string,JSONPath=`.spec.gpuType`
// +kubebuilder:printcolumn:name="Devices",type=integer,JSONPath=`.spec.devicesPerNode`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="only the name 'default' is accepted: a SimulationEnvironment is a singleton cluster configuration"
type SimulationEnvironment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SimulationEnvironmentSpec   `json:"spec,omitempty"`
	Status SimulationEnvironmentStatus `json:"status,omitempty"`
}

// SimulationEnvironmentSpec selects simulation nodes, synthetic GPU capacity,
// the simulated accelerator, available recorded profiles, and the real
// resources given to the simulated serving runtime.
type SimulationEnvironmentSpec struct {
	// SimulationNodes selects the nodes on which the operator advertises
	// synthetic GPUs. Admission rewrites GPU-specific placement so that
	// simulated Pods become eligible for these nodes.
	NodeSelection NodeSelection `json:"simulationNodes"`

	// DevicesPerNode is the number of synthetic devices advertised per
	// simulation node. It is a scheduling quantity, not a performance
	// multiplier: a Pod requesting two devices consumes two and selects a
	// profile whose recorded topology matches.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	DevicesPerNode int32 `json:"devicesPerNode"`

	// GPUType is the accelerator type being simulated, for example "H200". It
	// participates in profile selection so that recorded timing is not inferred
	// from device count alone.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]*$`
	GPUType string `json:"gpuType"`

	// Profiles lists the recorded profile OCI images available to admission.
	// Metadata is reconciled outside admission from these images; a Pod is
	// always pinned to the digest resolved at reconciliation time.
	//
	// +kubebuilder:validation:MaxItems=64
	Profiles []ProfileSource `json:"profiles,omitempty"`

	// ImagePullSecrets names Secrets in the operator namespace used to pull
	// profile images. Only the credentials required by a selected profile are
	// projected into the workload namespace.
	//
	// +kubebuilder:validation:MaxItems=16
	ImagePullSecrets []LocalSecretReference `json:"imagePullSecrets,omitempty"`

	// Runtime configures the real CPU and memory sizing applied to the
	// simulated serving runtime. Original GPU-server sizing is replaced because
	// it describes hardware that is not present.
	Runtime RuntimeSizing `json:"runtime"`

	// LogLevel sets the simulator log filter forwarded to the backend.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:default="info"
	LogLevel string `json:"logLevel,omitempty"`
}

// NodeSelection selects the nodes eligible for synthetic GPU advertisement.
type NodeSelection struct {
	// NodeSelector must match for a node to be eligible for simulation.
	//
	// +kubebuilder:validation:MinProperties=1
	NodeSelector map[string]string `json:"nodeSelector"`

	// Tolerations are applied to the device-plugin DaemonSet so that eligible
	// tainted nodes can run it.
	//
	// +kubebuilder:validation:Optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// ProfileSource references one recorded profile OCI image.
type ProfileSource struct {
	// Image is an OCI image reference. A digest reference pins the artifact
	// explicitly; a tag reference is resolved and pinned during reconciliation.
	//
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// PullSecretName optionally overrides the environment-wide pull secrets for
	// this profile image. It names a Secret in the operator namespace.
	//
	// +kubebuilder:validation:Optional
	PullSecretName string `json:"pullSecretName,omitempty"`
}

// LocalSecretReference names a Secret in the operator namespace.
type LocalSecretReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// RuntimeSizing holds the container resource sizing for the simulated runtime.
type RuntimeSizing struct {
	// Frontend sizes the Python vLLM frontend container that serves the
	// original inference port.
	Frontend corev1.ResourceRequirements `json:"frontend"`

	// Backend sizes the vllm-vcr simulator container.
	Backend corev1.ResourceRequirements `json:"backend"`

	// Provisioner sizes the profile provisioning init container.
	//
	// +kubebuilder:validation:Optional
	Provisioner *corev1.ResourceRequirements `json:"provisioner,omitempty"`
}

// SimulationEnvironmentStatus reports reconciliation results. Profile metadata
// is published here so that admission never contacts a registry.
type SimulationEnvironmentStatus struct {
	// ObservedGeneration is the spec generation last reconciled.
	//
	// +kubebuilder:validation:Optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions describe the environment's readiness. Ready is true only when
	// every configured profile has usable metadata and the device plugin
	// DaemonSet has been reconciled.
	//
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Profiles is the reconciled profile catalog used by admission.
	//
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=image
	Profiles []ResolvedProfile `json:"profiles,omitempty"`

	// SupportedVLLMVersion is the vLLM release this operator build translates
	// against. It is fixed per operator release, not resolved at runtime.
	//
	// +kubebuilder:validation:Optional
	SupportedVLLMVersion string `json:"supportedVLLMVersion,omitempty"`
}

// ResolvedProfile is one reconciled profile artifact.
type ResolvedProfile struct {
	// Image is the configured image reference.
	Image string `json:"image"`

	// Digest is the resolved immutable digest, for example
	// "sha256:0123...".
	Digest string `json:"digest,omitempty"`

	// Metadata is the profile metadata document validated at reconciliation.
	// It is JSON so that admission can evaluate compatibility without pulling.
	//
	// +kubebuilder:validation:Optional
	Metadata string `json:"metadata,omitempty"`

	// ReconciledAt records when the metadata was last verified.
	//
	// +kubebuilder:validation:Optional
	ReconciledAt metav1.Time `json:"reconciledAt,omitempty"`

	// Message explains why this profile is not usable, when applicable.
	//
	// +kubebuilder:validation:Optional
	Message string `json:"message,omitempty"`
}

// SimulationEnvironmentList contains a list of SimulationEnvironment.
//
// +kubebuilder:object:root=true
type SimulationEnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SimulationEnvironment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SimulationEnvironment{}, &SimulationEnvironmentList{})
}
