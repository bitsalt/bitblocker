package server

import (
	"net/http"
	"net/netip"
	"strings"
)

// Header names the daemon trusts to carry the originating client IP.
// Both are constant strings; callers should not need to spell them
// directly.
const (
	headerXRealIP       = "X-Real-IP"
	headerXForwardedFor = "X-Forwarded-For"
)

// extractClientIP returns the originating client address from r per the
// IP-extraction policy in docs/BitBlocker.md (decisions log 2026-04-22):
//
//  1. X-Real-IP wins if present and parseable.
//  2. Otherwise, the rightmost entry of X-Forwarded-For wins.
//
// The rightmost-XFF choice is deliberate. Under Traefik's
// trustForwardHeader: true, leftmost XFF is attacker-controllable; the
// rightmost entry is whatever proxy was actually adjacent to Traefik.
// A future config knob will surface leftmost-XFF for upstream-CDN
// scenarios — until then, we do not give callers a footgun.
//
// The boolean return is false when no header carries a parseable
// address. Callers must treat that as a fail-closed signal.
func extractClientIP(r *http.Request) (netip.Addr, bool) {
	if v := strings.TrimSpace(r.Header.Get(headerXRealIP)); v != "" {
		if addr, err := netip.ParseAddr(v); err == nil {
			return addr.Unmap(), true
		}
	}

	if v := r.Header.Get(headerXForwardedFor); v != "" {
		if addr, ok := rightmostXFF(v); ok {
			return addr, true
		}
	}

	return netip.Addr{}, false
}

// rightmostXFF parses an X-Forwarded-For header value and returns the
// rightmost parseable address. Empty entries (from a trailing comma) and
// unparseable entries between commas are skipped from the right; the
// first parseable entry from the right wins.
func rightmostXFF(v string) (netip.Addr, bool) {
	parts := strings.Split(v, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		p := strings.TrimSpace(parts[i])
		if p == "" {
			continue
		}
		if addr, err := netip.ParseAddr(p); err == nil {
			return addr.Unmap(), true
		}
	}
	return netip.Addr{}, false
}
