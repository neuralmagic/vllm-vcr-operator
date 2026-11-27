package plan

import (
	"fmt"
	"testing"
)

func TestPinnedImage(t *testing.T) {
	tests := []struct {
		name   string
		image  string
		digest string
		want   string
	}{
		{
			name:   "tagged reference is pinned by digest",
			image:  "quay.io/opendatahub/vllm-vcr-profiles:qwen3-0.6b-h200-tp1-bf16",
			digest: "sha256:abc123",
			want:   "quay.io/opendatahub/vllm-vcr-profiles@sha256:abc123",
		},
		{
			name:   "registry port is not a tag",
			image:  "registry.example.com:5000/vcr/profiles",
			digest: "sha256:abc123",
			want:   "registry.example.com:5000/vcr/profiles@sha256:abc123",
		},
		{
			name:   "already pinned reference is rewritten to the selected digest",
			image:  "quay.io/opendatahub/vllm-vcr-profiles@sha256:old",
			digest: "sha256:abc123",
			want:   "quay.io/opendatahub/vllm-vcr-profiles@sha256:abc123",
		},
		{
			name:   "untagged path with port and no digest",
			image:  "localhost:5000/profiles",
			digest: "sha256:abc123",
			want:   "localhost:5000/profiles@sha256:abc123",
		},
		{
			name:  "missing digest leaves the reference unchanged",
			image: "quay.io/opendatahub/vllm-vcr-profiles:tag",
			want:  "quay.io/opendatahub/vllm-vcr-profiles:tag",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Profile{Image: tc.image, Digest: tc.digest}.PinnedImage()
			if got != tc.want {
				t.Errorf("PinnedImage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestModelReference(t *testing.T) {
	tests := []struct {
		name string
		in   Model
		want string
	}{
		{name: "pinned revision", in: Model{Name: "Qwen/Qwen3-0.6B", Revision: "c9340e6f3"}, want: "Qwen/Qwen3-0.6B@c9340e6f3"},
		{name: "unpinned revision", in: Model{Name: "Qwen/Qwen3-0.6B"}, want: "Qwen/Qwen3-0.6B"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Reference(); got != tc.want {
				t.Errorf("Reference() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPullSecretsOverrideReplacesTheCandidateList locks the rule admission, the
// translator, and the reconciler all rely on: a profile-level Secret is not an
// addition to the environment-wide list, it replaces it. Otherwise one profile's
// private credential would be copied for every other profile's workloads.
func TestPullSecretsOverrideReplacesTheCandidateList(t *testing.T) {
	sources := PullSecrets{
		Candidates: []string{"environment-wide"},
		Overrides:  map[string]string{"quay.io/a/profile": "profile-only"},
	}

	choice := sources.For("quay.io/a/profile")
	if len(choice.Names) != 1 || choice.Names[0] != "profile-only" {
		t.Errorf("Names = %v, want only the profile's own Secret", choice.Names)
	}
	if choice.Override != "profile-only" {
		t.Errorf("Override = %q, want the profile's own Secret", choice.Override)
	}
}

// TestPullSecretsCandidatesAreTrimmedAndDeduplicated: the candidate list is
// configuration, so blanks and repeats must not become Secret names to read.
func TestPullSecretsCandidatesAreTrimmedAndDeduplicated(t *testing.T) {
	sources := PullSecrets{Candidates: []string{" one ", "", "two", "one"}}
	choice := sources.For("quay.io/a/profile")
	if got, want := fmt.Sprint(choice.Names), "[one two]"; got != want {
		t.Errorf("Names = %s, want %s", got, want)
	}
	if choice.Override != "" {
		t.Errorf("Override = %q, want it empty when no profile names one", choice.Override)
	}
}
