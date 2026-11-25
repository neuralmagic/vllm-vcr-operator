package translator

import (
	"fmt"
	"sort"

	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
	"github.com/neuralmagic/vllm-vcr-operator/internal/vllm"
)

// Combination checks.
//
// The policy table authorizes settings one at a time; these checks authorize them
// together. They live here rather than inside the value resolution in effective.go
// so that "every combination docs/settings-policy.md names is checked" is one
// reviewable file, and so a resolution change cannot accidentally drop a
// combination rule.

// checkPortCollisions rejects two processes that would bind the same port in the
// Pod network namespace (inference, engine-core handshake, control API, KV-event
// publisher).
func checkPortCollisions(ports map[string]int) Rejections {
	var rejections Rejections
	seen := make(map[int]string, len(ports))
	for _, name := range sortedKeysOf(ports) {
		port := ports[name]
		if prior, clash := seen[port]; clash {
			rejections = append(rejections, Rejection{
				Setting: "ports",
				Reason: fmt.Sprintf("the %s port %d collides with the %s port in the same Pod network namespace; one of them must be changed",
					name, port, prior),
			})
			continue
		}
		seen[port] = name
	}
	return rejections
}

// checkBatchingCombination rejects a per-step token budget that cannot hold a
// prompt while chunked prefill is disabled: the prompt would never be scheduled.
func checkBatchingCombination(chunkedPrefill bool, maxNumBatchedTokens, maxModelLen int64) *Rejection {
	if chunkedPrefill || maxNumBatchedTokens <= 0 || maxModelLen <= maxNumBatchedTokens {
		return nil
	}
	return &Rejection{
		Setting: string(vllm.SettingMaxNumBatchedTokens),
		Reason: fmt.Sprintf("per-step token budget %d is smaller than the maximum model length %d while chunked prefill is disabled: a prompt could never be scheduled in one step",
			maxNumBatchedTokens, maxModelLen),
	}
}

// podPorts returns the ports the Pod's processes share, named for the
// collision message. The KV-event port is only part of the namespace when the
// publisher is enabled.
func podPorts(eff effective) map[string]int {
	ports := map[string]int{
		"inference": eff.inferencePort,
		"handshake": eff.handshakePort,
		"control":   names.ControlPort,
	}
	if eff.kvEvents.enabled && eff.kvEvents.port > 0 {
		ports["kv-events"] = eff.kvEvents.port
	}
	return ports
}

// sortedKeysOf returns the keys of a string-keyed map with an integer value in
// sorted order, so a rejection set is deterministic.
func sortedKeysOf(in map[string]int) []string {
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
