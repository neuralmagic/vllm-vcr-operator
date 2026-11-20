package translator

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/plan"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/profile"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/supervisor"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/vllm"
)

// gpuOnlyEnvVars are environment variables that select accelerator hardware. They
// are removed from the frontend container: the simulation nodes have no GPU, and a
// frontend that tries to initialize one cannot start.
var gpuOnlyEnvVars = []string{
	"CUDA_VISIBLE_DEVICES",
	"NVIDIA_VISIBLE_DEVICES",
	"NVIDIA_DRIVER_CAPABILITIES",
}

// frontendInvocation builds the frontend container invocation.
//
// The container runs vcr-supervisor, which starts the Python vLLM frontend as its
// only long-lived child (docs/contracts/runtime-layout.md). The frontend runs in
// external-engine mode: it starts no local engine rank, binds the engine-core
// handshake on the handshake port, and connects the simulator that registers there.
// Preserved workload settings are passed as explicit flags so that the workload's
// HTTP, rendering, and credential behavior is unchanged.
func frontendInvocation(cfg *vllm.Config, eff effective, env Environment, preserved []string) plan.Invocation {
	args := []string{"vllm", "serve", modelName(cfg)}

	// Serving endpoint: re-emitted from the effective value so that an
	// environment-derived host or port is applied identically to a flag.
	args = append(args, "--host", eff.inferenceHost, "--port", strconv.Itoa(eff.inferencePort))

	// Maximum model length is enforced by both processes (specification,
	// "Settings policy"): the frontend would otherwise advertise and accept
	// prompts up to the model artifact's own maximum, while the simulator only
	// reproduces the recorded coverage. Both receive the same resolved bound.
	args = append(args, "--max-model-len", strconv.FormatInt(eff.maxModelLen, 10))

	// External-engine mode. data-parallel-size-local=0 is what makes the frontend
	// wait for an externally registered engine instead of starting one; the RPC
	// port is the engine-core handshake the simulator connects to.
	args = append(args,
		"--data-parallel-size", "1",
		"--data-parallel-size-local", "0",
		"--data-parallel-address", "127.0.0.1",
		"--data-parallel-rpc-port", strconv.Itoa(eff.handshakePort),
	)
	args = append(args, preserved...)

	removeEnv := append([]string(nil), gpuOnlyEnvVars...)
	for _, rule := range policy {
		if rule.RemoveEnv && cfg.IsExplicit(rule.Setting) {
			removeEnv = append(removeEnv, string(rule.Setting))
		}
	}

	// An API key passed on the workload's command line is copied verbatim at
	// mutation time: the plan must never carry the key.
	var copied []plan.CopiedArgument
	if cfg.IsExplicit(vllm.SettingAPIKey) {
		copied = append(copied, plan.CopiedArgument{
			Setting:    string(vllm.SettingAPIKey),
			Spellings:  []string{string(vllm.SettingAPIKey)},
			TakesValue: true,
		})
	}

	return plan.Invocation{
		Image: env.Images.Frontend,
		// The supervisor receives the frontend command after "--".
		Command: []string{names.SupervisorBinary, "--"},
		Args:    args,
		Env: []corev1.EnvVar{
			{Name: "VCR_HANDSHAKE_ADDRESS", Value: handshakeAddress(eff)},
			// Readiness combines the frontend's own health endpoint with the
			// simulator's control API (docs/contracts/runtime-layout.md). The
			// frontend port is the workload's, not a fixed 8000.
			{Name: "VCR_FRONTEND_HEALTH_URL", Value: fmt.Sprintf("http://127.0.0.1:%d/health", eff.inferencePort)},
			{Name: "VCR_BACKEND_HEALTH_URL", Value: fmt.Sprintf("http://127.0.0.1:%d/stats", names.ControlPort)},
			{Name: "VCR_PROFILE_DIR", Value: names.ProfileMountPath},
			{Name: "VCR_SHUTDOWN_TIMEOUT", Value: strconv.Itoa(eff.shutdownTimeout)},
			{Name: "VCR_READY_FILE", Value: supervisor.DefaultReadyFile},
		},
		RemoveEnv: dedupeStrings(removeEnv),
		Ports: []corev1.ContainerPort{
			{Name: "inference", ContainerPort: int32(eff.inferencePort), Protocol: corev1.ProtocolTCP},
			{Name: "engine-hs", ContainerPort: int32(eff.handshakePort), Protocol: corev1.ProtocolTCP},
		},
		CopiedArguments: copied,
	}
}

// backendInvocation builds the simulator container invocation.
//
// Every argument comes from SimulatorCommand, the builder the spikes also use, so
// the shipped flags and the flags the evidence was produced with cannot drift. The
// simulator emits no Prometheus metrics and has no GPU-only flag: metrics come from
// the frontend, and the simulator records timing and tokens.
func backendInvocation(cfg *vllm.Config, eff effective, env Environment, metadata *profile.Metadata, podIdentity string) plan.Invocation {
	trace := resolveRecordedTrace(metadata)
	// A controller-created Pod has no name at admission time: the API server
	// generates it. The engine id is therefore a kubelet-expanded placeholder that
	// the runtime guarantees through the downward API.
	if strings.TrimSpace(podIdentity) == "" {
		podIdentity = "$(POD_NAME)"
	}
	input := SimulatorInput{
		HandshakeAddress:    handshakeAddress(eff),
		ModelName:           modelName(cfg),
		MaxModelLen:         eff.maxModelLen,
		TokensPerBlock:      eff.blockSize,
		KVBlocks:            eff.kvBlocks,
		ShutdownTimeout:     eff.shutdownTimeout,
		Seed:                eff.seed,
		ControlAddress:      fmt.Sprintf("0.0.0.0:%d", names.ControlPort),
		MaxNumSeqs:          eff.maxNumSeqs,
		MaxNumBatchedTokens: eff.maxNumBatchedTokens,
		LongPrefillTokens:   eff.longPrefillTokens,
		ChunkedPrefill:      eff.chunkedPrefill,
		SchedulingPolicy:    eff.schedulingPolicy,
		LogRequests:         eff.logRequests,
		EngineID:            podIdentity,
		VocabSize:           metadata.Model.VocabSize,
		MaxLoras:            eff.maxLoras,
		PDRole:              eff.pdRole,
		KVEvents:            eff.kvEvents.enabled,
		KVEventsEndpoint:    eff.kvEvents.endpoint,
		KVEventsTopic:       eff.kvEvents.topic,
		LatencyTracePath:    trace.latencyPath,
		ReplayTokensPath:    trace.replayPath,
		ExpectConfigHash:    trace.configHash,
	}

	ports := []corev1.ContainerPort{
		{Name: "vcr-control", ContainerPort: int32(names.ControlPort), Protocol: corev1.ProtocolTCP},
	}
	if eff.kvEvents.enabled && eff.kvEvents.port > 0 {
		ports = append(ports, corev1.ContainerPort{
			Name:          "kv-events",
			ContainerPort: int32(eff.kvEvents.port),
			Protocol:      corev1.ProtocolTCP,
		})
	}

	return plan.Invocation{
		Image:   env.Images.Backend,
		Command: SimulatorCommand(input),
		Env:     []corev1.EnvVar{{Name: "RUST_LOG", Value: env.LogLevel}},
		Ports:   ports,
	}
}

// recordedTrace is the resolved trace content the simulator is pointed at: the
// recorded latency trace always (the policy rejects a profile without one), and the
// recorded token trace only when the profile authorizes content-identical replay.
type recordedTrace struct {
	latencyPath string
	replayPath  string
	configHash  string
}

// traceSettings resolves the recorded-profile arguments.
//
// Timing always comes from the profile's latency trace: the policy rejects a profile
// without one, because serving on synthetic timing would misrepresent the recorded
// hardware. Content replay is separate: it is requested only when the profile
// authorizes content-identical replay with recorded token ids, and the config hash is
// compared only when the profile records one.
func resolveRecordedTrace(metadata *profile.Metadata) recordedTrace {
	if metadata == nil || metadata.Trace.File == "" {
		return recordedTrace{}
	}
	path := names.ProfileMountPath + "/" + metadata.Trace.File
	out := recordedTrace{latencyPath: path}
	if metadata.Supports(profile.CapabilityContentReplay) {
		out.replayPath = path
	}
	out.configHash = profile.ConfigHashValue(metadata.VLLM.ConfigHash)
	return out
}

// provisionerInvocation builds the init container that exports the profile into
// the shared volume.
//
// The profile image carries both its metadata and its trace and declares the
// entrypoint that exports them (docs/contracts/profile-image.md), so the pinned
// profile image runs its own provisioning command with the volume path as its only
// argument.
func provisionerInvocation(selection profile.Selection) (plan.Invocation, error) {
	if len(selection.Metadata.Provisioning.Entrypoint) == 0 {
		return plan.Invocation{}, Rejections{{
			Setting: "profile provisioning.entrypoint",
			Reason: fmt.Sprintf("profile %q declares no provisioning entrypoint, so its metadata and trace cannot be exported into the Pod",
				selection.ID),
		}}
	}
	command := append([]string(nil), selection.Metadata.Provisioning.Entrypoint...)
	command = append(command, names.ProfileMountPath)
	return plan.Invocation{
		Image:   plan.Profile{Image: selection.Image, Digest: selection.Digest}.PinnedImage(),
		Command: command,
	}, nil
}

// handshakeAddress is the engine-core handshake endpoint. The frontend binds it and
// the simulator connects to it, so it is always the Pod's loopback address.
func handshakeAddress(eff effective) string {
	return fmt.Sprintf("tcp://127.0.0.1:%d", eff.handshakePort)
}
