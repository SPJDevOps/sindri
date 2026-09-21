// Package s3store implements storage.BlobStore on any S3 compatible object
// store: AWS S3, MinIO, Ceph RGW.
//
// Blobs use the same key layout as the filesystem backend, so an operator can
// read one store's keys after working with the other. Staged uploads accumulate
// as multipart parts under a staging key and reach their content addressed key
// through a server side copy, so committing a blob never re-uploads its bytes.
package s3store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/storage"
)

// MinPartSize is the smallest part S3 accepts for anything but the final part
// of a multipart upload.
const MinPartSize = 5 << 20

// DefaultPartSize is the buffer a staged upload accumulates before flushing a
// part. Parts are held in memory, so this trades memory per concurrent upload
// against the number of round trips.
const DefaultPartSize = 8 << 20

// Config describes how to reach a bucket. Credentials are read from the
// ambient AWS chain (environment, profile, instance role) unless AccessKeyID
// and SecretAccessKey are set explicitly.
type Config struct {
	Bucket          string
	Prefix          string
	Region          string
	Endpoint        string
	UsePathStyle    bool
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	PartSize        int
}

// Store is an S3 backed BlobStore.
type Store struct {
	client   *s3.Client
	bucket   string
	prefix   string
	partSize int

	mu      sync.Mutex
	uploads map[storage.UploadID]*upload
}

var (
	_ storage.BlobStore = (*Store)(nil)
	_ storage.Prober    = (*Store)(nil)
)

// New builds a Store from cfg, resolving AWS configuration as it goes.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("s3store: bucket must not be empty")
	}

	opts := []func(*awsconfig.LoadOptions) error{
		// MinIO and other S3 compatible stores reject the aws-chunked trailer
		// the SDK adds when it calculates a checksum unprompted, so ask for one
		// only where the API requires it.
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
	}
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	if cfg.AccessKeyID != "" || cfg.SecretAccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS configuration: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
	})

	partSize := cfg.PartSize
	if partSize <= 0 {
		partSize = DefaultPartSize
	}
	if partSize < MinPartSize {
		return nil, fmt.Errorf("s3store: part size %d is below the %d byte minimum S3 accepts", partSize, MinPartSize)
	}

	return &Store{
		client:   client,
		bucket:   cfg.Bucket,
		prefix:   strings.Trim(cfg.Prefix, "/"),
		partSize: partSize,
		uploads:  make(map[storage.UploadID]*upload),
	}, nil
}

func (s *Store) key(parts ...string) string {
	if s.prefix != "" {
		parts = append([]string{s.prefix}, parts...)
	}
	return strings.Join(parts, "/")
}

func (s *Store) blobKey(digest model.Digest) (string, error) {
	if !digest.Valid() {
		return "", fmt.Errorf("s3store: %w", storage.ErrInvalidDigest)
	}
	h := digest.Hex()
	return s.key("blobs", string(digest.Algorithm()), h[0:2], h[2:4], h), nil
}

func (s *Store) stagingKey(id storage.UploadID) string {
	return s.key("staging", string(id))
}

// Put writes a blob whose digest the caller computed from these bytes.
func (s *Store) Put(ctx context.Context, digest model.Digest, r io.Reader, size int64) error {
	key, err := s.blobKey(digest)
	if err != nil {
		return err
	}

	if _, err := s.Stat(ctx, digest); err == nil {
		return nil // already stored; content addressing makes the write redundant
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}

	// The body must be replayable for a signed retry, and callers only reach
	// Put with a payload they have already hashed in memory.
	body, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("reading blob %s: %w", digest, err)
	}
	if size >= 0 && int64(len(body)) != size {
		return fmt.Errorf("writing blob %s: got %d bytes, want %d", digest, len(body), size)
	}

	if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		return fmt.Errorf("putting blob %s: %w", digest, err)
	}
	return nil
}

// Get returns the whole blob.
func (s *Store) Get(ctx context.Context, digest model.Digest) (io.ReadCloser, error) {
	return s.GetRange(ctx, digest, 0, -1)
}

// GetRange returns length bytes of the blob starting at off.
func (s *Store) GetRange(ctx context.Context, digest model.Digest, off, length int64) (io.ReadCloser, error) {
	key, err := s.blobKey(digest)
	if err != nil {
		return nil, err
	}
	if off < 0 {
		return nil, fmt.Errorf("reading blob %s: offset %d is negative: %w", digest, off, storage.ErrInvalidRange)
	}

	// A zero length read still has to report whether the range was valid, and
	// S3 has no way to express it, so answer from the object's metadata.
	if length == 0 {
		info, err := s.Stat(ctx, digest)
		if err != nil {
			return nil, err
		}
		if !storage.ValidOffset(off, info.Size) {
			return nil, fmt.Errorf("reading blob %s: offset %d outside a blob of %d bytes: %w",
				digest, off, info.Size, storage.ErrInvalidRange)
		}
		return io.NopCloser(bytes.NewReader(nil)), nil
	}

	in := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	// An empty object rejects "bytes=0-", so a read of the whole blob from the
	// start asks for no range at all.
	if off > 0 || length > 0 {
		in.Range = aws.String(rangeHeader(off, length))
	}

	out, err := s.client.GetObject(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("reading blob %s: %w", digest, mapError(err))
	}
	return out.Body, nil
}

// Stat reports the size of a blob.
func (s *Store) Stat(ctx context.Context, digest model.Digest) (storage.BlobInfo, error) {
	key, err := s.blobKey(digest)
	if err != nil {
		return storage.BlobInfo{}, err
	}

	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return storage.BlobInfo{}, fmt.Errorf("stating blob %s: %w", digest, mapError(err))
	}
	return storage.BlobInfo{Digest: digest, Size: aws.ToInt64(out.ContentLength)}, nil
}

// Delete removes a blob. S3 treats deleting an absent key as success, which is
// the behaviour the interface asks for.
func (s *Store) Delete(ctx context.Context, digest model.Digest) error {
	key, err := s.blobKey(digest)
	if err != nil {
		return err
	}

	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("deleting blob %s: %w", digest, mapError(err))
	}
	return nil
}

// Probe checks the bucket is reachable and visible to our credentials.
func (s *Store) Probe(ctx context.Context) error {
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)}); err != nil {
		return fmt.Errorf("probing bucket %s: %w", s.bucket, err)
	}
	return nil
}

func rangeHeader(off, length int64) string {
	if length < 0 {
		return fmt.Sprintf("bytes=%d-", off)
	}
	return fmt.Sprintf("bytes=%d-%d", off, off+length-1)
}

// mapError turns a transport level failure into this package's sentinels.
// Matching on the status code covers both NoSuchKey from GetObject and the
// bodiless 404 HeadObject returns.
func mapError(err error) error {
	var statusErr interface{ HTTPStatusCode() int }
	if errors.As(err, &statusErr) {
		switch statusErr.HTTPStatusCode() {
		case 404:
			return fmt.Errorf("%w: %w", storage.ErrNotFound, err)
		case 416:
			return fmt.Errorf("%w: %w", storage.ErrInvalidRange, err)
		}
	}

	var noSuchKey *types.NoSuchKey
	var notFound *types.NotFound
	if errors.As(err, &noSuchKey) || errors.As(err, &notFound) {
		return fmt.Errorf("%w: %w", storage.ErrNotFound, err)
	}
	return err
}

func newUploadID() (storage.UploadID, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating upload id: %w", err)
	}
	return storage.UploadID(hex.EncodeToString(b)), nil
}
