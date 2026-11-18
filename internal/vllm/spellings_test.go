package vllm_test

import (
	"testing"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/vllm"
)

// The modelled spelling surface is an interface to a fixed vLLM release, so it is
// checked against that release's own spelling list: a canonical spelling the release
// does not register would silently reject every workload that uses the real flag, and
// a duplicate spelling would make the surface ambiguous.
func TestModelledSpellingsExistInVLLM(t *testing.T) {
	seen := make(map[string]vllm.Spelling)
	for _, spelling := range vllm.Spellings() {
		if previous, duplicate := seen[spelling.Name]; duplicate {
			t.Errorf("spelling %q is registered twice (as %s and %s)", spelling.Name, previous.Setting, spelling.Setting)
		}
		seen[spelling.Name] = spelling

		if spelling.Negated {
			// A negation is derived from the release's BooleanOptionalAction
			// convention, so it is checked by the boolean test below rather than
			// against the release's long-option list.
			continue
		}
		if !vllm.KnownSpelling(spelling.Name) {
			t.Errorf("modelled spelling %q is not a vLLM 0.33.0 spelling", spelling.Name)
		}
	}
}

// Every boolean setting has both spellings registered, the negated one maps to false,
// and no negated spelling shadows another setting. A missing negation would make an
// explicit false an unknown flag; a shadowing one would make one setting configure
// another.
func TestNegatedSpellingsFollowTheBooleanSurface(t *testing.T) {
	boolean := make(map[vllm.Setting]int)
	negated := make(map[vllm.Setting]int)
	for _, spelling := range vllm.Spellings() {
		if spelling.Short {
			continue
		}
		if spelling.Negated {
			negated[spelling.Setting]++
			if !spelling.Boolean {
				t.Errorf("negated spelling %q is not marked boolean", spelling.Name)
			}
			continue
		}
		if spelling.Boolean {
			boolean[spelling.Setting]++
		}
	}

	for _, spelling := range vllm.Spellings() {
		if spelling.Short || !spelling.Boolean || spelling.Negated {
			continue
		}
		if negated[spelling.Setting] == 0 {
			t.Errorf("boolean setting %s has no negated spelling", spelling.Setting)
		}
	}
	for setting := range negated {
		if boolean[setting] == 0 {
			t.Errorf("negated spelling for %s exists although no canonical boolean spelling does", setting)
		}
	}
	// A value-taking setting has no negated spelling at all.
	for _, spelling := range vllm.Spellings() {
		if spelling.Short || spelling.Boolean {
			continue
		}
		if negated[spelling.Setting] > 0 {
			t.Errorf("setting %s takes a value but has a negated spelling", spelling.Setting)
		}
	}
}

// Environment variables are modelled by name; a typo would make the resolver treat a
// real vLLM variable as unmodelled.
func TestModelledEnvironmentVariablesAreDistinct(t *testing.T) {
	seen := make(map[vllm.Setting]bool)
	for _, variable := range vllm.EnvironmentVariables() {
		if seen[variable] {
			t.Errorf("environment variable %q is modelled twice", variable)
		}
		seen[variable] = true
	}
	if len(seen) == 0 {
		t.Fatal("EnvironmentVariables() returned no variables")
	}
}
