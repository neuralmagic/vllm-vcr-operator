package supervisor_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/supervisor"
)

// baseConfig returns a Config for a long-lived fake frontend whose engine is
// not reachable yet. Tests adjust it as needed.
func baseConfig(t *testing.T, readyFile string) supervisor.Config {
	t.Helper()
	return supervisor.Config{
		Command:          []string{"/bin/sh", "-c", "sleep 30"},
		ReadyFile:        readyFile,
		HealthAddress:    "127.0.0.1:0",
		BackendLossGrace: 2 * time.Second,
		ShutdownTimeout:  2 * time.Second,
		PollInterval:     10 * time.Millisecond,
		Logf:             t.Logf,
	}
}

// startSupervisor runs s in the background and returns the exit code channel.
func startSupervisor(t *testing.T, s *supervisor.Supervisor, ctx context.Context) <-chan int {
	t.Helper()
	codes := make(chan int, 1)
	go func() {
		code, err := s.Run(ctx)
		if err != nil {
			t.Logf("Run returned code=%d err=%v", code, err)
		}
		codes <- code
	}()
	t.Cleanup(func() {
		select {
		case <-codes:
		default:
		}
	})
	return codes
}

func waitForExit(t *testing.T, codes <-chan int, within time.Duration) int {
	t.Helper()
	select {
	case code := <-codes:
		return code
	case <-time.After(within):
		t.Fatalf("supervisor did not exit within %s", within)
		return -1
	}
}

// reserveAddress returns a loopback address that nothing is listening on.
func reserveAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close reserved port: %v", err)
	}
	return addr
}

// waitHealthAddress returns the health endpoint the supervisor actually bound.
func waitHealthAddress(t *testing.T, s *supervisor.Supervisor) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := s.HealthAddress(); addr != "" {
			return addr
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("supervisor did not bind a health address")
	return ""
}

func httpStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// waitStatus polls url until it answers with want, or fails the test.
func waitStatus(t *testing.T, url string, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last int
	for time.Now().Before(deadline) {
		if last = httpStatus(t, url); last == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s: last status %d, want %d within %s", url, last, want, within)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestReadinessFollowsEngineConnectivity(t *testing.T) {
	engineAddr := reserveAddress(t)
	readyFile := filepath.Join(t.TempDir(), "runtime", "ready")

	cfg := baseConfig(t, readyFile)
	cfg.HandshakeAddress = "tcp://" + engineAddr
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := startSupervisor(t, s, ctx)

	health := waitHealthAddress(t, s)
	healthz := fmt.Sprintf("http://%s/healthz", health)
	readyz := fmt.Sprintf("http://%s/readyz", health)

	// The frontend process is alive, but no engine has registered yet.
	waitStatus(t, healthz, http.StatusOK, 5*time.Second)
	if got := httpStatus(t, readyz); got != http.StatusServiceUnavailable {
		t.Fatalf("readyz with no engine = %d, want %d", got, http.StatusServiceUnavailable)
	}
	if fileExists(readyFile) {
		t.Fatalf("ready file %s exists before the engine registered", readyFile)
	}

	// An engine registers: readiness flips true.
	engine, err := net.Listen("tcp", engineAddr)
	if err != nil {
		t.Fatalf("listen on engine address: %v", err)
	}
	waitStatus(t, readyz, http.StatusOK, 5*time.Second)
	if !fileExists(readyFile) {
		t.Fatalf("ready file %s missing after the engine registered", readyFile)
	}

	// The engine disappears: readiness flips false and the file is removed.
	if err := engine.Close(); err != nil {
		t.Fatalf("close engine listener: %v", err)
	}
	waitStatus(t, readyz, http.StatusServiceUnavailable, 5*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for fileExists(readyFile) && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if fileExists(readyFile) {
		t.Fatalf("ready file %s still exists after the engine went away", readyFile)
	}

	cancel()
	if code := waitForExit(t, codes, 10*time.Second); code != 0 {
		t.Fatalf("exit code after shutdown = %d, want 0", code)
	}
}

func TestFrontendExitCodePropagates(t *testing.T) {
	engine, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on engine address: %v", err)
	}
	defer engine.Close()

	readyFile := filepath.Join(t.TempDir(), "ready")
	cfg := baseConfig(t, readyFile)
	cfg.Command = []string{"/bin/sh", "-c", "sleep 0.3; exit 3"}
	cfg.HandshakeAddress = "tcp://" + engine.Addr().String()
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := startSupervisor(t, s, ctx)

	deadline := time.Now().Add(5 * time.Second)
	for !fileExists(readyFile) && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !fileExists(readyFile) {
		t.Fatalf("ready file %s never appeared", readyFile)
	}

	if code := waitForExit(t, codes, 10*time.Second); code != 3 {
		t.Fatalf("exit code = %d, want the frontend's 3", code)
	}
	if fileExists(readyFile) {
		t.Fatalf("ready file %s still exists after the frontend exited", readyFile)
	}
}

func TestBackendLossTerminatesFrontend(t *testing.T) {
	engineAddr := reserveAddress(t)
	engine, err := net.Listen("tcp", engineAddr)
	if err != nil {
		t.Fatalf("listen on engine address: %v", err)
	}

	marker := filepath.Join(t.TempDir(), "sigterm")
	readyFile := filepath.Join(t.TempDir(), "ready")
	cfg := baseConfig(t, readyFile)
	cfg.Command = []string{"/bin/sh", "-c", `trap 'printf term > "$VCR_TEST_MARKER"; exit 0' TERM
while true; do sleep 0.01; done`}
	cfg.Env = append(os.Environ(), "VCR_TEST_MARKER="+marker)
	cfg.HandshakeAddress = "tcp://" + engineAddr
	cfg.BackendLossGrace = 100 * time.Millisecond
	cfg.ShutdownTimeout = 2 * time.Second
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := startSupervisor(t, s, ctx)

	waitStatus(t, fmt.Sprintf("http://%s/readyz", waitHealthAddress(t, s)), http.StatusOK, 5*time.Second)

	if err := engine.Close(); err != nil {
		t.Fatalf("close engine listener: %v", err)
	}

	code := waitForExit(t, codes, 10*time.Second)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero after losing the engine")
	}
	if !fileExists(marker) {
		t.Fatalf("frontend did not receive SIGTERM after the engine was lost (marker %s missing)", marker)
	}
	if fileExists(readyFile) {
		t.Fatalf("ready file %s still exists after the engine was lost", readyFile)
	}
}

func TestSignalledShutdownTerminatesFrontendCleanly(t *testing.T) {
	engine, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on engine address: %v", err)
	}
	defer engine.Close()

	marker := filepath.Join(t.TempDir(), "sigterm")
	readyFile := filepath.Join(t.TempDir(), "ready")
	cfg := baseConfig(t, readyFile)
	cfg.Command = []string{"/bin/sh", "-c", `trap 'printf term > "$VCR_TEST_MARKER"; exit 0' TERM
while true; do sleep 0.01; done`}
	cfg.Env = append(os.Environ(), "VCR_TEST_MARKER="+marker)
	cfg.HandshakeAddress = "tcp://" + engine.Addr().String()
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := startSupervisor(t, s, ctx)

	waitStatus(t, fmt.Sprintf("http://%s/readyz", waitHealthAddress(t, s)), http.StatusOK, 5*time.Second)
	cancel()

	if code := waitForExit(t, codes, 10*time.Second); code != 0 {
		t.Fatalf("exit code = %d, want 0 for a coordinated shutdown", code)
	}
	if !fileExists(marker) {
		t.Fatalf("frontend did not receive SIGTERM on shutdown (marker %s missing)", marker)
	}
	if fileExists(readyFile) {
		t.Fatalf("ready file %s still exists after shutdown", readyFile)
	}
}

func TestShutdownEscalatesToSIGKILLWhenFrontendIgnoresSIGTERM(t *testing.T) {
	readyFile := filepath.Join(t.TempDir(), "ready")
	cfg := baseConfig(t, readyFile)
	cfg.HandshakeAddress = "tcp://" + reserveAddress(t)
	cfg.Command = []string{"/bin/sh", "-c", `trap '' TERM
while true; do sleep 0.01; done`}
	cfg.ShutdownTimeout = 150 * time.Millisecond
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := startSupervisor(t, s, ctx)

	waitStatus(t, fmt.Sprintf("http://%s/healthz", waitHealthAddress(t, s)), http.StatusOK, 5*time.Second)
	start := time.Now()
	cancel()

	if code := waitForExit(t, codes, 5*time.Second); code == 0 {
		t.Fatalf("exit code = 0, want non-zero when the frontend ignores SIGTERM")
	}
	if elapsed := time.Since(start); elapsed < cfg.ShutdownTimeout {
		t.Fatalf("shutdown gave up after %s, before the %s budget expired", elapsed, cfg.ShutdownTimeout)
	}
	if fileExists(readyFile) {
		t.Fatalf("ready file %s still exists after shutdown", readyFile)
	}
}

// syncBuffer is a goroutine-safe writer for capturing child output while the
// child is still running.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestFrontendOutputIsForwardedUnchanged(t *testing.T) {
	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
	cfg := baseConfig(t, filepath.Join(t.TempDir(), "ready"))
	cfg.HandshakeAddress = "tcp://" + reserveAddress(t)
	cfg.Command = []string{"/bin/sh", "-c", `printf 'frontend-stdout\n'; printf 'frontend-stderr\n' >&2; sleep 30`}
	cfg.Stdout = stdout
	cfg.Stderr = stderr
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := startSupervisor(t, s, ctx)
	defer func() {
		cancel()
		waitForExit(t, codes, 10*time.Second)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if stdout.String() == "frontend-stdout\n" && stderr.String() == "frontend-stderr\n" {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("forwarded stdout=%q stderr=%q, want the frontend's exact output", stdout.String(), stderr.String())
}

func TestRunRemovesStaleReadyFile(t *testing.T) {
	readyFile := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(readyFile, []byte("ready\n"), 0o644); err != nil {
		t.Fatalf("seed stale ready file: %v", err)
	}

	cfg := baseConfig(t, readyFile)
	cfg.HandshakeAddress = "tcp://" + reserveAddress(t)
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := startSupervisor(t, s, ctx)

	waitStatus(t, fmt.Sprintf("http://%s/healthz", waitHealthAddress(t, s)), http.StatusOK, 5*time.Second)
	if fileExists(readyFile) {
		t.Fatalf("stale ready file %s survived startup with no engine registered", readyFile)
	}

	cancel()
	waitForExit(t, codes, 10*time.Second)
}

func TestNewRejectsConfigurationItCannotObserve(t *testing.T) {
	tests := []struct {
		name string
		cfg  supervisor.Config
	}{
		{name: "no frontend command", cfg: supervisor.Config{}},
		{name: "non-tcp handshake scheme", cfg: supervisor.Config{
			Command:          []string{"/bin/sh", "-c", "true"},
			HandshakeAddress: "ipc:///tmp/vllm.sock",
		}},
		{name: "handshake without port", cfg: supervisor.Config{
			Command:          []string{"/bin/sh", "-c", "true"},
			HandshakeAddress: "tcp://127.0.0.1",
		}},
		{name: "negative backend loss grace", cfg: supervisor.Config{
			Command:          []string{"/bin/sh", "-c", "true"},
			BackendLossGrace: -time.Second,
		}},
		{name: "negative shutdown timeout", cfg: supervisor.Config{
			Command:         []string{"/bin/sh", "-c", "true"},
			ShutdownTimeout: -time.Second,
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := supervisor.New(tc.cfg); err == nil {
				t.Fatalf("New accepted %+v, want a rejection naming the problem", tc.cfg)
			}
		})
	}
}

// TestImmediateShutdownPolicyExitsCleanly locks the documented meaning of a zero
// shutdown budget: the frontend is stopped without waiting, and the container
// still reports a clean drain.
func TestImmediateShutdownPolicyExitsCleanly(t *testing.T) {
	readyFile := filepath.Join(t.TempDir(), "ready")
	cfg := baseConfig(t, readyFile)
	cfg.HandshakeAddress = "tcp://" + reserveAddress(t)
	cfg.ShutdownTimeout = 0
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := startSupervisor(t, s, ctx)
	waitStatus(t, fmt.Sprintf("http://%s/healthz", waitHealthAddress(t, s)), http.StatusOK, 5*time.Second)

	start := time.Now()
	cancel()
	if code := waitForExit(t, codes, 5*time.Second); code != 0 {
		t.Fatalf("exit code = %d, want 0 for the immediate shutdown policy", code)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("immediate shutdown took %s, want the frontend stopped without waiting", elapsed)
	}
	if fileExists(readyFile) {
		t.Fatalf("ready file %s still exists after shutdown", readyFile)
	}
}
