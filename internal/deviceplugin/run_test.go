package deviceplugin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
)

const runTestTimeout = 5 * time.Second

type fakeKubelet struct {
	pluginapi.UnimplementedRegistrationServer

	mu       sync.Mutex
	requests []*pluginapi.RegisterRequest
	notify   chan *pluginapi.RegisterRequest
}

func (f *fakeKubelet) Register(_ context.Context, req *pluginapi.RegisterRequest) (*pluginapi.Empty, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	select {
	case f.notify <- req:
	default:
	}
	return &pluginapi.Empty{}, nil
}

func (f *fakeKubelet) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeKubelet) waitRegister(t *testing.T) *pluginapi.RegisterRequest {
	t.Helper()
	select {
	case req := <-f.notify:
		return req
	case <-time.After(runTestTimeout):
		t.Fatal("timed out waiting for kubelet registration")
		return nil
	}
}

// startFakeKubelet serves the kubelet registration API over bufconn and returns
// a dial function so tests never need the real kubelet socket path.
func startFakeKubelet(t *testing.T) (*fakeKubelet, DialKubeletFunc) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	fake := &fakeKubelet{notify: make(chan *pluginapi.RegisterRequest, 8)}
	pluginapi.RegisterRegistrationServer(srv, fake)
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(srv.Stop)
	return fake, func(string) (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///fake-kubelet",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
	}
}

type pluginRun struct {
	done   chan struct{}
	cancel context.CancelFunc
	mu     sync.Mutex
	err    error
}

func (r *pluginRun) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	return r.wait(t)
}

func (r *pluginRun) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-r.done:
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.err
	case <-time.After(runTestTimeout):
		t.Fatal("plugin did not stop after context cancellation")
		return nil
	}
}

func runPlugin(t *testing.T, p *Plugin) *pluginRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	run := &pluginRun{done: make(chan struct{}), cancel: cancel}
	go func() {
		err := p.Run(ctx)
		run.mu.Lock()
		run.err = err
		run.mu.Unlock()
		close(run.done)
	}()
	t.Cleanup(func() {
		cancel()
		run.wait(t)
	})
	return run
}

// testPaths builds a socket directory plus a kubelet socket file outside it,
// mirroring the real layout while keeping every path inside t.TempDir().
func testPaths(t *testing.T) (socketDir, kubeletSocket, countFile string) {
	t.Helper()
	root := t.TempDir()
	socketDir = filepath.Join(root, "device-plugins")
	if err := os.MkdirAll(socketDir, 0o755); err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	kubeletSocket = filepath.Join(root, "kubelet.sock")
	if err := os.WriteFile(kubeletSocket, nil, 0o600); err != nil {
		t.Fatalf("create kubelet socket: %v", err)
	}
	countFile = filepath.Join(root, "device-count")
	return socketDir, kubeletSocket, countFile
}

func dialPluginSocket(t *testing.T, socketPath string) pluginapi.DevicePluginClient {
	t.Helper()
	conn, err := grpc.NewClient("unix://"+socketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial plugin socket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pluginapi.NewDevicePluginClient(conn)
}

func TestRunServesSocketAndRegistersWithKubelet(t *testing.T) {
	socketDir, kubeletSocket, countFile := testPaths(t)
	if err := os.WriteFile(countFile, []byte("3\n"), 0o600); err != nil {
		t.Fatalf("write device count file: %v", err)
	}
	fake, dial := startFakeKubelet(t)
	p, err := New(Options{
		DeviceCount:     1,
		SocketDir:       socketDir,
		KubeletSocket:   kubeletSocket,
		DeviceCountFile: countFile,
		PollInterval:    10 * time.Millisecond,
		DialKubelet:     dial,
		Logger:          testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	run := runPlugin(t, p)

	req := fake.waitRegister(t)
	waitRegistered(t, p)
	if req.ResourceName != names.SyntheticResourceName {
		t.Errorf("registration resource name = %q, want %q", req.ResourceName, names.SyntheticResourceName)
	}
	if req.Endpoint != names.DevicePluginSocketName {
		t.Errorf("registration endpoint = %q, want %q", req.Endpoint, names.DevicePluginSocketName)
	}
	if req.Version != pluginapi.Version {
		t.Errorf("registration version = %q, want %q", req.Version, pluginapi.Version)
	}

	socketPath := filepath.Join(socketDir, names.DevicePluginSocketName)
	client := dialPluginSocket(t, socketPath)
	ctx, streamCancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer streamCancel()
	stream, err := client.ListAndWatch(ctx, &pluginapi.Empty{})
	if err != nil {
		t.Fatalf("ListAndWatch over plugin socket: %v", err)
	}
	// The count file is authoritative at startup: it overrides --device-count.
	assertIDs(t, recvDevices(t, stream), []string{"vcr-gpu-0", "vcr-gpu-1", "vcr-gpu-2"})

	if err := run.stop(t); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if _, err := os.Stat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plugin socket still present after shutdown: %v", err)
	}
}

func TestRunFailsFastWhenSocketDirIsNotADirectory(t *testing.T) {
	root := t.TempDir()
	socketDir := filepath.Join(root, "device-plugins")
	if err := os.WriteFile(socketDir, nil, 0o600); err != nil {
		t.Fatalf("create socket dir placeholder: %v", err)
	}
	p, err := New(Options{
		DeviceCount:   1,
		SocketDir:     socketDir,
		KubeletSocket: filepath.Join(root, "kubelet.sock"),
		PollInterval:  10 * time.Millisecond,
		Logger:        testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = p.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded with an unusable socket directory, want error")
	}
	if !strings.Contains(err.Error(), socketDir) {
		t.Fatalf("Run error = %v, want it to name socket directory %q", err, socketDir)
	}
}

func TestRunFailsFastWhenKubeletIsUnreachable(t *testing.T) {
	socketDir, _, countFile := testPaths(t)
	kubeletSocket := filepath.Join(t.TempDir(), "missing-kubelet.sock")
	p, err := New(Options{
		DeviceCount:     1,
		SocketDir:       socketDir,
		KubeletSocket:   kubeletSocket,
		DeviceCountFile: countFile,
		PollInterval:    10 * time.Millisecond,
		RegisterTimeout: 200 * time.Millisecond,
		Logger:          testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := time.Now()
	err = p.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded with an unreachable kubelet, want error")
	}
	if !strings.Contains(err.Error(), kubeletSocket) {
		t.Fatalf("Run error = %v, want it to name kubelet socket %q", err, kubeletSocket)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Run took %s to fail, want fail-fast behavior", elapsed)
	}
	if _, statErr := os.Stat(filepath.Join(socketDir, names.DevicePluginSocketName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("plugin socket left behind after failed startup: %v", statErr)
	}
}

func TestRunReRegistersAfterKubeletRestart(t *testing.T) {
	socketDir, kubeletSocket, countFile := testPaths(t)
	fake, dial := startFakeKubelet(t)
	p, err := New(Options{
		DeviceCount:     2,
		SocketDir:       socketDir,
		KubeletSocket:   kubeletSocket,
		DeviceCountFile: countFile,
		PollInterval:    10 * time.Millisecond,
		DialKubelet:     dial,
		Logger:          testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runPlugin(t, p)
	fake.waitRegister(t)
	waitRegistered(t, p)

	// kubelet restart: the socket disappears, then a new kubelet recreates it.
	if err := os.Remove(kubeletSocket); err != nil {
		t.Fatalf("remove kubelet socket: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := fake.requestCount(); got != 1 {
		t.Fatalf("registrations while kubelet was absent = %d, want 1", got)
	}

	if err := os.WriteFile(kubeletSocket, nil, 0o600); err != nil {
		t.Fatalf("recreate kubelet socket: %v", err)
	}
	req := fake.waitRegister(t)
	if req.ResourceName != names.SyntheticResourceName || req.Endpoint != names.DevicePluginSocketName {
		t.Fatalf("re-registration payload = %+v, want resource %q endpoint %q", req, names.SyntheticResourceName, names.DevicePluginSocketName)
	}
	if got := fake.requestCount(); got != 2 {
		t.Fatalf("registrations after kubelet restart = %d, want 2", got)
	}

	// A restart that replaces the socket without the poller ever observing it
	// absent (new inode) is also a restart and must re-register.
	if err := os.Remove(kubeletSocket); err != nil {
		t.Fatalf("remove kubelet socket: %v", err)
	}
	if err := os.WriteFile(kubeletSocket, nil, 0o600); err != nil {
		t.Fatalf("recreate kubelet socket: %v", err)
	}
	fake.waitRegister(t)
	if got := fake.requestCount(); got != 3 {
		t.Fatalf("registrations after in-place kubelet restart = %d, want 3", got)
	}
}

func TestRunTouchesOnlyConfiguredPaths(t *testing.T) {
	socketDir, kubeletSocket, countFile := testPaths(t)
	if err := os.WriteFile(countFile, []byte("2"), 0o600); err != nil {
		t.Fatalf("write device count file: %v", err)
	}
	fake, dial := startFakeKubelet(t)
	p, err := New(Options{
		DeviceCount:     1,
		SocketDir:       socketDir,
		KubeletSocket:   kubeletSocket,
		DeviceCountFile: countFile,
		PollInterval:    10 * time.Millisecond,
		DialKubelet:     dial,
		Logger:          testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	run := runPlugin(t, p)
	fake.waitRegister(t)
	waitRegistered(t, p)

	entries, err := os.ReadDir(socketDir)
	if err != nil {
		t.Fatalf("read socket dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != names.DevicePluginSocketName {
		t.Fatalf("socket dir entries = %v, want only %q", entries, names.DevicePluginSocketName)
	}

	if err := run.stop(t); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	entries, err = os.ReadDir(socketDir)
	if err != nil {
		t.Fatalf("read socket dir after shutdown: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("socket dir entries after shutdown = %v, want none", entries)
	}
	if got, err := os.ReadFile(countFile); err != nil || string(got) != "2" {
		t.Fatalf("device count file changed: content=%q err=%v", got, err)
	}
	if _, err := os.Stat(kubeletSocket); err != nil {
		t.Fatalf("kubelet socket path changed: %v", err)
	}
}

// waitRegistered waits until the plugin has completed registration from its own
// point of view. The fake kubelet is notified inside its handler, before the
// gRPC response reaches the plugin.
func waitRegistered(t *testing.T, p *Plugin) {
	t.Helper()
	waitFor(t, func() bool {
		rec := httptest.NewRecorder()
		p.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec.Code == http.StatusOK
	})
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(runTestTimeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func TestRunAppliesLiveDeviceCountFileChanges(t *testing.T) {
	socketDir, kubeletSocket, countFile := testPaths(t)
	if err := os.WriteFile(countFile, []byte("2"), 0o600); err != nil {
		t.Fatalf("write device count file: %v", err)
	}
	fake, dial := startFakeKubelet(t)
	p, err := New(Options{
		DeviceCount:     1,
		SocketDir:       socketDir,
		KubeletSocket:   kubeletSocket,
		DeviceCountFile: countFile,
		PollInterval:    10 * time.Millisecond,
		DialKubelet:     dial,
		Logger:          testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runPlugin(t, p)
	fake.waitRegister(t)
	waitRegistered(t, p)

	client := dialPluginSocket(t, filepath.Join(socketDir, names.DevicePluginSocketName))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.ListAndWatch(ctx, &pluginapi.Empty{})
	if err != nil {
		t.Fatalf("ListAndWatch: %v", err)
	}
	assertIDs(t, recvDevices(t, stream), []string{"vcr-gpu-0", "vcr-gpu-1"})

	if err := os.WriteFile(countFile, []byte("4\n"), 0o600); err != nil {
		t.Fatalf("update device count file: %v", err)
	}
	assertIDs(t, recvDevices(t, stream), []string{"vcr-gpu-0", "vcr-gpu-1", "vcr-gpu-2", "vcr-gpu-3"})

	// Invalid content mid-flight is rejected, not approximated: the next update
	// the stream sees is the next valid count, never a value derived from the
	// invalid file.
	if err := os.WriteFile(countFile, []byte("a lot"), 0o600); err != nil {
		t.Fatalf("write invalid device count file: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	if err := os.WriteFile(countFile, []byte("6"), 0o600); err != nil {
		t.Fatalf("update device count file: %v", err)
	}
	assertIDs(t, recvDevices(t, stream), []string{
		"vcr-gpu-0", "vcr-gpu-1", "vcr-gpu-2", "vcr-gpu-3", "vcr-gpu-4", "vcr-gpu-5",
	})
}

func TestRunFailsFastOnInvalidDeviceCountFile(t *testing.T) {
	socketDir, kubeletSocket, countFile := testPaths(t)
	if err := os.WriteFile(countFile, []byte("not-a-count"), 0o600); err != nil {
		t.Fatalf("write device count file: %v", err)
	}
	p, err := New(Options{
		DeviceCount:     1,
		SocketDir:       socketDir,
		KubeletSocket:   kubeletSocket,
		DeviceCountFile: countFile,
		PollInterval:    10 * time.Millisecond,
		Logger:          testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = p.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded with an invalid device count file, want error")
	}
	if !strings.Contains(err.Error(), countFile) {
		t.Fatalf("Run error = %v, want it to name %q", err, countFile)
	}
}

func TestHealthHandlerReportsKubeletRegistration(t *testing.T) {
	socketDir, kubeletSocket, countFile := testPaths(t)
	fake, dial := startFakeKubelet(t)
	p, err := New(Options{
		DeviceCount:     1,
		SocketDir:       socketDir,
		KubeletSocket:   kubeletSocket,
		DeviceCountFile: countFile,
		PollInterval:    10 * time.Millisecond,
		DialKubelet:     dial,
		Logger:          testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	check := func(path string, wantCode int) {
		t.Helper()
		rec := httptest.NewRecorder()
		p.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != wantCode {
			t.Fatalf("%s status = %d, want %d (body %q)", path, rec.Code, wantCode, rec.Body.String())
		}
	}

	check("/healthz", http.StatusOK)
	check("/readyz", http.StatusServiceUnavailable)

	run := runPlugin(t, p)
	fake.waitRegister(t)
	waitRegistered(t, p)

	if err := run.stop(t); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	check("/readyz", http.StatusServiceUnavailable)
}

func TestShutdownRejectsNewAllocations(t *testing.T) {
	p := newTestPlugin(t, 1)
	req := &pluginapi.AllocateRequest{ContainerRequests: []*pluginapi.ContainerAllocateRequest{{DevicesIds: []string{"vcr-gpu-0"}}}}

	if _, err := p.Allocate(context.Background(), req); err != nil {
		t.Fatalf("Allocate before shutdown: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := p.Allocate(context.Background(), req); err == nil {
		t.Fatal("Allocate after shutdown succeeded, want rejection")
	}
	// Shutdown is idempotent.
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

func TestShutdownDrainsConcurrentAllocations(t *testing.T) {
	socketDir, kubeletSocket, countFile := testPaths(t)
	fake, dial := startFakeKubelet(t)
	p, err := New(Options{
		DeviceCount:     4,
		SocketDir:       socketDir,
		KubeletSocket:   kubeletSocket,
		DeviceCountFile: countFile,
		PollInterval:    10 * time.Millisecond,
		DialKubelet:     dial,
		Logger:          testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	run := runPlugin(t, p)
	fake.waitRegister(t)
	waitRegistered(t, p)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var unexpected []error
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, err := p.Allocate(context.Background(), &pluginapi.AllocateRequest{
					ContainerRequests: []*pluginapi.ContainerAllocateRequest{{DevicesIds: []string{"vcr-gpu-0"}}},
				})
				if err != nil && !strings.Contains(err.Error(), "shutting down") {
					mu.Lock()
					unexpected = append(unexpected, err)
					mu.Unlock()
					return
				}
			}
		}()
	}
	// Let some allocations enter before shutdown starts.
	time.Sleep(5 * time.Millisecond)
	if err := run.stop(t); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	waitDone := make(chan struct{})
	go func() { wg.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(runTestTimeout):
		t.Fatal("concurrent allocations did not finish after shutdown")
	}
	if len(unexpected) > 0 {
		t.Fatalf("unexpected allocation errors during shutdown: %v", unexpected)
	}
}

func TestRunFailsFastWhenSocketDirIsMissing(t *testing.T) {
	root := t.TempDir()
	socketDir := filepath.Join(root, "absent")
	p, err := New(Options{
		DeviceCount:   1,
		SocketDir:     socketDir,
		KubeletSocket: filepath.Join(root, "kubelet.sock"),
		Logger:        testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = p.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded with a missing socket directory, want error")
	}
	if !strings.Contains(err.Error(), socketDir) {
		t.Fatalf("Run error = %v, want it to name %q", err, socketDir)
	}
	if _, statErr := os.Stat(socketDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Run created the socket directory %q; the DaemonSet owns it", socketDir)
	}
}
