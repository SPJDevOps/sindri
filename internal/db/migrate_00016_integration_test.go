//go:build integration

package db_test

import (
	"testing"
	"time"

	"github.com/spj/sindri/internal/testsupport"
)

func TestMigrate_00016_BackfillsLastPublishedFromTheNewestDatedVersion(t *testing.T) {
	url := testsupport.StartPostgres(t)
	p, sqlDB := provider(t, url)
	ctx := t.Context()

	if _, err := p.UpTo(ctx, 15); err != nil {
		t.Fatalf("UpTo 15: %v", err)
	}
	newest := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	for _, stmt := range []string{
		`INSERT INTO repositories (name, type, protocol, managed_by) VALUES
			('dated', 'hosted', 'npm', 'api'),
			('undated', 'hosted', 'npm', 'api'),
			('empty', 'hosted', 'npm', 'api')`,
		`INSERT INTO packages (repo_id, normalized_name, display_name)
			SELECT id, p.n, p.n FROM repositories, (VALUES ('a'), ('b')) p(n) WHERE name IN ('dated', 'undated')`,
		`INSERT INTO versions (package_id, version, published_at)
			SELECT pk.id, '1.0.0', '2026-01-01T00:00:00Z' FROM packages pk JOIN repositories r ON r.id = pk.repo_id
			WHERE r.name = 'dated' AND pk.normalized_name = 'a'`,
		`INSERT INTO versions (package_id, version, published_at)
			SELECT pk.id, '1.0.0', '` + newest.Format(time.RFC3339) + `' FROM packages pk JOIN repositories r ON r.id = pk.repo_id
			WHERE r.name = 'dated' AND pk.normalized_name = 'b'`,
		`INSERT INTO versions (package_id, version, published_at)
			SELECT pk.id, '1.0.0', NULL FROM packages pk JOIN repositories r ON r.id = pk.repo_id WHERE r.name = 'undated'`,
	} {
		if _, err := sqlDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seeding (%s): %v", stmt, err)
		}
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}

	for name, want := range map[string]*time.Time{"dated": &newest, "undated": nil, "empty": nil} {
		var got *time.Time
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT last_published_at FROM repositories WHERE name = $1`, name).Scan(&got); err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		switch {
		case want == nil && got != nil:
			t.Fatalf("%s: last_published_at = %v, want NULL", name, got)
		case want != nil && (got == nil || !got.Equal(*want)):
			t.Fatalf("%s: last_published_at = %v, want %v", name, got, want)
		}
	}

	if _, err := p.DownTo(ctx, 15); err != nil {
		t.Fatalf("rolling back: %v", err)
	}
}
