package vllm

import (
	"fmt"
	"strings"
)

// resolveChatTemplate resolves "--chat-template", which vLLM accepts either as a
// template string or as a path to a template file.
//
// The two forms are told apart the way vLLM does: a value that names a file (it
// looks like a path and is not template text) is a path. A path is only
// resolvable when the file content comes from the Pod spec; otherwise the
// template cannot be rendered and the invocation is rejected.
func resolveChatTemplate(p *parser, raw string) *Rejection {
	if raw == "" {
		return &Rejection{Setting: SettingChatTemplate, Value: raw, Reason: "empty chat template"}
	}
	if !looksLikeTemplatePath(raw) {
		cfg := p.cfg
		cfg.ChatTemplate.Template = raw
		cfg.ChatTemplate.Inline = true
		cfg.ChatTemplate.Path = ""
		cfg.ChatTemplate.SHA256 = digestOf(raw)
		return nil
	}
	containerPath, rej := p.resolvePath(raw)
	if rej != nil {
		rej.Setting = SettingChatTemplate
		return rej
	}
	content, rej := p.files.read(containerPath, SettingChatTemplate)
	if rej != nil {
		return rej
	}
	p.cfg.ChatTemplate.Template = string(content)
	p.cfg.ChatTemplate.Path = containerPath
	p.cfg.ChatTemplate.Inline = false
	p.cfg.ChatTemplate.SHA256 = digestOf(string(content))
	return nil
}

// looksLikeTemplatePath reports whether a --chat-template value is a file path
// rather than inline template text.
func looksLikeTemplatePath(raw string) bool {
	if strings.Contains(raw, "{{") || strings.ContainsAny(raw, "\n<>") {
		return false
	}
	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "./") {
		return true
	}
	for _, ext := range []string{".jinja", ".jinja2", ".j2", ".template", ".json"} {
		if strings.HasSuffix(raw, ext) {
			return true
		}
	}
	return false
}

// resolveAPIKey records that the server requires an API key without retaining
// the key. vLLM accepts the flag repeatedly; every occurrence means "one of
// these keys".
func resolveAPIKey(p *parser, raw string) *Rejection {
	if raw == "" {
		return &Rejection{Setting: SettingAPIKey, Value: RedactedValue, Reason: "empty API key"}
	}
	p.cfg.Auth.APIKeyRequired = true
	p.cfg.Auth.APIKeySource = string(SettingAPIKey)
	return nil
}

// resolveMaxModelLen resolves "--max-model-len", which accepts human-readable
// sizes and the "-1"/"auto" auto-detection sentinel.
func resolveMaxModelLen(p *parser, raw string) *Rejection {
	v, err := humanReadableIntOrAuto(raw)
	if err != nil {
		return &Rejection{Setting: SettingMaxModelLen, Value: raw, Reason: "not an integer or \"auto\""}
	}
	if v < -1 || v == 0 {
		return &Rejection{Setting: SettingMaxModelLen, Value: raw, Reason: "must be positive, or -1/\"auto\" to use the model maximum"}
	}
	p.cfg.Limits.MaxModelLen = &v
	return nil
}

// resolveKVEventsConfig decodes "--kv-events-config" into its typed settings and
// keeps the raw object for profile compatibility checks.
func resolveKVEventsConfig(p *parser, raw string) *Rejection {
	m, err := decodeJSONObject(raw)
	if err != nil {
		return &Rejection{Setting: SettingKVEventsConfig, Value: raw, Reason: err.Error()}
	}
	cfg := &KVEventsConfig{Raw: m}
	fail := func(err error) *Rejection {
		return &Rejection{Setting: SettingKVEventsConfig, Value: raw, Reason: err.Error()}
	}
	if cfg.Enable, err = m.jsonBool("enable_kv_cache_events"); err != nil {
		return fail(err)
	}
	if cfg.Publisher, err = m.jsonString("publisher"); err != nil {
		return fail(err)
	}
	if cfg.Endpoint, err = m.jsonString("endpoint"); err != nil {
		return fail(err)
	}
	if cfg.ReplayEndpoint, err = m.jsonString("replay_endpoint"); err != nil {
		return fail(err)
	}
	if cfg.Topic, err = m.jsonString("topic"); err != nil {
		return fail(err)
	}
	if cfg.BufferSteps, err = m.jsonInt("buffer_steps"); err != nil {
		return fail(err)
	}
	if cfg.HWM, err = m.jsonInt("hwm"); err != nil {
		return fail(err)
	}
	if cfg.MaxQueueSize, err = m.jsonInt("max_queue_size"); err != nil {
		return fail(err)
	}
	if cfg.Publisher != nil && *cfg.Publisher != "zmq" && *cfg.Publisher != "null" {
		return &Rejection{
			Setting: SettingKVEventsConfig,
			Value:   *cfg.Publisher,
			Reason:  `publisher must be "zmq" or "null"`,
		}
	}
	p.cfg.KVEvents = cfg
	return nil
}

// resolveKVTransferConfig decodes "--kv-transfer-config", which carries the P/D
// role and KV connector configuration.
func resolveKVTransferConfig(p *parser, raw string) *Rejection {
	m, err := decodeJSONObject(raw)
	if err != nil {
		return &Rejection{Setting: SettingKVTransferConfig, Value: raw, Reason: err.Error()}
	}
	cfg := &KVTransferConfig{Raw: m}
	fail := func(err error) *Rejection {
		return &Rejection{Setting: SettingKVTransferConfig, Value: raw, Reason: err.Error()}
	}
	if cfg.Connector, err = m.jsonString("kv_connector"); err != nil {
		return fail(err)
	}
	if cfg.Role, err = m.jsonString("kv_role"); err != nil {
		return fail(err)
	}
	if cfg.ConnectorModulePath, err = m.jsonString("kv_connector_module_path"); err != nil {
		return fail(err)
	}
	if cfg.KVPort, err = m.jsonInt("kv_port"); err != nil {
		return fail(err)
	}
	if cfg.ExtraConfig, err = m.jsonObject("kv_connector_extra_config"); err != nil {
		return fail(err)
	}
	if cfg.Role != nil {
		switch *cfg.Role {
		case "kv_producer", "kv_consumer", "kv_both":
		default:
			return &Rejection{
				Setting: SettingKVTransferConfig,
				Value:   *cfg.Role,
				Reason:  `kv_role must be "kv_producer", "kv_consumer", or "kv_both"`,
			}
		}
	}
	p.cfg.KVTransfer = cfg
	return nil
}

// resolveSpeculativeConfig decodes "--speculative-config".
func resolveSpeculativeConfig(p *parser, raw string) *Rejection {
	m, err := decodeJSONObject(raw)
	if err != nil {
		return &Rejection{Setting: SettingSpeculativeConfig, Value: raw, Reason: err.Error()}
	}
	cfg := &SpeculativeConfig{Raw: m}
	fail := func(err error) *Rejection {
		return &Rejection{Setting: SettingSpeculativeConfig, Value: raw, Reason: err.Error()}
	}
	if cfg.Method, err = m.jsonString("method"); err != nil {
		return fail(err)
	}
	if cfg.Model, err = m.jsonString("model"); err != nil {
		return fail(err)
	}
	if cfg.NumSpeculativeTokens, err = m.jsonInt("num_speculative_tokens"); err != nil {
		return fail(err)
	}
	if cfg.DraftTensorParallelSize, err = m.jsonInt("draft_tensor_parallel_size"); err != nil {
		return fail(err)
	}
	if cfg.NumSpeculativeTokens != nil && *cfg.NumSpeculativeTokens < 1 {
		return &Rejection{
			Setting: SettingSpeculativeConfig,
			Value:   fmt.Sprint(*cfg.NumSpeculativeTokens),
			Reason:  "num_speculative_tokens must be positive",
		}
	}
	p.cfg.Speculative = cfg
	return nil
}

// resolveLimitMMPerPrompt decodes "--limit-mm-per-prompt", a JSON object mapping
// a modality name to the maximum number of items a request may carry.
func resolveLimitMMPerPrompt(p *parser, raw string) *Rejection {
	m, err := decodeJSONObject(raw)
	if err != nil {
		return &Rejection{Setting: SettingLimitMMPerPrompt, Value: raw, Reason: err.Error()}
	}
	limits := make(map[string]int, len(m))
	for modality, value := range m {
		number, ok := value.(float64)
		if !ok || number != float64(int(number)) || number < 0 {
			return &Rejection{
				Setting: SettingLimitMMPerPrompt,
				Value:   modality,
				Reason:  "the per-modality limit must be a non-negative integer",
			}
		}
		limits[modality] = int(number)
	}
	p.cfg.Multimodal.LimitMMPerPrompt = limits
	return nil
}

// resolveStructuredOutputs decodes "--structured-outputs-config".
func resolveStructuredOutputs(p *parser, raw string) *Rejection {
	m, err := decodeJSONObject(raw)
	if err != nil {
		return &Rejection{Setting: SettingStructuredOutputs, Value: raw, Reason: err.Error()}
	}
	cfg := &StructuredOutputsConfig{Raw: m}
	fail := func(err error) *Rejection {
		return &Rejection{Setting: SettingStructuredOutputs, Value: raw, Reason: err.Error()}
	}
	if cfg.Backend, err = m.jsonString("backend"); err != nil {
		return fail(err)
	}
	if cfg.DisableAnyWhitespace, err = m.jsonBool("disable_any_whitespace"); err != nil {
		return fail(err)
	}
	if cfg.DisableAdditionalProperties, err = m.jsonBool("disable_additional_properties"); err != nil {
		return fail(err)
	}
	if cfg.ReasoningParser, err = m.jsonString("reasoning_parser"); err != nil {
		return fail(err)
	}
	if cfg.ReasoningParserPlugin, err = m.jsonString("reasoning_parser_plugin"); err != nil {
		return fail(err)
	}
	if cfg.EnableInReasoning, err = m.jsonBool("enable_in_reasoning"); err != nil {
		return fail(err)
	}
	p.cfg.StructuredOutputs = cfg
	return nil
}
