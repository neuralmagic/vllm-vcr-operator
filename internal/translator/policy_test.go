package translator_test

import (
	"strings"
	"testing"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/translator"
)

// TestPolicyTableIsCompleteAndWellFormed asserts the settings policy is data with a
// reviewable row per setting: no duplicate rows, no row without a disposition, and
// no accepted row that silently drops a setting.
func TestPolicyTableIsCompleteAndWellFormed(t *testing.T) {
	rows := translator.Policy()
	if len(rows) == 0 {
		t.Fatal("the settings policy table is empty")
	}

	dispositions := map[translator.Disposition]bool{
		translator.DispositionPreserve:          true,
		translator.DispositionTranslate:         true,
		translator.DispositionRequireProfile:    true,
		translator.DispositionRequireCapability: true,
		translator.DispositionReject:            true,
	}

	seen := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.Setting == "" {
			t.Fatalf("policy row %+v has no setting", row)
		}
		if row.Family == "" {
			t.Errorf("policy row for %q has no settings family", row.Setting)
		}
		if !dispositions[row.Disposition] {
			t.Errorf("policy row for %q has the unknown disposition %q", row.Setting, row.Disposition)
		}
		if prior, duplicate := seen[string(row.Setting)]; duplicate {
			t.Errorf("policy row for %q duplicates the disposition %q", row.Setting, prior)
		}
		seen[string(row.Setting)] = string(row.Disposition)

		switch row.Disposition {
		case translator.DispositionReject:
			if strings.TrimSpace(row.Reason) == "" {
				t.Errorf("rejecting row for %q does not explain why", row.Setting)
			}
		case translator.DispositionRequireCapability:
			if row.Capability == "" {
				t.Errorf("capability row for %q names no capability", row.Setting)
			}
		case translator.DispositionPreserve:
			// A preserved setting is either re-emitted on the command line or is
			// forwarded some other way, which the row must document.
			if row.Frontend == nil && strings.TrimSpace(row.Reason) == "" {
				t.Errorf("preserving row for %q neither emits nor documents the setting", row.Setting)
			}
		case translator.DispositionTranslate:
			if row.Frontend == nil && strings.TrimSpace(row.Reason) == "" && strings.TrimSpace(row.Warning) == "" {
				t.Errorf("translating row for %q does not say where the setting goes", row.Setting)
			}
		}
		if row.Frontend != nil && (row.Disposition == translator.DispositionReject) {
			t.Errorf("rejecting row for %q also emits frontend arguments", row.Setting)
		}
	}
}

// TestPolicyCoversEverySettingsFamily asserts the policy table covers the
// specification's settings-policy families. The two families without rows are
// documented exceptions: the GPU quantity comes from the Pod spec, and unmodelled
// environment variables are rejected by name rather than by row.
func TestPolicyCoversEverySettingsFamily(t *testing.T) {
	documentedExceptions := map[string]string{
		"GPU resource count":                  "the device quantity comes from the Pod spec",
		"KV replay":                           "replay is a field of --kv-events-config, not a separate setting",
		"Unknown behavior-affecting settings": "unmodelled variables have no fixed name",
	}
	wantFamilies := []string{
		"Model identity/revision",
		"Tokenizer identity/revision and chat template",
		"Served-model aliases",
		"Host, inference port, API authentication, TLS",
		"Tool/reasoning parsers and frontend formatting",
		"Generation defaults and sampling",
		"Tensor parallelism, precision, quantization",
		"Pipeline parallelism, distributed workers, remote engine addresses",
		"GPU resource count",
		"GPU memory utilization, KV-memory allocation, cache-block overrides",
		"Maximum model length",
		"Maximum running sequences",
		"Per-step token budget",
		"Scheduling policy",
		"Chunked prefill",
		"KV block size",
		"Prefix caching",
		"KV live events",
		"KV replay",
		"Shutdown timeout",
		"Logging and telemetry",
		"GPU device/CUDA runtime selection",
		"Eager execution, CUDA graphs, compilation",
		"LoRA, speculative decoding, multimodal generation, embeddings/pooling, structured-output constraints",
		"P/D roles and KV-transfer settings",
		"Unknown behavior-affecting settings",
	}

	covered := make(map[string]int, len(wantFamilies))
	for _, row := range translator.Policy() {
		covered[row.Family]++
	}
	for _, family := range wantFamilies {
		if covered[family] > 0 {
			continue
		}
		if reason, exempt := documentedExceptions[family]; exempt {
			t.Logf("family %q has no policy row: %s", family, reason)
			continue
		}
		t.Errorf("settings family %q has no policy row", family)
	}
}

// TestPolicyReturnsACopy asserts callers cannot mutate the applied policy.
func TestPolicyReturnsACopy(t *testing.T) {
	rows := translator.Policy()
	if len(rows) == 0 {
		t.Fatal("policy table is empty")
	}
	rows[0].Setting = "--mutated"

	if got := translator.Policy()[0].Setting; got == "--mutated" {
		t.Fatal("Policy() returns the applied table, so callers can rewrite the policy")
	}
}
