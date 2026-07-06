// Package config loads and validates the BitBlocker YAML configuration.
// It exposes a single typed Config struct that every other component
// consumes; no other package reads environment variables or parses
// configuration directly.
//
// Load resolves the config file path, decodes the YAML, and runs
// Validate before returning. Malformed values — an invalid cron
// schedule, an out-of-range port, no enabled source — fail loudly at
// startup rather than at first use.
package config
