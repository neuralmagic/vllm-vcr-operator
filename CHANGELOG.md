# Changelog

## v0.2.0

Operational hardening after running the operator against real images.

- **Pull-secret projection is reconciled.** Copies are refreshed when a source
  Secret rotates and removed when no Pod references them; admission alone used to
  create them and nothing cleaned up.
- **Two admission replicas.** Controllers stay leader-elected; every replica
  serves admission, a PDB keeps one available during drains, and a replica is
  Ready only once its webhook server serves.
- **Admission decisions are visible.** `SimulationInjected` / `SimulationRejected`
  events carry the setting and reason, emitted through a bounded queue so
  telemetry cannot block admission.
- **Vocabulary size comes from the profile** (`model.vocabSize` → `--vocab-size`),
  so random token ids stay inside the model's vocabulary.
- **Simulator supervision is POSIX-shell**, not bash, and restarts the simulator
  process in place; recovery inside the loss grace costs no container restart.
- **Empty `simulationNodes.nodeSelector` means "every node"**, with an
  `AllNodesSelected` warning event; real-GPU nodes stay excluded.
- `hack/bump-vllm.sh` moves the operator to another vLLM release.
- Verified end to end: official CPU vLLM 0.33.0 frontend plus published
  `vllm-vcr` 0.2.5, and llm-d-router prefix-cache locality.

## v0.1.0

First complete implementation of the specification.

- Cluster-scoped `SimulationEnvironment` (name `default` only) selecting nodes,
  synthetic capacity, GPU type, profiles, and runtime sizing.
- Admission that translates GPU-requesting vLLM Pods or rejects them with an
  actionable reason; never silently approximates.
- vLLM 0.33.0 invocation resolution (args, config files, environment, defaults)
  and a settings policy where every setting has a disposition.
- Profile OCI images: metadata reconciliation, digest pinning, provisioning init
  container, private registries.
- Synthetic GPU device plugin advertising `nvidia.com/gpu` with a dedicated
  ServiceAccount and an operator-managed DaemonSet.
- Runtime layout: CPU vLLM frontend, `vllm-vcr` simulator, supervisor-owned
  readiness, coordinated shutdown.
- Manifests (kustomize), OpenShift install path (SCC, SELinux, certificates),
  envtest and kind acceptance suites, spike harnesses, and the contracts and
  policy documents.
