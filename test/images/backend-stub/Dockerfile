# Test-only simulator image for the kind acceptance suite (test/cluster).
#
# It provides a stub `vllm-vcr` program that stands in for the Rust simulator,
# which is unavailable on the machines the suite targets. It registers with the
# engine-core handshake, serves the control API the supervisor probes, listens on
# the KV-event port, and reads the provisioned latency trace.
#
# Build with the repository root as the context:
#   hack/images.sh --build --stub
#   podman build -t docker.io/vcrlocal/vllm-vcr-backend-stub:test \
#     -f test/images/backend-stub/Dockerfile .
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
    go build -trimpath -o /out/vllm-vcr ./test/images/backend-stub

FROM ${BASE_IMAGE}
COPY --from=builder /out/vllm-vcr /usr/local/bin/vllm-vcr
# The operator runs the simulator container as a non-root user with a read-only
# root filesystem (internal/runtime); the only writable path it needs is the
# emptyDir mounted at /tmp.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/vllm-vcr"]
