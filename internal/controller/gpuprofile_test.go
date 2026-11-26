package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/profile"
)

// selectionMetadata is a profile document the operator's own parser accepts.
func selectionMetadata(id string) *profile.Metadata {
	return &profile.Metadata{
		SchemaVersion: "vcr.opendatahub-io/profile/v1",
		ID:            id,
		Model: profile.Model{
			Name:              "Qwen/Qwen3-0.6B",
			Revision:          "c1899de",
			Tokenizer:         "Qwen/Qwen3-0.6B",
			TokenizerRevision: "c1899de",
			ChatTemplateHash:  "sha256:abc123",
		},
		GPU:       profile.GPU{Type: "H200", Count: 1},
		Execution: profile.Execution{TensorParallel: 1, PipelineParallel: 1, DType: "bfloat16"},
		Cache:     profile.Cache{BlockSize: 64, EnablePrefixCaching: true, KVBlocks: 18466},
		VLLM:      profile.VLLM{Version: "0.33.0", ConfigHash: "sha256:def456"},
		Coverage: profile.Coverage{
			MaxModelLen:         &profile.Range{Min: 1, Max: 40960},
			MaxNumSeqs:          &profile.Range{Min: 1, Max: 256},
			MaxNumBatchedTokens: &profile.Range{Min: 1, Max: 8192},
			KVBlocks:            &profile.Range{Min: 1, Max: 18466},
			SchedulingPolicies:  []string{"fcfs"},
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
			MetadataFile: "metadata.json",
			TraceFile:    "trace.jsonl.gz",
		},
		Capabilities: profile.Capabilities{ChatCompletions: true, Completions: true, Streaming: true},
	}
}

func profileCluster(t *testing.T, ids ...string) *vcrv1alpha1.VcrGpuCluster {
	t.Helper()
	cluster := &vcrv1alpha1.VcrGpuCluster{
		ObjectMeta: metav1.ObjectMeta{Name: vcrv1alpha1.VcrGpuClusterNameDefault},
	}
	for _, id := range ids {
		metadata := selectionMetadata(id)
		raw, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		cluster.Status.Profiles = append(cluster.Status.Profiles, vcrv1alpha1.ResolvedProfile{
			Image:    "ghcr.io/neuralmagic/vllm-vcr-profiles:" + id + "-0.1.0",
			Digest:   "sha256:" + id,
			Metadata: string(raw),
		})
	}
	return cluster
}

// TestResolveSelectionPinsTheRequestedIdentity: the status has to say which recording an
// id became, digest included, so a user can see the choice without reading the cluster
// configuration.
func TestResolveSelectionPinsTheRequestedIdentity(t *testing.T) {
	lookup := resolveSelection(profileCluster(t, "baseline", "poisson"), "poisson")
	resolved := lookup.resolved
	if resolved == nil {
		t.Fatalf("poisson did not resolve; offered %v", lookup.offered)
	}
	if resolved.ID != "poisson" || resolved.Digest != "sha256:poisson" {
		t.Errorf("resolved = %+v", resolved)
	}
	if resolved.Model != "Qwen/Qwen3-0.6B" || resolved.GPUType != "H200" || resolved.VLLMVersion != "0.33.0" {
		t.Errorf("resolution does not describe the recording: %+v", resolved)
	}
}

// TestResolveSelectionReportsWhatIsOffered: a wrong id is a mistake worth listing the
// alternatives for.
func TestResolveSelectionReportsWhatIsOffered(t *testing.T) {
	lookup := resolveSelection(profileCluster(t, "baseline", "poisson"), "staircase")
	if lookup.resolved != nil {
		t.Fatalf("an unknown id resolved to %+v", lookup.resolved)
	}
	if len(lookup.offered) != 2 || lookup.offered[0] != "baseline" || lookup.offered[1] != "poisson" {
		t.Errorf("offered = %v, want the catalog's identities sorted", lookup.offered)
	}
}

func profileScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := vcrv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// TestReconcilePublishesResolutionAndCondition exercises the reconciliation the way the
// manager runs it: an unknown id is reported on the object rather than left to fail later
// at Pod creation.
func TestReconcilePublishesResolutionAndCondition(t *testing.T) {
	selection := &vcrv1alpha1.VcrGpuProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "selection", Namespace: "team-a", Generation: 1},
		Spec:       vcrv1alpha1.VcrGpuProfileSpec{Profile: "poisson"},
	}
	unknown := selection.DeepCopy()
	unknown.Name = "wrong"
	unknown.Spec.Profile = "not-offered"

	base := fake.NewClientBuilder().
		WithScheme(profileScheme(t)).
		WithObjects(profileCluster(t, "baseline", "poisson"), selection, unknown).
		WithStatusSubresource(&vcrv1alpha1.VcrGpuProfile{}).
		Build()

	reconciler := NewVcrGpuProfileReconciler(base)
	reconciler.Now = func() time.Time { return time.Unix(1700000000, 0).UTC() }

	for _, name := range []string{"selection", "wrong"} {
		if _, err := reconciler.Reconcile(t.Context(), reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: "team-a", Name: name},
		}); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}
	}

	good := &vcrv1alpha1.VcrGpuProfile{}
	if err := base.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: "selection"}, good); err != nil {
		t.Fatal(err)
	}
	if good.Status.Resolved == nil || good.Status.Resolved.ID != "poisson" {
		t.Errorf("resolved = %+v", good.Status.Resolved)
	}
	if condition := findCondition(good.Status.Conditions, conditionProfileReady); condition == nil ||
		condition.Status != metav1.ConditionTrue {
		t.Errorf("Ready condition = %+v, want true", condition)
	}

	bad := &vcrv1alpha1.VcrGpuProfile{}
	if err := base.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: "wrong"}, bad); err != nil {
		t.Fatal(err)
	}
	if bad.Status.Resolved != nil {
		t.Errorf("an unknown profile resolved to %+v", bad.Status.Resolved)
	}
	condition := findCondition(bad.Status.Conditions, conditionProfileReady)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "NotOffered" {
		t.Fatalf("Ready condition = %+v", condition)
	}
	if !strings.Contains(condition.Message, "poisson") {
		t.Errorf("message %q does not list what the catalog offers", condition.Message)
	}
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// TestUnusablePublishedProfileIsReportedAsSuch: when the catalog publishes an identity it
// cannot use, a selection naming it must be told that — reporting it as absent would send
// the user looking for a profile that is right there and hide a broken catalog behind an
// empty list.
func TestUnusablePublishedProfileIsReportedAsSuch(t *testing.T) {
	cluster := profileCluster(t, "baseline")
	cluster.Status.Profiles[0].Message = "the image could not be resolved: registry unreachable"

	lookup := resolveSelection(cluster, "baseline")
	if lookup.resolved != nil {
		t.Fatalf("an unusable entry resolved to %+v", lookup.resolved)
	}
	reason := lookup.describeUnusable("baseline")
	if !strings.Contains(reason, "registry unreachable") {
		t.Errorf("reason = %q, want the catalog's message", reason)
	}
	if len(lookup.offered) != 1 || lookup.offered[0] != "baseline" {
		t.Errorf("offered = %v, want the identity to stay visible", lookup.offered)
	}
}
