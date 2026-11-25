package vllm_test

import (
	"testing"

	"github.com/neuralmagic/vllm-vcr-operator/internal/vllm"
)

// The supported invocation forms are the ones the workloads in the reference
// deployments use: an exec-form "vllm serve", the Python OpenAI API server
// module, and images whose default entrypoint is that module.
func TestResolveRecognizesInvocationForms(t *testing.T) {
	tests := []struct {
		name        string
		in          vllm.Invocation
		wantKind    vllm.EntrypointKind
		wantProgram string
	}{
		{
			name:        "vllm serve",
			in:          vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0", []string{"vllm", "serve"}, "Qwen/Qwen3-8B")},
			wantKind:    vllm.EntrypointCommand,
			wantProgram: "vllm serve",
		},
		{
			name:        "absolute path to vllm",
			in:          vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0", []string{"/usr/local/bin/vllm", "serve"}, "Qwen/Qwen3-8B")},
			wantKind:    vllm.EntrypointCommand,
			wantProgram: "vllm serve",
		},
		{
			name:        "python -m vllm.entrypoints.openai.api_server",
			in:          vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0", []string{"python", "-m", "vllm.entrypoints.openai.api_server"}, "Qwen/Qwen3-8B")},
			wantKind:    vllm.EntrypointPythonModule,
			wantProgram: "python -m vllm.entrypoints.openai.api_server",
		},
		{
			name:        "python3 with absolute interpreter path",
			in:          vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0", []string{"/usr/bin/python3", "-m", "vllm.entrypoints.openai.api_server"}, "--model=Qwen/Qwen3-8B")},
			wantKind:    vllm.EntrypointPythonModule,
			wantProgram: "python3 -m vllm.entrypoints.openai.api_server",
		},
		{
			name:        "python -m vllm.entrypoints.cli.main serve",
			in:          vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0", []string{"python", "-m", "vllm.entrypoints.cli.main", "serve"}, "Qwen/Qwen3-8B")},
			wantKind:    vllm.EntrypointPythonModule,
			wantProgram: "python -m vllm.entrypoints.cli.main serve",
		},
		{
			// vLLM v0.33.0's docker/Dockerfile ends with ENTRYPOINT ["vllm", "serve"].
			name:        "image default entrypoint with pod args",
			in:          vllm.Invocation{Container: container("docker.io/vllm/vllm-openai:v0.33.0", nil, "Qwen/Qwen3-8B", "--port", "8001")},
			wantKind:    vllm.EntrypointImageDefault,
			wantProgram: "vllm serve",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := resolve(t, tt.in)
			if cfg.Entrypoint.Kind != tt.wantKind {
				t.Fatalf("entrypoint kind = %q, want %q", cfg.Entrypoint.Kind, tt.wantKind)
			}
			if cfg.Entrypoint.Program != tt.wantProgram {
				t.Fatalf("entrypoint program = %q, want %q", cfg.Entrypoint.Program, tt.wantProgram)
			}
			if cfg.Version != "0.33.0" {
				t.Fatalf("resolved vLLM version = %q, want the operator's fixed release 0.33.0", cfg.Version)
			}
		})
	}
}

// Image-default resolution is knowledge, not a guess: only images whose default
// entrypoint is recorded may be resolved, and the record is pinned to a vLLM
// release.
func TestResolveImageDefaultEntrypointIsVersioned(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0", nil, "Qwen/Qwen3-8B")})

	if cfg.Entrypoint.Image != "vllm/vllm-openai:v0.33.0" {
		t.Fatalf("entrypoint image = %q, want the container image", cfg.Entrypoint.Image)
	}
	if cfg.Entrypoint.TableVersion != "0.33.0" {
		t.Fatalf("entrypoint table version = %q, want 0.33.0", cfg.Entrypoint.TableVersion)
	}
	wantString(t, "model", cfg.Model.Name, "Qwen/Qwen3-8B")
}

func TestResolveRejectsUnsupportedInvocationForms(t *testing.T) {
	tests := []struct {
		name    string
		in      vllm.Invocation
		setting vllm.Setting
		reason  string
	}{
		{
			name:    "shell wrapper",
			in:      vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0", []string{"/bin/sh", "-c"}, "exec vllm serve Qwen/Qwen3-8B")},
			setting: vllm.Setting("command"),
			reason:  "shell",
		},
		{
			name:    "bash wrapper with -c",
			in:      vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0", []string{"bash", "-c"}, "vllm serve Qwen/Qwen3-8B")},
			setting: vllm.Setting("command"),
			reason:  "shell",
		},
		{
			name:    "wrapper script",
			in:      vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0", []string{"/opt/entrypoint.sh"}, "Qwen/Qwen3-8B")},
			setting: vllm.Setting("command"),
			reason:  "not a supported vLLM entrypoint",
		},
		{
			name:    "unknown executable",
			in:      vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0", []string{"tini", "--"}, "vllm", "serve")},
			setting: vllm.Setting("command"),
			reason:  "not a supported vLLM entrypoint",
		},
		{
			name:    "vllm subcommand other than serve",
			in:      vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0", []string{"vllm", "bench", "serve"})},
			setting: vllm.Setting("command"),
			reason:  "subcommand",
		},
		{
			name:    "vllm without a subcommand",
			in:      vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0", []string{"vllm"})},
			setting: vllm.Setting("command"),
			reason:  "subcommand",
		},
		{
			name:    "unknown python module",
			in:      vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0", []string{"python", "-m", "my.server"}, "Qwen/Qwen3-8B")},
			setting: vllm.Setting("command"),
			reason:  "module",
		},
		{
			name:    "image with unknown default entrypoint",
			in:      vllm.Invocation{Container: container("registry.example.com/team/serving:1.2.3", nil, "Qwen/Qwen3-8B")},
			setting: vllm.Setting("image"),
			reason:  "default entrypoint",
		},
		{
			name:    "default entrypoint without a model",
			in:      vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0", nil)},
			setting: vllm.SettingModel,
			reason:  "model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := vllm.Resolve(tt.in)
			requireRejection(t, err, tt.setting, tt.reason)
		})
	}
}

// Kubernetes appends container args to the image entrypoint. Repeating the
// command there would make the server treat a command name as the model.
func TestResolveRejectsCommandRepeatedInArgs(t *testing.T) {
	for _, args := range [][]string{
		{"vllm", "serve", "Qwen/Qwen3-8B"},
		{"python3", "-m", "vllm.entrypoints.openai.api_server", "Qwen/Qwen3-8B"},
		{"/bin/sh", "-c", "vllm serve Qwen/Qwen3-8B"},
		{"serve", "Qwen/Qwen3-8B"},
	} {
		t.Run(args[0], func(t *testing.T) {
			_, err := vllm.Resolve(vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0", nil, args...)})
			requireRejection(t, err, vllm.Setting("command"), "must not repeat the command")
		})
	}
}
