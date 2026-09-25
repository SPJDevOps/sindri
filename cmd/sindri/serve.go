package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/spj/sindri/internal/api"
	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/proto/oci"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/server"
	"github.com/spj/sindri/internal/storage"
	"github.com/spj/sindri/internal/storage/fsstore"
	"github.com/spj/sindri/internal/storage/s3store"
)

func newServeCmd() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Sindri HTTP server",
		Long: `Run the Sindri HTTP server.

Reads one configuration file (sindri.yaml by default, or --config), applies
embedded database migrations, opens the configured blob store, and serves
/healthz, /readyz, /metrics, /api/v1, and /v2 until SIGTERM or SIGINT.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), configPath)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "",
		"path to sindri.yaml (default: sindri.yaml when present, else built-in defaults)")

	return cmd
}

func runServe(parent context.Context, explicitConfig string) error {
	cfg, loadedPath, err := config.LoadOrDefault(explicitConfig)
	if err != nil {
		return err
	}
	if loadedPath != "" {
		if err := cfg.AbsolutePaths(filepath.Dir(loadedPath)); err != nil {
			return err
		}
	}

	logger, err := server.NewLogger(os.Stderr, server.LogOptions{
		Level:  cfg.Log.Level,
		Format: cfg.Log.Format,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if loadedPath != "" {
		logger.InfoContext(ctx, "loaded configuration", "path", loadedPath)
	} else {
		logger.InfoContext(ctx, "using built-in configuration defaults")
	}

	dbOpts := db.Options{
		URL:            cfg.Database.URL,
		MaxConns:       cfg.Database.MaxConns,
		MinConns:       cfg.Database.MinConns,
		ConnectTimeout: cfg.Database.ConnectTimeout.Duration(),
	}

	logger.InfoContext(ctx, "migrating database", "url", cfg.Database.Redacted())
	if err := db.Migrate(ctx, dbOpts, logger); err != nil {
		return err
	}

	database, err := db.Open(ctx, dbOpts)
	if err != nil {
		return err
	}
	defer database.Close()

	blobs, err := openBlobStore(ctx, cfg.Storage)
	if err != nil {
		return err
	}

	casStore := cas.New(blobs)
	reg := registry.New(database, casStore, string(cfg.Storage.Backend))
	authenticator := auth.NewStaticBearer(cfg.Auth.StaticToken)
	apiHandler := api.Handler(api.Options{
		Registry:  reg,
		Auth:      authenticator,
		RequestID: server.RequestIDFromContext,
	})
	ociHandler := oci.Handler(oci.Options{
		Registry: reg,
		CAS:      casStore,
	})

	srv, err := server.New(server.Options{
		Address:           cfg.Listen.Address,
		ReadHeaderTimeout: cfg.Listen.ReadHeaderTimeout.Duration(),
		ShutdownTimeout:   cfg.Listen.ShutdownTimeout.Duration(),
		Database:          database,
		Blobs:             blobs,
		API:               apiHandler,
		OCI:               ociHandler,
		Logger:            logger,
	})
	if err != nil {
		return err
	}

	if err := srv.ListenAndServe(ctx); err != nil {
		return fmt.Errorf("serving: %w", err)
	}
	return nil
}

func openBlobStore(ctx context.Context, cfg config.Storage) (storage.BlobStore, error) {
	switch cfg.Backend {
	case config.BackendFilesystem:
		store, err := fsstore.New(cfg.Path)
		if err != nil {
			return nil, fmt.Errorf("opening filesystem storage at %s: %w", cfg.Path, err)
		}
		return store, nil

	case config.BackendS3:
		store, err := s3store.New(ctx, s3store.Config{
			Bucket:          cfg.Bucket,
			Prefix:          cfg.Prefix,
			Region:          cfg.Region,
			Endpoint:        cfg.Endpoint,
			UsePathStyle:    cfg.PathStyle,
			AccessKeyID:     cfg.AccessKeyID,
			SecretAccessKey: cfg.SecretAccessKey,
			SessionToken:    cfg.SessionToken,
		})
		if err != nil {
			return nil, fmt.Errorf("opening s3 storage bucket %s: %w", cfg.Bucket, err)
		}
		return store, nil

	default:
		return nil, fmt.Errorf("storage.backend: %q is not supported", cfg.Backend)
	}
}
