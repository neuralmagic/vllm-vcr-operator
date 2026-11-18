package spike_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendatahub-io/vllm-vcr-operator/test/spike"
)

// testEnvironment is the environment an evidence document records.
func testEnvironment() spike.Environment {
	return spike.Environment{
		FrontendImage: "registry.example/vllm-vcr-frontend:v0.33.0-cpu",
		BackendImage:  "registry.example/vllm-vcr-backend:v0.33.0",
		ProfileImage:  "registry.example/vllm-vcr-profiles:placeholder",
		ProfileDir:    "/var/run/vcr/profile",
		Model:         spike.DefaultModel,
		Engine:        "podman",
	}
}

// TestEvidenceRecordsPassingChecks asserts a document that records what ran:
// every check appears as its own step with an outcome, and only an all-passing
// document is reported as passed.
func TestEvidenceRecordsPassingChecks(t *testing.T) {
	evidence := spike.NewEvidence("runtime-layout", testEnvironment())
	evidence.Check("provisioner exported the profile", nil)
	evidence.Check("frontend became ready after the simulator registered", nil)
	evidence.Observe("readiness withdrawn 1.2s after backend loss", "readyz returned 503 for 1.2s before terminating")

	if !evidence.Succeeded() {
		t.Fatalf("evidence with only passing checks reports not passed: %+v", evidence.Steps)
	}
	if len(evidence.Steps) != 2 {
		t.Fatalf("recorded %d steps, want 2", len(evidence.Steps))
	}
	for _, step := range evidence.Steps {
		if step.Outcome != spike.OutcomePassed {
			t.Errorf("step %q outcome = %q, want %q", step.Name, step.Outcome, spike.OutcomePassed)
		}
	}
	if len(evidence.Observations) != 1 {
		t.Errorf("recorded %d observations, want 1", len(evidence.Observations))
	}
}

// TestEvidenceFailsWhenACheckFails asserts a failed check keeps its reason in
// the document and marks the whole spike failed, so an evidence file can never
// read as a pass while containing a failure.
func TestEvidenceFailsWhenACheckFails(t *testing.T) {
	evidence := spike.NewEvidence("serving-behavior", testEnvironment())
	evidence.Check("generation returns a choice", nil)
	evidence.Check("streaming emits more than one chunk", errors.New("got 1 chunk: no partial output"))

	if evidence.Succeeded() {
		t.Fatal("evidence with a failed check reports passed")
	}
	failed := evidence.Steps[1]
	if failed.Outcome != spike.OutcomeFailed {
		t.Errorf("outcome = %q, want %q", failed.Outcome, spike.OutcomeFailed)
	}
	if !strings.Contains(failed.Detail, "no partial output") {
		t.Errorf("failed step detail %q does not keep the reason", failed.Detail)
	}
}

// TestEvidenceRecordsWhatWasNotDemonstrated asserts the document can state a
// gap explicitly. The specification requires unresolved items to be recorded
// rather than omitted.
func TestEvidenceRecordsWhatWasNotDemonstrated(t *testing.T) {
	evidence := spike.NewEvidence("serving-behavior", testEnvironment())
	evidence.RecordGap("KV-event replay: the ready response reports replay_endpoint null")

	if evidence.Succeeded() {
		t.Fatal("evidence with an undemonstrated item reports passed")
	}
	if len(evidence.NotDemonstrated) != 1 {
		t.Fatalf("NotDemonstrated = %v", evidence.NotDemonstrated)
	}
}

// TestEvidenceWriteRoundTrips asserts the written document is the document a
// reader decodes: the file is the evidence, so it must not lose its steps,
// outcomes, or environment.
func TestEvidenceWriteRoundTrips(t *testing.T) {
	evidence := spike.NewEvidence("runtime-layout", testEnvironment())
	evidence.Check("frontend ready after registration", nil)
	evidence.Observe("shutdown exit code", "0")
	evidence.RecordGap("backend reconnection: not attempted")

	path := filepath.Join(t.TempDir(), "evidence", "runtime-layout.json")
	if err := evidence.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written evidence: %v", err)
	}
	var decoded spike.Evidence
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode written evidence: %v", err)
	}

	if decoded.Spike != "runtime-layout" {
		t.Errorf("spike = %q", decoded.Spike)
	}
	if decoded.SchemaVersion != spike.EvidenceSchemaVersion {
		t.Errorf("schemaVersion = %q, want %q", decoded.SchemaVersion, spike.EvidenceSchemaVersion)
	}
	if decoded.Images.Frontend != testEnvironment().FrontendImage {
		t.Errorf("images.frontend = %q", decoded.Images.Frontend)
	}
	if decoded.Environment.Engine != "podman" || decoded.Environment.Model != spike.DefaultModel {
		t.Errorf("environment = %+v", decoded.Environment)
	}
	if len(decoded.Steps) != 1 || decoded.Steps[0].Outcome != spike.OutcomePassed {
		t.Errorf("steps = %+v", decoded.Steps)
	}
	if len(decoded.Observations) != 1 || decoded.Observations[0].Name != "shutdown exit code" {
		t.Errorf("observations = %+v", decoded.Observations)
	}
	if len(decoded.NotDemonstrated) != 1 {
		t.Errorf("notDemonstrated = %v", decoded.NotDemonstrated)
	}
	if decoded.StartedAt.IsZero() || decoded.FinishedAt.IsZero() {
		t.Errorf("timestamps = %v .. %v, want both recorded", decoded.StartedAt, decoded.FinishedAt)
	}
	if decoded.Passed {
		t.Error("decoded document reports passed, want a run with an undemonstrated item to report not passed")
	}
}

// TestSchemaExampleIsAValidDocument asserts the checked-in example under
// test/spike/evidence/ decodes into the evidence type and matches the current
// schema version, so the documented format and the code cannot drift apart.
func TestSchemaExampleIsAValidDocument(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("evidence", "schema.example.json"))
	if err != nil {
		t.Fatalf("read the evidence schema example: %v", err)
	}
	var example spike.Evidence
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatalf("the evidence schema example does not decode: %v", err)
	}
	if example.SchemaVersion != spike.EvidenceSchemaVersion {
		t.Fatalf("example schemaVersion = %q, want %q", example.SchemaVersion, spike.EvidenceSchemaVersion)
	}
	if example.Spike == "" || len(example.Steps) == 0 || len(example.Images.Frontend) == 0 {
		t.Fatalf("the example documents too little to be a useful format reference: %+v", example)
	}
	if example.StartedAt.IsZero() || example.FinishedAt.IsZero() {
		t.Error("the example does not show the timestamps a reader interprets transitions with")
	}
}
