package credentials_test

import (
	"encoding/base64"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/neuralmagic/vllm-vcr-operator/internal/credentials"
)

func secret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pull", Namespace: "operator"},
		Data:       data,
	}
}

// TestAuthsAcceptsBothKeyFormats asserts the shared parser reads the current and the
// legacy docker config key, exactly as a kubelet does.
func TestAuthsAcceptsBothKeyFormats(t *testing.T) {
	tests := []struct {
		name string
		key  string
		raw  string
		host string
	}{
		{
			name: "dockerconfigjson",
			key:  corev1.DockerConfigJsonKey,
			raw:  `{"auths":{"quay.io":{"username":"u","password":"p"}}}`,
			host: "quay.io",
		},
		{
			name: "legacy dockercfg",
			key:  corev1.DockerConfigKey,
			raw:  `{"quay.io":{"username":"u","password":"p"}}`,
			host: "quay.io",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			auths, err := credentials.Auths(secret(map[string][]byte{tc.key: []byte(tc.raw)}))
			if err != nil {
				t.Fatalf("Auths: %v", err)
			}
			if _, ok := auths[tc.host]; !ok {
				t.Fatalf("auths %v do not carry %q", auths, tc.host)
			}
		})
	}
}

// TestAuthsNamesKeysNotValues asserts the shared errors identify the Secret and the
// expected key without echoing any part of the document.
func TestAuthsNamesKeysNotValues(t *testing.T) {
	const marker = "must-not-be-echoed"

	t.Run("missing key", func(t *testing.T) {
		_, err := credentials.Auths(secret(map[string][]byte{"other": []byte("{}")}))
		if err == nil {
			t.Fatal("Auths accepted a Secret with no docker config key")
		}
		for _, want := range []string{"operator/pull", corev1.DockerConfigJsonKey, corev1.DockerConfigKey} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})
	t.Run("malformed document", func(t *testing.T) {
		_, err := credentials.Auths(secret(map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths": {"quay.io": ` + marker),
		}))
		if err == nil {
			t.Fatal("Auths accepted a malformed document")
		}
		if strings.Contains(err.Error(), marker) {
			t.Errorf("error %q echoes the document", err)
		}
	})
}

// TestEntryAuthConfigDecodesTheLegacyAuthField asserts the base64 "user:password"
// form is accepted, and that an entry with no credential at all is an error rather
// than an anonymous-but-configured authenticator.
func TestEntryAuthConfigDecodesTheLegacyAuthField(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("user:password"))
	config, err := (credentials.Entry{Auth: encoded}).AuthConfig()
	if err != nil {
		t.Fatalf("AuthConfig: %v", err)
	}
	if config.Username != "user" || config.Password != "password" {
		t.Errorf("decoded %q/%q, want user/password", config.Username, config.Password)
	}

	if _, err := (credentials.Entry{}).AuthConfig(); err == nil {
		t.Fatal("AuthConfig accepted an entry with no credential")
	}
	if _, err := (credentials.Entry{Auth: "not-base64!"}).AuthConfig(); err == nil {
		t.Fatal("AuthConfig accepted an auth field that is not base64")
	}
}

// TestImageHost asserts the host an image pull authenticates against, including the
// Docker Hub default and a registry with a port.
func TestImageHost(t *testing.T) {
	tests := []struct {
		image string
		want  string
	}{
		{"quay.io/opendatahub/vllm-vcr-profiles:qwen3", "quay.io"},
		{"quay.io/opendatahub/vllm-vcr-profiles@sha256:0123", "quay.io"},
		{"ghcr.io/a/b/c:v1", "ghcr.io"},
		{"registry.example.com:5000/profiles/qwen3:v1", "registry.example.com:5000"},
		{"localhost:5000/qwen3", "localhost:5000"},
		{"vllm/vllm-openai:v0.33.0", "index.docker.io"},
		{"vllm:v1", "index.docker.io"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := credentials.ImageHost(tc.image); got != tc.want {
			t.Errorf("ImageHost(%q) = %q, want %q", tc.image, got, tc.want)
		}
	}
}

// TestNormalizeHost asserts the spellings a docker config may use for one registry
// collapse to the form a resolver reports.
func TestNormalizeHost(t *testing.T) {
	tests := map[string]string{
		"quay.io":              "quay.io",
		"https://quay.io":      "quay.io",
		"http://quay.io/":      "quay.io",
		"ghcr.io/v1":           "ghcr.io",
		"ghcr.io/v2":           "ghcr.io",
		"docker.io":            "index.docker.io",
		"index.docker.io":      "index.docker.io",
		"localhost:5000":       "localhost:5000",
		"registry.example.com": "registry.example.com",
	}
	for in, want := range tests {
		if got := credentials.NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}
