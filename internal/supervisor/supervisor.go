// Package supervisor implements the frontend container's entrypoint for a
// simulated model server.
//
// A simulated model-server Pod runs the Python vLLM frontend in the vllm-frontend
// container and the vllm-vcr simulator in its own container (see
// docs/contracts/runtime-layout.md). The supervisor is the frontend container's
// entrypoint: it starts the frontend as its only long-lived child, reports
// readiness from the readiness inputs the contract defines, propagates failures,
// and shuts the container down in a coordinated way.
//
// Readiness requires all three of the following, and any one of them failing
// withdraws it:
//
//  1. the frontend child process is alive;
//  2. the frontend's own HTTP health endpoint (FrontendHealthURL) answers 2xx;
//  3. the simulator's control API (BackendHealthURL) answers 2xx.
//
// Dialing the engine-core handshake address is deliberately not a readiness
// signal: the frontend binds that socket, so a connection to it succeeds while no
// simulator is registered, which would report ready for an engine-less server.
// The handshake address is still passed to the frontend, but the supervisor never
// probes it.
//
// The supervisor never restarts or resurrects the simulator; recovery is
// restart-based, not reconnect-based. When the simulator's control API stays
// unavailable past the backend loss grace, the frontend is terminated so that the
// container restarts instead of serving without an engine.
//
// Environment variables read by cmd/supervisor (docs/contracts/runtime-layout.md):
// VCR_HANDSHAKE_ADDRESS, VCR_FRONTEND_HEALTH_URL, VCR_BACKEND_HEALTH_URL,
// VCR_READY_FILE, and VCR_SHUTDOWN_TIMEOUT (seconds).
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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
	// frontend binds and the simulator connects to. The supervisor passes it to
	// the frontend and does not probe it.
	DefaultHandshakeAddress = fmt.Sprintf("tcp://127.0.0.1:%d", names.HandshakePort)

	// DefaultFrontendHealthURL is the frontend's own health endpoint on the
	// default inference port.
	DefaultFrontendHealthURL = fmt.Sprintf("http://127.0.0.1:%d/health", names.DefaultInferencePort)

	// DefaultBackendHealthURL is the simulator's control API.
	DefaultBackendHealthURL = fmt.Sprintf("http://127.0.0.1:%d/stats", names.ControlPort)

	// DefaultHealthAddress is the endpoint serving /healthz and /readyz for the
	// frontend container. It binds every interface because kubelet probes the
	// Pod's IP, not its loopback.
	DefaultHealthAddress = fmt.Sprintf("0.0.0.0:%d", DefaultHealthPort)
)

const (
	// HealthPath is the liveness path: the frontend child process is alive.
	HealthPath = "/healthz"

	// ReadyPath is the full readiness path: the three readiness inputs hold.
	ReadyPath = "/readyz"

	// DefaultHealthPort is the port the supervisor serves HealthPath and
	// ReadyPath on. The operator points the container's probes at it.
	DefaultHealthPort = 8082

	// DefaultReadyFile is the readiness marker incident diagnostics read.
	DefaultReadyFile = "/tmp/vcr-runtime/ready"

	// DefaultBackendLossGrace is how long the simulator control API may stay
	// unhealthy before the frontend is terminated.
	DefaultBackendLossGrace = 5 * time.Second

	// DefaultHealthPollInterval is how often each readiness input is probed.
	DefaultHealthPollInterval = 100 * time.Millisecond

	// exitCodeFailure is the container exit code used when the supervisor
	// itself forces a restart.
	exitCodeFailure = 1

	// reapTimeout bounds how long the supervisor waits for a SIGKILLed child to
	// be reaped, so that a wedged child cannot hang the container.
	reapTimeout = 10 * time.Second

	// healthProbeTimeout bounds a single health request. It is much shorter than
	// a kubelet probe timeout so that a hung endpoint cannot stall the poll loop
	// for long, and it is cancelled with the supervisor context so shutdown never
	// waits on an outstanding request.
	healthProbeTimeout = time.Second
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

	// HandshakeAddress is the engine-core handshake endpoint the frontend binds,
	// either "tcp://host:port" or "host:port". The supervisor passes it through
	// and never probes it: a connection to the socket the frontend itself binds
	// says nothing about whether an engine is registered.
	HandshakeAddress string

	// FrontendHealthURL is the frontend's own HTTP health endpoint on the
	// inference port. It must answer 2xx for the Pod to be ready.
	FrontendHealthURL string

	// BackendHealthURL is the simulator's control API URL. It must answer 2xx
	// for the Pod to be ready.
	BackendHealthURL string

	// ReadyFile is written atomically while all readiness inputs hold, and
	// removed as soon as one of them fails.
	ReadyFile string

	// HealthAddress is the listen address of the local health endpoint serving
	// HealthPath (frontend liveness) and ReadyPath (full readiness).
	HealthAddress string

	// BackendLossGrace is how long the simulator control API may stay unhealthy
	// before the frontend is terminated and the container exits non-zero.
	BackendLossGrace time.Duration

	// ShutdownTimeout is the graceful shutdown budget for the frontend. Zero
	// means immediate: SIGTERM is forwarded and SIGKILL follows without
	// waiting, matching a workload that configures vLLM --shutdown-timeout 0.
	ShutdownTimeout time.Duration

	// HealthPollInterval is how often each readiness input is probed.
	HealthPollInterval time.Duration

	// Logf logs supervisor decisions. Nil discards them.
	Logf func(format string, args ...any)
}

// Supervisor runs the frontend child and its health endpoint. A Supervisor is
// single-use: call Run once.
type Supervisor struct {
	cfg                Config
	frontendHealthURL  string
	backendHealthURL   string
	healthPollInterval time.Duration
	client             *http.Client
	logf               func(format string, args ...any)

	mu             sync.Mutex
	childAlive     bool
	frontendHealth bool
	backendHealth  bool
	backendLostAt  time.Time
	shuttingDown   bool
	publishedReady bool
	healthAddr     string
}

// New validates cfg, applies defaults, and returns a Supervisor.
func New(cfg Config) (*Supervisor, error) {
	if len(cfg.Command) == 0 || cfg.Command[0] == "" {
		return nil, errors.New("supervisor: a frontend command is required")
	}
	if cfg.HandshakeAddress == "" {
		cfg.HandshakeAddress = DefaultHandshakeAddress
	}
	if cfg.FrontendHealthURL == "" {
		cfg.FrontendHealthURL = DefaultFrontendHealthURL
	}
	if err := validateHealthURL("frontend health URL", cfg.FrontendHealthURL); err != nil {
		return nil, err
	}
	if cfg.BackendHealthURL == "" {
		cfg.BackendHealthURL = DefaultBackendHealthURL
	}
	if err := validateHealthURL("backend health URL", cfg.BackendHealthURL); err != nil {
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
	switch {
	case cfg.HealthPollInterval < 0:
		return nil, fmt.Errorf("supervisor: health poll interval %s must not be negative", cfg.HealthPollInterval)
	case cfg.HealthPollInterval == 0:
		cfg.HealthPollInterval = DefaultHealthPollInterval
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
		cfg:                cfg,
		frontendHealthURL:  cfg.FrontendHealthURL,
		backendHealthURL:   cfg.BackendHealthURL,
		healthPollInterval: cfg.HealthPollInterval,
		client: &http.Client{
			Timeout: healthProbeTimeout,
			// A redirect is not a 2xx answer, and following one would probe a
			// different endpoint than the configured one.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		logf: logf,
	}, nil
}

// validateHealthURL rejects a readiness input the supervisor cannot interpret.
func validateHealthURL(setting, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("supervisor: %s %q is not a URL: %v", setting, raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("supervisor: %s %q uses scheme %q: only http and https endpoints can be probed", setting, raw, parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("supervisor: %s %q has no host, so it cannot be probed", setting, raw)
	}
	return nil
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
	go s.watchHealth(watchCtx, "frontend health", s.frontendHealthURL, s.setFrontendHealthy)
	go s.watchBackendLoss(watchCtx, events)

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
				return code, nil
			}
			s.logf("simulator control API at %s unhealthy for longer than %s; terminating frontend", s.backendHealthURL, s.cfg.BackendLossGrace)
			s.beginShutdown()
			s.stopChild(cmd, exitCh)
			s.setChildAlive(false)
			return exitCodeFailure, fmt.Errorf("supervisor: simulator control API at %s was unhealthy for longer than %s", s.backendHealthURL, s.cfg.BackendLossGrace)
		}
	}
}

func (s *Supervisor) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, func(w http.ResponseWriter, r *http.Request) {
		if s.isChildAlive() {
			_, _ = io.WriteString(w, "frontend alive\n")
			return
		}
		http.Error(w, "frontend not running", http.StatusServiceUnavailable)
	})
	mux.HandleFunc(ReadyPath, func(w http.ResponseWriter, r *http.Request) {
		ready, reason := s.readiness()
		if !ready {
			http.Error(w, reason, http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ready\n")
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
	eventBackendLost watchEvent = iota
)

// watchHealth probes one readiness input on every poll interval and publishes the
// result. Each input has its own loop so that a hung endpoint cannot delay the
// others.
func (s *Supervisor) watchHealth(ctx context.Context, setting, healthURL string, apply func(bool)) {
	ticker := time.NewTicker(s.healthPollInterval)
	defer ticker.Stop()
	for {
		apply(s.probe(ctx, setting, healthURL))
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// watchBackendLoss probes the simulator control API and reports eventBackendLost
// once it has been unhealthy for longer than the configured grace period.
func (s *Supervisor) watchBackendLoss(ctx context.Context, events chan<- watchEvent) {
	ticker := time.NewTicker(s.healthPollInterval)
	defer ticker.Stop()
	for {
		healthy := s.probe(ctx, "simulator control API", s.backendHealthURL)
		s.setBackendHealthy(healthy)

		s.mu.Lock()
		lostAt, wasLost := s.backendLostAt, !s.backendLostAt.IsZero()
		s.mu.Unlock()
		if wasLost && time.Since(lostAt) > s.cfg.BackendLossGrace {
			// Deliver the loss without leaking when Run has already stopped.
			select {
			case events <- eventBackendLost:
			case <-ctx.Done():
			}
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// probe reports whether healthURL answers 2xx. The request is bounded by a short
// timeout and by ctx, so neither a hung endpoint nor an outstanding request can
// stall the poll loop or shutdown.
func (s *Supervisor) probe(ctx context.Context, setting, healthURL string) bool {
	reqCtx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, healthURL, nil)
	if err != nil {
		s.logf("%s: cannot build a request for %s: %v", setting, healthURL, err)
		return false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (s *Supervisor) setFrontendHealthy(healthy bool) {
	s.mu.Lock()
	changed := s.frontendHealth != healthy
	s.frontendHealth = healthy
	s.mu.Unlock()
	if changed {
		s.logf("frontend health at %s: %s", s.frontendHealthURL, describeHealth(healthy))
	}
	s.publishReadiness()
}

func (s *Supervisor) setBackendHealthy(healthy bool) {
	s.mu.Lock()
	changed := s.backendHealth != healthy
	s.backendHealth = healthy
	if healthy {
		s.backendLostAt = time.Time{}
	} else if s.backendLostAt.IsZero() {
		s.backendLostAt = time.Now()
	}
	s.mu.Unlock()
	if changed {
		s.logf("simulator control API at %s: %s", s.backendHealthURL, describeHealth(healthy))
	}
	s.publishReadiness()
}

func describeHealth(healthy bool) string {
	if healthy {
		return "healthy"
	}
	return "unhealthy"
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

// readiness reports the current readiness state and, when not ready, the input
// that fails.
func (s *Supervisor) readiness() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readinessLocked()
}

// readinessLocked is readiness for callers that already hold the lock, so that a
// decision and the state it is based on cannot be observed apart.
func (s *Supervisor) readinessLocked() (bool, string) {
	switch {
	case s.shuttingDown:
		return false, "shutting down"
	case !s.childAlive:
		return false, "frontend not running"
	case !s.frontendHealth:
		return false, "frontend health at " + s.frontendHealthURL + " is not 2xx"
	case !s.backendHealth:
		return false, "simulator control API at " + s.backendHealthURL + " is not 2xx"
	default:
		return true, "ready"
	}
}

// publishReadiness syncs the ready file with the current readiness state and
// logs transitions.
func (s *Supervisor) publishReadiness() {
	s.mu.Lock()
	ready, reason := s.readinessLocked()
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
		s.logf("ready: frontend alive, frontend health and simulator control API answered")
		return
	}
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
