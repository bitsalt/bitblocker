package blocklist

import "net/netip"

// Trie is an immutable-after-construction CIDR set supporting IPv4 and
// IPv6 prefixes. Contains answers "does any inserted prefix cover this
// address." Lookups are O(prefix-length) bit walks; for IPv6 that is at
// most 128 comparisons per query, which is the hot-path target the
// spec's <1ms budget allows for.
//
// A Trie is not safe for concurrent writes. Readers and a single writer
// may interleave safely only through the atomic-swap mechanism that the
// daemon uses at refresh time; that swap is this package's contract
// with the rest of the system, not a property of the Trie itself.
type Trie struct {
	v4    *node
	v6    *node
	count int
}

type node struct {
	children [2]*node
	terminal bool
}

// New returns an empty Trie.
func New() *Trie {
	return &Trie{v4: &node{}, v6: &node{}}
}

// Len returns the number of distinct prefixes inserted. Duplicate
// inserts of the same (masked) prefix count once.
func (t *Trie) Len() int {
	return t.count
}

// Insert adds p to the set. Invalid prefixes are ignored. The prefix
// is masked before insertion so callers need not pre-zero host bits.
// Duplicate insertions are idempotent.
func (t *Trie) Insert(p netip.Prefix) {
	if !p.IsValid() {
		return
	}
	p = p.Masked()
	addr := p.Addr().Unmap()
	bits := p.Bits()

	// If Unmap turned an IPv4-in-IPv6 prefix into a bare IPv4 prefix,
	// the reported bits are still the IPv6-framed value (e.g. /120 for
	// an intended /24). Callers that want IPv4 semantics on such
	// prefixes should supply a 4-byte form directly; we do not guess.

	root := t.v6
	maxBits := 128
	if addr.Is4() {
		root = t.v4
		maxBits = 32
	}
	if bits > maxBits {
		return
	}

	n := root
	bytes := addrBytes(addr)
	for i := 0; i < bits; i++ {
		b := bitAt(bytes, i)
		if n.children[b] == nil {
			n.children[b] = &node{}
		}
		n = n.children[b]
	}
	if !n.terminal {
		n.terminal = true
		t.count++
	}
}

// Contains reports whether any inserted prefix covers ip. IPv4-in-IPv6
// addresses (e.g. ::ffff:1.2.3.4) are normalized before lookup, so a
// client arriving over a dual-stack socket is matched against the
// IPv4 prefixes if applicable.
func (t *Trie) Contains(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()

	root := t.v6
	maxBits := 128
	if ip.Is4() {
		root = t.v4
		maxBits = 32
	}
	if root == nil {
		return false
	}

	n := root
	bytes := addrBytes(ip)
	for i := 0; i < maxBits; i++ {
		if n.terminal {
			return true
		}
		b := bitAt(bytes, i)
		if n.children[b] == nil {
			return false
		}
		n = n.children[b]
	}
	return n.terminal
}

// addrBytes returns the canonical byte form of addr: 4 bytes for IPv4,
// 16 for IPv6. Callers must have already normalized with Unmap.
func addrBytes(addr netip.Addr) []byte {
	if addr.Is4() {
		b := addr.As4()
		return b[:]
	}
	b := addr.As16()
	return b[:]
}

// bitAt returns the i-th bit of b, reading most-significant first
// within each byte. i is zero-based.
func bitAt(b []byte, i int) int {
	return int((b[i/8] >> (7 - i%8)) & 1)
}
