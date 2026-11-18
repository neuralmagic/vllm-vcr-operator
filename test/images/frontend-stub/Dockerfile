# Test-only frontend image for the kind acceptance suite (test/cluster).
#
# It contains the REAL vcr-supervisor (cmd/supervisor) and a stub `vllm` program
# that stands in for the Python vLLM frontend, which is unavailable on the
# machines the suite targets. The supervisor is not stubbed: the suite asserts
# its real readiness/termination behavior.
#
# Build with the repository root as the context:
#   hack/images.sh --build --stub      (or the explicit podman build below)
#   podman build -t docker.io/vcrlocal/vllm-vcr-frontend-stub:test \
#     -f test/images/frontend-stub/Dockerfile .
#
# Nothing in internal/, config/, or the root Dockerfiles references this image;
# it is never an operator default.
ARG GO_VERSION=1.26
ARG BASE_IMAGE=busybox:1.36

FROM --platform=${BUILDPLATFORM} golang:${GO_VERSION} AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH:-$(go env GOARCH)}" \
    go build -trimpath -o /out/vcr-supervisor ./cmd/supervisor && \
    GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH:-$(go env GOARCH)}" \
    go build -trimpath -o /out/vllm ./test/images/frontend-stub

# busybox provides sh, ls, cat, and wget, which the suite uses to observe the
# profile volume and the frontend's own HTTP surface from inside the Pod.
FROM ${BASE_IMAGE}
COPY --from=builder /out/vcr-supervisor /usr/local/bin/vcr-supervisor
COPY --from=builder /out/vllm /usr/local/bin/vllm
# The supervisor binds 0.0.0.0:8082 for kubelet's probes and writes its ready file
# under /tmp, which the Pod mounts as a writable emptyDir.
ENTRYPOINT ["/usr/local/bin/vcr-supervisor"]
