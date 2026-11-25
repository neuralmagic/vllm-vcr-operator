package translator_test

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
	"github.com/neuralmagic/vllm-vcr-operator/internal/plan"
	"github.com/neuralmagic/vllm-vcr-operator/internal/profile"
	"github.com/neuralmagic/vllm-vcr-operator/internal/translator"
	"github.com/neuralmagic/vllm-vcr-operator/internal/vllm"
)

const (
	testModel  = "Qwen/Qwen3-0.6B"
	testImage  = "quay.io/opendatahub/vllm-vcr-profiles:qwen3-0.6b-h200-tp1-bf16"
	testDigest = "sha256:0123456789abcdef"
)

// testMetadata is the profile document of docs/contracts/profile-image.md.
func testMetadata(mutate ...func(*profile.Metadata)) *profile.Metadata {
	metadata := &profile.Metadata{
		SchemaVersion: names.ProfileCatalogSchemaVersion,
		ID:            "qwen3-0.6b-h200-tp1-bf16",
		Model: profile.Model{
			Name:              testModel,
			Revision:          "c9340e6f3",
			Tokenizer:         testModel,
			TokenizerRevision: "c9340e6f3",
			ChatTemplateHash:  "sha256:abc123",
		},
		GPU:       profile.GPU{Type: "H200", Count: 1},
		Execution: profile.Execution{TensorParallel: 1, PipelineParallel: 1, DType: "bfloat16", Quantization: ""},
		Cache: profile.Cache{
			BlockSize:           64,
			EnablePrefixCaching: true,
			KVBlocks:            2048,
			ChunkedPrefill:      true,
		},
		VLLM: profile.VLLM{Version: "0.33.0", ConfigHash: "sha256:def456"},
		Coverage: profile.Coverage{
			MaxModelLen:               &profile.Range{Min: 1, Max: 131072},
			MaxNumSeqs:                &profile.Range{Min: 1, Max: 128},
			MaxNumBatchedTokens:       &profile.Range{Min: 1, Max: 8192},
			LongPrefillTokenThreshold: &profile.Range{Min: 0, Max: 4096},
			KVBlocks:                  &profile.Range{Min: 1, Max: 2048},
			SchedulingPolicies:        []string{"fcfs", "priority"},
		},
		Trace: profile.Trace{
			File:           "trace.jsonl.gz",
			Format:         profile.TraceFormatJSONL,
			RecordedTokens: true,
			LatencyModel:   true,
			KVEvents:       true,
		},
		Provisioning: profile.Provisioning{
			Entrypoint:   []string{"/usr/local/bin/vcr-profile-export"},
			MetadataFile: names.ProfileMetadataFileName,
			TraceFile:    "trace.jsonl.gz",
		},
		Capabilities: profile.Capabilities{
			ChatCompletions: true,
			Completions:     true,
			Streaming:       true,
		},
	}
	for _, apply := range mutate {
		if apply != nil {
			apply(metadata)
		}
	}
	return metadata
}

func testCatalog(t *testing.T, metadata *profile.Metadata) *profile.Catalog {
	t.Helper()
	catalog, err := profile.NewCatalog([]profile.Candidate{{
		Image:    testImage,
		Digest:   testDigest,
		Metadata: metadata,
	}})
	if err != nil {
		t.Fatalf("build catalog: %v", err)
	}
	return catalog
}

func testEnvironment() translator.Environment {
	return translator.Environment{
		GPUType:        "H200",
		DevicesPerNode: 8,
		Images: translator.Images{
			Frontend: "quay.io/opendatahub/vllm-cpu:v0.33.0",
			Backend:  "ghcr.io/neuralmagic/vllm-vcr:vllm0.33",
		},
		Sizing: plan.Resources{
			Frontend: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			}},
			Backend: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			}},
		},
		NodeSelector: map[string]string{"vcr.opendatahub-io/simulation": "true"},
		Tolerations: []corev1.Toleration{
			{Key: "vcr.opendatahub-io/simulation", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		},
		PullSecrets: []string{"profile-pull"},
	}
}

// resolveConfig resolves a workload invocation through the public resolver, so the
// tests exercise the same origins admission sees.
func resolveConfig(t *testing.T, args []string, env ...corev1.EnvVar) *vllm.Config {
	t.Helper()
	container := corev1.Container{
		Name:    "modelserver",
		Command: []string{"vllm", "serve"},
		Args:    append([]string{testModel}, args...),
		Env:     env,
	}
	cfg, err := vllm.Resolve(vllm.Invocation{
		Container: container,
		Volumes:   map[string]corev1.Volume{},
	})
	if err != nil {
		t.Fatalf("resolve invocation %v: %v", args, err)
	}
	return cfg
}

type translateCase struct {
	name     string
	args     []string
	env      []corev1.EnvVar
	mutate   func(*profile.Metadata)
	devices  int32
	identity string

	// wantSetting and wantReason assert a rejection. Both must match a rejection
	// in the reported set.
	wantSetting string
	wantReason  string

	// check asserts an accepted plan.
	check func(t *testing.T, admission *plan.Admission)
}

func runTranslate(t *testing.T, tc translateCase, environment translator.Environment) (*plan.Admission, error) {
	t.Helper()
	metadata := testMetadata(tc.mutate)
	devices := tc.devices
	if devices == 0 {
		devices = 1
	}
	return translator.Translate(translator.Request{
		Config:      resolveConfig(t, tc.args, tc.env...),
		Catalog:     testCatalog(t, metadata),
		Environment: environment,
		GPUDevices:  devices,
		PodIdentity: tc.identity,
	})
}

func mustAccept(t *testing.T, admission *plan.Admission, err error) *plan.Admission {
	t.Helper()
	if err != nil {
		t.Fatalf("Translate() rejected the workload: %v", err)
	}
	if admission == nil {
		t.Fatal("Translate() returned no admission and no error")
	}
	return admission
}

func mustReject(t *testing.T, tc translateCase, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("Translate() accepted a workload that the policy rejects (%s)", tc.wantSetting)
	}
	rejections, ok := translator.Rejected(err)
	if !ok {
		t.Fatalf("rejection is not structured: %v", err)
	}
	for _, rejection := range rejections {
		if rejection.Setting != tc.wantSetting {
			continue
		}
		if tc.wantReason != "" && !strings.Contains(rejection.Reason, tc.wantReason) {
			t.Fatalf("rejection for %s = %q, want it to mention %q", tc.wantSetting, rejection.Reason, tc.wantReason)
		}
		return
	}
	t.Fatalf("rejections %v do not name %q", rejections, tc.wantSetting)
}

// hasArgs reports whether argv contains the given contiguous flag/value sequence.
func hasArgs(argv []string, want ...string) bool {
	for i := 0; i+len(want) <= len(argv); i++ {
		match := true
		for j, token := range want {
			if argv[i+j] != token {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func requireArgs(t *testing.T, label string, argv []string, want ...string) {
	t.Helper()
	if !hasArgs(argv, want...) {
		t.Errorf("%s argv %v does not contain %v", label, argv, want)
	}
}

func rejectArgs(t *testing.T, label string, argv []string, unwanted string) {
	t.Helper()
	for _, token := range argv {
		if token == unwanted {
			t.Errorf("%s argv %v contains the rejected %q", label, argv, unwanted)
		}
	}
}

func warningsContain(admission *plan.Admission, want string) bool {
	for _, warning := range admission.Warnings {
		if strings.Contains(warning, want) {
			return true
		}
	}
	return false
}

// credentialFree fails when a credential value appears anywhere in the plan.
func credentialFree(t *testing.T, admission *plan.Admission, secret string) {
	t.Helper()
	encoded, err := json.Marshal(admission)
	if err != nil {
		t.Fatalf("marshal admission: %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("plan contains the credential value %q: %s", secret, encoded)
	}
}
