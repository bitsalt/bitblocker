package main

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bitsalt/bitblocker/internal/blocklist"
	"github.com/bitsalt/bitblocker/internal/server"
)

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

// staticAssertTrieSatisfiesLookup fails to compile if *blocklist.Trie
// stops satisfying server.Lookup — the closure's `return t` relies on
// that assignability.
var _ server.Lookup = (*blocklist.Trie)(nil)
