// Package profile defines the recorded-profile metadata contract and the
// selection rules the operator uses at admission.
//
// A profile is produced by a separate repository, packaged as an OCI image, and
// consumed here. The image carries its metadata in the image config label
// (names.ProfileMetadataLabel) so that the operator can reconcile compatibility
// without downloading trace layers. The provisioning stage exports the trace
// data itself into a shared Pod volume.
//
// This package is pure: it contains no Kubernetes client calls. Reconciliation
// and admission both depend on it, which keeps selection rules independently
// testable.
//
// The package's rules follow one principle: a profile either fully explains the
// workload or the workload is rejected. Parse rejects documents that cannot be
// completely understood, NewCatalog keeps unusable candidates visible as
// rejected candidates instead of dropping them, Select requires exactly one
// compatible profile, ValidateOverrides treats absent coverage as unsupported,
// and RequireCapability treats absent capabilities as unsupported. Nothing is
// inferred, defaulted, or approximated across a profile boundary.
package profile

// Metadata is the profile document published by the profile repository. The
// schema version is names.ProfileCatalogSchemaVersion.
type Metadata struct {
	SchemaVersion string       `json:"schemaVersion"`
	ID            string       `json:"id"`
	Model         Model        `json:"model"`
	GPU           GPU          `json:"gpu"`
	Execution     Execution    `json:"execution"`
	Cache         Cache        `json:"cache"`
	VLLM          VLLM         `json:"vllm"`
	Coverage      Coverage     `json:"coverage"`
	Trace         Trace        `json:"trace"`
	Provisioning  Provisioning `json:"provisioning"`
	Capabilities  Capabilities `json:"capabilities"`
}

// Model identifies the recorded model and tokenization assets. Selection uses
// the underlying model identity, never a served-model alias.
type Model struct {
	Name              string `json:"name"`
	Revision          string `json:"revision"`
	Tokenizer         string `json:"tokenizer,omitempty"`
	TokenizerRevision string `json:"tokenizerRevision,omitempty"`
	ChatTemplateHash  string `json:"chatTemplateHash,omitempty"`
	// VocabSize is the model's tokenizer vocabulary size. The simulator samples
	// random token ids in [0, vocab_size); the default (32000) is smaller than many
	// modern vocabularies but larger than some, and an out-of-range id makes
	// detokenization fail. 0 means "not declared": the simulator default applies.
	VocabSize int `json:"vocabSize,omitempty"`
}

// GPU identifies the recorded accelerator and the device count the capture used.
type GPU struct {
	Type  string `json:"type"`
	Count int32  `json:"count"`
}

// Execution identifies the recorded execution topology and numeric format.
type Execution struct {
	TensorParallel   int32  `json:"tensorParallel"`
	PipelineParallel int32  `json:"pipelineParallel"`
	DType            string `json:"dtype"`
	Quantization     string `json:"quantization,omitempty"`
}

// Cache captures the cache representation the trace was recorded with.
type Cache struct {
	BlockSize           int32 `json:"blockSize"`
	EnablePrefixCaching bool  `json:"enablePrefixCaching"`
	KVBlocks            int64 `json:"kvBlocks"`
	ChunkedPrefill      bool  `json:"chunkedPrefill"`
}

// VLLM identifies the frontend release and the configuration the profile was
// recorded against.
type VLLM struct {
	Version    string `json:"version"`
	ConfigHash string `json:"configHash,omitempty"`
}

// Coverage declares which workload overrides the profile can faithfully serve.
// A setting absent from coverage is not authorized: absence is not permission.
type Coverage struct {
	MaxModelLen               *Range   `json:"maxModelLen,omitempty"`
	MaxNumSeqs                *Range   `json:"maxNumSeqs,omitempty"`
	MaxNumBatchedTokens       *Range   `json:"maxNumBatchedTokens,omitempty"`
	LongPrefillTokenThreshold *Range   `json:"longPrefillTokenThreshold,omitempty"`
	KVBlocks                  *Range   `json:"kvBlocks,omitempty"`
	SchedulingPolicies        []string `json:"schedulingPolicies,omitempty"`
	// KVMemoryConversion states that this profile carries a validated conversion
	// from GPU memory utilization to simulated KV capacity. Without it, a
	// workload that sets a GPU memory fraction is rejected.
	KVMemoryConversion bool `json:"kvMemoryConversion,omitempty"`
}

// Range is an inclusive supported override interval.
type Range struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

// Trace describes the recorded data shipped in the profile image.
type Trace struct {
	File           string `json:"file"`
	Format         string `json:"format"`
	RecordedTokens bool   `json:"recordedTokens"`
	LatencyModel   bool   `json:"latencyModel"`
	KVEvents       bool   `json:"kvEvents"`
}

// Provisioning describes how the image exports its data into the Pod volume.
type Provisioning struct {
	// Entrypoint is the image command that exports profile data. The operator
	// runs it as an init container with a single argument: the target directory.
	Entrypoint []string `json:"entrypoint"`
	// MetadataFile is the metadata document the entrypoint must write.
	MetadataFile string `json:"metadataFile"`
	// TraceFile is the trace document the entrypoint must write.
	TraceFile string `json:"traceFile"`
}

// Capabilities is the validated serving capability set the serving-behavior
// spike established for this profile. Anything not listed is rejected, because
// an available flag is not evidence of verified end-to-end behavior.
type Capabilities struct {
	ChatCompletions      bool     `json:"chatCompletions"`
	Completions          bool     `json:"completions"`
	Messages             bool     `json:"messages"`
	Responses            bool     `json:"responses"`
	Streaming            bool     `json:"streaming"`
	ToolCallParsers      []string `json:"toolCallParsers,omitempty"`
	ReasoningParsers     []string `json:"reasoningParsers,omitempty"`
	KVEventReplay        bool     `json:"kvEventReplay"`
	Metrics              []string `json:"metrics,omitempty"`
	StructuredOutputs    bool     `json:"structuredOutputs,omitempty"`
	LoRA                 bool     `json:"lora,omitempty"`
	SpeculativeDecoding  bool     `json:"speculativeDecoding,omitempty"`
	Multimodal           bool     `json:"multimodal,omitempty"`
	Embeddings           bool     `json:"embeddings,omitempty"`
	DisaggregatedPrefill bool     `json:"disaggregatedPrefill,omitempty"`

	// ContentReplay states that recorded token IDs can be replayed with
	// validated matching, so content-identical assertions are authorized for
	// this profile. It requires trace.recordedTokens; metadata that claims it
	// without recorded token IDs is rejected.
	ContentReplay bool `json:"contentReplay,omitempty"`
}

// Requirements are the effective workload settings that constrain profile
// selection. A nil optional field means the workload did not constrain it and
// the profile's own recorded value stands. GPUType and GPUCount always come
// from the environment configuration; the workload supplies the requested
// device quantity.
type Requirements struct {
	// ProfileID pins the selection to one recorded profile identity, when the
	// namespace asked for one (a VcrGpuProfile). It narrows the candidate set to that
	// profile and never relaxes the other requirements: a selection an administrator
	// published still has to be able to serve the workload.
	//
	// Empty means the catalog is searched by the workload's own requirements.
	ProfileID string

	Model             string
	ModelRevision     string
	Tokenizer         string
	TokenizerRevision string
	ChatTemplateHash  string

	GPUType          string
	GPUCount         int32
	TensorParallel   int32
	PipelineParallel int32
	DType            string
	Quantization     string

	BlockSize                 int32
	EnablePrefixCaching       *bool
	MaxModelLen               *int64
	MaxNumSeqs                *int64
	MaxNumBatchedTokens       *int64
	LongPrefillTokenThreshold *int64
	KVBlocks                  *int64
	SchedulingPolicy          string
	GPUMemoryUtilization      *float64
}
