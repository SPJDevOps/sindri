package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
)

// PackageFile is a placed file under a package, including version and size
// needed to render a PEP 503/691 simple index.
type PackageFile struct {
	ID        string
	VersionID string
	Version   string
	Digest    model.Digest
	Filename  string
	MediaType string
	Size      int64
	Metadata  json.RawMessage
}

// requirePyPIRepo loads a hosted PyPI repository by name.
func (r *Registry) requirePyPIRepo(ctx context.Context, name string) (db.Repository, error) {
	repo, err := r.q.GetRepositoryByName(ctx, name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Repository{}, ErrNotFound
		}
		return db.Repository{}, fmt.Errorf("looking up repository %s: %w", name, err)
	}
	if repo.Protocol != string(model.ProtocolPyPI) {
		return db.Repository{}, ErrWrongProtocol
	}
	return repo, nil
}

// RequirePyPIRepository returns the named PyPI repository or ErrNotFound /
// ErrWrongProtocol. Unlike OCI, PyPI repositories are never auto-created
// (ADR 12).
func (r *Registry) RequirePyPIRepository(ctx context.Context, name string) (model.Repository, error) {
	repo, err := r.requirePyPIRepo(ctx, name)
	if err != nil {
		return model.Repository{}, err
	}
	return repositoryFromRow(repo), nil
}

// NormalizePackageName folds a package name the way PyPI does (PEP 503).
func NormalizePackageName(name string) string {
	return normalizePackageName(name)
}

// ListPackageFiles returns every file placed under a package in a repository,
// ordered by version then filename. The repository must speak PyPI.
func (r *Registry) ListPackageFiles(ctx context.Context, repo, pkg string) ([]PackageFile, error) {
	if _, err := r.requirePyPIRepo(ctx, repo); err != nil {
		return nil, err
	}
	normalized := normalizePackageName(pkg)
	if normalized == "" {
		return nil, fmt.Errorf("%w: package name is required", ErrInvalidInput)
	}

	rows, err := r.q.ListFilesByPackage(ctx, db.ListFilesByPackageParams{
		RepoName:       repo,
		NormalizedName: normalized,
	})
	if err != nil {
		return nil, fmt.Errorf("listing files for %s/%s: %w", repo, pkg, err)
	}

	out := make([]PackageFile, 0, len(rows))
	for _, row := range rows {
		digest, err := model.ParseDigest(row.BlobDigest)
		if err != nil {
			return nil, fmt.Errorf("stored digest %q: %w", row.BlobDigest, err)
		}
		meta := json.RawMessage(row.Metadata)
		if len(meta) == 0 {
			meta = json.RawMessage("{}")
		}
		out = append(out, PackageFile{
			ID:        row.ID.String(),
			VersionID: row.VersionID.String(),
			Version:   row.Version,
			Digest:    digest,
			Filename:  row.Filename,
			MediaType: row.MediaType,
			Size:      row.BlobSize,
			Metadata:  meta,
		})
	}
	return out, nil
}

// GetFileByFilename resolves a placed file by repository, package, and
// filename (no version segment). Placement is checked before cas.Get (I2).
func (r *Registry) GetFileByFilename(ctx context.Context, repo, pkg, filename string) (PackageFile, io.ReadCloser, error) {
	if _, err := r.requirePyPIRepo(ctx, repo); err != nil {
		return PackageFile{}, nil, err
	}
	normalized := normalizePackageName(pkg)
	if normalized == "" {
		return PackageFile{}, nil, fmt.Errorf("%w: package name is required", ErrInvalidInput)
	}
	if strings.TrimSpace(filename) == "" {
		return PackageFile{}, nil, fmt.Errorf("%w: filename is required", ErrInvalidInput)
	}

	row, err := r.q.GetFileByFilename(ctx, db.GetFileByFilenameParams{
		Name:           repo,
		NormalizedName: normalized,
		Filename:       filename,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PackageFile{}, nil, ErrNotFound
		}
		return PackageFile{}, nil, fmt.Errorf("resolving file %s/%s/%s: %w", repo, pkg, filename, err)
	}

	digest, err := model.ParseDigest(row.BlobDigest)
	if err != nil {
		return PackageFile{}, nil, fmt.Errorf("stored digest %q: %w", row.BlobDigest, err)
	}

	if err := r.CheckPlacement(ctx, repo, digest); err != nil {
		return PackageFile{}, nil, err
	}

	rc, err := r.cas.Get(ctx, digest)
	if err != nil {
		return PackageFile{}, nil, fmt.Errorf("reading blob %s: %w", digest, err)
	}

	meta := json.RawMessage(row.Metadata)
	if len(meta) == 0 {
		meta = json.RawMessage("{}")
	}

	return PackageFile{
		ID:        row.ID.String(),
		VersionID: row.VersionID.String(),
		Version:   row.Version,
		Digest:    digest,
		Filename:  row.Filename,
		MediaType: row.MediaType,
		Size:      row.BlobSize,
		Metadata:  meta,
	}, rc, nil
}
