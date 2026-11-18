package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
)

// PackageSort orders a repository's package rows.
type PackageSort string

const (
	// SortByName orders by normalized name, ascending.
	SortByName PackageSort = "name"
	// SortByPublished orders by last publish, newest first, packages with no
	// dated publish last, then by name.
	SortByPublished PackageSort = "published"
)

// PackageRowsParams selects one page of a repository's package rows.
type PackageRowsParams struct {
	Repo string
	// NameContains keeps packages whose normalized name contains it, ignoring
	// case. Empty keeps all.
	NameContains string
	Sort         PackageSort
	Limit        int32
	Offset       int32
	// WithFindings fills PackageRow.Findings. Hosted rows want it; proxy
	// content has no scans and is never summarised as if it did.
	WithFindings bool
}

// PackageRow is one package of a repository as its list shows it.
type PackageRow struct {
	Name        string // normalized, for links
	DisplayName string
	// Versions counts versions, or tags for an OCI image.
	Versions int64
	// LastPublished is the newest publish time of the package, or the zero time
	// when none is dated. An OCI image's is the newest first-publish of a tag's
	// target; proxy content has none.
	LastPublished time.Time
	// Latest is the version a plain install resolves, or an OCI image's most
	// recently first-published tag; empty when there is none to name.
	Latest string
	// Findings is what the newest Sindri scans of the current versions found,
	// when PackageRowsParams.WithFindings asked. Current means an OCI image's
	// every tag target, an npm package's latest version, a PyPI project's
	// highest version.
	Findings PackageFindings
}

// PackageFindings adds up the newest Sindri scan per platform of each current
// subject of a package. Only scans Sindri's own worker produced count (ADR 29).
type PackageFindings struct {
	// Subjects is how many distinct digests are current; Scanned how many of
	// them have a Sindri scan. A package is only known clean when they match.
	Subjects, Scanned int64
	Critical, High    int64
	Medium, Low       int64
	Unknown           int64
}

// AllScanned reports whether every current subject has a Sindri scan, and
// there is at least one.
func (f PackageFindings) AllScanned() bool { return f.Subjects > 0 && f.Scanned == f.Subjects }

// Counts returns the findings by Trivy severity name.
func (f PackageFindings) Counts() map[string]int {
	return map[string]int{
		"CRITICAL": int(f.Critical), "HIGH": int(f.High), "MEDIUM": int(f.Medium),
		"LOW": int(f.Low), "UNKNOWN": int(f.Unknown),
	}
}

func findingsOf(subjects, scanned, critical, high, medium, low, unknown int64) PackageFindings {
	return PackageFindings{Subjects: subjects, Scanned: scanned, Critical: critical, High: high, Medium: medium, Low: low, Unknown: unknown}
}

// PackageRows is one page of PackageRow and the number of matching packages
// across all pages.
type PackageRows struct {
	Rows  []PackageRow
	Total int64
}

// ListPackageRows returns one page of a repository's packages with the figures
// the repository page shows, in a constant number of queries. It reads no
// bytes and decides nothing about who may see the repository.
func (r *Registry) ListPackageRows(ctx context.Context, p PackageRowsParams) (PackageRows, error) {
	repo, err := r.q.GetRepositoryByName(ctx, p.Repo)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PackageRows{}, ErrNotFound
		}
		return PackageRows{}, fmt.Errorf("looking up repository %s: %w", p.Repo, err)
	}
	limit, err := pageLimit(p.Limit)
	if err != nil {
		return PackageRows{}, err
	}
	if p.Offset < 0 {
		return PackageRows{}, fmt.Errorf("%w: offset must not be negative", ErrInvalidInput)
	}
	protocol := model.Protocol(repo.Protocol)
	if protocol == model.ProtocolOCI {
		return r.ociPackageRows(ctx, repo, p, limit)
	}
	return r.versionedPackageRows(ctx, repo, p, limit)
}

func (r *Registry) versionedPackageRows(ctx context.Context, repo db.Repository, p PackageRowsParams, limit int32) (PackageRows, error) {
	params := db.ListPackageRowsVersionedParams{
		RepoID: repo.ID, NameFilter: p.NameContains, ByPublished: p.Sort == SortByPublished,
		PageLimit: limit, PageOffset: p.Offset,
	}
	rows, err := r.q.ListPackageRowsVersioned(ctx, params)
	if err != nil {
		return PackageRows{}, fmt.Errorf("listing package rows: %w", err)
	}
	if len(rows) == 0 && p.Offset > 0 {
		// A page past the end: the total still has to come from somewhere.
		params.PageOffset, params.PageLimit = 0, 1
		first, err := r.q.ListPackageRowsVersioned(ctx, params)
		if err != nil {
			return PackageRows{}, fmt.Errorf("listing package rows: %w", err)
		}
		if len(first) > 0 {
			return PackageRows{Total: first[0].Total}, nil
		}
	}
	out := PackageRows{Rows: make([]PackageRow, 0, len(rows))}
	ids := make([]uuid.UUID, 0, len(rows))
	index := make(map[uuid.UUID]int, len(rows))
	for _, row := range rows {
		out.Total = row.Total
		index[row.ID] = len(out.Rows)
		ids = append(ids, row.ID)
		out.Rows = append(out.Rows, PackageRow{
			Name: row.NormalizedName, DisplayName: row.DisplayName,
			Versions: row.VersionCount, LastPublished: timeOf(row.LastPublishedAt),
		})
	}
	if len(ids) == 0 {
		return out, nil
	}

	versions, err := r.q.ListVersionsForPackages(ctx, ids)
	if err != nil {
		return PackageRows{}, fmt.Errorf("listing versions: %w", err)
	}
	protocol := model.Protocol(repo.Protocol)
	byPackage := make(map[uuid.UUID][]string, len(ids))
	for _, v := range versions {
		byPackage[v.PackageID] = append(byPackage[v.PackageID], v.Version)
	}
	dist := map[uuid.UUID]string{}
	if protocol == model.ProtocolNPM {
		tags, err := r.q.ListLatestDistTags(ctx, ids)
		if err != nil {
			return PackageRows{}, fmt.Errorf("listing dist-tags: %w", err)
		}
		for _, t := range tags {
			dist[t.PackageID] = t.Version
		}
	}
	var withVersion []uuid.UUID
	var picked []string
	for id, i := range index {
		latest := CurrentVersion(protocol, dist[id], byPackage[id])
		out.Rows[i].Latest = latest
		if latest != "" {
			withVersion = append(withVersion, id)
			picked = append(picked, latest)
		}
	}
	if !p.WithFindings || len(withVersion) == 0 {
		return out, nil
	}
	found, err := r.q.PackageFindingsVersioned(ctx, db.PackageFindingsVersionedParams{PackageIds: withVersion, Versions: picked})
	if err != nil {
		return PackageRows{}, fmt.Errorf("summarising findings: %w", err)
	}
	for _, f := range found {
		out.Rows[index[f.PackageID]].Findings = findingsOf(f.Subjects, f.Scanned, f.Critical, f.High, f.Medium, f.Low, f.Unknown)
	}
	return out, nil
}

// CurrentVersion is the version a repository row calls current: the version
// the "latest" dist-tag names when it names one that exists (npm), else the
// highest version under the protocol's ordering (semver for npm, PEP 440 for
// PyPI). It is "" when there are no versions.
func CurrentVersion(p model.Protocol, latestTag string, versions []string) string {
	for _, v := range versions {
		if v != "" && v == latestTag {
			return latestTag
		}
	}
	return model.HighestVersion(p, versions)
}

func (r *Registry) ociPackageRows(ctx context.Context, repo db.Repository, p PackageRowsParams, limit int32) (PackageRows, error) {
	// Only a hosted repository's rows read subjects: a proxy page must not learn
	// the push time or tags of a hosted repository holding the same digest.
	hosted := model.RepoType(repo.Type) == model.RepoTypeHosted
	params := db.ListPackageRowsOCIParams{
		Hosted: hosted, RepoID: repo.ID, NameFilter: p.NameContains, ByPublished: p.Sort == SortByPublished,
		PageLimit: limit, PageOffset: p.Offset,
	}
	rows, err := r.q.ListPackageRowsOCI(ctx, params)
	if err != nil {
		return PackageRows{}, fmt.Errorf("listing package rows: %w", err)
	}
	if len(rows) == 0 && p.Offset > 0 {
		params.PageOffset, params.PageLimit = 0, 1
		first, err := r.q.ListPackageRowsOCI(ctx, params)
		if err != nil {
			return PackageRows{}, fmt.Errorf("listing package rows: %w", err)
		}
		if len(first) > 0 {
			return PackageRows{Total: first[0].Total}, nil
		}
	}
	out := PackageRows{Rows: make([]PackageRow, 0, len(rows))}
	ids := make([]uuid.UUID, 0, len(rows))
	index := make(map[uuid.UUID]int, len(rows))
	for _, row := range rows {
		out.Total = row.Total
		index[row.ID] = len(out.Rows)
		ids = append(ids, row.ID)
		out.Rows = append(out.Rows, PackageRow{
			Name: row.NormalizedName, DisplayName: row.DisplayName,
			Versions: row.TagCount, LastPublished: timeOf(row.LastPublishedAt),
		})
	}
	if len(ids) == 0 {
		return out, nil
	}
	tags, err := r.q.ListLatestTagsForPackages(ctx, db.ListLatestTagsForPackagesParams{PackageIds: ids, Hosted: hosted})
	if err != nil {
		return PackageRows{}, fmt.Errorf("finding latest tags: %w", err)
	}
	for _, t := range tags {
		out.Rows[index[t.PackageID]].Latest = t.LatestTag
	}
	if !p.WithFindings {
		return out, nil
	}
	found, err := r.q.PackageFindingsOCI(ctx, ids)
	if err != nil {
		return PackageRows{}, fmt.Errorf("summarising findings: %w", err)
	}
	for _, f := range found {
		out.Rows[index[f.PackageID]].Findings = findingsOf(f.Subjects, f.Scanned, f.Critical, f.High, f.Medium, f.Low, f.Unknown)
	}
	return out, nil
}

// timeOf is the time of a nullable column, the zero time when it is null.
func timeOf(v pgtype.Timestamptz) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return v.Time
}

// RemoteCacheStats is what a proxy repository holds.
type RemoteCacheStats struct {
	// Entries counts the cached upstream answers: metadata documents, tags, and
	// manifests the proxy fetched successfully. A recorded miss is not one.
	Entries int64
	// LastFetched is the last successful fetch, or the zero time when the proxy
	// has fetched nothing.
	LastFetched time.Time
}

// RemoteCacheStatsFor reports a proxy repository's cache figures. Nothing here
// contacts the upstream.
func (r *Registry) RemoteCacheStatsFor(ctx context.Context, repo string) (RemoteCacheStats, error) {
	row, err := r.q.RemoteCacheStats(ctx, repo)
	if err != nil {
		return RemoteCacheStats{}, fmt.Errorf("reading cache figures of %s: %w", repo, err)
	}
	return RemoteCacheStats{Entries: row.Entries, LastFetched: timeOf(row.LastFetchedAt)}, nil
}
