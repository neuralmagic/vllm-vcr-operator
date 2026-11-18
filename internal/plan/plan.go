// Package plan defines the admission plan: the translation result for one
// workload. internal/translator produces a plan, internal/runtime applies it to
// the submitted Pod, and internal/webhook reports its provenance.
//
// A plan is complete and self-contained. Every value the runtime writes into the
// Pod is already resolved here, so Pod mutation re-derives no policy decision:
// rewriting a Pod must be a mechanical application of the plan. A plan never
// carries credential material. Settings whose value is a credential are either
// preserved as an environment variable on the workload container (the runtime
// forwards the workload's own environment) or copied verbatim from the workload
// container at mutation time (see Invocation.CopiedArguments).
package plan

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// RuntimeLayoutVersion is the version of the injected container layout. It is
// recorded in the injected annotation (docs/contracts/admission.md) so that a
// Pod mutated by an older layout can be recognized.
const RuntimeLayoutVersion = "v1"

// Admission is the translation of one applicable workload.
type Admission struct {
	// Profile is the selected recorded profile, pinned by digest.
	Profile Profile

	// Model is the workload's model identity.
	Model Model

	// GPUDevices is the synthetic device quantity preserved for the workload.
	GPUDevices int32

	// Frontend runs the Python vLLM frontend under the supervisor.
	Frontend Invocation

	// Backend runs the vllm-vcr simulator.
	Backend Invocation

	// Provisioner is the init container that exports the profile data.
	Provisioner Invocation

	// Resources is the real CPU/memory sizing of the injected runtime.
	Resources Resources

	// Placement is the accelerator-free placement applied to the Pod.
	Placement Placement

	// PullSecrets are the Secret names the selected profile image may need. When
	// ProfilePullSecret is set, PullSecrets holds exactly that name; otherwise it
	// is the environment-wide candidate list and only entries whose docker config
	// covers the image's registry host are projected. Values never appear here:
	// only names.
	PullSecrets []string

	// ProfilePullSecret is the profile's explicit spec.profiles[].pullSecretName.
	// It is projected as configured, because the operator selected it for exactly
	// this profile. Empty means the environment-wide candidates apply and are
	// filtered by registry host, so unrelated registry credentials are never
	// copied into a workload namespace.
	ProfilePullSecret string

	// Provenance is recorded as Pod annotations.
	Provenance Provenance

	// Warnings are accepted-but-not-reproduced aspects of the workload. They are
	// observations, not failures: the workload is admitted.
	Warnings []string
}

// Profile identifies the recorded profile pinned for a workload.
type Profile struct {
	// ID is the recorded profile id from its metadata.
	ID string
	// Image is the configured image reference as reconciled.
	Image string
	// Digest is the resolved image digest, for example "sha256:0123...".
	Digest string
}

// PinnedImage returns the digest-pinned image reference. A workload is always
// pinned to the digest resolved at reconciliation, so a later tag move cannot
// change a running simulation.
func (p Profile) PinnedImage() string {
	if p.Digest == "" {
		return p.Image
	}
	return repositoryOf(p.Image) + "@" + p.Digest
}

// repositoryOf strips a tag or digest from an image reference so that a digest
// can be appended. A registry port is not a tag.
func repositoryOf(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	slash := strings.LastIndexByte(ref, '/')
	colon := strings.LastIndexByte(ref, ':')
	if colon > slash {
		ref = ref[:colon]
	}
	return ref
}

// Model is the workload's model identity. Selection uses the underlying model,
// never a served-model alias.
type Model struct {
	// Name is the underlying model identity.
	Name string
	// Revision is the pinned model revision, empty when the workload did not pin
	// one.
	Revision string
	// ServedNames are the API aliases the workload exposes.
	ServedNames []string
}

// Reference returns "name@revision" for provenance annotations.
func (m Model) Reference() string {
	if m.Revision == "" {
		return m.Name
	}
	return m.Name + "@" + m.Revision
}

// Invocation is a process the runtime starts in a container.
type Invocation struct {
	// Image is the container image reference.
	Image string

	// Command is the container command, argv[0] included.
	Command []string

	// Args are appended to Command when the container starts. CopiedArguments
	// are appended after Args, so an argument appended last cannot displace a
	// credential flag that must stay inside the command line the frontend
	// parses.
	Args []string

	// Env is the environment the operator sets. It takes precedence over
	// inherited entries with the same name.
	Env []corev1.EnvVar

	// RemoveEnv names environment variables of the workload container that must
	// not be forwarded to this process (GPU-only configuration).
	RemoveEnv []string

	// Ports are declared container ports.
	Ports []corev1.ContainerPort

	// CopiedArguments are argument fragments the runtime copies verbatim from
	// the workload container instead of the plan storing their values, so that
	// the plan never carries credential material.
	CopiedArguments []CopiedArgument
}

// CopiedArgument names an argument of the workload container that the runtime
// copies verbatim into the command it starts.
type CopiedArgument struct {
	// Setting names the vLLM setting the fragment configures, for example
	// "--api-key". It appears in diagnostics, never the value.
	Setting string

	// Spellings are the flag spellings to search for in the workload's command
	// and args, for example {"--api-key"}.
	Spellings []string

	// TakesValue reports whether the flag consumes the following token when it
	// is not written as "--flag=value".
	TakesValue bool
}

// Resources is the real CPU/memory sizing applied to the injected runtime. It
// replaces the original GPU-server sizing, which describes hardware that is not
// present.
type Resources struct {
	Frontend    corev1.ResourceRequirements
	Backend     corev1.ResourceRequirements
	Provisioner corev1.ResourceRequirements
}

// Placement is the placement rewrite applied to an admitted Pod.
//
// Simulation does not reproduce accelerator topology: the runtime removes the
// constraints that describe GPUs and applies the environment's simulation-node
// selection, while non-accelerator constraints (storage, zone, sidecars) are
// preserved.
type Placement struct {
	// NodeSelector is merged into the workload's node selector. Environment
	// entries win: they name the nodes the simulation runs on.
	NodeSelector map[string]string

	// Tolerations are appended to the workload's surviving tolerations.
	Tolerations []corev1.Toleration

	// AcceleratorNodeLabels are label keys that describe accelerator hardware.
	AcceleratorNodeLabels []string

	// AcceleratorNodeLabelPrefixes are label key prefixes that describe
	// accelerator hardware.
	AcceleratorNodeLabelPrefixes []string

	// AcceleratorNodeValues are label values that name accelerator hardware, for
	// example the simulated GPU type.
	AcceleratorNodeValues []string

	// AcceleratorTolerationKeys are taint keys only accelerator nodes carry.
	AcceleratorTolerationKeys []string

	// RuntimeClasses are container runtime classes that require an accelerator
	// runtime. A Pod using one cannot run on simulation nodes.
	RuntimeClasses []string
}

// Provenance is recorded as Pod annotations (docs/contracts/admission.md).
// Every field is a non-secret identity value.
type Provenance struct {
	ProfileID    string
	ProfileImage string
	Model        string
	VLLMVersion  string
}

// Annotation values are described by docs/contracts/admission.md; the runtime
// maps Provenance onto them together with the preserved device quantity.
//
// String renders a plan summary for diagnostics and tests. It never contains
// credential material because a plan never holds any.
func (a *Admission) String() string {
	if a == nil {
		return "<nil admission>"
	}
	return fmt.Sprintf("profile %s (%s), model %s, frontend %s, backend %s",
		a.Profile.ID, a.Profile.PinnedImage(), a.Model.Reference(),
		strings.Join(a.Frontend.Command, " "), strings.Join(a.Backend.Command, " "))
}
