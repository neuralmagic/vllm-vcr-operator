# Admission contract

Status: binding contract between `internal/translator`, `internal/runtime`,
`internal/webhook`, and the acceptance tests.

## Applicability

A Pod is *applicable* when any container or init container requests or limits
`nvidia.com/gpu`. Applicable Pods are either fully translated or rejected; they
are never admitted against synthetic devices in their original form.

Pods without GPU requests are returned unchanged. So are Pods already injected by
this operator (idempotency): the injected annotation is present and the injected
containers are present.

## Translation

`internal/translator` resolves the workload's effective vLLM configuration for
the operator's fixed vLLM release, applies the settings policy, selects exactly
one profile from the reconciled catalog, and produces an admission plan.

Rejection is the default outcome for unresolved or unsupported configuration.
Rejection messages name the offending setting and the reason, and never include
credential values.

## Mutation

For an applicable Pod the operator:

1. Keeps `resources.requests/limits[nvidia.com/gpu]` on the original requesting
   container, so allocation and capacity-exhaustion behavior stay testable.
2. Replaces the original vLLM container's entrypoint with the Python vLLM
   frontend, run under `vcr-supervisor` so that frontend and simulator share a
   lifecycle.
3. Adds the `vllm-vcr` simulator container, the profile provisioner init
   container, and the shared profile volume.
4. Pins the selected profile image by digest and adds the projected pull secret
   that carries a credential for that image's registry. An environment-wide
   `spec.imagePullSecrets` list is a candidate list: entries that cannot apply to
   the selected image are not projected (see below).
5. Rewrites accelerator-specific placement: removes `nvidia.com/gpu` node
   selectors, GPU topology affinity, and NVIDIA runtime classes; applies the
   environment's simulation node selection.
6. Applies the environment's CPU/memory sizing to the injected containers.
7. Preserves model identity, served-model aliases, inference port names and
   numbers, llm-d labels, related credential mounts and volumes, and unrelated
   sidecars.
8. Records provenance annotations: selected profile id and digest image,
   resolved model identity, GPU quantity, and the operator's vLLM version.

Injected containers do not request `nvidia.com/gpu`. Exactly one container in the
mutated Pod requests the original quantity.

## Provenance annotations

| Annotation | Value |
| --- | --- |
| `vcr.opendatahub-io/injected` | Runtime layout version, e.g. `v1` |
| `vcr.opendatahub-io/profile` | Profile id from metadata |
| `vcr.opendatahub-io/profile-image` | Digest-pinned image reference |
| `vcr.opendatahub-io/model` | Underlying model name and revision |
| `vcr.opendatahub-io/original-gpus` | GPU quantity preserved for the workload |
| `vcr.opendatahub-io/vllm-version` | Translation target, e.g. `0.33.0` |

No annotation, status field, log line, or event may contain a Secret value.

## Admission side effects

Admission is a translation, not a provisioning step. It reads the reconciled
profile catalog from `SimulationEnvironment` status, resolves the workload, and
returns a patch.

The specification says admission "does not download profile images or create
helper resources during admission", while this contract requires a per-namespace
copy of a pull credential. Both are reconciled here rather than left to drift:

- **Kept: the projected pull Secret.** `spec.imagePullSecrets` and a profile's
  `pullSecretName` name Secrets in the operator namespace, and a Pod can only
  reference Secrets in its own namespace. A per-namespace copy is therefore the
  only mechanism Kubernetes offers for the specification's requirement that
  "only the necessary pull credentials are made available to workload namespaces
  through operator-managed resources". It is the one resource admission creates.
- **Only on a real request.** A dry-run request evaluates the same projection and
  returns the same patch, but writes nothing.
- **Only credentials that can apply.** A profile-level `pullSecretName` is
  projected as configured, because the operator selected it for that profile. An
  environment-wide list is filtered: an entry is projected only when its docker
  config carries a credential for the selected image's registry host. When no
  candidate does, nothing is projected and the pull proceeds anonymously, rather
  than spreading unrelated registry credentials into the namespace.
- **Nothing else.** Admission never downloads or reads a profile image, never
  contacts a registry, never reconciles profile metadata, and never creates,
  updates, or deletes a workload resource (Pod, Deployment, Service, ServiceAccount,
  ConfigMap, or any other helper object). The submitted controller manifest is
  untouched: only the Pod the kubelet is about to create is patched.

`docs/limitations.md` states the same boundary from the operator's point of view.

## Failure behavior

- No reconciled catalog, no matching profile, ambiguous matches, or incompatible
  coverage: reject.
- Unsupported invocation form (shell wrapper, unknown entrypoint, unresolved
  configuration file, unknown flag): reject.
- Unsupported advanced feature (pipeline parallelism, distributed workers, P/D,
  LoRA, speculative decoding, multimodal, embeddings, structured outputs):
  reject unless the selected profile's capability set grants it.
- An init container that requests or limits `nvidia.com/gpu`: reject. A
  GPU-dependent setup step cannot be translated safely as a whole, so this
  release refuses every one of them by name instead of guessing from a container
  name whether the step is really GPU-free. Init containers that request no
  device are preserved unchanged.
- Dry-run requests receive the same response as real requests, but a dry-run
  request writes nothing (no pull-secret copy is created).
- The only resource a real admission creates is the operator-managed
  `imagePullSecret` copy projected into the workload namespace ("Admission side
  effects" above). The webhook never downloads profile images, never reconciles
  profile metadata, and never mutates the submitted controller manifest.

## Runtime layout

The injected layout is documented in `docs/contracts/runtime-layout.md`.
