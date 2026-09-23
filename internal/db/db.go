// Package db owns the Postgres connection pool and the embedded migrations.
//
// Postgres is the only supported metadata store (ADR 2), and migrations ship
// inside the binary and apply on startup, so upgrading Sindri is replacing one
// binary. See ARCHITECTURE.md section 9.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Options describes a connection pool.
//
// This package deliberately does not read the configuration file: it takes the
// few values it needs, so that the low level store stays independent of the
// configuration format the milestones keep extending.
type Options struct {
	URL            string
	MaxConns       int32
	MinConns       int32
	ConnectTimeout time.Duration
}

// DB is a Postgres connection pool.
type DB struct {
	pool *pgxpool.Pool
}

// Pinger is the readiness check's view of a database, so that an HTTP test does
// not need Postgres to exercise the endpoint that reports on it.
type Pinger interface {
	Ping(ctx context.Context) error
}

var _ Pinger = (*DB)(nil)

// Open parses opts, opens a pool, and verifies it can reach the server.
func Open(ctx context.Context, opts Options) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(opts.URL)
	if err != nil {
		// The URL carries a password, so report the failure without it.
		return nil, fmt.Errorf("parsing the database URL: %w", err)
	}

	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	if opts.ConnectTimeout > 0 {
		cfg.ConnConfig.ConnectTimeout = opts.ConnectTimeout
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("creating the connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to the database: %w", err)
	}

	return &DB{pool: pool}, nil
}

// Pool exposes the pool for query packages. M2 generates typed queries with
// sqlc against it.
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

// Ping reports whether the database is reachable. This is what /readyz asks.
func (d *DB) Ping(ctx context.Context) error {
	if err := d.pool.Ping(ctx); err != nil {
		return fmt.Errorf("pinging the database: %w", err)
	}
	return nil
}

// Close releases every connection.
func (d *DB) Close() { d.pool.Close() }
