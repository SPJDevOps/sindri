//go:build integration

package testsupport

import (
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/spj/sindri/internal/db"
)

// postgresImage matches the version deploy/compose/docker-compose.yml runs, so
// that a test passing here means the same thing in development.
const postgresImage = "postgres:18-alpine"

// StartPostgres starts Postgres and returns its connection URL, stopping the
// container when the test finishes.
func StartPostgres(t *testing.T) string {
	t.Helper()

	container, err := tcpostgres.Run(t.Context(), postgresImage,
		tcpostgres.WithDatabase("sindri"),
		tcpostgres.WithUsername("sindri"),
		tcpostgres.WithPassword("sindri"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute),
		),
	)
	testcontainers.CleanupContainer(t, container)
	if err != nil {
		t.Fatalf("starting Postgres: %v", err)
	}

	url, err := container.ConnectionString(t.Context(), "sslmode=disable")
	if err != nil {
		t.Fatalf("resolving the Postgres URL: %v", err)
	}
	return url
}

// DBOptions returns pool options pointed at url.
func DBOptions(url string) db.Options {
	return db.Options{URL: url, MaxConns: 4, ConnectTimeout: 10 * time.Second}
}
