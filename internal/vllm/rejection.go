package vllm

import (
	"github.com/neuralmagic/vllm-vcr-operator/internal/rejection"
)

// RedactedValue replaces any value that must not appear in errors, logs, or
// annotations (API keys, Hub tokens, key-file passwords).
const RedactedValue = "<redacted>"

// Rejection is the shared structured rejection (internal/rejection): a setting, its
// offending value, and the reason it cannot be resolved. The resolver and the
// settings policy produce the same type, so admission renders both with one path.
//
// Invariant: Value never contains a credential. Values that are secret by nature are
// reported as RedactedValue.
type Rejection = rejection.Rejection

// Rejections is a non-empty set of independent reasons an invocation could not be
// resolved. Resolve reports every problem it can establish safely rather than
// stopping at the first.
type Rejections = rejection.Rejections

// Rejected reports whether err carries structured rejections and returns them.
// Callers that need to react to a specific setting use this instead of matching on
// message text.
func Rejected(err error) (Rejections, bool) { return rejection.Rejected(err) }
