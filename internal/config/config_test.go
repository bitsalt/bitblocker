package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bitsalt/bitblocker/internal/config"
)

const validYAML = `
listen:
  host: "127.0.0.1"
  port: 8080
block:
  countries: [CN, RU]
  asns: [4134, 4837]
sources:
  maxmind:
    enabled: true
    license_key: "secret-key"
    edition: "GeoLite2-Country"
  bgptools:
    enabled: false
refresh:
  schedule: "0 3 * * *"
  timeout: 30s
behavior:
  log_blocked: true
  log_allowed: false
  response_code: 403
  startup_mode: "fail-closed"
logging:
  level: "info"
  format: "json"
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestLoad_Valid(t *testing.T) {
	path := writeConfig(t, validYAML)
	cfg, err := config.Load(path)
	require.NoError(t, err)

	require.Equal(t, "127.0.0.1", cfg.Listen.Host)
	require.Equal(t, 8080, cfg.Listen.Port)
	require.Equal(t, []config.CountryCode{"CN", "RU"}, cfg.Block.Countries)
	require.Equal(t, []config.ASN{4134, 4837}, cfg.Block.ASNs)
	require.True(t, cfg.Sources.MaxMind.Enabled)
	require.Equal(t, "secret-key", cfg.Sources.MaxMind.LicenseKey)
	require.Equal(t, 30*time.Second, cfg.Refresh.Timeout)
	require.Equal(t, config.StartupFailClosed, cfg.Behavior.StartupMode)
}

func TestLoad_AppliesDefaults(t *testing.T) {
	// Minimal config: omit every field that has a safe default.
	minimal := `
block:
  countries: [CN]
sources:
  maxmind:
    enabled: true
    license_key: "k"
    edition: "GeoLite2-Country"
`
	path := writeConfig(t, minimal)
	cfg, err := config.Load(path)
	require.NoError(t, err)

	require.Equal(t, "127.0.0.1", cfg.Listen.Host)
	require.Equal(t, 8080, cfg.Listen.Port)
	require.Equal(t, "0 3 * * *", cfg.Refresh.Schedule)
	require.Equal(t, 30*time.Second, cfg.Refresh.Timeout)
	require.Equal(t, 403, cfg.Behavior.ResponseCode)
	require.Equal(t, config.StartupFailClosed, cfg.Behavior.StartupMode)
	require.Equal(t, config.LogLevelInfo, cfg.Logging.Level)
	require.Equal(t, config.LogFormatJSON, cfg.Logging.Format)
	require.Equal(t, "/var/cache/bitblocker/GeoLite2-Country.mmdb", cfg.Cache.Path)
	require.Equal(t, 48*time.Hour, cfg.Cache.MaxAge)
}

func TestLoad_CacheBlockOverridesDefaults(t *testing.T) {
	body := validYAML + `
cache:
  path: "/tmp/bitblocker/cache.mmdb"
  max_age: 12h
`
	path := writeConfig(t, body)
	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.Equal(t, "/tmp/bitblocker/cache.mmdb", cfg.Cache.Path)
	require.Equal(t, 12*time.Hour, cfg.Cache.MaxAge)
}

func TestLoad_EnvOverridesLicenseKey(t *testing.T) {
	t.Setenv(config.EnvMaxMindLicenseKey, "env-key")
	path := writeConfig(t, validYAML)

	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.Equal(t, "env-key", cfg.Sources.MaxMind.LicenseKey)
}

func TestLoad_EnvSuppliesMissingLicenseKey(t *testing.T) {
	// Config file has empty license_key; env var provides it.
	body := strings.Replace(validYAML, `license_key: "secret-key"`, `license_key: ""`, 1)
	t.Setenv(config.EnvMaxMindLicenseKey, "env-key")
	path := writeConfig(t, body)

	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.Equal(t, "env-key", cfg.Sources.MaxMind.LicenseKey)
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "nope.yaml"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "read config file")
}

func TestLoad_RejectsUnknownFields(t *testing.T) {
	body := validYAML + "\nunknown_top_level: 1\n"
	path := writeConfig(t, body)

	_, err := config.Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse config")
}

func TestValidate_Invalid(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(s string) string
		wantSub string
	}{
		{
			name:    "port out of range",
			mutate:  func(s string) string { return strings.Replace(s, "port: 8080", "port: 70000", 1) },
			wantSub: "listen.port",
		},
		{
			name: "no countries or asns",
			mutate: func(s string) string {
				s = strings.Replace(s, "countries: [CN, RU]", "countries: []", 1)
				return strings.Replace(s, "asns: [4134, 4837]", "asns: []", 1)
			},
			wantSub: "block must list at least one",
		},
		{
			name:    "malformed country code",
			mutate:  func(s string) string { return strings.Replace(s, "[CN, RU]", "[CN, russia]", 1) },
			wantSub: "ISO 3166-1",
		},
		{
			name: "maxmind enabled but no license key and no env",
			mutate: func(s string) string {
				return strings.Replace(s, `license_key: "secret-key"`, `license_key: ""`, 1)
			},
			wantSub: "license_key is required",
		},
		{
			name: "maxmind enabled but no edition",
			mutate: func(s string) string {
				return strings.Replace(s, `edition: "GeoLite2-Country"`, `edition: ""`, 1)
			},
			wantSub: "edition is required",
		},
		{
			name: "no sources enabled",
			mutate: func(s string) string {
				// bgptools.enabled is already false in the base fixture;
				// flipping maxmind leaves both disabled.
				return strings.Replace(s, "maxmind:\n    enabled: true", "maxmind:\n    enabled: false", 1)
			},
			wantSub: "at least one source",
		},
		{
			name:    "zero timeout",
			mutate:  func(s string) string { return strings.Replace(s, "timeout: 30s", "timeout: 0s", 1) },
			wantSub: "refresh.timeout must be positive",
		},
		{
			name:    "bad startup_mode",
			mutate:  func(s string) string { return strings.Replace(s, `"fail-closed"`, `"whatever"`, 1) },
			wantSub: "startup_mode",
		},
		{
			name:    "response_code below 400",
			mutate:  func(s string) string { return strings.Replace(s, "response_code: 403", "response_code: 200", 1) },
			wantSub: "response_code",
		},
		{
			name:    "bad log level",
			mutate:  func(s string) string { return strings.Replace(s, `level: "info"`, `level: "verbose"`, 1) },
			wantSub: "logging.level",
		},
		{
			name:    "bad log format",
			mutate:  func(s string) string { return strings.Replace(s, `format: "json"`, `format: "xml"`, 1) },
			wantSub: "logging.format",
		},
		{
			name: "empty cache path",
			mutate: func(s string) string {
				return s + "\ncache:\n  path: \"\"\n  max_age: 48h\n"
			},
			wantSub: "cache.path must not be empty",
		},
		{
			name: "zero cache max_age",
			mutate: func(s string) string {
				return s + "\ncache:\n  path: \"/var/cache/bitblocker/c.mmdb\"\n  max_age: 0s\n"
			},
			wantSub: "cache.max_age must be positive",
		},
		{
			name: "negative cache max_age",
			mutate: func(s string) string {
				return s + "\ncache:\n  path: \"/var/cache/bitblocker/c.mmdb\"\n  max_age: -1h\n"
			},
			wantSub: "cache.max_age must be positive",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Ensure no stray env var from another test leaks in.
			t.Setenv(config.EnvMaxMindLicenseKey, "")
			os.Unsetenv(config.EnvMaxMindLicenseKey)

			path := writeConfig(t, tc.mutate(validYAML))
			_, err := config.Load(path)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

func TestValidate_ReportsAllErrors(t *testing.T) {
	body := strings.NewReplacer(
		"port: 8080", "port: 0",
		`level: "info"`, `level: "verbose"`,
		`format: "json"`, `format: "xml"`,
	).Replace(validYAML)

	path := writeConfig(t, body)
	_, err := config.Load(path)
	require.Error(t, err)
	msg := err.Error()
	require.Contains(t, msg, "listen.port")
	require.Contains(t, msg, "logging.level")
	require.Contains(t, msg, "logging.format")
}
