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

	// ErrFileExists means an immutable publish would replace a filename that
	// is already placed with different bytes. A published artifact is the
	// thing consumers pinned a hash of, so it does not change under them.
	ErrFileExists = errors.New("file already published with different content")

	// ErrHasHistory means a delete would remove a repository the lifecycle
	// ledger refers to. The ledger is append-only, so the repository stays.
	ErrHasHistory = errors.New("repository has lifecycle history")
)

// Registry owns placement metadata and gates every blob read.
type Registry struct {
	pool    *pgxpool.Pool
	q       *db.Queries
	cas     *cas.Store
	backend string
}

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

// DeleteRepository removes a repository and cascades its placements in the
// database. Blob bytes are left for garbage collection (never cas.Delete).
func (r *Registry) DeleteRepository(ctx context.Context, name string) error {
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
	// bytes (ErrFileExists), and a filename already used by another version
	// of the same package. Re-publishing identical bytes stays idempotent.
	Immutable bool

	// Provenance is the publisher's claim about where these bytes came from.
	// It is recorded on the subject the publish creates, unverified (ADR 16).
	Provenance model.Provenance
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

	kind := model.SubjectKindFile
	if repo.Protocol == string(model.ProtocolPyPI) {
		kind = model.SubjectKindPyPIDist
	}
	if err := recordSubject(ctx, q, repo, info.Digest, info.Size, kind, p.Provenance); err != nil {
		return model.File{}, err
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
	file, err := r.q.GetFile(ctx, db.GetFileParams{
		Name:           repo,
		NormalizedName: normalizePackageName(pkg),
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

// DeleteFile unlinks a placement. It never calls cas.Delete.
func (r *Registry) DeleteFile(ctx context.Context, repo, pkg, version, filename string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning delete transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := r.q.WithTx(tx)

	file, err := q.GetFile(ctx, db.GetFileParams{
		Name:           repo,
		NormalizedName: normalizePackageName(pkg),
		Version:        version,
		Filename:       filename,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("resolving file for delete: %w", err)
	}

	repoRow, err := q.GetRepositoryByName(ctx, repo)
	if err != nil {
		return fmt.Errorf("looking up repository for delete: %w", err)
	}
	pkgRow, err := q.GetPackageByRepoAndName(ctx, db.GetPackageByRepoAndNameParams{
		RepoID:         repoRow.ID,
		NormalizedName: normalizePackageName(pkg),
	})
	if err != nil {
		return fmt.Errorf("looking up package for delete: %w", err)
	}

	versionID := file.VersionID

	n, err := q.DeleteFile(ctx, db.DeleteFileParams{
		Name:           repo,
		NormalizedName: normalizePackageName(pkg),
		Version:        version,
		Filename:       filename,
	})
	if err != nil {
		return fmt.Errorf("unlinking file: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}

	// Empty parent cleanup. Blob rows and CAS bytes stay (GC is later).
	if _, err := q.DeleteVersionIfEmpty(ctx, versionID); err != nil {
		return fmt.Errorf("cleaning empty version: %w", err)
	}
	if _, err := q.DeletePackageIfEmpty(ctx, pkgRow.ID); err != nil {
		return fmt.Errorf("cleaning empty package: %w", err)
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

// ListPackages returns packages in a repository ordered by normalized name.
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
		NormalizedName: normalizePackageName(p.Package),
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
	return model.Repository{
		ID:        row.ID.String(),
		Name:      row.Name,
		Type:      model.RepoType(row.Type),
		Protocol:  model.Protocol(row.Protocol),
		Locked:    row.Locked,
		ManagedBy: model.ManagedBy(row.ManagedBy),
		CreatedAt: row.CreatedAt,
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
}

func validateRepoName(name string) error {
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
