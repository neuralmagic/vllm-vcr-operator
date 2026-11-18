package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
)

// TraceFormatJSONL is the only trace format this operator release consumes. A
// JSONL trace is a gzip-compressed or plain newline-delimited JSON document, so
// the declared file must carry a matching extension.
const TraceFormatJSONL = "vllm-vcr/jsonl"

// Parse decodes and validates a profile metadata document.
//
// Validation is strict by contract: a document that cannot be fully understood
// is unusable rather than best-effort parsed, because an approximate
// interpretation would authorize serving against a profile whose recorded
// content is unknown. Errors name the offending contract element and never echo
// the document's values.
//
// Two documented normalizations are applied before validation: an absent
// provisioning.metadataFile defaults to names.ProfileMetadataFileName and an
// absent provisioning.traceFile defaults to trace.file. An explicit value that
// disagrees with trace.file is a contract conflict and is rejected.
func Parse(raw []byte) (*Metadata, error) {
	var m Metadata
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("profile metadata: invalid JSON: %w", err)
	}

	if m.Provisioning.MetadataFile == "" {
		m.Provisioning.MetadataFile = names.ProfileMetadataFileName
	}
	if m.Provisioning.TraceFile == "" {
		m.Provisioning.TraceFile = m.Trace.File
	}

	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("profile metadata: %w", err)
	}
	return &m, nil
}

// validate enforces the profile image contract (docs/contracts/profile-image.md).
// It is shared by Parse and catalog construction so that a hand-built Metadata
// cannot bypass the contract through the catalog.
func (m *Metadata) validate() error {
	if m.SchemaVersion == "" {
		return fmt.Errorf("schemaVersion is required (supported: %q)", names.ProfileCatalogSchemaVersion)
	}
	if m.SchemaVersion != names.ProfileCatalogSchemaVersion {
		return fmt.Errorf("schemaVersion %q is not supported (supported: %q)",
			m.SchemaVersion, names.ProfileCatalogSchemaVersion)
	}

	if strings.TrimSpace(m.ID) == "" {
		return errors.New("id is required")
	}

	if strings.TrimSpace(m.Model.Name) == "" {
		return errors.New("model.name is required")
	}
	if strings.TrimSpace(m.Model.Revision) == "" {
		return errors.New("model.revision is required")
	}

	if strings.TrimSpace(m.GPU.Type) == "" {
		return errors.New("gpu.type is required: the simulated accelerator cannot be inferred from device count")
	}
	if m.GPU.Count <= 0 {
		return fmt.Errorf("gpu.count must be greater than 0, got %d", m.GPU.Count)
	}

	if m.Execution.TensorParallel <= 0 {
		return fmt.Errorf("execution.tensorParallel must be greater than 0, got %d", m.Execution.TensorParallel)
	}
	if m.Execution.TensorParallel != m.GPU.Count {
		return fmt.Errorf("execution.tensorParallel %d is inconsistent with gpu.count %d: the recorded topology must match the device count it implies",
			m.Execution.TensorParallel, m.GPU.Count)
	}
	if m.Execution.PipelineParallel != 1 {
		return fmt.Errorf("execution.pipelineParallel must be 1, got %d: pipeline-parallel recording is not supported",
			m.Execution.PipelineParallel)
	}
	if strings.TrimSpace(m.Execution.DType) == "" {
		return errors.New("execution.dtype is required")
	}

	if m.Cache.BlockSize <= 0 {
		return fmt.Errorf("cache.blockSize must be greater than 0, got %d", m.Cache.BlockSize)
	}

	if strings.TrimSpace(m.VLLM.Version) == "" {
		return errors.New("vllm.version is required: profile compatibility cannot be verified without it")
	}

	if strings.TrimSpace(m.Trace.Format) == "" {
		return fmt.Errorf("trace.format is required (supported: %q)", TraceFormatJSONL)
	}
	if m.Trace.Format != TraceFormatJSONL {
		return fmt.Errorf("trace.format %q is not supported (supported: %q)", m.Trace.Format, TraceFormatJSONL)
	}
	if strings.TrimSpace(m.Trace.File) == "" {
		return fmt.Errorf("trace.file is required for trace.format %q", m.Trace.Format)
	}
	if !traceFileMatchesFormat(m.Trace.Format, m.Trace.File) {
		return fmt.Errorf("trace.file %q does not match trace.format %q: expected a .jsonl or .jsonl.gz file",
			m.Trace.File, m.Trace.Format)
	}

	if len(m.Provisioning.Entrypoint) == 0 {
		return errors.New("provisioning.entrypoint is required: profile data cannot be exported without it")
	}
	for i, arg := range m.Provisioning.Entrypoint {
		if strings.TrimSpace(arg) == "" {
			return fmt.Errorf("provisioning.entrypoint[%d] must not be empty", i)
		}
	}
	if m.Provisioning.TraceFile != m.Trace.File {
		return fmt.Errorf("provisioning.traceFile %q conflicts with trace.file %q",
			m.Provisioning.TraceFile, m.Trace.File)
	}
	if strings.TrimSpace(m.Provisioning.MetadataFile) == "" {
		return errors.New("provisioning.metadataFile is required")
	}

	if err := m.Coverage.validate(); err != nil {
		return err
	}

	if m.Capabilities.ContentReplay && !m.Trace.RecordedTokens {
		return errors.New("capabilities.contentReplay is true but trace.recordedTokens is false: content-identical replay requires recorded token IDs")
	}

	return nil
}

func traceFileMatchesFormat(format, file string) bool {
	switch format {
	case TraceFormatJSONL:
		return strings.HasSuffix(file, ".jsonl") || strings.HasSuffix(file, ".jsonl.gz")
	default:
		return false
	}
}

// validate rejects coverage declarations that could authorize an override the
// profile cannot represent. An absent range is valid: absence means the setting
// is not authorized, which is stricter than a range.
func (c *Coverage) validate() error {
	positive := []struct {
		setting string
		r       *Range
	}{
		{"coverage.maxModelLen", c.MaxModelLen},
		{"coverage.maxNumSeqs", c.MaxNumSeqs},
		{"coverage.maxNumBatchedTokens", c.MaxNumBatchedTokens},
		{"coverage.kvBlocks", c.KVBlocks},
	}
	for _, entry := range positive {
		if err := validateRange(entry.setting, entry.r, 1); err != nil {
			return err
		}
	}
	if err := validateRange("coverage.longPrefillTokenThreshold", c.LongPrefillTokenThreshold, 0); err != nil {
		return err
	}

	for i, policy := range c.SchedulingPolicies {
		if strings.TrimSpace(policy) == "" {
			return fmt.Errorf("coverage.schedulingPolicies[%d] must not be empty", i)
		}
	}
	return nil
}

func validateRange(setting string, r *Range, minAllowed int64) error {
	if r == nil {
		return nil
	}
	if r.Min < minAllowed {
		if minAllowed == 0 {
			return fmt.Errorf("%s {min: %d, max: %d}: min must be non-negative", setting, r.Min, r.Max)
		}
		return fmt.Errorf("%s {min: %d, max: %d}: min must be greater than 0", setting, r.Min, r.Max)
	}
	if r.Max <= 0 {
		return fmt.Errorf("%s {min: %d, max: %d}: max must be greater than 0", setting, r.Min, r.Max)
	}
	if r.Min > r.Max {
		return fmt.Errorf("%s {min: %d, max: %d}: min must not exceed max", setting, r.Min, r.Max)
	}
	return nil
}

// Supports reports whether the profile's validated serving capability set grants
// capability. A flag accepted by the frontend is not evidence of support; only
// entries in the metadata authorize behavior.
//
// Capability names are the constants in this package (CapabilityChatCompletions
// and so on). Parser capabilities additionally accept a qualified form —
// "toolCallParser:<name>" and "reasoningParser:<name>" — to check a specific
// parser. Unknown capability names report false.
func (m *Metadata) Supports(capability string) bool {
	switch capability {
	case CapabilityChatCompletions:
		return m.Capabilities.ChatCompletions
	case CapabilityCompletions:
		return m.Capabilities.Completions
	case CapabilityMessages:
		return m.Capabilities.Messages
	case CapabilityResponses:
		return m.Capabilities.Responses
	case CapabilityStreaming:
		return m.Capabilities.Streaming
	case CapabilityToolCallParsers:
		return len(m.Capabilities.ToolCallParsers) > 0
	case CapabilityReasoningParsers:
		return len(m.Capabilities.ReasoningParsers) > 0
	case CapabilityKVEventReplay:
		return m.Capabilities.KVEventReplay
	case CapabilityLoRA:
		return m.Capabilities.LoRA
	case CapabilitySpeculativeDecoding:
		return m.Capabilities.SpeculativeDecoding
	case CapabilityMultimodal:
		return m.Capabilities.Multimodal
	case CapabilityEmbeddings:
		return m.Capabilities.Embeddings
	case CapabilityStructuredOutputs:
		return m.Capabilities.StructuredOutputs
	case CapabilityDisaggregatedPrefill:
		return m.Capabilities.DisaggregatedPrefill
	case CapabilityContentReplay:
		// Recorded token IDs are what make content-identical assertions
		// possible, so a profile cannot grant replay without them.
		return m.Capabilities.ContentReplay && m.Trace.RecordedTokens
	default:
		if name, ok := strings.CutPrefix(capability, toolCallParserPrefix); ok {
			return containsString(m.Capabilities.ToolCallParsers, name)
		}
		if name, ok := strings.CutPrefix(capability, reasoningParserPrefix); ok {
			return containsString(m.Capabilities.ReasoningParsers, name)
		}
		return false
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
