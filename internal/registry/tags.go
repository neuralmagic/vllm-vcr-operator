package registry

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Tagger lists the tags published in a repository.
type Tagger interface {
	Tags(ctx context.Context, repository string) ([]string, error)
}

var _ Tagger = (*Remote)(nil)

// Tags returns the repository's tags, sorted, so that a catalog built from them does not
// depend on the order a registry happened to answer in.
//
// A repository that does not exist yet is an error rather than an empty list: "there is
// nothing here" and "this registry name is wrong" must not look the same when the name came
// from cluster configuration.
func (r *Remote) Tags(ctx context.Context, repository string) ([]string, error) {
	var opts []name.Option
	if r.opts.Insecure {
		opts = append(opts, name.Insecure)
	}
	repo, err := name.NewRepository(repository, opts...)
	if err != nil {
		return nil, fmt.Errorf("profile registry %q: invalid repository: %w", repository, err)
	}

	auth := authn.Authenticator(authn.Anonymous)
	if r.opts.Credentials != nil {
		host := repo.RegistryStr()
		resolved, err := r.opts.Credentials(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("profile registry %q: resolve credentials for registry %q: %w", repository, host, err)
		}
		if resolved != nil {
			auth = resolved
		}
	}

	tags, err := remote.List(repo, remote.WithContext(ctx), remote.WithAuth(auth))
	if err != nil {
		return nil, fmt.Errorf("profile registry %q: list tags: %w", repository, err)
	}
	sort.Strings(tags)
	return tags, nil
}
