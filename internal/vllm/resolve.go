package vllm

import (
	"strings"
)

// Resolve turns a workload container into the effective vLLM configuration it
// asked for, or reports why the invocation cannot be resolved.
//
// Resolution answers "what did the workload ask for?" under the semantics of the
// operator's fixed vLLM release (Config.Version). It does not decide whether a
// setting is supported: that is the settings policy's job, and Config keeps
// unset settings distinct from explicit ones so the policy can tell a resolved
// default from a workload request.
//
// Settings are resolved from command-line arguments, the config file named by
// --config, environment variables vLLM reads, and vLLM 0.33.0 defaults, in that
// order of precedence. Unsupported invocation forms, unknown flags, unmodelled
// vLLM flags, and unresolvable values are rejections (Rejections); every problem
// that can be established safely is reported.
func Resolve(in Invocation) (*Config, error) {
	cfg := newConfig()

	entrypoint, tokens, rej := detectEntrypoint(in)
	if rej != nil {
		return nil, Rejections{*rej}
	}
	cfg.Entrypoint = entrypoint

	p := newParser(in, cfg)

	// Environment variables are the lowest-precedence source the workload
	// controls, so they are applied before the file and command-line layers.
	p.applyEnvironment()

	normalized := normalizeTokens(tokens)
	configPath, hasConfig, configRej := resolveConfigPathArg(normalized)
	switch {
	case configRej != nil:
		p.errs = append(p.errs, *configRej)
	case hasConfig:
		// Provenance: the invocation is driven by a config file, which is useful
		// in diagnostics even though the file itself configures no setting.
		cfg.setOrigin(SettingConfigFile, OriginCLI)
		p.applyConfigFileLayer(configPath)
	}

	// The --config flag itself configures nothing in the effective configuration;
	// the values it pulls in are applied above with config-file precedence.
	p.applyCLI(withoutConfigFlag(normalized))
	p.resolvePositionalModel()

	p.errs = append(p.errs, validateFlags(cfg)...)
	// A command line that stopped early has an unknown remainder, so "no model"
	// would be a guess rather than a finding.
	if !p.aborted {
		if rej := requireModel(cfg); rej != nil {
			p.errs = append(p.errs, *rej)
		}
	}
	if len(p.errs) > 0 {
		return nil, p.errs
	}
	return cfg, nil
}

// applyConfigFileLayer resolves the file named by --config and applies its
// values with config-file precedence.
func (p *parser) applyConfigFileLayer(rawPath string) {
	containerPath, rej := p.resolvePath(rawPath, SettingConfigFile)
	if rej != nil {
		p.errs = append(p.errs, *rej)
		return
	}
	content, rej := p.files.read(containerPath, SettingConfigFile)
	if rej != nil {
		p.errs = append(p.errs, *rej)
		return
	}
	tokens, rej := p.encodeConfigFile(content)
	if rej != nil {
		p.errs = append(p.errs, *rej)
		return
	}
	p.applyConfigFile(tokens)
}

// requireModel rejects invocations that name no model.
func requireModel(cfg *Config) *Rejection {
	if cfg.Model.Name != nil && strings.TrimSpace(*cfg.Model.Name) != "" {
		return nil
	}
	return &Rejection{
		Setting: string(SettingModel),
		Reason:  "the invocation does not specify a model, so no profile can be selected; pass it as `vllm serve <model>` or with --model",
	}
}

// normalizeTokens rewrites the argument spellings vLLM 0.33.0 accepts into the
// canonical long flags this package models: "-O3" and "-O 3" become
// "--optimization-level", and short flags are expanded.
func normalizeTokens(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		switch {
		case token == "--":
			out = append(out, tokens[i:]...)
			return out
		case strings.HasPrefix(token, "-O") && token != "-O":
			level := token[2:]
			if strings.HasPrefix(level, "=") {
				level = level[1:]
			}
			out = append(out, "--optimization-level", level)
		case token == "-O":
			// A trailing "-O" has no level; parsing reports the missing value.
			out = append(out, "--optimization-level", "")
		default:
			out = append(out, token)
		}
	}
	return out
}
