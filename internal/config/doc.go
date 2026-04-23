// Package config loads and validates the BitBlocker YAML configuration.
// It exposes a single typed Config struct that every other component
// consumes; no other package reads environment variables or parses
// configuration directly.
//
// Load resolves the config file path, applies environment-variable
// overrides (notably the MaxMind license key), and runs Validate before
// returning. Missing required secrets or malformed values fail loudly at
// startup rather than at first use.
package config
