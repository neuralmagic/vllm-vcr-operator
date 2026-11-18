package vllm

import (
	corev1 "k8s.io/api/core/v1"
)

// Invocation is the workload's vLLM server container together with the Pod
// context needed to resolve what it asked for.
//
// The resolver never talks to the API server: every input is a typed value the
// caller already holds. Content that only exists at runtime (ConfigMap, Secret,
// or downward-API data) is supplied through Files and Env when the caller can
// resolve it; when it cannot, the resolver rejects rather than guessing.
type Invocation struct {
	// Container is the container whose vLLM configuration is resolved.
	Container corev1.Container

	// Volumes is the Pod's volume map, keyed by volume name. It is used to
	// discover how a mounted path (for example a --config file or a chat
	// template) is backed.
	Volumes map[string]corev1.Volume

	// Files maps absolute container paths to file content for volumes whose
	// content the caller could resolve (for example a ConfigMap or Secret the
	// admission webhook read). A path that is mounted but absent from this map
	// is unresolved and causes a rejection when a setting needs it.
	Files map[string][]byte

	// Env maps environment variable names to values for container environment
	// entries that use valueFrom and whose value the caller could resolve
	// (for example a ConfigMap or Secret referenced by the entry). Entries that
	// carry a literal value in the container spec take precedence over this map.
	//
	// Credential values are never retained by the resolver: see AuthSettings.
	Env map[string]string
}
