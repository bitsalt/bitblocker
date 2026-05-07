// Package mmdb reads a MaxMind DB (MMDB) file from disk and projects it
// into the in-memory CIDR trie that the daemon serves /check against.
// It is the binding layer between MaxMind's binary format and the
// blocklist package — it owns no policy of its own beyond "for each
// network whose country code is in the configured set, insert the
// prefix into the trie."
//
// This package does not own the active blocklist; callers swap the
// returned trie in atomically. It also does not fetch the MMDB; that
// is the fetcher package's responsibility. See docs/bitblocker-spec.md
// for how these pieces compose.
package mmdb
