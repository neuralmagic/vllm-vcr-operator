#!/usr/bin/env bash
# openshift-install.sh installs the vllm-vcr-operator into an OpenShift cluster.
#
# OpenShift differs from a plain cluster in three ways this script handles:
#
#   1. cert-manager is usually absent. The operator expects a serving-certificate
#      Secret, so this script generates a self-signed CA + serving certificate,
#      creates that Secret, and uses `hack/install.sh --no-cert-manager` (which
#      publishes caBundle from it).
#   2. The device-plugin DaemonSet mounts kubelet's device-plugin directory
#      (hostPath), runs as root, and must be able to connect to kubelet's socket,
#      which SELinux labels kubelet_var_lib_t. `restricted-v2` rejects all of that,
#      so the plugin's dedicated ServiceAccount is bound to the `privileged` SCC.
#      This is the same permission the NVIDIA device plugin needs on OpenShift; it
#      is about reaching kubelet's socket, not about GPUs: the plugin requests no
#      device, mounts no NVIDIA runtime, and drops every capability.
#   3. A fail-closed Pod admission webhook is cluster-wide risk on a shared
#      cluster: if the operator is down, every Pod creation fails. With
#      --admission-namespaces the registrations are scoped to namespaces carrying
#      the label vcr.opendatahub-io/admission=enabled, and this script labels the
#      given namespaces. Use it whenever the cluster is not dedicated to tests.
#
# Usage:
#   hack/openshift-install.sh [--device-plugin-scc SCC] [--admission-namespaces NS[,NS...]]
#                             [--operator-image REF] [--device-plugin-image REF]
#                             [--frontend-image REF] [--backend-image REF]
#                             [--skip-scc] [--yes] [--delete]
#
#   --device-plugin-scc SCC   SCC bound to the device-plugin ServiceAccount
#                             (default: privileged; SELinux forbids a narrower SCC
#                             from connecting to kubelet's socket)
#   --admission-namespaces    comma-separated namespaces the Pod webhooks admit;
#                             omit to keep the shipped cluster-wide registrations
#   --operator-image REF      manager image (default: built image reference)
#   --device-plugin-image REF device-plugin image (default: built reference)
#   --frontend-image REF      Python vLLM frontend image (default: built reference)
#   --backend-image REF       vllm-vcr simulator image (default: built reference)
#   --skip-scc                do not touch SCC bindings (cluster already prepared)
#   --delete                  remove the operator, CRD, SCC bindings and namespace
#   --yes                     do not prompt
#
# The operator namespace is fixed at vllm-vcr-operator-system: the CRD, RBAC and
# webhook registrations are generated for that name, and the webhook Service
# namespace is part of the certificate SANs.
#
# Environment:
#   VCR_IMAGE_PREFIX        image prefix used by hack/images.sh (default: docker.io/vcrlocal)
#   VCR_IMAGE_TAG           image tag used by hack/images.sh (default: test)
#   OPENSHIFT_SCC_BINDINGS  space-separated extra SCCs to bind (default: none)
#
# Requires: kubectl or oc, openssl, yq, kustomize (or kubectl kustomize).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

namespace="vllm-vcr-operator-system"
scc="privileged"
admission_namespaces=""
operator_image=""
device_plugin_image=""
frontend_image=""
backend_image=""
skip_scc=false
delete=false
assume_yes=false

usage() { sed -n '2,52p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --device-plugin-scc) scc="${2:?}"; shift 2 ;;
    --admission-namespaces) admission_namespaces="${2:?}"; shift 2 ;;
    --operator-image) operator_image="${2:?}"; shift 2 ;;
    --device-plugin-image) device_plugin_image="${2:?}"; shift 2 ;;
    --frontend-image) frontend_image="${2:?}"; shift 2 ;;
    --backend-image) backend_image="${2:?}"; shift 2 ;;
    --skip-scc) skip_scc=true; shift ;;
    --delete) delete=true; shift ;;
    --yes) assume_yes=true; shift ;;
    -h | --help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if command -v oc >/dev/null; then kube=(oc); else kube=(kubectl); fi
for bin in openssl yq; do
  command -v "$bin" >/dev/null || { echo "$bin is required" >&2; exit 2; }
done

prefix="${VCR_IMAGE_PREFIX:-docker.io/vcrlocal}"
tag="${VCR_IMAGE_TAG:-test}"
operator_image="${operator_image:-$prefix/vllm-vcr-operator:$tag}"
device_plugin_image="${device_plugin_image:-$prefix/vllm-vcr-device-plugin:$tag}"
frontend_image="${frontend_image:-$prefix/vllm-vcr-frontend-stub:$tag}"
backend_image="${backend_image:-$prefix/vllm-vcr-backend-stub:$tag}"

webhook_service="vllm-vcr-operator-webhook-service"
cert_secret="vllm-vcr-operator-webhook-server-cert"
device_plugin_sa="vllm-vcr-device-plugin"

if [[ "$delete" == true ]]; then
  echo "removing webhook registrations (so Pod creation stops depending on the operator)"
  "${kube[@]}" delete mutatingwebhookconfiguration vllm-vcr-operator-mutating-webhook --ignore-not-found
  "${kube[@]}" delete validatingwebhookconfiguration vllm-vcr-operator-validating-webhook --ignore-not-found
  echo "removing SimulationEnvironment custom resource definition"
  "${kube[@]}" delete crd simulationenvironments.vcr.opendatahub-io --ignore-not-found
  if [[ "$skip_scc" == false ]] && command -v oc >/dev/null; then
    oc adm policy remove-scc-from-user "$scc" -z "$device_plugin_sa" -n "$namespace" >/dev/null 2>&1 || true
  fi
  echo "deleting namespace $namespace"
  "${kube[@]}" delete namespace "$namespace" --ignore-not-found --wait=false
  exit 0
fi

if ! "${kube[@]}" get clusterversion version >/dev/null 2>&1; then
  echo "this is not an OpenShift cluster (no clusterversion object)." >&2
  echo "On plain Kubernetes use hack/install.sh with cert-manager instead." >&2
  exit 1
fi

echo "installing into OpenShift namespace $namespace"

if [[ "$assume_yes" != true ]]; then
  cat <<EOF

This will:
  - create/update namespace $namespace (Pod Security: privileged)
  - create the webhook serving certificate Secret $cert_secret (self-signed)
  - apply CRD, RBAC, manager and webhook Service with:
      manager:  $operator_image
      frontend: $frontend_image
      backend:  $backend_image
      plugin:   $device_plugin_image
  - apply the fail-closed webhook registrations$( [[ -n "$admission_namespaces" ]] && echo " scoped to: $admission_namespaces" || echo " cluster-wide" )
EOF
  if [[ "$skip_scc" == false ]]; then
    echo "  - bind SCC $scc to ServiceAccount $namespace:$device_plugin_sa"
  fi
  read -r -p "Continue? [y/N] " reply
  [[ "$reply" == "y" || "$reply" == "Y" ]] || { echo "aborted"; exit 1; }
fi

# 1. Namespace first: the certificate Secret and the SCC RoleBinding live there.
"${kube[@]}" create namespace "$namespace" --dry-run=client -o yaml | "${kube[@]}" apply -f - >/dev/null
"${kube[@]}" label namespace "$namespace" \
  pod-security.kubernetes.io/enforce=privileged \
  pod-security.kubernetes.io/audit=privileged \
  pod-security.kubernetes.io/warn=privileged \
  app.kubernetes.io/name=vllm-vcr-operator --overwrite >/dev/null

# 2. Serving certificate. SANs cover the in-cluster Service name; ca.crt is what
#    install.sh --no-cert-manager publishes as caBundle.
if "${kube[@]}" -n "$namespace" get secret "$cert_secret" >/dev/null 2>&1; then
  echo "reusing existing Secret $namespace/$cert_secret"
else
  echo "generating a self-signed webhook serving certificate"
  workdir="$(mktemp -d)"
  trap 'rm -rf "$workdir"' EXIT
  openssl req -x509 -newkey rsa:2048 -nodes -days 730 \
    -keyout "$workdir/ca.key" -out "$workdir/ca.crt" \
    -subj "/CN=vllm-vcr-operator-webhook-ca" >/dev/null 2>&1
  openssl req -newkey rsa:2048 -nodes \
    -keyout "$workdir/tls.key" -out "$workdir/tls.csr" \
    -subj "/CN=$webhook_service.$namespace.svc" >/dev/null 2>&1
  cat >"$workdir/san.cnf" <<EOF
subjectAltName = DNS:$webhook_service, DNS:$webhook_service.$namespace, DNS:$webhook_service.$namespace.svc, DNS:$webhook_service.$namespace.svc.cluster.local, DNS:localhost, IP:127.0.0.1
extendedKeyUsage = serverAuth
EOF
  openssl x509 -req -in "$workdir/tls.csr" -CA "$workdir/ca.crt" -CAkey "$workdir/ca.key" \
    -CAcreateserial -days 730 -extfile "$workdir/san.cnf" -out "$workdir/tls.crt" >/dev/null 2>&1
  "${kube[@]}" -n "$namespace" create secret tls "$cert_secret" \
    --cert="$workdir/tls.crt" --key="$workdir/tls.key" >/dev/null
  "${kube[@]}" -n "$namespace" patch secret "$cert_secret" --type merge \
    -p "{\"data\":{\"ca.crt\":\"$(base64 -w0 "$workdir/ca.crt")\"}}" >/dev/null
fi

# 3. SCC binding. Created before the DaemonSet exists: the RoleBinding names the
#    ServiceAccount, so it applies as soon as the operator creates it.
if [[ "$skip_scc" == false ]]; then
  if ! command -v oc >/dev/null; then
    echo "oc is required to bind SCC $scc; re-run with --skip-scc after binding it yourself" >&2
    exit 1
  fi
  echo "binding SCC $scc to ServiceAccount $namespace:$device_plugin_sa"
  oc adm policy add-scc-to-user "$scc" -z "$device_plugin_sa" -n "$namespace" >/dev/null
  for extra in ${OPENSHIFT_SCC_BINDINGS:-}; do
    echo "binding SCC $extra to ServiceAccount $namespace:$device_plugin_sa"
    oc adm policy add-scc-to-user "$extra" -z "$device_plugin_sa" -n "$namespace" >/dev/null
  done
fi

# Split the image reference at the tag delimiter that follows the last slash, so
# a registry with a port (host:5000/repo:tag) is parsed correctly.
operator_repo="${operator_image}"
operator_tag=""
if [[ "${operator_image##*/}" == *:* ]]; then
  operator_repo="${operator_image%:*}"
  operator_tag="${operator_image##*:}"
fi

# 4. Overlay: operator images + runtime image env, on top of the shipped default
#    (install.sh strips the cert-manager objects itself).
overlay_root="$(mktemp -d)"
trap 'rm -rf "$overlay_root"' EXIT
cp -r config "$overlay_root/config"
mkdir -p "$overlay_root/config/openshift-install"
cat >"$overlay_root/config/openshift-install/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../default
images:
  - name: quay.io/opendatahub/vllm-vcr-operator
    newName: ${operator_repo}
    newTag: ${operator_tag}
patches:
  - path: runtime-images.yaml
EOF
# The in-cluster registry serves with a certificate from OpenShift's service CA.
# The operator resolves profile metadata over TLS, so it must trust that CA; the
# CRI (kubelet) already does. Mount the published CA bundle and point Go at it.
service_ca_trust=""
if "${kube[@]}" -n "$namespace" get configmap openshift-service-ca.crt >/dev/null 2>&1; then
  echo "trusting the OpenShift service CA for in-cluster registry access"
  cat >"$overlay_root/config/openshift-install/service-ca.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vllm-vcr-operator-controller-manager
  namespace: $namespace
spec:
  template:
    spec:
      containers:
        - name: manager
          env:
            - name: SSL_CERT_FILE
              value: /etc/pki/service-ca/service-ca.crt
          volumeMounts:
            - name: service-ca
              mountPath: /etc/pki/service-ca
              readOnly: true
      volumes:
        - name: service-ca
          configMap:
            name: openshift-service-ca.crt
EOF
  service_ca_trust='  - path: service-ca.yaml'
fi

cat >"$overlay_root/config/openshift-install/runtime-images.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vllm-vcr-operator-controller-manager
  namespace: $namespace
spec:
  template:
    spec:
      containers:
        - name: manager
          env:
            - name: VCR_DEVICE_PLUGIN_IMAGE
              value: "$device_plugin_image"
            - name: VCR_FRONTEND_IMAGE
              value: "$frontend_image"
            - name: VCR_BACKEND_IMAGE
              value: "$backend_image"
EOF
sed -i "s|  - path: runtime-images.yaml|  - path: runtime-images.yaml\n${service_ca_trust}|" \
  "$overlay_root/config/openshift-install/kustomization.yaml"

echo "applying the operator (certificates via the pre-created Secret)"
hack/install.sh --no-cert-manager --overlay "$overlay_root/config/openshift-install"

# 5. Optional admission scoping for shared clusters: only labelled namespaces are
#    admitted, so a stopped operator cannot block the rest of the cluster.
if [[ -n "$admission_namespaces" ]]; then
  echo "scoping the Pod admission webhooks to labelled namespaces"
  for ns in ${admission_namespaces//,/ }; do
    "${kube[@]}" create namespace "$ns" --dry-run=client -o yaml | "${kube[@]}" apply -f - >/dev/null
    "${kube[@]}" label namespace "$ns" vcr.opendatahub-io/admission=enabled --overwrite >/dev/null
  done
  scope_patch='[{"op":"replace","path":"/webhooks/0/namespaceSelector","value":{"matchLabels":{"vcr.opendatahub-io/admission":"enabled"}}}]'
  for cfg in mutatingwebhookconfiguration/vllm-vcr-operator-mutating-webhook \
    validatingwebhookconfiguration/vllm-vcr-operator-validating-webhook; do
    "${kube[@]}" patch "$cfg" --type=json -p "$scope_patch" >/dev/null
  done
fi

echo
echo "installed. next steps:"
echo "  1. label the nodes that should advertise synthetic GPUs, matching the environment:"
echo "       kubectl label node <node> vcr.opendatahub-io/simulation=true"
echo "  2. apply a SimulationEnvironment:"
echo "       kubectl apply -f test/fixtures/environments/h200.yaml"
echo "  3. check reconciliation:"
echo "       kubectl get simulationenvironment default -o yaml"
echo "       kubectl -n $namespace get daemonset,pods"
