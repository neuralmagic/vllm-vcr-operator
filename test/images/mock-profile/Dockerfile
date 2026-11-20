# Test-only mock profile image: contract-valid metadata plus a synthetic trace, so
# the operator, the CPU vLLM frontend and the real vllm-vcr simulator can be run
# together before any profile is recorded on GPUs. See README.md: no timing claim.
#
# Build with the repository root as the context:
#   podman build -t <registry>/<ns>/vllm-vcr-profile-mock:test \
#     -f test/images/mock-profile/Dockerfile .
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
COPY test/images/mock-profile/profile/ /profile/
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/vcr-profile-export"]
