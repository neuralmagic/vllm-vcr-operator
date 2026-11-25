package vllm

import (
	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
)

// Defaults of vLLM 0.33.0 encoded by this package.
//
// Provenance: each value was read from the release's own sources or its
// generated CLI reference (https://docs.vllm.ai/en/v0.33.0/cli/serve/):
//
//   - server port 8000, uvicorn log level "info": FrontendArgs in
//     vllm/entrypoints/common/cli_args.py.
//   - gpu memory utilization 0.92: CacheConfig.gpu_memory_utilization.
//   - block size 16: CacheConfig.DEFAULT_BLOCK_SIZE.
//   - prefix caching and chunked prefill enabled: CacheConfig.enable_prefix_caching
//     and SchedulerConfig.enable_chunked_prefill default to True.
//   - scheduling policy "fcfs": SchedulerConfig.policy.
//   - seed 0, max logprobs 20, dtype "auto", logprobs mode "raw_logprobs",
//     tokenizer mode "auto", runner "auto", convert "auto", generation config
//     "auto", config format "auto": vllm/config/model.py and
//     vllm/engine/arg_utils.py; load format "auto": vllm/config/load.py.
//   - kv cache dtype "auto", tensor/pipeline parallel size 1, optimization level
//     2, enforce eager false, shutdown timeout 0, log statistics on, request
//     logging off: vllm/config/{cache,parallel,compilation,observability}.py and
//     the 0.33.0 CLI reference.
//   - engine-ready timeout 600s, worker shutdown timeout 5s, log statistics
//     interval 10s, logging level "INFO", target device "cuda": vllm/envs.py.
//   - max sequences 1024 and max batched tokens 8192: EngineArgs.get_batch_defaults
//     for an OpenAI API server on a >=70GiB device (the H100/H200 class this
//     operator targets). These defaults are device-class dependent in vLLM, so
//     the value encoded here is the one for the targeted GPU class; workloads
//     that set them explicitly are unaffected.
//
// Deliberately not defaulted, because vLLM derives them from the model artifact
// or the device rather than from a constant: --max-model-len (from the model
// config), --dtype/--quantization (from the checkpoint), --data-parallel-size and
// --distributed-executor-backend (from the parallelism topology), and
// --async-scheduling (from model support). A nil pointer therefore means "vLLM
// resolves this from the model or device", never "the workload asked for zero".
const (
	defaultServerPort           = 8000
	defaultMaxNumSeqs           = 1024
	defaultMaxNumBatchedTokens  = 8192
	defaultGPUMemoryUtilization = 0.92
	defaultBlockSize            = 16
	defaultSchedulingPolicy     = "fcfs"
	defaultSeed                 = 0
	defaultMaxLogprobs          = 20
	defaultOptimizationLevel    = 2
	defaultStreamInterval       = 1
	defaultLogStatsIntervalSecs = 10.0
	defaultShutdownTimeoutSecs  = 0
	defaultWorkerShutdownSecs   = 5
	defaultEngineReadyTimeoutS  = 600
)

// newConfig returns the effective configuration of a container that sets
// nothing: every default this package encodes, with origin OriginDefault.
func newConfig() *Config {
	cfg := &Config{
		Version: names.SupportedVLLMVersion,
		origins: make(map[Setting]Origin, 64),
	}
	intPtr := func(v int) *int { return &v }
	boolPtr := func(v bool) *bool { return &v }
	floatPtr := func(v float64) *float64 { return &v }
	str := strPtr

	cfg.Model.TrustRemoteCode = boolPtr(false)
	cfg.Model.Runner = str("auto")
	cfg.Model.Convert = str("auto")
	cfg.Model.ConfigFormat = str("auto")
	cfg.Model.LoadFormat = str("auto")
	cfg.Tokenizer.Mode = str("auto")

	cfg.Server.Port = intPtr(defaultServerPort)
	cfg.Server.UvicornLogLevel = str("info")
	cfg.Server.Headless = boolPtr(false)
	cfg.Server.DisableUvicornAccessLog = boolPtr(false)

	cfg.Generation.Config = str("auto")
	cfg.Generation.Seed = intPtr(defaultSeed)
	cfg.Generation.MaxLogprobs = intPtr(defaultMaxLogprobs)
	cfg.Generation.LogprobsMode = str("raw_logprobs")
	cfg.Generation.UseFP64Gumbel = boolPtr(false)

	cfg.Frontend.StreamInterval = intPtr(defaultStreamInterval)

	cfg.Execution.EnforceEager = boolPtr(false)
	cfg.Execution.OptimizationLevel = intPtr(defaultOptimizationLevel)

	cfg.Parallel.TensorParallelSize = intPtr(1)
	cfg.Parallel.PipelineParallelSize = intPtr(1)
	cfg.Parallel.ExpertParallel = boolPtr(false)

	cfg.Precision.DType = str("auto")
	cfg.Precision.AllowDeprecatedQuantization = boolPtr(false)

	cfg.Memory.GPUMemoryUtilization = floatPtr(defaultGPUMemoryUtilization)

	cfg.Limits.MaxNumSeqs = intPtr(defaultMaxNumSeqs)
	cfg.Limits.MaxNumBatchedTokens = intPtr(defaultMaxNumBatchedTokens)

	cfg.Scheduling.Policy = str(defaultSchedulingPolicy)
	cfg.Scheduling.ChunkedPrefill = boolPtr(true)

	cfg.Cache.BlockSize = intPtr(defaultBlockSize)
	cfg.Cache.EnablePrefixCaching = boolPtr(true)
	cfg.Cache.KVCacheDtype = str("auto")

	cfg.Lifecycle.ShutdownTimeoutSeconds = intPtr(defaultShutdownTimeoutSecs)
	cfg.Lifecycle.WorkerShutdownTimeoutSeconds = intPtr(defaultWorkerShutdownSecs)
	cfg.Lifecycle.EngineReadyTimeoutSeconds = intPtr(defaultEngineReadyTimeoutS)

	cfg.Observability.DisableLogStats = boolPtr(false)
	cfg.Observability.EnableLogRequests = boolPtr(false)
	cfg.Observability.LogStatsIntervalSeconds = floatPtr(defaultLogStatsIntervalSecs)
	cfg.Observability.LogLevel = str("INFO")

	cfg.Device.TargetDevice = str("cuda")

	cfg.LoRA.Enabled = boolPtr(false)
	cfg.LoRA.MaxLoras = intPtr(1)
	cfg.LoRA.MaxLoRARank = intPtr(16)
	cfg.LoRA.FullyShardedLoRAs = boolPtr(false)

	cfg.Multimodal.LanguageModelOnly = boolPtr(false)

	for _, s := range []Setting{
		SettingTrustRemoteCode, SettingRunner, SettingConvert, SettingConfigFormat,
		SettingLoadFormat, SettingTokenizerMode,
		SettingPort, SettingUvicornLogLevel, SettingHeadless, SettingDisableAccessLog,
		SettingGenerationConfig, SettingSeed, SettingMaxLogprobs, SettingLogprobsMode,
		SettingFP64Gumbel, SettingStreamInterval, SettingEnforceEager,
		SettingOptimizationLevel, SettingTensorParallelSize, SettingPipelineParallel,
		SettingExpertParallel, SettingDtype, SettingWantDeprecatedQuant,
		SettingGPUMemUtilization, SettingMaxNumSeqs, SettingMaxNumBatchedTokens,
		SettingSchedulingPolicy, SettingChunkedPrefill, SettingBlockSize,
		SettingPrefixCaching, SettingKVCacheDtype, SettingShutdownTimeout,
		SettingWorkerShutdown, SettingEngineReadyTimeout, SettingDisableLogStats,
		SettingLogRequests, SettingLogStatsInterval, SettingLogLevel,
		SettingTargetDevice, SettingLoRAEnabled, SettingMaxLoras, SettingMaxLoRARank,
		SettingFullyShardedLoRAs, SettingLanguageModelOnly,
	} {
		cfg.origins[s] = OriginDefault
	}
	return cfg
}

// setOrigin records where a setting came from. Defaults keep OriginDefault.
func (c *Config) setOrigin(s Setting, o Origin) {
	c.origins[s] = o
}
