// Command supervisor is the entrypoint of the vllm-frontend container in a
// simulated model-server Pod.
//
// It runs the Python vLLM frontend as its only long-lived child, publishes
// readiness that combines frontend liveness with engine registration, and
// coordinates termination so that draining tests observe controlled shutdown.
// The simulator lives in its own container and is supervised by kubelet; the
// supervisor only observes its registration and never restarts it.
//
// Usage:
//
//	vcr-supervisor [flags] -- <frontend command> [args...]
//
// Environment fallbacks (see docs/contracts/runtime-layout.md):
//
//	VCR_HANDSHAKE_ADDRESS  engine-core handshake endpoint (default tcp://127.0.0.1:29550)
//	VCR_READY_FILE         readiness marker file (default /tmp/vcr-runtime/ready)
//	VCR_SHUTDOWN_TIMEOUT   graceful shutdown budget in seconds (default 0, immediate)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/supervisor"
)

// exitUsage is the exit code for an unusable invocation, matching the
// conventional command-line usage error.
const exitUsage = 2

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr))
}

func run(args []string, getenv func(string) string, stderr io.Writer) int {
	cfg, err := parseConfig(args, getenv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "vcr-supervisor: %v\n", err)
		return exitUsage
	}

	logger := log.New(stderr, "vcr-supervisor: ", log.LstdFlags)
	cfg.Logf = logger.Printf

	sup, err := supervisor.New(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "vcr-supervisor: %v\n", err)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	code, err := sup.Run(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "vcr-supervisor: %v\n", err)
	}
	return code
}

// parseConfig resolves flags over environment fallbacks over defaults, and
// treats the remaining arguments as the frontend command.
func parseConfig(args []string, getenv func(string) string, stderr io.Writer) (supervisor.Config, error) {
	handshakeDefault := supervisor.DefaultHandshakeAddress
	if v := getenv("VCR_HANDSHAKE_ADDRESS"); v != "" {
		handshakeDefault = v
	}
	readyDefault := supervisor.DefaultReadyFile
	if v := getenv("VCR_READY_FILE"); v != "" {
		readyDefault = v
	}
	shutdownDefault := time.Duration(0)
	if v := getenv("VCR_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := parseSeconds(v)
		if err != nil {
			return supervisor.Config{}, fmt.Errorf("VCR_SHUTDOWN_TIMEOUT=%q is not a shutdown budget in seconds: %w", v, err)
		}
		shutdownDefault = d
	}

	fs := flag.NewFlagSet("vcr-supervisor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	handshake := fs.String("handshake-address", handshakeDefault,
		"engine-core handshake endpoint observed for engine registration (tcp://host:port)")
	readyFile := fs.String("ready-file", readyDefault,
		"readiness marker file, written atomically while ready and removed otherwise")
	healthAddress := fs.String("health-address", supervisor.DefaultHealthAddress,
		"listen address of the local /healthz and /readyz endpoint")
	backendLossGrace := fs.Duration("backend-loss-grace", supervisor.DefaultBackendLossGrace,
		"how long the engine may stay unreachable before the frontend is terminated")
	shutdownTimeout := fs.Duration("shutdown-timeout", shutdownDefault,
		"graceful shutdown budget for the frontend; 0 terminates immediately")
	pollInterval := fs.Duration("poll-interval", supervisor.DefaultPollInterval,
		"how often engine registration is probed")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `Usage: vcr-supervisor [flags] -- <frontend command> [args...]

Runs the vLLM frontend and reports readiness only while the engine is registered.
Without --, the first non-flag argument starts the frontend command.

Environment fallbacks: VCR_HANDSHAKE_ADDRESS, VCR_READY_FILE, VCR_SHUTDOWN_TIMEOUT
(seconds). Flags take precedence.

Flags:
`)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return supervisor.Config{}, err
	}
	command := fs.Args()
	if len(command) == 0 {
		return supervisor.Config{}, errors.New("no frontend command: pass it after the flags, " +
			"e.g. vcr-supervisor -- python -m vllm.entrypoints.openai.api_server --model <name>")
	}

	return supervisor.Config{
		Command:          command,
		Env:              os.Environ(),
		HandshakeAddress: *handshake,
		ReadyFile:        *readyFile,
		HealthAddress:    *healthAddress,
		BackendLossGrace: *backendLossGrace,
		ShutdownTimeout:  *shutdownTimeout,
		PollInterval:     *pollInterval,
	}, nil
}

// parseSeconds accepts a bare number of seconds (the vLLM --shutdown-timeout
// form) or an explicit Go duration.
func parseSeconds(value string) (time.Duration, error) {
	if d, err := time.ParseDuration(value); err == nil {
		if d < 0 {
			return 0, fmt.Errorf("%q is negative", value)
		}
		return d, nil
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is neither a duration nor a number of seconds", value)
	}
	if seconds < 0 {
		return 0, fmt.Errorf("%q is negative", value)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}
