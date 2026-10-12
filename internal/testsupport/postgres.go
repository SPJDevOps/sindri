//go:build integration

package testsupport

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
		// The log line says Postgres is ready; the port wait says Docker
		// has published the mapping. Docker Desktop can report the first
		// before the second, which surfaced as an intermittent
		// `port "5432/tcp" not found` once several suites ran at once.
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
				wait.ForListeningPort("5432/tcp"),
			).WithDeadline(2*time.Minute),
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

// shared is one Postgres container per test binary, from which every
// MigratedDB call carves a database of its own. A container per test is the
// isolation StartPostgres gives; a database per test is the same isolation for
// the suites that need dozens of them, without a container start each.
var shared struct {
	once sync.Once
	url  string
	err  error
	n    atomic.Int64
}

// MigratedDB returns a pool on a new, fully migrated database of its own,
// closed when the test finishes. The container behind it is shared by the test
// binary and reaped by testcontainers when the process exits.
func MigratedDB(t *testing.T) *db.DB {
	t.Helper()
	opts := DBOptions(DatabaseURL(t))
	// The concurrency suites hold a transaction open while another writer
	// runs, so they need more than the default handful of connections.
	opts.MaxConns = 12
	if err := db.Migrate(t.Context(), opts, nil); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	database, err := db.Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(database.Close)
	return database
}

// DatabaseURL returns the URL of a new, empty database of its own on the
// shared container, for a test that runs migrations itself, as serve does.
func DatabaseURL(t *testing.T) string {
	t.Helper()
	shared.once.Do(func() {
		// The container outlives any one test, so it cannot take a test's
		// context; testcontainers' reaper stops it with the process.
		ctx := context.Background()
		container, err := tcpostgres.Run(ctx, postgresImage,
			tcpostgres.WithDatabase("sindri"),
			tcpostgres.WithUsername("sindri"),
			tcpostgres.WithPassword("sindri"),
			testcontainers.WithWaitStrategy(
				wait.ForAll(
					wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
					wait.ForListeningPort("5432/tcp"),
				).WithDeadline(2*time.Minute),
			),
		)
		if err != nil {
			shared.err = fmt.Errorf("starting shared Postgres: %w", err)
			return
		}
		shared.url, shared.err = container.ConnectionString(ctx, "sslmode=disable")
	})
	if shared.err != nil {
		t.Fatalf("%v", shared.err)
	}

	admin, err := pgx.Connect(t.Context(), shared.url)
	if err != nil {
		t.Fatalf("connecting to shared Postgres: %v", err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(t.Context())) }()

	name := fmt.Sprintf("t%d_%d", os.Getpid(), shared.n.Add(1))
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating database %s: %v", name, err)
	}

	u, err := url.Parse(shared.url)
	if err != nil {
		t.Fatalf("parsing the shared Postgres URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}
