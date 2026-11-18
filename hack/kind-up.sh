#!/usr/bin/env bash
# kind-up.sh creates an optional local kind cluster for trying the operator.
#
# kind runs Kubernetes nodes as Docker containers, so it needs a reachable Docker
# daemon (or a Docker-compatible runtime). This script fails with an explicit
# message instead of silently doing nothing when that prerequisite is missing.
#
# Usage:
#   hack/kind-up.sh [--name NAME] [--delete] [--no-images]
#
#   --name NAME   kind cluster name (default: vllm-vcr-operator)
#   --delete      delete the cluster and exit
#   --no-images   skip loading locally built images into the cluster
#
# Images are loaded from the same VCR_* references the manager Deployment uses
# (config/manager/manager.yaml) when they exist locally:
#   VCR_FRONTEND_IMAGE, VCR_BACKEND_IMAGE, VCR_DEVICE_PLUGIN_IMAGE,
#   VCR_SUPERVISOR_IMAGE, VCR_MANAGER_IMAGE
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

cluster="vllm-vcr-operator"
delete=false
load_images=true

usage() {
  sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --name)
      cluster="${2:?--name needs a value}"
      shift 2
      ;;
    --delete)
      delete=true
      shift
      ;;
    --no-images)
      load_images=false
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

command -v kind >/dev/null || {
  echo "kind is not installed: https://kind.sigs.k8s.io/docs/user/quick-start/#installation" >&2
  exit 1
}

if [[ "$delete" == true ]]; then
  kind delete cluster --name "$cluster"
  exit 0
fi

# Fail clearly, not silently: kind needs a working container runtime.
if ! command -v docker >/dev/null; then
  echo "docker is not installed. kind needs a Docker-compatible runtime to host nodes." >&2
  echo "Install Docker (or use a real cluster and 'hack/install.sh')." >&2
  exit 1
fi
if ! docker info >/dev/null 2>&1; then
  echo "the Docker daemon is not reachable; kind cannot create nodes." >&2
  echo "Start Docker (or use a real cluster and 'hack/install.sh')." >&2
  exit 1
fi

if kind get clusters 2>/dev/null | grep -qx "$cluster"; then
  echo "kind cluster '$cluster' already exists"
else
  echo "creating kind cluster '$cluster'"
  kind create cluster --name "$cluster" --wait 120s
fi

kubectl config use-context "kind-$cluster" >/dev/null

if [[ "$load_images" == true ]]; then
  loaded=0
  for var in VCR_MANAGER_IMAGE VCR_FRONTEND_IMAGE VCR_BACKEND_IMAGE \
    VCR_DEVICE_PLUGIN_IMAGE VCR_SUPERVISOR_IMAGE; do
    image="${!var:-}"
    if [[ -z "$image" ]]; then
      continue
    fi
    if docker image inspect "$image" >/dev/null 2>&1; then
      echo "loading $var=$image into the cluster"
      kind load docker-image --name "$cluster" "$image"
      loaded=$((loaded + 1))
    else
      echo "skipping $var=$image: not present in the local Docker image store"
    fi
  done
  if [[ $loaded -eq 0 ]]; then
    echo "no local images were loaded; the operator will pull the placeholders"
    echo "from config/manager/manager.yaml unless you patch them (see config/README.md)"
  fi
fi

cat <<EOF

Cluster 'kind-$cluster' is ready.

Next steps:
  1. label a node so the SimulationEnvironment selects it:
       kubectl label node $cluster-control-plane vcr.opendatahub-io/simulation=true
  2. install the operator:
       hack/install.sh --overlay config/overlays/local --with-sample
  3. tear down:
       hack/kind-up.sh --name $cluster --delete
EOF
