# Kind-cluster acceptance suite (`make test-cluster`)

This is the specification's **primary acceptance interface**: submit an unchanged,
GPU-requesting vLLM workload to a CPU-only Kubernetes cluster and observe synthetic
allocation, placement, admission mutation, profile provisioning, and
readiness/failure behavior.

It runs the real operator binaries against a real API server, a real kubelet, a real
device-plugin registration, and a real `vcr-supervisor`. Two containers are test
doubles — the Python vLLM frontend and the Rust `vllm-vcr` simulator — because neither
image is available on the machines this suite targets. `test/images/README.md` records
exactly what the doubles do and do not establish. **No claim about serving fidelity,
recorded timing, or token content is made by this suite**; that is `test/spike`'s job
with real images.

## Prerequisites

```bash
# 1. a two-node kind cluster (control-plane + worker), rootless podman is fine
hack/kind-up.sh                     # idempotent; --delete removes it

# 2. the operator images and the test-only runtime images, loaded into both nodes
hack/images.sh --build --stub --load

# 3. the suite
make test-cluster
```

`hack/kind-up.sh` uses Docker when its daemon answers and rootless podman otherwise
(`KIND_EXPERIMENTAL_PROVIDER=podman`); it fails with a message when neither works.
`hack/images.sh` never requires Docker and never uses `kind load` (it does not work
with the podman provider): it saves each fully qualified `docker.io/vcrlocal/...`
reference and imports it into every node's containerd, which is why Pods must use the
same qualified reference with `imagePullPolicy: IfNotPresent` (the operator already
does).

The suite skips — never passes — when the cluster or the images are absent, and its
skip message names the missing prerequisite. `VCR_CLUSTER_REQUIRED=1` turns a skip
into a failure, so a CI job that intends to run it cannot report a green skip.

| Variable | Default | Meaning |
| --- | --- | --- |
| `VCR_CLUSTER_CONTEXT` | `kind-vcr-e2e` | kubeconfig context to run against |
| `VCR_IMAGE_PREFIX` | `docker.io/vcrlocal` | image repository prefix (must match `hack/images.sh`) |
| `VCR_IMAGE_TAG` | `test` | image tag |
| `VCR_IMAGE_RUNTIME` | podman, else docker | runtime used for node-level operations |
| `VCR_CLUSTER_ARTIFACTS` | `test/cluster/_artifacts` | where failure diagnostics are written |
| `VCR_CLUSTER_REQUIRED` | unset | `1` turns a skip into a failure |

Runtime is about 2–3 minutes on a laptop: install ~20 s, workloads and readiness
~40 s, exhaustion, plugin restart, and failure behavior the rest.

## What the suite installs (and removes)

The suite is self-contained on purpose: `make test-cluster` needs nothing but a kind
cluster and the images.

1. **Operator, without cert-manager.** It renders `config/default` with
   `kubectl kustomize`, drops the `Certificate`/`Issuer` objects, generates a
   self-signed CA and webhook serving certificate (`test/cluster/certs`), creates the
   serving-certificate Secret, patches the manager Deployment to use the locally built
   images, applies the core objects, waits for the CRD and the manager rollout, and only
   then applies the fail-closed admission registrations and publishes the `caBundle`
   (the same two-phase order `hack/install.sh` uses).
2. **A TLS OCI registry.** The operator resolves profile metadata over HTTPS only
   (`internal/registry` is constructed without `Options.Insecure`), so a plain-HTTP
   registry cannot be used. The suite runs `registry:2` with a certificate from the same
   CA, pins it to one node with a hostPort, installs the CA into every node's containerd
   `certs.d` configuration (so kubelet can pull the digest-pinned profile image), and
   gives the manager `SSL_CERT_FILE` so it trusts the CA. The profile fixture image is
   pushed into that registry from the node's own containerd.
3. **The singleton `VcrGpuCluster`**, labelled onto the worker node only, so
   placement is observable: the control-plane node must never advertise synthetic GPUs.

Teardown deletes the workload namespaces, the `VcrGpuCluster`, the admission
registrations, the CRD, the RBAC, the operator and registry namespaces, the test
`RuntimeClass`, and the simulation node label, then asserts no cluster-scoped residue
remains. A run that fails earlier still tears down (`t.Cleanup`).

## What is asserted, step by step

| Step | Assertion (all against cluster state) |
| --- | --- |
| install | `VcrGpuCluster` reaches `Ready`; `status.profiles[0].digest` is resolved from the test registry and its metadata is the fixture document |
| device plugin | node `status.allocatable` has `nvidia.com/gpu: 8` on the simulation node and none elsewhere; the DaemonSet is Ready with one Pod on the simulation node; the Pod mounts `/var/lib/kubelet/device-plugins`, is not privileged, and uses no runtime class |
| admission | on a real API server: provenance annotations (profile id, digest-pinned image, model, `original-gpus`, vLLM version), injected `vllm-frontend`/`vllm-vcr-backend`/`vcr-profile-provisioner`, exactly one container keeping `nvidia.com/gpu: 1`, provisioner image = digest-pinned profile image, llm-d labels/ports/ServiceAccount/model-cache volume preserved, GPU-only env removed, and a non-GPU Pod plus an unsupported (`sh -c`) workload (denied, message visible on the ReplicaSet) |
| placement | `nvidia` runtime class and `nvidia.com/gpu.product` affinity removed, simulation node selector applied, `kubernetes.io/os=linux` affinity preserved; an unsatisfiable *non-accelerator* constraint is admitted and left `Pending` with the scheduler's `FailedScheduling` message (documented behavior) |
| provisioning / readiness | provisioner init container `Completed`; the profile volume holds the fixture `metadata.json` and a non-empty trace; the simulator logs reading that trace and registering the engine; the supervisor's ready file exists; `/v1/models` answers and `/v1/chat/completions` streams SSE; the simulator listens on the KV-event port |
| allocation / exhaustion / release | 8 replicas × 1 device with one device already held: 7 schedule, 1 stays `Pending` with `Insufficient nvidia.com/gpu`; deleting the holder's Deployment releases the device and the pending replica schedules and becomes Ready |
| plugin restart | deleting the device-plugin Pod yields a new Ready Pod, allocatable capacity returns to 8, and an existing allocation keeps running without a container restart (kubelet owns allocation state) |
| readiness withdrawal | making the simulator stop answering its control API withdraws readiness (and it stays withdrawn), the supervisor terminates the frontend after its grace period, kubelet's restart policy is what recovers it, the supervisor never restarts the simulator container, restoring the engine makes the Pod Ready again, and crashing the simulator container shows kubelet restarting it |
| cleanup | no namespaces, CRD, admission registrations, `VcrGpuCluster`, or reachable test registry remain |

## Failure diagnostics

On failure the step writes, under `test/cluster/_artifacts/<step>/`:

- `pods.yaml`, `pods.txt`, `deployments.yaml`, `replicasets.txt`, `events.txt`
- `injected-args.txt` — every container's `command`/`args` as admitted
- `describe-<pod>.txt`, `logs-<pod>-<container>.txt` (and `--previous` logs)
- `vcrgpucluster.yaml`, `nodes.txt`, `node-allocatable.txt`
- `operator-logs.txt`, `device-plugin-pods.txt`, `device-plugin-describe.txt`

The directory is gitignored by `test/cluster/_artifacts/.gitignore`.

## Verified locally

`kind-vcr-e2e` (kind cluster from `hack/kind-up.sh`, rootless podman as the kind
provider), on the machine the suite was developed on:

```
$ hack/kind-up.sh && hack/images.sh --build --stub --load && make test-cluster
ok  	github.com/neuralmagic/vllm-vcr-operator/test/cluster	151.829s
ok  	github.com/neuralmagic/vllm-vcr-operator/test/cluster/certs	0.221s
```

The run exercises the real path with no workaround: the shipped `config/default`
(including its namespace Pod Security label) is applied unchanged, and the profile
fixture runs the real `vcr-profile-export` binary with no wrapper. The suite asserts
the provisioner init container completes, that the profile volume holds exactly
`metadata.json` and the trace file, and that no staging directory is left behind.

## Defects this suite found, and where they were closed

The suite is the acceptance interface, so it reported product defects instead of
fixing them. All four are closed on the integration branch, and none is worked
around here any more:

| Defect | Closed by |
| --- | --- |
| The generated Pod was invalid: container port names longer than 15 characters (`engine-handshake`, `supervisor-health`) made the API server reject every admitted Pod | port renames in `internal/translator` and `internal/runtime` |
| The operator namespace was labelled `pod-security.kubernetes.io/enforce: baseline`, which forbids the device plugin's `hostPath`, so no synthetic capacity was ever advertised | `config/default/namespace.yaml` labels the namespace `privileged`, with the reason recorded there, in `config/README.md`, and asserted by `hack/verify-manifests.sh` |
| The provisioner could not stage under a read-only root filesystem (`EROFS` on `/var/run/vcr`), so the fixture shipped a wrapper that hid it | `cmd/profileexport` stages inside the target directory (the writable profile volume), publishes `metadata.json` last, and removes its staging directory on every path; the wrapper is gone |
| Status update conflicts were handled by requeue instead of a retry | `internal/controller` retries the status write with `k8s.io/client-go/util/retry.RetryOnConflict` and a fresh `Get`, requeueing only when the retry budget is spent |

## Knowingly unprovable on this machine

- Real vLLM serving, recorded timing, token identity, and llm-d routing (no Python vLLM
  image, no Rust simulator, no recorded profile). `test/spike` owns that evidence.
- Real GPU topology, real accelerator runtime classes, and DRA.
- Anything requiring a multi-node cluster beyond two kind nodes, or a node with NVIDIA
  hardware (the suite's whole point is that none is needed).
- Profile pull credentials: the fixture profile image is pulled anonymously, so the
  projected pull-secret path is not exercised here.
- Image publication: nothing in this repository publishes the operator or profile
  images; the suite loads locally built images by fully qualified reference.
