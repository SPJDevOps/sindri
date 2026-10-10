// Package storage is the only part of Sindri that touches bytes at rest.
//
// A BlobStore is keyed by content digest; it knows nothing about repositories,
// packages, or callers. Because deduplication means a blob one tenant pushed is
// physically present for every tenant, nothing in this package may be reached
// without a placement check first. See invariant I2 in ARCHITECTURE.md.
package storage

import (
	"context"
	"io"
	"time"

	"github.com/spj/sindri/internal/model"
)

// UploadID identifies a staged upload in progress.
type UploadID string

// BlobInfo is what a store knows about a blob without reading it.
type BlobInfo struct {
	Digest model.Digest
	Size   int64
}

// BlobStore stores and retrieves immutable blobs by digest.
//
// Writes come in two shapes. Put commits bytes straight to the content
// addressed key and is therefore only safe when the caller computed the digest
// from exactly those bytes, which in practice means a small in-memory payload.
// Everything streaming goes through the staged upload methods: bytes land under
// a staging key, and only CommitUpload moves them to the content addressed key,
// after the caller has verified the digest. Never write unverified bytes to a
// content addressed key, and never trust a digest a client supplied.
//
// Implementations must be safe for concurrent use.
type BlobStore interface {
	// Put writes a blob whose digest the caller has already computed from the
	// bytes in r. Putting a digest that already exists is a successful no-op:
	// content addressing makes the write redundant by definition.
	Put(ctx context.Context, digest model.Digest, r io.Reader, size int64) error

	// Get returns the whole blob. The caller closes the reader.
	Get(ctx context.Context, digest model.Digest) (io.ReadCloser, error)

	// GetRange returns length bytes starting at off. A negative length means
	// "to the end of the blob"; a length running past the end is truncated
	// there. An offset outside the blob is ErrInvalidRange.
	GetRange(ctx context.Context, digest model.Digest, off, length int64) (io.ReadCloser, error)

	// Stat reports what the store knows about a blob, or ErrNotFound.
	Stat(ctx context.Context, digest model.Digest) (BlobInfo, error)

	// Delete removes a blob. Deleting an absent blob is not an error, so that
	// a retried reclamation pass is safe.
	Delete(ctx context.Context, digest model.Digest) error

	// CreateUpload opens a staging area for a blob whose digest is not known yet.
	CreateUpload(ctx context.Context) (UploadID, error)

	// Append writes r to the end of the staged upload and returns the total
	// number of bytes staged so far, which is the offset the next Append starts
	// at.
	Append(ctx context.Context, id UploadID, r io.Reader) (int64, error)

	// CommitUpload moves the staged bytes to the content addressed key for
	// digest. The caller must have computed digest over the bytes it appended;
	// this is the moment unverified bytes would become trusted content, so it
	// is the one call that must never be made on a claim alone.
	CommitUpload(ctx context.Context, id UploadID, digest model.Digest) error

	// AbortUpload discards a staged upload. Aborting an upload that is already
	// gone is not an error.
	AbortUpload(ctx context.Context, id UploadID) error

	// PurgeUploads discards every staged upload last written to before
	// olderThan, including staging state a crashed process left behind, and
	// reports how many it removed. It never touches committed blobs. A purged
	// upload behaves as unknown: Append and CommitUpload return
	// ErrUploadNotFound, and the client starts again.
	PurgeUploads(ctx context.Context, olderThan time.Time) (int, error)
}

// Prober is implemented by stores that can cheaply check they are reachable.
// It is optional: the readiness endpoint probes a store only when it is
// implemented, so a backend without a cheap check is simply not probed.
type Prober interface {
	Probe(ctx context.Context) error
}

// ValidOffset reports whether off is a readable position in a blob of size
// bytes.
//
// Every backend shares this so that a range check cannot differ between them.
// The one subtlety is the empty blob: offset 0 is valid there, even though it
// is not less than the size, because reading nothing from an empty blob is a
// legitimate request.
func ValidOffset(off, size int64) bool {
	if off < 0 {
		return false
	}
	if size == 0 {
		return off == 0
	}
	return off < size
}
