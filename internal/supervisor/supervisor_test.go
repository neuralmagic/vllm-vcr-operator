package supervisor_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/supervisor"
)

// healthEndpoint is a controllable HTTP endpoint standing in for the frontend's
// health URL or the simulator's control API. Tests flip its status to simulate a
// healthy, an unhealthy, or a hung input.
type healthEndpoint struct {
	*httptest.Server
	status atomic.Int32
	hits   atomic.Int64
}

func newHealthEndpoint(t *testing.T, status int) *healthEndpoint {
	t.Helper()
	endpoint := &healthEndpoint{}
	endpoint.status.Store(int32(status))
	endpoint.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoint.hits.Add(1)
		code := int(endpoint.status.Load())
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, "endpoint\n")
	}))
	t.Cleanup(endpoint.Server.Close)
	return endpoint
}

func (e *healthEndpoint) URL() string { return e.Server.URL }

func (e *healthEndpoint) setStatus(status int) { e.status.Store(int32(status)) }

func (e *healthEndpoint) Hits() int64 { return e.hits.Load() }

// hungEndpoint is an HTTP endpoint that never answers, standing in for a wedged
// frontend. It is released before the test server is closed.
func newHungEndpoint(t *testing.T) (url string, hits *atomic.Int64) {
	t.Helper()
	release := make(chan struct{})
	hits = &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
	}))
	// Cleanups run last-registered-first: release the handlers before Close waits
	// for them.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv.URL, hits
}

// testConfig returns a Config for a long-lived fake frontend whose frontend
// health URL and simulator control URL are the returned endpoints.
func testConfig(t *testing.T) (supervisor.Config, *healthEndpoint, *healthEndpoint) {
	t.Helper()
	frontend := newHealthEndpoint(t, http.StatusOK)
	backend := newHealthEndpoint(t, http.StatusOK)
	return supervisor.Config{
		Command:            []string{"/bin/sh", "-c", "sleep 30"},
		ReadyFile:          filepath.Join(t.TempDir(), "runtime", "ready"),
		HealthAddress:      "127.0.0.1:0",
		FrontendHealthURL:  frontend.URL(),
		BackendHealthURL:   backend.URL(),
		BackendLossGrace:   2 * time.Second,
		ShutdownTimeout:    2 * time.Second,
		HealthPollInterval: 10 * time.Millisecond,
		Logf:               t.Logf,
	}, frontend, backend
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

// reserveAddress returns a loopback address nothing is listening on.
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
	return httpStatusBody(t, url).status
}

type statusAndBody struct {
	status int
	body   string
}

func httpStatusBody(t *testing.T, url string) statusAndBody {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return statusAndBody{}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return statusAndBody{status: resp.StatusCode, body: strings.TrimSpace(string(body))}
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

func waitNoFile(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for fileExists(path) && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if fileExists(path) {
		t.Fatalf("ready file %s still exists after %s", path, within)
	}
}

func waitFile(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !fileExists(path) && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !fileExists(path) {
		t.Fatalf("file %s did not appear within %s", path, within)
	}
}

func readyzURL(t *testing.T, s *supervisor.Supervisor) string {
	t.Helper()
	return fmt.Sprintf("http://%s%s", waitHealthAddress(t, s), supervisor.ReadyPath)
}

func healthzURL(t *testing.T, s *supervisor.Supervisor) string {
	t.Helper()
	return fmt.Sprintf("http://%s%s", waitHealthAddress(t, s), supervisor.HealthPath)
}

// TestReadinessRequiresBothHealthEndpoints is the core of this ticket: the Pod is
// ready only while the frontend reports healthy AND the simulator control API
// answers.
func TestReadinessRequiresBothHealthEndpoints(t *testing.T) {
	cfg, frontend, backend := testConfig(t)
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := startSupervisor(t, s, ctx)

	waitStatus(t, healthzURL(t, s), http.StatusOK, 5*time.Second)
	waitStatus(t, readyzURL(t, s), http.StatusOK, 5*time.Second)
	if !fileExists(cfg.ReadyFile) {
		t.Fatalf("ready file %s missing while both health inputs answer", cfg.ReadyFile)
	}

	// The frontend stops answering healthy: readiness is withdrawn at once, well
	// before the backend loss grace expires.
	frontend.setStatus(http.StatusServiceUnavailable)
	waitStatus(t, readyzURL(t, s), http.StatusServiceUnavailable, time.Second)
	waitNoFile(t, cfg.ReadyFile, time.Second)

	// The frontend recovers, then the control API goes away instead.
	frontend.setStatus(http.StatusOK)
	waitStatus(t, readyzURL(t, s), http.StatusOK, 5*time.Second)
	backend.setStatus(http.StatusServiceUnavailable)
	waitStatus(t, readyzURL(t, s), http.StatusServiceUnavailable, time.Second)
	waitNoFile(t, cfg.ReadyFile, time.Second)

	// Recovery on both inputs restores readiness.
	backend.setStatus(http.StatusOK)
	waitStatus(t, readyzURL(t, s), http.StatusOK, 5*time.Second)

	cancel()
	if code := waitForExit(t, codes, 10*time.Second); code != 0 {
		t.Fatalf("exit code after shutdown = %d, want 0", code)
	}
}

// TestHandshakeDialAloneNeverReportsReady is the regression test for this
// ticket's motivating bug: the frontend binds the engine-core handshake socket,
// so a TCP connection to it succeeds with no simulator registered. Readiness must
// not follow that dial.
func TestHandshakeDialAloneNeverReportsReady(t *testing.T) {
	// A listener that accepts connections stands in for the frontend's
	// engine-core socket: exactly the input the old readiness rule used.
	handshake, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on handshake address: %v", err)
	}
	defer handshake.Close()
	go func() {
		for {
			conn, err := handshake.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	cfg, frontend, backend := testConfig(t)
	cfg.HandshakeAddress = "tcp://" + handshake.Addr().String()
	cfg.BackendLossGrace = time.Hour
	frontend.setStatus(http.StatusServiceUnavailable)
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startSupervisor(t, s, ctx)

	waitStatus(t, healthzURL(t, s), http.StatusOK, 5*time.Second)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := httpStatus(t, readyzURL(t, s)); got != http.StatusServiceUnavailable {
			t.Fatalf("readyz = %d while the handshake socket accepts connections but no health input answers, want %d",
				got, http.StatusServiceUnavailable)
		}
		if fileExists(cfg.ReadyFile) {
			t.Fatalf("ready file %s exists although only the handshake socket accepts connections", cfg.ReadyFile)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The simulator's control API answering is not enough on its own either: the
	// frontend must report healthy too.
	backend.setStatus(http.StatusOK)
	time.Sleep(100 * time.Millisecond)
	if got := httpStatus(t, readyzURL(t, s)); got != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d with only the control API answering, want %d", got, http.StatusServiceUnavailable)
	}
}

// TestFrontendHealthFailureWithdrawsReadinessWithoutTerminating asserts a
// frontend that stays alive but stops serving healthy is reported not ready and is
// not terminated by the supervisor.
func TestFrontendHealthFailureWithdrawsReadinessWithoutTerminating(t *testing.T) {
	cfg, frontend, _ := testConfig(t)
	cfg.BackendLossGrace = 100 * time.Millisecond
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := startSupervisor(t, s, ctx)

	waitStatus(t, readyzURL(t, s), http.StatusOK, 5*time.Second)
	frontend.setStatus(http.StatusServiceUnavailable)

	// Longer than the backend loss grace: an unhealthy frontend is not a backend
	// loss, so the child must survive.
	time.Sleep(4 * cfg.BackendLossGrace)
	waitStatus(t, healthzURL(t, s), http.StatusOK, time.Second)
	if got := httpStatus(t, readyzURL(t, s)); got != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d while the frontend health endpoint fails, want %d", got, http.StatusServiceUnavailable)
	}
	if fileExists(cfg.ReadyFile) {
		t.Fatalf("ready file %s exists while the frontend health endpoint fails", cfg.ReadyFile)
	}
	select {
	case code := <-codes:
		t.Fatalf("supervisor exited with %d while only frontend health failed, want it to keep supervising", code)
	default:
	}

	cancel()
	if code := waitForExit(t, codes, 10*time.Second); code != 0 {
		t.Fatalf("exit code after shutdown = %d, want 0", code)
	}
}

// TestBackendLossWithdrawsReadinessThenTerminatesFrontend covers the failure
// propagation contract: readiness goes away immediately, the frontend receives
// SIGTERM once the grace expires, and the container exits non-zero.
func TestBackendLossWithdrawsReadinessThenTerminatesFrontend(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "sigterm")
	started := filepath.Join(t.TempDir(), "started")
	cfg, _, backend := testConfig(t)
	cfg.Command = []string{"/bin/sh", "-c", `trap 'printf term > "$VCR_TEST_MARKER"; exit 0' TERM
printf started > "$VCR_TEST_STARTED"
while true; do sleep 0.01; done`}
	cfg.Env = append(os.Environ(), "VCR_TEST_MARKER="+marker, "VCR_TEST_STARTED="+started)
	cfg.BackendLossGrace = 300 * time.Millisecond
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := startSupervisor(t, s, ctx)

	waitStatus(t, readyzURL(t, s), http.StatusOK, 5*time.Second)
	waitFile(t, started, 5*time.Second)
	backend.setStatus(http.StatusServiceUnavailable)

	waitStatus(t, readyzURL(t, s), http.StatusServiceUnavailable, time.Second)
	waitNoFile(t, cfg.ReadyFile, time.Second)

	code := waitForExit(t, codes, 10*time.Second)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero after losing the simulator control API")
	}
	if !fileExists(marker) {
		t.Fatalf("frontend did not receive SIGTERM after the control API was lost (marker %s missing)", marker)
	}
	if fileExists(cfg.ReadyFile) {
		t.Fatalf("ready file %s still exists after the control API was lost", cfg.ReadyFile)
	}
}

// TestHungHealthEndpointDoesNotStallOtherProbesOrShutdown asserts one wedged
// endpoint cannot freeze the remaining readiness inputs or block termination.
func TestHungHealthEndpointDoesNotStallOtherProbesOrShutdown(t *testing.T) {
	frontendURL, frontendHits := newHungEndpoint(t)
	backend := newHealthEndpoint(t, http.StatusOK)

	cfg, _, _ := testConfig(t)
	cfg.FrontendHealthURL = frontendURL
	cfg.BackendHealthURL = backend.URL()
	cfg.BackendLossGrace = 2 * time.Second
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := startSupervisor(t, s, ctx)

	waitStatus(t, healthzURL(t, s), http.StatusOK, 5*time.Second)
	// The frontend endpoint is hung, so the reason must name the frontend input:
	// that reason can only be produced once the control API probe answered.
	got := httpStatusBody(t, readyzURL(t, s))
	if got.status != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d while the frontend endpoint hangs, want %d", got.status, http.StatusServiceUnavailable)
	}
	if !strings.Contains(got.body, "frontend health") {
		t.Fatalf("readyz reason %q does not name the hung frontend health input", got.body)
	}

	// A hung request must not stop the poll loops: the control API keeps being
	// probed while the frontend request is still outstanding.
	if frontendHits.Load() == 0 {
		t.Fatal("the hung frontend endpoint never received a probe")
	}
	before := backend.Hits()
	time.Sleep(200 * time.Millisecond)
	if backend.Hits() <= before {
		t.Fatalf("control API probes stalled behind the hung frontend endpoint: %d hits before, %d after 200ms",
			before, backend.Hits())
	}

	start := time.Now()
	cancel()
	if code := waitForExit(t, codes, 3*time.Second); code != 0 {
		t.Fatalf("exit code = %d, want 0 for a coordinated shutdown", code)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("shutdown took %s with a hung health endpoint, want it bounded by the shutdown budget", elapsed)
	}
}

func TestFrontendExitCodePropagates(t *testing.T) {
	cfg, _, _ := testConfig(t)
	cfg.Command = []string{"/bin/sh", "-c", "sleep 0.3; exit 3"}
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := startSupervisor(t, s, ctx)

	deadline := time.Now().Add(5 * time.Second)
	for !fileExists(cfg.ReadyFile) && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !fileExists(cfg.ReadyFile) {
		t.Fatalf("ready file %s never appeared", cfg.ReadyFile)
	}

	if code := waitForExit(t, codes, 10*time.Second); code != 3 {
		t.Fatalf("exit code = %d, want the frontend's 3", code)
	}
	if fileExists(cfg.ReadyFile) {
		t.Fatalf("ready file %s still exists after the frontend exited", cfg.ReadyFile)
	}
}

func TestSignalledShutdownTerminatesFrontendCleanly(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "sigterm")
	started := filepath.Join(t.TempDir(), "started")
	cfg, _, _ := testConfig(t)
	cfg.Command = []string{"/bin/sh", "-c", `trap 'printf term > "$VCR_TEST_MARKER"; exit 0' TERM
printf started > "$VCR_TEST_STARTED"
while true; do sleep 0.01; done`}
	cfg.Env = append(os.Environ(), "VCR_TEST_MARKER="+marker, "VCR_TEST_STARTED="+started)
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := startSupervisor(t, s, ctx)

	waitStatus(t, readyzURL(t, s), http.StatusOK, 5*time.Second)
	waitFile(t, started, 5*time.Second)
	cancel()

	if code := waitForExit(t, codes, 10*time.Second); code != 0 {
		t.Fatalf("exit code = %d, want 0 for a coordinated shutdown", code)
	}
	if !fileExists(marker) {
		t.Fatalf("frontend did not receive SIGTERM on shutdown (marker %s missing)", marker)
	}
	if fileExists(cfg.ReadyFile) {
		t.Fatalf("ready file %s still exists after shutdown", cfg.ReadyFile)
	}
}

func TestShutdownEscalatesToSIGKILLWhenFrontendIgnoresSIGTERM(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	cfg, _, _ := testConfig(t)
	cfg.Command = []string{"/bin/sh", "-c", `trap '' TERM
printf started > "$VCR_TEST_STARTED"
while true; do sleep 0.01; done`}
	cfg.Env = append(os.Environ(), "VCR_TEST_STARTED="+started)
	cfg.ShutdownTimeout = 150 * time.Millisecond
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := startSupervisor(t, s, ctx)

	waitStatus(t, healthzURL(t, s), http.StatusOK, 5*time.Second)
	waitFile(t, started, 5*time.Second)
	start := time.Now()
	cancel()

	if code := waitForExit(t, codes, 5*time.Second); code == 0 {
		t.Fatalf("exit code = 0, want non-zero when the frontend ignores SIGTERM")
	}
	if elapsed := time.Since(start); elapsed < cfg.ShutdownTimeout {
		t.Fatalf("shutdown gave up after %s, before the %s budget expired", elapsed, cfg.ShutdownTimeout)
	}
	if fileExists(cfg.ReadyFile) {
		t.Fatalf("ready file %s still exists after shutdown", cfg.ReadyFile)
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
	cfg, _, _ := testConfig(t)
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
	cfg, frontend, _ := testConfig(t)
	if err := os.MkdirAll(filepath.Dir(cfg.ReadyFile), 0o755); err != nil {
		t.Fatalf("create ready file directory: %v", err)
	}
	if err := os.WriteFile(cfg.ReadyFile, []byte("ready\n"), 0o644); err != nil {
		t.Fatalf("seed stale ready file: %v", err)
	}
	frontend.setStatus(http.StatusServiceUnavailable)
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := startSupervisor(t, s, ctx)

	waitStatus(t, healthzURL(t, s), http.StatusOK, 5*time.Second)
	if fileExists(cfg.ReadyFile) {
		t.Fatalf("stale ready file %s survived startup with no healthy frontend", cfg.ReadyFile)
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
		{name: "frontend health URL without scheme", cfg: supervisor.Config{
			Command:           []string{"/bin/sh", "-c", "true"},
			FrontendHealthURL: "127.0.0.1:8000/health",
		}},
		{name: "frontend health URL with unsupported scheme", cfg: supervisor.Config{
			Command:           []string{"/bin/sh", "-c", "true"},
			FrontendHealthURL: "tcp://127.0.0.1:8000/health",
		}},
		{name: "backend health URL without host", cfg: supervisor.Config{
			Command:          []string{"/bin/sh", "-c", "true"},
			BackendHealthURL: "http:///stats",
		}},
		{name: "negative backend loss grace", cfg: supervisor.Config{
			Command:          []string{"/bin/sh", "-c", "true"},
			BackendLossGrace: -time.Second,
		}},
		{name: "negative shutdown timeout", cfg: supervisor.Config{
			Command:         []string{"/bin/sh", "-c", "true"},
			ShutdownTimeout: -time.Second,
		}},
		{name: "negative health poll interval", cfg: supervisor.Config{
			Command:            []string{"/bin/sh", "-c", "true"},
			HealthPollInterval: -time.Second,
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
	cfg, _, _ := testConfig(t)
	cfg.ShutdownTimeout = 0
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := startSupervisor(t, s, ctx)
	waitStatus(t, healthzURL(t, s), http.StatusOK, 5*time.Second)

	start := time.Now()
	cancel()
	if code := waitForExit(t, codes, 5*time.Second); code != 0 {
		t.Fatalf("exit code = %d, want 0 for the immediate shutdown policy", code)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("immediate shutdown took %s, want the frontend stopped without waiting", elapsed)
	}
	if fileExists(cfg.ReadyFile) {
		t.Fatalf("ready file %s still exists after shutdown", cfg.ReadyFile)
	}
}
