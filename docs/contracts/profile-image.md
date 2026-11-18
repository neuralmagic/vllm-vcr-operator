# Profile image contract (v1)

Status: binding contract between the profile-producing repository and this
operator. Consumers are `internal/profile`, `internal/registry`, and the profile
provisioning stage.

## Purpose

Recorded timing and content live outside this operator. A profile is an OCI image
that carries both its compatibility metadata and its trace data. The operator
reconciles metadata outside admission and admits a Pod only when exactly one
profile matches; the Pod then pins the profile image by digest.

## Image content

```text
/                       image filesystem
└── profile/
    ├── metadata.json   profile document (schema below)
    └── trace.jsonl.gz  recorded trace referenced by metadata.trace.file
```

The metadata document is also published as an OCI image config label named
`vcr.opendatahub-io/profile-metadata` (see `names.ProfileMetadataLabel`) so the
operator can reconcile compatibility without downloading trace layers. The label
value and `/profile/metadata.json` must be byte-identical JSON documents.

`metadata.provisioning.entrypoint` names the command that exports data. The
operator runs it as an init container with one argument, the target directory:

```text
<entrypoint...> /var/run/vcr/profile
```

The command must copy `metadata.json` and the trace file into that directory and
exit non-zero if any required file is missing or unreadable. Serving must not
start when provisioning fails.

## Metadata document

```json
{
  "schemaVersion": "vcr.opendatahub-io/profile/v1",
  "id": "qwen3-0.6b-h200-tp1-bf16",
  "model": {
    "name": "Qwen/Qwen3-0.6B",
    "revision": "c9340e6f3",
    "tokenizer": "Qwen/Qwen3-0.6B",
    "tokenizerRevision": "c9340e6f3",
    "chatTemplateHash": "sha256:..."
  },
  "gpu": { "type": "H200", "count": 1 },
  "execution": {
    "tensorParallel": 1,
    "pipelineParallel": 1,
    "dtype": "bfloat16",
    "quantization": ""
  },
  "cache": {
    "blockSize": 64,
    "enablePrefixCaching": true,
    "kvBlocks": 2048,
    "chunkedPrefill": true
  },
  "vllm": { "version": "0.33.0", "configHash": "3f6c...bare-hex-sha256" },
  "coverage": {
    "maxModelLen": { "min": 1, "max": 131072 },
    "maxNumSeqs": { "min": 1, "max": 128 },
    "maxNumBatchedTokens": { "min": 1, "max": 8192 },
    "longPrefillTokenThreshold": { "min": 0, "max": 4096 },
    "kvBlocks": { "min": 1, "max": 2048 },
    "schedulingPolicies": ["fcfs", "priority"],
    "kvMemoryConversion": false
  },
  "trace": {
    "file": "trace.jsonl.gz",
    "format": "vllm-vcr/jsonl",
    "recordedTokens": true,
    "latencyModel": true,
    "kvEvents": true
  },
  "provisioning": {
    "entrypoint": ["/usr/local/bin/vcr-profile-export"],
    "metadataFile": "metadata.json",
    "traceFile": "trace.jsonl.gz"
  },
  "capabilities": {
    "chatCompletions": true,
    "completions": true,
    "messages": false,
    "responses": false,
    "streaming": true,
    "toolCallParsers": [],
    "reasoningParsers": [],
    "kvEventReplay": false,
    "metrics": ["vllm:num_requests_running", "vllm:kv_cache_usage_perc"]
  }
}
```

## Rules

- `schemaVersion` must be exactly `vcr.opendatahub-io/profile/v1`. Unknown
  versions are unusable, not best-effort parsed.
- `gpu.type` is the simulated accelerator supplied by `SimulationEnvironment`
  (`spec.gpuType`), not a value inferred from device count.
- `gpu.count` states how many synthetic devices the recorded topology implies.
  A workload requesting 2 devices requires a profile whose `gpu.count` is 2 and
  whose `execution.tensorParallel` is consistent with that count.
- `coverage` authorizes overrides. A workload setting outside coverage is
  rejected. Absence of a coverage entry never implies support.
- `capabilities` is the validated serving surface produced by the
  serving-behavior spike. A flag being accepted by the frontend or simulator is
  not evidence of support; only entries here authorize serving features.
- `vllm.version` is the vLLM release the trace was captured against, and
  `vllm.configHash` is the trace's recorded `meta.config_hash` copied verbatim: a
  bare lowercase-hex SHA-256. The simulator compares that string as an opaque
  value and never recomputes it, so a profile whose hash carries the `sha256:`
  algorithm tag has it removed before `--expect-config-hash` is passed (the tag
  names an algorithm; it is not part of the recorded value). A `configHash` that
  is empty after the tag is rejected, because it would silently disable the check.
- `trace.recordedTokens` distinguishes content-identical replay from random-token
  output. Random tokens are valid for protocol and routing tests; content
  assertions require `recordedTokens`.
- A `capabilities.contentReplay` profile must also record per-request
  `block_hashes` in its trace: live clients name requests however they like, so
  replay matches the prompt's longest block-hash prefix
  (`--replay-match prefix`), and the simulator refuses to start in prefix mode
  against a trace without them. The operator never reads the trace, so it cannot
  verify this from metadata; the profile producer must.
- `capabilities.kvEventReplay` must be true before replay-based recovery tests
  can be claimed. The current simulator publishes live KV events but has no
  replay endpoint.
- Images referenced by tag are resolved and pinned by digest during
  reconciliation. Admission always pins a digest.
- Pull credentials are Secret references in the operator namespace. Their values
  never appear in Pod annotations, status, or diagnostics.
