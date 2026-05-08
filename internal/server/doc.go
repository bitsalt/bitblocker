// Package server implements the forwardAuth HTTP surface: the /check
// endpoint Traefik calls per request, and the /healthz readiness probe.
// It extracts the client IP from request headers per the IP-extraction
// policy in the decisions log (X-Real-IP first, rightmost-XFF
// fallback), consults a Lookup-shaped blocklist, and returns 200 or
// the configured block status with an empty body.
//
// Fail-closed semantics: an unparseable client IP, an absent header
// pair, or an empty blocklist all deny the request. The /healthz probe
// returns 503 with {"status":"empty"} while the blocklist is empty so
// that load balancers and operators can distinguish "not ready" from
// "wrong answer."
//
// This package depends only on a Lookup interface; the swap-and-disk-
// cache slice supplies the concrete source. Logging is propagated
// through request context per internal/logging.
package server
