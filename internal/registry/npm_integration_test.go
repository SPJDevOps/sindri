//go:build integration

package registry_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"maps"
	"sync"
	"testing"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

type npmFixture struct {
	*evictFixture
	repo string
}

func newNPMFixture(t *testing.T) *npmFixture {
	t.Helper()
	f := &npmFixture{evictFixture: newEvictFixture(t), repo: "npm-hosted"}
	f.hosted(f.repo)
	return f
}

func (f *npmFixture) hosted(name string) {
	f.t.Helper()
	if _, err := f.reg.CreateRepository(f.t.Context(), registry.CreateRepositoryParams{
		Name: name, Type: model.RepoTypeHosted, Protocol: model.ProtocolNPM,
	}); err != nil {
		f.t.Fatalf("CreateRepository %s: %v", name, err)
	}
}

// publish publishes a version whose tarball is body, as the adapter would:
// the digest is the one this function computed over the bytes.
func (f *npmFixture) publishTo(repo, pkg, version, tag, body string) error {
	f.t.Helper()
	_, err := f.reg.Publish(f.t.Context(), registry.PublishParams{
		Repo: repo, Package: pkg, Version: version,
		Filename:         pkg + "-" + version + ".tgz",
		MediaType:        "application/octet-stream",
		Body:             bytes.NewReader([]byte(body)),
		Size:             int64(len(body)),
		ExpectDigest:     model.NewDigest(model.AlgorithmSHA256, sha256Sum(body)),
		ExclusiveVersion: true,
		DistTag:          tag,
		Actor:            "alice",
		Provenance:       model.Provenance{SourceType: model.ProvenanceNPMPublish},
	})
	return err
}

func (f *npmFixture) publish(pkg, version, tag, body string) error {
	f.t.Helper()
	return f.publishTo(f.repo, pkg, version, tag, body)
}

func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func (f *npmFixture) mustPublish(pkg, version, tag, body string) {
	f.t.Helper()
	if err := f.publish(pkg, version, tag, body); err != nil {
		f.t.Fatalf("publish %s@%s: %v", pkg, version, err)
	}
}

func (f *npmFixture) distTags(pkg string) map[string]string {
	f.t.Helper()
	got, err := f.reg.GetNPMPackage(f.t.Context(), f.repo, pkg)
	if err != nil {
		f.t.Fatalf("GetNPMPackage: %v", err)
	}
	return got.DistTags
}

func (f *npmFixture) mustMatch(got, want map[string]string) {
	f.t.Helper()
	if !maps.Equal(got, want) {
		f.t.Fatalf("dist-tags = %v, want %v", got, want)
	}
}

type tagEvent struct {
	tag, from, to, actor string
}

// events returns the dist-tag history of a package in order. A NULL version
// reads as the empty string.
func (f *npmFixture) events(pkg string) []tagEvent {
	f.t.Helper()
	rows, err := f.db.Pool().Query(f.t.Context(),
		`SELECT tag, coalesce(from_version, ''), coalesce(to_version, ''), actor
		   FROM dist_tag_events WHERE package = $1 ORDER BY id`, pkg)
	if err != nil {
		f.t.Fatalf("reading events: %v", err)
	}
	defer rows.Close()
	var out []tagEvent
	for rows.Next() {
		var e tagEvent
		if err := rows.Scan(&e.tag, &e.from, &e.to, &e.actor); err != nil {
			f.t.Fatalf("scanning event: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func TestPublishNPM_FirstPublishSetsTagAndLatest(t *testing.T) {
	f := newNPMFixture(t)
	f.mustPublish("left-pad", "1.0.0", "next", "bytes-1")

	f.mustMatch(f.distTags("left-pad"), map[string]string{"next": "1.0.0", "latest": "1.0.0"})
	events := f.events("left-pad")
	if len(events) != 2 {
		t.Fatalf("events = %+v, want two", events)
	}
	for _, e := range events {
		if e.actor != "alice" || e.from != "" || e.to != "1.0.0" {
			t.Errorf("event %+v, want actor alice, from NULL, to 1.0.0", e)
		}
	}
}

func TestPublishNPM_LaterPublishMovesOnlyItsTag(t *testing.T) {
	f := newNPMFixture(t)
	f.mustPublish("left-pad", "1.0.0", "next", "bytes-1")
	f.mustPublish("left-pad", "1.1.0", "beta", "bytes-2")

	f.mustMatch(f.distTags("left-pad"), map[string]string{"next": "1.0.0", "beta": "1.1.0", "latest": "1.0.0"})
}

func TestPublishNPM_IdenticalBytesAreRefused(t *testing.T) {
	f := newNPMFixture(t)
	f.mustPublish("left-pad", "1.0.0", "latest", "bytes-1")

	if err := f.publish("left-pad", "1.0.0", "latest", "bytes-1"); !errors.Is(err, registry.ErrVersionExists) {
		t.Fatalf("second publish = %v, want ErrVersionExists", err)
	}
}

func TestPublishNPM_ConcurrentSameVersionHasOneWinner(t *testing.T) {
	f := newNPMFixture(t)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = f.publish("left-pad", "1.0.0", "latest", "bytes-"+string(rune('a'+i)))
		}()
	}
	wg.Wait()

	var winners, exists int
	for _, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, registry.ErrVersionExists):
			exists++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if winners != 1 || exists != 1 {
		t.Fatalf("errors = %v, want one nil and one ErrVersionExists", errs)
	}
	// latest is the only tag the publish set, so one winner means one event.
	if events := f.events("left-pad"); len(events) != 1 {
		t.Fatalf("events = %+v, want exactly one", events)
	}
}

func TestPublish_NpmRepositoryRefusesPublishWithoutDistTag(t *testing.T) {
	f := newNPMFixture(t)

	if err := f.publish("left-pad", "1.0.0", "", "bytes-1"); !errors.Is(err, registry.ErrInvalidInput) {
		t.Fatalf("publish without a dist-tag = %v, want ErrInvalidInput", err)
	}
	if n := f.count(`SELECT count(*) FROM files`); n != 0 {
		t.Fatalf("files = %d, want nothing placed", n)
	}
}

func TestPublish_NpmRepositoryRefusesPublishThatMayReplaceAVersion(t *testing.T) {
	f := newNPMFixture(t)
	f.mustPublish("left-pad", "1.0.0", "latest", "bytes-1")

	_, err := f.reg.Publish(t.Context(), registry.PublishParams{
		Repo: f.repo, Package: "left-pad", Version: "1.0.0",
		Filename:   "left-pad-1.0.0.tgz",
		Body:       bytes.NewReader([]byte("other bytes")),
		Size:       int64(len("other bytes")),
		DistTag:    "latest",
		Actor:      "alice",
		Provenance: model.Provenance{SourceType: model.ProvenanceNPMPublish},
	})
	if !errors.Is(err, registry.ErrInvalidInput) {
		t.Fatalf("publish without ExclusiveVersion = %v, want ErrInvalidInput", err)
	}
	_, rc, err := f.reg.GetNPMTarball(t.Context(), f.repo, "left-pad", "left-pad-1.0.0.tgz")
	if err != nil {
		t.Fatalf("GetNPMTarball: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if body, _ := io.ReadAll(rc); string(body) != "bytes-1" {
		t.Fatalf("tarball = %q, want the original bytes", body)
	}
	if n := f.count(`SELECT count(*) FROM blobs`); n != 1 {
		t.Fatalf("blobs = %d, want the refused bytes never stored", n)
	}
}

func TestRemoveDistTag_LatestIsRefused(t *testing.T) {
	f := newNPMFixture(t)
	f.mustPublish("left-pad", "1.0.0", "latest", "bytes-1")

	err := f.reg.RemoveDistTag(t.Context(), f.repo, "left-pad", "latest", "alice")
	if !errors.Is(err, registry.ErrInvalidInput) {
		t.Fatalf("RemoveDistTag(latest) = %v, want ErrInvalidInput", err)
	}
	f.mustMatch(f.distTags("left-pad"), map[string]string{"latest": "1.0.0"})
}

func TestRemoveDistTag_RecordsTheRemoval(t *testing.T) {
	f := newNPMFixture(t)
	f.mustPublish("left-pad", "1.0.0", "next", "bytes-1")

	if err := f.reg.RemoveDistTag(t.Context(), f.repo, "left-pad", "next", "bob"); err != nil {
		t.Fatalf("RemoveDistTag: %v", err)
	}
	f.mustMatch(f.distTags("left-pad"), map[string]string{"latest": "1.0.0"})
	events := f.events("left-pad")
	if last := events[len(events)-1]; last != (tagEvent{"next", "1.0.0", "", "bob"}) {
		t.Fatalf("last event = %+v", last)
	}
}

func TestSetDistTag_UnknownVersionIsNotFound(t *testing.T) {
	f := newNPMFixture(t)
	f.mustPublish("left-pad", "1.0.0", "latest", "bytes-1")

	err := f.reg.SetDistTag(t.Context(), f.repo, "left-pad", "next", "9.9.9", "alice")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("SetDistTag = %v, want ErrNotFound", err)
	}
	f.mustMatch(f.distTags("left-pad"), map[string]string{"latest": "1.0.0"})
}

func TestSetDistTag_RecordsFromAndTo(t *testing.T) {
	f := newNPMFixture(t)
	f.mustPublish("left-pad", "1.0.0", "latest", "bytes-1")
	f.mustPublish("left-pad", "1.1.0", "next", "bytes-2")

	if err := f.reg.SetDistTag(t.Context(), f.repo, "left-pad", "latest", "1.1.0", "bob"); err != nil {
		t.Fatalf("SetDistTag: %v", err)
	}
	f.mustMatch(f.distTags("left-pad"), map[string]string{"latest": "1.1.0", "next": "1.1.0"})
	events := f.events("left-pad")
	if last := events[len(events)-1]; last != (tagEvent{"latest", "1.0.0", "1.1.0", "bob"}) {
		t.Fatalf("last event = %+v", last)
	}
}

func TestSetDeprecations_UnknownVersionWritesNothing(t *testing.T) {
	f := newNPMFixture(t)
	f.mustPublish("left-pad", "1.0.0", "latest", "bytes-1")

	err := f.reg.SetDeprecations(t.Context(), f.repo, "left-pad", map[string]*string{"1.0.0": new("old"), "9.9.9": new("old")})
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("SetDeprecations = %v, want ErrNotFound", err)
	}
	got, err := f.reg.GetNPMPackage(t.Context(), f.repo, "left-pad")
	if err != nil {
		t.Fatalf("GetNPMPackage: %v", err)
	}
	if got.Versions[0].Deprecated != "" {
		t.Fatalf("1.0.0 deprecated = %q, want unchanged", got.Versions[0].Deprecated)
	}
}

func TestSetDeprecations_EmptyMessageClears(t *testing.T) {
	f := newNPMFixture(t)
	f.mustPublish("left-pad", "1.0.0", "latest", "bytes-1")
	deprecated := func() string {
		got, err := f.reg.GetNPMPackage(t.Context(), f.repo, "left-pad")
		if err != nil {
			t.Fatalf("GetNPMPackage: %v", err)
		}
		return got.Versions[0].Deprecated
	}

	if err := f.reg.SetDeprecations(t.Context(), f.repo, "left-pad", map[string]*string{"1.0.0": new("use something else")}); err != nil {
		t.Fatalf("deprecate: %v", err)
	}
	if got := deprecated(); got != "use something else" {
		t.Fatalf("deprecated = %q", got)
	}
	if err := f.reg.SetDeprecations(t.Context(), f.repo, "left-pad", map[string]*string{"1.0.0": new("")}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := deprecated(); got != "" {
		t.Fatalf("deprecated = %q, want cleared", got)
	}
	if n := f.count(`SELECT count(*) FROM versions WHERE deprecated IS NULL`); n != 1 {
		t.Fatalf("a cleared message must be NULL, rows with NULL = %d", n)
	}
}

func TestSetDeprecations_NilLeavesAVersionAsItIs(t *testing.T) {
	f := newNPMFixture(t)
	f.mustPublish("left-pad", "1.0.0", "latest", "bytes-1")
	if err := f.reg.SetDeprecations(t.Context(), f.repo, "left-pad", map[string]*string{"1.0.0": new("old")}); err != nil {
		t.Fatalf("deprecate: %v", err)
	}
	if err := f.reg.SetDeprecations(t.Context(), f.repo, "left-pad", map[string]*string{"1.0.0": nil}); err != nil {
		t.Fatalf("SetDeprecations with nil: %v", err)
	}
	got, err := f.reg.GetNPMPackage(t.Context(), f.repo, "left-pad")
	if err != nil {
		t.Fatalf("GetNPMPackage: %v", err)
	}
	if got.Versions[0].Deprecated != "old" {
		t.Fatalf("deprecated = %q, want it unchanged", got.Versions[0].Deprecated)
	}
	if err := f.reg.SetDeprecations(t.Context(), f.repo, "left-pad", map[string]*string{"9.9.9": nil}); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("nil for an unknown version = %v, want ErrNotFound", err)
	}
}

func TestDeleteFile_NpmRecordsRemovedTags(t *testing.T) {
	f := newNPMFixture(t)
	f.mustPublish("left-pad", "1.0.0", "next", "bytes-1") // also latest, being first
	f.mustPublish("left-pad", "1.1.0", "beta", "bytes-2")

	if err := f.reg.DeleteFile(t.Context(), f.repo, "left-pad", "1.0.0", "left-pad-1.0.0.tgz", "carol"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if n := f.count(`SELECT count(*) FROM dist_tags`); n != 1 {
		t.Fatalf("dist_tags = %d, want only beta", n)
	}
	removed := 0
	for _, e := range f.events("left-pad") {
		if e.actor == "carol" {
			removed++
			if e.from != "1.0.0" || e.to != "" || (e.tag != "next" && e.tag != "latest") {
				t.Errorf("removal event %+v", e)
			}
		}
	}
	if removed != 2 {
		t.Fatalf("removal events = %d, want one for next and one for latest", removed)
	}
}

func TestPublishNPM_OnlyTheFirstVersionSetsLatest(t *testing.T) {
	f := newNPMFixture(t)
	f.mustPublish("left-pad", "1.0.0", "latest", "bytes-1")
	f.mustPublish("left-pad", "1.1.0", "beta", "bytes-2")
	if err := f.reg.DeleteFile(t.Context(), f.repo, "left-pad", "1.0.0", "left-pad-1.0.0.tgz", "carol"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	f.mustMatch(f.distTags("left-pad"), map[string]string{"beta": "1.1.0"})

	f.mustPublish("left-pad", "2.0.0-beta.1", "beta", "bytes-3")

	f.mustMatch(f.distTags("left-pad"), map[string]string{"beta": "2.0.0-beta.1"})
}

func TestSetDistTag_ConcurrentMovesRecordAChain(t *testing.T) {
	f := newNPMFixture(t)
	f.mustPublish("left-pad", "1.0.0", "latest", "bytes-1")
	f.mustPublish("left-pad", "1.1.0", "beta", "bytes-2")
	f.mustPublish("left-pad", "1.2.0", "gamma", "bytes-3")

	var wg sync.WaitGroup
	for _, v := range []string{"1.1.0", "1.2.0"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.reg.SetDistTag(context.Background(), f.repo, "left-pad", "latest", v, "bob"); err != nil {
				t.Errorf("SetDistTag %s: %v", v, err)
			}
		}()
	}
	wg.Wait()

	var moves []tagEvent
	for _, e := range f.events("left-pad") {
		if e.tag == "latest" && e.actor == "bob" {
			moves = append(moves, e)
		}
	}
	if len(moves) != 2 || moves[0].from != "1.0.0" || moves[1].from != moves[0].to {
		t.Fatalf("latest moves = %+v, want a chain from 1.0.0", moves)
	}
	if final := f.distTags("left-pad")["latest"]; final != moves[1].to {
		t.Fatalf("latest = %s, last event says %s", final, moves[1].to)
	}
}

func TestRemoveDistTag_ConcurrentRemovalsRecordOnce(t *testing.T) {
	f := newNPMFixture(t)
	f.mustPublish("left-pad", "1.0.0", "next", "bytes-1")

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = f.reg.RemoveDistTag(context.Background(), f.repo, "left-pad", "next", "bob")
		}()
	}
	wg.Wait()

	var ok, missing int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, registry.ErrNotFound):
			missing++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || missing != 1 {
		t.Fatalf("errors = %v, want one success and one ErrNotFound", errs)
	}
	removals := 0
	for _, e := range f.events("left-pad") {
		if e.tag == "next" && e.to == "" {
			removals++
		}
	}
	if removals != 1 {
		t.Fatalf("removal events = %d, want 1", removals)
	}
}

func TestGetNPMTarball_ServesPlacedBytes(t *testing.T) {
	f := newNPMFixture(t)
	f.mustPublish("left-pad", "1.0.0", "latest", "bytes-1")

	file, rc, err := f.reg.GetNPMTarball(t.Context(), f.repo, "left-pad", "left-pad-1.0.0.tgz")
	if err != nil {
		t.Fatalf("GetNPMTarball: %v", err)
	}
	defer func() { _ = rc.Close() }()
	body, _ := io.ReadAll(rc)
	if string(body) != "bytes-1" || file.Version != "1.0.0" {
		t.Fatalf("got %q for version %q", body, file.Version)
	}
}

func TestGetNPMTarball_NotPlacedInOtherRepository(t *testing.T) {
	f := newNPMFixture(t)
	f.hosted("npm-other")
	f.mustPublish("left-pad", "1.0.0", "latest", "bytes-1")
	// The other repository holds the same package and the very same digest, under
	// another version, so only the placement of this file name can refuse the read.
	if err := f.publishTo("npm-other", "left-pad", "2.0.0", "latest", "bytes-1"); err != nil {
		t.Fatalf("publish to the other repository: %v", err)
	}
	if n := f.count(`SELECT count(*) FROM blobs`); n != 1 {
		t.Fatalf("blobs = %d, want one deduplicated blob", n)
	}

	held, heldBody, err := f.reg.GetNPMTarball(t.Context(), "npm-other", "left-pad", "left-pad-2.0.0.tgz")
	if err != nil {
		t.Fatalf("GetNPMTarball of what the other repository holds: %v", err)
	}
	_ = heldBody.Close()
	if want := model.NewDigest(model.AlgorithmSHA256, sha256Sum("bytes-1")); held.Digest != want {
		t.Fatalf("digest = %s, want the one both repositories share", held.Digest)
	}

	_, rc, err := f.reg.GetNPMTarball(t.Context(), "npm-other", "left-pad", "left-pad-1.0.0.tgz")
	if rc != nil {
		_ = rc.Close()
		t.Fatal("served bytes through a repository that does not place this file (I2)")
	}
	if !errors.Is(err, registry.ErrNotFound) && !errors.Is(err, registry.ErrNotPlaced) {
		t.Fatalf("err = %v, want ErrNotFound or ErrNotPlaced", err)
	}
}
