//go:build integration

package testsupport

import (
	"errors"
	"testing"
	"time"
)

func TestSeedLarge_WritesTheDocumentedShapeAndRefusesASecondRun(t *testing.T) {
	database := MigratedDB(t)
	start := time.Now()
	info := SeedLarge(t, database.Pool())
	t.Logf("seeded %d repositories, %d packages, %d artifacts, %d blobs, %d subjects, %d evidence, %d events in %s",
		info.Repositories, info.Packages, info.Artifacts, info.Blobs, info.Subjects, info.Evidence, info.Events, time.Since(start).Round(time.Millisecond))

	if info.Repositories != 500 {
		t.Errorf("repositories = %d, want 500", info.Repositories)
	}
	if info.Packages != 50000 {
		t.Errorf("packages = %d, want 50000", info.Packages)
	}
	for name, v := range map[string]string{"OCIDigest": info.OCIDigest, "PyPIDigest": info.PyPIDigest, "NPMDigest": info.NPMDigest} {
		if v == "" {
			t.Errorf("%s is empty", name)
		}
	}

	var scans, revoked int
	if err := database.Pool().QueryRow(t.Context(), `SELECT count(*) FROM evidence WHERE type = 'scan'`).Scan(&scans); err != nil || scans == 0 {
		t.Errorf("scan evidence = %d, %v", scans, err)
	}
	if err := database.Pool().QueryRow(t.Context(), `SELECT count(*) FROM lifecycle_current WHERE action = 'revoke'`).Scan(&revoked); err != nil || revoked == 0 {
		t.Errorf("revoked states = %d, %v", revoked, err)
	}

	// COPY fires the row triggers, so the seed's versions must have left each
	// repository's last publish at the newest of them.
	var wrong, populated int
	if err := database.Pool().QueryRow(t.Context(), `
		SELECT count(*) FILTER (WHERE r.last_published_at IS DISTINCT FROM s.newest),
		       count(*) FILTER (WHERE r.last_published_at IS NOT NULL)
		FROM repositories r
		JOIN (SELECT p.repo_id, max(v.published_at) AS newest
		      FROM versions v JOIN packages p ON p.id = v.package_id GROUP BY p.repo_id) s ON s.repo_id = r.id`).Scan(&wrong, &populated); err != nil {
		t.Fatalf("checking last_published_at: %v", err)
	}
	if wrong != 0 || populated == 0 {
		t.Errorf("%d repositories disagree with their newest version, %d populated; want 0 and some", wrong, populated)
	}

	if _, err := Seed(t.Context(), database.Pool()); !errors.Is(err, ErrAlreadySeeded) {
		t.Errorf("second Seed error = %v, want ErrAlreadySeeded", err)
	}
}
