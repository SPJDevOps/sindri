package cas

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/storage"
)

// Upload is a blob being written. Bytes go to a staging key and are hashed on
// the way through, so that Commit can compare a digest computed here against
// the digest a client claimed.
//
// There is deliberately no io.Writer on this type. Appending is I/O against a
// remote object store, so it takes a context, and an OCI chunked push appends
// across several requests whose contexts differ.
type Upload struct {
	store    *Store
	id       storage.UploadID
	digester *model.Digester

	// mu serialises the operations on one upload, and is what ExpireIdle
	// tries, so that it never expires an upload in the middle of a write.
	mu   sync.Mutex
	done atomic.Bool
	// lastActive is the UnixNano of the last write, readable without mu.
	lastActive atomic.Int64
}

// NewUpload opens a staging area for a blob whose digest is not known yet and
// registers it so a later request can ResumeUpload by id.
func (s *Store) NewUpload(ctx context.Context) (*Upload, error) {
	id, err := s.blobs.CreateUpload(ctx)
	if err != nil {
		return nil, err
	}
	u := &Upload{
		store:    s,
		id:       id,
		digester: model.NewDigester(model.AlgorithmSHA256),
	}
	u.touch()
	s.uploads.Store(id, u)
	return u, nil
}

// ResumeUpload returns an in-flight upload previously opened by NewUpload.
// Sessions live in process memory (same v1 limit as ADR 8 for S3 multipart
// state): a different instance cannot resume another process's upload.
func (s *Store) ResumeUpload(id storage.UploadID) (*Upload, error) {
	if id == "" {
		return nil, storage.ErrUploadNotFound
	}
	v, ok := s.uploads.Load(id)
	if !ok {
		return nil, storage.ErrUploadNotFound
	}
	u, ok := v.(*Upload)
	if !ok || u.done.Load() {
		s.uploads.Delete(id)
		return nil, storage.ErrUploadNotFound
	}
	return u, nil
}

// ID is the staging identifier, which a protocol adapter hands to a client so
// that a later request can continue the same upload.
func (u *Upload) ID() storage.UploadID { return u.id }

// Size is how many bytes have been staged so far, and so the offset the next
// Append starts at.
func (u *Upload) Size() int64 { return u.digester.Size() }

// Digest is the digest of the bytes staged so far.
func (u *Upload) Digest() model.Digest { return u.digester.Digest() }

// Append stages everything in r and folds it into the running hash. It returns
// the total number of bytes staged.
func (u *Upload) Append(ctx context.Context, r io.Reader) (int64, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.touch()
	defer u.touch()
	if u.done.Load() {
		return 0, fmt.Errorf("appending to upload %s: %w", u.id, storage.ErrUploadNotFound)
	}

	// Hash what the store accepted, not what the caller offered: the tee only
	// sees bytes that made it into r's read, and Append reports what landed.
	digested := u.digester.Tee(r)

	staged, err := u.store.blobs.Append(ctx, u.id, digested)
	if err != nil {
		return 0, err
	}
	if staged != u.digester.Size() {
		return 0, fmt.Errorf("appending to upload %s: staged %d bytes but hashed %d", u.id, staged, u.digester.Size())
	}
	return staged, nil
}

// Commit verifies the staged bytes and promotes them to their content
// addressed key.
//
// This is the security boundary the milestone exists for. A mismatch discards
// the staged bytes: a partially uploaded blob committed under its claimed
// digest is corruption every later integrity check would trust. A negative size
// means the caller claimed none.
func (u *Upload) Commit(ctx context.Context, expected model.Digest, size int64) (storage.BlobInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.done.Load() {
		return storage.BlobInfo{}, fmt.Errorf("committing upload %s: %w", u.id, storage.ErrUploadNotFound)
	}

	actual := u.digester.Digest()
	actualSize := u.digester.Size()

	if size >= 0 && actualSize != size {
		u.abortQuietly(ctx)
		return storage.BlobInfo{}, fmt.Errorf("committing upload %s: %w: received %d bytes, client claimed %d",
			u.id, ErrSizeMismatch, actualSize, size)
	}
	if expected.Valid() && expected != actual {
		u.abortQuietly(ctx)
		return storage.BlobInfo{}, fmt.Errorf("committing upload %s: %w: received %s, client claimed %s",
			u.id, ErrDigestMismatch, actual, expected)
	}

	// The digest is verified, so the intent may now create the row. It must
	// land before the bytes reach their content addressed key (ADR 19).
	if err := u.store.reserve(ctx, actual, actualSize); err != nil {
		u.abortQuietly(ctx)
		return storage.BlobInfo{}, err
	}
	if err := u.store.blobs.CommitUpload(ctx, u.id, actual); err != nil {
		u.abortQuietly(ctx)
		return storage.BlobInfo{}, err
	}
	u.done.Store(true)
	u.store.uploads.Delete(u.id)

	return storage.BlobInfo{Digest: actual, Size: actualSize}, nil
}

// Abort discards the staged bytes. Aborting twice is not an error.
func (u *Upload) Abort(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.done.Load() {
		return nil
	}
	u.done.Store(true)
	u.store.uploads.Delete(u.id)
	return u.store.blobs.AbortUpload(ctx, u.id)
}

// abortQuietly discards staged bytes on a path that is already returning an
// error, where the abort's own failure must not replace it. The context is
// detached because the request's context is often already cancelled by the
// time an upload fails, and leaked staging objects are what fills a disk.
func (u *Upload) abortQuietly(ctx context.Context) {
	u.done.Store(true)
	u.store.uploads.Delete(u.id)
	_ = u.store.blobs.AbortUpload(context.WithoutCancel(ctx), u.id)
}

func (u *Upload) touch() { u.lastActive.Store(time.Now().UnixNano()) }

// ExpireIdle aborts every upload session this process holds that was last
// written to before olderThan, and reports how many it aborted. An upload in
// the middle of a write is active by definition and is skipped.
//
// Sessions live in this process's memory, so this is the one cleanup that does
// not go through the queue table: a job claimed by another process could not
// reach them (ADR 20). Staging state in storage is purged separately, by
// BlobStore.PurgeUploads, which also reaches what a crashed process left.
func (s *Store) ExpireIdle(ctx context.Context, olderThan time.Time) int {
	cutoff := olderThan.UnixNano()
	n := 0
	s.uploads.Range(func(_, v any) bool {
		u, ok := v.(*Upload)
		if !ok || u.lastActive.Load() >= cutoff || !u.mu.TryLock() {
			return true
		}
		defer u.mu.Unlock()
		if u.done.Load() {
			return true
		}
		u.abortQuietly(ctx)
		n++
		return true
	})
	return n
}
