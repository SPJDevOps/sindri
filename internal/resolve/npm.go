package resolve

import (
	"context"
	"fmt"
	"io"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/remote"
)

// NPMPackument is a resolved packument. A hosted package is derived from rows and
// a proxied one is upstream's document, so exactly one field is set.
type NPMPackument struct {
	Hosted   *registry.NPMPackage
	Upstream *remote.NPMPackument
}

// GetNPMPackument resolves a package's packument. Through a virtual repository the
// first member with the package supplies the whole document, with no merging
// (ADR 24). The name must already have passed the adapter's read-side check:
// PackageClaimed reads a non-ASCII name as unclaimed, so such a name must never
// reach a claim walk.
func (r *Resolver) GetNPMPackument(ctx context.Context, repoName, name string) (NPMPackument, error) {
	repo, err := r.reg.RequireNPMRepository(ctx, repoName)
	if err != nil {
		return NPMPackument{}, err
	}
	if repo.Type != model.RepoTypeVirtual {
		return r.npmMemberPackument(ctx, repo, name)
	}
	var out NPMPackument
	err = r.walkMembers(ctx, repoName, name, map[string]struct{}{}, func(ctx context.Context, m model.Repository) (bool, error) {
		doc, err := r.npmMemberPackument(ctx, m, name)
		if err != nil {
			if absent(err) {
				return false, nil
			}
			return false, err
		}
		out = doc
		return true, nil
	})
	if err != nil {
		return NPMPackument{}, err
	}
	return out, nil
}

// npmMemberPackument reads a packument from one hosted or proxy repository.
func (r *Resolver) npmMemberPackument(ctx context.Context, repo model.Repository, name string) (NPMPackument, error) {
	switch repo.Type {
	case model.RepoTypeHosted:
		pkg, err := r.reg.GetNPMPackage(ctx, repo.Name, name)
		if err != nil {
			return NPMPackument{}, err
		}
		return NPMPackument{Hosted: &pkg}, nil
	case model.RepoTypeProxy:
		doc, err := r.remote.FetchNPMPackument(ctx, repo.Name, name)
		if err != nil {
			return NPMPackument{}, npmProxyError(err)
		}
		return NPMPackument{Upstream: &doc}, nil
	default:
		return NPMPackument{}, fmt.Errorf("unknown repository type %s", repo.Type)
	}
}

// GetNPMTarball resolves a tarball. A placed copy is read through the placement
// check in every case; only a proxy's uncached file comes from upstream, and the
// reader's final Read fails when those bytes do not verify.
//
// Through a virtual repository, the member whose packument a client would have
// been given also owns the tarballs, so one version cannot come from a different
// proxy than the document that listed it (ADR 24).
func (r *Resolver) GetNPMTarball(ctx context.Context, repoName, name, filename string) (registry.PackageFile, io.ReadCloser, error) {
	repo, err := r.reg.RequireNPMRepository(ctx, repoName)
	if err != nil {
		return registry.PackageFile{}, nil, err
	}
	if repo.Type != model.RepoTypeVirtual {
		return r.npmMemberTarball(ctx, repo, name, filename)
	}
	var (
		file registry.PackageFile
		rc   io.ReadCloser
	)
	err = r.walkMembers(ctx, repoName, name, map[string]struct{}{}, func(ctx context.Context, m model.Repository) (bool, error) {
		// A hosted member holds the file or lacks the package: if the name were
		// claimed there, the walk would have asked no other member.
		if m.Type == model.RepoTypeProxy {
			if _, err := r.npmMemberPackument(ctx, m, name); err != nil {
				if absent(err) {
					return false, nil
				}
				return false, err
			}
		}
		f, reader, err := r.npmMemberTarball(ctx, m, name, filename)
		if err != nil {
			if !absent(err) {
				return false, err
			}
			// A proxy that has the package owns this answer. It counts as a hit
			// with no reader rather than as ErrNotFound, because a walk over a
			// nested virtual repository moves on when it sees ErrNotFound.
			return m.Type == model.RepoTypeProxy, nil
		}
		file, rc = f, reader
		return true, nil
	})
	if err != nil {
		return registry.PackageFile{}, nil, err
	}
	if rc == nil {
		return registry.PackageFile{}, nil, ErrNotFound
	}
	return file, rc, nil
}

// npmMemberTarball reads a tarball from one hosted or proxy repository.
func (r *Resolver) npmMemberTarball(ctx context.Context, repo model.Repository, name, filename string) (registry.PackageFile, io.ReadCloser, error) {
	switch repo.Type {
	case model.RepoTypeHosted:
		return r.reg.GetNPMTarball(ctx, repo.Name, name, filename)
	case model.RepoTypeProxy:
		file, rc, err := r.remote.FetchNPMTarball(ctx, repo.Name, name, filename)
		if err != nil {
			return registry.PackageFile{}, nil, npmProxyError(err)
		}
		return file, rc, nil
	default:
		return registry.PackageFile{}, nil, fmt.Errorf("unknown repository type %s", repo.Type)
	}
}

// npmProxyError turns every way a proxy can lack something into ErrNotFound, and
// leaves an upstream failure to be reported as one.
func npmProxyError(err error) error {
	if absent(err) {
		return ErrNotFound
	}
	return err
}
