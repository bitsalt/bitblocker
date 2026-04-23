// Package blocklist owns the in-memory CIDR trie used for fast IP lookups
// against the active blocklist. It supports IPv4 and IPv6 prefixes and
// exposes an atomic-swap mechanism so readers never observe a partial
// update while a refresh is in flight.
//
// This package is pure data-structure and lookup logic. It performs no I/O;
// fetching and parsing source data is the fetcher package's responsibility.
package blocklist
