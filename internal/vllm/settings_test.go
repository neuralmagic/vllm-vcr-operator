package vllm_test

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/vllm"
)

// vLLM 0.33.0 accepts human-readable sizes for --max-model-len: decimal
// multipliers are lowercase ("1k" is 1000), binary multipliers are uppercase
// ("1K" is 1024), and "-1"/"auto" mean "use the model maximum".
func TestResolveHumanReadableMaxModelLen(t *testing.T) {
	tests := []struct {
		value string
		want  int
	}{
		{value: "4096", want: 4096},
		{value: "1k", want: 1000},
		{value: "1K", want: 1024},
		{value: "25.6k", want: 25600},
		{value: "1M", want: 1 << 20},
		{value: "131072", want: 131072},
		{value: "auto", want: -1},
		{value: "-1", want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
				[]string{"vllm", "serve", "Qwen/Qwen3-8B"}, "--max-model-len="+tt.value)})
			wantInt(t, "max-model-len", cfg.Limits.MaxModelLen, tt.want)
		})
	}

	// "1.5K" is rejected because vLLM 0.33.0 does not allow decimals with binary
	// multipliers; 0 and negative sizes are not context lengths.
	for _, invalid := range []string{"0", "-4096", "1.5K"} {
		t.Run("rejects "+invalid, func(t *testing.T) {
			_, err := vllm.Resolve(vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
				[]string{"vllm", "serve", "Qwen/Qwen3-8B"}, "--max-model-len="+invalid)})
			if err == nil {
				t.Fatalf("Resolve() accepted --max-model-len=%s", invalid)
			}
			if _, ok := vllm.Rejected(err); !ok {
				t.Fatalf("error is not a structured rejection: %v", err)
			}
		})
	}
}

func TestResolveStructuredOutputsConfig(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve", "Qwen/Qwen3-8B"},
		`--structured-outputs-config={"backend":"xgrammar","disable_any_whitespace":true,"enable_in_reasoning":true}`)})

	if cfg.StructuredOutputs == nil {
		t.Fatal("structured-outputs-config was not resolved")
	}
	wantString(t, "backend", cfg.StructuredOutputs.Backend, "xgrammar")
	wantBool(t, "disable any whitespace", cfg.StructuredOutputs.DisableAnyWhitespace, true)
	wantBool(t, "enable in reasoning", cfg.StructuredOutputs.EnableInReasoning, true)
	wantOrigin(t, cfg, vllm.SettingStructuredOutputs, vllm.OriginCLI)
}

func TestResolveRejectsMalformedJSONSettings(t *testing.T) {
	for _, arg := range []string{
		`--structured-outputs-config={"backend":"xgrammar","enable_in_reasoning":"yes"}`,
		`--speculative-config={"num_speculative_tokens":"four"}`,
		`--speculative-config={"num_speculative_tokens":0}`,
		`--kv-transfer-config={"kv_connector":42}`,
	} {
		t.Run(arg, func(t *testing.T) {
			_, err := vllm.Resolve(vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
				[]string{"vllm", "serve", "Qwen/Qwen3-8B"}, arg)})
			if err == nil {
				t.Fatalf("Resolve() accepted %s", arg)
			}
			rs, ok := vllm.Rejected(err)
			if !ok {
				t.Fatalf("error is not a structured rejection: %v", err)
			}
			if rs[0].Reason == "" || rs[0].Value == "" {
				t.Fatalf("rejection does not name the value and the reason: %+v", rs[0])
			}
		})
	}
}

// An emptyDir-backed configuration file cannot be inspected: the Pod spec names
// the volume kind but the content only exists at runtime.
func TestResolveRejectsRuntimeOnlyVolumeContent(t *testing.T) {
	inv := vllm.Invocation{
		Container: corev1.Container{
			Name: "modelserver", Image: "vllm/vllm-openai:v0.33.0",
			Command:      []string{"vllm", "serve", "Qwen/Qwen3-8B"},
			Args:         []string{"--config=/config/serve.yaml"},
			VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/config"}},
		},
		Volumes: map[string]corev1.Volume{
			"config": {Name: "config", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
	}

	_, err := vllm.Resolve(inv)
	requireRejection(t, err, vllm.SettingConfigFile, "emptyDir volume")
}

// The Rust frontend switch is read as an integer by vLLM 0.33.0, not as a
// general boolean.
func TestResolveRustFrontendEnvironment(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: corev1.Container{
		Name:    "modelserver",
		Image:   "vllm/vllm-openai:v0.33.0",
		Command: []string{"vllm", "serve", "Qwen/Qwen3-8B"},
		Env:     []corev1.EnvVar{{Name: "VLLM_USE_RUST_FRONTEND", Value: "1"}},
	}})
	wantBool(t, "rust frontend", cfg.Environment.RustFrontend, true)

	_, err := vllm.Resolve(vllm.Invocation{Container: corev1.Container{
		Name:    "modelserver",
		Image:   "vllm/vllm-openai:v0.33.0",
		Command: []string{"vllm", "serve", "Qwen/Qwen3-8B"},
		Env:     []corev1.EnvVar{{Name: "VLLM_USE_RUST_FRONTEND", Value: "true"}},
	}})
	requireRejection(t, err, vllm.Setting("VLLM_USE_RUST_FRONTEND"), "0 or 1")
}

// Rejected recognises a bare Rejection as well as a rejection set, so callers can
// wrap errors without losing structure.
func TestRejectedAcceptsSingleRejection(t *testing.T) {
	rs, ok := vllm.Rejected(vllm.Rejection{Setting: vllm.SettingPort, Reason: "test"})
	if !ok || len(rs) != 1 || rs[0].Setting != vllm.SettingPort {
		t.Fatalf("Rejected() = %v, %v; want the single rejection", rs, ok)
	}
	if _, ok := vllm.Rejected(errors.New("unrelated")); ok {
		t.Fatal("Rejected() claimed an unrelated error is a rejection")
	}
	if _, ok := vllm.Rejected(nil); ok {
		t.Fatal("Rejected(nil) claimed a rejection")
	}
}

func TestRejectionMessageShape(t *testing.T) {
	withValue := vllm.Rejection{Setting: vllm.SettingPort, Value: "70000", Reason: "not a valid TCP port"}
	if got, want := withValue.Error(), "vllm: --port=70000: not a valid TCP port"; got != want {
		t.Fatalf("rejection message = %q, want %q", got, want)
	}
	withoutValue := vllm.Rejection{Setting: vllm.SettingModel, Reason: "no model"}
	if got, want := withoutValue.Error(), "vllm: --model: no model"; got != want {
		t.Fatalf("rejection message = %q, want %q", got, want)
	}
}
