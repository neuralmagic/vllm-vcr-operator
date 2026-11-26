package controller_test

import (
	"os"
	"path/filepath"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// TestClusterRoleGrantsWhatOwnerReferencesNeed pins the coupling that broke a real install.
//
// The reconciler stamps an owner reference from the cluster-scoped VcrGpuCluster onto
// namespaced objects (device plugin ServiceAccount, DaemonSet) with blockOwnerDeletion set,
// because deleting the cluster must delete them. The API server refuses that unless the
// controller holds update on the owner's finalizers subresource:
//
//	cannot set blockOwnerDeletion if an ownerReference refers to a resource you can't set finalizers on
//
// envtest cannot catch it: its client is a cluster admin, so every permission is granted. That
// made it a defect only a real install could find, and only after the whole catalog,
// device-plugin and admission path had silently stopped working. This test reads the generated
// ClusterRole instead, so losing the verb fails in `go test`.
func TestClusterRoleGrantsWhatOwnerReferencesNeed(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatalf("read the generated ClusterRole: %v", err)
	}
	var clusterRole rbacv1.ClusterRole
	if err := yaml.Unmarshal(raw, &clusterRole); err != nil {
		t.Fatalf("parse the generated ClusterRole: %v", err)
	}

	// (group, resource, verb) triples the reconciler cannot work without.
	required := []struct{ group, resource, verb string }{
		// Cross-scope owner references, per the doc comment above.
		{"vcr.opendatahub-io", "vcrgpuclusters/finalizers", "update"},
		// Publishing the reconciled catalog and its conditions.
		{"vcr.opendatahub-io", "vcrgpuclusters/status", "update"},
		{"vcr.opendatahub-io", "vcrgpuclusters/status", "patch"},
		// The namespace selection's resolution.
		{"vcr.opendatahub-io", "vcrgpuprofiles/status", "update"},
	}
	for _, want := range required {
		if !grants(clusterRole, want.group, want.resource, want.verb) {
			t.Errorf("the generated ClusterRole does not grant %s/%s %s: check the kubebuilder marker in internal/controller/doc.go",
				want.group, want.resource, want.verb)
		}
	}
}

// grants reports whether the role contains the (group, resource, verb) triple.
func grants(role rbacv1.ClusterRole, group, resource, verb string) bool {
	for _, rule := range role.Rules {
		if !contains(rule.APIGroups, group) || !contains(rule.Resources, resource) {
			continue
		}
		if contains(rule.Verbs, verb) {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
