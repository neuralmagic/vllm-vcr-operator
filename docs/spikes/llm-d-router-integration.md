# llm-d-router integration (verified)

llm-d's own router served traffic to model servers simulated by this operator, on
OpenShift, with no GPUs. Nothing in the router or the workload was replaced: the
EPP is `ghcr.io/llm-d/llm-d-router-endpoint-picker` rendered from the repository's
`llm-d-router-standalone` chart, and the model servers are the unchanged GPU-
requesting fixture admitted by the operator.

## Topology

- Two simulated modelservers (unchanged `nvidia.com/gpu: 1` workload, KV events
  variant) admitted into one namespace and pinned to one profile digest.
- EPP with `precise-prefix-cache-producer` (topic filter `kv@`, pod discovery on
  port 5556), `prefix-cache-scorer`, and a token producer backed by the chart's
  `vllm launch render` sidecar.
- Envoy sidecar in front, fail-open disabled, service port 8081.

## What was observed

1. **Discovery.** EPP enumerated both simulated modelservers from the
   InferencePool and created a ZMQ subscriber per pod:
   `Subscriber created and started … endpoint tcp://10.128.2.58:5556`,
   `… tcp://10.128.2.59:5556`.
2. **Serving through the router.** A `POST /v1/chat/completions` sent to the Envoy
   service returned a completion produced by the simulator behind the selected
   pod.
3. **Prefix locality.** With two healthy endpoints in the pool and per-pod
   `vllm:request_success_total` measured around each step:

   | Step | `4wt7f` | `ngqbk` |
   | --- | --- | --- |
   | cold request, prefix α | +1 | — |
   | 4 more with α | +4 | — |
   | cold request, prefix β | — | +1 |
   | 4 more with β | — | +4 |

   A new prefix lands on the pod that has no cached prefix; repeats stick to the
   pod that does. Both endpoints stayed Ready and in the pool throughout (EPP
   logged both endpoint identities), so this is routing behaviour, not a dropped
   endpoint.

## What this required from the operator

- Preserved the workload's `modelserver` port and llm-d labels, so the
  InferencePool selector and PodMonitor-style discovery keep working.
- Translated `--kv-events-config` into the simulator's publisher, and published
  under `kv@<pod-ip>:<serving-port>@<model>`, the identity llm-d discovers engines
  by.
- Kept the Pod Ready only after the frontend and simulator both answered.

## Not claimed

- P/D disaggregation: the operator rejects pipeline parallelism, distributed
  workers, P/D roles, and KV-transfer settings until a profile's capability set
  grants them, so those llm-d cases are refused at admission by design.
- Timing/throughput fidelity: the profile used here is a mock.
- Recorded-token replay and KV-event replay.
- llm-d's own e2e harness: its model-server artifacts are simulator-shaped
  (port 8200, `--render-url`, no GPU request), so they do not route through this
  operator's admission. The integration above uses the operator-admitted workload
  with the repository's real chart and images.
