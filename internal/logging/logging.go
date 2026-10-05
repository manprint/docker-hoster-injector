// Package logging builds the structured logger used across the agent.
//
// Logs go to stderr so that stdout stays free for anything a future version
// might want to emit, and so that "docker logs" interleaves correctly with
// container stdout.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/manprint/docker-hoster-injector/internal/config"
)

// Options tweak the logger beyond what the config carries.
type Options struct {
	// Service is the value of the "service" log attribute.
	Service string
	// AddSource attaches file:line, which is useful at debug level.
	AddSource bool
	// Writer defaults to os.Stderr.
	Writer io.Writer
}

// New builds a logger honouring the configured level and format.
func New(cfg config.Config, opts Options) *slog.Logger {
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}

	handlerOpts := &slog.HandlerOptions{
		Level:       cfg.Level,
		AddSource:   opts.AddSource || cfg.Level == slog.LevelDebug,
		ReplaceAttr: redact,
	}

	var h slog.Handler
	if cfg.Format == config.LogFormatText {
		h = slog.NewTextHandler(w, handlerOpts)
	} else {
		h = slog.NewJSONHandler(w, handlerOpts)
	}

	logger := slog.New(h)
	if opts.Service != "" {
		logger = logger.With("service", opts.Service)
	}
	return logger
}

// redact drops noisy or sensitive attributes from every record.
//
// Only the keys the agent actually handles are listed; anything unexpected is
// left alone so a new field cannot silently vanish from the logs.
func redact(_ []string, a slog.Attr) slog.Attr {
	switch strings.ToLower(a.Key) {
	case "docker.host", "docker_host", "authorization", "token", "password":
		return slog.String(a.Key, "[redacted]")
	default:
		return a
	}
}

// Discard returns a logger that throws everything away, for tests.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// IntoContext stores a logger in a context, the way the rest of the packages
// expect to receive one.
func IntoContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// FromContext retrieves the logger, falling back to a discarding one so that
// no call site has to nil-check.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return Discard()
}

type loggerKey struct{}
