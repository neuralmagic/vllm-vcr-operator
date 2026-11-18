package webhook_test

import (
	"encoding/json"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vcrv1alpha1 "github.com/opendatahub-io/vllm-vcr-operator/api/v1alpha1"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/profile"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/webhook"
)

const (
	testModel     = "Qwen/Qwen3-0.6B"
	testProfileID = "qwen3-0.6b-h200-tp1-bf16"
	testImage     = "quay.io/opendatahub/vllm-vcr-profiles:qwen3-0.6b-h200-tp1-bf16"
	testDigest    = "sha256:0123456789abcdef"
	testFrontend  = "quay.io/opendatahub/vllm-vcr-frontend:v0.33.0-cpu"
	testBackend   = "quay.io/opendatahub/vllm-vcr-backend:v0.33.0"

	operatorNamespace = "vllm-vcr-operator-system"
	workloadNamespace = "llm-d"
)

// testMetadata is the profile document of docs/contracts/profile-image.md.
func testMetadata(mutate ...func(*profile.Metadata)) *profile.Metadata {
	metadata := &profile.Metadata{
		SchemaVersion: names.ProfileCatalogSchemaVersion,
		ID:            testProfileID,
		Model: profile.Model{
			Name:              testModel,
			Revision:          "c9340e6f3",
			Tokenizer:         testModel,
			TokenizerRevision: "c9340e6f3",
			ChatTemplateHash:  "sha256:abc123",
		},
		GPU:       profile.GPU{Type: "H200", Count: 1},
		Execution: profile.Execution{TensorParallel: 1, PipelineParallel: 1, DType: "bfloat16"},
		Cache: profile.Cache{
			BlockSize:           64,
			EnablePrefixCaching: true,
			KVBlocks:            2048,
			ChunkedPrefill:      true,
		},
		VLLM: profile.VLLM{Version: names.SupportedVLLMVersion, ConfigHash: "sha256:def456"},
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

// resolvedProfile is one reconciled status.profiles entry.
func resolvedProfile(t *testing.T, metadata *profile.Metadata) vcrv1alpha1.ResolvedProfile {
	t.Helper()
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal profile metadata: %v", err)
	}
	return vcrv1alpha1.ResolvedProfile{
		Image:        testImage,
		Digest:       testDigest,
		Metadata:     string(raw),
		ReconciledAt: metav1.NewTime(time.Unix(0, 0).UTC()),
	}
}

// testEnvironment is the reconciled singleton environment.
func testEnvironment(t *testing.T, mutate ...func(*vcrv1alpha1.SimulationEnvironment)) *vcrv1alpha1.SimulationEnvironment {
	t.Helper()
	env := &vcrv1alpha1.SimulationEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: vcrv1alpha1.SimulationEnvironmentNameDefault},
		Spec: vcrv1alpha1.SimulationEnvironmentSpec{
			NodeSelection: vcrv1alpha1.NodeSelection{
				NodeSelector: map[string]string{"vcr.opendatahub-io/simulation": "true"},
				Tolerations: []corev1.Toleration{{
					Key:      "vcr.opendatahub-io/simulation",
					Operator: corev1.TolerationOpExists,
					Effect:   corev1.TaintEffectNoSchedule,
				}},
			},
			DevicesPerNode: 8,
			GPUType:        "H200",
			Profiles:       []vcrv1alpha1.ProfileSource{{Image: testImage}},
			Runtime: vcrv1alpha1.RuntimeSizing{
				Frontend: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				}},
				Backend: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				}},
				Provisioner: &corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				}},
			},
			LogLevel: "info",
		},
		Status: vcrv1alpha1.SimulationEnvironmentStatus{
			ObservedGeneration:   1,
			SupportedVLLMVersion: names.SupportedVLLMVersion,
			Profiles:             []vcrv1alpha1.ResolvedProfile{resolvedProfile(t, testMetadata())},
			Conditions: []metav1.Condition{{
				Type:    vcrv1alpha1.ConditionReady,
				Status:  metav1.ConditionTrue,
				Reason:  vcrv1alpha1.ReasonEnvironmentReady,
				Message: "SimulationEnvironment is ready",
			}},
		},
	}
	for _, apply := range mutate {
		if apply != nil {
			apply(env)
		}
	}
	return env
}

// testPod is a GPU-requesting vLLM workload.
func testPod(args ...string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "qwen3-modelserver",
			Namespace: workloadNamespace,
			Labels:    map[string]string{"llm-d.ai/inferenceServing": "true"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:    "modelserver",
				Image:   "vllm/vllm-openai:v0.33.0",
				Command: []string{"vllm", "serve"},
				Args:    append([]string{testModel}, args...),
				Ports:   []corev1.ContainerPort{{Name: "http", ContainerPort: names.DefaultInferencePort}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("8"),
						corev1.ResourceName(names.SyntheticResourceName): resource.MustParse("1"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceName(names.SyntheticResourceName): resource.MustParse("1"),
					},
				},
			}},
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "workload-owned-pull"}},
		},
	}
}

// cpuOnlyPod requests no synthetic device.
func cpuOnlyPod() *corev1.Pod {
	pod := testPod()
	container := &pod.Spec.Containers[0]
	delete(container.Resources.Requests, corev1.ResourceName(names.SyntheticResourceName))
	container.Resources.Limits = nil
	return pod
}

// testScheme knows the operator API and core/v1.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := vcrv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add operator scheme: %v", err)
	}
	return scheme
}

// testClient builds a fake client with the supplied objects.
func testClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objects...).
		Build()
}

// testMutator builds a PodMutator over the fake client.
func testMutator(t *testing.T, c client.Client) *webhook.PodMutator {
	t.Helper()
	mutator, err := webhook.NewPodMutator(webhook.Options{
		Client:            c,
		OperatorNamespace: operatorNamespace,
		FrontendImage:     testFrontend,
		BackendImage:      testBackend,
		Now:               func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})
	if err != nil {
		t.Fatalf("new PodMutator: %v", err)
	}
	return mutator
}

// admissionRequest renders an AdmissionRequest whose object is the given object.
func admissionRequest(t *testing.T, operation admissionv1.Operation, object any, mutate ...func(*admissionv1.AdmissionRequest)) *admissionv1.AdmissionRequest {
	t.Helper()
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal admission object: %v", err)
	}
	req := &admissionv1.AdmissionRequest{
		Operation: operation,
		Object:    runtime.RawExtension{Raw: raw},
	}
	switch typed := object.(type) {
	case *corev1.Pod:
		req.Namespace = typed.Namespace
		req.Name = typed.Name
		req.Kind = metav1.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}
	case *vcrv1alpha1.SimulationEnvironment:
		req.Name = typed.Name
		req.Kind = metav1.GroupVersionKind{
			Group: vcrv1alpha1.GroupVersion.Group, Version: vcrv1alpha1.GroupVersion.Version,
			Kind: "SimulationEnvironment",
		}
	}
	for _, apply := range mutate {
		apply(req)
	}
	return req
}
