// Package registry resolves recorded-profile OCI images.
//
// A profile image carries its metadata document in the image config label
// names.ProfileMetadataLabel and its trace data in the image filesystem under
// /profile (see docs/contracts/profile-image.md). Reconciliation uses this
// package to turn a profile image reference into an immutable, digest-pinned
// reference plus the metadata bytes needed to select the profile; admission
// never calls it.
//
// # Metadata sources
//
// Resolve prefers the image config label because reading it fetches only the
// image config, never the trace layers. When the label is absent, Resolve falls
// back to the metadata document at /profile/metadata.json inside the image
// filesystem, scanning layers from the topmost down and stopping at the first
// layer that carries the file. The fallback therefore downloads at most the
// layers above and including the one that holds the document, never the full
// image. Resolved.Source reports which source produced the metadata.
//
// A label that is present but not valid JSON is a contract violation and is
// reported as an error rather than silently bypassed by the layer fallback.
//
// # Digest pinning
//
// A reference that is already pinned by digest is never re-resolved to a
// different digest: the requested digest is returned unchanged. A tag
// reference is resolved once and the caller pins the returned digest, so a
// later tag move cannot change an admitted or running simulation. For a
// multi-platform index the pinned digest is the digest of the resolved
// platform manifest (linux/amd64), which is the manifest the metadata and trace
// were read from.
//
// # Credentials
//
// Pull credentials are supplied through Options.Credentials so that this
// package stays free of Kubernetes imports and the reconciler can build
// authenticators from pull Secrets. The callback receives the registry host.
// Credential material is never included in returned errors or log output;
// callbacks must not place secret values in their error strings either.
//
// Options.Insecure disables TLS for a reference and exists only for test
// registries: profile images in a cluster are expected to be served over TLS.
package registry
