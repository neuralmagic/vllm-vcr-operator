package translator

import "github.com/opendatahub-io/vllm-vcr-operator/internal/rejection"

// Rejection is the shared structured rejection (internal/rejection). The resolver
// and the settings policy produce the same type so that admission has one error
// path and one message shape.
//
// Invariant: Reason never contains a credential. Settings whose value is secret are
// reported by name only.
type Rejection = rejection.Rejection

// Rejections is a non-empty set of independent admission rejections. Translate
// reports every problem it can establish, so one rejection explains the whole
// configuration.
type Rejections = rejection.Rejections

// Rejected reports whether err carries structured rejections and returns them.
func Rejected(err error) (Rejections, bool) { return rejection.Rejected(err) }
