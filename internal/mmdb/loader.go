package mmdb

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/oschwald/maxminddb-golang"

	"github.com/bitsalt/bitblocker/internal/blocklist"
	"github.com/bitsalt/bitblocker/internal/config"
)

// ErrNoCountries is returned by LoadCountryBlocklist when the supplied
// country set is empty. The daemon's config validation already rejects
// an empty block list at startup; this is a defense-in-depth check for
// callers wiring this package in other contexts.
var ErrNoCountries = errors.New("mmdb: country set is empty")

// countryRecord is the minimal subset of the GeoLite2-Country record
// shape we need: the ISO 3166-1 alpha-2 code of the country the IP
// geolocates to. We deliberately do not decode names, geoname IDs,
// continent, or registered_country — every extra field is decoder
// allocation we pay on every network in the DB (millions of rows).
type countryRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
}

// LoadCountryBlocklist opens the MMDB at path, walks every network in
// it, and returns a trie populated with the prefixes whose country code
// is in countries. The returned trie is fully constructed and ready for
// concurrent reads via Trie.Contains.
//
// The MMDB file is closed before this function returns; the trie has no
// onward dependency on it.
//
// Errors are returned for: an unreadable or malformed MMDB, a decoder
// failure on a record, or an iterator error surfaced during traversal.
// A network whose country code is not in the configured set is simply
// skipped — it is not an error.
func LoadCountryBlocklist(path string, countries []config.CountryCode) (*blocklist.Trie, error) {
	if len(countries) == 0 {
		return nil, ErrNoCountries
	}

	wanted := make(map[string]struct{}, len(countries))
	for _, c := range countries {
		wanted[string(c)] = struct{}{}
	}

	reader, err := maxminddb.Open(path)
	if err != nil {
		return nil, fmt.Errorf("mmdb: open %q: %w", path, err)
	}
	defer func() {
		// Close errors on a read-only mmap are not actionable; surface
		// them to the caller's logs only if the primary load succeeded.
		_ = reader.Close()
	}()

	trie := blocklist.New()

	// SkipAliasedNetworks: in an IPv6 database that also carries IPv4,
	// MaxMind aliases the IPv4 subtree under multiple IPv6 prefixes
	// (::ffff:0:0/96, 2001::/32, 2002::/16). Without this option we
	// would insert the same IPv4 ranges several times under different
	// IPv6 framings and inflate the trie. With it, IPv4 networks come
	// back as 4-byte IP / ≤32 prefix; IPv6 networks as 16-byte / ≤128.
	networks := reader.Networks(maxminddb.SkipAliasedNetworks)
	for networks.Next() {
		var rec countryRecord
		subnet, err := networks.Network(&rec)
		if err != nil {
			return nil, fmt.Errorf("mmdb: decode network record: %w", err)
		}
		if _, ok := wanted[rec.Country.ISOCode]; !ok {
			continue
		}
		prefix, ok := prefixFromIPNet(subnet)
		if !ok {
			// A network the MMDB reader produced but we cannot
			// represent as a netip.Prefix is a corrupt-DB signal
			// we do not want to swallow. Fail the whole load.
			return nil, fmt.Errorf("mmdb: cannot represent network %s as netip.Prefix", subnet)
		}
		trie.Insert(prefix)
	}
	if err := networks.Err(); err != nil {
		return nil, fmt.Errorf("mmdb: traverse networks: %w", err)
	}

	return trie, nil
}

// prefixFromIPNet converts a *net.IPNet (the maxminddb v1 API's network
// shape) into a netip.Prefix in the form the blocklist trie expects.
//
// Critical: the trie's Insert documents that an IPv4-in-IPv6 prefix is
// not transparently re-bitted to IPv4 semantics; callers must hand it a
// 4-byte form when they want IPv4 mask widths. SkipAliasedNetworks
// gives us 4-byte IPs for IPv4 networks and 16-byte IPs for IPv6
// networks, so the right addr constructor falls out of the IP length.
func prefixFromIPNet(n *net.IPNet) (netip.Prefix, bool) {
	if n == nil {
		return netip.Prefix{}, false
	}
	bits, _ := n.Mask.Size()
	switch len(n.IP) {
	case net.IPv4len:
		var b [4]byte
		copy(b[:], n.IP)
		return netip.PrefixFrom(netip.AddrFrom4(b), bits), true
	case net.IPv6len:
		var b [16]byte
		copy(b[:], n.IP)
		return netip.PrefixFrom(netip.AddrFrom16(b), bits), true
	default:
		return netip.Prefix{}, false
	}
}
