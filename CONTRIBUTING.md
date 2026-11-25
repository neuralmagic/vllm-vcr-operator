# Contributing

Thanks for helping. This document covers what you need to build, test, and get a
change merged.

## Before you start

Read, in this order:

1. `AGENTS.md` — the working agreement: package ownership, purity rules, and the
   "reject rather than approximate" principle.
2. `docs/contracts/` — the binding contracts between admission, runtime, and
   profiles. Change the contract and the code together, or not at all.
3. `docs/settings-policy.md` — every vLLM setting has an explicit disposition.
   Adding support means adding a row, a test, and a rejection example.

## Development loop

```bash
go build ./...
go test ./...              # unit tests, no cluster
make test-e2e              # envtest: real API server + webhooks (setup-envtest assets)
make test-cluster          # kind/podman acceptance suite
make manifests generate    # after api/v1alpha1 changes
hack/verify-manifests.sh   # cluster-free manifest checks (also in CI)
make check-upstream        # vllm-vcr pin and flag-surface drift
```

Tests are written first and behavior is asserted through exported interfaces.
Bugs found in production paths get a regression test that fails before the fix.

## Pull requests

- One logical change per PR. Generated files (`config/crd`, `zz_generated.*`) are
  committed with the change that caused them.
- CI must pass: gofmt, build, vet, unit tests, envtest suite, manifest checks.
- Commit messages say what changed and why the old behavior was wrong. If a
  behavior is intentionally rejected, say what would be needed to accept it.
- Do not add a capability to `docs/capability-matrix.md` without an executable
  test that demonstrates it. "The flag exists" is not evidence.
- No secret values anywhere: not in tests, fixtures, examples, or error strings.

## Renaming the module

The module path matches the repository: `github.com/neuralmagic/vllm-vcr-operator`.
If the repository moves to another org, update it in one change:

```bash
go mod edit -module github.com/<org>/vllm-vcr-operator
grep -rl 'github.com/neuralmagic/vllm-vcr-operator' --include='*.go' . | xargs sed -i 's|github.com/neuralmagic/vllm-vcr-operator|github.com/<org>/vllm-vcr-operator|g'
go mod tidy && go build ./...
```
