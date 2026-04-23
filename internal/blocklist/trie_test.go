package blocklist_test

import (
	"net/netip"
	"testing"

	"github.com/bitsalt/bitblocker/internal/blocklist"
	"github.com/stretchr/testify/require"
)

func prefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	require.NoError(t, err)
	return p
}

func addr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	require.NoError(t, err)
	return a
}

func buildTrie(t *testing.T, prefixes ...string) *blocklist.Trie {
	t.Helper()
	tr := blocklist.New()
	for _, p := range prefixes {
		tr.Insert(prefix(t, p))
	}
	return tr
}

func TestNew_Empty(t *testing.T) {
	tr := blocklist.New()
	require.Equal(t, 0, tr.Len())
	require.False(t, tr.Contains(addr(t, "1.2.3.4")))
	require.False(t, tr.Contains(addr(t, "::1")))
}

func TestInsertAndLookup_IPv4(t *testing.T) {
	cases := []struct {
		name    string
		cidrs   []string
		ip      string
		want    bool
	}{
		{"single host matches", []string{"1.2.3.4/32"}, "1.2.3.4", true},
		{"single host rejects neighbor", []string{"1.2.3.4/32"}, "1.2.3.5", false},
		{"/24 covers address inside", []string{"10.0.0.0/24"}, "10.0.0.123", true},
		{"/24 excludes address outside", []string{"10.0.0.0/24"}, "10.0.1.0", false},
		{"/0 covers everything", []string{"0.0.0.0/0"}, "192.0.2.1", true},
		{"disjoint prefixes both match", []string{"10.0.0.0/8", "192.168.0.0/16"}, "192.168.1.1", true},
		{"disjoint prefixes, neither covers", []string{"10.0.0.0/8", "192.168.0.0/16"}, "172.16.0.1", false},
		{"nested prefixes short-circuit at shallowest", []string{"10.0.0.0/8", "10.1.0.0/16"}, "10.1.1.1", true},
		{"nested prefixes: outside both", []string{"10.0.0.0/8", "10.1.0.0/16"}, "192.0.2.1", false},
		{"host bits in prefix are masked", []string{"10.0.0.255/24"}, "10.0.0.7", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := buildTrie(t, tc.cidrs...)
			require.Equal(t, tc.want, tr.Contains(addr(t, tc.ip)))
		})
	}
}

func TestInsertAndLookup_IPv6(t *testing.T) {
	cases := []struct {
		name  string
		cidrs []string
		ip    string
		want  bool
	}{
		{"/128 host matches", []string{"2001:db8::1/128"}, "2001:db8::1", true},
		{"/128 rejects neighbor", []string{"2001:db8::1/128"}, "2001:db8::2", false},
		{"/32 covers address inside", []string{"2001:db8::/32"}, "2001:db8:abcd::1", true},
		{"/32 excludes address outside", []string{"2001:db8::/32"}, "2001:db9::1", false},
		{"/0 covers everything", []string{"::/0"}, "2001:db8::1", true},
		{"nested: shallowest matches", []string{"2001:db8::/32", "2001:db8:1::/48"}, "2001:db8:1:2::3", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := buildTrie(t, tc.cidrs...)
			require.Equal(t, tc.want, tr.Contains(addr(t, tc.ip)))
		})
	}
}

func TestContains_IPv4Mapped6IsNormalizedToIPv4(t *testing.T) {
	// A client arriving as ::ffff:10.0.0.1 must match an IPv4 prefix.
	tr := buildTrie(t, "10.0.0.0/24")
	mapped := addr(t, "::ffff:10.0.0.5")
	require.True(t, tr.Contains(mapped))
}

func TestContains_V4PrefixDoesNotMatchNativeV6(t *testing.T) {
	// An IPv4 prefix does not match an unmapped IPv6 address.
	tr := buildTrie(t, "10.0.0.0/8")
	require.False(t, tr.Contains(addr(t, "2001:db8::10:0:0:1")))
}

func TestContains_V6PrefixDoesNotMatchV4(t *testing.T) {
	tr := buildTrie(t, "2001:db8::/32")
	require.False(t, tr.Contains(addr(t, "192.0.2.1")))
}

func TestInsert_Idempotent(t *testing.T) {
	tr := blocklist.New()
	p := prefix(t, "10.0.0.0/24")
	tr.Insert(p)
	tr.Insert(p)
	tr.Insert(p)
	require.Equal(t, 1, tr.Len())
	require.True(t, tr.Contains(addr(t, "10.0.0.1")))
}

func TestInsert_InvalidPrefixIsNoop(t *testing.T) {
	tr := blocklist.New()
	tr.Insert(netip.Prefix{}) // zero value is invalid
	require.Equal(t, 0, tr.Len())
}

func TestInsert_HostBitsMaskedBeforeStore(t *testing.T) {
	// 10.0.0.255/24 and 10.0.0.0/24 should coalesce; Len == 1.
	tr := blocklist.New()
	tr.Insert(prefix(t, "10.0.0.255/24"))
	tr.Insert(prefix(t, "10.0.0.0/24"))
	require.Equal(t, 1, tr.Len())
}

func TestLen_TracksMixedFamilies(t *testing.T) {
	tr := buildTrie(t,
		"10.0.0.0/8",
		"192.168.0.0/16",
		"2001:db8::/32",
	)
	require.Equal(t, 3, tr.Len())
}

func TestContains_InvalidAddrReturnsFalse(t *testing.T) {
	tr := buildTrie(t, "10.0.0.0/8")
	require.False(t, tr.Contains(netip.Addr{}))
}

func BenchmarkContains_IPv4(b *testing.B) {
	tr := blocklist.New()
	// Simulate a realistic blocklist: ~10k IPv4 /24s.
	base := netip.MustParseAddr("10.0.0.0")
	for i := 0; i < 10000; i++ {
		bytes := base.As4()
		bytes[1] = byte(i >> 8)
		bytes[2] = byte(i)
		p := netip.PrefixFrom(netip.AddrFrom4(bytes), 24)
		tr.Insert(p)
	}
	query := netip.MustParseAddr("192.0.2.1") // not in set; worst case short-circuits at root
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tr.Contains(query)
	}
}

func BenchmarkContains_IPv6(b *testing.B) {
	tr := blocklist.New()
	for i := 0; i < 10000; i++ {
		bytes := [16]byte{0x20, 0x01, 0x0d, 0xb8, byte(i >> 8), byte(i)}
		p := netip.PrefixFrom(netip.AddrFrom16(bytes), 48)
		tr.Insert(p)
	}
	query := netip.MustParseAddr("2001:db8:abcd::1")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tr.Contains(query)
	}
}
