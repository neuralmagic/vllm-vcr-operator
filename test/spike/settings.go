package spike

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/profile"
)

// Defaults for the spike run. They are the layout contract's ports and the
// supervisor's documented defaults; a run that changes one records the change in
// its evidence document.
const (
	// DefaultShutdownTimeout is the graceful shutdown budget the spikes give the
	// frontend, so that a coordinated shutdown is observable at all. The vLLM
	// default (0) aborts in-flight requests immediately.
	DefaultShutdownTimeout = 20 * time.Second

	// DefaultBackendLossGrace is how long the engine may be unreachable before
	// the supervisor terminates the frontend.
	DefaultBackendLossGrace = 5 * time.Second

	// SupervisorHealthAddress is the supervisor's local health endpoint.
	SupervisorHealthAddress = "127.0.0.1:8082"

	// SupervisorHealthPort is the port of SupervisorHealthAddress, used to build
	// the readiness URL the harness polls.
	SupervisorHealthPort = 8082

	// ContainerReadyFile is the readiness marker inside the frontend container,
	// from docs/contracts/runtime-layout.md.
	ContainerReadyFile = "/tmp/vcr-runtime/ready"

	// ContainerRuntimeDir is the directory inside the frontend container that
	// holds the readiness marker. The harness mounts a host directory there so
	// that the marker itself is observable, which is what incident diagnostics
	// read.
	ContainerRuntimeDir = "/tmp/vcr-runtime"

	// backendBinary is the simulator binary inside VCR_BACKEND_IMAGE.
	backendBinary = "vllm-vcr"

	// frontendBinary is the frontend binary inside VCR_FRONTEND_IMAGE.
	frontendBinary = "vllm"

	// defaultProvisionerEntrypoint is the provisioning command the profile image
	// contract documents (docs/contracts/profile-image.md). A profile image that
	// declares a different entrypoint needs it named in ProvisionerEntrypoint.
	defaultProvisionerEntrypoint = "/usr/local/bin/vcr-profile-export"
)

// Settings are the resolved parameters of one spike run: the two process command
// lines, the environment the frontend container needs, and the endpoints the
// checks drive.
type Settings struct {
	// Images and the exported profile directory.
	FrontendImage string
	BackendImage  string
	ProfileImage  string
	ProfileDir    string

	// Model is the served model name, taken from the profile's model identity.
	Model string

	// Ports and addresses from docs/contracts/runtime-layout.md and internal/names.
	InferencePort    int
	HandshakeAddress string
	ControlPort      int
	KVEventsPort     int

	// ReadyFile is the readiness marker's host path. The harness binds its
	// directory to ContainerRuntimeDir, so the marker a container writes is the
	// file the checks read.
	ReadyFile string

	// ShutdownTimeout and BackendLossGrace bound the lifecycle checks.
	ShutdownTimeout  time.Duration
	BackendLossGrace time.Duration

	// TraceFile is the profile's recorded trace file name.
	TraceFile string

	// Profile-derived limits the simulator must advertise.
	MaxModelLen    int64
	TokensPerBlock int32
	KVBlocks       int64
	MaxNumSeqs     int64

	// EnableKVEvents makes the simulator publish live KV-cache events.
	EnableKVEvents bool

	// ContentReplay states that the profile grants content-identical replay, so
	// the simulator serves recorded token ids matched by block-hash prefix.
	ContentReplay bool

	// ProvisionerEntrypoint is the command the profile image's provisioning
	// contract names.
	ProvisionerEntrypoint []string
}

// DefaultSettings resolves the run's parameters from the spike environment and
// the recorded profile.
//
// Everything that affects fidelity comes from the profile: block size, KV
// capacity, and the bounds the simulator enforces. A profile that declares no
// maximum model length bound is refused, because a spike that invented one would
// produce evidence for a configuration the recording never covered.
func DefaultSettings(env Environment, metadata *profile.Metadata) (Settings, error) {
	if metadata == nil {
		return Settings{}, errors.New("spike: no profile metadata: the run's limits come from the recorded profile")
	}
	if metadata.Coverage.MaxModelLen == nil {
		return Settings{}, fmt.Errorf(
			"spike: profile %q declares no coverage.maxModelLen: the spike will not invent a maximum model length the recording does not cover",
			metadata.ID)
	}

	settings := Settings{
		FrontendImage:    env.FrontendImage,
		BackendImage:     env.BackendImage,
		ProfileImage:     env.ProfileImage,
		ProfileDir:       env.ProfileDir,
		Model:            env.Model,
		InferencePort:    names.DefaultInferencePort,
		HandshakeAddress: fmt.Sprintf("tcp://127.0.0.1:%d", names.HandshakePort),
		ControlPort:      names.ControlPort,
		KVEventsPort:     names.KVEventsPort,
		ShutdownTimeout:  DefaultShutdownTimeout,
		BackendLossGrace: DefaultBackendLossGrace,
		TraceFile:        metadata.Trace.File,
		ContentReplay:    metadata.Capabilities.ContentReplay,
		MaxModelLen:      metadata.Coverage.MaxModelLen.Max,
		TokensPerBlock:   metadata.Cache.BlockSize,
		KVBlocks:         metadata.Cache.KVBlocks,
	}
	if settings.Model == "" {
		settings.Model = metadata.Model.Name
	}
	if metadata.Coverage.MaxNumSeqs != nil {
		settings.MaxNumSeqs = metadata.Coverage.MaxNumSeqs.Max
	}
	if len(metadata.Provisioning.Entrypoint) > 0 {
		settings.ProvisionerEntrypoint = metadata.Provisioning.Entrypoint
	} else {
		settings.ProvisionerEntrypoint = []string{defaultProvisionerEntrypoint}
	}
	return settings, nil
}

// TracePath is the exported trace file path the simulator replays.
func (s Settings) TracePath() string {
	return filepath.Join(s.ProfileDir, s.TraceFile)
}

// ContainerReadyFile is the readiness marker path inside the frontend container.
func (s Settings) ContainerReadyFile() string {
	return ContainerReadyFile
}

// InferenceURL is the frontend HTTP endpoint.
func (s Settings) InferenceURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(s.InferencePort)
}

// ControlURL is the simulator's control API endpoint.
func (s Settings) ControlURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(s.ControlPort)
}

// ReadinessURL is the supervisor's readiness endpoint, the readiness signal the
// contract's three inputs produce.
func (s Settings) ReadinessURL() string {
	return "http://" + SupervisorHealthAddress + "/readyz"
}

// FrontendCommand is the frontend container's argument tail: the supervisor runs
// the Python vLLM frontend, which binds the engine-core handshake and waits for
// the simulator to register.
//
// The command deliberately carries no accelerator settings. The workload's GPU
// engine is replaced, and the simulator advertises topology, cache capacity, and
// precision through the ready response.
func (s Settings) FrontendCommand() []string {
	return []string{
		names.SupervisorBinary,
		"--handshake-address", s.HandshakeAddress,
		"--ready-file", ContainerReadyFile,
		"--health-address", SupervisorHealthAddress,
		"--backend-loss-grace", s.BackendLossGrace.String(),
		"--shutdown-timeout", s.ShutdownTimeout.String(),
		"--",
		frontendBinary, "serve", s.Model,
		"--host", "0.0.0.0",
		"--port", strconv.Itoa(s.InferencePort),
		"--data-parallel-size", "1",
		"--data-parallel-size-local", "0",
		"--data-parallel-address", "127.0.0.1",
		"--data-parallel-rpc-port", strconv.Itoa(names.HandshakePort),
		"--max-model-len", strconv.FormatInt(s.MaxModelLen, 10),
		"--enable-prefix-caching",
	}
}

// BackendCommand is the simulator container's argument tail: `vllm-vcr play`
// connects to the handshake the frontend owns and serves the recorded profile.
func (s Settings) BackendCommand() []string {
	command := []string{
		backendBinary, "play",
		"--handshake-address", s.HandshakeAddress,
		"--model-name", s.Model,
		"--max-model-len", strconv.FormatInt(s.MaxModelLen, 10),
		"--tokens-per-block", strconv.Itoa(int(s.TokensPerBlock)),
		"--kv-cache-size", strconv.FormatInt(s.KVBlocks, 10),
		"--control-address", "tcp://0.0.0.0:" + strconv.Itoa(s.ControlPort),
		"--latency-trace", s.TracePath(),
		"--shutdown-timeout", strconv.Itoa(int(s.ShutdownTimeout.Seconds())),
		"--log-requests",
	}
	if s.MaxNumSeqs > 0 {
		command = append(command, "--max-num-seqs", strconv.FormatInt(s.MaxNumSeqs, 10))
	}
	if s.ContentReplay {
		command = append(command,
			"--replay-tokens", s.TracePath(),
			"--replay-match", "prefix",
		)
	}
	if s.EnableKVEvents {
		command = append(command,
			"--enable-kv-cache-events",
			"--kv-events-endpoint", fmt.Sprintf("tcp://*:%d", s.KVEventsPort),
			"--kv-events-topic", fmt.Sprintf("kv@127.0.0.1:%d@%s", s.InferencePort, s.Model),
		)
	}
	return command
}

// ProvisionerCommand is the profile provisioner init container's argument tail.
func (s Settings) ProvisionerCommand() []string {
	return append(append([]string{}, s.ProvisionerEntrypoint...), names.ProfileMountPath)
}

// FrontendEnv is the environment the frontend container receives: the readiness
// inputs the layout contract documents, plus the profile directory the simulator
// reads.
func (s Settings) FrontendEnv() []string {
	return []string{
		"VCR_HANDSHAKE_ADDRESS=" + s.HandshakeAddress,
		"VCR_FRONTEND_HEALTH_URL=" + s.InferenceURL() + "/health",
		"VCR_BACKEND_HEALTH_URL=" + s.ControlURL() + "/stats",
		"VCR_PROFILE_DIR=" + names.ProfileMountPath,
		"VCR_SHUTDOWN_TIMEOUT=" + strconv.Itoa(int(s.ShutdownTimeout.Seconds())),
		"VCR_READY_FILE=" + ContainerReadyFile,
	}
}

// BackendEnv is the simulator container's environment. The simulator takes its
// configuration from flags, so the only variable is its log filter.
func (s Settings) BackendEnv() []string {
	return []string{"RUST_LOG=info"}
}
