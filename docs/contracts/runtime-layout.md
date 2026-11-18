# Runtime layout contract

Status: binding contract between `internal/runtime`, `cmd/vcr-supervisor`, and the
runtime acceptance tests.

## Containers

One Pod runs one simulated model server. Three processes are involved:

| Process | Container | Role |
| --- | --- | --- |
| Profile provisioner | init container `vcr-profile-provisioner` | Copies metadata and trace from the pinned profile image into the shared profile volume, then exits 0. |
| `vllm-vcr play` | `vllm-vcr-backend` | Simulated engine. Connects to the frontend's engine-core ZMQ handshake, applies recorded timing, emits tokens and KV events. |
| Python vLLM frontend | `vllm-frontend` | Real frontend: tokenization, rendering, generation loop, streaming, metrics, HTTP on the original inference port. Runs with no local engine rank and connects to the simulator. |

The frontend container is PID 1's child through `vcr-supervisor`, which is the
container entrypoint. The simulator runs in its own container so that its
resource sizing, restart policy, and log stream are independent.

## Ordering

1. The provisioner runs to completion. A failure keeps the Pod out of the ready
   state: the frontend starts only after the profile volume contains the metadata
   and trace files.
2. The frontend binds the engine-core handshake port (`29550`) and waits for the
   simulator to register.
3. The simulator connects to `tcp://127.0.0.1:29550` and registers the engine.
4. The frontend becomes ready only after the engine registration handshake
   completes. Readiness is served by the frontend's own `/health` endpoint on the
   inference port; the frontend does not report ready while the engine is absent.

The frontend's `/v1/models` endpoint is used as the readiness probe for workload
compatibility (llm-d discovery expects it), while engine registration gates the
underlying health.

## Supervision and failure propagation

`vcr-supervisor` runs the frontend as its only long-lived child because the
simulator is supervised by kubelet in its own container. The supervisor:

- reports readiness by exposing a local health endpoint that combines frontend
  process liveness with engine-registration state, so a Pod is never ready with an
  unavailable backend;
- terminates the frontend when the backend disappears (the frontend's engine RPC
  channel closes) rather than leaving a healthy-looking but engine-less server;
- forwards `SIGTERM` to the frontend, waits for its graceful shutdown within the
  configured shutdown timeout, then exits, so draining tests observe controlled
  termination;
- exits non-zero when the frontend dies unexpectedly, so kubelet restarts it.

Both containers share the Pod network namespace, so the handshake is loopback and
no shared socket volume is needed. Restart behavior is restart-based, not
reconnect-based: independently restarted processes are not assumed to re-establish
the engine-core session. The spike harness in `test/spike` is the executable
evidence for this claim and records whichever behavior the pinned images exhibit.

## Environment

| Variable | Purpose |
| --- | --- |
| `VCR_HANDSHAKE_ADDRESS` | Handshake endpoint, default `tcp://127.0.0.1:29550`. |
| `VCR_PROFILE_DIR` | Exported profile directory, default `/var/run/vcr/profile`. |
| `VCR_SHUTDOWN_TIMEOUT` | Graceful shutdown budget, seconds, from the workload's vLLM setting. |
| `VCR_READY_FILE` | File the supervisor writes once the engine is registered. |
| `VLLM_*` | Preserved workload settings. GPU-only variables are removed or translated. |

The supervisor writes `/tmp/vcr-runtime/ready` (or `VCR_READY_FILE`) and the
frontend's liveness/readiness probes are configured by the operator against the
inference port; the supervisor's readiness file is what incident diagnostics use.
