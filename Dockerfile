# syntax=docker/dockerfile:1
# Manager image: the vllm-vcr-operator control plane (reconcilers + admission).
#
# Multi-arch friendly: the builder stage runs on the build platform and
# cross-compiles for TARGETOS/TARGETARCH, so
#   docker buildx build --platform linux/amd64,linux/arm64 -f Dockerfile -t <ref> .
# needs no QEMU for compilation.
#
# Build args (declared before the first FROM so they can be used in FROM lines):
#   GO_VERSION  Go toolchain; must satisfy go.mod (default 1.26)
#   BASE_IMAGE  runtime base (default gcr.io/distroless/static:nonroot)
#   LD_FLAGS    extra linker flags, e.g. -X main.version=... (default empty)
ARG GO_VERSION=1.26
ARG BASE_IMAGE=gcr.io/distroless/static:nonroot

FROM --platform=${BUILDPLATFORM} golang:${GO_VERSION} AS builder
ARG TARGETOS
ARG TARGETARCH
ARG LD_FLAGS
WORKDIR /src
# CGO is not needed: the operator talks to the API server over HTTP/gRPC only.
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH:-$(go env GOARCH)}" \
    go build -trimpath -ldflags="-s -w ${LD_FLAGS}" -o /out/manager ./cmd/manager

FROM ${BASE_IMAGE}
COPY --from=builder /out/manager /manager
# distroless/static:nonroot already uses 65532; stated explicitly so the image
# keeps running unprivileged if BASE_IMAGE is overridden.
USER 65532:65532
ENTRYPOINT ["/manager"]
