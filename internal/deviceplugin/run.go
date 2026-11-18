package deviceplugin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// Run serves the device plugin on the configured unix socket, registers with
// kubelet, and watches the kubelet socket and device-count file until ctx is
// canceled. Startup failures (unusable socket directory, unreachable kubelet)
// are returned immediately so the DaemonSet crash-loops visibly.
func (p *Plugin) Run(ctx context.Context) error {
	p.shutdownMu.Lock()
	alreadyStopped := p.shuttingDown
	p.shutdownMu.Unlock()
	if alreadyStopped {
		return fmt.Errorf("device plugin was already shut down before Run")
	}

	if err := p.start(); err != nil {
		p.shutdownNow()
		return err
	}

	socketPath := filepath.Join(p.opts.SocketDir, p.opts.Endpoint)
	p.grpcServer = grpc.NewServer()
	p.RegisterService(p.grpcServer)
	serveErr := make(chan error, 1)
	go func() {
		p.logger.Info("serving synthetic GPU device plugin", "socket", socketPath, "resource", p.opts.ResourceName, "devices", p.DeviceCount())
		serveErr <- p.grpcServer.Serve(p.listener)
	}()

	kubeletIdentity := identifySocket(p.opts.KubeletSocket)
	if err := p.register(ctx); err != nil {
		p.shutdownNow()
		return fmt.Errorf("register with kubelet at %q: %w", p.opts.KubeletSocket, err)
	}
	p.registered.Store(true)
	p.logger.Info("registered with kubelet",
		"kubelet_socket", p.opts.KubeletSocket,
		"endpoint", p.opts.Endpoint,
		"resource", p.opts.ResourceName,
		"devices", p.DeviceCount(),
	)

	watchCtx, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	p.watchKubelet(watchCtx, identifySocketAfter(kubeletIdentity, p.opts.KubeletSocket))
	p.watchDeviceCountFile(watchCtx)

	var runErr error
	select {
	case <-ctx.Done():
		p.logger.Info("shutdown requested; draining in-flight allocations")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			runErr = fmt.Errorf("device plugin gRPC server failed: %w", err)
		}
	}

	stopWatching()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), p.opts.ShutdownTimeout)
	defer cancel()
	if err := p.Shutdown(shutdownCtx); err != nil {
		p.logger.Warn("graceful shutdown did not complete", "error", err)
	}
	p.watchers.Wait()
	return runErr
}

// Shutdown stops advertising devices after in-flight allocations finish and
// removes the plugin socket. It is safe to call more than once.
func (p *Plugin) Shutdown(ctx context.Context) error {
	p.shutdownMu.Lock()
	if !p.shuttingDown {
		p.shuttingDown = true
		close(p.closing)
	}
	p.shutdownMu.Unlock()

	p.stopOnce.Do(func() {
		go p.stop(ctx)
	})

	select {
	case <-p.stopDone:
		return p.stopErr
	case <-ctx.Done():
		return fmt.Errorf("waiting for allocations and socket shutdown: %w", ctx.Err())
	}
}

// shutdownNow performs best-effort cleanup when Run fails before serving.
func (p *Plugin) shutdownNow() {
	ctx, cancel := context.WithTimeout(context.Background(), p.opts.ShutdownTimeout)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		p.logger.Warn("cleanup after failed startup did not complete", "error", err)
	}
}

func (p *Plugin) start() error {
	// The directory is provided by the DaemonSet hostPath. Do not create it:
	// a missing or unwritable directory must fail loudly instead of being
	// papered over, and the plugin must not write outside the configured path.
	info, err := os.Stat(p.opts.SocketDir)
	if err != nil {
		return fmt.Errorf("socket directory %q is not usable: %w", p.opts.SocketDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("socket directory %q is not a directory", p.opts.SocketDir)
	}

	if err := p.applyInitialDeviceCount(); err != nil {
		return err
	}

	socketPath := filepath.Join(p.opts.SocketDir, p.opts.Endpoint)
	// A crashed previous instance may leave the socket behind. Remove only the
	// endpoint inside the configured directory.
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("socket directory %q is not writable: %w", p.opts.SocketDir, err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("socket directory %q is not usable: %w", p.opts.SocketDir, err)
	}
	p.listener = listener
	p.socketPath = socketPath

	if p.opts.HealthPort > 0 {
		healthListener, err := net.Listen("tcp", fmt.Sprintf(":%d", p.opts.HealthPort))
		if err != nil {
			return fmt.Errorf("health port %d is not usable: %w", p.opts.HealthPort, err)
		}
		p.healthServer = &http.Server{Handler: p.HealthHandler(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := p.healthServer.Serve(healthListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				p.logger.Error("health endpoint failed", "error", err)
			}
		}()
	}
	return nil
}

func (p *Plugin) stop(ctx context.Context) {
	defer close(p.stopDone)
	var stopErr error

	if p.listener != nil {
		p.registered.Store(false)
	}
	// Let allocations that already entered Allocate finish before the gRPC
	// server goes away.
	if err := p.waitForAllocations(ctx); err != nil {
		p.logger.Warn("in-flight allocations did not finish before shutdown", "error", err)
	}
	if p.grpcServer != nil {
		stopped := make(chan struct{})
		go func() {
			p.grpcServer.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-ctx.Done():
			p.grpcServer.Stop()
			<-stopped
		}
	}
	if p.healthServer != nil {
		healthCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := p.healthServer.Shutdown(healthCtx); err != nil {
			_ = p.healthServer.Close()
		}
		cancel()
	}
	if p.socketPath != "" {
		if err := os.Remove(p.socketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			stopErr = fmt.Errorf("remove device plugin socket %q: %w", p.socketPath, err)
			p.logger.Warn("could not remove device plugin socket", "socket", p.socketPath, "error", err)
		}
	}
	p.stopErr = stopErr
	p.logger.Info("device plugin stopped", "socket", p.socketPath)
}

func (p *Plugin) waitForAllocations(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		p.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// register performs one kubelet registration using the shared version and the
// configured endpoint and resource name.
func (p *Plugin) register(ctx context.Context) error {
	registerCtx, cancel := context.WithTimeout(ctx, p.opts.RegisterTimeout)
	defer cancel()

	if p.opts.DialKubelet != nil {
		conn, err := p.opts.DialKubelet(p.opts.KubeletSocket)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		return p.callRegister(registerCtx, conn)
	}

	conn, err := grpc.NewClient("unix://"+p.opts.KubeletSocket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return p.callRegister(registerCtx, conn)
}

func (p *Plugin) callRegister(ctx context.Context, conn grpc.ClientConnInterface) error {
	_, err := pluginapi.NewRegistrationClient(conn).Register(ctx, &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     p.opts.Endpoint,
		ResourceName: p.opts.ResourceName,
	})
	return err
}

// watchKubelet re-registers whenever kubelet recreates its registration socket.
// kubelet removes the socket when it restarts, so a disappearance followed by a
// reappearance means the new kubelet does not know about this plugin.
func (p *Plugin) watchKubelet(ctx context.Context, initial socketIdentity) {
	p.watchers.Add(1)
	go func() {
		defer p.watchers.Done()
		ticker := time.NewTicker(p.opts.PollInterval)
		defer ticker.Stop()
		current := initial
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			next := identifySocket(p.opts.KubeletSocket)
			switch {
			case !next.exists:
				if current.exists {
					p.registered.Store(false)
					p.logger.Warn("kubelet device plugin socket disappeared; waiting for kubelet to restart", "kubelet_socket", p.opts.KubeletSocket)
				}
				current = next
			case !current.exists || !current.same(next):
				p.logger.Info("kubelet device plugin socket restarted; re-registering", "kubelet_socket", p.opts.KubeletSocket)
				if err := p.register(ctx); err != nil {
					p.logger.Error("re-registration with kubelet failed; retrying", "kubelet_socket", p.opts.KubeletSocket, "error", err)
					current = socketIdentity{}
					continue
				}
				p.registered.Store(true)
				p.logger.Info("re-registered with kubelet", "endpoint", p.opts.Endpoint, "resource", p.opts.ResourceName, "devices", p.DeviceCount())
				current = next
			}
		}
	}()
}

// watchDeviceCountFile applies operator-written device count changes to the
// live ListAndWatch stream. Invalid intermediate contents are logged and
// ignored; the last valid count stays advertised.
func (p *Plugin) watchDeviceCountFile(ctx context.Context) {
	if p.opts.DeviceCountFile == "" {
		return
	}
	p.watchers.Add(1)
	go func() {
		defer p.watchers.Done()
		ticker := time.NewTicker(p.opts.PollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			count, found, err := readDeviceCountFile(p.opts.DeviceCountFile)
			if err != nil {
				p.logger.Error("ignoring invalid device count file", "file", p.opts.DeviceCountFile, "error", err)
				continue
			}
			if !found || count == p.DeviceCount() {
				continue
			}
			if err := p.SetDeviceCount(count); err != nil {
				p.logger.Error("ignoring unusable device count", "file", p.opts.DeviceCountFile, "error", err)
			}
		}
	}()
}

// applyInitialDeviceCount makes the operator-written file authoritative at
// startup. A missing file is not an error; malformed content is.
func (p *Plugin) applyInitialDeviceCount() error {
	if p.opts.DeviceCountFile == "" {
		return nil
	}
	count, found, err := readDeviceCountFile(p.opts.DeviceCountFile)
	if err != nil {
		return fmt.Errorf("device count file %q: %w", p.opts.DeviceCountFile, err)
	}
	if !found {
		return nil
	}
	if err := p.SetDeviceCount(count); err != nil {
		return fmt.Errorf("device count file %q: %w", p.opts.DeviceCountFile, err)
	}
	return nil
}

// socketIdentity is what tells a restarted kubelet apart from the socket we
// already registered with. kubelet recreates the file on restart, which changes
// the inode even when the path never appears absent to a poller.
type socketIdentity struct {
	exists  bool
	inode   uint64
	modTime time.Time
}

func identifySocket(path string) socketIdentity {
	info, err := os.Stat(path)
	if err != nil {
		return socketIdentity{}
	}
	identity := socketIdentity{exists: true, modTime: info.ModTime()}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		identity.inode = stat.Ino
	}
	return identity
}

func (s socketIdentity) same(other socketIdentity) bool {
	return s.exists == other.exists && s.inode == other.inode && s.modTime.Equal(other.modTime)
}

func (p *Plugin) isShuttingDown() bool {
	p.shutdownMu.Lock()
	defer p.shutdownMu.Unlock()
	return p.shuttingDown
}

// identifySocketAfter prefers a fresh identity taken after registration, but
// falls back to the pre-registration identity if the socket briefly vanishes so
// that the watcher does not miss a restart.
func identifySocketAfter(before socketIdentity, path string) socketIdentity {
	if current := identifySocket(path); current.exists {
		return current
	}
	return before
}

func readDeviceCountFile(path string) (count int, found bool, err error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	value := strings.TrimSpace(string(content))
	count, err = strconv.Atoi(value)
	if err != nil {
		return 0, false, fmt.Errorf("content %q is not an integer", value)
	}
	if count < minDeviceCount || count > maxDeviceCount {
		return 0, false, fmt.Errorf("device count %d is outside the supported range %d..%d", count, minDeviceCount, maxDeviceCount)
	}
	return count, true, nil
}

// HealthHandler exposes liveness (/healthz) and kubelet-registration readiness
// (/readyz) for the DaemonSet probes.
func (p *Plugin) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc(ReadyPath, func(w http.ResponseWriter, _ *http.Request) {
		if !p.registered.Load() || p.isShuttingDown() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not registered with kubelet or shutting down\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	return mux
}
