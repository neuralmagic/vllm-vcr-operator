// Command vllm-vcr is a test double for the vllm-vcr simulator (`vllm-vcr play`).
//
// It exists so the kind acceptance suite (test/cluster) can exercise the real
// injected backend container — its real command line, its own container, its
// control API, its KV-event port, its probes — without the Rust simulator image,
// which is not available on the machines the suite targets.
//
// It is a test double, never an operator default: nothing in internal/, config/,
// or the Dockerfiles at the repository root references this image.
//
// What it genuinely reproduces (docs/contracts/runtime-layout.md):
//
//   - it registers with the frontend by connecting to the engine-core handshake
//     address it is given, which is what makes the frontend (and therefore the
//     supervisor's readiness) treat an engine as present;
//   - it serves the control API the supervisor probes (GET /stats on the control
//     address the operator passes), so a lost backend is observable exactly the
//     way the contract says it is;
//   - it listens on the KV-event endpoint and emits a placeholder event, so the
//     wiring to llm-d's consumer port is observable (plain TCP, not ZMQ: see the
//     README);
//   - it reads the latency trace the operator points it at, so provisioning into
//     the shared profile volume is observable end to end.
//
// It applies no recorded timing and generates no model output.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Test hooks on the control API, used by the acceptance suite to force the failure
// modes the runtime contract describes. They exist only in this double.
//
//	GET /stub/unavailable  stop answering /stats and drop the engine-core handshake
//	                       (a simulator that is alive but no longer serving)
//	GET /stub/available    undo it
//	GET /stub/exit         exit non-zero (a simulator crash, observable as a kubelet
//	                       restart of this container)
//
// None of them is part of the simulator's own control API.
const (
	stubUnavailablePath = "/stub/unavailable"
	stubAvailablePath   = "/stub/available"
	stubExitPath        = "/stub/exit"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// options is the subset of the generated `vllm-vcr play` invocation this double
// honors.
type options struct {
	handshakeAddress string
	controlAddress   string
	kvEventsEndpoint string
	latencyTrace     string
	engineID         string
}

func run(args []string) int {
	logger := log.New(os.Stdout, "vllm-vcr-stub: ", log.LstdFlags)

	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vllm-vcr-stub: %v\n", err)
		return 2
	}
	if opts.latencyTrace != "" {
		// The operator passes the profile volume path. Reading it proves the
		// provisioner exported the trace before the simulator started.
		info, err := os.Stat(opts.latencyTrace)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "vllm-vcr-stub: read --latency-trace %q: %v\n", opts.latencyTrace, err)
			return 1
		case info.Size() == 0:
			fmt.Fprintf(os.Stderr, "vllm-vcr-stub: --latency-trace %q is empty\n", opts.latencyTrace)
			return 1
		}
		logger.Printf("latency trace %s: %d bytes", opts.latencyTrace, info.Size())
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	controlAddr, err := listenAddress(opts.controlAddress)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vllm-vcr-stub: --control-address: %v\n", err)
		return 2
	}

	mux := http.NewServeMux()
	stats := &statsState{engineID: opts.engineID}
	mux.HandleFunc("/stats", stats.serve)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc(stubUnavailablePath, func(w http.ResponseWriter, _ *http.Request) {
		stats.setUnavailable(true)
		logger.Printf("test hook: simulator is now unavailable (control API answers 503, handshake dropped)")
		fmt.Fprintln(w, "unavailable")
	})
	mux.HandleFunc(stubAvailablePath, func(w http.ResponseWriter, _ *http.Request) {
		stats.setUnavailable(false)
		logger.Printf("test hook: simulator is available again")
		fmt.Fprintln(w, "available")
	})
	mux.HandleFunc(stubExitPath, func(w http.ResponseWriter, _ *http.Request) {
		// Answer first, exit shortly after: a caller waiting for the response must
		// see a complete answer, and only then does the container die.
		fmt.Fprintln(w, "exiting")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		logger.Printf("test hook: exiting non-zero to simulate a simulator crash")
		time.AfterFunc(200*time.Millisecond, func() { os.Exit(1) })
	})
	server := &http.Server{Addr: controlAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	if err := listenKVEvents(ctx, opts.kvEventsEndpoint, logger); err != nil {
		fmt.Fprintf(os.Stderr, "vllm-vcr-stub: KV-event endpoint: %v\n", err)
		return 1
	}

	go func() {
		logger.Printf("control API on %s", controlAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Printf("control API: %v", err)
		}
	}()
	go registerWithEngine(ctx, opts.handshakeAddress, stats, logger)

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	logger.Printf("stopped after SIGTERM")
	return 0
}

// parseArgs reads the `vllm-vcr play` command line the operator generates.
// Unknown flags are ignored: this double honors the endpoints the layout depends
// on and nothing else.
func parseArgs(args []string) (options, error) {
	opts := options{controlAddress: "tcp://0.0.0.0:8001"}

	// Only the endpoints this double honors take a value. Consuming the next token
	// for every valueless flag (--enable-kv-cache-events, --log-requests, ...) would
	// swallow the following flag and silently drop the endpoint the suite asserts.
	valued := map[string]*string{
		"handshake-address":  &opts.handshakeAddress,
		"control-address":    &opts.controlAddress,
		"kv-events-endpoint": &opts.kvEventsEndpoint,
		"latency-trace":      &opts.latencyTrace,
		"engine-id":          &opts.engineID,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "play" || !strings.HasPrefix(arg, "-") {
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		target, ok := valued[name]
		if !ok {
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				return options{}, fmt.Errorf("--%s has no value", name)
			}
			i++
			value = args[i]
		}
		*target = value
	}
	return opts, nil
}

// registerWithEngine holds a connection to the frontend's engine-core handshake
// port open, reconnecting when it drops. This is the registration the frontend
// (and the supervisor's readiness) observes.
func registerWithEngine(ctx context.Context, address string, stats *statsState, logger *log.Logger) {
	address = strings.TrimPrefix(strings.TrimSpace(address), "tcp://")
	if address == "" {
		address = "127.0.0.1:29550"
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if stats.unavailable() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}

		conn, err := net.DialTimeout("tcp", address, 2*time.Second)
		if err != nil {
			logger.Printf("waiting for the engine-core handshake at %s: %v", address, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		logger.Printf("registered with the engine-core handshake at %s", address)

		// Hold the connection open; the frontend treats the connection itself as
		// the registration.
		buf := make([]byte, 256)
		for {
			if _, err := conn.Read(buf); err != nil {
				break
			}
			if stats.unavailable() {
				break
			}
		}
		_ = conn.Close()
		logger.Printf("engine-core handshake connection closed, reconnecting")
	}
}

// listenKVEvents publishes one placeholder event on the KV-event endpoint so the
// port llm-d's consumer dials is observably alive. It is plain TCP, not ZMQ: the
// double proves wiring, not protocol compatibility.
func listenKVEvents(ctx context.Context, endpoint string, logger *log.Logger) error {
	if strings.TrimSpace(endpoint) == "" {
		return nil
	}
	// The KV-event endpoint is a ZMQ endpoint ("tcp://*:5556"), unlike the control
	// address, so its scheme is expected and translated here.
	addr, err := listenEndpoint(endpoint)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", addr, err)
	}
	logger.Printf("KV-event port listening on %s (placeholder events, plain TCP)", addr)
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			event, _ := json.Marshal(map[string]any{
				"type":    "BlockStored",
				"stub":    true,
				"blocks":  []int{},
				"emitted": time.Now().Unix(),
			})
			_, _ = fmt.Fprintf(conn, "%s\n", event)
			_ = conn.Close()
		}
	}()
	return nil
}

// statsState is the control API the supervisor probes (GET /stats).
type statsState struct {
	engineID string
	requests atomic.Int64
	offline  atomic.Bool
}

func (s *statsState) setUnavailable(unavailable bool) { s.offline.Store(unavailable) }

func (s *statsState) unavailable() bool { return s.offline.Load() }

func (s *statsState) serve(w http.ResponseWriter, _ *http.Request) {
	if s.offline.Load() {
		http.Error(w, "simulator unavailable", http.StatusServiceUnavailable)
		return
	}
	s.requests.Add(1)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"engine_id":        s.engineID,
		"stub":             true,
		"requests":         s.requests.Load(),
		"running":          0,
		"waiting":          0,
		"kv_cache_usage":   0.0,
		"replay_endpoint":  nil,
		"blocks_cached":    0,
		"blocks_replayed":  0,
		"tokens_generated": 0,
	})
}

// listenEndpoint turns a ZMQ endpoint ("tcp://*:5556", "tcp://127.0.0.1:5557")
// into a net.Listen address, mirroring the simulator's wildcard-binds rule.
func listenEndpoint(endpoint string) (string, error) {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return "", fmt.Errorf("no endpoint")
	}
	trimmed = strings.TrimPrefix(trimmed, "tcp://")
	idx := strings.LastIndex(trimmed, ":")
	if idx < 0 {
		return "", fmt.Errorf("endpoint %q has no port", endpoint)
	}
	host, port := trimmed[:idx], trimmed[idx+1:]
	if host == "*" || host == "" {
		host = "0.0.0.0"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", fmt.Errorf("endpoint %q has a non-numeric port: %w", endpoint, err)
	}
	return net.JoinHostPort(host, port), nil
}

// listenAddress turns the `--control-address` value into a net.Listen address.
//
// The real simulator hands that value straight to TcpListener::bind, so it accepts
// a bare HOST:PORT socket address and nothing else: a ZMQ-style "tcp://host:port"
// fails to resolve and the simulator exits. This double keeps the same rule
// instead of normalizing the mismatch away, because the kind suite is the only
// test that sees the generated command line end to end.
func listenAddress(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("no address")
	}
	if strings.Contains(trimmed, "://") {
		return "", fmt.Errorf("address %q carries a scheme: --control-address is a HOST:PORT socket address", raw)
	}
	idx := strings.LastIndex(trimmed, ":")
	if idx < 0 {
		return "", fmt.Errorf("address %q has no port", raw)
	}
	host, port := trimmed[:idx], trimmed[idx+1:]
	if host == "" {
		host = "0.0.0.0"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", fmt.Errorf("address %q has a non-numeric port: %w", raw, err)
	}
	return net.JoinHostPort(host, port), nil
}
