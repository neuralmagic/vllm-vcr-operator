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
make test-race             # the same set under the race detector
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
- CI must pass. `.github/workflows/ci.yaml` is the pre-submit gate and runs the same
  set on `main`, so a merge cannot land with a check that only ever ran on branches:
  gofmt, build, vet, unit tests, the race detector, `go mod tidy` drift, the envtest
  suite, generated-file drift, the manifest and documentation checks, and the
  sign-off check below. The three container images the release publishes are built
  too (no push), so a broken Dockerfile is found at the change that broke it.
- `.github/workflows/cluster-e2e.yaml` runs the kind acceptance suite
  (`make test-cluster`). It is opt-in because it starts a cluster and builds six
  images: add the `test/cluster` label to the pull request, or dispatch the workflow
  by hand. Use it when the change touches admission, the device plugin, the runtime
  layout, or anything else a cluster-free test cannot observe. It runs with
  `VCR_CLUSTER_REQUIRED=1`, so a missing prerequisite fails the job instead of
  reporting a green skip.
- Commit messages say what changed and why the old behavior was wrong. If a
  behavior is intentionally rejected, say what would be needed to accept it.
- Do not add a capability to `docs/capability-matrix.md` without an executable
  test that demonstrates it. "The flag exists" is not evidence.
- No secret values anywhere: not in tests, fixtures, examples, or error strings.

## Sign-off

Every commit must carry a `Signed-off-by:` trailer naming that commit's own author.
It is the [Developer Certificate of Origin](https://developercertificate.org/)
statement: you wrote the change, or you have the right to submit it. Sign-off is not
a copyright assignment — you keep the copyright.

```bash
git commit -s                 # a single commit
git rebase --signoff <base>   # every commit on your branch
hack/verify-dco.sh            # check the branch before pushing
```

The trailer must name the commit's author, not a reviewer or a maintainer on their
behalf; adding somebody else's sign-off is exactly what the certificate forbids. A
branch co-written by two people therefore stays on its authors' sign-offs. Merge
commits are exempt: they carry no change of their own, and requiring a trailer on
them would make "merge main into my branch" fail for a reason unrelated to the
contribution.

`.github/workflows/ci.yaml` runs `hack/verify-dco.sh` over every commit a pull
request adds, so an unsigned commit fails the same gate as a failing test.

## Renaming the module

The module path matches the repository: `github.com/neuralmagic/vllm-vcr-operator`.
If the repository moves to another org, update it in one change:

```bash
go mod edit -module github.com/<org>/vllm-vcr-operator
grep -rl 'github.com/neuralmagic/vllm-vcr-operator' --include='*.go' . | xargs sed -i 's|github.com/neuralmagic/vllm-vcr-operator|github.com/<org>/vllm-vcr-operator|g'
go mod tidy && go build ./...
```
