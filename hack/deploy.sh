#!/usr/bin/env bash
# deploy.sh installs the operator and proves it can admit one workload.
#
# Generic to Kubernetes, with two OpenShift differences handled here rather than in the
# operator:
#
#   1. The device plugin runs privileged (it serves the kubelet device-plugin socket and
#      reads host GPU state), so OpenShift needs the privileged SCC bound to its
#      ServiceAccount. Plain Kubernetes has no SCC; its Pod Security Admission level is
#      already set by install.sh.
#   2. OpenShift issues its own serving certificates for the API, nothing else. Webhook
#      certificates come from cert-manager on either platform, or from a pre-created Secret
#      with --no-cert-manager.
#
# Everything else — CRDs, RBAC, manager, webhook registrations, the VcrGpuCluster, the
# namespaced VcrGpuProfile, admission and the simulated workload — is the same API on both.
#
# Why this exists next to install.sh: install.sh takes images from a kustomize overlay, but
# the images the operator actually runs come from the manager's VCR_*_IMAGE environment
# variables, whose defaults are placeholders. Pointing only the manager image at a real
# registry leaves the device plugin pulling a reference that does not exist, which shows up
# as DevicePluginReady=False and every workload rejected. This script writes that overlay.
#
# Usage:
#   hack/deploy.sh [--operator-image REF] [--device-plugin-image REF]
#                  [--frontend-image REF] [--backend-image REF]
#                  [--namespace NS] [--profile-image REF] [--workload-namespace NS]
#                  [--smoke-workload] [--skip-scc] [--no-cert-manager] [--dry-run]
#
#   --profile-image REF       register one profile in the VcrGpuCluster (optional)
#   --smoke-workload          deploy a GPU-requesting modelserver and wait for it to be Ready
#   --dry-run                 render everything, apply nothing
#
# Requires: kubectl, kustomize (or kubectl kustomize), and oc on OpenShift.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
namespace="vllm-vcr-operator-system"
workload_namespace="vllm-vcr-simulated"
profile_image=""
smoke=false
skip_scc=false
cert_manager_args=()
dry_run=false

operator_image=${VCR_OPERATOR_IMAGE:-ghcr.io/neuralmagic/vllm-vcr-operator:0.4.1-e2e}
device_plugin_image=${VCR_DEVICE_PLUGIN_IMAGE:-ghcr.io/neuralmagic/vllm-vcr-device-plugin:0.4.0-e2e}
frontend_image=${VCR_FRONTEND_IMAGE:-ghcr.io/neuralmagic/vllm-vcr-frontend:0.2.5-vllm0.33}
backend_image=${VCR_BACKEND_IMAGE:-ghcr.io/neuralmagic/vllm-vcr:0.2.5-vllm0.33}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --operator-image) operator_image="${2:?}"; shift 2 ;;
    --device-plugin-image) device_plugin_image="${2:?}"; shift 2 ;;
    --frontend-image) frontend_image="${2:?}"; shift 2 ;;
    --backend-image) backend_image="${2:?}"; shift 2 ;;
    --namespace) namespace="${2:?}"; shift 2 ;;
    --profile-image) profile_image="${2:?}"; shift 2 ;;
    --workload-namespace) workload_namespace="${2:?}"; shift 2 ;;
    --smoke-workload) smoke=true; shift ;;
    --skip-scc) skip_scc=true; shift ;;
    --no-cert-manager) cert_manager_args=(--no-cert-manager); shift ;;
    --dry-run) dry_run=true; shift ;;
    -h|--help) sed -n '2,40p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

kube=(kubectl)
on_openshift=false
if command -v oc >/dev/null && oc get clusterversion version >/dev/null 2>&1; then
  on_openshift=true
  kube=(oc)
fi
echo "platform: $( $on_openshift && echo OpenShift || echo Kubernetes )"

# The overlay carries both the manager image and the three images the operator injects.
overlay=$(mktemp -d)
trap 'rm -rf "$overlay"' EXIT
cat >"$overlay/kustomization.yaml" <<YAML
resources:
  - ${repo_root}/config/default
images:
  - name: quay.io/opendatahub/vllm-vcr-operator
    newName: ${operator_image%%:*}
    newTag: ${operator_image##*:}
patches:
  - target:
      kind: Deployment
      name: vllm-vcr-operator-controller-manager
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: vllm-vcr-operator-controller-manager
      spec:
        template:
          spec:
            containers:
              - name: manager
                env:
                  - {name: VCR_DEVICE_PLUGIN_IMAGE, value: "${device_plugin_image}"}
                  - {name: VCR_FRONTEND_IMAGE, value: "${frontend_image}"}
                  - {name: VCR_BACKEND_IMAGE, value: "${backend_image}"}
YAML
# Kustomize refuses a kustomization outside its root, so stage it inside the repo.
staged="${repo_root}/.deploy-overlay"
rm -rf "$staged"
cp -r "$overlay" "$staged"
sed -i "s|${repo_root}/config/default|../config/default|" "$staged/kustomization.yaml"
trap 'rm -rf "$overlay" "$staged"' EXIT

if $dry_run; then
  kubectl kustomize "$staged" >/dev/null
  echo "overlay renders; nothing applied"
  exit 0
fi

"$repo_root/hack/install.sh" --overlay "$staged" "${cert_manager_args[@]}"

# OpenShift only: the device plugin needs the privileged SCC to reach the kubelet socket.
if $on_openshift && ! $skip_scc; then
  sa="${namespace}:vllm-vcr-device-plugin"
  echo "binding SCC privileged to ServiceAccount ${sa}"
  oc adm policy add-scc-to-user privileged -z vllm-vcr-device-plugin -n "$namespace" >/dev/null
fi

# One cluster-scoped configuration. Without a profile it advertises GPUs and admits nothing.
kubectl apply -f - <<YAML
apiVersion: vcr.opendatahub-io/v1alpha1
kind: VcrGpuCluster
metadata:
  name: default
spec:
  simulationNodes:
    nodeSelector:
      kubernetes.io/os: linux
  devicesPerNode: 8
  gpuType: H200
  $( [[ -n "$profile_image" ]] && printf 'profiles:\n    - image: %s' "$profile_image" )
  runtime:
    frontend:
      requests: {cpu: "500m", memory: 1Gi}
      limits:   {cpu: "1", memory: 2Gi}
    backend:
      requests: {cpu: "750m", memory: 2Gi}
      limits:   {cpu: "1500m", memory: 3Gi}
    provisioner:
      requests: {cpu: 50m, memory: 64Mi}
      limits:   {cpu: 100m, memory: 128Mi}
YAML

echo "waiting for the VcrGpuCluster to publish its catalog"
kubectl wait --for=condition=Ready vgc/default --timeout=300s

if ! $smoke; then
  echo "installed. Add --profile-image REF --smoke-workload to admit a workload end to end."
  exit 0
fi

[[ -n "$profile_image" ]] || { echo "--smoke-workload needs --profile-image" >&2; exit 2; }
kubectl create namespace "$workload_namespace" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$workload_namespace" apply -f "$repo_root/config/samples/llmd-modelserver-serviceaccount.yaml"
# The sample asks for a context longer than the recorded per-step budget; the profile is a
# measurement and does not move, so the workload is fitted to it.
sed 's/--max-model-len=32768/--max-model-len=8192/' \
  "$repo_root/config/samples/llmd-modelserver-deployment.yaml" |
  kubectl -n "$workload_namespace" apply -f -

kubectl -n "$workload_namespace" rollout status deploy/mock-prefix-cache --timeout=900s
kubectl -n "$workload_namespace" get pods \
  -o custom-columns='NAME:.metadata.name,READY:.status.containerStatuses[*].ready,PROFILE:.metadata.annotations.vcr\.opendatahub-io/profile'
