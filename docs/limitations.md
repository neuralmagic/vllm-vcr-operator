# Limitations

What this operator does not do, what it rejects, and what is unverified in this
checkout. Read this before relying on any behavior.

Two rules shaped this page:

- a capability is claimed only when code or an executable check supports it;
- a gap is stated, never softened into a partial capability. Where an unsupported
  configuration reaches admission, it is **rejected** — the rejection is what
  protects a test from mistaking fallback output for simulated behavior.

## Out of scope (from the specification)

| Not provided | Consequence |
| --- | --- |
| Actual inference, CUDA/GPU-driver emulation, model-quality equivalence | Output is recorded token ids or the simulator's seeded random tokens. Never assert on model content |
| Physical placement or topology reproduction, unmeasured hardware performance | Two synthetic devices are a scheduling quantity, not two engines. Placement does not reproduce GPU topology |
| A general substitute for arbitrary GPU workloads | Only vLLM serve workloads with a supported invocation are admitted. Anything else is rejected |
| Automatic support for every vLLM release | One release per operator build (`0.33.0`, `names.SupportedVLLMVersion`). A profile recorded against another release is rejected |
| Support for unclassified advanced features | A feature absent from a profile's capability set is rejected, not approximated |
| DRA integration | Requests go through the extended resource `nvidia.com/gpu` |
| Distributed worker / P/D execution beyond validated capability sets | Pipeline parallelism, data parallelism, multi-node ranks, and P/D roles are rejected unless a profile grants them — and no reconcilable profile does |
| Profile capture infrastructure | Profiles are produced by a separate repository. This operator consumes OCI images |
| Replacing real llm-d components with simulator-specific routers | llm-d's router, EPP, and discovery run as they are; only the engine is simulated |
| A separate renderer or HTTP dispatcher | Generation and streaming go through the real Python vLLM frontend on the original inference port |

## Known implementation gaps

### KV-event replay does not exist

The simulator publishes live KV-cache events (BlockStored, BlockRemoved,
AllBlocksCleared) over ZMQ, and its ready response always reports
`replay_endpoint: null`. There is no replay responder.

Consequence: a consumer that loses the event window — an EPP restart, a missed
subscription, a dropped connection — cannot re-synchronize its prefix index from
the engine. Only newly stored blocks are seen.

What the operator does about it: an explicit `replay_endpoint` in a workload's
`--kv-events-config` is **rejected unconditionally**, and the rejection names the
pinned simulator release (`names.SupportedSimulatorTag`, `v0.2.5`). The profile
capability check (`capabilities.kvEventReplay`) remains as a second, narrower gate
behind it, so re-pinning a simulator that implements replay relaxes one decision
instead of quietly promising a behavior the pinned build lacks. No reconcilable
profile for this release grants the capability.

Consequence for tests: reconnect/recovery acceptance for prefix-cache routing
cannot be claimed. Live-event routing tests alone do not establish it.

### Per-token logprobs are empty

The simulator produces no per-token logprobs. `--max-logprobs` is preserved for the
frontend, but a request that asks for logprobs observes empty entries. Do not
assert logprob values.

### GPU memory fractions need a validated conversion

`--gpu-memory-utilization` is accepted only for a profile that declares
`coverage.kvMemoryConversion`, and the plan records a warning that the simulated
capacity is the profile's recorded `cache.kvBlocks` rather than a function of the
requested fraction: the metadata records only the resulting block count. A profile
without that declaration rejects the setting.

`--kv-cache-memory-bytes` is **rejected** even with that declaration. This release
has no validated bytes → blocks conversion, so accepting the byte budget would
change nothing while looking as if it had; the profile's recorded `cache.kvBlocks`
is the only simulated capacity. Use `--num-gpu-blocks-override` (within
`coverage.kvBlocks`) to select a capacity explicitly.

### GPU-requesting init containers are not translated

An init container that requests or limits `nvidia.com/gpu` is rejected by name.
The operator replaces the GPU engine with a simulator, and an init container that
asks for a device would either hold the allocation the workload container must own
or run accelerator code where no accelerator exists. Guessing from a container's
name or command that a GPU-requesting setup step is really GPU-free is exactly the
approximation the settings policy refuses. Setup steps that request no device are
preserved unchanged.

### Booleans resolve only in their affirmative spelling

The vLLM resolver accepts the affirmative form of boolean flags, so an explicit
negative such as `--no-enable-prefix-caching` is rejected as an unknown flag
before the settings policy runs. A workload therefore cannot currently ask for the
opposite cache mode; it is rejected rather than served in the wrong mode. See
`docs/settings-policy.md` ("Known gaps").

### Unmatched replay is silent

Both replay modes fall back to the simulator's seeded random tokens for requests
the trace cannot match, and report no error. A content assertion cannot tell a
replay hit from a fallback by looking at the response. `--stats` counters are the
only witness, and they are the simulator's, not the frontend's.

## Admission side effects

Admission writes exactly one kind of resource: the operator-managed
`imagePullSecret` copy in the workload namespace. Everything else it does is a
patch to the Pod being created.

This is a documented reconciliation with the specification's statement that
admission "does not download profile images or create helper resources during
admission", not an exception taken quietly:

- the copy is the only mechanism Kubernetes offers to make an operator-namespace
  pull credential usable by a Pod in another namespace, and the specification
  separately requires operator-managed credential provisioning for workload
  namespaces;
- it is only credentials that can apply: a profile-level `pullSecretName` is
  projected as configured, an environment-wide entry only when its docker config
  covers the selected image's registry host, and nothing at all when no entry
  does (the pull then proceeds anonymously);
- a dry-run request writes nothing;
- admission never pulls or reads a profile image, never contacts a registry,
  never reconciles profile metadata, and never creates or changes a workload
  resource.

`docs/contracts/admission.md` ("Admission side effects") is the binding form of
this boundary.

## Unverified in this checkout

Nothing here has been executed against real vLLM or vllm-vcr images. No image is
published by this repository, and none was available locally, so:

| Unverified | Command that would verify it |
| --- | --- |
| Serving behavior: generation, streaming, tokenizer consistency, metric names and semantics, random versus recorded tokens, unmatched replay, cache pressure, KV-event publication | `make test-spike` (`test/spike/evidence/serving-behavior.json`) |
| Runtime layout: startup ordering, readiness transitions, backend loss, frontend loss, coordinated SIGTERM shutdown | `make test-spike` (`test/spike/evidence/runtime-layout.json`) |
| llm-d prefix-cache routing over a simulated engine (router, EPP, discovery decisions) | No automated evidence in this repository. `make test-cluster` keeps the llm-d labels, ports, ServiceAccount, and model-cache volume on a simulated workload, but no llm-d router is installed there, so routing is not exercised |
| Timing fidelity against a held-out real-GPU capture | Not automated anywhere in this repository. Functional success is not a calibrated performance claim |
| Any behavior of a published profile image | No profile image release exists |

`make test-spike` skips when the images are absent and fails when
`VCR_SPIKE_REQUIRED=1` is set, so a CI job that intends to produce evidence cannot
mistake a skip for a pass. `test/spike/evidence/` contains only the format
example in this checkout.

`make test-e2e` (`test/e2e`, envtest) and `make test-cluster` (`test/cluster`,
kind) are the acceptance targets that exist in this checkout. Both skip — never
pass — when their prerequisites are absent: the cluster suite's skip names the
missing prerequisite and `VCR_CLUSTER_REQUIRED=1` turns it into a failure, while
`make test-e2e` resolves the envtest assets itself so a run through the Makefile
cannot skip for want of them. `docs/operations.md` ("Acceptance evidence") records
what each one proves. Neither replaces `make test-spike`: both use test doubles for
the frontend and the simulator, so neither establishes serving behavior.

## Explicit non-claims

- **Serving fidelity.** The frontend is real vLLM, but no run here proves that
  tokenization, rendering, or streaming match a specific release's behavior.
- **Metric completeness.** The simulator emits no Prometheus metrics. All `vllm:*`
  series are frontend-derived, and the exact set depends on the frontend build.
  A profile's `capabilities.metrics` is the only list that authorizes assertions.
- **Cache-local routing.** The operator provides the prerequisites (live events,
  matching block size, published metrics); it does not demonstrate routing. That is
  llm-d's behavior and `make test-cluster`'s assertion.
- **Capacity as performance.** `devicesPerNode` is a scheduling quantity only.
- **Cleanup completeness.** The operator owns its namespace's resources. Workload
  namespaces, their pull-secret projections, and any objects a test creates are
  the caller's responsibility.
- **Secret handling beyond the contract.** No Secret value appears in annotations,
  status, events, logs, or errors. This is enforced by review and tests, not by a
  runtime guard; a new code path that formats a Secret must keep it true.
