package vllm_test

import (
	"testing"

	"github.com/neuralmagic/vllm-vcr-operator/internal/vllm"
)

// A workload that sets nothing still has an effective configuration: vLLM's
// defaults. They are reported with OriginDefault so the policy layer never
// mistakes them for a workload request. Values come from vLLM 0.33.0 sources;
// see the provenance comment on the defaults in the package.
func TestResolveAppliesVLLMDefaults(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve", "Qwen/Qwen3-8B"})})

	if cfg.Version != "0.33.0" {
		t.Fatalf("version = %q, want 0.33.0", cfg.Version)
	}
	wantInt(t, "port", cfg.Server.Port, 8000)
	wantString(t, "uvicorn log level", cfg.Server.UvicornLogLevel, "info")
	wantBool(t, "headless", cfg.Server.Headless, false)
	wantString(t, "runner", cfg.Model.Runner, "auto")
	wantString(t, "tokenizer mode", cfg.Tokenizer.Mode, "auto")
	wantString(t, "dtype", cfg.Precision.DType, "auto")
	wantFloat(t, "gpu memory utilization", cfg.Memory.GPUMemoryUtilization, 0.92)
	wantInt(t, "max num seqs", cfg.Limits.MaxNumSeqs, 1024)
	wantInt(t, "max num batched tokens", cfg.Limits.MaxNumBatchedTokens, 8192)
	wantString(t, "scheduling policy", cfg.Scheduling.Policy, "fcfs")
	wantBool(t, "chunked prefill", cfg.Scheduling.ChunkedPrefill, true)
	wantInt(t, "block size", cfg.Cache.BlockSize, 16)
	wantBool(t, "prefix caching", cfg.Cache.EnablePrefixCaching, true)
	wantString(t, "kv cache dtype", cfg.Cache.KVCacheDtype, "auto")
	wantInt(t, "tensor parallel size", cfg.Parallel.TensorParallelSize, 1)
	wantInt(t, "pipeline parallel size", cfg.Parallel.PipelineParallelSize, 1)
	wantInt(t, "optimization level", cfg.Execution.OptimizationLevel, 2)
	wantBool(t, "enforce eager", cfg.Execution.EnforceEager, false)
	wantInt(t, "seed", cfg.Generation.Seed, 0)
	wantInt(t, "max logprobs", cfg.Generation.MaxLogprobs, 20)
	wantString(t, "generation config", cfg.Generation.Config, "auto")
	wantInt(t, "stream interval", cfg.Frontend.StreamInterval, 1)
	wantInt(t, "shutdown timeout", cfg.Lifecycle.ShutdownTimeoutSeconds, 0)
	wantInt(t, "worker shutdown timeout", cfg.Lifecycle.WorkerShutdownTimeoutSeconds, 5)
	wantInt(t, "engine ready timeout", cfg.Lifecycle.EngineReadyTimeoutSeconds, 600)
	wantBool(t, "disable log stats", cfg.Observability.DisableLogStats, false)
	wantBool(t, "enable log requests", cfg.Observability.EnableLogRequests, false)
	wantString(t, "log level", cfg.Observability.LogLevel, "INFO")
	wantString(t, "target device", cfg.Device.TargetDevice, "cuda")
	wantBool(t, "lora enabled", cfg.LoRA.Enabled, false)

	for _, setting := range []vllm.Setting{
		vllm.SettingPort, vllm.SettingMaxNumSeqs, vllm.SettingBlockSize,
		vllm.SettingPrefixCaching, vllm.SettingChunkedPrefill,
		vllm.SettingGPUMemUtilization, vllm.SettingSchedulingPolicy,
		vllm.SettingDtype, vllm.SettingTensorParallelSize, vllm.SettingLogLevel,
	} {
		wantOrigin(t, cfg, setting, vllm.OriginDefault)
		if cfg.IsExplicit(setting) {
			t.Fatalf("%s reports as explicitly set for a workload that sets nothing", setting)
		}
	}
}

// Settings vLLM derives from the model artifact or the device stay unset: the
// operator does not read model configuration, so claiming a value would be an
// approximation.
func TestResolveLeavesModelDerivedSettingsUnset(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve", "Qwen/Qwen3-8B"})})

	if cfg.Limits.MaxModelLen != nil {
		t.Fatalf("max-model-len = %d, want unset (vLLM derives it from the model)", *cfg.Limits.MaxModelLen)
	}
	if cfg.Precision.Quantization != nil {
		t.Fatalf("quantization = %q, want unset (vLLM derives it from the checkpoint)", *cfg.Precision.Quantization)
	}
	if cfg.Parallel.DataParallel.Size != nil {
		t.Fatal("data-parallel size is set although the workload does not use data parallelism")
	}
	if cfg.Parallel.DistributedExecutorBackend != nil {
		t.Fatal("distributed executor backend is set although the workload does not configure one")
	}
	if cfg.KVEvents != nil {
		t.Fatal("kv events configuration is set although the workload does not configure it")
	}
	if cfg.KVTransfer != nil {
		t.Fatal("kv transfer configuration is set although the workload does not configure it")
	}
	if cfg.Speculative != nil {
		t.Fatal("speculative configuration is set although the workload does not configure it")
	}
	if cfg.StructuredOutputs != nil {
		t.Fatal("structured outputs configuration is set although the workload does not configure it")
	}
	if cfg.TLS != nil {
		t.Fatal("TLS settings are set although the workload does not configure TLS")
	}
	if cfg.ChatTemplate.Set() {
		t.Fatal("a chat template is reported although the workload does not configure one")
	}
	if cfg.Auth.APIKeyRequired || cfg.Auth.HuggingFaceTokenEnvVar != "" {
		t.Fatalf("authentication is reported for an unauthenticated workload: %+v", cfg.Auth)
	}
}

// Explicitly setting a value equal to the default is still a workload request.
func TestResolveDistinguishesExplicitDefaults(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve", "Qwen/Qwen3-8B"},
		"--max-num-seqs=1024",
		"--gpu-memory-utilization=0.92",
		"--enable-prefix-caching",
		"--enable-log-requests",
	)})

	wantInt(t, "max num seqs", cfg.Limits.MaxNumSeqs, 1024)
	wantOrigin(t, cfg, vllm.SettingMaxNumSeqs, vllm.OriginCLI)
	if !cfg.IsExplicit(vllm.SettingMaxNumSeqs) {
		t.Fatal("an explicitly set --max-num-seqs is reported as a default")
	}
	wantOrigin(t, cfg, vllm.SettingGPUMemUtilization, vllm.OriginCLI)
	wantOrigin(t, cfg, vllm.SettingPrefixCaching, vllm.OriginCLI)
	wantOrigin(t, cfg, vllm.SettingLogRequests, vllm.OriginCLI)

	// A setting the workload did not touch keeps its default origin.
	wantOrigin(t, cfg, vllm.SettingMaxNumBatchedTokens, vllm.OriginDefault)
}

func TestConfigOriginsAreACopy(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve", "Qwen/Qwen3-8B", "--port=9000"})})

	origins := cfg.Origins()
	origins[vllm.SettingPort] = vllm.OriginDefault
	if cfg.Origin(vllm.SettingPort) != vllm.OriginCLI {
		t.Fatal("Origins() exposes the resolver's internal map")
	}
}
