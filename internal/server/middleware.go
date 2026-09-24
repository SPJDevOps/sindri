package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
)

// RequestIDHeader carries the request id in and out.
const RequestIDHeader = "X-Request-Id"

type contextKey int

const (
	requestIDKey contextKey = iota
	loggerKey
)

// maxInboundRequestIDLen bounds an id we accept from a client.
const maxInboundRequestIDLen = 64

// requestID puts a request id on every request, echoes it in the response, and
// attaches it to the request's logger.
//
// An inbound id is adopted only if it is short and alphanumeric. A request id
// is written to logs, and a log line is read by humans and by log processors:
// accepting arbitrary client text there is log injection.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(RequestIDHeader))
		if id == "" {
			id = newRequestID()
		}

		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func sanitizeRequestID(id string) string {
	if id == "" || len(id) > maxInboundRequestIDLen {
		return ""
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return ""
		}
	}
	return id
}

func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// A request id is diagnostic, never a security token, so a clock
		// based fallback is better than failing the request.
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b)
}

// RequestIDFromContext returns the request id, or "" outside a request.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// LoggerFromContext returns the request scoped logger, falling back to a
// logger that discards nothing but carries no request context.
func LoggerFromContext(ctx context.Context) *slog.Logger {
	if log, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return log
	}
	return slog.Default()
}

// accessLog logs one line per request and makes a request scoped logger
// available to handlers.
//
// It logs the method, the route pattern, the status, the duration, and the
// request id. It deliberately logs neither the query string nor any header:
// tokens arrive in headers and in query strings, and a registry's logs are
// exactly where a leaked credential would be most useful to an attacker. See
// CLAUDE.md's fourth security rule.
func accessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			entry := log.With(
				slog.String("request_id", RequestIDFromContext(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)
			ctx := context.WithValue(r.Context(), loggerKey, entry)

			wrapped := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(wrapped, r.WithContext(ctx))

			entry.LogAttrs(ctx, statusLevel(wrapped.Status()), "request",
				slog.Int("status", wrapped.Status()),
				slog.Int("bytes", wrapped.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
				slog.String("route", routePattern(r)),
			)
		})
	}
}

func statusLevel(status int) slog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	case status >= http.StatusBadRequest:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// recoverer turns a panic into a 500 rather than a dropped connection, and
// logs it at error level with the request id so it can be traced.
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recovered := recover(); recovered != nil {
					LoggerFromContext(r.Context()).ErrorContext(r.Context(), "panic serving request",
						slog.Any("panic", recovered),
						slog.String("route", routePattern(r)),
					)
					writeJSON(r.Context(), w, http.StatusInternalServerError, ErrorBody{
						Code:      "INTERNAL",
						Message:   "the server failed to handle this request",
						RequestID: RequestIDFromContext(r.Context()),
					})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// observe records Prometheus counters and a latency histogram.
func (m *metrics) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := chimw.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(wrapped, r)

		m.record(r.Method, routePattern(r), wrapped.Status(), time.Since(start))
	})
}

// routePattern returns chi's matched pattern, never the raw path.
//
// An OCI repository name is unbounded, so labelling metrics with r.URL.Path
// would let any client mint new time series at will. An unmatched request is
// labelled "unmatched" for the same reason.
func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if pattern := rctx.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return "unmatched"
}
