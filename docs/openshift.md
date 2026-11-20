# OpenShift

The operator runs on OpenShift. This document records what differs from a plain
Kubernetes cluster, why, and what has actually been verified.

## Install

```bash
hack/openshift-install.sh --yes \
  --admission-namespaces vllm-vcr-e2e \
  --operator-image  image-registry.openshift-image-registry.svc:5000/vcr-images/vllm-vcr-operator:test \
  --device-plugin-image image-registry.openshift-image-registry.svc:5000/vcr-images/vllm-vcr-device-plugin:test \
  --frontend-image image-registry.openshift-image-registry.svc:5000/vcr-images/vllm-vcr-frontend-stub:test \
  --backend-image  image-registry.openshift-image-registry.svc:5000/vcr-images/vllm-vcr-backend-stub:test
```

The operator namespace is fixed at `vllm-vcr-operator-system`; the CRD, RBAC, and
webhook Service name are generated for it, and the webhook certificate SANs name
it explicitly.

## What differs

| Difference | Handling |
| --- | --- |
| cert-manager is usually absent | The script generates a self-signed CA and serving certificate, creates the Secret, and installs with `hack/install.sh --no-cert-manager`, which publishes `caBundle` from it. OpenShift's service-ca is not required. |
| SCCs restrict Pods | The operator manager runs with no fixed UID (`runAsNonRoot` only) so the namespace's allocated UID range applies. The device-plugin DaemonSet runs under the `privileged` SCC bound to its dedicated ServiceAccount. |
| SELinux | kubelet's socket is labelled `kubelet_var_lib_t`; the plugin container needs `seLinuxOptions.type: spc_t` to connect to it. The same permission the NVIDIA device plugin needs. The plugin still requests no device, mounts no NVIDIA runtime, and drops every capability. |
| Fail-closed webhooks are cluster-wide risk | `--admission-namespaces` scopes both registrations to namespaces labelled `vcr.opendatahub-io/admission=enabled`. Use it on any cluster not dedicated to tests: with cluster-wide registrations, a stopped operator blocks every Pod creation in the cluster. |
| Internal image registry | Profile metadata is resolved over TLS with the service CA mounted into the operator (`SSL_CERT_FILE`) and credentials from `spec.imagePullSecrets`. Only credentials whose registry host matches the selected profile image are projected into workload namespaces. |

## Verified on OpenShift (k8s v1.35.9, 3 workers, no GPUs)

Cluster: 3.5 CPU / ~14 GiB per node, `nvidia.com/gpu` absent before install.
Workload: the unchanged GPU-targeting llm-d fixture (`test/fixtures/llmd`), one
`nvidia.com/gpu` per replica. Runtime images were the test doubles
(`test/images`), so this proves operator and platform behavior, not vLLM serving
fidelity.

| Check | Observed |
| --- | --- |
| Device plugin registration | node advertises `nvidia.com/gpu: 8`; non-selected nodes advertise none |
| Admission of the unchanged workload | frontend image replaced, original GPU quantity kept on exactly one container, backend injected with no GPU request, provisioner digest-pinned, provenance annotations present, pull secret projected into the workload namespace |
| Profile resolution | digest `sha256:f748f182…` resolved from the in-cluster registry with the configured pull secret |
| Serving interface | frontend answers `/v1/models` on the original port; supervisor `/readyz` reports `ready` |
| Capacity exhaustion | 9 replicas on 8 devices → 8 Running, 1 Pending, `Insufficient nvidia.com/gpu`, allocation never exceeds 8 |
| Release | scale-down frees capacity back to 1 allocated |
| Device-plugin restart | plugin Pod deleted → re-registers, capacity back to 8, running workload restarts stay at 0 |
| Backend unavailable (stuck) | Pod `Ready=False`, supervisor terminates the frontend; no healthy-but-engine-less serving |
| Backend crash recovery | backend restart → frontend restart → `Ready=True` in 18s (restart-based, not reconnect-based) |

## Not verified here

Real vLLM/vllm-vcr images, recorded-token fidelity, timing calibration, llm-d
routing and KV-event consumption. Those require the profile-production deliverable
and a published CPU frontend image for the pinned vLLM release; see
`docs/limitations.md`.

## Verified after the operational changes (OCP 4.22, 3 workers)

| Check | Observed |
| --- | --- |
| Admission HA | two manager replicas Running, one holds the leader lease, PDB `minAvailable: 1` |
| Admission decision events | `SimulationInjected` on `namespace/llm-d-e2e`: profile id, digest, model, GPU count |
| Vocabulary size from the profile | backend args carry `--vocab-size 151936` (Qwen3-0.6B), not the simulator default |
| Pull-secret reconciliation | copy present while the Pod ran; after scaling to 0 it was deleted 160s later, logged by `projected-secrets` |
| Serving | Pod `Ready=True`, `engine connected to frontend`, KV publisher bound `tcp://0.0.0.0:5556` with `kv@10.128.2.26:8000@Qwen/Qwen3-0.6B` |

Startup TLS lines ("bad certificate") appear only before `caBundle` is published;
in steady state both replicas log none and admission succeeds.
