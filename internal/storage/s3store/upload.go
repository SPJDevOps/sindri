package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

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
	s.uploads[id] = &upload{key: s.stagingKey(id)}

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

func (s *Store) forget(id storage.UploadID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.uploads, id)
}
