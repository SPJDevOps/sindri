package server

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// LogOptions selects a log level and an output format.
type LogOptions struct {
	Level  string
	Format string
}

// NewLogger builds the process logger.
//
// JSON by default, because the first thing an operator does with a registry's
// logs is ship them somewhere. Text stays available for a terminal.
func NewLogger(w io.Writer, opts LogOptions) (*slog.Logger, error) {
	var level slog.Level
	switch strings.ToLower(opts.Level) {
	case "", "info":
		level = slog.LevelInfo
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("log level %q is not one of debug, info, warn, error", opts.Level)
	}

	handlerOpts := &slog.HandlerOptions{Level: level}

	switch strings.ToLower(opts.Format) {
	case "", "json":
		return slog.New(slog.NewJSONHandler(w, handlerOpts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, handlerOpts)), nil
	default:
		return nil, fmt.Errorf("log format %q is not json or text", opts.Format)
	}
}
