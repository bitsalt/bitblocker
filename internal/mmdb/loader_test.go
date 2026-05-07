package mmdb_test

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/stretchr/testify/require"

	"github.com/bitsalt/bitblocker/internal/config"
	"github.com/bitsalt/bitblocker/internal/mmdb"
)

// fixtureEntry is a single (cidr, country-code) pair that the test
// fixture will encode into a synthetic GeoLite2-Country-shaped MMDB.
type fixtureEntry struct {
	cidr    string
	country string // "" means write a record without a country.iso_code
}

// writeCountryMMDB renders entries into a temp MMDB whose record shape
// matches the subset of GeoLite2-Country that the loader decodes. The
// returned path is auto-cleaned via t.TempDir.
//
// We use mmdbwriter directly — pinned to v1.0.0 to stay below the
// Go 1.24 toolchain floor that mmdbwriter v1.1.0+ introduces — rather
// than checking a binary fixture into the repo, so the test stays
// self-contained and the input is obvious from the source.
func writeCountryMMDB(t *testing.T, entries []fixtureEntry) string {
	t.Helper()

	w, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            "GeoLite2-Country",
		RecordSize:              28,
		IncludeReservedNetworks: true, // documentation networks (10/8, 2001:db8::/32) are reserved
	})
	require.NoError(t, err)

	for _, e := range entries {
		_, network, perr := net.ParseCIDR(e.cidr)
		require.NoErrorf(t, perr, "parse fixture cidr %q", e.cidr)

		record := mmdbtype.Map{}
		if e.country != "" {
			record["country"] = mmdbtype.Map{
				"iso_code": mmdbtype.String(e.country),
			}
		}
		require.NoErrorf(t, w.Insert(network, record), "insert fixture %q", e.cidr)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.mmdb")
	fh, err := os.Create(path) //nolint:gosec // G304: t.TempDir is test-controlled
	require.NoError(t, err)
	t.Cleanup(func() { _ = fh.Close() })
	_, err = w.WriteTo(fh)
	require.NoError(t, err)
	return path
}

// addr is a small helper that mirrors the trie test idiom.
func addr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	require.NoError(t, err)
	return a
}

func TestLoadCountryBlocklist_HappyPath_IPv4(t *testing.T) {
	// Use disjoint /24s separated by a US-coded gap; if we put two
	// CN /24s back-to-back, mmdbwriter will coalesce them into a
	// single /23 in the on-disk tree (records with equal data merge),
	// and we would only see one prefix come back from Networks().
	path := writeCountryMMDB(t, []fixtureEntry{
		{"10.0.0.0/24", "CN"},
		{"10.0.1.0/24", "US"},
		{"10.0.2.0/24", "CN"},
		{"192.0.2.0/24", "US"},
	})

	trie, err := mmdb.LoadCountryBlocklist(path, []config.CountryCode{"CN"})
	require.NoError(t, err)
	require.NotNil(t, trie)
	require.Equal(t, 2, trie.Len())

	require.True(t, trie.Contains(addr(t, "10.0.0.5")))
	require.True(t, trie.Contains(addr(t, "10.0.2.250")))
	require.False(t, trie.Contains(addr(t, "10.0.1.5")))
	require.False(t, trie.Contains(addr(t, "192.0.2.5")))
	require.False(t, trie.Contains(addr(t, "172.16.0.1")))
}

func TestLoadCountryBlocklist_HappyPath_IPv6(t *testing.T) {
	path := writeCountryMMDB(t, []fixtureEntry{
		{"2001:db8::/48", "CN"},
		{"2001:db8:1::/48", "RU"},
		{"2001:db8:2::/48", "US"},
	})

	trie, err := mmdb.LoadCountryBlocklist(path, []config.CountryCode{"CN", "RU"})
	require.NoError(t, err)
	require.Equal(t, 2, trie.Len())

	require.True(t, trie.Contains(addr(t, "2001:db8::1")))
	require.True(t, trie.Contains(addr(t, "2001:db8:1::1")))
	require.False(t, trie.Contains(addr(t, "2001:db8:2::1")))
}

func TestLoadCountryBlocklist_DualStack(t *testing.T) {
	path := writeCountryMMDB(t, []fixtureEntry{
		{"10.0.0.0/24", "CN"},
		{"2001:db8::/48", "CN"},
		{"192.0.2.0/24", "US"},
		{"2001:db8:1::/48", "US"},
	})

	trie, err := mmdb.LoadCountryBlocklist(path, []config.CountryCode{"CN"})
	require.NoError(t, err)
	require.Equal(t, 2, trie.Len())

	require.True(t, trie.Contains(addr(t, "10.0.0.5")))
	require.True(t, trie.Contains(addr(t, "2001:db8::1")))
	require.False(t, trie.Contains(addr(t, "192.0.2.5")))
	require.False(t, trie.Contains(addr(t, "2001:db8:1::1")))

	// IPv4-mapped-in-IPv6 client (typical dual-stack arrival) must
	// match the IPv4 prefix — guarded by Trie.Contains' Unmap step.
	require.True(t, trie.Contains(addr(t, "::ffff:10.0.0.5")))
}

func TestLoadCountryBlocklist_NoMatchingCountries_TrieIsEmpty(t *testing.T) {
	path := writeCountryMMDB(t, []fixtureEntry{
		{"10.0.0.0/24", "US"},
		{"192.0.2.0/24", "GB"},
		{"2001:db8::/48", "DE"},
	})

	trie, err := mmdb.LoadCountryBlocklist(path, []config.CountryCode{"CN"})
	require.NoError(t, err)
	require.Equal(t, 0, trie.Len())
	require.False(t, trie.Contains(addr(t, "10.0.0.5")))
}

func TestLoadCountryBlocklist_RecordWithoutCountryFieldIsIgnored(t *testing.T) {
	// A real GeoLite2-Country DB occasionally has records with no
	// country (e.g. anonymous proxy ranges, anycast). Those decode to
	// an empty ISOCode and must not match any block-list entry.
	path := writeCountryMMDB(t, []fixtureEntry{
		{"10.0.0.0/24", "CN"},
		{"192.0.2.0/24", ""}, // no country
	})

	trie, err := mmdb.LoadCountryBlocklist(path, []config.CountryCode{"CN"})
	require.NoError(t, err)
	require.Equal(t, 1, trie.Len())
	require.True(t, trie.Contains(addr(t, "10.0.0.5")))
	require.False(t, trie.Contains(addr(t, "192.0.2.5")))
}

func TestLoadCountryBlocklist_MultipleCountriesUnion(t *testing.T) {
	path := writeCountryMMDB(t, []fixtureEntry{
		{"10.0.0.0/24", "CN"},
		{"10.1.0.0/24", "RU"},
		{"10.2.0.0/24", "KP"},
		{"10.3.0.0/24", "US"},
	})

	trie, err := mmdb.LoadCountryBlocklist(path, []config.CountryCode{"CN", "RU", "KP"})
	require.NoError(t, err)
	require.Equal(t, 3, trie.Len())

	require.True(t, trie.Contains(addr(t, "10.0.0.5")))
	require.True(t, trie.Contains(addr(t, "10.1.0.5")))
	require.True(t, trie.Contains(addr(t, "10.2.0.5")))
	require.False(t, trie.Contains(addr(t, "10.3.0.5")))
}

func TestLoadCountryBlocklist_EmptyCountryListErrors(t *testing.T) {
	path := writeCountryMMDB(t, []fixtureEntry{
		{"10.0.0.0/24", "CN"},
	})

	trie, err := mmdb.LoadCountryBlocklist(path, nil)
	require.ErrorIs(t, err, mmdb.ErrNoCountries)
	require.Nil(t, trie)

	trie, err = mmdb.LoadCountryBlocklist(path, []config.CountryCode{})
	require.ErrorIs(t, err, mmdb.ErrNoCountries)
	require.Nil(t, trie)
}

func TestLoadCountryBlocklist_MissingFileErrors(t *testing.T) {
	trie, err := mmdb.LoadCountryBlocklist(
		filepath.Join(t.TempDir(), "does-not-exist.mmdb"),
		[]config.CountryCode{"CN"},
	)
	require.Error(t, err)
	require.Nil(t, trie)
}

func TestLoadCountryBlocklist_MalformedFileErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "garbage.mmdb")
	// Random non-MMDB bytes; the magic-marker scan will fail.
	require.NoError(t, os.WriteFile(path, []byte("not an mmdb file at all"), 0o600))

	trie, err := mmdb.LoadCountryBlocklist(path, []config.CountryCode{"CN"})
	require.Error(t, err)
	require.Nil(t, trie)
}

func TestLoadCountryBlocklist_TruncatedFileErrors(t *testing.T) {
	// Build a real MMDB, then chop its tail off so the metadata block
	// is unreadable. This exercises the "Open succeeded but parsing
	// fails downstream" path — a different error surface than a file
	// of pure garbage.
	good := writeCountryMMDB(t, []fixtureEntry{{"10.0.0.0/24", "CN"}})
	raw, err := os.ReadFile(good)
	require.NoError(t, err)
	require.Greater(t, len(raw), 100, "fixture is unexpectedly small")

	truncated := filepath.Join(t.TempDir(), "truncated.mmdb")
	require.NoError(t, os.WriteFile(truncated, raw[:len(raw)/2], 0o600))

	trie, err := mmdb.LoadCountryBlocklist(truncated, []config.CountryCode{"CN"})
	require.Error(t, err)
	require.Nil(t, trie)
}

func TestLoadCountryBlocklist_DoesNotPanicOnUnusualPrefixLengths(t *testing.T) {
	// /32 host entries and /0 catch-alls sit at the boundary of the
	// trie's Insert validation. The MMDB must not produce a panic for
	// either; both should round-trip cleanly.
	path := writeCountryMMDB(t, []fixtureEntry{
		{"203.0.113.7/32", "CN"},
		{"2001:db8:dead:beef::1/128", "CN"},
	})

	trie, err := mmdb.LoadCountryBlocklist(path, []config.CountryCode{"CN"})
	require.NoError(t, err)
	require.Equal(t, 2, trie.Len())
	require.True(t, trie.Contains(addr(t, "203.0.113.7")))
	require.True(t, trie.Contains(addr(t, "2001:db8:dead:beef::1")))
}
