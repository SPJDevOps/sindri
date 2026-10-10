//go:build integration

package storage_test

import (
	"fmt"
	"testing"

	"github.com/spj/sindri/internal/storage"
	"github.com/spj/sindri/internal/storage/fsstore"
	"github.com/spj/sindri/internal/storage/s3store"
	"github.com/spj/sindri/internal/storage/storagetest"
	"github.com/spj/sindri/internal/testsupport"
)

// TestBlobStore_Conformance is the gate for milestone M1: one suite, run once
// per backend.
//
// "Choosing between them is configuration, never a code path"
// (ARCHITECTURE.md section 6) is a claim this test is the whole evidence for.
// Do not fork it into a per-backend suite: two suites drift, and the drift is
// invisible until a backend swap breaks a production registry.
func TestBlobStore_Conformance(t *testing.T) {
	minio := testsupport.StartMinIO(t)

	openFS := func(t *testing.T, root string) storage.BlobStore {
		s, err := fsstore.New(root)
		if err != nil {
			t.Fatalf("fsstore.New: %v", err)
		}
		return s
	}
	openS3 := func(t *testing.T, prefix string) storage.BlobStore {
		s, err := s3store.New(t.Context(), minio.Config(prefix))
		if err != nil {
			t.Fatalf("s3store.New: %v", err)
		}
		return s
	}

	backends := []struct {
		name     string
		newStore storagetest.NewStoreFunc
		reopen   storagetest.ReopenFunc
	}{
		{
			name:     "Filesystem",
			newStore: func(t *testing.T) storage.BlobStore { return openFS(t, t.TempDir()) },
			reopen: func(t *testing.T) (storage.BlobStore, func() storage.BlobStore) {
				root := t.TempDir()
				return openFS(t, root), func() storage.BlobStore { return openFS(t, root) }
			},
		},
		{
			name: "S3",
			// One container, one prefix per case, so the cases stay
			// independent without paying for a container each.
			newStore: func(t *testing.T) storage.BlobStore { return openS3(t, prefixFor(t)) },
			reopen: func(t *testing.T) (storage.BlobStore, func() storage.BlobStore) {
				prefix := prefixFor(t)
				return openS3(t, prefix), func() storage.BlobStore { return openS3(t, prefix) }
			},
		},
	}

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			storagetest.RunSuite(t, backend.newStore, storagetest.WithReopen(backend.reopen))
		})
	}
}

var prefixCounter int

func prefixFor(t *testing.T) string {
	t.Helper()
	prefixCounter++
	return fmt.Sprintf("case-%d", prefixCounter)
}
