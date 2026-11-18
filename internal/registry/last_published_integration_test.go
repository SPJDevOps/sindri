//go:build integration

package registry_test

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

func (f *evictFixture) lastPublished(repo string) *time.Time {
	f.t.Helper()
	var at *time.Time
	if err := f.db.Pool().QueryRow(f.t.Context(),
		`SELECT last_published_at FROM repositories WHERE name = $1`, repo).Scan(&at); err != nil {
		f.t.Fatalf("reading last_published_at of %s: %v", repo, err)
	}
	return at
}

func TestLastPublished_ProxyCacheFillAdvancesIt(t *testing.T) {
	f := newEvictFixture(t)
	f.proxy("pyproxy", model.ProtocolPyPI, 60, 0)
	if got := f.lastPublished("pyproxy"); got != nil {
		t.Fatalf("last_published_at before any fill = %v, want NULL", got)
	}
	f.place("pyproxy", "demo", "1.0", "demo-1.0-py3-none-any.whl")
	first := f.lastPublished("pyproxy")
	if first == nil {
		t.Fatal("last_published_at is NULL after a cache fill")
	}
	f.place("pyproxy", "demo", "1.1", "demo-1.1-py3-none-any.whl")
	if second := f.lastPublished("pyproxy"); second == nil || !second.After(*first) {
		t.Fatalf("last_published_at after a second fill = %v, want later than %v", second, first)
	}
}

func TestLastPublished_DeletingTheVersionLeavesItUnchanged(t *testing.T) {
	f := newEvictFixture(t)
	f.hostedRepo("py-hosted", model.ProtocolPyPI)
	filename := f.publishPyPI("py-hosted", "demo", "1.0.0")
	before := f.lastPublished("py-hosted")
	if before == nil {
		t.Fatal("last_published_at is NULL after a publish")
	}

	if err := f.reg.DeleteFile(t.Context(), "py-hosted", "demo", "1.0.0", filename, "tester"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if n := f.count(`SELECT count(*) FROM versions`); n != 0 {
		t.Fatalf("%d versions remain after deleting the only file, want 0", n)
	}
	after := f.lastPublished("py-hosted")
	if after == nil || !after.Equal(*before) {
		t.Fatalf("last_published_at after the delete = %v, want unchanged %v", after, before)
	}
}

func TestLastPublished_OnlyAdvancesForANewerDatedVersion(t *testing.T) {
	cases := []struct {
		name        string
		publishedAt string // SQL expression for versions.published_at
		wantSame    bool
	}{
		{name: "older version leaves it", publishedAt: "now() - interval '1 day'", wantSame: true},
		{name: "undated version leaves it", publishedAt: "NULL", wantSame: true},
		{name: "newer version advances it", publishedAt: "now() + interval '1 day'", wantSame: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEvictFixture(t)
			f.hostedRepo("py-hosted", model.ProtocolPyPI)
			f.publishPyPI("py-hosted", "demo", "1.0.0")
			before := f.lastPublished("py-hosted")

			f.exec(`INSERT INTO versions (package_id, version, published_at)
				SELECT id, '2.0.0', ` + tc.publishedAt + ` FROM packages`)
			after := f.lastPublished("py-hosted")
			if before == nil || after == nil {
				t.Fatalf("last_published_at before=%v after=%v, want both set", before, after)
			}
			if got := after.Equal(*before); got != tc.wantSame {
				t.Fatalf("last_published_at unchanged = %v, want %v (before %v, after %v)", got, tc.wantSame, before, after)
			}
		})
	}
}

// Every publish updates the repository row, so concurrent publishes into one
// repository serialise on it. This guards against that turning into a
// deadlock or a lost update.
func TestLastPublished_ConcurrentPublishesAllSucceedAndTheColumnIsTheMax(t *testing.T) {
	f := newEvictFixture(t)
	f.hostedRepo("py-hosted", model.ProtocolPyPI)

	const workers, perWorker = 8, 6
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWorker {
				pkg := fmt.Sprintf("pkg-%d", (w+i)%4) // packages shared between workers
				version := fmt.Sprintf("1.%d.%d", w, i)
				body := []byte(pkg + version)
				if _, err := f.reg.Publish(t.Context(), registry.PublishParams{
					Repo: "py-hosted", Package: pkg, Version: version, Filename: pkg + "-" + version + ".tar.gz",
					Body: bytes.NewReader(body), Size: int64(len(body)), Immutable: true,
				}); err != nil {
					errs <- fmt.Errorf("worker %d publish %d: %w", w, i, err)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if n := f.count(`SELECT count(*) FROM versions`); n != workers*perWorker {
		t.Fatalf("%d versions published, want %d", n, workers*perWorker)
	}
	var matches bool
	if err := f.db.Pool().QueryRow(t.Context(),
		`SELECT r.last_published_at = (SELECT max(v.published_at) FROM versions v) FROM repositories r WHERE r.name = 'py-hosted'`).Scan(&matches); err != nil {
		t.Fatalf("comparing last_published_at to the newest version: %v", err)
	}
	if !matches {
		t.Fatal("last_published_at is not the newest published_at after concurrent publishes")
	}
}

// The tag upsert always sets target_digest, and UPDATE OF fires whenever the
// column is in the SET list. A proxy refreshing an unchanged tag, or a re-push
// of the same digest, must not count as a publish; a tag that moves must.
func TestLastPublished_OnlyATagThatMovesCountsAsAPublish(t *testing.T) {
	const (
		first  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		second = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	)
	cases := []struct {
		name     string
		update   string
		wantSame bool
	}{
		{name: "same digest leaves it", update: "UPDATE tags SET target_digest = '" + first + "'", wantSame: true},
		{name: "moved tag advances it", update: "UPDATE tags SET target_digest = '" + second + "'", wantSame: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEvictFixture(t)
			f.hostedRepo("oci-hosted", model.ProtocolOCI)
			f.exec(`INSERT INTO blobs (digest, size, storage_backend) VALUES ('` + first + `', 1, 'test'), ('` + second + `', 1, 'test')`)
			f.exec(`INSERT INTO packages (repo_id, normalized_name, display_name)
				SELECT id, 'app', 'app' FROM repositories WHERE name = 'oci-hosted'`)
			f.exec(`INSERT INTO tags (repo_id, package_id, name, target_digest)
				SELECT p.repo_id, p.id, 'latest', '` + first + `' FROM packages p`)
			if f.lastPublished("oci-hosted") == nil {
				t.Fatal("last_published_at is NULL after a tag insert")
			}
			// Wind it back, so that any write by the update shows.
			f.exec(`UPDATE repositories SET last_published_at = '2000-01-01T00:00:00Z' WHERE name = 'oci-hosted'`)
			before := f.lastPublished("oci-hosted")

			f.exec(tc.update)
			after := f.lastPublished("oci-hosted")
			if got := after.Equal(*before); got != tc.wantSame {
				t.Fatalf("last_published_at unchanged = %v, want %v (before %v, after %v)", got, tc.wantSame, before, after)
			}
		})
	}
}
