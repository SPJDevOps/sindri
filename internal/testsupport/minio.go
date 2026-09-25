//go:build integration

// Package testsupport starts the real dependencies the integration suites run
// against. It is compiled only under the `integration` build tag, and exists so
// that "a real Postgres and a real MinIO" is one line in a test rather than a
// block of container wiring copied between packages.
package testsupport

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/spj/sindri/internal/storage/s3store"
)

// minioImage is pinned so a test failure is a change in Sindri, never a change
// upstream. MinIO's own images stopped serving anonymous pulls on both Docker
// Hub and quay.io, so this is Bitnami's archived build of the same server. See
// ADR 11 for why an archived image beats a credentialled pull here.
const minioImage = "bitnamilegacy/minio:2025.7.23-debian-12-r5"

// MinIO describes a running MinIO and the bucket it was given.
type MinIO struct {
	Endpoint string
	Bucket   string
	Username string
	Password string
}

// StartMinIO starts MinIO, creates a bucket, and stops the container when the
// test finishes.
//
// This drives the container directly rather than through testcontainers'
// minio module: that module overrides the command with `server /data`, which
// the Bitnami image's entrypoint does not accept, and whose data directory its
// non-root user cannot write.
func StartMinIO(t *testing.T) MinIO {
	t.Helper()
	ctx := t.Context()

	const (
		user     = "sindri"
		password = "sindri-test-secret"
		bucket   = "sindri-blobs"
	)

	container, err := testcontainers.Run(ctx, minioImage,
		testcontainers.WithExposedPorts(minioPort),
		testcontainers.WithEnv(map[string]string{
			"MINIO_ROOT_USER":     user,
			"MINIO_ROOT_PASSWORD": password,
		}),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/minio/health/live").WithPort(minioPort),
		),
	)
	testcontainers.CleanupContainer(t, container)
	if err != nil {
		t.Fatalf("starting MinIO: %v", err)
	}

	hostPort, err := container.PortEndpoint(ctx, minioPort, "")
	if err != nil {
		t.Fatalf("resolving the MinIO endpoint: %v", err)
	}

	m := MinIO{
		Endpoint: "http://" + hostPort,
		Bucket:   bucket,
		Username: user,
		Password: password,
	}
	createBucket(t, m)

	return m
}

// minioPort is the S3 API port. The Bitnami image serves the web console on
// 9001, which no test needs.
const minioPort = "9000/tcp"

// Config returns an s3store configuration pointed at m, isolated under prefix
// so that several stores can share one container without seeing each other.
func (m MinIO) Config(prefix string) s3store.Config {
	return s3store.Config{
		Bucket:          m.Bucket,
		Prefix:          prefix,
		Region:          "us-east-1",
		Endpoint:        m.Endpoint,
		UsePathStyle:    true,
		AccessKeyID:     m.Username,
		SecretAccessKey: m.Password,
	}
}

func createBucket(t *testing.T, m MinIO) {
	t.Helper()

	cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(m.Username, m.Password, ""),
		),
	)
	if err != nil {
		t.Fatalf("loading AWS configuration: %v", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(m.Endpoint)
		o.UsePathStyle = true
	})

	_, err = client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(m.Bucket)})
	var exists *types.BucketAlreadyOwnedByYou
	if err != nil && !errors.As(err, &exists) {
		t.Fatalf("creating bucket %s: %v", m.Bucket, err)
	}
}
