# Spike harnesses

The specification requires two spikes before the runtime layout and the supported
capability set are settled:

| Harness | Establishes | Evidence |
| --- | --- | --- |
| [runtime-layout](runtime_layout_test.go) | Startup ordering, readiness transitions, backend loss, frontend loss, coordinated SIGTERM shutdown | `evidence/runtime-layout.json` |
| [serving-behavior](serving_behavior_test.go) | Generation, streaming, tokenizer consistency, metric names and meaning, random versus recorded tokens, unmatched replay, cache pressure, KV-event publication | `evidence/serving-behavior.json` |

`docs/spikes/runtime-layout.md` and `docs/spikes/serving-behavior.md` interpret
the evidence; no claim in them may exceed what an evidence file shows.

## Running

```bash
export VCR_FRONTEND_IMAGE=<Python vLLM frontend image>   # required
export VCR_BACKEND_IMAGE=<vllm-vcr image>                # required
export VCR_PROFILE_IMAGE=<profile OCI image>             # required unless VCR_PROFILE_DIR
# or: export VCR_PROFILE_DIR=<already exported profile directory>

make test-spike                     # runs both harnesses
go test ./test/spike/ -run TestRuntimeLayout -v
```

Optional:

| Variable | Meaning |
| --- | --- |
| `VCR_PROFILE_DIR` | An exported profile directory (metadata plus trace) instead of a profile image. The provisioning entrypoint is then not exercised. |
| `VCR_SPIKE_REQUIRED=1` | Turn a missing environment into a failure. Use this in a CI job whose purpose is to produce evidence. |
| `VCR_SPIKE_RUNTIME` | Container engine to use (`podman` or `docker`). Defaults to podman, then docker. |
| `VCR_SPIKE_MODEL` | Served model name. Defaults to `Qwen/Qwen3-0.6B`; it must match the profile's model. |

Without the images the harnesses skip. They never build, pull, or substitute an
image: an evidence document names images a reader can pull and re-run.

## What a run needs

- a container engine on `PATH` (podman or docker);
- the images above, pulled or loadable by that engine;
- free ports `8000` (inference), `5556` (KV events), `8001` (simulator control
  API), `8082` (supervisor health), and `29550` (engine-core handshake). The
  containers run with `--network host`, which is what gives the frontend and the
  simulator the single loopback interface a Pod gives them;
- network access or a warm cache for the model's tokenizer assets, unless the
  profile's frontend image already carries them. `HF_TOKEN`, `HF_HOME`,
  `HF_HUB_CACHE`, `HF_HUB_OFFLINE`, `HF_HUB_DISABLE_XET`, `HF_ENDPOINT`, and
  `VLLM_ENGINE_READY_TIMEOUT_S` are forwarded to the frontend container when set
  on the host. A credential is forwarded, never logged, and never written to
  evidence.

## Evidence format

Every run writes one document:

```text
test/spike/evidence/
├── runtime-layout.json      # written by a run
├── serving-behavior.json    # written by a run
├── schema.example.json      # tracked format reference
└── diagnostics/<container>.log
```

Everything except `.gitignore` and `schema.example.json` is ignored by Git,
because an evidence document describes the machine that produced it: a committed
one would read as a current demonstration forever.

The format is `vcr.opendatahub-io/spike-evidence/v1`
(`test/spike/evidence.go`). Fields:

| Field | Meaning |
| --- | --- |
| `spike` | Harness name. |
| `startedAt` / `finishedAt` | Bounds of the run. |
| `images` | Exact image references, including the profile. |
| `environment` | Container engine, served model, profile source and directory, endpoints the checks drove. |
| `steps[]` | One entry per check: `name`, timings, `outcome` (`passed`/`failed`), and the `detail` that explains a failure. |
| `observations[]` | Recorded facts that are not pass/fail checks, for example exit codes and the recovery behavior the pinned images exhibited. |
| `notDemonstrated[]` | Everything the run did not establish. |
| `passed` | True only when every step passed **and** `notDemonstrated` is empty. A run that states a gap is not a clean pass. |

`TestSchemaExampleIsAValidDocument` decodes the tracked example, so the documented
format and the Go types cannot drift apart.

## Reading a failure

A failed step is a finding about the pinned images, not a harness bug:

- the runtime-layout assertions target `docs/contracts/runtime-layout.md`, so a
  failure is a layout change or an explicit rejection;
- the serving-behavior assertions target the capability matrix, so a failure is a
  row that must become `rejected` or identified implementation work.

Attach the evidence document to whatever records the finding. Do not delete the
harness's `diagnostics/` output: it holds the container logs the step failed to
explain.
