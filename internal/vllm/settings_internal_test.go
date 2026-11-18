package vllm

import "testing"

// The modelled flag table is an interface to a fixed vLLM release, so it is
// checked against that release's own spelling list: a canonical spelling that
// the release does not register would silently reject every workload that uses
// the real flag, and a duplicate spelling would make the table ambiguous.
func TestModelledFlagSpellingsExistInVLLM(t *testing.T) {
	seen := make(map[string]Setting)
	for _, spec := range flags(newConfig()) {
		name := string(spec.setting)
		if !knownFlag(name) {
			t.Errorf("modelled setting %q is not a vLLM 0.33.0 flag spelling", name)
		}
		if previous, dup := seen[name]; dup {
			t.Errorf("setting %q is modelled twice (also as %q)", name, previous)
		}
		seen[name] = spec.setting

		for _, negated := range negatedSpellings(name) {
			if knownFlag(negated) {
				if previous, dup := seen[negated]; dup {
					t.Errorf("spelling %q is registered twice (also as %q)", negated, previous)
				}
				seen[negated] = spec.setting
			}
		}
	}

	for short, long := range shortFlags {
		if !knownFlag(short) {
			t.Errorf("short flag %q is not a vLLM 0.33.0 spelling", short)
		}
		if _, modelled := seen[long]; !modelled {
			t.Errorf("short flag %q maps to %q, which is not a modelled setting", short, long)
		}
	}
}

// Environment variables are modelled by name; a typo would make the resolver
// treat a real vLLM variable as unmodelled.
func TestModelledEnvironmentVariablesAreDistinct(t *testing.T) {
	seen := make(map[string]bool)
	for _, spec := range environmentSpecs() {
		if seen[spec.variable] {
			t.Errorf("environment variable %q is modelled twice", spec.variable)
		}
		seen[spec.variable] = true
		if spec.apply == nil {
			t.Errorf("environment variable %q has no resolution", spec.variable)
		}
	}
}
