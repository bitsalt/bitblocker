package diskcache_test

import (
	"bytes"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/stretchr/testify/require"

	"github.com/bitsalt/bitblocker/internal/config"
	"github.com/bitsalt/bitblocker/internal/diskcache"
)

// writeCountryMMDB renders (cidr, country) pairs into an MMDB at path,
// matching the subset of GeoLite2-Country that the loader decodes. It
// mirrors the fixture builder in internal/mmdb's tests; mmdbwriter is
// pinned to v1.0.0 to stay below the Go 1.24 toolchain floor.
func writeCountryMMDB(t *testing.T, path string, entries map[string]string) {
	t.Helper()

	w, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            "GeoLite2-Country",
		RecordSize:              28,
		IncludeReservedNetworks: true,
	})
	require.NoError(t, err)

	for cidr, country := range entries {
		_, network, perr := net.ParseCIDR(cidr)
		require.NoErrorf(t, perr, "parse fixture cidr %q", cidr)
		require.NoErrorf(t, w.Insert(network, mmdbtype.Map{
			"country": mmdbtype.Map{"iso_code": mmdbtype.String(country)},
		}), "insert fixture %q", cidr)
	}

	fh, err := os.Create(path) //nolint:gosec // G304: t.TempDir is test-controlled
	require.NoError(t, err)
	defer func() { _ = fh.Close() }()
	_, err = w.WriteTo(fh)
	require.NoError(t, err)
}

// mmdbBytes returns a valid GeoLite2-Country MMDB as a byte slice.
func mmdbBytes(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "src.mmdb")
	writeCountryMMDB(t, path, entries)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}

func TestWrite_RoundTripsThroughLoad(t *testing.T) {
	raw := mmdbBytes(t, map[string]string{"10.0.0.0/24": "CN"})
	cachePath := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")

	require.NoError(t, diskcache.Write(cachePath, bytes.NewReader(raw)))

	trie, err := diskcache.Load(cachePath, time.Hour, time.Now(), []config.CountryCode{"CN"})
	require.NoError(t, err)
	require.Equal(t, 1, trie.Len())
	require.True(t, trie.Contains(netip.MustParseAddr("10.0.0.5")))
}

func TestWrite_CreatesMissingParentDirectory(t *testing.T) {
	raw := mmdbBytes(t, map[string]string{"10.0.0.0/24": "CN"})
	// A two-level-deep path whose parents do not exist yet.
	cachePath := filepath.Join(t.TempDir(), "nested", "cache", "GeoLite2-Country.mmdb")

	require.NoError(t, diskcache.Write(cachePath, bytes.NewReader(raw)))
	_, err := os.Stat(cachePath)
	require.NoError(t, err)
}

func TestWrite_LeavesNoTempFileOnSuccess(t *testing.T) {
	raw := mmdbBytes(t, map[string]string{"10.0.0.0/24": "CN"})
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "GeoLite2-Country.mmdb")

	require.NoError(t, diskcache.Write(cachePath, bytes.NewReader(raw)))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the final cache file should remain")
	require.Equal(t, "GeoLite2-Country.mmdb", entries[0].Name())
}

// failingReader returns an error partway through, simulating an I/O
// failure during the cache copy.
type failingReader struct{ failed bool }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.failed {
		return 0, os.ErrClosed
	}
	f.failed = true
	n := copy(p, "partial")
	return n, nil
}

func TestWrite_FailedWriteLeavesPriorCacheIntactAndNoTempFile(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "GeoLite2-Country.mmdb")

	// Seed a prior good cache.
	priorRaw := mmdbBytes(t, map[string]string{"10.0.0.0/24": "CN"})
	require.NoError(t, diskcache.Write(cachePath, bytes.NewReader(priorRaw)))

	// A write that fails mid-copy must not touch the prior cache.
	err := diskcache.Write(cachePath, &failingReader{})
	require.Error(t, err)

	got, err := os.ReadFile(cachePath)
	require.NoError(t, err)
	require.Equal(t, priorRaw, got, "the prior cache must be left untouched")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the failed write must leave no .tmp file behind")
}

func TestLoad_AbsentReturnsErrAbsent(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.mmdb")
	trie, err := diskcache.Load(missing, time.Hour, time.Now(), []config.CountryCode{"CN"})
	require.ErrorIs(t, err, diskcache.ErrAbsent)
	require.Nil(t, trie)
}

func TestLoad_StaleReturnsErrStale(t *testing.T) {
	raw := mmdbBytes(t, map[string]string{"10.0.0.0/24": "CN"})
	cachePath := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	require.NoError(t, diskcache.Write(cachePath, bytes.NewReader(raw)))

	info, err := os.Stat(cachePath)
	require.NoError(t, err)

	// now is injected far enough past the file's mod time to exceed
	// maxAge — no need to backdate the file itself.
	now := info.ModTime().Add(49 * time.Hour)
	trie, err := diskcache.Load(cachePath, 48*time.Hour, now, []config.CountryCode{"CN"})
	require.ErrorIs(t, err, diskcache.ErrStale)
	require.Nil(t, trie)
}

func TestLoad_FreshWithinMaxAgeLoads(t *testing.T) {
	raw := mmdbBytes(t, map[string]string{"10.0.0.0/24": "CN"})
	cachePath := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	require.NoError(t, diskcache.Write(cachePath, bytes.NewReader(raw)))

	info, err := os.Stat(cachePath)
	require.NoError(t, err)

	now := info.ModTime().Add(47 * time.Hour)
	trie, err := diskcache.Load(cachePath, 48*time.Hour, now, []config.CountryCode{"CN"})
	require.NoError(t, err)
	require.Equal(t, 1, trie.Len())
}

func TestLoad_CorruptFileReturnsWrappedError(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	require.NoError(t, os.WriteFile(cachePath, []byte("not an mmdb file at all"), 0o600))

	trie, err := diskcache.Load(cachePath, time.Hour, time.Now(), []config.CountryCode{"CN"})
	require.Error(t, err)
	require.NotErrorIs(t, err, diskcache.ErrAbsent)
	require.NotErrorIs(t, err, diskcache.ErrStale)
	require.Nil(t, trie)
}

func TestLoad_TruncatedFileReturnsWrappedError(t *testing.T) {
	raw := mmdbBytes(t, map[string]string{"10.0.0.0/24": "CN"})
	require.Greater(t, len(raw), 100)

	cachePath := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	require.NoError(t, os.WriteFile(cachePath, raw[:len(raw)/2], 0o600))

	trie, err := diskcache.Load(cachePath, time.Hour, time.Now(), []config.CountryCode{"CN"})
	require.Error(t, err)
	require.Nil(t, trie)
}

func TestLoad_ValidButEmptyTrieIsNotAnError(t *testing.T) {
	// The cache is a valid MMDB, but no network matches the configured
	// country set — e.g. the operator changed block.countries. The
	// loader returns an empty trie; Load returns it without error. The
	// server's existing Len() == 0 check keeps the daemon fail-closed.
	raw := mmdbBytes(t, map[string]string{"10.0.0.0/24": "US"})
	cachePath := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	require.NoError(t, diskcache.Write(cachePath, bytes.NewReader(raw)))

	trie, err := diskcache.Load(cachePath, time.Hour, time.Now(), []config.CountryCode{"CN"})
	require.NoError(t, err)
	require.NotNil(t, trie)
	require.Equal(t, 0, trie.Len())
}
