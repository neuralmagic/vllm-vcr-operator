// Package projection builds the operator-managed pull-secret copies that live in
// workload namespaces.
//
// It is shared by admission, which must have the copy in place before a Pod starts,
// and by the reconciler, which keeps the copies in sync afterwards: rotating an
// operator-namespace Secret must update every copy, and a copy no Pod references
// must disappear. Keeping one implementation is what makes those two agree.
package projection

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/neuralmagic/vllm-vcr-operator/internal/credentials"
	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
)

// Annotations recorded on a projected copy. They make the copy reconcilable: the
// sources and the registry filter are its inputs, so the payload can be rebuilt
// without guessing.
const (
	AppliedToAnnotation        = names.AnnotationPrefix + "applied-to"
	AppliedAtAnnotation        = names.AnnotationPrefix + "applied-at"
	AppliedSourcesAnnotation   = names.AnnotationPrefix + "applied-sources"
	AppliedImageHostAnnotation = names.AnnotationPrefix + "applied-image-host"
	// AppliedExplicitAnnotation records whether the source set was projected whole
	// (an explicit per-profile override) or filtered to the profile image's
	// registry. Rebuilding the payload needs that input, or the reconciler would
	// widen a filtered copy on the next pass.
	AppliedExplicitAnnotation = names.AnnotationPrefix + "applied-explicit"
)

// Merged is a projected payload together with the inputs that produced it.
type Merged struct {
	Payload   []byte
	Sources   []string
	ImageHost string
	Required  bool
}

// Name is the projected Secret's name: the operator prefix plus a digest of the
// contributing source names.
//
// The set, not a Pod, names the copy: many Pods share one profile, and two profiles
// served from different registries must not share a Secret whose payload then
// depends on which Pod was admitted last.
func Name(sources []string) string {
	ordered := append([]string(nil), sources...)
	sort.Strings(ordered)
	sum := sha256.Sum256([]byte(strings.Join(ordered, "\x00")))
	return names.PullSecretPrefix + hex.EncodeToString(sum[:6])
}

// Merge reads the source Secrets from the operator namespace and returns the docker
// config payload for one image.
//
// imageHost limits the result to credentials that can apply to that registry unless
// required is set, which means the caller selected the source explicitly for this
// profile and wants it projected whole. A merge that keeps no entry returns nil:
// nothing can be projected, and the pull proceeds anonymously.
//
// Two sources carrying the same registry are an error rather than a precedence
// rule, because the resulting copy would depend on ordering.
func Merge(ctx context.Context, reader client.Reader, operatorNamespace string, sources []string, imageHost string, required bool) (*Merged, error) {
	auths := make(map[string]json.RawMessage)
	origin := make(map[string]string, len(sources))
	contributing := make([]string, 0, len(sources))

	for _, source := range sources {
		key := types.NamespacedName{Namespace: operatorNamespace, Name: source}
		secret := &corev1.Secret{}
		if err := reader.Get(ctx, key, secret); err != nil {
			// The API client error names the resource; it never carries its data.
			return nil, fmt.Errorf("pull Secret %s cannot be read: %w", key, err)
		}
		entries, err := credentials.Auths(secret)
		if err != nil {
			return nil, err
		}
		hosts := make([]string, 0, len(entries))
		for host := range entries {
			hosts = append(hosts, host)
		}
		sort.Strings(hosts)

		contributed := false
		for _, host := range hosts {
			if !required && imageHost != "" && credentials.NormalizeHost(host) != imageHost {
				// A credential for another registry cannot authenticate this pull;
				// copying it would spread unrelated credentials.
				continue
			}
			if prior, taken := origin[host]; taken {
				return nil, fmt.Errorf(
					"pull Secrets %s and %s both carry credentials for registry %q: the merged pull Secret would be ambiguous",
					prior, key, host)
			}
			origin[host] = key.String()
			auths[host] = entries[host]
			contributed = true
		}
		if contributed {
			contributing = append(contributing, source)
		}
	}
	if len(auths) == 0 {
		return nil, nil
	}

	document := struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}{Auths: auths}
	payload, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("the merged pull Secret cannot be encoded: %w", err)
	}
	return &Merged{Payload: payload, Sources: contributing, ImageHost: imageHost, Required: required}, nil
}

// Ensure creates the projected copy in namespace, or updates it when its payload or
// inputs changed. An identical copy is never rewritten.
func Ensure(ctx context.Context, c client.Client, reader client.Reader, namespace, name string, merged *Merged, appliedTo string, now string) error {
	key := types.NamespacedName{Namespace: namespace, Name: name}
	existing := &corev1.Secret{}
	err := reader.Get(ctx, key, existing)
	switch {
	case err != nil && !isNotFound(err):
		return fmt.Errorf("projected pull Secret %s cannot be read: %w", key, err)
	case isNotFound(err):
		secret := &corev1.Secret{
			ObjectMeta: metav1ObjectMeta(namespace, name, merged, appliedTo, now),
			Type:       corev1.SecretTypeDockerConfigJson,
			Data:       map[string][]byte{corev1.DockerConfigJsonKey: merged.Payload},
		}
		if err := c.Create(ctx, secret); err != nil {
			return fmt.Errorf("project pull Secret %s: %w", key, err)
		}
		return nil
	}

	if existing.Labels[names.ManagedByLabel] != names.OperatorName {
		return fmt.Errorf(
			"a Secret named %s already exists but is not managed by %s: remove it or change the pull Secret names",
			key, names.OperatorName)
	}
	if existing.Type == corev1.SecretTypeDockerConfigJson &&
		existing.Data != nil && equalBytes(existing.Data[corev1.DockerConfigJsonKey], merged.Payload) &&
		existing.Annotations[AppliedSourcesAnnotation] == strings.Join(sorted(merged.Sources), ",") {
		return nil
	}

	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	existing.Annotations[AppliedSourcesAnnotation] = strings.Join(sorted(merged.Sources), ",")
	existing.Annotations[AppliedImageHostAnnotation] = merged.ImageHost
	existing.Annotations[AppliedExplicitAnnotation] = boolString(merged.Required)
	existing.Annotations[AppliedAtAnnotation] = now
	if appliedTo != "" {
		existing.Annotations[AppliedToAnnotation] = appliedTo
	}
	existing.Type = corev1.SecretTypeDockerConfigJson
	if existing.Data == nil {
		existing.Data = map[string][]byte{}
	}
	existing.Data[corev1.DockerConfigJsonKey] = merged.Payload
	if err := c.Update(ctx, existing); err != nil {
		return fmt.Errorf("update projected pull Secret %s: %w", key, err)
	}
	return nil
}

// SourcesOf returns the source names recorded on a projected copy, in the order
// they were written.
func SourcesOf(secret *corev1.Secret) []string {
	recorded := secret.Annotations[AppliedSourcesAnnotation]
	if recorded == "" {
		return nil
	}
	return strings.Split(recorded, ",")
}

// ExplicitOf reports whether the recorded inputs were projected whole.
func ExplicitOf(secret *corev1.Secret) bool {
	return secret.Annotations[AppliedExplicitAnnotation] == "true"
}

// ImageHostOf returns the registry filter recorded on the copy.
func ImageHostOf(secret *corev1.Secret) string {
	return secret.Annotations[AppliedImageHostAnnotation]
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// IsProjected reports whether the Secret is an operator-managed copy.
func IsProjected(secret *corev1.Secret) bool {
	return secret.Labels[names.ManagedByLabel] == names.OperatorName &&
		strings.HasPrefix(secret.Name, names.PullSecretPrefix)
}

func isNotFound(err error) bool { return apierrors.IsNotFound(err) }

func metav1ObjectMeta(namespace, name string, merged *Merged, appliedTo, now string) metav1.ObjectMeta {
	annotations := map[string]string{
		AppliedSourcesAnnotation:   strings.Join(sorted(merged.Sources), ","),
		AppliedImageHostAnnotation: merged.ImageHost,
		AppliedExplicitAnnotation:  boolString(merged.Required),
		AppliedAtAnnotation:        now,
	}
	if appliedTo != "" {
		annotations[AppliedToAnnotation] = appliedTo
	}
	return metav1.ObjectMeta{
		Namespace:   namespace,
		Name:        name,
		Labels:      map[string]string{names.ManagedByLabel: names.OperatorName},
		Annotations: annotations,
	}
}

func sorted(values []string) []string {
	ordered := append([]string(nil), values...)
	sort.Strings(ordered)
	return ordered
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
