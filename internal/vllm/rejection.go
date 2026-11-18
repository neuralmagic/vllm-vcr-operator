package vllm

import (
	"errors"
	"fmt"
	"strings"
)

// RedactedValue replaces any value that must not appear in errors, logs, or
// annotations (API keys, Hub tokens, key-file passwords).
const RedactedValue = "<redacted>"

// Rejection is one reason an invocation cannot be resolved.
//
// Invariant: Value never contains a credential. Values that are secret by nature
// are reported as RedactedValue.
type Rejection struct {
	// Setting names the offending setting, for example "--pipeline-parallel-size"
	// or "VLLM_DP_RANK".
	Setting Setting
	// Value is the offending value as it appeared in the workload, redacted when
	// it is a credential.
	Value string
	// Reason explains why the value cannot be resolved.
	Reason string
}

// Error implements error.
func (r Rejection) Error() string {
	switch {
	case r.Value == "":
		return fmt.Sprintf("vllm: %s: %s", r.Setting, r.Reason)
	default:
		return fmt.Sprintf("vllm: %s=%s: %s", r.Setting, r.Value, r.Reason)
	}
}

// Rejections is a non-empty set of independent reasons an invocation could not
// be resolved. Resolve reports every problem it can establish safely rather than
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
// Callers that need to react to a specific setting use this instead of matching
// on message text.
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
