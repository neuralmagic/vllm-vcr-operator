package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VcrGpuProfile pins the simulated GPUs in one namespace to one recorded profile.
//
// It exists so that the choice of recording is a Kubernetes object a workload owner
// controls, instead of an operator-wide setting an administrator has to change. A user
// names a profile id; the cluster's catalog decides which image and digest that id
// resolves to. Users therefore choose among recordings an administrator registered and
// can never introduce an image of their own.
//
// Exactly one VcrGpuProfile may exist per namespace. Admission reads it to pin every
// qualifying workload in the namespace to the same recording, so a second one would make
// the choice depend on listing order.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,path=vcrgpuprofiles,singular=vcrgpuprofile,shortName=vgp
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.spec.profile`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.status.resolved.model`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type VcrGpuProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VcrGpuProfileSpec   `json:"spec"`
	Status VcrGpuProfileStatus `json:"status,omitempty"`
}

// VcrGpuProfileSpec selects a recording by identity.
type VcrGpuProfileSpec struct {
	// Profile is the id of a recorded profile offered by the VcrGpuCluster's catalog,
	// for example "qwen3-0.6b-h200-tp1-bf16-poisson".
	//
	// It names a recording, never an image reference: images are registered by an
	// administrator in the cluster configuration, and a selection can only choose among
	// them. An id the catalog does not offer is reported on the resource rather than
	// accepted silently.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]*$`
	Profile string `json:"profile"`
}

// VcrGpuProfileStatus reports what the selection currently resolves to.
//
// The resolved image is digest-pinned: a selection names an identity, and every Pod it
// affects runs the recording that identity resolved to at reconciliation time.
type VcrGpuProfileStatus struct {
	// ObservedGeneration is the spec generation last reconciled.
	//
	// +kubebuilder:validation:Optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Resolved is the catalog entry the profile id currently pins. It is empty while the
	// id is not offered by the catalog.
	//
	// +kubebuilder:validation:Optional
	Resolved *VcrGpuProfileResolution `json:"resolved,omitempty"`

	// Conditions describe the selection. Ready is true only when the id resolves to a
	// usable catalog entry whose requirements admission can match.
	//
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VcrGpuProfileResolution is the catalog entry a selection pins.
type VcrGpuProfileResolution struct {
	// ID is the profile identity from the catalog.
	ID string `json:"id"`

	// Image is the digest-pinned profile image reference.
	Image string `json:"image"`

	// Digest is the immutable manifest digest.
	Digest string `json:"digest,omitempty"`

	// Model is the model the recording serves, so that a reader can tell what the id
	// means without consulting the catalog.
	Model string `json:"model,omitempty"`

	// GPUType and GPUCount are the recorded topology the profile can serve.
	GPUType  string `json:"gpuType,omitempty"`
	GPUCount int32  `json:"gpuCount,omitempty"`

	// VLLMVersion is the release the profile was recorded against. Admission only
	// accepts the operator's pinned release, so a mismatch here explains a rejection.
	VLLMVersion string `json:"vllmVersion,omitempty"`
}

// VcrGpuProfileList contains a list of VcrGpuProfile.
//
// +kubebuilder:object:root=true
type VcrGpuProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VcrGpuProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VcrGpuProfile{}, &VcrGpuProfileList{})
}
