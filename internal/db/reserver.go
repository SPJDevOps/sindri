package db

import (
	"context"
	"fmt"

	"github.com/spj/sindri/internal/model"
)

// BlobReserver records write intent on the blobs table (ADR 19). It satisfies
// cas.Reserver, so that the content addressable store can touch a blob's row
// before it writes or relies on the bytes without importing this package.
//
// Each call runs outside any caller transaction and commits on return: an
// intent that is still uncommitted when the collector looks is invisible to it.
type BlobReserver struct {
	q       *Queries
	backend string
}

// NewBlobReserver returns a reserver writing through d. backend names the
// storage backend recorded on rows it creates.
func NewBlobReserver(d *DB, backend string) *BlobReserver {
	return &BlobReserver{q: d.Queries(), backend: backend}
}

// Reserve creates or refreshes the row for a verified digest.
func (r *BlobReserver) Reserve(ctx context.Context, digest model.Digest, size int64) error {
	if err := r.q.ReserveBlob(ctx, ReserveBlobParams{
		Digest:         digest.String(),
		Size:           size,
		StorageBackend: r.backend,
	}); err != nil {
		return fmt.Errorf("reserving blob %s: %w", digest, err)
	}
	return nil
}

// Touch refreshes the row for a claimed digest, if there is one.
func (r *BlobReserver) Touch(ctx context.Context, digest model.Digest) (bool, error) {
	n, err := r.q.TouchBlob(ctx, digest.String())
	if err != nil {
		return false, fmt.Errorf("touching blob %s: %w", digest, err)
	}
	return n > 0, nil
}
