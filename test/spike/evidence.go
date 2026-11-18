package spike

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// EvidenceSchemaVersion is the version of the evidence document format the
// harnesses write and docs/spikes/*.md interpret.
const EvidenceSchemaVersion = "vcr.opendatahub-io/spike-evidence/v1"

// Step outcomes.
const (
	// OutcomePassed is a check the pinned images satisfied.
	OutcomePassed = "passed"

	// OutcomeFailed is a check the pinned images did not satisfy. It is a
	// finding, not a harness bug: the runtime choice must change or the
	// capability must be rejected.
	OutcomeFailed = "failed"
)

// Evidence is the JSON document a spike writes under test/spike/evidence/.
//
// It is the only acceptable basis for a claim in docs/spikes/*.md: a behavior
// that is not in an evidence document was not demonstrated, however plausible
// the code makes it look.
type Evidence struct {
	// SchemaVersion identifies this document format.
	SchemaVersion string `json:"schemaVersion"`

	// Spike names the harness that produced the document.
	Spike string `json:"spike"`

	// StartedAt and FinishedAt bound the run, so a reader can tell a fresh
	// document from a stale one.
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`

	// Images are the exact image references the run used, including the profile.
	Images Images `json:"images"`

	// Environment records the non-image configuration the run resolved.
	Environment EnvironmentEvidence `json:"environment"`

	// Steps are the checks the harness performed, in order.
	Steps []Step `json:"steps"`

	// Observations are recorded facts that are not pass/fail checks, for
	// example an exit code or the recovery behavior the pinned images exhibit.
	Observations []Observation `json:"observations,omitempty"`

	// NotDemonstrated lists what this run did not establish, so a gap is never
	// inferred from silence.
	NotDemonstrated []string `json:"notDemonstrated,omitempty"`

	// Passed is true only when every step passed and nothing was recorded as
	// not demonstrated.
	Passed bool `json:"passed"`
}

// Images are the image references a run used. Profile stays empty when the run
// served an already-exported profile directory.
type Images struct {
	Frontend string `json:"frontend"`
	Backend  string `json:"backend"`
	Profile  string `json:"profile,omitempty"`
}

// EnvironmentEvidence is the resolved non-image configuration of a run.
type EnvironmentEvidence struct {
	// Engine is the container engine binary the harness ran the images with.
	Engine string `json:"engine"`

	// Model is the served model name the frontend and the simulator were given.
	Model string `json:"model"`

	// ProfileSource is "image" when the profile image's provisioning entrypoint
	// ran, and "directory" when an exported profile directory was used.
	ProfileSource string `json:"profileSource"`

	// ProfileDir is the exported profile directory the simulator read its trace
	// from.
	ProfileDir string `json:"profileDir"`

	// HandshakeAddress is the engine-core handshake endpoint the frontend bound
	// and the simulator connected to.
	HandshakeAddress string `json:"handshakeAddress"`

	// InferenceURL is the frontend HTTP endpoint the serving checks drove.
	InferenceURL string `json:"inferenceURL"`

	// ControlURL is the simulator control API the readiness checks used.
	ControlURL string `json:"controlURL"`
}

// Step is one check with its timing and its reason.
type Step struct {
	Name       string    `json:"name"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	Outcome    string    `json:"outcome"`
	Detail     string    `json:"detail,omitempty"`
}

// Observation is a recorded fact that is not a pass/fail check.
type Observation struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// NewEvidence starts a document for one spike run against env.
func NewEvidence(spike string, env Environment) *Evidence {
	profileSource := "directory"
	if env.ProfileImage != "" {
		profileSource = "image"
	}
	return &Evidence{
		SchemaVersion: EvidenceSchemaVersion,
		Spike:         spike,
		StartedAt:     time.Now(),
		Images: Images{
			Frontend: env.FrontendImage,
			Backend:  env.BackendImage,
			Profile:  env.ProfileImage,
		},
		Environment: EnvironmentEvidence{
			Engine:        env.Engine,
			Model:         env.Model,
			ProfileSource: profileSource,
			ProfileDir:    env.ProfileDir,
		},
	}
}

// Check records the outcome of one check. A nil error is a pass; anything else
// is a finding with its reason kept verbatim.
func (e *Evidence) Check(name string, err error) {
	step := Step{Name: name, StartedAt: time.Now()}
	if err != nil {
		step.Outcome = OutcomeFailed
		step.Detail = err.Error()
	} else {
		step.Outcome = OutcomePassed
	}
	step.FinishedAt = time.Now()
	e.Steps = append(e.Steps, step)
}

// Observe records a fact that is not a check, for example an exit code or the
// behavior the pinned images exhibited where the contract does not fix one.
func (e *Evidence) Observe(name, detail string) {
	e.Observations = append(e.Observations, Observation{Name: name, Detail: detail})
}

// RecordGap records something this run did not establish.
func (e *Evidence) RecordGap(detail string) {
	e.NotDemonstrated = append(e.NotDemonstrated, detail)
}

// Failed reports whether any recorded check failed. A gap recorded with
// RecordGap is not a failure: it is a limitation the document states explicitly.
func (e *Evidence) Failed() bool {
	for _, step := range e.Steps {
		if step.Outcome != OutcomePassed {
			return true
		}
	}
	return false
}

// Succeeded reports whether the run established everything it checked.
func (e *Evidence) Succeeded() bool {
	if len(e.NotDemonstrated) > 0 {
		return false
	}
	for _, step := range e.Steps {
		if step.Outcome != OutcomePassed {
			return false
		}
	}
	return true
}

// Write finishes the document and writes it as indented JSON.
//
// The write is atomic: the document is staged next to its destination and
// renamed into place, so an interrupted run cannot leave a truncated evidence
// file that a reader would mistake for a complete one.
func (e *Evidence) Write(path string) error {
	e.FinishedAt = time.Now()
	e.Passed = e.Succeeded()

	encoded, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return fmt.Errorf("encode evidence: %w", err)
	}
	encoded = append(encoded, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create evidence directory %q: %w", dir, err)
	}
	staged, err := os.CreateTemp(dir, ".evidence-")
	if err != nil {
		return fmt.Errorf("stage evidence in %q: %w", dir, err)
	}
	stagedPath := staged.Name()
	defer os.Remove(stagedPath)

	if _, err := staged.Write(encoded); err != nil {
		staged.Close()
		return fmt.Errorf("write staged evidence: %w", err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("close staged evidence: %w", err)
	}
	if err := os.Rename(stagedPath, path); err != nil {
		return fmt.Errorf("publish evidence at %q: %w", path, err)
	}
	return nil
}
