package profile

import (
	"errors"
	"fmt"
	"strings"
)

// ValidateOverrides rejects workload overrides the selected profile did not
// declare support for.
//
// Coverage semantics are authorization, not capability hints:
//   - a declared range is inclusive; a value inside it is served faithfully, a
//     value outside it is rejected;
//   - a setting with no declared coverage entry is unsupported, not unlimited;
//   - a GPU memory fraction is only meaningful when the profile declares
//     coverage.kvMemoryConversion, because simulated KV capacity can never be
//     inferred from a memory percentage alone.
//
// Every violation is reported, not just the first, so that one admission
// rejection can explain the complete configuration problem. Messages name the
// setting, the requested value, and the supported range; they never include
// credential material.
func ValidateOverrides(req Requirements, m *Metadata) error {
	if m == nil {
		return errors.New("profile overrides: no profile metadata was selected, so no override can be authorized")
	}

	var failures []error
	checkRange := func(setting, flag string, value *int64, coverage *Range) {
		if value == nil {
			return
		}
		failures = append(failures, validateCoveredValue(m.ID, setting, "coverage."+setting, flag, *value, coverage))
	}

	checkRange("maxModelLen", "--max-model-len", req.MaxModelLen, m.Coverage.MaxModelLen)
	checkRange("maxNumSeqs", "--max-num-seqs", req.MaxNumSeqs, m.Coverage.MaxNumSeqs)
	checkRange("maxNumBatchedTokens", "--max-num-batched-tokens", req.MaxNumBatchedTokens, m.Coverage.MaxNumBatchedTokens)
	checkRange("longPrefillTokenThreshold", "--long-prefill-token-threshold", req.LongPrefillTokenThreshold, m.Coverage.LongPrefillTokenThreshold)
	checkRange("kvBlocks", "--num-gpu-blocks-override", req.KVBlocks, m.Coverage.KVBlocks)

	if policy := strings.TrimSpace(req.SchedulingPolicy); policy != "" {
		if len(m.Coverage.SchedulingPolicies) == 0 {
			failures = append(failures, fmt.Errorf(
				"schedulingPolicy %q is not authorized: profile %q does not declare coverage.schedulingPolicies",
				policy, m.ID))
		} else if !containsString(m.Coverage.SchedulingPolicies, policy) {
			failures = append(failures, fmt.Errorf(
				"schedulingPolicy %q is not authorized: profile %q declares [%s] in coverage.schedulingPolicies",
				policy, m.ID, strings.Join(m.Coverage.SchedulingPolicies, ", ")))
		}
	}

	if fraction := req.GPUMemoryUtilization; fraction != nil {
		switch {
		case *fraction <= 0 || *fraction > 1:
			failures = append(failures, fmt.Errorf(
				"gpuMemoryUtilization %v is invalid: a memory fraction must be in (0, 1]", *fraction))
		case !m.Coverage.KVMemoryConversion:
			failures = append(failures, fmt.Errorf(
				"gpuMemoryUtilization (--gpu-memory-utilization) %v is not authorized: profile %q does not declare coverage.kvMemoryConversion, so simulated KV capacity cannot be derived from a memory fraction",
				*fraction, m.ID))
		}
	}

	return errors.Join(failures...)
}

func validateCoveredValue(profileID, setting, coveragePath, flag string, value int64, coverage *Range) error {
	name := setting
	if flag != "" {
		name = fmt.Sprintf("%s (%s)", setting, flag)
	}
	if coverage == nil {
		return fmt.Errorf(
			"%s %d is not authorized: profile %q does not declare %s (an absent coverage entry is not permission)",
			name, value, profileID, coveragePath)
	}
	if value < coverage.Min || value > coverage.Max {
		return fmt.Errorf(
			"%s %d is outside profile %q %s range [%d, %d]: only values inside the recorded coverage are supported",
			name, value, profileID, coveragePath, coverage.Min, coverage.Max)
	}
	return nil
}
