# Test-only profile image for the kind acceptance suite (test/cluster).
#
# It is a real OCI profile image per docs/contracts/profile-image.md: /profile
# carries metadata.json and the trace file the metadata declares, and the image
# installs the real vcr-profile-export (cmd/profileexport), which the operator runs
# as the vcr-profile-provisioner init container.
#
# Its metadata is a TEST DOUBLE, not a recording: the id starts with "stub-", the
# documented accelerator/topology/cache shape (H200, TP=1, Qwen3-0.6B, 2048 KV
# blocks of 64 tokens) is asserted by the suite, and no timing fidelity is claimed.
#
# Build with the repository root as the context:
#   hack/images.sh --build --stub
#   podman build -t docker.io/vcrlocal/vllm-vcr-profile-fixture:test \
#     -f test/images/profile-fixture/Dockerfile .
#
# The suite pushes this image to the in-cluster TLS registry the operator resolves
# profile metadata from; nothing in this repository publishes it.
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
    go build -trimpath -o /out/vcr-profile-export ./cmd/profileexport

FROM ${BASE_IMAGE}
COPY --from=builder /out/vcr-profile-export /usr/local/bin/vcr-profile-export
# Wrapper that keeps the exporter's validation but stages inside the mounted
# profile volume. See the script and test/cluster/README.md ("Known product
# defect") for why the operator's read-only-rootfs provisioner needs it.
COPY test/images/profile-fixture/vcr-profile-export-volume /usr/local/bin/vcr-profile-export-volume
COPY test/images/profile-fixture/profile/ /profile/
# The operator runs the provisioner as a non-root user (internal/runtime), so the
# image must not rely on being root.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/vcr-profile-export"]
