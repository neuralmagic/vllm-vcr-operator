package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/supervisor"
)

// getenvFrom returns a getenv func backed by a map, so tests never depend on the
// ambient environment.
func getenvFrom(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestParseConfigUsesDefaults(t *testing.T) {
	cfg, err := parseConfig([]string{"--", "python", "-m", "vllm.serve"}, getenvFrom(nil), os.Stderr)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if got, want := strings.Join(cfg.Command, " "), "python -m vllm.serve"; got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
	if cfg.HandshakeAddress != supervisor.DefaultHandshakeAddress {
		t.Errorf("handshake address = %q, want %q", cfg.HandshakeAddress, supervisor.DefaultHandshakeAddress)
	}
	if cfg.ReadyFile != supervisor.DefaultReadyFile {
		t.Errorf("ready file = %q, want %q", cfg.ReadyFile, supervisor.DefaultReadyFile)
	}
	if cfg.HealthAddress != supervisor.DefaultHealthAddress {
		t.Errorf("health address = %q, want %q", cfg.HealthAddress, supervisor.DefaultHealthAddress)
	}
	if cfg.BackendLossGrace != supervisor.DefaultBackendLossGrace {
		t.Errorf("backend loss grace = %s, want %s", cfg.BackendLossGrace, supervisor.DefaultBackendLossGrace)
	}
	if cfg.ShutdownTimeout != 0 {
		t.Errorf("shutdown timeout = %s, want 0 (immediate)", cfg.ShutdownTimeout)
	}
}

func TestParseConfigReadsRuntimeLayoutEnv(t *testing.T) {
	env := map[string]string{
		"VCR_HANDSHAKE_ADDRESS": "tcp://127.0.0.1:29551",
		"VCR_READY_FILE":        "/var/run/vcr/ready",
		"VCR_SHUTDOWN_TIMEOUT":  "7",
	}
	cfg, err := parseConfig([]string{"python", "-m", "vllm.serve"}, getenvFrom(env), os.Stderr)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.HandshakeAddress != "tcp://127.0.0.1:29551" {
		t.Errorf("handshake address = %q, want the VCR_HANDSHAKE_ADDRESS value", cfg.HandshakeAddress)
	}
	if cfg.ReadyFile != "/var/run/vcr/ready" {
		t.Errorf("ready file = %q, want the VCR_READY_FILE value", cfg.ReadyFile)
	}
	if cfg.ShutdownTimeout != 7*time.Second {
		t.Errorf("shutdown timeout = %s, want 7s from VCR_SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout)
	}
}

func TestFlagsOverrideEnv(t *testing.T) {
	env := map[string]string{
		"VCR_HANDSHAKE_ADDRESS": "tcp://127.0.0.1:29551",
		"VCR_READY_FILE":        "/var/run/vcr/ready",
		"VCR_SHUTDOWN_TIMEOUT":  "7",
	}
	args := []string{
		"--handshake-address", "tcp://127.0.0.1:29552",
		"--ready-file", "/tmp/other/ready",
		"--shutdown-timeout", "1200ms",
		"--health-address", "127.0.0.1:18082",
		"--backend-loss-grace", "900ms",
		"--poll-interval", "25ms",
		"--", "python", "-m", "vllm.serve",
	}
	cfg, err := parseConfig(args, getenvFrom(env), os.Stderr)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.HandshakeAddress != "tcp://127.0.0.1:29552" {
		t.Errorf("handshake address = %q, want the flag value", cfg.HandshakeAddress)
	}
	if cfg.ReadyFile != "/tmp/other/ready" {
		t.Errorf("ready file = %q, want the flag value", cfg.ReadyFile)
	}
	if cfg.ShutdownTimeout != 1200*time.Millisecond {
		t.Errorf("shutdown timeout = %s, want 1200ms from the flag", cfg.ShutdownTimeout)
	}
	if cfg.HealthAddress != "127.0.0.1:18082" {
		t.Errorf("health address = %q, want the flag value", cfg.HealthAddress)
	}
	if cfg.BackendLossGrace != 900*time.Millisecond {
		t.Errorf("backend loss grace = %s, want 900ms", cfg.BackendLossGrace)
	}
	if cfg.PollInterval != 25*time.Millisecond {
		t.Errorf("poll interval = %s, want 25ms", cfg.PollInterval)
	}
}

func TestParseConfigRequiresFrontendCommand(t *testing.T) {
	_, err := parseConfig(nil, getenvFrom(nil), os.Stderr)
	if err == nil {
		t.Fatal("parseConfig accepted an invocation without a frontend command")
	}
	if !strings.Contains(err.Error(), "frontend command") {
		t.Fatalf("error %q does not explain that a frontend command is required", err)
	}
}

func TestParseConfigRejectsInvalidShutdownTimeoutEnv(t *testing.T) {
	env := map[string]string{"VCR_SHUTDOWN_TIMEOUT": "soon"}
	_, err := parseConfig([]string{"python", "-m", "vllm.serve"}, getenvFrom(env), os.Stderr)
	if err == nil {
		t.Fatal("parseConfig accepted VCR_SHUTDOWN_TIMEOUT=soon")
	}
	if !strings.Contains(err.Error(), "VCR_SHUTDOWN_TIMEOUT") {
		t.Fatalf("error %q does not name VCR_SHUTDOWN_TIMEOUT", err)
	}
}

// TestBinaryShutsDownOnSIGTERM exercises the real entrypoint: build it, run it
// with a fake frontend, and drive the container lifecycle with signals.
func TestBinaryShutsDownOnSIGTERM(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "vcr-supervisor")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build cmd/supervisor: %v", err)
	}

	// A loopback listener standing in for the registered simulator.
	engine, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on engine address: %v", err)
	}
	defer engine.Close()

	readyFile := filepath.Join(t.TempDir(), "runtime", "ready")
	cmd := exec.Command(bin,
		"--health-address", "127.0.0.1:0",
		"--handshake-address", "tcp://"+engine.Addr().String(),
		"--poll-interval", "10ms",
		"--shutdown-timeout", "2s",
		"--", "/bin/sh", "-c", `trap 'exit 0' TERM
while true; do sleep 0.01; done`)
	cmd.Env = append(os.Environ(), "VCR_READY_FILE="+readyFile)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start supervisor binary: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("supervisor binary never wrote the ready file %s", readyFile)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal supervisor: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervisor binary exited with %v, want a clean 0", err)
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("supervisor binary did not exit after SIGTERM")
	}
	if _, err := os.Stat(readyFile); err == nil {
		t.Fatalf("ready file %s survived shutdown", readyFile)
	}
}
