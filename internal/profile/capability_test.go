package profile_test

import (
	"strings"
	"testing"

	"github.com/neuralmagic/vllm-vcr-operator/internal/profile"
)

func TestSupportsReportsDeclaredCapabilities(t *testing.T) {
	m := &profile.Metadata{
		ID: "qwen3-0.6b-h200-tp1-bf16",
		Trace: profile.Trace{
			RecordedTokens: true,
		},
		Capabilities: profile.Capabilities{
			ChatCompletions: true,
			Streaming:       true,
			ToolCallParsers: []string{"hermes"},
			ContentReplay:   true,
		},
	}

	tests := []struct {
		capability string
		want       bool
	}{
		{profile.CapabilityChatCompletions, true},
		{profile.CapabilityStreaming, true},
		{profile.CapabilityCompletions, false},
		{profile.CapabilityMessages, false},
		{profile.CapabilityKVEventReplay, false},
		{profile.CapabilityLoRA, false},
		{"toolCallParser:hermes", true},
		{"toolCallParser:mistral", false},
		{"reasoningParser:deepseek_r1", false},
		{profile.CapabilityContentReplay, true},
		{"featureThisOperatorHasNeverHeardOf", false},
	}
	for _, tc := range tests {
		if got := m.Supports(tc.capability); got != tc.want {
			t.Errorf("Supports(%q) = %v, want %v", tc.capability, got, tc.want)
		}
	}
}

func TestSupportsContentReplayRequiresRecordedTokens(t *testing.T) {
	m := &profile.Metadata{
		ID: "qwen3-0.6b-h200-tp1-bf16",
		Trace: profile.Trace{
			RecordedTokens: false,
		},
		Capabilities: profile.Capabilities{ContentReplay: true},
	}

	if m.Supports(profile.CapabilityContentReplay) {
		t.Error("Supports(contentReplay) = true without recorded token IDs, want false")
	}
}

func TestRequireCapabilityAcceptsDeclaredCapability(t *testing.T) {
	m := &profile.Metadata{
		ID:           "qwen3-0.6b-h200-tp1-bf16",
		Capabilities: profile.Capabilities{Embeddings: true},
	}

	if err := profile.RequireCapability(m, profile.CapabilityEmbeddings); err != nil {
		t.Fatalf("RequireCapability(embeddings) returned error: %v", err)
	}
}

func TestRequireCapabilityRejectsUndeclaredCapability(t *testing.T) {
	m := &profile.Metadata{
		ID:           "qwen3-0.6b-h200-tp1-bf16",
		Capabilities: profile.Capabilities{ChatCompletions: true},
	}

	err := profile.RequireCapability(m, profile.CapabilityLoRA)
	if err == nil {
		t.Fatal("RequireCapability(lora) accepted an undeclared capability, want rejection")
	}
	for _, sub := range []string{"qwen3-0.6b-h200-tp1-bf16", profile.CapabilityLoRA, "does not support"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not name %q", err.Error(), sub)
		}
	}
}

func TestRequireCapabilityRejectsUnknownCapabilityName(t *testing.T) {
	m := &profile.Metadata{ID: "qwen3-0.6b-h200-tp1-bf16"}

	err := profile.RequireCapability(m, "quantumSpeculation")
	if err == nil {
		t.Fatal("RequireCapability(quantumSpeculation) accepted an unknown capability, want rejection")
	}
	if !strings.Contains(err.Error(), "quantumSpeculation") {
		t.Errorf("error %q does not name the unknown capability", err.Error())
	}
}

func TestRequireCapabilityRejectsMissingProfile(t *testing.T) {
	err := profile.RequireCapability(nil, profile.CapabilityChatCompletions)
	if err == nil {
		t.Fatal("RequireCapability(nil, ...) returned nil, want rejection")
	}
	if !strings.Contains(err.Error(), "no profile metadata") {
		t.Errorf("error %q does not explain the missing profile metadata", err.Error())
	}
}

func TestSupportsEachDeclaredCapabilityFlag(t *testing.T) {
	m := &profile.Metadata{
		ID: "all-capabilities",
		Trace: profile.Trace{
			RecordedTokens: true,
		},
		Capabilities: profile.Capabilities{
			ChatCompletions:      true,
			Completions:          true,
			Messages:             true,
			Responses:            true,
			Streaming:            true,
			ToolCallParsers:      []string{"hermes"},
			ReasoningParsers:     []string{"deepseek_r1"},
			KVEventReplay:        true,
			LoRA:                 true,
			SpeculativeDecoding:  true,
			Multimodal:           true,
			Embeddings:           true,
			StructuredOutputs:    true,
			DisaggregatedPrefill: true,
			ContentReplay:        true,
		},
	}

	capabilities := []string{
		profile.CapabilityChatCompletions,
		profile.CapabilityCompletions,
		profile.CapabilityMessages,
		profile.CapabilityResponses,
		profile.CapabilityStreaming,
		profile.CapabilityToolCallParsers,
		profile.CapabilityReasoningParsers,
		profile.CapabilityKVEventReplay,
		profile.CapabilityLoRA,
		profile.CapabilitySpeculativeDecoding,
		profile.CapabilityMultimodal,
		profile.CapabilityEmbeddings,
		profile.CapabilityStructuredOutputs,
		profile.CapabilityDisaggregatedPrefill,
		profile.CapabilityContentReplay,
		"reasoningParser:deepseek_r1",
	}
	for _, capability := range capabilities {
		if !m.Supports(capability) {
			t.Errorf("Supports(%q) = false, want true", capability)
		}
		if err := profile.RequireCapability(m, capability); err != nil {
			t.Errorf("RequireCapability(%q) returned error: %v", capability, err)
		}
	}

	if m.Supports("reasoningParser:other") {
		t.Error("Supports(reasoningParser:other) = true, want false")
	}
}
