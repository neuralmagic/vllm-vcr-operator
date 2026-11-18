// Package rejection defines the single structured rejection type the admission
// path produces.
//
// A workload is rejected by two stages: internal/vllm when the invocation cannot be
// resolved, and internal/translator when the settings policy refuses it. Both
// stages, and the webhook that reports them, used to carry their own type and their
// own extraction helper, so admission had two error paths to keep in step. This
// package is the one definition: the same shape, the same message format, and the
// same errors.As-based unwrapping, so a caller can render either stage's rejection
// without knowing which one produced it.
//
// Invariant: no field of a Rejection ever contains a credential value. Settings whose
// value is secret are reported by name with vllm.RedactedValue in place of the value.
package rejection

import (
	"errors"
	"fmt"
	"strings"
)

// Rejection is one reason a workload cannot be admitted: the offending setting, the
// value as it appeared in the workload (redacted when it is a credential), and the
// reason it cannot be served.
type Rejection struct {
	// Setting names the offending setting, for example "--pipeline-parallel-size",
	// "VLLM_DP_RANK", or a synthetic requirement such as "nvidia.com/gpu".
	Setting string
	// Value is the offending value, empty when the setting has none or the value
	// is not quoted back.
	Value string
	// Reason explains why the setting cannot be served.
	Reason string
}

// Error implements error.
func (r Rejection) Error() string {
	switch {
	case r.Value == "":
		return fmt.Sprintf("%s: %s", r.Setting, r.Reason)
	default:
		return fmt.Sprintf("%s=%s: %s", r.Setting, r.Value, r.Reason)
	}
}

// Rejections is a non-empty set of independent reasons an invocation could not be
// accepted. Both stages report every problem they can establish safely rather than
// stopping at the first.
type Rejections []Rejection

// Error implements error.
func (rs Rejections) Error() string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, r.Error())
	}
	return strings.Join(parts, "; ")
}

// Unwrap exposes the individual rejections to errors.Is/As.
func (rs Rejections) Unwrap() []error {
	out := make([]error, 0, len(rs))
	for _, r := range rs {
		out = append(out, r)
	}
	return out
}

// Rejected reports whether err carries structured rejections and returns them.
// Callers that need to react to a specific setting use this instead of matching on
// message text.
func Rejected(err error) (Rejections, bool) {
	if err == nil {
		return nil, false
	}
	var rs Rejections
	if errors.As(err, &rs) {
		return rs, len(rs) > 0
	}
	var r Rejection
	if errors.As(err, &r) {
		return Rejections{r}, true
	}
	return nil, false
}
