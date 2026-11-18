// Package credentials owns the docker config pull-secret document format and the
// registry-host identity that credentials are keyed by.
//
// Two operator packages read docker config pull Secrets: internal/controller to
// authenticate registry pulls during reconciliation, and internal/webhook to
// project only the credentials a selected profile image needs. They used to carry
// two parsers with two key sets and two error messages; this package is the single
// owner of both, and of the invariant that a credential value never escapes:
//
//   - a decoder error is never wrapped, because encoding/json quotes the offending
//     bytes and those bytes are credential material;
//   - every error names a Secret, a key, or a registry host, never a value.
package credentials

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	corev1 "k8s.io/api/core/v1"
)

// Auths returns the registry entries of one pull Secret, keyed by the registry
// host exactly as the document spells it.
//
// Both the current "kubernetes.io/dockerconfigjson" key and the legacy
// ".dockercfg" key are accepted, exactly as a kubelet accepts them. Entries are
// returned as raw JSON so that a credential field this operator does not model
// survives a merge untouched. Callers that need a decoded entry use DecodeEntry.
func Auths(secret *corev1.Secret) (map[string]json.RawMessage, error) {
	if raw, ok := secret.Data[corev1.DockerConfigJsonKey]; ok {
		var document struct {
			Auths map[string]json.RawMessage `json:"auths"`
		}
		if err := json.Unmarshal(raw, &document); err != nil {
			// The decoder error is deliberately not wrapped: it can quote bytes of
			// the document, which is credential material.
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
	return nil, fmt.Errorf("pull Secret %s/%s: neither %q nor %q is present: only docker config pull Secrets can be used",
		secret.Namespace, secret.Name, corev1.DockerConfigJsonKey, corev1.DockerConfigKey)
}

// Entry is one credential entry of a docker config document. Its fields are the
// documented docker config spelling; Auth is the legacy base64 "user:password"
// pair some clients emit instead of the separate fields.
type Entry struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	Auth          string `json:"auth,omitempty"`
	IdentityToken string `json:"identityToken,omitempty"`
	RegistryToken string `json:"registryToken,omitempty"`
}

// DecodeEntry decodes one raw docker config entry.
func DecodeEntry(raw json.RawMessage) (Entry, error) {
	var entry Entry
	if err := json.Unmarshal(raw, &entry); err != nil {
		// Not wrapped for the same reason as Auths: the error can quote the value.
		return Entry{}, fmt.Errorf("the registry entry is not a docker config credential object")
	}
	return entry, nil
}

// AuthConfig converts the entry into a go-containerregistry auth configuration,
// decoding the legacy "auth" field when the separate fields are absent.
func (e Entry) AuthConfig() (authn.AuthConfig, error) {
	config := authn.AuthConfig{
		Username:      e.Username,
		Password:      e.Password,
		IdentityToken: e.IdentityToken,
		RegistryToken: e.RegistryToken,
	}
	if config.Username == "" && config.Password == "" && e.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(e.Auth)
		if err != nil {
			// The decoder error is not wrapped: it can quote the encoded value.
			return authn.AuthConfig{}, fmt.Errorf("the %q field is not base64-encoded", "auth")
		}
		username, password, found := strings.Cut(string(decoded), ":")
		if !found {
			return authn.AuthConfig{}, fmt.Errorf("the %q field is not a base64-encoded user:password pair", "auth")
		}
		config.Username = username
		config.Password = password
	}
	if config.Username == "" && config.Password == "" && config.IdentityToken == "" && config.RegistryToken == "" {
		return authn.AuthConfig{}, fmt.Errorf("no credential is present: set one of username/password, auth, identityToken, or registryToken")
	}
	return config, nil
}

// Authenticator converts the entry into a go-containerregistry authenticator.
func (e Entry) Authenticator() (authn.Authenticator, error) {
	config, err := e.AuthConfig()
	if err != nil {
		return nil, err
	}
	return authn.FromConfig(config), nil
}

// NormalizeHost reduces a docker config key or a registry host to the form
// go-containerregistry reports in Resource.RegistryStr().
func NormalizeHost(host string) string {
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

// ImageHost returns the normalized registry host of an image reference.
//
// Docker's naming rules decide what the host is: the first path component is a
// registry only when it contains a dot or a port, or is "localhost". A reference
// without one is a Docker Hub repository, and its host is index.docker.io. Tags and
// digests are stripped, so "quay.io/a/b:t" and "quay.io/a/b@sha256:…" share a host.
func ImageHost(image string) string {
	reference := strings.TrimSpace(image)
	if reference == "" {
		return ""
	}
	if i := strings.IndexByte(reference, '@'); i >= 0 {
		reference = reference[:i]
	}
	if slash := strings.LastIndexByte(reference, '/'); slash >= 0 {
		if colon := strings.LastIndexByte(reference, ':'); colon > slash {
			reference = reference[:colon]
		}
	} else if colon := strings.LastIndexByte(reference, ':'); colon > 0 {
		// A single-component reference with a tag: "vllm:v1".
		reference = reference[:colon]
	}

	host := reference
	if i := strings.IndexByte(reference, '/'); i >= 0 {
		host = reference[:i]
	}
	if host == "" || (!strings.ContainsAny(host, ".:") && host != "localhost") {
		// No registry component: Docker Hub.
		return "index.docker.io"
	}
	return NormalizeHost(host)
}
