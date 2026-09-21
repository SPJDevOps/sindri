package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	// Registers the pgx driver with database/sql, which is how goose reaches
	// Postgres without a second driver dependency.
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns the embedded migration set, so that a test can count what
// shipped without reaching into this package's internals.
func Migrations() (fs.FS, error) {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("opening the embedded migrations: %w", err)
	}
	return sub, nil
}

// Migrate applies every outstanding migration.
//
// It runs under a Postgres advisory lock, because nothing in the design may
// assume the API and the worker share a process (ARCHITECTURE.md section 9):
// two instances starting at once must not both try to migrate. The lock is a
// session lock rather than a transaction lock so that a migration which cannot
// run inside a transaction is still serialised.
func Migrate(ctx context.Context, opts Options, log *slog.Logger) error {
	dir, err := Migrations()
	if err != nil {
		return err
	}

	// goose speaks database/sql, so open a second, short lived handle over the
	// same pgx driver rather than keeping the pool's connections busy.
	sqlDB, err := sql.Open("pgx", opts.URL)
	if err != nil {
		return fmt.Errorf("opening a migration connection: %w", err)
	}
	defer func() {
		if cerr := sqlDB.Close(); cerr != nil && log != nil {
			log.WarnContext(ctx, "closing the migration connection", slog.String("error", cerr.Error()))
		}
	}()

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("creating the migration lock: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, dir, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("preparing migrations: %w", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}

	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("reading the schema version: %w", err)
	}
	if log != nil {
		log.InfoContext(ctx, "database schema is up to date",
			slog.Int64("version", version),
			slog.Int("applied", len(results)),
		)
	}
	return nil
}
