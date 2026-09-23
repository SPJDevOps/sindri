// Package cas is the content addressable store: the one place that decides a
// blob's digest and the one place that commits bytes to a content addressed
// key.
//
// Its whole job is the rule in ARCHITECTURE.md section 6: hash what was
// actually received, compare it against what was claimed, and only then commit.
// A client supplied digest is a claim to be checked, never a key to write
// under. Everything above this package should be unable to write a blob any
// other way.
//
// Placement is deliberately not here. This package answers "what are these
// bytes", never "is this caller allowed to read them"; that check belongs to
// the registry, on every read, per invariant I2.
package cas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/storage"
)

var (
	// ErrDigestMismatch means the bytes received hash to something other than
	// the digest the caller claimed. The staged bytes are discarded.
	ErrDigestMismatch = errors.New("digest mismatch")

	// ErrSizeMismatch means the bytes received are not the length the caller
	// claimed, which is usually a truncated upload.
	ErrSizeMismatch = errors.New("size mismatch")
)

// Store computes digests and commits verified blobs to a BlobStore.
type Store struct {
	blobs storage.BlobStore
}

// New returns a Store writing through blobs.
func New(blobs storage.BlobStore) *Store {
	if blobs == nil {
		panic("cas.New: nil BlobStore")
	}
	return &Store{blobs: blobs}
}

// Put streams r into staging, hashes it, and commits it only if the result
// matches expected.
//
// A zero expected means the caller does not know the digest yet and wants to be
// told what it was; the bytes are still hashed and still committed under their
// real digest, so nothing is trusted on a claim. A negative size means unknown.
func (s *Store) Put(ctx context.Context, r io.Reader, expected model.Digest, size int64) (storage.BlobInfo, error) {
	// A blob we already hold needs no upload at all. This is the dedup that
	// invariant I1 promises: one blob per digest, however many repositories
	// place it.
	if expected.Valid() {
		if info, err := s.blobs.Stat(ctx, expected); err == nil {
			if _, err := io.Copy(io.Discard, r); err != nil {
				return storage.BlobInfo{}, fmt.Errorf("draining a deduplicated upload of %s: %w", expected, err)
			}
			return info, nil
		} else if !errors.Is(err, storage.ErrNotFound) {
			return storage.BlobInfo{}, err
		}
	}

	upload, err := s.NewUpload(ctx)
	if err != nil {
		return storage.BlobInfo{}, err
	}

	if _, err := upload.Append(ctx, r); err != nil {
		upload.abortQuietly(ctx)
		return storage.BlobInfo{}, fmt.Errorf("staging blob: %w", err)
	}

	return upload.Commit(ctx, expected, size)
}

// PutBytes commits an in-memory payload under its own digest.
//
// Manifests, index documents, and evidence are small and already in memory, so
// they are hashed where they sit and written in one call rather than paying for
// a staging round trip. The digest still comes from the bytes being written,
// which is what makes the direct write safe.
func (s *Store) PutBytes(ctx context.Context, payload []byte) (storage.BlobInfo, error) {
	d := model.NewDigester(model.AlgorithmSHA256)
	if _, err := d.Write(payload); err != nil {
		return storage.BlobInfo{}, err
	}
	digest := d.Digest()

	info := storage.BlobInfo{Digest: digest, Size: int64(len(payload))}
	if _, err := s.blobs.Stat(ctx, digest); err == nil {
		return info, nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return storage.BlobInfo{}, err
	}

	if err := s.blobs.Put(ctx, digest, bytes.NewReader(payload), int64(len(payload))); err != nil {
		return storage.BlobInfo{}, err
	}
	return info, nil
}

// Get returns the whole blob. Callers must have checked placement first.
func (s *Store) Get(ctx context.Context, digest model.Digest) (io.ReadCloser, error) {
	if !digest.Valid() {
		return nil, fmt.Errorf("reading blob: %w", storage.ErrInvalidDigest)
	}
	return s.blobs.Get(ctx, digest)
}

// GetRange returns part of a blob. Callers must have checked placement first.
func (s *Store) GetRange(ctx context.Context, digest model.Digest, off, length int64) (io.ReadCloser, error) {
	if !digest.Valid() {
		return nil, fmt.Errorf("reading blob: %w", storage.ErrInvalidDigest)
	}
	return s.blobs.GetRange(ctx, digest, off, length)
}

// Stat reports what is known about a blob.
func (s *Store) Stat(ctx context.Context, digest model.Digest) (storage.BlobInfo, error) {
	if !digest.Valid() {
		return storage.BlobInfo{}, fmt.Errorf("stating blob: %w", storage.ErrInvalidDigest)
	}
	return s.blobs.Stat(ctx, digest)
}

// Delete removes a blob from storage. Reclaiming a blob is the garbage
// collector's decision, never an adapter's: deleting one repository's
// placement must not delete bytes another repository still places.
func (s *Store) Delete(ctx context.Context, digest model.Digest) error {
	if !digest.Valid() {
		return fmt.Errorf("deleting blob: %w", storage.ErrInvalidDigest)
	}
	return s.blobs.Delete(ctx, digest)
}
