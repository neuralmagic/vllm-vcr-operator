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
`--kv-events-config` is **rejected** unless the selected profile's
`capabilities.kvEventReplay` is true, and even then the plan records that
re-synchronization cannot happen. Reconciliable profiles for this release declare
`false`.

Consequence for tests: reconnect/recovery acceptance for prefix-cache routing
cannot be claimed. Live-event routing tests alone do not establish it.

### The supervisor's readiness inputs are narrower than the contract

`docs/contracts/runtime-layout.md` defines readiness as three inputs: the frontend
child is alive, the frontend's own HTTP health endpoint answers `200`, and the
simulator's control API answers. The supervisor in this tree publishes readiness
from the first input plus a **TCP dial of the handshake port**.

The handshake port is bound by the frontend, so a successful dial is not proof
that an engine registered. Consequence: `/readyz` and the readiness marker
(`/tmp/vcr-runtime/ready`) can report ready for an engine-less frontend. Pod
readiness itself is unaffected — the workload's own `/v1/models` probe is
preserved and does not pass before engine registration — but the diagnostic signal
is weaker than documented.

The runtime-layout spike asserts the contract, so a `make test-spike` run with
images reports this as a failing step (`readiness stays withdrawn while the
simulator is absent`) until the supervisor reads `VCR_FRONTEND_HEALTH_URL` and
`VCR_BACKEND_HEALTH_URL`.

### Per-token logprobs are empty

The simulator produces no per-token logprobs. `--max-logprobs` is preserved for the
frontend, but a request that asks for logprobs observes empty entries. Do not
assert logprob values.

### GPU memory fractions need a validated conversion

`--gpu-memory-utilization` and `--kv-cache-memory-bytes` are accepted only for a
profile that declares `coverage.kvMemoryConversion`. Even then, the metadata
records only the resulting `cache.kvBlocks`, so the simulated capacity is the
recorded value, not a function of the requested number. A profile without that
declaration rejects both settings: capacity is never inferred from a percentage.

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

## Unverified in this checkout

Nothing here has been executed against real vLLM or vllm-vcr images. No image is
published by this repository, and none was available locally, so:

| Unverified | Command that would verify it |
| --- | --- |
| Serving behavior: generation, streaming, tokenizer consistency, metric names and semantics, random versus recorded tokens, unmatched replay, cache pressure, KV-event publication | `make test-spike` (`test/spike/evidence/serving-behavior.json`) |
| Runtime layout: startup ordering, readiness transitions, backend loss, frontend loss, coordinated SIGTERM shutdown | `make test-spike` (`test/spike/evidence/runtime-layout.json`) |
| Admission end to end against a cluster-shaped API server: mutation, provenance annotations, idempotency, rejection | `make test-e2e` |
| A GPU-free cluster scheduling an unchanged GPU workload and serving it with a real llm-d router | `make test-cluster` |
| Timing fidelity against a held-out real-GPU capture | Not automated anywhere in this repository. Functional success is not a calibrated performance claim |
| Any behavior of a published profile image | No profile image release exists |

`make test-spike` skips when the images are absent and fails when
`VCR_SPIKE_REQUIRED=1` is set, so a CI job that intends to produce evidence cannot
mistake a skip for a pass. `test/spike/evidence/` contains only the format
example in this checkout.

`make test-e2e` and `make test-cluster` are the acceptance targets of `test/e2e`
and `test/cluster`. A checkout that does not contain those suites yet has no such
target: the commands name the evidence, and the trees named are the ones that
produce it.

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
