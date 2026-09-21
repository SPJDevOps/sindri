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
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/spj/sindri/internal/storage/s3store"
)

// minioImage is pinned so a test failure is a change in Sindri, never a change
// upstream. It comes from quay.io because MinIO's Docker Hub repository no
// longer serves anonymous pulls.
const minioImage = "quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z"

// MinIO describes a running MinIO and the bucket it was given.
type MinIO struct {
	Endpoint string
	Bucket   string
	Username string
	Password string
}

// StartMinIO starts MinIO, creates a bucket, and stops the container when the
// test finishes.
func StartMinIO(t *testing.T) MinIO {
	t.Helper()
	ctx := t.Context()

	const (
		user     = "sindri"
		password = "sindri-test-secret"
		bucket   = "sindri-blobs"
	)

	container, err := tcminio.Run(ctx, minioImage,
		tcminio.WithUsername(user),
		tcminio.WithPassword(password),
	)
	testcontainers.CleanupContainer(t, container)
	if err != nil {
		t.Fatalf("starting MinIO: %v", err)
	}

	hostPort, err := container.ConnectionString(ctx)
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
