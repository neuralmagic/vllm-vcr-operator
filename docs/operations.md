# Operations

How to install the operator, prepare nodes, and diagnose the states a simulated
modelserver can get stuck in. Every behavior named here is implemented by the code
paths cited; every assertion named here is produced by the command that runs it.

Related: [`config/README.md`](../config/README.md) (manifests, certificates,
images, RBAC), [`docs/settings-policy.md`](settings-policy.md) (why a workload was
rejected), [`docs/limitations.md`](limitations.md) (what cannot be claimed).

## Prerequisites

- A Kubernetes cluster where you can create a DaemonSet, a webhook Service, and
  two `*WebhookConfiguration` objects, and where you can label nodes.
- No GPU and no NVIDIA runtime: that is the point. Nodes must **not** already
  advertise `nvidia.com/gpu` from a real provider.
- A container runtime on the worker nodes that can run the frontend, simulator,
  and device-plugin images.
- A profile OCI image (see [`docs/profile-contract.md`](profile-contract.md)). No
  profile image is published by this repository.
- cert-manager, or a pre-created webhook serving certificate Secret.

Cluster scope: the operator namespace is `vllm-vcr-operator-system`, the
`VcrGpuCluster` is cluster scoped, and the admission webhook is
cluster scoped. Workloads may live in any namespace except the operator's own
(the mutating registration excludes it, so the operator's Pods are never
translated).

## Install

```bash
hack/install.sh                       # cert-manager path
hack/install.sh --no-cert-manager     # pre-created serving certificate
hack/install.sh --with-sample         # also apply config/samples
```

`hack/install.sh` applies the CRD, RBAC, webhook Service, certificate, and manager
first, waits for the CRD to be `Established` and the manager rollout to complete,
and only then applies the two webhook registrations — both are
`failurePolicy: Fail`, so applying them before the webhook has endpoints would
reject Pod creation cluster-wide. Details, required Secrets, and the manual
`caBundle` procedure are in [`config/README.md`](../config/README.md).

Verify the install:

```bash
kubectl -n vllm-vcr-operator-system rollout status deploy/vllm-vcr-operator-controller-manager
kubectl get crd vcrgpuclusters.vcr.opendatahub-io \
  -o jsonpath='{.status.conditions[?(@.type=="Established")].status}{"\n"}'
kubectl get mutatingwebhookconfiguration vllm-vcr-operator-mutating-webhook \
  validatingwebhookconfiguration vllm-vcr-operator-validating-webhook
```

`hack/verify-manifests.sh` is the cluster-free check of the same manifest set
(kustomize build, install set, RBAC superset, CRD idempotency); CI runs it.

## Configure

One cluster-scoped `VcrGpuCluster` named `default`; any other name is
rejected by the CRD's CEL rule.

```bash
kubectl apply -f test/fixtures/environments/h200.yaml   # or -k config/samples
kubectl get vgc default -o yaml
```

| Field | Effect |
| --- | --- |
| `spec.simulationNodes.nodeSelector` | Nodes that receive the synthetic device plugin. Required, at least one label. |
| `spec.simulationNodes.tolerations` | Applied to the device-plugin DaemonSet, so tainted simulation nodes can run it. |
| `spec.devicesPerNode` | Synthetic devices advertised per node (1–64). A scheduling quantity: a Pod requesting two devices consumes two, and selects a profile whose recorded topology matches. |
| `spec.gpuType` | The simulated accelerator. Participates in profile selection, so timing is never inferred from device count alone. |
| `spec.profileRegistry` | The OCI repository profile references resolve against, e.g. `ghcr.io/acme/vllm-vcr-profiles`. With it set, a `spec.profiles[].image` may name a tag or path inside it (`qwen3-0.6b-h200-tp1-bf16-0.1.0`, `nested/<id>`), and an absolute reference must live under it — one configured registry means one source of profiles. Unset means references are used exactly as written, which is how images in several registries are expressed. |
| `spec.profiles[].image` | Profile image or, with `spec.profileRegistry`, an identity inside it. A tag is resolved and pinned by digest during reconciliation; admission always pins a digest. |
| `spec.profiles[].pullSecretName`, `spec.imagePullSecrets` | Pull credentials in the operator namespace, for private profile images. |
| `spec.runtime.frontend/backend/provisioner` | Real CPU and memory for the injected containers. Must not request `nvidia.com/gpu`. |
| `spec.logLevel` | Simulator log filter (`RUST_LOG`). The frontend keeps the workload's own vLLM log level. |

Reconciliation publishes what admission uses:

- `status.profiles[]` — one entry per configured image, with the resolved
  `digest`, the parsed `metadata` document, `reconciledAt`, and a `message` when
  the profile is not usable. Admission reads this; it never contacts a registry.
- `status.supportedVLLMVersion` — the vLLM release this operator build translates
  against (`0.33.0`), fixed per release.
- `status.conditions` — see the table under *Diagnostics*.

Updating the configuration does not re-translate existing Pods: each admitted Pod
carries the profile id and digest it was pinned to, so a profile release cannot
silently change a running test. Roll the workload to pick up a new profile.

## Prepare nodes

```bash
kubectl label node <node> vcr.opendatahub-io/simulation=true
# optionally taint them so only tolerated workloads land there:
kubectl taint node <node> vcr.opendatahub-io/simulation=true:NoSchedule
```

The label must match `spec.simulationNodes.nodeSelector`. On every matching node
the reconciler creates the DaemonSet `vllm-vcr-device-plugin` in the operator
namespace. It:

- mounts the kubelet device-plugin directory (`/var/lib/kubelet/device-plugins`)
  and registers a device plugin for `nvidia.com/gpu` on socket
  `vllm-vcr-gpu.sock`;
- advertises `spec.devicesPerNode` devices, read from the operator-owned ConfigMap
  `vllm-vcr-device-plugin-config` key `device-count`, so the count can change
  without a new image;
- serves `/healthz` and `/readyz` on port `8080` for its own probes; `/readyz`
  succeeds only while the plugin is registered with kubelet;
- **refuses to run on nodes that already advertise `nvidia.com/gpu`** from another
  provider: the rendered Pod has a required node affinity excluding
  `nvidia.com/gpu.present` and `nvidia.com/gpu.product`. Two providers for one
  resource on one node corrupt kubelet's allocation bookkeeping, so this exclusion
  is not opt-out.

The plugin needs no host device, no capability, and no NVIDIA runtime class. It
runs with `privileged: false`, a read-only root filesystem, and all capabilities
dropped; the only privileged-looking requirement is that it creates its socket in
kubelet's root-owned directory, so it runs as uid 0 there. If that directory is
not writable, the container exits non-zero and crash-loops visibly instead of
advertising nothing.

Check advertisement from the node's point of view:

```bash
kubectl -n vllm-vcr-operator-system logs ds/vllm-vcr-device-plugin | tail
kubectl get node <node> -o jsonpath='{.status.allocatable.nvidia\.com/gpu}{"\n"}'

# The plugin image is distroless (no shell), so probe it over the network rather
# than exec'ing into it. Port-forward reaches one DaemonSet pod.
kubectl -n vllm-vcr-operator-system port-forward ds/vllm-vcr-device-plugin 8080:8080 &
curl -fsS http://127.0.0.1:8080/readyz && curl -fsS http://127.0.0.1:8080/healthz
kill %1
```

`/readyz` answers `503` with `not registered with kubelet or shutting down` when
the plugin has not registered, which is the fastest way to tell "no devices" from
"devices not yet advertised".

## Admission

For a Pod that requests or limits `nvidia.com/gpu`, admission either translates the
Pod completely or rejects it. Pods without a GPU request are returned unchanged.
The mutation is described in [`docs/contracts/admission.md`](contracts/admission.md);
the resulting Pod is:

| Container | Image | Role |
| --- | --- | --- |
| `vllm-frontend` (the workload's own container, renamed) | `VCR_FRONTEND_IMAGE` | `vcr-supervisor` running `vllm serve`: tokenization, rendering, generation loop, streaming, metrics, HTTP on the original inference port. Keeps the original `nvidia.com/gpu` quantity, ports, probes, mounts, and unrelated environment. |
| `vllm-vcr-backend` | `VCR_BACKEND_IMAGE` | `vllm-vcr play`: connects to the frontend's engine-core handshake, serves recorded timing and tokens, publishes KV events. |
| `vcr-profile-provisioner` (init) | the pinned profile image | Runs the profile's `provisioning.entrypoint` into the shared `vcr-profile` volume, then exits 0. |

Accelerator-specific placement is rewritten (GPU node selectors, GPU topology
affinity, NVIDIA runtime classes, real-device host paths); the environment's node
selector and tolerations are applied; every other placement constraint is kept and
reported if it cannot be satisfied. See *Diagnostics* when a Pod stays `Pending`.

Provenance annotations on the Pod, all under `vcr.opendatahub-io/`:

| Annotation | Value |
| --- | --- |
| `injected` | Runtime layout version (`v1`) |
| `profile` | Profile id from the selected metadata |
| `profile-image` | Digest-pinned profile image |
| `model` | Underlying model identity |
| `original-gpus` | GPU quantity preserved for the workload |
| `vllm-version` | Translation target (`0.33.0`) |

No annotation, status field, log line, or event contains a Secret value.

```bash
kubectl -n <ns> get pod <pod> -o jsonpath='{.metadata.annotations}' | jq .
kubectl -n <ns> get pod <pod> -o jsonpath='{range .spec.containers[*]}{.name}{"\t"}{.resources}{"\n"}{end}'
```

## Choosing a recorded profile per namespace

The cluster registers candidate profile images and publishes the reconciled catalog:

```bash
kubectl get vgc default -o jsonpath='{range .status.profiles[*]}{.image}{"\n"}{end}'
```

Without any further object, admission selects by the workload's own requirements — model,
GPU count, tensor parallelism, dtype, and so on. When that is not enough (two recordings fit
the same workload, and you want a specific one), a workload owner pins the choice with one
namespaced object:

```yaml
apiVersion: vcr.opendatahub-io/v1alpha1
kind: VcrGpuProfile
metadata:
  name: selection
  namespace: team-a
spec:
  profile: qwen3-0.6b-h200-tp1-bf16-poisson     # an id from the catalog, never an image
```

Exactly one may exist per namespace: admission pins every simulated workload in the
namespace to it, so a second one is refused with the name of the object that already holds
the choice. The object reports what the id resolved to, digest included, without waiting for
a Pod to be submitted:

```bash
kubectl -n team-a get vgp selection -o yaml
kubectl -n team-a get vgp selection -o jsonpath='{.status.resolved}{"\n"}'
kubectl -n team-a get vgp selection -o jsonpath='{range .status.conditions[*]}{.type}{"="}{.status}{" "}{.reason}{": "}{.message}{"\n"}{end}'
```

An id the catalog does not publish is reported on the object (`Ready=False`, reason
`NotOffered`, with the ids that are published), and admission refuses a Pod in that namespace
naming the same list. Deleting the object returns the namespace to requirement-based
selection. A `VcrGpuProfile` never relaxes the workload's requirements: it narrows the
catalog to one recording that must still be able to serve the Pod.

## Capacity exhaustion

Capacity is `devicesPerNode × selected nodes`. It is a scheduling quantity:

```bash
kubectl get node -l vcr.opendatahub-io/simulation=true \
  -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.allocatable.nvidia\.com/gpu}{"\n"}{end}'
kubectl get pods -A -o json | jq -r '.items[] | select(.spec.containers[]?.resources.requests["nvidia.com/gpu"]) | "\(.metadata.namespace)/\(.metadata.name) \(.status.phase)"'
```

A Pod beyond capacity stays `Pending` with
`0/N nodes are available: ... Insufficient nvidia.com/gpu`. That is the intended
signal for a capacity-exhaustion test, not a failure of the operator. Deleting a
Pod releases its allocation; kubelet owns allocation state, so the count returns
without operator involvement. Recreating the device-plugin Pod re-registers with
kubelet and re-advertises the same count.

## Failure diagnostics

Read the environment first: its conditions explain whether admission has anything
to select from.

```bash
kubectl get vgc default -o jsonpath='{range .status.conditions[*]}{.type}{"="}{.status}{" ("}{.reason}{") "}{.message}{"\n"}{end}'
```

| Condition | Reason | Meaning |
| --- | --- | --- |
| `Ready` | `EnvironmentReady` | Every profile resolved and the device plugin is ready on all selected nodes. |
| `Ready` | `ProfilesNotReconciled` | At least one configured profile is unusable, so admission would reject or mis-select. |
| `Ready` | `DevicePluginNotReady` | The DaemonSet is missing or not ready on at least one selected node. |
| `ProfilesReconciled` | `AllProfilesUsable` | Every profile's metadata label resolved and parsed. |
| `ProfilesReconciled` | `ProfileUnusable` | A profile image resolved but its metadata violates the profile contract. |
| `ProfilesReconciled` | `RegistryUnavailable` | A registry or pull-credential failure prevented resolution. |
| `DevicePluginReady` | `DaemonSetReady` / `DaemonSetNotReady` | DaemonSet rollout state. |

Messages name the image, the Secret, or the setting at fault and never a
credential value. The reconciler also emits a warning Event when a condition's
reason or message changes.

| Symptom | Cause and next step |
| --- | --- |
| `Error from server: admission webhook ... denied the request: <setting>: <reason>` | Working as designed. The message names the setting; look it up in [`docs/settings-policy.md`](settings-policy.md). Unsupported configuration is rejected, never approximated. |
| Webhook call fails entirely (`failed calling webhook`) | The webhook Service has no endpoints, or the `caBundle` does not match the serving certificate. Check the manager Pod and the Secret; see [`config/README.md`](../config/README.md). |
| Pod stays `Pending`: `Insufficient nvidia.com/gpu` | Capacity exhausted, or the node that has devices is excluded by the workload's own preserved constraints (zone, storage class, sidecar affinity). |
| Pod stays `Pending`: no matching node at all | Node labels do not match `spec.simulationNodes.nodeSelector`, or the device-plugin DaemonSet is not ready (see `DevicePluginReady`). |
| Pod `Error`/`CrashLoopBackOff` in `vcr-profile-provisioner` | The profile image's entrypoint failed or exports no trace. `kubectl logs <pod> -c vcr-profile-provisioner` names the missing document. Serving never starts in this case. |
| Pod `Pending` with `CreateContainerConfigError` | A projected pull secret is missing in the workload namespace. Check the profile's conditions and the operator-namespace Secret. |
| Pod runs but never becomes ready | Readiness requires the engine handshake. `kubectl logs <pod> -c vllm-vcr-backend` shows the simulator's handshake result; `kubectl logs <pod> -c vllm-frontend` shows the supervisor's readiness transitions and the frontend's own startup. The supervisor writes its readiness marker at `/tmp/vcr-runtime/ready` inside the frontend container and serves `/healthz` and `/readyz` on `127.0.0.1:8082`. |
| Requests hang, then fail | The simulator's control API (`/stats` on port `8001`) reports its own counters; it is JSON, **not** a metrics endpoint. Prometheus series (`vllm:*`) come from the frontend only. |
| Router never routes by prefix | Two independent causes: the workload does not configure `--kv-events-config` (see `test/fixtures/llmd/mock-prefix-cache-kv-events-deployment.yaml`), or the router's `tokenProcessorConfig.blockSize` differs from the profile's `cache.blockSize`, so block hashes never agree. Nothing logs an error for the second case. |
| Rejected: `--kv-events-config ... profile records no KV-cache events` | The profile's `trace.kvEvents` is false. Pick a profile recorded with events, or drop the setting. |

Useful commands:

```bash
kubectl -n <ns> describe pod <pod>                      # events, including admission's rejection reason
kubectl -n <ns> logs <pod> -c vllm-frontend --tail=200
kubectl -n <ns> logs <pod> -c vllm-vcr-backend --tail=200
kubectl -n <ns> logs <pod> -c vcr-profile-provisioner   # the export summary on success
kubectl -n vllm-vcr-operator-system logs deploy/vllm-vcr-operator-controller-manager --tail=200
```

## Acceptance evidence

Two commands produce the acceptance evidence. They are the only evidence this
repository generates for a cluster-shaped run, and they are deliberately separate
because they prove different things:

| Command | What it proves |
| --- | --- |
| `make test-e2e` | Admission against a real API server (envtest: etcd + kube-apiserver, the manager in-process): the mutating webhook's provenance annotations, injected layout, and GPU-placement invariants, its idempotency on reinvocation, preserved serving identity, the profile catalog published by reconciliation (from a fake registry, resolved with the configured pull credentials), and the fail-closed rejections with their messages visible to the caller. No kubelet, so nothing is scheduled, pulled, or served. |
| `make test-cluster` | The specification's primary interface on a real two-node kind cluster with a real kubelet: install from the shipped manifests, device-plugin registration and synthetic capacity, an unchanged GPU-requesting workload admitted and translated, profile provisioning into the shared volume by the real `vcr-profile-export`, readiness through the supervisor, allocation/exhaustion/release, device-plugin restart, and readiness withdrawal. The vLLM frontend and the `vllm-vcr` simulator are test doubles, so nothing about serving fidelity, recorded timing, or token content is claimed. |

Both suites skip — never pass — when their prerequisites are absent, and the
cluster suite's skip names the missing prerequisite (`VCR_CLUSTER_REQUIRED=1`
turns that skip into a failure). `make test-spike` remains the only command that
can produce serving evidence, and it needs the real images.

```
$ make test-e2e
$ hack/kind-up.sh && hack/images.sh --build --stub --load && make test-cluster
$ hack/kind-up.sh --delete            # after the run
```

## Cleanup

```bash
kubectl -n <ns> delete deploy,po -l llm-d.ai/guide=precise-prefix-cache-routing
kubectl delete vgc default            # removes the device-plugin DaemonSet and its ConfigMap (owner references)
kubectl label node <node> vcr.opendatahub-io/simulation-
# uninstall the operator itself:
kustomize build config/default | kubectl delete -f -
```

Deleting the `VcrGpuCluster` deletes the operator-owned DaemonSet and its
ConfigMap through owner references; kubelet stops advertising `nvidia.com/gpu`
once the plugin unregisters.

Pull credentials live as Secrets in the operator namespace. The admission contract
(`docs/contracts/admission.md`) requires only the credentials the selected profile
needs to be made available to the workload namespace, under names carrying the
`vcr-profile-pull-` prefix (`names.PullSecretPrefix`). Before deleting a namespace
by hand, list them:

```bash
kubectl -n <ns> get secrets | grep vcr-profile-pull || true
```

Pod deletion drains through the normal path: `SIGTERM` reaches the frontend
container's supervisor and the simulator container, readiness is withdrawn, and
the frontend's graceful shutdown budget comes from the workload's
`--shutdown-timeout`. The mechanism is documented in
[`docs/contracts/runtime-layout.md`](contracts/runtime-layout.md) and asserted by
`make test-spike` (with images) and `make test-cluster`.
