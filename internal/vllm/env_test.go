package vllm_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/vllm"
)

// vLLM reads some of its configuration from the environment. The resolver
// reports those values with OriginEnv, between the config file and the defaults.
func TestResolveEnvironmentSettings(t *testing.T) {
	inv := vllm.Invocation{Container: corev1.Container{
		Name:    "modelserver",
		Image:   "vllm/vllm-openai:v0.33.0",
		Command: []string{"vllm", "serve", "Qwen/Qwen3-8B"},
		Env: []corev1.EnvVar{
			{Name: "VLLM_HOST_IP", Value: "0.0.0.0"},
			{Name: "VLLM_PORT", Value: "8200"},
			{Name: "VLLM_LOGGING_LEVEL", Value: "warning"},
			{Name: "VLLM_LOG_STATS_INTERVAL", Value: "2.5"},
			{Name: "VLLM_ENGINE_READY_TIMEOUT_S", Value: "3600"},
			{Name: "VLLM_WORKER_SHUTDOWN_TIMEOUT_SECONDS", Value: "15"},
			{Name: "VLLM_TARGET_DEVICE", Value: "cpu"},
			{Name: "VLLM_USE_MODELSCOPE", Value: "1"},
			{Name: "VLLM_ALLOW_LONG_MAX_MODEL_LEN", Value: "true"},
			{Name: "VLLM_MODEL_REDIRECT_PATH", Value: "/models/qwen3-8b"},
			{Name: "VLLM_CPU_KVCACHE_SPACE", Value: "4"},
			{Name: "HF_ENDPOINT", Value: "https://hub.internal.example"},
		},
	}}

	cfg := resolve(t, inv)

	wantString(t, "host", cfg.Server.Host, "0.0.0.0")
	wantInt(t, "port", cfg.Server.Port, 8200)
	wantString(t, "log level", cfg.Observability.LogLevel, "warning")
	wantInt(t, "engine ready timeout", cfg.Lifecycle.EngineReadyTimeoutSeconds, 3600)
	wantInt(t, "worker shutdown timeout", cfg.Lifecycle.WorkerShutdownTimeoutSeconds, 15)
	wantString(t, "target device", cfg.Device.TargetDevice, "cpu")
	wantBool(t, "model scope", cfg.Environment.UseModelScope, true)
	wantBool(t, "allow long max model len", cfg.Environment.AllowLongMaxModelLen, true)
	wantString(t, "model redirect", cfg.Environment.ModelRedirectPath, "/models/qwen3-8b")
	wantInt(t, "cpu kv cache space", cfg.Memory.CPUKVCacheSpaceGB, 4)
	wantString(t, "hf endpoint", cfg.Environment.HFEndpoint, "https://hub.internal.example")

	if cfg.Observability.LogStatsIntervalSeconds == nil || *cfg.Observability.LogStatsIntervalSeconds != 2.5 {
		t.Fatalf("log stats interval = %v, want 2.5", cfg.Observability.LogStatsIntervalSeconds)
	}

	for _, setting := range []vllm.Setting{
		vllm.SettingHost, vllm.SettingPort, vllm.SettingLogLevel,
		vllm.SettingLogStatsInterval, vllm.SettingEngineReadyTimeout,
		vllm.SettingWorkerShutdown, vllm.SettingTargetDevice,
		vllm.SettingModelScope, vllm.SettingLongMaxModelLen,
		vllm.SettingModelRedirect, vllm.SettingCPUKVCacheSpace, vllm.SettingHFEndpoint,
	} {
		wantOrigin(t, cfg, setting, vllm.OriginEnv)
	}
}

// Data-parallel environment variables are how vLLM runs a remote engine behind
// an external load balancer; the topology must be visible to the policy layer.
func TestResolveDataParallelEnvironmentTopology(t *testing.T) {
	inv := vllm.Invocation{Container: corev1.Container{
		Name:    "decode",
		Image:   "vllm/vllm-openai:v0.33.0",
		Command: []string{"vllm", "serve", "Qwen/Qwen3-8B"},
		Env: []corev1.EnvVar{
			{Name: "VLLM_DP_SIZE", Value: "2"},
			{Name: "VLLM_DP_RANK", Value: "1"},
			{Name: "VLLM_DP_RANK_LOCAL", Value: "0"},
			{Name: "VLLM_DP_MASTER_IP", Value: "10.0.0.7"},
			{Name: "VLLM_DP_MASTER_PORT", Value: "13345"},
		},
	}}

	cfg := resolve(t, inv)

	wantInt(t, "data-parallel-size", cfg.Parallel.DataParallel.Size, 2)
	wantInt(t, "data-parallel-rank", cfg.Parallel.DataParallel.Rank, 1)
	wantInt(t, "data-parallel-rank-local", cfg.Parallel.DataParallel.LocalRank, 0)
	wantString(t, "data-parallel master ip", cfg.Parallel.DataParallel.MasterIP, "10.0.0.7")
	wantInt(t, "data-parallel master port", cfg.Parallel.DataParallel.MasterPort, 13345)
	wantOrigin(t, cfg, vllm.SettingDPSize, vllm.OriginEnv)
}

// Credentials are recorded by name only. The value must never appear in the
// resolved configuration, in an error, or in the string form of either.
func TestResolveNeverRetainsCredentialValues(t *testing.T) {
	const secret = "sk-do-not-leak"

	inv := vllm.Invocation{Container: corev1.Container{
		Name:    "modelserver",
		Image:   "vllm/vllm-openai:v0.33.0",
		Command: []string{"vllm", "serve", "Qwen/Qwen3-8B"},
		Env: []corev1.EnvVar{
			{Name: "VLLM_API_KEY", Value: secret},
			{Name: "HF_TOKEN", Value: secret},
		},
	}}

	cfg := resolve(t, inv)
	if !cfg.Auth.APIKeyRequired {
		t.Fatal("VLLM_API_KEY did not mark the server as requiring an API key")
	}
	if cfg.Auth.APIKeySource != "VLLM_API_KEY" {
		t.Fatalf("API key source = %q, want the variable name", cfg.Auth.APIKeySource)
	}
	if cfg.Auth.HuggingFaceTokenEnvVar != "HF_TOKEN" {
		t.Fatalf("Hub token variable = %q, want HF_TOKEN", cfg.Auth.HuggingFaceTokenEnvVar)
	}
	if leaked := containsSecret(cfg, secret); leaked {
		t.Fatal("the resolved configuration retains a credential value")
	}
}

// A credential supplied on the command line is never echoed back, including in
// the rejection for another problem in the same invocation.
func TestResolveRedactsCredentialsInRejections(t *testing.T) {
	const secret = "sk-do-not-leak"

	_, err := vllm.Resolve(vllm.Invocation{Container: corev1.Container{
		Name:    "modelserver",
		Image:   "vllm/vllm-openai:v0.33.0",
		Command: []string{"vllm", "serve", "Qwen/Qwen3-8B", "--api-key=" + secret, "--hf-token", secret, "--not-a-flag"},
	}})
	if err == nil {
		t.Fatal("Resolve() accepted an invocation with an unknown flag")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("rejection leaks a credential: %v", err)
	}

	// The api-key flag itself is modelled; an empty key is a rejection that must
	// also be redacted.
	_, err = vllm.Resolve(vllm.Invocation{Container: corev1.Container{
		Name:    "modelserver",
		Image:   "vllm/vllm-openai:v0.33.0",
		Command: []string{"vllm", "serve", "Qwen/Qwen3-8B", "--api-key="},
	}})
	requireRejection(t, err, vllm.SettingAPIKey, "empty")
}

// An API key env var that comes from a Secret reference is still only a
// presence: the Pod spec proves the server is authenticated without revealing
// the value.
func TestResolveCredentialFromSecretReference(t *testing.T) {
	inv := vllm.Invocation{Container: corev1.Container{
		Name:    "modelserver",
		Image:   "vllm/vllm-openai:v0.33.0",
		Command: []string{"vllm", "serve", "Qwen/Qwen3-8B"},
		Env: []corev1.EnvVar{
			{Name: "VLLM_API_KEY", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "api-key"},
					Key:                  "token",
				},
			}},
		},
	}}

	cfg := resolve(t, inv)
	if !cfg.Auth.APIKeyRequired || cfg.Auth.APIKeySource != "VLLM_API_KEY" {
		t.Fatalf("API key mode = %+v, want requirement recorded from VLLM_API_KEY", cfg.Auth)
	}
}

// A modelled variable whose value is a reference the caller could not resolve
// cannot be resolved, so the invocation is rejected instead of falling back to a
// default that the workload did not ask for.
func TestResolveRejectsUnresolvableEnvironmentValue(t *testing.T) {
	inv := vllm.Invocation{Container: corev1.Container{
		Name:    "modelserver",
		Image:   "vllm/vllm-openai:v0.33.0",
		Command: []string{"vllm", "serve", "Qwen/Qwen3-8B"},
		Env: []corev1.EnvVar{
			{Name: "VLLM_PORT", ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "serve-config"},
					Key:                  "port",
				},
			}},
		},
	}}

	_, err := vllm.Resolve(inv)
	requireRejection(t, err, vllm.Setting("VLLM_PORT"), "does not resolve")

	// When the caller resolves the reference, the value is used.
	inv.Env = map[string]string{"VLLM_PORT": "8321"}
	cfg := resolve(t, inv)
	wantInt(t, "port", cfg.Server.Port, 8321)
}

func TestResolveRejectsInvalidEnvironmentValues(t *testing.T) {
	tests := []struct {
		name    string
		env     corev1.EnvVar
		setting vllm.Setting
		reason  string
	}{
		{
			name:    "port is not an integer",
			env:     corev1.EnvVar{Name: "VLLM_PORT", Value: "eight-thousand"},
			setting: vllm.Setting("VLLM_PORT"),
			reason:  "not an integer",
		},
		{
			name:    "boolean is not a boolean",
			env:     corev1.EnvVar{Name: "VLLM_USE_MODELSCOPE", Value: "maybe"},
			setting: vllm.Setting("VLLM_USE_MODELSCOPE"),
			reason:  "not a boolean",
		},
		{
			name:    "stats interval is not positive",
			env:     corev1.EnvVar{Name: "VLLM_LOG_STATS_INTERVAL", Value: "0"},
			setting: vllm.Setting("VLLM_LOG_STATS_INTERVAL"),
			reason:  "positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := vllm.Resolve(vllm.Invocation{Container: corev1.Container{
				Name:    "modelserver",
				Image:   "vllm/vllm-openai:v0.33.0",
				Command: []string{"vllm", "serve", "Qwen/Qwen3-8B"},
				Env:     []corev1.EnvVar{tt.env},
			}})
			requireRejection(t, err, tt.setting, tt.reason)
		})
	}
}

// containsSecret reports whether any field of the resolved configuration holds
// the given secret value.
func containsSecret(cfg *vllm.Config, secret string) bool {
	values := []*string{
		cfg.Model.Name, cfg.Model.Revision, cfg.Tokenizer.Name, cfg.Server.Host,
		cfg.Server.UDS,
	}
	for _, v := range values {
		if v != nil && strings.Contains(*v, secret) {
			return true
		}
	}
	for _, s := range []string{
		cfg.Auth.APIKeySource,
		cfg.Auth.HuggingFaceTokenEnvVar,
		cfg.Environment.HuggingFaceTokenEnvVar,
	} {
		if strings.Contains(s, secret) {
			return true
		}
	}
	if cfg.TLS != nil {
		for _, v := range []*string{cfg.TLS.KeyFile, cfg.TLS.CertFile, cfg.TLS.CACerts} {
			if v != nil && strings.Contains(*v, secret) {
				return true
			}
		}
	}
	return false
}
