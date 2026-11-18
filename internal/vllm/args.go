package vllm

import (
	"fmt"
	"path"
	"strings"
)

// parser resolves one layer of configuration into a Config.
//
// Layers are applied in increasing precedence: vLLM defaults, environment
// variables, the config file named by --config, then command-line arguments.
type parser struct {
	inv    Invocation
	cfg    *Config
	origin Origin
	files  *fileResolver

	// specs maps every accepted flag spelling to the modelled setting.
	specs map[string]flagSpellings

	// errs collects independent problems found while applying the layer.
	errs Rejections
	// positionals are the non-flag tokens of the command line.
	positionals []string
	// aborted reports that a layer stopped parsing with tokens left over, so the
	// remainder of that layer is unknown.
	aborted bool
}

// newParser builds a parser for cfg. The flag table is derived from cfg so that
// each spec writes directly into the Config being resolved.
func newParser(inv Invocation, cfg *Config) *parser {
	p := &parser{
		inv:   inv,
		cfg:   cfg,
		files: newFileResolver(inv),
		specs: make(map[string]flagSpellings),
	}
	for _, spec := range flags(cfg) {
		specCopy := spec
		p.specs[string(spec.setting)] = flagSpellings{setting: spec.setting, spec: &specCopy, boolValue: true}
		if spec.kind != kindBool {
			continue
		}
		// BooleanOptionalAction spellings exist only where vLLM 0.33.0 registers
		// them; knownFlags is the complete list of spellings for the release.
		for _, negated := range negatedSpellings(string(spec.setting)) {
			if !knownFlag(negated) {
				continue
			}
			p.specs[negated] = flagSpellings{setting: spec.setting, spec: &specCopy, boolValue: false}
		}
	}
	return p
}

// applyCLI applies command-line arguments, excluding the tokens of the
// entrypoint itself.
func (p *parser) applyCLI(tokens []string) {
	p.origin = OriginCLI
	p.applyTokens(tokens)
}

// applyConfigFile applies the arguments from the config file named by --config.
func (p *parser) applyConfigFile(tokens []string) {
	p.origin = OriginConfigFile
	p.applyTokens(tokens)
}

// applyTokens parses one layer of arguments.
func (p *parser) applyTokens(tokens []string) {
	tokens, ok := p.expandDottedJSON(tokens)
	if !ok {
		// The dotted-JSON pass established an unresolvable argument; the arity of
		// the remaining tokens is unknown, so parsing stops here.
		p.aborted = true
		return
	}
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if token == "--" {
			p.positionals = append(p.positionals, tokens[i+1:]...)
			return
		}
		if !strings.HasPrefix(token, "-") || token == "-" {
			p.positionals = append(p.positionals, token)
			continue
		}
		name, inlineValue, hasInline := splitFlagToken(token)
		if normalized, ok := shortFlags[name]; ok {
			name = normalized
		}
		name = normalizeFlagName(name)
		if name == string(SettingConfigFile) {
			p.errs = append(p.errs, Rejection{
				Setting: SettingConfigFile,
				Value:   inlineValue,
				Reason:  "a config file cannot itself name another config file",
			})
			return
		}
		spelling, ok := p.specs[name]
		if !ok {
			// The arity of an unrecognised flag is unknown, so continuing could
			// mistake a value for a flag. Stop this layer and report why.
			p.errs = append(p.errs, unmodelledFlagRejection(name, inlineValue))
			p.aborted = true
			return
		}
		spec := spelling.spec
		if spec.kind == kindBool {
			if hasInline {
				p.errs = append(p.errs, Rejection{
					Setting: spelling.setting,
					Value:   inlineValue,
					Reason:  "boolean flags take no value in vLLM 0.33.0",
				})
				p.aborted = true
				return
			}
			if err := p.setBool(spec, spelling.boolValue); err != nil {
				p.errs = append(p.errs, *err)
			}
			continue
		}
		value := inlineValue
		if !hasInline {
			if i+1 >= len(tokens) {
				p.errs = append(p.errs, Rejection{
					Setting: spelling.setting,
					Reason:  "the flag requires a value",
				})
				p.aborted = true
				return
			}
			i++
			value = tokens[i]
		}
		if err := spec.assign(p, value); err != nil {
			p.errs = append(p.errs, *err)
			continue
		}
		// Flags registered with nargs="+" consume the following values until the
		// next flag, which is how --served-model-name takes several aliases.
		if spec.kind == kindStringList && !hasInline {
			for i+1 < len(tokens) && !isFlagToken(tokens[i+1]) && tokens[i+1] != "--" {
				i++
				if err := spec.assign(p, tokens[i]); err != nil {
					p.errs = append(p.errs, *err)
					break
				}
			}
		}
		p.record(spelling.setting)
	}
}

// setBool applies a value-less boolean flag.
func (p *parser) setBool(spec *flagSpec, value bool) *Rejection {
	v := value
	*spec.bptr = &v
	p.record(spec.setting)
	return nil
}

// record stores the origin of a setting that was successfully resolved.
func (p *parser) record(s Setting) {
	p.cfg.setOrigin(s, p.origin)
}

// resolvePositionalModel resolves the "vllm serve MODEL" positional argument.
//
// vLLM 0.33.0 moves a --model value to the positional slot and lets the
// positional value win, so the positional model overrides --model, the config
// file, and the environment.
func (p *parser) resolvePositionalModel() {
	if len(p.positionals) == 0 {
		return
	}
	if len(p.positionals) > 1 {
		p.errs = append(p.errs, Rejection{
			Setting: SettingModelTag,
			Value:   strings.Join(p.positionals, " "),
			Reason:  "unexpected positional arguments; vllm serve takes exactly one model argument",
		})
		return
	}
	model := p.positionals[0]
	p.cfg.Model.Name = &model
	p.cfg.setOrigin(SettingModel, OriginCLI)
}

// isFlagToken reports whether a token is a flag rather than a value.
func isFlagToken(token string) bool {
	return token != "-" && len(token) > 1 && strings.HasPrefix(token, "-")
}

// splitFlagToken splits "--flag=value" into its parts.
func splitFlagToken(token string) (name, value string, hasValue bool) {
	if i := strings.Index(token, "="); i >= 0 {
		return token[:i], token[i+1:], true
	}
	return token, "", false
}

// normalizeFlagName converts the flag part of an argument to its canonical long
// form: the segment between "--" and the first "." has underscores replaced by
// dashes, which is how vLLM 0.33.0 normalises its own arguments.
func normalizeFlagName(name string) string {
	if !strings.HasPrefix(name, "--") {
		return name
	}
	head, rest, hasRest := strings.Cut(name, ".")
	head = strings.ReplaceAll(head, "_", "-")
	if !hasRest {
		return head
	}
	return head + "." + rest
}

// expandDottedJSON folds "--flag.key value" arguments into the JSON object of
// their flag, the way vLLM 0.33.0 supports JSON CLI arguments.
//
// It returns ok=false when it established a rejection it cannot recover from.
func (p *parser) expandDottedJSON(tokens []string) ([]string, bool) {
	if !hasDotted(tokens) {
		return tokens, true
	}
	type merged struct {
		flag  string
		value JSONMap
	}
	var (
		order  []*merged
		byFlag = map[string]*merged{}
		out    = make([]string, 0, len(tokens))
	)
	fail := func(r Rejection) ([]string, bool) {
		p.errs = append(p.errs, r)
		return nil, false
	}
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if !strings.HasPrefix(token, "--") || !strings.Contains(token, ".") {
			out = append(out, token)
			continue
		}
		name, inlineValue, hasInline := splitFlagToken(token)
		if normalized, ok := shortFlags[name]; ok {
			name = normalized
		}
		name = normalizeFlagName(name)
		flagName, keyPath, _ := strings.Cut(name, ".")
		if keyPath == "" {
			return fail(Rejection{Setting: Setting(flagName), Value: token, Reason: "empty JSON key in a JSON CLI argument"})
		}
		spelling, ok := p.specs[flagName]
		if !ok {
			return fail(unmodelledFlagRejection(flagName, ""))
		}
		if !spelling.spec.takesJSONObject() {
			return fail(Rejection{
				Setting: spelling.setting,
				Value:   keyPath,
				Reason:  "this flag is not a JSON object, so it has no sub-keys",
			})
		}
		value := inlineValue
		if !hasInline {
			if i+1 >= len(tokens) {
				return fail(Rejection{Setting: spelling.setting, Reason: "the JSON key requires a value"})
			}
			i++
			value = tokens[i]
		}
		appendList := strings.HasSuffix(keyPath, "+")
		keyPath = strings.TrimSuffix(keyPath, "+")
		pathKeys := strings.Split(keyPath, ".")
		var parsed any
		if appendList {
			items := strings.Split(value, ",")
			list := make([]any, 0, len(items))
			for _, item := range items {
				list = append(list, item)
			}
			parsed = list
		} else if v, ok := parseJSONScalar(value); ok {
			parsed = v
		} else {
			parsed = value
		}
		m, ok := byFlag[flagName]
		if !ok {
			m = &merged{flag: flagName, value: JSONMap{}}
			byFlag[flagName] = m
			order = append(order, m)
		}
		insertNested(m.value, pathKeys, parsed)
	}
	for _, m := range order {
		raw, err := encodeJSONObject(m.value)
		if err != nil {
			return fail(Rejection{Setting: Setting(m.flag), Value: "", Reason: "the JSON CLI arguments could not be encoded"})
		}
		out = append(out, m.flag, raw)
	}
	return out, true
}

// hasDotted reports whether any token is a dotted JSON argument.
func hasDotted(tokens []string) bool {
	for _, token := range tokens {
		if strings.HasPrefix(token, "--") {
			if name, _, _ := splitFlagToken(token); strings.Contains(name, ".") {
				return true
			}
		}
	}
	return false
}

// insertNested writes value at keys inside m.
func insertNested(m JSONMap, keys []string, value any) {
	if len(keys) == 1 {
		m[keys[0]] = value
		return
	}
	nested, ok := m[keys[0]].(JSONMap)
	if !ok {
		nested = JSONMap{}
		m[keys[0]] = nested
	}
	insertNested(nested, keys[1:], value)
}

// resolveConfigPathArg returns the path named by --config in tokens, if any.
func resolveConfigPathArg(tokens []string) (string, bool, *Rejection) {
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if token == "--" {
			return "", false, nil
		}
		name, inlineValue, hasInline := splitFlagToken(token)
		if name != "--config" {
			continue
		}
		if hasInline {
			return inlineValue, true, nil
		}
		if i+1 >= len(tokens) {
			return "", false, &Rejection{Setting: SettingConfigFile, Reason: "the flag requires a value"}
		}
		return tokens[i+1], true, nil
	}
	return "", false, nil
}

// withoutConfigFlag removes --config and its value from a token list.
func withoutConfigFlag(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		name, _, hasInline := splitFlagToken(tokens[i])
		if name != "--config" {
			out = append(out, tokens[i])
			continue
		}
		if !hasInline && i+1 < len(tokens) {
			i++
		}
	}
	return out
}

// resolvePath resolves a path argument against the container's working
// directory, which is the only way a relative path becomes absolute.
func (p *parser) resolvePath(raw string) (string, *Rejection) {
	if path.IsAbs(raw) {
		return raw, nil
	}
	if p.inv.Container.WorkingDir == "" {
		return "", &Rejection{
			Setting: "",
			Value:   raw,
			Reason:  "a relative path is used but the container sets no working directory, so the file cannot be located",
		}
	}
	return path.Join(p.inv.Container.WorkingDir, raw), nil
}

// unmodelledFlagRejection explains why a flag cannot be resolved, naming whether
// it exists in vLLM 0.33.0 at all.
func unmodelledFlagRejection(name, value string) Rejection {
	if value != "" && credentialishFlag(name) {
		value = RedactedValue
	}
	if knownFlag(name) {
		return Rejection{
			Setting: Setting(name),
			Value:   value,
			Reason:  "a vLLM 0.33.0 flag that this operator does not model, so its policy treatment is undefined",
		}
	}
	return Rejection{
		Setting: Setting(name),
		Value:   value,
		Reason:  "unknown flag; it is not part of the vLLM 0.33.0 command line",
	}
}

// credentialishFlag reports whether a flag spelling carries a credential, in
// which case its value must never appear in a rejection, log, or annotation.
// Every modelled credential flag is listed by name; the substring check keeps
// unrecognised credential-bearing spellings from leaking.
func credentialishFlag(name string) bool {
	switch name {
	case "--api-key", "--hf-token", "--tool-parser-plugin", "--reasoning-parser-plugin":
		return true
	}
	lower := strings.ToLower(name)
	for _, fragment := range []string{"token", "key", "secret", "password", "credential", "auth"} {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

// parseAndEncodeConfigFile converts config-file YAML into arguments, the way
// vLLM 0.33.0 loads them: keys are long flag names, objects become JSON, lists
// become repeated values, and booleans become the flag or its negated spelling.
func parseAndEncodeConfigFile(content []byte) ([]string, *Rejection) {
	values, err := decodeYAMLMapping(content)
	if err != nil {
		return nil, &Rejection{Setting: SettingConfigFile, Reason: err.Error()}
	}
	tokens := make([]string, 0, 2*len(values))
	for _, entry := range values {
		key := normalizeConfigKey(entry.key)
		switch value := entry.value.(type) {
		case bool:
			if value {
				tokens = append(tokens, "--"+key)
				continue
			}
			// vLLM only expresses "false" through a negated spelling, which
			// exists only for BooleanOptionalAction flags.
			for _, negated := range negatedSpellings("--" + key) {
				if knownFlag(negated) {
					tokens = append(tokens, negated)
					break
				}
			}
		case []any:
			tokens = append(tokens, "--"+key)
			for _, item := range value {
				s, err := scalarToken(item)
				if err != nil {
					return nil, &Rejection{Setting: SettingConfigFile, Value: key, Reason: err.Error()}
				}
				tokens = append(tokens, s)
			}
		case map[string]any:
			raw, err := encodeJSONObject(JSONMap(value))
			if err != nil {
				return nil, &Rejection{Setting: SettingConfigFile, Value: key, Reason: "value cannot be encoded as JSON"}
			}
			tokens = append(tokens, "--"+key, raw)
		case nil:
			return nil, &Rejection{Setting: SettingConfigFile, Value: key, Reason: "null values are not resolvable"}
		default:
			s, err := scalarToken(value)
			if err != nil {
				return nil, &Rejection{Setting: SettingConfigFile, Value: key, Reason: err.Error()}
			}
			tokens = append(tokens, "--"+key, s)
		}
	}
	return tokens, nil
}

// normalizeConfigKey converts a config-file key to the flag spelling vLLM
// accepts: underscores in the first segment become dashes.
func normalizeConfigKey(key string) string {
	head, rest, hasRest := strings.Cut(key, ".")
	head = strings.ReplaceAll(head, "_", "-")
	if !hasRest {
		return head
	}
	return head + "." + rest
}

// scalarToken renders a YAML scalar the way vLLM's config loader does.
func scalarToken(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case float64:
		return formatNumber(v), nil
	case int:
		return fmt.Sprint(v), nil
	case int64:
		return fmt.Sprint(v), nil
	case nil:
		return "", fmt.Errorf("null values are not resolvable")
	default:
		return "", fmt.Errorf("value of type %T is not resolvable", value)
	}
}
