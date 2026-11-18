// Fixture: the `Opt` struct of vllm-vcr v0.2.5, copied verbatim from
// src/lib.rs:107-376. testdata is not compiled or linted; it exists so the flag
// extraction and the drift reporting can be tested without a network or a
// simulator checkout.
pub struct Opt {
    /// Frontend-owned ZMQ handshake address.
    #[arg(long, default_value = "tcp://127.0.0.1:29550")]
    pub handshake_address: String,

    /// Number of mock engine identities to register with the frontend.
    #[arg(long, default_value_t = 1)]
    pub engine_count: usize,

    /// Prefill/decode role this process plays.
    #[arg(long, value_enum, default_value_t = PdRole::Both)]
    pub pd_role: PdRole,

    /// Number of accepted output tokens included in each EngineCoreOutput.
    #[arg(long, default_value_t = 1)]
    pub output_token_chunk_size: usize,

    /// Random token IDs are sampled uniformly from 0..vocab_size.
    #[arg(long, default_value_t = 32_000)]
    pub vocab_size: u32,

    /// Path or `s3://bucket/key`/`hf://org/repo/file` URI to a JSONL trace whose
    /// recorded output token ids (tap `--record-tokens`) are served verbatim instead
    /// of random tokens, making replayed streams content-identical to the capture.
    /// A request resolves to its record through the trailing `-<index>` of its request
    /// id (the arrival-replay harness names them `replay-{i}`) or, with
    /// `--replay-match prefix`, by longest block-hash prefix match. Unmatched requests
    /// fall back to random tokens.
    ///
    /// Also accepts HuggingFace-style dataset files (JSON/JSONL/CSV/Parquet): the
    /// dataset is loaded in memory at startup, prompts and responses are tokenized
    /// with the HuggingFace model named by `--model-name` / `MODEL` (default
    /// [`tokens::DEFAULT_DATASET_TOKENIZER`]), and output tokens are served directly
    /// via [`tokens::HFDatasetTokens`] — no trace conversion.
    #[arg(long)]
    pub replay_tokens: Option<TraceUri>,

    /// JSONL trace (path or `s3://bucket/key` URI) whose recorded per-chunk
    /// decode timing (`itl_ms` gaps and
    /// `itl_tokens` burst sizes) is replayed VERBATIM, the timing/framing
    /// analogue of `--replay-tokens`. This is the *replay* timing axis;
    /// `--latency-trace` is the *modeled* one (gaps and bursts sampled from a
    /// fitted model). Each matched request emits its recorded chunk sizes
    /// (speculative-decoding bursts, diffusion blocks) at its recorded gaps and
    /// stops at the recorded length; unmatched requests stay on the configured
    /// latency model. Requests resolve to records via `--replay-match`.
    #[arg(long)]
    pub replay_steps: Option<TraceUri>,

    /// How `--replay-tokens` and `--replay-steps` map incoming requests to
    /// trace records: `index` trusts the request id's trailing `-<i>` (our own
    /// replay harness), `prefix` matches the incoming prompt's block-hash chain
    /// against the records (live clients, e.g. an agent re-run offline against
    /// the sim).
    #[arg(long, value_enum, default_value_t = ReplayMatch::Index)]
    pub replay_match: ReplayMatch,

    /// Refuse to replay unless every provided trace's recorded `config_hash`
    /// matches this value. The CI profile-once/replay-many cache stamps the
    /// hash at capture (tap `--config-hash`); checking it here stops a trace
    /// from being replayed against a config it was not captured for. Empty
    /// (default) disables the check.
    #[arg(long, default_value = "")]
    pub expect_config_hash: String,

    /// vLLM version this engine advertises in the registration ready response.
    /// Defaults to the build's target line (stamped from `compat.toml` at build
    /// time, or overridden by the CI matrix via the `VLLM_TARGET_VERSION` env);
    /// set this only for capture runs that must mimic an exact tag. Empty means
    /// "use the build stamp".
    #[arg(long, default_value = "")]
    pub vllm_version: String,

    /// Base seed for deterministic random token generation.
    #[arg(long, default_value_t = 0)]
    pub seed: u64,

    /// Bytes per fabricated KV block (NIXL data plane only).
    #[arg(long, default_value_t = 128 * 1024)]
    pub kv_block_bytes: usize,

    /// Prompt tokens that map to one KV block (NIXL data plane only).
    #[arg(long, default_value_t = 16)]
    pub tokens_per_block: usize,

    /// This engine's id, advertised as `remote_engine_id` in kv_transfer_params.
    /// Set per-pod (e.g. from POD_NAME) so a decode peer can address this prefill.
    #[arg(long, env = "MOCK_ENGINE_ID", default_value = "mock-engine-0")]
    pub engine_id: String,

    /// Host advertised as `remote_host` for the NIXL metadata side channel
    /// (set to the pod IP in k8s).
    #[arg(long, env = "MOCK_SIDE_CHANNEL_HOST", default_value = "127.0.0.1")]
    pub side_channel_host: String,

    /// Port advertised as `remote_port` for the NIXL metadata side channel.
    #[arg(long, env = "MOCK_SIDE_CHANNEL_PORT", default_value_t = 5600)]
    pub side_channel_port: u32,

    /// Log a summary line for each request.
    #[arg(long)]
    pub log_requests: bool,

    /// Bind address for the HTTP control API (`GET/PATCH /config`, `GET /stats`,
    /// `POST /stats/reset`, `GET/PUT /log`), which reads and changes the latency,
    /// scheduler-cap, failure-injection, and log-filter knobs at runtime and reports
    /// per-request counters. Unset (default) disables the API.
    #[arg(long, env = "MOCK_CONTROL_ADDRESS")]
    pub control_address: Option<String>,

    // === Latency model (all milliseconds; 0 = instant, the default) ===
    // Ported from llm-d-inference-sim. The real frontend measures TTFT/ITL from when we
    // emit tokens, so these knobs drive both response timing and the vllm:* latency metrics.
    /// Path or `s3://bucket/key` URI to a JSONL trace file for MODELED latency. Mutually exclusive with the
    /// timing knobs below (time_to_first_token, inter_token_latency, prefill_*). When set,
    /// first-token and inter-token delays are SAMPLED from a model fitted to the recorded
    /// observations (conditioned on concurrency, context depth, prompt size) rather than
    /// synthesized from knob parameters or played back verbatim, so a model fit on one
    /// workload transfers to another. For bit-exact per-request timing/framing replay
    /// instead, use `--replay-steps`. KV-transfer timing for do_remote_prefill requests
    /// still uses the kv_cache_transfer_* knobs (the trace does not cover P/D transfer
    /// latencies).
    #[arg(long)]
    pub latency_trace: Option<TraceUri>,

    /// Fixed time-to-first-token. When this and its std-dev are 0, the token-count prefill
    /// model (`--prefill-overhead` + `--prefill-time-per-token`) is used instead.
    #[arg(long, default_value_t = 0)]
    pub time_to_first_token: u64,

    /// Standard deviation for time-to-first-token.
    #[arg(long, default_value_t = 0)]
    pub time_to_first_token_std_dev: u64,

    /// Time to generate one output token (decode step).
    #[arg(long, default_value_t = 0)]
    pub inter_token_latency: u64,

    /// Standard deviation for inter-token latency.
    #[arg(long, default_value_t = 0)]
    pub inter_token_latency_std_dev: u64,

    /// Fixed prefill overhead, added once per request in the token-count prefill model.
    #[arg(long, default_value_t = 0)]
    pub prefill_overhead: u64,

    /// Per-(uncached-)prompt-token prefill cost in the token-count prefill model.
    #[arg(long, default_value_t = 0)]
    pub prefill_time_per_token: u64,

    /// Standard deviation for the token-count prefill time.
    #[arg(long, default_value_t = 0)]
    pub prefill_time_std_dev: u64,

    /// Fixed KV-cache transfer time for a `do_remote_prefill` decode request. When this and
    /// its std-dev are 0, the per-token transfer model is used instead.
    #[arg(long, default_value_t = 0)]
    pub kv_cache_transfer_latency: u64,

    /// Standard deviation for the fixed KV-cache transfer time.
    #[arg(long, default_value_t = 0)]
    pub kv_cache_transfer_latency_std_dev: u64,

    /// Per-prompt-token KV-cache transfer cost for a `do_remote_prefill` decode request.
    #[arg(long, default_value_t = 0)]
    pub kv_cache_transfer_time_per_token: u64,

    /// Standard deviation for the per-token KV-cache transfer cost.
    #[arg(long, default_value_t = 0)]
    pub kv_cache_transfer_time_std_dev: u64,

    /// Latency multiplier at full load (`>= 1.0`; 1.0 disables load scaling). Latency grows
    /// linearly from 1.0 with one request to this value at `--max-num-seqs` concurrent ones.
    #[arg(long, default_value_t = 1.0)]
    pub time_factor_under_load: f64,

    /// Maximum requests running (in the model batch) at once (vLLM `--max-num-seqs`). Excess
    /// requests wait in an unbounded FIFO/priority queue, producing `vllm:num_requests_waiting`
    /// and realistic backpressure. Also the load-factor denominator. vLLM never rejects on
    /// queue length, so neither do we.
    #[arg(long, default_value_t = 128)]
    pub max_num_seqs: u64,

    /// Per-step token budget (vLLM `--max-num-batched-tokens`): the running batch's per-step
    /// token demand (1 per decoding request + each prefilling request's prompt chunk) cannot
    /// exceed this. Throttles prefill admission under load even when batch slots are free.
    #[arg(long, default_value_t = 2048)]
    pub max_num_batched_tokens: u64,

    /// Chunked-prefill cap (vLLM `--long-prefill-token-threshold`): a single prefill consumes
    /// at most this many tokens of budget per step. `0` (default) disables the cap (a prefill
    /// is bounded only by the token budget).
    #[arg(long, default_value_t = 0)]
    pub long_prefill_token_threshold: u64,

    /// Waiting-queue ordering (vLLM `--scheduling-policy`): `fcfs` (arrival order) or
    /// `priority` (smaller `priority` value first, ties by earlier arrival).
    #[arg(long, value_enum, default_value_t = SchedulingPolicy::Fcfs)]
    pub scheduling_policy: SchedulingPolicy,

    /// KV-cache capacity in blocks, used to report `vllm:kv_cache_usage_perc`.
    #[arg(long, default_value_t = 1024)]
    pub kv_cache_size: u64,

    /// Maximum distinct LoRA adapters allowed in the running batch at once (vLLM
    /// `--max-loras`). A request needing an adapter not already resident waits once the batch
    /// holds this many distinct adapters. `0` (default) disables the cap; adapter accounting
    /// (the `running_lora_adapters`/`waiting_lora_adapters` stats behind
    /// `vllm:lora_requests_info`) is always on regardless. Advertised in the ready response
    /// as `max_loras` with `supports_lora = max_loras > 0`, so a 0.29+ frontend only routes
    /// LoRA requests to the sim when this is set.
    #[arg(long, default_value_t = 0)]
    pub max_loras: u32,

    // === KV-cache events (vLLM `--kv-events-config`) ===
    /// Publish KV-cache events (BlockStored/BlockRemoved/AllBlocksCleared) over ZMQ so the
    /// llm-d cache-aware router can index this engine's prefix cache.
    #[arg(long, default_value_t = false)]
    pub enable_kv_cache_events: bool,

    /// ZMQ PUB endpoint for KV-cache events. Wildcards (`tcp://*:5557`) bind; concrete hosts
    /// connect. Offset by engine index when running multiple engines in one process.
    #[arg(long, default_value = "tcp://*:5557")]
    pub kv_events_endpoint: String,

    /// KV-cache event topic. The llm-d router expects `kv@<pod-id>@<model-name>` (its SUB
    /// filter defaults to `kv@`). Leave empty to auto-build from `--engine-id`/`--model-name`.
    #[arg(long, default_value = "")]
    pub kv_events_topic: String,

    /// Served model name: builds the default KV-event topic and tokenizes HuggingFace
    /// dataset rows for `--replay-tokens` (defaults to [`tokens::DEFAULT_DATASET_TOKENIZER`
    /// when unset).
    #[arg(long, env = "MODEL", default_value = "")]
    pub model_name: String,

    /// Fixed seed chaining the first block of every sequence's hash (vLLM's `NONE_HASH`).
    /// Pinned (not random) so block hashes are reproducible across restarts and peers.
    #[arg(long, default_value_t = 0)]
    pub kv_cache_none_seed: u64,

    // === Failure injection (Phase 5) ===
    /// Maximum context length (prompt + output tokens). `0` (default) disables the check; a
    /// request whose `prompt + max_tokens` exceeds it finishes immediately with a length error.
    #[arg(long, default_value_t = 0)]
    pub max_model_len: u64,

    /// Probability in `[0, 1]` that an incoming request is failed on arrival. `0` disables
    /// injection.
    #[arg(long, default_value_t = 0.0)]
    pub failure_injection_rate: f64,

    /// Divide every model delay by this factor (time compression for fast
    /// calibration loops). Wire-level measurements must be re-multiplied by
    /// the same factor; timer granularity and transport jitter do not scale,
    /// so fidelity degrades as the factor grows. `1.0` is real time.
    #[arg(long, default_value_t = 1.0)]
    pub time_scale: f64,

    /// Failure kinds to inject (one is chosen uniformly per injected failure).
    #[arg(long, value_enum, value_delimiter = ',', default_value = "error")]
    pub failure_types: Vec<FailureType>,

    /// Graceful-shutdown grace period in seconds (vLLM `shutdown_timeout`). On
    /// SIGTERM/SIGINT the engine rejects new requests and lets in-flight ones finish for
    /// up to this long; whatever remains is then aborted. `0` (vLLM's default) aborts
    /// every in-flight request immediately.
    #[arg(long, default_value_t = 0)]
    pub shutdown_timeout: u64,
}