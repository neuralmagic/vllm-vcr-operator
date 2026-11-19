# Real frontend + simulator validation (OpenShift)

First run of the shipped layout with **real** images instead of test doubles, on
OpenShift (k8s v1.35.9, one simulation worker, no GPUs).

## Images

| Role | Image |
| --- | --- |
| Frontend | `vllm/vllm-openai-cpu:v0.33.0` (official CPU build) + `vcr-supervisor` (built in-cluster from `Dockerfile.supervisor`) |
| Simulator | `ghcr.io/neuralmagic/vllm-vcr:0.2.5-vllm0.33` (published release for the pinned line) |
| Profile | mock profile image: contract-valid metadata, 40 synthetic latency records, id `mock-*` — **no fidelity claim** |

No vLLM build was required: the official CPU image already supports
external-engine mode. It binds the engine-core handshake and waits for the
simulator before serving HTTP.

## Verified

- `engine connected to frontend`; Pod `Ready=True` with **0 container restarts**
- `GET /v1/models` returns the real model list
- `POST /v1/chat/completions` returns a real chat completion (tokens are the
  simulator's, so content is not model output)
- `GET /metrics` exposes **389 `vllm:*` series**, including
  `vllm:num_requests_running`, `vllm:num_requests_waiting`,
  `vllm:kv_cache_usage_perc`, `vllm:cache_config_info{block_size="64"}`,
  `vllm:prompt_tokens_total`, and TTFT histograms

## Defects the real images exposed (fixed)

1. The simulator gives up dialing the handshake after 30s while the frontend needs
   ~40s to import torch and fetch tokenizer files, so every cold start cost a
   container restart. The simulator now restarts in place instead.
2. The supervisor treated a never-healthy simulator as a lost backend and killed
   the frontend after 5s, producing `CrashLoopBackOff`. It now distinguishes cold
   start (`--backend-startup-timeout`, default 5m) from loss.
3. A wait loop that probed the handshake socket was rejected: a dial occupies the
   accept queue the simulator's real connection needs, and a successful dial never
   proved an engine was registered. Nothing probes that socket.

## Not claimed

Latency or throughput fidelity (the profile is a mock), recorded-token replay,
KV-event replay, llm-d routing. Those remain gated on real profile production.
