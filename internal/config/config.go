package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// EnvMaxMindLicenseKey is the environment variable that, when set, overrides
// sources.maxmind.license_key. Secrets belong in the environment, not the
// config file that lives on disk.
const EnvMaxMindLicenseKey = "MAXMIND_LICENSE_KEY"

// StartupMode controls how the daemon behaves when cold-started before the
// first blocklist has loaded.
type StartupMode string

const (
	StartupFailClosed StartupMode = "fail-closed"
	StartupFailOpen   StartupMode = "fail-open"
)

// LogFormat is the structured-log output format.
type LogFormat string

const (
	LogFormatJSON LogFormat = "json"
	LogFormatText LogFormat = "text"
)

// LogLevel is the minimum log level emitted.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// CountryCode is an ISO 3166-1 alpha-2 country code.
type CountryCode string

// ASN is a 32-bit autonomous system number.
type ASN uint32

// Config is the fully validated runtime configuration. It is the single
// source of truth; no other package reads the environment or parses YAML.
type Config struct {
	Listen   ListenConfig   `yaml:"listen"`
	Block    BlockConfig    `yaml:"block"`
	Sources  SourcesConfig  `yaml:"sources"`
	Refresh  RefreshConfig  `yaml:"refresh"`
	Behavior BehaviorConfig `yaml:"behavior"`
	Logging  LoggingConfig  `yaml:"logging"`
}

type ListenConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type BlockConfig struct {
	Countries []CountryCode `yaml:"countries"`
	ASNs      []ASN         `yaml:"asns"`
}

type SourcesConfig struct {
	MaxMind  MaxMindConfig  `yaml:"maxmind"`
	BGPTools BGPToolsConfig `yaml:"bgptools"`
}

type MaxMindConfig struct {
	Enabled    bool   `yaml:"enabled"`
	LicenseKey string `yaml:"license_key"`
	Edition    string `yaml:"edition"`
}

type BGPToolsConfig struct {
	Enabled bool `yaml:"enabled"`
}

type RefreshConfig struct {
	Schedule string        `yaml:"schedule"`
	Timeout  time.Duration `yaml:"timeout"`
}

type BehaviorConfig struct {
	LogBlocked   bool        `yaml:"log_blocked"`
	LogAllowed   bool        `yaml:"log_allowed"`
	ResponseCode int         `yaml:"response_code"`
	StartupMode  StartupMode `yaml:"startup_mode"`
}

type LoggingConfig struct {
	Level  LogLevel  `yaml:"level"`
	Format LogFormat `yaml:"format"`
}

// Load reads and validates the YAML config at path, applies environment
// overrides for secrets, and returns the validated Config. Any failure —
// unreadable file, malformed YAML, missing required secret, invalid value —
// surfaces as an error; callers must fail startup on a non-nil result.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}

	cfg := defaults()
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	applyEnvOverrides(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config %q: %w", path, err)
	}

	return cfg, nil
}

// defaults returns a Config prefilled with values that apply when the user
// has omitted a field but a safe default exists. Required fields intentionally
// remain zero so Validate catches their absence.
func defaults() *Config {
	return &Config{
		Listen: ListenConfig{
			Host: "127.0.0.1",
			Port: 8080,
		},
		Refresh: RefreshConfig{
			Schedule: "0 3 * * *",
			Timeout:  30 * time.Second,
		},
		Behavior: BehaviorConfig{
			LogBlocked:   true,
			LogAllowed:   false,
			ResponseCode: 403,
			StartupMode:  StartupFailClosed,
		},
		Logging: LoggingConfig{
			Level:  LogLevelInfo,
			Format: LogFormatJSON,
		},
	}
}

func applyEnvOverrides(cfg *Config) {
	if v, ok := os.LookupEnv(EnvMaxMindLicenseKey); ok {
		cfg.Sources.MaxMind.LicenseKey = v
	}
}

var countryCodePattern = regexp.MustCompile(`^[A-Z]{2}$`)

// Validate checks that every field holds a value the daemon can actually use.
// It returns a joined error describing every problem found, not just the
// first — easier on operators iterating on a broken config.
func (c *Config) Validate() error {
	var errs []error

	if c.Listen.Port < 1 || c.Listen.Port > 65535 {
		errs = append(errs, fmt.Errorf("listen.port must be 1-65535, got %d", c.Listen.Port))
	}
	if c.Listen.Host == "" {
		errs = append(errs, errors.New("listen.host must not be empty"))
	}

	if len(c.Block.Countries) == 0 && len(c.Block.ASNs) == 0 {
		errs = append(errs, errors.New("block must list at least one country or ASN"))
	}
	for i, cc := range c.Block.Countries {
		if !countryCodePattern.MatchString(string(cc)) {
			errs = append(errs, fmt.Errorf("block.countries[%d]=%q is not an ISO 3166-1 alpha-2 code", i, cc))
		}
	}

	if c.Sources.MaxMind.Enabled {
		if c.Sources.MaxMind.LicenseKey == "" {
			errs = append(errs, fmt.Errorf("sources.maxmind.license_key is required when maxmind is enabled (set %s to inject at runtime)", EnvMaxMindLicenseKey))
		}
		if c.Sources.MaxMind.Edition == "" {
			errs = append(errs, errors.New("sources.maxmind.edition is required when maxmind is enabled"))
		}
	}
	if !c.Sources.MaxMind.Enabled && !c.Sources.BGPTools.Enabled {
		errs = append(errs, errors.New("at least one source (maxmind, bgptools) must be enabled"))
	}

	if c.Refresh.Schedule == "" {
		errs = append(errs, errors.New("refresh.schedule must not be empty"))
	}
	if c.Refresh.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("refresh.timeout must be positive, got %s", c.Refresh.Timeout))
	}

	if !isValidStartupMode(c.Behavior.StartupMode) {
		errs = append(errs, fmt.Errorf("behavior.startup_mode must be %q or %q, got %q", StartupFailClosed, StartupFailOpen, c.Behavior.StartupMode))
	}
	if c.Behavior.ResponseCode < 400 || c.Behavior.ResponseCode > 599 {
		errs = append(errs, fmt.Errorf("behavior.response_code must be a 4xx or 5xx status, got %d", c.Behavior.ResponseCode))
	}

	if !isValidLogLevel(c.Logging.Level) {
		errs = append(errs, fmt.Errorf("logging.level must be debug|info|warn|error, got %q", c.Logging.Level))
	}
	if !isValidLogFormat(c.Logging.Format) {
		errs = append(errs, fmt.Errorf("logging.format must be json|text, got %q", c.Logging.Format))
	}

	return errors.Join(errs...)
}

func isValidStartupMode(m StartupMode) bool {
	return m == StartupFailClosed || m == StartupFailOpen
}

func isValidLogLevel(l LogLevel) bool {
	switch l {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
		return true
	}
	return false
}

func isValidLogFormat(f LogFormat) bool {
	return f == LogFormatJSON || f == LogFormatText
}
