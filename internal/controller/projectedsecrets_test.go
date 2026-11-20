package controller_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/controller"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/projection"
)

// projectedCopy builds an operator-managed copy the way admission writes it.
func projectedCopy(t *testing.T, namespace, name string, payload []byte, sources []string, imageHost string) *corev1.Secret {
	t.Helper()
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels:    map[string]string{names.ManagedByLabel: names.OperatorName},
			Annotations: map[string]string{
				projection.AppliedSourcesAnnotation:   sources[0],
				projection.AppliedImageHostAnnotation: imageHost,
				projection.AppliedExplicitAnnotation:  "true",
				projection.AppliedAtAnnotation:        "2026-01-01T00:00:00Z",
			},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: payload},
	}
}

// sourcePullSecret builds a docker-config Secret in the operator namespace, the
// input every projected copy is built from.
func sourcePullSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testOperatorNamespace, Name: name},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(
			`{"auths":{"registry.example.com":{"auth":"b2xk"}}}`)},
	}
}

// podUsing builds a Pod that references the projected Secret.
func podUsing(namespace, secretName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "workload"},
		Spec: corev1.PodSpec{
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: secretName}},
			Containers:       []corev1.Container{{Name: "c", Image: "busybox"}},
		},
	}
}

// TestProjectedSecretIsRemovedWhenNoPodUsesIt covers the leak this reconciler
// exists for: admission creates a copy per workload namespace and nothing else
// deletes it. A copy no Pod references must disappear.
func TestProjectedSecretIsRemovedWhenNoPodUsesIt(t *testing.T) {
	namespace := "workload"
	copyName := names.PullSecretPrefix + "deadbeef"
	c := testClient(t,
		sourcePullSecret("profile-pull"),
		projectedCopy(t, namespace, copyName, []byte(`{"auths":{}}`), []string{"profile-pull"}, ""),
	)
	r := &controller.ProjectedSecretReconciler{
		Client:            c,
		Reader:            c,
		OperatorNamespace: testOperatorNamespace,
		Now:               func() time.Time { return fixedTime },
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: copyName}, &corev1.Secret{})
	if err == nil {
		t.Fatalf("unreferenced copy %s/%s still exists", namespace, copyName)
	}
}

// TestProjectedSecretIsKeptWhileAPodUsesIt is the other half: deleting a copy a
// running Pod references would break its next image pull.
func TestProjectedSecretIsKeptWhileAPodUsesIt(t *testing.T) {
	namespace := "workload"
	copyName := names.PullSecretPrefix + "cafebabe"
	c := testClient(t,
		sourcePullSecret("profile-pull"),
		projectedCopy(t, namespace, copyName, []byte(`{"auths":{}}`), []string{"profile-pull"}, ""),
		podUsing(namespace, copyName),
	)
	r := &controller.ProjectedSecretReconciler{
		Client:            c,
		Reader:            c,
		OperatorNamespace: testOperatorNamespace,
		Now:               func() time.Time { return fixedTime },
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: copyName}, &corev1.Secret{}); err != nil {
		t.Fatalf("referenced copy was removed: %v", err)
	}
}

// TestProjectedSecretIsRefreshedFromItsSources covers rotation: the source Secret
// changed, so the copy must be rewritten even though its Pods are still running.
func TestProjectedSecretIsRefreshedFromItsSources(t *testing.T) {
	namespace := "workload"
	copyName := projection.Name([]string{"rotating-pull"})
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testOperatorNamespace, Name: "rotating-pull"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(
			`{"auths":{"registry.example.com":{"auth":"bmV3"}}}`)},
	}
	stale := projectedCopy(t, namespace, copyName, []byte(`{"auths":{}}`), []string{"rotating-pull"}, "")
	c := testClient(t, source, stale, podUsing(namespace, copyName))

	r := &controller.ProjectedSecretReconciler{
		Client:            c,
		Reader:            c,
		OperatorNamespace: testOperatorNamespace,
		Now:               func() time.Time { return fixedTime },
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: copyName}, updated); err != nil {
		t.Fatalf("get refreshed copy: %v", err)
	}
	if string(updated.Data[corev1.DockerConfigJsonKey]) == `{"auths":{}}` {
		t.Fatalf("copy was not refreshed from its rotated source: %s", updated.Data[corev1.DockerConfigJsonKey])
	}
}

// TestForeignSecretsAreIgnored: only operator-managed copies are swept.
func TestForeignSecretsAreIgnored(t *testing.T) {
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "workload", Name: names.PullSecretPrefix + "foreign"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{}`)},
	}
	c := testClient(t, foreign)
	r := &controller.ProjectedSecretReconciler{
		Client:            c,
		Reader:            c,
		OperatorNamespace: testOperatorNamespace,
		Now:               func() time.Time { return fixedTime },
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), &corev1.Secret{}); err != nil {
		t.Fatalf("a Secret the operator does not manage was removed: %v", err)
	}
}
