// Command vllm is a test double for the Python vLLM frontend.
//
// It exists so the kind acceptance suite (test/cluster) can exercise the real
// lifecycle layout — the real vcr-supervisor as the frontend container's
// entrypoint, the real injected backend container, the real probes — without a
// Python vLLM image, which is not available on the machines the suite targets
// (see test/cluster/README.md).
//
// It is a test double, never an operator default: nothing in internal/, config/,
// or the Dockerfiles at the repository root references this image.
//
// The behavior it reproduces is the part of the runtime-layout contract the
// layout itself depends on (docs/contracts/runtime-layout.md):
//
//   - it binds the engine-core handshake port and reports "engine registered"
//     only while a backend holds a connection to it, exactly like the external
//     engine mode the operator configures;
//   - it serves /health and the OpenAI endpoints on the workload's inference
//     port, and answers 503 until an engine is registered, so a readiness signal
//     that probed only the frontend would report an engine-less server as ready;
//   - it streams /v1/chat/completions as server-sent events.
//
// It does not generate model output. Tokens are fixed placeholders and no
// recorded timing is applied: serving *content* is what test/spike establishes
// with real images, and this program makes no claim about it.
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

// stubTokens are the placeholder output tokens the SSE endpoint emits. They are
// not a recording and carry no model-behavior claim.
var stubTokens = []string{"vcr", "-stub", "-frontend"}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv))
}

// run parses the invocation the operator generates and serves it.
//
// The operator runs the frontend as `vllm serve <model> --host <host> --port
// <port> --data-parallel-rpc-port <port> ...`, so the flags it must honor are
// parsed and every other vLLM flag is ignored: this program cannot serve what it
// does not implement, and silently pretending to would make the suite assert the
// wrong thing.
func run(args []string, getenv func(string) string) int {
	logger := log.New(os.Stdout, "vllm-stub: ", log.LstdFlags)

	opts, err := parseArgs(args, getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vllm-stub: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	engine := &engineState{}
	handshake, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(opts.handshakePort)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "vllm-stub: bind engine-core handshake port %d: %v\n", opts.handshakePort, err)
		return 1
	}
	defer func() { _ = handshake.Close() }()
	go engine.accept(ctx, handshake, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if !engine.registered() {
			http.Error(w, "engine not registered", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		if !engine.registered() {
			http.Error(w, "engine not registered", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]any{
			"object": "list",
			"data": []map[string]any{{
				"id":       opts.model,
				"object":   "model",
				"owned_by": "vcr-stub",
			}},
		})
	})
	mux.HandleFunc("/v1/completions", func(w http.ResponseWriter, _ *http.Request) {
		if !engine.registered() {
			http.Error(w, "engine not registered", http.StatusServiceUnavailable)
			return
		}
		serveSSE(w, opts.model)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		if !engine.registered() {
			http.Error(w, "engine not registered", http.StatusServiceUnavailable)
			return
		}
		serveSSE(w, opts.model)
	})

	server := &http.Server{
		Addr:              net.JoinHostPort(opts.host, strconv.Itoa(opts.port)),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errs := make(chan error, 1)
	go func() {
		logger.Printf("serving %s on %s (engine-core handshake on :%d)", opts.model, server.Addr, opts.handshakePort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		fmt.Fprintf(os.Stderr, "vllm-stub: serve: %v\n", err)
		return 1
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Printf("shutdown: %v", err)
	}
	logger.Printf("stopped after SIGTERM")
	return 0
}

// options is the subset of the generated frontend invocation this double honors.
type options struct {
	model         string
	host          string
	port          int
	handshakePort int
}

// parseArgs reads the invocation the operator generates. Unknown flags are
// ignored on purpose; a --flag=value or --flag value spelling is accepted, and
// the model is the first positional argument after `serve`.
func parseArgs(args []string, getenv func(string) string) (options, error) {
	opts := options{model: "unknown", host: "0.0.0.0", port: 8000, handshakePort: 29550}

	// The handshake address also arrives as an environment variable; the flag
	// spelling wins because the operator emits both.
	if value := strings.TrimSpace(getenv("VCR_HANDSHAKE_ADDRESS")); value != "" {
		if port, err := portFromAddress(value); err == nil {
			opts.handshakePort = port
		}
	}

	positional := 0
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "serve" {
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			positional++
			if positional == 1 {
				opts.model = arg
			}
			continue
		}

		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		nextValue := func() (string, bool) {
			if hasValue {
				return value, true
			}
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				return args[i], true
			}
			return "", false
		}

		switch name {
		case "host":
			if v, ok := nextValue(); ok {
				opts.host = v
			}
		case "port":
			v, ok := nextValue()
			if !ok {
				return options{}, fmt.Errorf("--port has no value")
			}
			port, err := strconv.Atoi(v)
			if err != nil {
				return options{}, fmt.Errorf("--port %q is not a number: %w", v, err)
			}
			opts.port = port
		case "data-parallel-rpc-port":
			v, ok := nextValue()
			if !ok {
				return options{}, fmt.Errorf("--data-parallel-rpc-port has no value")
			}
			port, err := strconv.Atoi(v)
			if err != nil {
				return options{}, fmt.Errorf("--data-parallel-rpc-port %q is not a number: %w", v, err)
			}
			opts.handshakePort = port
		case "served-model-name":
			if v, ok := nextValue(); ok && positional == 0 {
				opts.model = v
			}
		default:
			// Any other vLLM flag is accepted and ignored: this program cannot
			// honor it, and pretending to would misrepresent what is asserted.
			_ = name
		}
	}
	return opts, nil
}

// portFromAddress extracts the port from "tcp://host:port" or "host:port".
func portFromAddress(address string) (int, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(address), "tcp://")
	idx := strings.LastIndex(trimmed, ":")
	if idx < 0 {
		return 0, fmt.Errorf("address %q has no port", address)
	}
	return strconv.Atoi(trimmed[idx+1:])
}

// engineState reports whether a simulator holds the engine-core handshake.
//
// The real frontend binds the handshake socket itself and vLLM's external-engine
// mode answers its health endpoint only once an engine registered over it. This
// double models that with the connection the backend stub keeps open.
type engineState struct {
	connections atomic.Int32
}

func (e *engineState) registered() bool { return e.connections.Load() > 0 }

func (e *engineState) accept(ctx context.Context, listener net.Listener, logger *log.Logger) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			logger.Printf("handshake accept: %v", err)
			return
		}
		total := e.connections.Add(1)
		logger.Printf("engine registered (handshake connections: %d)", total)
		go func() {
			defer func() {
				_ = conn.Close()
				logger.Printf("engine deregistered (handshake connections: %d)", e.connections.Add(-1))
			}()
			buf := make([]byte, 256)
			for {
				if _, err := conn.Read(buf); err != nil {
					return
				}
			}
		}()
	}
}

// serveSSE writes an OpenAI-shaped server-sent event stream. This is a protocol
// double: the chunks are fixed tokens, not model output.
func serveSSE(w http.ResponseWriter, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	now := time.Now().Unix()
	for i, token := range stubTokens {
		chunk := map[string]any{
			"id":      "chatcmpl-vcr-stub",
			"object":  "chat.completion.chunk",
			"created": now,
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{"content": token},
				"finish_reason": nil,
			}},
		}
		if i == len(stubTokens)-1 {
			chunk["choices"] = []map[string]any{{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			}}
		}
		data, err := json.Marshal(chunk)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	_ = encoder.Encode(value)
}
