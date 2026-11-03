package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
)

const (
	// latestTag is the dist-tag a plain install resolves. A package always
	// has one (ADR 24).
	latestTag = "latest"
)

// NPMVersion is one published version of an npm package and its tarball.
type NPMVersion struct {
	Version    string
	Deprecated string // empty when the version is not deprecated
	File       PackageFile
}

// NPMPackage is everything a packument is derived from.
type NPMPackage struct {
	Name     string
	Versions []NPMVersion
	DistTags map[string]string // tag to version
}

// RequireNPMRepository returns the named npm repository or ErrNotFound /
// ErrWrongProtocol. npm repositories are never auto-created.
func (r *Registry) RequireNPMRepository(ctx context.Context, name string) (model.Repository, error) {
	repo, err := r.requireNPMRepo(ctx, name)
	if err != nil {
		return model.Repository{}, err
	}
	return repositoryFromRow(repo), nil
}

func (r *Registry) requireNPMRepo(ctx context.Context, name string) (db.Repository, error) {
	repo, err := r.q.GetRepositoryByName(ctx, name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Repository{}, ErrNotFound
		}
		return db.Repository{}, fmt.Errorf("looking up repository %s: %w", name, err)
	}
	if repo.Protocol != string(model.ProtocolNPM) {
		return db.Repository{}, ErrWrongProtocol
	}
	return repo, nil
}

// npmPackageRow loads an npm package inside whatever transaction q belongs to.
func (r *Registry) npmPackageRow(ctx context.Context, q *db.Queries, repoName, name string) (db.Package, error) {
	repo, err := q.GetRepositoryByName(ctx, repoName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Package{}, ErrNotFound
		}
		return db.Package{}, fmt.Errorf("looking up repository %s: %w", repoName, err)
	}
	if repo.Protocol != string(model.ProtocolNPM) {
		return db.Package{}, ErrWrongProtocol
	}
	normalized := NormalizeName(model.ProtocolNPM, name)
	if normalized == "" {
		return db.Package{}, fmt.Errorf("%w: package name is required", ErrInvalidInput)
	}
	pkg, err := q.GetPackageByRepoAndName(ctx, db.GetPackageByRepoAndNameParams{
		RepoID:         repo.ID,
		NormalizedName: normalized,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Package{}, ErrNotFound
		}
		return db.Package{}, fmt.Errorf("looking up package %s: %w", name, err)
	}
	return pkg, nil
}

// GetNPMPackage returns every version and dist-tag of a package. It reads no
// bytes. ErrNotFound means no version exists.
func (r *Registry) GetNPMPackage(ctx context.Context, repo, name string) (NPMPackage, error) {
	pkg, err := r.npmPackageRow(ctx, r.q, repo, name)
	if err != nil {
		return NPMPackage{}, err
	}
	rows, err := r.q.ListNPMVersions(ctx, pkg.ID)
	if err != nil {
		return NPMPackage{}, fmt.Errorf("listing versions of %s: %w", name, err)
	}
	if len(rows) == 0 {
		return NPMPackage{}, ErrNotFound
	}

	out := NPMPackage{Name: pkg.DisplayName, Versions: make([]NPMVersion, 0, len(rows))}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if seen[row.Version] {
			continue // a version holds one tarball; ignore any stray second file
		}
		seen[row.Version] = true

		digest, err := model.ParseDigest(row.BlobDigest)
		if err != nil {
			return NPMPackage{}, fmt.Errorf("stored digest %q: %w", row.BlobDigest, err)
		}
		meta := json.RawMessage(row.Metadata)
		if len(meta) == 0 {
			meta = json.RawMessage("{}")
		}
		out.Versions = append(out.Versions, NPMVersion{
			Version:    row.Version,
			Deprecated: row.Deprecated.String,
			File: PackageFile{
				ID:        row.ID.String(),
				VersionID: row.VersionID.String(),
				Version:   row.Version,
				Digest:    digest,
				Filename:  row.Filename,
				MediaType: row.MediaType,
				Size:      row.BlobSize,
				Metadata:  meta,
			},
		})
	}

	tags, err := r.q.ListDistTags(ctx, pkg.ID)
	if err != nil {
		return NPMPackage{}, fmt.Errorf("listing dist-tags of %s: %w", name, err)
	}
	out.DistTags = make(map[string]string, len(tags))
	for _, t := range tags {
		out.DistTags[t.Tag] = t.Version
	}
	return out, nil
}

// GetNPMTarball resolves a placed tarball by package and filename and returns
// a reader. Placement is checked before cas.Get (I2).
func (r *Registry) GetNPMTarball(ctx context.Context, repo, name, filename string) (PackageFile, io.ReadCloser, error) {
	if _, err := r.requireNPMRepo(ctx, repo); err != nil {
		return PackageFile{}, nil, err
	}
	normalized := NormalizeName(model.ProtocolNPM, name)
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
		return PackageFile{}, nil, fmt.Errorf("resolving tarball %s/%s/%s: %w", repo, name, filename, err)
	}
	return r.openPlacedFile(ctx, repo, row)
}

// SetDistTag points tag at an existing version and records the move.
func (r *Registry) SetDistTag(ctx context.Context, repo, name, tag, version, actor string) error {
	if strings.TrimSpace(tag) == "" {
		return fmt.Errorf("%w: dist-tag is required", ErrInvalidInput)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning dist-tag transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := r.q.WithTx(tx)

	pkg, err := r.npmPackageRow(ctx, q, repo, name)
	if err != nil {
		return err
	}
	if _, err := q.LockPackage(ctx, pkg.ID); err != nil {
		return fmt.Errorf("locking package %s: %w", name, err)
	}
	ver, err := q.GetVersion(ctx, db.GetVersionParams{PackageID: pkg.ID, Version: version})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("looking up version %s: %w", version, err)
	}
	if err := moveDistTag(ctx, q, repo, pkg, tag, ver, actor); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing dist-tag: %w", err)
	}
	return nil
}

// RemoveDistTag deletes a tag and records the removal. latest cannot be
// removed, because a package always has one. A tag that does not exist is
// ErrNotFound.
func (r *Registry) RemoveDistTag(ctx context.Context, repo, name, tag, actor string) error {
	if tag == latestTag {
		return fmt.Errorf("%w: the %s dist-tag cannot be removed", ErrInvalidInput, latestTag)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning dist-tag transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := r.q.WithTx(tx)

	pkg, err := r.npmPackageRow(ctx, q, repo, name)
	if err != nil {
		return err
	}
	if _, err := q.LockPackage(ctx, pkg.ID); err != nil {
		return fmt.Errorf("locking package %s: %w", name, err)
	}
	from, err := q.GetDistTagVersion(ctx, db.GetDistTagVersionParams{PackageID: pkg.ID, Tag: tag})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("reading dist-tag %s: %w", tag, err)
	}
	removed, err := q.DeleteDistTag(ctx, db.DeleteDistTagParams{PackageID: pkg.ID, Tag: tag})
	if err != nil {
		return fmt.Errorf("removing dist-tag %s: %w", tag, err)
	}
	if removed == 0 {
		return ErrNotFound
	}
	if err := q.InsertDistTagEvent(ctx, db.InsertDistTagEventParams{
		Repository:  repo,
		Package:     pkg.DisplayName,
		Tag:         tag,
		FromVersion: textCursor(from),
		Actor:       actor,
	}); err != nil {
		return fmt.Errorf("recording removal of dist-tag %s: %w", tag, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing dist-tag removal: %w", err)
	}
	return nil
}

// SetDeprecations sets the deprecation message of each named version; an empty
// message clears it, and nil leaves the version as it is. It is all or nothing:
// one unknown version, even with nil, refuses the whole call with ErrNotFound and
// writes nothing.
func (r *Registry) SetDeprecations(ctx context.Context, repo, name string, messages map[string]*string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning deprecation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := r.q.WithTx(tx)

	pkg, err := r.npmPackageRow(ctx, q, repo, name)
	if err != nil {
		return err
	}
	for version, message := range messages {
		ver, err := q.GetVersion(ctx, db.GetVersionParams{PackageID: pkg.ID, Version: version})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: %s has no version %s", ErrNotFound, name, version)
			}
			return fmt.Errorf("looking up version %s: %w", version, err)
		}
		if message == nil {
			continue
		}
		if _, err := q.SetVersionDeprecated(ctx, db.SetVersionDeprecatedParams{
			ID:         ver.ID,
			Deprecated: pgtype.Text{String: *message, Valid: *message != ""},
		}); err != nil {
			return fmt.Errorf("deprecating version %s: %w", version, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing deprecations: %w", err)
	}
	return nil
}

// moveDistTag points tag at ver and appends the event. A tag already there
// changes nothing and records nothing.
func moveDistTag(ctx context.Context, q *db.Queries, repoName string, pkg db.Package, tag string, ver db.Version, actor string) error {
	var from pgtype.Text
	prev, err := q.GetDistTagVersion(ctx, db.GetDistTagVersionParams{PackageID: pkg.ID, Tag: tag})
	switch {
	case err == nil:
		if prev == ver.Version {
			return nil
		}
		from = textCursor(prev)
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("reading dist-tag %s: %w", tag, err)
	}
	if err := q.UpsertDistTag(ctx, db.UpsertDistTagParams{PackageID: pkg.ID, Tag: tag, VersionID: ver.ID}); err != nil {
		return fmt.Errorf("setting dist-tag %s: %w", tag, err)
	}
	if err := q.InsertDistTagEvent(ctx, db.InsertDistTagEventParams{
		Repository:  repoName,
		Package:     pkg.DisplayName,
		Tag:         tag,
		FromVersion: from,
		ToVersion:   textCursor(ver.Version),
		Actor:       actor,
	}); err != nil {
		return fmt.Errorf("recording dist-tag %s: %w", tag, err)
	}
	return nil
}
