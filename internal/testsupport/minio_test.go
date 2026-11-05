//go:build integration

package testsupport

import (
	"io"
	"strings"
	"testing"
)

// TestStartMinIO_ReturnsOnlyOnceTheRealServerIsRunning guards SIN-67. The
// Bitnami entrypoint runs a throwaway MinIO while it configures the server,
// stops it, and then starts the real one. A readiness check that only asks the
// health endpoint passes on the throwaway, so tests that began at once lost
// requests (499 ClientDisconnected, broken pipe) and multipart uploads when
// the throwaway stopped. The setup script prints "MinIO setup finished!" after
// the stop, so by the time StartMinIO returns the container's log must hold it.
func TestStartMinIO_ReturnsOnlyOnceTheRealServerIsRunning(t *testing.T) {
	m := StartMinIO(t)

	r, err := m.container.Logs(t.Context())
	if err != nil {
		t.Fatalf("reading MinIO's log: %v", err)
	}
	defer func() { _ = r.Close() }()
	logs, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading MinIO's log: %v", err)
	}
	if !strings.Contains(string(logs), "MinIO setup finished!") {
		t.Fatalf("StartMinIO returned while the throwaway setup instance could still be serving; log:\n%s", logs)
	}
}
