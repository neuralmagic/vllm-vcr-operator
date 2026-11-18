# vllm-vcr-operator — working agreement

Read before touching code.

## Sources of truth (in priority order)

1. `vllm-vcr-operator-spec.md` — the specification. Never edit it.
2. `docs/contracts/*.md` — binding contracts:
   - `profile-image.md` (profile OCI image + metadata schema)
   - `admission.md` (webhook behavior + provenance annotations)
   - `runtime-layout.md` (process/container layout)
3. `api/v1alpha1/` — the `SimulationEnvironment` API. Treat as frozen unless the
   ticket says otherwise; changes need an API review.
4. `internal/names/names.go` — shared identifiers. Do not duplicate literals.

Prior art for `vllm-vcr play`, trace formats, and llm-d fixture deployments lives
in `/home/acardace/work/vllm-vcr`:
`docs/control-api.md`, `docs/trace-replay/`, `deploy/llm-d-prefix-cache/`,
`deploy/trace-capture/base/offline-replay.yaml`, and `src/lib.rs` (`Opt`, the
`play` flags).

## Conventions

- Go 1.26, module `github.com/opendatahub-io/vllm-vcr-operator`.
- Tests are written first (see `/home/acardace/.agents/skills/tdd/SKILL.md`).
  Test behavior through exported interfaces; no assertions on private fields.
- Packages stay pure where possible. Kubernetes client access is confined to
  `internal/controller` and `internal/webhook`; `internal/profile`,
  `internal/vllm`, `internal/translator`, and `internal/runtime` must be
  unit-testable without a cluster.
- No secret values in logs, errors, annotations, events, or status.
- Rejection over silent approximation: unsupported configuration must produce an
  error that names the setting and the reason.
- Do not edit `cmd/manager/main.go` — the integrator wires packages there.

## Commands

```bash
go build ./...                  # compile
go test ./...                   # unit tests
make test-e2e                   # envtest acceptance suite (needs assets)
make manifests generate         # regenerate CRD/deepcopy after API edits
~/go/bin/controller-gen object:headerFile="" paths="./..."
```

`controller-gen` and `setup-envtest` are installed in `~/go/bin`.

## Layout

| Path | Owner topic |
| --- | --- |
| `api/v1alpha1` | SimulationEnvironment API |
| `internal/vllm` | vLLM invocation/config resolution |
| `internal/profile` | profile metadata, catalog, selection |
| `internal/translator` | settings policy → admission plan |
| `internal/runtime` | plan → Pod mutation/container specs |
| `internal/webhook` | admission + validation webhooks, secret projection |
| `internal/controller` | environment, device-plugin, profile reconcilers |
| `internal/deviceplugin` | kubelet device-plugin gRPC server |
| `internal/supervisor` | frontend/simulator lifecycle supervision |
| `internal/registry` | OCI profile metadata/digest resolution |
| `cmd/*` | binaries (manager, deviceplugin, supervisor, profileexport) |
| `config/` | kustomize manifests, CRD, RBAC, webhook config |
| `test/e2e` | envtest acceptance tests |
| `test/spike` | runtime/serving spike harnesses (skipped without images) |
