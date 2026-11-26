# Examples

Small, copy-pasteable walkthroughs. They exist to show what the operator's
interfaces look like; the assertions live in `test/e2e` (`make test-e2e`) and
`test/cluster` (`make test-cluster`).

| File | What it shows |
| --- | --- |
| `vllm-smoke-pod.yaml` | A directly submitted GPU-requesting Pod. Same admission path as a Deployment, with no controller to recreate it. |
| `completions.sh` | Port-forward a simulated modelserver and drive `/v1/models`, `/v1/completions`, the streaming API, and `/metrics` over the original inference port. |

Both rely on the fixtures in `test/fixtures/`: the namespace
`vllm-vcr-simulated`, the ServiceAccount `mock-pc-sa`, and a
`VcrGpuCluster` with a real profile image.

## Walkthrough

```bash
# 1. Install the operator (see config/README.md for the certificate paths).
hack/install.sh

# 2. Label the node that should simulate GPUs.
kubectl label node <node> vcr.opendatahub-io/simulation=true

# 3. Configure the environment. Replace the placeholder profile image with a real
#    digest-pinned one: docs/profile-contract.md explains how profiles are built
#    and published.
kubectl apply -f test/fixtures/environments/h200.yaml
kubectl wait --for=condition=Ready vgc/default --timeout=5m

# 4. Apply an unchanged GPU-requesting workload.
kubectl apply -k test/fixtures/llmd
kubectl -n vllm-vcr-simulated rollout status deploy/mock-prefix-cache --timeout=10m

# 5. See what admission did.
kubectl -n vllm-vcr-simulated get pod -l llm-d.ai/guide=precise-prefix-cache-routing \
  -o jsonpath='{range .items[0]}{.metadata.annotations}{"\n"}{range .spec.containers[*]}{.name}{" "}{end}{"\n"}{end}'

# 6. Drive it.
examples/completions.sh vllm-vcr-simulated deploy/mock-prefix-cache
```

Step 5 shows the provenance annotations and the injected container names:
`vllm-frontend` (supervisor plus CPU vLLM frontend) and `vllm-vcr-backend` (the
simulator), with the original `nvidia.com/gpu` quantity still on the workload's
container.

Step 6 prints what a client sees. Two responses deserve a warning:

- generated text is recorded output or the simulator's seeded random tokens: it
  is never real model output, so do not assert on its content
  (`docs/capability-matrix.md`);
- streaming is real SSE from the real frontend, so chunk counts and `[DONE]` are
  meaningful, while token boundaries depend on the profile's recorded output.

## A directly submitted Pod

```bash
kubectl apply -f examples/vllm-smoke-pod.yaml
kubectl -n vllm-vcr-simulated get pod vllm-smoke -w
examples/completions.sh vllm-vcr-simulated pod/vllm-smoke
```

The Pod is admitted exactly like the Deployment's Pods. It has
`restartPolicy: Never`, so it does not restart after deletion — useful when you
want one admission and one lifecycle, not a controller's reconciliation loop.

## What these examples deliberately do not do

- They do not add simulator labels, annotations, or sidecars: admission detects
  the GPU request and nothing else.
- They do not assert: a walkthrough that fails prints the response, which is what
  a human debugging wants. The suites assert.
- They do not install llm-d. The router path is described in
  `test/fixtures/README.md`.
