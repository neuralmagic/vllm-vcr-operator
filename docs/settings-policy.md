# Settings policy

Status: reference for `internal/translator` and `internal/runtime`. The table below
is generated from the policy table in `internal/translator/policy.go`; that table is
the implementation, and this document is its data reference plus the rationale.

Related contracts: `docs/contracts/admission.md` (admission behavior and provenance),
`docs/contracts/runtime-layout.md` (container layout), `docs/contracts/profile-image.md`
(profile metadata). The specification's "Settings policy" section is the source of the
families listed here.

## How the policy is applied

A workload is translated in four steps, and each step can only reject more:

1. **Resolve.** `internal/vllm` reports the workload's effective vLLM 0.33.0
   configuration and, for every setting, whether the *workload* set it (CLI, config
   file, or environment) or the value is a vLLM default. Defaults never trigger a
   policy decision: a resolved default is not a workload request.
2. **Apply the table.** Every setting the workload set is looked up here. A setting
   with no row is rejected as an unknown behavior-affecting setting. Every row yields
   one disposition, and dispositions that accept may forward the setting to the
   frontend, translate it into the simulator invocation, or add a plan warning.
3. **Select exactly one profile.** Explicit settings become profile requirements
   (model, revision, tokenizer, chat template, GPU type and count, tensor parallel
   size, dtype, quantization, block size, prefix caching). Zero or several compatible
   profiles is a rejection.
4. **Validate combinations.** Coverage ranges (`coverage.*`), the profile's
   capability set, chunking/batching consistency, cache/event consistency, and the
   engine topology are checked as combinations, not as individual flags.

Rejection is the default outcome. Every rejection names the setting and the reason,
and never contains a credential value.

## Dispositions

| Disposition | Meaning |
| --- | --- |
| `preserve` | Forwarded to the operator-provided Python frontend, which is the only process that implements it. |
| `translate` | Reproduced in another form: a simulator flag, a profile requirement, or a deliberate no-op that changes no observable behavior and carries a plan warning. |
| `require-profile` | Accepted only when it agrees with what the profile recorded (topology, precision, cache representation). |
| `require-capability` | Accepted only when the profile's validated capability set grants the capability it selects. |
| `reject` | Refused. A row that also carries a value check accepts only the values that check authorizes. |

## Documented resolution decisions

Some settings need a value even when the workload did not set one, and the profile
metadata does not always record one. The rules are fixed so that admission is
deterministic and never invents support:

- **Workload value wins.** If the workload set the setting and the profile's coverage
  authorizes it, the workload's value is used verbatim.
- **Coverage maximum otherwise.** When the workload did not set a batching limit
  (`--max-num-seqs`, `--max-num-batched-tokens`, long-prefill threshold) the
  profile's declared coverage maximum is used: it is the largest value the recording
  supports. If the profile declares no coverage for it, the simulator's built-in
  default applies and the plan records a warning naming the setting, because the
  profile does not vouch for that default.
- **Maximum model length is bounded.** `--max-model-len` resolves to the workload's
  value when it is inside coverage, to `coverage.maxModelLen.max` when the workload
  left it unset or asked for `-1`/`auto`, and is rejected when the workload asks for
  more than the profile can serve or when the profile declares no bound at all. A
  model length the operator cannot bound would make the frontend and the simulator
  advertise a length no recording supports.
- **Cache representation comes from the profile.** Block size, KV block count, prefix
  caching, and chunked prefill are recorded properties of the profile. A workload
  that asks for a different value is rejected rather than approximated; a workload
  that sets nothing inherits the recorded value.
- **Scheduling policy.** FCFS is used when the profile's coverage authorizes it (it is
  vLLM's default). Otherwise nothing is passed, the simulator's default stands, and
  the plan records a warning.
- **KV capacity.** `--num-gpu-blocks-override` maps directly onto the simulator's
  `--kv-cache-size` inside `coverage.kvBlocks`. A memory fraction
  (`--gpu-memory-utilization`) or a byte budget (`--kv-cache-memory-bytes`) is
  accepted only for a profile that declares `coverage.kvMemoryConversion`, because
  capacity is never inferred from a percentage alone; the simulated capacity is then
  the profile's recorded `cache.kvBlocks`, which is what its validated conversion
  produced.
- **Timing.** Timing always comes from the profile's latency trace. A profile whose
  metadata records no latency model is rejected: serving on synthetic timing would
  misrepresent the recorded hardware.
- **Content replay.** `--replay-tokens` and `--replay-match prefix` are used only
  when the profile grants `capabilities.contentReplay` (which requires recorded token
  ids). Otherwise responses carry the simulator's seeded random tokens, which is valid
  for protocol and routing checks and never for content assertions, and the plan says
  so. Prefix matching is the only match mode that works for live clients, because the
  replay harness's request ids do not appear on the wire.
- **Log levels have two owners.** The frontend keeps the workload's vLLM
  `--log-level`; the simulator log filter (`RUST_LOG`) comes from the environment's
  `logLevel`, because the two use different syntaxes.
- **API keys stay out of the plan.** A key passed with `--api-key` is copied verbatim
  from the workload container at mutation time, so the key never enters a plan,
  annotation, status, log line, or event. A key supplied through the config file
  cannot be re-emitted without retaining its value and is rejected with that reason; a
  key supplied through `VLLM_API_KEY` is preserved as the workload's own environment
  variable.

## Combination checks

Individual authorizations are not combination authorizations. The translator
additionally enforces:

| Combination | Outcome |
| --- | --- |
| `--max-num-seqs`, `--max-num-batched-tokens`, `--max-model-len`, `--long-prefill-token-threshold`, `--num-gpu-blocks-override` outside the profile's coverage | Rejected, with every violated range reported |
| `--max-num-batched-tokens` smaller than the effective model length while the profile records chunked prefill disabled | Rejected: one prompt could never be scheduled in a single step |
| Chunked prefill or prefix caching changed relative to the profile's recording | Rejected: the recorded timing and cache representation cannot serve the other mode |
| KV block events requested while prefix caching is disabled | Accepted only when the profile records publishing events with caching disabled |
| KV block events requested from a profile with `trace.kvEvents` false | Rejected |
| `--tensor-parallel-size` different from the requested device count, or from the recorded topology | Rejected |
| Requested devices above `spec.devicesPerNode` | Rejected: the request cannot be scheduled on a simulation node |
| Ports colliding inside the Pod network namespace (inference, handshake, control API, KV events) | Rejected |
| `coverage.kvMemoryConversion` absent while a memory fraction or byte budget is set | Rejected |
| Profile capability required by a selection (`lora`, `speculativeDecoding`, `multimodal`, `embeddings`, `structuredOutputs`, `disaggregatedPrefill`, `kvEventReplay`, specific tool/reasoning parsers) not granted | Rejected, naming the setting that selected the capability |

## The table

Each row is one vLLM 0.33.0 setting. Environment-backed settings appear under the
variable name vLLM reads.

### Model identity/revision

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--model` | preserve | preserved as the `vllm serve` positional argument of the frontend |
| `model` | preserve | preserved as the `vllm serve` positional argument of the frontend |
| `--revision` | preserve | forwarded to the frontend unchanged |
| `--code-revision` | preserve | forwarded to the frontend unchanged |
| `--hf-config-path` | preserve | forwarded to the frontend unchanged |
| `--trust-remote-code` | preserve | forwarded to the frontend unchanged |
| `--runner` | preserve | the model runner is not executed: outputs come from the recorded profile or the simulator's token model |
| `--convert` | preserve | forwarded to the frontend unchanged |
| `--config-format` | preserve | forwarded to the frontend unchanged |
| `--load-format` | translate | weights are never loaded by the frontend or the simulator, so the load format has no effect |
| `--hf-overrides` | preserve | forwarded to the frontend unchanged |

### Tokenizer identity/revision and chat template

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--tokenizer` | preserve | forwarded to the frontend unchanged |
| `--tokenizer-revision` | preserve | forwarded to the frontend unchanged |
| `--tokenizer-mode` | preserve | forwarded to the frontend unchanged |
| `--skip-tokenizer-init` | reject | the frontend renders prompts and streams tokens, so it must load the tokenizer: a skipped tokenizer cannot serve requests |
| `--chat-template` | preserve | preserved as --chat-template, either the container path (the mount is preserved) or the inline template |
| `--default-chat-template-kwargs` | preserve | forwarded to the frontend unchanged |

### Served-model aliases

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--served-model-name` | preserve | forwarded to the frontend unchanged |

### Host, inference port, API authentication, TLS

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--host` | preserve | the frontend builder always emits --host from the effective value, so no duplicate flag is produced |
| `--port` | preserve | the frontend builder always emits --port from the effective value, so no duplicate flag is produced |
| `--uds` | preserve | forwarded to the frontend unchanged |
| `--grpc-port` | preserve | forwarded to the frontend unchanged |
| `--headless` | preserve | a headless frontend serves no HTTP API: probes and llm-d discovery cannot pass without one |
| `--api-server-count` | preserve | forwarded to the frontend unchanged |
| `--uvicorn-log-level` | preserve | forwarded to the frontend unchanged |
| `--disable-uvicorn-access-log` | preserve | forwarded to the frontend unchanged |
| `--api-key` | preserve | preserved by copying the workload's own argument verbatim at mutation time, so the key never enters the plan |
| `VLLM_API_KEY` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged |
| `--ssl-keyfile` | preserve | forwarded to the frontend unchanged |
| `--ssl-certfile` | preserve | forwarded to the frontend unchanged |
| `--ssl-ca-certs` | preserve | forwarded to the frontend unchanged |
| `--ssl-cert-reqs` | preserve | forwarded to the frontend unchanged |

### Tool/reasoning parsers and frontend formatting

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--enable-auto-tool-choice` | preserve | forwarded to the frontend unchanged |
| `--tool-call-parser` | require-capability | forwarded to the frontend unchanged; capability `toolCallParsers` |
| `--tool-parser-plugin` | reject | a tool-parser plugin is code that must exist in the operator-provided frontend image; arbitrary plugin modules are not loaded |
| `--reasoning-parser` | require-capability | forwarded to the frontend unchanged; capability `reasoningParsers` |
| `--reasoning-parser-plugin` | reject | a reasoning-parser plugin is code that must exist in the operator-provided frontend image; arbitrary plugin modules are not loaded |
| `--enable-prompt-tokens-details` | preserve | forwarded to the frontend unchanged |
| `--return-tokens-as-token-ids` | preserve | forwarded to the frontend unchanged |
| `--stream-interval` | preserve | forwarded to the frontend unchanged |

### Generation defaults and sampling

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--generation-config` | preserve | forwarded to the frontend unchanged |
| `--override-generation-config` | preserve | forwarded to the frontend unchanged |
| `--seed` | translate | translated to the simulator's --seed, which makes random tokens and failure injection reproducible |
| `--max-logprobs` | preserve | the simulator produces no per-token logprobs, so requests asking for them observe empty logprob entries |
| `--logprobs-mode` | preserve | forwarded to the frontend unchanged |
| `--use-fp64-gumbel` | translate | sampling is not performed: outputs are recorded token ids or the simulator's seeded token model |
| `--return-sampling-mask` | translate | sampling is not performed, so no sampling mask is produced |

### Tensor parallelism, precision, quantization

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--tensor-parallel-size` | require-profile | translated to a profile requirement: the recorded topology must match the requested device quantity, and no GPU computation runs |
| `--dtype` | require-profile | translated to a profile requirement: the recorded precision must match, and no weights are loaded |
| `--quantization` | require-profile | translated to a profile requirement: the recorded quantization must match, and no quantized kernels run |
| `--quantization-config` | reject | quantization details are not modelled: the profile's recorded quantization is the only supported precision, so an inline quantization configuration is rejected |
| `--allow-deprecated-quantization` | reject | deprecated quantization schemes require real quantized kernels, which are not executed |
| `--enable-expert-parallel` | reject | expert parallelism needs a multi-device MoE engine; the simulated engine is a single-process recorder |
| `--kv-cache-dtype` | reject | the simulated KV cache has no dtype: recorded cache capacity comes from the profile, so a KV-cache dtype cannot be applied |

### Pipeline parallelism, distributed workers, remote engine addresses

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--pipeline-parallel-size` | reject | pipeline parallelism greater than one is rejected pending validated translation: the simulated engine models one stage |
| `--distributed-executor-backend` | reject | only the external-engine mode (external_launcher) is supported: ray, mp and uni worker backends would start a real engine |
| `--data-parallel-size` | reject | data parallelism greater than one needs several engines; one Pod runs one simulated model server |
| `--data-parallel-size-local` | reject | a local engine rank is rejected: the operator runs the frontend with no local engine and connects the simulator instead |
| `--data-parallel-address` | reject | an explicit engine address selects a remote or user-managed engine, which is rejected pending validated translation |
| `--data-parallel-rpc-port` | translate | translated to the engine-core handshake port the frontend binds and the simulator connects to |
| `--data-parallel-backend` | reject | the data-parallel backend selects a distributed transport, which the simulated runtime does not implement |
| `--data-parallel-start-rank` | reject | a data-parallel start rank configures a multi-process group, which the simulated runtime does not implement |
| `--data-parallel-rank` | reject | a data-parallel rank configures a distributed group, which the simulated runtime does not implement |
| `VLLM_DP_RANK` | reject | a data-parallel rank configures a distributed group, which the simulated runtime does not implement |
| `VLLM_DP_RANK_LOCAL` | reject | a local data-parallel rank starts an in-Pod engine group, which the simulated runtime does not implement |
| `VLLM_DP_SIZE` | reject | a data-parallel size greater than one needs several engines; one Pod runs one simulated model server |
| `VLLM_DP_MASTER_IP` | reject | a data-parallel master address selects a remote engine, which is rejected pending validated translation |
| `VLLM_DP_MASTER_PORT` | reject | a data-parallel master port selects a remote engine, which is rejected pending validated translation |
| `--nnodes` | reject | multi-node execution is rejected: one Pod runs one simulated model server on one node |
| `--node-rank` | reject | a multi-node rank configures torch-distributed execution, which the simulated runtime does not implement |
| `--master-addr` | reject | a torch-distributed master address selects a multi-node group, which the simulated runtime does not implement |
| `--master-port` | reject | a torch-distributed master port selects a multi-node group, which the simulated runtime does not implement |
| `VLLM_WORKER_MULTIPROC_METHOD` | reject | a worker multiprocessing method configures real engine worker processes, which are not started |

### GPU memory utilization, KV-memory allocation, cache-block overrides

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--gpu-memory-utilization` | require-profile | a memory fraction is accepted only when the profile declares coverage.kvMemoryConversion: cache capacity is never inferred from a percentage alone |
| `--kv-cache-memory-bytes` | require-profile | an explicit KV-cache byte budget is accepted only when the profile declares coverage.kvMemoryConversion, which carries the validated conversion to simulated capacity |
| `--num-gpu-blocks-override` | translate | translated to the simulator's --kv-cache-size within the profile's coverage.kvBlocks |
| `--cpu-offload-gb` | reject | CPU offload of model weights requires real weight loading and host memory accounting, which the simulated runtime does not perform |
| `VLLM_CPU_KVCACHE_SPACE` | reject | a CPU KV-cache space selects the CPU target device; the simulated KV capacity comes from the profile |

### Maximum model length

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--max-model-len` | translate | enforced by the frontend and the simulator: a lower cap inside coverage is translated, an increase beyond it is rejected |
| `VLLM_ALLOW_LONG_MAX_MODEL_LEN` | reject | permitting model lengths beyond the model maximum would bypass the recorded coverage bound, which this policy rejects |

### Maximum running sequences

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--max-num-seqs` | translate | translated to the simulator's sequence admission limit within the profile's coverage.maxNumSeqs |

### Per-step token budget

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--max-num-batched-tokens` | translate | translated to the simulator's per-step token budget within the profile's coverage.maxNumBatchedTokens |
| `--max-num-active-seqs` | reject | an active-sequence cap requires asynchronous scheduling, which the simulated engine does not implement |

### Scheduling policy

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--scheduling-policy` | translate | translated to the simulator's --scheduling-policy: FCFS and priority are supported, other policies are rejected |
| `--async-scheduling` | reject | asynchronous scheduling is not implemented by the simulated engine |
| `--scheduler-cls` | reject | a custom scheduler class is code that must exist in the engine; the simulated engine only implements its own scheduler |

### Chunked prefill

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--enable-chunked-prefill` | translate | translated to the simulator's chunked-prefill threshold; a workload that changes chunking against the recording is rejected |
| `--long-prefill-token-threshold` | translate | translated to the simulator's --long-prefill-token-threshold within the profile's coverage |

### KV block size

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--block-size` | translate | translated to the simulator's --tokens-per-block, which must equal the profile's recorded cache.blockSize |
| `--prefix-caching-hash-algo` | reject | the prefix-cache hash algorithm fixes the recorded block-hash representation the profile was captured with, so it cannot be selected by the workload |

### Prefix caching

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--enable-prefix-caching` | require-profile | translated to a profile requirement: the recorded cache either has prefix caching enabled or not, and the simulator's cache model is the recording |
| `VLLM_KV_CACHE_LAYOUT` | reject | a GPU KV-cache memory layout has no meaning for a simulated cache |

### KV live events

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--kv-events-config` | translate | translated to the simulator's KV-event publisher: enablement, endpoint and topic are forwarded and the profile must publish events |

### Shutdown timeout

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--shutdown-timeout` | translate | applied to the simulator's coordinated drain and to the supervisor's graceful shutdown budget |
| `VLLM_WORKER_SHUTDOWN_TIMEOUT_SECONDS` | translate | no local engine workers exist, so the worker shutdown timeout has no effect; the supervisor uses --shutdown-timeout |

### Logging and telemetry

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--disable-log-stats` | preserve | the simulator emits no Prometheus metrics: vllm:* series come from the frontend only |
| `--enable-log-requests` | preserve | forwarded to the frontend and translated to the simulator's --log-requests |
| `VLLM_LOG_STATS_INTERVAL` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged |
| `--log-level` | preserve | the frontend log level is preserved; the simulator log filter comes from the environment's logLevel |
| `--otlp-traces-endpoint` | preserve | only frontend spans are exported: the simulator produces no metrics or traces |
| `--collect-detailed-traces` | reject | detailed engine-core traces require a real engine; the simulator produces no metrics or spans |

### GPU device/CUDA runtime selection

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--device-ids` | translate | replaced: the operator runs a CPU frontend with an external simulated engine, so no device is selected |
| `VLLM_TARGET_DEVICE` | translate | replaced: the frontend runs on the CPU target device and the engine is simulated, so the accelerator target is not forwarded |
| `--numa-bind` | reject | NUMA binding requires the accelerator host topology, which the simulation nodes do not reproduce |
| `--enable-sleep-mode` | reject | sleep mode is not implemented by the simulated engine, whose ready response reports sleep support disabled |

### Eager execution, CUDA graphs, compilation

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--enforce-eager` | translate | no CUDA graphs or compilation are executed: recorded timing is used regardless of the execution mode |
| `--optimization-level` | translate | no compilation is executed: recorded timing is used regardless of the optimization level |
| `--compilation-config` | translate | no compilation is executed: recorded timing is used regardless of the compilation configuration |
| `--max-cudagraph-capture-size` | translate | no CUDA graphs are captured: recorded timing is used regardless of the capture size |

### LoRA, speculative decoding, multimodal generation, embeddings/pooling, structured-output constraints

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--enable-lora` | require-capability | LoRA serving is unconditional in the frontend when adapters are configured; the simulator caps adapter diversity instead of loading weights; capability `lora` |
| `--max-loras` | require-capability | translated to the simulator's --max-loras diversity cap; capability `lora` |
| `--max-lora-rank` | require-capability | adapter weights are not loaded, so the maximum LoRA rank is not enforced; capability `lora` |
| `--lora-modules` | require-capability | adapter names are served by the frontend when the capability set grants LoRA; capability `lora` |
| `--fully-sharded-loras` | require-capability | tensor-sharded LoRA needs real tensor parallelism, which is not executed; capability `lora` |
| `--max-cpu-loras` | require-capability | adapter weights are not loaded, so the CPU LoRA cache size has no effect; capability `lora` |
| `--lora-dtype` | require-capability | adapter weights are not loaded, so the LoRA dtype has no effect; capability `lora` |
| `--speculative-config` | require-capability | the draft model is not run: speculation is served by recorded timing; capability `speculativeDecoding` |
| `--limit-mm-per-prompt` | require-capability | forwarded to the frontend when the capability set grants multimodal serving; capability `multimodal` |
| `--allowed-local-media-path` | require-capability | forwarded to the frontend when the capability set grants multimodal serving; capability `multimodal` |
| `--allowed-media-domains` | require-capability | forwarded to the frontend when the capability set grants multimodal serving; capability `multimodal` |
| `--mm-processor-kwargs` | require-capability | forwarded to the frontend when the capability set grants multimodal serving; capability `multimodal` |
| `--language-model-only` | preserve | forwarded to the frontend unchanged |
| `--enable-mm-embeds` | require-capability | forwarded to the frontend when the capability set grants multimodal serving; capability `multimodal` |
| `--interleave-mm-strings` | require-capability | forwarded to the frontend when the capability set grants multimodal serving; capability `multimodal` |
| `--pooler-config` | require-capability | forwarded to the frontend when the capability set grants embeddings or pooling output; capability `embeddings` |
| `--structured-outputs-config` | require-capability | forwarded to the frontend when the capability set grants structured-output constraints; capability `structuredOutputs` |

### P/D roles and KV-transfer settings

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--kv-transfer-config` | require-capability | translated to the simulator's --pd-role; a connector that is not in the validated capability set is rejected; capability `disaggregatedPrefill` |

### Invocation form

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--config` | translate | the config file's effective values are re-emitted as explicit flags; the file itself is not passed to the frontend |

### Model identity/revision

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `VLLM_USE_MODELSCOPE` | reject | ModelScope model hosting requires the modelscope client in the operator-provided frontend image, which does not include it |
| `VLLM_MODEL_REDIRECT_PATH` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged |

### Host, inference port, API authentication, TLS

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `VLLM_USE_RUST_FRONTEND` | reject | the workload cannot substitute the operator-provided Python frontend with another implementation |

### Tokenizer identity/revision and chat template

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `HF_ENDPOINT` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged |
| `HF_HUB_CACHE` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged |
| `HF_HOME` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged |
| `HF_HUB_OFFLINE` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged |
| `TRANSFORMERS_OFFLINE` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged |
| `HF_TOKEN` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged; its value is never read |

### Shutdown timeout

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `VLLM_ENGINE_READY_TIMEOUT_S` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged |

## Frontend invocation

The workload's vLLM container keeps its name, ports, probes, mounts, sizing
sources, and service-account identity, and its entrypoint becomes the supervisor:

```text
vcr-supervisor -- vllm serve <model> --host <h> --port <p> \
    --data-parallel-size 1 --data-parallel-size-local 0 \
    --data-parallel-address 127.0.0.1 --data-parallel-rpc-port <handshake> [<preserved flags>]
```

- `--data-parallel-size-local 0` is what makes the frontend wait for an externally
  registered engine instead of starting one. The operator never passes
  `--tensor-parallel-size`, `--gpu-memory-utilization`, cache sizing, or precision
  flags to the frontend: the workload's GPU engine is replaced, and the simulator
  advertises the configuration through the ready response.
- `--host` and `--port` are always emitted from the effective value, so an
  environment-derived or CLI-derived endpoint produces exactly one flag.
- Preserved settings are re-emitted from the resolved configuration (see the table).
  The workload's `--config` file itself is not forwarded: its effective values are
  already re-emitted as explicit flags, so the file cannot reintroduce a setting the
  policy rejected.
- A `--tool-parser-plugin` or `--reasoning-parser-plugin` is rejected: a plugin is code
  that would have to exist in the operator-provided frontend image.

Environment set on the frontend container:

| Variable | Value |
| --- | --- |
| `VCR_HANDSHAKE_ADDRESS` | `tcp://127.0.0.1:<handshake>`, the engine-core handshake the frontend **binds** |
| `VCR_PROFILE_DIR` | `names.ProfileMountPath` |
| `VCR_SHUTDOWN_TIMEOUT` | the workload's `--shutdown-timeout`, in seconds |
| `VCR_READY_FILE` | the supervisor's readiness marker |

The workload's remaining environment is forwarded unchanged, which is how Hub tokens
and other credential mounts keep working without their values ever entering a plan.
Accelerator-selecting variables are removed instead: `CUDA_VISIBLE_DEVICES`,
`NVIDIA_VISIBLE_DEVICES`, `NVIDIA_DRIVER_CAPABILITIES`, and `VLLM_TARGET_DEVICE` (the
simulation nodes have no GPU, and a frontend that tries to initialize one cannot
start).

Container ports: the workload's own ports are preserved, and the handshake port is
added. The frontend serves the workload's inference port; readiness and llm-d
discovery continue to use `/v1/models`.

## Simulator invocation

The simulator container runs `vllm-vcr play` with real CLI flags. The `MOCK_*`
variables found in the example manifests are invented by that image's
`entrypoint.sh`; the operator replaces the entrypoint, so they are not used here.

| `play` flag | Source |
| --- | --- |
| `--handshake-address` | `tcp://127.0.0.1:<handshake>`; the simulator **connects**, the frontend owns the endpoint |
| `--model-name` | the underlying model identity, never a served-model alias |
| `--max-model-len` | effective maximum model length |
| `--tokens-per-block` | the profile's recorded `cache.blockSize` |
| `--kv-cache-size` | effective KV block count |
| `--max-num-seqs` | effective sequence cap |
| `--max-num-batched-tokens` | effective per-step budget |
| `--long-prefill-token-threshold` | effective threshold, only when chunked prefill is active |
| `--scheduling-policy` | effective policy, only when the profile authorizes it |
| `--shutdown-timeout` | the workload's shutdown timeout (also applied to the supervisor) |
| `--seed` | the workload's `--seed` (vLLM default `0`) |
| `--latency-trace` | `<profile dir>/<trace.file>` |
| `--replay-tokens`, `--replay-match prefix` | only when the profile grants `capabilities.contentReplay` |
| `--expect-config-hash` | the profile's `vllm.configHash`, when recorded |
| `--enable-kv-cache-events`, `--kv-events-endpoint`, `--kv-events-topic` | KV live events (below) |
| `--control-address` | `tcp://0.0.0.0:<control port>`; JSON control API, not a metrics endpoint |
| `--engine-id` | the Pod identity, when the caller supplies one |
| `--log-requests` | the workload's `--enable-log-requests` |
| `--max-loras` | the workload's `--max-loras`, when the profile grants `lora` |
| `--pd-role` | the workload's KV-transfer role, when the profile grants `disaggregatedPrefill` |

No GPU-only flag is ever passed: the simulator has none, and every accelerator setting
is either translated into one of the flags above or rejected.

The simulator container's only environment variable is `RUST_LOG`, taken from
`spec.logLevel` (default `info`). It mounts the profile volume read-only at
`names.ProfileMountPath` and a writable `emptyDir` at `/tmp` so that its root
filesystem can stay read-only. Container ports: the control API and, when live events
are enabled, the KV-event port.

The profile provisioner init container runs the pinned profile image's own
`provisioning.entrypoint` with the profile mount path as its only argument, so a
profile image always exports the content it was captured with. A profile that declares
no entrypoint is rejected at admission rather than producing a Pod whose backend would
start without profile data.

### KV live events

- Events are enabled when the workload's `--kv-events-config` enables them. An
  operator-side "llm-d mode" does not exist in this API release, so nothing enables
  them implicitly.
- The profile must record `trace.kvEvents`; otherwise the stream the workload
  configures cannot be produced and admission rejects.
- The endpoint is the workload's own when it names one (the simulator binds a
  wildcard host and connects to a concrete one); otherwise the simulator binds
  `tcp://*:<kv events port>`.
- The topic is the workload's own when it names one, otherwise
  `kv@<pod identity>@<model>`, which is the form llm-d expects. Without a Pod identity
  the flag is omitted and the plan records that the simulator derives its own topic.
- Publisher tuning (`buffer_steps`, `hwm`, `max_queue_size`), an unsupported
  publisher, and unknown fields are rejected: the simulator's publisher has none of
  those knobs, and silently ignoring them would misrepresent the emitted stream.

### KV replay

Replay is the one capability the pinned simulator does not implement: it publishes
live events and its ready response always reports `replay_endpoint: null`. An explicit
`replay_endpoint` is therefore accepted only when the profile's capability set grants
`capabilities.kvEventReplay`, and even then the plan records that a consumer which
loses the event window cannot be re-synchronized from the engine. Every reconcilable
profile of this release should declare `kvEventReplay: false`; the capability, not an
available flag, is the authority.

## Placement rewrite

- Removed from the workload: `nvidia.com/gpu*` node selectors and required/preferred
  node-affinity expressions, node labels whose values name the simulated accelerator,
  accelerator taint tolerations, accelerator runtime classes (`nvidia`, `nvidia-cdi`),
  and host volumes for real devices or driver userspace (the device-plugin directory,
  `/usr/local/nvidia`, `/var/lib/nvidia`, `/run/nvidia`, `/dev/nvidia*`, and the
  driver libraries).
- Applied from the environment: the simulation node selector and tolerations.
- Preserved: every other constraint — storage, operating system, zone, sidecar-driven
  affinity. A preserved workload constraint that contradicts the environment's node
  selection cannot be satisfied on a simulation node and is reported instead of being
  dropped.

## Known gaps

- The resolver in `internal/vllm` accepts only the affirmative spelling of boolean
  flags, so an explicit `false` (for example `--no-enable-prefix-caching`) is rejected
  as an unknown flag before this policy runs. The combination checks handle the case
  (a workload cannot currently ask for the opposite cache mode), but expressing it
  awaits a resolver change.
- `--kv-cache-memory-bytes` and `--gpu-memory-utilization` are authorized by
  `coverage.kvMemoryConversion`, but the metadata contract records only the resulting
  `cache.kvBlocks`, so the simulated capacity is the recorded value rather than a
  function of the requested number.
- Simulated capability sets that the serving-behavior spike has not validated must be
  empty: an available frontend flag is not evidence of working end-to-end behavior.
