package resolve

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/remote"
)

// NPMPackument is a resolved packument. A hosted package is derived from rows and
// a proxied one is upstream's document, so exactly one field is set. Repo is the
// repository that answered, and owns any tarball the document names.
type NPMPackument struct {
	Repo     string
	Hosted   *registry.NPMPackage
	Upstream *remote.NPMPackument
}

// errNPMVirtual is what the virtual cases answer until virtual npm repositories
// exist. The adapter answers 501 before it gets here.
var errNPMVirtual = errors.New("reading through a virtual npm repository is not implemented")

// GetNPMPackument resolves a package's packument. The name must already have
// passed the adapter's read-side check: PackageClaimed reads a non-ASCII name as
// unclaimed, so such a name must never reach a claim walk.
func (r *Resolver) GetNPMPackument(ctx context.Context, repoName, name string) (NPMPackument, error) {
	repo, err := r.reg.RequireNPMRepository(ctx, repoName)
	if err != nil {
		return NPMPackument{}, err
	}
	switch repo.Type {
	case model.RepoTypeHosted:
		pkg, err := r.reg.GetNPMPackage(ctx, repoName, name)
		if err != nil {
			return NPMPackument{}, err
		}
		return NPMPackument{Repo: repoName, Hosted: &pkg}, nil
	case model.RepoTypeProxy:
		doc, err := r.remote.FetchNPMPackument(ctx, repoName, name)
		if err != nil {
			return NPMPackument{}, npmProxyError(err)
		}
		return NPMPackument{Repo: repoName, Upstream: &doc}, nil
	case model.RepoTypeVirtual:
		return NPMPackument{}, errNPMVirtual
	default:
		return NPMPackument{}, fmt.Errorf("unknown repository type %s", repo.Type)
	}
}

// GetNPMTarball resolves a tarball. A placed copy is read through the placement
// check in every case; only a proxy's uncached file comes from upstream, and the
// reader's final Read fails when those bytes do not verify.
func (r *Resolver) GetNPMTarball(ctx context.Context, repoName, name, filename string) (registry.PackageFile, io.ReadCloser, error) {
	repo, err := r.reg.RequireNPMRepository(ctx, repoName)
	if err != nil {
		return registry.PackageFile{}, nil, err
	}
	switch repo.Type {
	case model.RepoTypeHosted:
		return r.reg.GetNPMTarball(ctx, repoName, name, filename)
	case model.RepoTypeProxy:
		file, rc, err := r.remote.FetchNPMTarball(ctx, repoName, name, filename)
		if err != nil {
			return registry.PackageFile{}, nil, npmProxyError(err)
		}
		return file, rc, nil
	case model.RepoTypeVirtual:
		return registry.PackageFile{}, nil, errNPMVirtual
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
