//go:build integration

package db_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/testsupport"
)

// m7UpsertBlob is the blob upsert exactly as an M7 binary sends it. Migrations
// must stay backward compatible for one version, so a rollback to M7 keeps
// writing against the M8 schema without a restore.
const m7UpsertBlob = `INSERT INTO blobs (digest, size, storage_backend)
VALUES ($1, $2, $3)
ON CONFLICT (digest) DO UPDATE
SET size = EXCLUDED.size
RETURNING digest, size, storage_backend, created_at`

func provider(t *testing.T, url string) (*goose.Provider, *sql.DB) {
	t.Helper()
	dir, err := db.Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, dir)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p, sqlDB
}

func m7Writes(t *testing.T, sqlDB *sql.DB, digest string) {
	t.Helper()
	ctx := t.Context()
	var (
		gotDigest, backend string
		size               int64
		created            time.Time
	)
	if err := sqlDB.QueryRowContext(ctx, m7UpsertBlob, digest, 3, "filesystem").
		Scan(&gotDigest, &size, &backend, &created); err != nil {
		t.Fatalf("M7 blob upsert: %v", err)
	}
	if err := sqlDB.QueryRowContext(ctx, m7UpsertBlob, digest, 4, "filesystem").
		Scan(&gotDigest, &size, &backend, &created); err != nil {
		t.Fatalf("M7 blob upsert on conflict: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO blobs (digest, size, storage_backend) VALUES ($1, $2, $3) ON CONFLICT (digest) DO NOTHING`,
		digest+"0", 1, "filesystem"); err != nil {
		t.Fatalf("M7 placeholder insert: %v", err)
	}
}

func TestMigrate_00007AcceptsM7Writes(t *testing.T) {
	url := testsupport.StartPostgres(t)
	p, sqlDB := provider(t, url)
	ctx := t.Context()

	if _, err := p.UpTo(ctx, 6); err != nil {
		t.Fatalf("migrating to M7's schema: %v", err)
	}
	m7Writes(t, sqlDB, "sha256:"+hex64('a'))
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("migrating to M8's schema over M7 data: %v", err)
	}
	m7Writes(t, sqlDB, "sha256:"+hex64('b'))

	var missing int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM blobs WHERE last_seen_at IS NULL`).Scan(&missing); err != nil {
		t.Fatalf("reading last_seen_at: %v", err)
	}
	if missing != 0 {
		t.Fatalf("%d blobs have no last_seen_at after the upgrade", missing)
	}

	// Rows from before the upgrade count as seen at the upgrade, never at
	// their first write: nothing an intent never covered is collectable
	// before one grace period has passed.
	var stale int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM blobs WHERE last_seen_at < now() - interval '1 minute'`).Scan(&stale); err != nil {
		t.Fatalf("reading last_seen_at: %v", err)
	}
	if stale != 0 {
		t.Fatalf("%d pre-upgrade blobs look old enough to collect", stale)
	}
}

func TestMigrate_00007DownRestoresM7Schema(t *testing.T) {
	url := testsupport.StartPostgres(t)
	p, sqlDB := provider(t, url)
	ctx := t.Context()

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	m7Writes(t, sqlDB, "sha256:"+hex64('c'))
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO jobs (kind, created_by) VALUES ('gc', 'test')`); err != nil {
		t.Fatalf("enqueueing: %v", err)
	}

	if _, err := p.DownTo(ctx, 6); err != nil {
		t.Fatalf("rolling back to M7's schema with data present: %v", err)
	}
	m7Writes(t, sqlDB, "sha256:"+hex64('d'))

	var hasColumn bool
	if err := sqlDB.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'blobs' AND column_name = 'last_seen_at')`).Scan(&hasColumn); err != nil {
		t.Fatalf("inspecting blobs: %v", err)
	}
	if hasColumn {
		t.Fatal("blobs.last_seen_at survived the down migration")
	}
}

func hex64(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
