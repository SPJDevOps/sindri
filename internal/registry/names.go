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

// PlacedIn lists, in name order, the repositories among among that place a
// digest, at most limit of them, and reports whether more were left out. It
// looks only at the repositories it is given, so the caller passes the ones its
// reader may read: the limit then applies after that filter, and no other
// repository can change, name, or count in the answer. It does no authorization
// itself.
func (r *Registry) PlacedIn(ctx context.Context, digest model.Digest, among []string, limit int) (names []string, more bool, err error) {
	if !digest.Valid() {
		return nil, false, fmt.Errorf("%w: digest is required", ErrInvalidInput)
	}
	if len(among) == 0 || limit <= 0 {
		return nil, false, nil
	}
	names, err = r.q.ListPlacingRepositories(ctx, db.ListPlacingRepositoriesParams{
		Digest: digest.String(), RepoNames: among, MaxRepos: int32(limit) + 1, //nolint:gosec // G115: limit is a small page size
	})
	if err != nil {
		return nil, false, fmt.Errorf("listing the repositories placing %s: %w", digest, err)
	}
	if len(names) > limit {
		return names[:limit], true, nil
	}
	return names, false, nil
}
