package spike_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/opendatahub-io/vllm-vcr-operator/test/spike"
)

// TestRunArgsKeepTheProcessArgumentTail asserts the container invocation is
// faithful to the process being started. The frontend is started as
//
//	vcr-supervisor -- vllm serve <model> --host 0.0.0.0 --port 8000
//
// and re-splitting or re-quoting that tail would change the process the spike
// claims to have exercised.
func TestRunArgsKeepTheProcessArgumentTail(t *testing.T) {
	runner := spike.Runner{Engine: "podman", Bin: "/usr/bin/podman"}
	container := spike.Container{
		Name:  "vcr-spike-frontend",
		Image: "registry.example/vllm-vcr-frontend:v0.33.0-cpu",
		Args:  []string{"vcr-supervisor", "--", "vllm", "serve", "Qwen/Qwen3-0.6B", "--default-chat-template-kwargs", `{"enable_thinking": false}`},
		Env:   []string{"VCR_HANDSHAKE_ADDRESS=tcp://127.0.0.1:29550"},
		Mounts: []spike.Mount{
			{Source: "/tmp/profile", Target: "/var/run/vcr/profile", ReadOnly: true},
		},
		Network: "host",
	}

	args := runner.RunArgs(container)

	if args[0] != "/usr/bin/podman" || args[1] != "run" {
		t.Fatalf("argv does not start with the engine's run subcommand: %v", args[:2])
	}
	if !slices.Contains(args, "--name") || !slices.Contains(args, container.Name) {
		t.Errorf("argv does not name the container %q: %v", container.Name, args)
	}
	if !slices.Contains(args, "--network") || !slices.Contains(args, "host") {
		t.Errorf("argv does not share the host network namespace: %v", args)
	}
	if !slices.Contains(args, "VCR_HANDSHAKE_ADDRESS=tcp://127.0.0.1:29550") {
		t.Errorf("argv does not forward the environment: %v", args)
	}
	if !slices.Contains(args, "/tmp/profile:/var/run/vcr/profile:ro") {
		t.Errorf("argv does not mount the profile directory read-only: %v", args)
	}

	imageIndex := slices.Index(args, container.Image)
	if imageIndex < 0 {
		t.Fatalf("argv does not reference the image %q: %v", container.Image, args)
	}
	tail := args[imageIndex+1:]
	if !slices.Equal(tail, container.Args) {
		t.Fatalf("argument tail after the image = %q, want it passed through unchanged: %q", tail, container.Args)
	}
	if strings.Join(tail, " ") != strings.Join(container.Args, " ") {
		t.Fatal("argument tail was re-split")
	}
}

// TestSignalAndRemoveArgsUseEngineSyntax asserts lifecycle control is expressed
// as engine commands, because killing the engine client process does not stop
// the container.
func TestSignalAndRemoveArgsUseEngineSyntax(t *testing.T) {
	runner := spike.Runner{Engine: "docker", Bin: "/usr/bin/docker"}

	if got := runner.SignalArgs("vcr-spike-backend", "TERM"); !slices.Equal(got, []string{"/usr/bin/docker", "kill", "--signal", "TERM", "vcr-spike-backend"}) {
		t.Errorf("SignalArgs = %v", got)
	}
	if got := runner.RemoveArgs("vcr-spike-backend"); !slices.Equal(got, []string{"/usr/bin/docker", "rm", "--force", "--volumes", "vcr-spike-backend"}) {
		t.Errorf("RemoveArgs = %v", got)
	}
}

// TestEnvironmentRunnerUsesTheResolvedEngine asserts the harness runs the
// images with the engine the environment resolved.
func TestEnvironmentRunnerUsesTheResolvedEngine(t *testing.T) {
	env := spike.Environment{Engine: "podman", FrontendImage: "frontend:1", BackendImage: "backend:1"}

	runner := env.Runner()
	if runner.Engine != "podman" {
		t.Errorf("Engine = %q", runner.Engine)
	}
	if !strings.HasSuffix(runner.Bin, "podman") {
		t.Errorf("Bin = %q, want the resolved engine binary", runner.Bin)
	}
}
