package names_test

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
)

// pinFile is the machine-readable record of the pinned vllm-vcr build
// (docs/upstream-vllm-vcr.md). Rejections that name the pin quote
// names.SupportedSimulatorTag, so a re-pin that changes the record must change
// the constant too: a stale name in a rejection would tell a workload author to
// wait for a release that is already pinned.
const pinFile = "../../hack/vllm-vcr-pin.env"

// TestSimulatorTagMatchesThePinRecord keeps the constant and the pin record in
// step.
func TestSimulatorTagMatchesThePinRecord(t *testing.T) {
	raw, err := os.Open(pinFile)
	if err != nil {
		t.Fatalf("read %s: %v", pinFile, err)
	}
	defer raw.Close()

	pinned := ""
	scanner := bufio.NewScanner(raw)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "VLLM_VCR_PIN_TAG=") {
			continue
		}
		pinned = strings.TrimPrefix(line, "VLLM_VCR_PIN_TAG=")
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", pinFile, err)
	}
	if pinned == "" {
		t.Fatalf("%s does not record VLLM_VCR_PIN_TAG", pinFile)
	}
	if names.SupportedSimulatorTag != pinned {
		t.Errorf("names.SupportedSimulatorTag = %q, want the pinned tag %q from %s",
			names.SupportedSimulatorTag, pinned, pinFile)
	}
}
