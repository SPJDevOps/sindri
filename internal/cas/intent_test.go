package cas_test

import (
	"bytes"
	"context"
	"io"
	"slices"
	"sync"
	"testing"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/storage"
	"github.com/spj/sindri/internal/storage/memstore"
)

// callLog records the order of reservations and storage calls, which is the
// whole content of the write-intent rule (ADR 19): the row is touched before
// any byte is written or relied on.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *callLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// firstIndex is the position of the first call named call, or -1.
func (l *callLog) firstIndex(call string) int {
	return slices.Index(l.snapshot(), call)
}

type recordingReserver struct {
	log    *callLog
	exists bool
}

func (r *recordingReserver) Reserve(_ context.Context, _ model.Digest, _ int64) error {
	r.log.add("reserve")
	return nil
}

func (r *recordingReserver) Touch(_ context.Context, _ model.Digest) (bool, error) {
	r.log.add("touch")
	return r.exists, nil
}

// recordingStore logs the storage calls the intent must precede.
type recordingStore struct {
	storage.BlobStore
	log *callLog
}

func (s recordingStore) Put(ctx context.Context, d model.Digest, r io.Reader, size int64) error {
	s.log.add("put")
	return s.BlobStore.Put(ctx, d, r, size)
}

func (s recordingStore) Stat(ctx context.Context, d model.Digest) (storage.BlobInfo, error) {
	s.log.add("stat")
	return s.BlobStore.Stat(ctx, d)
}

func (s recordingStore) CommitUpload(ctx context.Context, id storage.UploadID, d model.Digest) error {
	s.log.add("commit")
	return s.BlobStore.CommitUpload(ctx, id, d)
}

func newRecordingCAS(t *testing.T, exists bool) (*cas.Store, *callLog, *memstore.Store) {
	t.Helper()
	log := &callLog{}
	mem := memstore.New()
	store := cas.New(recordingStore{BlobStore: mem, log: log},
		cas.WithReserver(&recordingReserver{log: log, exists: exists}))
	return store, log, mem
}

func assertBefore(t *testing.T, log *callLog, first, then string) {
	t.Helper()
	i, j := log.firstIndex(first), log.firstIndex(then)
	if i < 0 {
		t.Fatalf("%s never happened; calls %v", first, log.snapshot())
	}
	if j >= 0 && j < i {
		t.Fatalf("%s happened before %s; calls %v", then, first, log.snapshot())
	}
}

func TestStore_PutBytesReservesBeforeWriting(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, mem *memstore.Store, payload []byte)
	}{
		{name: "NewBlob", setup: func(*testing.T, *memstore.Store, []byte) {}},
		{
			// A deduplicated write relies on bytes it found. The reservation
			// must come before the Stat that finds them, or a collector could
			// delete them between the Stat and the placement.
			name: "BlobAlreadyStored",
			setup: func(t *testing.T, mem *memstore.Store, payload []byte) {
				t.Helper()
				if err := mem.Put(t.Context(), digestOf(payload), bytes.NewReader(payload), int64(len(payload))); err != nil {
					t.Fatalf("seeding: %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, log, mem := newRecordingCAS(t, true)
			payload := []byte(`{"schemaVersion":2}`)
			tt.setup(t, mem, payload)

			if _, err := store.PutBytes(t.Context(), payload); err != nil {
				t.Fatalf("PutBytes: %v", err)
			}
			assertBefore(t, log, "reserve", "stat")
			assertBefore(t, log, "reserve", "put")
		})
	}
}

func TestStore_CommitReservesBeforeCommitUpload(t *testing.T) {
	store, log, _ := newRecordingCAS(t, false)
	payload := []byte("a layer of unknown digest")

	if _, err := store.Put(t.Context(), bytes.NewReader(payload), model.Digest{}, int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	assertBefore(t, log, "reserve", "commit")
}

// A mismatched upload must not reserve anything: the reservation is for the
// verified digest, and an unverified claim never creates a row.
func TestStore_MismatchedCommitReservesNothing(t *testing.T) {
	store, log, _ := newRecordingCAS(t, false)
	payload := []byte("these bytes")

	_, err := store.Put(t.Context(), bytes.NewReader(payload), digestOf([]byte("other bytes")), int64(len(payload)))
	if err == nil {
		t.Fatal("Put accepted a mismatched digest")
	}
	if i := log.firstIndex("reserve"); i >= 0 {
		t.Fatalf("a rejected upload reserved a row; calls %v", log.snapshot())
	}
}

func TestStore_DedupTouchesBeforeStat(t *testing.T) {
	store, log, mem := newRecordingCAS(t, true)
	payload := []byte("already here")
	if err := mem.Put(t.Context(), digestOf(payload), bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if _, err := store.Put(t.Context(), bytes.NewReader(payload), digestOf(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	assertBefore(t, log, "touch", "stat")
	if i := log.firstIndex("reserve"); i >= 0 {
		t.Fatalf("a claimed digest created a row before verification; calls %v", log.snapshot())
	}
}

func TestStore_HasReserver(t *testing.T) {
	if cas.New(memstore.New()).HasReserver() {
		t.Fatal("a store built without a reserver reports one")
	}
	store, _, _ := newRecordingCAS(t, false)
	if !store.HasReserver() {
		t.Fatal("a store built with a reserver reports none")
	}
}
