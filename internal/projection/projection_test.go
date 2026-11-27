package projection_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
	"github.com/neuralmagic/vllm-vcr-operator/internal/projection"
)

const operatorNamespace = "vllm-vcr-operator-system"

func testClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithObjects(objects...).
		Build()
	return c
}

// pullSecret builds an operator-namespace docker config Secret.
func pullSecret(name, document string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorNamespace, Name: name},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(document)},
	}
}

// authKeys returns the registry keys of a merged payload.
func authKeys(t *testing.T, merged *projection.Merged) []string {
	t.Helper()
	var document struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	if err := json.Unmarshal(merged.Payload, &document); err != nil {
		t.Fatalf("merged payload is not a docker config document: %v", err)
	}
	keys := make([]string, 0, len(document.Auths))
	for key := range document.Auths {
		keys = append(keys, key)
	}
	return keys
}

// TestMergeTreatsRegistrySpellingsAsOneRegistry: docker config documents spell one
// registry several ways, so the ambiguity check must compare normalized hosts.
// Otherwise two sources carrying credentials for the same registry merge into one
// payload and the surviving credential depends on source order.
func TestMergeTreatsRegistrySpellingsAsOneRegistry(t *testing.T) {
	c := testClient(t,
		pullSecret("hub-plain", `{"auths":{"docker.io":{"auth":"b25l"}}}`),
		pullSecret("hub-index", `{"auths":{"https://index.docker.io/v1/":{"auth":"dHdv"}}}`),
	)
	_, err := projection.Merge(context.Background(), c, operatorNamespace,
		[]string{"hub-plain", "hub-index"}, "", true)
	if err == nil {
		t.Fatalf("Merge accepted two sources carrying credentials for the same registry")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error = %v, want it to report the ambiguity", err)
	}
}

// TestMergeKeysThePayloadByNormalizedHost: the projected copy is consumed by a
// kubelet that resolves the index host, so the payload must be keyed by it too.
func TestMergeKeysThePayloadByNormalizedHost(t *testing.T) {
	c := testClient(t, pullSecret("hub", `{"auths":{"docker.io":{"auth":"b25l"}}}`))
	merged, err := projection.Merge(context.Background(), c, operatorNamespace,
		[]string{"hub"}, "index.docker.io", false)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if merged == nil {
		t.Fatalf("Merge dropped a credential for the selected registry")
	}
	keys := authKeys(t, merged)
	if len(keys) != 1 || keys[0] != "index.docker.io" {
		t.Fatalf("payload keys = %v, want the normalized host", keys)
	}
}

// TestMergeFiltersToTheImageHost: the environment-wide list is only a set of
// candidates, so a credential for another registry must not travel with the copy.
func TestMergeFiltersToTheImageHost(t *testing.T) {
	c := testClient(t, pullSecret("wide", `{"auths":{"quay.io":{"auth":"b25l"},"ghcr.io":{"auth":"dHdv"}}}`))
	merged, err := projection.Merge(context.Background(), c, operatorNamespace,
		[]string{"wide"}, "ghcr.io", false)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if keys := authKeys(t, merged); len(keys) != 1 || keys[0] != "ghcr.io" {
		t.Fatalf("payload keys = %v, want only the selected registry", keys)
	}
	if len(merged.Sources) != 1 || merged.Sources[0] != "wide" {
		t.Errorf("sources = %v, want the contributing source", merged.Sources)
	}
}

// TestMergeReportsNothingToProject: a merge that keeps no entry returns nil rather
// than an empty document, which is how admission knows to pull anonymously.
func TestMergeReportsNothingToProject(t *testing.T) {
	c := testClient(t, pullSecret("wide", `{"auths":{"quay.io":{"auth":"b25l"}}}`))
	merged, err := projection.Merge(context.Background(), c, operatorNamespace,
		[]string{"wide"}, "ghcr.io", false)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if merged != nil {
		t.Fatalf("Merge = %+v, want nil when no credential can apply", merged)
	}
}

// TestNameIsDerivedFromTheSourceSet: the copy is named after the sources, not after
// a Pod, so every Pod using one profile shares one copy.
func TestNameIsDerivedFromTheSourceSet(t *testing.T) {
	first := projection.Name([]string{"a", "b"})
	second := projection.Name([]string{"b", "a"})
	if first != second {
		t.Errorf("Name depends on source order: %q and %q", first, second)
	}
	if !strings.HasPrefix(first, names.PullSecretPrefix) {
		t.Errorf("Name = %q, want the operator prefix", first)
	}
	if other := projection.Name([]string{"a"}); other == first {
		t.Errorf("Name(%v) collides with Name(%v)", []string{"a"}, []string{"a", "b"})
	}
}

// TestEnsureCreatesThenLeavesAnIdenticalCopy covers both halves of the contract:
// the first call creates the copy, and re-running with identical inputs produces no
// write at all (a rewritten Secret would churn every Pod that references it).
func TestEnsureCreatesThenLeavesAnIdenticalCopy(t *testing.T) {
	c := testClient(t, pullSecret("wide", `{"auths":{"ghcr.io":{"auth":"b25l"}}}`))
	merged, err := projection.Merge(context.Background(), c, operatorNamespace, []string{"wide"}, "ghcr.io", false)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	ctx := context.Background()
	name := projection.Name(merged.Sources)
	projected := projection.Copy{
		Merged:    *merged,
		Namespace: "workload",
		Name:      name,
		AppliedTo: "workload/pod",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	if err := projection.Ensure(ctx, c, c, projected); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	created := &corev1.Secret{}
	key := types.NamespacedName{Namespace: "workload", Name: name}
	if err := c.Get(ctx, key, created); err != nil {
		t.Fatalf("projected Secret %s: %v", key, err)
	}
	if created.Labels[names.ManagedByLabel] != names.OperatorName {
		t.Errorf("labels = %v, want the managed-by label", created.Labels)
	}
	if got := created.Annotations[projection.AppliedToAnnotation]; got != "workload/pod" {
		t.Errorf("%s = %q, want the workload it was applied for", projection.AppliedToAnnotation, got)
	}

	projected.UpdatedAt = "2026-01-02T00:00:00Z"
	if err := projection.Ensure(ctx, c, c, projected); err != nil {
		t.Fatalf("Ensure (identical): %v", err)
	}
	unchanged := &corev1.Secret{}
	if err := c.Get(ctx, key, unchanged); err != nil {
		t.Fatalf("projected Secret %s: %v", key, err)
	}
	if got := unchanged.Annotations[projection.AppliedAtAnnotation]; got != "2026-01-01T00:00:00Z" {
		t.Errorf("%s = %q, want an identical copy left untouched", projection.AppliedAtAnnotation, got)
	}
}

// TestEnsureRefusesAForeignSecret: a Secret that already holds the copy's name but
// is not operator-managed is somebody else's object, and overwriting it would
// destroy their data.
func TestEnsureRefusesAForeignSecret(t *testing.T) {
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "workload", Name: names.PullSecretPrefix + "abc123"},
	}
	c := testClient(t, foreign)
	err := projection.Ensure(context.Background(), c, c, projection.Copy{
		Merged:    projection.Merged{Payload: []byte(`{"auths":{}}`)},
		Namespace: "workload",
		Name:      foreign.Name,
		UpdatedAt: "2026-01-01T00:00:00Z",
	})
	if err == nil {
		t.Fatalf("Ensure overwrote a Secret it does not manage")
	}
	if !strings.Contains(err.Error(), names.OperatorName) {
		t.Errorf("error = %v, want it to name the operator that owns the copy", err)
	}
}

// TestPackageHasNoClusterRuntimeDependencies guards the package boundary: this
// package holds the one implementation admission and the reconciler share, and it
// must be exercised without a cluster. A real client configuration would mean the
// seam had been bypassed.
func TestPackageHasNoClusterRuntimeDependencies(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") ||
			strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		source, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		text := string(source)
		for _, forbidden := range []string{"clientcmd", "rest.Config", "ctrl.GetConfig", "NewManager"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s uses %q: projection must take its client from the caller", entry.Name(), forbidden)
			}
		}
	}
}

// TestEnsureKeepsAnnotationsItDoesNotOwn: a copy may carry annotations an operator or
// a policy added. Refreshing the payload must not delete them.
func TestEnsureKeepsAnnotationsItDoesNotOwn(t *testing.T) {
	name := projection.Name([]string{"wide"})
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "workload",
			Name:      name,
			Labels:    map[string]string{names.ManagedByLabel: names.OperatorName},
			Annotations: map[string]string{
				"example.com/owner":                 "team-a",
				projection.AppliedSourcesAnnotation: "wide",
			},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
	}
	c := testClient(t, existing)

	if err := projection.Ensure(context.Background(), c, c, projection.Copy{
		Merged:    projection.Merged{Payload: []byte(`{"auths":{"ghcr.io":{"auth":"b25l"}}}`), Sources: []string{"wide"}, ImageHost: "ghcr.io"},
		Namespace: "workload",
		Name:      name,
		UpdatedAt: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	refreshed := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "workload", Name: name}, refreshed); err != nil {
		t.Fatalf("get refreshed copy: %v", err)
	}
	if got := refreshed.Annotations["example.com/owner"]; got != "team-a" {
		t.Errorf("annotation example.com/owner = %q, want it preserved", got)
	}
	if string(refreshed.Data[corev1.DockerConfigJsonKey]) == `{"auths":{}}` {
		t.Errorf("payload was not refreshed")
	}
}
