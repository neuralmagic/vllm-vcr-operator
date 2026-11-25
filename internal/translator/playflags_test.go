package translator_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/neuralmagic/vllm-vcr-operator/internal/translator"
)

// playFlagsPath is the checked-in list of `vllm-vcr play` flags this translator
// can emit. hack/check-vllm-vcr-upstream.sh compares it against the pinned
// simulator release, so it must describe the code exactly.
const playFlagsPath = "../../hack/vllm-vcr-play-flags.txt"

// TestPlayFlagSurfaceMatchesDeclaration keeps the checked-in play-flag list and the
// invocation builder in step.
//
// The comparison is against the package's exported flag surface
// (translator.SimulatorFlags), which is derived from the same builder admission
// calls: a flag added there appears here automatically, so it cannot escape the
// upstream drift check.
func TestPlayFlagSurfaceMatchesDeclaration(t *testing.T) {
	declared, err := readPlayFlags(playFlagsPath)
	if err != nil {
		t.Fatalf("read %s: %v", playFlagsPath, err)
	}
	emitted := translator.SimulatorFlags()
	if len(emitted) == 0 {
		t.Fatal("translator.SimulatorFlags() returned no flags")
	}

	if missing := difference(emitted, declared); len(missing) > 0 {
		t.Errorf("the translator emits play flags missing from %s: %v", playFlagsPath, missing)
	}
	if stale := difference(declared, emitted); len(stale) > 0 {
		t.Errorf("%s lists play flags the translator never emits: %v", playFlagsPath, stale)
	}
}

// TestPlayFlagsAreLongFormNames guards the format the upstream comparison relies
// on: one bare `--name` per entry, no `=value`, no negation and no short form.
func TestPlayFlagsAreLongFormNames(t *testing.T) {
	declared, err := readPlayFlags(playFlagsPath)
	if err != nil {
		t.Fatalf("read %s: %v", playFlagsPath, err)
	}
	for _, flag := range declared {
		switch {
		case !strings.HasPrefix(flag, "--"):
			t.Errorf("%s: %q is not a long-form flag", playFlagsPath, flag)
		case strings.Contains(flag, "="):
			t.Errorf("%s: %q embeds a value; list bare flag names", playFlagsPath, flag)
		case len(flag) < 3:
			t.Errorf("%s: %q is not a flag name", playFlagsPath, flag)
		}
	}
}

func readPlayFlags(path string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	var flags []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		flags = append(flags, line)
	}
	return flags, nil
}

// difference returns the members of a that are absent from b.
func difference(a, b []string) []string {
	var out []string
	for _, value := range a {
		if !slices.Contains(b, value) {
			out = append(out, value)
		}
	}
	return out
}
