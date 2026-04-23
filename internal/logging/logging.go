// Package logging owns the daemon's structured logger. It constructs a
// single *slog.Logger at startup from the validated LoggingConfig and
// propagates it through request-scoped contexts. Callers retrieve the
// logger via FromContext; no component reads slog.Default().
//
// The package also exposes Redact, the one approved way to put a
// sensitive value into a log line: it replaces the value with a fixed
// placeholder plus a short, stable hash prefix that lets operators
// correlate across log lines without recovering the secret.
package logging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/bitsalt/bitblocker/internal/config"
)

// New builds a *slog.Logger that writes to stderr using the handler and
// minimum level implied by cfg. It returns an error if cfg carries a
// value that Validate did not reject (defensive — should never fire in
// practice).
func New(cfg config.LoggingConfig) (*slog.Logger, error) {
	return newWithWriter(cfg, os.Stderr)
}

func newWithWriter(cfg config.LoggingConfig, w io.Writer) (*slog.Logger, error) {
	level, err := slogLevel(cfg.Level)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch cfg.Format {
	case config.LogFormatJSON:
		handler = slog.NewJSONHandler(w, opts)
	case config.LogFormatText:
		handler = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("unsupported log format %q", cfg.Format)
	}

	return slog.New(handler), nil
}

func slogLevel(l config.LogLevel) (slog.Level, error) {
	switch l {
	case config.LogLevelDebug:
		return slog.LevelDebug, nil
	case config.LogLevelInfo:
		return slog.LevelInfo, nil
	case config.LogLevelWarn:
		return slog.LevelWarn, nil
	case config.LogLevelError:
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unsupported log level %q", l)
}

type ctxKey struct{}

// WithContext returns a new context carrying logger. Subsequent
// FromContext calls on derived contexts will return this logger.
func WithContext(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, logger)
}

// FromContext returns the logger attached to ctx, or a discard logger if
// none is present. Returning a working logger rather than nil means no
// caller needs a nil check before emitting a log line.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return discard
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// Redact returns a safe representation of a sensitive string. Empty
// input maps to an empty string (nothing to protect); non-empty input
// maps to "[redacted:xxxxxxxx]" where the 8-char suffix is a stable
// prefix of the SHA-256 digest. The digest is intentionally truncated —
// enough entropy to correlate log lines, too little to brute-force a
// short secret.
func Redact(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return "[redacted:" + hex.EncodeToString(sum[:4]) + "]"
}
