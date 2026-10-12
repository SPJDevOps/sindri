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
	"sync"

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

// Reserver records the write intent for a blob before its bytes are written
// or relied on (ADR 19). It is how the garbage collector, which deletes bytes
// only under a lock on the blob's metadata row and only when that row has not
// been seen within its grace period, can never delete bytes a write in flight
// is about to depend on.
//
// It is an interface so that this package stays below the database in the
// dependency order: the implementation lives in package db.
type Reserver interface {
	// Reserve creates or refreshes the row for a digest computed from the
	// bytes themselves. It must commit before it returns.
	Reserve(ctx context.Context, digest model.Digest, size int64) error

	// Touch refreshes an existing row for a digest that is only claimed so
	// far, and reports whether there was one. It never creates a row.
	Touch(ctx context.Context, digest model.Digest) (bool, error)
}

// Store computes digests and commits verified blobs to a BlobStore.
type Store struct {
	blobs    storage.BlobStore
	reserver Reserver
	uploads  sync.Map // storage.UploadID -> *Upload
}

// Option configures a Store.
type Option func(*Store)

// WithReserver records write intent through r before bytes are written or
// relied on. Every Store that shares a database with a garbage collector must
// have one; the collector refuses to run against a Store without it.
func WithReserver(r Reserver) Option {
	return func(s *Store) { s.reserver = r }
}

// New returns a Store writing through blobs.
func New(blobs storage.BlobStore, opts ...Option) *Store {
	if blobs == nil {
		panic("cas.New: nil BlobStore")
	}
	s := &Store{blobs: blobs}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// HasReserver reports whether writes record their intent, which is what makes
// running a garbage collector beside this Store safe.
func (s *Store) HasReserver() bool { return s.reserver != nil }

func (s *Store) reserve(ctx context.Context, digest model.Digest, size int64) error {
	if s.reserver == nil {
		return nil
	}
	if err := s.reserver.Reserve(ctx, digest, size); err != nil {
		return fmt.Errorf("reserving blob %s: %w", digest, err)
	}
	return nil
}

func (s *Store) touch(ctx context.Context, digest model.Digest) error {
	if s.reserver == nil {
		return nil
	}
	if _, err := s.reserver.Touch(ctx, digest); err != nil {
		return fmt.Errorf("touching blob %s: %w", digest, err)
	}
	return nil
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
		// Touch before the Stat: a deduplicated write relies on bytes it did
		// not write, and the touch is what keeps the collector off them
		// until the placement commits. It only refreshes an existing row;
		// the claim is still unverified, so it must not create one.
		if err := s.touch(ctx, expected); err != nil {
			return storage.BlobInfo{}, err
		}
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
	if err := s.reserve(ctx, digest, info.Size); err != nil {
		return storage.BlobInfo{}, err
	}
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
