package registry

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
)

// imageProfileDir is the profile directory inside a profile image, as defined
// by docs/contracts/profile-image.md.
const imageProfileDir = "/profile"

// metadataPathInImage is the layer path of the fallback metadata document.
const metadataPathInImage = imageProfileDir + "/" + names.ProfileMetadataFileName

// Source identifies where the metadata document was read from.
type Source string

const (
	// SourceLabel means the metadata came from the image config label
	// names.ProfileMetadataLabel.
	SourceLabel Source = "label"
	// SourceLayer means the metadata came from /profile/metadata.json inside
	// the image filesystem.
	SourceLayer Source = "layer"
)

// Resolved is a profile image pinned to a digest together with its metadata.
type Resolved struct {
	// Image is the digest-pinned image reference, repository@sha256:...
	// Callers pin this value so that later tag moves cannot change the
	// simulation.
	Image string
	// Digest is the resolved content digest, sha256:...
	Digest string
	// Metadata is the raw profile metadata document, byte-identical to the
	// value published in the image config label when Source is SourceLabel.
	Metadata []byte
	// Source reports whether Metadata came from the config label or from the
	// image filesystem.
	Source Source
}

// Options configures a resolver.
type Options struct {
	// Insecure allows fetching a reference over plain HTTP. It exists for
	// test registries only.
	Insecure bool

	// Credentials resolves the authenticator for a registry host. A nil
	// callback means anonymous access. Errors returned by the callback must
	// not contain credential material; they are surfaced to callers verbatim
	// (wrapped with the image reference and registry host).
	Credentials func(ctx context.Context, registry string) (authn.Authenticator, error)
}

// Resolver resolves a profile image reference to a digest-pinned reference and
// its metadata document.
type Resolver interface {
	Resolve(ctx context.Context, ref string) (Resolved, error)
}

// FileFetcher reads a file from a profile image filesystem. It is used by
// export verification and tests; reconciliation does not need it.
type FileFetcher interface {
	FetchFile(ctx context.Context, ref, filePath string) ([]byte, error)
}

// Remote resolves profile images from an OCI registry.
type Remote struct {
	opts Options
}

var (
	_ Resolver    = (*Remote)(nil)
	_ FileFetcher = (*Remote)(nil)
)

// New returns a resolver for profile images in OCI registries.
func New(opts Options) *Remote {
	return &Remote{opts: opts}
}

// Resolve returns the digest-pinned reference for ref and its profile metadata
// document. See the package documentation for the metadata source precedence.
func (r *Remote) Resolve(ctx context.Context, ref string) (Resolved, error) {
	nr, err := r.parseReference(ref)
	if err != nil {
		return Resolved{}, err
	}
	img, err := r.image(ctx, nr, ref)
	if err != nil {
		return Resolved{}, err
	}

	pinned, err := r.pin(nr, img)
	if err != nil {
		return Resolved{}, fmt.Errorf("profile image %q: resolve digest: %w", ref, err)
	}

	config, err := img.ConfigFile()
	if err != nil {
		return Resolved{}, fmt.Errorf("profile image %q: read image config: %w", ref, err)
	}
	if raw, ok := config.Config.Labels[names.ProfileMetadataLabel]; ok {
		if !json.Valid([]byte(raw)) {
			return Resolved{}, fmt.Errorf(
				"profile image %q: image config label %q is not valid JSON",
				ref, names.ProfileMetadataLabel)
		}
		return Resolved{
			Image:    pinned.String(),
			Digest:   pinned.DigestStr(),
			Metadata: []byte(raw),
			Source:   SourceLabel,
		}, nil
	}

	raw, err := fileFromImage(img, metadataPathInImage)
	if err != nil {
		if errors.Is(err, errFileNotFound) {
			return Resolved{}, fmt.Errorf(
				"profile image %q: profile metadata not found: image config label %q is absent and no layer provides %q",
				ref, names.ProfileMetadataLabel, metadataPathInImage)
		}
		return Resolved{}, fmt.Errorf("profile image %q: read %q from layers: %w", ref, metadataPathInImage, err)
	}
	return Resolved{
		Image:    pinned.String(),
		Digest:   pinned.DigestStr(),
		Metadata: raw,
		Source:   SourceLayer,
	}, nil
}

// FetchFile reads filePath (leading slash optional) from the image ref resolves
// to. It never downloads more layers than needed to find the file, scanning
// from the topmost layer down.
func (r *Remote) FetchFile(ctx context.Context, ref, filePath string) ([]byte, error) {
	nr, err := r.parseReference(ref)
	if err != nil {
		return nil, err
	}
	img, err := r.image(ctx, nr, ref)
	if err != nil {
		return nil, err
	}
	raw, err := fileFromImage(img, filePath)
	if err != nil {
		if errors.Is(err, errFileNotFound) {
			return nil, fmt.Errorf("profile image %q: file %q not found in any layer", ref, filePath)
		}
		return nil, fmt.Errorf("profile image %q: read %q from layers: %w", ref, filePath, err)
	}
	return raw, nil
}

// parseReference parses ref, allowing plain HTTP when Insecure is set.
func (r *Remote) parseReference(ref string) (name.Reference, error) {
	// Strict validation: a profile image reference must be fully qualified so
	// that reconciliation never silently falls back to a default registry.
	var opts []name.Option
	if r.opts.Insecure {
		opts = append(opts, name.Insecure)
	}
	nr, err := name.ParseReference(ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("profile image %q: invalid image reference: %w", ref, err)
	}
	return nr, nil
}

// remoteOptions builds the remote options for nr, resolving credentials from
// the injected callback without ever recording them.
func (r *Remote) remoteOptions(ctx context.Context, nr name.Reference, ref string) ([]remote.Option, error) {
	auth := authn.Authenticator(authn.Anonymous)
	if r.opts.Credentials != nil {
		host := nr.Context().RegistryStr()
		resolved, err := r.opts.Credentials(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("profile image %q: resolve credentials for registry %q: %w", ref, host, err)
		}
		if resolved != nil {
			auth = resolved
		}
	}
	return []remote.Option{remote.WithContext(ctx), remote.WithAuth(auth)}, nil
}

// image fetches the image ref names.
func (r *Remote) image(ctx context.Context, nr name.Reference, ref string) (v1.Image, error) {
	opts, err := r.remoteOptions(ctx, nr, ref)
	if err != nil {
		return nil, err
	}
	img, err := remote.Image(nr, opts...)
	if err != nil {
		return nil, fmt.Errorf("profile image %q: fetch from registry: %w", ref, err)
	}
	return img, nil
}

// pin returns the digest reference for nr. Digest references are preserved
// exactly; tag references are resolved once through the image manifest.
func (r *Remote) pin(nr name.Reference, img v1.Image) (name.Digest, error) {
	if digest, ok := nr.(name.Digest); ok {
		return digest, nil
	}
	hash, err := img.Digest()
	if err != nil {
		return name.Digest{}, err
	}
	return name.NewDigest(nr.Context().Name() + "@" + hash.String())
}

var errFileNotFound = errors.New("file not found in image")

// fileFromImage reads filePath from img's layers, scanning from the topmost
// layer down and stopping at the first layer holding the file.
func fileFromImage(img v1.Image, filePath string) ([]byte, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	want := normalizeImagePath(filePath)
	for i := len(layers) - 1; i >= 0; i-- {
		content, err := readLayerFile(layers[i], want)
		if errors.Is(err, errFileNotFound) {
			continue
		}
		return content, err
	}
	return nil, errFileNotFound
}

// readLayerFile reads a single file from one layer's uncompressed tar stream.
func readLayerFile(layer v1.Layer, want string) ([]byte, error) {
	rc, err := layer.Uncompressed()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	tr := tar.NewReader(rc)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errFileNotFound
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if normalizeImagePath(header.Name) != want {
			continue
		}
		return io.ReadAll(tr)
	}
}

// normalizeImagePath makes a path comparable regardless of leading "./" or "/".
func normalizeImagePath(p string) string {
	cleaned := path.Clean("/" + strings.TrimPrefix(p, "./"))
	return strings.TrimPrefix(cleaned, "/")
}
