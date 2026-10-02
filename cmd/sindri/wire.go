package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/spj/sindri/internal/api"
	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/proto/oci"
	"github.com/spj/sindri/internal/proto/pypi"
	"github.com/spj/sindri/internal/reconcile"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/remote"
	"github.com/spj/sindri/internal/resolve"
	"github.com/spj/sindri/internal/server"
	"github.com/spj/sindri/internal/storage"
)

// app is a wired server: everything `serve` builds from one configuration.
// Integration tests build the same thing, so they exercise the real wiring
// rather than a copy of it that drifts.
type app struct {
	logger      *slog.Logger
	db          *db.DB
	reg         *registry.Registry
	reconciler  *reconcile.Reconciler
	credentials *config.CredentialStore
	auth        *auth.Chain
	server      *server.Server
}

// buildOptions carries what differs between `serve` and a test.
type buildOptions struct {
	// Blobs overrides the configured blob store; tests pass memstore.
	Blobs storage.BlobStore
	// RemoteOptions adjusts the proxy client; tests point it at stubs.
	RemoteOptions func(*remote.Options)
}

// build migrates the database and wires every component. close releases the
// database pool.
func build(ctx context.Context, cfg *config.Config, logger *slog.Logger, bo buildOptions) (*app, func(), error) {
	dbOpts := db.Options{
		URL:            cfg.Database.URL,
		MaxConns:       cfg.Database.MaxConns,
		MinConns:       cfg.Database.MinConns,
		ConnectTimeout: cfg.Database.ConnectTimeout.Duration(),
	}

	logger.InfoContext(ctx, "migrating database", "url", cfg.Database.Redacted())
	if err := db.Migrate(ctx, dbOpts, logger); err != nil {
		return nil, nil, err
	}
	database, err := db.Open(ctx, dbOpts)
	if err != nil {
		return nil, nil, err
	}

	blobs := bo.Blobs
	if blobs == nil {
		blobs, err = openBlobStore(ctx, cfg.Storage)
		if err != nil {
			database.Close()
			return nil, nil, err
		}
	}

	casStore := cas.New(blobs)
	reg := registry.New(database, casStore, string(cfg.Storage.Backend))
	creds := config.NewCredentialStore(cfg.Credentials)

	remoteOpts := remote.Options{Registry: reg, Logger: logger, Credentials: creds}
	if bo.RemoteOptions != nil {
		bo.RemoteOptions(&remoteOpts)
	}
	remoteClient := remote.New(remoteOpts)
	resolver := resolve.New(reg, remoteClient)
	reconciler := reconcile.New(database, reg, creds.Names)

	if cfg.Auth.StaticToken == config.DevStaticToken {
		logger.WarnContext(ctx, "auth.static_token is the development default; anyone who has read the README is an administrator of this server")
	}
	tokens := auth.NewTokenService(database)
	chain := auth.NewChain(cfg.Auth.StaticToken, tokens, nil, cfg.Auth.Admins)
	// A virtual repository answers only from members the caller may read.
	resolver.SetMemberAccess(func(ctx context.Context, m model.Repository) bool {
		p, _ := auth.PrincipalFromContext(ctx)
		return auth.Authorize(p, m, auth.RoleRead) == nil
	})

	apiHandler := api.Handler(api.Options{
		Registry:    reg,
		Lifecycle:   lifecycle.New(database),
		Evidence:    evidence.New(database, casStore, string(cfg.Storage.Backend)),
		Auth:        chain,
		RequestID:   server.RequestIDFromContext,
		Credentials: creds.Names,
		Reconciler:  reconciler,
		Tokens:      tokens,
	})
	ociHandler := oci.Handler(oci.Options{
		Resolver: resolver,
		CAS:      casStore,
		Auth:     chain,
	})
	pypiHandler := pypi.Handler(pypi.Options{
		Resolver: resolver,
		Auth:     chain,
	})

	srv, err := server.New(server.Options{
		Address:           cfg.Listen.Address,
		ReadHeaderTimeout: cfg.Listen.ReadHeaderTimeout.Duration(),
		ShutdownTimeout:   cfg.Listen.ShutdownTimeout.Duration(),
		Database:          database,
		Blobs:             blobs,
		API:               apiHandler,
		OCI:               ociHandler,
		PyPI:              pypiHandler,
		Logger:            logger,
	})
	if err != nil {
		database.Close()
		return nil, nil, err
	}

	return &app{
		logger:      logger,
		db:          database,
		reg:         reg,
		reconciler:  reconciler,
		credentials: creds,
		auth:        chain,
		server:      srv,
	}, database.Close, nil
}

// reconcileFile converges the database onto the desired state of a
// sindri.yaml. It never prunes: removing a repository from the file reports
// it as orphaned, and deleting it takes an explicit `sindri apply --prune`.
func (a *app) reconcileFile(ctx context.Context, cfg *config.Config) error {
	if cfg.Desired == nil {
		return nil
	}
	res, err := a.reconciler.Reconcile(ctx, cfg.Desired, reconcile.Options{
		Source: model.SourceFile,
		Scope:  reconcile.Scope{Global: true},
	})
	if err != nil {
		return fmt.Errorf("reconciling configuration: %w", err)
	}
	ch := res.Repositories
	updated := make([]string, 0, len(ch.Updated))
	for _, u := range ch.Updated {
		updated = append(updated, u.Name)
	}
	a.logger.InfoContext(ctx, "reconciled configuration",
		"created", strings.Join(ch.Created, ","),
		"updated", strings.Join(updated, ","),
		"unchanged", len(ch.Unchanged),
		"stages_changed", res.Stages != nil,
	)
	if len(ch.Orphaned) > 0 {
		a.logger.WarnContext(ctx, "config-managed repositories are no longer in the configuration; delete them with sindri apply --prune",
			"orphaned", strings.Join(ch.Orphaned, ","))
	}
	return nil
}
