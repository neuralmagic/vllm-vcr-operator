// Package cluster is the kind-cluster acceptance suite: the specification's
// primary acceptance interface ("submit an unchanged GPU-requesting vLLM workload
// to a CPU-only Kubernetes cluster and observe synthetic allocation, placement,
// admission mutation, profile provisioning, and readiness/failure behavior").
//
// It runs against a real API server, a real kubelet, and the real operator
// binaries. What it cannot run is real vLLM serving and the Rust simulator, which
// are unavailable on the machines it targets; test/images provides documented test
// doubles for those two containers only, and test/cluster/README.md records exactly
// which claims the doubles do and do not support.
//
// The suite skips — with a reason naming the missing prerequisite — when the kind
// cluster or the images are absent. It never passes vacuously: every assertion is
// against cluster state (allocatable capacity, Pod conditions, admission
// annotations, container statuses, events).
//
// This package contains only test files: it is a test suite, not a library. The
// helpers are _test.go files so that `go build ./...` never ships them and so that
// nothing outside a test run can depend on them.
package cluster
