# Spike: serving behavior and the llm-d contract

The specification requires a spike that establishes, against the selected
frontend and simulator: render and generation behavior, tokenization consistency,
metric names and semantics, random and recorded token behavior, unmatched replay
requests, prefix-cache modes and eviction, and KV live/replay events — and then
explicitly classifies the advanced features from the settings policy. Its exit
criteria are "an executable compatibility suite and a supported-capability
matrix", with the rule that *an available CLI flag is not evidence of working E2E
behavior*.

This page is the record. The executable half is
`test/spike/serving_behavior_test.go`; the evidence is
`test/spike/evidence/serving-behavior.json`; the outcome is
[`docs/capability-matrix.md`](../capability-matrix.md).

## Status in this checkout

**Not demonstrated here.** No vLLM/vllm-vcr image was available, so the harness
skipped and `test/spike/evidence/` holds only the format example. The capability
matrix therefore marks every serving row `gated` or `unverified`, and no row is
`validated`.

```bash
export VCR_FRONTEND_IMAGE=<cpu vllm frontend> VCR_BACKEND_IMAGE=<vllm-vcr> \
       VCR_PROFILE_IMAGE=<profile image>
export VCR_SPIKE_REQUIRED=1
make test-spike
```

## What the harness drives

It starts the same process tree as the runtime-layout spike (provisioner →
frontend → simulator, one loopback interface) with KV events enabled, then talks
only to the public surfaces: the frontend's inference port and the simulator's
control API.

| Check | Interaction | Independent cross-check |
| --- | --- | --- |
| Model identity | `GET /v1/models` | The id must equal the profile's model, not a served-model alias |
| Tokenizer consistency | `POST /tokenize` on a fixed prompt | A frontend that serves no tokenizer endpoint is recorded as a gap, not as a pass |
| Generation | `POST /v1/completions` (`max_tokens: 8`, `temperature: 0`) | `usage.prompt_tokens` must equal the length of the tokenizer's id list — two independent surfaces, not a second tokenizer |
| Streaming | `POST /v1/completions` with `stream: true` | At least two `data:` events and a `[DONE]` terminator; the event count is recorded |
| Metrics | `GET /metrics` on the inference port | `vllm:num_requests_running` and `vllm:kv_cache_usage_perc` must be present; every `vllm:*` name is recorded |
| Simulator metrics | `GET /metrics` on the control API port | Must **not** be `200`: the simulator has no metrics surface |
| Random vs recorded tokens | Two identical prompts | Which mode produced them is the profile's decision. The harness records whether the text matched and, when the profile grants no `contentReplay`, records that this run's tokens are seeded random output and content assertions are not authorized |
| Engine accounting | `GET /stats` on the control API | The counters are the authoritative witness that every request reached the engine once and none failed: `requests_received ≥ issued`, `requests_failed == 0`, `received ≥ completed + failed + aborted` |
| Unmatched replay | A prompt no trace record contains | Must still be served; the absence of an error signal is recorded as a gap, not smoothed over |
| Cache pressure / eviction | Distinct long prompts until `vllm:kv_cache_usage_perc` ≥ 0.95 | Bounded loop (120 requests); the peak usage is recorded. Capacity is the profile's `cache.kvBlocks × cache.blockSize` |
| KV-event publication | TCP connect to the llm-d discovery port (`5556`) | The simulator binds it only while the publisher is enabled |

Every request and response is the real protocol. No check inspects a container's
internals.

## Metric names and meaning

The one property that is not a maybe: **the simulator emits no Prometheus
metrics.** `vllm-vcr play`'s only HTTP surface is a JSON control API with
`/config`, `/stats`, `/stats/reset`, and `/log`. Every `vllm:*` series is produced
by the Python frontend from the scheduler statistics the simulator sends over the
wire.

Consequences the matrix depends on:

- a scrape of the simulator port yields nothing; assertions must target the
  inference port;
- the frontend's series reflect simulated state, so they are meaningful for
  routing and queueing assertions (which Pod is busy, which has a full cache) but
  never for hardware performance;
- the exact series names depend on the frontend build, which is why a profile
  records the set under `capabilities.metrics` instead of the operator hard-coding
  it.

The harness records the full `vllm:*` name list into `observations` on every run,
so a rename shows up as a diff in evidence rather than as a silently skipped
assertion.

## Recorded versus random tokens

| Mode | When | What a test may assert |
| --- | --- | --- |
| Random | The profile does not grant `capabilities.contentReplay` | Protocol shape, streaming, routing, metrics, queueing. **Not content** |
| Recorded (`--replay-tokens --replay-match prefix`) | The profile grants `capabilities.contentReplay`, which requires `trace.recordedTokens` | Content, for prompts the trace matches |

Three properties of recorded mode that the spike records rather than hides:

- **Prefix matching is the only usable mode for a live client.** Index matching
  relies on the replay harness's request ids (`replay-<i>`), which never appear on
  the wire.
- **Each recorded record is consumed once.** Repeating a prompt therefore
  legitimately falls back to random tokens afterwards, so "identical prompt twice ⇒
  identical text" is *not* a valid replay assertion. The harness records the
  observation instead of encoding a wrong expectation.
- **Unmatched requests fall back to random tokens, silently.** The spike asserts
  the request is still served and records that no error signal exists, so a
  content assertion cannot distinguish a hit from a fallback. Match-rate
  assertions must come from the simulator's `/stats` counters; the harness reads
  them and asserts the accounting (`received`, `completed`, `failed`, `aborted`),
  which is the part that is strict.

## Prefix-cache eviction

The harness raises `vllm:kv_cache_usage_perc` to at least 0.95 by sending distinct
long prompts, which forces the cache past the profile's capacity and makes
eviction occur. It asserts the capacity is reachable and records the peak usage.

What it does not do is decode the KV-event stream to observe `BlockRemoved`
directly: the harness links no ZMQ client, and inventing one would duplicate what
llm-d's own consumer does. Frame-level validation against the real consumer is
`make test-cluster`'s job, with the real EPP.

## KV events, and what is not there

Live events: enablement, endpoint, and topic come from the workload's
`--kv-events-config`. The operator translates them to the simulator's
`--enable-kv-cache-events`, `--kv-events-endpoint`, and `--kv-events-topic`, and
derives the llm-d topic form `kv@<pod identity>@<model>` when the workload names
no topic. Two conditions are rejections, not best effort: the profile must record
`trace.kvEvents`, and publisher tuning fields (`buffer_steps`, `hwm`,
`max_queue_size`) are refused because the publisher has no such knobs.

Replay: **does not exist.** The simulator's ready response mirrors vLLM's
`KVEventsConfig` with `replay_endpoint: null`, and the only publisher socket is a
PUB with no responder counterpart. The consequence, stated in the specification
itself: full recovery acceptance cannot be claimed until compatible replay is
available, and a missing capability is not waived by marking the broader work
complete. The operator's response is an unconditional rejection for an explicit
`replay_endpoint` while the pinned simulator lacks replay; a profile's
`capabilities.kvEventReplay` is the narrower gate behind it.

## Evidence format

`test/spike/evidence/serving-behavior.json`, same schema
(`vcr.opendatahub-io/spike-evidence/v1`) as the runtime-layout record:

| Field | How to read it |
| --- | --- |
| `images` | Exact references; the pinned frontend and simulator this result belongs to |
| `environment` | Served model, profile source, endpoints |
| `steps[]` | Pass/fail per check above, with the reason for a failure |
| `observations[]` | Streaming event count, tokenized prompt length, the `vllm:*` series names, simulator `/metrics` status, simulator request counters, whether two identical prompts produced identical text, peak cache usage |
| `notDemonstrated[]` | Gaps the run states explicitly |
| `passed` | True only if every step passed and no gap was recorded |

A failed step is a capability finding, and the specification fixes what happens
next: it becomes an **explicit rejection** in the policy table or **identified
implementation work**. It never becomes undocumented fallback behavior, and the
matrix is never edited to call it supported.

## Not demonstrated in this environment

| Not demonstrated | Consequence |
| --- | --- |
| Any of the above, currently | The harness skipped; there is no evidence file, so the matrix's `gated`/`unverified` rows stay unverified |
| KV-event wire format | No ZMQ client in the harness; frames were never decoded. `make test-cluster` (real EPP) is the check |
| KV-event replay | Impossible with the pinned simulator; the operator rejects the setting |
| Cache-local routing and eviction effects on routing | No router ran. The operator provides the prerequisites only |
| Model output quality | Output is recorded ids or seeded random tokens; no comparison with real inference |
| GPU behavior and timing fidelity | No accelerator; recorded latency served as recorded |
| Advanced features (LoRA, speculative decoding, multimodal, embeddings/pooling, structured outputs, P/D) | Not exercised at all, so every one stays `gated` in the matrix. A flag existing in the frontend is not evidence |

## Consequences for the capability matrix

Each unresolved item maps to a decision that is already in force:

| Item | Decision |
| --- | --- |
| KV replay absent | Rejected for every profile without `capabilities.kvEventReplay`; live-event routing only |
| Unmatched replay silent | Content assertions require recorded tokens **and** a match-rate check via `/stats`; otherwise only protocol assertions are allowed |
| Simulator has no metrics | Assertions target the frontend's `/metrics`; no series is attributed to the simulator |
| Logprobs absent | Requests are served, values are empty; never asserted |
| Advanced features unvalidated | `require-capability` rows in `internal/translator/policy.go`; rejected unless a profile grants them, which none currently does |
