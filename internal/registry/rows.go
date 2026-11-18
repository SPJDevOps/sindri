package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	// Current are the digests of the versions that count as current: an OCI
	// image's every tag target, an npm package's latest version, a PyPI
	// project's highest version. They are placed in this repository: they came
	// from its own tag and file rows.
	Current []model.Digest
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

	versions, err := r.q.ListVersionDigestsForPackages(ctx, ids)
	if err != nil {
		return PackageRows{}, fmt.Errorf("listing versions: %w", err)
	}
	protocol := model.Protocol(repo.Protocol)
	byPackage := make(map[uuid.UUID]map[string][]string, len(ids))
	for _, v := range versions {
		if byPackage[v.PackageID] == nil {
			byPackage[v.PackageID] = map[string][]string{}
		}
		byPackage[v.PackageID][v.Version] = v.Digests
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
	for id, i := range index {
		have := byPackage[id]
		names := make([]string, 0, len(have))
		for v := range have {
			names = append(names, v)
		}
		latest := CurrentVersion(protocol, dist[id], names)
		row := &out.Rows[i]
		row.Latest = latest
		for _, d := range have[latest] {
			digest, err := model.ParseDigest(d)
			if err != nil {
				return PackageRows{}, fmt.Errorf("stored digest %q: %w", d, err)
			}
			row.Current = append(row.Current, digest)
		}
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
	params := db.ListPackageRowsOCIParams{
		RepoID: repo.ID, NameFilter: p.NameContains, ByPublished: p.Sort == SortByPublished,
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
	tags, err := r.q.ListTagSummariesForPackages(ctx, ids)
	if err != nil {
		return PackageRows{}, fmt.Errorf("summarising tags: %w", err)
	}
	for _, t := range tags {
		row := &out.Rows[index[t.PackageID]]
		row.Latest = t.LatestTag
		for _, d := range t.Digests {
			digest, err := model.ParseDigest(d)
			if err != nil {
				return PackageRows{}, fmt.Errorf("stored digest %q: %w", d, err)
			}
			row.Current = append(row.Current, digest)
		}
	}
	return out, nil
}

// timeOf reads an aggregate timestamp sqlc could not type: a time, or nil when
// the aggregate had nothing to take the maximum of.
func timeOf(v any) time.Time {
	t, _ := v.(time.Time)
	return t
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
