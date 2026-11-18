package registry_test

import (
	"archive/tar"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// startRegistry serves an in-memory OCI registry over plain HTTP and returns
// its "host:port" (the form used in image references).
func startRegistry(t *testing.T) string {
	t.Helper()
	return startRegistryWithHandler(t, registry.New())
}

// startRegistryWithHandler serves the supplied handler and returns its host.
func startRegistryWithHandler(t *testing.T, handler http.Handler) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

// basicAuthHandler wraps a registry handler so that every request must carry
// the supplied HTTP basic credentials.
func basicAuthHandler(username, password string) http.Handler {
	inner := registry.New()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != username || pass != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="test-registry"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// pushSpec describes the image a test pushes to the in-memory registry.
type pushSpec struct {
	tag    string
	labels map[string]string
	// files are written into a single layer, keyed by their path in the image
	// filesystem (leading slash optional).
	files map[string][]byte
	auth  authn.Authenticator
	host  string
}

// pushImage pushes the described image and returns its content digest.
func pushImage(t *testing.T, spec pushSpec) string {
	t.Helper()

	img := empty.Image
	if len(spec.files) > 0 {
		img = mutate.MediaType(img, "application/vnd.oci.image.manifest.v1+json")
		img = mutate.ConfigMediaType(img, "application/vnd.oci.image.config.v1+json")
		layer, err := tarball.LayerFromReader(bytes.NewReader(tarBytes(t, spec.files)))
		if err != nil {
			t.Fatalf("build layer: %v", err)
		}
		img, err = mutate.AppendLayers(img, layer)
		if err != nil {
			t.Fatalf("append layer: %v", err)
		}
	}

	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("config file: %v", err)
	}
	cfg = cfg.DeepCopy()
	cfg.Architecture = "amd64"
	cfg.OS = "linux"
	if len(spec.labels) > 0 {
		cfg.Config.Labels = spec.labels
	}
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		t.Fatalf("set config: %v", err)
	}

	tag, err := name.NewTag(spec.host+"/profiles/"+spec.tag, name.Insecure)
	if err != nil {
		t.Fatalf("parse tag: %v", err)
	}
	pushAuth := spec.auth
	if pushAuth == nil {
		pushAuth = authn.Anonymous
	}
	if err := remote.Write(tag, img, remote.WithAuth(pushAuth)); err != nil {
		t.Fatalf("push image: %v", err)
	}

	digest, err := img.Digest()
	if err != nil {
		t.Fatalf("image digest: %v", err)
	}
	return digest.String()
}

// tarBytes builds an uncompressed tar archive holding the supplied files.
func tarBytes(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		hdr := &tar.Header{
			Name:     strings.TrimPrefix(name, "/"),
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("tar content: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

// recordingCredentials returns an authn.Authenticator callback that records
// which registry host it was asked about.
type recordingCredentials struct {
	auth  authn.Authenticator
	hosts []string
}

func (c *recordingCredentials) callback(_ context.Context, registry string) (authn.Authenticator, error) {
	c.hosts = append(c.hosts, registry)
	return c.auth, nil
}
