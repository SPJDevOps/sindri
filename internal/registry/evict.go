package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
)

// Cache key prefixes. The proxy client writes remote_cache rows under these
// keys and retention parses them back, so both live here, in one place.
const (
	cacheKeyOCIManifestPrefix  = "oci:manifest:"
	cacheKeyPyPIIndexPrefix    = "pypi:index:"
	cacheKeyNPMPackumentPrefix = "npm:packument:"
)

// CacheKeyOCIManifest is the remote_cache key for an OCI tag or digest
// reference of image.
func CacheKeyOCIManifest(image, reference string) string {
	return cacheKeyOCIManifestPrefix + image + ":" + reference
}

// CacheKeyPyPIIndex is the remote_cache key for a project's simple index.
func CacheKeyPyPIIndex(project string) string {
	return cacheKeyPyPIIndexPrefix + project
}

// CacheKeyNPMPackument is the remote_cache key for a package's packument. The
// name is kept exact, since npm names are not folded (ADR 24).
func CacheKeyNPMPackument(name string) string {
	return cacheKeyNPMPackumentPrefix + name
}

// EvictResult reports one eviction pass.
type EvictResult struct {
	Hits   int `json:"hits"`
	Misses int `json:"misses"`
	// Skipped counts entries refreshed or locked by a concurrent fetch
	// between the listing and the eviction, which wins.
	Skipped int `json:"skipped"`
	// GoneFiles counts cached files removed because upstream stopped listing
	// them more than staleAfter ago (SIN-54).
	GoneFiles int `json:"goneFiles"`
}

const evictPage = 500

// EvictStaleCache removes proxy cache entries retention no longer keeps
// (ADR 19). A hit goes once it is older than its repository's TTL plus
// staleAfter, together with the proxy tag or files it justified, so the
// collector can then reclaim the bytes. A miss goes once its negative TTL has
// run out. A TTL of zero means "fresh forever" and is never evicted. A cached
// file that a refresh showed upstream no longer lists goes once it has been
// flagged longer than staleAfter, whatever its cache entry says.
//
// Each entry is evicted in its own transaction that re-reads it under a row
// lock, so a fetch that refreshed it in the meantime wins and it is kept.
// Hosted and virtual repositories are never touched: the queries join on the
// repository type, so this cannot remove a hosted tag whatever the key says.
func (r *Registry) EvictStaleCache(ctx context.Context, staleAfter time.Duration) (EvictResult, error) {
	var res EvictResult
	stale := int64(staleAfter / time.Second)
	if err := r.evictGoneFiles(ctx, stale, &res); err != nil {
		return res, err
	}
	seen := map[string]bool{}
	for {
		rows, err := r.q.ListEvictableCacheEntries(ctx, db.ListEvictableCacheEntriesParams{
			StaleSeconds: stale,
			PageLimit:    evictPage,
		})
		if err != nil {
			return res, fmt.Errorf("listing stale cache entries: %w", err)
		}
		fresh := 0
		for _, row := range rows {
			id := row.RepoID.String() + "\x00" + row.CacheKey
			if seen[id] {
				continue
			}
			seen[id] = true
			fresh++
			status, err := r.evictOne(ctx, row.RepoID, row.CacheKey, stale)
			if err != nil {
				return res, fmt.Errorf("evicting %s from %s: %w", row.CacheKey, row.RepoName, err)
			}
			switch status {
			case CacheHit:
				res.Hits++
			case CacheMiss:
				res.Misses++
			default:
				res.Skipped++
			}
		}
		// Entries a concurrent fetch holds come back on the next listing;
		// stop once a page brings nothing this pass has not already tried.
		if fresh == 0 || len(rows) < evictPage {
			return res, nil
		}
	}
}

// evictGoneFiles removes cached files that a refresh showed upstream no longer
// lists, once they have been flagged longer than the stale allowance, and the
// version and package rows that leaves empty. Rows only: the collector reclaims
// the bytes (ADR 19). The allowance runs from the flag, not from the cache
// entry, so a file is kept as long as an outage-length stale copy would be.
func (r *Registry) evictGoneFiles(ctx context.Context, stale int64, res *EvictResult) error {
	for {
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("beginning gone-file eviction: %w", err)
		}
		q := r.q.WithTx(tx)
		rows, err := q.DeleteGoneProxyFiles(ctx, db.DeleteGoneProxyFilesParams{
			StaleSeconds: stale, PageLimit: evictPage,
		})
		if err == nil {
			err = pruneEmptied(ctx, q, rows)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return fmt.Errorf("removing files gone upstream: %w", err)
		}
		res.GoneFiles += len(rows)
		if len(rows) < evictPage {
			return nil
		}
	}
}

// pruneEmptied removes the versions, then the packages, that deleting files
// left with nothing in them.
func pruneEmptied(ctx context.Context, q *db.Queries, gone []db.DeleteGoneProxyFilesRow) error {
	versions := map[uuid.UUID]bool{}
	packages := map[uuid.UUID]bool{}
	for _, g := range gone {
		versions[g.VersionID] = true
		packages[g.PackageID] = true
	}
	for id := range versions {
		if _, err := q.DeleteVersionIfEmpty(ctx, id); err != nil {
			return fmt.Errorf("deleting an emptied version: %w", err)
		}
	}
	for id := range packages {
		if _, err := q.DeletePackageIfEmpty(ctx, id); err != nil {
			return fmt.Errorf("deleting an emptied package: %w", err)
		}
	}
	return nil
}

// evictOne returns the status of the entry it evicted, or "" when a
// concurrent refresh or lock kept it.
func (r *Registry) evictOne(ctx context.Context, repoID uuid.UUID, key string, stale int64) (CacheStatus, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("beginning eviction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := r.q.WithTx(tx)

	entry, err := q.LockEvictableCacheEntry(ctx, db.LockEvictableCacheEntryParams{
		RepoID: repoID, CacheKey: key, StaleSeconds: stale,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("locking: %w", err)
	}

	if CacheStatus(entry.Status) == CacheHit {
		if err := evictPlacement(ctx, q, repoID, key); err != nil {
			return "", err
		}
	}
	if _, err := q.DeleteRemoteCache(ctx, db.DeleteRemoteCacheParams{RepoID: repoID, CacheKey: key}); err != nil {
		return "", fmt.Errorf("deleting the entry: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("committing eviction: %w", err)
	}
	return CacheStatus(entry.Status), nil
}

// evictPlacement removes what a cache hit justified in its proxy repository:
// the tag for an OCI tag reference, the cached files for a PyPI project or an
// npm package. A digest reference placed no tag; its manifest goes when nothing
// else reaches it, which is the collector's call.
func evictPlacement(ctx context.Context, q *db.Queries, repoID uuid.UUID, key string) error {
	switch {
	case strings.HasPrefix(key, cacheKeyOCIManifestPrefix):
		image, ref, ok := strings.Cut(strings.TrimPrefix(key, cacheKeyOCIManifestPrefix), ":")
		if !ok {
			return nil
		}
		if _, err := model.ParseDigest(ref); err == nil {
			return nil
		}
		if _, err := q.DeleteProxyTag(ctx, db.DeleteProxyTagParams{
			RepoID: repoID, NormalizedName: ociImagePackage(image), TagName: ref,
		}); err != nil {
			return fmt.Errorf("deleting proxy tag %s:%s: %w", image, ref, err)
		}
	case strings.HasPrefix(key, cacheKeyPyPIIndexPrefix):
		project := NormalizeName(model.ProtocolPyPI, strings.TrimPrefix(key, cacheKeyPyPIIndexPrefix))
		return evictCachedFiles(ctx, q, repoID, project)
	case strings.HasPrefix(key, cacheKeyNPMPackumentPrefix):
		name := NormalizeName(model.ProtocolNPM, strings.TrimPrefix(key, cacheKeyNPMPackumentPrefix))
		return evictCachedFiles(ctx, q, repoID, name)
	}
	return nil
}

// evictCachedFiles removes one package's cached files from a proxy repository,
// and the version and package rows that leaves empty. The query joins on the
// repository type, so a hosted package's rows, and its name claim, are out of
// reach.
func evictCachedFiles(ctx context.Context, q *db.Queries, repoID uuid.UUID, normalized string) error {
	versions, err := q.ListProxyPackageVersions(ctx, db.ListProxyPackageVersionsParams{
		RepoID: repoID, NormalizedName: normalized,
	})
	if err != nil {
		return fmt.Errorf("listing cached versions of %s: %w", normalized, err)
	}
	packages := map[uuid.UUID]bool{}
	for _, v := range versions {
		if _, err := q.DeleteFilesByVersion(ctx, v.VersionID); err != nil {
			return fmt.Errorf("deleting cached files of %s: %w", normalized, err)
		}
		if _, err := q.DeleteVersionIfEmpty(ctx, v.VersionID); err != nil {
			return fmt.Errorf("deleting an emptied version of %s: %w", normalized, err)
		}
		packages[v.PackageID] = true
	}
	for id := range packages {
		if _, err := q.DeletePackageIfEmpty(ctx, id); err != nil {
			return fmt.Errorf("deleting emptied package %s: %w", normalized, err)
		}
	}
	return nil
}
