#!/usr/bin/env bash
# Drive a simulated modelserver through its inference port, the way llm-d does.
#
# Usage:
#   examples/completions.sh <namespace> <pod|service-name> [local-port]
#
# It port-forwards the model server, waits for /v1/models, then issues a
# non-streaming completion, a streaming completion, and reads the frontend's
# metrics. Every response is printed as received: the point of this script is to
# show the interface, not to assert on it. The assertions live in test/e2e and
# test/cluster.
set -euo pipefail

namespace="${1:?usage: examples/completions.sh <namespace> <pod|service-name> [local-port]}"
target="${2:?usage: examples/completions.sh <namespace> <pod|service-name> [local-port]}"
port="${3:-8000}"

log() { printf '\n=== %s ===\n' "$*"; }

kubectl -n "${namespace}" port-forward "${target}" "${port}:8000" >/dev/null 2>&1 &
forward_pid=$!
trap 'kill "${forward_pid}" 2>/dev/null || true' EXIT

log "waiting for /v1/models on 127.0.0.1:${port}"
for _ in $(seq 1 120); do
  if curl -fsS "http://127.0.0.1:${port}/v1/models" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

log "GET /v1/models"
curl -fsS "http://127.0.0.1:${port}/v1/models"

log "POST /v1/completions (non-streaming)"
curl -fsS "http://127.0.0.1:${port}/v1/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"Qwen/Qwen3-0.6B","prompt":"The prefix cache routes by block hash.","max_tokens":8,"temperature":0}'

log "POST /v1/completions (streaming)"
curl -fsS -N "http://127.0.0.1:${port}/v1/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"Qwen/Qwen3-0.6B","prompt":"The prefix cache routes by block hash.","max_tokens":8,"temperature":0,"stream":true}'

log "GET /metrics (vllm series)"
curl -fsS "http://127.0.0.1:${port}/metrics" | grep -E '^vllm:' || true

log "done"
