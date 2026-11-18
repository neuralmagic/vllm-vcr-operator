package spike_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/opendatahub-io/vllm-vcr-operator/test/spike"
)

// env builds a getenv function from a literal map, so a case states exactly the
// environment it describes.
func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

// lookPath returns the first candidate it is configured to find, so engine
// detection never depends on the machine running the test.
func lookPath(found ...string) func(string) (string, error) {
	return func(name string) (string, error) {
		for _, candidate := range found {
			if candidate == name {
				return "/usr/bin/" + name, nil
			}
		}
		return "", errNotFound{name: name}
	}
}

type errNotFound struct{ name string }

func (e errNotFound) Error() string { return e.name + ": executable file not found in $PATH" }

// configured is the smallest environment the harnesses accept: both runtime
// images, a profile source, and a container engine.
func configured() map[string]string {
	return map[string]string{
		spike.EnvFrontendImage: "registry.example/vllm-vcr-frontend:v0.33.0-cpu",
		spike.EnvBackendImage:  "registry.example/vllm-vcr-backend:v0.33.0",
		spike.EnvProfileDir:    "/var/lib/vcr/profiles/qwen3-0.6b-h200-tp1-bf16",
	}
}

// TestFromEnvNamesEveryUnmetRequirement asserts the gate reports the whole
// missing configuration at once. A spike that silently ran with a partial
// environment would produce evidence for a layout it never exercised.
func TestFromEnvNamesEveryUnmetRequirement(t *testing.T) {
	_, err := spike.FromEnv(env(nil), lookPath())
	if err == nil {
		t.Fatal("FromEnv accepted an empty environment")
	}

	var missing *spike.MissingError
	if !errors.As(err, &missing) {
		t.Fatalf("FromEnv error is %T, want *spike.MissingError so callers can distinguish a skipped spike", err)
	}
	message := err.Error()
	for _, want := range []string{spike.EnvFrontendImage, spike.EnvBackendImage, spike.EnvProfileImage, spike.EnvProfileDir, "podman"} {
		if !strings.Contains(message, want) {
			t.Errorf("missing-environment error does not mention %q:\n%s", want, message)
		}
	}
}

// TestFromEnvResolvesConfiguredEnvironment asserts the resolved configuration a
// harness needs: images, profile source, container engine, and the model the
// frontend serves.
func TestFromEnvResolvesConfiguredEnvironment(t *testing.T) {
	got, err := spike.FromEnv(env(configured()), lookPath("podman", "docker"))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}

	if got.FrontendImage != "registry.example/vllm-vcr-frontend:v0.33.0-cpu" {
		t.Errorf("FrontendImage = %q", got.FrontendImage)
	}
	if got.BackendImage != "registry.example/vllm-vcr-backend:v0.33.0" {
		t.Errorf("BackendImage = %q", got.BackendImage)
	}
	if got.ProfileDir != "/var/lib/vcr/profiles/qwen3-0.6b-h200-tp1-bf16" {
		t.Errorf("ProfileDir = %q", got.ProfileDir)
	}
	if got.ProfileImage != "" {
		t.Errorf("ProfileImage = %q, want empty when only a profile directory is given", got.ProfileImage)
	}
	if got.Engine != "podman" {
		t.Errorf("Engine = %q, want the first available engine (podman)", got.Engine)
	}
	if got.Model != spike.DefaultModel {
		t.Errorf("Model = %q, want the documented default %q", got.Model, spike.DefaultModel)
	}
	if got.Required {
		t.Error("Required is true without VCR_SPIKE_REQUIRED")
	}
}

// TestFromEnvAcceptsProfileImage asserts a profile image is an alternative
// profile source, because then the provisioner init container is exercised too.
func TestFromEnvAcceptsProfileImage(t *testing.T) {
	values := configured()
	delete(values, spike.EnvProfileDir)
	values[spike.EnvProfileImage] = "registry.example/vllm-vcr-profiles:qwen3-0.6b-h200-tp1-bf16"

	got, err := spike.FromEnv(env(values), lookPath("podman"))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if got.ProfileImage != values[spike.EnvProfileImage] {
		t.Errorf("ProfileImage = %q", got.ProfileImage)
	}
	if got.ProfileDir != "" {
		t.Errorf("ProfileDir = %q, want empty when only an image is given", got.ProfileDir)
	}
}

// TestFromEnvHonorsExplicitRuntime asserts VCR_SPIKE_RUNTIME selects the engine
// and that an unavailable engine is reported, not silently replaced.
func TestFromEnvHonorsExplicitRuntime(t *testing.T) {
	values := configured()
	values[spike.EnvRuntime] = "docker"

	got, err := spike.FromEnv(env(values), lookPath("podman", "docker"))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if got.Engine != "docker" {
		t.Errorf("Engine = %q, want the explicitly requested docker", got.Engine)
	}

	values[spike.EnvRuntime] = "nerdctl"
	if _, err := spike.FromEnv(env(values), lookPath("podman", "docker")); err == nil {
		t.Fatal("FromEnv accepted an unavailable VCR_SPIKE_RUNTIME=nerdctl")
	} else if !strings.Contains(err.Error(), spike.EnvRuntime) {
		t.Errorf("error does not name %s:\n%s", spike.EnvRuntime, err)
	}
}

// TestFromEnvReadsModelOverride asserts the served model can be pointed at a
// profile's model without editing the harness.
func TestFromEnvReadsModelOverride(t *testing.T) {
	values := configured()
	values[spike.EnvModel] = "Qwen/Qwen3-8B"

	got, err := spike.FromEnv(env(values), lookPath("podman"))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if got.Model != "Qwen/Qwen3-8B" {
		t.Errorf("Model = %q", got.Model)
	}
}

// TestRequiredParsesExplicitRequest asserts only an explicit true value makes a
// missing environment fatal.
func TestRequiredParsesExplicitRequest(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"no", false},
		{"garbage", false},
	} {
		got := spike.Required(env(map[string]string{spike.EnvRequired: tc.value}))
		if got != tc.want {
			t.Errorf("Required(VCR_SPIKE_REQUIRED=%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// TestGateSkipsUnlessRequired asserts the two documented gate behaviors: an
// incomplete environment skips the spike, and an explicitly requested spike
// fails instead of skipping.
func TestGateSkipsUnlessRequired(t *testing.T) {
	skipped := &fakeTB{TB: t}
	gate(skipped, env(nil), lookPath())
	if !skipped.skipped {
		t.Error("Gate did not skip an unconfigured, unrequested spike")
	}
	if skipped.failed {
		t.Error("Gate failed an unrequested spike")
	}

	required := &fakeTB{TB: t}
	values := map[string]string{spike.EnvRequired: "1"}
	gate(required, env(values), lookPath())
	if required.skipped {
		t.Error("Gate skipped a spike that VCR_SPIKE_REQUIRED=1 explicitly requested")
	}
	if !required.failed {
		t.Error("Gate did not fail a requested spike with a missing environment")
	}
}

// TestGateReturnsResolvedEnvironment asserts a configured spike receives the
// environment it runs against.
func TestGateReturnsResolvedEnvironment(t *testing.T) {
	tb := &fakeTB{TB: t}
	got := gate(tb, env(configured()), lookPath("podman"))
	if tb.skipped || tb.failed {
		t.Fatalf("Gate skipped/failed a configured spike (skipped=%v failed=%v)", tb.skipped, tb.failed)
	}
	if got.Engine != "podman" || got.FrontendImage == "" {
		t.Errorf("Gate returned %+v", got)
	}
}

// fakeTB records the decisions Gate takes instead of aborting the test binary.
type fakeTB struct {
	testing.TB
	skipped bool
	failed  bool
	log     []string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Skip(args ...any) {
	f.skipped = true
	f.log = append(f.log, "skip")
}

func (f *fakeTB) Skipf(format string, args ...any) {
	f.skipped = true
	f.log = append(f.log, format)
}

func (f *fakeTB) Fatal(args ...any) {
	f.failed = true
	f.log = append(f.log, fmt.Sprint(args...))
	panic(fatalSignal{})
}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.failed = true
	f.log = append(f.log, fmt.Sprintf(format, args...))
	panic(fatalSignal{})
}

// fatalSignal is what fakeTB panics with, so a fatal decision stops the test
// body the way testing.T.Fatal stops a test.
type fatalSignal struct{}

// gate runs Gate and swallows fakeTB's fatal signal.
func gate(tb *fakeTB, getenv func(string) string, lookPath func(string) (string, error)) (resolved spike.Environment) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(fatalSignal); !ok {
				panic(r)
			}
		}
	}()
	return spike.Gate(tb, getenv, lookPath)
}
