package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bitsalt/bitblocker/internal/config"
	"github.com/bitsalt/bitblocker/internal/logging"
)

func newTestLogger(t *testing.T, cfg config.LoggingConfig) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	logger, err := logging.NewForTest(cfg, buf)
	require.NoError(t, err)
	return logger, buf
}

func TestNew_JSONHandlerEmitsParsableLines(t *testing.T) {
	logger, buf := newTestLogger(t, config.LoggingConfig{
		Level:  config.LogLevelInfo,
		Format: config.LogFormatJSON,
	})
	logger.Info("hello", "k", "v")

	var rec map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec))
	require.Equal(t, "hello", rec["msg"])
	require.Equal(t, "v", rec["k"])
	require.Equal(t, "INFO", rec["level"])
}

func TestNew_TextHandler(t *testing.T) {
	logger, buf := newTestLogger(t, config.LoggingConfig{
		Level:  config.LogLevelInfo,
		Format: config.LogFormatText,
	})
	logger.Info("hello", "k", "v")

	out := buf.String()
	require.Contains(t, out, "level=INFO")
	require.Contains(t, out, "msg=hello")
	require.Contains(t, out, "k=v")
}

func TestNew_LevelFiltering(t *testing.T) {
	cases := []struct {
		name        string
		level       config.LogLevel
		wantDebug   bool
		wantInfo    bool
		wantWarn    bool
		wantError   bool
		description string
	}{
		{"debug passes all", config.LogLevelDebug, true, true, true, true, ""},
		{"info drops debug", config.LogLevelInfo, false, true, true, true, ""},
		{"warn drops info", config.LogLevelWarn, false, false, true, true, ""},
		{"error drops warn", config.LogLevelError, false, false, false, true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := newTestLogger(t, config.LoggingConfig{
				Level:  tc.level,
				Format: config.LogFormatJSON,
			})
			logger.Debug("d")
			logger.Info("i")
			logger.Warn("w")
			logger.Error("e")

			out := buf.String()
			require.Equal(t, tc.wantDebug, strings.Contains(out, `"msg":"d"`))
			require.Equal(t, tc.wantInfo, strings.Contains(out, `"msg":"i"`))
			require.Equal(t, tc.wantWarn, strings.Contains(out, `"msg":"w"`))
			require.Equal(t, tc.wantError, strings.Contains(out, `"msg":"e"`))
		})
	}
}

func TestNew_UnsupportedLevel(t *testing.T) {
	_, err := logging.NewForTest(config.LoggingConfig{
		Level:  "verbose",
		Format: config.LogFormatJSON,
	}, io.Discard)
	require.Error(t, err)
	require.Contains(t, err.Error(), "verbose")
}

func TestNew_UnsupportedFormat(t *testing.T) {
	_, err := logging.NewForTest(config.LoggingConfig{
		Level:  config.LogLevelInfo,
		Format: "xml",
	}, io.Discard)
	require.Error(t, err)
	require.Contains(t, err.Error(), "xml")
}

func TestContext_RoundTrip(t *testing.T) {
	logger, _ := newTestLogger(t, config.LoggingConfig{
		Level:  config.LogLevelInfo,
		Format: config.LogFormatJSON,
	})
	ctx := logging.WithContext(context.Background(), logger)

	require.Same(t, logger, logging.FromContext(ctx))
}

func TestContext_AbsentReturnsDiscardLogger(t *testing.T) {
	// Should return a usable logger (never nil) even when nothing is attached.
	got := logging.FromContext(context.Background())
	require.NotNil(t, got)
	// Emitting through the fallback logger must not panic and must not
	// write anywhere observable — we cannot intercept slog's discard
	// directly, but this exercises the code path.
	got.Info("should be discarded")
}

func TestRedact(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"short secret", "abc", "[redacted:"},
		{"realistic license key", "YmFuYW5hcy1hcmUtZ3JlYXQ", "[redacted:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := logging.Redact(tc.in)
			if tc.in == "" {
				require.Equal(t, "", got)
				return
			}
			require.True(t, strings.HasPrefix(got, tc.want))
			require.True(t, strings.HasSuffix(got, "]"))
			require.NotContains(t, got, tc.in, "redacted value must not echo the secret")
		})
	}
}

func TestRedact_Stable(t *testing.T) {
	// Same input must produce the same placeholder so operators can
	// correlate log lines referring to the same secret.
	require.Equal(t, logging.Redact("same-key"), logging.Redact("same-key"))
	require.NotEqual(t, logging.Redact("key-a"), logging.Redact("key-b"))
}
