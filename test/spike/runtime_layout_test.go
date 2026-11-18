package spike_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/opendatahub-io/vllm-vcr-operator/test/spike"
)

// TestRuntimeLayout is the runtime-layout and lifecycle spike the specification
// requires: it starts the profile provisioner, the simulator, and the supervisor
// plus real frontend as one process tree on one loopback interface, then
// establishes readiness ordering, the backend-loss path, the frontend-loss path,
// and coordinated SIGTERM shutdown.
//
// It runs only when VCR_FRONTEND_IMAGE, VCR_BACKEND_IMAGE, and a recorded profile
// are configured; VCR_SPIKE_REQUIRED=1 turns a missing environment into a
// failure. The run writes test/spike/evidence/runtime-layout.json, which is the
// only evidence docs/spikes/runtime-layout.md may cite.
//
// The assertions target docs/contracts/runtime-layout.md, not the behavior of a
// particular build: a check that fails here is a finding about the pinned images,
// and it must become a layout change or an explicit rejection.
func TestRuntimeLayout(t *testing.T) {
	env := spike.Gate(t, os.Getenv, exec.LookPath)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	stack := spike.NewStack(t, env)
	defer stack.Close(ctx)

	evidence := spike.NewEvidence("runtime-layout", env)
	defer func() {
		if err := evidence.Write(spike.EvidencePath("runtime-layout")); err != nil {
			t.Errorf("write evidence: %v", err)
		}
	}()

	// The profile is exported before anything serves; a failure here means the
	// run has nothing to serve and there is no point starting the frontend.
	err := stack.Provision(ctx)
	evidence.Check("the profile image's provisioning entrypoint exports into the profile volume", err)
	if err != nil {
		evidence.Observe("stack diagnostics", stack.Diagnostics())
		return
	}
	if env.ProfileImage == "" {
		evidence.RecordGap("profile provisioning: the run served an already-exported profile directory, so the image's provisioning entrypoint was not exercised")
	}
	err = stack.LoadProfile()
	evidence.Check("the exported profile metadata is valid and its declared trace is present", err)
	if err != nil {
		evidence.Observe("stack diagnostics", stack.Diagnostics())
		return
	}
	settings := stack.Settings()
	evidence.Observe("profile", fmt.Sprintf("model=%s maxModelLen=%d tokensPerBlock=%d kvBlocks=%d maxNumSeqs=%d trace=%s",
		settings.Model, settings.MaxModelLen, settings.TokensPerBlock, settings.KVBlocks, settings.MaxNumSeqs, settings.TracePath()))

	err = stack.StartFrontend(ctx)
	evidence.Check("the frontend container starts the supervisor and the Python frontend", err)
	if err != nil {
		evidence.Observe("stack diagnostics", stack.Diagnostics())
		return
	}

	// A frontend without a registered engine must not look ready. Both readiness
	// inputs are checked, because the contract defines readiness as their
	// conjunction and a single one can be satisfied spuriously.
	evidence.Check("the frontend answers its liveness endpoint while no engine is registered",
		awaitStatus(ctx, stack.Readiness(), "/healthz", 200, 5*time.Minute))
	notReady := settle(ctx, 5*time.Second, func(ctx context.Context) (bool, string) {
		ready, detail := stack.Ready(ctx)
		return !ready, detail
	})
	evidence.Check("readiness stays withdrawn while the simulator is absent", notReady)

	err = stack.StartBackend(ctx)
	evidence.Check("the simulator container starts and registers with the frontend", err)
	if err != nil {
		evidence.Observe("stack diagnostics", stack.Diagnostics())
		return
	}

	elapsed, err := stack.AwaitReady(ctx)
	evidence.Check("readiness is reached only after the simulator registers", err)
	if err == nil {
		evidence.Observe("time to readiness", elapsed.String())
	}

	err = awaitStatus(ctx, stack.Inference(), "/v1/models", 200, time.Minute)
	evidence.Check("the inference port serves model discovery once ready", err)

	// Backend loss: readiness must be withdrawn, and a backend that stays absent
	// must not leave a healthy-looking engine-less server.
	evidence.Check("the simulator can be removed abruptly", stack.Backend().Kill(ctx))

	withdrawn, err := stack.AwaitNotReady(ctx)
	evidence.Check("readiness is withdrawn when the simulator is lost", err)
	if err == nil {
		evidence.Observe("time to withdraw readiness", withdrawn.String())
	}

	exitCtx, exitCancel := context.WithTimeout(ctx, 3*settings.BackendLossGrace+time.Minute)
	frontendExit, waitErr := stack.Frontend().Wait(exitCtx)
	exitCancel()
	evidence.Check("the frontend container terminates instead of serving without an engine", waitErr)
	evidence.Observe("frontend exit code after backend loss", fmt.Sprint(frontendExit))
	if waitErr == nil && frontendExit == 0 {
		evidence.Check("the supervisor exits non-zero when it loses the engine", fmt.Errorf(
			"exit code 0: kubelet would not restart a container that gave up"))
	}

	// Restart recovery: the layout is restart-based. Whether the pinned images
	// reconnect is recorded as an observation, because the contract does not
	// promise reconnection and kubelet restarts the containers instead.
	err = stack.StartBackend(ctx)
	evidence.Check("a restarted simulator starts again", err)
	if err == nil {
		err = stack.StartFrontend(ctx)
		evidence.Check("a restarted frontend starts again", err)
	}
	var restarted time.Duration
	if err == nil {
		restarted, err = stack.AwaitReady(ctx)
		evidence.Check("a restarted pair reaches readiness again", err)
	}
	evidence.Observe("recovery mode", fmt.Sprintf(
		"restart-based: the frontend and the simulator were restarted independently and readiness returned after %s; the run did not establish that either process reconnects to a still-running peer", restarted))

	// Coordinated shutdown: readiness is withdrawn before the process exits, and
	// the exit is clean within the configured budget.
	signalErr := stack.Frontend().Signal(ctx, syscall.SIGTERM)
	evidence.Check("SIGTERM reaches the frontend container", signalErr)
	stopped, stopErr := stack.AwaitNotReady(ctx)
	evidence.Check("readiness is withdrawn before the frontend exits", stopErr)
	if stopErr == nil {
		evidence.Observe("time to withdraw readiness on SIGTERM", stopped.String())
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, settings.ShutdownTimeout+time.Minute)
	code, shutdownErr := stack.Frontend().Wait(shutdownCtx)
	shutdownCancel()
	evidence.Check("the frontend exits within the configured shutdown budget", shutdownErr)
	evidence.Observe("frontend exit code after SIGTERM", fmt.Sprint(code))
	if shutdownErr == nil && code != 0 {
		evidence.Check("a drained frontend exits zero", fmt.Errorf("exit code %d", code))
	}

	// What this run cannot establish, whatever the checks above reported.
	evidence.RecordGap("GPU hardware behavior: no NVIDIA device was used, so nothing here validates accelerator execution")
	evidence.RecordGap("timing fidelity: the profile's recorded latency was served and was not compared with a fresh capture")
	evidence.RecordGap("llm-d interaction: no router or discovery component ran, so cache-local routing is not established by this spike")

	if evidence.Failed() {
		t.Errorf("the runtime layout does not satisfy docs/contracts/runtime-layout.md; see %s\n%s",
			spike.EvidencePath("runtime-layout"), stack.Diagnostics())
	}
}

// awaitStatus polls url until it returns the wanted status.
func awaitStatus(ctx context.Context, client spike.Client, path string, want int, timeout time.Duration) error {
	_, _, err := spike.Await(ctx, spike.Waiter{Interval: 250 * time.Millisecond, Timeout: timeout},
		func(ctx context.Context) (bool, string) {
			status, body, err := client.Get(ctx, path)
			if err != nil {
				return false, err.Error()
			}
			if status != want {
				return false, fmt.Sprintf("%s returned %d: %s", path, status, truncate(string(body), 200))
			}
			return true, fmt.Sprintf("%s returned %d", path, status)
		})
	return err
}

// settle asserts a condition holds for the whole window, which is how a spike
// distinguishes "not ready yet" from "not ready".
func settle(ctx context.Context, window time.Duration, condition func(context.Context) (bool, string)) error {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		ok, detail := condition(ctx)
		if !ok {
			return fmt.Errorf("condition did not hold during the %s window: %s", window, detail)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return nil
}

// truncate shortens a body for an evidence detail.
func truncate(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[:n] + "…"
}
