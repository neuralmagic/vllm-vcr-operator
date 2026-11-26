#!/usr/bin/env bash
# install.sh installs the vllm-vcr-operator into the current kubeconfig cluster.
#
# The webhook registrations are fail-closed, so they are applied only after the
# manager (and therefore the webhook Service) is available: a fail-closed webhook
# with no backend would reject Pod creation cluster-wide.
#
# Usage:
#   hack/install.sh [--overlay DIR] [--with-sample] [--no-cert-manager] [--dry-run]
#
#   --overlay DIR      kustomization to apply (default: config/default)
#   --with-sample      also apply config/samples after the operator is ready
#   --no-cert-manager  install without cert-manager objects; you must pre-create
#                      the webhook certificate Secret and set caBundle yourself
#                      (see config/README.md)
#   --dry-run          print the two apply phases instead of applying them
#
# Requires: kubectl, yq (mikefarah v4), and kustomize or `kubectl kustomize`.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

overlay="config/default"
with_sample=false
use_cert_manager=true
dry_run=false

usage() {
  sed -n '2,19p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --overlay)
      overlay="${2:?--overlay needs a directory}"
      shift 2
      ;;
    --with-sample)
      with_sample=true
      shift
      ;;
    --no-cert-manager)
      use_cert_manager=false
      shift
      ;;
    --dry-run)
      dry_run=true
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

for bin in kubectl yq; do
  command -v "$bin" >/dev/null || {
    echo "$bin is required" >&2
    exit 2
  }
done

if [[ -n "${KUSTOMIZE:-}" ]]; then
  build_cmd=("$KUSTOMIZE" build)
elif command -v kustomize >/dev/null; then
  build_cmd=(kustomize build)
else
  build_cmd=(kubectl kustomize)
fi

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

echo "building $overlay"
"${build_cmd[@]}" "$overlay" >"$workdir/raw.yaml"

# Phase 1: everything except the fail-closed admission registrations.
yq ea 'select(.kind != "MutatingWebhookConfiguration" and .kind != "ValidatingWebhookConfiguration")' \
  "$workdir/raw.yaml" >"$workdir/core.yaml"
# Phase 2: the registrations themselves.
yq ea 'select(.kind == "MutatingWebhookConfiguration" or .kind == "ValidatingWebhookConfiguration")' \
  "$workdir/raw.yaml" >"$workdir/webhooks.yaml"

if [[ "$use_cert_manager" == false ]]; then
  echo "omitting cert-manager objects: Secret vllm-vcr-operator-webhook-server-cert must already exist"
  yq ea 'select(.kind != "Certificate" and .kind != "Issuer")' "$workdir/core.yaml" >"$workdir/core.yaml.tmp"
  mv "$workdir/core.yaml.tmp" "$workdir/core.yaml"
elif [[ "$dry_run" == false ]] && ! kubectl get crd certificates.cert-manager.io >/dev/null 2>&1; then
  echo "cert-manager is not installed: the Certificate/Issuer objects cannot be applied." >&2
  echo "Install cert-manager, or re-run with --no-cert-manager and pre-create the webhook Secret." >&2
  exit 1
fi

if [[ "$dry_run" == true ]]; then
  echo "--- phase 1: namespace, CRD, RBAC, certificates, manager ---"
  cat "$workdir/core.yaml"
  echo "--- phase 2: webhook registrations (after the manager is Available) ---"
  cat "$workdir/webhooks.yaml"
  exit 0
fi

echo "phase 1: applying namespace, CRD, RBAC, certificates, and manager"
kubectl apply -f "$workdir/core.yaml"

echo "waiting for the VcrGpuCluster CRD"
kubectl wait --for=condition=Established --timeout=60s \
  crd/vcrgpuclusters.vcr.opendatahub-io

echo "waiting for the operator manager rollout"
if ! kubectl -n vllm-vcr-operator-system rollout status \
  deployment/vllm-vcr-operator-controller-manager --timeout=180s; then
  echo "the manager did not become ready; the webhook registrations were NOT applied." >&2
  echo "Inspect: kubectl -n vllm-vcr-operator-system describe deploy/vllm-vcr-operator-controller-manager" >&2
  echo "(a missing webhook certificate Secret is the usual cause)" >&2
  exit 1
fi

echo "phase 2: applying the fail-closed webhook registrations"
kubectl apply -f "$workdir/webhooks.yaml"

if [[ "$use_cert_manager" == false ]]; then
  # Without cert-manager nothing fills caBundle, so publish the CA from the
  # pre-created Secret. Prefer ca.crt, fall back to the certificate itself.
  secret_ns=vllm-vcr-operator-system
  secret=vllm-vcr-operator-webhook-server-cert
  ca="$(kubectl -n "$secret_ns" get secret "$secret" -o jsonpath='{.data.ca\.crt}' 2>/dev/null || true)"
  if [[ -z "$ca" ]]; then
    ca="$(kubectl -n "$secret_ns" get secret "$secret" -o jsonpath='{.data.tls\.crt}' 2>/dev/null || true)"
  fi
  if [[ -z "$ca" ]]; then
    echo "cannot read ca.crt/tls.crt from Secret $secret_ns/$secret: set caBundle on both webhook" >&2
    echo "configurations manually (see config/README.md)" >&2
    exit 1
  fi
  # Every entry needs the CA: the validating configuration serves more than one
  # webhook (VcrGpuCluster and VcrGpuProfile), and an entry with an empty caBundle
  # makes the API server reject the objects it guards.
  for cfg in mutatingwebhookconfiguration/vllm-vcr-operator-mutating-webhook \
    validatingwebhookconfiguration/vllm-vcr-operator-validating-webhook; do
    count=$(kubectl get "$cfg" -o jsonpath='{.webhooks[*].name}' | wc -w)
    ops=""
    for ((i = 0; i < count; i++)); do
      ops+="{\"op\":\"add\",\"path\":\"/webhooks/$i/clientConfig/caBundle\",\"value\":\"$ca\"},"
    done
    kubectl patch "$cfg" --type=json -p "[${ops%,}]" >/dev/null
  done
  echo "published caBundle from $secret_ns/$secret"
fi

if [[ "$with_sample" == true ]]; then
  echo "applying the sample VcrGpuCluster and modelserver"
  kubectl apply -k config/samples
  echo "the sample modelserver stays Pending until a node carries the"
  echo "vcr.opendatahub-io/simulation=true label selected by the environment."
else
  echo "next: apply a VcrGpuCluster, for example"
  echo "  kubectl apply -k config/samples"
fi
