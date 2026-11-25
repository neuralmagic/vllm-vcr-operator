// Package profile is the public face of the operator's profile contract.
//
// The operator's rules live in internal/profile so they stay out of the import
// graph of everything else. Profile producers need exactly those rules — a
// producer that validates with a copy of the schema drifts from admission — so
// this package re-exports the types and the validation entry points. There is no
// logic here: an alias makes the two literally the same type.
package profile

import internal "github.com/neuralmagic/vllm-vcr-operator/internal/profile"

// Contract types.
type (
	Metadata     = internal.Metadata
	Model        = internal.Model
	GPU          = internal.GPU
	Execution    = internal.Execution
	Cache        = internal.Cache
	VLLM         = internal.VLLM
	Coverage     = internal.Coverage
	Range        = internal.Range
	Trace        = internal.Trace
	Provisioning = internal.Provisioning
	Capabilities = internal.Capabilities
	Requirements = internal.Requirements
	Selection    = internal.Selection
	Candidate    = internal.Candidate
	Catalog      = internal.Catalog
)

// Validation entry points.
var (
	// Parse validates a metadata document against the contract.
	Parse = internal.Parse

	// NewCatalog builds a selection catalog from resolved profiles.
	NewCatalog = internal.NewCatalog

	// ValidateOverrides checks workload tunables against a profile's coverage.
	ValidateOverrides = internal.ValidateOverrides

	// RequireCapability reports whether a profile grants one serving capability.
	RequireCapability = internal.RequireCapability
)

// TraceFormatJSONL is the only trace format this contract defines.
const TraceFormatJSONL = internal.TraceFormatJSONL
