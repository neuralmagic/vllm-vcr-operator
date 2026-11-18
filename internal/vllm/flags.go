package vllm

import (
	"fmt"
	"strconv"
	"strings"
)

// flagKind is the argument shape of a vLLM CLI flag in release 0.33.0.
type flagKind int

const (
	// kindBool is a BooleanOptionalAction or store_true flag: it takes no value.
	kindBool flagKind = iota
	kindString
	kindInt
	kindFloat
	kindStringList
	kindJSON
	// kindCustom is a flag whose value needs package-specific handling
	// (mounted files, structured JSON objects).
	kindCustom
)

// flagSpec describes one modelled vLLM 0.33.0 flag and where its value goes.
//
// Exactly one target pointer is set, according to kind, except for kindCustom
// which uses custom.
type flagSpec struct {
	setting Setting
	kind    flagKind

	bptr **bool
	sptr **string
	iptr **int
	fptr **float64
	lptr *[]string
	jptr *JSONMap
	// custom parses raw and writes the result to the config. It returns a
	// Rejection when the value cannot be resolved.
	custom func(p *parser, raw string) *Rejection
	// jsonObject marks a custom flag whose value is a JSON object, so that vLLM's
	// dotted sub-key argument form applies to it.
	jsonObject bool
}

func boolFlag(s Setting, p **bool) flagSpec { return flagSpec{setting: s, kind: kindBool, bptr: p} }
func stringFlag(s Setting, p **string) flagSpec {
	return flagSpec{setting: s, kind: kindString, sptr: p}
}
func intFlag(s Setting, p **int) flagSpec { return flagSpec{setting: s, kind: kindInt, iptr: p} }
func floatFlag(s Setting, p **float64) flagSpec {
	return flagSpec{setting: s, kind: kindFloat, fptr: p}
}
func stringListFlag(s Setting, p *[]string) flagSpec {
	return flagSpec{setting: s, kind: kindStringList, lptr: p}
}
func jsonFlag(s Setting, p *JSONMap) flagSpec { return flagSpec{setting: s, kind: kindJSON, jptr: p} }
func customFlag(s Setting, fn func(p *parser, raw string) *Rejection) flagSpec {
	return flagSpec{setting: s, kind: kindCustom, custom: fn}
}

// customJSONFlag is a custom flag whose value is a JSON object that vLLM also
// accepts as dotted sub-keys.
func customJSONFlag(s Setting, fn func(p *parser, raw string) *Rejection) flagSpec {
	return flagSpec{setting: s, kind: kindCustom, custom: fn, jsonObject: true}
}

// takesJSONObject reports whether dotted sub-key arguments apply to the flag.
func (s *flagSpec) takesJSONObject() bool {
	return s.kind == kindJSON || s.jsonObject
}

// flagSpellings maps every CLI spelling this package recognises to the setting
// it configures and whether the spelling is a negated boolean form.
type flagSpellings struct {
	setting Setting
	spec    *flagSpec
	// boolValue is the value a value-less boolean spelling implies.
	boolValue bool
}

// flags is the modelled flag table. The canonical spelling of every entry is the
// vLLM 0.33.0 long flag form; negated boolean spellings ("--no-x",
// "--disable-x") are added automatically when vLLM 0.33.0 defines them.
//
// Anything outside this table is rejected by Resolve: either because it is not a
// vLLM 0.33.0 flag at all, or because the operator does not model it and
// therefore cannot apply its settings policy (see knownFlags).
func flags(cfg *Config) []flagSpec {
	return []flagSpec{
		stringFlag(SettingModel, &cfg.Model.Name),
		stringFlag(SettingRevision, &cfg.Model.Revision),
		stringFlag(SettingCodeRevision, &cfg.Model.CodeRevision),
		stringFlag(SettingHFConfigPath, &cfg.Model.HFConfigPath),
		boolFlag(SettingTrustRemoteCode, &cfg.Model.TrustRemoteCode),
		stringFlag(SettingRunner, &cfg.Model.Runner),
		stringFlag(SettingConvert, &cfg.Model.Convert),
		stringFlag(SettingConfigFormat, &cfg.Model.ConfigFormat),
		stringFlag(SettingLoadFormat, &cfg.Model.LoadFormat),
		stringListFlag(SettingServedModelName, &cfg.Model.ServedModelNames),
		jsonFlag(SettingHFOverrides, &cfg.Model.HFOverrides),

		stringFlag(SettingTokenizer, &cfg.Tokenizer.Name),
		stringFlag(SettingTokenizerRevision, &cfg.Tokenizer.Revision),
		stringFlag(SettingTokenizerMode, &cfg.Tokenizer.Mode),
		boolFlag(SettingSkipTokenizerInit, &cfg.Tokenizer.SkipInit),
		customFlag(SettingChatTemplate, resolveChatTemplate),
		jsonFlag(SettingChatTemplateKwargs, &cfg.ChatTemplate.Kwargs),

		stringFlag(SettingHost, &cfg.Server.Host),
		intFlag(SettingPort, &cfg.Server.Port),
		stringFlag(SettingUDS, &cfg.Server.UDS),
		intFlag(SettingGRPCPort, &cfg.Server.GRPCPort),
		boolFlag(SettingHeadless, &cfg.Server.Headless),
		intFlag(SettingAPIServerCount, &cfg.Server.APIServerCount),
		stringFlag(SettingUvicornLogLevel, &cfg.Server.UvicornLogLevel),
		boolFlag(SettingDisableAccessLog, &cfg.Server.DisableUvicornAccessLog),

		customFlag(SettingAPIKey, resolveAPIKey),
		customFlag(SettingSSLKeyFile, func(p *parser, raw string) *Rejection {
			p.cfg.tls().KeyFile = strPtr(raw)
			return nil
		}),
		customFlag(SettingSSLCertFile, func(p *parser, raw string) *Rejection {
			p.cfg.tls().CertFile = strPtr(raw)
			return nil
		}),
		customFlag(SettingSSLCACerts, func(p *parser, raw string) *Rejection {
			p.cfg.tls().CACerts = strPtr(raw)
			return nil
		}),
		customFlag(SettingSSLCertReqs, func(p *parser, raw string) *Rejection {
			v, err := strconv.Atoi(raw)
			if err != nil {
				return &Rejection{Setting: SettingSSLCertReqs, Value: raw, Reason: "not an integer"}
			}
			p.cfg.tls().CertReqs = &v
			return nil
		}),

		boolFlag(SettingAutoToolChoice, &cfg.Frontend.EnableAutoToolChoice),
		stringFlag(SettingToolCallParser, &cfg.Frontend.ToolCallParser),
		stringFlag(SettingToolParserPlugin, &cfg.Frontend.ToolParserPlugin),
		stringFlag(SettingReasoningParser, &cfg.Frontend.ReasoningParser),
		stringFlag(SettingReasoningPlugin, &cfg.Frontend.ReasoningParserPlugin),
		boolFlag(SettingPromptTokensDetails, &cfg.Frontend.EnablePromptTokensDetails),
		boolFlag(SettingTokenIDsInUsage, &cfg.Frontend.ReturnTokensAsTokenIDs),
		intFlag(SettingStreamInterval, &cfg.Frontend.StreamInterval),

		stringFlag(SettingGenerationConfig, &cfg.Generation.Config),
		jsonFlag(SettingGenConfigOverride, &cfg.Generation.Override),
		intFlag(SettingSeed, &cfg.Generation.Seed),
		intFlag(SettingMaxLogprobs, &cfg.Generation.MaxLogprobs),
		stringFlag(SettingLogprobsMode, &cfg.Generation.LogprobsMode),
		boolFlag(SettingFP64Gumbel, &cfg.Generation.UseFP64Gumbel),
		boolFlag(SettingSamplingMask, &cfg.Generation.ReturnSamplingMask),

		boolFlag(SettingEnforceEager, &cfg.Execution.EnforceEager),
		intFlag(SettingOptimizationLevel, &cfg.Execution.OptimizationLevel),
		jsonFlag(SettingCompilationConfig, &cfg.Execution.CompilationConfig),
		intFlag(SettingMaxCudagraphSize, &cfg.Execution.MaxCudagraphCaptureSize),

		intFlag(SettingTensorParallelSize, &cfg.Parallel.TensorParallelSize),
		intFlag(SettingPipelineParallel, &cfg.Parallel.PipelineParallelSize),
		boolFlag(SettingExpertParallel, &cfg.Parallel.ExpertParallel),
		intFlag(SettingDataParallelSize, &cfg.Parallel.DataParallel.Size),
		intFlag(SettingDataParallelLocal, &cfg.Parallel.DataParallel.SizeLocal),
		stringFlag(SettingDataParallelAddress, &cfg.Parallel.DataParallel.Address),
		intFlag(SettingDataParallelRPC, &cfg.Parallel.DataParallel.RPCPort),
		stringFlag(SettingDataParallelBackend, &cfg.Parallel.DataParallel.Backend),
		intFlag(SettingDataParallelStart, &cfg.Parallel.DataParallel.StartRank),
		intFlag(SettingDataParallelRank, &cfg.Parallel.DataParallel.Rank),
		stringFlag(SettingDistributedExecutor, &cfg.Parallel.DistributedExecutorBackend),
		intFlag(SettingNNodes, &cfg.Parallel.MultiNode.NNodes),
		intFlag(SettingNodeRank, &cfg.Parallel.MultiNode.NodeRank),
		stringFlag(SettingMasterAddr, &cfg.Parallel.MultiNode.MasterAddr),
		intFlag(SettingMasterPort, &cfg.Parallel.MultiNode.MasterPort),

		floatFlag(SettingGPUMemUtilization, &cfg.Memory.GPUMemoryUtilization),
		intFlag(SettingKVCacheMemoryBytes, &cfg.Memory.KVCacheMemoryBytes),
		intFlag(SettingNumGPUBlocks, &cfg.Memory.NumGPUBlocksOverride),
		floatFlag(SettingCPUOffloadGB, &cfg.Memory.CPUOffloadGB),

		stringFlag(SettingDtype, &cfg.Precision.DType),
		stringFlag(SettingQuantization, &cfg.Precision.Quantization),
		jsonFlag(SettingQuantizationConfig, &cfg.Precision.QuantizationConfig),
		boolFlag(SettingWantDeprecatedQuant, &cfg.Precision.AllowDeprecatedQuantization),

		customFlag(SettingMaxModelLen, resolveMaxModelLen),
		intFlag(SettingMaxNumSeqs, &cfg.Limits.MaxNumSeqs),
		intFlag(SettingMaxNumBatchedTokens, &cfg.Limits.MaxNumBatchedTokens),
		intFlag(SettingMaxNumActiveSeqs, &cfg.Limits.MaxNumActiveSeqs),

		stringFlag(SettingSchedulingPolicy, &cfg.Scheduling.Policy),
		boolFlag(SettingChunkedPrefill, &cfg.Scheduling.ChunkedPrefill),
		boolFlag(SettingAsyncScheduling, &cfg.Scheduling.AsyncScheduling),
		stringFlag(SettingSchedulerClass, &cfg.Scheduling.SchedulerClass),
		intFlag(SettingLongPrefillTokens, &cfg.Scheduling.LongPrefillTokenThreshold),

		intFlag(SettingBlockSize, &cfg.Cache.BlockSize),
		boolFlag(SettingPrefixCaching, &cfg.Cache.EnablePrefixCaching),
		stringFlag(SettingPrefixCacheHashAlgo, &cfg.Cache.PrefixCachingHashAlgo),
		stringFlag(SettingKVCacheDtype, &cfg.Cache.KVCacheDtype),

		customJSONFlag(SettingKVEventsConfig, resolveKVEventsConfig),
		customJSONFlag(SettingKVTransferConfig, resolveKVTransferConfig),

		intFlag(SettingShutdownTimeout, &cfg.Lifecycle.ShutdownTimeoutSeconds),

		boolFlag(SettingDisableLogStats, &cfg.Observability.DisableLogStats),
		boolFlag(SettingLogRequests, &cfg.Observability.EnableLogRequests),
		stringFlag(SettingLogLevel, &cfg.Observability.LogLevel),
		stringFlag(SettingOtlpTracesEndpoint, &cfg.Observability.OTLPTracesEndpoint),
		stringFlag(SettingCollectDetailed, &cfg.Observability.CollectDetailedTraces),

		stringFlag(SettingDeviceIDs, &cfg.Device.DeviceIDs),
		boolFlag(SettingNUMABind, &cfg.Device.NUMABind),
		boolFlag(SettingSleepMode, &cfg.Device.EnableSleepMode),

		boolFlag(SettingLoRAEnabled, &cfg.LoRA.Enabled),
		intFlag(SettingMaxLoras, &cfg.LoRA.MaxLoras),
		intFlag(SettingMaxLoRARank, &cfg.LoRA.MaxLoRARank),
		stringListFlag(SettingLoRAModules, &cfg.LoRA.Modules),
		boolFlag(SettingFullyShardedLoRAs, &cfg.LoRA.FullyShardedLoRAs),
		intFlag(SettingMaxCPULoras, &cfg.LoRA.MaxCPULoras),
		stringFlag(SettingLoRADtype, &cfg.LoRA.Dtype),

		customJSONFlag(SettingSpeculativeConfig, resolveSpeculativeConfig),

		customJSONFlag(SettingLimitMMPerPrompt, resolveLimitMMPerPrompt),
		stringFlag(SettingLocalMediaPath, &cfg.Multimodal.AllowedLocalMediaPath),
		stringListFlag(SettingMediaDomains, &cfg.Multimodal.AllowedMediaDomains),
		jsonFlag(SettingMMProcessorKwargs, &cfg.Multimodal.MMProcessorKwargs),
		boolFlag(SettingLanguageModelOnly, &cfg.Multimodal.LanguageModelOnly),
		boolFlag(SettingMMEmbeds, &cfg.Multimodal.EnableMMEmbeds),
		boolFlag(SettingInterleaveMM, &cfg.Multimodal.InterleaveMMStrings),

		jsonFlag(SettingPoolerConfig, &cfg.Pooling.PoolerConfig),

		customJSONFlag(SettingStructuredOutputs, resolveStructuredOutputs),
	}
}

// negatedSpellings returns the spellings vLLM 0.33.0 may use to express "false"
// for a boolean setting: "--no-<flag>" and, for "--enable-<x>" flags,
// "--disable-<x>".
func negatedSpellings(setting string) []string {
	base := strings.TrimPrefix(setting, "--")
	out := []string{"--no-" + base}
	if rest, ok := strings.CutPrefix(base, "enable-"); ok {
		out = append(out, "--disable-"+rest)
	}
	return out
}

// shortFlags maps the vLLM 0.33.0 short flag spellings to their long form.
// Short flags are listed only where release 0.33.0 registers them.
var shortFlags = map[string]string{
	"-q":   string(SettingQuantization),
	"-tp":  string(SettingTensorParallelSize),
	"-pp":  string(SettingPipelineParallel),
	"-ep":  string(SettingExpertParallel),
	"-dpl": string(SettingDataParallelLocal),
	"-dpa": string(SettingDataParallelAddress),
	"-dpp": string(SettingDataParallelRPC),
	"-dpn": string(SettingDataParallelRank),
	"-n":   string(SettingNNodes),
	"-r":   string(SettingNodeRank),
	"-sc":  string(SettingSpeculativeConfig),
	"-cc":  string(SettingCompilationConfig),
	"-asc": string(SettingAPIServerCount),
}

// assign converts a raw CLI or config-file value into the setting's Go type.
func (s *flagSpec) assign(p *parser, raw string) *Rejection {
	switch s.kind {
	case kindString:
		*s.sptr = strPtr(raw)
	case kindInt:
		v, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return &Rejection{Setting: s.setting, Value: raw, Reason: "not an integer"}
		}
		*s.iptr = &v
	case kindFloat:
		v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return &Rejection{Setting: s.setting, Value: raw, Reason: "not a number"}
		}
		*s.fptr = &v
	case kindStringList:
		*s.lptr = append(*s.lptr, raw)
	case kindJSON:
		m, err := decodeJSONObject(raw)
		if err != nil {
			return &Rejection{Setting: s.setting, Value: raw, Reason: err.Error()}
		}
		*s.jptr = m
	case kindCustom:
		return s.custom(p, raw)
	case kindBool:
		// Boolean flags take no value; assign is never called for them.
	}
	return nil
}

func strPtr(s string) *string { return &s }

// tls returns the TLS settings, allocating them on first use.
func (c *Config) tls() *TLSSettings {
	if c.TLS == nil {
		c.TLS = &TLSSettings{}
	}
	return c.TLS
}

// validateFlags rejects values that are structurally impossible for the vLLM
// release, independently of any policy decision.
func validateFlags(cfg *Config) Rejections {
	var rs Rejections
	if cfg.Server.Port != nil && (*cfg.Server.Port < 1 || *cfg.Server.Port > 65535) {
		rs = append(rs, Rejection{Setting: SettingPort, Value: fmt.Sprint(*cfg.Server.Port), Reason: "not a valid TCP port"})
	}
	if cfg.Parallel.TensorParallelSize != nil && *cfg.Parallel.TensorParallelSize < 1 {
		rs = append(rs, Rejection{Setting: SettingTensorParallelSize, Value: fmt.Sprint(*cfg.Parallel.TensorParallelSize), Reason: "must be at least 1"})
	}
	if cfg.Parallel.PipelineParallelSize != nil && *cfg.Parallel.PipelineParallelSize < 1 {
		rs = append(rs, Rejection{Setting: SettingPipelineParallel, Value: fmt.Sprint(*cfg.Parallel.PipelineParallelSize), Reason: "must be at least 1"})
	}
	if cfg.Cache.BlockSize != nil && *cfg.Cache.BlockSize < 1 {
		rs = append(rs, Rejection{Setting: SettingBlockSize, Value: fmt.Sprint(*cfg.Cache.BlockSize), Reason: "must be at least 1"})
	}
	if cfg.Limits.MaxNumSeqs != nil && *cfg.Limits.MaxNumSeqs < 1 {
		rs = append(rs, Rejection{Setting: SettingMaxNumSeqs, Value: fmt.Sprint(*cfg.Limits.MaxNumSeqs), Reason: "must be at least 1"})
	}
	if cfg.Limits.MaxNumBatchedTokens != nil && *cfg.Limits.MaxNumBatchedTokens < 1 {
		rs = append(rs, Rejection{Setting: SettingMaxNumBatchedTokens, Value: fmt.Sprint(*cfg.Limits.MaxNumBatchedTokens), Reason: "must be at least 1"})
	}
	if cfg.Memory.GPUMemoryUtilization != nil && (*cfg.Memory.GPUMemoryUtilization <= 0 || *cfg.Memory.GPUMemoryUtilization > 1) {
		rs = append(rs, Rejection{Setting: SettingGPUMemUtilization, Value: fmt.Sprint(*cfg.Memory.GPUMemoryUtilization), Reason: "must be in (0, 1]"})
	}
	if cfg.Scheduling.Policy != nil && *cfg.Scheduling.Policy != "fcfs" && *cfg.Scheduling.Policy != "priority" {
		rs = append(rs, Rejection{Setting: SettingSchedulingPolicy, Value: *cfg.Scheduling.Policy, Reason: `must be "fcfs" or "priority"`})
	}
	return rs
}
