package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/storage"
)

// ociPackageName is the singleton package that carries tags when the Sindri
// repository name is the OCI image name (no remainder after prefix match).
const ociPackageName = "_"

// ErrWrongProtocol means the named repository exists but speaks a different
// protocol than the caller requires (OCI vs PyPI).
var ErrWrongProtocol = errors.New("wrong repository protocol")

// ociImagePackage returns the packages.normalized_name for an OCI image
// within a Sindri repository. An empty image means the repository itself is
// the image (M3 shape).
func ociImagePackage(image string) string {
	image = strings.Trim(image, "/")
	if image == "" {
		return ociPackageName
	}
	return image
}

// Manifest is a placed OCI manifest or index document.
type Manifest struct {
	Digest    model.Digest
	MediaType string
	Size      int64
	Body      []byte
}

// requireOCIRepo loads a hosted OCI repository by name.
func (r *Registry) requireOCIRepo(ctx context.Context, name string) (db.Repository, error) {
	repo, err := r.q.GetRepositoryByName(ctx, name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Repository{}, ErrNotFound
		}
		return db.Repository{}, fmt.Errorf("looking up repository %s: %w", name, err)
	}
	if repo.Protocol != string(model.ProtocolOCI) {
		return db.Repository{}, ErrWrongProtocol
	}
	return repo, nil
}

// EnsureOCIRepository returns the named hosted OCI repository, creating it
// when absent. Docker and the distribution-spec conformance suite push to
// names that were never declared via /api/v1; auto-create on first write is
// how every OCI registry stays usable. Explicit create via the management API
// remains available for operators who want the row first.
//
// Proxy and virtual repositories are never auto-created and never writable.
func (r *Registry) EnsureOCIRepository(ctx context.Context, name string) (model.Repository, error) {
	repo, err := r.requireOCIRepo(ctx, name)
	if err == nil {
		if repo.Type != string(model.RepoTypeHosted) {
			return model.Repository{}, fmt.Errorf("%w: %s is %s", ErrReadOnly, name, repo.Type)
		}
		return repositoryFromRow(repo), nil
	}
	if !errors.Is(err, ErrNotFound) {
		return model.Repository{}, err
	}
	created, err := r.CreateRepository(ctx, CreateRepositoryParams{
		Name:     name,
		Type:     model.RepoTypeHosted,
		Protocol: model.ProtocolOCI,
	})
	if err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			row, err := r.requireOCIRepo(ctx, name)
			if err != nil {
				return model.Repository{}, err
			}
			if row.Type != string(model.RepoTypeHosted) {
				return model.Repository{}, fmt.Errorf("%w: %s is %s", ErrReadOnly, name, row.Type)
			}
			return repositoryFromRow(row), nil
		}
		return model.Repository{}, err
	}
	return created, nil
}

// LinkCachedBlob places upstream OCI bytes, already committed to the CAS under
// their verified digest by a CacheWriter, into a proxy repository (I2).
func (r *Registry) LinkCachedBlob(ctx context.Context, repoName string, info storage.BlobInfo) error {
	repo, err := r.requireOCIRepo(ctx, repoName)
	if err != nil {
		return err
	}
	if repo.Type != string(model.RepoTypeProxy) {
		return fmt.Errorf("%w: LinkCachedBlob requires a proxy repository", ErrInvalidInput)
	}
	if !info.Digest.Valid() {
		return fmt.Errorf("%w: digest is required", ErrInvalidInput)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning cached blob transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := r.q.WithTx(tx)
	if _, err := q.UpsertBlob(ctx, db.UpsertBlobParams{
		Digest:         info.Digest.String(),
		Size:           info.Size,
		StorageBackend: r.backend,
	}); err != nil {
		return fmt.Errorf("recording blob %s: %w", info.Digest, err)
	}
	if err := q.LinkRepositoryBlob(ctx, db.LinkRepositoryBlobParams{
		RepoID: repo.ID,
		Digest: info.Digest.String(),
	}); err != nil {
		return fmt.Errorf("linking blob %s into %s: %w", info.Digest, repoName, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing cached blob: %w", err)
	}
	return nil
}

// LinkBlob records that digest belongs to repo under invariant I2. The blob
// bytes must already be in the CAS and a blobs row must exist (or is created).
func (r *Registry) LinkBlob(ctx context.Context, repoName string, digest model.Digest, size int64) error {
	if !digest.Valid() {
		return fmt.Errorf("%w: digest is required", ErrInvalidInput)
	}
	if _, err := r.EnsureOCIRepository(ctx, repoName); err != nil {
		return err
	}
	repo, err := r.requireOCIRepo(ctx, repoName)
	if err != nil {
		return err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning link transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := r.q.WithTx(tx)

	if _, err := q.UpsertBlob(ctx, db.UpsertBlobParams{
		Digest:         digest.String(),
		Size:           size,
		StorageBackend: r.backend,
	}); err != nil {
		return fmt.Errorf("recording blob %s: %w", digest, err)
	}
	if err := q.LinkRepositoryBlob(ctx, db.LinkRepositoryBlobParams{
		RepoID: repo.ID,
		Digest: digest.String(),
	}); err != nil {
		return fmt.Errorf("linking blob %s into %s: %w", digest, repoName, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing link: %w", err)
	}
	return nil
}

// UnlinkBlob removes a repository_blobs placement. Bytes stay for GC.
func (r *Registry) UnlinkBlob(ctx context.Context, repoName string, digest model.Digest) error {
	if !digest.Valid() {
		return fmt.Errorf("%w: digest is required", ErrInvalidInput)
	}
	repo, err := r.requireOCIRepo(ctx, repoName)
	if err != nil {
		return err
	}
	n, err := r.q.UnlinkRepositoryBlob(ctx, db.UnlinkRepositoryBlobParams{
		RepoID: repo.ID,
		Digest: digest.String(),
	})
	if err != nil {
		return fmt.Errorf("unlinking blob %s: %w", digest, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CommitBlobUpload finishes a staged CAS upload and places the digest in repo.
func (r *Registry) CommitBlobUpload(ctx context.Context, repoName string, upload *cas.Upload, expected model.Digest) (storage.BlobInfo, error) {
	if upload == nil {
		return storage.BlobInfo{}, fmt.Errorf("%w: upload is required", ErrInvalidInput)
	}
	if _, err := r.requireOCIRepo(ctx, repoName); err != nil {
		return storage.BlobInfo{}, err
	}

	info, err := upload.Commit(ctx, expected, -1)
	if err != nil {
		return storage.BlobInfo{}, err
	}
	if err := r.LinkBlob(ctx, repoName, info.Digest, info.Size); err != nil {
		return storage.BlobInfo{}, err
	}
	return info, nil
}

// StatBlob reports a placed blob. Placement is checked before Stat (I2).
func (r *Registry) StatBlob(ctx context.Context, repoName string, digest model.Digest) (storage.BlobInfo, error) {
	if err := r.CheckPlacement(ctx, repoName, digest); err != nil {
		if errors.Is(err, ErrNotPlaced) {
			return storage.BlobInfo{}, ErrNotFound
		}
		return storage.BlobInfo{}, err
	}
	info, err := r.cas.Stat(ctx, digest)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return storage.BlobInfo{}, ErrNotFound
		}
		return storage.BlobInfo{}, fmt.Errorf("stating blob %s: %w", digest, err)
	}
	return info, nil
}

// GetBlob returns a placed blob reader. Placement is checked before Get (I2).
func (r *Registry) GetBlob(ctx context.Context, repoName string, digest model.Digest) (storage.BlobInfo, io.ReadCloser, error) {
	info, err := r.StatBlob(ctx, repoName, digest)
	if err != nil {
		return storage.BlobInfo{}, nil, err
	}
	rc, err := r.cas.Get(ctx, digest)
	if err != nil {
		return storage.BlobInfo{}, nil, fmt.Errorf("reading blob %s: %w", digest, err)
	}
	return info, rc, nil
}

// GetBlobRange returns a byte range of a placed blob.
func (r *Registry) GetBlobRange(ctx context.Context, repoName string, digest model.Digest, off, length int64) (storage.BlobInfo, io.ReadCloser, error) {
	info, err := r.StatBlob(ctx, repoName, digest)
	if err != nil {
		return storage.BlobInfo{}, nil, err
	}
	rc, err := r.cas.GetRange(ctx, digest, off, length)
	if err != nil {
		return storage.BlobInfo{}, nil, fmt.Errorf("reading blob range %s: %w", digest, err)
	}
	return info, rc, nil
}

// PutManifest stores a manifest or index, records children, places the digest,
// and optionally binds a tag. image is the OCI image name within the Sindri
// repository; empty means the repository name is the image (package "_").
// The manifest digest becomes a subject carrying prov (ADR 16).
func (r *Registry) PutManifest(ctx context.Context, repoName, image, reference, mediaType string, body []byte, prov model.Provenance) (Manifest, error) {
	if len(body) == 0 {
		return Manifest{}, fmt.Errorf("%w: manifest body is required", ErrInvalidInput)
	}
	if _, err := r.EnsureOCIRepository(ctx, repoName); err != nil {
		return Manifest{}, err
	}
	repo, err := r.requireOCIRepo(ctx, repoName)
	if err != nil {
		return Manifest{}, err
	}
	pkgName := ociImagePackage(image)

	children, resolvedMediaType, err := parseManifestChildren(mediaType, body)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if mediaType == "" {
		mediaType = resolvedMediaType
	}

	info, err := r.cas.PutBytes(ctx, body)
	if err != nil {
		return Manifest{}, fmt.Errorf("storing manifest: %w", err)
	}

	refIsDigest := false
	if d, perr := model.ParseDigest(reference); perr == nil {
		refIsDigest = true
		if d != info.Digest {
			return Manifest{}, fmt.Errorf("%w: reference digest %s does not match body digest %s",
				ErrInvalidInput, d, info.Digest)
		}
	} else if reference == "" {
		return Manifest{}, fmt.Errorf("%w: reference is required", ErrInvalidInput)
	} else if err := validateTagName(reference); err != nil {
		return Manifest{}, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Manifest{}, fmt.Errorf("beginning manifest transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := r.q.WithTx(tx)

	if _, err := q.UpsertBlob(ctx, db.UpsertBlobParams{
		Digest:         info.Digest.String(),
		Size:           info.Size,
		StorageBackend: r.backend,
	}); err != nil {
		return Manifest{}, fmt.Errorf("recording manifest blob %s: %w", info.Digest, err)
	}
	if err := q.LinkRepositoryBlob(ctx, db.LinkRepositoryBlobParams{
		RepoID: repo.ID,
		Digest: info.Digest.String(),
	}); err != nil {
		return Manifest{}, fmt.Errorf("linking manifest %s: %w", info.Digest, err)
	}

	if err := q.DeleteManifestChildren(ctx, info.Digest.String()); err != nil {
		return Manifest{}, fmt.Errorf("clearing manifest children: %w", err)
	}
	for _, child := range children {
		if err := q.UpsertManifestChild(ctx, db.UpsertManifestChildParams{
			ParentDigest: info.Digest.String(),
			ChildDigest:  child.String(),
		}); err != nil {
			return Manifest{}, fmt.Errorf("recording child %s of %s: %w", child, info.Digest, err)
		}
	}

	if !refIsDigest {
		pkg, err := q.GetOrCreatePackage(ctx, db.GetOrCreatePackageParams{
			RepoID:         repo.ID,
			NormalizedName: pkgName,
			DisplayName:    pkgName,
		})
		if err != nil {
			return Manifest{}, fmt.Errorf("ensuring OCI package: %w", err)
		}
		if _, err := q.UpsertTag(ctx, db.UpsertTagParams{
			RepoID:       repo.ID,
			PackageID:    pkg.ID,
			Name:         reference,
			TargetDigest: info.Digest.String(),
		}); err != nil {
			return Manifest{}, fmt.Errorf("upserting tag %s: %w", reference, err)
		}
	}

	kind := model.SubjectKindOCIManifest
	if isIndexMediaType(resolvedMediaType) {
		kind = model.SubjectKindOCIIndex
	}
	if err := recordSubject(ctx, q, repo, info.Digest, info.Size, kind, prov); err != nil {
		return Manifest{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Manifest{}, fmt.Errorf("committing manifest: %w", err)
	}

	return Manifest{
		Digest:    info.Digest,
		MediaType: mediaType,
		Size:      info.Size,
		Body:      body,
	}, nil
}

// PlaceCachedManifest places an upstream OCI manifest into a proxy repository.
func (r *Registry) PlaceCachedManifest(ctx context.Context, repoName, image, reference, mediaType string, body []byte) (Manifest, error) {
	repo, err := r.requireOCIRepo(ctx, repoName)
	if err != nil {
		return Manifest{}, err
	}
	if repo.Type != string(model.RepoTypeProxy) {
		return Manifest{}, fmt.Errorf("%w: PlaceCachedManifest requires a proxy repository", ErrInvalidInput)
	}
	if len(body) == 0 {
		return Manifest{}, fmt.Errorf("%w: manifest body is required", ErrInvalidInput)
	}
	pkgName := ociImagePackage(image)

	children, resolvedMediaType, err := parseManifestChildren(mediaType, body)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if mediaType == "" {
		mediaType = resolvedMediaType
	}

	info, err := r.cas.PutBytes(ctx, body)
	if err != nil {
		return Manifest{}, fmt.Errorf("storing cached manifest: %w", err)
	}

	refIsDigest := false
	if d, perr := model.ParseDigest(reference); perr == nil {
		refIsDigest = true
		if d != info.Digest {
			return Manifest{}, fmt.Errorf("%w: reference digest %s does not match body digest %s",
				ErrInvalidInput, d, info.Digest)
		}
	} else if reference == "" {
		return Manifest{}, fmt.Errorf("%w: reference is required", ErrInvalidInput)
	} else if err := validateTagName(reference); err != nil {
		return Manifest{}, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Manifest{}, fmt.Errorf("beginning cached manifest transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := r.q.WithTx(tx)

	if _, err := q.UpsertBlob(ctx, db.UpsertBlobParams{
		Digest:         info.Digest.String(),
		Size:           info.Size,
		StorageBackend: r.backend,
	}); err != nil {
		return Manifest{}, fmt.Errorf("recording manifest blob %s: %w", info.Digest, err)
	}
	if err := q.LinkRepositoryBlob(ctx, db.LinkRepositoryBlobParams{
		RepoID: repo.ID,
		Digest: info.Digest.String(),
	}); err != nil {
		return Manifest{}, fmt.Errorf("linking manifest %s: %w", info.Digest, err)
	}
	if err := q.DeleteManifestChildren(ctx, info.Digest.String()); err != nil {
		return Manifest{}, fmt.Errorf("clearing manifest children: %w", err)
	}
	childSizes := manifestDescriptorSizes(body)
	for _, child := range children {
		// Proxy fetches place a manifest before its layers, and an index
		// before platform manifests that may never be pulled. The FK on
		// manifest_children requires a blobs row, so record a placeholder
		// sized from the descriptor. It never overwrites an existing row, and
		// LinkCachedBlob records the real size when the bytes arrive. A
		// placeholder has no bytes behind it: GC must treat an unplaced blobs
		// row as absent rather than as something to delete from storage.
		size := childSizes[child.String()]
		if size < 0 {
			size = 0
		}
		if err := q.InsertBlobPlaceholder(ctx, db.InsertBlobPlaceholderParams{
			Digest:         child.String(),
			Size:           size,
			StorageBackend: r.backend,
		}); err != nil {
			return Manifest{}, fmt.Errorf("recording child blob %s: %w", child, err)
		}
		if err := q.UpsertManifestChild(ctx, db.UpsertManifestChildParams{
			ParentDigest: info.Digest.String(),
			ChildDigest:  child.String(),
		}); err != nil {
			return Manifest{}, fmt.Errorf("recording child %s of %s: %w", child, info.Digest, err)
		}
	}
	if !refIsDigest {
		pkg, err := q.GetOrCreatePackage(ctx, db.GetOrCreatePackageParams{
			RepoID:         repo.ID,
			NormalizedName: pkgName,
			DisplayName:    pkgName,
		})
		if err != nil {
			return Manifest{}, fmt.Errorf("ensuring OCI package: %w", err)
		}
		if _, err := q.UpsertTag(ctx, db.UpsertTagParams{
			RepoID:       repo.ID,
			PackageID:    pkg.ID,
			Name:         reference,
			TargetDigest: info.Digest.String(),
		}); err != nil {
			return Manifest{}, fmt.Errorf("upserting tag %s: %w", reference, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Manifest{}, fmt.Errorf("committing cached manifest: %w", err)
	}
	return Manifest{
		Digest:    info.Digest,
		MediaType: mediaType,
		Size:      info.Size,
		Body:      body,
	}, nil
}

// GetManifest resolves a tag or digest reference to a placed manifest.
// image is the OCI image within the repository; empty means package "_".
func (r *Registry) GetManifest(ctx context.Context, repoName, image, reference string) (Manifest, error) {
	digest, err := r.resolveManifestDigest(ctx, repoName, image, reference)
	if err != nil {
		return Manifest{}, err
	}
	if err := r.CheckPlacement(ctx, repoName, digest); err != nil {
		if errors.Is(err, ErrNotPlaced) {
			return Manifest{}, ErrNotFound
		}
		return Manifest{}, err
	}

	rc, err := r.cas.Get(ctx, digest)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Manifest{}, ErrNotFound
		}
		return Manifest{}, fmt.Errorf("reading manifest %s: %w", digest, err)
	}
	defer func() { _ = rc.Close() }()

	body, err := io.ReadAll(rc)
	if err != nil {
		return Manifest{}, fmt.Errorf("reading manifest body %s: %w", digest, err)
	}

	mediaType := sniffManifestMediaType(body)
	info, err := r.cas.Stat(ctx, digest)
	size := int64(len(body))
	if err == nil {
		size = info.Size
	}

	return Manifest{
		Digest:    digest,
		MediaType: mediaType,
		Size:      size,
		Body:      body,
	}, nil
}

// DeleteManifest removes a tag reference, or unlinks a digest placement.
// Blob bytes are left for garbage collection. image selects the package when
// deleting by tag; empty means "_".
func (r *Registry) DeleteManifest(ctx context.Context, repoName, image, reference string) error {
	repo, err := r.requireOCIRepo(ctx, repoName)
	if err != nil {
		return err
	}
	if repo.Type != string(model.RepoTypeHosted) {
		return fmt.Errorf("%w: %s is %s", ErrReadOnly, repoName, repo.Type)
	}

	if digest, perr := model.ParseDigest(reference); perr == nil {
		n, err := r.q.UnlinkRepositoryBlob(ctx, db.UnlinkRepositoryBlobParams{
			RepoID: repo.ID,
			Digest: digest.String(),
		})
		if err != nil {
			return fmt.Errorf("unlinking manifest %s: %w", digest, err)
		}
		if n == 0 {
			return ErrNotFound
		}
		if err := r.q.DeleteTagsByDigest(ctx, db.DeleteTagsByDigestParams{
			RepoID:       repo.ID,
			TargetDigest: digest.String(),
		}); err != nil {
			return fmt.Errorf("clearing tags for %s: %w", digest, err)
		}
		return nil
	}

	if err := validateTagName(reference); err != nil {
		return err
	}
	n, err := r.q.DeleteTag(ctx, db.DeleteTagParams{
		RepoName:       repoName,
		NormalizedName: ociImagePackage(image),
		TagName:        reference,
	})
	if err != nil {
		return fmt.Errorf("deleting tag %s: %w", reference, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListTagsParams controls tag listing pagination.
type ListTagsParams struct {
	Repo  string
	Image string // empty means package "_"
	After string // last tag from a previous page (exclusive)
	Limit int32
}

// ListTags returns tag names in lexicographic order.
func (r *Registry) ListTags(ctx context.Context, p ListTagsParams) (Page[string], error) {
	if _, err := r.requireOCIRepo(ctx, p.Repo); err != nil {
		return Page[string]{}, err
	}
	limit, err := pageLimit(p.Limit)
	if err != nil {
		return Page[string]{}, err
	}

	rows, err := r.q.ListTags(ctx, db.ListTagsParams{
		RepoName:       p.Repo,
		NormalizedName: ociImagePackage(p.Image),
		AfterName:      textCursor(p.After),
		PageLimit:      limit + 1,
	})
	if err != nil {
		return Page[string]{}, fmt.Errorf("listing tags: %w", err)
	}

	page := Page[string]{Items: make([]string, 0, len(rows))}
	for i, name := range rows {
		if int32(i) >= limit {
			page.NextCursor = rows[limit-1]
			break
		}
		page.Items = append(page.Items, name)
	}
	return page, nil
}

func (r *Registry) resolveManifestDigest(ctx context.Context, repoName, image, reference string) (model.Digest, error) {
	if digest, err := model.ParseDigest(reference); err == nil {
		return digest, nil
	}
	// Unknown or malformed tags are "not found" on the read path so clients
	// see 404 rather than 400 (distribution-spec pull behaviour).
	if err := validateTagName(reference); err != nil {
		return model.Digest{}, ErrNotFound
	}
	if _, err := r.requireOCIRepo(ctx, repoName); err != nil {
		return model.Digest{}, err
	}
	tag, err := r.q.GetTag(ctx, db.GetTagParams{
		RepoName:       repoName,
		NormalizedName: ociImagePackage(image),
		TagName:        reference,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Digest{}, ErrNotFound
		}
		return model.Digest{}, fmt.Errorf("resolving tag %s: %w", reference, err)
	}
	digest, err := model.ParseDigest(tag.TargetDigest)
	if err != nil {
		return model.Digest{}, fmt.Errorf("stored tag digest %q: %w", tag.TargetDigest, err)
	}
	return digest, nil
}

func validateTagName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: tag is required", ErrInvalidInput)
	}
	if len(name) > 128 {
		return fmt.Errorf("%w: tag exceeds 128 characters", ErrInvalidInput)
	}
	// Distribution-spec: [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		case i > 0 && (c == '.' || c == '-'):
		default:
			return fmt.Errorf("%w: tag %q is malformed", ErrInvalidInput, name)
		}
	}
	return nil
}

// manifestDescriptorSizes returns the size each descriptor in a manifest or
// index claims for its target, keyed by digest. Unparseable bodies yield an
// empty map; parseManifestChildren is the validating parse.
func manifestDescriptorSizes(body []byte) map[string]int64 {
	var doc struct {
		Config    *ocispec.Descriptor  `json:"config"`
		Layers    []ocispec.Descriptor `json:"layers"`
		Manifests []ocispec.Descriptor `json:"manifests"`
	}
	sizes := map[string]int64{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return sizes
	}
	if doc.Config != nil {
		sizes[doc.Config.Digest.String()] = doc.Config.Size
	}
	for _, d := range append(doc.Layers, doc.Manifests...) {
		sizes[d.Digest.String()] = d.Size
	}
	return sizes
}

func parseManifestChildren(mediaType string, body []byte) ([]model.Digest, string, error) {
	mt := mediaType
	if mt == "" {
		mt = sniffManifestMediaType(body)
	}

	switch mt {
	case ocispec.MediaTypeImageIndex,
		"application/vnd.docker.distribution.manifest.list.v2+json":
		var idx ocispec.Index
		if err := json.Unmarshal(body, &idx); err != nil {
			return nil, "", fmt.Errorf("decoding image index: %w", err)
		}
		out := make([]model.Digest, 0, len(idx.Manifests))
		for _, d := range idx.Manifests {
			child, err := model.ParseDigest(d.Digest.String())
			if err != nil {
				return nil, "", fmt.Errorf("index child digest: %w", err)
			}
			out = append(out, child)
		}
		return out, mt, nil

	case ocispec.MediaTypeImageManifest,
		"application/vnd.docker.distribution.manifest.v2+json":
		var man ocispec.Manifest
		if err := json.Unmarshal(body, &man); err != nil {
			return nil, "", fmt.Errorf("decoding image manifest: %w", err)
		}
		out := make([]model.Digest, 0, 1+len(man.Layers))
		if man.Config.Digest != "" {
			cfg, err := model.ParseDigest(man.Config.Digest.String())
			if err != nil {
				return nil, "", fmt.Errorf("config digest: %w", err)
			}
			out = append(out, cfg)
		}
		for _, layer := range man.Layers {
			child, err := model.ParseDigest(layer.Digest.String())
			if err != nil {
				return nil, "", fmt.Errorf("layer digest: %w", err)
			}
			out = append(out, child)
		}
		return out, mt, nil

	default:
		// Unknown but still JSON: store without children rather than reject
		// schema versions we have not specialised for yet.
		if !json.Valid(body) {
			return nil, "", errors.New("manifest is not valid JSON")
		}
		if mt == "" {
			mt = "application/vnd.oci.image.manifest.v1+json"
		}
		return nil, mt, nil
	}
}

// isIndexMediaType reports whether mt names a multi-platform index.
func isIndexMediaType(mt string) bool {
	return mt == ocispec.MediaTypeImageIndex ||
		mt == "application/vnd.docker.distribution.manifest.list.v2+json"
}

func sniffManifestMediaType(body []byte) string {
	var probe struct {
		MediaType string `json:"mediaType"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.MediaType != "" {
		return probe.MediaType
	}
	// Fall back by shape: indexes have "manifests", image manifests have "layers".
	var shape struct {
		Manifests []json.RawMessage `json:"manifests"`
		Layers    []json.RawMessage `json:"layers"`
		Config    json.RawMessage   `json:"config"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		return ocispec.MediaTypeImageManifest
	}
	if len(shape.Manifests) > 0 {
		return ocispec.MediaTypeImageIndex
	}
	if shape.Config != nil || len(shape.Layers) > 0 {
		return ocispec.MediaTypeImageManifest
	}
	return ocispec.MediaTypeImageManifest
}
