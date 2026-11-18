package registry

import (
	"context"
	"fmt"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
)

// maxDigestNames bounds how many names, and how many repositories, one digest
// lookup returns, so a digest tagged ten thousand times cannot make a page of
// ten thousand names.
const maxDigestNames = 50

// maxPlacingRepositories bounds the repositories one digest lookup reads. The
// caller filters them to the ones its reader may see, so the bound is generous.
const maxPlacingRepositories = 1000

// DigestName is one name inside a repository that points at a digest.
type DigestName struct {
	// IsTag is true for an OCI tag. Otherwise the name is an npm or PyPI file.
	IsTag bool
	// Package is the normalized package or image name, for links; Display is
	// the name as published.
	Package string
	Display string
	// Ref is the tag, or the version of the file.
	Ref      string
	Filename string
}

// NamesFor lists the names inside one repository that point at a digest: its
// OCI tags, or the versions whose file is this blob. It reads only the named
// repository, and only names that place the digest there (invariant I2), so it
// reveals nothing about any other repository.
func (r *Registry) NamesFor(ctx context.Context, repo string, digest model.Digest) ([]DigestName, error) {
	if !digest.Valid() {
		return nil, fmt.Errorf("%w: digest is required", ErrInvalidInput)
	}
	rows, err := r.q.ListNamesForDigest(ctx, db.ListNamesForDigestParams{
		RepoName: repo, Digest: digest.String(), MaxNames: maxDigestNames,
	})
	if err != nil {
		return nil, fmt.Errorf("listing the names of %s in %s: %w", digest, repo, err)
	}
	out := make([]DigestName, len(rows))
	for i, row := range rows {
		out[i] = DigestName{
			IsTag: row.Kind == "tag", Package: row.Package, Display: row.DisplayName,
			Ref: row.Ref, Filename: row.Filename,
		}
	}
	return out, nil
}

// PlacedIn lists the repositories that place a digest, in name order, in one
// query. It does no authorization: the caller must drop every repository its
// reader may not read before naming or counting any of them.
func (r *Registry) PlacedIn(ctx context.Context, digest model.Digest) ([]string, error) {
	if !digest.Valid() {
		return nil, fmt.Errorf("%w: digest is required", ErrInvalidInput)
	}
	names, err := r.q.ListPlacingRepositories(ctx, db.ListPlacingRepositoriesParams{
		Digest: digest.String(), MaxRepos: maxPlacingRepositories,
	})
	if err != nil {
		return nil, fmt.Errorf("listing the repositories placing %s: %w", digest, err)
	}
	return names, nil
}
