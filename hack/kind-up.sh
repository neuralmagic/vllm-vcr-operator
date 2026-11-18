#!/usr/bin/env bash
# kind-up.sh creates the two-node kind cluster the acceptance suite uses.
#
# The cluster has one control-plane node and one worker node so that placement on
# a simulation node is observable: the worker is the node the suite labels
#   vcr.opendatahub-io/simulation=true
# and the control-plane node must never receive the synthetic device plugin.
#
# kind runs Kubernetes nodes as containers, so it needs a container runtime. This
# script uses Docker when its daemon is reachable and rootless podman otherwise
# (KIND_EXPERIMENTAL_PROVIDER=podman), and fails with an explicit message when
# neither works instead of silently doing nothing.
#
# Usage:
#   hack/kind-up.sh [--name NAME] [--delete] [--force] [--no-label]
#
#   --name NAME   kind cluster name (default: vcr-e2e)
#   --delete      delete the cluster and exit
#   --force       delete and recreate the cluster even if it exists
#   --no-label    do not label the worker as a simulation node
#
# After the cluster exists, load the images with:
#   hack/images.sh --build --stub --load
#
# Images are loaded by hack/images.sh, not by this script: `kind load` does not
# work with the podman provider (see test/cluster/README.md).
set -euo pipefail

cluster="vcr-e2e"
delete=false
force=false
label=true

usage() {
  sed -n '2,24p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
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
    --force)
      force=true
      shift
      ;;
    --no-label)
      label=false
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

# Resolve the kind provider: Docker if its daemon answers, podman otherwise.
provider_args=()
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  provider=docker
elif command -v podman >/dev/null 2>&1; then
  provider=podman
  export KIND_EXPERIMENTAL_PROVIDER=podman
else
  echo "kind needs a container runtime: neither a running Docker daemon nor podman is available." >&2
  echo "Install podman (rootless podman 5.x works) or start Docker." >&2
  exit 1
fi
echo "kind provider: $provider"

if [[ "$delete" == true ]]; then
  if kind get clusters 2>/dev/null | grep -qx "$cluster"; then
    kind delete cluster --name "$cluster"
  else
    echo "kind cluster '$cluster' does not exist"
  fi
  exit 0
fi

if kind get clusters 2>/dev/null | grep -qx "$cluster"; then
  if [[ "$force" == true ]]; then
    echo "deleting existing kind cluster '$cluster'"
    kind delete cluster --name "$cluster"
  else
    echo "kind cluster '$cluster' already exists: reusing it"
  fi
fi

if ! kind get clusters 2>/dev/null | grep -qx "$cluster"; then
  workdir="$(mktemp -d)"
  trap 'rm -rf "$workdir"' EXIT
  cat >"$workdir/cluster.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: $cluster
nodes:
  # Two nodes so that placement is observable: only the worker is labelled as a
  # simulation node, so a Pod that schedules on the control-plane node would be a
  # visible placement bug rather than a hidden one.
  - role: control-plane
  - role: worker
    labels:
      vcr.opendatahub-io/simulation: "true"
EOF
  echo "creating kind cluster '$cluster' (control-plane + worker)"
  kind create cluster --config "$workdir/cluster.yaml" --wait 120s
fi

kubectl config use-context "kind-$cluster" >/dev/null

if [[ "$label" == true ]]; then
  # kind's `labels:` node field is applied at node creation; re-apply so a reused
  # cluster is labelled too (idempotent).
  worker="$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  if [[ -n "$worker" ]]; then
    kubectl label node "$worker" vcr.opendatahub-io/simulation=true --overwrite >/dev/null
    echo "labelled $worker as a simulation node (vcr.opendatahub-io/simulation=true)"
  else
    echo "warning: no worker node found; the simulation node label was not applied" >&2
  fi
fi

kubectl wait --for=condition=Ready nodes --all --timeout=120s >/dev/null

cat <<EOF

Cluster 'kind-$cluster' is ready (nodes: $(kubectl get nodes --no-headers | wc -l | tr -d ' ')).

Next steps:
  1. build and load the images (never requires Docker):
       hack/images.sh --build --stub --load
  2. run the acceptance suite (it installs the operator, the webhook certificates,
     and a test TLS registry itself):
       make test-cluster
  3. tear down:
       hack/kind-up.sh --name $cluster --delete
EOF
