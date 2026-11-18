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

// Every boolean setting has both spellings registered, the negated one maps to
// false, and no negated spelling shadows a different setting's canonical
// spelling. A missing negation would make an explicit false an unknown flag; a
// shadowing one would make one setting configure another.
func TestNegatedSpellingsAreRegisteredPerBooleanSetting(t *testing.T) {
	p := newParser(Invocation{}, newConfig())

	for _, spec := range flags(newConfig()) {
		canonical, ok := p.specs[string(spec.setting)]
		if !ok {
			t.Fatalf("canonical spelling %q is not registered", spec.setting)
		}
		if !canonical.boolValue {
			t.Errorf("canonical spelling %q does not imply true", spec.setting)
		}
		if spec.kind != kindBool {
			continue
		}
		for _, negated := range negatedSpellings(string(spec.setting)) {
			spelling, registered := p.specs[negated]
			if !registered {
				t.Errorf("negated spelling %q of %s is not registered", negated, spec.setting)
				continue
			}
			if spelling.setting != spec.setting {
				t.Errorf("negated spelling %q configures %s, want %s", negated, spelling.setting, spec.setting)
			}
			if spelling.boolValue {
				t.Errorf("negated spelling %q implies true", negated)
			}
		}
	}

	// A negation is only ever derived from a boolean setting, so a value-taking
	// setting has no negated spelling at all.
	for _, spec := range flags(newConfig()) {
		if spec.kind == kindBool {
			continue
		}
		for _, negated := range negatedSpellings(string(spec.setting)) {
			if _, registered := p.specs[negated]; registered {
				t.Errorf("%q is registered although %s takes a value", negated, spec.setting)
			}
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
