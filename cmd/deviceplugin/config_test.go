package main

import (
	"bytes"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/deviceplugin"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
)

func emptyEnv(string) string { return "" }

func envFrom(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func parseOrFail(t *testing.T, args []string, getenv func(string) string) *config {
	t.Helper()
	cfg, err := parseConfig(args, getenv, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	return cfg
}

func TestDefaultConfigMatchesSharedIdentifiers(t *testing.T) {
	cfg := parseOrFail(t, nil, emptyEnv)

	if cfg.options.ResourceName != names.SyntheticResourceName {
		t.Errorf("resource name = %q, want %q", cfg.options.ResourceName, names.SyntheticResourceName)
	}
	if cfg.options.DeviceCount != 1 {
		t.Errorf("device count = %d, want 1", cfg.options.DeviceCount)
	}
	if cfg.options.SocketDir != names.DevicePluginDir {
		t.Errorf("socket dir = %q, want %q", cfg.options.SocketDir, names.DevicePluginDir)
	}
	if cfg.options.Endpoint != names.DevicePluginSocketName {
		t.Errorf("endpoint = %q, want %q", cfg.options.Endpoint, names.DevicePluginSocketName)
	}
	wantKubeletSocket := filepath.Join(names.DevicePluginDir, "kubelet.sock")
	if cfg.options.KubeletSocket != wantKubeletSocket {
		t.Errorf("kubelet socket = %q, want %q", cfg.options.KubeletSocket, wantKubeletSocket)
	}
	if cfg.options.DeviceCountFile != deviceplugin.DefaultDeviceCountFile {
		t.Errorf("device count file = %q, want %q", cfg.options.DeviceCountFile, deviceplugin.DefaultDeviceCountFile)
	}
	if cfg.options.HealthPort != deviceplugin.DefaultHealthPort {
		t.Errorf("health port = %d, want %d", cfg.options.HealthPort, deviceplugin.DefaultHealthPort)
	}
	if cfg.logLevel != slog.LevelInfo {
		t.Errorf("log level = %v, want info", cfg.logLevel)
	}
}

func TestEnvironmentOverridesDefaults(t *testing.T) {
	cfg := parseOrFail(t, nil, envFrom(map[string]string{
		envResourceName:    "example.com/synthetic-gpu",
		envDeviceCount:     "8",
		envSocketDir:       "/run/vcr/plugins",
		envEndpoint:        "custom.sock",
		envKubeletSocket:   "/run/kubelet.sock",
		envDeviceCountFile: "/run/vcr/count",
		envHealthPort:      "9091",
		envLogLevel:        "debug",
	}))

	if cfg.options.ResourceName != "example.com/synthetic-gpu" ||
		cfg.options.DeviceCount != 8 ||
		cfg.options.SocketDir != "/run/vcr/plugins" ||
		cfg.options.Endpoint != "custom.sock" ||
		cfg.options.KubeletSocket != "/run/kubelet.sock" ||
		cfg.options.DeviceCountFile != "/run/vcr/count" ||
		cfg.options.HealthPort != 9091 {
		t.Fatalf("environment overrides not applied: %+v", cfg.options)
	}
	if cfg.logLevel != slog.LevelDebug {
		t.Fatalf("log level = %v, want debug", cfg.logLevel)
	}
}

func TestFlagsOverrideEnvironment(t *testing.T) {
	cfg := parseOrFail(t, []string{"--device-count", "7", "--health-port", "0"}, envFrom(map[string]string{
		envDeviceCount: "8",
		envHealthPort:  "9091",
	}))
	if cfg.options.DeviceCount != 7 {
		t.Errorf("device count = %d, want 7", cfg.options.DeviceCount)
	}
	if cfg.options.HealthPort != 0 {
		t.Errorf("health port = %d, want 0 (disabled)", cfg.options.HealthPort)
	}
}

func TestHelpDocumentsEveryFlag(t *testing.T) {
	var out bytes.Buffer
	_, err := parseConfig([]string{"--help"}, emptyEnv, &out)
	if !errors.Is(err, pflag.ErrHelp) {
		t.Fatalf("parseConfig(--help) error = %v, want pflag.ErrHelp", err)
	}
	help := out.String()
	for _, want := range []string{
		"--resource-name", envResourceName,
		"--device-count", envDeviceCount,
		"--device-count-file", envDeviceCountFile,
		"--socket-dir", envSocketDir,
		"--endpoint", envEndpoint,
		"--kubelet-socket", envKubeletSocket,
		"--health-port", envHealthPort,
		"--log-level", envLogLevel,
		"--shutdown-timeout", envShutdownTimeout,
	} {
		if !strings.Contains(help, want) {
			t.Errorf("--help output does not document %q:\n%s", want, help)
		}
	}
}

func TestInvalidEnvironmentIsRejected(t *testing.T) {
	_, err := parseConfig(nil, envFrom(map[string]string{envDeviceCount: "four"}), &bytes.Buffer{})
	if err == nil {
		t.Fatal("parseConfig accepted an invalid device count, want error")
	}
	if !strings.Contains(err.Error(), envDeviceCount) || !strings.Contains(err.Error(), "four") {
		t.Fatalf("error = %v, want it to name %s and its value", err, envDeviceCount)
	}
}

func TestInvalidLogLevelIsRejected(t *testing.T) {
	_, err := parseConfig([]string{"--log-level", "loud"}, emptyEnv, &bytes.Buffer{})
	if err == nil {
		t.Fatal("parseConfig accepted an invalid log level, want error")
	}
	if !strings.Contains(err.Error(), "log-level") {
		t.Fatalf("error = %v, want it to name log-level", err)
	}
}

func TestUnexpectedArgumentsAreRejected(t *testing.T) {
	_, err := parseConfig([]string{"extra"}, emptyEnv, &bytes.Buffer{})
	if err == nil {
		t.Fatal("parseConfig accepted a positional argument, want error")
	}
}

func TestInvalidShutdownTimeoutIsRejected(t *testing.T) {
	_, err := parseConfig([]string{"--shutdown-timeout=-1s"}, emptyEnv, &bytes.Buffer{})
	if err == nil {
		t.Fatal("parseConfig accepted a negative shutdown timeout, want error")
	}
	if !strings.Contains(err.Error(), "shutdown-timeout") {
		t.Fatalf("error = %v, want it to name shutdown-timeout", err)
	}
}
