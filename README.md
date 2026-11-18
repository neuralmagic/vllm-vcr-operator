# vllm-vcr-operator

Run llm-d's real routing, discovery, and lifecycle tests against **simulated
model servers on GPU-free Kubernetes clusters**, without editing a single
GPU-targeted workload manifest.

The operator watches for Pods that request `nvidia.com/gpu`. For those Pods it
keeps the GPU request (so scheduling and capacity-exhaustion tests still work),
replaces the real vLLM engine with a CPU-hosted Python vLLM frontend connected to
[`vllm-vcr play`](https://github.com/nealimgic/vllm-vcr), and injects a
profile-provisioning init container that exports recorded model timing and content
from an OCI image. llm-d components stay real: only the engine is simulated.

```
                    unchanged GPU workload (Deployment, Pod, …)
                                    │  admission
                                    ▼
┌───────────────────────────── simulated modelserver Pod ─────────────────────────────┐
│ nvidia.com/gpu: 1  (preserved, allocated by the synthetic device plugin)            │
│                                                                                     │
│  init: vcr-profile-provisioner  → exports metadata + trace into vcr-profile volume   │
│                                                                                     │
│  vllm-frontend    vcr-supervisor ─▶ vllm serve (CPU frontend, original port)         │
│  vllm-vcr-backend vllm-vcr play  ─▶ engine-core handshake (loopback :29550)          │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

## Status

Early. What exists is the operator, its translation and runtime logic, the
synthetic device plugin, the profile reconciliation and provisioning path, the
manifests, and the spike harnesses. Read
[`docs/limitations.md`](docs/limitations.md) before relying on any behavior: it
lists what is verified, what is rejected, and what cannot be claimed yet.

Status of specific guarantees:

| Guarantee | Where it comes from | Verified how |
| --- | --- | --- |
| Admission translates or rejects, never silently approximates | [`docs/contracts/admission.md`](docs/contracts/admission.md) | `make test-e2e` asserts admitted/rejected Pods and provenance annotations |
| One Pod runs one simulated model server with the documented container layout | [`docs/contracts/runtime-layout.md`](docs/contracts/runtime-layout.md) | `make test-e2e` asserts the mutated Pod; `go test ./test/spike/...` asserts the lifecycle with real images |
| Every supported or rejected vLLM setting has a reviewable policy row | [`docs/settings-policy.md`](docs/settings-policy.md) | `go test ./internal/translator/...` |
| A profile either explains the workload or the workload is rejected | [`docs/contracts/profile-image.md`](docs/contracts/profile-image.md) | `go test ./internal/profile/...` |
| A GPU-free cluster can schedule and serve the workload | `test/cluster` | `make test-cluster` (kind or any cluster, no GPU) |

Nothing in this repository has been executed against real vLLM/vllm-vcr images in
this checkout, so no serving-fidelity claim is made. The command that produces
that evidence is named next to every claim. `make test-e2e` and
`make test-cluster` are the targets of `test/e2e` and `test/cluster`; a checkout
without those suites has no such target.

## Quick start

CPU-only cluster, no GPU and no NVIDIA runtime.

Prerequisites that no script can decide for you:

- **a webhook serving certificate.** `config/default` uses cert-manager
  (`cert-manager.io/inject-ca-from`); without cert-manager, create the Secret and
  install with `hack/install.sh --no-cert-manager`. The manager does not start
  until the certificate Secret exists.
- **a profile image.** `config/samples` references a documented placeholder, and
  admission rejects every workload until a usable profile is reconciled. A profile
  is an OCI image carrying recorded metadata and trace data, produced by a separate
  repository; [`docs/profile-contract.md`](docs/profile-contract.md) explains how to
  build, publish, and reference one.

```bash
# 1. Install the operator: CRD, RBAC, webhook Service, manager.
hack/install.sh

# 2. Configure the synthetic devices and the serving runtime sizing, then label
#    the nodes that may simulate GPUs. The label must match
#    spec.simulationNodes.nodeSelector. Replace the sample's placeholder profile
#    image with a real digest-pinned reference before applying it.
kubectl apply -k config/samples
kubectl label node <node> vcr.opendatahub-io/simulation=true

# 3. Watch the environment reconcile: the profile catalog and the device plugin.
kubectl get sime default -w
kubectl -n vllm-vcr-operator-system get ds vllm-vcr-device-plugin

# 4. The sample GPU workload is applied by step 2 — unchanged, with no simulator
#    opt-in. Watch admission rewrite its Pods, then drive the modelserver.
kubectl -n vllm-vcr-simulated rollout status deploy/mock-prefix-cache
examples/completions.sh vllm-vcr-simulated deploy/mock-prefix-cache
```

Install details, required Secrets, image overrides, and RBAC are in
[`config/README.md`](config/README.md) and [`docs/operations.md`](docs/operations.md).

## Documentation

| Document | Contents |
| --- | --- |
| [`docs/operations.md`](docs/operations.md) | Install, configuration, node preparation, device plugin behavior, capacity exhaustion, diagnostics, cleanup |
| [`docs/capability-matrix.md`](docs/capability-matrix.md) | Supported, unverified, and rejected serving capabilities |
| [`docs/settings-policy.md`](docs/settings-policy.md) | Every vLLM 0.33.0 setting the operator accepts, translates, or rejects, with rationale and rejection examples |
| [`docs/profile-contract.md`](docs/profile-contract.md) | What a profile image is, how to build and publish one, and the private-registry flow |
| [`docs/contracts/admission.md`](docs/contracts/admission.md) | Binding admission, mutation, and provenance contract |
| [`docs/contracts/runtime-layout.md`](docs/contracts/runtime-layout.md) | Binding container/process layout, ordering, readiness, and supervision |
| [`docs/contracts/profile-image.md`](docs/contracts/profile-image.md) | Binding profile image and metadata schema |
| [`docs/spikes/runtime-layout.md`](docs/spikes/runtime-layout.md) | Runtime-layout spike record and evidence format |
| [`docs/spikes/serving-behavior.md`](docs/spikes/serving-behavior.md) | Serving-behavior spike record and evidence format |
| [`docs/limitations.md`](docs/limitations.md) | Out-of-scope items and known gaps, stated plainly |
| [`docs/upstream-vllm-vcr.md`](docs/upstream-vllm-vcr.md) | The pinned `vllm-vcr` release, the facts encoded from it, and how to re-pin (`make check-upstream`) |
| [`config/README.md`](config/README.md) | Manifests, install order, certificates, images, RBAC |
| [`test/fixtures/README.md`](test/fixtures/README.md) | Integration fixtures and the llm-d end-to-end path |
| [`test/spike/README.md`](test/spike/README.md) | How to run the spike harnesses and read their evidence |
| [`examples/README.md`](examples/README.md) | Copy-pasteable walkthroughs |

## What this does not do

- **No inference, no CUDA/GPU-driver emulation, no model-quality equivalence.**
  Output is recorded token ids or the simulator's seeded random tokens.
- **No performance claims.** Latency is the profile's recorded latency. Two
  synthetic devices are a scheduling quantity, not two engines.
- **Not a general GPU substitute.** Only vLLM "serve" workloads with a supported
  invocation are admitted; anything else is rejected with a reason.
- **No distributed execution.** Pipeline parallelism, data/tensor-parallel worker
  groups, multi-node ranks, and P/D roles are rejected unless a profile grants a
  validated capability set.
- **No KV-event replay.** The simulator publishes live cache events and has no
  replay endpoint, so a router that loses the event window cannot re-synchronize
  from the engine. See [`docs/limitations.md`](docs/limitations.md).
- **No DRA.** Device requests go through the extended resource
  `nvidia.com/gpu`, not Dynamic Resource Allocation.
- **No profile production.** Profile capture, packaging, and publishing happen in
  a separate repository; this operator consumes the images.
- **No router or dispatcher of its own.** llm-d's components are used as they are;
  replacing them would defeat the purpose.

## Repository layout

| Path | Contents |
| --- | --- |
| `api/v1alpha1` | The cluster-scoped `SimulationEnvironment` API |
| `internal/vllm` | vLLM 0.33.0 invocation and configuration resolution |
| `internal/profile` | Profile metadata, catalog, and selection rules |
| `internal/translator` | Settings policy → admission plan |
| `internal/runtime` | Admission plan → mutated Pod |
| `internal/webhook` | Admission webhook and secret projection |
| `internal/controller` | Environment, profile-catalog, and device-plugin reconcilers |
| `internal/deviceplugin` | Kubelet device-plugin gRPC server advertising synthetic GPUs |
| `internal/supervisor` | Frontend lifecycle supervision and readiness |
| `internal/registry` | Profile OCI metadata and digest resolution |
| `cmd/*` | `manager`, `deviceplugin`, `supervisor`, `profileexport` binaries |
| `config/` | Kustomize manifests, CRD, RBAC, webhook registrations |
| `docs/` | Contracts, policy, capability matrix, operations, limitations, spike records |
| `test/e2e`, `test/cluster` | envtest acceptance and GPU-free cluster acceptance |
| `test/spike` | The two required spikes, with evidence documents |
| `test/fixtures`, `examples/` | Integration fixtures and walkthroughs |

## Development

```bash
go build ./...        # compile
go test ./...         # unit tests
make test-e2e         # envtest acceptance suite (needs envtest assets)
make test-cluster     # GPU-free cluster acceptance suite (needs a cluster)
make test-spike       # the two spikes; skips without images
make manifests generate
```

`hack/verify-manifests.sh` is the cluster-free manifest check (also run in CI).
