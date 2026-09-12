// Package logging provides a single, process-wide structured logger built
// on the standard library's log/slog, so every component in the control
// plane and data plane emits consistent, machine-parseable log records
// instead of ad-hoc fmt-style lines.
//
// Configuration is intentionally tiny and mirrors the rest of the project:
// two settings, each available as a CLI flag or the matching LB_* env var.
//
//   - LB_LOG_LEVEL  — debug | info | warn | error   (default: info)
//   - LB_LOG_FORMAT — json | text                    (default: text)
//
// text is the default so local development stays human-readable; json is
// the format to select in production for log aggregation. The same two
// settings also govern the per-request access log (see the dataplane
// access-log middleware), so an operator picks a format once and both the
// application log and the access log agree.
//
// Setup installs the built logger as slog's default, so any code that
// calls slog.Info/slog.Error directly (including third-party libraries
// that adopt slog) flows through the same handler. Components should take
// a *slog.Logger and tag themselves with Component so every record says
// where it came from, e.g. logging.Component(base, "dataplane").
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Config selects the logger's minimum level and output format. The zero
// value (empty strings) is valid and resolves to the documented defaults
// (info / text), so callers can pass a partially-filled Config safely.
type Config struct {
	// Level is one of "debug", "info", "warn", "error" (case-insensitive).
	// An empty or unrecognised value falls back to "info".
	Level string
	// Format is "json" or "text" (case-insensitive). An empty or
	// unrecognised value falls back to "text".
	Format string
}

// ParseLevel maps a level string to a slog.Level, defaulting to
// slog.LevelInfo for empty or unrecognised input. Exposed so config
// validation can report an unknown level distinctly from silently
// defaulting it.
func ParseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, true
	case "debug":
		return slog.LevelDebug, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// NewLogger builds a *slog.Logger writing to w with the given config. It
// does not install the logger as the default; use Setup for that. w is
// typically os.Stderr for application logs. A nil w defaults to
// os.Stderr.
func NewLogger(w io.Writer, cfg Config) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	level, _ := ParseLevel(cfg.Level)
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if strings.EqualFold(strings.TrimSpace(cfg.Format), "json") {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler)
}

// Setup builds the application logger, installs it as slog's default (so
// bare slog.Info/slog.Error calls and slog-aware libraries route through
// it), and returns it for callers that want to attach component fields.
// Application logs go to stderr, keeping stdout free for anything that
// legitimately belongs there.
func Setup(cfg Config) *slog.Logger {
	logger := NewLogger(os.Stderr, cfg)
	slog.SetDefault(logger)
	return logger
}

// Component returns a child logger tagged with a "component" field, so
// every record it emits identifies its origin (e.g. "controlplane",
// "dataplane", "healthcheck"). A nil base falls back to the current slog
// default.
func Component(base *slog.Logger, name string) *slog.Logger {
	if base == nil {
		base = slog.Default()
	}
	return base.With(slog.String("component", name))
}

// contextKey is unexported so only this package can key the context with
// a request-scoped logger.
type contextKey struct{}

var loggerKey contextKey

// WithContext returns a copy of ctx carrying logger, so downstream code
// that has only a context.Context can recover a logger already tagged
// with request-scoped fields (e.g. a request id). This is how the data
// plane threads the X-Request-Id into every log line emitted while
// serving a single request.
func WithContext(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, logger)
}

// FromContext returns the logger stored in ctx by WithContext, or the
// slog default if none was set (so it is always safe to call). Callers
// use this to emit a log line that is automatically correlated to the
// request being served.
func FromContext(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok && l != nil {
			return l
		}
	}
	return slog.Default()
}
