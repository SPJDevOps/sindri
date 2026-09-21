// Package memstore is an in-memory BlobStore for tests.
//
// It is not a deployable backend and is not reachable from configuration: it
// exists so that packages above storage can be tested without a filesystem or a
// container. It passes the same conformance suite as the real backends, which
// is what keeps a test against it honest.
package memstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sync"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/storage"
)

// Store keeps blobs in memory.
type Store struct {
	mu      sync.RWMutex
	blobs   map[model.Digest][]byte
	uploads map[storage.UploadID][]byte
}

var (
	_ storage.BlobStore = (*Store)(nil)
	_ storage.Prober    = (*Store)(nil)
)

// New returns an empty Store.
func New() *Store {
	return &Store{
		blobs:   make(map[model.Digest][]byte),
		uploads: make(map[storage.UploadID][]byte),
	}
}

// Len reports how many distinct blobs are stored, which is how a test asserts
// that deduplication actually deduplicated.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.blobs)
}

// Uploads reports how many staged uploads are outstanding, which is how a test
// asserts that a rejected upload left nothing behind.
func (s *Store) Uploads() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.uploads)
}

// Put stores a blob under a digest the caller computed from these bytes.
func (s *Store) Put(_ context.Context, digest model.Digest, r io.Reader, size int64) error {
	if !digest.Valid() {
		return fmt.Errorf("memstore: %w", storage.ErrInvalidDigest)
	}

	body, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("reading blob %s: %w", digest, err)
	}
	if size >= 0 && int64(len(body)) != size {
		return fmt.Errorf("writing blob %s: got %d bytes, want %d", digest, len(body), size)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.blobs[digest]; !ok {
		s.blobs[digest] = body
	}
	return nil
}

// Get returns the whole blob.
func (s *Store) Get(ctx context.Context, digest model.Digest) (io.ReadCloser, error) {
	return s.GetRange(ctx, digest, 0, -1)
}

// GetRange returns length bytes of the blob starting at off.
func (s *Store) GetRange(_ context.Context, digest model.Digest, off, length int64) (io.ReadCloser, error) {
	body, err := s.lookup(digest)
	if err != nil {
		return nil, err
	}

	size := int64(len(body))
	if !storage.ValidOffset(off, size) {
		return nil, fmt.Errorf("reading blob %s: offset %d outside a blob of %d bytes: %w",
			digest, off, size, storage.ErrInvalidRange)
	}
	end := size
	if length >= 0 && off+length < size {
		end = off + length
	}

	return io.NopCloser(bytes.NewReader(body[off:end])), nil
}

// Stat reports the size of a blob.
func (s *Store) Stat(_ context.Context, digest model.Digest) (storage.BlobInfo, error) {
	body, err := s.lookup(digest)
	if err != nil {
		return storage.BlobInfo{}, err
	}
	return storage.BlobInfo{Digest: digest, Size: int64(len(body))}, nil
}

// Delete removes a blob, tolerating one that is already gone.
func (s *Store) Delete(_ context.Context, digest model.Digest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.blobs, digest)
	return nil
}

// CreateUpload opens an empty staging buffer.
func (s *Store) CreateUpload(_ context.Context) (storage.UploadID, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating upload id: %w", err)
	}
	id := storage.UploadID(hex.EncodeToString(b))

	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads[id] = nil

	return id, nil
}

// Append adds to the staged upload and returns its new size.
func (s *Store) Append(_ context.Context, id storage.UploadID, r io.Reader) (int64, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return 0, fmt.Errorf("appending to upload %s: %w", id, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	staged, ok := s.uploads[id]
	if !ok {
		return 0, fmt.Errorf("appending to upload %s: %w", id, storage.ErrUploadNotFound)
	}
	staged = append(staged, body...)
	s.uploads[id] = staged

	return int64(len(staged)), nil
}

// CommitUpload moves the staged bytes under digest.
func (s *Store) CommitUpload(_ context.Context, id storage.UploadID, digest model.Digest) error {
	if !digest.Valid() {
		return fmt.Errorf("memstore: %w", storage.ErrInvalidDigest)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	staged, ok := s.uploads[id]
	if !ok {
		return fmt.Errorf("committing upload %s: %w", id, storage.ErrUploadNotFound)
	}
	delete(s.uploads, id)
	if _, exists := s.blobs[digest]; !exists {
		s.blobs[digest] = staged
	}
	return nil
}

// AbortUpload discards a staged upload, tolerating one that is already gone.
func (s *Store) AbortUpload(_ context.Context, id storage.UploadID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.uploads, id)
	return nil
}

// Probe always succeeds: memory is always reachable.
func (s *Store) Probe(_ context.Context) error { return nil }

func (s *Store) lookup(digest model.Digest) ([]byte, error) {
	if !digest.Valid() {
		return nil, fmt.Errorf("memstore: %w", storage.ErrInvalidDigest)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	body, ok := s.blobs[digest]
	if !ok {
		return nil, fmt.Errorf("reading blob %s: %w", digest, storage.ErrNotFound)
	}
	return body, nil
}
