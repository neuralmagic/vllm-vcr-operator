package spike_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/profile"
	"github.com/opendatahub-io/vllm-vcr-operator/test/spike"
)

// fixtureMetadata is a recorded profile shaped like the example in
// docs/contracts/profile-image.md: one H200, one token per block position,
// prefix caching on, and coverage that authorizes this run's overrides.
func fixtureMetadata() *profile.Metadata {
	return &profile.Metadata{
		SchemaVersion: names.ProfileCatalogSchemaVersion,
		ID:            "qwen3-0.6b-h200-tp1-bf16",
		Model:         profile.Model{Name: spike.DefaultModel, Revision: "c9340e6f3"},
		GPU:           profile.GPU{Type: "H200", Count: 1},
		Execution:     profile.Execution{TensorParallel: 1, PipelineParallel: 1, DType: "bfloat16"},
		Cache:         profile.Cache{BlockSize: 64, EnablePrefixCaching: true, KVBlocks: 2048, ChunkedPrefill: true},
		VLLM:          profile.VLLM{Version: names.SupportedVLLMVersion},
		Coverage: profile.Coverage{
			MaxModelLen:         &profile.Range{Min: 1, Max: 32768},
			MaxNumSeqs:          &profile.Range{Min: 1, Max: 16},
			MaxNumBatchedTokens: &profile.Range{Min: 1, Max: 8192},
			KVBlocks:            &profile.Range{Min: 1, Max: 2048},
			SchedulingPolicies:  []string{"fcfs", "priority"},
		},
		Trace: profile.Trace{File: "trace.jsonl.gz", Format: "vllm-vcr/jsonl", LatencyModel: true, RecordedTokens: true, KVEvents: true},
		Provisioning: profile.Provisioning{
			Entrypoint:   []string{"/usr/local/bin/vcr-profile-export"},
			MetadataFile: names.ProfileMetadataFileName,
			TraceFile:    "trace.jsonl.gz",
		},
	}
}

// fixtureEnvironment is a resolved environment with an exported profile.
func fixtureEnvironment() spike.Environment {
	return spike.Environment{
		FrontendImage: "registry.example/vllm-vcr-frontend:v0.33.0-cpu",
		BackendImage:  "registry.example/vllm-vcr-backend:v0.33.0",
		ProfileDir:    "/tmp/vcr-spike/profile",
		Model:         spike.DefaultModel,
		Engine:        "podman",
		EnginePath:    "/usr/bin/podman",
	}
}

// TestSettingsComeFromTheProfile asserts the run is driven by the recorded
// profile: block size, KV capacity, and the model length bound are the profile's
// recorded values, never harness constants.
func TestSettingsComeFromTheProfile(t *testing.T) {
	settings, err := spike.DefaultSettings(fixtureEnvironment(), fixtureMetadata())
	if err != nil {
		t.Fatalf("DefaultSettings: %v", err)
	}

	if settings.TokensPerBlock != 64 {
		t.Errorf("TokensPerBlock = %d, want the profile's cache.blockSize 64", settings.TokensPerBlock)
	}
	if settings.KVBlocks != 2048 {
		t.Errorf("KVBlocks = %d, want the profile's cache.kvBlocks 2048", settings.KVBlocks)
	}
	if settings.MaxModelLen != 32768 {
		t.Errorf("MaxModelLen = %d, want coverage.maxModelLen.max 32768", settings.MaxModelLen)
	}
	if settings.MaxNumSeqs != 16 {
		t.Errorf("MaxNumSeqs = %d, want coverage.maxNumSeqs.max 16", settings.MaxNumSeqs)
	}
	if settings.InferencePort != names.DefaultInferencePort {
		t.Errorf("InferencePort = %d", settings.InferencePort)
	}
	if settings.HandshakeAddress != "tcp://127.0.0.1:29550" {
		t.Errorf("HandshakeAddress = %q", settings.HandshakeAddress)
	}
}

// TestSettingsRefuseAProfileWithoutCoverage asserts an unbounded profile is
// refused: a spike served with an invented maximum model length would be
// evidence for a configuration the profile never recorded.
func TestSettingsRefuseAProfileWithoutCoverage(t *testing.T) {
	metadata := fixtureMetadata()
	metadata.Coverage.MaxModelLen = nil

	_, err := spike.DefaultSettings(fixtureEnvironment(), metadata)
	if err == nil {
		t.Fatal("DefaultSettings accepted a profile without coverage.maxModelLen")
	}
	if !strings.Contains(err.Error(), "maxModelLen") {
		t.Errorf("error does not name the missing coverage:\n%s", err)
	}
}

// TestFrontendCommandBindsTheHandshakeAndWaitsForAnExternalEngine asserts the
// frontend half of the layout contract: the supervisor runs the frontend, the
// frontend binds the engine-core handshake, and it is started with no local
// engine rank so it waits for the simulator to register.
func TestFrontendCommandBindsTheHandshakeAndWaitsForAnExternalEngine(t *testing.T) {
	settings, err := spike.DefaultSettings(fixtureEnvironment(), fixtureMetadata())
	if err != nil {
		t.Fatalf("DefaultSettings: %v", err)
	}
	command := settings.FrontendCommand()

	if command[0] != names.SupervisorBinary {
		t.Errorf("frontend command starts with %q, want the supervisor %q", command[0], names.SupervisorBinary)
	}
	separator := slices.Index(command, "--")
	if separator < 0 {
		t.Fatalf("supervisor invocation has no -- separator: %v", command)
	}
	frontend := command[separator+1:]
	if !slices.Equal(frontend[:2], []string{"vllm", "serve"}) {
		t.Errorf("frontend command after -- = %v, want `vllm serve …`", frontend[:2])
	}
	if !containsPair(frontend, "--data-parallel-size-local", "0") {
		t.Errorf("frontend does not wait for an external engine:\n%v", frontend)
	}
	if !containsPair(frontend, "--data-parallel-rpc-port", "29550") {
		t.Errorf("frontend does not bind the handshake port the simulator connects to:\n%v", frontend)
	}
	if !containsPair(frontend, "--port", "8000") {
		t.Errorf("frontend does not serve the inference port:\n%v", frontend)
	}
	if !containsValue(frontend, spike.DefaultModel) {
		t.Errorf("frontend does not serve the profile's model:\n%v", frontend)
	}
	if !slices.Contains(frontend, "--enable-prefix-caching") {
		t.Errorf("frontend does not enable the profile's recorded prefix caching:\n%v", frontend)
	}
	for _, forbidden := range []string{"--tensor-parallel-size", "--gpu-memory-utilization", "--dtype", "--quantization", "--num-gpu-blocks-override"} {
		if slices.Contains(frontend, forbidden) {
			t.Errorf("frontend received accelerator flag %q, which the simulator advertises instead", forbidden)
		}
	}

	if !containsPair(command, "--handshake-address", "tcp://127.0.0.1:29550") {
		t.Errorf("supervisor does not observe the handshake:\n%v", command)
	}
	if !containsPair(command, "--ready-file", settings.ContainerReadyFile()) {
		t.Errorf("supervisor does not publish the readiness marker:\n%v", command)
	}
	if !containsPair(command, "--shutdown-timeout", "20s") {
		t.Errorf("supervisor has no graceful shutdown budget:\n%v", command)
	}
}

// TestFrontendCommandFollowsTheRecordedCacheMode asserts prefix caching is the
// profile's recorded property: a workload cannot ask for the opposite mode, and
// the harness must not either.
func TestFrontendCommandFollowsTheRecordedCacheMode(t *testing.T) {
	metadata := fixtureMetadata()
	metadata.Cache.EnablePrefixCaching = false

	settings, err := spike.DefaultSettings(fixtureEnvironment(), metadata)
	if err != nil {
		t.Fatalf("DefaultSettings: %v", err)
	}
	if slices.Contains(settings.FrontendCommand(), "--enable-prefix-caching") {
		t.Errorf("frontend enables prefix caching although the profile records it disabled:\n%v", settings.FrontendCommand())
	}
}

// TestBackendCommandConnectsAndServesTheProfile asserts the simulator half of
// the layout contract: it connects to the handshake it does not own, takes the
// profile's cache representation and trace, and exposes its control API on the
// documented port.
func TestBackendCommandConnectsAndServesTheProfile(t *testing.T) {
	settings, err := spike.DefaultSettings(fixtureEnvironment(), fixtureMetadata())
	if err != nil {
		t.Fatalf("DefaultSettings: %v", err)
	}
	command := settings.BackendCommand()

	if !slices.Equal(command[:2], []string{"vllm-vcr", "play"}) {
		t.Errorf("backend command = %v, want `vllm-vcr play …`", command[:2])
	}
	if !containsPair(command, "--handshake-address", "tcp://127.0.0.1:29550") {
		t.Errorf("simulator does not connect to the frontend's handshake:\n%v", command)
	}
	if !containsPair(command, "--model-name", spike.DefaultModel) {
		t.Errorf("simulator does not serve the profile's model:\n%v", command)
	}
	if !containsPair(command, "--tokens-per-block", "64") {
		t.Errorf("simulator block size does not match the profile:\n%v", command)
	}
	if !containsPair(command, "--kv-cache-size", "2048") {
		t.Errorf("simulator KV capacity does not match the profile:\n%v", command)
	}
	if !containsPair(command, "--max-model-len", "32768") {
		t.Errorf("simulator model length is not bounded by the profile:\n%v", command)
	}
	if !containsPair(command, "--latency-trace", "/var/run/vcr/profile/trace.jsonl.gz") {
		t.Errorf("simulator does not read the exported trace at the profile mount path:\n%v", command)
	}
	if !containsPair(command, "--max-num-batched-tokens", "8192") {
		t.Errorf("simulator per-step budget does not come from the profile's coverage:\n%v", command)
	}
	// A plain HOST:PORT socket address: the simulator binds it with
	// TcpListener::bind, which rejects a ZMQ-style "tcp://" scheme
	// (upstream docs/control-api.md:3).
	if !containsPair(command, "--control-address", "0.0.0.0:8001") {
		t.Errorf("simulator control API is not on the documented HOST:PORT address:\n%v", command)
	}
	if slices.Contains(command, "--enable-kv-cache-events") {
		t.Errorf("simulator publishes KV events although the run did not ask for them:\n%v", command)
	}
}

// TestBackendCommandPublishesKVEventsWhenAsked asserts live KV events are
// requested explicitly, with the topic form llm-d filters on, because a router
// that receives no events cannot be exercised.
func TestBackendCommandPublishesKVEventsWhenAsked(t *testing.T) {
	settings, err := spike.DefaultSettings(fixtureEnvironment(), fixtureMetadata())
	if err != nil {
		t.Fatalf("DefaultSettings: %v", err)
	}
	settings.EnableKVEvents = true
	command := settings.BackendCommand()

	if !slices.Contains(command, "--enable-kv-cache-events") {
		t.Fatalf("simulator does not enable KV events:\n%v", command)
	}
	if !containsPair(command, "--kv-events-endpoint", "tcp://*:5556") {
		t.Errorf("simulator does not bind the KV-event port llm-d discovers:\n%v", command)
	}
	if !containsPair(command, "--kv-events-topic", "kv@127.0.0.1:8000@Qwen/Qwen3-0.6B") {
		t.Errorf("simulator topic does not use the llm-d kv@<pod ip:port>@<model> form:\n%v", command)
	}
}

// TestFrontendEnvironmentCarriesTheReadinessInputs asserts the frontend
// container receives the inputs the layout contract documents, including the
// profile directory the simulator reads.
func TestFrontendEnvironmentCarriesTheReadinessInputs(t *testing.T) {
	settings, err := spike.DefaultSettings(fixtureEnvironment(), fixtureMetadata())
	if err != nil {
		t.Fatalf("DefaultSettings: %v", err)
	}
	env := settings.FrontendEnv()

	for key, want := range map[string]string{
		"VCR_HANDSHAKE_ADDRESS":   "tcp://127.0.0.1:29550",
		"VCR_FRONTEND_HEALTH_URL": "http://127.0.0.1:8000/health",
		"VCR_BACKEND_HEALTH_URL":  "http://127.0.0.1:8001/stats",
		"VCR_PROFILE_DIR":         names.ProfileMountPath,
		"VCR_SHUTDOWN_TIMEOUT":    "20",
		"VCR_READY_FILE":          settings.ContainerReadyFile(),
	} {
		if got := envValue(env, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// TestEndpointURLsAreDerivedFromThePorts asserts the spike drives the same
// surfaces the operator configures, not a harness-specific port.
func TestEndpointURLsAreDerivedFromThePorts(t *testing.T) {
	settings, err := spike.DefaultSettings(fixtureEnvironment(), fixtureMetadata())
	if err != nil {
		t.Fatalf("DefaultSettings: %v", err)
	}
	if got := settings.InferenceURL(); got != "http://127.0.0.1:8000" {
		t.Errorf("InferenceURL = %q", got)
	}
	if got := settings.ControlURL(); got != "http://127.0.0.1:8001" {
		t.Errorf("ControlURL = %q", got)
	}
	if got := settings.ReadinessURL(); got != "http://127.0.0.1:8082/readyz" {
		t.Errorf("ReadinessURL = %q, want the supervisor's readiness endpoint", got)
	}
	if got := settings.ShutdownTimeout; got != 20*time.Second {
		t.Errorf("ShutdownTimeout = %v, want the documented default 20s", got)
	}
}

func containsPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func containsValue(args []string, value string) bool {
	return slices.Contains(args, value)
}

func envValue(env []string, key string) string {
	for _, entry := range env {
		if name, value, ok := strings.Cut(entry, "="); ok && name == key {
			return value
		}
	}
	return ""
}

// TestBackendCommandReplaysRecordedTokensOnlyWhenTheProfileGrantsIt asserts
// content-identical replay is a profile capability, not a flag the harness can
// turn on: without the capability the run serves seeded random tokens and must
// not be described as replaying content.
func TestBackendCommandReplaysRecordedTokensOnlyWhenTheProfileGrantsIt(t *testing.T) {
	denied, err := spike.DefaultSettings(fixtureEnvironment(), fixtureMetadata())
	if err != nil {
		t.Fatalf("DefaultSettings: %v", err)
	}
	if slices.Contains(denied.BackendCommand(), "--replay-tokens") {
		t.Errorf("simulator replays tokens although the profile grants no capabilities.contentReplay:\n%v", denied.BackendCommand())
	}

	metadata := fixtureMetadata()
	metadata.Capabilities.ContentReplay = true
	granted, err := spike.DefaultSettings(fixtureEnvironment(), metadata)
	if err != nil {
		t.Fatalf("DefaultSettings: %v", err)
	}
	command := granted.BackendCommand()
	if !containsPair(command, "--replay-tokens", "/var/run/vcr/profile/trace.jsonl.gz") {
		t.Errorf("simulator does not replay the profile's recorded tokens:\n%v", command)
	}
	if !containsPair(command, "--replay-match", "prefix") {
		t.Errorf("simulator does not match live requests by block-hash prefix, the only mode that works for a live client:\n%v", command)
	}
}

// TestTraceReferenceUsesTheContainerMountPath asserts the simulator is pointed at
// the profile directory as the container sees it, not at the host directory the
// harness exported into: the host path does not exist inside the container, and a
// wrong path would make the simulator fail at startup for a reason that looks like
// a broken profile.
func TestTraceReferenceUsesTheContainerMountPath(t *testing.T) {
	settings, err := spike.DefaultSettings(fixtureEnvironment(), fixtureMetadata())
	if err != nil {
		t.Fatalf("DefaultSettings: %v", err)
	}
	if got := settings.TracePath(); got != "/var/run/vcr/profile/trace.jsonl.gz" {
		t.Errorf("TracePath = %q, want the mounted profile path", got)
	}
	if got := settings.HostTracePath(); got != "/tmp/vcr-spike/profile/trace.jsonl.gz" {
		t.Errorf("HostTracePath = %q, want the exported directory the harness checks", got)
	}
	if !strings.Contains(strings.Join(settings.BackendCommand(), " "), settings.TracePath()) {
		t.Errorf("the simulator invocation does not reference the container trace path: %v", settings.BackendCommand())
	}
}
