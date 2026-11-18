package vllm

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// environmentSpec describes how one environment variable vLLM 0.33.0 reads
// affects the effective configuration.
type environmentSpec struct {
	// variable is the environment variable name.
	variable string
	// setting is the configuration setting the variable feeds; the resolver
	// records that setting's origin as OriginEnv.
	setting Setting
	// presenceOnly marks a credential: only whether it is set matters, and its
	// value is never read, stored, or reported.
	presenceOnly bool
	apply        func(p *parser, raw string) *Rejection
}

// environmentSpecs is the modelled set of vLLM and Hub environment variables.
//
// Provenance: the variable names and their meaning come from vllm/envs.py at
// v0.33.0. Only variables that change server configuration, topology, device
// selection, lifecycle timing, or tokenizer provenance are modelled; the rest are
// reported as unmodelled so that the settings policy can decide about them
// instead of this package silently ignoring them.
func environmentSpecs() []environmentSpec {
	str := func(target func(p *parser) **string) func(*parser, string) *Rejection {
		return func(p *parser, raw string) *Rejection {
			*target(p) = strPtr(raw)
			return nil
		}
	}
	boolean := func(target func(p *parser) **bool, rawStyle func(string) (bool, error)) func(*parser, string) *Rejection {
		return func(p *parser, raw string) *Rejection {
			v, err := rawStyle(raw)
			if err != nil {
				return &Rejection{Setting: "", Value: raw, Reason: err.Error()}
			}
			*target(p) = &v
			return nil
		}
	}
	integer := func(target func(p *parser) **int) func(*parser, string) *Rejection {
		return func(p *parser, raw string) *Rejection {
			v, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				return &Rejection{Value: raw, Reason: "not an integer"}
			}
			*target(p) = &v
			return nil
		}
	}

	return []environmentSpec{
		// API authentication: the name is recorded, the value is not.
		{variable: "VLLM_API_KEY", setting: SettingVLLMAPIKey, presenceOnly: true, apply: func(p *parser, _ string) *Rejection {
			p.cfg.Auth.APIKeyRequired = true
			p.cfg.Auth.APIKeySource = string(SettingVLLMAPIKey)
			return nil
		}},

		{variable: "VLLM_HOST_IP", setting: SettingHost, apply: str(func(p *parser) **string { return &p.cfg.Server.Host })},
		{variable: "VLLM_PORT", setting: SettingPort, apply: integer(func(p *parser) **int { return &p.cfg.Server.Port })},

		{variable: "VLLM_LOGGING_LEVEL", setting: SettingLogLevel, apply: str(func(p *parser) **string { return &p.cfg.Observability.LogLevel })},
		{variable: "VLLM_LOG_STATS_INTERVAL", setting: SettingLogStatsInterval, apply: func(p *parser, raw string) *Rejection {
			v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil || v <= 0 {
				return &Rejection{Setting: string(SettingLogStatsInterval), Value: raw, Reason: "not a positive number of seconds"}
			}
			p.cfg.Observability.LogStatsIntervalSeconds = &v
			return nil
		}},

		{variable: "VLLM_ENGINE_READY_TIMEOUT_S", setting: SettingEngineReadyTimeout, apply: func(p *parser, raw string) *Rejection {
			return applyEnvInt(p, raw, SettingEngineReadyTimeout, &p.cfg.Lifecycle.EngineReadyTimeoutSeconds)
		}},
		{variable: "VLLM_WORKER_SHUTDOWN_TIMEOUT_SECONDS", setting: SettingWorkerShutdown, apply: func(p *parser, raw string) *Rejection {
			return applyEnvInt(p, raw, SettingWorkerShutdown, &p.cfg.Lifecycle.WorkerShutdownTimeoutSeconds)
		}},

		{variable: "VLLM_TARGET_DEVICE", setting: SettingTargetDevice, apply: str(func(p *parser) **string { return &p.cfg.Device.TargetDevice })},
		{variable: "VLLM_KV_CACHE_LAYOUT", setting: SettingKVCacheLayout, apply: str(func(p *parser) **string { return &p.cfg.Device.KVCacheLayout })},
		{variable: "VLLM_CPU_KVCACHE_SPACE", setting: SettingCPUKVCacheSpace, apply: integer(func(p *parser) **int { return &p.cfg.Memory.CPUKVCacheSpaceGB })},

		{variable: "VLLM_USE_MODELSCOPE", setting: SettingModelScope, apply: boolean(func(p *parser) **bool { return &p.cfg.Environment.UseModelScope }, envBoolVLLM)},
		{variable: "VLLM_MODEL_REDIRECT_PATH", setting: SettingModelRedirect, apply: str(func(p *parser) **string { return &p.cfg.Environment.ModelRedirectPath })},
		{variable: "VLLM_USE_RUST_FRONTEND", setting: SettingRustFrontend, apply: boolean(func(p *parser) **bool { return &p.cfg.Environment.RustFrontend }, envBoolInt)},
		{variable: "VLLM_ALLOW_LONG_MAX_MODEL_LEN", setting: SettingLongMaxModelLen, apply: boolean(func(p *parser) **bool { return &p.cfg.Environment.AllowLongMaxModelLen }, envBoolVLLM)},

		// Data-parallel topology, including the external load-balancer mode where
		// the engine address is remote.
		{variable: "VLLM_DP_SIZE", setting: SettingDPSize, apply: integer(func(p *parser) **int { return &p.cfg.Parallel.DataParallel.Size })},
		{variable: "VLLM_DP_RANK", setting: SettingDPRank, apply: integer(func(p *parser) **int { return &p.cfg.Parallel.DataParallel.Rank })},
		{variable: "VLLM_DP_RANK_LOCAL", setting: SettingDPRankLocal, apply: integer(func(p *parser) **int { return &p.cfg.Parallel.DataParallel.LocalRank })},
		{variable: "VLLM_DP_MASTER_IP", setting: SettingDPMasterIP, apply: str(func(p *parser) **string { return &p.cfg.Parallel.DataParallel.MasterIP })},
		{variable: "VLLM_DP_MASTER_PORT", setting: SettingDPMasterPort, apply: integer(func(p *parser) **int { return &p.cfg.Parallel.DataParallel.MasterPort })},
		{variable: "VLLM_RPC_TIMEOUT", setting: SettingVLLMRPCTimeout, apply: ignoreEnvValue()},
		{variable: "VLLM_WORKER_MULTIPROC_METHOD", setting: SettingWorkerMultiproc, apply: str(func(p *parser) **string { return &p.cfg.Parallel.WorkerMultiprocMethod })},

		// Hub settings that decide which tokenizer artifacts are loaded.
		{variable: "HF_ENDPOINT", setting: SettingHFEndpoint, apply: str(func(p *parser) **string { return &p.cfg.Environment.HFEndpoint })},
		{variable: "HF_HUB_CACHE", setting: SettingHFCache, apply: str(func(p *parser) **string { return &p.cfg.Environment.HFCachePath })},
		{variable: "HF_HOME", setting: SettingHFHome, apply: str(func(p *parser) **string { return &p.cfg.Environment.HFHome })},
		{variable: "HF_HUB_OFFLINE", setting: SettingHFOffline, apply: boolean(func(p *parser) **bool { return &p.cfg.Environment.HFOffline }, envBoolVLLM)},
		{variable: "TRANSFORMERS_OFFLINE", setting: SettingHFTransformersOffl, apply: boolean(func(p *parser) **bool { return &p.cfg.Environment.HFOffline }, envBoolVLLM)},

		// Hub transport and logging switches: recorded so the policy layer can
		// classify them, but they carry no tokenizer or rendering semantics, so
		// there is nothing to apply. The runtime forwards them unchanged.
		{variable: "HF_HUB_DISABLE_XET", setting: SettingHFDisableXET, apply: ignoreEnvValue()},
		{variable: "HF_HUB_ENABLE_HF_TRANSFER", setting: SettingHFEnableHFTransfer, apply: ignoreEnvValue()},
		{variable: "HF_HUB_DISABLE_PROGRESS_BARS", setting: SettingHFDisableProgressBars, apply: ignoreEnvValue()},
		{variable: "HF_HUB_DISABLE_TELEMETRY", setting: SettingHFDisableTelemetry, apply: ignoreEnvValue()},
		{variable: "HF_HUB_VERBOSITY", setting: SettingHFVerbosity, apply: ignoreEnvValue()},
		{variable: "HF_HUB_ETAG_TIMEOUT", setting: SettingHFETagTimeout, apply: ignoreEnvValue()},
		{variable: "HF_HUB_DOWNLOAD_TIMEOUT", setting: SettingHFDownloadTimeout, apply: ignoreEnvValue()},
	}
}

// ignoreEnvValue is the apply step for a variable with no modelled setting
// semantics: it is recorded as present (so it is not rejected as unknown) and
// forwarded unchanged by the runtime.
func ignoreEnvValue() func(*parser, string) *Rejection {
	return func(*parser, string) *Rejection { return nil }
}

// hfTokenEnvVars are the Hub credential spellings the transformers stack reads.
// They are registered uniformly below: the variable name is recorded, the value
// is never read.
var hfTokenEnvVars = []string{"HF_TOKEN", "HUGGING_FACE_HUB_TOKEN", "HUGGINGFACE_HUB_TOKEN"}

// unmodelledEnvPrefixes are the namespaces whose variables can change vLLM or
// tokenizer behaviour.
var unmodelledEnvPrefixes = []string{"VLLM_", "HF_", "TRANSFORMERS_", "HUGGING_FACE_", "HUGGINGFACE_"}

// applyEnvironment applies every container environment variable this package
// models, and records the names of the ones it does not.
func (p *parser) applyEnvironment() {
	p.origin = OriginEnv
	specs := make(map[string]environmentSpec, 32)
	for _, spec := range environmentSpecs() {
		specs[spec.variable] = spec
	}
	for _, name := range hfTokenEnvVars {
		tokenVar := name
		specs[tokenVar] = environmentSpec{
			variable:     tokenVar,
			setting:      SettingHFToken,
			presenceOnly: true,
			apply: func(p *parser, _ string) *Rejection {
				p.cfg.Auth.HuggingFaceTokenEnvVar = tokenVar
				p.cfg.Environment.HuggingFaceTokenEnvVar = tokenVar
				return nil
			},
		}
	}

	var unmodelled []string
	for _, env := range p.inv.Container.Env {
		raw, resolved := p.envValue(env)
		spec, modelled := specs[env.Name]
		if !modelled {
			if hasAnyPrefix(env.Name, unmodelledEnvPrefixes) {
				unmodelled = append(unmodelled, env.Name)
			}
			continue
		}
		if spec.presenceOnly {
			if resolved && raw == "" {
				// An empty credential is not a credential.
				continue
			}
			if rej := spec.apply(p, ""); rej != nil {
				p.errs = append(p.errs, *rej)
				continue
			}
			p.record(spec.setting)
			continue
		}
		if !resolved {
			p.errs = append(p.errs, Rejection{
				Setting: string(Setting(spec.variable)),
				Reason:  "the variable's value comes from a reference the Pod spec does not resolve, so the setting cannot be resolved",
			})
			continue
		}
		if rej := spec.apply(p, raw); rej != nil {
			if rej.Setting == "" {
				rej.Setting = string(Setting(spec.variable))
			}
			p.errs = append(p.errs, *rej)
			continue
		}
		p.record(spec.setting)
	}

	sort.Strings(unmodelled)
	p.cfg.Environment.Unmodeled = unmodelled
}

// envValue returns the resolved value of a container environment entry and
// whether it could be resolved at all.
func (p *parser) envValue(env corev1.EnvVar) (string, bool) {
	if env.ValueFrom == nil {
		return env.Value, true
	}
	value, ok := p.inv.Env[env.Name]
	return value, ok
}

// applyEnvInt parses an integer environment variable.
func applyEnvInt(p *parser, raw string, setting Setting, target **int) *Rejection {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return &Rejection{Setting: string(setting), Value: raw, Reason: "not an integer"}
	}
	*target = &v
	return nil
}

// envBoolVLLM parses the "1"/"true" style booleans vllm/envs.py uses.
func envBoolVLLM(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("not a boolean")
	}
}

// envBoolInt parses the int() booleans vllm/envs.py uses for a few variables.
func envBoolInt(raw string) (bool, error) {
	switch strings.TrimSpace(raw) {
	case "1":
		return true, nil
	case "0":
		return false, nil
	default:
		return false, fmt.Errorf("must be 0 or 1")
	}
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
