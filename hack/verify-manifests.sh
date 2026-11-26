#!/usr/bin/env bash
# verify-manifests.sh checks the deploy manifests without a cluster.
#
# It is the acceptance test for the manifests ticket: every check below asserts an
# externally observable property of the built kustomize output (an installable
# operator, matching admission registration, sufficient RBAC) or of the repository
# (CRD regeneration is idempotent, the CI workflow parses).
#
# Usage:
#   hack/verify-manifests.sh            # all checks
#   hack/verify-manifests.sh --kubectl  # additionally run kubectl apply --dry-run=client
#
# Requires: yq (mikefarah v4) and one of `kustomize` or `kubectl` (for
# `kubectl kustomize`). The CRD idempotency check needs controller-gen; set
# CONTROLLER_GEN to override the default $(go env GOPATH)/bin/controller-gen.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

KUBECTL_DRY_RUN=false
for arg in "$@"; do
  case "$arg" in
    --kubectl) KUBECTL_DRY_RUN=true ;;
    -h | --help)
      sed -n '2,14p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "unknown argument: $arg" >&2
      exit 2
      ;;
  esac
done

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}
ok() { echo "ok: $*"; }

command -v yq >/dev/null || {
  echo "yq (mikefarah v4) is required" >&2
  exit 2
}

if [[ -n "${KUSTOMIZE:-}" ]]; then
  build_cmd=("$KUSTOMIZE" build)
elif command -v kustomize >/dev/null; then
  build_cmd=(kustomize build)
elif command -v kubectl >/dev/null; then
  build_cmd=(kubectl kustomize)
else
  echo "kustomize (or kubectl for 'kubectl kustomize') is required" >&2
  exit 2
fi

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

# Must match config/default/kustomization.yaml and the OPERATOR_NAMESPACE
# fallback in cmd/manager/main.go.
OPERATOR_NAMESPACE="vllm-vcr-operator-system"

# select_docs prints a yq expression that keeps matching documents. Every check
# below wraps its selection in `[ ... ] | length > 0` so the result does not
# depend on which document happens to be last in the built stream.
DEPLOY_SEL='select(.kind == "Deployment" and .metadata.name == "vllm-vcr-operator-controller-manager")'
MUTATING_SEL='select(.kind == "MutatingWebhookConfiguration") | .webhooks[]'
VALIDATING_SEL='select(.kind == "ValidatingWebhookConfiguration") | .webhooks[]'

# build_has DESCRIPTION YQ_EXPRESSION
build_has() {
  local desc="$1" expr="$2"
  if yq ea -e "[ $expr ] | length > 0" "$BUILD" >/dev/null 2>&1; then
    ok "$desc"
  else
    fail "$desc"
  fi
}

########################################
# 1. Every kustomization builds and parses.
########################################
overlays=(config/default config/overlays/local)
for overlay in "${overlays[@]}"; do
  out="$workdir/$(basename "$overlay").yaml"
  if ! "${build_cmd[@]}" "$overlay" >"$out" 2>"$workdir/err"; then
    fail "kustomize build $overlay"
    sed 's/^/    /' "$workdir/err" >&2
    continue
  fi
  if ! yq '.' "$out" >/dev/null 2>&1; then
    fail "kustomize build $overlay: output is not valid YAML"
    continue
  fi
  ok "kustomize build $overlay"
done

BUILD="$workdir/default.yaml"
if [[ ! -s "$BUILD" ]]; then
  echo "cannot continue: config/default did not build" >&2
  exit 1
fi

########################################
# 2. The install set is complete: CRD, namespace, manager, webhook service and
#    registrations, certificates, RBAC, and no competing DaemonSet.
########################################
kinds="$(yq ea -r '.kind' "$BUILD" | sort | tr '\n' ' ')"
for want in CustomResourceDefinition Namespace Deployment Service MutatingWebhookConfiguration \
  ValidatingWebhookConfiguration Certificate Issuer ClusterRole ServiceAccount; do
  if grep -qw "$want" <<<"$kinds"; then
    ok "install set contains $want"
  else
    fail "install set is missing $want"
  fi
done

if grep -qw DaemonSet <<<"$kinds"; then
  fail "config/default must not contain a DaemonSet: the operator reconciles the device plugin"
else
  ok "no DaemonSet in config/default (operator-owned)"
fi

# Pod Security: the operator-owned device-plugin DaemonSet must mount
# /var/lib/kubelet/device-plugins as a hostPath, which the restricted and baseline
# levels forbid. A namespace labelled baseline (the previous value) makes the API
# server reject that DaemonSet, so no synthetic capacity is ever advertised.
build_has "operator namespace admits the device plugin's hostPath (Pod Security privileged)" \
  'select(.kind == "Namespace" and .metadata.name == "'"$OPERATOR_NAMESPACE"'")
   | .metadata.labels["pod-security.kubernetes.io/enforce"]
   | select(. == "privileged")'

build_has "VcrGpuCluster CRD is cluster scoped" \
  'select(.kind == "CustomResourceDefinition" and .spec.scope == "Cluster")'
build_has "CRD keeps the singleton CEL rule" \
  'select(.kind == "CustomResourceDefinition")
   | .spec.versions[0].schema.openAPIV3Schema["x-kubernetes-validations"][0]
   | select(.rule == "self.metadata.name == '"'"'default'"'"'")'

########################################
# 3. Manager Deployment: image, arguments, ports, env-injected images, certs.
########################################
args="$(yq ea -r "$DEPLOY_SEL"' | .spec.template.spec.containers[0].args[]' "$BUILD")"
for want in --leader-elect --metrics-bind-address=:8080 --health-probe-bind-address=:8081 --webhook-port=9443; do
  if grep -qx -- "$want" <<<"$args"; then
    ok "manager arg $want"
  else
    fail "manager Deployment is missing argument $want"
  fi
done

ports="$(yq ea -r "$DEPLOY_SEL"' | .spec.template.spec.containers[0].ports[] | "\(.containerPort)/\(.name)"' "$BUILD" | sort | tr '\n' ' ')"
for want in "8080/metrics" "8081/health" "9443/webhook"; do
  if [[ " $ports" == *" $want "* ]]; then
    ok "manager port $want"
  else
    fail "manager Deployment is missing port $want (have: $ports)"
  fi
done

envnames="$(yq ea -r "$DEPLOY_SEL"' | .spec.template.spec.containers[0].env[] | .name' "$BUILD" | sort | tr '\n' ' ')"
for want in OPERATOR_NAMESPACE VCR_FRONTEND_IMAGE VCR_BACKEND_IMAGE VCR_DEVICE_PLUGIN_IMAGE VCR_SUPERVISOR_IMAGE; do
  if [[ " $envnames" == *" $want "* ]]; then
    ok "manager env $want"
  else
    fail "manager Deployment is missing env $want (have: $envnames)"
  fi
done

build_has "OPERATOR_NAMESPACE is projected from the Pod namespace" \
  "$DEPLOY_SEL"' | .spec.template.spec.containers[0].env[]
   | select(.name == "OPERATOR_NAMESPACE"
     and .valueFrom.fieldRef.fieldPath == "metadata.namespace")'

build_has "manager mounts the webhook certificate Secret" \
  "$DEPLOY_SEL"' | .spec.template.spec.volumes[]
   | select(.name == "webhook-certs"
     and .secret.secretName == "vllm-vcr-operator-webhook-server-cert")'

build_has "cert-manager Certificate issues the mounted Secret" \
  'select(.kind == "Certificate" and .metadata.name == "vllm-vcr-operator-serving-cert"
     and .spec.secretName == "vllm-vcr-operator-webhook-server-cert")'

# Every injected image reference must be overridable and none may be a private
# default: placeholders live under the project's public registry namespace.
for name in VCR_FRONTEND_IMAGE VCR_BACKEND_IMAGE VCR_DEVICE_PLUGIN_IMAGE VCR_SUPERVISOR_IMAGE; do
  value="$(yq ea -r "$DEPLOY_SEL"' | .spec.template.spec.containers[0].env[]
    | select(.name == "'"$name"'") | .value' "$BUILD")"
  if [[ -z "$value" || "$value" == "null" ]]; then
    fail "$name has no default image reference"
  elif [[ "$value" != quay.io/opendatahub/* ]]; then
    fail "$name default ($value) must be a documented public placeholder"
  else
    ok "$name default $value"
  fi
done

########################################
# 4. Admission registration matches the webhook paths the Go code registers.
########################################
mutating_path='/mutate-v1-pod'
validating_path='/validate-vcr-opendatahub-io-v1alpha1-vcrgpucluster'

build_has "mutating webhook path $mutating_path" \
  "$MUTATING_SEL"' | select(.clientConfig.service.path == "'"$mutating_path"'")'

# The Pod webhook is fail-closed, so its Deployment must not be a single replica:
# one lost Pod would block Pod creation in every admitted namespace.
build_has "manager runs two admission replicas" \
  'select(.kind == "Deployment" and .metadata.name == "vllm-vcr-operator-controller-manager") | select(.spec.replicas >= 2)'

# Voluntary disruptions must not take both replicas at once.
build_has "manager has a PodDisruptionBudget keeping one replica" \
  'select(.kind == "PodDisruptionBudget" and .metadata.name == "vllm-vcr-operator-controller-manager") | select(.spec.minAvailable == 1)'

# The mutating webhook projects a pull Secret on non-dry-run requests, so its
# declared side effects are NoneOnDryRun; dry-run requests must write nothing.
build_has "mutating webhook is fail-closed, dry-run side-effect free, v1, Equivalent" \
  "$MUTATING_SEL"' | select(.failurePolicy == "Fail"
     and .sideEffects == "NoneOnDryRun"
     and .matchPolicy == "Equivalent"
     and (.admissionReviewVersions | any_c(. == "v1")))'

build_has "mutating webhook targets the operator webhook Service" \
  "$MUTATING_SEL"' | select(.clientConfig.service.name == "vllm-vcr-operator-webhook-service"
     and .clientConfig.service.namespace == "'"$OPERATOR_NAMESPACE"'")'

build_has "mutating webhook excludes the operator namespace" \
  "$MUTATING_SEL"' | .namespaceSelector.matchExpressions[]
   | select(.key == "kubernetes.io/metadata.name"
     and .operator == "NotIn"
     and (.values | any_c(. == "'"$OPERATOR_NAMESPACE"'")))'

# Admission is idempotent, so reinvocation is not requested (the field defaults to
# Never). A non-empty reinvocationPolicy must be a deliberate, documented change.
reinvoke="$(yq ea -r "$MUTATING_SEL"' | .reinvocationPolicy // "unset"' "$BUILD")"
if [[ "$reinvoke" == "unset" ]]; then
  ok "mutating webhook does not request reinvocation (idempotent)"
else
  fail "mutating webhook reinvocationPolicy is $reinvoke: admission is idempotent, leave it unset"
fi

build_has "validating webhook path $validating_path" \
  "$VALIDATING_SEL"' | select(.clientConfig.service.path == "'"$validating_path"'")'

build_has "validating webhook validates vcrgpuclusters, fail-closed" \
  "$VALIDATING_SEL"' | select(.failurePolicy == "Fail"
     and .sideEffects == "None"
     and (.admissionReviewVersions | any_c(. == "v1"))
     and (.rules[0].resources | any_c(. == "vcrgpuclusters")))'

# CA injection is wired through cert-manager; both registrations must point at the
# Certificate that also issues the manager's serving certificate.
for kind in MutatingWebhookConfiguration ValidatingWebhookConfiguration; do
  annotation="$(yq ea -r 'select(.kind == "'"$kind"'") | .metadata.annotations["cert-manager.io/inject-ca-from"] // ""' "$BUILD")"
  if [[ "$annotation" == "$OPERATOR_NAMESPACE/vllm-vcr-operator-serving-cert" ]]; then
    ok "$kind CA injection annotation"
  else
    fail "$kind must set cert-manager.io/inject-ca-from=$OPERATOR_NAMESPACE/vllm-vcr-operator-serving-cert (have: '$annotation')"
  fi
done

########################################
# 5. RBAC covers everything the operator touches, cluster-wide.
########################################
rbac_keys="$(yq ea -r '
  select(.kind == "ClusterRole") | .rules[]?
  | (.apiGroups // [""])[] as $g
  | (.resources // [])[] as $r
  | (.verbs // [])[] as $v
  | "\($g)|\($r)|\($v)"' "$BUILD" | sort -u)"

# require_rbac GROUP RESOURCE VERB...
require_rbac() {
  local group="$1" resource="$2"
  shift 2
  local verb
  for verb in "$@"; do
    if ! grep -qxF "$group|$resource|$verb" <<<"$rbac_keys"; then
      fail "RBAC is missing $group/$resource verb $verb"
    fi
  done
}

require_rbac "" pods get list watch
require_rbac "" secrets get list watch create update delete
require_rbac "" nodes get list watch
require_rbac apps daemonsets get list watch create update patch delete
require_rbac vcr.opendatahub-io vcrgpuclusters get list watch update patch
require_rbac vcr.opendatahub-io vcrgpuclusters/status get update patch
ok "RBAC covers pods, secrets, nodes, daemonsets, vcrgpuclusters(+status)"

build_has "manager ServiceAccount is bound to manager-role" \
  'select(.kind == "ClusterRoleBinding" and .roleRef.name == "manager-role")
   | .subjects[] | select(.kind == "ServiceAccount"
     and .name == "vllm-vcr-operator-controller-manager")'

# Leader election must be scoped to the operator namespace and to the lease name
# the manager requests (LeaderElectionID in cmd/manager/main.go).
build_has "leader election Role is scoped to the manager's lease name" \
  'select(.kind == "Role") | .rules[]
   | select(.resources | any_c(. == "leases"))
   | (.resourceNames // []) | any_c(. == "vllm-vcr-operator.vcr.opendatahub-io")'

########################################
# 6. Synthetic device advertisement only appears in the workload fixture, RBAC
#    documentation, and the README.
########################################
outside=0
while IFS= read -r hit; do
  case "$hit" in
    config/samples/* | config/rbac/* | config/README.md) ;;
    *)
      fail "nvidia.com/gpu referenced outside the workload fixture: $hit"
      outside=1
      ;;
  esac
done < <(grep -rl "nvidia.com/gpu" config/ 2>/dev/null || true)
[[ $outside -eq 0 ]] && ok "synthetic device name is confined to samples, RBAC comments, and docs"

########################################
# 7. Samples: the fixture is an unchanged GPU-requesting llm-d workload, and the
#    environment sample is the singleton.
########################################
sample_env=config/samples/vcr_v1alpha1_vcrgpucluster.yaml
sample_workload=config/samples/llmd-modelserver-deployment.yaml

if [[ "$(yq -r '.metadata.name' "$sample_env")" == "default" ]]; then
  ok "environment sample uses the singleton name"
else
  fail "$sample_env must be named default"
fi

if yq -e '.spec.template.spec.containers[].resources.requests["nvidia.com/gpu"]' "$sample_workload" >/dev/null 2>&1; then
  ok "workload sample requests nvidia.com/gpu"
else
  fail "$sample_workload must contain an unchanged GPU request"
fi
if yq -e '[.spec.template.metadata.labels, .metadata.labels]
  | any_c(keys | any_c(test("^llm-d\\.ai/")))' "$sample_workload" >/dev/null 2>&1; then
  ok "workload sample keeps llm-d labels"
else
  fail "$sample_workload must keep llm-d labels"
fi
if grep -qE '^\s+(vcr\.opendatahub-io/)?(injected|profile|profile-image):' "$sample_workload"; then
  fail "$sample_workload must not carry simulator opt-in metadata"
else
  ok "workload sample has no simulator metadata"
fi

########################################
# 8. CRD regeneration is idempotent (controller-gen output == committed base).
########################################
CONTROLLER_GEN="${CONTROLLER_GEN:-$(go env GOPATH)/bin/controller-gen}"
if [[ -x "$CONTROLLER_GEN" ]] || command -v "$CONTROLLER_GEN" >/dev/null; then
  mkdir -p "$workdir/gen/crd" "$workdir/gen/rbac" "$workdir/gen/webhook"
  if "$CONTROLLER_GEN" rbac:roleName=manager-role crd webhook paths="./..." \
    output:crd:dir="$workdir/gen/crd" \
    output:rbac:dir="$workdir/gen/rbac" \
    output:webhook:dir="$workdir/gen/webhook" 2>"$workdir/gen/err"; then
    if diff -ru config/crd/bases "$workdir/gen/crd" >"$workdir/gen/diff"; then
      ok "config/crd/bases matches controller-gen output"
    else
      fail "config/crd/bases is stale; run 'make manifests'"
      sed 's/^/    /' "$workdir/gen/diff" >&2
    fi
  else
    fail "controller-gen could not regenerate manifests"
    sed 's/^/    /' "$workdir/gen/err" >&2
  fi
  # A marker-generated webhook manifest would silently shadow the hand-written
  # registrations, which are the only ones the kustomization applies.
  if [[ -n "$(ls -A "$workdir/gen/webhook" 2>/dev/null)" ]]; then
    fail "controller-gen now emits a webhook manifest from kubebuilder markers; include it in config/webhook/kustomization.yaml (and drop the hand-written registrations) or remove the markers"
  fi
else
  echo "skip: controller-gen not found (set CONTROLLER_GEN to run the CRD idempotency check)"
fi

########################################
# 9. Every YAML file parses, and the CI workflow is syntactically valid.
########################################
yaml_fail=0
while IFS= read -r file; do
  yq '.' "$file" >/dev/null 2>&1 || {
    fail "$file is not valid YAML"
    yaml_fail=1
  }
done < <(find config hack .github -type f \( -name '*.yaml' -o -name '*.yml' \) 2>/dev/null)
[[ $yaml_fail -eq 0 ]] && ok "config/, hack/, .github/ YAML parses"

ci=.github/workflows/ci.yaml
if [[ -f "$ci" ]] && yq '.' "$ci" >/dev/null 2>&1; then
  jobs="$(yq -r '.jobs | keys | join(" ")' "$ci")"
  for want in build test-e2e manifests; do
    if [[ " $jobs " == *" $want "* ]]; then
      ok "CI job $want"
    else
      fail "CI workflow is missing job $want (have: $jobs)"
    fi
  done
else
  fail "$ci is missing or is not valid YAML"
fi

########################################
# 10. Optional: client-side kubectl validation.
########################################
if [[ "$KUBECTL_DRY_RUN" == true ]]; then
  if command -v kubectl >/dev/null; then
    if kubectl apply --dry-run=client -f "$BUILD" >/dev/null 2>"$workdir/kubectl.err"; then
      ok "kubectl apply --dry-run=client"
    else
      fail "kubectl apply --dry-run=client"
      sed 's/^/    /' "$workdir/kubectl.err" >&2
    fi
  else
    echo "skip: kubectl not found"
  fi
fi

if [[ $failures -gt 0 ]]; then
  echo "${failures} manifest check(s) failed" >&2
  exit 1
fi
echo "all manifest checks passed"
