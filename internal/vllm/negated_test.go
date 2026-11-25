package vllm_test

import (
	"testing"

	"github.com/neuralmagic/vllm-vcr-operator/internal/vllm"
)

// boolSettingSpellings is one modelled vLLM 0.33.0 boolean setting together with
// every spelling of the release's command line that expresses an explicit false.
//
// The positive spelling is derived from the setting name; a setting declared as
// "--enable-x" also keeps the release's "--disable-x" alias. The table is the
// public surface of the resolution: a spelling listed here must resolve to an
// explicit false that the settings policy can see.
var boolSettingSpellings = []struct {
	setting   vllm.Setting
	negative  []string
	effective func(*vllm.Config) *bool
}{
	{vllm.SettingTrustRemoteCode, []string{"--no-trust-remote-code"},
		func(c *vllm.Config) *bool { return c.Model.TrustRemoteCode }},
	{vllm.SettingSkipTokenizerInit, []string{"--no-skip-tokenizer-init"},
		func(c *vllm.Config) *bool { return c.Tokenizer.SkipInit }},
	{vllm.SettingHeadless, []string{"--no-headless"},
		func(c *vllm.Config) *bool { return c.Server.Headless }},
	{vllm.SettingDisableAccessLog, []string{"--no-disable-uvicorn-access-log"},
		func(c *vllm.Config) *bool { return c.Server.DisableUvicornAccessLog }},
	{vllm.SettingAutoToolChoice, []string{"--no-enable-auto-tool-choice", "--disable-auto-tool-choice"},
		func(c *vllm.Config) *bool { return c.Frontend.EnableAutoToolChoice }},
	{vllm.SettingPromptTokensDetails, []string{"--no-enable-prompt-tokens-details", "--disable-prompt-tokens-details"},
		func(c *vllm.Config) *bool { return c.Frontend.EnablePromptTokensDetails }},
	{vllm.SettingTokenIDsInUsage, []string{"--no-return-tokens-as-token-ids"},
		func(c *vllm.Config) *bool { return c.Frontend.ReturnTokensAsTokenIDs }},
	{vllm.SettingFP64Gumbel, []string{"--no-use-fp64-gumbel"},
		func(c *vllm.Config) *bool { return c.Generation.UseFP64Gumbel }},
	{vllm.SettingSamplingMask, []string{"--no-return-sampling-mask"},
		func(c *vllm.Config) *bool { return c.Generation.ReturnSamplingMask }},
	{vllm.SettingEnforceEager, []string{"--no-enforce-eager"},
		func(c *vllm.Config) *bool { return c.Execution.EnforceEager }},
	{vllm.SettingExpertParallel, []string{"--no-enable-expert-parallel", "--disable-expert-parallel"},
		func(c *vllm.Config) *bool { return c.Parallel.ExpertParallel }},
	{vllm.SettingWantDeprecatedQuant, []string{"--no-allow-deprecated-quantization"},
		func(c *vllm.Config) *bool { return c.Precision.AllowDeprecatedQuantization }},
	{vllm.SettingAsyncScheduling, []string{"--no-async-scheduling"},
		func(c *vllm.Config) *bool { return c.Scheduling.AsyncScheduling }},
	{vllm.SettingChunkedPrefill, []string{"--no-enable-chunked-prefill", "--disable-chunked-prefill"},
		func(c *vllm.Config) *bool { return c.Scheduling.ChunkedPrefill }},
	{vllm.SettingPrefixCaching, []string{"--no-enable-prefix-caching", "--disable-prefix-caching"},
		func(c *vllm.Config) *bool { return c.Cache.EnablePrefixCaching }},
	{vllm.SettingDisableLogStats, []string{"--no-disable-log-stats"},
		func(c *vllm.Config) *bool { return c.Observability.DisableLogStats }},
	{vllm.SettingLogRequests, []string{"--no-enable-log-requests", "--disable-log-requests"},
		func(c *vllm.Config) *bool { return c.Observability.EnableLogRequests }},
	{vllm.SettingNUMABind, []string{"--no-numa-bind"},
		func(c *vllm.Config) *bool { return c.Device.NUMABind }},
	{vllm.SettingSleepMode, []string{"--no-enable-sleep-mode", "--disable-sleep-mode"},
		func(c *vllm.Config) *bool { return c.Device.EnableSleepMode }},
	{vllm.SettingLoRAEnabled, []string{"--no-enable-lora", "--disable-lora"},
		func(c *vllm.Config) *bool { return c.LoRA.Enabled }},
	{vllm.SettingFullyShardedLoRAs, []string{"--no-fully-sharded-loras"},
		func(c *vllm.Config) *bool { return c.LoRA.FullyShardedLoRAs }},
	{vllm.SettingLanguageModelOnly, []string{"--no-language-model-only"},
		func(c *vllm.Config) *bool { return c.Multimodal.LanguageModelOnly }},
	{vllm.SettingMMEmbeds, []string{"--no-enable-mm-embeds", "--disable-mm-embeds"},
		func(c *vllm.Config) *bool { return c.Multimodal.EnableMMEmbeds }},
	{vllm.SettingInterleaveMM, []string{"--no-interleave-mm-strings"},
		func(c *vllm.Config) *bool { return c.Multimodal.InterleaveMMStrings }},
}

// Every negated boolean spelling of the release's command line resolves to an
// explicit false, attributed to the canonical setting name, so that the settings
// policy sees a workload request ("disabled") rather than an unknown flag.
func TestResolveNegatedBooleanSpellings(t *testing.T) {
	for _, tc := range boolSettingSpellings {
		t.Run(string(tc.setting), func(t *testing.T) {
			cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
				[]string{"vllm", "serve", "Qwen/Qwen3-8B"}, string(tc.setting))})
			wantBool(t, "positive spelling", tc.effective(cfg), true)
			wantOrigin(t, cfg, tc.setting, vllm.OriginCLI)

			for _, negative := range tc.negative {
				t.Run(negative, func(t *testing.T) {
					cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
						[]string{"vllm", "serve", "Qwen/Qwen3-8B"}, negative)})
					wantBool(t, "negated spelling", tc.effective(cfg), false)
					if !cfg.IsExplicit(tc.setting) {
						t.Fatalf("%s is not reported as set by the workload", tc.setting)
					}
					wantOrigin(t, cfg, tc.setting, vllm.OriginCLI)
				})
			}
		})
	}
}

// The unset/explicit distinction survives negation: a workload that says nothing
// about a boolean setting leaves it unset (a resolved vLLM default), and only an
// explicit spelling — positive or negated — makes it a workload request.
func TestResolveNegatedSpellingKeepsUnsetDistinct(t *testing.T) {
	cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve", "Qwen/Qwen3-8B"})})
	if cfg.IsExplicit(vllm.SettingPrefixCaching) || cfg.IsExplicit(vllm.SettingChunkedPrefill) {
		t.Fatal("an invocation that names no cache setting reports one as set")
	}
	wantOrigin(t, cfg, vllm.SettingPrefixCaching, vllm.OriginDefault)
	wantOrigin(t, cfg, vllm.SettingChunkedPrefill, vllm.OriginDefault)

	negated := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve", "Qwen/Qwen3-8B"}, "--no-enable-prefix-caching")})
	if !negated.IsExplicit(vllm.SettingPrefixCaching) {
		t.Fatal("--no-enable-prefix-caching is not reported as set by the workload")
	}
	// The spelling is negated but the setting it configures is unchanged, so a
	// setting the workload did not mention keeps its default.
	if negated.IsExplicit(vllm.SettingChunkedPrefill) {
		t.Fatal("negating prefix caching also reported chunked prefill as set")
	}
}

// A command line may name both spellings of one boolean setting. vLLM 0.33.0
// registers the positive and the negated spelling on one argparse option, so the
// last occurrence wins; the resolver reports the same answer instead of rejecting
// the repetition.
func TestResolveLastBooleanSpellingWins(t *testing.T) {
	tests := []struct {
		name string
		args []string
		get  func(*vllm.Config) *bool
		want bool
	}{
		{"positive then negated",
			[]string{"--enable-prefix-caching", "--no-enable-prefix-caching"},
			func(c *vllm.Config) *bool { return c.Cache.EnablePrefixCaching }, false},
		{"negated then positive",
			[]string{"--no-enable-prefix-caching", "--enable-prefix-caching"},
			func(c *vllm.Config) *bool { return c.Cache.EnablePrefixCaching }, true},
		{"two negated spellings of one setting",
			[]string{"--disable-prefix-caching", "--no-enable-prefix-caching"},
			func(c *vllm.Config) *bool { return c.Cache.EnablePrefixCaching }, false},
		{"three spellings alternate",
			[]string{"--no-enable-chunked-prefill", "--enable-chunked-prefill", "--no-enable-chunked-prefill"},
			func(c *vllm.Config) *bool { return c.Scheduling.ChunkedPrefill }, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"vllm", "serve", "Qwen/Qwen3-8B"}, tt.args...)
			cfg := resolve(t, vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0", args)})
			wantBool(t, "resolved value", tt.get(cfg), tt.want)
		})
	}
}

// The command line stays above the config file for negated spellings in both
// directions: a CLI negation overrides a config file that enables the setting,
// and a CLI positive spelling overrides a config file that disables it.
func TestResolveNegatedSpellingPrecedence(t *testing.T) {
	const disabled = "model: Qwen/Qwen3-8B\nenable-prefix-caching: false\n"
	const enabled = "model: Qwen/Qwen3-8B\nenable-prefix-caching: true\n"

	tests := []struct {
		name       string
		content    string
		args       []string
		want       bool
		wantOrigin vllm.Origin
	}{
		{"cli negation overrides an enabling file", enabled, []string{"--no-enable-prefix-caching"}, false, vllm.OriginCLI},
		{"cli positive spelling overrides a disabling file", disabled, []string{"--enable-prefix-caching"}, true, vllm.OriginCLI},
		{"a disabling file alone stays explicit", disabled, nil, false, vllm.OriginConfigFile},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := configMapVolume("/etc/vllm", "serve.yaml", []byte(tt.content))
			inv.Container.Args = append([]string{"--config=/etc/vllm/serve.yaml"}, tt.args...)
			cfg := resolve(t, inv)
			wantBool(t, "prefix caching", cfg.Cache.EnablePrefixCaching, tt.want)
			wantOrigin(t, cfg, vllm.SettingPrefixCaching, tt.wantOrigin)
		})
	}
}

// Negation is not a general prefix: only a modelled boolean setting has a negated
// spelling, so "--no-<unknown>" and a negation of a value-taking setting stay
// rejections that name the spelling.
func TestResolveRejectsUnknownNegatedSpellings(t *testing.T) {
	for _, spelling := range []string{
		"--no-such-setting",
		"--no-enable-imaginary-feature",
		"--no-max-num-seqs",
		"--no-enable-prefix-caching=true",
	} {
		t.Run(spelling, func(t *testing.T) {
			_, err := vllm.Resolve(vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
				[]string{"vllm", "serve", "Qwen/Qwen3-8B"}, spelling)})
			if err == nil {
				t.Fatalf("Resolve() accepted %s", spelling)
			}
			rs, ok := vllm.Rejected(err)
			if !ok {
				t.Fatalf("error is not a structured rejection: %v", err)
			}
			if got := string(rs[0].Setting); got == "" {
				t.Fatalf("rejection %+v names no setting", rs[0])
			}
		})
	}
}

// A config file expresses "false" through the setting's negated spelling, so a key
// whose negation vLLM 0.33.0 cannot express is a rejection rather than a value the
// resolver silently drops from the effective configuration.
func TestResolveConfigFileRejectsUnexpressibleFalse(t *testing.T) {
	for _, key := range []string{"not-a-vllm-setting", "port", "max-num-seqs"} {
		t.Run(key, func(t *testing.T) {
			_, err := vllm.Resolve(configFileInvocation("model: Qwen/Qwen3-8B\n" + key + ": false\n"))
			requireRejection(t, err, vllm.SettingConfigFile, "negated spelling")
		})
	}
}

// A config-file false for a flag the release registers but this operator does not
// model keeps the "does not model" rejection, so the workload author is told to
// wait for support rather than to remove the key.
func TestResolveConfigFileRejectsUnmodelledFalse(t *testing.T) {
	_, err := vllm.Resolve(configFileInvocation("model: Qwen/Qwen3-8B\nenable-log-deltas: false\n"))
	requireRejection(t, err, vllm.Setting("--disable-log-deltas"), "does not model")
}
