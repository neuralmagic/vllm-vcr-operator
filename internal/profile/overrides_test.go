package profile_test

import (
	"strings"
	"testing"

	"github.com/neuralmagic/vllm-vcr-operator/internal/profile"
)

func int64Ptr(v int64) *int64       { return &v }
func float64Ptr(v float64) *float64 { return &v }
func requirementsWith(f func(*profile.Requirements)) profile.Requirements {
	r := matchingRequirements()
	f(&r)
	return r
}

func TestValidateOverridesAcceptsValuesWithinCoverage(t *testing.T) {
	m := metadataFor("h200-tp1")

	tests := []struct {
		name string
		req  profile.Requirements
	}{
		{
			name: "no overrides",
			req:  matchingRequirements(),
		},
		{
			name: "max model len at lower bound",
			req:  requirementsWith(func(r *profile.Requirements) { r.MaxModelLen = int64Ptr(1) }),
		},
		{
			name: "max model len at upper bound",
			req:  requirementsWith(func(r *profile.Requirements) { r.MaxModelLen = int64Ptr(131072) }),
		},
		{
			name: "max num seqs at bounds",
			req:  requirementsWith(func(r *profile.Requirements) { r.MaxNumSeqs = int64Ptr(128) }),
		},
		{
			name: "batched tokens at bounds",
			req:  requirementsWith(func(r *profile.Requirements) { r.MaxNumBatchedTokens = int64Ptr(8192) }),
		},
		{
			name: "chunked prefill threshold at zero",
			req:  requirementsWith(func(r *profile.Requirements) { r.LongPrefillTokenThreshold = int64Ptr(0) }),
		},
		{
			name: "kv blocks at upper bound",
			req:  requirementsWith(func(r *profile.Requirements) { r.KVBlocks = int64Ptr(2048) }),
		},
		{
			name: "declared scheduling policy",
			req:  requirementsWith(func(r *profile.Requirements) { r.SchedulingPolicy = "priority" }),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := profile.ValidateOverrides(tc.req, m); err != nil {
				t.Errorf("ValidateOverrides() returned error: %v", err)
			}
		})
	}
}

func TestValidateOverridesRejectsValuesOutsideCoverage(t *testing.T) {
	profileID := "h200-tp1"

	tests := []struct {
		name    string
		req     profile.Requirements
		wantSub []string
	}{
		{
			name:    "max model len below range",
			req:     requirementsWith(func(r *profile.Requirements) { r.MaxModelLen = int64Ptr(0) }),
			wantSub: []string{"maxModelLen", "0", "[1, 131072]", profileID},
		},
		{
			name:    "max model len above range",
			req:     requirementsWith(func(r *profile.Requirements) { r.MaxModelLen = int64Ptr(262144) }),
			wantSub: []string{"maxModelLen", "262144", "[1, 131072]", profileID},
		},
		{
			name:    "max num seqs above range",
			req:     requirementsWith(func(r *profile.Requirements) { r.MaxNumSeqs = int64Ptr(129) }),
			wantSub: []string{"maxNumSeqs", "129", "[1, 128]", profileID},
		},
		{
			name:    "batched tokens below range",
			req:     requirementsWith(func(r *profile.Requirements) { r.MaxNumBatchedTokens = int64Ptr(0) }),
			wantSub: []string{"maxNumBatchedTokens", "0", "[1, 8192]", profileID},
		},
		{
			name:    "long prefill threshold above range",
			req:     requirementsWith(func(r *profile.Requirements) { r.LongPrefillTokenThreshold = int64Ptr(4097) }),
			wantSub: []string{"longPrefillTokenThreshold", "4097", "[0, 4096]", profileID},
		},
		{
			name:    "kv blocks above range",
			req:     requirementsWith(func(r *profile.Requirements) { r.KVBlocks = int64Ptr(4096) }),
			wantSub: []string{"kvBlocks", "4096", "[1, 2048]", profileID},
		},
		{
			name:    "undeclared scheduling policy",
			req:     requirementsWith(func(r *profile.Requirements) { r.SchedulingPolicy = "edf" }),
			wantSub: []string{"schedulingPolicy", "edf", "fcfs", "priority", profileID},
		},
		{
			name:    "gpu memory utilization without conversion",
			req:     requirementsWith(func(r *profile.Requirements) { r.GPUMemoryUtilization = float64Ptr(0.9) }),
			wantSub: []string{"gpuMemoryUtilization", "0.9", "kvMemoryConversion", profileID},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := metadataFor(profileID)
			err := profile.ValidateOverrides(tc.req, m)
			if err == nil {
				t.Fatal("ValidateOverrides() accepted an override outside coverage, want rejection")
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not name %q", err.Error(), sub)
				}
			}
		})
	}
}

func TestValidateOverridesRequiresDeclaredCoverage(t *testing.T) {
	m := metadataFor("h200-tp1")
	m.Coverage.MaxNumSeqs = nil
	m.Coverage.SchedulingPolicies = nil

	req := requirementsWith(func(r *profile.Requirements) { r.MaxNumSeqs = int64Ptr(32) })
	err := profile.ValidateOverrides(req, m)
	if err == nil {
		t.Fatal("ValidateOverrides() treated absent coverage as unlimited, want rejection")
	}
	for _, sub := range []string{"maxNumSeqs", "coverage.maxNumSeqs", "h200-tp1"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not name %q", err.Error(), sub)
		}
	}

	req = requirementsWith(func(r *profile.Requirements) { r.SchedulingPolicy = "fcfs" })
	err = profile.ValidateOverrides(req, m)
	if err == nil {
		t.Fatal("ValidateOverrides() accepted a policy without declared coverage, want rejection")
	}
	for _, sub := range []string{"schedulingPolicy", "coverage.schedulingPolicies", "fcfs"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not name %q", err.Error(), sub)
		}
	}
}

func TestValidateOverridesAcceptsGPUMemoryUtilizationWithConversion(t *testing.T) {
	m := metadataFor("h200-tp1")
	m.Coverage.KVMemoryConversion = true

	req := requirementsWith(func(r *profile.Requirements) { r.GPUMemoryUtilization = float64Ptr(0.9) })
	if err := profile.ValidateOverrides(req, m); err != nil {
		t.Fatalf("ValidateOverrides() rejected a converted memory fraction: %v", err)
	}
}

func TestValidateOverridesRejectsInvalidMemoryFraction(t *testing.T) {
	m := metadataFor("h200-tp1")
	m.Coverage.KVMemoryConversion = true

	for _, fraction := range []float64{0, -0.5, 1.5} {
		req := requirementsWith(func(r *profile.Requirements) { r.GPUMemoryUtilization = float64Ptr(fraction) })
		err := profile.ValidateOverrides(req, m)
		if err == nil {
			t.Fatalf("ValidateOverrides() accepted gpuMemoryUtilization %v, want rejection", fraction)
		}
		if !strings.Contains(err.Error(), "gpuMemoryUtilization") {
			t.Errorf("error %q does not name gpuMemoryUtilization", err.Error())
		}
	}
}

func TestValidateOverridesReportsEveryViolation(t *testing.T) {
	m := metadataFor("h200-tp1")
	req := requirementsWith(func(r *profile.Requirements) {
		r.MaxModelLen = int64Ptr(262144)
		r.MaxNumSeqs = int64Ptr(1024)
		r.KVBlocks = int64Ptr(65536)
	})

	err := profile.ValidateOverrides(req, m)
	if err == nil {
		t.Fatal("ValidateOverrides() accepted several invalid overrides, want rejection")
	}
	for _, setting := range []string{"maxModelLen", "maxNumSeqs", "kvBlocks"} {
		if !strings.Contains(err.Error(), setting) {
			t.Errorf("error %q does not name %q", err.Error(), setting)
		}
	}
}

func TestValidateOverridesRejectsMissingProfile(t *testing.T) {
	err := profile.ValidateOverrides(matchingRequirements(), nil)
	if err == nil {
		t.Fatal("ValidateOverrides(nil) returned nil, want rejection")
	}
	if !strings.Contains(err.Error(), "no profile metadata") {
		t.Errorf("error %q does not explain the missing profile metadata", err.Error())
	}
}

func TestValidateOverridesCoverageBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		req     profile.Requirements
		wantErr bool
	}{
		{"max model len at min", requirementsWith(func(r *profile.Requirements) { r.MaxModelLen = int64Ptr(1) }), false},
		{"max model len at max", requirementsWith(func(r *profile.Requirements) { r.MaxModelLen = int64Ptr(131072) }), false},
		{"max model len below min", requirementsWith(func(r *profile.Requirements) { r.MaxModelLen = int64Ptr(0) }), true},
		{"max model len above max", requirementsWith(func(r *profile.Requirements) { r.MaxModelLen = int64Ptr(131073) }), true},

		{"max num seqs at min", requirementsWith(func(r *profile.Requirements) { r.MaxNumSeqs = int64Ptr(1) }), false},
		{"max num seqs at max", requirementsWith(func(r *profile.Requirements) { r.MaxNumSeqs = int64Ptr(128) }), false},
		{"max num seqs below min", requirementsWith(func(r *profile.Requirements) { r.MaxNumSeqs = int64Ptr(0) }), true},
		{"max num seqs above max", requirementsWith(func(r *profile.Requirements) { r.MaxNumSeqs = int64Ptr(129) }), true},

		{"batched tokens at min", requirementsWith(func(r *profile.Requirements) { r.MaxNumBatchedTokens = int64Ptr(1) }), false},
		{"batched tokens at max", requirementsWith(func(r *profile.Requirements) { r.MaxNumBatchedTokens = int64Ptr(8192) }), false},
		{"batched tokens below min", requirementsWith(func(r *profile.Requirements) { r.MaxNumBatchedTokens = int64Ptr(0) }), true},
		{"batched tokens above max", requirementsWith(func(r *profile.Requirements) { r.MaxNumBatchedTokens = int64Ptr(8193) }), true},

		{"prefill threshold at min", requirementsWith(func(r *profile.Requirements) { r.LongPrefillTokenThreshold = int64Ptr(0) }), false},
		{"prefill threshold at max", requirementsWith(func(r *profile.Requirements) { r.LongPrefillTokenThreshold = int64Ptr(4096) }), false},
		{"prefill threshold below min", requirementsWith(func(r *profile.Requirements) { r.LongPrefillTokenThreshold = int64Ptr(-1) }), true},
		{"prefill threshold above max", requirementsWith(func(r *profile.Requirements) { r.LongPrefillTokenThreshold = int64Ptr(4097) }), true},

		{"kv blocks at min", requirementsWith(func(r *profile.Requirements) { r.KVBlocks = int64Ptr(1) }), false},
		{"kv blocks at max", requirementsWith(func(r *profile.Requirements) { r.KVBlocks = int64Ptr(2048) }), false},
		{"kv blocks below min", requirementsWith(func(r *profile.Requirements) { r.KVBlocks = int64Ptr(0) }), true},
		{"kv blocks above max", requirementsWith(func(r *profile.Requirements) { r.KVBlocks = int64Ptr(2049) }), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := profile.ValidateOverrides(tc.req, metadataFor("h200-tp1"))
			if tc.wantErr && err == nil {
				t.Fatal("ValidateOverrides() accepted an out-of-coverage value, want rejection")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateOverrides() rejected an in-coverage value: %v", err)
			}
		})
	}
}

func TestOverrideErrorsDoNotDumpProfileMetadata(t *testing.T) {
	m := metadataFor("h200-tp1")
	m.VLLM.ConfigHash = "sha256:never-in-diagnostics"

	req := requirementsWith(func(r *profile.Requirements) { r.MaxModelLen = int64Ptr(262144) })
	err := profile.ValidateOverrides(req, m)
	if err == nil {
		t.Fatal("ValidateOverrides() accepted an out-of-coverage value, want rejection")
	}
	if strings.Contains(err.Error(), "sha256:never-in-diagnostics") {
		t.Errorf("error %q dumps profile metadata", err.Error())
	}
}
