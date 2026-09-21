package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/spj/sindri/internal/storage"
)

// errorBody is the shape every error response takes until M2 defines the one
// error model for the management API. A stable code, a human message, and the
// request id that ties the response to a log line.
type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// healthBody reports liveness and readiness.
type healthBody struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

func (s *Server) router() http.Handler {
	r := chi.NewRouter()

	// Order matters: the request id exists before anything logs, the access
	// log wraps everything a handler does, and the recoverer is inside them
	// both so that a panic is still logged with its request id.
	r.Use(requestID)
	r.Use(accessLog(s.opts.Logger))
	r.Use(recoverer(s.opts.Logger))
	r.Use(s.metrics.observe)

	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)
	r.Method(http.MethodGet, "/metrics", s.metrics.handler())

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(r.Context(), w, http.StatusNotFound, errorBody{
			Code:      "NOT_FOUND",
			Message:   "no route matches this path",
			RequestID: RequestIDFromContext(r.Context()),
		})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(r.Context(), w, http.StatusMethodNotAllowed, errorBody{
			Code:      "METHOD_NOT_ALLOWED",
			Message:   "this route does not accept that method",
			RequestID: RequestIDFromContext(r.Context()),
		})
	})

	return r
}

// handleHealthz reports that the process is alive. It deliberately checks
// nothing: a liveness probe that fails when the database is down gets the
// process killed and restarted for someone else's outage.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(r.Context(), w, http.StatusOK, healthBody{Status: "ok"})
}

// handleReadyz reports whether this instance can serve traffic.
//
// The database is required. The blob store is probed as well when the backend
// offers a cheap check, because a registry whose storage is unreachable can
// answer no useful request either.
//
// Failures name the check but not the error: the detail goes to the log, where
// it is tied to the same request id, rather than to an unauthenticated caller.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	checks := map[string]string{}

	if err := s.opts.Database.Ping(ctx); err != nil {
		LoggerFromContext(ctx).ErrorContext(ctx, "readiness: database is unreachable",
			slog.String("error", err.Error()))
		checks["database"] = "unreachable"
	} else {
		checks["database"] = "ok"
	}

	if prober, ok := s.opts.Blobs.(storage.Prober); ok && prober != nil {
		if err := prober.Probe(ctx); err != nil {
			LoggerFromContext(ctx).ErrorContext(ctx, "readiness: blob storage is unreachable",
				slog.String("error", err.Error()))
			checks["storage"] = "unreachable"
		} else {
			checks["storage"] = "ok"
		}
	}

	status := http.StatusOK
	body := healthBody{Status: "ready", Checks: checks}
	for _, result := range checks {
		if result != "ok" {
			status = http.StatusServiceUnavailable
			body.Status = "unready"
			break
		}
	}

	writeJSON(ctx, w, status, body)
}

// writeJSON writes a response body, logging rather than panicking if the
// connection is already gone.
func writeJSON(ctx context.Context, w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		LoggerFromContext(ctx).WarnContext(ctx, "writing a response body",
			slog.String("error", err.Error()))
	}
}
