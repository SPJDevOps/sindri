//go:build integration

package registry_test

import (
	"encoding/json"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/testsupport"
)

const staleAfter = time.Hour

type evictFixture struct {
	t   *testing.T
	db  *db.DB
	cas *cas.Store
	reg *registry.Registry
}

func newEvictFixture(t *testing.T) *evictFixture {
	t.Helper()
	database := testsupport.MigratedDB(t)
	store := cas.New(memstore.New(), cas.WithReserver(db.NewBlobReserver(database, "memory")))
	return &evictFixture{t: t, db: database, cas: store, reg: registry.New(database, store, "memory")}
}

func (f *evictFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Pool().Exec(f.t.Context(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *evictFixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.Pool().QueryRow(f.t.Context(), sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (f *evictFixture) proxy(name string, protocol model.Protocol, ttl, negativeTTL int32) {
	f.t.Helper()
	if _, err := f.reg.CreateRepository(f.t.Context(), registry.CreateRepositoryParams{
		Name: name, Type: model.RepoTypeProxy, Protocol: protocol,
		UpstreamURL: "https://upstream.invalid", TTLSeconds: ttl, NegativeTTL: negativeTTL,
	}); err != nil {
		f.t.Fatalf("CreateRepository %s: %v", name, err)
	}
}

// fetchedAgo moves a cache entry's fetch time into the past.
func (f *evictFixture) fetchedAgo(key string, ago time.Duration) {
	f.t.Helper()
	f.exec(`UPDATE remote_cache SET fetched_at = now() - make_interval(secs => $2) WHERE cache_key = $1`,
		key, ago.Seconds())
}

func (f *evictFixture) cachedTag(repo, image, tag string) string {
	f.t.Helper()
	man := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest}
	man.SchemaVersion = 2
	body, _ := json.Marshal(man)
	placed, err := f.reg.PlaceCachedManifest(f.t.Context(), repo, image, tag, ocispec.MediaTypeImageManifest, body)
	if err != nil {
		f.t.Fatalf("PlaceCachedManifest: %v", err)
	}
	key := registry.CacheKeyOCIManifest(image, tag)
	if err := f.reg.PutCacheEntry(f.t.Context(), repo, key, registry.CacheHit, placed.Digest.String()); err != nil {
		f.t.Fatalf("PutCacheEntry: %v", err)
	}
	return key
}

func (f *evictFixture) evict() registry.EvictResult {
	f.t.Helper()
	res, err := f.reg.EvictStaleCache(f.t.Context(), staleAfter)
	if err != nil {
		f.t.Fatalf("EvictStaleCache: %v", err)
	}
	return res
}

func (f *evictFixture) entries(key string) int {
	return f.count(`SELECT count(*) FROM remote_cache WHERE cache_key = $1`, key)
}

func (f *evictFixture) tags(name string) int {
	return f.count(`SELECT count(*) FROM tags WHERE name = $1`, name)
}

func TestEvictProxyCache_StaleHitAndItsProxyTagAreEvicted(t *testing.T) {
	f := newEvictFixture(t)
	f.proxy("hub", model.ProtocolOCI, 60, 0)
	key := f.cachedTag("hub", "library/alpine", "3.20")
	f.fetchedAgo(key, time.Minute+staleAfter+time.Minute)

	res := f.evict()
	if res.Hits != 1 || f.entries(key) != 0 || f.tags("3.20") != 0 {
		t.Fatalf("evict = %+v, entry %d, tag %d; want the hit and its tag gone", res, f.entries(key), f.tags("3.20"))
	}
}

// Within the stale allowance an upstream outage still serves the cached copy,
// so the entry must still be there to serve.
func TestEvictProxyCache_StaleHitWithinTheAllowanceIsKept(t *testing.T) {
	f := newEvictFixture(t)
	f.proxy("hub", model.ProtocolOCI, 60, 0)
	key := f.cachedTag("hub", "library/alpine", "3.20")
	f.fetchedAgo(key, time.Minute+staleAfter/2)

	if res := f.evict(); res.Hits != 0 || f.entries(key) != 1 || f.tags("3.20") != 1 {
		t.Fatalf("evict = %+v; want a hit inside its stale allowance kept", res)
	}
}

func TestEvictProxyCache_TTLZeroNeverEvicts(t *testing.T) {
	f := newEvictFixture(t)
	f.proxy("hub", model.ProtocolOCI, 0, 0)
	key := f.cachedTag("hub", "library/alpine", "3.20")
	f.fetchedAgo(key, 10*365*24*time.Hour)

	if res := f.evict(); res.Hits != 0 || f.entries(key) != 1 {
		t.Fatalf("evict = %+v; a TTL of zero means fresh forever", res)
	}
}

func TestEvictProxyCache_ExpiredNegativeEntriesAreDeletedFreshKept(t *testing.T) {
	f := newEvictFixture(t)
	f.proxy("hub", model.ProtocolOCI, 60, 30)
	for _, ref := range []string{"gone", "recent"} {
		if err := f.reg.PutCacheEntry(t.Context(), "hub", registry.CacheKeyOCIManifest("x", ref), registry.CacheMiss, ""); err != nil {
			t.Fatalf("PutCacheEntry: %v", err)
		}
	}
	f.fetchedAgo(registry.CacheKeyOCIManifest("x", "gone"), time.Hour)

	res := f.evict()
	if res.Misses != 1 || f.entries(registry.CacheKeyOCIManifest("x", "gone")) != 0 {
		t.Fatalf("evict = %+v; want the expired miss gone", res)
	}
	if f.entries(registry.CacheKeyOCIManifest("x", "recent")) != 1 {
		t.Fatal("a miss inside its negative TTL was evicted")
	}
}

func TestEvictProxyCache_RefreshDuringEvictionWins(t *testing.T) {
	f := newEvictFixture(t)
	f.proxy("hub", model.ProtocolOCI, 60, 0)
	key := f.cachedTag("hub", "library/alpine", "3.20")
	f.fetchedAgo(key, 2*staleAfter)

	// A fetch is refreshing the entry and holds its row.
	tx, err := f.db.Pool().Begin(t.Context())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE remote_cache SET fetched_at = now() WHERE cache_key = $1`, key); err != nil {
		t.Fatalf("refreshing: %v", err)
	}
	if res := f.evict(); res.Hits != 0 || res.Skipped != 1 {
		_ = tx.Rollback(t.Context())
		t.Fatalf("evict while a refresh held the entry = %+v, want it skipped", res)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if res := f.evict(); res.Hits != 0 || f.entries(key) != 1 || f.tags("3.20") != 1 {
		t.Fatalf("evict after the refresh = %+v; want the refreshed entry kept", res)
	}
}

func TestEvictProxyCache_StaleIndexHitEvictsTheProjectsCachedFiles(t *testing.T) {
	f := newEvictFixture(t)
	ctx := t.Context()
	f.proxy("pyproxy", model.ProtocolPyPI, 60, 0)
	index, err := f.reg.StoreMetadataBlob(ctx, []byte(`{"name":"demo","files":[]}`))
	if err != nil {
		t.Fatalf("StoreMetadataBlob: %v", err)
	}
	key := registry.CacheKeyPyPIIndex("demo")
	if err := f.reg.PutCacheEntry(ctx, "pyproxy", key, registry.CacheHit, index.String()); err != nil {
		t.Fatalf("PutCacheEntry: %v", err)
	}
	dist, err := f.cas.PutBytes(ctx, []byte("a cached wheel"))
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	if _, err := f.reg.PlaceCachedFile(ctx, registry.PlaceCachedFileParams{
		Repo: "pyproxy", Package: "demo", Version: "1.0", Filename: "demo-1.0-py3-none-any.whl", Blob: dist,
	}); err != nil {
		t.Fatalf("PlaceCachedFile: %v", err)
	}
	f.fetchedAgo(key, 2*staleAfter)

	if res := f.evict(); res.Hits != 1 {
		t.Fatalf("evict = %+v, want one hit", res)
	}
	for table, n := range map[string]int{
		"files":        f.count(`SELECT count(*) FROM files`),
		"versions":     f.count(`SELECT count(*) FROM versions`),
		"packages":     f.count(`SELECT count(*) FROM packages`),
		"remote_cache": f.entries(key),
	} {
		if n != 0 {
			t.Fatalf("%s holds %d rows after evicting the project", table, n)
		}
	}
}

// A hosted repository is never a cache. Even a remote_cache row pointing at
// one, which no code path writes, must not let retention remove a hosted tag.
func TestEvictProxyCache_HostedRepositoriesAreNeverTouched(t *testing.T) {
	f := newEvictFixture(t)
	ctx := t.Context()
	man := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest}
	man.SchemaVersion = 2
	body, _ := json.Marshal(man)
	placed, err := f.reg.PutManifest(ctx, "team", "app", "1.0", ocispec.MediaTypeImageManifest, body, model.Provenance{})
	if err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	key := registry.CacheKeyOCIManifest("app", "1.0")
	f.exec(`INSERT INTO remotes (repo_id, upstream_url, ttl_seconds)
		SELECT id, 'https://upstream.invalid', 60 FROM repositories WHERE name = 'team'`)
	f.exec(`INSERT INTO remote_cache (repo_id, cache_key, status, blob_digest, fetched_at)
		SELECT id, $1, 'hit', $2, now() - interval '30 days' FROM repositories WHERE name = 'team'`,
		key, placed.Digest.String())

	if res := f.evict(); res.Hits != 0 {
		t.Fatalf("evict = %+v; a hosted repository was treated as a cache", res)
	}
	if f.tags("1.0") != 1 {
		t.Fatal("retention removed a hosted tag")
	}
}
