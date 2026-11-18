package vllm_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/vllm"
)

const chatTemplate = `{% for message in messages %}{{ message["role"] }}: {{ message["content"] }}{% endfor %}`

// chatTemplateSHA256 is the SHA-256 of chatTemplate, computed outside this
// package, so the reported digest is checked against a known-good value.
const chatTemplateSHA256 = "d721172f74caaabde52b5ccc6e0f15fc061b2d4a3c0c354a77282141f8d8225a"

func TestResolveInlineChatTemplate(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve", "Qwen/Qwen3-8B"},
		"--chat-template", chatTemplate)})

	if !cfg.ChatTemplate.Set() {
		t.Fatal("the inline chat template was not resolved")
	}
	if !cfg.ChatTemplate.Inline {
		t.Fatal("the inline chat template is reported as a file")
	}
	if cfg.ChatTemplate.Template != chatTemplate {
		t.Fatalf("template = %q, want the inline value", cfg.ChatTemplate.Template)
	}
	if cfg.ChatTemplate.Path != "" {
		t.Fatalf("template path = %q, want empty for an inline template", cfg.ChatTemplate.Path)
	}
	if cfg.ChatTemplate.SHA256 != chatTemplateSHA256 {
		t.Fatalf("template digest = %q, want %q", cfg.ChatTemplate.SHA256, chatTemplateSHA256)
	}
	wantOrigin(t, cfg, vllm.SettingChatTemplate, vllm.OriginCLI)
}

// A chat template file must come from the Pod spec: the operator does not read
// the filesystem and does not execute the container.
func TestResolveChatTemplateFromConfigMapVolume(t *testing.T) {
	const path = "/etc/vllm/templates/qwen3.jinja"
	inv := vllm.Invocation{
		Container: corev1.Container{
			Name:         "modelserver",
			Image:        "vllm/vllm-openai:v0.33.0",
			Command:      []string{"vllm", "serve", "Qwen/Qwen3-8B"},
			Args:         []string{"--chat-template", path},
			VolumeMounts: []corev1.VolumeMount{{Name: "templates", MountPath: "/etc/vllm/templates"}},
		},
		Volumes: map[string]corev1.Volume{
			"templates": {
				Name: "templates",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "templates"}},
				},
			},
		},
		Files: map[string][]byte{path: []byte(chatTemplate)},
	}

	cfg := resolve(t, inv)

	if cfg.ChatTemplate.Inline {
		t.Fatal("a mounted chat template is reported as inline")
	}
	if cfg.ChatTemplate.Path != path {
		t.Fatalf("template path = %q, want %q", cfg.ChatTemplate.Path, path)
	}
	if cfg.ChatTemplate.Template != chatTemplate {
		t.Fatalf("template content = %q, want the ConfigMap content", cfg.ChatTemplate.Template)
	}
	if cfg.ChatTemplate.SHA256 != chatTemplateSHA256 {
		t.Fatalf("template digest = %q, want the digest of the mounted file %q", cfg.ChatTemplate.SHA256, chatTemplateSHA256)
	}
}

func TestResolveRejectsUnresolvableChatTemplate(t *testing.T) {
	tests := []struct {
		name   string
		inv    vllm.Invocation
		reason string
	}{
		{
			name: "path is not mounted",
			inv: vllm.Invocation{Container: corev1.Container{
				Name: "modelserver", Image: "vllm/vllm-openai:v0.33.0",
				Command: []string{"vllm", "serve", "Qwen/Qwen3-8B"},
				Args:    []string{"--chat-template", "/etc/vllm/chat.jinja"},
			}},
			reason: "not backed by a volume mount",
		},
		{
			name: "volume content is not available",
			inv: vllm.Invocation{
				Container: corev1.Container{
					Name: "modelserver", Image: "vllm/vllm-openai:v0.33.0",
					Command:      []string{"vllm", "serve", "Qwen/Qwen3-8B"},
					Args:         []string{"--chat-template", "/templates/chat.jinja"},
					VolumeMounts: []corev1.VolumeMount{{Name: "templates", MountPath: "/templates"}},
				},
				Volumes: map[string]corev1.Volume{
					"templates": {
						Name: "templates",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{SecretName: "templates"},
						},
					},
				},
			},
			reason: "Secret volume",
		},
		{
			name: "empty template",
			inv: vllm.Invocation{Container: corev1.Container{
				Name: "modelserver", Image: "vllm/vllm-openai:v0.33.0",
				Command: []string{"vllm", "serve", "Qwen/Qwen3-8B"},
				Args:    []string{"--chat-template="},
			}},
			reason: "empty chat template",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := vllm.Resolve(tt.inv)
			requireRejection(t, err, vllm.SettingChatTemplate, tt.reason)
		})
	}
}

// vLLM 0.33.0 also accepts JSON objects as dotted arguments
// ("--json-arg.key value"), which is how llm-d fixtures configure KV events.
func TestResolveDottedJSONArguments(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve", "Qwen/Qwen3-8B"},
		"--kv-events-config.publisher=zmq",
		"--kv-events-config.endpoint", "tcp://*:5556",
		"--kv-events-config.topic=kv@10.0.0.7:8000@Qwen/Qwen3-8B",
		"--kv-events-config.enable_kv_cache_events=true",
		"--kv-events-config.buffer_steps=500",
		"--compilation-config.pass_config.fuse_allreduce_rms=true",
	)})

	if cfg.KVEvents == nil {
		t.Fatal("dotted kv-events-config arguments were not resolved")
	}
	wantString(t, "publisher", cfg.KVEvents.Publisher, "zmq")
	wantString(t, "endpoint", cfg.KVEvents.Endpoint, "tcp://*:5556")
	wantString(t, "topic", cfg.KVEvents.Topic, "kv@10.0.0.7:8000@Qwen/Qwen3-8B")
	wantBool(t, "kv events enabled", cfg.KVEvents.Enable, true)
	wantInt(t, "buffer steps", cfg.KVEvents.BufferSteps, 500)

	passConfig, ok := cfg.Execution.CompilationConfig["pass_config"].(map[string]any)
	if !ok {
		t.Fatalf("nested dotted JSON argument was not resolved: %v", cfg.Execution.CompilationConfig)
	}
	if passConfig["fuse_allreduce_rms"] != true {
		t.Fatalf("pass_config.fuse_allreduce_rms = %v, want true", passConfig["fuse_allreduce_rms"])
	}
}

// The KV transfer configuration carries the prefill/decode role, which the policy
// layer must be able to see.
func TestResolveKVTransferRole(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve", "Qwen/Qwen3-8B"},
		`--kv-transfer-config={"kv_connector":"NixlConnector","kv_role":"kv_both","kv_connector_extra_config":{"num_threads":4}}`,
	)})

	if cfg.KVTransfer == nil {
		t.Fatal("kv-transfer-config was not resolved")
	}
	wantString(t, "kv connector", cfg.KVTransfer.Connector, "NixlConnector")
	wantString(t, "kv role", cfg.KVTransfer.Role, "kv_both")
	if cfg.KVTransfer.ExtraConfig == nil || cfg.KVTransfer.ExtraConfig["num_threads"] != float64(4) {
		t.Fatalf("kv connector extra config = %v, want num_threads 4", cfg.KVTransfer.ExtraConfig)
	}
	wantOrigin(t, cfg, vllm.SettingKVTransferConfig, vllm.OriginCLI)
}

// Served-model aliases are preserved verbatim: llm-d selects on the underlying
// model, but clients call the aliases.
func TestResolveServedModelAliases(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve", "RedHatAI/Qwen3-8B-quantized.w8a8"},
		"--served-model-name", "Qwen/Qwen3-8B", "qwen3-8b",
	)})

	if len(cfg.Model.ServedModelNames) != 2 {
		t.Fatalf("served model names = %v, want both aliases", cfg.Model.ServedModelNames)
	}
	if cfg.Model.ServedModelNames[0] != "Qwen/Qwen3-8B" || cfg.Model.ServedModelNames[1] != "qwen3-8b" {
		t.Fatalf("served model names = %v, want [Qwen/Qwen3-8B qwen3-8b]", cfg.Model.ServedModelNames)
	}
	wantString(t, "model", cfg.Model.Name, "RedHatAI/Qwen3-8B-quantized.w8a8")
}

// Short flags and the -O shorthand are part of the 0.33.0 interface.
func TestResolveShortFlagSpellings(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve", "Qwen/Qwen3-8B"},
		"-tp", "2", "-pp", "1", "-O3",
	)})

	wantInt(t, "tensor parallel size", cfg.Parallel.TensorParallelSize, 2)
	wantInt(t, "pipeline parallel size", cfg.Parallel.PipelineParallelSize, 1)
	wantInt(t, "optimization level", cfg.Execution.OptimizationLevel, 3)
	wantOrigin(t, cfg, vllm.SettingOptimizationLevel, vllm.OriginCLI)
}

func TestResolveTLSAuthenticationAndLoRA(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: corev1.Container{
		Name:    "modelserver",
		Image:   "vllm/vllm-openai:v0.33.0",
		Command: []string{"vllm", "serve", "Qwen/Qwen3-8B"},
		Args: []string{
			"--ssl-certfile=/certs/tls.crt",
			"--ssl-keyfile=/certs/tls.key",
			"--ssl-ca-certs=/certs/ca.crt",
			"--ssl-cert-reqs=2",
			"--api-key=vllm-local",
			"--enable-lora",
			"--max-loras=2",
			"--max-lora-rank=32",
		},
	}})

	if cfg.TLS == nil {
		t.Fatal("TLS settings were not resolved")
	}
	wantString(t, "cert file", cfg.TLS.CertFile, "/certs/tls.crt")
	wantString(t, "key file", cfg.TLS.KeyFile, "/certs/tls.key")
	wantString(t, "ca certs", cfg.TLS.CACerts, "/certs/ca.crt")
	wantInt(t, "cert reqs", cfg.TLS.CertReqs, 2)

	if !cfg.Auth.APIKeyRequired || cfg.Auth.APIKeySource != "--api-key" {
		t.Fatalf("API key mode = %+v, want the requirement recorded from --api-key", cfg.Auth)
	}
	wantBool(t, "lora enabled", cfg.LoRA.Enabled, true)
	wantInt(t, "max loras", cfg.LoRA.MaxLoras, 2)
	wantInt(t, "max lora rank", cfg.LoRA.MaxLoRARank, 32)
}

// Underscore spellings are normalised the way vLLM 0.33.0 normalises them, so a
// workload that uses underscores is not rejected as if the flag were unknown.
func TestResolveUnderscoreFlagSpellings(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve", "Qwen/Qwen3-8B"},
		"--max_num_seqs=64", "--enable_prefix_caching",
	)})

	wantInt(t, "max num seqs", cfg.Limits.MaxNumSeqs, 64)
	wantBool(t, "prefix caching", cfg.Cache.EnablePrefixCaching, true)
}

// A "--" separator ends flag parsing, as in vLLM 0.33.0.
func TestResolveDoubleDashSeparator(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve"}, "--port=8400", "--", "Qwen/Qwen3-8B")})

	wantString(t, "model", cfg.Model.Name, "Qwen/Qwen3-8B")
	wantInt(t, "port", cfg.Server.Port, 8400)
}
