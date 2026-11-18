package profile

import (
	"fmt"
	"strings"
)

// Serving capability names accepted by Metadata.Supports and RequireCapability.
//
// The set mirrors the profile image contract. A feature absent from the profile
// metadata is not authorized: an available frontend flag is not evidence of
// verified end-to-end behavior.
const (
	CapabilityChatCompletions      = "chatCompletions"
	CapabilityCompletions          = "completions"
	CapabilityMessages             = "messages"
	CapabilityResponses            = "responses"
	CapabilityStreaming            = "streaming"
	CapabilityToolCallParsers      = "toolCallParsers"
	CapabilityReasoningParsers     = "reasoningParsers"
	CapabilityKVEventReplay        = "kvEventReplay"
	CapabilityLoRA                 = "lora"
	CapabilitySpeculativeDecoding  = "speculativeDecoding"
	CapabilityMultimodal           = "multimodal"
	CapabilityEmbeddings           = "embeddings"
	CapabilityStructuredOutputs    = "structuredOutputs"
	CapabilityDisaggregatedPrefill = "disaggregatedPrefill"
	CapabilityContentReplay        = "contentReplay"

	// toolCallParserPrefix qualifies a specific parser check, for example
	// "toolCallParser:hermes". The singular form matches the vLLM setting name.
	toolCallParserPrefix = "toolCallParser:"
	// reasoningParserPrefix qualifies a specific parser check, for example
	// "reasoningParser:deepseek_r1".
	reasoningParserPrefix = "reasoningParser:"
)

// knownCapabilities lists the capability names the operator can evaluate.
var knownCapabilities = map[string]struct{}{
	CapabilityChatCompletions:      {},
	CapabilityCompletions:          {},
	CapabilityMessages:             {},
	CapabilityResponses:            {},
	CapabilityStreaming:            {},
	CapabilityToolCallParsers:      {},
	CapabilityReasoningParsers:     {},
	CapabilityKVEventReplay:        {},
	CapabilityLoRA:                 {},
	CapabilitySpeculativeDecoding:  {},
	CapabilityMultimodal:           {},
	CapabilityEmbeddings:           {},
	CapabilityStructuredOutputs:    {},
	CapabilityDisaggregatedPrefill: {},
	CapabilityContentReplay:        {},
}

// RequireCapability rejects a serving feature the selected profile does not
// grant. The reason names the profile id and the capability so that an admission
// rejection identifies both what was requested and which recorded profile was
// considered.
func RequireCapability(m *Metadata, capability string) error {
	if m == nil {
		return fmt.Errorf("capability %q: no profile metadata: a profile must be selected before serving features are authorized", capability)
	}
	if !isKnownCapability(capability) {
		return fmt.Errorf("capability %q is not a known profile capability: it cannot be authorized by recorded metadata", capability)
	}
	if !m.Supports(capability) {
		return fmt.Errorf("profile %q does not support capability %q: the profile's validated capability set does not grant it", m.ID, capability)
	}
	return nil
}

func isKnownCapability(capability string) bool {
	if _, ok := knownCapabilities[capability]; ok {
		return true
	}
	if name, ok := strings.CutPrefix(capability, toolCallParserPrefix); ok {
		return name != ""
	}
	if name, ok := strings.CutPrefix(capability, reasoningParserPrefix); ok {
		return name != ""
	}
	return false
}
