//go:build integration

package testsupport

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ClockSkew is how far MinIO's clock is ahead of this process's, measured from
// the Date header of its health endpoint. The header has one-second resolution
// and the request takes time, so it is good to a second or two, which is plenty
// to tell a skew of a minute from none.
func (m MinIO) ClockSkew(ctx context.Context) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.Endpoint+"/minio/health/live", nil)
	if err != nil {
		return 0, err
	}
	sent := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	received := time.Now()
	server, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return 0, err
	}
	// The server stamped the response somewhere between send and receive, and the
	// header is truncated to the second, so compare against the midpoint plus half
	// a second.
	midpoint := sent.Add(received.Sub(sent) / 2).Add(500 * time.Millisecond)
	return server.Sub(midpoint), nil
}

// LogUploads logs every multipart upload MinIO lists for the bucket, with when
// MinIO says it was initiated, against this process's clock and MinIO's
// measured skew. It is for failure diagnostics and never fails the test.
func (m MinIO) LogUploads(t *testing.T) {
	t.Helper()
	// Cleanups run after the test's context is cancelled.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 15*time.Second)
	defer cancel()

	skew, err := m.ClockSkew(ctx)
	if err != nil {
		t.Logf("MinIO clock skew: unmeasured: %v", err)
	} else {
		t.Logf("MinIO clock skew (MinIO minus host): %s", skew)
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(m.Username, m.Password, "")),
	)
	if err != nil {
		t.Logf("listing uploads: %v", err)
		return
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(m.Endpoint)
		o.UsePathStyle = true
	})
	out, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(m.Bucket)})
	if err != nil {
		t.Logf("listing uploads: %v", err)
		return
	}
	now := time.Now()
	t.Logf("host time %s, %d multipart uploads listed (truncated=%t)", now.UTC().Format(time.RFC3339Nano), len(out.Uploads), aws.ToBool(out.IsTruncated))
	for _, u := range out.Uploads {
		var initiated string
		if u.Initiated != nil {
			initiated = u.Initiated.UTC().Format(time.RFC3339Nano) + " (" + now.Sub(*u.Initiated).String() + " before host time)"
		}
		t.Logf("  key %s initiated %s", aws.ToString(u.Key), initiated)
	}
}

// traceServer, when SINDRI_MINIO_TRACE_DIR is set, records MinIO's own view of
// every request (mc admin trace, run inside the container) to a file in that
// directory, and its container log on failure. It is off by default.
func (m MinIO) traceServer(t *testing.T, containerID string) {
	t.Helper()
	dir := os.Getenv("SINDRI_MINIO_TRACE_DIR")
	if dir == "" {
		return
	}
	out, err := os.Create(filepath.Join(dir, fmt.Sprintf("trace-%s.log", containerID[:12])))
	if err != nil {
		t.Logf("trace file: %v", err)
		return
	}
	script := fmt.Sprintf("mc alias set l http://localhost:9000 %s %s >/dev/null && exec mc admin trace l --verbose --all", m.Username, m.Password)
	cmd := exec.Command("docker", "exec", containerID, "sh", "-c", script)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Logf("starting trace: %v", err)
		return
	}
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", containerID).CombinedOutput()
			_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("container-%s.log", containerID[:12])), logs, 0o600)
			t.Logf("MinIO trace kept: %s", out.Name())
		}
		_ = cmd.Process.Kill()
		_ = out.Close()
	})
}
