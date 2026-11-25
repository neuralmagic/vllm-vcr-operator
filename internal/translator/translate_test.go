package translator_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
	"github.com/neuralmagic/vllm-vcr-operator/internal/plan"
	"github.com/neuralmagic/vllm-vcr-operator/internal/profile"
	"github.com/neuralmagic/vllm-vcr-operator/internal/translator"
)

// TestSettingsPolicyFamilies drives one accepted and one rejected workload per
// settings family of the specification's settings-policy table. Every rejection
// must name the offending setting.
func TestSettingsPolicyFamilies(t *testing.T) {
	allowLoRA := func(m *profile.Metadata) { m.Capabilities.LoRA = true }
	allowSpeculative := func(m *profile.Metadata) { m.Capabilities.SpeculativeDecoding = true }
	allowDisaggregated := func(m *profile.Metadata) { m.Capabilities.DisaggregatedPrefill = true }
	allowEmbeddings := func(m *profile.Metadata) { m.Capabilities.Embeddings = true }
	allowStructured := func(m *profile.Metadata) { m.Capabilities.StructuredOutputs = true }
	allowToolParser := func(m *profile.Metadata) { m.Capabilities.ToolCallParsers = []string{"hermes"} }
	allowKVReplay := func(m *profile.Metadata) { m.Capabilities.KVEventReplay = true }
	noKVEvents := func(m *profile.Metadata) { m.Trace.KVEvents = false }
	allowKVMemoryConversion := func(m *profile.Metadata) { m.Coverage.KVMemoryConversion = true }
	withoutSchedulingCoverage := func(m *profile.Metadata) { m.Coverage.SchedulingPolicies = nil }

	tests := []translateCase{
		// Model identity/revision.
		{name: "model revision is preserved", args: []string{"--revision", "c9340e6f3"},
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "frontend", a.Frontend.Args, "--revision", "c9340e6f3")
			}},
		{name: "unknown model revision is rejected", args: []string{"--revision", "deadbeef"},
			wantSetting: "profile", wantReason: "c9340e6f3"},

		// Tokenizer identity/revision and chat template.
		{name: "tokenizer is preserved", args: []string{"--tokenizer", testModel},
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "frontend", a.Frontend.Args, "--tokenizer", testModel)
			}},
		{name: "different tokenizer is rejected", args: []string{"--tokenizer", "bert-base-uncased"},
			wantSetting: "profile", wantReason: "tokenizer"},
		{name: "skipped tokenizer init is rejected", args: []string{"--skip-tokenizer-init"},
			wantSetting: "--skip-tokenizer-init", wantReason: "tokenizer"},

		// Served-model aliases.
		{name: "served aliases are preserved", args: []string{"--served-model-name", "qwen3", "--served-model-name", "default"},
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "frontend", a.Frontend.Args, "--served-model-name", "qwen3")
				requireArgs(t, "frontend", a.Frontend.Args, "--served-model-name", "default")
				if len(a.Model.ServedNames) != 2 {
					t.Errorf("plan served names = %v, want two aliases", a.Model.ServedNames)
				}
			}},

		// Host, inference port, API authentication, TLS.
		{name: "serving endpoint is preserved", args: []string{"--host", "0.0.0.0", "--port", "8080"},
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "frontend", a.Frontend.Args, "--host", "0.0.0.0")
				requireArgs(t, "frontend", a.Frontend.Args, "--port", "8080")
			}},
		{name: "unix socket serving is rejected", args: []string{"--uds", "/tmp/vllm.sock"},
			wantSetting: "--uds", wantReason: "Unix-domain-socket"},
		{name: "headless serving is rejected", args: []string{"--headless"},
			wantSetting: "--headless", wantReason: "headless"},
		{name: "api key never enters the plan", args: []string{"--api-key", "s3cr3t-api-key"},
			check: func(t *testing.T, a *plan.Admission) {
				credentialFree(t, a, "s3cr3t-api-key")
				copied := a.Frontend.CopiedArguments
				if len(copied) != 1 || copied[0].Setting != "--api-key" {
					t.Fatalf("copied arguments = %v, want the --api-key argument copied at mutation time", copied)
				}
			}},
		{name: "custom plugin path is not a credential leak", args: []string{"--tool-parser-plugin", "/opt/plugin.py"},
			wantSetting: "--tool-parser-plugin", wantReason: "plugin"},

		// Tool/reasoning parsers and frontend formatting.
		{name: "authorized tool parser is preserved", args: []string{"--tool-call-parser", "hermes"}, mutate: allowToolParser,
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "frontend", a.Frontend.Args, "--tool-call-parser", "hermes")
			}},
		{name: "unauthorized tool parser is rejected", args: []string{"--tool-call-parser", "hermes"},
			wantSetting: "--tool-call-parser", wantReason: "capability"},
		{name: "a different tool parser than the validated one is rejected",
			args: []string{"--tool-call-parser", "llama3_json"}, mutate: allowToolParser,
			wantSetting: "--tool-call-parser", wantReason: "llama3_json"},

		// Generation defaults and sampling.
		{name: "seed is translated to the simulator", args: []string{"--seed", "42"},
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "backend", a.Backend.Command, "--seed", "42")
				rejectArgs(t, "frontend", a.Frontend.Args, "--seed")
			}},
		{name: "generation override is preserved", args: []string{"--override-generation-config", `{"temperature":0.25}`},
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "frontend", a.Frontend.Args, "--override-generation-config", `{"temperature":0.25}`)
			}},

		// Tensor parallelism, precision, quantization.
		{name: "matching tensor parallel size is accepted", args: []string{"--tensor-parallel-size", "1"},
			check: func(t *testing.T, a *plan.Admission) {
				rejectArgs(t, "frontend", a.Frontend.Args, "--tensor-parallel-size")
			}},
		{name: "tensor parallel size above the device count is rejected", args: []string{"--tensor-parallel-size", "2"},
			wantSetting: "--tensor-parallel-size", wantReason: "device"},
		{name: "different dtype is rejected", args: []string{"--dtype", "float16"},
			wantSetting: "profile", wantReason: "bfloat16"},
		{name: "different quantization is rejected", args: []string{"--quantization", "fp8"},
			wantSetting: "profile", wantReason: "quantization"},

		// Pipeline parallelism, distributed workers, remote engine addresses.
		{name: "single-stage pipeline is accepted", args: []string{"--pipeline-parallel-size", "1"}},
		{name: "pipeline parallelism above one is rejected", args: []string{"--pipeline-parallel-size", "2"},
			wantSetting: "--pipeline-parallel-size", wantReason: "pipeline"},
		{name: "ray executor backend is rejected", args: []string{"--distributed-executor-backend", "ray"},
			wantSetting: "--distributed-executor-backend", wantReason: "external_launcher"},
		{name: "external launcher executor backend is accepted", args: []string{"--distributed-executor-backend", "external_launcher"}},
		{name: "remote engine address is rejected", args: []string{"--data-parallel-address", "10.1.2.3"},
			wantSetting: "--data-parallel-address", wantReason: "remote"},
		{name: "data parallel rank env is rejected", env: []corev1.EnvVar{{Name: "VLLM_DP_RANK", Value: "1"}},
			wantSetting: "VLLM_DP_RANK", wantReason: "rank"},

		// GPU resource count.
		{name: "more devices than the environment advertises is rejected", devices: 16,
			wantSetting: names.SyntheticResourceName, wantReason: "advertises"},

		// GPU memory utilization, KV-memory allocation, cache-block overrides.
		{name: "block override inside coverage is translated", args: []string{"--num-gpu-blocks-override", "1024"},
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "backend", a.Backend.Command, "--kv-cache-size", "1024")
			}},
		{name: "block override outside coverage is rejected", args: []string{"--num-gpu-blocks-override", "4096"},
			wantSetting: "profile coverage", wantReason: "kvBlocks"},
		{name: "memory fraction without a conversion is rejected", args: []string{"--gpu-memory-utilization", "0.9"},
			wantSetting: "profile coverage", wantReason: "kvMemoryConversion"},
		{name: "memory fraction with a conversion is accepted", args: []string{"--gpu-memory-utilization", "0.9"},
			mutate: allowKVMemoryConversion,
			check: func(t *testing.T, a *plan.Admission) {
				// The fraction authorizes the setting but never becomes the simulated
				// capacity: the plan must say the recorded kvBlocks stand.
				if !warningsContain(a, "cache.kvBlocks") {
					t.Errorf("warnings %v do not state that simulated capacity is the recorded cache.kvBlocks", a.Warnings)
				}
				requireArgs(t, "backend", a.Backend.Command, "--kv-cache-size", "2048")
			}},
		{name: "kv-cache byte budget is rejected: no validated conversion", args: []string{"--kv-cache-memory-bytes", "1073741824"},
			mutate:      allowKVMemoryConversion,
			wantSetting: "--kv-cache-memory-bytes", wantReason: "blocks"},
		{name: "cpu offload is rejected", args: []string{"--cpu-offload-gb", "4"},
			wantSetting: "--cpu-offload-gb", wantReason: "offload"},

		// Maximum model length.
		{name: "lower maximum model length is translated", args: []string{"--max-model-len", "16384"},
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "backend", a.Backend.Command, "--max-model-len", "16384")
				// The frontend must enforce the same cap: leaving it unset would let
				// the frontend advertise and accept prompts beyond recorded coverage.
				requireArgs(t, "frontend", a.Frontend.Args, "--max-model-len", "16384")
			}},
		{name: "auto maximum model length resolves to the coverage bound", args: []string{"--max-model-len", "-1"},
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "backend", a.Backend.Command, "--max-model-len", "131072")
				requireArgs(t, "frontend", a.Frontend.Args, "--max-model-len", "131072")
				if !warningsContain(a, "model maximum") {
					t.Errorf("warnings %v do not explain the resolved bound", a.Warnings)
				}
			}},
		{name: "unset maximum model length is enforced by both processes",
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "backend", a.Backend.Command, "--max-model-len", "131072")
				requireArgs(t, "frontend", a.Frontend.Args, "--max-model-len", "131072")
			}},
		{name: "maximum model length above coverage is rejected", args: []string{"--max-model-len", "262144"},
			wantSetting: "profile coverage", wantReason: "maxModelLen"},

		// Maximum running sequences and per-step token budget.
		{name: "sequence cap inside coverage is translated", args: []string{"--max-num-seqs", "64"},
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "backend", a.Backend.Command, "--max-num-seqs", "64")
			}},
		{name: "sequence cap above coverage is rejected", args: []string{"--max-num-seqs", "256"},
			wantSetting: "profile coverage", wantReason: "maxNumSeqs"},
		{name: "token budget inside coverage is translated", args: []string{"--max-num-batched-tokens", "4096"},
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "backend", a.Backend.Command, "--max-num-batched-tokens", "4096")
			}},
		{name: "token budget above coverage is rejected", args: []string{"--max-num-batched-tokens", "16384"},
			wantSetting: "profile coverage", wantReason: "maxNumBatchedTokens"},

		// Scheduling policy.
		{name: "priority scheduling is translated", args: []string{"--scheduling-policy", "priority"},
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "backend", a.Backend.Command, "--scheduling-policy", "priority")
			}},
		{name: "scheduling policy without coverage is rejected", args: []string{"--scheduling-policy", "priority"},
			mutate: withoutSchedulingCoverage, wantSetting: "profile coverage", wantReason: "schedulingPolicies"},
		{name: "async scheduling is rejected", args: []string{"--async-scheduling"},
			wantSetting: "--async-scheduling", wantReason: "asynchronous"},

		// Chunked prefill.
		{name: "chunked prefill matches the profile", args: []string{"--long-prefill-token-threshold", "1024"},
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "backend", a.Backend.Command, "--long-prefill-token-threshold", "1024")
			}},
		{name: "chunked prefill threshold outside coverage is rejected", args: []string{"--long-prefill-token-threshold", "8192"},
			wantSetting: "profile coverage", wantReason: "longPrefillTokenThreshold"},

		// KV block size.
		{name: "matching block size is translated", args: []string{"--block-size", "64"},
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "backend", a.Backend.Command, "--tokens-per-block", "64")
			}},
		{name: "different block size is rejected", args: []string{"--block-size", "32"},
			wantSetting: "profile", wantReason: "blockSize"},

		// Prefix caching.

		// KV live events.
		{name: "kv events are translated", args: []string{"--kv-events-config", `{"enable_kv_cache_events":true,"publisher":"zmq"}`},
			identity: "pod-1",
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "backend", a.Backend.Command, "--enable-kv-cache-events")
				requireArgs(t, "backend", a.Backend.Command, "--kv-events-endpoint", "tcp://*:5556")
				requireArgs(t, "backend", a.Backend.Command, "--kv-events-topic", "kv@$(POD_IP):8000@"+testModel)
			}},
		{name: "kv events for a profile without events are rejected",
			args:        []string{"--kv-events-config", `{"enable_kv_cache_events":true}`},
			mutate:      noKVEvents,
			wantSetting: "--kv-events-config", wantReason: "kvEvents"},
		{name: "kv event replay is rejected for the pinned simulator",
			args:        []string{"--kv-events-config", `{"enable_kv_cache_events":true,"replay_endpoint":"tcp://router:5557"}`},
			wantSetting: "--kv-events-config", wantReason: names.SupportedSimulatorTag},
		{name: "kv event replay is rejected even when the profile claims the capability",
			args:        []string{"--kv-events-config", `{"enable_kv_cache_events":true,"replay_endpoint":"tcp://router:5557"}`},
			mutate:      allowKVReplay,
			wantSetting: "--kv-events-config", wantReason: names.SupportedSimulatorTag},
		{name: "kv event replay is rejected when events are disabled too",
			args:        []string{"--kv-events-config", `{"enable_kv_cache_events":false,"replay_endpoint":"tcp://router:5557"}`},
			wantSetting: "--kv-events-config", wantReason: "replay endpoint"},
		{name: "kv event publisher high-water mark is rejected",
			args:        []string{"--kv-events-config", `{"enable_kv_cache_events":true,"hwm":1000}`},
			wantSetting: "--kv-events-config", wantReason: "hwm"},

		// Shutdown timeout.
		{name: "shutdown timeout is translated to both processes", args: []string{"--shutdown-timeout", "30"},
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "backend", a.Backend.Command, "--shutdown-timeout", "30")
				requireArgs(t, "frontend", a.Frontend.Args, "--shutdown-timeout", "30")
				if got := envValue(a.Frontend.Env, "VCR_SHUTDOWN_TIMEOUT"); got != "30" {
					t.Errorf("VCR_SHUTDOWN_TIMEOUT = %q, want 30", got)
				}
			}},

		// Logging and telemetry.
		{name: "request logging is translated to both processes", args: []string{"--enable-log-requests"},
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "backend", a.Backend.Command, "--log-requests")
				requireArgs(t, "frontend", a.Frontend.Args, "--enable-log-requests")
			}},
		{name: "log statistics warning names the metric source", args: []string{"--disable-log-stats"},
			check: func(t *testing.T, a *plan.Admission) {
				if !warningsContain(a, "no Prometheus metrics") {
					t.Errorf("warnings %v do not classify simulator metrics", a.Warnings)
				}
			}},
		{name: "detailed engine traces are rejected", args: []string{"--collect-detailed-traces", "model"},
			wantSetting: "--collect-detailed-traces", wantReason: "engine"},

		// GPU device/CUDA runtime selection.
		{name: "gpu target device is replaced", env: []corev1.EnvVar{{Name: "VLLM_TARGET_DEVICE", Value: "cuda"}},
			check: func(t *testing.T, a *plan.Admission) {
				if len(a.Frontend.RemoveEnv) == 0 {
					t.Fatal("expected the accelerator target variable to be removed")
				}
				found := false
				for _, name := range a.Frontend.RemoveEnv {
					if name == "VLLM_TARGET_DEVICE" {
						found = true
					}
				}
				if !found {
					t.Errorf("RemoveEnv = %v, want VLLM_TARGET_DEVICE removed", a.Frontend.RemoveEnv)
				}
			}},
		{name: "numa binding is rejected", args: []string{"--numa-bind"},
			wantSetting: "--numa-bind", wantReason: "NUMA"},
		{name: "sleep mode is rejected", args: []string{"--enable-sleep-mode"},
			wantSetting: "--enable-sleep-mode", wantReason: "sleep mode"},

		// Eager execution, CUDA graphs, compilation.
		{name: "eager execution is accepted with a timing warning", args: []string{"--enforce-eager"},
			check: func(t *testing.T, a *plan.Admission) {
				if !warningsContain(a, "recorded timing") {
					t.Errorf("warnings %v do not explain recorded timing", a.Warnings)
				}
			}},

		// LoRA, speculative decoding, multimodal, embeddings, structured outputs.
		{name: "lora without the capability is rejected", args: []string{"--enable-lora"},
			wantSetting: "--enable-lora", wantReason: "lora"},
		{name: "lora with the capability is translated", args: []string{"--enable-lora", "--max-loras", "4"}, mutate: allowLoRA,
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "backend", a.Backend.Command, "--max-loras", "4")
			}},
		{name: "speculative decoding without the capability is rejected",
			args:        []string{"--speculative-config", `{"method":"ngram","num_speculative_tokens":2}`},
			wantSetting: "--speculative-config", wantReason: "speculativeDecoding"},
		{name: "speculative decoding with the capability is accepted",
			args:   []string{"--speculative-config", `{"method":"ngram","num_speculative_tokens":2}`},
			mutate: allowSpeculative},
		{name: "structured outputs without the capability are rejected",
			args:        []string{"--structured-outputs-config", `{"backend":"xgrammar"}`},
			wantSetting: "--structured-outputs-config", wantReason: "structuredOutputs"},
		{name: "structured outputs with the capability are accepted",
			args:   []string{"--structured-outputs-config", `{"backend":"xgrammar"}`},
			mutate: allowStructured},
		{name: "multimodal limits are forwarded with the capability",
			args:   []string{"--limit-mm-per-prompt", `{"image":2}`, "--allowed-media-domains", "cdn.example.com"},
			mutate: func(m *profile.Metadata) { m.Capabilities.Multimodal = true },
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "frontend", a.Frontend.Args, "--limit-mm-per-prompt", `{"image":2}`)
				requireArgs(t, "frontend", a.Frontend.Args, "--allowed-media-domains", "cdn.example.com")
			}},
		{name: "lora modules are forwarded with the capability",
			args: []string{"--enable-lora", "--lora-modules", "qwen3-lora=Qwen/Qwen3-0.6B-lora"}, mutate: allowLoRA,
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "frontend", a.Frontend.Args, "--lora-modules", "qwen3-lora=Qwen/Qwen3-0.6B-lora")
			}},
		{name: "structured output config is forwarded with the capability",
			args: []string{"--structured-outputs-config", `{"backend":"xgrammar"}`}, mutate: allowStructured,
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "frontend", a.Frontend.Args, "--structured-outputs-config", `{"backend":"xgrammar"}`)
			}},
		{name: "pooling without the capability is rejected", args: []string{"--runner", "pooling"},
			wantSetting: "--runner", wantReason: "embeddings"},
		{name: "pooling with the capability is accepted", args: []string{"--runner", "pooling"}, mutate: allowEmbeddings},

		// P/D roles and KV-transfer settings.
		{name: "disaggregated role without the capability is rejected",
			args:        []string{"--kv-transfer-config", `{"kv_role":"kv_producer","kv_connector":"NixlConnector"}`},
			wantSetting: "--kv-transfer-config", wantReason: "disaggregatedPrefill"},
		{name: "disaggregated role with the capability is translated",
			args:   []string{"--kv-transfer-config", `{"kv_role":"kv_producer","kv_connector":"NixlConnector"}`},
			mutate: allowDisaggregated,
			check: func(t *testing.T, a *plan.Admission) {
				requireArgs(t, "backend", a.Backend.Command, "--pd-role", "prefill")
			}},

		// Unknown behavior-affecting settings.
		{name: "unknown vllm environment variable is rejected", env: []corev1.EnvVar{{Name: "VLLM_SURPRISE_FEATURE", Value: "1"}},
			wantSetting: "VLLM_SURPRISE_FEATURE", wantReason: "undefined"},
		{name: "custom scheduler class is rejected", args: []string{"--scheduler-cls", "my.Scheduler"},
			wantSetting: "--scheduler-cls", wantReason: "scheduler class"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			admission, err := runTranslate(t, tc, testEnvironment())
			if tc.wantSetting != "" {
				mustReject(t, tc, err)
				return
			}
			mustAccept(t, admission, err)
			if tc.check != nil {
				tc.check(t, admission)
			}
		})
	}
}

func TestRunnerPoolingRequiresEmbeddingsCapability(t *testing.T) {
	tc := translateCase{name: "pooling", args: []string{"--runner", "pooling"}, mutate: func(m *profile.Metadata) {
		m.Capabilities.Embeddings = false
	}}
	_, err := runTranslate(t, tc, testEnvironment())
	if err == nil {
		t.Fatal("Translate() accepted pooling without the embeddings capability")
	}
	if !strings.Contains(err.Error(), profile.CapabilityEmbeddings) {
		t.Errorf("rejection %q does not name the embeddings capability", err)
	}
}

func TestProfileWithoutLatencyModelIsRejected(t *testing.T) {
	tc := translateCase{name: "no latency model", mutate: func(m *profile.Metadata) { m.Trace.LatencyModel = false }}
	_, err := runTranslate(t, tc, testEnvironment())
	if err == nil {
		t.Fatal("Translate() accepted a profile without recorded timing")
	}
	if !strings.Contains(err.Error(), "latency model") {
		t.Errorf("rejection %q does not explain the missing latency model", err)
	}
}

func TestProfileProvisioningEntrypointIsRequired(t *testing.T) {
	tc := translateCase{name: "no provisioning entrypoint", mutate: func(m *profile.Metadata) {
		m.Provisioning.Entrypoint = nil
	}}
	_, err := runTranslate(t, tc, testEnvironment())
	if err == nil {
		t.Fatal("Translate() accepted a profile without a provisioning entrypoint")
	}
	if !strings.Contains(err.Error(), "provisioning") {
		t.Errorf("rejection %q does not name the provisioning entrypoint", err)
	}
}

func TestSelectionRequiresCompatibleVLLMRelease(t *testing.T) {
	tc := translateCase{name: "vllm release", mutate: func(m *profile.Metadata) { m.VLLM.Version = "0.32.0" }}
	_, err := runTranslate(t, tc, testEnvironment())
	if err == nil {
		t.Fatal("Translate() selected a profile recorded against another release")
	}
	if !strings.Contains(err.Error(), "vllm.version") {
		t.Errorf("rejection %q does not name the profile release", err)
	}
}

// TestCombinationCoverageAndBatching asserts that per-setting acceptance is not
// combination acceptance: both limits are individually outside coverage and both
// are reported in one rejection.
func TestCombinationCoverageAndBatching(t *testing.T) {
	tc := translateCase{
		name: "two limits outside coverage",
		args: []string{"--max-num-seqs", "512", "--max-num-batched-tokens", "65536"},
	}
	_, err := runTranslate(t, tc, testEnvironment())
	if err == nil {
		t.Fatal("Translate() accepted limits outside coverage")
	}
	message := err.Error()
	for _, setting := range []string{"maxNumSeqs", "maxNumBatchedTokens"} {
		if !strings.Contains(message, setting) {
			t.Errorf("rejection %q does not report %s", message, setting)
		}
	}
}

// TestCombinationChunkedPrefillAndTokenBudget asserts the cross-family check: a
// token budget below the model length cannot serve a non-chunked profile.
func TestCombinationChunkedPrefillAndTokenBudget(t *testing.T) {
	tc := translateCase{
		name:   "token budget below the model length without chunking",
		args:   []string{"--max-num-batched-tokens", "1024"},
		mutate: func(m *profile.Metadata) { m.Cache.ChunkedPrefill = false },
	}
	_, err := runTranslate(t, tc, testEnvironment())
	if err == nil {
		t.Fatal("Translate() accepted a per-step budget that cannot hold a prompt")
	}
	if !strings.Contains(err.Error(), "--max-num-batched-tokens") {
		t.Errorf("rejection %q does not name the per-step token budget", err)
	}
}

// TestCombinationPrefixCachingWithKVEvents covers the documented combination:
// block events with caching disabled require a profile recorded that way.
func TestCombinationPrefixCachingWithKVEvents(t *testing.T) {
	events := []string{"--kv-events-config", `{"enable_kv_cache_events":true}`}
	t.Run("profile without events", func(t *testing.T) {
		tc := translateCase{name: "no events", args: events, mutate: func(m *profile.Metadata) {
			m.Cache.EnablePrefixCaching = false
			m.Trace.KVEvents = false
		}}
		_, err := runTranslate(t, tc, testEnvironment())
		if err == nil {
			t.Fatal("Translate() accepted events from a profile without KV events")
		}
		if !strings.Contains(err.Error(), "kvEvents") {
			t.Errorf("rejection %q does not name the recorded event support", err)
		}
	})
	t.Run("profile with events and caching disabled", func(t *testing.T) {
		tc := translateCase{name: "events without caching", args: events, identity: "pod-2", check: func(t *testing.T, a *plan.Admission) {
			requireArgs(t, "backend", a.Backend.Command, "--enable-kv-cache-events")
		}}
		metadata := testMetadata(func(m *profile.Metadata) { m.Cache.EnablePrefixCaching = false })
		admission, err := translator.Translate(translator.Request{
			Config:      resolveConfig(t, tc.args),
			Catalog:     testCatalog(t, metadata),
			Environment: testEnvironment(),
			GPUDevices:  1,
			PodIdentity: "pod-2",
		})
		mustAccept(t, admission, err)
		tc.check(t, admission)
	})
}

// TestEnvironmentCapabilitiesAreValidated asserts that a misconfigured environment
// is an operator error, not an admission decision.
func TestEnvironmentCapabilitiesAreValidated(t *testing.T) {
	tests := []struct {
		name string
		env  func(*translator.Environment)
		want string
	}{
		{name: "missing gpu type", env: func(e *translator.Environment) { e.GPUType = "" }, want: "gpuType"},
		{name: "no devices per node", env: func(e *translator.Environment) { e.DevicesPerNode = 0 }, want: "devicesPerNode"},
		{name: "missing frontend image", env: func(e *translator.Environment) { e.Images.Frontend = "" }, want: "frontend image"},
		{name: "missing backend image", env: func(e *translator.Environment) { e.Images.Backend = "" }, want: "backend image"},
		{name: "unsupported release", env: func(e *translator.Environment) { e.SupportedVLLMVersion = "0.32.0" }, want: "supportedVLLMVersion"},
		{name: "sizing requests the synthetic device", env: func(e *translator.Environment) {
			e.Sizing.Backend.Requests = corev1.ResourceList{names.SyntheticResourceName: resource.MustParse("1")}
		}, want: "synthetic device"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnvironment()
			tc.env(&env)
			_, err := translator.Translate(translator.Request{
				Config:      resolveConfig(t, nil),
				Catalog:     testCatalog(t, testMetadata()),
				Environment: env,
				GPUDevices:  1,
			})
			if err == nil {
				t.Fatal("Translate() accepted a misconfigured environment")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("rejection %q does not name %q", err, tc.want)
			}
		})
	}
}

func TestTranslateRequiresInputs(t *testing.T) {
	if _, err := translator.Translate(translator.Request{}); err == nil {
		t.Fatal("Translate() accepted a request without a configuration")
	}
	if _, err := translator.Translate(translator.Request{Config: resolveConfig(t, nil)}); err == nil {
		t.Fatal("Translate() accepted a request without a catalog")
	}
}

// TestNoDeviceRequestIsRejected covers the synthetic-resource guard.
func TestNoDeviceRequestIsRejected(t *testing.T) {
	_, err := translator.Translate(translator.Request{
		Config:      resolveConfig(t, nil),
		Catalog:     testCatalog(t, testMetadata()),
		Environment: testEnvironment(),
		GPUDevices:  0,
	})
	if err == nil {
		t.Fatal("Translate() accepted a workload requesting no device")
	}
	if !strings.Contains(err.Error(), names.SyntheticResourceName) {
		t.Errorf("rejection %q does not name the synthetic resource", err)
	}
}

// TestPortCollisionIsRejected covers the shared Pod network namespace.
func TestPortCollisionIsRejected(t *testing.T) {
	_, err := translator.Translate(translator.Request{
		Config:      resolveConfig(t, []string{"--port", "8001"}),
		Catalog:     testCatalog(t, testMetadata()),
		Environment: testEnvironment(),
		GPUDevices:  1,
	})
	if err == nil {
		t.Fatal("Translate() accepted an inference port that collides with the control API")
	}
	if !strings.Contains(err.Error(), "8001") {
		t.Errorf("rejection %q does not name the colliding port", err)
	}
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, entry := range env {
		if entry.Name == name {
			return entry.Value
		}
	}
	return ""
}
