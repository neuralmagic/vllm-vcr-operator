# Integration fixtures

The manifests the acceptance paths use, and the invariants that make them
fixtures rather than examples.

| Path | What it is | Used by |
| --- | --- | --- |
| `llmd/mock-prefix-cache-deployment.yaml` | An unchanged, GPU-requesting llm-d modelserver. llm-d labels, ports and probes preserved; `nvidia.com/gpu` requested; no simulator opt-in. | `make test-e2e`, `make test-cluster`, `examples/` |
| `llmd/mock-prefix-cache-kv-events-deployment.yaml` | The same workload plus `--kv-events-config`, for prefix-cache routing. | The llm-d integration path (`make test-cluster`) |
| `llmd/namespace.yaml`, `llmd/serviceaccount.yaml` | The workload namespace and ServiceAccount the fixtures name. | `kubectl apply -k test/fixtures/llmd` |
| `llmd/kustomization.yaml` | Namespace, ServiceAccount, and the base fixture. Apply the kv-events variant *instead of* the base. | — |
| `environments/h200.yaml` | The singleton `SimulationEnvironment` with a digest-pinned placeholder profile image. | `make test-e2e`, `make test-cluster` |
| `router/precise-prefix-cache-routing.values.yaml` | Reference values for llm-d's precise-prefix-cache-routing guide. | Human reference for the integration path |
| `profiles/qwen3-0.6b-h200-tp1-bf16/` | A **placeholder** profile image filesystem: metadata and a trace file, claiming no capability. | `test/fixtures` (contract checks only) |

## Why the modelserver fixture looks the way it does

The prior art file (`vllm-vcr`'s
`deploy/llm-d-prefix-cache/modelserver/deployment.yaml`) is a GPU-free mock: it
runs the `vllm-vcr` image's bash entrypoint with `MOCK_*` variables and requests
no GPU. That workload cannot exercise this operator, which is triggered by a GPU
request and then *replaces* the engine container.

The fixture therefore keeps everything that identifies the llm-d workload and
changes only what makes it a real GPU workload:

| Kept unchanged | Changed, with reason |
| --- | --- |
| `llm-d.ai/guide`, `llm-d.ai/model`, `llm-d.ai/inference-serving` labels | `image`: `vllm/vllm-openai:v0.33.0` instead of the stale `vllm-vcr:vllm0.23` mock |
| `modelserver` container name, ports `8000` and `5556` | `command`/`args`: a real `vllm serve` invocation instead of the mock entrypoint |
| liveness `/health`, readiness `/v1/models`, startup `/v1/models` probes and their timings | `MOCK_*` environment: replaced by `POD_NAME`, `POD_IP`, `HF_HOME`, `HF_HUB_DISABLE_XET` |
| ServiceAccount `mock-pc-sa`, three replicas, qwen3-0.6b model | `resources`: adds `nvidia.com/gpu: "1"` requests and limits, which is the whole point |
| `llm-d.ai/inference-serving: "true"` discovery label | — |

The result contains no simulator label, annotation, or container: admission has to
detect the GPU request. `TestModelserverFixturesStayUnmodifiedGPUWorkloads`
enforces that.

## Applying the fixtures

```bash
kubectl apply -k test/fixtures/llmd                 # namespace, ServiceAccount, base modelserver
kubectl apply -f test/fixtures/environments/h200.yaml
```

The environment's profile image is a placeholder: see the comment in the file and
`docs/profile-contract.md`. Nothing in this repository publishes a profile image,
so an acceptance run replaces that reference with a real digest-pinned one.

## The llm-d integration path, end to end

`make test-cluster` is the suite that asserts this path; the steps below are what
it exercises, so a reader can reproduce them by hand.

1. **Label the simulation nodes.** `kubectl label node <node>
   vcr.opendatahub-io/simulation=true`. The label must match
   `spec.simulationNodes.nodeSelector`. Only matching nodes get the synthetic
   device plugin.
2. **Apply the environment.** The reconciler resolves each profile image's
   metadata label into `status.profiles` and renders the device-plugin DaemonSet.
   `kubectl get sime default -o jsonpath='{.status.conditions}'` shows
   `EnvironmentReady`; `kubectl -n vllm-vcr-operator-system get ds` shows
   `vllm-vcr-device-plugin` ready on every selected node.
3. **Apply the modelserver fixture.** The webhook translates the Pods: the GPU
   request stays, the engine container becomes `vllm-frontend` running
   `vcr-supervisor` plus the CPU vLLM frontend, `vllm-vcr-backend` runs
   `vllm-vcr play`, and `vcr-profile-provisioner` exports the profile first.
   `kubectl get pods -o jsonpath='{.items[0].metadata.annotations}'` shows the
   provenance annotations (`vcr.opendatahub-io/profile`,
   `vcr.opendatahub-io/profile-image`, `vcr.opendatahub-io/model`,
   `vcr.opendatahub-io/original-gpus`, `vcr.opendatahub-io/vllm-version`,
   `vcr.opendatahub-io/injected`).
4. **Install llm-d's precise-prefix-cache-routing guide** with the values in
   `router/`. The EPP matches `llm-d.ai/guide=precise-prefix-cache-routing`,
   dials each pod on port `5556`, filters topics by `kv@`, and hashes prompts at
   `blockSize: 64`.
5. **Send traffic through the router** and assert cache-local routing: repeat a
   long shared prefix so one Pod stores the prefix chain, then observe that the
   EPP prefers that Pod (`vllm:prefix_cache_*` and
   `vllm:kv_cache_usage_perc` move on the selected Pod, not on its peers).

What this path can **not** claim yet:

- **KV-event replay.** The simulator publishes live events and its ready response
  always reports `replay_endpoint: null`; there is no replay responder. An EPP
  restart, or a missed event window, cannot be re-synchronized from the engine, so
  recovery tests are out of scope. The operator rejects an explicit
  `replay_endpoint` unless a profile grants `capabilities.kvEventReplay`.
- **Real GPU behavior, model quality, or timing fidelity.** Outputs are recorded
  tokens or the simulator's seeded random tokens; latency is the profile's
  recorded latency.
- **Block-size agreement is a configuration, not a check.** If
  `tokenProcessorConfig.blockSize` differs from the profile's `cache.blockSize`,
  the router's block hashes disagree with the publisher's and prefix matches stop
  happening — with no error anywhere. `TestRouterValuesAgreeWithTheFixtures` keeps
  the checked-in values consistent; a deployment that edits one value must edit
  the other.

## Fixture invariants

`test/fixtures/fixtures_test.go` enforces:

| Check | Why |
| --- | --- |
| Every manifest parses as YAML; manifest files carry `apiVersion` and `kind` | A fixture that does not parse is not evidence |
| The environment decodes into `v1alpha1.SimulationEnvironment`, is named `default`, has a digest-pinned profile, and its runtime sizing does not request `nvidia.com/gpu` | Only the workload's container may hold the synthetic allocation |
| The modelserver fixtures request `nvidia.com/gpu`, keep llm-d labels/ports/probes, and carry no `vcr.opendatahub-io/*` label or annotation | Admission must trigger on the GPU request alone |
| The placeholder profile parses under `internal/profile` and claims no recorded tokens, no latency model, no KV events, no capability | A fixture that claimed a capability would authorize serving features no recording supports |
| `vcr-profile-export` accepts the placeholder profile and writes both documents | The provisioning contract is checked without images |
| Router values agree with the fixtures on label, ports, and block size | A silent disagreement makes prefix-cache locality never happen |
| Every relative Markdown link in this repository resolves | Documentation cannot point at pages that do not exist |
