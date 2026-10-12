//go:build integration

package gc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/gc"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/storage"
	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/testsupport"
)

const (
	grace = time.Hour
	repo  = "demo"
	image = "app"
)

// controlledStore lets a test stop or fail the collector inside Delete, the
// moment it holds the row lock and has deleted the rows but not committed.
type controlledStore struct {
	storage.BlobStore
	mu       sync.Mutex
	onDelete func(model.Digest) error
}

func (s *controlledStore) Delete(ctx context.Context, d model.Digest) error {
	s.mu.Lock()
	hook := s.onDelete
	s.mu.Unlock()
	if hook != nil {
		if err := hook(d); err != nil {
			return err
		}
	}
	return s.BlobStore.Delete(ctx, d)
}

func (s *controlledStore) setOnDelete(f func(model.Digest) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onDelete = f
}

type fixture struct {
	t     *testing.T
	db    *db.DB
	mem   *memstore.Store
	store *controlledStore
	cas   *cas.Store
	reg   *registry.Registry
	lc    *lifecycle.Service
	ev    *evidence.Service
	gc    *gc.Collector
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	database := testsupport.MigratedDB(t)
	mem := memstore.New()
	store := &controlledStore{BlobStore: mem}
	casStore := cas.New(store, cas.WithReserver(db.NewBlobReserver(database, "memory")))
	collector, err := gc.New(gc.Options{Database: database, CAS: casStore, Grace: grace})
	if err != nil {
		t.Fatalf("gc.New: %v", err)
	}
	return &fixture{
		t: t, db: database, mem: mem, store: store, cas: casStore,
		reg: registry.New(database, casStore, "memory"),
		lc:  lifecycle.New(database),
		ev:  evidence.New(database, casStore, "memory"),
		gc:  collector,
	}
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Pool().Exec(f.t.Context(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// age puts every blob seen so far outside the grace period. The collector
// reads the database clock, so tests move rows, not time.
func (f *fixture) age() {
	f.t.Helper()
	f.exec(`UPDATE blobs SET last_seen_at = now() - interval '2 days'`)
}

func (f *fixture) uploadLayer(repoName string, payload []byte) model.Digest {
	f.t.Helper()
	ctx := f.t.Context()
	if _, err := f.reg.EnsureOCIRepository(ctx, repoName); err != nil {
		f.t.Fatalf("EnsureOCIRepository: %v", err)
	}
	up, err := f.cas.NewUpload(ctx)
	if err != nil {
		f.t.Fatalf("NewUpload: %v", err)
	}
	if _, err := up.Append(ctx, bytes.NewReader(payload)); err != nil {
		f.t.Fatalf("Append: %v", err)
	}
	info, err := f.reg.CommitBlobUpload(ctx, repoName, up, model.Digest{})
	if err != nil {
		f.t.Fatalf("CommitBlobUpload: %v", err)
	}
	return info.Digest
}

func descriptor(mediaType string, d model.Digest, size int64) ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: mediaType, Digest: digestOf(d), Size: size}
}

// pushImage uploads a config and layers and puts an image manifest under ref,
// which is a tag or "" for a push by digest.
func (f *fixture) pushImage(ref string, layers ...string) (model.Digest, []model.Digest) {
	f.t.Helper()
	config := []byte(`{"architecture":"amd64","os":"linux","layers":"` + strings.Join(layers, ",") + `"}`)
	cfgDigest := f.uploadLayer(repo, config)
	man := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    descriptor(ocispec.MediaTypeImageConfig, cfgDigest, int64(len(config))),
	}
	man.SchemaVersion = 2
	digests := []model.Digest{cfgDigest}
	for _, l := range layers {
		d := f.uploadLayer(repo, []byte(l))
		digests = append(digests, d)
		man.Layers = append(man.Layers, descriptor(ocispec.MediaTypeImageLayer, d, int64(len(l))))
	}
	return f.putManifest(ref, ocispec.MediaTypeImageManifest, man), digests
}

func (f *fixture) putManifest(ref, mediaType string, v any) model.Digest {
	f.t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		f.t.Fatalf("marshal: %v", err)
	}
	if ref == "" {
		ref = sha256Of(body).String()
	}
	man, err := f.reg.PutManifest(f.t.Context(), repo, image, ref, mediaType, body, model.Provenance{SourceType: "test"})
	if err != nil {
		f.t.Fatalf("PutManifest %s: %v", ref, err)
	}
	return man.Digest
}

func (f *fixture) collect() gc.Result {
	f.t.Helper()
	res, err := f.gc.Collect(f.t.Context())
	if err != nil {
		f.t.Fatalf("Collect: %v", err)
	}
	if res.Errors != 0 {
		f.t.Fatalf("Collect reported %d errors: %+v", res.Errors, res)
	}
	return res
}

func (f *fixture) rowExists(digest model.Digest) bool {
	f.t.Helper()
	var ok bool
	if err := f.db.Pool().QueryRow(f.t.Context(),
		`SELECT EXISTS (SELECT 1 FROM blobs WHERE digest = $1)`, digest.String()).Scan(&ok); err != nil {
		f.t.Fatalf("reading blobs: %v", err)
	}
	return ok
}

func (f *fixture) bytesExist(digest model.Digest) bool {
	f.t.Helper()
	_, err := f.mem.Stat(f.t.Context(), digest)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		f.t.Fatalf("Stat: %v", err)
	}
	return err == nil
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.Pool().QueryRow(f.t.Context(), sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (f *fixture) assertSurvives(what string, digests ...model.Digest) {
	f.t.Helper()
	for _, d := range digests {
		if !f.rowExists(d) || !f.bytesExist(d) {
			f.t.Fatalf("%s %s was collected (row %v, bytes %v)", what, d, f.rowExists(d), f.bytesExist(d))
		}
	}
}

func (f *fixture) assertCollected(what string, digests ...model.Digest) {
	f.t.Helper()
	for _, d := range digests {
		if f.rowExists(d) || f.bytesExist(d) {
			f.t.Fatalf("%s %s survived (row %v, bytes %v)", what, d, f.rowExists(d), f.bytesExist(d))
		}
	}
}

// assertNotCandidate checks the mark itself, not just the outcome. The sweep's
// re-check and the foreign keys would save a root the mark forgot, so a
// survival test that looked only at the outcome would pass with a broken mark.
func (f *fixture) assertNotCandidate(what string, digests ...model.Digest) {
	f.t.Helper()
	cands, err := f.gc.Mark(f.t.Context())
	if err != nil {
		f.t.Fatalf("Mark: %v", err)
	}
	marked := digestsOf(cands)
	for _, d := range digests {
		if slices.Contains(marked, d.String()) {
			f.t.Fatalf("%s %s is a collection candidate", what, d)
		}
	}
}

func (f *fixture) stages(names ...string) {
	f.t.Helper()
	specs := make([]lifecycle.StageSpec, 0, len(names))
	for _, n := range names {
		specs = append(specs, lifecycle.StageSpec{Name: n})
	}
	if _, err := f.lc.SetStages(f.t.Context(), specs); err != nil {
		f.t.Fatalf("SetStages: %v", err)
	}
}

func (f *fixture) promote(d model.Digest, stage string) error {
	_, err := f.lc.Promote(f.t.Context(), lifecycle.ChangeParams{
		Digest: d, Repository: repo, Stage: stage, Actor: "user:test", Reason: "test",
	})
	return err
}

// --- Roots, closure, and collection ---------------------------------------

func TestGC_TaggedManifestSurvives(t *testing.T) {
	f := newFixture(t)
	man, layers := f.pushImage("1.0", "layer-a", "layer-b")
	f.age()

	f.assertNotCandidate("tagged manifest", man)
	f.assertNotCandidate("layer of a tagged manifest", layers...)
	f.collect()
	f.assertSurvives("tagged manifest", man)
	f.assertSurvives("layer of a tagged manifest", layers...)
}

func TestGC_LayersOfATaggedIndexSurviveThroughEveryLevel(t *testing.T) {
	f := newFixture(t)
	child, layers := f.pushImage("", "platform-layer")
	idx := ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{descriptor(ocispec.MediaTypeImageManifest, child, 1)},
	}
	idx.SchemaVersion = 2
	index := f.putManifest("multi", ocispec.MediaTypeImageIndex, idx)
	f.age()

	f.assertNotCandidate("tagged index", index, child)
	f.assertNotCandidate("layer two levels under a tagged index", layers...)
	f.collect()
	f.assertSurvives("tagged index", index)
	f.assertSurvives("platform manifest under a tagged index", child)
	f.assertSurvives("layer two levels under a tagged index", layers...)
}

func TestGC_UntaggedUnpromotedManifestAndItsExclusiveLayersAreCollected(t *testing.T) {
	f := newFixture(t)
	man, layers := f.pushImage("tmp", "doomed-layer")
	if err := f.reg.DeleteManifest(t.Context(), repo, image, "tmp"); err != nil {
		t.Fatalf("untagging: %v", err)
	}
	f.age()
	before := f.mem.Len()

	res := f.collect()
	f.assertCollected("untagged manifest", man)
	f.assertCollected("layer only the untagged manifest used", layers...)
	if res.Deleted != 1+len(layers) {
		t.Fatalf("deleted %d, want %d: %+v", res.Deleted, 1+len(layers), res)
	}
	if got := f.mem.Len(); got != before-res.Deleted {
		t.Fatalf("storage holds %d blobs, want %d", got, before-res.Deleted)
	}
	for table, col := range map[string]string{
		"subjects": "digest", "subject_owners": "digest", "provenance": "digest",
		"repository_blobs": "digest", "manifest_children": "parent_digest",
	} {
		if n := f.count(`SELECT count(*) FROM `+table+` WHERE `+col+` = $1`, man.String()); n != 0 {
			t.Fatalf("%s still holds %d rows for the collected manifest", table, n)
		}
	}
}

func TestGC_LayerSharedWithATaggedManifestIsKept(t *testing.T) {
	f := newFixture(t)
	_, keepLayers := f.pushImage("1.0", "shared", "kept")
	_, dropLayers := f.pushImage("tmp", "shared", "dropped")
	if err := f.reg.DeleteManifest(t.Context(), repo, image, "tmp"); err != nil {
		t.Fatalf("untagging: %v", err)
	}
	f.age()

	f.collect()
	f.assertSurvives("layers of the tagged manifest", keepLayers...)
	// dropLayers[1] is "shared", the same bytes as keepLayers[1].
	f.assertSurvives("shared layer", dropLayers[1])
	f.assertCollected("layer only the untagged manifest used", dropLayers[2])
}

func TestGC_UnreferencedUploadedLayerIsCollectedAfterGrace(t *testing.T) {
	f := newFixture(t)
	orphan := f.uploadLayer(repo, []byte("pushed, then the manifest push failed"))
	f.age()

	f.collect()
	f.assertCollected("unreferenced layer", orphan)
	if n := f.count(`SELECT count(*) FROM repository_blobs WHERE digest = $1`, orphan.String()); n != 0 {
		t.Fatalf("the collected layer is still placed in %d repositories", n)
	}
}

func TestGC_RecentBlobIsProtectedByGracePeriod(t *testing.T) {
	f := newFixture(t)
	old := f.uploadLayer(repo, []byte("old orphan"))
	f.age()
	recent := f.uploadLayer(repo, []byte("an upload whose manifest is still on its way"))

	res := f.collect()
	f.assertCollected("old orphan", old)
	f.assertSurvives("recent orphan", recent)
	if res.Deleted != 1 {
		t.Fatalf("deleted %d, want 1: %+v", res.Deleted, res)
	}
}

func TestGC_PromotedUntaggedManifestSurvives(t *testing.T) {
	f := newFixture(t)
	f.stages("dev")
	man, layers := f.pushImage("2.0", "promoted-layer")
	if err := f.promote(man, "dev"); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if err := f.reg.DeleteManifest(t.Context(), repo, image, "2.0"); err != nil {
		t.Fatalf("untagging: %v", err)
	}
	f.age()

	f.assertNotCandidate("promoted manifest", man)
	f.assertNotCandidate("layer of a promoted manifest", layers...)
	f.collect()
	f.assertSurvives("promoted manifest", man)
	f.assertSurvives("layer of a promoted manifest", layers...)
}

func TestGC_EvidenceBlobAndItsSubjectSurvive(t *testing.T) {
	f := newFixture(t)
	man, layers := f.pushImage("tmp", "evidenced-layer")
	sbom := []byte(`{"spdxVersion":"SPDX-2.3"}`)
	ev, err := f.ev.Attach(t.Context(), evidence.AttachParams{
		Subject: man, Type: model.EvidenceSBOM, MediaType: "application/spdx+json",
		Body: bytes.NewReader(sbom), Size: int64(len(sbom)), Actor: "user:test",
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := f.reg.DeleteManifest(t.Context(), repo, image, "tmp"); err != nil {
		t.Fatalf("untagging: %v", err)
	}
	f.age()

	f.assertNotCandidate("evidence document", ev.Digest)
	f.assertNotCandidate("subject with evidence", man)
	f.collect()
	f.assertSurvives("evidence document", ev.Digest)
	f.assertSurvives("subject with evidence", man)
	f.assertSurvives("layer of a subject with evidence", layers...)
}

func TestGC_PyPIFileSurvivesAndDeletedFileIsCollected(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	if _, err := f.reg.CreateRepository(ctx, registry.CreateRepositoryParams{
		Name: "py", Type: model.RepoTypeHosted, Protocol: model.ProtocolPyPI,
	}); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	publish := func(version, payload string) model.Digest {
		t.Helper()
		file, err := f.reg.Publish(ctx, registry.PublishParams{
			Repo: "py", Package: "demo", Version: version, Filename: "demo-" + version + ".tar.gz",
			Body: strings.NewReader(payload), Size: int64(len(payload)),
		})
		if err != nil {
			t.Fatalf("Publish: %v", err)
		}
		return file.Digest
	}
	kept := publish("1.0", "kept sdist")
	dropped := publish("2.0", "deleted sdist")
	if err := f.reg.DeleteFile(ctx, "py", "demo", "2.0", "demo-2.0.tar.gz"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	f.age()

	f.assertNotCandidate("published file", kept)
	f.collect()
	f.assertSurvives("published file", kept)
	f.assertCollected("deleted file", dropped)
}

// --- Proxy content and placeholders ---------------------------------------

func (f *fixture) proxyRepo() {
	f.t.Helper()
	if _, err := f.reg.CreateRepository(f.t.Context(), registry.CreateRepositoryParams{
		Name: "hub", Type: model.RepoTypeProxy, Protocol: model.ProtocolOCI,
		UpstreamURL: "https://upstream.invalid", TTLSeconds: 60,
	}); err != nil {
		f.t.Fatalf("CreateRepository: %v", err)
	}
}

// cachedManifest places an upstream manifest whose layers have not been
// fetched, so they exist only as placeholder rows with no bytes.
func (f *fixture) cachedManifest(ref string, layers ...string) (model.Digest, []model.Digest) {
	f.t.Helper()
	man := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest}
	man.SchemaVersion = 2
	cfg := []byte(`{"upstream":"` + ref + `"}`)
	man.Config = descriptor(ocispec.MediaTypeImageConfig, sha256Of(cfg), int64(len(cfg)))
	children := []model.Digest{sha256Of(cfg)}
	for _, l := range layers {
		d := sha256Of([]byte(l))
		children = append(children, d)
		man.Layers = append(man.Layers, descriptor(ocispec.MediaTypeImageLayer, d, int64(len(l))))
	}
	body, _ := json.Marshal(man)
	placed, err := f.reg.PlaceCachedManifest(f.t.Context(), "hub", "library/alpine", ref, ocispec.MediaTypeImageManifest, body)
	if err != nil {
		f.t.Fatalf("PlaceCachedManifest: %v", err)
	}
	if err := f.reg.PutCacheEntry(f.t.Context(), "hub", registry.CacheKeyOCIManifest("library/alpine", ref), registry.CacheHit, placed.Digest.String()); err != nil {
		f.t.Fatalf("PutCacheEntry: %v", err)
	}
	return placed.Digest, children
}

func TestGC_FreshRemoteCacheHitIsARoot(t *testing.T) {
	f := newFixture(t)
	f.proxyRepo()
	man, _ := f.cachedManifest("3.20", "upstream-layer")
	// Only the cache entry keeps it now.
	f.exec(`DELETE FROM tags`)
	f.age()

	f.assertNotCandidate("manifest behind a cache hit", man)
	f.collect()
	f.assertSurvives("manifest behind a cache hit", man)
}

func TestGC_PlaceholderUnderALiveProxyManifestSurvives(t *testing.T) {
	f := newFixture(t)
	f.proxyRepo()
	_, children := f.cachedManifest("3.20", "never-pulled-layer")
	f.age()

	f.collect()
	for _, d := range children {
		if !f.rowExists(d) {
			t.Fatalf("placeholder %s under a live manifest was collected", d)
		}
	}
}

func TestGC_PlaceholderWithoutBytesIsCollectedWithoutError(t *testing.T) {
	f := newFixture(t)
	f.proxyRepo()
	man, children := f.cachedManifest("3.20", "never-pulled-layer")
	f.exec(`DELETE FROM tags`)
	f.exec(`DELETE FROM remote_cache`)
	f.age()

	res := f.collect()
	f.assertCollected("evicted proxy manifest", man)
	for _, d := range children {
		if f.rowExists(d) {
			t.Fatalf("placeholder %s of an evicted manifest survived", d)
		}
	}
	if res.Deleted != 1+len(children) {
		t.Fatalf("deleted %d, want %d: %+v", res.Deleted, 1+len(children), res)
	}
}

// --- Dry runs and repeat runs ---------------------------------------------

func TestGC_DryRunDeletesNothing(t *testing.T) {
	f := newFixture(t)
	f.pushImage("tmp", "a", "b")
	if err := f.reg.DeleteManifest(t.Context(), repo, image, "tmp"); err != nil {
		t.Fatalf("untagging: %v", err)
	}
	f.age()
	rows, stored := f.count(`SELECT count(*) FROM blobs`), f.mem.Len()

	cands, err := f.gc.Mark(t.Context())
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if len(cands) == 0 {
		t.Fatal("Mark found nothing to collect")
	}
	if got := f.count(`SELECT count(*) FROM blobs`); got != rows {
		t.Fatalf("a dry run changed the blobs table from %d to %d rows", rows, got)
	}
	if got := f.mem.Len(); got != stored {
		t.Fatalf("a dry run changed storage from %d to %d blobs", stored, got)
	}
}

func TestGC_DryRunListsExactlyWhatARealRunDeletes(t *testing.T) {
	f := newFixture(t)
	f.pushImage("1.0", "kept")
	f.pushImage("tmp", "dropped-1", "dropped-2")
	f.uploadLayer(repo, []byte("orphan"))
	if err := f.reg.DeleteManifest(t.Context(), repo, image, "tmp"); err != nil {
		t.Fatalf("untagging: %v", err)
	}
	f.age()

	cands, err := f.gc.Mark(t.Context())
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	listed := digestsOf(cands)

	before := f.allDigests()
	f.collect()
	after := f.allDigests()
	var deleted []string
	for _, d := range before {
		if !slices.Contains(after, d) {
			deleted = append(deleted, d)
		}
	}
	if !slices.Equal(listed, deleted) {
		t.Fatalf("dry run listed %v, real run deleted %v", listed, deleted)
	}
}

func TestGC_SecondRunDeletesNothing(t *testing.T) {
	f := newFixture(t)
	f.uploadLayer(repo, []byte("orphan"))
	f.age()
	f.collect()

	if res := f.collect(); res.Deleted != 0 || res.Candidates != 0 {
		t.Fatalf("second run = %+v, want nothing to do", res)
	}
}

func TestGC_PagedCandidatesCoverTheMarkExactlyOnce(t *testing.T) {
	f := newFixture(t)
	for i := range 7 {
		f.uploadLayer(repo, []byte(fmt.Sprintf("orphan %d", i)))
	}
	f.age()

	all, err := f.gc.Mark(t.Context())
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	var paged []gc.Candidate
	after := ""
	for {
		page, next, err := f.gc.Candidates(t.Context(), after, 3)
		if err != nil {
			t.Fatalf("Candidates: %v", err)
		}
		paged = append(paged, page...)
		if next == "" {
			break
		}
		after = next
	}
	if !slices.Equal(digestsOf(all), digestsOf(paged)) {
		t.Fatalf("paged %v, marked %v", digestsOf(paged), digestsOf(all))
	}
}

// --- Concurrency and failure ----------------------------------------------

// parkDelete makes the collector stop inside the byte delete of target, with
// its row lock held, until release is closed.
func (f *fixture) parkDelete(target model.Digest) (entered <-chan struct{}, release chan<- struct{}) {
	in := make(chan struct{})
	out := make(chan struct{})
	var once sync.Once
	f.store.setOnDelete(func(d model.Digest) error {
		if d == target {
			once.Do(func() { close(in) })
			<-out
		}
		return nil
	})
	return in, out
}

func (f *fixture) collectAsync() <-chan gc.Result {
	done := make(chan gc.Result, 1)
	go func() {
		res, err := f.gc.Collect(context.WithoutCancel(f.t.Context()))
		if err != nil {
			f.t.Errorf("Collect: %v", err)
		}
		done <- res
	}()
	return done
}

func assertBlocked(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s finished while the collector held the blob (err %v)", what, err)
	case <-time.After(300 * time.Millisecond):
	}
}

// The classic registry data-loss bug: a push that lands the same bytes while
// the collector is deleting them. Whichever way the race goes, the push must
// end with its bytes, its row, and its placement.
func TestGC_InFlightPushSurvivesConcurrentCollection(t *testing.T) {
	f := newFixture(t)
	payload := []byte("the same layer, pushed again mid-collection")
	layer := f.uploadLayer(repo, payload)
	f.age()

	entered, release := f.parkDelete(layer)
	collected := f.collectAsync()
	<-entered

	pushed := make(chan error, 1)
	go func() {
		ctx := context.WithoutCancel(t.Context())
		up, err := f.cas.NewUpload(ctx)
		if err == nil {
			_, err = up.Append(ctx, bytes.NewReader(payload))
		}
		if err == nil {
			_, err = f.reg.CommitBlobUpload(ctx, repo, up, layer)
		}
		pushed <- err
	}()
	assertBlocked(t, pushed, "the concurrent push")

	close(release)
	<-collected
	if err := <-pushed; err != nil {
		t.Fatalf("the concurrent push failed: %v", err)
	}
	f.assertSurvives("re-pushed layer", layer)
	_, rc, err := f.reg.GetBlob(t.Context(), repo, layer)
	if err != nil {
		t.Fatalf("GetBlob after the race: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("read back %q, want %q", got, payload)
	}
}

// A publish that finds the bytes already stored skips writing them. If the
// collector is deleting exactly those bytes, the skip must not happen.
func TestGC_PushThatDedupsAgainstACollectedBlobRewritesItsBytes(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	if _, err := f.reg.CreateRepository(ctx, registry.CreateRepositoryParams{
		Name: "py", Type: model.RepoTypeHosted, Protocol: model.ProtocolPyPI,
	}); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	payload := "an sdist deleted and then published again"
	publish := func(ctx context.Context) error {
		_, err := f.reg.Publish(ctx, registry.PublishParams{
			Repo: "py", Package: "demo", Version: "1.0", Filename: "demo-1.0.tar.gz",
			Body: strings.NewReader(payload), Size: int64(len(payload)),
			ExpectDigest: sha256Of([]byte(payload)),
		})
		return err
	}
	if err := publish(ctx); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := f.reg.DeleteFile(ctx, "py", "demo", "1.0", "demo-1.0.tar.gz"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	f.age()
	digest := sha256Of([]byte(payload))

	entered, release := f.parkDelete(digest)
	collected := f.collectAsync()
	<-entered

	published := make(chan error, 1)
	go func() { published <- publish(context.WithoutCancel(ctx)) }()
	assertBlocked(t, published, "the concurrent publish")

	close(release)
	<-collected
	if err := <-published; err != nil {
		t.Fatalf("the concurrent publish failed: %v", err)
	}
	f.assertSurvives("re-published file", digest)
}

func TestGC_SweepSkipsABlobReferencedAfterTheMarkSnapshot(t *testing.T) {
	f := newFixture(t)
	man, _ := f.pushImage("tmp", "layer")
	if err := f.reg.DeleteManifest(t.Context(), repo, image, "tmp"); err != nil {
		t.Fatalf("untagging: %v", err)
	}
	f.age()

	cands, err := f.gc.Mark(t.Context())
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	// Tagged after the snapshot, without touching last_seen_at: only the
	// sweep's re-check can see it.
	f.exec(`INSERT INTO tags (repo_id, package_id, name, target_digest)
		SELECT p.repo_id, p.id, 'late', $1 FROM packages p LIMIT 1`, man.String())

	res, err := f.gc.Sweep(t.Context(), cands)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	f.assertSurvives("manifest tagged after the mark", man)
	if res.Errors != 0 || res.SkippedReferenced == 0 {
		t.Fatalf("sweep = %+v, want the manifest skipped as referenced", res)
	}
}

// The mark filters on the grace period, so a collection run alone cannot show
// the sweep re-reading it. A blob touched between mark and sweep is a write
// that started after the snapshot, and the sweep must leave it.
func TestGC_SweepRechecksTheGracePeriod(t *testing.T) {
	f := newFixture(t)
	layer := f.uploadLayer(repo, []byte("touched after the mark"))
	f.age()

	cands, err := f.gc.Mark(t.Context())
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	f.exec(`UPDATE blobs SET last_seen_at = now() WHERE digest = $1`, layer.String())

	res, err := f.gc.Sweep(t.Context(), cands)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	f.assertSurvives("blob touched after the mark", layer)
	if res.SkippedRecent != 1 {
		t.Fatalf("sweep = %+v, want the blob skipped as recent", res)
	}
}

func TestGC_SweepSkipsALockedRow(t *testing.T) {
	f := newFixture(t)
	layer := f.uploadLayer(repo, []byte("held by a writer"))
	f.age()

	tx, err := f.db.Pool().Begin(t.Context())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), `SELECT 1 FROM blobs WHERE digest = $1 FOR KEY SHARE`, layer.String()); err != nil {
		t.Fatalf("locking: %v", err)
	}

	res := f.collect()
	f.assertSurvives("locked blob", layer)
	if res.SkippedLocked != 1 {
		t.Fatalf("sweep = %+v, want one blob skipped as locked", res)
	}
}

func TestGC_PromoteRacingCollectionNeverLeavesAnEventWithoutASubject(t *testing.T) {
	f := newFixture(t)
	f.stages("dev")
	man, _ := f.pushImage("tmp", "layer")
	if err := f.reg.DeleteManifest(t.Context(), repo, image, "tmp"); err != nil {
		t.Fatalf("untagging: %v", err)
	}
	f.age()

	entered, release := f.parkDelete(man)
	collected := f.collectAsync()
	<-entered

	promoted := make(chan error, 1)
	go func() { promoted <- f.promote(man, "dev") }()
	assertBlocked(t, promoted, "the concurrent promotion")
	close(release)
	<-collected

	err := <-promoted
	events := f.count(`SELECT count(*) FROM lifecycle_events WHERE subject_digest = $1`, man.String())
	switch {
	case err == nil && (events != 1 || !f.rowExists(man)):
		t.Fatalf("promotion succeeded but the manifest was collected (events %d)", events)
	case err != nil && events != 0:
		t.Fatalf("promotion failed (%v) yet left %d events", err, events)
	case err != nil && f.rowExists(man):
		t.Fatalf("promotion failed (%v) and the manifest survived", err)
	}
}

func TestGC_FailedByteDeleteRollsBackRows(t *testing.T) {
	f := newFixture(t)
	layer := f.uploadLayer(repo, []byte("storage refuses to delete this"))
	f.age()
	f.store.setOnDelete(func(d model.Digest) error {
		if d == layer {
			return errors.New("storage unavailable")
		}
		return nil
	})

	res, err := f.gc.Collect(t.Context())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.Errors != 1 || res.Deleted != 0 {
		t.Fatalf("collect = %+v, want one error and nothing deleted", res)
	}
	f.assertSurvives("blob whose bytes could not be deleted", layer)
	if n := f.count(`SELECT count(*) FROM repository_blobs WHERE digest = $1`, layer.String()); n != 1 {
		t.Fatalf("placement rows = %d after a failed delete, want 1", n)
	}

	// Storage recovers; the next run finishes the job.
	f.store.setOnDelete(nil)
	f.collect()
	f.assertCollected("blob on the retry", layer)
}

func TestGC_HostedManifestPutAfterChildCollectedReturnsBlobUnknown(t *testing.T) {
	f := newFixture(t)
	layer := f.uploadLayer(repo, []byte("collected before its manifest arrived"))
	f.age()
	f.collect()

	man := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest}
	man.SchemaVersion = 2
	man.Config = descriptor(ocispec.MediaTypeImageConfig, layer, 1)
	body, _ := json.Marshal(man)
	_, err := f.reg.PutManifest(t.Context(), repo, image, "late", ocispec.MediaTypeImageManifest, body, model.Provenance{})
	if !errors.Is(err, registry.ErrBlobUnknown) {
		t.Fatalf("PutManifest = %v, want ErrBlobUnknown", err)
	}
}

func TestNew_RefusesACASWithoutReserver(t *testing.T) {
	database := testsupport.MigratedDB(t)
	_, err := gc.New(gc.Options{Database: database, CAS: cas.New(memstore.New()), Grace: grace})
	if !errors.Is(err, gc.ErrUnsafeWiring) {
		t.Fatalf("New = %v, want ErrUnsafeWiring", err)
	}
}

// --- helpers --------------------------------------------------------------

func (f *fixture) allDigests() []string {
	f.t.Helper()
	rows, err := f.db.Pool().Query(f.t.Context(), `SELECT digest FROM blobs ORDER BY digest`)
	if err != nil {
		f.t.Fatalf("listing blobs: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			f.t.Fatalf("scan: %v", err)
		}
		out = append(out, d)
	}
	return out
}

func digestsOf(cands []gc.Candidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Digest.String())
	}
	slices.Sort(out)
	return out
}

func digestOf(d model.Digest) godigest.Digest { return godigest.Digest(d.String()) }

func sha256Of(b []byte) model.Digest {
	d := model.NewDigester(model.AlgorithmSHA256)
	_, _ = d.Write(b)
	return d.Digest()
}

// The mark's recursive query against a breadth-first search written in Go,
// over random graphs of manifests, layers, and roots. Each seed is a named
// case, so a failure reproduces exactly.
func TestGCMark_MatchesReferenceReachabilityOnRandomGraphs(t *testing.T) {
	for seed := range 12 {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			f := newFixture(t)
			rng := rand.New(rand.NewPCG(uint64(seed), 0x5eed)) //nolint:gosec // G404: a reproducible test graph, not a secret
			const n = 40

			if _, err := f.reg.EnsureOCIRepository(t.Context(), repo); err != nil {
				t.Fatalf("EnsureOCIRepository: %v", err)
			}
			f.exec(`INSERT INTO packages (repo_id, normalized_name, display_name)
				SELECT id, 'g', 'g' FROM repositories WHERE name = $1`, repo)

			nodes := make([]string, n)
			for i := range nodes {
				nodes[i] = sha256Of([]byte(fmt.Sprintf("seed %d node %d", seed, i))).String()
				f.exec(`INSERT INTO blobs (digest, size, storage_backend, last_seen_at)
					VALUES ($1, 1, 'memory', now() - interval '2 days')`, nodes[i])
			}
			edges := map[int][]int{}
			for parent := range n {
				for child := parent + 1; child < n; child++ {
					if rng.IntN(12) == 0 {
						edges[parent] = append(edges[parent], child)
						f.exec(`INSERT INTO manifest_children (parent_digest, child_digest) VALUES ($1, $2)`,
							nodes[parent], nodes[child])
					}
				}
			}
			var roots []int
			for i := range n {
				switch rng.IntN(10) {
				case 0:
					roots = append(roots, i)
					f.exec(`INSERT INTO tags (repo_id, package_id, name, target_digest)
						SELECT p.repo_id, p.id, $1, $2 FROM packages p WHERE p.normalized_name = 'g'`,
						fmt.Sprintf("t%d", i), nodes[i])
				case 1:
					roots = append(roots, i)
					f.exec(`INSERT INTO remote_cache (repo_id, cache_key, status, blob_digest)
						SELECT id, $1, 'hit', $2 FROM repositories WHERE name = $3`,
						fmt.Sprintf("k%d", i), nodes[i], repo)
				}
			}

			live := map[int]bool{}
			queue := slices.Clone(roots)
			for len(queue) > 0 {
				i := queue[0]
				queue = queue[1:]
				if live[i] {
					continue
				}
				live[i] = true
				queue = append(queue, edges[i]...)
			}
			var want []string
			for i, d := range nodes {
				if !live[i] {
					want = append(want, d)
				}
			}
			slices.Sort(want)

			cands, err := f.gc.Mark(t.Context())
			if err != nil {
				t.Fatalf("Mark: %v", err)
			}
			if got := digestsOf(cands); !slices.Equal(got, want) {
				t.Fatalf("mark found %d candidates, reference search %d\n got %v\nwant %v", len(got), len(want), got, want)
			}
		})
	}
}

// Retention and collection together: a proxy tag that was cached, went stale,
// and was evicted leaves content nothing references, which the collector then
// reclaims. Before eviction the same content is a root.
func TestGC_StaleProxyContentIsCollectedOnceRetentionEvictsIt(t *testing.T) {
	f := newFixture(t)
	f.proxyRepo()
	man, children := f.cachedManifest("3.20", "upstream-layer")
	f.age()
	f.exec(`UPDATE remote_cache SET fetched_at = now() - interval '30 days'`)

	f.assertNotCandidate("proxy manifest before eviction", man)
	if _, err := f.reg.EvictStaleCache(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("EvictStaleCache: %v", err)
	}
	f.collect()
	f.assertCollected("evicted proxy manifest", man)
	for _, d := range children {
		if f.rowExists(d) {
			t.Fatalf("child %s of an evicted proxy manifest survived", d)
		}
	}
}
