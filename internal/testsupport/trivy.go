//go:build integration

package testsupport

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// trivyImage is pinned by version and digest (the multi-arch index) so a
// test failure is a change in Sindri, never a change upstream. The digest
// matters more than the tag: a tag can be moved, and this binary decides
// whether the scan tests pass.
const trivyImage = "aquasec/trivy:0.69.3@sha256:bcc376de8d77cfe086a917230e818dc9f8528e3c852f7b1aff648949b6258d1c"

// TrivyBinary returns the path of a Trivy executable the host can run. It
// prefers one on PATH, so a developer's own install is used as is. On linux
// without one it copies the binary out of the pinned image, which is how CI
// runs it, and a failure there fails the test when CI is set. Anywhere else the test is skipped: the image holds a linux binary
// and a darwin or windows host cannot execute it.
func TrivyBinary(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("trivy"); err == nil {
		return p
	}
	if runtime.GOOS != "linux" {
		t.Skipf("no trivy on PATH and %s cannot run the linux binary in %s; install trivy to run this test", runtime.GOOS, trivyImage)
	}
	ctx := t.Context()
	container, err := startContainer(t, ctx, func(ctx context.Context) (*testcontainers.DockerContainer, error) {
		// The image's entrypoint is trivy itself; the container only has to
		// exist for its filesystem to be read.
		return testcontainers.Run(ctx, trivyImage,
			testcontainers.WithEntrypoint("sleep"),
			testcontainers.WithCmd("600"),
			testcontainers.WithWaitStrategy(wait.ForExec([]string{"true"})),
		)
	})
	if err != nil {
		// CI is expected to have Docker, so there a failure is a failure.
		if os.Getenv("CI") != "" {
			t.Fatalf("no trivy on PATH and the pinned image could not be started: %v", err)
		}
		t.Skipf("no trivy on PATH and the pinned image could not be started: %v", err)
	}
	testcontainers.CleanupContainer(t, container)

	rc, err := container.CopyFileFromContainer(ctx, "/usr/local/bin/trivy")
	if err != nil {
		t.Fatalf("copying trivy out of %s: %v", trivyImage, err)
	}
	defer func() { _ = rc.Close() }()
	path := filepath.Join(t.TempDir(), "trivy")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o700) //nolint:gosec // G302: the test must execute this file
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadFrom(rc); err != nil {
		_ = f.Close()
		t.Fatalf("writing trivy: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
