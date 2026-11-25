package vllm_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/neuralmagic/vllm-vcr-operator/internal/vllm"
)

// configMapVolume builds the Pod context of a config file injected from a
// ConfigMap, with the ConfigMap content already resolved by the caller.
func configMapVolume(mountPath, fileName string, content []byte) vllm.Invocation {
	inv := vllm.Invocation{
		Container: corev1.Container{
			Name:         "modelserver",
			Image:        "vllm/vllm-openai:v0.33.0",
			Command:      []string{"vllm", "serve"},
			VolumeMounts: []corev1.VolumeMount{{Name: "serve-config", MountPath: mountPath, ReadOnly: true}},
		},
		Volumes: map[string]corev1.Volume{
			"serve-config": {
				Name: "serve-config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "serve-config"},
					},
				},
			},
		},
	}
	if content != nil {
		inv.Files = map[string][]byte{mountPath + "/" + fileName: content}
	}
	return inv
}

const serveConfigYAML = `
model: Qwen/Qwen3-8B
port: 8100
max-model-len: 4096
tensor-parallel-size: 2
enable-prefix-caching: false
uvicorn-log-level: debug
kv-events-config:
  enable_kv_cache_events: true
  publisher: zmq
  endpoint: tcp://*:5556
  topic: kv@127.0.0.1:8000@Qwen/Qwen3-8B
`

func TestResolveConfigFileMountedFromConfigMap(t *testing.T) {
	inv := configMapVolume("/etc/vllm", "serve.yaml", []byte(serveConfigYAML))
	inv.Container.Args = []string{"--config=/etc/vllm/serve.yaml"}

	cfg := resolve(t, inv)

	wantString(t, "model", cfg.Model.Name, "Qwen/Qwen3-8B")
	wantInt(t, "port", cfg.Server.Port, 8100)
	wantInt(t, "max-model-len", cfg.Limits.MaxModelLen, 4096)
	wantInt(t, "tensor-parallel-size", cfg.Parallel.TensorParallelSize, 2)
	wantString(t, "uvicorn-log-level", cfg.Server.UvicornLogLevel, "debug")
	// "enable-prefix-caching: false" is a negated BooleanOptionalAction, which is
	// how vLLM expresses an explicit false.
	wantBool(t, "prefix caching", cfg.Cache.EnablePrefixCaching, false)

	if cfg.KVEvents == nil {
		t.Fatal("kv-events-config from the config file was not resolved")
	}
	wantBool(t, "kv events enabled", cfg.KVEvents.Enable, true)
	wantString(t, "kv events publisher", cfg.KVEvents.Publisher, "zmq")
	wantString(t, "kv events endpoint", cfg.KVEvents.Endpoint, "tcp://*:5556")
	wantString(t, "kv events topic", cfg.KVEvents.Topic, "kv@127.0.0.1:8000@Qwen/Qwen3-8B")

	for _, setting := range []vllm.Setting{
		vllm.SettingModel, vllm.SettingPort, vllm.SettingMaxModelLen,
		vllm.SettingTensorParallelSize, vllm.SettingUvicornLogLevel,
		vllm.SettingPrefixCaching, vllm.SettingKVEventsConfig,
	} {
		wantOrigin(t, cfg, setting, vllm.OriginConfigFile)
	}
	// The config file itself is a workload input, so it is reported as such.
	wantOrigin(t, cfg, vllm.SettingConfigFile, vllm.OriginCLI)
}

// The order of priorities is documented by vLLM 0.33.0 as command line >
// config file values > defaults; environment variables sit between the config
// file and the defaults because they are not part of the argument parser.
func TestResolveConfigFilePrecedence(t *testing.T) {
	inv := configMapVolume("/etc/vllm", "serve.yaml", []byte(serveConfigYAML))
	inv.Container.Args = []string{"Qwen/Qwen3-Coder-30B-A3B-Instruct", "--config", "/etc/vllm/serve.yaml", "--port=9000"}
	inv.Container.Env = []corev1.EnvVar{
		{Name: "VLLM_PORT", Value: "7000"},
		{Name: "VLLM_LOGGING_LEVEL", Value: "warning"},
	}

	cfg := resolve(t, inv)

	// The positional model beats the config file, and --port beats both the config
	// file and VLLM_PORT.
	wantString(t, "model", cfg.Model.Name, "Qwen/Qwen3-Coder-30B-A3B-Instruct")
	wantInt(t, "port", cfg.Server.Port, 9000)
	// The config file beats the environment.
	wantString(t, "uvicorn-log-level", cfg.Server.UvicornLogLevel, "debug")
	// vLLM's log level has no CLI or file value here, so the environment wins over
	// the default.
	wantString(t, "log level", cfg.Observability.LogLevel, "warning")

	wantOrigin(t, cfg, vllm.SettingPort, vllm.OriginCLI)
	wantOrigin(t, cfg, vllm.SettingUvicornLogLevel, vllm.OriginConfigFile)
	wantOrigin(t, cfg, vllm.SettingLogLevel, vllm.OriginEnv)
}

func TestResolveConfigFileFromSecretVolume(t *testing.T) {
	inv := vllm.Invocation{
		Container: corev1.Container{
			Name:         "modelserver",
			Image:        "vllm/vllm-openai:v0.33.0",
			Command:      []string{"vllm", "serve"},
			Args:         []string{"--config", "/etc/serve/config.yaml"},
			VolumeMounts: []corev1.VolumeMount{{Name: "serve-config", MountPath: "/etc/serve"}},
		},
		Volumes: map[string]corev1.Volume{
			"serve-config": {
				Name: "serve-config",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: "serve-config"},
				},
			},
		},
		Files: map[string][]byte{"/etc/serve/config.yaml": []byte("model: Qwen/Qwen3-8B\nport: 8123\n")},
	}

	cfg := resolve(t, inv)
	wantString(t, "model", cfg.Model.Name, "Qwen/Qwen3-8B")
	wantInt(t, "port", cfg.Server.Port, 8123)
}

// A JSON config file is accepted because YAML is a superset of JSON and
// generated configuration is often JSON.
func TestResolveConfigFileAsJSON(t *testing.T) {
	inv := configMapVolume("/etc/vllm", "serve.json", []byte(`{"model":"Qwen/Qwen3-8B","port":8123}`))
	inv.Container.Args = []string{"--config=/etc/vllm/serve.json"}

	cfg := resolve(t, inv)
	wantString(t, "model", cfg.Model.Name, "Qwen/Qwen3-8B")
	wantInt(t, "port", cfg.Server.Port, 8123)
}

// Config-file values keep the types vLLM's loader assigns them: lists become
// repeated arguments and numbers are rendered without a trailing ".0".
func TestResolveConfigFileScalarsAndLists(t *testing.T) {
	content := `
model: RedHatAI/Qwen3-8B-quantized.w8a8
served-model-name:
  - Qwen/Qwen3-8B
  - qwen3-8b
gpu-memory-utilization: 0.85
max-num-seqs: 64
enable-log-requests: true
`
	inv := configMapVolume("/etc/vllm", "serve.yaml", []byte(content))
	inv.Container.Args = []string{"--config=/etc/vllm/serve.yaml"}

	cfg := resolve(t, inv)

	if names := cfg.Model.ServedModelNames; len(names) != 2 || names[0] != "Qwen/Qwen3-8B" || names[1] != "qwen3-8b" {
		t.Fatalf("served model names = %v, want [Qwen/Qwen3-8B qwen3-8b]", names)
	}
	wantFloat(t, "gpu memory utilization", cfg.Memory.GPUMemoryUtilization, 0.85)
	wantInt(t, "max num seqs", cfg.Limits.MaxNumSeqs, 64)
	wantBool(t, "enable log requests", cfg.Observability.EnableLogRequests, true)
	wantOrigin(t, cfg, vllm.SettingLogRequests, vllm.OriginConfigFile)
}

func TestResolveConfigFileNestedObjects(t *testing.T) {
	content := `
model: Qwen/Qwen3-8B
compilation-config:
  mode: 3
  pass_config:
    fuse_allreduce_rms: true
speculative-config:
  method: ngram
  num_speculative_tokens: 4
`
	inv := configMapVolume("/etc/vllm", "serve.yaml", []byte(content))
	inv.Container.Args = []string{"--config=/etc/vllm/serve.yaml"}

	cfg := resolve(t, inv)

	if cfg.Execution.CompilationConfig == nil {
		t.Fatal("nested compilation-config was not resolved into a JSON object")
	}
	if got := cfg.Execution.CompilationConfig["mode"]; got != float64(3) {
		t.Fatalf("compilation-config.mode = %v, want 3", got)
	}
	if cfg.Speculative == nil {
		t.Fatal("nested speculative-config was not resolved")
	}
	wantString(t, "speculative method", cfg.Speculative.Method, "ngram")
	wantInt(t, "num speculative tokens", cfg.Speculative.NumSpeculativeTokens, 4)
}

func TestResolveRejectsUnresolvableConfigFiles(t *testing.T) {
	tests := []struct {
		name    string
		inv     vllm.Invocation
		setting vllm.Setting
		reason  string
	}{
		{
			name: "path is not mounted",
			inv: vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
				[]string{"vllm", "serve"}, "--config=/etc/vllm/serve.yaml")},
			setting: vllm.SettingConfigFile,
			reason:  "not backed by a volume mount",
		},
		{
			name:    "volume content unavailable",
			inv:     unavailableConfigMapInvocation(),
			setting: vllm.SettingConfigFile,
			reason:  "ConfigMap volume",
		},
		{
			name:    "unexpected key",
			inv:     configFileInvocation("model: Qwen/Qwen3-8B\nmax-num-seqs-typo: 4\n"),
			setting: vllm.Setting("--max-num-seqs-typo"),
			reason:  "unknown flag",
		},
		{
			name:    "unmodelled vllm flag",
			inv:     configFileInvocation("model: Qwen/Qwen3-8B\nperformance-mode: throughput\n"),
			setting: vllm.Setting("--performance-mode"),
			reason:  "does not model",
		},
		{
			name:    "value is not a mapping",
			inv:     configFileInvocation("- model: Qwen/Qwen3-8B\n"),
			setting: vllm.SettingConfigFile,
			reason:  "mapping",
		},
		{
			name:    "value is not valid yaml",
			inv:     configFileInvocation("model: [Qwen/Qwen3-8B\n"),
			setting: vllm.SettingConfigFile,
			reason:  "not valid YAML",
		},
		{
			name:    "null value",
			inv:     configFileInvocation("model: Qwen/Qwen3-8B\nport:\n"),
			setting: vllm.SettingConfigFile,
			reason:  "null",
		},
		{
			name:    "relative path without a working directory",
			inv:     relativeConfigPathInvocation(),
			setting: vllm.SettingConfigFile,
			reason:  "working directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := vllm.Resolve(tt.inv)
			requireRejection(t, err, tt.setting, tt.reason)
		})
	}
}

func configFileInvocation(content string) vllm.Invocation {
	inv := configMapVolume("/etc/vllm", "serve.yaml", []byte(content))
	inv.Container.Args = []string{"--config=/etc/vllm/serve.yaml"}
	return inv
}

func unavailableConfigMapInvocation() vllm.Invocation {
	inv := configMapVolume("/etc/vllm", "serve.yaml", nil)
	inv.Container.Args = []string{"--config=/etc/vllm/serve.yaml"}
	return inv
}

func relativeConfigPathInvocation() vllm.Invocation {
	inv := configMapVolume("/etc/vllm", "serve.yaml", []byte(serveConfigYAML))
	inv.Container.Args = []string{"--config=serve.yaml"}
	return inv
}
