//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/testsupport"
)

func TestMigrate_AppliesAndIsIdempotent(t *testing.T) {
	url := testsupport.StartPostgres(t)
	opts := testsupport.DBOptions(url)

	// Applying twice is the upgrade path: a restarted binary re-runs Migrate
	// against a schema that is already current and must simply carry on.
	for i := range 2 {
		if err := db.Migrate(t.Context(), opts, nil); err != nil {
			t.Fatalf("Migrate run %d: %v", i+1, err)
		}
	}

	pool, err := db.Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)

	var version int64
	err = pool.Pool().QueryRow(t.Context(), "SELECT max(version_id) FROM goose_db_version").Scan(&version)
	if err != nil {
		t.Fatalf("reading the schema version: %v", err)
	}
	if version < 3 {
		t.Fatalf("schema version = %d, want at least 3 (repository_blobs)", version)
	}

	var rows int
	if err := pool.Pool().QueryRow(t.Context(), "SELECT count(*) FROM schema_origin").Scan(&rows); err != nil {
		t.Fatalf("reading schema_origin: %v", err)
	}
	if rows != 1 {
		t.Fatalf("schema_origin holds %d rows after two migration runs, want 1", rows)
	}

	for _, table := range []string{
		"blobs", "repositories", "repository_members", "remotes",
		"packages", "versions", "files", "tags", "manifest_children",
		"repository_blobs",
	} {
		var exists bool
		err := pool.Pool().QueryRow(t.Context(),
			`SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = 'public' AND table_name = $1
			)`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("checking table %s: %v", table, err)
		}
		if !exists {
			t.Fatalf("expected placement table %s after migrate", table)
		}
	}
}

func TestOpen_PingsAndReportsAnUnreachableDatabase(t *testing.T) {
	url := testsupport.StartPostgres(t)

	pool, err := db.Open(t.Context(), testsupport.DBOptions(url))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// A closed pool is what readiness sees when the database goes away.
	pool.Close()
	if err := pool.Ping(context.WithoutCancel(t.Context())); err == nil {
		t.Fatal("Ping on a closed pool succeeded")
	}
}

func TestOpen_RejectsAMalformedURL(t *testing.T) {
	_, err := db.Open(t.Context(), db.Options{URL: "not-a-url"})
	if err == nil {
		t.Fatal("Open accepted a malformed URL")
	}
}
