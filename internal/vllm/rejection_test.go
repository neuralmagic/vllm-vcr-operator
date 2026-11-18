package vllm_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/vllm"
)

// Rejection is the default outcome for configuration the resolver cannot
// establish. A rejection names the setting, the offending value, and the reason.
func TestResolveRejectionShape(t *testing.T) {
	_, err := vllm.Resolve(vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve", "Qwen/Qwen3-8B", "--block-size=lots"})})

	r := requireRejection(t, err, vllm.SettingBlockSize, "not an integer")
	if r.Value != "lots" {
		t.Fatalf("rejection value = %q, want the offending value %q", r.Value, "lots")
	}
	if !strings.Contains(r.Error(), string(vllm.SettingBlockSize)) {
		t.Fatalf("rejection message %q does not name the setting", r.Error())
	}

	rejections, ok := vllm.Rejected(err)
	if !ok || len(rejections) != 1 {
		t.Fatalf("Rejected() = %v, %v; want the single rejection", rejections, ok)
	}
	var single vllm.Rejection
	if !errors.As(err, &single) {
		t.Fatal("the rejection set does not unwrap to individual rejections")
	}
}

func TestResolveRejectsUnknownAndUnmodelledFlags(t *testing.T) {
	tests := []struct {
		name    string
		arg     string
		setting vllm.Setting
		reason  string
	}{
		{
			name:    "misspelled flag",
			arg:     "--max-model-length=4096",
			setting: vllm.Setting("--max-model-length"),
			reason:  "unknown flag",
		},
		{
			name:    "not a vllm flag",
			arg:     "--gpu-count=2",
			setting: vllm.Setting("--gpu-count"),
			reason:  "unknown flag",
		},
		{
			name:    "vllm 0.33.0 flag outside the modelled settings",
			arg:     "--performance-mode=throughput",
			setting: vllm.Setting("--performance-mode"),
			reason:  "does not model",
		},
		{
			name:    "per-request decoding knobs outside the modelled settings",
			arg:     "--cudagraph-capture-sizes=1,2,4",
			setting: vllm.Setting("--cudagraph-capture-sizes"),
			reason:  "does not model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := vllm.Resolve(vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
				[]string{"vllm", "serve", "Qwen/Qwen3-8B"}, tt.arg)})
			requireRejection(t, err, tt.setting, tt.reason)
		})
	}
}

func TestResolveRejectsUnresolvableValues(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		setting vllm.Setting
		reason  string
	}{
		{
			name:    "tensor parallel size is not an integer",
			args:    []string{"Qwen/Qwen3-8B", "--tensor-parallel-size=two"},
			setting: vllm.SettingTensorParallelSize,
			reason:  "not an integer",
		},
		{
			name:    "pipeline parallel size below one",
			args:    []string{"Qwen/Qwen3-8B", "--pipeline-parallel-size=0"},
			setting: vllm.SettingPipelineParallel,
			reason:  "at least 1",
		},
		{
			name:    "port outside the TCP range",
			args:    []string{"Qwen/Qwen3-8B", "--port=70000"},
			setting: vllm.SettingPort,
			reason:  "TCP port",
		},
		{
			name:    "gpu memory utilization above one",
			args:    []string{"Qwen/Qwen3-8B", "--gpu-memory-utilization=1.5"},
			setting: vllm.SettingGPUMemUtilization,
			reason:  "must be in",
		},
		{
			name:    "unsupported scheduling policy",
			args:    []string{"Qwen/Qwen3-8B", "--scheduling-policy=random"},
			setting: vllm.SettingSchedulingPolicy,
			reason:  "fcfs",
		},
		{
			name:    "max model len is not a size",
			args:    []string{"Qwen/Qwen3-8B", "--max-model-len=very-long"},
			setting: vllm.SettingMaxModelLen,
			reason:  "not an integer",
		},
		{
			name:    "kv events config is not JSON",
			args:    []string{"Qwen/Qwen3-8B", "--kv-events-config=tcp://*:5556"},
			setting: vllm.SettingKVEventsConfig,
			reason:  "not a JSON object",
		},
		{
			name:    "kv events publisher is not a vllm publisher",
			args:    []string{"Qwen/Qwen3-8B", `--kv-events-config={"publisher":"kafka"}`},
			setting: vllm.SettingKVEventsConfig,
			reason:  "publisher must be",
		},
		{
			name:    "kv transfer role is not a vllm role",
			args:    []string{"Qwen/Qwen3-8B", `--kv-transfer-config={"kv_connector":"NixlConnector","kv_role":"router"}`},
			setting: vllm.SettingKVTransferConfig,
			reason:  "kv_role must be",
		},
		{
			name:    "boolean flag with a value",
			args:    []string{"Qwen/Qwen3-8B", "--enforce-eager=true"},
			setting: vllm.SettingEnforceEager,
			reason:  "take no value",
		},
		{
			name:    "flag without a value",
			args:    []string{"Qwen/Qwen3-8B", "--port"},
			setting: vllm.SettingPort,
			reason:  "requires a value",
		},
		{
			name:    "two positional arguments",
			args:    []string{"Qwen/Qwen3-8B", "Qwen/Qwen3-30B-A3B"},
			setting: vllm.SettingModelTag,
			reason:  "exactly one model",
		},
		{
			name:    "dotted key on a scalar flag",
			args:    []string{"Qwen/Qwen3-8B", "--port.number=8000"},
			setting: vllm.SettingPort,
			reason:  "not a JSON object",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := vllm.Resolve(vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
				[]string{"vllm", "serve"}, tt.args...)})
			requireRejection(t, err, tt.setting, tt.reason)
		})
	}
}

// Independent problems are reported together: an operator or a workload author
// should not need one admission round-trip per mistake.
func TestResolveReportsIndependentRejections(t *testing.T) {
	_, err := vllm.Resolve(vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve", "Qwen/Qwen3-8B"},
		"--tensor-parallel-size=0",
		"--gpu-memory-utilization=2",
	)})

	rs, ok := vllm.Rejected(err)
	if !ok {
		t.Fatalf("Resolve() error is not a structured rejection: %v", err)
	}
	if len(rs) < 2 {
		t.Fatalf("got %d rejections, want at least 2 independent problems: %v", len(rs), rs)
	}
	found := map[vllm.Setting]bool{}
	for _, r := range rs {
		found[r.Setting] = true
	}
	if !found[vllm.SettingTensorParallelSize] || !found[vllm.SettingGPUMemUtilization] {
		t.Fatalf("rejections %v do not name both offending settings", rs)
	}
}

// A missing model is a rejection: without model identity no profile can be
// selected.
func TestResolveRejectsMissingModel(t *testing.T) {
	_, err := vllm.Resolve(vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve"}, "--port=8000")})
	requireRejection(t, err, vllm.SettingModel, "does not specify a model")
}

// Accepting an invocation is the only path that returns a Config, so a rejected
// invocation can never be mistaken for a partially resolved one.
func TestResolveReturnsNoConfigOnRejection(t *testing.T) {
	cfg, err := vllm.Resolve(vllm.Invocation{Container: container("vllm/vllm-openai:v0.33.0",
		[]string{"vllm", "serve", "Qwen/Qwen3-8B", "--not-a-flag"})})
	if err == nil {
		t.Fatal("Resolve() accepted an unknown flag")
	}
	if cfg != nil {
		t.Fatalf("Resolve() returned a configuration together with an error: %v", cfg)
	}
}
