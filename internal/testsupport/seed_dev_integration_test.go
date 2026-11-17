//go:build integration

package testsupport

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/spj/sindri/internal/db"
)

// seedDatabaseEnv names the database `make ui-seed` seeds.
const seedDatabaseEnv = "SINDRI_SEED_DATABASE_URL"

// TestSeedDevelopmentDatabase fills the `make up` database with the large
// seed, so a person can look at the UI at volume through `make run`. It skips
// unless seedDatabaseEnv is set, which only `make ui-seed` does, so
// `make test-integration` never touches a developer's database.
func TestSeedDevelopmentDatabase(t *testing.T) {
	url := os.Getenv(seedDatabaseEnv)
	if url == "" {
		t.Skipf("%s is not set; run `make ui-seed` to seed the development database", seedDatabaseEnv)
	}
	database, err := db.Open(t.Context(), DBOptions(url))
	if err != nil {
		t.Fatalf("opening the development database (is `make up` running?): %v", err)
	}
	defer database.Close()

	var migrated bool
	if err := database.Pool().QueryRow(t.Context(), `SELECT to_regclass('public.lifecycle_events') IS NOT NULL`).Scan(&migrated); err != nil {
		t.Fatalf("checking the schema: %v", err)
	}
	if !migrated {
		t.Fatal("the database has no schema yet: start the server once with `make run` so it migrates, stop it, then run `make ui-seed`")
	}

	start := time.Now()
	info, err := Seed(t.Context(), database.Pool())
	if errors.Is(err, ErrAlreadySeeded) {
		t.Fatal("the development database is already seeded; `make down && make up` resets it")
	}
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	t.Logf("seeded %d repositories and %d packages in %s", info.Repositories, info.Packages, time.Since(start).Round(time.Millisecond))
	t.Logf("start with `make run`; the UI is at the server's /ui/. Look at %s, %s, and %s",
		info.HostedOCI, info.VirtualOCI, info.OCIImage+" in "+info.HostedOCI)
}
