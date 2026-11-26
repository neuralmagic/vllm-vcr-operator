package registry

import (
	"fmt"
	"strings"
)

// JoinReference resolves a configured profile reference against the cluster's profile
// registry.
//
// A cluster names one registry and its profile entries name what to look for inside it:
//
//	base ghcr.io/acme/vllm-vcr-profiles
//	"qwen3-0.6b-h200-tp1-bf16"        → ghcr.io/acme/vllm-vcr-profiles:qwen3-0.6b-h200-tp1-bf16
//	"nested/qwen3-0.6b-h200-tp1-bf16" → ghcr.io/acme/vllm-vcr-profiles/nested/qwen3-0.6b-h200-tp1-bf16
//
// A reference that already names a registry host must live under the configured one.
// Fetching from anywhere else would mean the cluster has two sources of profiles while
// claiming one, and the second source would carry no registration at all.
//
// With no base configured, references are used as given: that is how a cluster with
// images in several registries is expressed, deliberately.
func JoinReference(base, reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return "", fmt.Errorf("profile reference is empty")
	}
	base = strings.TrimSuffix(strings.TrimSpace(base), "/")
	if base == "" {
		return reference, nil
	}
	if hasRegistryHost(reference) {
		inBase := reference == base ||
			strings.HasPrefix(reference, base+":") ||
			strings.HasPrefix(reference, base+"@") ||
			strings.HasPrefix(reference, base+"/")
		if !inBase {
			return "", fmt.Errorf(
				"profile %q is outside the configured profile registry %q: name a tag inside it, "+
					"or unset spec.profileRegistry to allow several registries", reference, base)
		}
		return reference, nil
	}
	if strings.Contains(reference, "/") {
		return base + "/" + reference, nil
	}
	return base + ":" + reference, nil
}

// hasRegistryHost reports whether the first path component of a reference names a
// registry, following the same rule the container ecosystem uses: it contains a dot or a
// colon, or it is localhost. A bare "name" or "name:tag" is therefore not a host.
func hasRegistryHost(reference string) bool {
	first, _, found := strings.Cut(reference, "/")
	if !found {
		// "name:tag" — the colon belongs to the tag, so there is no host.
		return false
	}
	return first == "localhost" || strings.ContainsAny(first, ".:")
}
