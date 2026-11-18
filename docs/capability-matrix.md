# Capability matrix

What this operator can be relied on to simulate, what it accepts only with
evidence from a recorded profile, and what it refuses. The matrix is derived from
two sources and nothing else:

- the settings policy — `internal/translator.Policy()`, documented row by row in
  [`docs/settings-policy.md`](settings-policy.md);
- the profile capability set — `internal/profile/capability.go`, documented in
  [`docs/contracts/profile-image.md`](contracts/profile-image.md).

## Status vocabulary

| Status | Meaning |
| --- | --- |
| `validated` | An evidence document under `test/spike/evidence/` shows the behavior, **and** a profile's `capabilities` grants it. A flag existing is never enough. |
| `gated` | Accepted only when the selected profile grants the capability. No evidence document in this checkout establishes it, so it is unverified here. |
| `preserved, unverified` | Forwarded to the real Python vLLM frontend, which implements it. The behavior is vLLM's, not this operator's; no evidence document here exercises it end to end. |
| `translated, unverified` | Rewritten into a simulator setting or a profile requirement. The mapping is unit-tested; the resulting behavior is not demonstrated here. |
| `rejected` | Refused at admission with a message naming the setting and the reason. |

**No row in this matrix is `validated` in this checkout.** No vLLM/vllm-vcr image
was available, so the spike harnesses skipped (they do not fail) and
`test/spike/evidence/` holds only the format example. The command that produces
the evidence is `make test-spike`, and the command that produces cluster-level
evidence is `make test-cluster`. Until one of them has run and its evidence is
recorded in [`docs/spikes/serving-behavior.md`](spikes/serving-behavior.md), every
`gated` and `unverified` row stays unverified — including the ones that look
obvious.

## Serving surface

| Surface | Status | Basis |
| --- | --- | --- |
| `GET /v1/models` on the workload's inference port | preserved, unverified | The workload's own readiness/startup probes are preserved, and llm-d discovery calls it. `docs/contracts/runtime-layout.md` |
| `POST /v1/completions` (non-streaming) | gated (`capabilities.completions`) | Policy table; `internal/profile` capability check |
| `POST /v1/completions` (streaming) | gated (`capabilities.streaming`) | Policy table; the frontend streams SSE through the original port |
| `POST /v1/chat/completions` | gated (`capabilities.chatCompletions`) | Policy table; rendering uses the workload's tokenizer and chat template |
| `POST /v1/messages`, `POST /v1/responses` | gated (`capabilities.messages` / `responses`) | Policy table |
| `POST /tokenize` | preserved, unverified | Frontend feature; the serving spike uses it to cross-check reported `prompt_tokens` |
| `GET /metrics` (Prometheus) | preserved, unverified | Frontend only. Series names in `capabilities.metrics` |
| `/health`, `/readyz`, `/healthz` | preserved, unverified | Frontend `/health`; supervisor `/healthz` and `/readyz` on `127.0.0.1:8082` |
| Simulator control API (`/config`, `/stats`, `/stats/reset`, `/log` on `8001`, bound from `--control-address HOST:PORT`) | translated, unverified | JSON, **not** Prometheus: used only as a readiness input and for diagnostics |
| Engine-core ZMQ handshake on `29550` | preserved, unverified | The frontend binds it; the simulator connects. `docs/contracts/runtime-layout.md` |

## Generation and rendering

| Capability | Status | Notes |
| --- | --- | --- |
| Tokenization and prompt rendering | preserved, unverified | The frontend loads the tokenizer; `--skip-tokenizer-init` and `--uds`/`--headless` are rejected because rendering or the HTTP surface would disappear |
| Chat template (`--chat-template`, `--default-chat-template-kwargs`) | preserved, unverified | Profile selection compares `model.chatTemplateHash` when the profile records one |
| Generation defaults, sampling parameters (`--generation-config`, `--override-generation-config`, `--max-logprobs`, `--logprobs-mode`) | preserved, unverified | Request processing and limits are the frontend's; **output is simulated**, so no probability or quality equivalence is claimed |
| Per-token logprobs | preserved, **gap** | The simulator produces no logprobs, so a request that asks for them observes empty entries. Do not assert logprob values |
| `--seed` | translated, unverified | Becomes the simulator's `--seed`; random tokens are reproducible per request id, which a router-assigned id changes |
| Content-identical replay of recorded tokens | gated (`capabilities.contentReplay`) | Requires `trace.recordedTokens`; only `--replay-match prefix` is usable for live clients, and unmatched requests silently fall back to random tokens |
| Unmatched-request signalling | **rejected as a claim** | The simulator reports no error for a request the trace cannot match; a content assertion cannot distinguish a hit from a fallback |
| Embeddings / pooling | gated (`capabilities.embeddings`) | `--pooler-config` |
| Structured-output constraints | gated (`capabilities.structuredOutputs`) | `--structured-outputs-config` |

## Cache and scheduling

| Capability | Status | Notes |
| --- | --- | --- |
| Prefix caching | translated, unverified | Require-profile: the workload's mode must equal `cache.enablePrefixCaching` |
| KV block size (`--block-size`) | translated, unverified | Becomes `--tokens-per-block` and must equal `cache.blockSize`; llm-d's `tokenProcessorConfig.blockSize` must agree, or prefix matches silently stop |
| Chunked prefill (`--enable-chunked-prefill`, `--long-prefill-token-threshold`) | translated, unverified | Must be consistent with the recording; a change against the recording is rejected |
| Maximum model length | translated, unverified | Bounded by `coverage.maxModelLen` and passed to **both** the frontend (`--max-model-len`) and the simulator; a lower cap inside coverage is accepted, an increase is rejected, and a profile without a bound is refused rather than given an invented limit |
| Maximum running sequences / per-step token budget | translated, unverified | Within `coverage.maxNumSeqs` / `coverage.maxNumBatchedTokens` |
| Scheduling policy (`fcfs`, `priority`) | translated, unverified | Any other policy is rejected |
| KV capacity from a block override (`--num-gpu-blocks-override`) | translated, unverified | Within `coverage.kvBlocks` |
| KV capacity from a memory fraction (`--gpu-memory-utilization`) | gated (`coverage.kvMemoryConversion`) | Capacity is never inferred from a percentage alone; the simulated capacity is the profile's recorded `cache.kvBlocks`, which the plan's warnings state |
| KV capacity from a byte budget (`--kv-cache-memory-bytes`) | rejected | This release has no validated bytes → blocks conversion, so the budget could not change the simulated cache; `--num-gpu-blocks-override` inside `coverage.kvBlocks` is the supported explicit selection |
| KV live events (`--kv-events-config`) | translated, unverified | Requires `trace.kvEvents`; the topic defaults to the llm-d form `kv@<pod identity>@<model>`; publisher tuning fields (`buffer_steps`, `hwm`, `max_queue_size`) are rejected |
| **KV event replay** | **rejected** | The pinned simulator's ready response always reports `replay_endpoint: null` and it has no replay responder. An explicit `replay_endpoint` is rejected unconditionally for this release, with a message naming the pin (`v0.2.5`); the `capabilities.kvEventReplay` gate stays behind it for a future re-pin, and no reconcilable profile grants it. The operator never ignores the setting |
| Sleep mode (`--enable-sleep-mode`) | rejected | Not implemented by the simulated engine |
| Async scheduling, custom scheduler classes, active-sequence caps | rejected | Not implemented by the simulated engine |

## Execution topology and precision

| Capability | Status | Notes |
| --- | --- | --- |
| Tensor parallel size, dtype, quantization | gated (`require-profile`) | Must equal the recorded topology and precision; no GPU computation runs |
| GPU request quantity | translated, unverified | Preserved verbatim on the workload's container; the profile's `gpu.count` must match |
| GPU-requesting init containers | rejected | A GPU-dependent setup step cannot be translated as a whole, so every init container that requests or limits `nvidia.com/gpu` is refused by name. Init containers without a device request are preserved |
| Pipeline parallelism > 1 | rejected | A validated translation does not exist |
| Data parallelism, distributed workers, multi-node ranks, remote engine addresses | rejected | One Pod runs one simulated model server |
| P/D roles and KV-transfer connectors (`--kv-transfer-config`) | gated (`capabilities.disaggregatedPrefill`) | Rejected for every profile that does not grant it |
| DRA (Dynamic Resource Allocation) | **out of scope** | Requests go through the extended resource `nvidia.com/gpu` |
| CUDA graphs, compilation, eager execution | translated, unverified | Not executed; recorded timing is served regardless |
| `--kv-cache-dtype`, `--cpu-offload-gb`, `VLLM_CPU_KVCACHE_SPACE`, `--numa-bind`, `--device-ids`, `VLLM_TARGET_DEVICE` | translated/rejected per row | Accelerator-only settings: some removed, some rejected, none executed. See the policy table |

## Advanced serving features

Every row here is `gated`, and every one is `rejected` for a profile whose
capability set does not grant it — which is every profile this release can
reconcile. No evidence document shows any of them working end to end.

| Feature | Capability | Status |
| --- | --- | --- |
| LoRA adapters (`--enable-lora`, `--max-loras`, `--lora-modules`, …) | `lora` | gated |
| Speculative decoding (`--speculative-config`) | `speculativeDecoding` | gated |
| Multimodal input (`--limit-mm-per-prompt`, `--allowed-media-domains`, …) | `multimodal` | gated |
| Tool-call parsers (`--tool-call-parser <name>`) | `toolCallParser:<name>` | gated |
| Reasoning parsers (`--reasoning-parser <name>`) | `reasoningParser:<name>` | gated |
| P/D serving (`--kv-transfer-config`) | `disaggregatedPrefill` | gated |

A parser or plugin that would have to exist **in the operator-provided image**
(`--tool-parser-plugin`, `--reasoning-parser-plugin`) is always rejected: the
operator does not load arbitrary plugin modules.

## Metrics

| Signal | Source | Status |
| --- | --- | --- |
| `vllm:num_requests_running`, `vllm:num_requests_waiting` | Python frontend, from the simulator's scheduler stats | preserved, unverified |
| `vllm:kv_cache_usage_perc` | Frontend, from the simulator's cache accounting (`--kv-cache-size`) | preserved, unverified |
| `vllm:prefix_cache_*` counters | Frontend, from the simulator's prefix-cache hit/query deltas | preserved, unverified. Exact series names depend on the frontend build |
| `vllm:*` latency histograms | Frontend, driven by the simulator's pacing | preserved, unverified |
| `vllm:lora_requests_info` | Frontend | preserved, unverified |
| Any series from the simulator itself | — | **does not exist**: `vllm-vcr play` has no `/metrics` handler and the simulator container exposes no metrics port |

Metric-driven assertions must therefore target the frontend. A test that scrapes
the simulator's port gets nothing, and that is a documented property, not a bug.

## What is verified without images

These are operator-level properties, not simulated-serving properties. They are
asserted by unit and envtest tests that need no container images:

| Property | Command |
| --- | --- |
| Every vLLM 0.33.0 setting has exactly one policy row, and dispositions are complete | `go test ./internal/translator/...` |
| Unsupported configuration is rejected with a message naming the setting and never a credential | `go test ./internal/translator/...` |
| Profile selection is all-or-nothing; absence of coverage is not permission | `go test ./internal/profile/...` |
| Mutation keeps the GPU quantity on exactly one container, injects the documented layout, and is idempotent | `go test ./internal/runtime/...` |
| The profile export refuses a missing or empty trace and publishes atomically | `go test ./cmd/profileexport/...` |
| The device plugin advertises the synthetic resource and registers with kubelet | `go test ./internal/deviceplugin/...` |
| Fixtures request the synthetic resource, keep llm-d labels, and carry no simulator opt-in | `go test ./test/fixtures/...` |
| Admission, provenance annotations, and rejection in a cluster-shaped API server | `make test-e2e` |
| Every emitted `vllm-vcr play` flag, the pin, and the target vLLM line match the pinned simulator release | `make check-upstream` (skips cleanly offline) and `go test ./hack/...` |

## How a capability moves

1. **Nothing is `validated` without evidence.** Run `make test-spike` with the
   pinned images and a recorded profile; the harness writes
   `test/spike/evidence/serving-behavior.json`.
2. **A failing check becomes a rejection or identified work**, never a softening
   of the matrix: add or change a row in `internal/translator/policy.go`, then
   update [`docs/settings-policy.md`](settings-policy.md) and this page.
3. **A passing check becomes a profile capability.** The profile producer sets
   the corresponding `capabilities` entry; the operator then admits workloads that
   select it, and only for that profile.
4. **Update the spike record** ([`docs/spikes/serving-behavior.md`](spikes/serving-behavior.md))
   with the evidence file name and the pinned image digests, so the claim has a
   date and a subject.
