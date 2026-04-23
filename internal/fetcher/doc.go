// Package fetcher retrieves CIDR data from upstream sources (MaxMind
// GeoLite2, and — later — BGP.tools) and parses it into the shape the
// blocklist package consumes. It handles conditional requests (ETag /
// If-Modified-Since), retries with exponential backoff, and signals the
// daemon whether a refresh produced a new ruleset.
//
// This package does not own the active blocklist; it returns parsed data
// for the caller to swap in atomically.
package fetcher
