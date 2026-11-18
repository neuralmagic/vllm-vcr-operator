package spike

import (
	"path/filepath"
)

// evidenceDir is where a harness writes its evidence document and the
// container diagnostics it kept. Tests run with this package directory as the
// working directory, and a harness run from the repository root resolves the
// same path, so the directory is a single documented location.
const evidenceDir = "evidence"

// EvidencePath returns the path of the evidence document for one spike.
//
// The name is the evidence file name docs/spikes/*.md reference:
// "runtime-layout.json" or "serving-behavior.json".
func EvidencePath(spike string) string {
	return filepath.Join(evidenceDir, spike+".json")
}
