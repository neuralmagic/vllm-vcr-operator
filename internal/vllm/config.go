package vllm

// Setting names one resolvable vLLM setting.
//
// Flag-backed settings use the vLLM 0.33.0 long flag form (for example
// "--max-model-len"); environment-backed settings that have no CLI flag use the
// environment variable name (for example "VLLM_DP_RANK").
type Setting string

// Setting names for the settings this package resolves. They are part of the
// package interface: rejections and provenance use them verbatim.
const (
	SettingModel               Setting = "--model"
	SettingRevision            Setting = "--revision"
	SettingCodeRevision        Setting = "--code-revision"
	SettingHFConfigPath        Setting = "--hf-config-path"
	SettingTrustRemoteCode     Setting = "--trust-remote-code"
	SettingRunner              Setting = "--runner"
	SettingConvert             Setting = "--convert"
	SettingConfigFormat        Setting = "--config-format"
	SettingLoadFormat          Setting = "--load-format"
	SettingServedModelName     Setting = "--served-model-name"
	SettingHFOverrides         Setting = "--hf-overrides"
	SettingTokenizer           Setting = "--tokenizer"
	SettingTokenizerRevision   Setting = "--tokenizer-revision"
	SettingTokenizerMode       Setting = "--tokenizer-mode"
	SettingSkipTokenizerInit   Setting = "--skip-tokenizer-init"
	SettingChatTemplate        Setting = "--chat-template"
	SettingChatTemplateKwargs  Setting = "--default-chat-template-kwargs"
	SettingHost                Setting = "--host"
	SettingPort                Setting = "--port"
	SettingUDS                 Setting = "--uds"
	SettingGRPCPort            Setting = "--grpc-port"
	SettingHeadless            Setting = "--headless"
	SettingAPIServerCount      Setting = "--api-server-count"
	SettingUvicornLogLevel     Setting = "--uvicorn-log-level"
	SettingDisableAccessLog    Setting = "--disable-uvicorn-access-log"
	SettingAPIKey              Setting = "--api-key"
	SettingSSLKeyFile          Setting = "--ssl-keyfile"
	SettingSSLCertFile         Setting = "--ssl-certfile"
	SettingSSLCACerts          Setting = "--ssl-ca-certs"
	SettingSSLCertReqs         Setting = "--ssl-cert-reqs"
	SettingAutoToolChoice      Setting = "--enable-auto-tool-choice"
	SettingToolCallParser      Setting = "--tool-call-parser"
	SettingToolParserPlugin    Setting = "--tool-parser-plugin"
	SettingReasoningParser     Setting = "--reasoning-parser"
	SettingReasoningPlugin     Setting = "--reasoning-parser-plugin"
	SettingPromptTokensDetails Setting = "--enable-prompt-tokens-details"
	SettingTokenIDsInUsage     Setting = "--return-tokens-as-token-ids"
	SettingStreamInterval      Setting = "--stream-interval"
	SettingGenerationConfig    Setting = "--generation-config"
	SettingGenConfigOverride   Setting = "--override-generation-config"
	SettingSeed                Setting = "--seed"
	SettingMaxLogprobs         Setting = "--max-logprobs"
	SettingLogprobsMode        Setting = "--logprobs-mode"
	SettingFP64Gumbel          Setting = "--use-fp64-gumbel"
	SettingSamplingMask        Setting = "--return-sampling-mask"
	SettingEnforceEager        Setting = "--enforce-eager"
	SettingOptimizationLevel   Setting = "--optimization-level"
	SettingCompilationConfig   Setting = "--compilation-config"
	SettingMaxCudagraphSize    Setting = "--max-cudagraph-capture-size"
	SettingTensorParallelSize  Setting = "--tensor-parallel-size"
	SettingPipelineParallel    Setting = "--pipeline-parallel-size"
	SettingExpertParallel      Setting = "--enable-expert-parallel"
	SettingDataParallelSize    Setting = "--data-parallel-size"
	SettingDataParallelLocal   Setting = "--data-parallel-size-local"
	SettingDataParallelAddress Setting = "--data-parallel-address"
	SettingDataParallelRPC     Setting = "--data-parallel-rpc-port"
	SettingDataParallelBackend Setting = "--data-parallel-backend"
	SettingDataParallelStart   Setting = "--data-parallel-start-rank"
	SettingDataParallelRank    Setting = "--data-parallel-rank"
	SettingDPRank              Setting = "VLLM_DP_RANK"
	SettingDPRankLocal         Setting = "VLLM_DP_RANK_LOCAL"
	SettingDPSize              Setting = "VLLM_DP_SIZE"
	SettingDPMasterIP          Setting = "VLLM_DP_MASTER_IP"
	SettingDPMasterPort        Setting = "VLLM_DP_MASTER_PORT"
	SettingDistributedExecutor Setting = "--distributed-executor-backend"
	SettingNNodes              Setting = "--nnodes"
	SettingNodeRank            Setting = "--node-rank"
	SettingMasterAddr          Setting = "--master-addr"
	SettingMasterPort          Setting = "--master-port"
	SettingVLLMRPCTimeout      Setting = "VLLM_RPC_TIMEOUT"
	SettingWorkerMultiproc     Setting = "VLLM_WORKER_MULTIPROC_METHOD"
	SettingGPUMemUtilization   Setting = "--gpu-memory-utilization"
	SettingKVCacheMemoryBytes  Setting = "--kv-cache-memory-bytes"
	SettingNumGPUBlocks        Setting = "--num-gpu-blocks-override"
	SettingCPUOffloadGB        Setting = "--cpu-offload-gb"
	SettingCPUKVCacheSpace     Setting = "VLLM_CPU_KVCACHE_SPACE"
	SettingMaxModelLen         Setting = "--max-model-len"
	SettingMaxNumSeqs          Setting = "--max-num-seqs"
	SettingMaxNumBatchedTokens Setting = "--max-num-batched-tokens"
	SettingMaxNumActiveSeqs    Setting = "--max-num-active-seqs"
	SettingChunkedPrefill      Setting = "--enable-chunked-prefill"
	SettingSchedulingPolicy    Setting = "--scheduling-policy"
	SettingAsyncScheduling     Setting = "--async-scheduling"
	SettingSchedulerClass      Setting = "--scheduler-cls"
	SettingLongPrefillTokens   Setting = "--long-prefill-token-threshold"
	SettingBlockSize           Setting = "--block-size"
	SettingPrefixCaching       Setting = "--enable-prefix-caching"
	SettingPrefixCacheHashAlgo Setting = "--prefix-caching-hash-algo"
	SettingKVCacheDtype        Setting = "--kv-cache-dtype"
	SettingDtype               Setting = "--dtype"
	SettingQuantization        Setting = "--quantization"
	SettingQuantizationConfig  Setting = "--quantization-config"
	SettingWantDeprecatedQuant Setting = "--allow-deprecated-quantization"
	SettingKVCacheLayout       Setting = "VLLM_KV_CACHE_LAYOUT"
	SettingKVEventsConfig      Setting = "--kv-events-config"
	SettingKVTransferConfig    Setting = "--kv-transfer-config"
	SettingShutdownTimeout     Setting = "--shutdown-timeout"
	SettingWorkerShutdown      Setting = "VLLM_WORKER_SHUTDOWN_TIMEOUT_SECONDS"
	SettingEngineReadyTimeout  Setting = "VLLM_ENGINE_READY_TIMEOUT_S"
	SettingDisableLogStats     Setting = "--disable-log-stats"
	SettingLogRequests         Setting = "--enable-log-requests"
	SettingLogStatsInterval    Setting = "VLLM_LOG_STATS_INTERVAL"
	SettingLogLevel            Setting = "--log-level"
	SettingOtlpTracesEndpoint  Setting = "--otlp-traces-endpoint"
	SettingCollectDetailed     Setting = "--collect-detailed-traces"
	SettingDeviceIDs           Setting = "--device-ids"
	SettingTargetDevice        Setting = "VLLM_TARGET_DEVICE"
	SettingNUMABind            Setting = "--numa-bind"
	SettingSleepMode           Setting = "--enable-sleep-mode"
	SettingLoRAEnabled         Setting = "--enable-lora"
	SettingMaxLoras            Setting = "--max-loras"
	SettingMaxLoRARank         Setting = "--max-lora-rank"
	SettingLoRAModules         Setting = "--lora-modules"
	SettingFullyShardedLoRAs   Setting = "--fully-sharded-loras"
	SettingMaxCPULoras         Setting = "--max-cpu-loras"
	SettingLoRADtype           Setting = "--lora-dtype"
	SettingSpeculativeConfig   Setting = "--speculative-config"
	SettingLimitMMPerPrompt    Setting = "--limit-mm-per-prompt"
	SettingLocalMediaPath      Setting = "--allowed-local-media-path"
	SettingMediaDomains        Setting = "--allowed-media-domains"
	SettingMMProcessorKwargs   Setting = "--mm-processor-kwargs"
	SettingLanguageModelOnly   Setting = "--language-model-only"
	SettingMMEmbeds            Setting = "--enable-mm-embeds"
	SettingInterleaveMM        Setting = "--interleave-mm-strings"
	SettingPoolerConfig        Setting = "--pooler-config"
	SettingStructuredOutputs   Setting = "--structured-outputs-config"
	SettingConfigFile          Setting = "--config"
	SettingModelTag            Setting = "model"
	SettingModelScope          Setting = "VLLM_USE_MODELSCOPE"
	SettingModelRedirect       Setting = "VLLM_MODEL_REDIRECT_PATH"
	SettingRustFrontend        Setting = "VLLM_USE_RUST_FRONTEND"
	SettingLongMaxModelLen     Setting = "VLLM_ALLOW_LONG_MAX_MODEL_LEN"
	SettingHFEndpoint          Setting = "HF_ENDPOINT"
	SettingHFCache             Setting = "HF_HUB_CACHE"
	SettingHFHome              Setting = "HF_HOME"
	SettingHFOffline           Setting = "HF_HUB_OFFLINE"
	SettingHFTransformersOffl  Setting = "TRANSFORMERS_OFFLINE"

	// HuggingFace Hub download transport and logging switches. They change how
	// artifacts are fetched or logged, never which tokenizer artifacts are used
	// or which token IDs they produce, so a simulated model server preserves them
	// instead of rejecting the workload.
	SettingHFDisableXET          Setting = "HF_HUB_DISABLE_XET"
	SettingHFEnableHFTransfer    Setting = "HF_HUB_ENABLE_HF_TRANSFER"
	SettingHFDisableProgressBars Setting = "HF_HUB_DISABLE_PROGRESS_BARS"
	SettingHFDisableTelemetry    Setting = "HF_HUB_DISABLE_TELEMETRY"
	SettingHFVerbosity           Setting = "HF_HUB_VERBOSITY"
	SettingHFETagTimeout         Setting = "HF_HUB_ETAG_TIMEOUT"
	SettingHFDownloadTimeout     Setting = "HF_HUB_DOWNLOAD_TIMEOUT"
	SettingHFToken               Setting = "HF_TOKEN"
	SettingVLLMAPIKey            Setting = "VLLM_API_KEY"
)

// Origin records where an effective value came from.
type Origin string

const (
	// OriginDefault means the workload did not set the setting; the value is
	// the vLLM 0.33.0 default.
	OriginDefault Origin = "default"
	// OriginEnv means the value came from an environment variable vLLM reads.
	OriginEnv Origin = "env"
	// OriginConfigFile means the value came from the file named by --config.
	OriginConfigFile Origin = "config-file"
	// OriginCLI means the value came from the container's command or args.
	OriginCLI Origin = "cli"
)

// Config is the effective vLLM configuration of one workload container.
//
// Every field a workload can set is a pointer (or a nil-able map or slice) so
// that "unset" stays distinct from "explicitly set to the default value": the
// settings policy must never treat a resolved default as a workload request.
// Origin reports where each setting came from.
//
// The struct mirrors the settings families the translation policy needs; it is
// not a complete model of vLLM. Recognised vLLM 0.33.0 flags outside these
// families are rejected by Resolve instead of being silently dropped.
type Config struct {
	// Version is the vLLM release whose semantics this resolution targets.
	Version string

	// Entrypoint records how the container starts the vLLM server.
	Entrypoint Entrypoint

	Model             ModelSettings
	Tokenizer         TokenizerSettings
	ChatTemplate      ChatTemplateSettings
	Server            ServerSettings
	Auth              AuthSettings
	TLS               *TLSSettings
	Frontend          FrontendSettings
	Generation        GenerationSettings
	Execution         ExecutionSettings
	Parallel          ParallelSettings
	Precision         PrecisionSettings
	Memory            MemorySettings
	Limits            LimitsSettings
	Scheduling        SchedulingSettings
	Cache             CacheSettings
	KVEvents          *KVEventsConfig
	KVTransfer        *KVTransferConfig
	Lifecycle         LifecycleSettings
	Observability     ObservabilitySettings
	Device            DeviceSettings
	LoRA              LoRASettings
	Speculative       *SpeculativeConfig
	Multimodal        MultimodalSettings
	Pooling           PoolingSettings
	StructuredOutputs *StructuredOutputsConfig
	Environment       EnvironmentSettings

	origins map[Setting]Origin
}

// Origin reports where the effective value of s came from. Settings the
// workload did not set report OriginDefault.
func (c *Config) Origin(s Setting) Origin {
	if c == nil {
		return OriginDefault
	}
	if o, ok := c.origins[s]; ok {
		return o
	}
	return OriginDefault
}

// IsExplicit reports whether the workload set s in its command, args, config
// file, or environment, as opposed to inheriting the vLLM default.
func (c *Config) IsExplicit(s Setting) bool {
	return c.Origin(s) != OriginDefault
}

// Origins returns a copy of the setting-to-origin map, including defaults.
func (c *Config) Origins() map[Setting]Origin {
	out := make(map[Setting]Origin, len(c.origins))
	for k, v := range c.origins {
		out[k] = v
	}
	return out
}

// Entrypoint describes how the container starts vLLM.
type Entrypoint struct {
	// Kind is where the entrypoint came from.
	Kind EntrypointKind
	// Program is the resolved server program, for example "vllm serve" or
	// "python3 -m vllm.entrypoints.openai.api_server".
	Program string
	// Image is the container image the entrypoint was resolved from. It is set
	// only for EntrypointImageDefault.
	Image string
	// TableVersion is the vLLM release the image-entrypoint table entry was
	// validated against. It is set only for EntrypointImageDefault.
	TableVersion string
}

// EntrypointKind says how the vLLM server program was determined.
type EntrypointKind string

const (
	// EntrypointCommand means the container's command and args start vLLM
	// directly.
	EntrypointCommand EntrypointKind = "command"
	// EntrypointPythonModule means the container runs vLLM as a Python module.
	EntrypointPythonModule EntrypointKind = "python-module"
	// EntrypointImageDefault means the container has no command and the image's
	// default entrypoint starts vLLM.
	EntrypointImageDefault EntrypointKind = "image-default"
)

// ModelSettings is model identity and loading.
type ModelSettings struct {
	// Name is the model the server serves ("--model", the "vllm serve"
	// positional argument, or a config file "model" key).
	Name *string
	// ServedModelNames are the API aliases ("--served-model-name").
	ServedModelNames []string
	Revision         *string
	CodeRevision     *string
	HFConfigPath     *string
	TrustRemoteCode  *bool
	// Runner selects the model runner ("auto", "generate", "pooling", ...).
	Runner *string
	// Convert selects a conversion adapter ("auto", "embed", "classify",
	// "none").
	Convert      *string
	ConfigFormat *string
	LoadFormat   *string
	// HFOverrides is the "--hf-overrides" JSON object.
	HFOverrides JSONMap
}

// TokenizerSettings is tokenizer identity.
type TokenizerSettings struct {
	Name     *string
	Revision *string
	Mode     *string
	// SkipInit is "--skip-tokenizer-init": the server does not load a tokenizer.
	SkipInit *bool
}

// ChatTemplateSettings is the resolved chat template and its rendering kwargs.
type ChatTemplateSettings struct {
	// Template is the resolved template text. It is empty when unset.
	Template string
	// Path is the container path the template was read from, empty for an
	// inline template.
	Path string
	// Inline reports that Template came from a literal value rather than a file.
	Inline bool
	// SHA256 is the hex digest of Template, for profile compatibility checks.
	SHA256 string
	// Kwargs is "--default-chat-template-kwargs" JSON.
	Kwargs JSONMap
}

// Set reports whether the workload configured a chat template.
func (c ChatTemplateSettings) Set() bool { return c.SHA256 != "" }

// ServerSettings is the OpenAI-compatible HTTP server configuration.
type ServerSettings struct {
	Host                    *string
	Port                    *int
	UDS                     *string
	GRPCPort                *int
	Headless                *bool
	APIServerCount          *int
	UvicornLogLevel         *string
	DisableUvicornAccessLog *bool
}

// AuthSettings records API authentication without retaining credentials.
type AuthSettings struct {
	// APIKeyRequired reports that the server requires an API key.
	APIKeyRequired bool
	// APIKeySource names where the requirement came from: "--api-key" or an
	// environment variable name. It never contains the key itself.
	APIKeySource string
	// HuggingFaceTokenEnvVar names the environment variable holding a Hub token,
	// when the container sets one. It never contains the token itself.
	HuggingFaceTokenEnvVar string
}

// TLSSettings is the TLS configuration of the HTTP server.
type TLSSettings struct {
	KeyFile  *string
	CertFile *string
	CACerts  *string
	CertReqs *int
}

// FrontendSettings is request-frontend behaviour: parsers and formatting.
type FrontendSettings struct {
	EnableAutoToolChoice      *bool
	ToolCallParser            *string
	ToolParserPlugin          *string
	ReasoningParser           *string
	ReasoningParserPlugin     *string
	EnablePromptTokensDetails *bool
	ReturnTokensAsTokenIDs    *bool
	StreamInterval            *int
}

// GenerationSettings is generation and sampling defaults.
type GenerationSettings struct {
	Config             *string
	Override           JSONMap
	Seed               *int
	MaxLogprobs        *int
	LogprobsMode       *string
	UseFP64Gumbel      *bool
	ReturnSamplingMask *bool
}

// ExecutionSettings is eager execution, compilation and CUDA-graph capture.
type ExecutionSettings struct {
	EnforceEager            *bool
	OptimizationLevel       *int
	CompilationConfig       JSONMap
	MaxCudagraphCaptureSize *int
}

// ParallelSettings is execution topology: tensor, pipeline, data parallel,
// multi-node and distributed-executor configuration.
type ParallelSettings struct {
	TensorParallelSize   *int
	PipelineParallelSize *int
	ExpertParallel       *bool

	// DataParallel is the data-parallel topology. It includes explicit remote
	// engine addresses, which the translation policy rejects until a validated
	// translation exists.
	DataParallel DataParallelSettings

	DistributedExecutorBackend *string
	WorkerMultiprocMethod      *string

	// MultiNode is torch-distributed multi-node configuration.
	MultiNode MultiNodeSettings
}

// DataParallelSettings is the data-parallel topology.
type DataParallelSettings struct {
	Size       *int
	SizeLocal  *int
	StartRank  *int
	Rank       *int
	LocalRank  *int
	Address    *string
	RPCPort    *int
	Backend    *string
	MasterIP   *string
	MasterPort *int
}

// MultiNodeSettings is "--nnodes"/"--node-rank"/"--master-addr"/"--master-port".
type MultiNodeSettings struct {
	NNodes     *int
	NodeRank   *int
	MasterAddr *string
	MasterPort *int
}

// PrecisionSettings is weight precision, quantization and KV-cache dtype
// selection.
type PrecisionSettings struct {
	// DType is "--dtype": "auto" resolves from the checkpoint's config.
	DType *string
	// Quantization is "--quantization" together with the checkpoint's
	// quantization_config. "auto" (unset) means the checkpoint decides.
	Quantization *string
	// QuantizationConfig is the "--quantization-config" JSON object.
	QuantizationConfig JSONMap
	// AllowDeprecatedQuantization is "--allow-deprecated-quantization".
	AllowDeprecatedQuantization *bool
}

// MemorySettings is accelerator memory policy and KV-cache sizing.
type MemorySettings struct {
	GPUMemoryUtilization *float64
	KVCacheMemoryBytes   *int
	NumGPUBlocksOverride *int
	CPUOffloadGB         *float64
	CPUKVCacheSpaceGB    *int
}

// LimitsSettings is sequence, batching and context-length limits.
type LimitsSettings struct {
	MaxModelLen         *int
	MaxNumSeqs          *int
	MaxNumBatchedTokens *int
	MaxNumActiveSeqs    *int
}

// SchedulingSettings is scheduler policy and chunked-prefill behaviour.
type SchedulingSettings struct {
	Policy                    *string
	ChunkedPrefill            *bool
	AsyncScheduling           *bool
	SchedulerClass            *string
	LongPrefillTokenThreshold *int
}

// CacheSettings is KV-cache block and prefix-caching configuration.
type CacheSettings struct {
	BlockSize             *int
	EnablePrefixCaching   *bool
	PrefixCachingHashAlgo *string
	KVCacheDtype          *string
}

// KVEventsConfig is the "--kv-events-config" JSON object.
type KVEventsConfig struct {
	Enable         *bool
	Publisher      *string
	Endpoint       *string
	ReplayEndpoint *string
	Topic          *string
	BufferSteps    *int
	HWM            *int
	MaxQueueSize   *int
	// Raw is the decoded JSON object exactly as the workload supplied it.
	Raw JSONMap
}

// KVTransferConfig is the "--kv-transfer-config" JSON object, including the P/D
// role of the instance.
type KVTransferConfig struct {
	Connector           *string
	Role                *string
	ConnectorModulePath *string
	KVPort              *int
	ExtraConfig         JSONMap
	// Raw is the decoded JSON object exactly as the workload supplied it.
	Raw JSONMap
}

// LifecycleSettings is coordinated startup and shutdown timing.
type LifecycleSettings struct {
	ShutdownTimeoutSeconds       *int
	WorkerShutdownTimeoutSeconds *int
	EngineReadyTimeoutSeconds    *int
}

// ObservabilitySettings is logging and tracing configuration.
type ObservabilitySettings struct {
	DisableLogStats         *bool
	EnableLogRequests       *bool
	LogStatsIntervalSeconds *float64
	LogLevel                *string
	OTLPTracesEndpoint      *string
	CollectDetailedTraces   *string
}

// DeviceSettings is device and accelerator-runtime selection.
type DeviceSettings struct {
	DeviceIDs       *string
	TargetDevice    *string
	NUMABind        *bool
	EnableSleepMode *bool
	KVCacheLayout   *string
}

// LoRASettings is LoRA adapter serving configuration.
type LoRASettings struct {
	Enabled           *bool
	MaxLoras          *int
	MaxLoRARank       *int
	Modules           []string
	FullyShardedLoRAs *bool
	MaxCPULoras       *int
	Dtype             *string
}

// SpeculativeConfig is the "--speculative-config" JSON object.
type SpeculativeConfig struct {
	Method                  *string
	Model                   *string
	NumSpeculativeTokens    *int
	DraftTensorParallelSize *int
	// Raw is the decoded JSON object exactly as the workload supplied it.
	Raw JSONMap
}

// MultimodalSettings is multimodal input-processor configuration.
type MultimodalSettings struct {
	// LimitMMPerPrompt caps how many items of each modality a request may carry.
	LimitMMPerPrompt      map[string]int
	AllowedLocalMediaPath *string
	AllowedMediaDomains   []string
	MMProcessorKwargs     JSONMap
	LanguageModelOnly     *bool
	EnableMMEmbeds        *bool
	InterleaveMMStrings   *bool
}

// PoolingSettings is pooling/embedding output configuration.
type PoolingSettings struct {
	PoolerConfig JSONMap
}

// StructuredOutputsConfig is the "--structured-outputs-config" JSON object.
type StructuredOutputsConfig struct {
	Backend                     *string
	DisableAnyWhitespace        *bool
	DisableAdditionalProperties *bool
	ReasoningParser             *string
	ReasoningParserPlugin       *string
	EnableInReasoning           *bool
	// Raw is the decoded JSON object exactly as the workload supplied it.
	Raw JSONMap
}

// EnvironmentSettings records vLLM and Hub environment variables that change
// configuration, plus the ones this resolver does not model.
type EnvironmentSettings struct {
	UseModelScope        *bool
	ModelRedirectPath    *string
	RustFrontend         *bool
	AllowLongMaxModelLen *bool

	// Hub settings that affect which tokenizer artifacts are loaded.
	HFEndpoint  *string
	HFCachePath *string
	HFHome      *string
	HFOffline   *bool
	// HuggingFaceTokenEnvVar names the variable holding a Hub token, if any.
	HuggingFaceTokenEnvVar string

	// Unmodeled lists VLLM_*/HF_*/TRANSFORMERS_* variables present on the
	// container that this resolver does not model, so the policy layer can
	// reject them instead of ignoring them.
	Unmodeled []string
}

// JSONMap is a JSON object supplied through a vLLM flag or config file. A nil
// map means the setting was not supplied; a non-nil empty map means the workload
// supplied an explicit empty object.
//
// Values decoded into a JSONMap keep encoding/json's shapes: nested objects are
// map[string]any, arrays are []any, and numbers are float64. The object is
// otherwise exactly what the workload supplied, because the policy layer compares
// recorded profile assumptions against it.
type JSONMap map[string]any
