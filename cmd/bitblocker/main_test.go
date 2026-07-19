package main

import (
	"bytes"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bitsalt/bitblocker/internal/blocklist"
	"github.com/bitsalt/bitblocker/internal/config"
	"github.com/bitsalt/bitblocker/internal/server"
)

// TestConfigExampleIsValid guards config.example.yaml against schema
// drift: the shipped example must load and validate against the live
// config schema, so an operator copying it gets a working daemon. It
// also pins the ADR 0003 source-surface change (dbip, no maxmind).
func TestConfigExampleIsValid(t *testing.T) {
	cfg, err := config.Load("../../config.example.yaml")
	require.NoError(t, err)
	require.True(t, cfg.Sources.DBIP.Enabled)
	require.Equal(t, "/var/cache/bitblocker/dbip-country-lite.mmdb", cfg.Cache.Path)
}

// TestNewLookupSource_ColdStartReturnsUntypedNil is the mandated
// regression guard for the nil-interface trap (ADR 0001, interface
// spec §2.5). When no trie is loaded the closure must return an
// *untyped* nil — NOT a server.Lookup wrapping a (*blocklist.Trie)(nil).
//
// A non-nil interface holding a nil pointer would pass the server's
// `lookup == nil` guard and then panic on lookup.Len(). `lookup == nil`
// below verifies the interface value itself is nil, which is exactly
// the property the server depends on.
func TestNewLookupSource_ColdStartReturnsUntypedNil(t *testing.T) {
	src := blocklist.NewSource()
	lookupSource := newLookupSource(src)

	lookup := lookupSource()

	require.Nil(t, lookup, "cold-start closure must return a nil interface value")
	require.True(t, lookup == nil, "interface comparison must see an untyped nil, not a wrapped nil pointer")

	// The server's actual guard: `lookup == nil || lookup.Len() == 0`.
	// With a wrapped nil pointer the first clause is false and Len()
	// dereferences nil. This must not happen.
	require.NotPanics(t, func() {
		if lookup == nil || lookup.Len() == 0 {
			_ = "fail closed"
		}
	}, "the server's lookup guard must not panic on the cold-start return")
}

func TestNewLookupSource_AfterSwapReturnsLiveTrie(t *testing.T) {
	src := blocklist.NewSource()
	tr := blocklist.New()
	tr.Insert(netip.MustParsePrefix("10.0.0.0/24"))
	src.Swap(tr)

	lookup := newLookupSource(src)()

	require.NotNil(t, lookup)
	require.Equal(t, 1, lookup.Len())
	require.True(t, lookup.Contains(netip.MustParseAddr("10.0.0.5")))
}

func TestNewLookupSource_SwapBackToNilReturnsUntypedNil(t *testing.T) {
	src := blocklist.NewSource()
	tr := blocklist.New()
	tr.Insert(netip.MustParsePrefix("10.0.0.0/24"))
	src.Swap(tr)
	src.Swap(nil)

	lookup := newLookupSource(src)()
	require.True(t, lookup == nil, "after Swap(nil) the closure must again return an untyped nil")
}

// cacheTestConfig builds the minimal config loadDiskCache reads: the
// cache path + max age and the configured country set.
func cacheTestConfig(path string, maxAge time.Duration) *config.Config {
	return &config.Config{
		Block: config.BlockConfig{Countries: []config.CountryCode{"CN"}},
		Cache: config.CacheConfig{Path: path, MaxAge: maxAge},
	}
}

// TestLoadDiskCache_RemovesUnusableCache is the OQ-CACHE-2 regression
// guard: a cache file that fails to load at startup — because it is
// corrupt or stale — must be removed so it does not re-trip the load
// attempt (and its WARN) on the next start. In every case the daemon
// stays fail-closed: the source is never swapped, so no trie is served.
func TestLoadDiskCache_RemovesUnusableCache(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, path string)
		maxAge     time.Duration
		wantWarn   string // substring the WARN output must contain
		wantExists bool   // whether the path should survive the call
	}{
		{
			name: "corrupt file is removed",
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.WriteFile(path, []byte("not an mmdb file"), 0o600))
			},
			maxAge:     time.Hour,
			wantWarn:   "unreadable",
			wantExists: false,
		},
		{
			name: "stale file is removed",
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.WriteFile(path, []byte("stale bytes"), 0o600))
				// Backdate past maxAge; staleness is decided by ModTime
				// before the MMDB is ever parsed, so the contents are
				// irrelevant here.
				old := time.Now().Add(-2 * time.Hour)
				require.NoError(t, os.Chtimes(path, old, old))
			},
			maxAge:     time.Hour,
			wantWarn:   "stale",
			wantExists: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dbip-country-lite.mmdb")
			tc.setup(t, path)

			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			src := blocklist.NewSource()

			loadDiskCache(logger, src, cacheTestConfig(path, tc.maxAge))

			require.Contains(t, buf.String(), tc.wantWarn)
			require.Nil(t, src.Current(), "fail-closed: the source must not be swapped from an unusable cache")

			_, statErr := os.Stat(path)
			if tc.wantExists {
				require.NoError(t, statErr)
			} else {
				require.ErrorIs(t, statErr, os.ErrNotExist, "the unusable cache file must be removed")
			}
		})
	}
}

// TestLoadDiskCache_RemovalFailureIsNonFatal proves the os.Remove error
// path is non-fatal: a stale cache that cannot be removed (here a
// non-empty directory standing in for the cache path) is logged at WARN
// and the daemon still cold-starts fail-closed rather than aborting.
func TestLoadDiskCache_RemovalFailureIsNonFatal(t *testing.T) {
	// A non-empty directory at the cache path: os.Stat succeeds (so
	// staleness is detected) but os.Remove fails with ENOTEMPTY.
	path := filepath.Join(t.TempDir(), "dbip-country-lite.mmdb")
	require.NoError(t, os.Mkdir(path, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(path, "child"), []byte("x"), 0o600))
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	src := blocklist.NewSource()

	require.NotPanics(t, func() {
		loadDiskCache(logger, src, cacheTestConfig(path, time.Hour))
	})

	require.Contains(t, buf.String(), "could not remove", "the removal failure must be logged at WARN")
	require.True(t, strings.Contains(buf.String(), "stale"), "the fallback WARN must still be emitted")
	require.Nil(t, src.Current(), "fail-closed: the source must not be swapped")

	_, statErr := os.Stat(path)
	require.NoError(t, statErr, "removal failed, so the path still exists — and startup did not abort")
}

// staticAssertTrieSatisfiesLookup fails to compile if *blocklist.Trie
// stops satisfying server.Lookup — the closure's `return t` relies on
// that assignability.
var _ server.Lookup = (*blocklist.Trie)(nil)
