#!/usr/bin/env bash
# images.sh builds the operator images and loads them into a local kind cluster.
#
# It works with rootless podman as well as with Docker, and it never requires
# Docker: when Docker is unavailable podman is used for both building and loading.
#
# Why not `kind load`: with the podman provider `kind load docker-image` cannot
# see podman's image store, and `kind load image-archive` silently does nothing.
# The working recipe (verified; see test/cluster/README.md) is:
#
#   tag the image with a fully qualified reference  (docker.io/vcrlocal/...)
#   save it to a tar                                ($runtime save -q -o)
#   import it into every node's containerd          (ctr -n k8s.io images import)
#
# Pods must reference the same fully qualified name with imagePullPolicy
# IfNotPresent (what the operator already sets), or kubelet asks the registry for
# docker.io/library/... and fails with ErrImageNeverPull.
#
# Usage:
#   hack/images.sh [--list] [--build] [--stub] [--load] [--all]
#
#   --list    print the image references this script manages, one per line
#   --build   build the operator images (manager, device plugin)
#   --stub    build the test-only runtime images under test/images (frontend stub
#             containing the real vcr-supervisor, backend stub, profile fixture)
#   --load    import the images into the kind cluster's nodes
#   --all     --build --stub --load
#
# Environment:
#   VCR_IMAGE_RUNTIME   container runtime to build/load with (default: docker if
#                       its daemon is reachable, otherwise podman)
#   VCR_CLUSTER_NAME    kind cluster name for --load (default: vcr-e2e)
#   VCR_IMAGE_PREFIX    image repository prefix (default: docker.io/vcrlocal)
#   VCR_IMAGE_TAG       image tag (default: test)
#
# The test-only images are test doubles: see test/images/README.md. Nothing in
# internal/, config/, or the root Dockerfiles references them.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

prefix="${VCR_IMAGE_PREFIX:-docker.io/vcrlocal}"
tag="${VCR_IMAGE_TAG:-test}"
cluster="${VCR_CLUSTER_NAME:-vcr-e2e}"

manager_image="$prefix/vllm-vcr-operator:$tag"
device_plugin_image="$prefix/vllm-vcr-device-plugin:$tag"
frontend_stub_image="$prefix/vllm-vcr-frontend-stub:$tag"
backend_stub_image="$prefix/vllm-vcr-backend-stub:$tag"
profile_fixture_image="$prefix/vllm-vcr-profile-fixture:$tag"
mock_profile_image="$prefix/vllm-vcr-profile-mock:$tag"

operator_images=("$manager_image" "$device_plugin_image")
stub_images=("$frontend_stub_image" "$backend_stub_image" "$profile_fixture_image" "$mock_profile_image")

build=false
stub=false
load=false
list=false

usage() {
  sed -n '2,40p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --build) build=true ;;
    --stub) stub=true ;;
    --load) load=true ;;
    --all) build=true; stub=true; load=true ;;
    --list) list=true ;;
    -h | --help) usage; exit 0 ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done

if [[ "$build" == false && "$stub" == false && "$load" == false && "$list" == false ]]; then
  usage
  exit 2
fi

if [[ "$list" == true ]]; then
  printf '%s\n' "${operator_images[@]}" "${stub_images[@]}"
  exit 0
fi

# Resolve the container runtime: never assume Docker is running.
runtime=""
if [[ -n "${VCR_IMAGE_RUNTIME:-}" ]]; then
  runtime="$VCR_IMAGE_RUNTIME"
elif command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  runtime=docker
elif command -v podman >/dev/null 2>&1; then
  runtime=podman
fi
if [[ -z "$runtime" ]]; then
  echo "neither a running Docker daemon nor podman is available: cannot build or load images." >&2
  echo "Install podman (rootless works), or start Docker." >&2
  exit 1
fi
command -v "$runtime" >/dev/null || {
  echo "VCR_IMAGE_RUNTIME=$runtime is not installed" >&2
  exit 1
}

build_image() {
  local ref="$1"
  local dockerfile="$2"
  shift 2
  echo "building $ref from $dockerfile"
  "$runtime" build --network host -f "$dockerfile" -t "$ref" "$@" .
}

if [[ "$build" == true ]]; then
  build_image "$manager_image" Dockerfile
  build_image "$device_plugin_image" Dockerfile.device-plugin
fi

if [[ "$stub" == true ]]; then
  build_image "$frontend_stub_image" test/images/frontend-stub/Dockerfile
  build_image "$backend_stub_image" test/images/backend-stub/Dockerfile
  build_image "$profile_fixture_image" test/images/profile-fixture/Dockerfile
  build_image "$mock_profile_image" test/images/mock-profile/Dockerfile
fi

if [[ "$load" == true ]]; then
  mapfile -t nodes < <("$runtime" ps --format '{{.Names}}' | grep -E "^${cluster}-" || true)
  if [[ ${#nodes[@]} -eq 0 ]]; then
    echo "no nodes found for kind cluster '$cluster'" >&2
    echo "create it first: hack/kind-up.sh --name $cluster" >&2
    exit 1
  fi

  workdir="$(mktemp -d)"
  trap 'rm -rf "$workdir"' EXIT

  for ref in "${operator_images[@]}" "${stub_images[@]}"; do
    if ! "$runtime" image inspect "$ref" >/dev/null 2>&1; then
      echo "skipping $ref: not present in the $runtime image store (build it first)" >&2
      continue
    fi
    tar="$workdir/$(echo "$ref" | tr '/:' '__').tar"
    echo "exporting $ref"
    "$runtime" save -q -o "$tar" "$ref"
    for node in "${nodes[@]}"; do
      echo "importing $ref into $node"
      "$runtime" exec -i "$node" ctr -n k8s.io images import - <"$tar" >/dev/null
    done
    rm -f "$tar"
  done
  echo "images loaded into ${#nodes[@]} node(s) of kind cluster '$cluster'"
fi
