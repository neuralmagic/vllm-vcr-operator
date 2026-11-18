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
   policy decision: a resolved default is not a workload request. Both spellings of a
   boolean setting resolve: `--enable-prefix-caching` is an explicit `true` and
   `--no-enable-prefix-caching` (or the release's `--disable-prefix-caching` alias) is
   an explicit `false` attributed to the setting's canonical name, which is what makes
   "preserve enabled/disabled semantics" expressible at all. A command line that names
   both spellings resolves the way vLLM 0.33.0's argparse does — the last occurrence
   wins — because the resolver reports what the release would do rather than
   inventing a conflict rule; the precedence between layers is unchanged, so a CLI
   negation still overrides a config file or environment value.
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
| `VLLM_USE_MODELSCOPE` | reject | ModelScope model hosting requires the modelscope client in the operator-provided frontend image, which does not include it |
| `VLLM_MODEL_REDIRECT_PATH` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged |

### Tokenizer identity/revision and chat template

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--tokenizer` | preserve | forwarded to the frontend unchanged |
| `--tokenizer-revision` | preserve | forwarded to the frontend unchanged |
| `--tokenizer-mode` | preserve | forwarded to the frontend unchanged |
| `--skip-tokenizer-init` | reject | the frontend renders prompts and streams tokens, so it must load the tokenizer: a skipped tokenizer cannot serve requests |
| `--chat-template` | preserve | preserved as --chat-template, either the container path (the mount is preserved) or the inline template |
| `--default-chat-template-kwargs` | preserve | forwarded to the frontend unchanged |
| `HF_ENDPOINT` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged |
| `HF_HUB_CACHE` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged |
| `HF_HOME` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged |
| `HF_HUB_OFFLINE` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged |
| `TRANSFORMERS_OFFLINE` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged |
| `HF_TOKEN` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged; its value is never read |

### Served-model aliases

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--served-model-name` | preserve | forwarded to the frontend unchanged |

### Host, inference port, API authentication, TLS

| Setting | Disposition | Treatment |
| --- | --- | --- |
| `--host` | preserve | the frontend builder always emits --host from the effective value, so no duplicate flag is produced |
| `--port` | preserve | the frontend builder always emits --port from the effective value, so no duplicate flag is produced |
| `--uds` | reject | a Unix-domain-socket server has no TCP endpoint, so kubelet probes, llm-d discovery, and the supervisor's readiness input cannot reach it; serve on the inference port instead |
| `--grpc-port` | preserve | forwarded to the frontend unchanged |
| `--headless` | reject | a headless server exposes no HTTP API, so readiness, probes, and llm-d discovery cannot pass; only a serving workload can be simulated |
| `--api-server-count` | preserve | forwarded to the frontend unchanged |
| `--uvicorn-log-level` | preserve | forwarded to the frontend unchanged |
| `--disable-uvicorn-access-log` | preserve | forwarded to the frontend unchanged |
| `--api-key` | preserve | preserved by copying the workload's own argument verbatim at mutation time, so the key never enters the plan |
| `VLLM_API_KEY` | preserve | preserved as the workload container's environment variable, which the runtime forwards unchanged |
| `--ssl-keyfile` | preserve | forwarded to the frontend unchanged |
| `--ssl-certfile` | preserve | forwarded to the frontend unchanged |
| `--ssl-ca-certs` | preserve | forwarded to the frontend unchanged |
| `--ssl-cert-reqs` | preserve | forwarded to the frontend unchanged |
| `VLLM_USE_RUST_FRONTEND` | reject | the workload cannot substitute the operator-provided Python frontend with another implementation |

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
| `VLLM_ENGINE_READY_TIMEOUT_S` | preserve | preserved as the workload container's environment variable: the frontend must wait for the simulator to register |

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

## Frontend invocation

The workload's vLLM container keeps its ports, probes, mounts, sizing sources, and
service-account identity, takes the layout's documented container name
(`vllm-frontend`, `names.FrontendContainerName`), and its entrypoint becomes the
supervisor:

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
- `--uds` and `--headless` are rejected: neither exposes an HTTP endpoint on the
  inference port, so kubelet probes, llm-d discovery, and the supervisor's readiness
  input could never pass. Readiness is not something admission can fake.

Environment set on the frontend container:

| Variable | Value |
| --- | --- |
| `VCR_HANDSHAKE_ADDRESS` | `tcp://127.0.0.1:<handshake>`, the engine-core handshake the frontend **binds** |
| `VCR_FRONTEND_HEALTH_URL` | `http://127.0.0.1:<inference port>/health`, the readiness input served by the frontend |
| `VCR_BACKEND_HEALTH_URL` | `http://127.0.0.1:<control port>/stats`, the readiness input served by the simulator control API |
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
- Applied from the environment: the simulation node selector and tolerations, and
  the CPU/memory sizing of the injected containers. Environment sizing that asks for
  `nvidia.com/gpu` is rejected: only the workload's own container holds the synthetic
  allocation, so the count stays exactly one.
- Preserved: every other constraint — storage, operating system, zone, sidecar-driven
  affinity. A preserved workload constraint that contradicts the environment's node
  selection cannot be satisfied on a simulation node and is reported instead of being
  dropped.

## Known gaps

- The supervisor's readiness inputs are configured by name from
  `docs/contracts/runtime-layout.md` (`VCR_FRONTEND_HEALTH_URL`,
  `VCR_BACKEND_HEALTH_URL`). A supervisor build that does not read them falls back to
  its own defaults; the plan already sets the correct values, so the fallback is only a
  fidelity gap for non-default inference ports.
- A negated spelling is derived from the modelled setting, so `--no-<flag>` is
  accepted only where the operator models `<flag>`. A negation whose positive
  counterpart is not modelled (`--no-use-tqdm`) and any spelling the release does not
  register at all stay rejections that name the spelling; nothing is accepted as an
  inert no-op.
- `--kv-cache-memory-bytes` and `--gpu-memory-utilization` are authorized by
  `coverage.kvMemoryConversion`, but the metadata contract records only the resulting
  `cache.kvBlocks`, so the simulated capacity is the recorded value rather than a
  function of the requested number.
- Simulated capability sets that the serving-behavior spike has not validated must be
  empty: an available frontend flag is not evidence of working end-to-end behavior.

## Why these dispositions

The four accepting dispositions are not degrees of support; they are four
different claims, and each one is only as strong as its evidence.

- **`preserve`** means "the real Python vLLM frontend implements this, and the
  operator does not interfere". It is the strongest claim the operator can make
  for a serving feature, and it is still only a claim about *forwarding*: whether
  the feature behaves as a user expects is vLLM's behavior, not this operator's.
  A preserved setting that the frontend cannot act on in external-engine mode
  (there is no engine rank, no weights, no accelerator) carries a `Warning`
  instead of pretending.
- **`translate`** means "the setting survives in a different form". The form is
  always one of three: a simulator flag, a profile requirement, or a deliberate
  no-op that changes no observable behavior. A no-op that *would* change observable
  behavior is a `reject`, not a `translate` with a warning: recorded timing is
  served either way, so `--enforce-eager` is a no-op, while `--pipeline-parallel-size
  2` is not.
- **`require-profile`** means "the profile's recording is the authority". Topology,
  precision, and cache representation are recorded properties; a workload value
  that disagrees with the recording is rejected rather than approximated, because
  the recorded timing was produced by exactly one configuration.
- **`require-capability`** means "only a validated capability set may authorize
  this". The set is produced by the serving-behavior spike and shipped in profile
  metadata ([`docs/contracts/profile-image.md`](contracts/profile-image.md)). A
  frontend flag that exists is not evidence: an available `--enable-lora` does not
  authorize LoRA serving, and [`docs/capability-matrix.md`](capability-matrix.md)
  says so row by row.
- **`reject`** is the default for everything else, including settings nobody has
  classified. The policy table is data, so a new vLLM flag is rejected as an
  unknown behavior-affecting setting until someone adds a reviewed row.

Two structural consequences follow, and both are testable:

- the table is the implementation (`translator.Policy()`), and a test asserts every
  row has a disposition and a reason, so there is no silent fallthrough;
- omission is never permission. A missing policy row rejects, a missing coverage
  range rejects, a missing capability rejects, and a missing profile rejects.

## vLLM 0.33.0 flag names

The policy table keys are the vLLM 0.33.0 spellings exactly, because the resolver
matches them exactly. Details that matter when writing a workload or adding a row:

- **Two namespaces.** Command-line flags appear as `--flag-name`; settings vLLM
  reads from the environment appear as the variable name (`VLLM_DP_RANK`,
  `HF_HOME`, `TF_*`). A row is one or the other, never both.
- **The model is positional.** `--model` and `model` both appear as rows because
  `vllm serve <model>` and `vllm serve --model <model>` are both accepted; both
  are preserved identically.
- **JSON-valued flags.** `--kv-events-config`, `--kv-transfer-config`,
  `--speculative-config`, `--compilation-config`, `--structured-outputs-config`,
  `--pooler-config`, `--hf-overrides`, and `--limit-mm-per-prompt` take JSON
  objects. They are decoded into typed settings so their fields can be validated
  individually; an unknown field inside them is a rejection that names the field.
- **Booleans resolve only affirmatively** (see *Known gaps*): `--enable-prefix-caching`
  parses; `--no-enable-prefix-caching` is an unknown flag.
- **Defaults are not requests.** A setting the workload did not set never triggers
  a policy decision, even when vLLM has a default for it. A default that the
  simulator must know (maximum model length, sequence caps) is resolved from the
  profile's coverage instead, and a warning is recorded when the profile does not
  vouch for it.
- **Unknown flags are rejected before the policy runs.** The resolver refuses an
  unrecognized flag or an unknown `--config` file key rather than executing a
  shell wrapper or introspecting the image to guess its meaning.

## Rejection examples

Rejections are structured (`translator.Rejection`, `vllm.Rejection`): a setting
name followed by a reason. The reason text below is taken from the policy table
and the validators it drives, so it is the text a rejected workload actually sees.
Reason text that lives in a `fmt.Sprintf` template is shown with the profile's
recorded value.

| Workload | Rejection | What protects the user |
| --- | --- | --- |
| `--pipeline-parallel-size 2` | `--pipeline-parallel-size: pipeline parallelism greater than one is rejected pending validated translation: the simulated engine models one stage` | The workload cannot silently run with one simulated stage while claiming two |
| `--enable-lora --max-loras 4`, profile grants no LoRA | `--enable-lora: profile "<id>" does not support capability "lora": the profile's validated capability set does not grant it` | Adapter behavior is never faked; the profile must have validated it |
| `--tool-call-parser hermes`, profile grants no parsers | `--tool-call-parser: profile "<id>" does not support capability "toolCallParser:hermes": the profile's validated capability set does not grant it` | A parser is code and behavior, not a forwarded string |
| `--speculative-config '{"method":"ngram"}'` | `--speculative-config: profile "<id>" does not support capability "speculativeDecoding": …` | Speculation is not simulated by recording a longer output |
| `--kv-events-config '{"replay_endpoint":"tcp://0.0.0.0:5557"}'` | `--kv-events-config: profile "<id>" does not support capability "kvEventReplay": …` | A router cannot be told replay exists when the engine has no replay responder |
| `--kv-events-config '{"enable_kv_cache_events":true,…}'`, profile `trace.kvEvents` false | `--kv-events-config: profile "<id>" records no KV-cache events (trace.kvEvents is false), so the simulator cannot publish the stream the workload configures` | No empty event stream that a router quietly indexes as "no cache" |
| `--kv-events-config` with `"buffer_steps": 8` | `--kv-events-config: bufferSteps is not supported: the simulator's publisher has no step buffer to configure` | Publisher tuning is not silently ignored |
| `--kv-events-config` with an unknown field | `--kv-events-config: field "<name>" is not an event-publisher setting this operator can apply` | A misspelled or newer field cannot be dropped on the floor |
| `--gpu-memory-utilization 0.9`, profile declares no `coverage.kvMemoryConversion` | `gpuMemoryUtilization 0.9 is not authorized: profile "<id>" does not declare coverage.kvMemoryConversion, so simulated KV capacity cannot be derived from a memory fraction` | Cache capacity is never inferred from a percentage |
| `--max-num-seqs 512`, `coverage.maxNumSeqs.max = 128` | `maxNumSeqs 512 is not authorized: profile "<id>" does not declare … (an absent coverage entry is not permission)`, or a violated-range message when the entry exists | A recording cannot vouch for a concurrency it never saw |
| `--max-model-len 131072`, `coverage.maxModelLen.max = 32768` | Rejected as an increase beyond coverage | The frontend and the simulator would advertise a length no recording supports |
| `--max-model-len` unset, profile declares no bound | `the workload does not set a maximum model length and profile "<id>" declares no coverage.maxModelLen, so no authorized bound exists; declare the setting or extend the profile coverage` | No invented model length |
| `--tensor-parallel-size 2` with a 1-GPU request | Rejected: the requested device count and the recorded topology disagree | Synthetic devices are not multiplied into engines |
| `--enable-chunked-prefill` vs a profile recorded without it | `the workload asks for chunked prefill true but profile "<id>" was recorded with it false: the recorded timing and cache representation cannot serve the other mode` | Timing is not borrowed across execution modes |
| `--scheduling-policy ltr` | `--scheduling-policy: policy "ltr" is not supported: the simulator implements FCFS and priority only` | Only scheduling the simulator models is accepted |
| Profile with `trace.latencyModel` false | `profile "<id>" records no latency model: serving would fall back to synthetic timing, which the policy forbids` | Timing is never invented |
| Inference port collides with the handshake/control/KV-event port | `the <a> port <n> collides with the <b> port in the same Pod network namespace; one of them must be changed` | A Pod that could never work is not admitted |
| `--uds /tmp/vllm.sock` | `--uds: a Unix-domain-socket server has no TCP endpoint, so kubelet probes, llm-d discovery, and the supervisor's readiness input cannot reach it; serve on the inference port instead` | A workload is never admitted into a state where readiness can never pass |
| `--headless` | `--headless: a headless server exposes no HTTP API, so readiness, probes, and llm-d discovery cannot pass; only a serving workload can be simulated` | Same |
| `--skip-tokenizer-init` | `--skip-tokenizer-init: the frontend renders prompts and streams tokens, so it must load the tokenizer: a skipped tokenizer cannot serve requests` | Rendering is not silently disabled |
| `--tool-parser-plugin my.module` | `--tool-parser-plugin: a tool-parser plugin is code that must exist in the operator-provided frontend image; arbitrary plugin modules are not loaded` | Arbitrary modules are never loaded |
| `--api-key` supplied through a `--config` file | `--api-key: an API key supplied through the config file cannot be re-emitted without retaining its value; pass it with --api-key on the command line or with the VLLM_API_KEY environment variable` | The key never enters a plan, annotation, status, log, or event |
| A flag the resolver does not know | `vllm: --some-new-flag=<value>: unknown flag; it is not part of the vLLM 0.33.0 command line` | New vLLM flags are rejected until someone adds a reviewed row; nothing introspects an image to guess a flag's meaning |
| An unmodelled environment variable | `VLLM_SOME_NEW_SETTING: Unknown behavior-affecting settings: the variable can change vLLM or tokenizer behaviour, and its policy treatment is undefined` | An unrecognized environment setting cannot quietly change behavior |

Every message keeps that shape: the setting, then the reason. None contains a
credential value; a secret-bearing setting is reported by name with
`<redacted>` in place of its value. A workload that passes produces no rejection
at all — the plan records what was accepted instead, including a warning wherever
a behavior is not reproduced exactly.

`go test ./internal/translator/...` and `go test ./internal/vllm/...` assert these
shapes at the packages' public interfaces.
