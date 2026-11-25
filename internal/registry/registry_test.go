package registry_test

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
	"github.com/neuralmagic/vllm-vcr-operator/internal/registry"
)

func TestResolveUsesConfigLabel(t *testing.T) {
	host := startRegistry(t)
	metadata := []byte(`{"schemaVersion":"vcr.opendatahub-io/profile/v1","id":"qwen3-h200-tp1"}`)
	digest := pushImage(t, pushSpec{
		host:   host,
		tag:    "labeled:latest",
		labels: map[string]string{names.ProfileMetadataLabel: string(metadata)},
	})

	resolver := registry.New(registry.Options{})
	got, err := resolver.Resolve(context.Background(), host+"/profiles/labeled:latest")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if string(got.Metadata) != string(metadata) {
		t.Errorf("metadata = %q, want %q", got.Metadata, metadata)
	}
	if got.Source != registry.SourceLabel {
		t.Errorf("source = %q, want %q", got.Source, registry.SourceLabel)
	}
	if got.Digest != digest {
		t.Errorf("digest = %q, want %q", got.Digest, digest)
	}
	wantImage := host + "/profiles/labeled@" + digest
	if got.Image != wantImage {
		t.Errorf("image = %q, want %q", got.Image, wantImage)
	}
	if strings.Contains(got.Image, ":latest") {
		t.Errorf("image %q is still tag-pinned", got.Image)
	}
}

func TestResolveReportsMissingMetadata(t *testing.T) {
	host := startRegistry(t)
	pushImage(t, pushSpec{host: host, tag: "bare:latest"})
	ref := host + "/profiles/bare:latest"

	_, err := registry.New(registry.Options{}).Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("Resolve succeeded for an image without profile metadata")
	}
	for _, want := range []string{ref, names.ProfileMetadataLabel, "/profile/metadata.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestResolveRejectsUnparseableLabel(t *testing.T) {
	host := startRegistry(t)
	pushImage(t, pushSpec{
		host:   host,
		tag:    "broken-label:latest",
		labels: map[string]string{names.ProfileMetadataLabel: "not-json"},
	})
	ref := host + "/profiles/broken-label:latest"

	_, err := registry.New(registry.Options{}).Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("Resolve succeeded for an image with an unparseable metadata label")
	}
	for _, want := range []string{ref, names.ProfileMetadataLabel, "JSON"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestResolveFallsBackToLayerMetadata(t *testing.T) {
	host := startRegistry(t)
	metadata := []byte(`{"schemaVersion":"vcr.opendatahub-io/profile/v1","id":"from-layer"}`)
	digest := pushImage(t, pushSpec{
		host:  host,
		tag:   "layer:latest",
		files: map[string][]byte{"/profile/metadata.json": metadata},
	})

	got, err := registry.New(registry.Options{}).Resolve(context.Background(), host+"/profiles/layer:latest")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got.Metadata) != string(metadata) {
		t.Errorf("metadata = %q, want %q", got.Metadata, metadata)
	}
	if got.Source != registry.SourceLayer {
		t.Errorf("source = %q, want %q", got.Source, registry.SourceLayer)
	}
	if got.Digest != digest {
		t.Errorf("digest = %q, want %q", got.Digest, digest)
	}
}

func TestResolvePreservesDigestReference(t *testing.T) {
	host := startRegistry(t)
	metadata := []byte(`{"schemaVersion":"vcr.opendatahub-io/profile/v1","id":"pinned"}`)
	digest := pushImage(t, pushSpec{
		host:   host,
		tag:    "pinned:latest",
		labels: map[string]string{names.ProfileMetadataLabel: string(metadata)},
	})
	ref := host + "/profiles/pinned@" + digest

	got, err := registry.New(registry.Options{}).Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Image != ref {
		t.Errorf("image = %q, want the requested digest reference %q", got.Image, ref)
	}
	if got.Digest != digest {
		t.Errorf("digest = %q, want %q", got.Digest, digest)
	}
}

func TestResolveRejectsInvalidReference(t *testing.T) {
	_, err := registry.New(registry.Options{}).Resolve(context.Background(), "not a valid reference")
	if err == nil {
		t.Fatal("Resolve succeeded for an invalid reference")
	}
	if !strings.Contains(err.Error(), "not a valid reference") {
		t.Errorf("error %q does not name the offending reference", err)
	}
}

func TestResolvePrivateRegistryUsesInjectedCredentials(t *testing.T) {
	const (
		username = "profile-puller"
		password = "s3cr3t-pull-token"
	)
	host := startRegistryWithHandler(t, basicAuthHandler(username, password))
	metadata := []byte(`{"schemaVersion":"vcr.opendatahub-io/profile/v1","id":"private"}`)
	pushImage(t, pushSpec{
		host:   host,
		tag:    "private:latest",
		labels: map[string]string{names.ProfileMetadataLabel: string(metadata)},
		auth:   &authn.Basic{Username: username, Password: password},
	})
	ref := host + "/profiles/private:latest"

	anonymous := registry.New(registry.Options{})
	if _, err := anonymous.Resolve(context.Background(), ref); err == nil {
		t.Fatal("Resolve succeeded against a private registry without credentials")
	}

	creds := &recordingCredentials{auth: &authn.Basic{Username: username, Password: password}}
	resolver := registry.New(registry.Options{Credentials: creds.callback})
	got, err := resolver.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve with credentials: %v", err)
	}
	if string(got.Metadata) != string(metadata) {
		t.Errorf("metadata = %q, want %q", got.Metadata, metadata)
	}
	if len(creds.hosts) == 0 {
		t.Fatal("credentials callback was never consulted")
	}
	for _, seen := range creds.hosts {
		if seen != host {
			t.Errorf("credentials callback saw registry %q, want %q", seen, host)
		}
	}
}

func TestResolveRejectedCredentialsNeverLeak(t *testing.T) {
	const (
		username = "profile-puller"
		password = "wrong-pull-token"
	)
	host := startRegistryWithHandler(t, basicAuthHandler("profile-puller", "right-pull-token"))
	metadata := []byte(`{"schemaVersion":"vcr.opendatahub-io/profile/v1","id":"private"}`)
	pushImage(t, pushSpec{
		host:   host,
		tag:    "private:latest",
		labels: map[string]string{names.ProfileMetadataLabel: string(metadata)},
		auth:   &authn.Basic{Username: "profile-puller", Password: "right-pull-token"},
	})

	resolver := registry.New(registry.Options{
		Credentials: func(context.Context, string) (authn.Authenticator, error) {
			return &authn.Basic{Username: username, Password: password}, nil
		},
	})
	_, err := resolver.Resolve(context.Background(), host+"/profiles/private:latest")
	if err == nil {
		t.Fatal("Resolve succeeded with rejected credentials")
	}
	for _, secret := range []string{password, "wrong-pull-token", "Basic "} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error %q leaks credential material %q", err, secret)
		}
	}
}

func TestFetchFileReadsImageFilesystem(t *testing.T) {
	host := startRegistry(t)
	trace := []byte("replayed trace bytes\n")
	pushImage(t, pushSpec{
		host: host,
		tag:  "trace:latest",
		files: map[string][]byte{
			"/profile/metadata.json":  []byte(`{"id":"trace"}`),
			"/profile/trace.jsonl.gz": trace,
		},
	})

	resolver := registry.New(registry.Options{})
	for _, filePath := range []string{"/profile/trace.jsonl.gz", "profile/trace.jsonl.gz"} {
		got, err := resolver.FetchFile(context.Background(), host+"/profiles/trace:latest", filePath)
		if err != nil {
			t.Fatalf("FetchFile(%q): %v", filePath, err)
		}
		if string(got) != string(trace) {
			t.Errorf("FetchFile(%q) = %q, want %q", filePath, got, trace)
		}
	}

	_, err := resolver.FetchFile(context.Background(), host+"/profiles/trace:latest", "/profile/absent.json")
	if err == nil {
		t.Fatal("FetchFile succeeded for an absent file")
	}
	if !strings.Contains(err.Error(), "/profile/absent.json") {
		t.Errorf("error %q does not name the absent file", err)
	}
}

// TestPackageHasNoClusterDependencies guards the package boundary: registry
// resolution runs outside the cluster client and credentials are injected, so
// the package must not import Kubernetes machinery.
func TestPackageHasNoClusterDependencies(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	forbidden := []string{"k8s.io/", "sigs.k8s.io/", "github.com/neuralmagic/vllm-vcr-operator/internal/controller", "github.com/neuralmagic/vllm-vcr-operator/internal/webhook"}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", name, err)
			}
			for _, prefix := range forbidden {
				if strings.HasPrefix(path, prefix) {
					t.Errorf("%s imports %q; registry must stay independent of cluster clients", name, path)
				}
			}
		}
	}
}

func TestResolvePrefersLabelOverLayerFile(t *testing.T) {
	host := startRegistry(t)
	fromLabel := []byte(`{"schemaVersion":"vcr.opendatahub-io/profile/v1","id":"from-label"}`)
	fromLayer := []byte(`{"schemaVersion":"vcr.opendatahub-io/profile/v1","id":"from-layer"}`)
	pushImage(t, pushSpec{
		host:   host,
		tag:    "both:latest",
		labels: map[string]string{names.ProfileMetadataLabel: string(fromLabel)},
		files:  map[string][]byte{"/profile/metadata.json": fromLayer},
	})

	got, err := registry.New(registry.Options{}).Resolve(context.Background(), host+"/profiles/both:latest")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got.Metadata) != string(fromLabel) {
		t.Errorf("metadata = %q, want the config label value %q", got.Metadata, fromLabel)
	}
	if got.Source != registry.SourceLabel {
		t.Errorf("source = %q, want %q", got.Source, registry.SourceLabel)
	}
}
