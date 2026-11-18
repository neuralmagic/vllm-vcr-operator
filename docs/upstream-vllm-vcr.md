# Upstream `vllm-vcr` pin

This operator is a translation layer for one specific `vllm-vcr` build: it emits
that build's `play` flags, expects its control API, its KV-event wire format, and
its trace format, and it advertises the vLLM release that build targets. This page
records *which* build, which facts the operator encodes from it, what was adapted
when it was last re-checked, and how to re-pin a future release.

Two files are the machine-readable half of this record:

| File | What it holds |
| --- | --- |
| [`hack/vllm-vcr-pin.env`](../hack/vllm-vcr-pin.env) | pinned commit, tag, verification date, `compat.toml` default line, target vLLM release |
| [`hack/vllm-vcr-play-flags.txt`](../hack/vllm-vcr-play-flags.txt) | every `vllm-vcr play` flag the translator can emit (kept honest by `TestPlayFlagSurfaceMatchesDeclaration`, which asserts the exported `translator.SimulatorFlags()` surface) |

[`hack/check-vllm-vcr-upstream.sh`](../hack/check-vllm-vcr-upstream.sh) compares
the pin, the flag surface, and the compat line against upstream and fails on real
drift. `make check-upstream` runs it.

## The pin

| Item | Value |
| --- | --- |
| Upstream repository | `https://github.com/neuralmagic/vllm-vcr` (HTTPS only: the project machines have no SSH key for it) |
| Commit | `22e64afcd7021da06a9ee3616574a2c03d380180` |
| Release tag | `v0.2.5` ("release: v0.2.5, honor HF_* env vars…"), tagged 2026-11-06 |
| Re-checked | 2026-11-18 |
| Upstream movement since the previous pin | **none**: `upstream/main` and the newest release tag are `22e64af` / `v0.2.5`, identical to the reference checkout `v0.2.5` at that commit |
| `compat.toml` default line | `0.33` (`tag = "v0.33.0"`, `default = true`); the window is `nightly`, `rc` (`v0.32.1rc0`), `0.33` (default), `0.32`, `0.31` — there is no `0.34` line |
| Target vLLM release | **0.33.0** (`internal/names.SupportedVLLMVersion`) |

The target stays 0.33.0 because the pinned build does not support any newer line:
a default-line bump upstream is a release-level change and is the only thing that
moves this operator's target (`compat.toml:64-67`, `internal/names/names.go`).

## How the check works

```
hack/check-vllm-vcr-upstream.sh            # fetch upstream over HTTPS, compare, report
hack/check-vllm-vcr-upstream.sh --no-fetch # compare against local refs only
hack/check-vllm-vcr-upstream.sh --verbose  # also list upstream flags we do not emit
```

Three surfaces, each reported on its own line:

| Surface | Compared | Fails when |
| --- | --- | --- |
| `pin` | `upstream/<ref>` commit and newest `vX.Y.Z` tag versus `hack/vllm-vcr-pin.env` | upstream moved: a classifier re-run is due |
| `flags` | every flag in `hack/vllm-vcr-play-flags.txt` versus the long options of the simulator's clap `Opt` struct (`src/lib.rs`) | the translator emits a flag the pinned build does not define |
| `compat` | `compat.toml`'s default line/tag versus `hack/vllm-vcr-pin.env` and `internal/names.SupportedVLLMVersion` | the simulator defaults to a vLLM line the operator does not translate |

Properties the script keeps:

- **Exit status**: `0` in sync or skipped, `1` on drift (one `DRIFT <surface>: …`
  line per drifted surface), `2` on a broken invocation or a missing repository
  file (the pin record, the flag list).
- **Offline-safe**: a surface whose input is unavailable (no network, no simulator
  checkout) is printed as `skip <surface>: …` and does not fail the run, so CI
  without network stays green. A surface that *can* be checked is always strict.
  `--no-fetch` needs no network at all.
- **Read-only towards the checkout**: the network surface uses `git ls-remote`,
  never `git fetch`, so no refs move and nothing is written into the simulator
  checkout. SSH remotes are refused outright.
- **Fixtures, no network**: `go test ./hack/...` drives the script against
  `hack/upstreamcheck/testdata/` (an `Opt` snippet, compat documents, a names
  constant) and a throwaway git repository, covering the in-sync, three drift,
  explicit-`long`-name, skip, and refusal paths.

## Facts the operator encodes

Every row was read from the pinned checkout; the citation is `path:line` there.

| Fact | Pinned evidence | Where the operator encodes it |
| --- | --- | --- |
| Invocation is `vllm-vcr play <flags>`; there are no global flags | `src/main.rs:18-52`, `src/lib.rs:107-376` | `internal/translator/frontend.go` (`backendInvocation`) |
| The frontend **binds** the engine-core handshake; the simulator connects. Default `tcp://127.0.0.1:29550` | `src/lib.rs:108-110`, `crates/sim-protocol/src/frontend_connect.rs:160-166` | `names.HandshakePort`, `docs/contracts/runtime-layout.md` |
| Control API is `--control-address HOST:PORT` — a plain socket address, **not** a ZMQ endpoint — with `/config`, `/stats`, `/stats/reset`, `/log`; off unless set | `docs/control-api.md:3`, `entrypoint.sh:76`, `src/lib.rs:210-215,955-957`, `src/control.rs:303-326` | `names.ControlPort` (8001), `internal/translator/frontend.go`, supervisor readiness input |
| The simulator binds the control address with `TcpListener::bind`, so any scheme in the value fails resolution and aborts `play` | `src/control.rs:319` | `internal/translator/frontend.go` (`--control-address 0.0.0.0:<port>`), `test/images/backend-stub` |
| KV events: `--enable-kv-cache-events`, `--kv-events-endpoint` (`tcp://*:5557` by default, wildcard binds), `--kv-events-topic` (default `kv@<engine-id>@<model>`) | `src/lib.rs:321-335,438-450`, `src/kvevents.rs:81-101` | `names.KVEventsPort` (5556, the llm-d port), `internal/translator/effective.go` (`resolveKVEvents`) |
| KV events are ZMQ **PUB**: topic frame, 8-byte big-endian sequence, msgpack payload; block hashes are binary big-endian | `src/kvevents.rs:8-22,103-110` | Documented, not produced, by the operator: `docs/capability-matrix.md` |
| **No KV-event replay endpoint exists**: the ready response always reports `replay_endpoint: null` and the publisher has no responder | `crates/sim-protocol/src/frontend_connect.rs:92,107`, `src/kvevents.rs:165-228` | Admission rejects an explicit `replay_endpoint` unconditionally, naming the pin (`names.SupportedSimulatorTag`); the profile `capabilities.kvEventReplay` gate stays behind it (`internal/translator/effective.go`) |
| Replay inputs: `--latency-trace` (modeled timing), `--replay-tokens` (recorded output ids), `--replay-match index|prefix`, `--expect-config-hash` | `src/lib.rs:141-170,226-230` | `internal/translator/frontend.go` (`traceArgs`) |
| `--replay-match prefix` requires per-record `block_hashes` and, for dataset traces, `--model-name`; unmatched requests silently fall back to random tokens | `src/lib.rs:482-520`, `docs/trace-replay/content-identical.md:8-29` | Profile contract obligation (`docs/contracts/profile-image.md`); replay warning in every plan |
| `config_hash` is a **bare lowercase-hex** SHA-256 stamped into the trace header and compared **verbatim** (never recomputed) | `crates/sim-trace/src/config_hash.rs:62-70`, `src/lib.rs:604-641` | `internal/profile.ConfigHashValue` strips the profile schema's optional `sha256:` tag before emission |
| `--shutdown-timeout` is a grace period in **seconds**; `0` aborts unfinished requests immediately; SIGTERM and SIGINT both drain | `src/lib.rs:370-375,849-889`, `src/engine_core.rs:66-170` | `internal/translator/effective.go`, `internal/supervisor` (same budget for the frontend) |
| The simulator emits **no** Prometheus metrics; metrics are frontend-derived | `src/control.rs:303-311`, `docs/control-api.md:26-35` | `docs/capability-matrix.md` ("Any series from the simulator itself: does not exist") |
| Only five flags have env backing (`MOCK_ENGINE_ID`, `MOCK_SIDE_CHANNEL_HOST`, `MOCK_SIDE_CHANNEL_PORT`, `MOCK_CONTROL_ADDRESS`, `MODEL`); every other `MOCK_*` name is an `entrypoint.sh` invention, and the operator replaces that entrypoint | `src/lib.rs:194-199,203,214,340`, `entrypoint.sh:9-118` | `internal/translator/frontend.go` emits **flags**; the backend container only gets `RUST_LOG` |
| `--seed` is mixed with the engine index **and the request id**, so random tokens are reproducible only per request id | `src/engine.rs:48-55` | Documented in `docs/capability-matrix.md` (`--seed` row) |
| Advertised `vllm_version` is the build-stamped `compat.toml` default tag; `--max-model-len 0` advertises 1 MiB | `src/lib.rs:592-598,694-708` | Translated away: the operator always passes the profile-bounded length |
| Ports: handshake 29550, control 8001 (image entrypoint default), KV events 5557 (binary) / 5556 (llm-d manifests), NIXL side channel 5600 | `src/lib.rs:108,329,203`, `entrypoint.sh:76,78-83` | `internal/names` (fixed ports), one Pod network namespace, collision rejection in `internal/translator` |
| The simulator has no GPU-only flag; the image entrypoint is a shell script the operator bypasses by replacing the container command | `src/lib.rs:107-376`, `Dockerfile:113,126`, `entrypoint.sh:1-7` | `docs/settings-policy.md` ("No GPU-only flag is ever passed") |

The research notes in
`/home/acardace/work/vllm-vcr-operator-research/notes/vllm-vcr.md` carry the full
flag/default table with citations; this page records only what the operator acts
on, plus the adaptations below.

## What this re-check adapted

Upstream had not moved, so no default, spelling, or version changed. Two encoded
facts were wrong and are fixed here; both would have broken a real (non-stub)
deployment at simulator startup, and both had been masked by the test double and
by fixtures that copied the wrong form.

### 1. `--control-address` is `HOST:PORT`, not a ZMQ endpoint

The translator emitted `--control-address tcp://0.0.0.0:8001`. The simulator hands
that value to `TcpListener::bind` (`src/control.rs:319`), which accepts
`HOST:PORT` only: a scheme makes resolution fail ("failed to lookup address
information: Name or service not known", reproduced with tokio 1.53.1), the control
task returns an error, and `play` exits (`src/lib.rs:955-975`). Upstream documents the form as
`HOST:PORT` (`docs/control-api.md:3`) and its own entrypoint passes
`0.0.0.0:${MOCK_CONTROL_PORT:-8001}` (`entrypoint.sh:76`).

Adapted to `0.0.0.0:<control port>`, in `internal/translator/frontend.go` and the
spike harness (`test/spike/settings.go`), pinned by
`TestBackendArgumentsAreSimulatorFlags` and
`TestBackendCommandConnectsAndServesTheProfile`. The cluster test double
(`test/images/backend-stub`) used to strip the scheme, which is exactly why the
kind suite never noticed; it now rejects a scheme the way the real binary does
(`TestListenAddressRejectsSchemes`) and keeps its separate ZMQ-endpoint handling
for the KV-event port.

### 2. `vllm.configHash` is compared verbatim and has no `sha256:` tag

The profile schema showed `"configHash": "sha256:…"` and the translator passed the
value straight to `--expect-config-hash`. The simulator compares the recorded
`meta.config_hash` as an opaque string (`src/lib.rs:626-631`), and what it stamps
is bare lowercase hex (`crates/sim-trace/src/config_hash.rs:62-70`), so a prefixed
profile value could never match: the simulator would refuse to start. Stripping
the tag in the operator would also have been silent for a value that is *only* the
tag, so metadata validation now rejects that.

Adapted in `internal/profile` (`ConfigHashValue`, plus a `Parse` rule) and
`internal/translator` (`traceArgs`), pinned by
`TestBackendArgumentsAreSimulatorFlags`,
`TestParseAcceptsContractDocument` and the
`config hash carries only the algorithm tag` rejection case. The contract text in
[`docs/contracts/profile-image.md`](contracts/profile-image.md) now states the
expected form.

### No change for

`play` flag names and their defaults (all 23 emitted flags still exist with the
same spelling); the `compat.toml` default line and the target vLLM release; the
handshake ownership and port; KV-event flags, wire format, topic form, and the
absent replay endpoint; the trace format and its `arrival_ms`/`block_hashes`
requirements; the config-hash scheme (`config-fingerprint-v3`); the absence of
simulator metrics; the traced `MODEL`/`MOCK_*` environment reality; shutdown and
signal semantics; the advertised `vllm_version`.

## Re-pinning a future release

1. **Re-check.** `make check-upstream` (add `--verbose` to see upstream flags the
   translator does not emit). A `skip` line means a surface could not be checked,
   not that it passed.
2. **Classify.** For each `DRIFT` line, use a *separate clean worktree outside this
   repository* — never the reference checkout, whose working tree is read-only for
   this purpose:

   ```bash
   git -C /home/acardace/work/vllm-vcr worktree add /home/acardace/work/vllm-vcr-latest <new-commit>
   ```

3. **Diff the surfaces this operator acts on**, in that order of blast radius:
   - `compat.toml` default line → target release. A bump is a **release-level
     change**: `internal/names.SupportedVLLMVersion`, the pin file, defaults,
     spellings, tests, and docs move together, or nothing moves.
   - `vllm_vcr::Opt` (`src/lib.rs`) → flag names, defaults, validation. Rename or
     drop a flag the translator emits and `internal/translator` plus
     `hack/vllm-vcr-play-flags.txt` change together.
   - Trace/replay surfaces (`--replay-tokens`, `--replay-steps`,
     `--replay-match`, `--expect-config-hash`, `config_hash`) → the profile
     contract, the translator, and `internal/profile` metadata.
   - KV events (flags, endpoint, topic, wire format, and whether a **replay
     endpoint** appeared) → `internal/names`, the translator's capability gate,
     and the capability matrix. Relax a rejection only with a profile capability
     *and* a passing test.
   - Env-var backing (`env = "…"`, `MODEL`, `MOCK_*`) → the backend env and
     `internal/names`.
   - Ports, control-API routes, shutdown/readiness semantics → `internal/names`,
     `internal/supervisor`, `docs/contracts/runtime-layout.md`.
   - Metrics: if the simulator ever exports Prometheus metrics, the capability
     matrix and the metrics section of `docs/settings-policy.md` change; until
     then every `vllm:*` series stays frontend-derived.
4. **Adapt test-first** at the existing seams (`internal/vllm`,
   `internal/translator`, `internal/supervisor`, `internal/profile`), updating
   `hack/vllm-vcr-play-flags.txt` only when the emitted surface really changes.
5. **Re-record.** Update `hack/vllm-vcr-pin.env` (commit, tag, dates, compat line,
   target), this page (pin table, encoded facts, adaptation section), and the
   research notes with the new `path:line` citations.
6. **Gate.** `gofmt -w` the changed files, `go build ./...`, `go vet ./...`,
   `go test ./...`, `make check-upstream`, and any suite whose assertions the
   change touches.

A drift the operator does not adapt is a finding to report, not a check to
weaken: the `flags` and `compat` surfaces stay strict even when they are
inconvenient.
