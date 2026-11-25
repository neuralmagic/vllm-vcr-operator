package main

import (
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/neuralmagic/vllm-vcr-operator/internal/deviceplugin"
	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
)

const (
	// defaultShutdownTimeout bounds the SIGTERM drain period.
	defaultShutdownTimeout = 30 * time.Second

	envResourceName    = "VCR_DEVICE_PLUGIN_RESOURCE_NAME"
	envDeviceCount     = "VCR_DEVICE_PLUGIN_DEVICE_COUNT"
	envDeviceCountFile = "VCR_DEVICE_PLUGIN_DEVICE_COUNT_FILE"
	envSocketDir       = "VCR_DEVICE_PLUGIN_SOCKET_DIR"
	envEndpoint        = "VCR_DEVICE_PLUGIN_ENDPOINT"
	envKubeletSocket   = "VCR_DEVICE_PLUGIN_KUBELET_SOCKET"
	envHealthPort      = "VCR_DEVICE_PLUGIN_HEALTH_PORT"
	envLogLevel        = "VCR_DEVICE_PLUGIN_LOG_LEVEL"
	envShutdownTimeout = "VCR_DEVICE_PLUGIN_SHUTDOWN_TIMEOUT"
	logLevelHelp       = "log level: debug, info, warn, or error"
)

// config is the fully parsed command line and environment configuration.
type config struct {
	options  deviceplugin.Options
	logLevel slog.Level
}

// parseConfig resolves environment defaults, then command-line flags, and
// validates values that are not validated by deviceplugin.New. Every flag is
// documented in --help together with its environment variable.
func parseConfig(args []string, getenv func(string) string, out io.Writer) (*config, error) {
	resourceName, err := stringFromEnv(getenv, envResourceName, names.SyntheticResourceName)
	if err != nil {
		return nil, err
	}
	deviceCount, err := intFromEnv(getenv, envDeviceCount, 1)
	if err != nil {
		return nil, err
	}
	countFile, err := stringFromEnv(getenv, envDeviceCountFile, deviceplugin.DefaultDeviceCountFile)
	if err != nil {
		return nil, err
	}
	socketDir, err := stringFromEnv(getenv, envSocketDir, names.DevicePluginDir)
	if err != nil {
		return nil, err
	}
	endpoint, err := stringFromEnv(getenv, envEndpoint, names.DevicePluginSocketName)
	if err != nil {
		return nil, err
	}
	kubeletSocket, err := stringFromEnv(getenv, envKubeletSocket, filepath.Join(socketDir, "kubelet.sock"))
	if err != nil {
		return nil, err
	}
	healthPort, err := intFromEnv(getenv, envHealthPort, deviceplugin.DefaultHealthPort)
	if err != nil {
		return nil, err
	}
	logLevel, err := stringFromEnv(getenv, envLogLevel, "info")
	if err != nil {
		return nil, err
	}
	shutdownTimeout, err := durationFromEnv(getenv, envShutdownTimeout, defaultShutdownTimeout)
	if err != nil {
		return nil, err
	}

	cfg := &config{}
	fs := pflag.NewFlagSet("vllm-vcr-device-plugin", pflag.ContinueOnError)
	fs.SetOutput(out)

	fs.StringVar(&cfg.options.ResourceName, "resource-name", resourceName,
		"extended resource advertised to kubelet (env "+envResourceName+")")
	fs.IntVar(&cfg.options.DeviceCount, "device-count", deviceCount,
		"number of synthetic devices advertised when the count file is absent (env "+envDeviceCount+")")
	fs.StringVar(&cfg.options.DeviceCountFile, "device-count-file", countFile,
		"file polled for live device count changes written by the operator (env "+envDeviceCountFile+")")
	fs.StringVar(&cfg.options.SocketDir, "socket-dir", socketDir,
		"host directory shared with kubelet for the plugin socket (env "+envSocketDir+")")
	fs.StringVar(&cfg.options.Endpoint, "endpoint", endpoint,
		"plugin socket file name inside --socket-dir, registered with kubelet (env "+envEndpoint+")")
	fs.StringVar(&cfg.options.KubeletSocket, "kubelet-socket", kubeletSocket,
		"kubelet device-plugin registration socket (env "+envKubeletSocket+")")
	fs.IntVar(&cfg.options.HealthPort, "health-port", healthPort,
		"port for /healthz and /readyz; 0 disables the endpoint (env "+envHealthPort+")")
	fs.StringVar(&logLevel, "log-level", logLevel,
		logLevelHelp+" (env "+envLogLevel+")")
	fs.DurationVar(&shutdownTimeout, "shutdown-timeout", shutdownTimeout,
		"how long SIGTERM waits for in-flight allocations before stopping the socket (env "+envShutdownTimeout+")")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if shutdownTimeout <= 0 {
		return nil, fmt.Errorf("--shutdown-timeout must be positive, got %s", shutdownTimeout)
	}
	cfg.options.ShutdownTimeout = shutdownTimeout
	cfg.logLevel, err = parseLogLevel(logLevel)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func stringFromEnv(getenv func(string) string, key, fallback string) (string, error) {
	if value := getenv(key); value != "" {
		return value, nil
	}
	return fallback, nil
}

func intFromEnv(getenv func(string) string, key string, fallback int) (int, error) {
	value := getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("environment variable %s=%q is not an integer", key, value)
	}
	return parsed, nil
}

func durationFromEnv(getenv func(string) string, key string, fallback time.Duration) (time.Duration, error) {
	value := getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("environment variable %s=%q is not a duration", key, value)
	}
	return parsed, nil
}

func parseLogLevel(value string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.TrimSpace(value))); err != nil {
		return 0, fmt.Errorf("invalid --log-level %q: %s", value, logLevelHelp)
	}
	return level, nil
}
