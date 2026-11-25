// Package deviceplugin implements the operator-managed synthetic GPU device
// plugin. It advertises stable, healthy devices under an extended resource name
// (nvidia.com/gpu by default) and never configures real GPU devices, mounts, or
// runtime hooks: kubelet owns allocation state, and the plugin only reports
// scheduling capacity.
package deviceplugin

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
)

const (
	// deviceIDPrefix prefixes every synthetic device id. Ids are
	// index-derived so they are stable across plugin restarts.
	deviceIDPrefix = "vcr-gpu-"

	// kubeletSocketName is the kubelet device-plugin registration socket inside
	// names.DevicePluginDir.
	kubeletSocketName = "kubelet.sock"

	// DefaultDeviceCountFile is where the operator writes the desired device
	// count so that a running DaemonSet can be reconfigured without a new
	// image.
	DefaultDeviceCountFile = "/etc/vcr/device-count"

	// DefaultHealthPort is the default port for the health endpoints. The
	// DaemonSet rendered by the operator probes this port.
	DefaultHealthPort = 8080

	// HealthPath is the liveness probe path.
	HealthPath = "/healthz"

	// ReadyPath is the readiness probe path. It succeeds only while the plugin
	// is registered with kubelet and not shutting down.
	ReadyPath = "/readyz"

	// minDeviceCount and maxDeviceCount bound the advertised count. The upper
	// bound matches the SimulationEnvironment API limit.
	minDeviceCount = 1
	maxDeviceCount = 64

	defaultPollInterval    = time.Second
	defaultRegisterTimeout = 5 * time.Second
	defaultShutdownTimeout = 30 * time.Second
)

// DialKubeletFunc opens a connection to kubelet's device-plugin registration
// socket. Tests inject an in-memory dialer.
type DialKubeletFunc func(target string) (*grpc.ClientConn, error)

// Options configures a Plugin. Empty values select the package defaults, which
// match the identifiers shared with the operator and the generated manifests.
// DeviceCount has no default and must always be set.
type Options struct {
	// ResourceName is the extended resource advertised to kubelet.
	// Default: names.SyntheticResourceName.
	ResourceName string

	// DeviceCount is the number of synthetic devices advertised when no count
	// file is present. It must be between 1 and 64; zero is not a valid
	// configuration.
	DeviceCount int

	// SocketDir is the host directory shared with kubelet for the plugin
	// socket. Default: names.DevicePluginDir.
	SocketDir string

	// Endpoint is the socket file name inside SocketDir.
	// Default: names.DevicePluginSocketName.
	Endpoint string

	// KubeletSocket is the kubelet registration socket path.
	// Default: <SocketDir>/kubelet.sock.
	KubeletSocket string

	// DeviceCountFile is the file polled for live device-count changes. It is
	// written by the operator. An empty value selects DefaultDeviceCountFile.
	// Default: DefaultDeviceCountFile.
	DeviceCountFile string

	// HealthPort is the TCP port for /healthz and /readyz. Zero disables the
	// health endpoint.
	HealthPort int

	// Logger receives structured logs. Default: slog.Default().
	Logger *slog.Logger

	// PollInterval is how often the count file and the kubelet socket are
	// checked. Default: 1s.
	PollInterval time.Duration

	// RegisterTimeout bounds one kubelet registration attempt.
	// Default: 5s.
	RegisterTimeout time.Duration

	// ShutdownTimeout bounds the wait for in-flight allocations before the
	// gRPC server is stopped. Default: 30s.
	ShutdownTimeout time.Duration

	// DialKubelet overrides how the plugin connects to kubelet. It is intended
	// for tests. Default: a unix-socket gRPC connection.
	DialKubelet DialKubeletFunc
}

// Plugin is a kubelet device plugin that advertises synthetic GPU capacity.
// RegisterService exposes it to any gRPC server; Run additionally manages the
// unix socket, kubelet registration, the health endpoint, and shutdown.
type Plugin struct {
	pluginapi.UnimplementedDevicePluginServer

	opts         Options
	logger       *slog.Logger
	count        atomic.Int64
	changeMu     sync.Mutex
	changes      chan struct{}
	shutdownMu   sync.Mutex
	shuttingDown bool
	closing      chan struct{}
	inflight     sync.WaitGroup

	registered   atomic.Bool
	socketPath   string
	listener     net.Listener
	grpcServer   *grpc.Server
	healthServer *http.Server
	watchers     sync.WaitGroup
	stopOnce     sync.Once
	stopDone     chan struct{}
	stopErr      error
}

// New validates options, applies defaults, and returns a plugin that can serve
// ListAndWatch immediately.
func New(opts Options) (*Plugin, error) {
	opts = opts.withDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}
	p := &Plugin{
		opts:     opts,
		logger:   opts.Logger,
		changes:  make(chan struct{}),
		closing:  make(chan struct{}),
		stopDone: make(chan struct{}),
	}
	p.count.Store(int64(opts.DeviceCount))
	return p, nil
}

func (o Options) withDefaults() Options {
	if o.ResourceName == "" {
		o.ResourceName = names.SyntheticResourceName
	}
	if o.SocketDir == "" {
		o.SocketDir = names.DevicePluginDir
	}
	if o.Endpoint == "" {
		o.Endpoint = names.DevicePluginSocketName
	}
	if o.KubeletSocket == "" {
		o.KubeletSocket = filepath.Join(o.SocketDir, kubeletSocketName)
	}
	if o.DeviceCountFile == "" {
		o.DeviceCountFile = DefaultDeviceCountFile
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.PollInterval == 0 {
		o.PollInterval = defaultPollInterval
	}
	if o.RegisterTimeout == 0 {
		o.RegisterTimeout = defaultRegisterTimeout
	}
	if o.ShutdownTimeout == 0 {
		o.ShutdownTimeout = defaultShutdownTimeout
	}
	return o
}

func (o Options) validate() error {
	// The operator advertises the NVIDIA resource name on purpose so that
	// unmodified workloads schedule against synthetic devices. Any configured
	// name still has to be a valid extended resource name.
	if errs := validation.IsQualifiedName(o.ResourceName); len(errs) > 0 {
		return fmt.Errorf("resource name %q is not a valid extended resource name: %s", o.ResourceName, errs[0])
	}
	if !strings.Contains(o.ResourceName, "/") {
		return fmt.Errorf("resource name %q must be qualified as <domain>/<name>", o.ResourceName)
	}
	if strings.HasPrefix(o.ResourceName, "kubernetes.io/") {
		return fmt.Errorf("resource name %q must not use the reserved kubernetes.io domain", o.ResourceName)
	}
	if o.DeviceCount < minDeviceCount || o.DeviceCount > maxDeviceCount {
		return fmt.Errorf("device count %d is outside the supported range %d..%d", o.DeviceCount, minDeviceCount, maxDeviceCount)
	}
	if o.Endpoint != filepath.Base(o.Endpoint) || o.Endpoint == "." || o.Endpoint == ".." {
		return fmt.Errorf("endpoint %q must be a file name inside the socket directory", o.Endpoint)
	}
	if o.HealthPort < 0 || o.HealthPort > 65535 {
		return fmt.Errorf("health port %d is outside the valid range 0..65535", o.HealthPort)
	}
	if o.PollInterval < 0 {
		return fmt.Errorf("poll interval %s must not be negative", o.PollInterval)
	}
	if o.RegisterTimeout < 0 {
		return fmt.Errorf("register timeout %s must not be negative", o.RegisterTimeout)
	}
	if o.ShutdownTimeout < 0 {
		return fmt.Errorf("shutdown timeout %s must not be negative", o.ShutdownTimeout)
	}
	return nil
}

// RegisterService registers the kubelet device plugin service on srv.
func (p *Plugin) RegisterService(srv grpc.ServiceRegistrar) {
	pluginapi.RegisterDevicePluginServer(srv, p)
}

// SetDeviceCount changes the advertised device count and notifies all active
// ListAndWatch streams. Setting the current count is a no-op.
func (p *Plugin) SetDeviceCount(count int) error {
	if count < minDeviceCount || count > maxDeviceCount {
		return fmt.Errorf("device count %d is outside the supported range %d..%d", count, minDeviceCount, maxDeviceCount)
	}
	if int(p.count.Load()) == count {
		return nil
	}
	p.changeMu.Lock()
	p.count.Store(int64(count))
	close(p.changes)
	p.changes = make(chan struct{})
	p.changeMu.Unlock()
	p.logger.Info("advertised synthetic device count changed", "count", count, "resource", p.opts.ResourceName)
	return nil
}

// DeviceCount returns the currently advertised device count.
func (p *Plugin) DeviceCount() int {
	return int(p.count.Load())
}

// subscribe returns a channel that is closed on the next count change.
func (p *Plugin) subscribe() <-chan struct{} {
	p.changeMu.Lock()
	defer p.changeMu.Unlock()
	return p.changes
}

func (p *Plugin) devices() []*pluginapi.Device {
	count := p.DeviceCount()
	devices := make([]*pluginapi.Device, 0, count)
	for i := 0; i < count; i++ {
		devices = append(devices, &pluginapi.Device{
			ID:     fmt.Sprintf("%s%d", deviceIDPrefix, i),
			Health: pluginapi.Healthy,
		})
	}
	return devices
}

func (p *Plugin) knownDeviceIDs() map[string]struct{} {
	ids := make(map[string]struct{}, p.DeviceCount())
	for i := 0; i < p.DeviceCount(); i++ {
		ids[fmt.Sprintf("%s%d", deviceIDPrefix, i)] = struct{}{}
	}
	return ids
}

// GetDevicePluginOptions reports that Allocate requires no runtime setup and
// that the plugin implements GetPreferredAllocation.
func (p *Plugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{
		PreStartRequired:                false,
		GetPreferredAllocationAvailable: true,
	}, nil
}

// ListAndWatch streams the synthetic device list and re-sends it whenever the
// device count changes.
func (p *Plugin) ListAndWatch(_ *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	changes := p.subscribe()
	if err := stream.Send(&pluginapi.ListAndWatchResponse{Devices: p.devices()}); err != nil {
		return err
	}
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-p.closing:
			return nil
		case <-changes:
			if err := stream.Send(&pluginapi.ListAndWatchResponse{Devices: p.devices()}); err != nil {
				return err
			}
			changes = p.subscribe()
		}
	}
}

// Allocate accepts known synthetic device ids. It returns an empty response:
// synthetic devices need no device files, environment variables, mounts, or CDI
// specification. Unknown ids are rejected instead of silently ignored.
func (p *Plugin) Allocate(_ context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	if !p.beginAllocation() {
		return nil, status.Error(codes.Unavailable, "device plugin is shutting down")
	}
	defer p.inflight.Done()

	known := p.knownDeviceIDs()
	resp := &pluginapi.AllocateResponse{
		ContainerResponses: make([]*pluginapi.ContainerAllocateResponse, 0, len(req.GetContainerRequests())),
	}
	for _, creq := range req.GetContainerRequests() {
		for _, id := range creq.GetDevicesIds() {
			if _, ok := known[id]; !ok {
				return nil, status.Errorf(codes.InvalidArgument, "unknown device id %q for resource %q", id, p.opts.ResourceName)
			}
		}
		resp.ContainerResponses = append(resp.ContainerResponses, &pluginapi.ContainerAllocateResponse{})
	}
	return resp, nil
}

// GetPreferredAllocation returns a deterministic subset of the available
// devices, honoring kubelet's must-include set.
func (p *Plugin) GetPreferredAllocation(_ context.Context, req *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	resp := &pluginapi.PreferredAllocationResponse{
		ContainerResponses: make([]*pluginapi.ContainerPreferredAllocationResponse, 0, len(req.GetContainerRequests())),
	}
	for _, creq := range req.GetContainerRequests() {
		available := make(map[string]struct{}, len(creq.GetAvailableDeviceIDs()))
		for _, id := range creq.GetAvailableDeviceIDs() {
			available[id] = struct{}{}
		}
		size := int(creq.GetAllocationSize())
		if size < 0 {
			return nil, status.Errorf(codes.InvalidArgument, "preferred allocation size %d is negative", size)
		}
		if size > len(available) {
			return nil, status.Errorf(codes.InvalidArgument, "preferred allocation size %d exceeds %d available devices", size, len(available))
		}
		chosen := make([]string, 0, size)
		for _, id := range creq.GetMustIncludeDeviceIDs() {
			if _, ok := available[id]; !ok {
				return nil, status.Errorf(codes.InvalidArgument, "must-include device %q is not available", id)
			}
			if len(chosen) >= size {
				return nil, status.Errorf(codes.InvalidArgument, "must-include devices exceed preferred allocation size %d", size)
			}
			chosen = append(chosen, id)
			delete(available, id)
		}
		for _, id := range creq.GetAvailableDeviceIDs() {
			if len(chosen) >= size {
				break
			}
			if _, ok := available[id]; !ok {
				continue
			}
			chosen = append(chosen, id)
			delete(available, id)
		}
		resp.ContainerResponses = append(resp.ContainerResponses, &pluginapi.ContainerPreferredAllocationResponse{DeviceIDs: chosen})
	}
	return resp, nil
}

// PreStartContainer is a no-op because GetDevicePluginOptions reports
// PreStartRequired=false.
func (p *Plugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

// beginAllocation records an in-flight Allocate call unless shutdown started.
func (p *Plugin) beginAllocation() bool {
	p.shutdownMu.Lock()
	defer p.shutdownMu.Unlock()
	if p.shuttingDown {
		return false
	}
	p.inflight.Add(1)
	return true
}
