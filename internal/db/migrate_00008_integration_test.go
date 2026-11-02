//go:build integration

package db_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/spj/sindri/internal/testsupport"
)

func TestMigrate_00008_RollsBackWithNpmRowsPresent(t *testing.T) {
	url := testsupport.StartPostgres(t)
	p, sqlDB := provider(t, url)
	ctx := t.Context()

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	digest := "sha256:" + hex64('e')
	for _, stmt := range []string{
		`INSERT INTO repositories (name, type, protocol, managed_by) VALUES ('npm-hosted', 'hosted', 'npm', 'api')`,
		`INSERT INTO blobs (digest, size, storage_backend) VALUES ('` + digest + `', 1, 'filesystem')`,
		`INSERT INTO subjects (digest, size, kind) VALUES ('` + digest + `', 1, 'npm-tarball')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seeding npm rows (%s): %v", stmt, err)
		}
	}

	if _, err := p.DownTo(ctx, 7); err != nil {
		t.Fatalf("rolling back to the pre-npm schema with npm rows present: %v", err)
	}

	for table, want := range map[string]string{
		"repositories": `SELECT count(*) FROM repositories WHERE protocol = 'npm'`,
		"subjects":     `SELECT count(*) FROM subjects WHERE kind = 'npm-tarball'`,
	} {
		var n int
		if err := sqlDB.QueryRowContext(ctx, want).Scan(&n); err != nil {
			t.Fatalf("counting npm rows in %s: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("%s has %d npm rows after the rollback, want 1", table, n)
		}
	}

	_, err := sqlDB.ExecContext(ctx,
		`INSERT INTO repositories (name, type, protocol, managed_by) VALUES ('npm-second', 'hosted', 'npm', 'api')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("new npm repository after the rollback: err = %v, want a check violation", err)
	}

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("migrating up again over npm rows: %v", err)
	}
}

func TestDistTagEvents_AreAppendOnly(t *testing.T) {
	pool := testsupport.MigratedDB(t)
	ctx := t.Context()

	if _, err := pool.Pool().Exec(ctx,
		`INSERT INTO dist_tag_events (repository, package, tag, to_version, actor)
		 VALUES ('r', 'p', 'latest', '1.0.0', 'alice')`); err != nil {
		t.Fatalf("appending an event: %v", err)
	}

	for name, stmt := range map[string]string{
		"update":   `UPDATE dist_tag_events SET actor = 'mallory'`,
		"delete":   `DELETE FROM dist_tag_events`,
		"truncate": `TRUNCATE dist_tag_events`,
	} {
		if _, err := pool.Pool().Exec(ctx, stmt); err == nil {
			t.Fatalf("%s on dist_tag_events succeeded, want it refused", name)
		}
	}
}
