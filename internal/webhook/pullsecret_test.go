package webhook_test

import (
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
	"github.com/neuralmagic/vllm-vcr-operator/internal/projection"
)

const (
	workloadPullSecret = "workload-private-registry"
	profilePullSecret  = "profile-registry"
	workloadPassword   = "workload-s3cret-value"
)

// dockerConfigSecret is an operator-namespace pull Secret.
func dockerConfigSecret(name string, auths map[string]map[string]string) *corev1.Secret {
	document := map[string]any{"auths": auths}
	raw, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: operatorNamespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: raw},
	}
}

// environmentWithPullSecret points the environment at one operator-namespace pull
// Secret.
func environmentWithPullSecret(t *testing.T, secretName string) *vcrv1alpha1.VcrGpuCluster {
	t.Helper()
	return testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Spec.ImagePullSecrets = []vcrv1alpha1.LocalSecretReference{{Name: secretName}}
	})
}

// projectedPullSecretName returns the Secret name the mutated Pod references for
// the profile.
func projectedPullSecretName(t *testing.T, pod *corev1.Pod) string {
	t.Helper()
	var projected []string
	for _, reference := range pod.Spec.ImagePullSecrets {
		if strings.HasPrefix(reference.Name, names.PullSecretPrefix) {
			projected = append(projected, reference.Name)
		}
	}
	if len(projected) != 1 {
		t.Fatalf("imagePullSecrets %v carry %d projected profiles, want exactly one", pod.Spec.ImagePullSecrets, len(projected))
	}
	return projected[0]
}

// TestPrivateProfileSecretIsProjected asserts the selected profile's credentials
// are copied into the workload namespace and referenced next to the workload's own
// pull secret.
func TestPrivateProfileSecretIsProjected(t *testing.T) {
	source := dockerConfigSecret(workloadPullSecret, map[string]map[string]string{
		"quay.io": {"username": "profile-puller", "password": workloadPassword},
	})
	c := testClient(t, environmentWithPullSecret(t, workloadPullSecret), source)
	mutator := testMutator(t, c)
	pod := testPod()

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
	if !resp.Allowed {
		t.Fatalf("GPU Pod denied: %s", messageOf(resp))
	}
	mutated := applyPatch(t, pod, resp.Patch)

	if !hasPullSecret(mutated, "workload-owned-pull") {
		t.Errorf("imagePullSecrets %v dropped the workload's own secret", mutated.Spec.ImagePullSecrets)
	}
	projectedName := projectedPullSecretName(t, mutated)

	projected := &corev1.Secret{}
	key := types.NamespacedName{Namespace: workloadNamespace, Name: projectedName}
	if err := c.Get(t.Context(), key, projected); err != nil {
		t.Fatalf("projected Secret %s: %v", key, err)
	}
	if projected.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("projected Secret type = %q, want %q", projected.Type, corev1.SecretTypeDockerConfigJson)
	}
	if got := projected.Labels[names.ManagedByLabel]; got != names.OperatorName {
		t.Errorf("projected Secret label %s = %q, want %q", names.ManagedByLabel, got, names.OperatorName)
	}
	if got, want := projected.Annotations[projection.AppliedToAnnotation], workloadNamespace+"/"+pod.Name; got != want {
		t.Errorf("annotation %s = %q, want %q", projection.AppliedToAnnotation, got, want)
	}
	if projected.Annotations[projection.AppliedAtAnnotation] == "" {
		t.Errorf("annotation %s is not set", projection.AppliedAtAnnotation)
	}

	// The projected payload is the merged source document, not a re-encoded or
	// redacted form of it.
	var payload struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(projected.Data[corev1.DockerConfigJsonKey], &payload); err != nil {
		t.Fatalf("projected payload is not a docker config document: %v", err)
	}
	entry, ok := payload.Auths["quay.io"]
	if !ok {
		t.Fatalf("projected payload has no quay.io entry: %v", payload.Auths)
	}
	if entry.Username != "profile-puller" || entry.Password != workloadPassword {
		t.Errorf("projected quay.io entry does not match the source")
	}

	// The response never carries the payload.
	if strings.Contains(string(resp.Patch), workloadPassword) {
		t.Errorf("admission patch contains the pull credential")
	}
	if strings.Contains(messageOf(resp), workloadPassword) {
		t.Errorf("admission message contains the pull credential")
	}
}

// TestProfilePullSecretOverridesEnvironmentSecrets asserts only the Secret the
// selected profile names is projected when it overrides the environment-wide list.
func TestProfilePullSecretOverridesEnvironmentSecrets(t *testing.T) {
	env := testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Spec.ImagePullSecrets = []vcrv1alpha1.LocalSecretReference{{Name: workloadPullSecret}}
		e.Spec.Profiles[0].PullSecretName = profilePullSecret
	})
	c := testClient(t, env,
		dockerConfigSecret(workloadPullSecret, map[string]map[string]string{
			"quay.io": {"username": "wide", "password": "wide-value"},
		}),
		dockerConfigSecret(profilePullSecret, map[string]map[string]string{
			"ghcr.io": {"username": "profile", "password": "profile-value"},
		}),
	)
	mutator := testMutator(t, c)
	pod := testPod()

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
	if !resp.Allowed {
		t.Fatalf("GPU Pod denied: %s", messageOf(resp))
	}
	mutated := applyPatch(t, pod, resp.Patch)
	projected := &corev1.Secret{}
	key := types.NamespacedName{Namespace: workloadNamespace, Name: projectedPullSecretName(t, mutated)}
	if err := c.Get(t.Context(), key, projected); err != nil {
		t.Fatalf("projected Secret %s: %v", key, err)
	}

	var payload struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	if err := json.Unmarshal(projected.Data[corev1.DockerConfigJsonKey], &payload); err != nil {
		t.Fatalf("projected payload is not a docker config document: %v", err)
	}
	if _, ok := payload.Auths["ghcr.io"]; !ok {
		t.Errorf("projected payload does not carry the profile's registry entry: %v", payload.Auths)
	}
	if _, ok := payload.Auths["quay.io"]; ok {
		t.Errorf("projected payload carries the overridden environment-wide credential: %v", payload.Auths)
	}
}

// TestLegacyDockercfgSecretIsProjected asserts the legacy docker config key is
// accepted and converted into a dockerconfigjson payload.
func TestLegacyDockercfgSecretIsProjected(t *testing.T) {
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: workloadPullSecret, Namespace: operatorNamespace},
		// The entry names the selected profile image's registry: an entry for
		// another registry cannot authenticate that pull and is not projected.
		Data: map[string][]byte{corev1.DockerConfigKey: []byte(
			`{"quay.io":{"username":"legacy","password":"legacy-value"}}`)},
	}
	c := testClient(t, environmentWithPullSecret(t, workloadPullSecret), legacy)
	mutator := testMutator(t, c)
	pod := testPod()

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
	if !resp.Allowed {
		t.Fatalf("GPU Pod denied: %s", messageOf(resp))
	}
	mutated := applyPatch(t, pod, resp.Patch)
	projected := &corev1.Secret{}
	key := types.NamespacedName{Namespace: workloadNamespace, Name: projectedPullSecretName(t, mutated)}
	if err := c.Get(t.Context(), key, projected); err != nil {
		t.Fatalf("projected Secret %s: %v", key, err)
	}
	if projected.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("projected Secret type = %q, want %q", projected.Type, corev1.SecretTypeDockerConfigJson)
	}
	var payload struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	if err := json.Unmarshal(projected.Data[corev1.DockerConfigJsonKey], &payload); err != nil {
		t.Fatalf("projected payload is not a docker config document: %v", err)
	}
	if _, ok := payload.Auths["quay.io"]; !ok {
		t.Errorf("projected payload does not carry the legacy registry entry: %v", payload.Auths)
	}
}

// TestExistingProjectedSecretIsReused asserts an unchanged copy is never rewritten,
// so admitting many Pods with one profile does not churn one Secret.
func TestExistingProjectedSecretIsReused(t *testing.T) {
	source := dockerConfigSecret(workloadPullSecret, map[string]map[string]string{
		"quay.io": {"username": "profile-puller", "password": workloadPassword},
	})
	c := testClient(t, environmentWithPullSecret(t, workloadPullSecret), source)
	mutator := testMutator(t, c)
	pod := testPod()

	first := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
	if !first.Allowed {
		t.Fatalf("first admission denied: %s", messageOf(first))
	}
	projectedName := projectedPullSecretName(t, applyPatch(t, pod, first.Patch))

	before := &corev1.Secret{}
	key := types.NamespacedName{Namespace: workloadNamespace, Name: projectedName}
	if err := c.Get(t.Context(), key, before); err != nil {
		t.Fatalf("projected Secret %s: %v", key, err)
	}

	second := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
	if !second.Allowed {
		t.Fatalf("second admission denied: %s", messageOf(second))
	}
	after := &corev1.Secret{}
	if err := c.Get(t.Context(), key, after); err != nil {
		t.Fatalf("projected Secret %s: %v", key, err)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("projected Secret was rewritten: resourceVersion %s -> %s",
			before.ResourceVersion, after.ResourceVersion)
	}
	if string(second.Patch) != string(first.Patch) {
		t.Errorf("reused copy changed the admission patch")
	}
}

// TestDryRunProjectsNothingButDecidesTheSame asserts a dry-run request never
// creates the Secret and returns the same patch as a real request.
func TestDryRunProjectsNothingButDecidesTheSame(t *testing.T) {
	source := func() *corev1.Secret {
		return dockerConfigSecret(workloadPullSecret, map[string]map[string]string{
			"quay.io": {"username": "profile-puller", "password": workloadPassword},
		})
	}
	pod := testPod()
	request := func(dryRun bool) *admissionv1.AdmissionRequest {
		return admissionRequest(t, admissionv1.Create, pod, func(r *admissionv1.AdmissionRequest) {
			r.DryRun = &dryRun
		})
	}

	realClient := testClient(t, environmentWithPullSecret(t, workloadPullSecret), source())
	real := testMutator(t, realClient).Admit(t.Context(), request(false))
	if !real.Allowed {
		t.Fatalf("real admission denied: %s", messageOf(real))
	}
	dryClient := testClient(t, environmentWithPullSecret(t, workloadPullSecret), source())
	dry := testMutator(t, dryClient).Admit(t.Context(), request(true))
	if !dry.Allowed {
		t.Fatalf("dry-run admission denied: %s", messageOf(dry))
	}

	if string(dry.Patch) != string(real.Patch) {
		t.Errorf("dry-run patch differs from the real patch:\n%s\n%s", dry.Patch, real.Patch)
	}
	projectedName := projectedPullSecretName(t, applyPatch(t, pod, dry.Patch))
	secret := &corev1.Secret{}
	err := dryClient.Get(t.Context(), types.NamespacedName{Namespace: workloadNamespace, Name: projectedName}, secret)
	if !apierrors.IsNotFound(err) {
		t.Errorf("dry-run created the projected Secret %s (err = %v)", projectedName, err)
	}
}

// TestNoPullSecretsConfiguredProjectsNothing asserts a public profile needs no copy
// and the Pod's own imagePullSecrets are untouched.
func TestNoPullSecretsConfiguredProjectsNothing(t *testing.T) {
	c := testClient(t, testEnvironment(t))
	pod := testPod()

	resp := testMutator(t, c).Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
	if !resp.Allowed {
		t.Fatalf("GPU Pod denied: %s", messageOf(resp))
	}
	mutated := applyPatch(t, pod, resp.Patch)
	if len(mutated.Spec.ImagePullSecrets) != 1 || mutated.Spec.ImagePullSecrets[0].Name != "workload-owned-pull" {
		t.Errorf("imagePullSecrets = %v, want only the workload's own secret", mutated.Spec.ImagePullSecrets)
	}
	secrets := &corev1.SecretList{}
	if err := c.List(t.Context(), secrets); err != nil {
		t.Fatalf("list Secrets: %v", err)
	}
	for _, secret := range secrets.Items {
		if strings.HasPrefix(secret.Name, names.PullSecretPrefix) {
			t.Errorf("a projected Secret %s was created without configured pull secrets", secret.Name)
		}
	}
}

// TestConflictingPullSecretsDenied asserts a merge that would depend on ordering is
// reported instead of resolved silently, and that no credential appears in the
// message.
func TestConflictingPullSecretsDenied(t *testing.T) {
	const secondSecret = "second-registry"
	env := testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Spec.ImagePullSecrets = []vcrv1alpha1.LocalSecretReference{
			{Name: workloadPullSecret}, {Name: secondSecret},
		}
	})
	c := testClient(t, env,
		dockerConfigSecret(workloadPullSecret, map[string]map[string]string{
			"quay.io": {"username": "first", "password": "first-value"},
		}),
		dockerConfigSecret(secondSecret, map[string]map[string]string{
			"quay.io": {"username": "second", "password": "second-value"},
		}),
	)

	resp := testMutator(t, c).Admit(t.Context(), admissionRequest(t, admissionv1.Create, testPod()))

	if resp.Allowed {
		t.Fatal("GPU Pod admitted with conflicting pull credentials")
	}
	message := messageOf(resp)
	for _, want := range []string{workloadPullSecret, secondSecret, "quay.io"} {
		if !strings.Contains(message, want) {
			t.Errorf("denial message %q does not mention %q", message, want)
		}
	}
	for _, value := range []string{"first-value", "second-value"} {
		if strings.Contains(message, value) {
			t.Errorf("denial message contains the pull credential %q", value)
		}
	}
}

// TestMissingPullSecretDeniedAsOperatorProblem asserts a referenced Secret that does
// not exist is reported as a cluster configuration problem, not as a workload
// problem.
func TestMissingPullSecretDeniedAsOperatorProblem(t *testing.T) {
	c := testClient(t, environmentWithPullSecret(t, workloadPullSecret))

	resp := testMutator(t, c).Admit(t.Context(), admissionRequest(t, admissionv1.Create, testPod()))

	if resp.Allowed {
		t.Fatal("GPU Pod admitted without the referenced pull Secret")
	}
	message := messageOf(resp)
	if !strings.Contains(message, "operator problem") {
		t.Errorf("denial message %q does not identify an operator problem", message)
	}
	if !strings.Contains(message, workloadPullSecret) {
		t.Errorf("denial message %q does not name the missing Secret", message)
	}
}

// TestMalformedPullSecretDeniedWithoutPayload asserts a Secret that is not a docker
// config document is reported by key only.
func TestMalformedPullSecretDeniedWithoutPayload(t *testing.T) {
	const marker = "must-not-be-echoed"
	broken := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: workloadPullSecret, Namespace: operatorNamespace},
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(
			`{"auths": {"quay.io": ` + marker)},
	}
	c := testClient(t, environmentWithPullSecret(t, workloadPullSecret), broken)

	resp := testMutator(t, c).Admit(t.Context(), admissionRequest(t, admissionv1.Create, testPod()))

	if resp.Allowed {
		t.Fatal("GPU Pod admitted with an unreadable pull Secret")
	}
	message := messageOf(resp)
	if !strings.Contains(message, corev1.DockerConfigJsonKey) {
		t.Errorf("denial message %q does not name the expected key", message)
	}
	if strings.Contains(message, marker) {
		t.Errorf("denial message echoes the Secret payload")
	}
}

// TestCredentialRotationUpdatesTheSameCopy asserts the projected name identifies the
// source Secrets, so a rotated credential rewrites one copy instead of accumulating
// copies every Pod still points at.
func TestCredentialRotationUpdatesTheSameCopy(t *testing.T) {
	source := dockerConfigSecret(workloadPullSecret, map[string]map[string]string{
		"quay.io": {"username": "profile-puller", "password": "first-value"},
	})
	c := testClient(t, environmentWithPullSecret(t, workloadPullSecret), source)
	mutator := testMutator(t, c)
	pod := testPod()

	first := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
	if !first.Allowed {
		t.Fatalf("first admission denied: %s", messageOf(first))
	}
	projectedName := projectedPullSecretName(t, applyPatch(t, pod, first.Patch))

	rotated := dockerConfigSecret(workloadPullSecret, map[string]map[string]string{
		"quay.io": {"username": "profile-puller", "password": "rotated-value"},
	})
	rotated.ResourceVersion = source.ResourceVersion
	if err := c.Update(t.Context(), rotated); err != nil {
		t.Fatalf("rotate source Secret: %v", err)
	}

	second := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
	if !second.Allowed {
		t.Fatalf("second admission denied: %s", messageOf(second))
	}
	if got := projectedPullSecretName(t, applyPatch(t, pod, second.Patch)); got != projectedName {
		t.Errorf("projected name changed after a credential rotation: %q -> %q", projectedName, got)
	}

	projected := &corev1.Secret{}
	key := types.NamespacedName{Namespace: workloadNamespace, Name: projectedName}
	if err := c.Get(t.Context(), key, projected); err != nil {
		t.Fatalf("projected Secret %s: %v", key, err)
	}
	if !strings.Contains(string(projected.Data[corev1.DockerConfigJsonKey]), "rotated-value") {
		t.Errorf("projected payload was not updated after the rotation")
	}
}

// TestProjectedNameIgnoresSourceOrder asserts the same credential set referenced in a
// different order produces one copy, not two.
func TestProjectedNameIgnoresSourceOrder(t *testing.T) {
	const secondSecret = "second-registry"
	first := testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Spec.ImagePullSecrets = []vcrv1alpha1.LocalSecretReference{
			{Name: workloadPullSecret}, {Name: secondSecret},
		}
	})
	second := testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Spec.ImagePullSecrets = []vcrv1alpha1.LocalSecretReference{
			{Name: secondSecret}, {Name: workloadPullSecret},
		}
	})
	sources := func() []client.Object {
		return []client.Object{
			dockerConfigSecret(workloadPullSecret, map[string]map[string]string{
				"quay.io": {"username": "first", "password": "first-value"},
			}),
			dockerConfigSecret(secondSecret, map[string]map[string]string{
				"ghcr.io": {"username": "second", "password": "second-value"},
			}),
		}
	}
	pod := testPod()

	admit := func(env *vcrv1alpha1.VcrGpuCluster) string {
		objects := append([]client.Object{env}, sources()...)
		c := testClient(t, objects...)
		resp := testMutator(t, c).Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
		if !resp.Allowed {
			t.Fatalf("admission denied: %s", messageOf(resp))
		}
		return projectedPullSecretName(t, applyPatch(t, pod, resp.Patch))
	}

	if a, b := admit(first), admit(second); a != b {
		t.Errorf("source order changed the projected name: %q vs %q", a, b)
	}
}

// TestForeignSecretWithTheProjectedNameIsNotOverwritten asserts admission refuses to
// take over a Secret it does not manage, instead of silently replacing its data.
func TestForeignSecretWithTheProjectedNameIsNotOverwritten(t *testing.T) {
	pod := testPod()
	// A dry-run admission reveals the name the operator would use without creating
	// anything.
	dryRun := true
	dryClient := testClient(t,
		environmentWithPullSecret(t, workloadPullSecret),
		dockerConfigSecret(workloadPullSecret, map[string]map[string]string{
			"quay.io": {"username": "profile-puller", "password": "value"},
		}))
	dry := testMutator(t, dryClient).Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod,
		func(r *admissionv1.AdmissionRequest) { r.DryRun = &dryRun }))
	if !dry.Allowed {
		t.Fatalf("dry-run admission denied: %s", messageOf(dry))
	}
	name := projectedPullSecretName(t, applyPatch(t, pod, dry.Patch))

	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: workloadNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"key": []byte("value")},
	}
	c := testClient(t,
		environmentWithPullSecret(t, workloadPullSecret),
		dockerConfigSecret(workloadPullSecret, map[string]map[string]string{
			"quay.io": {"username": "profile-puller", "password": "value"},
		}),
		foreign,
	)

	resp := testMutator(t, c).Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
	if resp.Allowed {
		t.Fatal("admission overwrote a Secret it does not manage")
	}
	message := messageOf(resp)
	if !strings.Contains(message, name) || !strings.Contains(message, "operator problem") {
		t.Errorf("denial message %q does not explain the foreign Secret", message)
	}

	after := &corev1.Secret{}
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: workloadNamespace, Name: name}, after); err != nil {
		t.Fatalf("read foreign Secret: %v", err)
	}
	if string(after.Data["key"]) != "value" {
		t.Errorf("foreign Secret data was modified")
	}
}

// TestUnrelatedRegistrySecretIsNotProjected asserts the environment-wide list is a
// candidate list: a Secret whose docker config carries no credential for the
// selected image's registry is not copied into the workload namespace.
func TestUnrelatedRegistrySecretIsNotProjected(t *testing.T) {
	const unrelated = "unrelated-registry"
	const unrelatedPassword = "unrelated-s3cret-value"
	env := testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Spec.ImagePullSecrets = []vcrv1alpha1.LocalSecretReference{
			{Name: workloadPullSecret}, {Name: unrelated},
		}
	})
	c := testClient(t, env,
		dockerConfigSecret(workloadPullSecret, map[string]map[string]string{
			"quay.io": {"username": "profile-puller", "password": workloadPassword},
		}),
		dockerConfigSecret(unrelated, map[string]map[string]string{
			"ghcr.io": {"username": "other", "password": unrelatedPassword},
		}),
	)
	pod := testPod()

	resp := testMutator(t, c).Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
	if !resp.Allowed {
		t.Fatalf("GPU Pod denied: %s", messageOf(resp))
	}
	projected := &corev1.Secret{}
	key := types.NamespacedName{Namespace: workloadNamespace, Name: projectedPullSecretName(t, applyPatch(t, pod, resp.Patch))}
	if err := c.Get(t.Context(), key, projected); err != nil {
		t.Fatalf("projected Secret %s: %v", key, err)
	}
	payload := string(projected.Data[corev1.DockerConfigJsonKey])
	if strings.Contains(payload, unrelatedPassword) || strings.Contains(payload, "ghcr.io") {
		t.Errorf("projected payload carries a credential for a registry the profile image does not use: %s", payload)
	}
	if !strings.Contains(payload, workloadPassword) {
		t.Errorf("projected payload does not carry the matching registry credential: %s", payload)
	}
}

// TestOnlyMatchingRegistryEntriesAreProjected asserts a single Secret that carries
// credentials for several registries contributes only the entry that applies to the
// selected image.
func TestOnlyMatchingRegistryEntriesAreProjected(t *testing.T) {
	const unrelatedPassword = "unrelated-s3cret-value"
	source := dockerConfigSecret(workloadPullSecret, map[string]map[string]string{
		"quay.io": {"username": "profile-puller", "password": workloadPassword},
		"ghcr.io": {"username": "other", "password": unrelatedPassword},
	})
	c := testClient(t, environmentWithPullSecret(t, workloadPullSecret), source)
	pod := testPod()

	resp := testMutator(t, c).Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
	if !resp.Allowed {
		t.Fatalf("GPU Pod denied: %s", messageOf(resp))
	}
	projected := &corev1.Secret{}
	key := types.NamespacedName{Namespace: workloadNamespace, Name: projectedPullSecretName(t, applyPatch(t, pod, resp.Patch))}
	if err := c.Get(t.Context(), key, projected); err != nil {
		t.Fatalf("projected Secret %s: %v", key, err)
	}
	payload := string(projected.Data[corev1.DockerConfigJsonKey])
	if strings.Contains(payload, unrelatedPassword) {
		t.Errorf("projected payload carries an entry for another registry: %s", payload)
	}
	if !strings.Contains(payload, workloadPassword) {
		t.Errorf("projected payload does not carry the matching registry credential: %s", payload)
	}
}

// TestNoMatchingRegistryProjectsNothing asserts that when no configured Secret can
// authenticate the profile image's registry, admission lets the pull proceed
// anonymously instead of copying unrelated credentials.
func TestNoMatchingRegistryProjectsNothing(t *testing.T) {
	env := testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Spec.ImagePullSecrets = []vcrv1alpha1.LocalSecretReference{{Name: workloadPullSecret}}
	})
	c := testClient(t, env, dockerConfigSecret(workloadPullSecret, map[string]map[string]string{
		"ghcr.io": {"username": "other", "password": "other-value"},
	}))
	pod := testPod()

	resp := testMutator(t, c).Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
	if !resp.Allowed {
		t.Fatalf("GPU Pod denied: %s", messageOf(resp))
	}
	mutated := applyPatch(t, pod, resp.Patch)
	if len(mutated.Spec.ImagePullSecrets) != 1 || mutated.Spec.ImagePullSecrets[0].Name != "workload-owned-pull" {
		t.Errorf("imagePullSecrets = %v, want only the workload's own secret", mutated.Spec.ImagePullSecrets)
	}
	secrets := &corev1.SecretList{}
	if err := c.List(t.Context(), secrets); err != nil {
		t.Fatalf("list Secrets: %v", err)
	}
	for _, secret := range secrets.Items {
		if strings.HasPrefix(secret.Name, names.PullSecretPrefix) {
			t.Errorf("a projected Secret %s was created although no credential covers the profile image's registry", secret.Name)
		}
	}
}

// TestProjectedNameFollowsTheProjectedCredentials asserts the copy is named after the
// sources that actually contributed a credential for the selected image, not after
// every configured candidate. Otherwise two profiles served from different registries
// would share one copy, and whichever Pod was admitted last would decide the
// credential both of them reference.
func TestProjectedNameFollowsTheProjectedCredentials(t *testing.T) {
	const secondSecret = "second-registry"
	const secondImage = "ghcr.io/example/profiles:qwen3-0.6b-h200-tp1-bf16"

	env := testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Spec.ImagePullSecrets = []vcrv1alpha1.LocalSecretReference{
			{Name: workloadPullSecret}, {Name: secondSecret},
		}
	})
	sources := []client.Object{
		dockerConfigSecret(workloadPullSecret, map[string]map[string]string{
			"quay.io": {"username": "quay", "password": "quay-value"},
		}),
		dockerConfigSecret(secondSecret, map[string]map[string]string{
			"ghcr.io": {"username": "ghcr", "password": "ghcr-value"},
		}),
	}
	pod := testPod()

	admit := func(profileImage string) string {
		env := env.DeepCopy()
		env.Spec.Profiles[0].Image = profileImage
		env.Status.Profiles[0].Image = profileImage
		objects := append([]client.Object{env}, sources...)
		c := testClient(t, objects...)
		resp := testMutator(t, c).Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
		if !resp.Allowed {
			t.Fatalf("admission denied for %s: %s", profileImage, messageOf(resp))
		}
		return projectedPullSecretName(t, applyPatch(t, pod, resp.Patch))
	}

	quayCopy := admit(testImage)
	ghcrCopy := admit(secondImage)
	if quayCopy == ghcrCopy {
		t.Errorf("both profiles reference one projected Secret %q although they are served from different registries", quayCopy)
	}
}

// TestProfilePullSecretIsFoundThroughTheProfileRegistry is the regression test for
// a real failure: the override was keyed by the spec's spelling while the
// translator looks it up by the reference resolved against spec.profileRegistry. A
// cluster that names a profile registry therefore lost the override and fell back
// to the environment-wide candidates, pulling the private profile anonymously.
func TestProfilePullSecretIsFoundThroughTheProfileRegistry(t *testing.T) {
	const registry = "ghcr.io/acme/vllm-vcr-profiles"
	env := testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		e.Spec.ImagePullSecrets = []vcrv1alpha1.LocalSecretReference{{Name: workloadPullSecret}}
		// The spec names what to look for inside the registry; status.profiles, and
		// therefore the selected image, carries the resolved reference.
		e.Spec.ProfileRegistry = registry
		e.Spec.Profiles[0].Image = "qwen3-0.6b-h200-tp1-bf16"
		e.Spec.Profiles[0].PullSecretName = profilePullSecret
		for i := range e.Status.Profiles {
			e.Status.Profiles[i].Image = registry + ":qwen3-0.6b-h200-tp1-bf16"
		}
	})
	c := testClient(t, env,
		dockerConfigSecret(workloadPullSecret, map[string]map[string]string{
			"quay.io": {"username": "wide", "password": "wide-value"},
		}),
		dockerConfigSecret(profilePullSecret, map[string]map[string]string{
			"ghcr.io": {"username": "profile", "password": "profile-value"},
		}),
	)
	mutator := testMutator(t, c)
	pod := testPod()

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
	if !resp.Allowed {
		t.Fatalf("GPU Pod denied: %s", messageOf(resp))
	}
	mutated := applyPatch(t, pod, resp.Patch)

	projected := &corev1.Secret{}
	key := types.NamespacedName{Namespace: workloadNamespace, Name: projectedPullSecretName(t, mutated)}
	if err := c.Get(t.Context(), key, projected); err != nil {
		t.Fatalf("projected Secret %s: %v", key, err)
	}
	var payload struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	if err := json.Unmarshal(projected.Data[corev1.DockerConfigJsonKey], &payload); err != nil {
		t.Fatalf("projected payload is not a docker config document: %v", err)
	}
	if _, ok := payload.Auths["ghcr.io"]; !ok {
		t.Errorf("projected payload does not carry the profile's registry entry: %v", payload.Auths)
	}
}
