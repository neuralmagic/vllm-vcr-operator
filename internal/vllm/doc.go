// Package vllm resolves the effective vLLM configuration of a workload
// container.
//
// It answers one question — "what did the workload ask for?" — for the operator's
// fixed vLLM release, 0.33.0 (names.SupportedVLLMVersion). It does not decide
// whether a setting is supported: that is the settings policy in
// internal/translator, which consumes Config.
//
// # Encoded semantics
//
// The command line, config-file, and environment-variable semantics encoded here
// are those of vLLM 0.33.0, read from that release's sources:
//
//   - vllm/entrypoints/cli/serve.py, .../cli/openai.py, .../common/cli_args.py
//     and vllm/engine/arg_utils.py for the `vllm serve` interface, including the
//     BooleanOptionalAction spellings ("--enable-x"/"--disable-x"),
//     nargs="+" flags, the "-O" optimisation shorthand, and the rule that the
//     positional model argument overrides --model.
//   - vllm/utils/argparse_utils.py for the --config file format (long flag names,
//     nested objects as JSON, lists as repeated values), the precedence "command
//     line > config file > defaults", and dotted JSON arguments.
//   - vllm/config/*.py and vllm/envs.py for setting semantics and defaults; the
//     provenance of every default is recorded in defaults.go.
//
// A different release changes these semantics, so the target version is fixed per
// operator release and is reported as Config.Version.
//
// # Invocation forms
//
// Resolve accepts exec-form "vllm serve", the Python server modules
// (python -m vllm.entrypoints.openai.api_server, and
// python -m vllm.entrypoints.cli.main serve), and images whose default entrypoint
// is recorded in a versioned table (entrypoint.go). Everything else is rejected:
// shell wrappers, wrapper scripts, unknown executables, unknown images, and the
// other `vllm` subcommands. Nothing is executed to discover meaning.
//
// # Resolution order and provenance
//
// Settings are resolved from command-line arguments, the file named by --config,
// environment variables vLLM reads, and vLLM defaults, in that order of
// precedence. Config keeps "unset" distinct from "explicitly set": pointer fields
// are nil when the workload did not set them, and Origin reports the layer a value
// came from, so the policy layer never mistakes a resolved default for a workload
// request.
//
// # Rejection
//
// Unresolvable or unrecognised configuration is rejected, never approximated.
// A Rejection names the setting, the offending value, and the reason; credential
// values are replaced by RedactedValue and are never retained anywhere in Config.
// Resolve reports every independent problem it can establish, and returns no
// Config when anything is rejected.
//
// # Inputs
//
// The package performs no Kubernetes API calls. Invocation carries a typed
// container plus the Pod-level context the caller already holds: volumes, and the
// content of files and valueFrom environment variables when the caller could
// resolve them. Mounted configuration the caller cannot resolve — for example a
// ConfigMap read by the admission webhook is unavailable — is a rejection, because
// the alternative would be guessing what the workload asked for.
package vllm
