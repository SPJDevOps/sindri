package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/spj/sindri/internal/config"
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
/healthz, /readyz, /metrics, /api/v1, /v2, and /pypi until SIGTERM or SIGINT.

When the file declares repos: or lifecycle:, the database is reconciled onto
them before the server listens, and again on SIGHUP. Nothing is deleted by a
reconcile from the file; a repository removed from it is reported as orphaned.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), configPath)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "",
		"path to sindri.yaml (default: sindri.yaml when present, else built-in defaults)")

	return cmd
}

func runServe(parent context.Context, explicitConfig string) error {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	return serve(parent, explicitConfig, serveHooks{reload: hup})
}

// serveHooks are the seams a test drives instead of process signals.
type serveHooks struct {
	// reload re-reads the configuration and reconciles on every receive.
	reload <-chan os.Signal
	// listening is told the bound address once the socket is open.
	listening func(addr string)
	// build adjusts wiring; tests substitute the blob store and upstreams.
	build buildOptions
}

func serve(parent context.Context, explicitConfig string, hooks serveHooks) error {
	cfg, loadedPath, err := loadServeConfig(explicitConfig)
	if err != nil {
		return err
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

	a, closeApp, err := build(ctx, cfg, logger, hooks.build)
	if err != nil {
		return err
	}
	defer closeApp()

	// Reconcile before listening: a client that can connect sees the
	// repositories the file declares, with no apply step (ADR 7).
	if err := a.reconcileFile(ctx, cfg); err != nil {
		return err
	}

	listener, err := a.server.Listen(ctx)
	if err != nil {
		return err
	}
	if hooks.listening != nil {
		hooks.listening(a.server.Addr())
	}

	if hooks.reload != nil && loadedPath != "" {
		go a.reloadLoop(ctx, loadedPath, cfg, hooks.reload)
	}

	if err := a.server.Serve(ctx, listener); err != nil {
		return fmt.Errorf("serving: %w", err)
	}
	return nil
}

func loadServeConfig(explicit string) (*config.Config, string, error) {
	cfg, loadedPath, err := config.LoadOrDefault(explicit)
	if err != nil {
		return nil, "", err
	}
	if loadedPath != "" {
		if err := cfg.AbsolutePaths(filepath.Dir(loadedPath)); err != nil {
			return nil, "", err
		}
	}
	return cfg, loadedPath, nil
}

// reloadLoop re-reads the file on every signal. A file that fails to load or
// reconcile is logged and the running state is kept: a typo pushed to a
// mounted ConfigMap must not take the registry down.
func (a *app) reloadLoop(ctx context.Context, path string, running *config.Config, signals <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
		}
		a.logger.InfoContext(ctx, "reloading configuration", "path", path)
		next, _, err := loadServeConfig(path)
		if err != nil {
			a.logger.ErrorContext(ctx, "reload failed; keeping the running configuration", "error", err)
			continue
		}
		for _, section := range restartSections(running, next) {
			a.logger.WarnContext(ctx, "configuration section changed but takes effect only on restart", "section", section)
		}
		// New credentials first: the reconcile validates references against
		// them, and a reload that adds a secret and a proxy using it is one
		// change.
		a.credentials.Swap(next.Credentials)
		if err := a.reconcileFile(ctx, next); err != nil {
			a.credentials.Swap(running.Credentials)
			a.logger.ErrorContext(ctx, "reload failed; keeping the running configuration", "error", err)
			continue
		}
		if running.Desired != nil && next.Desired == nil {
			a.logger.WarnContext(ctx, "the configuration no longer declares repos or lifecycle; config-managed repositories are left as they are")
		}
		running = next
	}
}

// restartSections names the server settings a reload cannot apply in place.
func restartSections(before, after *config.Config) []string {
	var out []string
	if before.Listen != after.Listen {
		out = append(out, "listen")
	}
	if before.Storage != after.Storage {
		out = append(out, "storage")
	}
	if before.Database != after.Database {
		out = append(out, "database")
	}
	if before.Log != after.Log {
		out = append(out, "log")
	}
	return out
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
