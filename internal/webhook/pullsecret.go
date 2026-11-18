package webhook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
)

const (
	// AppliedToAnnotation records which workload a projected pull secret was
	// created for. The copy carries no owner reference: nothing namespace-local
	// owns it (the workload's ServiceAccount or Deployment lives in the
	// controller's manifest), so labels and this annotation identify it instead.
	AppliedToAnnotation = names.AnnotationPrefix + "applied-to"

	// AppliedAtAnnotation records when the projected copy was created.
	AppliedAtAnnotation = names.AnnotationPrefix + "applied-at"
)

// projectedSecretBytes is how much of the source digest is used in the projected
// Secret name. 64 bits keep names short while making an accidental collision
// irrelevant; a copy is compared byte for byte before it is reused.
const projectedSecretBytes = 8

// projectPullSecrets copies the pull credentials of the selected profile into the
// workload namespace and returns the name of the copy to reference.
//
// Sources are Secret names in the operator namespace. Their docker config payloads
// are merged into one document, because a Pod may only reference whole Secrets and
// a profile may need credentials for more than one registry. When no source is
// configured, nothing is projected and the Pod keeps only its own pull secrets.
//
// The copy is named after the set of sources, not after its content: rotating a
// credential then updates one copy instead of leaving the old one behind, and every
// Pod that needs the same credentials references the same Secret. An existing copy
// is only written when its payload differs.
//
// The payload never leaves this function: the returned name and every error name
// Secrets and registry hosts only. dryRun skips the write but still reads and merges
// the sources, so a dry-run request fails or succeeds for exactly the same reasons
// as a real one and produces the same response.
func (m *PodMutator) projectPullSecrets(ctx context.Context, pod *corev1.Pod, sources []string, dryRun bool) (string, error) {
	if len(sources) == 0 {
		return "", nil
	}
	payload, err := m.mergePullSecrets(ctx, sources)
	if err != nil {
		return "", err
	}
	name := names.PullSecretPrefix + hex.EncodeToString(sourceDigest(sources))
	if dryRun {
		return name, nil
	}
	if err := m.ensurePullSecret(ctx, pod, name, payload); err != nil {
		return "", err
	}
	return name, nil
}

// sourceDigest names the projected copy after the set of source Secrets.
//
// The sources are sorted first: the same set spelled in a different spec order is
// the same credential set and must not produce a second copy.
func sourceDigest(sources []string) []byte {
	ordered := append([]string(nil), sources...)
	sort.Strings(ordered)
	sum := sha256.Sum256([]byte(strings.Join(ordered, "\x00")))
	return sum[:projectedSecretBytes]
}

// mergePullSecrets merges the docker config payloads of the source Secrets.
//
// The merge is strict: two sources that carry credentials for the same registry
// would make the copy depend on ordering, so that configuration is reported rather
// than resolved by precedence. Output is sorted by registry host, so an unchanged
// set of sources always produces the same payload.
func (m *PodMutator) mergePullSecrets(ctx context.Context, sources []string) ([]byte, error) {
	auths := make(map[string]json.RawMessage)
	origin := make(map[string]string, len(sources))

	for _, source := range sources {
		key := types.NamespacedName{Namespace: m.operatorNamespace, Name: source}
		secret := &corev1.Secret{}
		if err := m.reader.Get(ctx, key, secret); err != nil {
			// The API client error names the resource; it never carries its data.
			return nil, fmt.Errorf("pull Secret %s cannot be read: %w", key, err)
		}
		entries, err := dockerConfigAuths(secret)
		if err != nil {
			return nil, err
		}
		hosts := make([]string, 0, len(entries))
		for host := range entries {
			hosts = append(hosts, host)
		}
		sort.Strings(hosts)
		for _, host := range hosts {
			if prior, taken := origin[host]; taken {
				return nil, fmt.Errorf(
					"pull Secrets %s and %s both carry credentials for registry %q: the merged pull Secret would be ambiguous",
					prior, key, host)
			}
			origin[host] = key.String()
			auths[host] = entries[host]
		}
	}

	document := struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}{Auths: auths}
	payload, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("the merged pull Secret cannot be encoded: %w", err)
	}
	return payload, nil
}

// dockerConfigAuths returns the registry entries of one pull Secret.
//
// Both the current "kubernetes.io/dockerconfigjson" key and the legacy
// ".dockercfg" key are accepted, exactly as a kubelet accepts them. Entries are
// kept as raw JSON so a credential field this operator does not model survives the
// merge untouched.
func dockerConfigAuths(secret *corev1.Secret) (map[string]json.RawMessage, error) {
	if raw, ok := secret.Data[corev1.DockerConfigJsonKey]; ok {
		var document struct {
			Auths map[string]json.RawMessage `json:"auths"`
		}
		if err := json.Unmarshal(raw, &document); err != nil {
			// The decoder error can quote bytes of the document, which is
			// credential material, so it is deliberately not wrapped.
			return nil, fmt.Errorf("pull Secret %s/%s: key %q is not a docker config JSON document",
				secret.Namespace, secret.Name, corev1.DockerConfigJsonKey)
		}
		return document.Auths, nil
	}
	if raw, ok := secret.Data[corev1.DockerConfigKey]; ok {
		var legacy map[string]json.RawMessage
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return nil, fmt.Errorf("pull Secret %s/%s: key %q is not a docker config JSON document",
				secret.Namespace, secret.Name, corev1.DockerConfigKey)
		}
		return legacy, nil
	}
	return nil, fmt.Errorf("pull Secret %s/%s: neither %q nor %q is present: only docker config pull Secrets can be projected",
		secret.Namespace, secret.Name, corev1.DockerConfigJsonKey, corev1.DockerConfigKey)
}

// ensurePullSecret creates the projected copy, or reuses an existing one whose
// payload already matches.
//
// An unchanged copy is never rewritten: the workload namespace may hold many Pods
// sharing one profile, and each admission must not bump the same Secret.
func (m *PodMutator) ensurePullSecret(ctx context.Context, pod *corev1.Pod, name string, payload []byte) error {
	key := types.NamespacedName{Namespace: pod.Namespace, Name: name}
	existing := &corev1.Secret{}
	err := m.reader.Get(ctx, key, existing)
	switch {
	case apierrors.IsNotFound(err):
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: pod.Namespace,
				Labels: map[string]string{
					names.ManagedByLabel: names.OperatorName,
				},
				Annotations: map[string]string{
					AppliedToAnnotation: pod.Namespace + "/" + pod.Name,
					AppliedAtAnnotation: m.now().UTC().Format(time.RFC3339),
				},
			},
			Type: corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{corev1.DockerConfigJsonKey: payload},
		}
		if err := m.client.Create(ctx, secret); err != nil {
			return fmt.Errorf("project pull Secret %s: %w", key, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("projected pull Secret %s cannot be read: %w", key, err)
	}

	if existing.Labels[names.ManagedByLabel] != names.OperatorName {
		// The name carries the operator prefix, so this is either a copy written
		// without the label or a collision. Overwriting someone else's Secret is
		// never the answer.
		return fmt.Errorf(
			"a Secret named %s already exists but is not managed by %s: remove it or change the pull Secret names",
			key, names.OperatorName)
	}

	if existing.Type == corev1.SecretTypeDockerConfigJson &&
		bytes.Equal(existing.Data[corev1.DockerConfigJsonKey], payload) {
		// Same content: reuse the copy without a write, so a shared profile does
		// not cause an update per admitted Pod.
		return nil
	}
	existing.Type = corev1.SecretTypeDockerConfigJson
	if existing.Data == nil {
		existing.Data = map[string][]byte{}
	}
	existing.Data[corev1.DockerConfigJsonKey] = payload
	if err := m.client.Update(ctx, existing); err != nil {
		return fmt.Errorf("update projected pull Secret %s: %w", key, err)
	}
	return nil
}
