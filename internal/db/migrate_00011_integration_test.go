//go:build integration

package db_test

import (
	"testing"

	"github.com/spj/sindri/internal/testsupport"
)

func TestMigrate_00011_LeavesExistingVersionsUndatedAndDatesNewOnes(t *testing.T) {
	url := testsupport.StartPostgres(t)
	p, sqlDB := provider(t, url)
	ctx := t.Context()

	if _, err := p.UpTo(ctx, 10); err != nil {
		t.Fatalf("UpTo 10: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO repositories (name, type, protocol, managed_by) VALUES ('npm-hosted', 'hosted', 'npm', 'api')`,
		`INSERT INTO packages (repo_id, normalized_name, display_name) SELECT id, 'foo', 'foo' FROM repositories`,
		`INSERT INTO versions (package_id, version) SELECT id, '1.0.0' FROM packages`,
	} {
		if _, err := sqlDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seeding (%s): %v", stmt, err)
		}
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO versions (package_id, version) SELECT id, '2.0.0' FROM packages`); err != nil {
		t.Fatalf("inserting a version after the migration: %v", err)
	}

	for version, wantDated := range map[string]bool{"1.0.0": false, "2.0.0": true} {
		var dated bool
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT published_at IS NOT NULL FROM versions WHERE version = $1`, version).Scan(&dated); err != nil {
			t.Fatalf("reading %s: %v", version, err)
		}
		if dated != wantDated {
			t.Fatalf("version %s has a published_at = %v, want %v", version, dated, wantDated)
		}
	}

	if _, err := p.DownTo(ctx, 10); err != nil {
		t.Fatalf("rolling back: %v", err)
	}
}
