package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/storage"
)

// upload is one staged upload in progress.
//
// Appended bytes accumulate in buf and are flushed as multipart parts once they
// pass the part size, because S3 rejects any part but the last below 5 MiB and
// a protocol adapter is free to append in whatever sizes its client chose. An
// upload that never reaches one part is written as a single object at commit,
// which saves a multipart round trip for the small payloads that dominate.
type upload struct {
	mu          sync.Mutex
	key         string
	multipartID string
	parts       []types.CompletedPart
	buf         []byte
	size        int64
	// lastActive is when the upload was created or last appended to, for
	// PurgeUploads. Guarded by the store's mu, not the upload's, so that a
	// purge can read it without waiting behind a slow Append.
	lastActive time.Time
}

// CreateUpload registers a staging area. No object exists until the first part
// is flushed, so an abandoned upload that never grew costs nothing.
func (s *Store) CreateUpload(_ context.Context) (storage.UploadID, error) {
	id, err := newUploadID()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads[id] = &upload{key: s.stagingKey(id), lastActive: time.Now()}

	return id, nil
}

// Append writes r at the end of the staged upload and returns the new size.
func (s *Store) Append(ctx context.Context, id storage.UploadID, r io.Reader) (int64, error) {
	u, err := s.lookup(id)
	if err != nil {
		return 0, fmt.Errorf("appending to upload %s: %w", id, err)
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	s.touch(u)
	defer s.touch(u)

	chunk := make([]byte, 32<<10)
	for {
		n, readErr := r.Read(chunk)
		if n > 0 {
			u.buf = append(u.buf, chunk[:n]...)
			u.size += int64(n)
			for len(u.buf) >= s.partSize {
				if err := s.flushPart(ctx, u, s.partSize); err != nil {
					return 0, err
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return 0, fmt.Errorf("appending to upload %s: %w", id, readErr)
		}
	}

	return u.size, nil
}

// flushPart uploads the first n buffered bytes as the next multipart part.
// The caller holds u.mu.
func (s *Store) flushPart(ctx context.Context, u *upload, n int) error {
	if u.multipartID == "" {
		out, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(u.key),
		})
		if err != nil {
			return fmt.Errorf("starting multipart upload for %s: %w", u.key, err)
		}
		u.multipartID = aws.ToString(out.UploadId)
	}

	number := int32(len(u.parts) + 1) //nolint:gosec // part numbers are capped at 10000 by S3 and by the part size
	out, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(u.key),
		UploadId:      aws.String(u.multipartID),
		PartNumber:    aws.Int32(number),
		Body:          bytes.NewReader(u.buf[:n]),
		ContentLength: aws.Int64(int64(n)),
	})
	if err != nil {
		return fmt.Errorf("uploading part %d of %s: %w", number, u.key, err)
	}

	u.parts = append(u.parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(number)})
	u.buf = append(u.buf[:0], u.buf[n:]...)

	return nil
}

// CommitUpload assembles the staged parts and moves them to the content
// addressed key. The caller has already compared digest against the bytes it
// appended; this is the point where they become trusted content.
func (s *Store) CommitUpload(ctx context.Context, id storage.UploadID, digest model.Digest) error {
	final, err := s.blobKey(digest)
	if err != nil {
		return err
	}
	u, err := s.lookup(id)
	if err != nil {
		return fmt.Errorf("committing upload %s: %w", id, err)
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	// Small upload: no part was ever flushed, so write the buffer straight to
	// its content addressed key and skip the staging object entirely.
	if u.multipartID == "" {
		if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(final),
			Body:          bytes.NewReader(u.buf),
			ContentLength: aws.Int64(u.size),
		}); err != nil {
			return fmt.Errorf("committing upload %s as blob %s: %w", id, digest, err)
		}
		s.forget(id)
		return nil
	}

	if len(u.buf) > 0 {
		if err := s.flushPart(ctx, u, len(u.buf)); err != nil {
			return err
		}
	}

	if _, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(u.key),
		UploadId:        aws.String(u.multipartID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: u.parts},
	}); err != nil {
		return fmt.Errorf("completing upload %s: %w", id, err)
	}

	// A server side copy, so committing a multi gigabyte layer moves no bytes
	// through this process.
	if _, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(final),
		CopySource: aws.String(s.bucket + "/" + u.key),
	}); err != nil {
		return fmt.Errorf("committing upload %s as blob %s: %w", id, digest, err)
	}

	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(u.key),
	}); err != nil {
		return fmt.Errorf("removing staged upload %s: %w", id, err)
	}

	s.forget(id)
	return nil
}

// AbortUpload discards a staged upload, tolerating one that is already gone.
func (s *Store) AbortUpload(ctx context.Context, id storage.UploadID) error {
	u, err := s.lookup(id)
	if err != nil {
		return nil //nolint:nilerr // aborting an unknown upload is a successful no-op
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	s.forget(id)

	if u.multipartID == "" {
		return nil
	}
	if _, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(u.key),
		UploadId: aws.String(u.multipartID),
	}); err != nil {
		return fmt.Errorf("aborting upload %s: %w", id, err)
	}
	return nil
}

func (s *Store) lookup(id storage.UploadID) (*upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	u, ok := s.uploads[id]
	if !ok {
		return nil, storage.ErrUploadNotFound
	}
	return u, nil
}

// touch records activity on u.
func (s *Store) touch(u *upload) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u.lastActive = time.Now()
}

func (s *Store) forget(id storage.UploadID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.uploads, id)
}

// PurgeUploads discards staged uploads idle since before olderThan.
//
// Two kinds of state are purged. Sessions this process holds are dropped from
// memory, which also frees the up to one part of bytes each buffers, and their
// multipart uploads are aborted. Then the bucket is listed for staging state
// no live session owns, which is what a crashed process leaves behind: open
// multipart uploads under the staging prefix, and staging objects a commit
// completed but never copied to their content addressed key.
//
// S3 records when a multipart upload was initiated, not when it was last
// written to, so remote state belonging to a session this process still holds
// is never judged by that date.
func (s *Store) PurgeUploads(ctx context.Context, olderThan time.Time) (int, error) {
	n := 0
	for _, id := range s.idleSessions(olderThan) {
		u, err := s.lookup(id)
		if err != nil {
			continue // committed or aborted since
		}
		// An upload whose lock is held is mid-Append or mid-Commit, which is
		// activity: leave it.
		if !u.mu.TryLock() {
			continue
		}
		s.forget(id)
		multipartID := u.multipartID
		u.mu.Unlock()
		n++
		if multipartID == "" {
			continue
		}
		if err := s.abortMultipart(ctx, u.key, multipartID); err != nil {
			return n, err
		}
	}

	live := s.liveKeys()
	orphans, err := s.purgeOrphanMultiparts(ctx, olderThan, live)
	n += orphans
	if err != nil {
		return n, err
	}
	objects, err := s.purgeOrphanObjects(ctx, olderThan, live)
	n += objects
	return n, err
}

func (s *Store) idleSessions(olderThan time.Time) []storage.UploadID {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []storage.UploadID
	for id, u := range s.uploads {
		if u.lastActive.Before(olderThan) {
			ids = append(ids, id)
		}
	}
	return ids
}

// liveKeys is the staging key of every session this process still holds.
func (s *Store) liveKeys() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make(map[string]bool, len(s.uploads))
	for _, u := range s.uploads {
		keys[u.key] = true
	}
	return keys
}

func (s *Store) stagingPrefix() string { return s.key("staging") + "/" }

func (s *Store) purgeOrphanMultiparts(ctx context.Context, olderThan time.Time, live map[string]bool) (int, error) {
	n := 0
	prefix := s.stagingPrefix()
	// No Prefix on the request: MinIO answers ListMultipartUploads with a
	// prefix only when the prefix is a whole object key, and returns nothing
	// for a directory-like one, which would leave every orphan in place. The
	// listing is filtered here instead, which AWS and MinIO agree on.
	in := &s3.ListMultipartUploadsInput{Bucket: aws.String(s.bucket)}
	for {
		out, err := s.client.ListMultipartUploads(ctx, in)
		if err != nil {
			return n, fmt.Errorf("listing staged multipart uploads: %w", err)
		}
		for _, mu := range out.Uploads {
			key := aws.ToString(mu.Key)
			if !strings.HasPrefix(key, prefix) || live[key] || mu.Initiated == nil || !mu.Initiated.Before(olderThan) {
				continue
			}
			if err := s.abortMultipart(ctx, key, aws.ToString(mu.UploadId)); err != nil {
				return n, err
			}
			n++
		}
		if !aws.ToBool(out.IsTruncated) {
			return n, nil
		}
		in.KeyMarker = out.NextKeyMarker
		in.UploadIdMarker = out.NextUploadIdMarker
	}
}

func (s *Store) purgeOrphanObjects(ctx context.Context, olderThan time.Time, live map[string]bool) (int, error) {
	n := 0
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(s.stagingPrefix()),
	})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return n, fmt.Errorf("listing staged objects: %w", err)
		}
		for _, obj := range out.Contents {
			key := aws.ToString(obj.Key)
			if live[key] || obj.LastModified == nil || !obj.LastModified.Before(olderThan) {
				continue
			}
			if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(s.bucket),
				Key:    aws.String(key),
			}); err != nil {
				return n, fmt.Errorf("removing staged object %s: %w", key, err)
			}
			n++
		}
	}
	return n, nil
}

func (s *Store) abortMultipart(ctx context.Context, key, uploadID string) error {
	if _, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	}); err != nil {
		var noSuch *types.NoSuchUpload
		if errors.As(err, &noSuch) {
			return nil
		}
		return fmt.Errorf("aborting staged multipart upload %s: %w", key, err)
	}
	return nil
}
