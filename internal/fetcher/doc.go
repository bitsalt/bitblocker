// Package fetcher retrieves the DB-IP "IP-to-Country Lite" MMDB from its
// public monthly download URL and publishes it into the daemon's active
// blocklist. It performs a plain HTTPS GET (no auth), supports
// conditional requests (ETag / If-Modified-Since) so an already-current
// file returns 304 and is skipped, and falls back to the prior month's
// file on the first days of a month before the current one is published.
//
// A successful fetch is decompressed (single-stream gunzip — DB-IP ships
// a plain gzip of one .mmdb, not a tar archive), written through the
// internal/diskcache crash-safe write path, rebuilt into a trie via the
// one internal/mmdb loader, and swapped into the active blocklist.Source
// atomically. The fetcher does not own the active blocklist; it hands a
// fully-constructed trie to Source.Swap. See ADR 0003.
package fetcher
