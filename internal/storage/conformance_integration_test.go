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

	backends := []struct {
		name     string
		newStore storagetest.NewStoreFunc
	}{
		{
			name: "Filesystem",
			newStore: func(t *testing.T) storage.BlobStore {
				s, err := fsstore.New(t.TempDir())
				if err != nil {
					t.Fatalf("fsstore.New: %v", err)
				}
				return s
			},
		},
		{
			name: "S3",
			newStore: func(t *testing.T) storage.BlobStore {
				// One container, one prefix per case, so the cases stay
				// independent without paying for a container each.
				s, err := s3store.New(t.Context(), minio.Config(prefixFor(t)))
				if err != nil {
					t.Fatalf("s3store.New: %v", err)
				}
				return s
			},
		},
	}

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			storagetest.RunSuite(t, backend.newStore)
		})
	}
}

var prefixCounter int

func prefixFor(t *testing.T) string {
	t.Helper()
	prefixCounter++
	return fmt.Sprintf("case-%d", prefixCounter)
}
