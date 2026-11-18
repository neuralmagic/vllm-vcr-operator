package vllm_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/vllm"
)

// offlineReplayFrontend is the Python frontend container from
// deploy/trace-capture/base/offline-replay.yaml in the vllm-vcr repository: the
// llm-d well-lit-path argument set this operator must resolve unchanged.
func offlineReplayFrontend() corev1.Container {
	return corev1.Container{
		Name:    "frontend",
		Image:   "quay.io/wseaton/mock-engine-nixl:vllm-frontend-cpu-c9340e6f3",
		Command: []string{"vllm", "serve"},
		Args: []string{
			"Qwen/Qwen3-Coder-30B-A3B-Instruct",
			"--data-parallel-size=1",
			"--data-parallel-size-local=0",
			"--data-parallel-address=127.0.0.1",
			"--data-parallel-rpc-port=5570",
			"--host=0.0.0.0",
			"--port=8000",
			"--max-model-len=131072",
			"--enable-auto-tool-choice",
			"--tool-call-parser=qwen3_coder",
		},
		Env: []corev1.EnvVar{
			{Name: "HF_HUB_CACHE", Value: "/hf-cache"},
			{Name: "HF_HUB_DISABLE_XET", Value: "1"},
			{Name: "VLLM_RPC_TIMEOUT", Value: "7200000"},
		},
	}
}

func TestResolveLLMDWellLitPath(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: offlineReplayFrontend()})

	wantString(t, "model", cfg.Model.Name, "Qwen/Qwen3-Coder-30B-A3B-Instruct")
	wantString(t, "host", cfg.Server.Host, "0.0.0.0")
	wantInt(t, "port", cfg.Server.Port, 8000)
	wantInt(t, "max-model-len", cfg.Limits.MaxModelLen, 131072)
	wantBool(t, "enable-auto-tool-choice", cfg.Frontend.EnableAutoToolChoice, true)
	wantString(t, "tool-call-parser", cfg.Frontend.ToolCallParser, "qwen3_coder")

	// The frontend addresses a remote engine over vLLM's data-parallel RPC. The
	// resolver reports the topology; the settings policy decides what to do with
	// an explicit remote engine address.
	wantInt(t, "data-parallel-size", cfg.Parallel.DataParallel.Size, 1)
	wantInt(t, "data-parallel-size-local", cfg.Parallel.DataParallel.SizeLocal, 0)
	wantString(t, "data-parallel-address", cfg.Parallel.DataParallel.Address, "127.0.0.1")
	wantInt(t, "data-parallel-rpc-port", cfg.Parallel.DataParallel.RPCPort, 5570)

	for _, setting := range []vllm.Setting{
		vllm.SettingModel,
		vllm.SettingHost,
		vllm.SettingPort,
		vllm.SettingMaxModelLen,
		vllm.SettingAutoToolChoice,
		vllm.SettingToolCallParser,
		vllm.SettingDataParallelSize,
		vllm.SettingDataParallelLocal,
		vllm.SettingDataParallelAddress,
		vllm.SettingDataParallelRPC,
	} {
		wantOrigin(t, cfg, setting, vllm.OriginCLI)
	}

	// The llm-d well-lit-path manifest carries HF_HUB_DISABLE_XET (a Hub download
	// transport switch) and VLLM_RPC_TIMEOUT (the frontend's engine-handshake
	// timeout). Both are modelled as preserved settings: rejecting either would
	// mean a real GPU-targeted manifest could not be submitted unchanged.
	if len(cfg.Environment.Unmodeled) != 0 {
		t.Fatalf("unmodelled environment = %v, want none", cfg.Environment.Unmodeled)
	}
	wantOrigin(t, cfg, vllm.SettingHFDisableXET, vllm.OriginEnv)
	wantOrigin(t, cfg, vllm.SettingVLLMRPCTimeout, vllm.OriginEnv)
	wantString(t, "HF cache path", cfg.Environment.HFCachePath, "/hf-cache")
}

// h200Engine is the real-engine container from deploy/trace-capture/base/
// h200-capture.yaml: GPU sizing and engine flags a capture workload sets.
func TestResolveEngineSizingFlags(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: corev1.Container{
		Name:    "engine",
		Image:   "public.ecr.aws/q9t5s3a7/vllm-ci-postmerge-repo:c9340e6f350a009cf835878abad2a0e379b9e6a4",
		Command: []string{"vllm", "serve"},
		Args: []string{
			"Qwen/Qwen3-8B",
			"--headless",
			"--data-parallel-address=127.0.0.1",
			"--data-parallel-rpc-port=5580",
			"--data-parallel-size=1",
			"--tensor-parallel-size=1",
			"--gpu-memory-utilization=0.90",
			"--max-model-len=16384",
			"--enforce-eager",
			`--speculative-config={"method":"ngram","num_speculative_tokens":4,"prompt_lookup_max":4,"prompt_lookup_min":2}`,
		},
	}})

	wantBool(t, "headless", cfg.Server.Headless, true)
	wantInt(t, "tensor-parallel-size", cfg.Parallel.TensorParallelSize, 1)
	wantFloat(t, "gpu-memory-utilization", cfg.Memory.GPUMemoryUtilization, 0.90)
	wantInt(t, "max-model-len", cfg.Limits.MaxModelLen, 16384)
	wantBool(t, "enforce-eager", cfg.Execution.EnforceEager, true)

	if cfg.Speculative == nil {
		t.Fatal("speculative decoding configuration is unresolved")
	}
	wantString(t, "speculative method", cfg.Speculative.Method, "ngram")
	wantInt(t, "num speculative tokens", cfg.Speculative.NumSpeculativeTokens, 4)
	if got, ok := cfg.Speculative.Raw["prompt_lookup_max"]; !ok || got == nil {
		t.Fatalf("speculative raw object lost prompt_lookup_max: %v", cfg.Speculative.Raw)
	}
}

func TestResolveMultimodalLimits(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: corev1.Container{
		Name:    "engine",
		Image:   "public.ecr.aws/q9t5s3a7/vllm-ci-postmerge-repo:16e91176",
		Command: []string{"vllm", "serve"},
		Args: []string{
			"RedHatAI/gemma-4-26B-A4B-it-FP8-Dynamic",
			"--trust-remote-code",
			"--generation-config=vllm",
			`--limit-mm-per-prompt={"image": 2, "audio": 0, "video": 0}`,
			"--dtype=bfloat16",
			"-q=fp8",
		},
	}})

	wantBool(t, "trust-remote-code", cfg.Model.TrustRemoteCode, true)
	wantString(t, "generation-config", cfg.Generation.Config, "vllm")
	wantString(t, "dtype", cfg.Precision.DType, "bfloat16")
	wantString(t, "quantization", cfg.Precision.Quantization, "fp8")

	limit := cfg.Multimodal.LimitMMPerPrompt
	if limit == nil {
		t.Fatal("--limit-mm-per-prompt was not resolved")
	}
	if got := limit["image"]; got != 2 {
		t.Fatalf("image limit = %v, want 2", got)
	}
	wantOrigin(t, cfg, vllm.SettingLimitMMPerPrompt, vllm.OriginCLI)
}

// requestSampling checks that generation and sampling defaults a workload sets
// are resolved as explicit values, not as inherited defaults.
func TestResolveGenerationAndSchedulingFlags(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve", "Qwen/Qwen3-8B"},
		"--max-num-seqs=512",
		"--max-num-batched-tokens=4096",
		"--block-size=32",
		"--disable-prefix-caching",
		"--scheduling-policy=priority",
		"--disable-chunked-prefill",
		"--shutdown-timeout=30",
		"--kv-cache-dtype=fp8",
		"--num-gpu-blocks-override=1024",
		"--kv-cache-memory-bytes=8589934592",
		"--max-logprobs=5",
		"--seed=42",
	)})

	wantInt(t, "max-num-seqs", cfg.Limits.MaxNumSeqs, 512)
	wantInt(t, "max-num-batched-tokens", cfg.Limits.MaxNumBatchedTokens, 4096)
	wantInt(t, "block-size", cfg.Cache.BlockSize, 32)
	wantBool(t, "prefix caching", cfg.Cache.EnablePrefixCaching, false)
	wantString(t, "scheduling policy", cfg.Scheduling.Policy, "priority")
	wantBool(t, "chunked prefill", cfg.Scheduling.ChunkedPrefill, false)
	wantInt(t, "shutdown timeout", cfg.Lifecycle.ShutdownTimeoutSeconds, 30)
	wantString(t, "kv cache dtype", cfg.Cache.KVCacheDtype, "fp8")
	wantInt(t, "num-gpu-blocks-override", cfg.Memory.NumGPUBlocksOverride, 1024)
	wantInt(t, "kv-cache-memory-bytes", cfg.Memory.KVCacheMemoryBytes, 8589934592)
	wantInt(t, "max-logprobs", cfg.Generation.MaxLogprobs, 5)
	wantInt(t, "seed", cfg.Generation.Seed, 42)

	for _, setting := range []vllm.Setting{
		vllm.SettingMaxNumSeqs, vllm.SettingMaxNumBatchedTokens, vllm.SettingBlockSize,
		vllm.SettingPrefixCaching, vllm.SettingSchedulingPolicy, vllm.SettingChunkedPrefill,
		vllm.SettingShutdownTimeout, vllm.SettingKVCacheDtype, vllm.SettingNumGPUBlocks,
		vllm.SettingKVCacheMemoryBytes, vllm.SettingMaxLogprobs, vllm.SettingSeed,
	} {
		wantOrigin(t, cfg, setting, vllm.OriginCLI)
	}
}
