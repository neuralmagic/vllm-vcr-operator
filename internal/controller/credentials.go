package controller

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/credentials"
	"github.com/neuralmagic/vllm-vcr-operator/internal/pullsecrets"
)

// registryCredentials maps a normalized registry host to a pull credential read
// from an operator-namespace Secret.
//
// Only registry hosts are keys, so a credential value can never be selected by
// accident, and no method returns the map for logging.
type registryCredentials map[string]authn.Authenticator

// authenticate is the registry.Options.Credentials callback. An unknown registry
// host pulls anonymously, exactly as an image pull without a credential entry
// does. The returned error names the host only.
func (c registryCredentials) authenticate(_ context.Context, host string) (authn.Authenticator, error) {
	if authenticator, ok := c[credentials.NormalizeHost(host)]; ok {
		return authenticator, nil
	}
	return authn.Anonymous, nil
}

// pullCredentials reads the pull Secrets that apply to one profile image.
//
// Errors name the Secret and the expected key, never a value: a Secret payload
// must not reach status, conditions, events, or logs.
func (r *VcrGpuClusterReconciler) pullCredentials(
	ctx context.Context,
	env *vcrv1alpha1.VcrGpuCluster,
	source vcrv1alpha1.ProfileSource,
) (registryCredentials, error) {
	credentialsByHost := registryCredentials{}
	// Which Secrets apply is decided once, in internal/plan.PullSecrets, so this
	// reconciler and admission cannot disagree about it.
	for _, secretName := range pullsecrets.FromSpec(env.Spec).For(source.Image).Names {
		secret := &corev1.Secret{}
		key := types.NamespacedName{Namespace: r.OperatorNamespace, Name: secretName}
		if err := r.Get(ctx, key, secret); err != nil {
			return nil, fmt.Errorf("pull Secret %s: %w", key, err)
		}
		authenticators, err := secretAuthenticators(secret)
		if err != nil {
			return nil, err
		}
		for host, authenticator := range authenticators {
			credentialsByHost[host] = authenticator
		}
	}
	return credentialsByHost, nil
}

// secretAuthenticators converts a docker config pull Secret into per-host
// authenticators. The document format and its errors belong to
// internal/credentials, which every pull-secret reader shares.
func secretAuthenticators(secret *corev1.Secret) (registryCredentials, error) {
	auths, err := credentials.Auths(secret)
	if err != nil {
		return nil, err
	}
	authenticators := make(registryCredentials, len(auths))
	for host, raw := range auths {
		normalized := credentials.NormalizeHost(host)
		entry, err := credentials.DecodeEntry(raw)
		if err != nil {
			return nil, fmt.Errorf("pull Secret %s/%s: registry %q: %w",
				secret.Namespace, secret.Name, normalized, err)
		}
		authenticator, err := entry.Authenticator()
		if err != nil {
			return nil, fmt.Errorf("pull Secret %s/%s: registry %q: %w",
				secret.Namespace, secret.Name, normalized, err)
		}
		authenticators[normalized] = authenticator
	}
	return authenticators, nil
}
