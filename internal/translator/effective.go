package translator

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/profile"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/vllm"
)

// effective holds the resolved values the injected runtime is configured with.
//
// Two rules govern the resolution, and both are documented in
// docs/settings-policy.md:
//
//   - a setting the workload asked for is used verbatim after the policy
//     authorized it;
//   - a setting the workload did not ask for falls back to the profile's
//     recorded value where the metadata records one, and to the profile's
//     declared coverage maximum where it does not. The simulator's built-in
//     default is used only when the profile declares no coverage at all, and that
//     case is reported as a warning because it is a default the profile does not
//     vouch for.
type effective struct {
	handshakePort int
	inferenceHost string
	inferencePort int

	maxModelLen         int64
	maxNumSeqs          int64
	maxNumBatchedTokens int64
	longPrefillTokens   int64
	kvBlocks            int64
	blockSize           int64
	schedulingPolicy    string
	chunkedPrefill      bool
	prefixCaching       bool

	shutdownTimeout int
	seed            int
	logRequests     bool
	pdRole          string
	maxLoras        int

	kvEvents kvEventsSettings

	// warnings records accepted-but-not-vouched-for resolutions.
	warnings []string
}

// kvEventsSettings is the resolved KV-cache event publisher configuration.
type kvEventsSettings struct {
	enabled bool
	// endpoint is the ZMQ endpoint the simulator publishes on. A wildcard host
	// binds, a concrete host connects, which matches the simulator's semantics.
	endpoint string
	// topic is the llm-d topic, "kv@<pod>@<model>".
	topic string
	// port is the container port the endpoint exposes, 0 when the endpoint does
	// not name a port the Pod exposes.
	port int
}

// resolveEffective resolves the runtime values for an admitted workload.
func resolveEffective(cfg *vllm.Config, metadata *profile.Metadata, req Request, env Environment) (effective, Rejections) {
	eff := effective{
		inferenceHost: "0.0.0.0",
		inferencePort: names.DefaultInferencePort,
		seed:          intValue(cfg.Generation.Seed),
	}
	var rejections Rejections

	if cfg.Server.Host != nil && strings.TrimSpace(*cfg.Server.Host) != "" {
		eff.inferenceHost = *cfg.Server.Host
	}
	if cfg.Server.Port != nil && *cfg.Server.Port > 0 {
		eff.inferencePort = *cfg.Server.Port
	}
	if isLoopbackHost(eff.inferenceHost) {
		eff.warnings = append(eff.warnings, fmt.Sprintf(
			"--host: the frontend binds %q, so kubelet probes on the Pod IP and llm-d discovery cannot reach it",
			eff.inferenceHost))
	} else if !isWildcardHost(eff.inferenceHost) {
		eff.warnings = append(eff.warnings, fmt.Sprintf(
			"--host: the frontend binds %q while readiness observes 127.0.0.1:%d, which that address does not accept",
			eff.inferenceHost, eff.inferencePort))
	}

	// Engine-core handshake port: the frontend binds it, the simulator connects.
	eff.handshakePort = names.HandshakePort
	if cfg.IsExplicit(vllm.SettingDataParallelRPC) && cfg.Parallel.DataParallel.RPCPort != nil {
		port := *cfg.Parallel.DataParallel.RPCPort
		if port <= 0 || port > 65535 {
			rejections = append(rejections, Rejection{
				Setting: string(vllm.SettingDataParallelRPC),
				Reason:  fmt.Sprintf("port %d is not a usable TCP port", port),
			})
		} else {
			eff.handshakePort = port
		}
	}

	// Maximum model length: enforced by the frontend and the simulator.
	eff.maxModelLen, eff.warnings, rejections = resolveMaxModelLen(cfg, metadata, eff.warnings, rejections)

	// Chunking and cache representation: the profile records them, so a workload
	// that changes them is rejected rather than approximated.
	eff.chunkedPrefill = metadata.Cache.ChunkedPrefill
	if cfg.IsExplicit(vllm.SettingChunkedPrefill) && cfg.Scheduling.ChunkedPrefill != nil {
		if *cfg.Scheduling.ChunkedPrefill != metadata.Cache.ChunkedPrefill {
			rejections = append(rejections, Rejection{
				Setting: string(vllm.SettingChunkedPrefill),
				Reason: fmt.Sprintf("the workload asks for chunked prefill %t but profile %q was recorded with it %t: the recorded timing and cache representation cannot serve the other mode",
					*cfg.Scheduling.ChunkedPrefill, metadata.ID, metadata.Cache.ChunkedPrefill),
			})
		}
	}
	eff.prefixCaching = metadata.Cache.EnablePrefixCaching
	if cfg.IsExplicit(vllm.SettingPrefixCaching) && cfg.Cache.EnablePrefixCaching != nil {
		if *cfg.Cache.EnablePrefixCaching != metadata.Cache.EnablePrefixCaching {
			rejections = append(rejections, Rejection{
				Setting: string(vllm.SettingPrefixCaching),
				Reason: fmt.Sprintf("the workload asks for prefix caching %t but profile %q was recorded with it %t: the recorded cache representation cannot serve the other mode",
					*cfg.Cache.EnablePrefixCaching, metadata.ID, metadata.Cache.EnablePrefixCaching),
			})
		}
	}

	// Batching limits.
	eff.maxNumSeqs, eff.warnings = resolveLimit(explicitInt(cfg, vllm.SettingMaxNumSeqs, cfg.Limits.MaxNumSeqs),
		metadata.Coverage.MaxNumSeqs, string(vllm.SettingMaxNumSeqs), eff.warnings)
	eff.maxNumBatchedTokens, eff.warnings = resolveLimit(explicitInt(cfg, vllm.SettingMaxNumBatchedTokens, cfg.Limits.MaxNumBatchedTokens),
		metadata.Coverage.MaxNumBatchedTokens, string(vllm.SettingMaxNumBatchedTokens), eff.warnings)

	// Chunked prefill and the per-step budget interact: without chunking, one
	// prompt must fit in a single step (combinations.go).
	if rejection := checkBatchingCombination(eff.chunkedPrefill, eff.maxNumBatchedTokens, eff.maxModelLen); rejection != nil {
		rejections = append(rejections, *rejection)
	}

	if eff.chunkedPrefill {
		eff.longPrefillTokens, eff.warnings = resolveLimit(explicitInt(cfg, vllm.SettingLongPrefillTokens, cfg.Scheduling.LongPrefillTokenThreshold),
			metadata.Coverage.LongPrefillTokenThreshold, string(vllm.SettingLongPrefillTokens), eff.warnings)
	}

	// KV cache capacity.
	eff.kvBlocks = metadata.Cache.KVBlocks
	if cfg.Memory.NumGPUBlocksOverride != nil {
		eff.kvBlocks = int64(*cfg.Memory.NumGPUBlocksOverride)
	}
	eff.blockSize = int64(metadata.Cache.BlockSize)

	// Scheduling policy.
	eff.schedulingPolicy, eff.warnings = resolveSchedulingPolicy(cfg, metadata, eff.warnings)
	if eff.schedulingPolicy != "" && eff.schedulingPolicy != "fcfs" && eff.schedulingPolicy != "priority" {
		rejections = append(rejections, Rejection{
			Setting: string(vllm.SettingSchedulingPolicy),
			Reason:  fmt.Sprintf("policy %q is not supported: the simulator implements FCFS and priority only", eff.schedulingPolicy),
		})
	}

	// Lifecycle timing.
	if cfg.Lifecycle.ShutdownTimeoutSeconds != nil && *cfg.Lifecycle.ShutdownTimeoutSeconds >= 0 {
		eff.shutdownTimeout = *cfg.Lifecycle.ShutdownTimeoutSeconds
	}
	eff.logRequests = cfg.Observability.EnableLogRequests != nil && *cfg.Observability.EnableLogRequests

	if !metadata.Trace.LatencyModel {
		rejections = append(rejections, Rejection{
			Setting: "profile trace",
			Reason: fmt.Sprintf("profile %q records no latency model: serving would fall back to synthetic timing, which the policy forbids",
				metadata.ID),
		})
	}

	// Capability-gated serving features that need runtime values.
	var kvRejections Rejections
	eff.kvEvents, eff.warnings, kvRejections = resolveKVEvents(cfg, metadata, req, eff, eff.warnings)
	rejections = append(rejections, kvRejections...)
	eff.pdRole, eff.warnings, kvRejections = resolveKVTransfer(cfg, metadata, eff.warnings)
	rejections = append(rejections, kvRejections...)
	eff.maxLoras = resolveMaxLoras(cfg, metadata)

	// Ports must stay distinct: the frontend, the control API, and the KV-event
	// publisher share the Pod network namespace (combinations.go).
	rejections = append(rejections, checkPortCollisions(podPorts(eff))...)

	return eff, rejections
}

// resolveMaxModelLen resolves the effective model length. An increase beyond the
// recorded coverage is rejected, a lower cap inside it is translated, and an
// unset (or "auto") value resolves to the coverage maximum because the operator
// never learns the model artifact's own maximum.
func resolveMaxModelLen(cfg *vllm.Config, metadata *profile.Metadata, warnings []string, rejections Rejections) (int64, []string, Rejections) {
	setting := string(vllm.SettingMaxModelLen)
	if cfg.Limits.MaxModelLen != nil && *cfg.Limits.MaxModelLen > 0 {
		return int64(*cfg.Limits.MaxModelLen), warnings, rejections
	}
	switch {
	case metadata.Coverage.MaxModelLen == nil:
		rejections = append(rejections, Rejection{
			Setting: setting,
			Reason: fmt.Sprintf("the workload does not set a maximum model length and profile %q declares no coverage.maxModelLen, so no authorized bound exists; declare the setting or extend the profile coverage",
				metadata.ID),
		})
		return 0, warnings, rejections
	case cfg.Limits.MaxModelLen != nil && *cfg.Limits.MaxModelLen < 0:
		// -1/"auto" asks for the model maximum. The profile's declared bound is
		// the largest length the recording supports.
		warnings = append(warnings, fmt.Sprintf(
			"%s: the workload asks for the model maximum, which resolves to profile %q coverage.maxModelLen %d",
			setting, metadata.ID, metadata.Coverage.MaxModelLen.Max))
		return metadata.Coverage.MaxModelLen.Max, warnings, rejections
	default:
		return metadata.Coverage.MaxModelLen.Max, warnings, rejections
	}
}

// explicitInt returns the workload's value for a setting, or nil when the workload
// did not set it: a resolved default is not a workload request.
func explicitInt(cfg *vllm.Config, setting vllm.Setting, value *int) *int {
	if !cfg.IsExplicit(setting) {
		return nil
	}
	return value
}

// resolveLimit resolves one batching limit: the workload's value when it set one,
// otherwise the profile's declared coverage maximum, otherwise nothing (the
// simulator default applies and the plan says so).
func resolveLimit(explicit *int, coverage *profile.Range, setting string, warnings []string) (int64, []string) {
	if explicit != nil {
		return int64(*explicit), warnings
	}
	if coverage != nil {
		return coverage.Max, warnings
	}
	warnings = append(warnings, fmt.Sprintf(
		"%s: the workload does not set it and the profile declares no coverage, so the simulator's built-in default applies",
		setting))
	return 0, warnings
}

// resolveSchedulingPolicy resolves the scheduling policy. FCFS is vLLM's default,
// so it is used when authorized; otherwise the simulator default stands and the
// plan records that the profile does not vouch for it.
func resolveSchedulingPolicy(cfg *vllm.Config, metadata *profile.Metadata, warnings []string) (string, []string) {
	if cfg.IsExplicit(vllm.SettingSchedulingPolicy) {
		return stringValue(cfg.Scheduling.Policy), warnings
	}
	for _, policy := range metadata.Coverage.SchedulingPolicies {
		if policy == "fcfs" {
			return "fcfs", warnings
		}
	}
	warnings = append(warnings, fmt.Sprintf(
		"%s: the workload does not set it and profile %q does not authorize fcfs, so the simulator's built-in policy applies",
		vllm.SettingSchedulingPolicy, metadata.ID))
	return "", warnings
}

// simulatorReplaySupport reports whether the pinned simulator build implements the
// KV-event replay endpoint. The pinned build does not: its ready response always
// reports replay_endpoint null and its publisher has no responder
// (docs/upstream-vllm-vcr.md), so every explicit replay request is rejected with a
// reason that names the pin. Re-pinning a build that implements replay changes this
// function and the profile capability gate below it, not the policy table.
func simulatorReplaySupport() error {
	return fmt.Errorf(
		"the pinned vllm-vcr release %s has no KV-event replay endpoint: its ready response always reports replay_endpoint null, so a consumer that loses the event window could never re-synchronize; remove replay_endpoint or re-pin a simulator release that implements it",
		names.SupportedSimulatorTag)
}

// resolveKVEvents resolves the KV-cache event publisher configuration.
func resolveKVEvents(cfg *vllm.Config, metadata *profile.Metadata, req Request, eff effective, warnings []string) (kvEventsSettings, []string, Rejections) {
	var rejections Rejections
	var out kvEventsSettings
	events := cfg.KVEvents
	if events == nil {
		return out, warnings, rejections
	}
	setting := string(vllm.SettingKVEventsConfig)

	// KV-event replay is rejected outright for the pinned simulator build, which
	// has no replay responder: an explicit replay endpoint would promise a
	// re-synchronization that can never happen. The pin check is deliberately the
	// broader gate and runs first; the profile capability check stays as the
	// narrower second gate, so re-pinning a simulator that implements replay
	// relaxes exactly one decision (simulatorReplaySupport) instead of two.
	if strings.TrimSpace(stringValue(events.ReplayEndpoint)) != "" {
		switch replayErr := simulatorReplaySupport(); {
		case replayErr != nil:
			rejections = append(rejections, Rejection{Setting: setting, Reason: replayErr.Error()})
		default:
			if err := profile.RequireCapability(metadata, profile.CapabilityKVEventReplay); err != nil {
				rejections = append(rejections, Rejection{Setting: setting, Reason: err.Error()})
			} else {
				warnings = append(warnings, fmt.Sprintf(
					"%s: KV-event replay is authorized by profile capabilities.kvEventReplay, but the pinned simulator publishes live events only: a consumer that loses the event window cannot be re-synchronized from the engine",
					setting))
			}
		}
	}

	// An explicit --kv-events-config publishes events unless it disables them; a
	// "null" publisher is vLLM's way of asking for no publisher at all.
	enabled := (events.Enable == nil || *events.Enable) && !strings.EqualFold(stringValue(events.Publisher), "null")
	if !enabled {
		return out, warnings, rejections
	}

	if !metadata.Trace.KVEvents {
		rejections = append(rejections, Rejection{
			Setting: setting,
			Reason: fmt.Sprintf("profile %q records no KV-cache events (trace.kvEvents is false), so the simulator cannot publish the stream the workload configures",
				metadata.ID),
		})
	}

	// Prefix caching disabled with block events is only serveable when the
	// recording itself published events with caching disabled.
	if !eff.prefixCaching && !(metadata.Trace.KVEvents && !metadata.Cache.EnablePrefixCaching) {
		rejections = append(rejections, Rejection{
			Setting: setting,
			Reason: fmt.Sprintf("block events are requested while prefix caching is disabled, but profile %q was not recorded publishing events with caching disabled",
				metadata.ID),
		})
	}

	if publisher := stringValue(events.Publisher); publisher != "" && !strings.EqualFold(publisher, "zmq") {
		rejections = append(rejections, Rejection{
			Setting: setting,
			Reason:  fmt.Sprintf("publisher %q is not supported: the simulator publishes over ZMQ", publisher),
		})
	}
	if events.BufferSteps != nil && *events.BufferSteps != 0 {
		rejections = append(rejections, Rejection{
			Setting: setting,
			Reason:  "bufferSteps is not supported: the simulator's publisher has no step buffer to configure",
		})
	}
	if events.HWM != nil && *events.HWM != 0 {
		rejections = append(rejections, Rejection{
			Setting: setting,
			Reason:  "hwm is not supported: the simulator's publisher has no configurable high-water mark",
		})
	}
	if events.MaxQueueSize != nil && *events.MaxQueueSize != 0 {
		rejections = append(rejections, Rejection{
			Setting: setting,
			Reason:  "maxQueueSize is not supported: the simulator's publisher has no configurable queue",
		})
	}
	for _, key := range sortedKeysOfRaw(events.Raw) {
		switch key {
		case "enable_kv_cache_events", "publisher", "endpoint", "replay_endpoint", "topic", "buffer_steps", "hwm", "max_queue_size":
		default:
			rejections = append(rejections, Rejection{
				Setting: setting,
				Reason:  fmt.Sprintf("field %q is not an event-publisher setting this operator can apply", key),
			})
		}
	}

	// The replay endpoint is not a simulator flag: it is advertised in the ready
	// response, which the pinned build always reports as null, so there is nothing
	// to forward. The gate above already rejected an explicit one.
	out.enabled = true
	out.endpoint = fmt.Sprintf("tcp://*:%d", names.KVEventsPort)
	out.port = names.KVEventsPort
	if endpoint := stringValue(events.Endpoint); endpoint != "" {
		parsed := endpointPort(endpoint)
		out.endpoint = endpoint
		out.port = parsed
	}
	if topic := stringValue(events.Topic); strings.TrimSpace(topic) != "" {
		out.topic = topic
	} else if identity := strings.TrimSpace(req.PodIdentity); identity != "" {
		out.topic = fmt.Sprintf("kv@%s@%s", identity, modelName(cfg))
	} else {
		warnings = append(warnings, fmt.Sprintf(
			"%s: no Pod identity was supplied, so the simulator derives the KV-event topic from its engine id instead of the llm-d form kv@<pod>@<model>",
			setting))
	}
	return out, warnings, rejections
}

// resolveKVTransfer resolves the P/D role of the simulated engine. The setting is
// capability-gated by the policy table, so reaching this point means the profile
// claims validated disaggregated-prefill support.
func resolveKVTransfer(cfg *vllm.Config, metadata *profile.Metadata, warnings []string) (string, []string, Rejections) {
	var rejections Rejections
	transfer := cfg.KVTransfer
	if transfer == nil {
		return "", warnings, rejections
	}
	setting := string(vllm.SettingKVTransferConfig)
	role := ""
	switch strings.ToLower(strings.TrimSpace(stringValue(transfer.Role))) {
	case "", "kv_both", "both", "none":
		role = "both"
	case "kv_producer", "prefill":
		role = "prefill"
	case "kv_consumer", "decode":
		role = "decode"
	default:
		rejections = append(rejections, Rejection{
			Setting: setting,
			Reason:  fmt.Sprintf("role %q is not a KV-transfer role this operator can translate", stringValue(transfer.Role)),
		})
		return "", warnings, rejections
	}
	if connector := stringValue(transfer.Connector); connector != "" {
		warnings = append(warnings, fmt.Sprintf(
			"%s: connector %q is not executed; the simulator advertises the %q role and records transfer latency instead",
			setting, connector, role))
	}
	if err := profile.RequireCapability(metadata, profile.CapabilityDisaggregatedPrefill); err != nil {
		rejections = append(rejections, Rejection{Setting: setting, Reason: err.Error()})
	}
	return role, warnings, rejections
}

// resolveMaxLoras returns the simulator's diversity cap when LoRA serving is
// authorized and enabled.
func resolveMaxLoras(cfg *vllm.Config, metadata *profile.Metadata) int {
	enabled := cfg.LoRA.Enabled != nil && *cfg.LoRA.Enabled
	if !enabled || !metadata.Supports(profile.CapabilityLoRA) {
		return 0
	}
	if cfg.LoRA.MaxLoras != nil && *cfg.LoRA.MaxLoras > 0 {
		return *cfg.LoRA.MaxLoras
	}
	return 1
}

// endpointPort returns the TCP port a ZMQ endpoint names, or 0.
func endpointPort(endpoint string) int {
	trimmed := strings.TrimSpace(endpoint)
	if i := strings.LastIndex(trimmed, ":"); i >= 0 {
		port, err := strconv.Atoi(trimmed[i+1:])
		if err == nil && port > 0 && port <= 65535 {
			return port
		}
	}
	return 0
}

// isWildcardHost reports whether host accepts connections on every address.
func isWildcardHost(host string) bool {
	switch strings.TrimSpace(host) {
	case "", "0.0.0.0", "::", "[::]":
		return true
	default:
		return false
	}
}

// isLoopbackHost reports whether host only accepts connections from inside the
// Pod.
func isLoopbackHost(host string) bool {
	switch strings.TrimSpace(host) {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	default:
		return false
	}
}

func intValue(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// sortedKeysOfRaw returns the keys of a JSON object in sorted order.
func sortedKeysOfRaw(in vllm.JSONMap) []string {
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
