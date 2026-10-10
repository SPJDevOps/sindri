package fsstore_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/storage"
	"github.com/spj/sindri/internal/storage/fsstore"
	"github.com/spj/sindri/internal/storage/storagetest"
)

// The filesystem backend runs the shared conformance suite without Docker, so
// every `make test` exercises it. The S3 backend runs the same suite under the
// integration tag. See internal/storage/conformance_integration_test.go.
func TestFSStore_Conformance(t *testing.T) {
	storagetest.RunSuite(t, func(t *testing.T) storage.BlobStore {
		return newStore(t)
	}, storagetest.WithReopen(reopenFS))
}

// reopenFS is a second store over the same root, as a restarted process sees it.
func reopenFS(t *testing.T) (storage.BlobStore, func() storage.BlobStore) {
	root := t.TempDir()
	open := func() storage.BlobStore {
		s, err := fsstore.New(root)
		if err != nil {
			t.Fatalf("fsstore.New: %v", err)
		}
		return s
	}
	return open(), open
}

func TestNew_RejectsAnEmptyRoot(t *testing.T) {
	if _, err := fsstore.New("  "); err == nil {
		t.Fatal("New(\"  \") succeeded, want an error")
	}
}

func TestFSStore_FansOutByDigestPrefix(t *testing.T) {
	s := newStore(t)
	payload := []byte("fanned out")
	digest := digestOf(payload)

	if err := s.Put(t.Context(), digest, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	h := digest.Hex()
	want := filepath.Join(s.Root(), "blobs", "sha256", h[0:2], h[2:4], h)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("blob is not at the fanned-out path %s: %v", want, err)
	}
}

func TestFSStore_PutRejectsASizeMismatch(t *testing.T) {
	s := newStore(t)
	payload := []byte("claimed longer than it is")
	digest := digestOf(payload)

	err := s.Put(t.Context(), digest, bytes.NewReader(payload), int64(len(payload))+10)
	if err == nil {
		t.Fatal("Put with a wrong size succeeded")
	}
	if _, err := s.Stat(t.Context(), digest); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a short write reached the content addressed key: %v", err)
	}
}

// A rejected write must not leave anything behind in staging either, or a
// pathological client could fill the disk one failed upload at a time.
func TestFSStore_FailedPutLeavesNoStagingFile(t *testing.T) {
	s := newStore(t)
	payload := []byte("discarded")

	if err := s.Put(t.Context(), digestOf(payload), bytes.NewReader(payload), 99); err == nil {
		t.Fatal("Put with a wrong size succeeded")
	}

	entries, err := os.ReadDir(filepath.Join(s.Root(), "staging"))
	if err != nil {
		t.Fatalf("reading staging: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging holds %d leftover entries", len(entries))
	}
}

func TestFSStore_ProbeReportsAReadOnlyRoot(t *testing.T) {
	s := newStore(t)

	if err := s.Probe(t.Context()); err != nil {
		t.Fatalf("Probe on a writable root: %v", err)
	}

	staging := filepath.Join(s.Root(), "staging")
	if err := os.Chmod(staging, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(staging, 0o750) })

	if os.Geteuid() == 0 {
		t.Skip("running as root, which ignores directory permissions")
	}
	if err := s.Probe(t.Context()); err == nil {
		t.Fatal("Probe on a read-only staging directory succeeded")
	}
}

func TestFSStore_RejectsAMalformedUploadID(t *testing.T) {
	s := newStore(t)

	if _, err := s.Append(t.Context(), storage.UploadID("../../etc/passwd"), bytes.NewReader(nil)); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("Append with a traversal id = %v, want ErrUploadNotFound", err)
	}
}

func newStore(t *testing.T) *fsstore.Store {
	t.Helper()

	s, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	return s
}

func digestOf(payload []byte) model.Digest {
	d := model.NewDigester(model.AlgorithmSHA256)
	_, _ = d.Write(payload)
	return d.Digest()
}
