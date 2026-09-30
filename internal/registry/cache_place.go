package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/storage"
)

// PlaceCachedFileParams places upstream bytes into a proxy repository.
type PlaceCachedFileParams struct {
	Repo      string
	Package   string
	Version   string
	Filename  string
	MediaType string
	Metadata  json.RawMessage
	// Blob is the upstream bytes, already committed to the CAS under their
	// verified digest by a CacheWriter.
	Blob storage.BlobInfo
}

// PlaceCachedFile places upstream bytes under a proxy repository. Hosted and
// virtual repositories are rejected: hosted uses Publish, and a virtual owns
// no bytes.
func (r *Registry) PlaceCachedFile(ctx context.Context, p PlaceCachedFileParams) (model.File, error) {
	if err := validatePlacementNames(p.Repo, p.Package, p.Version, p.Filename); err != nil {
		return model.File{}, err
	}
	if !p.Blob.Digest.Valid() {
		return model.File{}, fmt.Errorf("%w: cached file requires a digest", ErrInvalidInput)
	}

	repo, err := r.q.GetRepositoryByName(ctx, p.Repo)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.File{}, ErrNotFound
		}
		return model.File{}, fmt.Errorf("looking up repository %s: %w", p.Repo, err)
	}
	if repo.Type != string(model.RepoTypeProxy) {
		return model.File{}, fmt.Errorf("%w: PlaceCachedFile requires a proxy repository", ErrInvalidInput)
	}

	mediaType := p.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	metadata := p.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage("{}")
	}

	info := p.Blob
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return model.File{}, fmt.Errorf("beginning cache place transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := r.q.WithTx(tx)

	if _, err := q.UpsertBlob(ctx, db.UpsertBlobParams{
		Digest:         info.Digest.String(),
		Size:           info.Size,
		StorageBackend: r.backend,
	}); err != nil {
		return model.File{}, fmt.Errorf("recording blob %s: %w", info.Digest, err)
	}

	normalized := normalizePackageName(p.Package)
	pkg, err := q.GetOrCreatePackage(ctx, db.GetOrCreatePackageParams{
		RepoID:         repo.ID,
		NormalizedName: normalized,
		DisplayName:    p.Package,
	})
	if err != nil {
		return model.File{}, fmt.Errorf("ensuring package %s: %w", p.Package, err)
	}
	ver, err := q.GetOrCreateVersion(ctx, db.GetOrCreateVersionParams{
		PackageID: pkg.ID,
		Version:   p.Version,
	})
	if err != nil {
		return model.File{}, fmt.Errorf("ensuring version %s: %w", p.Version, err)
	}

	file, err := q.InsertFileImmutable(ctx, db.InsertFileImmutableParams{
		VersionID:  ver.ID,
		BlobDigest: info.Digest.String(),
		Filename:   p.Filename,
		MediaType:  mediaType,
		Metadata:   []byte(metadata),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Identical bytes already placed: treat as success.
			existing, gerr := q.GetFile(ctx, db.GetFileParams{
				Name:           p.Repo,
				NormalizedName: normalized,
				Version:        p.Version,
				Filename:       p.Filename,
			})
			if gerr != nil {
				return model.File{}, fmt.Errorf("%w: %s", ErrFileExists, p.Filename)
			}
			file = existing
		} else {
			return model.File{}, fmt.Errorf("placing cached file %s: %w", p.Filename, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return model.File{}, fmt.Errorf("committing cache place: %w", err)
	}
	return model.File{
		ID:        file.ID.String(),
		VersionID: file.VersionID.String(),
		Digest:    info.Digest,
		Filename:  file.Filename,
		MediaType: file.MediaType,
		Size:      info.Size,
	}, nil
}

// StoreMetadataBlob commits a small metadata document (upstream index JSON)
// and records a blobs row. It does not place the digest into a repository;
// remote_cache holds the reference.
func (r *Registry) StoreMetadataBlob(ctx context.Context, payload []byte) (model.Digest, error) {
	info, err := r.cas.PutBytes(ctx, payload)
	if err != nil {
		return model.Digest{}, fmt.Errorf("storing metadata blob: %w", err)
	}
	if _, err := r.q.UpsertBlob(ctx, db.UpsertBlobParams{
		Digest:         info.Digest.String(),
		Size:           info.Size,
		StorageBackend: r.backend,
	}); err != nil {
		return model.Digest{}, fmt.Errorf("recording metadata blob %s: %w", info.Digest, err)
	}
	return info.Digest, nil
}

// ReadBlobBytes returns the full contents of a digest after verifying it has
// a blobs row. Used for cached metadata documents referenced by remote_cache;
// callers must not use this as a substitute for placement-gated reads of
// tenant artifacts (invariant I2).
func (r *Registry) ReadBlobBytes(ctx context.Context, digest model.Digest) ([]byte, error) {
	if !digest.Valid() {
		return nil, fmt.Errorf("%w: digest is required", ErrInvalidInput)
	}
	if _, err := r.q.GetBlob(ctx, digest.String()); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("looking up blob %s: %w", digest, err)
	}
	rc, err := r.cas.Get(ctx, digest)
	if err != nil {
		return nil, fmt.Errorf("reading blob %s: %w", digest, err)
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("reading blob body %s: %w", digest, err)
	}
	return body, nil
}
