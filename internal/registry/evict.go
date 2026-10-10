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
	cacheKeyOCIManifestPrefix = "oci:manifest:"
	cacheKeyPyPIIndexPrefix   = "pypi:index:"
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

// EvictResult reports one eviction pass.
type EvictResult struct {
	Hits   int `json:"hits"`
	Misses int `json:"misses"`
	// Skipped counts entries refreshed or locked by a concurrent fetch
	// between the listing and the eviction, which wins.
	Skipped int `json:"skipped"`
}

const evictPage = 500

// EvictStaleCache removes proxy cache entries retention no longer keeps
// (ADR 19). A hit goes once it is older than its repository's TTL plus
// staleAfter, together with the proxy tag or files it justified, so the
// collector can then reclaim the bytes. A miss goes once its negative TTL has
// run out. A TTL of zero means "fresh forever" and is never evicted.
//
// Each entry is evicted in its own transaction that re-reads it under a row
// lock, so a fetch that refreshed it in the meantime wins and it is kept.
// Hosted and virtual repositories are never touched: the queries join on the
// repository type, so this cannot remove a hosted tag whatever the key says.
func (r *Registry) EvictStaleCache(ctx context.Context, staleAfter time.Duration) (EvictResult, error) {
	var res EvictResult
	stale := int64(staleAfter / time.Second)
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
// the tag for an OCI tag reference, the cached files for a PyPI project. A
// digest reference placed no tag; its manifest goes when nothing else reaches
// it, which is the collector's call.
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
		project := normalizePackageName(strings.TrimPrefix(key, cacheKeyPyPIIndexPrefix))
		versions, err := q.ListProxyPackageVersions(ctx, db.ListProxyPackageVersionsParams{
			RepoID: repoID, NormalizedName: project,
		})
		if err != nil {
			return fmt.Errorf("listing cached versions of %s: %w", project, err)
		}
		packages := map[uuid.UUID]bool{}
		for _, v := range versions {
			if _, err := q.DeleteFilesByVersion(ctx, v.VersionID); err != nil {
				return fmt.Errorf("deleting cached files of %s: %w", project, err)
			}
			if _, err := q.DeleteVersionIfEmpty(ctx, v.VersionID); err != nil {
				return fmt.Errorf("deleting an emptied version of %s: %w", project, err)
			}
			packages[v.PackageID] = true
		}
		for id := range packages {
			if _, err := q.DeletePackageIfEmpty(ctx, id); err != nil {
				return fmt.Errorf("deleting emptied package %s: %w", project, err)
			}
		}
	}
	return nil
}
