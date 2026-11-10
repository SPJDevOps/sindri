//go:build integration

package scan

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/scanjob"
	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/testsupport"
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
