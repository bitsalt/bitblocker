// Package diskcache persists the blocklist's source MMDB to disk and
// reads it back on startup, so the daemon can serve a recent blocklist
// ahead of the first network fetch.
//
// The on-disk artifact is the raw MaxMind MMDB file — there is no
// derived format and no project-owned codec (see ADR 0002). The read
// path rebuilds the trie through the same internal/mmdb loader the
// Sprint 3 fetcher uses, so there is exactly one code path from MMDB
// bytes to a populated trie.
//
// This package lives separately from internal/blocklist because the
// read path depends on internal/mmdb, which already depends on
// internal/blocklist; folding the cache into blocklist would create an
// import cycle.
package diskcache
