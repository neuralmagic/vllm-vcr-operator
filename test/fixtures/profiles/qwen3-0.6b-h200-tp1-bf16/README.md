# Placeholder profile fixture

This directory is **not a recording**. It is a profile *image filesystem* with
the layout `docs/contracts/profile-image.md` defines, used to check that:

- the fixture metadata document parses under `internal/profile` (`profile.Parse`);
- `vcr-profile-export` exports a profile image filesystem that satisfies the
  contract, which is the entrypoint the operator's `vcr-profile-provisioner` init
  container runs.

Everything a real profile would establish is explicitly absent:

| Field | Value | Why |
| --- | --- | --- |
| `id` | `placeholder-qwen3-0.6b-h200-tp1-bf16` | No profile release exists in this repository. |
| `trace.recordedTokens` | `false` | The trace file carries a `meta` line only; there are no output token ids. |
| `trace.latencyModel` | `false` | No latency was recorded, so nothing can be replayed for timing. |
| `trace.kvEvents` | `false` | No KV-cache events were captured. |
| `capabilities` | all `false` | The serving-behavior spike has not validated any capability for this profile. |

`trace.jsonl.gz` therefore exists only so the provisioner has the trace file its
metadata declares: a profile whose trace file is missing or empty is refused by
`vcr-profile-export`, and that refusal is part of what the fixture checks.

The spike harnesses do **not** use this directory. They require
`VCR_PROFILE_IMAGE` or `VCR_PROFILE_DIR` to name a real recorded profile, because
a spike served from this fixture would produce evidence for a recording that does
not exist. See `test/spike/README.md`.
