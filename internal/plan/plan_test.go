package plan

import "testing"

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
