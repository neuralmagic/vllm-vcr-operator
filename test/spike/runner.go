package spike

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Runner runs the spike's container images with a container engine.
//
// The spikes do not build, fetch, or substitute images: a runner starts exactly
// the images the environment names, so an evidence document describes images a
// user can pull and re-run.
type Runner struct {
	// Engine is the engine name ("podman" or "docker").
	Engine string

	// Bin is the engine binary's path.
	Bin string
}

// Mount makes a host directory available inside a container.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// Container describes one container to run.
type Container struct {
	// Name is the container name the harness uses to control it afterwards.
	Name string

	// Image is the image reference to run.
	Image string

	// Args is the argument tail passed to the image unchanged. It is never
	// re-split, so a frontend invocation such as
	// "vcr-supervisor -- vllm serve …" reaches the container as written.
	Args []string

	// Env is the container environment in KEY=VALUE form.
	Env []string

	// Mounts are host directories made available in the container.
	Mounts []Mount

	// Network is the engine network mode. The Pod layout gives the frontend and
	// the simulator one loopback interface, so the spikes use "host".
	Network string

	// WorkDir is the container working directory, when it matters.
	WorkDir string
}

// Runner returns the runner the environment resolved. A hand-built Environment
// without EnginePath runs the engine by name, so a caller that sets only Engine
// still gets a usable runner.
func (e Environment) Runner() Runner {
	bin := e.EnginePath
	if bin == "" {
		bin = e.Engine
	}
	return Runner{Engine: e.Engine, Bin: bin}
}

// RunArgs returns the engine argv that starts c in the foreground.
func (r Runner) RunArgs(c Container) []string {
	args := []string{r.Bin, "run", "--rm", "--name", c.Name}
	if c.Network != "" {
		args = append(args, "--network", c.Network)
	}
	if c.WorkDir != "" {
		args = append(args, "--workdir", c.WorkDir)
	}
	for _, mount := range c.Mounts {
		spec := mount.Source + ":" + mount.Target
		if mount.ReadOnly {
			spec += ":ro"
		}
		args = append(args, "--volume", spec)
	}
	for _, env := range c.Env {
		args = append(args, "--env", env)
	}
	args = append(args, c.Image)
	return append(args, c.Args...)
}

// SignalArgs returns the engine argv that sends sig to the named container.
func (r Runner) SignalArgs(name, sig string) []string {
	return []string{r.Bin, "kill", "--signal", sig, name}
}

// RemoveArgs returns the engine argv that forcibly removes the named container
// and its anonymous volumes.
func (r Runner) RemoveArgs(name string) []string {
	return []string{r.Bin, "rm", "--force", "--volumes", name}
}

// Start runs c in a background process and returns a handle to it.
func (r Runner) Start(ctx context.Context, c Container) (*Handle, error) {
	if r.Bin == "" {
		return nil, errors.New("spike: no container engine binary to run the spike with")
	}
	// The engine client is removed first so that an aborted earlier run cannot
	// leave a name collision behind.
	if err := r.remove(ctx, c.Name); err != nil {
		return nil, err
	}

	logPath, logFile, err := createLog(c.Name)
	if err != nil {
		return nil, err
	}

	cmd := command(context.WithoutCancel(ctx), r.RunArgs(c))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("spike: start container %q: %w", c.Name, err)
	}

	handle := &Handle{runner: r, name: c.Name, cmd: cmd, logPath: logPath, logFile: logFile}
	// Reap exactly once, so repeated Wait calls observe the same exit code
	// instead of racing for the process status.
	handle.done = make(chan error, 1)
	go func() { handle.done <- cmd.Wait() }()
	return handle, nil
}

// Run starts c, waits for it to exit, and returns its exit code.
func (r Runner) Run(ctx context.Context, c Container) (int, error) {
	handle, err := r.Start(ctx, c)
	if err != nil {
		return -1, err
	}
	return handle.Wait(ctx)
}

// remove deletes a container left over from an earlier attempt. A container
// that does not exist is not an error.
func (r Runner) remove(ctx context.Context, name string) error {
	cmd := command(ctx, r.RemoveArgs(name))
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "no such container") {
			return nil
		}
		return fmt.Errorf("spike: remove stale container %q: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Handle is a started container.
type Handle struct {
	runner Runner
	name   string
	cmd    *exec.Cmd
	closed bool

	logPath string
	logFile *os.File
	done    chan error
	waited  bool
	code    int
	waitErr error
}

// Name is the container name.
func (h *Handle) Name() string { return h.name }

// LogPath is the file receiving the container's combined output.
func (h *Handle) LogPath() string { return h.logPath }

// Wait waits for the container to exit and returns its exit code. Repeated calls
// report the same result.
func (h *Handle) Wait(ctx context.Context) (int, error) {
	if h.cmd == nil || h.done == nil {
		return -1, errors.New("spike: container handle has no process")
	}
	if h.waited {
		return h.code, h.waitErr
	}

	select {
	case err := <-h.done:
		h.closeLog()
		h.waited = true
		var exitErr *exec.ExitError
		switch {
		case errors.As(err, &exitErr):
			h.code, h.waitErr = exitErr.ExitCode(), nil
		case err != nil:
			h.code, h.waitErr = -1, fmt.Errorf("spike: wait for container %q: %w", h.name, err)
		default:
			h.code = 0
		}
		return h.code, h.waitErr
	case <-ctx.Done():
		return -1, fmt.Errorf("spike: container %q did not exit before the deadline: %w", h.name, ctx.Err())
	}
}

// ExitCode returns the container's exit code once Wait has returned.
func (h *Handle) ExitCode() int {
	if h.cmd == nil || h.cmd.ProcessState == nil {
		return -1
	}
	return h.cmd.ProcessState.ExitCode()
}

// Signal sends sig to the container through the engine.
func (h *Handle) Signal(ctx context.Context, sig syscall.Signal) error {
	cmd := command(ctx, h.runner.SignalArgs(h.name, strconv.Itoa(int(sig))))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("spike: signal container %q: %w: %s", h.name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Kill removes the container immediately, used to simulate an abrupt loss such
// as a node-level OOM kill or a crash.
func (h *Handle) Kill(ctx context.Context) error {
	return h.runner.remove(ctx, h.name)
}

// Logs returns the container's combined output captured so far.
func (h *Handle) Logs() string {
	if h.logPath == "" {
		return ""
	}
	raw, err := os.ReadFile(h.logPath)
	if err != nil {
		return ""
	}
	return string(raw)
}

// Close releases the log file. It never stops the container: lifecycle control
// is explicit, so a harness cannot accidentally clean up the state it is about
// to assert on.
func (h *Handle) Close() {
	h.closeLog()
}

func (h *Handle) closeLog() {
	if h.closed || h.logFile == nil {
		return
	}
	h.closed = true
	h.logFile.Close()
}

// command builds an exec.Cmd from an engine argv.
func command(ctx context.Context, argv []string) *exec.Cmd {
	return exec.CommandContext(ctx, argv[0], argv[1:]...)
}

// createLog opens the diagnostic log for a container.
func createLog(name string) (string, *os.File, error) {
	dir := filepath.Join(evidenceDir, "diagnostics")
	path := filepath.Join(dir, name+".log")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("spike: create diagnostic directory %q: %w", dir, err)
	}
	file, err := os.Create(path)
	if err != nil {
		return "", nil, fmt.Errorf("spike: create diagnostics for %q: %w", name, err)
	}
	return path, file, nil
}
