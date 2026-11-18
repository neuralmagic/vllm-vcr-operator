package translator_test

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/profile"
)

// TestBackendArgumentsAreSimulatorFlags pins the simulator invocation for the
// contract profile. The expected argv is written out literally, so a change in the
// mapping is a deliberate change to this test.
func TestBackendArgumentsAreSimulatorFlags(t *testing.T) {
	admission, err := runTranslate(t, translateCase{name: "minimal"}, testEnvironment())
	mustAccept(t, admission, err)

	want := []string{
		"vllm-vcr", "play",
		"--handshake-address", "tcp://127.0.0.1:29550",
		"--model-name", "Qwen/Qwen3-0.6B",
		"--max-model-len", "131072",
		"--tokens-per-block", "64",
		"--kv-cache-size", "2048",
		"--shutdown-timeout", "0",
		"--seed", "0",
		"--control-address", "tcp://0.0.0.0:8001",
		"--max-num-seqs", "128",
		"--max-num-batched-tokens", "8192",
		"--long-prefill-token-threshold", "4096",
		"--scheduling-policy", "fcfs",
		"--latency-trace", "/var/run/vcr/profile/trace.jsonl.gz",
		"--expect-config-hash", "sha256:def456",
	}
	if got := admission.Backend.Command; !reflect.DeepEqual(got, want) {
		t.Fatalf("backend command =\n %v\nwant\n %v", got, want)
	}
	if got := admission.Backend.Image; got != "ghcr.io/neuralmagic/vllm-vcr:vllm0.33" {
		t.Errorf("backend image = %q", got)
	}
	if len(admission.Backend.CopiedArguments) != 0 {
		t.Errorf("backend copied arguments = %v, want none", admission.Backend.CopiedArguments)
	}
	for _, token := range admission.Backend.Command {
		if token == "--gpu-memory-utilization" || token == "--tensor-parallel-size" || token == "--enable-prefix-caching" {
			t.Errorf("backend argv %v contains the GPU-only flag %q", admission.Backend.Command, token)
		}
	}
}

// TestFrontendArgumentsRunUnderTheSupervisor pins the frontend invocation: the
// container runs the supervisor, which starts the frontend in external-engine mode.
func TestFrontendArgumentsRunUnderTheSupervisor(t *testing.T) {
	admission, err := runTranslate(t, translateCase{name: "minimal"}, testEnvironment())
	mustAccept(t, admission, err)

	if got, want := admission.Frontend.Command, []string{names.SupervisorBinary, "--"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("frontend command = %v, want %v", got, want)
	}
	wantArgs := []string{
		"vllm", "serve", "Qwen/Qwen3-0.6B",
		"--host", "0.0.0.0",
		"--port", "8000",
		"--data-parallel-size", "1",
		"--data-parallel-size-local", "0",
		"--data-parallel-address", "127.0.0.1",
		"--data-parallel-rpc-port", "29550",
	}
	if got := admission.Frontend.Args; !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("frontend args =\n %v\nwant\n %v", got, wantArgs)
	}

	wantEnv := []corev1.EnvVar{
		{Name: "VCR_HANDSHAKE_ADDRESS", Value: "tcp://127.0.0.1:29550"},
		{Name: "VCR_FRONTEND_HEALTH_URL", Value: "http://127.0.0.1:8000/health"},
		{Name: "VCR_BACKEND_HEALTH_URL", Value: "http://127.0.0.1:8001/stats"},
		{Name: "VCR_PROFILE_DIR", Value: names.ProfileMountPath},
		{Name: "VCR_SHUTDOWN_TIMEOUT", Value: "0"},
		{Name: "VCR_READY_FILE", Value: "/tmp/vcr-runtime/ready"},
	}
	if got := admission.Frontend.Env; !reflect.DeepEqual(got, wantEnv) {
		t.Fatalf("frontend env =\n %v\nwant\n %v", got, wantEnv)
	}

	wantPorts := []corev1.ContainerPort{
		{Name: "inference", ContainerPort: 8000, Protocol: corev1.ProtocolTCP},
		{Name: "engine-hs", ContainerPort: 29550, Protocol: corev1.ProtocolTCP},
	}
	if got := admission.Frontend.Ports; !reflect.DeepEqual(got, wantPorts) {
		t.Fatalf("frontend ports =\n %v\nwant\n %v", got, wantPorts)
	}
}

// TestProvisionerRunsThePinnedProfileEntrypoint asserts the profile image exports
// its own data, pinned by digest, into the shared volume.
func TestProvisionerRunsThePinnedProfileEntrypoint(t *testing.T) {
	admission, err := runTranslate(t, translateCase{name: "minimal"}, testEnvironment())
	mustAccept(t, admission, err)

	wantImage := "quay.io/opendatahub/vllm-vcr-profiles@sha256:0123456789abcdef"
	if got := admission.Provisioner.Image; got != wantImage {
		t.Errorf("provisioner image = %q, want %q", got, wantImage)
	}
	wantCommand := []string{"/usr/local/bin/vcr-profile-export", names.ProfileMountPath}
	if got := admission.Provisioner.Command; !reflect.DeepEqual(got, wantCommand) {
		t.Errorf("provisioner command = %v, want %v", got, wantCommand)
	}
}

// TestPlanCarriesEverythingTheRuntimeNeeds asserts the plan is self-contained:
// provenance, sizing, placement intent, and pull secrets are already resolved.
func TestPlanCarriesEverythingTheRuntimeNeeds(t *testing.T) {
	env := testEnvironment()
	env.ProfilePullSecrets = map[string]string{testImage: "private-profile-pull"}
	admission, err := runTranslate(t, translateCase{name: "minimal"}, env)
	mustAccept(t, admission, err)

	if got, want := admission.Profile.ID, "qwen3-0.6b-h200-tp1-bf16"; got != want {
		t.Errorf("profile id = %q, want %q", got, want)
	}
	if got, want := admission.Profile.PinnedImage(), "quay.io/opendatahub/vllm-vcr-profiles@sha256:0123456789abcdef"; got != want {
		t.Errorf("pinned profile image = %q, want %q", got, want)
	}
	if got, want := admission.Provenance.Model, "Qwen/Qwen3-0.6B@c9340e6f3"; got != want {
		t.Errorf("provenance model = %q, want %q", got, want)
	}
	if got, want := admission.Provenance.VLLMVersion, names.SupportedVLLMVersion; got != want {
		t.Errorf("provenance vllm version = %q, want %q", got, want)
	}
	if got, want := admission.GPUDevices, int32(1); got != want {
		t.Errorf("gpu devices = %d, want %d", got, want)
	}

	// The per-profile pull-secret override replaces the environment-wide list.
	if got, want := admission.PullSecrets, []string{"private-profile-pull"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pull secrets = %v, want %v", got, want)
	}
	// Without an override the environment-wide names are used.
	admission, err = runTranslate(t, translateCase{name: "minimal"}, testEnvironment())
	mustAccept(t, admission, err)
	if got, want := admission.PullSecrets, []string{"profile-pull"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pull secrets = %v, want %v", got, want)
	}

	if got, want := admission.Resources.Frontend.Requests.Cpu().String(), "2"; got != want {
		t.Errorf("frontend cpu = %q, want %q", got, want)
	}
	if got, want := admission.Resources.Backend.Requests.Memory().String(), "2Gi"; got != want {
		t.Errorf("backend memory = %q, want %q", got, want)
	}

	if got, want := admission.Placement.NodeSelector, map[string]string{"vcr.opendatahub-io/simulation": "true"}; !reflect.DeepEqual(got, want) {
		t.Errorf("placement node selector = %v, want %v", got, want)
	}
	if len(admission.Placement.Tolerations) != 1 {
		t.Errorf("placement tolerations = %v, want the environment toleration", admission.Placement.Tolerations)
	}
	if !containsString(admission.Placement.AcceleratorNodeLabels, names.SyntheticResourceName) {
		t.Errorf("placement accelerator labels = %v, want the synthetic resource", admission.Placement.AcceleratorNodeLabels)
	}
	if !containsString(admission.Placement.AcceleratorNodeValues, "h200") {
		t.Errorf("placement accelerator values = %v, want the simulated gpu type", admission.Placement.AcceleratorNodeValues)
	}
	if !containsString(admission.Placement.RuntimeClasses, "nvidia") {
		t.Errorf("placement runtime classes = %v, want the NVIDIA runtime class", admission.Placement.RuntimeClasses)
	}
}

// TestContentReplayFollowsTheProfileCapability asserts recorded content is replayed
// only when the profile authorizes it, and that the plan explains the difference.
func TestContentReplayFollowsTheProfileCapability(t *testing.T) {
	t.Run("authorized", func(t *testing.T) {
		admission, err := runTranslate(t, translateCase{name: "content replay", mutate: func(m *profile.Metadata) {
			m.Capabilities.ContentReplay = true
		}}, testEnvironment())
		mustAccept(t, admission, err)
		requireArgs(t, "backend", admission.Backend.Command, "--replay-tokens", "/var/run/vcr/profile/trace.jsonl.gz")
		requireArgs(t, "backend", admission.Backend.Command, "--replay-match", "prefix")
		if !warningsContain(admission, "unmatched requests fall back to random tokens") {
			t.Errorf("warnings %v do not state the unmatched-replay behavior", admission.Warnings)
		}
	})
	t.Run("not authorized", func(t *testing.T) {
		admission, err := runTranslate(t, translateCase{name: "random tokens"}, testEnvironment())
		mustAccept(t, admission, err)
		rejectArgs(t, "backend", admission.Backend.Command, "--replay-tokens")
		if !warningsContain(admission, "content-identical assertions are not authorized") {
			t.Errorf("warnings %v do not classify random-token output", admission.Warnings)
		}
	})
}

// TestKVEventTopicNeedsPodIdentity asserts the llm-d topic is built from the Pod
// identity, and that its absence is reported rather than silently defaulted.
func TestKVEventTopicNeedsPodIdentity(t *testing.T) {
	args := []string{"--kv-events-config", `{"enable_kv_cache_events":true}`}
	admission, err := runTranslate(t, translateCase{name: "no identity", args: args}, testEnvironment())
	mustAccept(t, admission, err)
	rejectArgs(t, "backend", admission.Backend.Command, "--kv-events-topic")
	if !warningsContain(admission, "derives the KV-event topic") {
		t.Errorf("warnings %v do not explain the derived topic", admission.Warnings)
	}
}

// TestWorkloadKVEventEndpointIsPreserved asserts a concrete endpoint is forwarded
// verbatim, because the simulator connects to it instead of binding a wildcard.
func TestWorkloadKVEventEndpointIsPreserved(t *testing.T) {
	admission, err := runTranslate(t, translateCase{
		name:     "concrete endpoint",
		args:     []string{"--kv-events-config", `{"enable_kv_cache_events":true,"endpoint":"tcp://kv-relay:5557","topic":"kv@custom@model"}`},
		identity: "pod-3",
	}, testEnvironment())
	mustAccept(t, admission, err)
	requireArgs(t, "backend", admission.Backend.Command, "--kv-events-endpoint", "tcp://kv-relay:5557")
	requireArgs(t, "backend", admission.Backend.Command, "--kv-events-topic", "kv@custom@model")
	requireArgs(t, "backend", admission.Backend.Command, "--engine-id", "pod-3")

	found := false
	for _, port := range admission.Backend.Ports {
		if port.Name == "kv-events" && port.ContainerPort == 5557 {
			found = true
		}
	}
	if !found {
		t.Errorf("backend ports = %v, want a kv-events port 5557", admission.Backend.Ports)
	}
}

// TestHandshakePortFollowsTheWorkload asserts the workload's data-parallel RPC port
// becomes the engine-core handshake port for both processes.
func TestHandshakePortFollowsTheWorkload(t *testing.T) {
	admission, err := runTranslate(t, translateCase{
		name: "custom handshake port",
		args: []string{"--data-parallel-rpc-port", "5570"},
	}, testEnvironment())
	mustAccept(t, admission, err)
	requireArgs(t, "backend", admission.Backend.Command, "--handshake-address", "tcp://127.0.0.1:5570")
	requireArgs(t, "frontend", admission.Frontend.Args, "--data-parallel-rpc-port", "5570")
	if got, want := envValue(admission.Frontend.Env, "VCR_HANDSHAKE_ADDRESS"), "tcp://127.0.0.1:5570"; got != want {
		t.Errorf("VCR_HANDSHAKE_ADDRESS = %q, want %q", got, want)
	}
}

// TestReadinessInputsFollowTheWorkloadPort asserts the supervisor observes the
// workload's own inference port, not a fixed default.
func TestReadinessInputsFollowTheWorkloadPort(t *testing.T) {
	admission, err := runTranslate(t, translateCase{
		name: "custom inference port",
		args: []string{"--port", "8200"},
	}, testEnvironment())
	mustAccept(t, admission, err)
	if got, want := envValue(admission.Frontend.Env, "VCR_FRONTEND_HEALTH_URL"), "http://127.0.0.1:8200/health"; got != want {
		t.Errorf("VCR_FRONTEND_HEALTH_URL = %q, want %q", got, want)
	}
}

// TestSimulatorLogFilterComesFromTheEnvironment asserts the simulator log filter is
// an operator setting, so a vLLM log level cannot be mistaken for RUST_LOG syntax.
func TestSimulatorLogFilterComesFromTheEnvironment(t *testing.T) {
	env := testEnvironment()
	env.LogLevel = "debug"
	admission, err := runTranslate(t, translateCase{
		name: "log level",
		args: []string{"--log-level", "WARNING"},
	}, env)
	mustAccept(t, admission, err)
	if got := envValue(admission.Backend.Env, "RUST_LOG"); got != "debug" {
		t.Errorf("backend RUST_LOG = %q, want debug", got)
	}
	requireArgs(t, "frontend", admission.Frontend.Args, "--log-level", "WARNING")
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
