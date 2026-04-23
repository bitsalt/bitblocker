// Package server implements the forwardAuth HTTP surface: the /check
// endpoint Traefik calls per request, and the /healthz liveness probe.
// It extracts the client IP from request headers, consults the blocklist,
// and returns 200 or 403 with an empty body.
//
// Fail-closed semantics: unparseable client IPs and an empty blocklist
// both deny the request. See docs/BitBlocker.md decisions log for the
// rationale.
package server
