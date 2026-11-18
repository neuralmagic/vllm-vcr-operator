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

The frontend keeps serving `/v1/models` on the inference port for llm-d discovery,
but the Pod's own readiness probe is the supervisor's `/readyz` (see below): the
inference endpoint alone cannot observe a lost simulator, so probing it would keep an
engine-less server Ready.

## Supervision and failure propagation

`vcr-supervisor` runs the frontend as its only long-lived child because the
simulator is supervised by kubelet in its own container.

### What readiness means

Readiness requires all three inputs, and any one of them failing withdraws it:

1. the frontend child process is alive;
2. the frontend's own HTTP health endpoint on the inference port answers `200`
   (in external-engine mode the vLLM server does not serve before the engine
   handshake completes);
3. the simulator's control API (`GET /stats` on `names.ControlPort`) answers.

Dialing the handshake port is **not** a readiness signal. The frontend binds that
socket, so a TCP connection succeeds while no engine is registered; using it would
report ready for an engine-less server. This is a verified property of the
`vllm-vcr`/vLLM frontend pairing, not an assumption.

The supervisor exposes `/healthz` (frontend child alive) and `/readyz` (the three
inputs above) on `--health-address`, and writes the ready file when `/readyz`
becomes true. It:

- withdraws readiness immediately when either HTTP input fails, so a Pod is never
  ready with an unavailable backend;
- terminates the frontend when the backend stays unavailable past
  `--backend-loss-grace` rather than leaving a healthy-looking engine-less server;
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
| `VCR_HANDSHAKE_ADDRESS` | Handshake endpoint, default `tcp://127.0.0.1:29550` (supervisor passes it to the frontend; it is not a readiness signal). |
| `VCR_FRONTEND_HEALTH_URL` | Frontend health URL, default `http://127.0.0.1:8000/health`. |
| `VCR_BACKEND_HEALTH_URL` | Simulator control URL, default `http://127.0.0.1:8001/stats`. |
| `VCR_PROFILE_DIR` | Exported profile directory, default `/var/run/vcr/profile`. |
| `VCR_SHUTDOWN_TIMEOUT` | Graceful shutdown budget, seconds, from the workload's vLLM setting. |
| `VCR_READY_FILE` | File the supervisor writes once the engine is registered. |
| `VLLM_*` | Preserved workload settings. GPU-only variables are removed or translated. |

The supervisor writes `/tmp/vcr-runtime/ready` (or `VCR_READY_FILE`). The operator
probes the frontend container through the supervisor: liveness is `GET /healthz` and
readiness is `GET /readyz` on the supervisor's health port (`8082`), which the
supervisor binds on every interface because kubelet probes the Pod's IP rather than
its loopback. The ready file is what incident diagnostics use.
