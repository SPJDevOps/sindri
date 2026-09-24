// Package server assembles Sindri's HTTP surface: the router, the middleware
// chain, and the operational endpoints every deployment needs.
//
// The management API (/api/v1) and OCI adapter (/v2) mount onto this router
// via Options. Protocol adapters are not part of this package, so that "what
// every request passes through" stays readable in one place.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/storage"
)

// Options describes a server.
type Options struct {
	// Address is the listen address, for example ":8080".
	Address string

	// ReadHeaderTimeout bounds how long a client may take to send its request
	// headers. A registry is a public endpoint; an unbounded header read is a
	// free connection exhaustion attack.
	ReadHeaderTimeout time.Duration

	// ShutdownTimeout bounds how long in-flight requests have to finish after
	// a SIGTERM. Layer pushes are long, so this is generous by default.
	ShutdownTimeout time.Duration

	// Database is what readiness pings. Required.
	Database db.Pinger

	// Blobs is probed by readiness when the backend supports it.
	Blobs storage.BlobStore

	// API is the management surface mounted at /api/v1. Optional so unit tests
	// of the ops endpoints need not wire a registry.
	API http.Handler

	// OCI is the distribution-spec surface mounted at /v2. Optional for the
	// same reason as API.
	OCI http.Handler

	// Logger receives access logs and server errors. Required.
	Logger *slog.Logger

	// Metrics is the registry the /metrics endpoint serves. A nil registry
	// gets a fresh one, so two servers in one test process do not collide on
	// the global default registry.
	Metrics *prometheus.Registry
}

// Server owns an HTTP listener and the handler behind it.
type Server struct {
	opts    Options
	http    *http.Server
	metrics *metrics
	addr    string
}

// New validates opts and builds the handler.
func New(opts Options) (*Server, error) {
	if opts.Address == "" {
		return nil, errors.New("server: listen address is required")
	}
	if opts.Logger == nil {
		return nil, errors.New("server: logger is required")
	}
	if opts.Database == nil {
		return nil, errors.New("server: a database is required for the readiness check")
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = 20 * time.Second
	}
	if opts.ReadHeaderTimeout <= 0 {
		opts.ReadHeaderTimeout = 10 * time.Second
	}

	registry := opts.Metrics
	if registry == nil {
		registry = prometheus.NewRegistry()
	}
	m, err := newMetrics(registry)
	if err != nil {
		return nil, err
	}

	s := &Server{opts: opts, metrics: m, addr: opts.Address}
	s.http = &http.Server{
		Handler:           s.router(),
		ReadHeaderTimeout: opts.ReadHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(opts.Logger.Handler(), slog.LevelWarn),
	}
	return s, nil
}

// Handler exposes the router, so that tests can exercise it without a socket.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Addr is the address the server is listening on. After Serve it reflects the
// port the kernel chose, which is what a test listening on :0 needs.
func (s *Server) Addr() string { return s.addr }

// Listen binds the socket. It is separate from Serve so that a caller can
// learn the bound address, and so that a port already in use fails startup
// rather than surfacing later.
func (s *Server) Listen(ctx context.Context) (net.Listener, error) {
	var lc net.ListenConfig

	listener, err := lc.Listen(ctx, "tcp", s.opts.Address)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", s.opts.Address, err)
	}
	s.addr = listener.Addr().String()

	return listener, nil
}

// Serve handles requests until ctx is cancelled, then drains in-flight
// requests within the shutdown timeout.
//
// This is the SIGTERM path: an orchestrator stopping the process must not cut
// a layer upload in half, and a client must not see a connection reset where a
// clean response was possible.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	serveErr := make(chan error, 1)
	go func() {
		err := s.http.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	s.opts.Logger.InfoContext(ctx, "listening", slog.String("address", s.addr))

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serving HTTP: %w", err)
		}
		return nil

	case <-ctx.Done():
		s.opts.Logger.InfoContext(ctx, "shutting down",
			slog.Duration("timeout", s.opts.ShutdownTimeout))

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.ShutdownTimeout)
		defer cancel()

		if err := s.http.Shutdown(shutdownCtx); err != nil {
			// Closing outright is worse than a clean drain but better than
			// hanging: the caller is already on its way out.
			_ = s.http.Close()
			return fmt.Errorf("shutting down: %w", err)
		}
		return <-serveErr
	}
}

// ListenAndServe binds and serves in one call.
func (s *Server) ListenAndServe(ctx context.Context) error {
	listener, err := s.Listen(ctx)
	if err != nil {
		return err
	}
	return s.Serve(ctx, listener)
}
