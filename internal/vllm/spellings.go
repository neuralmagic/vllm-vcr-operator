package vllm

import "sort"

// Spelling is one command-line spelling the resolver registers for the pinned vLLM
// release.
//
// The surface exists so that the resolver's interface with the release can be
// asserted from outside the package: tests and documentation generators compare
// Spellings, KnownSpelling, and EnvironmentVariables against the release instead of
// reading the package's private tables.
type Spelling struct {
	// Name is the spelling as it appears on the command line, for example
	// "--max-model-len", "--no-enable-prefix-caching", or "-tp".
	Name string
	// Setting is the setting the spelling configures.
	Setting Setting
	// Boolean reports that the spelling configures a boolean: the canonical
	// spelling implies true and its negated form implies false.
	Boolean bool
	// Negated reports that this spelling is the negated form of Setting, which a
	// caller uses to express an explicit false.
	Negated bool
	// Short reports that this spelling is a short form of Setting.
	Short bool
}

// Spellings returns every command-line spelling the resolver registers, sorted by
// name: each modelled setting's canonical spelling, the negated spellings of every
// boolean setting, and the release's short forms.
func Spellings() []Spelling {
	specs := flags(newConfig())
	boolean := make(map[Setting]bool, len(specs))
	for _, spec := range specs {
		boolean[spec.setting] = spec.kind == kindBool
	}

	out := make([]Spelling, 0, len(specs)*2+len(shortFlags))
	seen := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		name := string(spec.setting)
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, Spelling{
			Name:    name,
			Setting: spec.setting,
			Boolean: spec.kind == kindBool,
		})
		if spec.kind != kindBool {
			continue
		}
		for _, negated := range negatedSpellings(name) {
			out = append(out, Spelling{
				Name:    negated,
				Setting: spec.setting,
				Boolean: true,
				Negated: true,
			})
		}
	}
	for short, long := range shortFlags {
		setting := Setting(long)
		out = append(out, Spelling{
			Name:    short,
			Setting: setting,
			Boolean: boolean[setting],
			Short:   true,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// EnvironmentVariables returns every environment variable name the resolver models,
// sorted. A variable is modelled by name: a typo would make the resolver treat a real
// vLLM variable as unmodelled and reject the workload.
func EnvironmentVariables() []Setting {
	specs := environmentSpecs()
	out := make([]Setting, 0, len(specs))
	for _, spec := range specs {
		out = append(out, Setting(spec.variable))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// KnownSpelling reports whether name is a command-line spelling registered by the
// pinned vLLM release.
//
// It distinguishes a spelling the release does not define from one it defines but
// this operator does not model; both are rejected, but the reasons differ.
func KnownSpelling(name string) bool { return knownFlag(name) }
