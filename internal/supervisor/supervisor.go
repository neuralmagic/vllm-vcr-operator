// Package supervisor implements the frontend container's entrypoint for a
// simulated model server.
//
// A simulated model-server Pod runs the Python vLLM frontend in the vllm-frontend
// container and the vllm-vcr simulator in its own container (see
// docs/contracts/runtime-layout.md). The supervisor is the frontend container's
// entrypoint: it starts the frontend as its only long-lived child, reports
// readiness that combines frontend process liveness with engine registration,
// propagates failures, and shuts the container down in a coordinated way.
//
// The engine registration state is observed by dialing the engine-core handshake
// address over TCP: readiness requires the frontend process to be alive and that
// endpoint to accept connections. The supervisor never restarts or resurrects
// the simulator; recovery is restart-based, not reconnect-based.
//
// Integration note: because readiness is "a connection to HandshakeAddress
// succeeds", that address must name an endpoint that only accepts connections
// while an engine is registered. The runtime layout therefore passes the
// registration endpoint the simulator publishes to the frontend, not the port
// the frontend itself binds; the runtime spike records the pinned images'
// observed behavior and must configure HandshakeAddress accordingly.
//
// Environment variables read by cmd/supervisor (docs/contracts/runtime-layout.md):
// VCR_HANDSHAKE_ADDRESS, VCR_READY_FILE, and VCR_SHUTDOWN_TIMEOUT (seconds).
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
)

// Defaults applied by New when a Config field is left zero.
var (
	// DefaultHandshakeAddress is the engine-core handshake endpoint the
	// frontend binds and the simulator connects to.
	DefaultHandshakeAddress = fmt.Sprintf("tcp://127.0.0.1:%d", names.HandshakePort)
)

const (
	// DefaultReadyFile is the readiness marker incident diagnostics read.
	DefaultReadyFile = "/tmp/vcr-runtime/ready"

	// DefaultHealthAddress is the loopback endpoint serving /healthz and
	// /readyz for the frontend container.
	DefaultHealthAddress = "127.0.0.1:8082"

	// DefaultBackendLossGrace is how long the engine may stay unreachable
	// before the frontend is terminated.
	DefaultBackendLossGrace = 5 * time.Second

	// DefaultPollInterval is how often engine registration is probed.
	DefaultPollInterval = 100 * time.Millisecond

	// exitCodeFailure is the container exit code used when the supervisor
	// itself forces a restart.
	exitCodeFailure = 1

	// reapTimeout bounds how long the supervisor waits for a SIGKILLed child to
	// be reaped, so that a wedged child cannot hang the container.
	reapTimeout = 10 * time.Second

	// probeTimeout bounds a single engine-registration probe.
	probeTimeout = 200 * time.Millisecond
)

// Config describes how the supervisor runs and observes the frontend.
type Config struct {
	// Command is the frontend command (argv[0] plus arguments). Required.
	Command []string

	// Env is the child environment. Nil inherits the supervisor's environment.
	Env []string

	// Dir is the child working directory. Empty inherits the supervisor's.
	Dir string

	// Stdout and Stderr receive the frontend's output unchanged, without
	// intermediate buffering. Nil means os.Stdout and os.Stderr respectively.
	Stdout io.Writer
	Stderr io.Writer

	// HandshakeAddress is the engine-core handshake endpoint, either
	// "tcp://host:port" or "host:port". Readiness requires a successful TCP
	// connection to it, which stands for engine registration.
	HandshakeAddress string

	// ReadyFile is written atomically once the frontend is alive and the engine
	// is registered, and removed whenever readiness is lost.
	ReadyFile string

	// HealthAddress is the listen address of the local health endpoint serving
	// /healthz (frontend liveness) and /readyz (readiness).
	HealthAddress string

	// BackendLossGrace is how long the engine may stay unreachable before the
	// frontend is terminated and the container exits non-zero.
	BackendLossGrace time.Duration

	// ShutdownTimeout is the graceful shutdown budget for the frontend. Zero
	// means immediate: SIGTERM is forwarded and SIGKILL follows without
	// waiting, matching a workload that configures vLLM --shutdown-timeout 0.
	ShutdownTimeout time.Duration

	// PollInterval is how often engine registration is probed.
	PollInterval time.Duration

	// Logf logs supervisor decisions. Nil discards them.
	Logf func(format string, args ...any)
}

// Supervisor runs the frontend child and its health endpoint. A Supervisor is
// single-use: call Run once.
type Supervisor struct {
	cfg          Config
	handshake    string
	pollInterval time.Duration
	logf         func(format string, args ...any)

	mu               sync.Mutex
	childAlive       bool
	backendConnected bool
	shuttingDown     bool
	publishedReady   bool
	healthAddr       string
}

// New validates cfg, applies defaults, and returns a Supervisor.
func New(cfg Config) (*Supervisor, error) {
	if len(cfg.Command) == 0 || cfg.Command[0] == "" {
		return nil, errors.New("supervisor: a frontend command is required")
	}
	if cfg.HandshakeAddress == "" {
		cfg.HandshakeAddress = DefaultHandshakeAddress
	}
	handshake, err := ParseHandshakeAddress(cfg.HandshakeAddress)
	if err != nil {
		return nil, err
	}
	if cfg.ReadyFile == "" {
		cfg.ReadyFile = DefaultReadyFile
	}
	if cfg.HealthAddress == "" {
		cfg.HealthAddress = DefaultHealthAddress
	}
	switch {
	case cfg.BackendLossGrace < 0:
		return nil, fmt.Errorf("supervisor: backend loss grace %s must not be negative", cfg.BackendLossGrace)
	case cfg.BackendLossGrace == 0:
		cfg.BackendLossGrace = DefaultBackendLossGrace
	}
	if cfg.ShutdownTimeout < 0 {
		return nil, fmt.Errorf("supervisor: shutdown timeout %s must not be negative", cfg.ShutdownTimeout)
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.Stdout == nil {
		cfg.Stdout = os.Stdout
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Supervisor{
		cfg:          cfg,
		handshake:    handshake,
		pollInterval: cfg.PollInterval,
		logf:         logf,
	}, nil
}

// HealthAddress returns the address the health endpoint is bound to, or "" when
// Run has not started serving yet.
func (s *Supervisor) HealthAddress() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthAddr
}

// Run starts the frontend and supervises it until it exits or ctx is cancelled
// (SIGTERM/SIGINT in the container). The returned code is the process exit code
// the container must use.
func (s *Supervisor) Run(ctx context.Context) (int, error) {
	removeReadyFile(s.cfg.ReadyFile)

	ln, err := net.Listen("tcp", s.cfg.HealthAddress)
	if err != nil {
		return exitCodeFailure, fmt.Errorf("supervisor: listen on health address %q: %w", s.cfg.HealthAddress, err)
	}
	s.mu.Lock()
	s.healthAddr = ln.Addr().String()
	s.mu.Unlock()

	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logf("health endpoint stopped: %v", err)
		}
	}()
	defer func() { _ = srv.Close() }()

	cmd := exec.Command(s.cfg.Command[0], s.cfg.Command[1:]...)
	cmd.Env = s.cfg.Env
	cmd.Dir = s.cfg.Dir
	cmd.Stdout = s.cfg.Stdout
	cmd.Stderr = s.cfg.Stderr
	if err := cmd.Start(); err != nil {
		return exitCodeFailure, fmt.Errorf("supervisor: start frontend %q: %w", strings.Join(s.cfg.Command, " "), err)
	}
	s.logf("started frontend pid %d", cmd.Process.Pid)
	s.setChildAlive(true)

	exitCh := make(chan error, 1)
	go func() { exitCh <- cmd.Wait() }()

	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	events := make(chan watchEvent, 1)
	go s.watchBackend(watchCtx, events)

	for {
		select {
		case err := <-exitCh:
			s.setChildAlive(false)
			if code := exitCode(err); code != 0 {
				return code, fmt.Errorf("supervisor: frontend exited unexpectedly: %w", err)
			}
			return 0, nil

		case <-ctx.Done():
			// Already gone by the time the signal arrived: propagate its code.
			if code, ok := drainExit(exitCh); ok {
				s.setChildAlive(false)
				s.publishReadiness()
				return code, nil
			}
			s.logf("shutdown requested; terminating frontend")
			s.beginShutdown()
			clean := s.stopChild(cmd, exitCh)
			s.setChildAlive(false)
			if clean {
				return 0, nil
			}
			return exitCodeFailure, fmt.Errorf("supervisor: frontend did not exit within the %s shutdown timeout and was killed", s.cfg.ShutdownTimeout)

		case ev := <-events:
			if ev != eventBackendLost {
				continue
			}
			if code, ok := drainExit(exitCh); ok {
				s.setChildAlive(false)
				s.publishReadiness()
				return code, nil
			}
			s.logf("engine at %s unreachable for longer than %s; terminating frontend", s.handshake, s.cfg.BackendLossGrace)
			s.beginShutdown()
			s.stopChild(cmd, exitCh)
			s.setChildAlive(false)
			return exitCodeFailure, fmt.Errorf("supervisor: engine at %s was unreachable for longer than %s", s.handshake, s.cfg.BackendLossGrace)
		}
	}
}

func (s *Supervisor) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if s.isChildAlive() {
			_, _ = io.WriteString(w, "frontend alive\n")
			return
		}
		http.Error(w, "frontend not running", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.isReady() {
			_, _ = io.WriteString(w, "ready\n")
			return
		}
		http.Error(w, s.readinessReason(), http.StatusServiceUnavailable)
	})
	return mux
}

// stopChild forwards SIGTERM, waits up to the shutdown budget, then escalates to
// SIGKILL. It reports whether the child exited without the SIGKILL escalation.
func (s *Supervisor) stopChild(cmd *exec.Cmd, exitCh <-chan error) (clean bool) {
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		s.logf("cannot signal frontend %d: %v", cmd.Process.Pid, err)
	}
	if s.cfg.ShutdownTimeout <= 0 {
		s.logf("shutdown timeout is 0; sending SIGKILL without waiting")
		if err := cmd.Process.Kill(); err != nil {
			s.logf("cannot kill frontend %d: %v", cmd.Process.Pid, err)
		}
		reap(exitCh)
		// The immediate policy was requested and honoured: the container exits
		// cleanly so that a drain does not look like a crash.
		return true
	}
	timer := time.NewTimer(s.cfg.ShutdownTimeout)
	defer timer.Stop()
	select {
	case <-exitCh:
		return true
	case <-timer.C:
		s.logf("frontend did not exit within %s; sending SIGKILL", s.cfg.ShutdownTimeout)
		if err := cmd.Process.Kill(); err != nil {
			s.logf("cannot kill frontend %d: %v", cmd.Process.Pid, err)
		}
		reap(exitCh)
		return false
	}
}

// reap waits for the child to be reaped, bounding the wait so that a wedged
// child cannot hang the container forever.
func reap(exitCh <-chan error) {
	select {
	case <-exitCh:
	case <-time.After(reapTimeout):
	}
}

// drainExit reports the child's exit code if it has already exited.
func drainExit(exitCh <-chan error) (int, bool) {
	select {
	case err := <-exitCh:
		return exitCode(err), true
	default:
		return 0, false
	}
}

type watchEvent int

const (
	eventBackendRegistered watchEvent = iota
	eventBackendLost
)

// watchBackend polls engine registration and publishes readiness. It reports
// eventBackendLost once the engine has been unreachable for longer than the
// configured grace period, then stops.
func (s *Supervisor) watchBackend(ctx context.Context, events chan<- watchEvent) {
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	var connected bool
	var lostSince time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		ok := s.probeBackend()
		if ok != connected {
			connected = ok
			s.setBackendConnected(ok)
			if ok {
				lostSince = time.Time{}
				s.logf("engine registered at %s", s.handshake)
				select {
				case events <- eventBackendRegistered:
				default:
				}
			} else {
				lostSince = time.Now()
				s.logf("engine connection to %s lost", s.handshake)
			}
			continue
		}
		if !ok && !lostSince.IsZero() && time.Since(lostSince) > s.cfg.BackendLossGrace {
			// Deliver the loss without leaking when Run has already stopped.
			select {
			case events <- eventBackendLost:
			case <-ctx.Done():
			}
			return
		}
	}
}

func (s *Supervisor) probeBackend() bool {
	timeout := s.pollInterval
	if timeout > probeTimeout {
		timeout = probeTimeout
	}
	conn, err := net.DialTimeout("tcp", s.handshake, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (s *Supervisor) setBackendConnected(connected bool) {
	s.mu.Lock()
	s.backendConnected = connected
	s.mu.Unlock()
	s.publishReadiness()
}

func (s *Supervisor) setChildAlive(alive bool) {
	s.mu.Lock()
	s.childAlive = alive
	s.mu.Unlock()
	s.publishReadiness()
}

// beginShutdown withdraws readiness before the frontend is signalled, so that
// traffic stops as soon as termination starts.
func (s *Supervisor) beginShutdown() {
	s.mu.Lock()
	s.shuttingDown = true
	s.mu.Unlock()
	s.publishReadiness()
}

func (s *Supervisor) isChildAlive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.childAlive
}

func (s *Supervisor) isReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.childAlive && s.backendConnected && !s.shuttingDown
}

func (s *Supervisor) readinessReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.shuttingDown:
		return "shutting down"
	case !s.childAlive:
		return "frontend not running"
	case !s.backendConnected:
		return "no engine registered on " + s.handshake
	default:
		return "ready"
	}
}

// publishReadiness syncs the ready file with the current readiness state and
// logs transitions.
func (s *Supervisor) publishReadiness() {
	s.mu.Lock()
	ready := s.childAlive && s.backendConnected && !s.shuttingDown
	changed := ready != s.publishedReady
	s.publishedReady = ready
	s.mu.Unlock()
	if !changed {
		return
	}
	if ready {
		if err := writeReadyFile(s.cfg.ReadyFile); err != nil {
			s.logf("cannot write ready file %q: %v", s.cfg.ReadyFile, err)
			return
		}
		s.logf("ready: frontend alive and engine registered")
		return
	}
	reason := s.readinessReason()
	removeReadyFile(s.cfg.ReadyFile)
	s.logf("not ready: %s", reason)
}

// writeReadyFile publishes the readiness marker atomically: the content is
// written to a temporary file in the same directory and renamed into place.
func writeReadyFile(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".vcr-ready-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.WriteString("ready\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func removeReadyFile(path string) {
	if path == "" {
		return
	}
	_ = os.Remove(path)
}

// exitCode maps a child wait result to the container exit code, using the
// shell convention 128+signal for a signal-terminated child.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return exitCodeFailure
	}
	if code := ee.ExitCode(); code >= 0 {
		return code
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return exitCodeFailure
}

// ParseHandshakeAddress normalizes "tcp://host:port" or "host:port" into the
// host:port form the probe dials, rejecting endpoints it cannot observe.
func ParseHandshakeAddress(addr string) (string, error) {
	rest := addr
	if scheme, tail, ok := strings.Cut(addr, "://"); ok {
		if scheme != "tcp" {
			return "", fmt.Errorf("supervisor: handshake address %q uses unsupported scheme %q: only tcp:// endpoints can be observed", addr, scheme)
		}
		rest = tail
	}
	host, port, err := net.SplitHostPort(rest)
	if err != nil {
		return "", fmt.Errorf("supervisor: handshake address %q is not a tcp host:port endpoint: %v", addr, err)
	}
	if host == "" || port == "" {
		return "", fmt.Errorf("supervisor: handshake address %q is not a tcp host:port endpoint", addr)
	}
	return net.JoinHostPort(host, port), nil
}
