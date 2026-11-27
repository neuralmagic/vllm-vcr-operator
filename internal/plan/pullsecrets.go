package plan

import "strings"

// PullSecrets decides which operator-namespace Secrets hold credentials for a
// profile image.
//
// The rule lives next to the plan it feeds because three places need it: admission
// builds a plan from it, the translator reads it, and the reconciler reads the
// credential Secrets with it. Three copies of the rule is how a profile-level
// override once silently stopped applying.
type PullSecrets struct {
	// Candidates are spec.imagePullSecrets, in spec order. They are candidates
	// only: admission projects an entry when it can authenticate the selected
	// image's registry host, so one profile's private credentials are never copied
	// for a profile served from another registry.
	Candidates []string

	// Overrides maps a configured profile image to the
	// spec.profiles[].pullSecretName that replaces Candidates for it.
	Overrides map[string]string
}

// Choice is the pull-secret decision for one selected profile image.
type Choice struct {
	// Names are the operator-namespace Secret names to read, in order. When
	// Override is set this holds exactly that one name.
	Names []string
	// Override is the profile-level Secret name, empty when Names came from the
	// environment-wide candidate list.
	Override string
}

// For returns the decision for one selected profile image.
//
// A profile-level Secret replaces the environment-wide list instead of adding to
// it: the operator named it for exactly this profile, so a profile that keeps its
// credentials elsewhere does not widen what every other profile copies.
func (p PullSecrets) For(image string) Choice {
	if override := strings.TrimSpace(p.Overrides[image]); override != "" {
		return Choice{Names: []string{override}, Override: override}
	}

	names := make([]string, 0, len(p.Candidates))
	seen := make(map[string]struct{}, len(p.Candidates))
	for _, candidate := range p.Candidates {
		name := strings.TrimSpace(candidate)
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return Choice{Names: names}
}
