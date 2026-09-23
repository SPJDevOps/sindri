package memstore_test

import (
	"testing"

	"github.com/spj/sindri/internal/storage"
	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/storage/storagetest"
)

// The in-memory store stands in for a real backend in tests above the storage
// layer, so it runs the same conformance suite. A fake that behaves differently
// from the thing it fakes turns every test above it into a false positive.
func TestMemStore_Conformance(t *testing.T) {
	storagetest.RunSuite(t, func(_ *testing.T) storage.BlobStore {
		return memstore.New()
	})
}
