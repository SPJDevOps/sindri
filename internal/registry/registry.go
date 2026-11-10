// Package registry is the domain spine: repositories, packages, versions, and
// file placements. It is the only package allowed to decide whether a digest
// may be read (invariant I2) and the only package that writes placement rows
// onto a shared blob (invariant I1).
//
// Protocol adapters and the management API call into this package. Nothing
// here imports an adapter or the HTTP layer.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
)

var (
	// ErrNotFound means the requested repository, package, version, or file
	// does not exist (or is not placed for this caller).
	ErrNotFound = errors.New("not found")

	// ErrAlreadyExists means a create would violate a uniqueness constraint.
	ErrAlreadyExists = errors.New("already exists")

	// ErrInvalidInput means the caller sent a value the domain rejects.
	ErrInvalidInput = errors.New("invalid input")

	// ErrNotPlaced means a digest is physically present but has no placement
	// in the named repository. Serving it would break invariant I2.
	ErrNotPlaced = errors.New("digest not placed in repository")

	// ErrFileExists means an immutable publish would reuse a filename that is
	// taken: placed with different bytes, placed under another version, or
	// published once and since deleted. A published artifact is the thing
	// consumers pinned a hash of, so its name does not change hands. The text
	// must hold for all three, including a publisher who re-sent identical bytes.
	ErrFileExists = errors.New("filename already taken in this package")

	// ErrVersionExists means an exclusive publish named a version that is
	// already published. npm versions are immutable even for identical bytes.
	ErrVersionExists = errors.New("version already published")

	// ErrHasHistory means a delete would remove a repository the lifecycle
	// ledger refers to. The ledger is append-only, so the repository stays.
	ErrHasHistory = errors.New("repository has lifecycle history")

	// ErrBlobUnknown means a manifest references a blob Sindri does not hold,
	// either never pushed or collected before the manifest arrived. The OCI
	// adapter answers MANIFEST_BLOB_UNKNOWN, and the client pushes it again.
	ErrBlobUnknown = errors.New("manifest references an unknown blob")
)

// Registry owns placement metadata and gates every blob read.
type Registry struct {
	pool    *pgxpool.Pool
	q       *db.Queries
	cas     *cas.Store
	backend string

	scanOnPublish bool
}

// SetScanOnPublish makes every hosted publish of a scannable kind enqueue a
// scan job in the publish transaction (ADR 23). Call it once while wiring,
// before serving; it is not safe to call concurrently with publishes.
func (r *Registry) SetScanOnPublish(on bool) { r.scanOnPublish = on }

// New wires a Registry. backend is recorded on blob rows (filesystem, s3, …).
func New(database *db.DB, store *cas.Store, backend string) *Registry {
	if database == nil {
		panic("registry.New: nil database")
	}
	if store == nil {
		panic("registry.New: nil cas.Store")
	}
	if backend == "" {
		backend = "cas"
	}
	return &Registry{
		pool:    database.Pool(),
		q:       database.Queries(),
		cas:     store,
		backend: backend,
	}
}

// GetRepository returns one repository by name.
func (r *Registry) GetRepository(ctx context.Context, name string) (model.Repository, error) {
	row, err := r.q.GetRepositoryByName(ctx, name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Repository{}, ErrNotFound
		}
		return model.Repository{}, fmt.Errorf("getting repository %s: %w", name, err)
	}
	return repositoryFromRow(row), nil
}

// ListRepositoriesParams controls cursor pagination over repositories.
type ListRepositoriesParams struct {
	After string
	Limit int32
}

// Page is a cursor-paginated result.
type Page[T any] struct {
	Items      []T
	NextCursor string
}

// AllRepositories returns every repository, unpaged. It is for callers that
// must decide per repository, such as the audit trail filtering events to the
// repositories a caller may read; a registry has tens or hundreds of
// repositories, not millions.
func (r *Registry) AllRepositories(ctx context.Context) ([]model.Repository, error) {
	rows, err := r.q.ListAllRepositories(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing repositories: %w", err)
	}
	out := make([]model.Repository, 0, len(rows))
	for _, row := range rows {
		out = append(out, repositoryFromRow(row))
	}
	return out, nil
}

// ListRepositories returns repositories ordered by name.
func (r *Registry) ListRepositories(ctx context.Context, p ListRepositoriesParams) (Page[model.Repository], error) {
	limit, err := pageLimit(p.Limit)
	if err != nil {
		return Page[model.Repository]{}, err
	}

	rows, err := r.q.ListRepositories(ctx, db.ListRepositoriesParams{
		AfterName: textCursor(p.After),
		PageLimit: limit + 1, // one extra to detect a next page
	})
	if err != nil {
		return Page[model.Repository]{}, fmt.Errorf("listing repositories: %w", err)
	}

	page := Page[model.Repository]{Items: make([]model.Repository, 0, len(rows))}
	for i, row := range rows {
		if int32(i) >= limit {
			page.NextCursor = rows[limit-1].Name
			break
		}
		page.Items = append(page.Items, repositoryFromRow(row))
	}
	return page, nil
}

// DeleteRepository removes an API-managed repository and cascades its
// placements in the database. Blob bytes are left for garbage collection
// (never cas.Delete). A config-managed repository is refused: removing it from
// the configuration and pruning is the only way it goes, or the next
// reconcile would quietly bring it back.
func (r *Registry) DeleteRepository(ctx context.Context, name string) error {
	repo, err := r.GetRepository(ctx, name)
	if err != nil {
		return err
	}
	if repo.ManagedBy == model.ManagedByConfig {
		return fmt.Errorf("%w: remove %s from the configuration and apply with --prune", ErrConfigManaged, name)
	}
	n, err := r.q.DeleteRepositoryByName(ctx, name)
	if err != nil {
		if isForeignKeyViolation(err, "lifecycle_events_repo_id_fkey") {
			return fmt.Errorf("%w: %s has promotion events and cannot be deleted", ErrHasHistory, name)
		}
		return fmt.Errorf("deleting repository %s: %w", name, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PublishParams describes a file upload under a repository path.
type PublishParams struct {
	Repo      string
	Package   string
	Version   string
	Filename  string
	MediaType string
	// Metadata is stored on the files row as jsonb. Nil or empty becomes {}.
	Metadata json.RawMessage
	Body     io.Reader
	Size     int64 // negative means unknown

	// ExpectDigest, when set, is the digest the CAS must arrive at for these
	// bytes. It is the caller's own computation over the bytes it is handing
	// over, never a value a client claimed: a claim is checked against the
	// bytes at the edge, because a digest passed through here is also the
	// deduplication key.
	ExpectDigest model.Digest

	// Immutable rejects a second publish of the same filename with different
	// bytes (ErrFileExists), a filename already used by another version of the
	// same package, and a filename that was published and deleted (even with
	// identical bytes). Re-publishing identical bytes of a live file stays
	// idempotent.
	Immutable bool

	// Provenance is the publisher's claim about where these bytes came from.
	// It is recorded on the subject the publish creates, unverified (ADR 16).
	Provenance model.Provenance

	// ExclusiveVersion refuses a version that already exists, whatever the
	// bytes (ErrVersionExists). The check is the insert itself, so two
	// concurrent publishes of one version have exactly one winner.
	ExclusiveVersion bool

	// DistTag is the tag an npm publish moves to this version. It is required
	// for an npm repository, so no path can place a tarball without it. The
	// package's first publish also sets latest, whatever the tag says.
	DistTag string

	// Actor is recorded on the dist-tag events the publish writes.
	Actor string
}

// Publish stores bytes in the CAS and creates placement rows. A digest already
// in the CAS is not rewritten (I1); a new files row still places it.
func (r *Registry) Publish(ctx context.Context, p PublishParams) (model.File, error) {
	if err := validatePublishParams(p); err != nil {
		return model.File{}, err
	}
	mediaType := p.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	metadata := p.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage("{}")
	}

	repo, err := r.q.GetRepositoryByName(ctx, p.Repo)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.File{}, ErrNotFound
		}
		return model.File{}, fmt.Errorf("looking up repository %s: %w", p.Repo, err)
	}
	if repo.Type != string(model.RepoTypeHosted) {
		return model.File{}, fmt.Errorf("%w: %s is %s", ErrReadOnly, p.Repo, repo.Type)
	}
	npm := model.Protocol(repo.Protocol) == model.ProtocolNPM
	if npm && p.DistTag == "" {
		return model.File{}, fmt.Errorf("%w: an npm publish needs a dist-tag", ErrInvalidInput)
	}
	// A version is immutable (ADR 24). Without this a caller could replace its
	// bytes through the upsert below.
	if npm && !p.ExclusiveVersion {
		return model.File{}, fmt.Errorf("%w: an npm publish must not replace a version", ErrInvalidInput)
	}

	info, err := r.cas.Put(ctx, p.Body, p.ExpectDigest, p.Size)
	if err != nil {
		return model.File{}, fmt.Errorf("storing blob for %s/%s/%s/%s: %w", p.Repo, p.Package, p.Version, p.Filename, err)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return model.File{}, fmt.Errorf("beginning publish transaction: %w", err)
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

	normalized := NormalizeName(model.Protocol(repo.Protocol), p.Package)
	pkg, err := q.GetOrCreatePackage(ctx, db.GetOrCreatePackageParams{
		RepoID:         repo.ID,
		NormalizedName: normalized,
		DisplayName:    p.Package,
	})
	if err != nil {
		return model.File{}, fmt.Errorf("ensuring package %s: %w", p.Package, err)
	}
	// Package first, then versions and files: the order DeleteFile and the npm
	// dist-tag paths take, so none of them can wait on each other in a cycle.
	// GetOrCreatePackage's upsert already row-locks an existing package; this
	// lock makes the order explicit and also covers a package just created.
	// Holding it queues this publish behind a delete in flight, so the
	// tombstone checks below see that delete's committed work (SIN-64).
	if retiresNames(repo) {
		if _, err := q.LockPackage(ctx, pkg.ID); err != nil {
			return model.File{}, fmt.Errorf("locking package %s: %w", p.Package, err)
		}
	}

	var ver db.Version
	if p.ExclusiveVersion {
		ver, err = q.InsertVersionExclusive(ctx, db.InsertVersionExclusiveParams{
			PackageID: pkg.ID,
			Version:   p.Version,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return model.File{}, fmt.Errorf("%w: %s %s", ErrVersionExists, p.Package, p.Version)
			}
			return model.File{}, fmt.Errorf("creating version %s: %w", p.Version, err)
		}
		// A deleted version stays taken (SIN-52). The insert above waited for
		// any delete in flight on this version, so its tombstone is visible
		// here. Returning rolls the new version row back; the bytes stay in
		// the CAS unplaced for GC.
		retired, err := q.IsVersionRetired(ctx, db.IsVersionRetiredParams{PackageID: pkg.ID, Version: p.Version})
		if err != nil {
			return model.File{}, fmt.Errorf("checking retired versions of %s: %w", p.Package, err)
		}
		if retired {
			return model.File{}, fmt.Errorf("%w: %s %s was published and deleted; a version is never reused",
				ErrVersionExists, p.Package, p.Version)
		}
	} else {
		ver, err = q.GetOrCreateVersion(ctx, db.GetOrCreateVersionParams{
			PackageID: pkg.ID,
			Version:   p.Version,
		})
		if err != nil {
			return model.File{}, fmt.Errorf("ensuring version %s: %w", p.Version, err)
		}
	}

	placeParams := db.UpsertFileParams{
		VersionID:  ver.ID,
		BlobDigest: info.Digest.String(),
		Filename:   p.Filename,
		MediaType:  mediaType,
		Metadata:   []byte(metadata),
	}

	var file db.File
	if p.Immutable {
		// The files unique constraint is (version_id, filename), so the same
		// filename under a second version of one package would make the
		// version-less read path ambiguous. Reject it while still in the
		// transaction that would create it.
		others, err := q.CountPackageFilesWithFilename(ctx, db.CountPackageFilesWithFilenameParams{
			PackageID: pkg.ID,
			Filename:  p.Filename,
			VersionID: ver.ID,
		})
		if err != nil {
			return model.File{}, fmt.Errorf("checking existing placements of %s: %w", p.Filename, err)
		}
		if others > 0 {
			return model.File{}, fmt.Errorf("%w: %s is already published under another version of %s",
				ErrFileExists, p.Filename, p.Package)
		}

		file, err = q.InsertFileImmutable(ctx, db.InsertFileImmutableParams(placeParams))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return model.File{}, fmt.Errorf("%w: %s", ErrFileExists, p.Filename)
			}
			return model.File{}, fmt.Errorf("placing file %s: %w", p.Filename, err)
		}
	} else {
		file, err = q.UpsertFile(ctx, placeParams)
		if err != nil {
			return model.File{}, fmt.Errorf("placing file %s: %w", p.Filename, err)
		}
	}

	// A deleted PyPI filename stays taken, whichever version held it (SIN-52).
	// npm is checked by version above; OCI-protocol repositories keep their
	// v1.0 behaviour. This runs after the placement so the insert has waited
	// for a delete in flight on the same file.
	if model.Protocol(repo.Protocol) == model.ProtocolPyPI {
		retired, err := q.IsFilenameRetired(ctx, db.IsFilenameRetiredParams{PackageID: pkg.ID, Filename: p.Filename})
		if err != nil {
			return model.File{}, fmt.Errorf("checking retired files of %s: %w", p.Package, err)
		}
		if retired {
			return model.File{}, fmt.Errorf("%w: %s was published and deleted; a filename is never reused",
				ErrFileExists, p.Filename)
		}
	}

	kind := model.SubjectKindFile
	switch model.Protocol(repo.Protocol) {
	case model.ProtocolPyPI:
		kind = model.SubjectKindPyPIDist
	case model.ProtocolNPM:
		kind = model.SubjectKindNPMTarball
	case model.ProtocolOCI:
	}
	if err := r.recordSubject(ctx, q, repo, info.Digest, info.Size, kind, p.Provenance); err != nil {
		return model.File{}, err
	}

	if p.DistTag != "" {
		if err := moveDistTag(ctx, q, repo.Name, pkg, p.DistTag, ver, p.Actor); err != nil {
			return model.File{}, err
		}
		// Only the package's first version sets latest. After that a publish
		// moves its own tag alone, so deleting latest's version and then
		// publishing a prerelease cannot make the prerelease latest.
		versions, err := q.CountVersionsByPackage(ctx, pkg.ID)
		if err != nil {
			return model.File{}, fmt.Errorf("counting versions of %s: %w", p.Package, err)
		}
		if versions == 1 {
			if err := moveDistTag(ctx, q, repo.Name, pkg, latestTag, ver, p.Actor); err != nil {
				return model.File{}, err
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return model.File{}, fmt.Errorf("committing publish: %w", err)
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

// GetFile resolves a placed file and returns a reader. Placement is checked
// before every cas.Get (invariant I2).
func (r *Registry) GetFile(ctx context.Context, repo, pkg, version, filename string) (model.File, io.ReadCloser, error) {
	repoRow, err := r.q.GetRepositoryByName(ctx, repo)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.File{}, nil, ErrNotFound
		}
		return model.File{}, nil, fmt.Errorf("looking up repository %s: %w", repo, err)
	}
	file, err := r.q.GetFile(ctx, db.GetFileParams{
		Name:           repo,
		NormalizedName: NormalizeName(model.Protocol(repoRow.Protocol), pkg),
		Version:        version,
		Filename:       filename,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.File{}, nil, ErrNotFound
		}
		return model.File{}, nil, fmt.Errorf("resolving file: %w", err)
	}

	digest, err := model.ParseDigest(file.BlobDigest)
	if err != nil {
		return model.File{}, nil, fmt.Errorf("stored digest %q: %w", file.BlobDigest, err)
	}

	// I2: never serve by digest alone. The path lookup found a placement, but
	// every read still goes through CheckPlacement so a future path that only
	// has a digest cannot bypass it.
	if err := r.CheckPlacement(ctx, repo, digest); err != nil {
		return model.File{}, nil, err
	}

	rc, err := r.cas.Get(ctx, digest)
	if err != nil {
		return model.File{}, nil, fmt.Errorf("reading blob %s: %w", digest, err)
	}

	blob, err := r.q.GetBlob(ctx, digest.String())
	size := int64(0)
	if err == nil {
		size = blob.Size
	}

	return model.File{
		ID:        file.ID.String(),
		VersionID: file.VersionID.String(),
		Digest:    digest,
		Filename:  file.Filename,
		MediaType: file.MediaType,
		Size:      size,
	}, rc, nil
}

// CheckPlacement reports whether digest is linked into repo's placement graph.
// Every read path must call this before cas.Get* (invariant I2).
func (r *Registry) CheckPlacement(ctx context.Context, repo string, digest model.Digest) error {
	if !digest.Valid() {
		return fmt.Errorf("%w: digest is required", ErrInvalidInput)
	}
	placed, err := r.q.CheckPlacement(ctx, db.CheckPlacementParams{
		RepoName: repo,
		Digest:   digest.String(),
	})
	if err != nil {
		return fmt.Errorf("checking placement of %s in %s: %w", digest, repo, err)
	}
	if !placed {
		return ErrNotPlaced
	}
	return nil
}

// retiresNames reports whether a repository's published names are immutable
// (SIN-52): a hosted npm or PyPI repository. A proxy only mirrors upstream,
// and an OCI-protocol repository keeps its v1.0 behaviour; both must be able
// to place a file again, so neither keeps tombstones or takes the package
// lock for them.
func retiresNames(repo db.Repository) bool {
	if repo.Type != string(model.RepoTypeHosted) {
		return false
	}
	protocol := model.Protocol(repo.Protocol)
	return protocol == model.ProtocolNPM || protocol == model.ProtocolPyPI
}

// DeleteFile unlinks a placement. It never calls cas.Delete. actor is
// recorded on the dist-tag events a removed npm version leaves behind.
func (r *Registry) DeleteFile(ctx context.Context, repo, pkg, version, filename, actor string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning delete transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := r.q.WithTx(tx)

	repoRow, err := q.GetRepositoryByName(ctx, repo)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("looking up repository for delete: %w", err)
	}
	normalized := NormalizeName(model.Protocol(repoRow.Protocol), pkg)

	file, err := q.GetFile(ctx, db.GetFileParams{
		Name:           repo,
		NormalizedName: normalized,
		Version:        version,
		Filename:       filename,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("resolving file for delete: %w", err)
	}

	pkgRow, err := q.GetPackageByRepoAndName(ctx, db.GetPackageByRepoAndNameParams{
		RepoID:         repoRow.ID,
		NormalizedName: normalized,
	})
	if err != nil {
		return fmt.Errorf("looking up package for delete: %w", err)
	}

	versionID := file.VersionID

	// Lock the package before touching its rows, the order Publish and the npm
	// dist-tag paths use (SIN-64). A concurrent publish then waits here and
	// finds this delete's tombstone committed. Re-resolving is unnecessary: the
	// delete below reports a file another delete already removed as not found.
	// Only a repository that keeps tombstones takes the lock here. An npm proxy
	// keeps its earlier order, file then package, which is the order
	// evictGoneFiles takes; moving it first would pair the two the other way.
	npm := model.Protocol(repoRow.Protocol) == model.ProtocolNPM
	retires := retiresNames(repoRow)
	if retires {
		if _, err := q.LockPackage(ctx, pkgRow.ID); err != nil {
			return fmt.Errorf("locking package %s: %w", pkg, err)
		}
	}

	n, err := q.DeleteFile(ctx, db.DeleteFileParams{
		Name:           repo,
		NormalizedName: normalized,
		Version:        version,
		Filename:       filename,
	})
	if err != nil {
		return fmt.Errorf("unlinking file: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}

	// Dist-tags cascade with their version, so record the tags a removal takes
	// with it before the row goes. The history outlives the version (ADR 24).
	var strandedTags []string
	if npm {
		if !retires {
			if _, err := q.LockPackage(ctx, pkgRow.ID); err != nil {
				return fmt.Errorf("locking package %s: %w", pkg, err)
			}
		}
		strandedTags, err = q.ListDistTagsForVersion(ctx, versionID)
		if err != nil {
			return fmt.Errorf("listing dist-tags of version %s: %w", version, err)
		}
	}

	// A hosted npm or PyPI repository's published names are immutable, so a
	// deleted file keeps its version and filename taken (SIN-52). A proxy only
	// mirrors upstream, and an OCI-protocol repository keeps its v1.0 behaviour;
	// both must be able to place the file again.
	if retires {
		if err := q.InsertRetiredFile(ctx, db.InsertRetiredFileParams{
			PackageID: pkgRow.ID,
			Version:   version,
			Filename:  filename,
		}); err != nil {
			return fmt.Errorf("retiring %s: %w", filename, err)
		}
	}

	// Empty parent cleanup. Blob rows and CAS bytes stay (GC is later).
	removed, err := q.DeleteVersionIfEmpty(ctx, versionID)
	if err != nil {
		return fmt.Errorf("cleaning empty version: %w", err)
	}
	if removed > 0 {
		for _, tag := range strandedTags {
			if err := q.InsertDistTagEvent(ctx, db.InsertDistTagEventParams{
				Repository:  repo,
				Package:     pkgRow.DisplayName,
				Tag:         tag,
				FromVersion: textCursor(version),
				Actor:       actor,
			}); err != nil {
				return fmt.Errorf("recording removal of dist-tag %s: %w", tag, err)
			}
		}
	}
	// A hosted package row is the name's claim (I5). It outlives the name's last
	// file, or a proxy member could serve the name the moment its owner deleted
	// it. Deleting the repository is what releases a claim.
	if repoRow.Type != string(model.RepoTypeHosted) {
		if _, err := q.DeletePackageIfEmpty(ctx, pkgRow.ID); err != nil {
			return fmt.Errorf("cleaning empty package: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing delete: %w", err)
	}
	return nil
}

// ListPackagesParams controls cursor pagination over packages in a repo.
type ListPackagesParams struct {
	Repo  string
	After string
	Limit int32
}

// ListPackages returns packages in a repository ordered by normalized name. A
// name kept only as a claim, with no versions left, is not listed.
func (r *Registry) ListPackages(ctx context.Context, p ListPackagesParams) (Page[model.Package], error) {
	repo, err := r.q.GetRepositoryByName(ctx, p.Repo)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Page[model.Package]{}, ErrNotFound
		}
		return Page[model.Package]{}, fmt.Errorf("looking up repository %s: %w", p.Repo, err)
	}

	limit, err := pageLimit(p.Limit)
	if err != nil {
		return Page[model.Package]{}, err
	}
	rows, err := r.q.ListPackagesByRepo(ctx, db.ListPackagesByRepoParams{
		RepoID:    repo.ID,
		AfterName: textCursor(p.After),
		PageLimit: limit + 1,
	})
	if err != nil {
		return Page[model.Package]{}, fmt.Errorf("listing packages: %w", err)
	}

	page := Page[model.Package]{Items: make([]model.Package, 0, len(rows))}
	for i, row := range rows {
		if int32(i) >= limit {
			page.NextCursor = rows[limit-1].NormalizedName
			break
		}
		page.Items = append(page.Items, model.Package{
			ID:             row.ID.String(),
			RepoID:         row.RepoID.String(),
			NormalizedName: row.NormalizedName,
			DisplayName:    row.DisplayName,
		})
	}
	return page, nil
}

// ListVersionsParams controls cursor pagination over versions of a package.
type ListVersionsParams struct {
	Repo    string
	Package string
	After   string
	Limit   int32
}

// ListVersions returns versions of a package ordered by version string.
func (r *Registry) ListVersions(ctx context.Context, p ListVersionsParams) (Page[model.Version], error) {
	repo, err := r.q.GetRepositoryByName(ctx, p.Repo)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Page[model.Version]{}, ErrNotFound
		}
		return Page[model.Version]{}, fmt.Errorf("looking up repository %s: %w", p.Repo, err)
	}

	pkg, err := r.q.GetPackageByRepoAndName(ctx, db.GetPackageByRepoAndNameParams{
		RepoID:         repo.ID,
		NormalizedName: NormalizeName(model.Protocol(repo.Protocol), p.Package),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Page[model.Version]{}, ErrNotFound
		}
		return Page[model.Version]{}, fmt.Errorf("looking up package %s: %w", p.Package, err)
	}

	limit, err := pageLimit(p.Limit)
	if err != nil {
		return Page[model.Version]{}, err
	}
	rows, err := r.q.ListVersionsByPackage(ctx, db.ListVersionsByPackageParams{
		PackageID:    pkg.ID,
		AfterVersion: textCursor(p.After),
		PageLimit:    limit + 1,
	})
	if err != nil {
		return Page[model.Version]{}, fmt.Errorf("listing versions: %w", err)
	}
	// A package row kept as a claim is still no package to a reader. An OCI
	// image has no versions rows at all, so an empty list says nothing there.
	if len(rows) == 0 && p.After == "" && model.Protocol(repo.Protocol) != model.ProtocolOCI {
		return Page[model.Version]{}, ErrNotFound
	}

	page := Page[model.Version]{Items: make([]model.Version, 0, len(rows))}
	for i, row := range rows {
		if int32(i) >= limit {
			page.NextCursor = rows[limit-1].Version
			break
		}
		page.Items = append(page.Items, model.Version{
			ID:        row.ID.String(),
			PackageID: row.PackageID.String(),
			Version:   row.Version,
		})
	}
	return page, nil
}

func repositoryFromRow(row db.Repository) model.Repository {
	return repositoryFromColumns(row.ID, row.Name, row.Type, row.Protocol, row.Config, row.Locked, row.ManagedBy, row.CreatedAt)
}

func repositoryFromColumns(id uuid.UUID, name, typ, protocol string, cfg []byte, locked bool, managedBy string, created time.Time) model.Repository {
	rc := parseRepoConfig(cfg)
	return model.Repository{
		ID:        id.String(),
		Name:      name,
		Type:      model.RepoType(typ),
		Protocol:  model.Protocol(protocol),
		Locked:    locked,
		ManagedBy: model.ManagedBy(managedBy),
		CreatedAt: created,
		Namespace: rc.Namespace,
		Source:    rc.Source,
		Access:    rc.Access,
		Lifecycle: rc.Lifecycle,
	}
}

func pageLimit(limit int32) (int32, error) {
	const defaultLimit = 50
	const maxLimit = 200
	if limit == 0 {
		return defaultLimit, nil
	}
	if limit < 0 || limit > maxLimit {
		return 0, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidInput, maxLimit)
	}
	return limit, nil
}

// reservedNameSegments are the path markers protocol adapters split on. A
// repository whose name contains one of them mid-path is ambiguous with the
// route it would be served under: /pypi/team/packages/simple/foo/ could name
// the repository "team/packages" or a download from "team". Adapters resolve
// the ambiguity by taking the leftmost marker, which is only correct because a
// repository name can never contain one.
var reservedNameSegments = map[string]struct{}{
	"blobs":     {}, // OCI
	"manifests": {}, // OCI
	"tags":      {}, // OCI
	"uploads":   {}, // OCI
	"simple":    {}, // PyPI
	"packages":  {}, // PyPI
	"upload":    {}, // PyPI
	"-":         {}, // UI: /ui/r/{repository}/-/{page}
}

// ValidateRepoName applies the one repository-name rule (ADR 13): legal
// characters, well-formed slashes, and no segment an adapter splits paths on.
// The reconciler calls it statically, so `config check` reports a bad name
// without a database.
func ValidateRepoName(name string) error {
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == '/':
		default:
			return fmt.Errorf("%w: repository name %q has illegal character", ErrInvalidInput, name)
		}
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return fmt.Errorf("%w: repository name %q is malformed", ErrInvalidInput, name)
	}
	for _, segment := range strings.Split(name, "/") {
		if _, reserved := reservedNameSegments[strings.ToLower(segment)]; reserved {
			return fmt.Errorf("%w: repository name %q uses the reserved path segment %q",
				ErrInvalidInput, name, segment)
		}
	}
	return nil
}

func validatePublishParams(p PublishParams) error {
	if err := validatePlacementNames(p.Repo, p.Package, p.Version, p.Filename); err != nil {
		return err
	}
	if p.Body == nil {
		return fmt.Errorf("%w: body is required", ErrInvalidInput)
	}
	return nil
}

// validatePlacementNames checks the coordinates every file placement needs.
func validatePlacementNames(repo, pkg, version, filename string) error {
	if strings.TrimSpace(repo) == "" {
		return fmt.Errorf("%w: repository is required", ErrInvalidInput)
	}
	if strings.TrimSpace(pkg) == "" {
		return fmt.Errorf("%w: package name is required", ErrInvalidInput)
	}
	if strings.TrimSpace(version) == "" {
		return fmt.Errorf("%w: version is required", ErrInvalidInput)
	}
	if strings.TrimSpace(filename) == "" {
		return fmt.Errorf("%w: filename is required", ErrInvalidInput)
	}
	return nil
}

// NormalizeName returns the key a protocol stores a package under. npm names
// are case-sensitive and distinguish "-", "_", and ".", so folding them the
// PyPI way would merge packages that the protocol keeps apart.
func NormalizeName(p model.Protocol, name string) string {
	if p == model.ProtocolNPM {
		return strings.TrimSpace(name)
	}
	return normalizePackageName(name)
}

// normalizePackageName folds a package name the way PyPI does (PEP 503). For
// OCI and other protocols M2 still stores a stable key; adapters can refine
// this later without changing the uniqueness column.
func normalizePackageName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	b.Grow(len(name))
	prevDash := false
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
			prevDash = false
		case c == '-', c == '_', c == '.':
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isForeignKeyViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == constraint
}

func textCursor(value string) pgtype.Text {
	if value == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: value, Valid: true}
}
