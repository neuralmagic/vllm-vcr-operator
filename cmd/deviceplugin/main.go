// Command deviceplugin advertises stable synthetic GPU devices to kubelet. It
// requires no NVIDIA device files or runtime: Allocate returns no runtime
// configuration, and kubelet owns allocation state.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/pflag"

	"github.com/neuralmagic/vllm-vcr-operator/internal/deviceplugin"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "vllm-vcr-device-plugin: %v\n", err)
		os.Exit(1)
	}
}

// run wires configuration to the device plugin and blocks until SIGTERM/SIGINT
// or a fatal error. SIGTERM is not acted on immediately: the plugin drains
// in-flight allocations and only then stops its socket.
func run(args []string, stdout, stderr io.Writer) error {
	cfg, err := parseConfig(args, os.Getenv, stdout)
	if err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}

	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: cfg.logLevel}))
	slog.SetDefault(logger)
	cfg.options.Logger = logger

	plugin, err := deviceplugin.New(cfg.options)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(signals)
	go func() {
		sig := <-signals
		// Further SIGTERMs are delivered to this channel and ignored while the
		// plugin drains, so an in-flight Allocate is never cut short by a
		// second signal.
		logger.Info("signal received; draining in-flight allocations before stopping the socket", "signal", sig.String())
		cancel()
	}()

	return plugin.Run(ctx)
}
