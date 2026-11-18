# Test images

Images used only by the kind-cluster acceptance suite (`test/cluster`). They are
built by `hack/images.sh --stub` and loaded with `hack/images.sh --load`.

**They are test doubles. Nothing in `internal/`, `config/`, or the Dockerfiles at the
repository root references them, and no operator default may point at them.** The real
frontend is `Dockerfile.supervisor --target frontend`; the real simulator is the
`vllm-vcr` image; the real profile is published by the profile repository.

| Image | Contains | Stands in for |
| --- | --- | --- |
| `docker.io/vcrlocal/vllm-vcr-frontend-stub:test` | the **real** `vcr-supervisor` (`cmd/supervisor`) and a stub `vllm` program | the Python vLLM frontend in external-engine mode |
| `docker.io/vcrlocal/vllm-vcr-backend-stub:test` | a stub `vllm-vcr` program | the `vllm-vcr play` simulator |
| `docker.io/vcrlocal/vllm-vcr-profile-fixture:test` | the **real** `vcr-profile-export` (`cmd/profileexport`) at `/usr/local/bin/vcr-profile-export`, which its metadata declares as `provisioning.entrypoint`, and an OCI profile filesystem (`/profile/metadata.json`, `/profile/trace.jsonl.gz`) | a published recorded profile |

The supervisor is *not* a double: the suite asserts its real readiness combination,
its backend-loss grace period, its termination of the frontend, and its refusal to
restart the simulator. That is the part of the runtime contract worth exercising here.

## What the doubles reproduce

`frontend-stub` (`test/images/frontend-stub/main.go`):

- parses the invocation the operator generates (`vllm serve <model> --host --port
  --data-parallel-rpc-port ...`) and binds the engine-core handshake port;
- answers `/health`, `/v1/models`, `/v1/completions`, and `/v1/chat/completions`
  **only** while a backend holds a handshake connection, and `503` otherwise — so a
  readiness signal that probed only the frontend would report an engine-less server as
  ready, which is the property `docs/contracts/runtime-layout.md` warns about;
- streams `/v1/chat/completions` as server-sent events with three placeholder tokens and
  `[DONE]`.

`backend-stub` (`test/images/backend-stub/main.go`):

- connects to the engine-core handshake it is given and keeps the connection open
  (this is the registration the frontend and supervisor observe);
- serves the control API the supervisor probes (`GET /stats`) on the control address the
  operator passes, and listens on the KV-event endpoint with placeholder events over
  plain TCP (**not ZMQ**: the port wiring is what is asserted, not protocol
  compatibility);
- reads the `--latency-trace` file the operator points at, which makes provisioning into
  the shared profile volume observable end to end;
- exposes test hooks on its control API, which no other code path uses:
  `/stub/unavailable` (alive but not serving: `/stats` answers 503 and the handshake is
  dropped), `/stub/available`, `/stub/exit` (exit non-zero, for kubelet's restart
  policy).

Neither double applies recorded timing or generates model output. **No test in this
repository may use them to claim serving behavior.**

## Profile fixture

`profile-fixture/profile/metadata.json` is a **test double**, not a recording. It is
named `stub-qwen3-0.6b-h200-tp1-bf16` so it can never be mistaken for a release, and its
documented shape is H200 / TP=1 / Qwen3-0.6B / 2048 KV blocks of 64 tokens, which the
suite asserts survives into `status.profiles` and into the Pod's profile volume.

Two fields are set so that the *layout* can be exercised and are explicitly not
evidence of anything recorded:

- `trace.latencyModel: true` and `trace.kvEvents: true` — the operator refuses a
  workload that configures KV events unless the profile records them, so the suite
  needs the declaration to test the publisher wiring; the trace file contains a `meta`
  line and two synthetic timing records.
- `capabilities.chatCompletions/completions/streaming: true` — the stub frontend serves
  those endpoints; nothing about model output is claimed.

The fixture runs the real `vcr-profile-export` binary with no wrapper: the operator
appends the profile volume path as its only argument, and the exporter stages its export
inside that target directory — the only writable path in the provisioner container, which
runs with a read-only root filesystem. The suite asserts the provisioner init container
completes, that the volume holds `metadata.json` plus the trace, and that no staging
directory is left behind.

## Building

```bash
hack/images.sh --stub                 # build the three images
hack/images.sh --stub --load          # build and import them into kind nodes
hack/images.sh --list                 # print every managed reference
```

The build context is the repository root, so each Dockerfile can build the real Go
binaries with the module's own toolchain. `hack/images.sh` uses Docker when its daemon
answers and rootless podman otherwise.
