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
	checkRange := func(setting string, value *int64, coverage *Range) {
		if value == nil {
			return
		}
		failures = append(failures, validateCoveredValue(m.ID, setting, "coverage."+setting, *value, coverage))
	}

	checkRange("maxModelLen", req.MaxModelLen, m.Coverage.MaxModelLen)
	checkRange("maxNumSeqs", req.MaxNumSeqs, m.Coverage.MaxNumSeqs)
	checkRange("maxNumBatchedTokens", req.MaxNumBatchedTokens, m.Coverage.MaxNumBatchedTokens)
	checkRange("longPrefillTokenThreshold", req.LongPrefillTokenThreshold, m.Coverage.LongPrefillTokenThreshold)
	checkRange("kvBlocks", req.KVBlocks, m.Coverage.KVBlocks)

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
				"gpuMemoryUtilization %v is not authorized: profile %q does not declare coverage.kvMemoryConversion, so simulated KV capacity cannot be derived from a memory fraction",
				*fraction, m.ID))
		}
	}

	return errors.Join(failures...)
}

func validateCoveredValue(profileID, setting, coveragePath string, value int64, coverage *Range) error {
	if coverage == nil {
		return fmt.Errorf(
			"%s %d is not authorized: profile %q does not declare %s (an absent coverage entry is not permission)",
			setting, value, profileID, coveragePath)
	}
	if value < coverage.Min || value > coverage.Max {
		return fmt.Errorf(
			"%s %d is outside profile %q %s range [%d, %d]: only values inside the recorded coverage are supported",
			setting, value, profileID, coveragePath, coverage.Min, coverage.Max)
	}
	return nil
}
