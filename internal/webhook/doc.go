// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=vcr.opendatahub-io,resources=simulationenvironments,verbs=get;list;watch
// +kubebuilder:rbac:groups=vcr.opendatahub-io,resources=simulationenvironments/status,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Package webhook implements the operator's admission webhooks and pull-secret
// projection (docs/contracts/admission.md).
//
// Two registrations are served; config/webhook/ installs them and the paths below
// are the contract between this package and those manifests:
//
//	PodMutatorPath           "/mutate-v1-pod" (mutating, Pods, CREATE)
//	EnvironmentValidatorPath  "/validate-vcr-opendatahub-io-v1alpha1-simulationenvironment"
//	                          (validating, SimulationEnvironments, CREATE/UPDATE)
//
// Use SetupWithManager (or the two per-handler SetupWithManager methods) to
// register them on a manager's webhook server.
//
// # Pod mutator
//
// A Pod is applicable when any container or init container requests or limits
// names.SyntheticResourceName. Applicable Pods are translated by
// internal/translator and rewritten by internal/runtime; they are never admitted
// against synthetic devices in their original form (fail closed). Pods without
// the request, and Pods this operator already injected, are returned unchanged
// with an empty patch.
//
// Admission never re-derives a policy decision: it resolves the singleton
// SimulationEnvironment, builds the reconciled profile catalog from its published
// status, resolves the workload's own invocation with internal/vllm, and reports
// what internal/translator decided. A structured translator/vllm rejection is a
// workload problem and the denial names the setting and reason; a failure to read
// or understand operator configuration is reported as an operator problem so the
// two never look alike. Neither message contains credential material.
//
// A configuration file named by --config that is only described by a ConfigMap or
// Secret volume is unresolved and therefore rejected (docs/contracts/admission.md,
// which lists an unresolved configuration file as a rejection): admission does not
// read workload configuration data.
//
// # Pull-secret projection
//
// The profile's images may need credentials. Admission copies the docker config
// payload merged from the operator-namespace Secrets referenced by the selected
// profile into an operator-managed Secret in the workload namespace, named
// names.PullSecretPrefix plus a digest of the contributing Secret names, and
// references that copy from the mutated Pod alongside the workload's own
// imagePullSecrets. Only credentials that can apply are copied: a profile-level
// spec.profiles[].pullSecretName is projected as configured, while an
// environment-wide spec.imagePullSecrets entry contributes only when its docker
// config covers the selected image's registry host. When no candidate does, nothing
// is projected and the pull proceeds anonymously. Naming the copy after the
// contributing sources (not its content) means a credential rotation rewrites one
// copy instead of accumulating copies every admitted Pod still points at, and two
// profiles served from different registries cannot share one copy. The copy carries
// labels from internal/names and an applied-to/applied-at annotation pair instead of
// an owner reference, because nothing namespace-local owns it. An existing copy whose
// payload is unchanged is reused without a write. The payload never appears in a
// response, annotation, event, error, or log line; only Secret names and registry
// hosts do.
//
// Projection is the only side effect of admission and it is skipped for dry-run
// requests. Sources are still read and merged on dry-run, so the decision matches a
// real request, and the response is identical because the projected name is derived
// from the sources rather than from a create response.
//
// # Environment validator
//
// The CRD already enforces the singleton name through a CEL rule; the validator
// is defense in depth plus the transition rules CEL cannot express. It rejects
//   - an object whose name is not "default";
//   - an update that both removes a profile and changes gpuType or devicesPerNode,
//     because the resulting catalog transition is ambiguous (admitted Pods pinned
//     a profile the new configuration no longer explains);
//   - a devicesPerNode decrease below the currently configured capacity unless the
//     update carries AllowCapacityReductionAnnotation: "true", because advertised
//     capacity is lowered only deliberately.
//
// Everything else is mutable by design, and deletion is always allowed: a
// configuration the operator must be able to replace must never be blocked.
package webhook
