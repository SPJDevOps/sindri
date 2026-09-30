// Package resolve implements virtual repository ordered resolution and the
// name-claim rule (invariant I5).
//
// Adapters call into Resolver for reads. Writes stay on registry and are
// rejected for proxy and virtual repositories.
package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/remote"
	"github.com/spj/sindri/internal/storage"
)

// Resolver walks hosted, proxy, and virtual repositories for reads.
type Resolver struct {
	reg    *registry.Registry
	remote *remote.Client
}

// New wires a Resolver.
func New(reg *registry.Registry, rem *remote.Client) *Resolver {
	if reg == nil {
		panic("resolve.New: nil registry")
	}
	if rem == nil {
		panic("resolve.New: nil remote client")
	}
	return &Resolver{reg: reg, remote: rem}
}

// Registry exposes the underlying registry for write paths that still go
// directly to hosted repositories.
func (r *Resolver) Registry() *registry.Registry { return r.reg }

// ErrNotFound means nothing matched across the resolution walk.
var ErrNotFound = registry.ErrNotFound

// ErrUpstreamUnavailable means a proxy upstream failed and nothing cached could
// answer instead. Adapters report it as a bad gateway, not an internal error.
var ErrUpstreamUnavailable = remote.ErrUpstreamUnavailable

// absent reports whether err means "this member does not have it", which moves
// a virtual walk on to the next member rather than failing it.
func absent(err error) bool {
	return errors.Is(err, registry.ErrNotFound) ||
		errors.Is(err, registry.ErrNotPlaced) ||
		errors.Is(err, remote.ErrUpstreamNotFound) ||
		errors.Is(err, remote.ErrNegativeCached) ||
		errors.Is(err, remote.ErrUpstreamDenied)
}

// SplitOCIPath finds the longest repository name prefix of path and returns
// the Sindri repository, the image name remainder, and the rest after the
// route marker (blobs|manifests|tags).
func (r *Resolver) SplitOCIPath(ctx context.Context, path string) (repo model.Repository, image, rest string, err error) {
	path = strings.Trim(path, "/")
	name, rest, ok := splitOCIMarkers(path)
	if !ok {
		return model.Repository{}, "", "", ErrNotFound
	}
	matched, remainder, err := r.reg.MatchRepositoryPrefix(ctx, name)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			// No configured repository: treat the whole name as a hosted image
			// path (M3 auto-create on write).
			return model.Repository{Name: name, Type: model.RepoTypeHosted, Protocol: model.ProtocolOCI}, "", rest, nil
		}
		return model.Repository{}, "", "", err
	}
	return matched, remainder, rest, nil
}

func splitOCIMarkers(path string) (name, rest string, ok bool) {
	for _, marker := range []string{"/blobs/", "/manifests/", "/tags/"} {
		if i := strings.Index(path, marker); i > 0 {
			return path[:i], path[i+1:], true
		}
	}
	if i := strings.Index(path, "/blobs/uploads"); i > 0 {
		return path[:i], path[i+1:], true
	}
	return "", "", false
}

// claimOwner walks members in order and returns the first hosted member that
// has ever published normalizedName. An empty name means unclaimed.
func (r *Resolver) claimOwner(ctx context.Context, members []model.Repository, normalizedName string, visiting map[string]struct{}) (model.Repository, bool, error) {
	for _, m := range members {
		switch m.Type {
		case model.RepoTypeHosted:
			claimed, err := r.reg.PackageClaimed(ctx, m.Name, normalizedName)
			if err != nil {
				return model.Repository{}, false, err
			}
			if claimed {
				return m, true, nil
			}
		case model.RepoTypeVirtual:
			if _, seen := visiting[m.Name]; seen {
				continue
			}
			next := copyVisited(visiting)
			next[m.Name] = struct{}{}
			nested, err := r.reg.ListMembers(ctx, m.Name)
			if err != nil {
				return model.Repository{}, false, err
			}
			if owner, ok, err := r.claimOwner(ctx, nested, normalizedName, next); err != nil {
				return model.Repository{}, false, err
			} else if ok {
				return owner, true, nil
			}
		}
	}
	return model.Repository{}, false, nil
}

func copyVisited(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}

// walkMembers invokes fn for each member in order until fn returns a non-nil
// result or all members are exhausted. Hosted name claims short-circuit the
// walk when normalizedName is non-empty.
func (r *Resolver) walkMembers(ctx context.Context, virtualName, normalizedName string, visiting map[string]struct{}, fn func(context.Context, model.Repository) (bool, error)) error {
	if _, seen := visiting[virtualName]; seen {
		return fmt.Errorf("%w: membership cycle at %s", registry.ErrCycle, virtualName)
	}
	visiting[virtualName] = struct{}{}

	members, err := r.reg.ListMembers(ctx, virtualName)
	if err != nil {
		return err
	}

	if normalizedName != "" {
		if owner, claimed, err := r.claimOwner(ctx, members, normalizedName, copyVisited(visiting)); err != nil {
			return err
		} else if claimed {
			hit, err := fn(ctx, owner)
			if err != nil {
				return err
			}
			if !hit {
				return ErrNotFound
			}
			return nil
		}
	}

	for _, m := range members {
		switch m.Type {
		case model.RepoTypeVirtual:
			if err := r.walkMembers(ctx, m.Name, normalizedName, copyVisited(visiting), fn); err != nil {
				if errors.Is(err, ErrNotFound) {
					continue
				}
				return err
			}
			return nil
		default:
			hit, err := fn(ctx, m)
			if err != nil {
				return err
			}
			if hit {
				return nil
			}
		}
	}
	return ErrNotFound
}

// PyPIProjectFiles is the resolved project index content for adapters.
type PyPIProjectFiles struct {
	Repo      string // repository that owns the placement (member, not virtual)
	Project   string
	Files     []registry.PackageFile
	FromProxy bool
	Index     *remote.PyPIIndex // set when served from a freshly fetched / cached proxy index
}

// ListPyPIFiles resolves a project across hosted / proxy / virtual.
func (r *Resolver) ListPyPIFiles(ctx context.Context, repoName, project string) (PyPIProjectFiles, error) {
	repo, err := r.reg.GetRepository(ctx, repoName)
	if err != nil {
		return PyPIProjectFiles{}, err
	}
	if repo.Protocol != model.ProtocolPyPI {
		return PyPIProjectFiles{}, registry.ErrWrongProtocol
	}
	normalized := registry.NormalizePackageName(project)
	if normalized == "" {
		return PyPIProjectFiles{}, ErrNotFound
	}

	switch repo.Type {
	case model.RepoTypeHosted:
		files, err := r.reg.ListPackageFiles(ctx, repoName, normalized)
		if err != nil {
			return PyPIProjectFiles{}, err
		}
		if len(files) == 0 {
			return PyPIProjectFiles{}, ErrNotFound
		}
		return PyPIProjectFiles{Repo: repoName, Project: normalized, Files: files}, nil

	case model.RepoTypeProxy:
		return r.listProxyPyPI(ctx, repoName, normalized)

	case model.RepoTypeVirtual:
		var out PyPIProjectFiles
		err := r.walkMembers(ctx, repoName, normalized, map[string]struct{}{}, func(ctx context.Context, m model.Repository) (bool, error) {
			switch m.Type {
			case model.RepoTypeHosted:
				files, err := r.reg.ListPackageFiles(ctx, m.Name, normalized)
				if err != nil {
					return false, err
				}
				if len(files) == 0 {
					// Claimed but empty versions: still a hit for the claim rule.
					claimed, cerr := r.reg.PackageClaimed(ctx, m.Name, normalized)
					if cerr != nil {
						return false, cerr
					}
					if claimed {
						out = PyPIProjectFiles{Repo: m.Name, Project: normalized, Files: files}
						return true, nil
					}
					return false, nil
				}
				out = PyPIProjectFiles{Repo: m.Name, Project: normalized, Files: files}
				return true, nil
			case model.RepoTypeProxy:
				res, err := r.listProxyPyPI(ctx, m.Name, normalized)
				if err != nil {
					if absent(err) {
						return false, nil
					}
					return false, err
				}
				out = res
				return true, nil
			default:
				return false, nil
			}
		})
		if err != nil {
			return PyPIProjectFiles{}, err
		}
		return out, nil
	default:
		return PyPIProjectFiles{}, fmt.Errorf("unknown repository type %s", repo.Type)
	}
}

func (r *Resolver) listProxyPyPI(ctx context.Context, repoName, project string) (PyPIProjectFiles, error) {
	idx, err := r.remote.FetchPyPIIndex(ctx, repoName, project)
	if err != nil {
		if absent(err) {
			return PyPIProjectFiles{}, ErrNotFound
		}
		// Stale local files may still be listable.
		files, lerr := r.reg.ListPackageFiles(ctx, repoName, project)
		if lerr == nil && len(files) > 0 {
			return PyPIProjectFiles{Repo: repoName, Project: project, Files: files, FromProxy: true}, nil
		}
		return PyPIProjectFiles{}, err
	}

	files, err := r.reg.ListPackageFiles(ctx, repoName, project)
	if err != nil && !errors.Is(err, registry.ErrNotFound) {
		return PyPIProjectFiles{}, err
	}
	return PyPIProjectFiles{
		Repo:      repoName,
		Project:   project,
		Files:     files,
		FromProxy: true,
		Index:     &idx,
	}, nil
}

// GetPyPIFile resolves a package file download.
func (r *Resolver) GetPyPIFile(ctx context.Context, repoName, project, filename string) (registry.PackageFile, io.ReadCloser, error) {
	repo, err := r.reg.GetRepository(ctx, repoName)
	if err != nil {
		return registry.PackageFile{}, nil, err
	}
	normalized := registry.NormalizePackageName(project)

	switch repo.Type {
	case model.RepoTypeHosted:
		return r.reg.GetFileByFilename(ctx, repoName, normalized, filename)
	case model.RepoTypeProxy:
		file, rc, err := r.getProxyPyPIFile(ctx, repoName, normalized, filename)
		if err != nil && absent(err) {
			return registry.PackageFile{}, nil, ErrNotFound
		}
		return file, rc, err
	case model.RepoTypeVirtual:
		var file registry.PackageFile
		var rc io.ReadCloser
		err := r.walkMembers(ctx, repoName, normalized, map[string]struct{}{}, func(ctx context.Context, m model.Repository) (bool, error) {
			switch m.Type {
			case model.RepoTypeHosted:
				f, reader, err := r.reg.GetFileByFilename(ctx, m.Name, normalized, filename)
				if err != nil {
					if errors.Is(err, registry.ErrNotFound) || errors.Is(err, registry.ErrNotPlaced) {
						claimed, cerr := r.reg.PackageClaimed(ctx, m.Name, normalized)
						if cerr != nil {
							return false, cerr
						}
						if claimed {
							return true, ErrNotFound
						}
						return false, nil
					}
					return false, err
				}
				file, rc = f, reader
				return true, nil
			case model.RepoTypeProxy:
				f, reader, err := r.getProxyPyPIFile(ctx, m.Name, normalized, filename)
				if err != nil {
					if absent(err) {
						return false, nil
					}
					return false, err
				}
				file, rc = f, reader
				return true, nil
			default:
				return false, nil
			}
		})
		if err != nil {
			return registry.PackageFile{}, nil, err
		}
		return file, rc, nil
	default:
		return registry.PackageFile{}, nil, fmt.Errorf("unknown repository type %s", repo.Type)
	}
}

func (r *Resolver) getProxyPyPIFile(ctx context.Context, repoName, project, filename string) (registry.PackageFile, io.ReadCloser, error) {
	if file, rc, err := r.reg.GetFileByFilename(ctx, repoName, project, filename); err == nil {
		return file, rc, nil
	} else if !errors.Is(err, registry.ErrNotFound) && !errors.Is(err, registry.ErrNotPlaced) {
		return registry.PackageFile{}, nil, err
	}

	idx, err := r.remote.FetchPyPIIndex(ctx, repoName, project)
	if err != nil {
		if absent(err) {
			return registry.PackageFile{}, nil, ErrNotFound
		}
		return registry.PackageFile{}, nil, err
	}
	remoteCfg, err := r.reg.GetRemote(ctx, repoName)
	if err != nil {
		return registry.PackageFile{}, nil, err
	}

	for _, f := range idx.Files {
		if f.Filename != filename {
			continue
		}
		fileURL, err := remote.ResolveFileURL(remoteCfg.UpstreamURL, f.URL)
		if err != nil {
			return registry.PackageFile{}, nil, err
		}
		var expect model.Digest
		if hex, ok := f.Hashes["sha256"]; ok && hex != "" {
			d, perr := model.ParseDigest("sha256:" + hex)
			if perr == nil {
				expect = d
			}
		}
		version := versionFromFilename(filename)
		return r.remote.FetchPyPIFile(ctx, repoName, project, version, filename, fileURL, expect, f.Size)
	}
	return registry.PackageFile{}, nil, ErrNotFound
}

func versionFromFilename(filename string) string {
	// best-effort: name-version-...
	base := strings.TrimSuffix(filename, ".whl")
	base = strings.TrimSuffix(base, ".tar.gz")
	base = strings.TrimSuffix(base, ".zip")
	parts := strings.Split(base, "-")
	if len(parts) >= 2 {
		return parts[1]
	}
	return "0"
}

// ListPyPIProjects returns project names for a simple root index. Proxy and
// virtual repositories list only what is already cached locally; they do not
// enumerate upstream.
func (r *Resolver) ListPyPIProjects(ctx context.Context, repoName string) ([]string, error) {
	repo, err := r.reg.GetRepository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	switch repo.Type {
	case model.RepoTypeHosted, model.RepoTypeProxy:
		return r.listLocalProjects(ctx, repoName)
	case model.RepoTypeVirtual:
		seen := map[string]struct{}{}
		var names []string
		members, err := r.reg.ListMembers(ctx, repoName)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			part, err := r.ListPyPIProjects(ctx, m.Name)
			if err != nil {
				return nil, err
			}
			for _, n := range part {
				if _, ok := seen[n]; ok {
					continue
				}
				seen[n] = struct{}{}
				names = append(names, n)
			}
		}
		return names, nil
	default:
		return nil, fmt.Errorf("unknown repository type %s", repo.Type)
	}
}

func (r *Resolver) listLocalProjects(ctx context.Context, repoName string) ([]string, error) {
	var names []string
	var after string
	for {
		page, err := r.reg.ListPackages(ctx, registry.ListPackagesParams{Repo: repoName, After: after, Limit: 200})
		if err != nil {
			return nil, err
		}
		for _, pkg := range page.Items {
			names = append(names, pkg.NormalizedName)
		}
		if page.NextCursor == "" {
			break
		}
		after = page.NextCursor
	}
	return names, nil
}

// GetOCIManifest resolves a manifest through hosted / proxy / virtual.
func (r *Resolver) GetOCIManifest(ctx context.Context, repoName, image, reference, accept string) (registry.Manifest, error) {
	repo, err := r.reg.GetRepository(ctx, repoName)
	if err != nil {
		return registry.Manifest{}, err
	}
	pkgName := image
	if pkgName == "" {
		pkgName = "_"
	}

	switch repo.Type {
	case model.RepoTypeHosted:
		return r.reg.GetManifest(ctx, repoName, image, reference)
	case model.RepoTypeProxy:
		man, err := r.remote.FetchOCIManifest(ctx, repoName, image, reference, accept)
		if err != nil {
			if absent(err) {
				return registry.Manifest{}, ErrNotFound
			}
			return registry.Manifest{}, err
		}
		return man, nil
	case model.RepoTypeVirtual:
		var out registry.Manifest
		err := r.walkMembers(ctx, repoName, pkgName, map[string]struct{}{}, func(ctx context.Context, m model.Repository) (bool, error) {
			switch m.Type {
			case model.RepoTypeHosted:
				man, err := r.reg.GetManifest(ctx, m.Name, image, reference)
				if err != nil {
					if absent(err) {
						claimed, cerr := r.reg.PackageClaimed(ctx, m.Name, pkgName)
						if cerr != nil {
							return false, cerr
						}
						if claimed {
							return true, ErrNotFound
						}
						return false, nil
					}
					return false, err
				}
				out = man
				return true, nil
			case model.RepoTypeProxy:
				man, err := r.remote.FetchOCIManifest(ctx, m.Name, image, reference, accept)
				if err != nil {
					if absent(err) {
						return false, nil
					}
					return false, err
				}
				out = man
				return true, nil
			default:
				return false, nil
			}
		})
		if err != nil {
			return registry.Manifest{}, err
		}
		return out, nil
	default:
		return registry.Manifest{}, fmt.Errorf("unknown repository type %s", repo.Type)
	}
}

// eachOCIBlobSource calls fn for the repository that can hold a blob: the
// repository itself when hosted or proxy, or each member in order when
// virtual. Blobs are addressed by digest, so no name claim applies; placement
// is still checked on whichever member answers.
func (r *Resolver) eachOCIBlobSource(ctx context.Context, repoName string, fn func(context.Context, model.Repository) (bool, error)) error {
	repo, err := r.reg.GetRepository(ctx, repoName)
	if err != nil {
		return err
	}
	switch repo.Type {
	case model.RepoTypeHosted, model.RepoTypeProxy:
		hit, err := fn(ctx, repo)
		if err != nil {
			return err
		}
		if !hit {
			return ErrNotFound
		}
		return nil
	case model.RepoTypeVirtual:
		return r.walkMembers(ctx, repoName, "", map[string]struct{}{}, fn)
	default:
		return fmt.Errorf("unknown repository type %s", repo.Type)
	}
}

// StatOCIBlob reports a blob's size without downloading it. For an uncached
// proxy blob this is an upstream HEAD.
func (r *Resolver) StatOCIBlob(ctx context.Context, repoName, image string, digest model.Digest) (int64, error) {
	var size int64
	err := r.eachOCIBlobSource(ctx, repoName, func(ctx context.Context, m model.Repository) (bool, error) {
		var err error
		switch m.Type {
		case model.RepoTypeHosted:
			var info storage.BlobInfo
			info, err = r.reg.StatBlob(ctx, m.Name, digest)
			size = info.Size
		case model.RepoTypeProxy:
			size, err = r.remote.StatOCIBlob(ctx, m.Name, image, digest)
		default:
			return false, nil
		}
		if err != nil {
			if absent(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	if err != nil {
		return 0, err
	}
	return size, nil
}

// GetOCIBlob resolves a blob through hosted / proxy / virtual. An uncached
// proxy blob streams from upstream while it is cached; the size is -1 when
// upstream did not declare one.
func (r *Resolver) GetOCIBlob(ctx context.Context, repoName, image string, digest model.Digest) (io.ReadCloser, int64, error) {
	var (
		rc   io.ReadCloser
		size int64
	)
	err := r.eachOCIBlobSource(ctx, repoName, func(ctx context.Context, m model.Repository) (bool, error) {
		var err error
		switch m.Type {
		case model.RepoTypeHosted:
			var info storage.BlobInfo
			info, rc, err = r.reg.GetBlob(ctx, m.Name, digest)
			size = info.Size
		case model.RepoTypeProxy:
			rc, size, err = r.remote.FetchOCIBlob(ctx, m.Name, image, digest)
		default:
			return false, nil
		}
		if err != nil {
			if absent(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	if err != nil {
		return nil, 0, err
	}
	return rc, size, nil
}

// GetOCIBlobRange returns length bytes of a blob from off. A byte range can
// only be served from storage, so an uncached proxy blob is fetched and cached
// in full first, and the range is then read from the member that holds it.
func (r *Resolver) GetOCIBlobRange(ctx context.Context, repoName, image string, digest model.Digest, off, length int64) (io.ReadCloser, error) {
	var owner string
	err := r.eachOCIBlobSource(ctx, repoName, func(ctx context.Context, m model.Repository) (bool, error) {
		if m.Type != model.RepoTypeHosted && m.Type != model.RepoTypeProxy {
			return false, nil
		}
		err := r.reg.CheckPlacement(ctx, m.Name, digest)
		if err == nil {
			owner = m.Name
			return true, nil
		}
		if !absent(err) {
			return false, err
		}
		if m.Type == model.RepoTypeHosted {
			return false, nil
		}
		rc, _, err := r.remote.FetchOCIBlob(ctx, m.Name, image, digest)
		if err != nil {
			if absent(err) {
				return false, nil
			}
			return false, err
		}
		_, copyErr := io.Copy(io.Discard, rc)
		closeErr := rc.Close()
		if copyErr != nil {
			return false, copyErr
		}
		if closeErr != nil {
			return false, closeErr
		}
		owner = m.Name
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	_, rc, err := r.reg.GetBlobRange(ctx, owner, digest, off, length)
	if err != nil {
		return nil, err
	}
	return rc, nil
}

// ListOCITags lists tags for an image through hosted / proxy / virtual.
func (r *Resolver) ListOCITags(ctx context.Context, repoName, image string, after string, limit int32) (registry.Page[string], error) {
	repo, err := r.reg.GetRepository(ctx, repoName)
	if err != nil {
		return registry.Page[string]{}, err
	}
	switch repo.Type {
	case model.RepoTypeHosted, model.RepoTypeProxy:
		return r.reg.ListTags(ctx, registry.ListTagsParams{Repo: repoName, Image: image, After: after, Limit: limit})
	case model.RepoTypeVirtual:
		// First member with any tags wins (first-hit, no merge).
		var page registry.Page[string]
		err := r.walkMembers(ctx, repoName, "", map[string]struct{}{}, func(ctx context.Context, m model.Repository) (bool, error) {
			p, err := r.reg.ListTags(ctx, registry.ListTagsParams{Repo: m.Name, Image: image, After: after, Limit: limit})
			if err != nil {
				if errors.Is(err, registry.ErrNotFound) {
					return false, nil
				}
				return false, err
			}
			if len(p.Items) == 0 {
				return false, nil
			}
			page = p
			return true, nil
		})
		if err != nil {
			return registry.Page[string]{}, err
		}
		return page, nil
	default:
		return registry.Page[string]{}, fmt.Errorf("unknown repository type %s", repo.Type)
	}
}

// IndexFileMeta helps adapters render proxy index entries that are not yet
// placed as files rows.
func IndexFileMeta(f remote.PyPIIndexFile) json.RawMessage {
	meta := map[string]any{}
	if f.Requires != "" {
		meta["requires_python"] = f.Requires
	}
	b, _ := json.Marshal(meta)
	return b
}
