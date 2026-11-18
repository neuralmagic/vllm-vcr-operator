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
// Every argument is a real vllm-vcr `play` flag; the MOCK_* variables of the
// example manifests are entrypoint-script inventions and are not used. The
// simulator emits no Prometheus metrics and has no GPU-only flag: metrics come from
// the frontend, and the simulator records timing and tokens.
func backendInvocation(cfg *vllm.Config, eff effective, env Environment, metadata *profile.Metadata, podIdentity string) plan.Invocation {
	args := []string{
		"--handshake-address", handshakeAddress(eff),
		"--model-name", modelName(cfg),
		"--max-model-len", strconv.FormatInt(eff.maxModelLen, 10),
		"--tokens-per-block", strconv.FormatInt(eff.blockSize, 10),
		"--kv-cache-size", strconv.FormatInt(eff.kvBlocks, 10),
		"--shutdown-timeout", strconv.Itoa(eff.shutdownTimeout),
		"--seed", strconv.Itoa(eff.seed),
		// The control API is the only in-Pod witness for replay match rates and
		// request statistics. It is JSON, not a metrics endpoint.
		"--control-address", fmt.Sprintf("tcp://0.0.0.0:%d", names.ControlPort),
	}
	if eff.maxNumSeqs > 0 {
		args = append(args, "--max-num-seqs", strconv.FormatInt(eff.maxNumSeqs, 10))
	}
	if eff.maxNumBatchedTokens > 0 {
		args = append(args, "--max-num-batched-tokens", strconv.FormatInt(eff.maxNumBatchedTokens, 10))
	}
	if eff.chunkedPrefill {
		args = append(args, "--long-prefill-token-threshold", strconv.FormatInt(eff.longPrefillTokens, 10))
	}
	if eff.schedulingPolicy != "" {
		args = append(args, "--scheduling-policy", eff.schedulingPolicy)
	}
	if eff.logRequests {
		args = append(args, "--log-requests")
	}
	if identity := strings.TrimSpace(podIdentity); identity != "" {
		// A stable engine id keeps the ready-response instance id and the default
		// KV-event topic tied to the Pod rather than to a process-local default.
		args = append(args, "--engine-id", identity)
	}
	if eff.maxLoras > 0 {
		args = append(args, "--max-loras", strconv.Itoa(eff.maxLoras))
	}
	if eff.pdRole != "" && eff.pdRole != "both" {
		args = append(args, "--pd-role", eff.pdRole)
	}
	if eff.kvEvents.enabled {
		args = append(args, "--enable-kv-cache-events", "--kv-events-endpoint", eff.kvEvents.endpoint)
		if eff.kvEvents.topic != "" {
			args = append(args, "--kv-events-topic", eff.kvEvents.topic)
		}
	}
	args = append(args, traceArgs(metadata)...)

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
		Command: append([]string{"vllm-vcr", "play"}, args...),
		Env:     []corev1.EnvVar{{Name: "RUST_LOG", Value: env.LogLevel}},
		Ports:   ports,
	}
}

// traceArgs returns the recorded-profile arguments the simulator replays.
//
// Timing always comes from the profile's latency trace: the policy rejects a
// profile without one, because serving on synthetic timing would misrepresent the
// recorded hardware. Content replay is separate: it is requested only when the
// profile authorizes content-identical replay with recorded token ids.
func traceArgs(metadata *profile.Metadata) []string {
	if metadata == nil || metadata.Trace.File == "" {
		return nil
	}
	tracePath := names.ProfileMountPath + "/" + metadata.Trace.File
	args := []string{"--latency-trace", tracePath}
	if metadata.Supports(profile.CapabilityContentReplay) {
		args = append(args,
			"--replay-tokens", tracePath,
			// Live clients (routers, load generators) do not use the replay
			// harness's request ids, so prefix matching against the recorded block
			// hashes is the only match mode that works for them.
			"--replay-match", "prefix",
		)
	}
	if metadata.VLLM.ConfigHash != "" {
		args = append(args, "--expect-config-hash", metadata.VLLM.ConfigHash)
	}
	return args
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
