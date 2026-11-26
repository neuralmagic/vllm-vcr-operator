# Architecture — what was built

The operator lets GPU-targeting vLLM workloads run on CPU-only clusters without
changing their manifests. It intercepts their Pods, swaps the GPU engine for a
CPU vLLM frontend plus a recorded simulator, and fakes the GPU capacity the
scheduler needs. llm-d components still talk to a real frontend over a real
network API; only inference execution is simulated.

## Pieces

| Part | Binary | What it does |
| --- | --- | --- |
| Manager | `cmd/manager` | Admission webhooks, configuration/profile reconciliation, device-plugin DaemonSet |
| Device plugin | `cmd/deviceplugin` | Registers with kubelet, advertises synthetic `nvidia.com/gpu` |
| Supervisor | `cmd/supervisor` | The frontend container's entrypoint: runs the Python frontend, owns readiness, kills it when the simulator is gone |
| Profile exporter | `cmd/profileexport` | Shipped inside profile images; copies metadata + trace into the Pod volume |
| Translator/runtime (library) | `internal/translator`, `internal/runtime` | Turns a workload into settings, a plan, and a mutated Pod |

Configuration is one cluster-scoped CR, `VcrGpuCluster`, name `default`
only (enforced by a CRD rule, not convention). Nothing else is a CR: profile
metadata lives in the CR's status, not in a `Profile` resource.

## Path of a Pod

```
user applies Deployment (unchanged, nvidia.com/gpu: 1)
        │
        ▼
API server → mutating webhook (only GPU Pods, only labelled namespaces)
        │  1. resolve vLLM invocation  → internal/vllm   (0.33.0 semantics)
        │  2. apply settings policy    → internal/translator
        │  3. select exactly one profile from status.profiles
        │  4. build the plan, mutate the Pod → internal/runtime
        │  5. copy the pull Secret the profile needs into the namespace
        ▼
Pod: vllm-frontend (supervisor + real vLLM, keeps the GPU request)
     vllm-vcr-backend (simulator, no GPU request)
     init: vcr-profile-provisioner (runs the profile image's exporter)
        │
        ▼
scheduler places it using synthetic capacity → kubelet starts it
        │
        ├─ provisioner exports metadata + trace into an emptyDir
        ├─ frontend starts, binds the engine-core handshake (:29550)
        ├─ supervisor probes frontend /health and simulator /stats
        ├─ simulator connects, registers as the engine
        └─ frontend starts serving on the workload's original port
```

Rejection is the default for anything not understood: unknown flags, unresolved
config files, shell wrappers, unsupported features, no/ambiguous profile. The
message names the setting and says what to change.

## The two decisions that shape everything

**1. Settings are interpreted, not passed through.** The workload's vLLM config is
resolved the way vLLM 0.33.0 would resolve it (args > config file > env >
defaults), and each setting has a policy row: preserve, translate, require a
matching profile, or reject. E.g. `--max-num-seqs` becomes the simulator's
admission limit inside the profile's declared range; LoRA, speculative decoding,
P/D, multimodal, embeddings and KV replay are rejected outright because nothing
has demonstrated them end to end.

**2. Recorded timing comes from profile images, not guesses.** A profile image
carries `metadata.json` (model, GPU type, topology, cache shape, coverage ranges,
capabilities) and a trace. The operator reconciles metadata into status, selects
by model/revision + GPU + topology + dtype + block size, pins the digest on each
Pod, and the init container exports the trace. No match, no admission.

## Pod layout and lifecycle

- **Frontend container** keeps the workload's name-independent bits: original
  port, probes, mounts, labels, service account, and *all* of its GPU allocation.
  Its image becomes the operator's CPU frontend image, its command becomes
  `vcr-supervisor`.
- **Simulator container** runs `vllm-vcr play` with flags derived from the plan
  (handshake address, model, scheduler caps, cache size, KV events, trace). It
  requests no GPU.
- **Provisioner init container** runs the pinned profile image's own entrypoint;
  serving never starts without it.
- **Supervisor** owns readiness: frontend alive *and* frontend `/health` 2xx *and*
  simulator `/stats` 2xx. The handshake socket is deliberately not a readiness
  signal — the frontend binds it, so a dial proves nothing.
- **Cold start** is the hard part: a real frontend needs ~40s to import torch and
  fetch tokenizer files, while `vllm-vcr` gives up dialing after 30s. The
  simulator runs under a POSIX-shell restart loop, so it retries the process in
  place every 2s instead of paying kubelet's restart backoff, and the supervisor
  keeps the frontend alive for a startup budget (5m) instead of treating "never
  healthy" as "lost".
- The trade-off is explicit: a simulator crash that recovers within the 5s loss
  grace costs no restart and leaves the container restart count at 0; an outage
  longer than the grace terminates the frontend, and the Pod restarts as a unit.
  Recovery is restart-based, not reconnect-based — the wrapper runs a fresh
  simulator process, not a resumed session.

## Synthetic GPUs

The operator renders a DaemonSet that runs the device plugin on the nodes selected
by `spec.simulationNodes`. The plugin registers `vllm-vcr-gpu.sock` with kubelet,
advertises `vllm-vcr-gpu-<i>` × `spec.devicesPerNode`, and never touches a device
file. Kubelet owns allocation; the plugin only reports capacity. Count changes
arrive through a ConfigMap the plugin polls, so no image rebuild is needed. Nodes
that already advertise `nvidia.com/gpu` from a real provider are excluded by
affinity.

## Placement and resources

Admission strips accelerator-only constraints: `nvidia.com/gpu` node selectors,
GPU topology affinity, `runtimeClassName: nvidia`, GPU tolerations, and driver
volume mounts. It applies the environment's node selection and its CPU/memory
sizing to the injected containers, and keeps non-accelerator constraints (zone,
storage) — reporting a combination it cannot satisfy instead of silently dropping
it.

## KV events and llm-d

If the workload enables KV events, the simulator publishes them over ZMQ on 5556.
The topic is `kv@$(POD_IP):<port>@<model>` and the engine id is `$(POD_NAME)`,
both expanded by kubelet, because a controller-created Pod has neither a name nor
an IP when admission runs, and llm-d attributes events by Pod IP and port. The
runtime injects those downward-API variables so the placeholders actually expand.

## Operating behaviour

- **Every decision is visible.** Accepted translations and refusals are logged and
  fired as events (`SimulationInjected`, `SimulationRejected`) on the Pod, or on
  its namespace when a controller-created Pod has no name yet. Events are queued
  and dropped under pressure rather than blocking admission.
- **Admission survives replica loss.** Two manager replicas run; controllers act
  only on the leader, but every replica serves admission, and a PDB keeps one
  up during drains. A replica reports Ready only once its webhook server serves.
- **Projected pull secrets are reconciled, not just created.** A sweep refreshes
  copies when a source Secret rotates and deletes copies no Pod references, so
  workload namespaces do not accumulate stale credentials.
- **An empty `simulationNodes.nodeSelector` follows Kubernetes semantics and
  selects every node**, including control planes; real-GPU nodes are still
  excluded, and the reconciler warns with an `AllNodesSelected` event.
- **Vocabulary size comes from the profile.** The simulator samples token ids below
  it; the 32000 default would produce out-of-range ids for models with a smaller
  vocabulary, so a profile that declares `model.vocabSize` drives `--vocab-size`.

## Platform notes (OpenShift)

- Manager runs with `runAsNonRoot` but no fixed UID, because SCCs assign a range.
- The device plugin needs `seLinuxOptions.type: spc_t` and the `privileged` SCC:
  kubelet's socket is labelled `kubelet_var_lib_t`. Same requirement as NVIDIA's
  own plugin; the container still requests no device and drops all capabilities.
- It runs as its own ServiceAccount so that SCC binding applies to it alone.
- No cert-manager: the install script issues a self-signed serving cert, creates
  the Secret, and installs with `install.sh --no-cert-manager`.
- Because a fail-closed Pod webhook is cluster-wide risk, admission can be scoped
  to namespaces labelled `vcr.opendatahub-io/admission=enabled`.

## What was actually verified

| Surface | Evidence |
| --- | --- |
| Admission, profiles, singleton | envtest suite (11 tests) against a real API server |
| Scheduling, allocation, exhaustion, plugin restart, readiness | kind suite, and the same checks on OpenShift |
| Real stack | official CPU vLLM 0.33 frontend + published `vllm-vcr` 0.2.5 + mock profile: Pod Ready with 0 restarts, `/v1/models`, chat completions, streaming, 389 `vllm:*` series |
| llm-d routing | real EPP from llm-d-router's chart discovered both pods, subscribed to their KV publishers, and routed by prefix locality (new prefix → uncached pod, repeats → cached pod) |

See `docs/spikes/real-stack-validation.md` and
`docs/spikes/llm-d-router-integration.md`.

## Not implemented, on purpose

Real inference or model quality, CUDA drivers, physical topology, arbitrary GPU
workloads, P/D disaggregation (rejected until validated), KV-event replay (the
pinned simulator has no replay endpoint), automatic support for other vLLM
releases, and timing fidelity beyond what a recorded profile provides. The mock
profile used for integration proves plumbing, not performance.
