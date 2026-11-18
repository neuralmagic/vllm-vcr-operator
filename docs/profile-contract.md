# Profile contract and production

A **profile** is the recorded artifact that makes simulation faithful: the model
identity it was captured from, the accelerator and topology, the cache
representation, the recorded timing and token data, and the set of serving
capabilities that were validated against it. Profiles live outside this operator,
in their own Git repository, are packaged as OCI images, and are consumed here.

This page is the practical companion to the binding contract
[`docs/contracts/profile-image.md`](contracts/profile-image.md): it repeats what a
profile producer must produce, then explains how to build, publish, and use one,
including private registries.

## What a profile image contains

```text
/                              image filesystem
└── profile/
    ├── metadata.json          profile document (schema below)
    └── trace.jsonl.gz         recorded trace named by metadata.trace.file
```

Plus one OCI image config label, `vcr.opendatahub-io/profile-metadata`
(`names.ProfileMetadataLabel`), whose value is the **same** JSON document as
`/profile/metadata.json`. The label is what reconciliation reads, so the operator
can build its catalog without downloading trace layers; the file inside the image
is what the provisioner exports. They must be byte-identical.

The image also declares how to export itself:
`metadata.provisioning.entrypoint` is run as an init container with exactly one
argument, the target directory:

```text
<entrypoint...> /var/run/vcr/profile
```

The command must copy the metadata document and the trace file into that
directory and exit non-zero if either is missing or unreadable. This repository
ships that command as `cmd/profileexport` (`vcr-profile-export`), which:

- validates `metadata.json` against the schema (`internal/profile.Parse`);
- requires `trace.file` to exist, be a regular file, and be non-empty;
- stages a copy next to the target and `rename`s it into place, so a failed export
  never leaves a partial profile volume that serving could mistake for a valid one;
- prints a one-line JSON summary (`profileId`, both file names, byte counts,
  source, target) on success.

`make build` produces it at `bin/vcr-profile-export`. The fixture profile in
`test/fixtures/profiles/` is accepted by it; that is the one provisioning path
checked without any images (`go test ./test/fixtures/`).

## The metadata document

Annotated example — the authoritative schema is
[`docs/contracts/profile-image.md`](contracts/profile-image.md):

```json
{
  "schemaVersion": "vcr.opendatahub-io/profile/v1",
  "id": "qwen3-0.6b-h200-tp1-bf16",
  "model":   { "name": "Qwen/Qwen3-0.6B", "revision": "c9340e6f3",
               "tokenizer": "Qwen/Qwen3-0.6B", "tokenizerRevision": "c9340e6f3",
               "chatTemplateHash": "sha256:..." },
  "gpu":     { "type": "H200", "count": 1 },
  "execution": { "tensorParallel": 1, "pipelineParallel": 1,
                 "dtype": "bfloat16", "quantization": "" },
  "cache":   { "blockSize": 64, "enablePrefixCaching": true,
               "kvBlocks": 2048, "chunkedPrefill": true },
  "vllm":    { "version": "0.33.0", "configHash": "3f6c...bare-hex-sha256" },
  "coverage": {
    "maxModelLen":               { "min": 1, "max": 131072 },
    "maxNumSeqs":                { "min": 1, "max": 128 },
    "maxNumBatchedTokens":       { "min": 1, "max": 8192 },
    "longPrefillTokenThreshold": { "min": 0, "max": 4096 },
    "kvBlocks":                  { "min": 1, "max": 2048 },
    "schedulingPolicies": ["fcfs", "priority"],
    "kvMemoryConversion": false
  },
  "trace":   { "file": "trace.jsonl.gz", "format": "vllm-vcr/jsonl",
               "recordedTokens": true, "latencyModel": true, "kvEvents": true },
  "provisioning": { "entrypoint": ["/usr/local/bin/vcr-profile-export"],
                    "metadataFile": "metadata.json",
                    "traceFile": "trace.jsonl.gz" },
  "capabilities": { "chatCompletions": true, "completions": true,
                    "messages": false, "responses": false, "streaming": true,
                    "toolCallParsers": [], "reasoningParsers": [],
                    "kvEventReplay": false,
                    "metrics": ["vllm:num_requests_running",
                                "vllm:kv_cache_usage_perc"] }
}
```

Rules that decide whether a workload is admitted:

| Field | Rule |
| --- | --- |
| `schemaVersion` | Exactly `vcr.opendatahub-io/profile/v1`. An unknown version is unusable, never best-effort parsed. |
| `gpu.type` | The accelerator `spec.gpuType` names. Never inferred from device count. |
| `gpu.count` | The device quantity the recorded topology implies. A workload requesting 2 devices needs `gpu.count: 2` and a consistent `execution.tensorParallel`. |
| `execution.pipelineParallel` | Must be 1. Pipeline-parallel recording is not supported. |
| `vllm.version` | Must equal the operator's supported release (`names.SupportedVLLMVersion`, `0.33.0`). Compatibility is an exact match, never "close enough". |
| `coverage` | Authorizes overrides. **Absence is not permission**: a setting outside coverage, or a missing coverage entry, is rejected. |
| `trace.latencyModel` | Must be true. Timing always comes from the recorded latency; a profile with no model is rejected rather than served on invented timing. |
| `trace.recordedTokens` | Distinguishes content-identical replay from random tokens. Content assertions require it (and `capabilities.contentReplay`). |
| `trace.kvEvents` | Required before a workload may configure `--kv-events-config`. |
| `capabilities` | The validated serving surface. A flag being accepted by the frontend or simulator is **not** evidence of support. |
| `capabilities.contentReplay` | Requires `trace.recordedTokens`. |
| `capabilities.kvEventReplay` | Must be true before replay-based recovery can be claimed. The current simulator has no replay endpoint, so a reconcilable profile for this release declares `false`. |
| `provisioning.entrypoint` | Required. `provisioning.traceFile` must not conflict with `trace.file`; `provisioning.metadataFile` must be `metadata.json`. |

Selection is all-or-nothing (`internal/profile.Catalog.Select`): exactly one
profile must match the workload's model identity, revision, tokenizer, chat
template, GPU type and count, tensor parallel size, dtype, quantization, block
size, and prefix-caching mode. Zero matches and several matches are both
rejections, with a message that names the candidates and the reasons they missed.

## Build a profile image

Profile production is a separate deliverable; this section is the contract the
image must satisfy, not a capture procedure.

```dockerfile
# The profile repository's Dockerfile. Pinned by digest in its own release pipeline.
FROM scratch                       # or any base image: only /profile is read
COPY profile/metadata.json /profile/metadata.json
COPY profile/trace.jsonl.gz /profile/trace.jsonl.gz

# The label value must be byte-identical to /profile/metadata.json.
LABEL vcr.opendatahub-io/profile-metadata="{\"schemaVersion\":\"vcr.opendatahub-io/profile/v1\", ...}"

# The entrypoint the operator runs as the init container. `scratch` cannot host a
# Go binary, so a real image starts from a minimal base and copies
# vcr-profile-export in:
#   COPY --from=<vcr-profile-export build> /vcr-profile-export /usr/local/bin/vcr-profile-export
ENTRYPOINT ["/usr/local/bin/vcr-profile-export"]
```

Practical notes:

- The label value is a JSON string; most builders require the document to be
  escaped. Generate it from the metadata file so the two cannot drift — a
  reconciler that reads the label and a provisioner that reads the file must see
  the same document.
- Keep the image small: only `metadata.json` and the trace are read. The operator
  never downloads trace layers during admission, and the trace is fetched only
  when the Pod's init container runs.
- Tag with the recorded identity (`qwen3-0.6b-h200-tp1-bf16`), and publish the
  digest. The operator resolves a tag to a digest during reconciliation and pins
  the digest on every admitted Pod, so a re-tagged image cannot silently change a
  running test.

Validate before publishing:

```bash
# The schema, coverage, and capability rules.
go test ./internal/profile/...
# The export path, on a directory shaped like the image filesystem.
go run ./cmd/profileexport --source path/to/image-filesystem /tmp/exported
# A configured profile in a cluster (metadata label + digest resolution):
kubectl apply -f test/fixtures/environments/h200.yaml
kubectl get sime default -o jsonpath='{range .status.profiles[*]}{.image}{" "}{.digest}{" "}{.message}{"\n"}{end}'
```

## Publish and reference

```yaml
apiVersion: vcr.opendatahub-io/v1alpha1
kind: SimulationEnvironment
metadata:
  name: default
spec:
  gpuType: H200
  devicesPerNode: 8
  profiles:
    - image: quay.io/opendatahub/vllm-vcr-profiles:qwen3-0.6b-h200-tp1-bf16  # tag: pinned by reconciliation
    # - image: quay.io/opendatahub/vllm-vcr-profiles@sha256:...               # digest: pinned explicitly
  imagePullSecrets:
    - name: profile-pull-credentials
```

- A tag reference is resolved and pinned at reconciliation time; a digest
  reference is used as given. Either way the Pod annotation
  `vcr.opendatahub-io/profile-image` carries the digest.
- Up to 64 profile images may be configured. Each is reconciled independently;
  `status.profiles[]` reports the resolved digest, the parsed metadata, and a
  message for any image that is unusable.
- `spec.profiles[].pullSecretName` overrides `spec.imagePullSecrets` for one image.

## Private registries

Pull credentials are Kubernetes Secrets **in the operator namespace**
(`vllm-vcr-operator-system` by default), in docker-config JSON form:

```bash
kubectl -n vllm-vcr-operator-system create secret generic profile-pull-credentials \
  --from-file=.dockerconfigjson=credentials.json \
  --type=kubernetes.io/dockerconfigjson
```

```yaml
spec:
  imagePullSecrets:
    - name: profile-pull-credentials
  profiles:
    - image: registry.internal.example/vcr/qwen3-0.6b-h200-tp1-bf16
      pullSecretName: profile-pull-credentials   # optional per-image override
```

What the operator does with them:

- the reconciler uses the credentials to read the image's metadata label; a
  registry or credential failure is reported as the `RegistryUnavailable`
  condition with the Secret **name**, never a value;
- what admission must make available to the workload namespace is exactly the
  credentials the selected profile needs, under names prefixed
  `vcr-profile-pull-` (`names.PullSecretPrefix`) — the admission contract's rule
  (`docs/contracts/admission.md`); `make test-e2e` is the check that asserts it;
- no Secret value appears in a Pod annotation, status field, event, log line, or
  error message.

A non-docker-config Secret (for example a `.gitconfig` or a bare token) is
rejected with a message naming the expected key: only docker-config pull Secrets
can be used.

## What a profile cannot do

- **It cannot grant a capability the spike has not validated.** `capabilities`
  is the authority; see [`docs/capability-matrix.md`](capability-matrix.md).
- **It cannot authorize an override it does not cover.** Coverage ranges are the
  only authorization; a missing range means the workload's value is rejected.
- **It cannot make KV-event replay work.** The pinned simulator publishes live
  events and reports `replay_endpoint: null`; a profile that declared
  `kvEventReplay: true` would authorize a re-synchronization that does not exist.
- **It cannot change the operator's vLLM release.** The release is fixed per
  operator build; a profile recorded against another release is rejected.
