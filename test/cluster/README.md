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
3. **The singleton `SimulationEnvironment`**, labelled onto the worker node only, so
   placement is observable: the control-plane node must never advertise synthetic GPUs.

Teardown deletes the workload namespaces, the `SimulationEnvironment`, the admission
registrations, the CRD, the RBAC, the operator and registry namespaces, the test
`RuntimeClass`, and the simulation node label, then asserts no cluster-scoped residue
remains. A run that fails earlier still tears down (`t.Cleanup`).

## What is asserted, step by step

| Step | Assertion (all against cluster state) |
| --- | --- |
| install | `SimulationEnvironment` reaches `Ready`; `status.profiles[0].digest` is resolved from the test registry and its metadata is the fixture document |
| device plugin | node `status.allocatable` has `nvidia.com/gpu: 8` on the simulation node and none elsewhere; the DaemonSet is Ready with one Pod on the simulation node; the Pod mounts `/var/lib/kubelet/device-plugins`, is not privileged, and uses no runtime class |
| admission | on a real API server: provenance annotations (profile id, digest-pinned image, model, `original-gpus`, vLLM version), injected `vllm-frontend`/`vllm-vcr-backend`/`vcr-profile-provisioner`, exactly one container keeping `nvidia.com/gpu: 1`, provisioner image = digest-pinned profile image, llm-d labels/ports/ServiceAccount/model-cache volume preserved, GPU-only env removed, and a non-GPU Pod plus an unsupported (`sh -c`) workload (denied, message visible on the ReplicaSet) |
| placement | `nvidia` runtime class and `nvidia.com/gpu.product` affinity removed, simulation node selector applied, `kubernetes.io/os=linux` affinity preserved; an unsatisfiable *non-accelerator* constraint is admitted and left `Pending` with the scheduler's `FailedScheduling` message (documented behavior) |
| provisioning / readiness | provisioner init container `Completed`; the profile volume holds the fixture `metadata.json` and a non-empty trace; the simulator logs reading that trace and registering the engine; the supervisor's ready file exists; `/v1/models` answers and `/v1/chat/completions` streams SSE; the simulator listens on the KV-event port |
| allocation / exhaustion / release | 8 replicas × 1 device with one device already held: 7 schedule, 1 stays `Pending` with `Insufficient nvidia.com/gpu`; deleting the holder's Deployment releases the device and the pending replica schedules and becomes Ready |
| plugin restart | deleting the device-plugin Pod yields a new Ready Pod, allocatable capacity returns to 8, and an existing allocation keeps running without a container restart (kubelet owns allocation state) |
| readiness withdrawal | making the simulator stop answering its control API withdraws readiness (and it stays withdrawn), the supervisor terminates the frontend after its grace period, kubelet's restart policy is what recovers it, the supervisor never restarts the simulator container, restoring the engine makes the Pod Ready again, and crashing the simulator container shows kubelet restarting it |
| cleanup | no namespaces, CRD, admission registrations, `SimulationEnvironment`, or reachable test registry remain |

## Failure diagnostics

On failure the step writes, under `test/cluster/_artifacts/<step>/`:

- `pods.yaml`, `pods.txt`, `deployments.yaml`, `replicasets.txt`, `events.txt`
- `injected-args.txt` — every container's `command`/`args` as admitted
- `describe-<pod>.txt`, `logs-<pod>-<container>.txt` (and `--previous` logs)
- `simulationenvironment.yaml`, `nodes.txt`, `node-allocatable.txt`
- `operator-logs.txt`, `device-plugin-pods.txt`, `device-plugin-describe.txt`

The directory is gitignored by `test/cluster/_artifacts/.gitignore`.

## Verified locally

`kind-vcr-e2e` (kind v0.32.0, Kubernetes v1.36.1, rootless podman 5.8.9 as the kind
provider), on this machine:

```
$ go test -count=1 -timeout 30m -v ./test/cluster/
...
--- PASS: TestClusterAcceptance (132.37s)
```

Three consecutive runs passed (132 s, 148 s, 152 s). They required the first known
defect below to be fixed in the working tree. With the defect present the suite fails in
the admission step after about three seconds, with the API server's own words — the
correct outcome for a Pod the operator cannot produce:

```
the API server rejected the Pod the operator generated: Error creating: Pod
"modelserver-admission-…" is invalid: [spec.containers[0].ports[2].name: Invalid
value: "engine-handshake": must be no more than 15 characters,
spec.containers[0].ports[3].name: Invalid value: "supervisor-health": must be no more
than 15 characters]
```

See "Known product defects".

## Known product defects found by this suite

These are reported here rather than fixed: `test/cluster/**` and `test/images/**` are
the suite's ownership boundary. Each is reproducible with `make test-cluster` on the
cluster above.

1. **Blocking: the injected Pod is invalid, so no workload can be admitted.**
   `internal/translator/frontend.go` names a container port `engine-handshake` (16
   characters) and `internal/runtime/runtime.go` names one `supervisor-health` (17).
   Kubernetes requires container port names to be at most 15 characters
   (`IANA_SVC_NAME`), so the API server rejects every Pod the webhook produces:
   `spec.containers[0].ports[2].name: Invalid value: "engine-handshake": must be no
   more than 15 characters`. The ReplicaSet records `FailedCreate`, no Pod ever exists,
   and nothing downstream can run. Suggested fix (two renames, no behavior change):
   `engine-handshake` → `engine-hs`, `supervisor-health` → `supervisor-hl`, with the
   two unit-test expectations in `internal/runtime/mutate_test.go` and
   `internal/translator/invocation_test.go` updated to match. The suite was verified
   green with exactly that patch applied.
2. **The operator namespace's Pod Security label rejects the device plugin.**
   `config/default/namespace.yaml` sets `pod-security.kubernetes.io/enforce: baseline`,
   but the operator-managed DaemonSet mounts `/var/lib/kubelet/device-plugins` as a
   hostPath, which baseline forbids. The DaemonSet is then rejected
   (`would violate PodSecurity "baseline:latest": hostPath volumes`) and no synthetic
   capacity ever appears, so `SimulationEnvironment.status` never reports
   `DevicePluginReady`. The suite relabels the namespace `privileged` after applying
   `config/default`; the manifest should carry that label itself (the device plugin's
   need is exactly the case baseline cannot express).
3. **Provisioning cannot stage on a read-only root filesystem.**
   `internal/runtime` runs the provisioner with `readOnlyRootFilesystem: true` and
   mounts only `/var/run/vcr/profile`, while `cmd/profileexport` stages its output next
   to the target (`/var/run/vcr`) and renames it into place. On a real kubelet the
   provisioner therefore fails with `EROFS`/`permission denied` before serving starts.
   The fixture profile image ships `/usr/local/bin/vcr-profile-export-volume`, which
   runs the real exporter (its metadata and trace validation included) with a staging
   directory inside the mounted volume. The product fix belongs in `internal/runtime`:
   make the provisioner's staging parent writable (mount an emptyDir at
   `/var/run/vcr`, or give the exporter a staging directory under the target).
4. **Status update conflicts are not retried.** The reconciler logs
   `update SimulationEnvironment default status: Operation cannot be fulfilled ... the
   object has been modified` and relies on a full requeue with backoff. It recovers, but
   a conflict on its own status write should be re-read and retried.

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
