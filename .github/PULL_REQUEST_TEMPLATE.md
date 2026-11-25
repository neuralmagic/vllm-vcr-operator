## What and why

<!-- What changed, and why the previous behavior was wrong or insufficient. -->

## Contract impact

- [ ] No change to `docs/contracts/` or `docs/settings-policy.md`
- [ ] Contract updated in this PR (say which)

## Verification

<!-- Commands you ran. Do not claim a suite you did not run. -->

- [ ] `go build ./... && go vet ./... && go test ./...`
- [ ] `make test-e2e` (or explain why not)
- [ ] `hack/verify-manifests.sh`
- [ ] New capability has an executable test, not just a flag

## Safety

- [ ] No Secret values, tokens, or kubeconfig material in code, tests, docs, or output
- [ ] Rejections name the setting and the reason
