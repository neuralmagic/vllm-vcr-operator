package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	vcrv1alpha1 "github.com/opendatahub-io/vllm-vcr-operator/api/v1alpha1"
)

// dockerConfigDocument is the subset of a .dockerconfigjson document that
// carries pull credentials.
type dockerConfigDocument struct {
	Auths map[string]dockerAuthEntry `json:"auths"`
}

// dockerAuthEntry is one credential entry of a docker config document.
type dockerAuthEntry struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	Auth          string `json:"auth,omitempty"`
	IdentityToken string `json:"identityToken,omitempty"`
	RegistryToken string `json:"registryToken,omitempty"`
}

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
	if authenticator, ok := c[normalizeRegistryHost(host)]; ok {
		return authenticator, nil
	}
	return authn.Anonymous, nil
}

// pullSecretNames returns the Secret names that hold credentials for one profile
// image. A profile-level secret replaces the environment-wide list rather than
// adding to it, so a single profile can pull from a private registry without
// changing the credentials every other profile uses.
func pullSecretNames(env *vcrv1alpha1.SimulationEnvironment, source vcrv1alpha1.ProfileSource) []string {
	if source.PullSecretName != "" {
		return []string{source.PullSecretName}
	}
	names := make([]string, 0, len(env.Spec.ImagePullSecrets))
	for _, reference := range env.Spec.ImagePullSecrets {
		names = append(names, reference.Name)
	}
	return names
}

// pullCredentials reads the pull Secrets that apply to one profile image.
//
// Errors name the Secret and the expected key, never a value: a Secret payload
// must not reach status, conditions, events, or logs.
func (r *SimulationEnvironmentReconciler) pullCredentials(
	ctx context.Context,
	env *vcrv1alpha1.SimulationEnvironment,
	source vcrv1alpha1.ProfileSource,
) (registryCredentials, error) {
	credentials := registryCredentials{}
	for _, secretName := range pullSecretNames(env, source) {
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
			credentials[host] = authenticator
		}
	}
	return credentials, nil
}

// secretAuthenticators converts a docker config pull Secret into per-host
// authenticators.
func secretAuthenticators(secret *corev1.Secret) (registryCredentials, error) {
	if raw, ok := secret.Data[corev1.DockerConfigJsonKey]; ok {
		var document dockerConfigDocument
		if err := json.Unmarshal(raw, &document); err != nil {
			// The decoder error is deliberately not wrapped: it can quote bytes of
			// the document, which is credential material.
			return nil, fmt.Errorf(
				"pull Secret %s/%s: key %q is not a docker config JSON document",
				secret.Namespace, secret.Name, corev1.DockerConfigJsonKey)
		}
		return authenticatorsFor(secret, document.Auths)
	}
	if raw, ok := secret.Data[corev1.DockerConfigKey]; ok {
		var legacy map[string]dockerAuthEntry
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return nil, fmt.Errorf(
				"pull Secret %s/%s: key %q is not a docker config JSON document",
				secret.Namespace, secret.Name, corev1.DockerConfigKey)
		}
		return authenticatorsFor(secret, legacy)
	}
	return nil, fmt.Errorf(
		"pull Secret %s/%s: neither %q nor %q is present: only docker config pull Secrets can be used",
		secret.Namespace, secret.Name, corev1.DockerConfigJsonKey, corev1.DockerConfigKey)
}

// authenticatorsFor converts the auths section of a docker config document.
func authenticatorsFor(secret *corev1.Secret, auths map[string]dockerAuthEntry) (registryCredentials, error) {
	credentials := make(registryCredentials, len(auths))
	for host, entry := range auths {
		authenticator, err := authenticatorFor(entry)
		if err != nil {
			return nil, fmt.Errorf("pull Secret %s/%s: registry %q: %w",
				secret.Namespace, secret.Name, normalizeRegistryHost(host), err)
		}
		credentials[normalizeRegistryHost(host)] = authenticator
	}
	return credentials, nil
}

// authenticatorFor converts one docker config entry into an authenticator. The
// documented "auth" field is a base64 "user:password" pair that some clients emit
// instead of the separate fields.
func authenticatorFor(entry dockerAuthEntry) (authn.Authenticator, error) {
	config := authn.AuthConfig{
		Username:      entry.Username,
		Password:      entry.Password,
		IdentityToken: entry.IdentityToken,
		RegistryToken: entry.RegistryToken,
	}
	if config.Username == "" && config.Password == "" && entry.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err != nil {
			// The decoder error is not wrapped: it can quote the encoded value.
			return nil, fmt.Errorf("the %q field is not base64-encoded", "auth")
		}
		username, password, found := strings.Cut(string(decoded), ":")
		if !found {
			return nil, fmt.Errorf("the %q field is not a base64-encoded user:password pair", "auth")
		}
		config.Username = username
		config.Password = password
	}
	if config.Username == "" && config.Password == "" && config.IdentityToken == "" && config.RegistryToken == "" {
		return nil, fmt.Errorf("no credential is present: set one of username/password, auth, identityToken, or registryToken")
	}
	return authn.FromConfig(config), nil
}

// normalizeRegistryHost reduces a docker config key or a registry host to the
// form go-containerregistry reports in Resource.RegistryStr().
func normalizeRegistryHost(host string) string {
	normalized := strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	normalized = strings.TrimSuffix(normalized, "/")
	normalized = strings.TrimSuffix(normalized, "/v1")
	normalized = strings.TrimSuffix(normalized, "/v2")
	// Docker Hub is spelled several ways; the resolver reports the index host.
	if normalized == "docker.io" {
		return "index.docker.io"
	}
	return normalized
}
