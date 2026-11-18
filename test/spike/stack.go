package spike

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/profile"
)

// Stack is the process tree of one spike run: the profile provisioner, the
// frontend container (supervisor plus Python vLLM), and the simulator container,
// all sharing the host loopback so that the loopback handshake has the same
// meaning it has in the Pod layout.
type Stack struct {
	env    Environment
	runner Runner
	getenv func(string) string
	tb     testing.TB

	profileDir string
	runtimeDir string

	settings Settings
	metadata *profile.Metadata

	frontend *Handle
	backend  *Handle
}

// passThroughEnv lists host environment variables forwarded to the frontend
// container. They are Hugging Face cache/endpoint settings a real run needs to
// load a tokenizer offline or through a proxy.
//
// A value is forwarded and never logged: HF_TOKEN is a credential, and the
// evidence document must not contain one.
var passThroughEnv = []string{
	"HF_HOME",
	"HF_HUB_CACHE",
	"HF_HUB_OFFLINE",
	"HF_HUB_DISABLE_XET",
	"HF_ENDPOINT",
	"HF_TOKEN",
	"VLLM_ENGINE_READY_TIMEOUT_S",
}

// NewStack resolves the run's directories and container commands. It performs no
// I/O beyond creating the temporary directories the run uses.
func NewStack(tb testing.TB, env Environment) *Stack {
	tb.Helper()

	root := tb.TempDir()
	stack := &Stack{
		env:        env,
		runner:     env.Runner(),
		getenv:     os.Getenv,
		tb:         tb,
		profileDir: env.ProfileDir,
		runtimeDir: filepath.Join(root, "runtime"),
	}
	if stack.profileDir == "" {
		stack.profileDir = filepath.Join(root, "profile")
	}
	if err := os.MkdirAll(stack.runtimeDir, 0o777); err != nil {
		tb.Fatalf("create runtime directory: %v", err)
	}
	stack.settings = Settings{
		FrontendImage:         env.FrontendImage,
		BackendImage:          env.BackendImage,
		ProfileImage:          env.ProfileImage,
		ProfileDir:            stack.profileDir,
		Model:                 env.Model,
		InferencePort:         names.DefaultInferencePort,
		HandshakeAddress:      fmt.Sprintf("tcp://127.0.0.1:%d", names.HandshakePort),
		ControlPort:           names.ControlPort,
		KVEventsPort:          names.KVEventsPort,
		ReadyFile:             filepath.Join(stack.runtimeDir, "ready"),
		ShutdownTimeout:       DefaultShutdownTimeout,
		BackendLossGrace:      DefaultBackendLossGrace,
		ProvisionerEntrypoint: []string{defaultProvisionerEntrypoint},
	}
	return stack
}

// Settings returns the run's resolved parameters. The profile-derived fields are
// only complete after Provision and LoadProfile.
func (s *Stack) Settings() Settings { return s.settings }

// Metadata is the parsed profile the run serves, which is nil before
// LoadProfile.
func (s *Stack) Metadata() *profile.Metadata { return s.metadata }

// EnableKVEvents makes the simulator publish live KV-cache events. It must be
// called before StartBackend.
func (s *Stack) EnableKVEvents() { s.settings.EnableKVEvents = true }

// ProfileDir is the host directory holding the exported profile.
func (s *Stack) ProfileDir() string { return s.profileDir }

// Provision exports the profile the run serves.
//
// With a profile image, the image's provisioning entrypoint runs exactly as the
// operator runs it in the init container: the image command receives the profile
// mount path as its only argument. Without an image, an already-exported
// directory is used, and the run records that the provisioner was not exercised.
func (s *Stack) Provision(ctx context.Context) error {
	if err := os.MkdirAll(s.profileDir, 0o777); err != nil {
		return fmt.Errorf("spike: create profile directory %q: %w", s.profileDir, err)
	}

	if s.env.ProfileImage != "" {
		exitCode, err := s.runner.Run(ctx, Container{
			Name:  "vcr-spike-provisioner",
			Image: s.env.ProfileImage,
			Args:  s.settings.ProvisionerCommand(),
			Mounts: []Mount{
				{Source: s.profileDir, Target: names.ProfileMountPath},
			},
		})
		if err != nil {
			return err
		}
		if exitCode != 0 {
			return fmt.Errorf("spike: the profile provisioner exited %d: %s", exitCode, s.runnerDiagnostics("vcr-spike-provisioner"))
		}
	}

	return requireNonEmptyFile(filepath.Join(s.profileDir, names.ProfileMetadataFileName), "exported profile metadata")
}

// requireNonEmptyFile asserts a required profile document was exported.
func requireNonEmptyFile(path, description string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("spike: no %s at %q: %w", description, path, err)
	}
	if info.IsDir() || info.Size() == 0 {
		return fmt.Errorf("spike: %s at %q is not a non-empty file", description, path)
	}
	return nil
}

// LoadProfile parses the exported profile metadata and resolves the run's
// limits from it.
func (s *Stack) LoadProfile() error {
	raw, err := os.ReadFile(filepath.Join(s.profileDir, names.ProfileMetadataFileName))
	if err != nil {
		return fmt.Errorf("spike: read exported profile metadata: %w", err)
	}
	metadata, err := profile.Parse(raw)
	if err != nil {
		return fmt.Errorf("spike: the exported profile metadata is not valid: %w", err)
	}
	settings, err := DefaultSettings(s.env, metadata)
	if err != nil {
		return err
	}
	settings.ReadyFile = s.settings.ReadyFile
	s.settings = settings
	s.metadata = metadata

	if err := requireNonEmptyFile(settings.TracePath(), "exported trace the profile declares"); err != nil {
		return err
	}

	// The provisioner contract names the command that exported the data. When
	// the image declares one, the harness must have run that command, or the run
	// did not exercise the image's own provisioning.
	if len(metadata.Provisioning.Entrypoint) > 0 {
		declared := metadata.Provisioning.Entrypoint[0]
		if filepath.Base(declared) != filepath.Base(defaultProvisionerEntrypoint) {
			return fmt.Errorf(
				"spike: profile %q declares provisioning entrypoint %q, but this harness runs %q: extend the harness before claiming its provisioning was exercised",
				metadata.ID, declared, defaultProvisionerEntrypoint)
		}
	}
	return nil
}

// StartFrontend starts the frontend container in the background.
func (s *Stack) StartFrontend(ctx context.Context) error {
	handle, err := s.runner.Start(ctx, Container{
		Name:  "vcr-spike-frontend",
		Image: s.settings.FrontendImage,
		Args:  s.settings.FrontendCommand(),
		Env:   append(s.settings.FrontendEnv(), s.passThrough()...),
		Mounts: []Mount{
			{Source: s.profileDir, Target: names.ProfileMountPath, ReadOnly: true},
			{Source: s.runtimeDir, Target: ContainerRuntimeDir},
		},
		Network: "host",
	})
	if err != nil {
		return err
	}
	s.frontend = handle
	return nil
}

// StartBackend starts the simulator container in the background.
func (s *Stack) StartBackend(ctx context.Context) error {
	handle, err := s.runner.Start(ctx, Container{
		Name:  "vcr-spike-backend",
		Image: s.settings.BackendImage,
		Args:  s.settings.BackendCommand(),
		Env:   s.settings.BackendEnv(),
		Mounts: []Mount{
			{Source: s.profileDir, Target: names.ProfileMountPath, ReadOnly: true},
		},
		Network: "host",
	})
	if err != nil {
		return err
	}
	s.backend = handle
	return nil
}

// Frontend returns the frontend container handle, which is nil before
// StartFrontend.
func (s *Stack) Frontend() *Handle { return s.frontend }

// Backend returns the simulator container handle, which is nil before
// StartBackend.
func (s *Stack) Backend() *Handle { return s.backend }

// Inference is a client for the frontend's inference port.
func (s *Stack) Inference() Client { return Client{BaseURL: s.settings.InferenceURL()} }

// Control is a client for the simulator's control API.
func (s *Stack) Control() Client { return Client{BaseURL: s.settings.ControlURL()} }

// Readiness is a client for the supervisor's readiness endpoint.
func (s *Stack) Readiness() Client { return Client{BaseURL: "http://" + SupervisorHealthAddress} }

// Ready reports whether the run is ready, and what was observed.
//
// Readiness requires both of the signals the fixed layout exposes: the
// supervisor's readiness endpoint answering 200, and the readiness marker file.
// The handshake port is deliberately not probed: the frontend binds it, so a
// successful dial says nothing about engine registration.
func (s *Stack) Ready(ctx context.Context) (bool, string) {
	status, body, err := s.Readiness().Get(ctx, "/readyz")
	if err != nil {
		return false, "readiness endpoint unreachable: " + err.Error()
	}
	if status != 200 {
		return false, fmt.Sprintf("readyz returned %d: %s", status, snippet(body))
	}
	if _, err := os.Stat(s.settings.ReadyFile); err != nil {
		return false, "readyz is 200 but the readiness marker is missing: " + err.Error()
	}
	return true, "ready"
}

// AwaitReady waits for the run to become ready.
func (s *Stack) AwaitReady(ctx context.Context) (time.Duration, error) {
	elapsed, _, err := Await(ctx, Waiter{Interval: 250 * time.Millisecond, Timeout: 5 * time.Minute}, s.Ready)
	if err != nil {
		return elapsed, fmt.Errorf("%w\n%s", err, s.Diagnostics())
	}
	return elapsed, nil
}

// AwaitNotReady waits for readiness to be withdrawn.
func (s *Stack) AwaitNotReady(ctx context.Context) (time.Duration, error) {
	elapsed, _, err := Await(ctx, Waiter{Interval: 100 * time.Millisecond, Timeout: 2 * time.Minute},
		func(ctx context.Context) (bool, string) {
			ready, detail := s.Ready(ctx)
			return !ready, detail
		})
	if err != nil {
		return elapsed, fmt.Errorf("%w\n%s", err, s.Diagnostics())
	}
	return elapsed, nil
}

// Diagnostics is the failure diagnostic a spike records: the container output
// and the last readiness observation.
func (s *Stack) Diagnostics() string {
	var out strings.Builder
	out.WriteString("--- stack diagnostics ---\n")
	if ready, detail := s.Ready(context.Background()); !ready {
		fmt.Fprintf(&out, "readiness: %s\n", detail)
	}
	for _, part := range []struct {
		name   string
		handle *Handle
	}{{"frontend", s.frontend}, {"backend", s.backend}} {
		if part.handle == nil {
			continue
		}
		fmt.Fprintf(&out, "--- %s (%s) ---\n%s\n", part.name, part.handle.Name(), tail(part.handle.Logs(), 4000))
	}
	return out.String()
}

// Close tears the run down. It is safe to call at any point, including after a
// failed step.
func (s *Stack) Close(ctx context.Context) {
	for _, handle := range []*Handle{s.backend, s.frontend} {
		if handle == nil {
			continue
		}
		if err := handle.Kill(ctx); err != nil {
			s.tb.Logf("cleanup: %v", err)
		}
		handle.Close()
	}
}

// passThrough returns the host environment variables forwarded to the frontend
// container, in KEY=VALUE form.
func (s *Stack) passThrough() []string {
	if s.getenv == nil {
		return nil
	}
	var env []string
	for _, key := range passThroughEnv {
		if value := s.getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// runnerDiagnostics returns the captured output of a container that has already
// exited.
func (s *Stack) runnerDiagnostics(name string) string {
	path := filepath.Join(evidenceDir, "diagnostics", name+".log")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "(no output captured)"
	}
	return tail(string(raw), 4000)
}

// tail returns the last n bytes of text.
func tail(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return "…" + text[len(text)-n:]
}
