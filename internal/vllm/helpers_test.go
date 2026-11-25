package vllm_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/neuralmagic/vllm-vcr-operator/internal/vllm"
)

// container builds a workload container the way a Deployment manifest does:
// an image reference plus an exec-form command and args.
func container(image string, command []string, args ...string) corev1.Container {
	return corev1.Container{Name: "modelserver", Image: image, Command: command, Args: args}
}

// resolve resolves an invocation that must be accepted.
func resolve(t *testing.T, in vllm.Invocation) *vllm.Config {
	t.Helper()
	cfg, err := vllm.Resolve(in)
	if err != nil {
		t.Fatalf("Resolve() returned an error for an accepted invocation: %v", err)
	}
	if cfg == nil {
		t.Fatal("Resolve() returned a nil Config without an error")
	}
	return cfg
}

// requireRejection asserts that err is a rejection for setting whose reason
// mentions want.
func requireRejection(t *testing.T, err error, setting vllm.Setting, want string) vllm.Rejection {
	t.Helper()
	if err == nil {
		t.Fatalf("Resolve() accepted an invocation that must be rejected for %s", setting)
	}
	rs, ok := vllm.Rejected(err)
	if !ok {
		t.Fatalf("Resolve() error is not a structured rejection: %v", err)
	}
	for _, r := range rs {
		if r.Setting == string(setting) && strings.Contains(r.Reason, want) {
			return r
		}
	}
	t.Fatalf("no rejection for %s mentioning %q; got %v", setting, want, rs)
	return vllm.Rejection{}
}

func wantString(t *testing.T, what string, got *string, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is unset, want %q", what, want)
	}
	if *got != want {
		t.Fatalf("%s = %q, want %q", what, *got, want)
	}
}

func wantInt(t *testing.T, what string, got *int, want int) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is unset, want %d", what, want)
	}
	if *got != want {
		t.Fatalf("%s = %d, want %d", what, *got, want)
	}
}

func wantBool(t *testing.T, what string, got *bool, want bool) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is unset, want %v", what, want)
	}
	if *got != want {
		t.Fatalf("%s = %v, want %v", what, *got, want)
	}
}

func wantUnsetString(t *testing.T, what string, got *string) {
	t.Helper()
	if got != nil {
		t.Fatalf("%s = %q, want unset", what, *got)
	}
}

func wantOrigin(t *testing.T, cfg *vllm.Config, setting vllm.Setting, want vllm.Origin) {
	t.Helper()
	if got := cfg.Origin(setting); got != want {
		t.Fatalf("origin of %s = %q, want %q", setting, got, want)
	}
}

func wantFloat(t *testing.T, what string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is unset, want %v", what, want)
	}
	if *got != want {
		t.Fatalf("%s = %v, want %v", what, *got, want)
	}
}
