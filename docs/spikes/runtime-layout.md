# Spike: runtime layout and lifecycle

The specification requires a spike that *chooses and demonstrates* the
frontend/backend container and supervision layout: profile provisioning,
handshake ordering, readiness, restart behavior, connection recovery or
coordinated restart, failure propagation, and graceful shutdown, with "do not
assume independently restarted processes reconnect" as an explicit warning.

This page is the record. The executable halves are
`test/spike/runtime_layout_test.go` (the harness) and
`test/spike/evidence/runtime-layout.json` (the evidence it writes). The binding
outcome is [`docs/contracts/runtime-layout.md`](../contracts/runtime-layout.md).

## Status in this checkout

**Not demonstrated here.** No vLLM/vllm-vcr image was available, so the harness
skipped (`make test-spike` → `--- SKIP`), and `test/spike/evidence/` contains only
the format example. Everything below describes what the harness establishes when
run with images; nothing below may be quoted as a current result.

```bash
export VCR_FRONTEND_IMAGE=<cpu vllm frontend> VCR_BACKEND_IMAGE=<vllm-vcr> \
       VCR_PROFILE_IMAGE=<profile image>
export VCR_SPIKE_REQUIRED=1            # a skip must fail in a CI job that wants evidence
make test-spike
```

## The layout chosen

One Pod runs one simulated model server, as three processes in two containers plus
an init container:

| Process | Container | Why there |
| --- | --- | --- |
| Profile provisioner | init `vcr-profile-provisioner` | Runs the profile image's `provisioning.entrypoint` into the shared `vcr-profile` volume and exits 0. A failure keeps serving from starting, so an unavailable profile can never produce apparently successful unprofiled responses. |
| Python vLLM frontend | `vllm-frontend` (the workload's own container, renamed) | Tokenization, rendering, generation loop, streaming, metrics, and the HTTP API on the original inference port. Runs with `--data-parallel-size-local 0`, so it waits for an external engine instead of starting one. |
| `vllm-vcr play` | `vllm-vcr-backend` | The simulated engine: connects to the frontend's engine-core handshake, applies recorded timing, emits tokens and KV events. |

`vcr-supervisor` is the frontend container's entrypoint and runs `vllm serve` as
its only long-lived child.

Why the simulator is a **separate container** rather than a second child of the
supervisor:

- its CPU/memory sizing is independent (`spec.runtime.backend`), and so is its
  restart policy and log stream;
- kubelet can restart it without restarting the frontend, which is the only way to
  observe the restart-versus-reconnect question at all;
- the supervisor stays a single-child supervisor, which keeps its failure
  propagation unambiguous (child died ⇒ container failed).

Why loopback rather than a shared socket volume: both containers share the Pod's
network namespace, so the handshake is `tcp://127.0.0.1:29550` and nothing has to
be shared on disk. The harness reproduces this with `--network host`.

## Ordering the harness asserts

1. The provisioner exits 0 and the exported profile directory holds
   `metadata.json` and the trace file the metadata declares, both non-empty.
2. The frontend container starts. The supervisor's `/healthz` answers `200`
   (frontend child alive) while `/readyz` stays non-ready — readiness must not be
   satisfied by "the process started".
3. Readiness stays withdrawn for a full window with no simulator. The harness
   samples the states the contract exposes (the supervisor's `/readyz` and the
   readiness marker file the harness mounts out of the container) every 250 ms for
   5 s and requires them to stay negative.
4. The simulator starts and registers. `AwaitReady` records how long this took.
5. `/v1/models` on the inference port answers `200` only after that.

Readiness is never probed by dialing the handshake port. The frontend binds that
port, so a successful dial says nothing about engine registration — a property
confirmed by the `vllm-vcr` sources: a socket that answers is not an engine that
registered.

## Failure paths the harness asserts

| Path | Induced by | Assertion |
| --- | --- | --- |
| Backend loss | Removing the simulator container abruptly | Readiness is withdrawn; the elapsed time is recorded |
| Backend stays absent | — | The frontend container terminates instead of serving without an engine, and its exit code is non-zero (so kubelet restarts it) |
| Frontend loss | (Observed through the same path: the supervisor exits when it loses the engine) | Recovery is restart-based: the harness restarts both containers and asserts readiness returns |
| Coordinated shutdown | `SIGTERM` to the frontend container | Readiness is withdrawn **before** exit, the frontend exits within the workload's shutdown budget, and the exit code is 0 |

The recovery assertion is deliberately shaped as "restart, then readiness
returns", not "the frontend reconnects". The pinned simulator performs exactly one
handshake attempt per process and exits if its engine loop ends; the prior art
tears the pod down with `wait -n` for the same reason. An independently restarted
process is therefore not assumed to re-establish the session, and the harness
records an observation saying so instead of claiming reconnection.

## Evidence format

`test/spike/evidence/runtime-layout.json` (`vcr.opendatahub-io/spike-evidence/v1`;
types in `test/spike/evidence.go`, example in
`test/spike/evidence/schema.example.json`):

| Field | How to read it |
| --- | --- |
| `images` | Exact frontend/backend/profile references. Cite these digests, not tags |
| `environment` | Container engine, served model, whether the provisioner ran (`profileSource`), the endpoints the checks drove |
| `steps[]` | One entry per assertion above, with `startedAt`/`finishedAt` and, for a failure, the reason in `detail` |
| `observations[]` | Exit codes, time to readiness, recovery mode — facts the checks do not turn into pass/fail |
| `notDemonstrated[]` | What the run did not establish |
| `passed` | True only if every step passed and `notDemonstrated` is empty |

Timings are `startedAt`/`finishedAt` differences. A step that fails is a finding
about the pinned images: the layout must change, or the behavior must become an
explicit rejection. Deleting a failing step is not an option; a layout that cannot
demonstrate readiness or shutdown cannot be claimed.

## Not demonstrated in this environment

| Not demonstrated | Consequence |
| --- | --- |
| Any of the above, currently | The harness skipped; there is no evidence file. The layout is a design decision backed by the contracts, not a demonstrated result |
| GPU hardware behavior | None is expected: the run uses no NVIDIA device. Accelerator behavior is out of scope |
| Timing fidelity | The profile's recorded latency is served unchanged and is never compared with a fresh capture. Functional success is not a performance claim |
| llm-d interaction | No router or discovery component runs in this spike. Cache-local routing is `make test-cluster`'s assertion |
| Reconnection of a restarted peer | Not attempted and not claimed (see above) |
| The supervisor's `/readyz` matching the contract | The supervisor in this tree publishes readiness from child liveness plus a handshake TCP dial, not from the two HTTP inputs the contract names. Consequence: the readiness step fails until the supervisor reads `VCR_FRONTEND_HEALTH_URL` and `VCR_BACKEND_HEALTH_URL`. Recorded as an open gap in [`docs/limitations.md`](../limitations.md) |
| Pod-level readiness (kubelet probes) | This spike drives the container surfaces directly. The webhook's preserved `/v1/models` probe is asserted by `make test-e2e` |

## Unresolved items and their consequences

| Open item | Consequence now | Where it resolves |
| --- | --- | --- |
| Supervisor readiness inputs narrower than the contract | `/readyz` and the readiness marker can report ready for an engine-less frontend; the diagnostic signal is weaker than documented. Pod readiness is unaffected | Supervisor implementation work; the harness already fails on it |
| No reconnection in either process | Every recovery path is a Pod/container restart. A test that restarts one process and expects the other to continue is wrong | Accepted design; the harness records it |
| KV-event replay missing | Router re-synchronization after a lost event window is impossible; recovery acceptance cannot be claimed | The operator **rejects** an explicit `replay_endpoint` unless a profile grants `capabilities.kvEventReplay`, and no reconcilable profile does |
| `--shutdown-timeout 0` (vLLM's default) aborts in-flight requests | A workload that does not set a shutdown timeout gets immediate termination; draining tests must set one | Preserved semantics: the workload's value is applied to both the simulator and the supervisor. Not a gap |
