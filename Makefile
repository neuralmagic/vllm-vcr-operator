# Make targets for the vllm-vcr-operator.

SHELL := /bin/bash
GO ?= go
CONTROLLER_GEN ?= $(HOME)/go/bin/controller-gen
ENVTEST ?= $(HOME)/go/bin/setup-envtest
ENVTEST_BIN_DIR ?= $(HOME)/.local/share/envtest
ENVTEST_K8S_VERSION ?= 1.34.x

BIN := bin
MANAGER := $(BIN)/manager
DEVICE_PLUGIN := $(BIN)/vllm-vcr-device-plugin
SUPERVISOR := $(BIN)/vcr-supervisor
PROFILE_EXPORT := $(BIN)/vcr-profile-export

.PHONY: all
all: build

.PHONY: build
build: $(MANAGER) $(DEVICE_PLUGIN) $(SUPERVISOR) $(PROFILE_EXPORT)

$(BIN):
	mkdir -p $(BIN)

$(MANAGER): $(shell find cmd/manager api internal -name '*.go' 2>/dev/null) | $(BIN)
	$(GO) build -o $@ ./cmd/manager

$(DEVICE_PLUGIN): $(shell find cmd/deviceplugin internal -name '*.go' 2>/dev/null) | $(BIN)
	$(GO) build -o $@ ./cmd/deviceplugin

$(SUPERVISOR): $(shell find cmd/supervisor internal -name '*.go' 2>/dev/null) | $(BIN)
	$(GO) build -o $@ ./cmd/supervisor

$(PROFILE_EXPORT): $(shell find cmd/profileexport internal -name '*.go' 2>/dev/null) | $(BIN)
	$(GO) build -o $@ ./cmd/profileexport

.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: test
test:
	$(GO) test ./...

# check-upstream compares this operator against the pinned vllm-vcr release: the
# upstream commit/tag, every `play` flag the translator emits, and the simulator's
# default vLLM line (docs/upstream-vllm-vcr.md). It skips cleanly when the network
# or the simulator checkout is unavailable, and exits non-zero on real drift.
#
# The checkout it reads defaults to $$VLLM_VCR_DIR, then /home/acardace/work/vllm-vcr;
# set VLLM_VCR_DIR to point it elsewhere.
.PHONY: check-upstream
check-upstream:
	./hack/check-vllm-vcr-upstream.sh $(if $(VLLM_VCR_DIR),--vllm-vcr-dir $(VLLM_VCR_DIR))

.PHONY: test-unit
test-unit:
	$(GO) test $$($(GO) list ./... | grep -v '/test/e2e' | grep -v '/test/spike')

.PHONY: test-e2e
test-e2e:
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_BIN_DIR) -p path)" \
		$(GO) test -count=1 -timeout 20m ./test/e2e/...

.PHONY: test-spike
test-spike:
	$(GO) test -count=1 -timeout 30m ./test/spike/...

# test-cluster runs the kind-cluster acceptance suite: the specification's primary
# acceptance interface (an unchanged GPU-requesting vLLM workload on a CPU-only
# cluster). It needs the two-node cluster and the locally built images:
#
#   hack/kind-up.sh
#   hack/images.sh --build --stub --load
#
# Without them the suite skips and names the missing prerequisite. The suite
# installs the operator, its webhook certificates, and a test TLS registry itself,
# and removes them again when it passes. See test/cluster/README.md.
.PHONY: test-cluster
test-cluster:
	$(GO) test -count=1 -timeout 30m ./test/cluster/...

.PHONY: cover
cover:
	$(GO) test -coverprofile=cover.out ./...
	$(GO) tool cover -func=cover.out | tail -1

.PHONY: verify
verify: fmt vet test

.PHONY: manifests
manifests:
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." \
		output:crd:dir=config/crd/bases output:rbac:dir=config/rbac output:webhook:dir=config/webhook

.PHONY: generate
generate:
	$(CONTROLLER_GEN) object:headerFile="" paths="./..."

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: clean
clean:
	rm -rf $(BIN) cover.out coverage.html
