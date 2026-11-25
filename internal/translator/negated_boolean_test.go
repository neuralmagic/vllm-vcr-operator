package translator_test

import (
	"strings"
	"testing"

	"github.com/neuralmagic/vllm-vcr-operator/internal/plan"
	"github.com/neuralmagic/vllm-vcr-operator/internal/profile"
	"github.com/neuralmagic/vllm-vcr-operator/internal/translator"
)

// A workload that explicitly disables prefix caching with the release's negated
// spelling must reach the prefix-caching policy branch as an explicit false: the
// setting is a profile requirement, so it is accepted for a profile recorded with
// caching disabled and rejected — naming the setting, not an unknown flag — for a
// profile recorded with it enabled.
func TestNegatedPrefixCachingSpelling(t *testing.T) {
	t.Run("rejected when the profile records prefix caching", func(t *testing.T) {
		tc := translateCase{name: "negated prefix caching", args: []string{"--no-enable-prefix-caching"}}
		_, err := runTranslate(t, tc, testEnvironment())

		if err == nil {
			t.Fatal("Translate() accepted a workload that asks for the opposite cache representation")
		}
		rejections, ok := translator.Rejected(err)
		if !ok {
			t.Fatalf("rejection is not structured: %v", err)
		}
		for _, rejection := range rejections {
			if rejection.Setting == "--no-enable-prefix-caching" {
				t.Fatalf("the negated spelling is rejected as a spelling instead of being resolved: %v", err)
			}
		}
		found := false
		for _, rejection := range rejections {
			if rejection.Setting != "profile" {
				continue
			}
			// Prefix caching is a profile requirement, so the rejection comes from
			// profile selection and must report the value the workload asked for.
			if !strings.Contains(rejection.Reason, "enablePrefixCaching") || !strings.Contains(rejection.Reason, "required false") {
				continue
			}
			found = true
		}
		if !found {
			t.Fatalf("rejections %v do not report the requested prefix-caching value", rejections)
		}
	})

	t.Run("accepted when the profile records no prefix caching", func(t *testing.T) {
		tc := translateCase{
			name:   "negated prefix caching",
			args:   []string{"--no-enable-prefix-caching"},
			mutate: func(m *profile.Metadata) { m.Cache.EnablePrefixCaching = false },
			check: func(t *testing.T, a *plan.Admission) {
				// The profile's recording is the cache model; the workload's explicit
				// false agrees with it and adds no flag of its own.
				rejectArgs(t, "frontend", a.Frontend.Args, "--no-enable-prefix-caching")
			},
		}
		admission, err := runTranslate(t, tc, testEnvironment())
		mustAccept(t, admission, err)
		tc.check(t, admission)
	})
}

// The same holds for chunked prefill: an explicit false reaches the chunking
// branch, so it is rejected against a profile recorded with chunking and accepted
// against one recorded without it, where the simulator is not asked for a
// long-prefill threshold.
func TestNegatedChunkedPrefillSpelling(t *testing.T) {
	t.Run("rejected when the profile records chunked prefill", func(t *testing.T) {
		tc := translateCase{name: "negated chunked prefill", args: []string{"--no-enable-chunked-prefill"}}
		_, err := runTranslate(t, tc, testEnvironment())

		if err == nil {
			t.Fatal("Translate() accepted a workload that asks for the opposite chunking mode")
		}
		rejections, ok := translator.Rejected(err)
		if !ok {
			t.Fatalf("rejection is not structured: %v", err)
		}
		found := false
		for _, rejection := range rejections {
			if strings.HasPrefix(rejection.Setting, "--no-") {
				t.Fatalf("the negated spelling is rejected as a spelling instead of being resolved: %v", err)
			}
			if rejection.Setting != "--enable-chunked-prefill" {
				continue
			}
			found = true
			if !strings.Contains(rejection.Reason, "chunked prefill false") {
				t.Errorf("rejection %q does not report the requested value", rejection.Reason)
			}
		}
		if !found {
			t.Fatalf("rejections %v do not name the chunked-prefill setting", rejections)
		}
	})

	t.Run("accepted when the profile records no chunked prefill", func(t *testing.T) {
		tc := translateCase{
			name: "negated chunked prefill",
			// Without chunking one prompt must fit in a single step, so the model
			// length has to stay inside the per-step budget.
			args:   []string{"--no-enable-chunked-prefill", "--max-model-len", "4096"},
			mutate: func(m *profile.Metadata) { m.Cache.ChunkedPrefill = false },
			check: func(t *testing.T, a *plan.Admission) {
				for _, argv := range [][]string{a.Backend.Command, a.Frontend.Args} {
					rejectArgs(t, "plan", argv, "--long-prefill-token-threshold")
				}
			},
		}
		admission, err := runTranslate(t, tc, testEnvironment())
		mustAccept(t, admission, err)
		tc.check(t, admission)
	})
}
