package deviceplugin

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const bufSize = 1024 * 1024

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestPlugin(t *testing.T, count int) *Plugin {
	t.Helper()
	p, err := New(Options{DeviceCount: count, Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// startService serves the device plugin gRPC service over an in-memory
// connection. It is the seam the tests use to observe kubelet-facing behavior
// without touching the filesystem.
func startService(t *testing.T, p *Plugin) pluginapi.DevicePluginClient {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	p.RegisterService(srv)
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pluginapi.NewDevicePluginClient(conn)
}

func recvDevices(t *testing.T, stream pluginapi.DevicePlugin_ListAndWatchClient) []*pluginapi.Device {
	t.Helper()
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("ListAndWatch recv: %v", err)
	}
	return msg.Devices
}

func expectNoDevices(t *testing.T, stream pluginapi.DevicePlugin_ListAndWatchClient, wait time.Duration) {
	t.Helper()
	type result struct {
		msg *pluginapi.ListAndWatchResponse
		err error
	}
	done := make(chan result, 1)
	go func() {
		msg, err := stream.Recv()
		done <- result{msg, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("unexpected ListAndWatch update: devices=%v err=%v", got.msg.GetDevices(), got.err)
	case <-time.After(wait):
	}
}

func deviceIDs(devices []*pluginapi.Device) []string {
	ids := make([]string, 0, len(devices))
	for _, d := range devices {
		ids = append(ids, d.ID)
	}
	return ids
}

func assertIDs(t *testing.T, devices []*pluginapi.Device, want []string) {
	t.Helper()
	got := deviceIDs(devices)
	if len(got) != len(want) {
		t.Fatalf("device ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("device ids = %v, want %v", got, want)
		}
	}
}

func TestListAndWatchAdvertisesStableSyntheticDevices(t *testing.T) {
	p := newTestPlugin(t, 3)
	client := startService(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.ListAndWatch(ctx, &pluginapi.Empty{})
	if err != nil {
		t.Fatalf("ListAndWatch: %v", err)
	}
	devices := recvDevices(t, stream)
	assertIDs(t, devices, []string{"vcr-gpu-0", "vcr-gpu-1", "vcr-gpu-2"})
	for _, d := range devices {
		if d.Health != pluginapi.Healthy {
			t.Errorf("device %s health = %q, want %q", d.ID, d.Health, pluginapi.Healthy)
		}
	}

	// Device ids are stable across plugin restarts: a fresh plugin with the
	// same count advertises the same ids.
	restarted := newTestPlugin(t, 3)
	client2 := startService(t, restarted)
	stream2, err := client2.ListAndWatch(ctx, &pluginapi.Empty{})
	if err != nil {
		t.Fatalf("ListAndWatch after restart: %v", err)
	}
	assertIDs(t, recvDevices(t, stream2), deviceIDs(devices))
}

func TestListAndWatchStreamsCountChanges(t *testing.T) {
	p := newTestPlugin(t, 2)
	client := startService(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.ListAndWatch(ctx, &pluginapi.Empty{})
	if err != nil {
		t.Fatalf("ListAndWatch: %v", err)
	}
	assertIDs(t, recvDevices(t, stream), []string{"vcr-gpu-0", "vcr-gpu-1"})

	if err := p.SetDeviceCount(4); err != nil {
		t.Fatalf("SetDeviceCount: %v", err)
	}
	assertIDs(t, recvDevices(t, stream), []string{"vcr-gpu-0", "vcr-gpu-1", "vcr-gpu-2", "vcr-gpu-3"})

	// Setting the same count is not a change and must not produce an update.
	if err := p.SetDeviceCount(4); err != nil {
		t.Fatalf("SetDeviceCount: %v", err)
	}
	expectNoDevices(t, stream, 100*time.Millisecond)

	if err := p.SetDeviceCount(0); err == nil {
		t.Fatal("SetDeviceCount(0) succeeded, want error")
	}
}

func TestAllocateAcceptsKnownDevicesAndRejectsUnknown(t *testing.T) {
	p := newTestPlugin(t, 2)
	client := startService(t, p)
	ctx := context.Background()

	resp, err := client.Allocate(ctx, &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIds: []string{"vcr-gpu-0", "vcr-gpu-1"}},
		},
	})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if resp == nil || len(resp.ContainerResponses) != 1 {
		t.Fatalf("Allocate response = %v, want exactly one container response", resp)
	}
	cr := resp.ContainerResponses[0]
	if len(cr.Devices) != 0 || len(cr.Envs) != 0 || len(cr.Mounts) != 0 || len(cr.CdiDevices) != 0 || len(cr.Annotations) != 0 {
		t.Fatalf("Allocate added runtime configuration for synthetic devices: %+v", cr)
	}

	if _, err := client.Allocate(ctx, &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIds: []string{"vcr-gpu-7"}},
		},
	}); err == nil {
		t.Fatal("Allocate(unknown device) succeeded, want error")
	}
}

func TestPluginOptionsMatchBehavior(t *testing.T) {
	p := newTestPlugin(t, 1)
	client := startService(t, p)
	ctx := context.Background()

	opts, err := client.GetDevicePluginOptions(ctx, &pluginapi.Empty{})
	if err != nil {
		t.Fatalf("GetDevicePluginOptions: %v", err)
	}
	if opts.PreStartRequired {
		t.Error("PreStartRequired = true, want false: Allocate returns no runtime setup")
	}
	if !opts.GetPreferredAllocationAvailable {
		t.Error("GetPreferredAllocationAvailable = false, want true")
	}

	if _, err := client.PreStartContainer(ctx, &pluginapi.PreStartContainerRequest{
		DevicesIds: []string{"vcr-gpu-0"},
	}); err != nil {
		t.Errorf("PreStartContainer: %v, want no-op success", err)
	}
}

func TestGetPreferredAllocation(t *testing.T) {
	p := newTestPlugin(t, 3)
	client := startService(t, p)
	ctx := context.Background()

	resp, err := client.GetPreferredAllocation(ctx, &pluginapi.PreferredAllocationRequest{
		ContainerRequests: []*pluginapi.ContainerPreferredAllocationRequest{{
			AvailableDeviceIDs: []string{"vcr-gpu-0", "vcr-gpu-1", "vcr-gpu-2"},
			AllocationSize:     2,
		}},
	})
	if err != nil {
		t.Fatalf("GetPreferredAllocation: %v", err)
	}
	if len(resp.ContainerResponses) != 1 || len(resp.ContainerResponses[0].DeviceIDs) != 2 {
		t.Fatalf("GetPreferredAllocation = %+v, want two device ids", resp)
	}

	resp, err = client.GetPreferredAllocation(ctx, &pluginapi.PreferredAllocationRequest{
		ContainerRequests: []*pluginapi.ContainerPreferredAllocationRequest{{
			AvailableDeviceIDs:   []string{"vcr-gpu-0", "vcr-gpu-1", "vcr-gpu-2"},
			MustIncludeDeviceIDs: []string{"vcr-gpu-1"},
			AllocationSize:       2,
		}},
	})
	if err != nil {
		t.Fatalf("GetPreferredAllocation with must-include: %v", err)
	}
	got := resp.ContainerResponses[0].DeviceIDs
	if len(got) != 2 || (got[0] != "vcr-gpu-1" && got[1] != "vcr-gpu-1") {
		t.Fatalf("GetPreferredAllocation must-include = %v, want vcr-gpu-1 included", got)
	}

	if _, err := client.GetPreferredAllocation(ctx, &pluginapi.PreferredAllocationRequest{
		ContainerRequests: []*pluginapi.ContainerPreferredAllocationRequest{{
			AvailableDeviceIDs: []string{"vcr-gpu-0"},
			AllocationSize:     2,
		}},
	}); err == nil {
		t.Fatal("GetPreferredAllocation with allocation larger than available succeeded, want error")
	}

	if _, err := client.GetPreferredAllocation(ctx, &pluginapi.PreferredAllocationRequest{
		ContainerRequests: []*pluginapi.ContainerPreferredAllocationRequest{{
			AvailableDeviceIDs:   []string{"vcr-gpu-0", "vcr-gpu-1"},
			MustIncludeDeviceIDs: []string{"vcr-gpu-9"},
			AllocationSize:       1,
		}},
	}); err == nil {
		t.Fatal("GetPreferredAllocation with unavailable must-include succeeded, want error")
	}
}

func TestNewRejectsUnsupportedOptions(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"unqualified resource name", Options{ResourceName: "synthetic-gpu", DeviceCount: 1}, "resource name"},
		{"reserved resource domain", Options{ResourceName: "kubernetes.io/gpu", DeviceCount: 1}, "kubernetes.io"},
		{"zero devices", Options{DeviceCount: 0}, "device count"},
		{"too many devices", Options{DeviceCount: 65}, "device count"},
		{"endpoint escapes socket directory", Options{DeviceCount: 1, Endpoint: "../other.sock"}, "endpoint"},
		{"absolute endpoint", Options{DeviceCount: 1, Endpoint: "/tmp/other.sock"}, "endpoint"},
		{"health port out of range", Options{DeviceCount: 1, HealthPort: 70000}, "health port"},
		{"negative shutdown timeout", Options{DeviceCount: 1, ShutdownTimeout: -time.Second}, "shutdown timeout"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.opts); err == nil {
				t.Fatalf("New(%+v) succeeded, want error naming %q", tc.opts, tc.want)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}
