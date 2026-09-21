//go:build integration

package cas_test

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/storage"
	"github.com/spj/sindri/internal/storage/fsstore"
	"github.com/spj/sindri/internal/storage/s3store"
	"github.com/spj/sindri/internal/testsupport"
)

// backend pairs a real store with a way to count what is physically there, so
// that "nothing was left behind" is an assertion about the disk or the bucket
// rather than about our own bookkeeping.
type backend struct {
	name  string
	store storage.BlobStore
	count func(t *testing.T) int
}

// TestCAS_AcrossBackends is the milestone's end-to-end claim: a verified blob
// round-trips on both backends, and a blob whose bytes do not match its claimed
// digest is refused and leaves nothing behind. The second half is the one that
// matters: a partially uploaded blob committed under its claimed digest is
// corruption every later integrity check would trust.
func TestCAS_AcrossBackends(t *testing.T) {
	minio := testsupport.StartMinIO(t)

	for _, b := range backends(t, minio) {
		t.Run(b.name, func(t *testing.T) {
			store := cas.New(b.store)
			payload := bytes.Repeat([]byte("sindri blob "), 4096)
			digest := digestOf(payload)

			info, err := store.Put(t.Context(), bytes.NewReader(payload), digest, int64(len(payload)))
			if err != nil {
				t.Fatalf("Put: %v", err)
			}
			if info.Digest != digest {
				t.Fatalf("Put digest = %v, want %v", info.Digest, digest)
			}

			rc, err := store.Get(t.Context(), digest)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			got, err := io.ReadAll(rc)
			if cerr := rc.Close(); cerr != nil {
				t.Fatalf("closing the blob: %v", cerr)
			}
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("round trip is not byte identical: %d bytes back, %d out", len(got), len(payload))
			}

			// Pushing the same bytes again must not write a second object.
			if _, err := store.Put(t.Context(), bytes.NewReader(payload), digest, int64(len(payload))); err != nil {
				t.Fatalf("second Put: %v", err)
			}
			if n := b.count(t); n != 1 {
				t.Fatalf("storage holds %d objects after two identical pushes, want 1", n)
			}

			// A client claiming someone else's digest is refused, and the
			// claimed key stays empty.
			lie := digestOf([]byte("a different artifact entirely"))
			_, err = store.Put(t.Context(), bytes.NewReader([]byte("hostile payload")), lie, -1)
			if !errors.Is(err, cas.ErrDigestMismatch) {
				t.Fatalf("Put with a mismatched digest = %v, want ErrDigestMismatch", err)
			}
			if _, err := store.Stat(t.Context(), lie); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("the claimed digest is readable after a mismatch: %v", err)
			}
			if n := b.count(t); n != 1 {
				t.Fatalf("storage holds %d objects after a rejected push, want 1", n)
			}

			if err := store.Delete(t.Context(), digest); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if n := b.count(t); n != 0 {
				t.Fatalf("storage holds %d objects after Delete, want 0", n)
			}
		})
	}
}

func backends(t *testing.T, minio testsupport.MinIO) []backend {
	t.Helper()

	root := t.TempDir()
	fsBackend, err := fsstore.New(root)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}

	const prefix = "cas-integration"
	s3Backend, err := s3store.New(t.Context(), minio.Config(prefix))
	if err != nil {
		t.Fatalf("s3store.New: %v", err)
	}

	return []backend{
		{
			name:  "Filesystem",
			store: fsBackend,
			count: func(t *testing.T) int { return countFiles(t, filepath.Join(root, "blobs")) },
		},
		{
			name:  "S3",
			store: s3Backend,
			count: func(t *testing.T) int { return countObjects(t, minio, prefix+"/blobs/") },
		},
	}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()

	count := 0
	err := filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			count++
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return count
}

func countObjects(t *testing.T, minio testsupport.MinIO, prefix string) int {
	t.Helper()

	cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(minio.Username, minio.Password, ""),
		),
	)
	if err != nil {
		t.Fatalf("loading AWS configuration: %v", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(minio.Endpoint)
		o.UsePathStyle = true
	})

	out, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
		Bucket: aws.String(minio.Bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		t.Fatalf("listing %s: %v", prefix, err)
	}

	count := 0
	for _, obj := range out.Contents {
		if !strings.HasSuffix(aws.ToString(obj.Key), "/") {
			count++
		}
	}
	return count
}
