//go:build integration

package scan

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/scanjob"
	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/testsupport"
	"github.com/spj/sindri/internal/worker"
)

// placementFixture stores blobs through a CAS that records them in the real
// blobs table, and links them into repositories by hand.
type placementFixture struct {
	*fixture
	q    *db.Queries
	repo map[string]db.Repository
}

func newPlacementFixture(t *testing.T, repos ...string) *placementFixture {
	t.Helper()
	database := testsupport.MigratedDB(t)
	f := &fixture{t: t, ev: &fakeAttacher{},
		store: cas.New(memstore.New(), cas.WithReserver(db.NewBlobReserver(database, "memory")))}
	pf := &placementFixture{fixture: f, q: database.Queries(), repo: make(map[string]db.Repository)}
	for _, name := range repos {
		r, err := pf.q.CreateRepository(t.Context(), db.CreateRepositoryParams{
			Name: name, Type: "hosted", Protocol: "oci", Config: []byte(`{}`), ManagedBy: "api"})
		if err != nil {
			t.Fatalf("CreateRepository %s: %v", name, err)
		}
		pf.repo[name] = r
	}
	return pf
}

func (p *placementFixture) scannerFor(opts Options) *Scanner {
	return New(p.store, p.ev, p.q, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// link places digests in repo.
func (p *placementFixture) link(repo string, digests ...model.Digest) {
	p.t.Helper()
	for _, d := range digests {
		if err := p.q.LinkRepositoryBlob(p.t.Context(), db.LinkRepositoryBlobParams{RepoID: p.repo[repo].ID, Digest: d.String()}); err != nil {
			p.t.Fatalf("LinkRepositoryBlob: %v", err)
		}
	}
}

// image stores a manifest with its config and layer, returning all three
// digests; none is linked anywhere yet.
func (p *placementFixture) image(tag string) (manifest model.Digest, blobs []model.Digest) {
	p.t.Helper()
	m, layer := p.imageWithLayer(tag, []byte("layer of "+tag))
	var raw ocispec.Manifest
	rc, err := p.store.Get(p.t.Context(), m)
	if err != nil {
		p.t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if err := json.NewDecoder(rc).Decode(&raw); err != nil {
		p.t.Fatal(err)
	}
	cfg, err := model.ParseDigest(raw.Config.Digest.String())
	if err != nil {
		p.t.Fatal(err)
	}
	return m, []model.Digest{m, cfg, layer}
}

func (p *placementFixture) assertSkipped(res json.RawMessage, err error, wantDigest model.Digest) {
	p.t.Helper()
	if err != nil {
		p.t.Fatalf("Handle: %v", err)
	}
	want := `{"skipped":"blob not in repository","digest":"` + wantDigest.String() + `"}`
	if string(res) != want || len(p.ev.attached) != 0 {
		p.t.Fatalf("result = %s (attached %d), want %s and no evidence", res, len(p.ev.attached), want)
	}
}

func TestPlacement_IndexChildLinkedOnlyInAnotherRepositoryIsRefused(t *testing.T) {
	p := newPlacementFixture(t, "a", "b")
	own, ownBlobs := p.image("own")
	foreign, foreignBlobs := p.image("foreign")
	idx := p.index(manifestDesc(own, "linux", "amd64"), manifestDesc(foreign, "linux", "arm64"))
	p.link("a", append(ownBlobs, idx)...)
	p.link("b", foreignBlobs...)

	res, err := p.scannerFor(Options{Trivy: "/nonexistent"}).Handle(t.Context(), p.jobIn("a", idx, "oci-index"))
	p.assertSkipped(res, err, foreign)
}

func TestPlacement_ManifestLayerLinkedOnlyInAnotherRepositoryIsRefused(t *testing.T) {
	p := newPlacementFixture(t, "a", "b")
	m, blobs := p.image("x")
	p.link("a", blobs[0], blobs[1]) // manifest and config; the layer is only in b
	p.link("b", blobs[2])

	res, err := p.scannerFor(Options{Trivy: "/nonexistent"}).Handle(t.Context(), p.jobIn("a", m, "oci-manifest"))
	p.assertSkipped(res, err, blobs[2])
}

func TestPlacement_SameRepositoryImageStillAttaches(t *testing.T) {
	p := newPlacementFixture(t, "a")
	m, blobs := p.image("x")
	p.link("a", blobs...)

	if _, err := p.scannerFor(Options{Trivy: stubPath(t)}).Handle(t.Context(), p.jobIn("a", m, "oci-manifest")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(p.ev.attached) != 2 {
		t.Fatalf("attached = %d, want an sbom and a scan", len(p.ev.attached))
	}
}

func (p *placementFixture) jobIn(repo string, digest model.Digest, kind string) queue.Job {
	args, _ := json.Marshal(map[string]string{"digest": digest.String(), "kind": kind, "repository": repo})
	return queue.Job{ID: 7, Kind: scanjob.JobKind, Args: args}
}

// runWorker runs the real worker, with the scanner as its handler, against
// the real queue until the test ends.
func (p *placementFixture) runWorker(t *testing.T, q *queue.Queue, s *Scanner, backoff time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = worker.Run(ctx, worker.Options{
			Queue: q, Owner: "w", Poll: 20 * time.Millisecond, Backoff: backoff,
			Handlers: map[string]worker.Handler{scanjob.JobKind: s.Handle},
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
}

func (p *placementFixture) waitJob(t *testing.T, q *queue.Queue, id int64, cond func(queue.Job) bool) queue.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		job, err := q.Get(t.Context(), id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if cond(job) {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out; job = %+v", job)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestQueue_OverCapArchiveFailsAfterOneAttemptWithNoEvidence(t *testing.T) {
	p := newPlacementFixture(t, "a")
	q := queue.New(testsupport.MigratedDB(t))
	digest := p.put(zipOf(t, map[string]string{"big": strings.Repeat("a", 4096)}))
	p.link("a", digest)
	args, _ := json.Marshal(map[string]string{"digest": digest.String(), "kind": "pypi-dist", "repository": "a"})
	job, _, err := q.Enqueue(t.Context(), queue.EnqueueParams{Kind: scanjob.JobKind, Args: args, MaxAttempts: 5, CreatedBy: "t"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	p.runWorker(t, q, p.scannerFor(Options{Trivy: "/nonexistent", MaxUnpackedBytes: 1000}), time.Millisecond)

	got := p.waitJob(t, q, job.ID, queue.Job.Done)
	if got.State != queue.StateFailed || got.Attempts != 1 {
		t.Fatalf("job = %+v, want failed after one attempt of five", got)
	}
	if !strings.Contains(got.LastError, "scanning.maxUnpackedBytes (1000)") {
		t.Fatalf("last_error = %q, want it to name the cap", got.LastError)
	}
	if len(p.ev.attached) != 0 {
		t.Fatalf("attached = %d, want no evidence", len(p.ev.attached))
	}
}

func TestQueue_TransientScanErrorStillRetries(t *testing.T) {
	p := newPlacementFixture(t, "a")
	q := queue.New(testsupport.MigratedDB(t))
	m, blobs := p.image("x")
	p.link("a", blobs...)
	args, _ := json.Marshal(map[string]string{"digest": m.String(), "kind": "oci-manifest", "repository": "a"})
	job, _, err := q.Enqueue(t.Context(), queue.EnqueueParams{Kind: scanjob.JobKind, Args: args, MaxAttempts: 5, CreatedBy: "t"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// A missing scanner binary is the operator's to fix, not the subject's.
	p.runWorker(t, q, p.scannerFor(Options{Trivy: "/nonexistent"}), time.Hour)

	got := p.waitJob(t, q, job.ID, func(j queue.Job) bool { return j.LastError != "" })
	if got.State != queue.StateQueued || got.Attempts != 1 || got.FinishedAt != nil {
		t.Fatalf("job = %+v, want queued for a retry", got)
	}
}
