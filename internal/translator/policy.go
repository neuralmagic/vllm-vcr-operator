package translator

import (
	"fmt"
	"strings"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/profile"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/vllm"
)

// Disposition is the settings policy's decision for one vLLM setting.
//
// The values are the vocabulary of docs/settings-policy.md; every row of the
// policy table declares exactly one, so every setting family has an explicit,
// reviewable treatment rather than an implicit default.
type Disposition string

const (
	// DispositionPreserve keeps the workload's semantics by forwarding the
	// setting to the operator-provided Python frontend, which is the only process
	// that implements it.
	DispositionPreserve Disposition = "preserve"

	// DispositionTranslate reproduces the setting in a different form: it becomes
	// a simulator argument, a profile selection requirement, or is deliberately
	// dropped because the simulated runtime has no equivalent and dropping it
	// changes no observable behavior. A dropped setting carries a Warning.
	DispositionTranslate Disposition = "translate"

	// DispositionRequireProfile accepts the setting only when it matches the
	// recorded profile (structure, precision, cache representation), because a
	// profile either explains the configuration or the configuration is rejected.
	DispositionRequireProfile Disposition = "require-profile"

	// DispositionRequireCapability accepts the setting only when the selected
	// profile's validated capability set grants the capability it selects.
	DispositionRequireCapability Disposition = "require-capability"

	// DispositionReject refuses the setting. When Rule.Check authorizes a specific
	// value (for example "--pipeline-parallel-size 1"), that value is accepted and
	// no rejection is produced.
	DispositionReject Disposition = "reject"
)

// Settings policy families. The names mirror the settings-policy table of the
// specification so that a reviewer can match every row to a policy decision.
const (
	familyModelIdentity     = "Model identity/revision"
	familyTokenizer         = "Tokenizer identity/revision and chat template"
	familyServedAliases     = "Served-model aliases"
	familyServer            = "Host, inference port, API authentication, TLS"
	familyParsers           = "Tool/reasoning parsers and frontend formatting"
	familyGeneration        = "Generation defaults and sampling"
	familyParallelPrecision = "Tensor parallelism, precision, quantization"
	familyDistributed       = "Pipeline parallelism, distributed workers, remote engine addresses"
	familyGPUCount          = "GPU resource count"
	familyMemory            = "GPU memory utilization, KV-memory allocation, cache-block overrides"
	familyMaxModelLen       = "Maximum model length"
	familyMaxNumSeqs        = "Maximum running sequences"
	familyTokenBudget       = "Per-step token budget"
	familyScheduling        = "Scheduling policy"
	familyChunkedPrefill    = "Chunked prefill"
	familyKVBlockSize       = "KV block size"
	familyPrefixCaching     = "Prefix caching"
	familyKVEvents          = "KV live events"
	familyKVReplay          = "KV replay"
	familyShutdown          = "Shutdown timeout"
	familyLogging           = "Logging and telemetry"
	familyDevice            = "GPU device/CUDA runtime selection"
	familyExecutionMode     = "Eager execution, CUDA graphs, compilation"
	familyAdvancedServing   = "LoRA, speculative decoding, multimodal generation, embeddings/pooling, structured-output constraints"
	familyDisaggregated     = "P/D roles and KV-transfer settings"
	familyInvocation        = "Invocation form"
	familyUnknown           = "Unknown behavior-affecting settings"
)

// Rule is one row of the settings policy table (docs/settings-policy.md).
type Rule struct {
	// Setting is the vLLM 0.33.0 setting the row governs.
	Setting vllm.Setting

	// Family is the specification's settings family.
	Family string

	// Disposition is the row's policy decision.
	Disposition Disposition

	// Frontend emits the preserved setting as frontend arguments. It is nil for
	// settings that are not passed on the command line; such a row explains in
	// Reason how the setting is preserved instead.
	Frontend frontendArg

	// RemoveEnv reports that the environment variable backing the setting must
	// not be forwarded to the injected runtime, because it selects accelerator
	// hardware that is not present.
	RemoveEnv bool

	// Capability is the profile capability the row requires when Disposition is
	// RequireCapability. The tool/reasoning parser rows name a specific parser
	// capability, for example "toolCallParser:hermes".
	Capability string

	// Check authorizes values the disposition would otherwise reject. A nil
	// Check with Disposition Reject rejects every value.
	Check func(*vllm.Config) error

	// Reason explains the disposition. For dispositions that reject, it is the
	// rejection reason and must name what is unsupported and why.
	Reason string

	// Warning is recorded in the plan when the setting is accepted but its
	// behavior is not reproduced exactly.
	Warning string
}

// policy is the settings policy table. It is data, not control flow: the
// translator evaluates it row by row in this order, so every setting the
// workload actually set has a disposition and provenance is deterministic.
//
// A setting that does not appear here is rejected as an unknown
// behavior-affecting setting. Adding support therefore requires a reviewed row,
// never a silent fallthrough.
var policy = []Rule{
	// Model identity/revision.
	{Setting: vllm.SettingModel, Family: familyModelIdentity, Disposition: DispositionPreserve,
		Reason: "preserved as the `vllm serve` positional argument of the frontend"},
	{Setting: vllm.SettingModelTag, Family: familyModelIdentity, Disposition: DispositionPreserve,
		Reason: "preserved as the `vllm serve` positional argument of the frontend"},
	{Setting: vllm.SettingRevision, Family: familyModelIdentity, Disposition: DispositionPreserve,
		Frontend: strArg("--revision", func(c *vllm.Config) *string { return c.Model.Revision })},
	{Setting: vllm.SettingCodeRevision, Family: familyModelIdentity, Disposition: DispositionPreserve,
		Frontend: strArg("--code-revision", func(c *vllm.Config) *string { return c.Model.CodeRevision })},
	{Setting: vllm.SettingHFConfigPath, Family: familyModelIdentity, Disposition: DispositionPreserve,
		Frontend: strArg("--hf-config-path", func(c *vllm.Config) *string { return c.Model.HFConfigPath })},
	{Setting: vllm.SettingTrustRemoteCode, Family: familyModelIdentity, Disposition: DispositionPreserve,
		Frontend: boolArg("--trust-remote-code", func(c *vllm.Config) *bool { return c.Model.TrustRemoteCode })},
	{Setting: vllm.SettingRunner, Family: familyModelIdentity, Disposition: DispositionPreserve,
		Frontend: strArg("--runner", func(c *vllm.Config) *string { return c.Model.Runner }),
		Warning:  "the model runner is not executed: outputs come from the recorded profile or the simulator's token model"},
	{Setting: vllm.SettingConvert, Family: familyModelIdentity, Disposition: DispositionPreserve,
		Frontend: strArg("--convert", func(c *vllm.Config) *string { return c.Model.Convert })},
	{Setting: vllm.SettingConfigFormat, Family: familyModelIdentity, Disposition: DispositionPreserve,
		Frontend: strArg("--config-format", func(c *vllm.Config) *string { return c.Model.ConfigFormat })},
	{Setting: vllm.SettingLoadFormat, Family: familyModelIdentity, Disposition: DispositionTranslate,
		Warning: "weights are never loaded by the frontend or the simulator, so the load format has no effect"},
	{Setting: vllm.SettingHFOverrides, Family: familyModelIdentity, Disposition: DispositionPreserve,
		Frontend: jsonArg("--hf-overrides", func(c *vllm.Config) vllm.JSONMap { return c.Model.HFOverrides })},
	{Setting: vllm.SettingModelScope, Family: familyModelIdentity, Disposition: DispositionReject,
		Reason: "ModelScope model hosting requires the modelscope client in the operator-provided frontend image, which does not include it"},
	{Setting: vllm.SettingModelRedirect, Family: familyModelIdentity, Disposition: DispositionPreserve,
		Reason: "preserved as the workload container's environment variable, which the runtime forwards unchanged"},

	// Tokenizer identity/revision and chat template.
	{Setting: vllm.SettingTokenizer, Family: familyTokenizer, Disposition: DispositionPreserve,
		Frontend: strArg("--tokenizer", func(c *vllm.Config) *string { return c.Tokenizer.Name })},
	{Setting: vllm.SettingTokenizerRevision, Family: familyTokenizer, Disposition: DispositionPreserve,
		Frontend: strArg("--tokenizer-revision", func(c *vllm.Config) *string { return c.Tokenizer.Revision })},
	{Setting: vllm.SettingTokenizerMode, Family: familyTokenizer, Disposition: DispositionPreserve,
		Frontend: strArg("--tokenizer-mode", func(c *vllm.Config) *string { return c.Tokenizer.Mode })},
	{Setting: vllm.SettingSkipTokenizerInit, Family: familyTokenizer, Disposition: DispositionReject,
		Reason: "the frontend renders prompts and streams tokens, so it must load the tokenizer: a skipped tokenizer cannot serve requests"},
	{Setting: vllm.SettingChatTemplate, Family: familyTokenizer, Disposition: DispositionPreserve,
		Frontend: chatTemplateArg,
		Reason:   "preserved as --chat-template, either the container path (the mount is preserved) or the inline template"},
	{Setting: vllm.SettingChatTemplateKwargs, Family: familyTokenizer, Disposition: DispositionPreserve,
		Frontend: jsonArg("--default-chat-template-kwargs", func(c *vllm.Config) vllm.JSONMap { return c.ChatTemplate.Kwargs })},
	{Setting: vllm.SettingHFEndpoint, Family: familyTokenizer, Disposition: DispositionPreserve,
		Reason: "preserved as the workload container's environment variable, which the runtime forwards unchanged"},
	{Setting: vllm.SettingHFCache, Family: familyTokenizer, Disposition: DispositionPreserve,
		Reason: "preserved as the workload container's environment variable, which the runtime forwards unchanged"},
	{Setting: vllm.SettingHFHome, Family: familyTokenizer, Disposition: DispositionPreserve,
		Reason: "preserved as the workload container's environment variable, which the runtime forwards unchanged"},
	{Setting: vllm.SettingHFOffline, Family: familyTokenizer, Disposition: DispositionPreserve,
		Reason: "preserved as the workload container's environment variable, which the runtime forwards unchanged"},
	{Setting: vllm.SettingHFTransformersOffl, Family: familyTokenizer, Disposition: DispositionPreserve,
		Reason: "preserved as the workload container's environment variable, which the runtime forwards unchanged"},
	{Setting: vllm.SettingHFDisableXET, Family: familyTokenizer, Disposition: DispositionPreserve,
		Reason: "preserved: a Hub download transport switch, it cannot change the tokenizer identity or the token IDs a prompt renders to"},
	{Setting: vllm.SettingHFEnableHFTransfer, Family: familyTokenizer, Disposition: DispositionPreserve,
		Reason: "preserved: a Hub download transport switch, it cannot change the tokenizer identity or the token IDs a prompt renders to"},
	{Setting: vllm.SettingHFDisableProgressBars, Family: familyTokenizer, Disposition: DispositionPreserve,
		Reason: "preserved: it only affects progress output"},
	{Setting: vllm.SettingHFDisableTelemetry, Family: familyTokenizer, Disposition: DispositionPreserve,
		Reason: "preserved: it only affects telemetry"},
	{Setting: vllm.SettingHFVerbosity, Family: familyTokenizer, Disposition: DispositionPreserve,
		Reason: "preserved: it only affects logging"},
	{Setting: vllm.SettingHFETagTimeout, Family: familyTokenizer, Disposition: DispositionPreserve,
		Reason: "preserved: it only affects Hub metadata request timing"},
	{Setting: vllm.SettingHFDownloadTimeout, Family: familyTokenizer, Disposition: DispositionPreserve,
		Reason: "preserved: it only affects Hub download timing"},
	{Setting: vllm.SettingHFToken, Family: familyTokenizer, Disposition: DispositionPreserve,
		Reason: "preserved as the workload container's environment variable, which the runtime forwards unchanged; its value is never read"},

	{Setting: vllm.SettingVLLMRPCTimeout, Family: familyServer, Disposition: DispositionPreserve,
		Reason: "preserved: it bounds how long the frontend waits for the simulator to register, which profile provisioning can extend; it does not change tokenization or generation semantics"},

	// Served-model aliases.
	{Setting: vllm.SettingServedModelName, Family: familyServedAliases, Disposition: DispositionPreserve,
		Frontend: listArg("--served-model-name", func(c *vllm.Config) []string { return c.Model.ServedModelNames })},

	// Host, inference port, API authentication, TLS.
	{Setting: vllm.SettingHost, Family: familyServer, Disposition: DispositionPreserve,
		Reason: "the frontend builder always emits --host from the effective value, so no duplicate flag is produced"},
	{Setting: vllm.SettingPort, Family: familyServer, Disposition: DispositionPreserve,
		Reason: "the frontend builder always emits --port from the effective value, so no duplicate flag is produced"},
	{Setting: vllm.SettingUDS, Family: familyServer, Disposition: DispositionReject,
		Reason: "a Unix-domain-socket server has no TCP endpoint, so kubelet probes, llm-d discovery, and the supervisor's readiness input cannot reach it; serve on the inference port instead"},
	{Setting: vllm.SettingGRPCPort, Family: familyServer, Disposition: DispositionPreserve,
		Frontend: intArg("--grpc-port", func(c *vllm.Config) *int { return c.Server.GRPCPort })},
	{Setting: vllm.SettingHeadless, Family: familyServer, Disposition: DispositionReject,
		Check:  checkFalse(func(c *vllm.Config) *bool { return c.Server.Headless }),
		Reason: "a headless server exposes no HTTP API, so readiness, probes, and llm-d discovery cannot pass; only a serving workload can be simulated"},
	{Setting: vllm.SettingAPIServerCount, Family: familyServer, Disposition: DispositionPreserve,
		Frontend: intArg("--api-server-count", func(c *vllm.Config) *int { return c.Server.APIServerCount })},
	{Setting: vllm.SettingUvicornLogLevel, Family: familyServer, Disposition: DispositionPreserve,
		Frontend: strArg("--uvicorn-log-level", func(c *vllm.Config) *string { return c.Server.UvicornLogLevel })},
	{Setting: vllm.SettingDisableAccessLog, Family: familyServer, Disposition: DispositionPreserve,
		Frontend: boolArg("--disable-uvicorn-access-log", func(c *vllm.Config) *bool { return c.Server.DisableUvicornAccessLog })},
	{Setting: vllm.SettingAPIKey, Family: familyServer, Disposition: DispositionPreserve,
		Reason: "preserved by copying the workload's own argument verbatim at mutation time, so the key never enters the plan"},
	{Setting: vllm.SettingVLLMAPIKey, Family: familyServer, Disposition: DispositionPreserve,
		Reason: "preserved as the workload container's environment variable, which the runtime forwards unchanged"},
	{Setting: vllm.SettingSSLKeyFile, Family: familyServer, Disposition: DispositionPreserve,
		Frontend: strArg("--ssl-keyfile", func(c *vllm.Config) *string {
			return tlsField(c, func(t *vllm.TLSSettings) *string { return t.KeyFile })
		})},
	{Setting: vllm.SettingSSLCertFile, Family: familyServer, Disposition: DispositionPreserve,
		Frontend: strArg("--ssl-certfile", func(c *vllm.Config) *string {
			return tlsField(c, func(t *vllm.TLSSettings) *string { return t.CertFile })
		})},
	{Setting: vllm.SettingSSLCACerts, Family: familyServer, Disposition: DispositionPreserve,
		Frontend: strArg("--ssl-ca-certs", func(c *vllm.Config) *string {
			return tlsField(c, func(t *vllm.TLSSettings) *string { return t.CACerts })
		})},
	{Setting: vllm.SettingSSLCertReqs, Family: familyServer, Disposition: DispositionPreserve,
		Frontend: intArg("--ssl-cert-reqs", func(c *vllm.Config) *int { return tlsField(c, func(t *vllm.TLSSettings) *int { return t.CertReqs }) })},
	{Setting: vllm.SettingRustFrontend, Family: familyServer, Disposition: DispositionReject,
		Check:  checkFalse(func(c *vllm.Config) *bool { return c.Environment.RustFrontend }),
		Reason: "the workload cannot substitute the operator-provided Python frontend with another implementation"},

	// Tool/reasoning parsers and frontend formatting.
	{Setting: vllm.SettingAutoToolChoice, Family: familyParsers, Disposition: DispositionPreserve,
		Frontend: boolArg("--enable-auto-tool-choice", func(c *vllm.Config) *bool { return c.Frontend.EnableAutoToolChoice })},
	{Setting: vllm.SettingToolCallParser, Family: familyParsers, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityToolCallParsers,
		Frontend:   strArg("--tool-call-parser", func(c *vllm.Config) *string { return c.Frontend.ToolCallParser })},
	{Setting: vllm.SettingToolParserPlugin, Family: familyParsers, Disposition: DispositionReject,
		Reason: "a tool-parser plugin is code that must exist in the operator-provided frontend image; arbitrary plugin modules are not loaded"},
	{Setting: vllm.SettingReasoningParser, Family: familyParsers, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityReasoningParsers,
		Frontend:   strArg("--reasoning-parser", func(c *vllm.Config) *string { return c.Frontend.ReasoningParser })},
	{Setting: vllm.SettingReasoningPlugin, Family: familyParsers, Disposition: DispositionReject,
		Reason: "a reasoning-parser plugin is code that must exist in the operator-provided frontend image; arbitrary plugin modules are not loaded"},
	{Setting: vllm.SettingPromptTokensDetails, Family: familyParsers, Disposition: DispositionPreserve,
		Frontend: boolArg("--enable-prompt-tokens-details", func(c *vllm.Config) *bool { return c.Frontend.EnablePromptTokensDetails })},
	{Setting: vllm.SettingTokenIDsInUsage, Family: familyParsers, Disposition: DispositionPreserve,
		Frontend: boolArg("--return-tokens-as-token-ids", func(c *vllm.Config) *bool { return c.Frontend.ReturnTokensAsTokenIDs })},
	{Setting: vllm.SettingStreamInterval, Family: familyParsers, Disposition: DispositionPreserve,
		Frontend: intArg("--stream-interval", func(c *vllm.Config) *int { return c.Frontend.StreamInterval })},

	// Generation defaults and sampling.
	{Setting: vllm.SettingGenerationConfig, Family: familyGeneration, Disposition: DispositionPreserve,
		Frontend: strArg("--generation-config", func(c *vllm.Config) *string { return c.Generation.Config })},
	{Setting: vllm.SettingGenConfigOverride, Family: familyGeneration, Disposition: DispositionPreserve,
		Frontend: jsonArg("--override-generation-config", func(c *vllm.Config) vllm.JSONMap { return c.Generation.Override })},
	{Setting: vllm.SettingSeed, Family: familyGeneration, Disposition: DispositionTranslate,
		Reason: "translated to the simulator's --seed, which makes random tokens and failure injection reproducible"},
	{Setting: vllm.SettingMaxLogprobs, Family: familyGeneration, Disposition: DispositionPreserve,
		Frontend: intArg("--max-logprobs", func(c *vllm.Config) *int { return c.Generation.MaxLogprobs }),
		Warning:  "the simulator produces no per-token logprobs, so requests asking for them observe empty logprob entries"},
	{Setting: vllm.SettingLogprobsMode, Family: familyGeneration, Disposition: DispositionPreserve,
		Frontend: strArg("--logprobs-mode", func(c *vllm.Config) *string { return c.Generation.LogprobsMode })},
	{Setting: vllm.SettingFP64Gumbel, Family: familyGeneration, Disposition: DispositionTranslate,
		Warning: "sampling is not performed: outputs are recorded token ids or the simulator's seeded token model"},
	{Setting: vllm.SettingSamplingMask, Family: familyGeneration, Disposition: DispositionTranslate,
		Warning: "sampling is not performed, so no sampling mask is produced"},

	// Tensor parallelism, precision, quantization.
	{Setting: vllm.SettingTensorParallelSize, Family: familyParallelPrecision, Disposition: DispositionRequireProfile,
		Reason: "translated to a profile requirement: the recorded topology must match the requested device quantity, and no GPU computation runs"},
	{Setting: vllm.SettingDtype, Family: familyParallelPrecision, Disposition: DispositionRequireProfile,
		Reason: "translated to a profile requirement: the recorded precision must match, and no weights are loaded"},
	{Setting: vllm.SettingQuantization, Family: familyParallelPrecision, Disposition: DispositionRequireProfile,
		Reason: "translated to a profile requirement: the recorded quantization must match, and no quantized kernels run"},
	{Setting: vllm.SettingQuantizationConfig, Family: familyParallelPrecision, Disposition: DispositionReject,
		Reason: "quantization details are not modelled: the profile's recorded quantization is the only supported precision, so an inline quantization configuration is rejected"},
	{Setting: vllm.SettingWantDeprecatedQuant, Family: familyParallelPrecision, Disposition: DispositionReject,
		Check:  checkFalse(func(c *vllm.Config) *bool { return c.Precision.AllowDeprecatedQuantization }),
		Reason: "deprecated quantization schemes require real quantized kernels, which are not executed"},
	{Setting: vllm.SettingExpertParallel, Family: familyParallelPrecision, Disposition: DispositionReject,
		Check:  checkFalse(func(c *vllm.Config) *bool { return c.Parallel.ExpertParallel }),
		Reason: "expert parallelism needs a multi-device MoE engine; the simulated engine is a single-process recorder"},
	{Setting: vllm.SettingKVCacheDtype, Family: familyParallelPrecision, Disposition: DispositionReject,
		Check:  checkAuto(func(c *vllm.Config) *string { return c.Cache.KVCacheDtype }),
		Reason: "the simulated KV cache has no dtype: recorded cache capacity comes from the profile, so a KV-cache dtype cannot be applied"},

	// Pipeline parallelism, distributed workers, remote engine addresses.
	{Setting: vllm.SettingPipelineParallel, Family: familyDistributed, Disposition: DispositionReject,
		Check:  checkIntEquals(func(c *vllm.Config) *int { return c.Parallel.PipelineParallelSize }, 1),
		Reason: "pipeline parallelism greater than one is rejected pending validated translation: the simulated engine models one stage"},
	{Setting: vllm.SettingDistributedExecutor, Family: familyDistributed, Disposition: DispositionReject,
		Check:  checkOneOf(func(c *vllm.Config) *string { return c.Parallel.DistributedExecutorBackend }, "external_launcher"),
		Reason: "only the external-engine mode (external_launcher) is supported: ray, mp and uni worker backends would start a real engine"},
	{Setting: vllm.SettingDataParallelSize, Family: familyDistributed, Disposition: DispositionReject,
		Check:  checkIntEquals(func(c *vllm.Config) *int { return c.Parallel.DataParallel.Size }, 1),
		Reason: "data parallelism greater than one needs several engines; one Pod runs one simulated model server"},
	{Setting: vllm.SettingDataParallelLocal, Family: familyDistributed, Disposition: DispositionReject,
		Check:  checkIntEquals(func(c *vllm.Config) *int { return c.Parallel.DataParallel.SizeLocal }, 0),
		Reason: "a local engine rank is rejected: the operator runs the frontend with no local engine and connects the simulator instead"},
	{Setting: vllm.SettingDataParallelAddress, Family: familyDistributed, Disposition: DispositionReject,
		Reason: "an explicit engine address selects a remote or user-managed engine, which is rejected pending validated translation"},
	{Setting: vllm.SettingDataParallelRPC, Family: familyDistributed, Disposition: DispositionTranslate,
		Reason: "translated to the engine-core handshake port the frontend binds and the simulator connects to"},
	{Setting: vllm.SettingDataParallelBackend, Family: familyDistributed, Disposition: DispositionReject,
		Reason: "the data-parallel backend selects a distributed transport, which the simulated runtime does not implement"},
	{Setting: vllm.SettingDataParallelStart, Family: familyDistributed, Disposition: DispositionReject,
		Reason: "a data-parallel start rank configures a multi-process group, which the simulated runtime does not implement"},
	{Setting: vllm.SettingDataParallelRank, Family: familyDistributed, Disposition: DispositionReject,
		Reason: "a data-parallel rank configures a distributed group, which the simulated runtime does not implement"},
	{Setting: vllm.SettingDPRank, Family: familyDistributed, Disposition: DispositionReject,
		Reason: "a data-parallel rank configures a distributed group, which the simulated runtime does not implement"},
	{Setting: vllm.SettingDPRankLocal, Family: familyDistributed, Disposition: DispositionReject,
		Reason: "a local data-parallel rank starts an in-Pod engine group, which the simulated runtime does not implement"},
	{Setting: vllm.SettingDPSize, Family: familyDistributed, Disposition: DispositionReject,
		Check:  checkIntEquals(func(c *vllm.Config) *int { return c.Parallel.DataParallel.Size }, 1),
		Reason: "a data-parallel size greater than one needs several engines; one Pod runs one simulated model server"},
	{Setting: vllm.SettingDPMasterIP, Family: familyDistributed, Disposition: DispositionReject,
		Reason: "a data-parallel master address selects a remote engine, which is rejected pending validated translation"},
	{Setting: vllm.SettingDPMasterPort, Family: familyDistributed, Disposition: DispositionReject,
		Reason: "a data-parallel master port selects a remote engine, which is rejected pending validated translation"},
	{Setting: vllm.SettingNNodes, Family: familyDistributed, Disposition: DispositionReject,
		Check:  checkIntEquals(func(c *vllm.Config) *int { return c.Parallel.MultiNode.NNodes }, 1),
		Reason: "multi-node execution is rejected: one Pod runs one simulated model server on one node"},
	{Setting: vllm.SettingNodeRank, Family: familyDistributed, Disposition: DispositionReject,
		Reason: "a multi-node rank configures torch-distributed execution, which the simulated runtime does not implement"},
	{Setting: vllm.SettingMasterAddr, Family: familyDistributed, Disposition: DispositionReject,
		Reason: "a torch-distributed master address selects a multi-node group, which the simulated runtime does not implement"},
	{Setting: vllm.SettingMasterPort, Family: familyDistributed, Disposition: DispositionReject,
		Reason: "a torch-distributed master port selects a multi-node group, which the simulated runtime does not implement"},
	{Setting: vllm.SettingWorkerMultiproc, Family: familyDistributed, Disposition: DispositionReject,
		Reason: "a worker multiprocessing method configures real engine worker processes, which are not started"},

	// GPU resource count: the quantity comes from the Pod spec, not from a vLLM
	// setting, so the requirement is recorded by the translator and the policy
	// table has no row for it. See docs/settings-policy.md.
	// GPU memory utilization, KV-memory allocation, cache-block overrides.
	{Setting: vllm.SettingGPUMemUtilization, Family: familyMemory, Disposition: DispositionRequireProfile,
		Reason: "a memory fraction is accepted only when the profile declares coverage.kvMemoryConversion: cache capacity is never inferred from a percentage alone"},
	{Setting: vllm.SettingKVCacheMemoryBytes, Family: familyMemory, Disposition: DispositionRequireProfile,
		Reason: "an explicit KV-cache byte budget is accepted only when the profile declares coverage.kvMemoryConversion, which carries the validated conversion to simulated capacity"},
	{Setting: vllm.SettingNumGPUBlocks, Family: familyMemory, Disposition: DispositionTranslate,
		Reason: "translated to the simulator's --kv-cache-size within the profile's coverage.kvBlocks"},
	{Setting: vllm.SettingCPUOffloadGB, Family: familyMemory, Disposition: DispositionReject,
		Reason: "CPU offload of model weights requires real weight loading and host memory accounting, which the simulated runtime does not perform"},
	{Setting: vllm.SettingCPUKVCacheSpace, Family: familyMemory, Disposition: DispositionReject,
		Reason: "a CPU KV-cache space selects the CPU target device; the simulated KV capacity comes from the profile"},

	// Maximum model length.
	{Setting: vllm.SettingMaxModelLen, Family: familyMaxModelLen, Disposition: DispositionTranslate,
		Reason: "enforced by the frontend and the simulator: a lower cap inside coverage is translated, an increase beyond it is rejected"},
	{Setting: vllm.SettingLongMaxModelLen, Family: familyMaxModelLen, Disposition: DispositionReject,
		Check:  checkFalse(func(c *vllm.Config) *bool { return c.Environment.AllowLongMaxModelLen }),
		Reason: "permitting model lengths beyond the model maximum would bypass the recorded coverage bound, which this policy rejects"},

	// Maximum running sequences.
	{Setting: vllm.SettingMaxNumSeqs, Family: familyMaxNumSeqs, Disposition: DispositionTranslate,
		Reason: "translated to the simulator's sequence admission limit within the profile's coverage.maxNumSeqs"},

	// Per-step token budget.
	{Setting: vllm.SettingMaxNumBatchedTokens, Family: familyTokenBudget, Disposition: DispositionTranslate,
		Reason: "translated to the simulator's per-step token budget within the profile's coverage.maxNumBatchedTokens"},
	{Setting: vllm.SettingMaxNumActiveSeqs, Family: familyTokenBudget, Disposition: DispositionReject,
		Reason: "an active-sequence cap requires asynchronous scheduling, which the simulated engine does not implement"},

	// Scheduling policy.
	{Setting: vllm.SettingSchedulingPolicy, Family: familyScheduling, Disposition: DispositionTranslate,
		Reason: "translated to the simulator's --scheduling-policy: FCFS and priority are supported, other policies are rejected"},
	{Setting: vllm.SettingAsyncScheduling, Family: familyScheduling, Disposition: DispositionReject,
		Check:  checkFalse(func(c *vllm.Config) *bool { return c.Scheduling.AsyncScheduling }),
		Reason: "asynchronous scheduling is not implemented by the simulated engine"},
	{Setting: vllm.SettingSchedulerClass, Family: familyScheduling, Disposition: DispositionReject,
		Reason: "a custom scheduler class is code that must exist in the engine; the simulated engine only implements its own scheduler"},

	// Chunked prefill.
	{Setting: vllm.SettingChunkedPrefill, Family: familyChunkedPrefill, Disposition: DispositionTranslate,
		Reason: "translated to the simulator's chunked-prefill threshold; a workload that changes chunking against the recording is rejected"},
	{Setting: vllm.SettingLongPrefillTokens, Family: familyChunkedPrefill, Disposition: DispositionTranslate,
		Reason: "translated to the simulator's --long-prefill-token-threshold within the profile's coverage"},

	// KV block size.
	{Setting: vllm.SettingBlockSize, Family: familyKVBlockSize, Disposition: DispositionTranslate,
		Reason: "translated to the simulator's --tokens-per-block, which must equal the profile's recorded cache.blockSize"},
	{Setting: vllm.SettingPrefixCacheHashAlgo, Family: familyKVBlockSize, Disposition: DispositionReject,
		Reason: "the prefix-cache hash algorithm fixes the recorded block-hash representation the profile was captured with, so it cannot be selected by the workload"},

	// Prefix caching.
	{Setting: vllm.SettingPrefixCaching, Family: familyPrefixCaching, Disposition: DispositionRequireProfile,
		Reason: "translated to a profile requirement: the recorded cache either has prefix caching enabled or not, and the simulator's cache model is the recording"},
	{Setting: vllm.SettingKVCacheLayout, Family: familyPrefixCaching, Disposition: DispositionReject,
		Reason: "a GPU KV-cache memory layout has no meaning for a simulated cache"},

	// KV live events.
	{Setting: vllm.SettingKVEventsConfig, Family: familyKVEvents, Disposition: DispositionTranslate,
		Reason: "translated to the simulator's KV-event publisher: enablement, endpoint and topic are forwarded and the profile must publish events"},

	// KV replay is a field of --kv-events-config, not a separate setting, so the
	// policy table carries no row: the translator rejects an explicit replay
	// endpoint unless the profile grants capabilities.kvEventReplay
	// (docs/settings-policy.md, "KV replay").

	// Shutdown timeout.
	{Setting: vllm.SettingShutdownTimeout, Family: familyShutdown, Disposition: DispositionTranslate,
		Frontend: strArg("--shutdown-timeout", func(c *vllm.Config) *string { return shutdownTimeoutArg(c) }),
		Reason:   "applied to the simulator's coordinated drain and to the supervisor's graceful shutdown budget"},
	{Setting: vllm.SettingWorkerShutdown, Family: familyShutdown, Disposition: DispositionTranslate,
		Warning: "no local engine workers exist, so the worker shutdown timeout has no effect; the supervisor uses --shutdown-timeout"},
	{Setting: vllm.SettingEngineReadyTimeout, Family: familyShutdown, Disposition: DispositionPreserve,
		Reason: "preserved as the workload container's environment variable: the frontend must wait for the simulator to register"},

	// Logging and telemetry.
	{Setting: vllm.SettingDisableLogStats, Family: familyLogging, Disposition: DispositionPreserve,
		Frontend: boolArg("--disable-log-stats", func(c *vllm.Config) *bool { return c.Observability.DisableLogStats }),
		Warning:  "the simulator emits no Prometheus metrics: vllm:* series come from the frontend only"},
	{Setting: vllm.SettingLogRequests, Family: familyLogging, Disposition: DispositionPreserve,
		Frontend: boolArg("--enable-log-requests", func(c *vllm.Config) *bool { return c.Observability.EnableLogRequests }),
		Reason:   "forwarded to the frontend and translated to the simulator's --log-requests"},
	{Setting: vllm.SettingLogStatsInterval, Family: familyLogging, Disposition: DispositionPreserve,
		Reason: "preserved as the workload container's environment variable, which the runtime forwards unchanged"},
	{Setting: vllm.SettingLogLevel, Family: familyLogging, Disposition: DispositionPreserve,
		Frontend: strArg("--log-level", func(c *vllm.Config) *string { return c.Observability.LogLevel }),
		Reason:   "the frontend log level is preserved; the simulator log filter comes from the environment's logLevel"},
	{Setting: vllm.SettingOtlpTracesEndpoint, Family: familyLogging, Disposition: DispositionPreserve,
		Frontend: strArg("--otlp-traces-endpoint", func(c *vllm.Config) *string { return c.Observability.OTLPTracesEndpoint }),
		Warning:  "only frontend spans are exported: the simulator produces no metrics or traces"},
	{Setting: vllm.SettingCollectDetailed, Family: familyLogging, Disposition: DispositionReject,
		Reason: "detailed engine-core traces require a real engine; the simulator produces no metrics or spans"},

	// GPU device/CUDA runtime selection.
	{Setting: vllm.SettingDeviceIDs, Family: familyDevice, Disposition: DispositionTranslate,
		Reason: "replaced: the operator runs a CPU frontend with an external simulated engine, so no device is selected"},
	{Setting: vllm.SettingTargetDevice, Family: familyDevice, Disposition: DispositionTranslate,
		Check:     checkOneOf(func(c *vllm.Config) *string { return c.Device.TargetDevice }, "cuda", "auto"),
		RemoveEnv: true,
		Reason:    "replaced: the frontend runs on the CPU target device and the engine is simulated, so the accelerator target is not forwarded"},
	{Setting: vllm.SettingNUMABind, Family: familyDevice, Disposition: DispositionReject,
		Reason: "NUMA binding requires the accelerator host topology, which the simulation nodes do not reproduce"},
	{Setting: vllm.SettingSleepMode, Family: familyDevice, Disposition: DispositionReject,
		Reason: "sleep mode is not implemented by the simulated engine, whose ready response reports sleep support disabled"},

	// Eager execution, CUDA graphs, compilation.
	{Setting: vllm.SettingEnforceEager, Family: familyExecutionMode, Disposition: DispositionTranslate,
		Warning: "no CUDA graphs or compilation are executed: recorded timing is used regardless of the execution mode"},
	{Setting: vllm.SettingOptimizationLevel, Family: familyExecutionMode, Disposition: DispositionTranslate,
		Warning: "no compilation is executed: recorded timing is used regardless of the optimization level"},
	{Setting: vllm.SettingCompilationConfig, Family: familyExecutionMode, Disposition: DispositionTranslate,
		Warning: "no compilation is executed: recorded timing is used regardless of the compilation configuration"},
	{Setting: vllm.SettingMaxCudagraphSize, Family: familyExecutionMode, Disposition: DispositionTranslate,
		Warning: "no CUDA graphs are captured: recorded timing is used regardless of the capture size"},

	// LoRA, speculative decoding, multimodal generation, embeddings/pooling,
	// structured-output constraints.
	{Setting: vllm.SettingLoRAEnabled, Family: familyAdvancedServing, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityLoRA,
		Reason:     "LoRA serving is unconditional in the frontend when adapters are configured; the simulator caps adapter diversity instead of loading weights"},
	{Setting: vllm.SettingMaxLoras, Family: familyAdvancedServing, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityLoRA,
		Reason:     "translated to the simulator's --max-loras diversity cap"},
	{Setting: vllm.SettingMaxLoRARank, Family: familyAdvancedServing, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityLoRA,
		Warning:    "adapter weights are not loaded, so the maximum LoRA rank is not enforced"},
	{Setting: vllm.SettingLoRAModules, Family: familyAdvancedServing, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityLoRA,
		Frontend:   listArg("--lora-modules", func(c *vllm.Config) []string { return c.LoRA.Modules }),
		Reason:     "adapter names are served by the frontend when the capability set grants LoRA"},
	{Setting: vllm.SettingFullyShardedLoRAs, Family: familyAdvancedServing, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityLoRA,
		Check:      checkFalse(func(c *vllm.Config) *bool { return c.LoRA.FullyShardedLoRAs }),
		Reason:     "tensor-sharded LoRA needs real tensor parallelism, which is not executed"},
	{Setting: vllm.SettingMaxCPULoras, Family: familyAdvancedServing, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityLoRA,
		Warning:    "adapter weights are not loaded, so the CPU LoRA cache size has no effect"},
	{Setting: vllm.SettingLoRADtype, Family: familyAdvancedServing, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityLoRA,
		Warning:    "adapter weights are not loaded, so the LoRA dtype has no effect"},
	{Setting: vllm.SettingSpeculativeConfig, Family: familyAdvancedServing, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilitySpeculativeDecoding,
		Warning:    "the draft model is not run: speculation is served by recorded timing"},
	{Setting: vllm.SettingLimitMMPerPrompt, Family: familyAdvancedServing, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityMultimodal,
		Frontend:   intMapArg("--limit-mm-per-prompt", func(c *vllm.Config) map[string]int { return c.Multimodal.LimitMMPerPrompt }),
		Reason:     "forwarded to the frontend when the capability set grants multimodal serving"},
	{Setting: vllm.SettingLocalMediaPath, Family: familyAdvancedServing, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityMultimodal,
		Frontend:   strArg("--allowed-local-media-path", func(c *vllm.Config) *string { return c.Multimodal.AllowedLocalMediaPath }),
		Reason:     "forwarded to the frontend when the capability set grants multimodal serving"},
	{Setting: vllm.SettingMediaDomains, Family: familyAdvancedServing, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityMultimodal,
		Frontend:   listArg("--allowed-media-domains", func(c *vllm.Config) []string { return c.Multimodal.AllowedMediaDomains }),
		Reason:     "forwarded to the frontend when the capability set grants multimodal serving"},
	{Setting: vllm.SettingMMProcessorKwargs, Family: familyAdvancedServing, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityMultimodal,
		Frontend:   jsonArg("--mm-processor-kwargs", func(c *vllm.Config) vllm.JSONMap { return c.Multimodal.MMProcessorKwargs }),
		Reason:     "forwarded to the frontend when the capability set grants multimodal serving"},
	{Setting: vllm.SettingLanguageModelOnly, Family: familyAdvancedServing, Disposition: DispositionPreserve,
		Frontend: boolArg("--language-model-only", func(c *vllm.Config) *bool { return c.Multimodal.LanguageModelOnly })},
	{Setting: vllm.SettingMMEmbeds, Family: familyAdvancedServing, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityMultimodal,
		Frontend:   boolArg("--enable-mm-embeds", func(c *vllm.Config) *bool { return c.Multimodal.EnableMMEmbeds }),
		Reason:     "forwarded to the frontend when the capability set grants multimodal serving"},
	{Setting: vllm.SettingInterleaveMM, Family: familyAdvancedServing, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityMultimodal,
		Frontend:   boolArg("--interleave-mm-strings", func(c *vllm.Config) *bool { return c.Multimodal.InterleaveMMStrings }),
		Reason:     "forwarded to the frontend when the capability set grants multimodal serving"},
	{Setting: vllm.SettingPoolerConfig, Family: familyAdvancedServing, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityEmbeddings,
		Frontend:   jsonArg("--pooler-config", func(c *vllm.Config) vllm.JSONMap { return c.Pooling.PoolerConfig }),
		Reason:     "forwarded to the frontend when the capability set grants embeddings or pooling output"},
	{Setting: vllm.SettingStructuredOutputs, Family: familyAdvancedServing, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityStructuredOutputs,
		Frontend:   jsonArg("--structured-outputs-config", structuredOutputsRaw),
		Reason:     "forwarded to the frontend when the capability set grants structured-output constraints"},

	// P/D roles and KV-transfer settings.
	{Setting: vllm.SettingKVTransferConfig, Family: familyDisaggregated, Disposition: DispositionRequireCapability,
		Capability: profile.CapabilityDisaggregatedPrefill,
		Reason:     "translated to the simulator's --pd-role; a connector that is not in the validated capability set is rejected"},

	// Invocation form. The config file's effective values are part of the typed
	// configuration, so the file itself is not forwarded.
	{Setting: vllm.SettingConfigFile, Family: familyInvocation, Disposition: DispositionTranslate,
		Warning: "the config file's effective values are re-emitted as explicit flags; the file itself is not passed to the frontend"},
}

// Policy returns the settings policy table in evaluation order.
//
// The returned slice is a copy of the table's rows, so callers (documentation
// generators and tests) cannot alter the policy the translator applies.
func Policy() []Rule {
	out := make([]Rule, len(policy))
	copy(out, policy)
	return out
}

// init fails fast on a duplicated policy row: two rows for one setting would apply
// two dispositions to the same workload request, so the table must stay a function.
func init() {
	seen := make(map[vllm.Setting]struct{}, len(policy))
	for _, rule := range policy {
		if _, duplicate := seen[rule.Setting]; duplicate {
			panic(fmt.Sprintf("translator: duplicate settings policy row for %q", rule.Setting))
		}
		seen[rule.Setting] = struct{}{}
	}
}

// tlsField reads a TLS setting through the config's optional TLS block.
func tlsField[T any](c *vllm.Config, get func(*vllm.TLSSettings) *T) *T {
	if c.TLS == nil {
		return nil
	}
	return get(c.TLS)
}

// structuredOutputsRaw returns the structured-output configuration exactly as the
// workload supplied it.
func structuredOutputsRaw(c *vllm.Config) vllm.JSONMap {
	if c.StructuredOutputs == nil {
		return nil
	}
	return c.StructuredOutputs.Raw
}

// shutdownTimeoutArg renders the effective shutdown timeout as the flag value
// the frontend parses.
func shutdownTimeoutArg(c *vllm.Config) *string {
	if c.Lifecycle.ShutdownTimeoutSeconds == nil {
		return nil
	}
	value := fmt.Sprintf("%d", *c.Lifecycle.ShutdownTimeoutSeconds)
	return &value
}

// chatTemplateArg renders the resolved chat template: the container path when the
// workload mounted a template file (the mount survives mutation), otherwise the
// inline template text.
func chatTemplateArg(c *vllm.Config) []string {
	if !c.ChatTemplate.Set() {
		return nil
	}
	if c.ChatTemplate.Path != "" && !c.ChatTemplate.Inline {
		return []string{"--chat-template", c.ChatTemplate.Path}
	}
	if c.ChatTemplate.Template == "" {
		return nil
	}
	return []string{"--chat-template", c.ChatTemplate.Template}
}

// checkOneOf authorizes only the listed values; an empty pointer is authorized.
func checkOneOf(get func(*vllm.Config) *string, allowed ...string) func(*vllm.Config) error {
	return func(c *vllm.Config) error {
		value := get(c)
		if value == nil {
			return nil
		}
		for _, candidate := range allowed {
			if *value == candidate {
				return nil
			}
		}
		return fmt.Errorf("value %q is not supported; supported values are [%s]", *value, strings.Join(allowed, ", "))
	}
}

// checkAuto authorizes the "auto" sentinel, which means "the recorded profile
// decides" and is therefore equivalent to leaving the setting unset.
func checkAuto(get func(*vllm.Config) *string) func(*vllm.Config) error {
	return checkOneOf(get, "auto")
}

// checkFalse authorizes only an explicit false: the feature behind the setting is
// unsupported, so asking for it is rejected while turning it off explicitly is
// harmless.
func checkFalse(get func(*vllm.Config) *bool) func(*vllm.Config) error {
	return func(c *vllm.Config) error {
		value := get(c)
		if value == nil || !*value {
			return nil
		}
		return fmt.Errorf("value true is not supported")
	}
}

// checkIntEquals authorizes only the given value.
func checkIntEquals(get func(*vllm.Config) *int, want int) func(*vllm.Config) error {
	return func(c *vllm.Config) error {
		value := get(c)
		if value == nil || *value == want {
			return nil
		}
		return fmt.Errorf("value %d is not supported; the only supported value is %d", *value, want)
	}
}
