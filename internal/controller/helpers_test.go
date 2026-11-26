package controller_test

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
)

const testOperatorNamespace = "vllm-vcr-operator-system"

// testScheme registers the types the controller touches, so tests exercise the
// same owner-reference and scheme paths the manager uses.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(vcrv1alpha1.AddToScheme(scheme))
	return scheme
}

func testClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&vcrv1alpha1.VcrGpuCluster{}).
		WithObjects(objects...).
		Build()
}

// testEnvironment returns a VcrGpuCluster with the singleton name and a
// node selector, which the CRD requires to be non-empty.
func testEnvironment() *vcrv1alpha1.VcrGpuCluster {
	return &vcrv1alpha1.VcrGpuCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       vcrv1alpha1.VcrGpuClusterNameDefault,
			Generation: 1,
		},
		Spec: vcrv1alpha1.VcrGpuClusterSpec{
			NodeSelection: vcrv1alpha1.NodeSelection{
				NodeSelector: map[string]string{"vcr.opendatahub-io/simulation": "true"},
			},
			DevicesPerNode: 2,
			GPUType:        "H200",
			LogLevel:       "info",
		},
	}
}

// contractMetadata returns a profile metadata document that satisfies
// docs/contracts/profile-image.md, with the given id and model.
func contractMetadata(t *testing.T, id, model string) []byte {
	t.Helper()
	document := map[string]any{
		"schemaVersion": names.ProfileCatalogSchemaVersion,
		"id":            id,
		"model": map[string]any{
			"name":              model,
			"revision":          "revision-1",
			"tokenizer":         model,
			"tokenizerRevision": "revision-1",
			"chatTemplateHash":  "sha256:template",
		},
		"gpu": map[string]any{"type": "H200", "count": 1},
		"execution": map[string]any{
			"tensorParallel":   1,
			"pipelineParallel": 1,
			"dtype":            "bfloat16",
			"quantization":     "",
		},
		"cache": map[string]any{
			"blockSize":           64,
			"enablePrefixCaching": true,
			"kvBlocks":            2048,
			"chunkedPrefill":      true,
		},
		"vllm": map[string]any{"version": names.SupportedVLLMVersion, "configHash": "sha256:config"},
		"coverage": map[string]any{
			"maxModelLen":         map[string]any{"min": 1, "max": 131072},
			"maxNumSeqs":          map[string]any{"min": 1, "max": 128},
			"maxNumBatchedTokens": map[string]any{"min": 1, "max": 8192},
			"kvBlocks":            map[string]any{"min": 1, "max": 2048},
			"schedulingPolicies":  []any{"fcfs"},
		},
		"trace": map[string]any{
			"file":           "trace.jsonl.gz",
			"format":         "vllm-vcr/jsonl",
			"recordedTokens": true,
			"latencyModel":   true,
			"kvEvents":       true,
		},
		"provisioning": map[string]any{
			"entrypoint":   []any{"/usr/local/bin/vcr-profile-export"},
			"metadataFile": "metadata.json",
			"traceFile":    "trace.jsonl.gz",
		},
		"capabilities": map[string]any{
			"chatCompletions": true,
			"completions":     true,
			"streaming":       true,
		},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal profile metadata fixture: %v", err)
	}
	return raw
}

func conditionOf(t *testing.T, env *vcrv1alpha1.VcrGpuCluster, conditionType string) metav1.Condition {
	t.Helper()
	for _, condition := range env.Status.Conditions {
		if condition.Type == conditionType {
			return condition
		}
	}
	t.Fatalf("environment has no %q condition: %+v", conditionType, env.Status.Conditions)
	return metav1.Condition{}
}
