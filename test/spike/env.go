// Package spike contains the executable spike harnesses the specification
// requires, plus the support code they share.
//
// The spikes answer two questions the implementation cannot answer from
// documentation: which concrete frontend/backend/supervisor layout demonstrably
// reaches readiness, survives failure, and shuts down in coordination, and which
// serving behavior a pinned frontend/simulator pair actually exhibits.
//
// A spike is an experiment, not a unit test: it needs container images for the
// Python vLLM frontend, the vllm-vcr simulator, and a recorded profile. Nothing
// in this package downloads, builds, or substitutes those images. When they are
// absent the harnesses skip, and when the environment explicitly requests them
// (VCR_SPIKE_REQUIRED=1) they fail instead, so a CI job that intends to produce
// evidence cannot report a green skip as success.
//
// Every harness writes a JSON evidence document under test/spike/evidence/.
// docs/spikes/runtime-layout.md and docs/spikes/serving-behavior.md interpret
// those documents; a claim that is not in an evidence file is not established.
package spike

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// Environment variables that configure a spike run.
const (
	// EnvFrontendImage is the Python vLLM frontend image, the value of the
	// operator's VCR_FRONTEND_IMAGE. Required.
	EnvFrontendImage = "VCR_FRONTEND_IMAGE"

	// EnvBackendImage is the vllm-vcr simulator image, the value of the
	// operator's VCR_BACKEND_IMAGE. Required.
	EnvBackendImage = "VCR_BACKEND_IMAGE"

	// EnvProfileImage is a recorded profile OCI image. Optional: when set, the
	// harness runs the profile's provisioning entrypoint exactly as the
	// operator's init container does. Required unless EnvProfileDir is set.
	EnvProfileImage = "VCR_PROFILE_IMAGE"

	// EnvProfileDir is an already-exported profile directory (metadata plus
	// trace). Required unless EnvProfileImage is set.
	EnvProfileDir = "VCR_PROFILE_DIR"

	// EnvRequired turns a skipped spike into a failure.
	EnvRequired = "VCR_SPIKE_REQUIRED"

	// EnvRuntime selects the container engine ("podman" or "docker"). Optional:
	// podman is preferred over docker when both are installed.
	EnvRuntime = "VCR_SPIKE_RUNTIME"

	// EnvModel overrides the served model name. Optional: it must match the
	// resolved profile's model, which is why it is not inferred.
	EnvModel = "VCR_SPIKE_MODEL"
)

// DefaultModel is the model the checked-in fixture and examples use. It is the
// model of the recorded profile the profile workstream plans; a different
// profile needs EnvModel.
const DefaultModel = "Qwen/Qwen3-0.6B"

// enginePreference is the container engine search order.
var enginePreference = []string{"podman", "docker"}

// Environment is a resolved spike configuration: the images to run, the profile
// to serve, and the container engine that runs them.
type Environment struct {
	FrontendImage string
	BackendImage  string
	ProfileImage  string
	ProfileDir    string
	Model         string

	// Engine is the container engine the harness runs the images with.
	Engine string

	// EnginePath is the resolved path of Engine, so a run does not depend on
	// the PATH at the moment a container is started.
	EnginePath string

	Required bool
}

// MissingError reports the unmet requirements of a spike environment. Callers
// distinguish it from a real configuration error so that a skipped spike is not
// confused with a broken one.
type MissingError struct {
	// Requirements lists each unmet requirement in the form a user can act on.
	Requirements []string
}

func (e *MissingError) Error() string {
	return "spike environment is incomplete: " + strings.Join(e.Requirements, "; ")
}

// FromEnv resolves the spike environment from getenv and lookPath.
//
// It reports every unmet requirement at once, because a spike run against a
// partially configured environment would record evidence for a layout it never
// exercised. An unavailable explicitly requested container engine is a
// configuration error, not a missing requirement, because the caller named it.
func FromEnv(getenv func(string) string, lookPath func(string) (string, error)) (Environment, error) {
	out := Environment{
		FrontendImage: strings.TrimSpace(getenv(EnvFrontendImage)),
		BackendImage:  strings.TrimSpace(getenv(EnvBackendImage)),
		ProfileImage:  strings.TrimSpace(getenv(EnvProfileImage)),
		ProfileDir:    strings.TrimSpace(getenv(EnvProfileDir)),
		Model:         strings.TrimSpace(getenv(EnvModel)),
		Required:      Required(getenv),
	}
	if out.Model == "" {
		out.Model = DefaultModel
	}

	var missing []string
	if out.FrontendImage == "" {
		missing = append(missing, EnvFrontendImage+" is not set (the Python vLLM frontend image)")
	}
	if out.BackendImage == "" {
		missing = append(missing, EnvBackendImage+" is not set (the vllm-vcr simulator image)")
	}
	if out.ProfileImage == "" && out.ProfileDir == "" {
		missing = append(missing, EnvProfileImage+" or "+EnvProfileDir+" must name a recorded profile to serve")
	}

	engine, enginePath, err := resolveEngine(strings.TrimSpace(getenv(EnvRuntime)), lookPath)
	if err != nil {
		return Environment{}, err
	}
	if engine == "" {
		missing = append(missing, "no container engine found: install podman or docker, or name one with "+EnvRuntime)
	}
	out.Engine, out.EnginePath = engine, enginePath

	if len(missing) > 0 {
		return Environment{}, &MissingError{Requirements: missing}
	}
	return out, nil
}

// resolveEngine returns the container engine to use and its path, or empty
// strings when none is available. An explicitly requested engine that is not
// installed is an error.
func resolveEngine(requested string, lookPath func(string) (string, error)) (string, string, error) {
	if requested != "" {
		path, err := lookPath(requested)
		if err != nil {
			return "", "", fmt.Errorf("%s=%q is not an installed container engine: %w", EnvRuntime, requested, err)
		}
		return requested, path, nil
	}
	for _, candidate := range enginePreference {
		if path, err := lookPath(candidate); err == nil {
			return candidate, path, nil
		}
	}
	return "", "", nil
}

// Required reports whether the environment explicitly requests the spikes. Only
// an explicit true value counts: an unparseable value leaves the harnesses
// skipping, because a typo must not silently turn a skip into a green pass.
func Required(getenv func(string) string) bool {
	required, err := strconv.ParseBool(strings.TrimSpace(getenv(EnvRequired)))
	return err == nil && required
}

// Gate resolves the spike environment for tb: it skips the harness when the
// environment is merely absent, and fails it when the environment requested the
// spike explicitly (EnvRequired) or is misconfigured.
//
// The distinction is the point: "no images on this machine" is a skip, while
// "we asked for the spike and it could not run" is a failure.
func Gate(tb testing.TB, getenv func(string) string, lookPath func(string) (string, error)) Environment {
	tb.Helper()

	resolved, err := FromEnv(getenv, lookPath)
	if err == nil {
		return resolved
	}

	var missing *MissingError
	if !asMissing(err, &missing) {
		tb.Fatalf("spike environment is misconfigured: %v", err)
	}
	if Required(getenv) {
		tb.Fatalf("%s=1 requests this spike, but it cannot run: %v", EnvRequired, err)
	}
	tb.Skipf("skipping spike: %v\nSet %s=%s and %s=%s, a recorded profile (%s or %s), and install podman or docker to run it.",
		err, EnvFrontendImage, "<frontend image>", EnvBackendImage, "<simulator image>", EnvProfileImage, EnvProfileDir)
	return Environment{}
}

// asMissing reports whether err is or wraps a *MissingError.
func asMissing(err error, target **MissingError) bool {
	for err != nil {
		if missing, ok := err.(*MissingError); ok {
			*target = missing
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
