// Package fsstore implements storage.BlobStore on a local filesystem.
//
// Blobs live under a two-level fan-out keyed by the digest hex
// (blobs/sha256/ab/cd/abcd...), because a flat directory holding a million
// entries is slow to traverse on every filesystem worth supporting. Staged
// uploads live in a sibling directory and reach their content addressed key by
// rename, which is atomic within a filesystem: a reader never observes a
// partially written blob.
package fsstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/storage"
)

const (
	dirPerm  fs.FileMode = 0o750
	filePerm fs.FileMode = 0o600

	blobsDir   = "blobs"
	stagingDir = "staging"
	probeName  = ".probe"
)

// Store is a filesystem backed BlobStore.
type Store struct {
	root string
}

var (
	_ storage.BlobStore = (*Store)(nil)
	_ storage.Prober    = (*Store)(nil)
)

// New prepares root and returns a Store rooted there. The directory is created
// if it does not exist.
func New(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("fsstore: root directory must not be empty")
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving storage root %s: %w", root, err)
	}
	for _, dir := range []string{filepath.Join(abs, blobsDir), filepath.Join(abs, stagingDir)} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return nil, fmt.Errorf("creating storage directory %s: %w", dir, err)
		}
	}

	return &Store{root: abs}, nil
}

// Root returns the absolute directory the store writes under.
func (s *Store) Root() string { return s.root }

// blobPath maps a digest to its content addressed path. The digest is already
// validated, so its algorithm and hex cannot contain a path separator.
func (s *Store) blobPath(digest model.Digest) (string, error) {
	if !digest.Valid() {
		return "", fmt.Errorf("fsstore: %w", storage.ErrInvalidDigest)
	}
	h := digest.Hex()
	return filepath.Join(s.root, blobsDir, string(digest.Algorithm()), h[0:2], h[2:4], h), nil
}

func (s *Store) uploadPath(id storage.UploadID) (string, error) {
	if !validUploadID(id) {
		return "", fmt.Errorf("fsstore upload %q: %w", id, storage.ErrUploadNotFound)
	}
	return filepath.Join(s.root, stagingDir, string(id)), nil
}

// Put writes a blob whose digest the caller computed from these bytes.
func (s *Store) Put(ctx context.Context, digest model.Digest, r io.Reader, size int64) error {
	final, err := s.blobPath(digest)
	if err != nil {
		return err
	}

	// Content addressing makes a rewrite redundant, and skipping it keeps a
	// concurrent reader of the existing blob undisturbed.
	if _, err := os.Stat(final); err == nil {
		return nil
	}

	tmp, err := s.newStagingFile()
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	written, err := copyAndSync(ctx, tmp, r)
	if cerr := tmp.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("closing staged blob %s: %w", digest, cerr)
	}
	if err != nil {
		return err
	}
	if size >= 0 && written != size {
		return fmt.Errorf("writing blob %s: got %d bytes, want %d", digest, written, size)
	}

	return s.promote(tmp.Name(), final, digest)
}

// promote moves a fully written, verified file to its content addressed key.
func (s *Store) promote(from, to string, digest model.Digest) error {
	if err := os.MkdirAll(filepath.Dir(to), dirPerm); err != nil {
		return fmt.Errorf("creating directory for blob %s: %w", digest, err)
	}
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("committing blob %s: %w", digest, err)
	}
	return syncDir(filepath.Dir(to))
}

// Get returns the whole blob.
func (s *Store) Get(ctx context.Context, digest model.Digest) (io.ReadCloser, error) {
	return s.GetRange(ctx, digest, 0, -1)
}

// GetRange returns length bytes of the blob starting at off.
func (s *Store) GetRange(_ context.Context, digest model.Digest, off, length int64) (io.ReadCloser, error) {
	path, err := s.blobPath(digest)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path) //nolint:gosec // path is derived from a validated digest, never from user input
	if err != nil {
		return nil, wrapNotFound(err, digest)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stating blob %s: %w", digest, err)
	}

	start, count, err := clampRange(info.Size(), off, length)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("reading blob %s: %w", digest, err)
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("seeking blob %s to %d: %w", digest, start, err)
	}

	return sectionReader{Reader: io.LimitReader(f, count), closer: f}, nil
}

// Stat reports the size of a blob.
func (s *Store) Stat(_ context.Context, digest model.Digest) (storage.BlobInfo, error) {
	path, err := s.blobPath(digest)
	if err != nil {
		return storage.BlobInfo{}, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return storage.BlobInfo{}, wrapNotFound(err, digest)
	}
	return storage.BlobInfo{Digest: digest, Size: info.Size()}, nil
}

// Delete removes a blob, tolerating one that is already gone.
func (s *Store) Delete(_ context.Context, digest model.Digest) error {
	path, err := s.blobPath(digest)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("deleting blob %s: %w", digest, err)
	}
	return nil
}

// CreateUpload opens an empty staging file.
func (s *Store) CreateUpload(_ context.Context) (storage.UploadID, error) {
	id, err := newUploadID()
	if err != nil {
		return "", err
	}

	path, err := s.uploadPath(id)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm) //nolint:gosec // G304: the path is the staging root plus a generated hex id, validated by validUploadID
	if err != nil {
		return "", fmt.Errorf("creating upload %s: %w", id, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("creating upload %s: %w", id, err)
	}
	return id, nil
}

// Append writes r at the end of the staged upload and returns the new size.
func (s *Store) Append(ctx context.Context, id storage.UploadID, r io.Reader) (int64, error) {
	path, err := s.uploadPath(id)
	if err != nil {
		return 0, err
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, filePerm) //nolint:gosec // path is an opaque generated id under the staging root
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("appending to upload %s: %w", id, storage.ErrUploadNotFound)
		}
		return 0, fmt.Errorf("opening upload %s: %w", id, err)
	}
	defer func() { _ = f.Close() }()

	if _, err := copyAndSync(ctx, f, r); err != nil {
		return 0, err
	}

	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stating upload %s: %w", id, err)
	}
	return info.Size(), nil
}

// CommitUpload renames the staged file onto its content addressed key.
func (s *Store) CommitUpload(ctx context.Context, id storage.UploadID, digest model.Digest) error {
	path, err := s.uploadPath(id)
	if err != nil {
		return err
	}
	final, err := s.blobPath(digest)
	if err != nil {
		return err
	}

	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("committing upload %s: %w", id, storage.ErrUploadNotFound)
		}
		return fmt.Errorf("stating upload %s: %w", id, err)
	}

	if _, err := os.Stat(final); err == nil {
		// The blob already exists, so the staged copy is redundant.
		return s.AbortUpload(ctx, id)
	}
	return s.promote(path, final, digest)
}

// AbortUpload discards a staged upload, tolerating one that is already gone.
func (s *Store) AbortUpload(_ context.Context, id storage.UploadID) error {
	path, err := s.uploadPath(id)
	if err != nil {
		if errors.Is(err, storage.ErrUploadNotFound) {
			return nil
		}
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("aborting upload %s: %w", id, err)
	}
	return nil
}

// PurgeUploads removes staging files last written to before olderThan. Every
// Append rewrites the file, so its modification time is when the upload was
// last active. The staging directory is the whole of an upload's state, so
// this also covers uploads a crashed process left behind, and the put-* files
// of a Put that never finished.
func (s *Store) PurgeUploads(ctx context.Context, olderThan time.Time) (int, error) {
	dir := filepath.Join(s.root, stagingDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("listing staged uploads: %w", err)
	}
	n := 0
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		if e.IsDir() || e.Name() == probeName {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // committed or aborted while we listed
			}
			return n, fmt.Errorf("stating staged upload %s: %w", e.Name(), err)
		}
		if !info.ModTime().Before(olderThan) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return n, fmt.Errorf("removing staged upload %s: %w", e.Name(), err)
		}
		n++
	}
	return n, nil
}

// Probe checks the store is present and writable, which is what readiness
// actually depends on: a readable but full or read-only volume is not ready.
func (s *Store) Probe(_ context.Context) error {
	path := filepath.Join(s.root, stagingDir, probeName)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, filePerm) //nolint:gosec // G304: the path is the configured root plus a constant file name
	if err != nil {
		return fmt.Errorf("probing storage root %s: %w", s.root, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("probing storage root %s: %w", s.root, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("probing storage root %s: %w", s.root, err)
	}
	return nil
}

func (s *Store) newStagingFile() (*os.File, error) {
	f, err := os.CreateTemp(filepath.Join(s.root, stagingDir), "put-*")
	if err != nil {
		return nil, fmt.Errorf("creating staging file: %w", err)
	}
	if err := f.Chmod(filePerm); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("setting staging file mode: %w", err)
	}
	return f, nil
}

// copyAndSync copies r into f and flushes it to disk. A blob that is visible
// but not durable is a blob an integrity check will later trust wrongly.
func copyAndSync(ctx context.Context, f *os.File, r io.Reader) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("writing %s: %w", f.Name(), err)
	}

	n, err := io.Copy(f, r)
	if err != nil {
		return n, fmt.Errorf("writing %s: %w", f.Name(), err)
	}
	if err := f.Sync(); err != nil {
		return n, fmt.Errorf("syncing %s: %w", f.Name(), err)
	}
	return n, nil
}

// syncDir flushes a directory entry so a rename survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // dir is built from the configured root and a validated digest
	if err != nil {
		return fmt.Errorf("opening directory %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()

	if err := d.Sync(); err != nil {
		return fmt.Errorf("syncing directory %s: %w", dir, err)
	}
	return nil
}

func wrapNotFound(err error, digest model.Digest) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading blob %s: %w", digest, storage.ErrNotFound)
	}
	return fmt.Errorf("reading blob %s: %w", digest, err)
}

// clampRange turns a requested range into an offset and a byte count, or
// reports that it does not lie within the blob.
func clampRange(size, off, length int64) (int64, int64, error) {
	if !storage.ValidOffset(off, size) {
		return 0, 0, fmt.Errorf("offset %d outside a blob of %d bytes: %w", off, size, storage.ErrInvalidRange)
	}
	if length < 0 || off+length > size {
		return off, size - off, nil
	}
	return off, length, nil
}

func newUploadID() (storage.UploadID, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating upload id: %w", err)
	}
	return storage.UploadID(hex.EncodeToString(b)), nil
}

// validUploadID keeps a generated id from ever being a path traversal, even if
// a future caller passes one in from a request.
func validUploadID(id storage.UploadID) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// sectionReader pairs a bounded reader with the file it came from.
type sectionReader struct {
	io.Reader
	closer io.Closer
}

func (s sectionReader) Close() error {
	if err := s.closer.Close(); err != nil {
		return fmt.Errorf("closing blob: %w", err)
	}
	return nil
}
