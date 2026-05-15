# ADR 0002: Disk cache stores the raw MMDB file, written via temp-file + atomic rename, and is rebuilt through the existing loader on startup

- **Status:** proposed
- **Date:** 2026-05-15
- **Deciders:** Architect; confirmable at Developer touch time
- **Supersedes:** none
- **Superseded by:** none

## Context

The spec's fail-closed posture (`bitblocker-spec.md` § Blocklist update
strategy; decisions log 2026-04-22) is operationally tolerable only because a
set of guardrails make a cold start survivable. The disk cache is one of those
guardrails: on a clean restart — a deploy, a crash, a host reboot — the daemon
should be able to serve a *recent* blocklist immediately instead of waiting for
the Sprint 3 fetcher to complete its first network fetch and possibly exhaust a
retry budget while every request fails closed.

The Sprint 2 disk-cache task is two halves:

1. **Write** a snapshot to disk after a blocklist load succeeds.
2. **Read** that snapshot on startup, before (and independent of) the fetcher,
   so the daemon can populate the trie from local state.

This ADR settles three things the Developer cannot proceed without:

- **A. What is serialized** — the raw MaxMind MMDB bytes, or a derived snapshot
  of the constructed trie.
- **B. Where it lives and how the write is made crash-safe.**
- **C. How a stale or corrupt snapshot is handled on the startup read, and how
  that interacts with the fail-closed cold-start posture.**

### The constraint that drives A

`blocklist.Trie` has **no exported serialization seam**. Its fields (`v4`,
`v6`, `count`, and the `node` type with `children`/`terminal`) are all
unexported. Serializing the *trie* would mean either (a) adding
`encoding.BinaryMarshaler`/`Unmarshaler` (or `gob` support) to the `blocklist`
package — inventing and versioning a bespoke on-disk codec for a radix-trie
node graph — or (b) walking the trie back out to a flat prefix list and
serializing that.

Meanwhile the daemon **already has** a fully-tested, format-stable path from an
on-disk artifact to a populated `*Trie`: `mmdb.LoadCountryBlocklist(path,
countries)`. The MMDB format is MaxMind's, versioned by MaxMind, parsed by the
pinned `maxminddb-golang v1.13.1`. The fetcher (Sprint 3) downloads exactly this
file. If the disk cache *is* a copy of that file, the startup read is just
"call the loader we already have, pointed at the cached path."

## Decision

### A. Serialize the raw MMDB file

**The disk cache is a byte-for-byte copy of the MMDB file the blocklist was
built from.** No derived format, no trie snapshot, no bespoke codec.

The write side copies the source MMDB into the cache location. The read side
calls `mmdb.LoadCountryBlocklist(cachePath, cfg.Block.Countries)` — the same
function `cmd/bitblocker` will call for a freshly-fetched file — and on success
calls `Source.Swap` (ADR 0001) with the resulting trie.

A consequence worth stating plainly: the cached artifact is the *input* to trie
construction, not the trie itself. The trie is always rebuilt from MMDB bytes,
whether those bytes came from the network or from the cache. There is exactly
one code path from "MMDB bytes" to "populated trie," and it is already written
and tested (`internal/mmdb`, PR #2).

### B. Location and crash-safe write

**Location.** A new config field under `behavior` (or a small new `cache`
block — Developer's call, see Open questions): `cache.path`, default
`/var/cache/bitblocker/GeoLite2-Country.mmdb`. The directory follows the FHS
convention for regenerable cached data; the systemd unit (Sprint 4) will own
its creation and the `bitblocker` user's write permission via `CacheDirectory=`.
For Sprint 2 the daemon must tolerate the directory not existing (see C) and
must `MkdirAll` the parent before its first write.

**Crash-safe write — temp file + atomic rename.** The write procedure:

1. Create a temp file in the **same directory** as the final cache path (same
   directory so the rename is same-filesystem and therefore atomic) —
   `os.CreateTemp(dir, "GeoLite2-Country.*.mmdb.tmp")`.
2. Copy the MMDB bytes into the temp file. `Sync()` the file to flush the OS
   buffer to stable storage before the rename.
3. `os.Rename(tmp, finalPath)` — on POSIX a same-filesystem rename is atomic: a
   concurrent or subsequent reader sees either the entire old file or the
   entire new file, never a partial write.
4. On any failure before the rename, remove the temp file and return the error;
   the previous cache (if any) is untouched.

This is the same temp-file-plus-rename discipline the spec's "atomic swap only"
principle applies in memory (ADR 0001), applied to the filesystem. A crash at
any point leaves either the prior good cache or no cache — never a half-written
one.

The write is **best-effort and non-fatal.** A failed cache write logs `WARN` and
the daemon continues — the in-memory trie is already swapped in and serving; the
cache is an optimization for the *next* start, not for this one. A cache-write
failure must never fail a load that otherwise succeeded.

### C. Startup read — staleness, validation, and fail-closed interaction

The startup sequence in `cmd/bitblocker` becomes:

1. Construct the empty `blocklist.Source` (ADR 0001).
2. **Attempt the disk-cache read.** If `cache.path` exists:
   - **Staleness check.** Stat the file; read its modification time. If the file
     is older than a configurable `cache.max_age` (default **48h** — comfortably
     longer than the default 24h refresh cycle, so a cache written by yesterday's
     refresh is still fresh, but a cache from a daemon that has been down for
     days is rejected), treat it as stale: log `WARN`, skip it, do not load. A
     stale cache is discarded, not served.
   - **Validation = a successful load.** There is no separate integrity check.
     If `mmdb.LoadCountryBlocklist` returns without error, the file is a
     structurally valid MMDB and the trie is built — `Source.Swap` it in. If the
     loader returns an error (truncated file, corrupt MMDB, decode failure — all
     already surfaced as errors by `internal/mmdb`), log `WARN` and skip the
     cache. A corrupt cache is discarded, not served. Optionally remove the
     corrupt file so it does not trip the next start.
   - A loaded trie with `Len() == 0` (cache valid, but no prefixes matched the
     configured countries — e.g. the operator changed `block.countries`) is
     *not* an error and *not* served as ready: `Source` holds it, but the
     server's existing `Len() == 0` check keeps `/healthz` at 503 and `/check`
     failing closed. The Sprint 3 fetcher will replace it. This falls out of the
     existing server logic with no special-casing.
3. **Whether or not the cache loaded, hand off to the fetcher** (Sprint 3). The
   cache is a *head start*, never a *substitute* for a fresh fetch. If the cache
   loaded, the daemon serves the cached blocklist while the first fetch runs; if
   it did not, the daemon is in the documented fail-closed cold-start posture
   until the fetch succeeds.

**Interaction with fail-closed.** The disk cache *narrows the fail-closed window*
but never *weakens* the posture:

- A successful, fresh cache load means the daemon serves a recent blocklist from
  the first request — the fail-closed window is effectively zero on a routine
  restart.
- A missing, stale, corrupt, or empty cache means the daemon is exactly where it
  would be with no cache at all: `Source` holds `nil`/empty, `/healthz` 503,
  `/check` fails closed, until the fetcher succeeds.
- The cache is never the *reason* a request is allowed when it should not be: a
  stale-but-loadable cache is rejected by the age check *before* it can serve.
  The worst a cache can do is be up to `cache.max_age` old — and `max_age` is
  chosen to bound that to "older than one refresh cycle but not dangerously so."

The disk cache therefore lives entirely on the "make fail-closed operationally
tolerable" side of the 2026-04-22 decision; it does not touch the
`behavior.startup_mode` knob (that knob governs what happens *while* empty —
deny vs. allow — and is Sprint 3's concern).

## Consequences

### Positive

- **No bespoke serialization format to design, version, or test.** The on-disk
  artifact is MaxMind's format, already parsed by a pinned, tested library. The
  `blocklist` package keeps its unexported internals and gains no
  marshalling surface.
- **One code path from artifact to trie.** `mmdb.LoadCountryBlocklist` is reused
  verbatim for both the cache read and the Sprint 3 fetch result. The cache read
  is tested by the loader's existing tests plus a thin "read a fixture from a
  temp dir" test.
- **The cache and the fetch download are the same file**, so the Sprint 3
  fetcher's write-to-cache step is a plain copy/rename of what it already
  downloaded — no transcoding.
- **Crash safety is the standard POSIX idiom**, the filesystem analogue of
  ADR 0001's in-memory atomic swap. No partial cache is ever observable.
- **Fail-closed posture is strictly preserved.** Every cache-absent path lands
  in the existing cold-start behavior; the staleness bound caps how old a served
  cache can be.

### Negative

- **The cache stores the whole MMDB, not just the prefixes for the configured
  countries.** GeoLite2-Country is on the order of a few MB — negligible disk
  cost for a self-hosted daemon, and the alternative (a filtered snapshot)
  reintroduces the bespoke-format problem. Accepted.
- **A `block.countries` change is not reflected until the next fetch** if the
  daemon restarts against an old cache — the cache holds *all* countries' data,
  but the trie is rebuilt with the *current* config's country set, so this
  actually resolves correctly on restart (the loader filters by the live
  `cfg.Block.Countries`). The only stale dimension is the MMDB *data* age, which
  the `max_age` check bounds. Net: not actually a negative once the loader's
  filter-on-read behavior is accounted for — noted here so the Developer does
  not add a redundant config-fingerprint check.
- **Two new config fields** (`cache.path`, `cache.max_age`) — small additive
  surface; both have safe defaults and need `Validate()` coverage (path
  non-empty, `max_age` positive).

### Neutral

- **The write trigger is "after a successful load," not "after a successful
  fetch."** In Sprint 2 there is no fetcher, so the write is exercised by
  whatever loads the trie. In Sprint 3 the fetcher is the load trigger. The
  write belongs at the same seam as `Source.Swap` — wherever a new trie is
  published, the originating MMDB file is also cached. Stating this so Sprint 3
  wires it consistently rather than re-deciding.

## Alternatives considered

### Serialize a derived trie snapshot (`gob`, or a flat prefix list)

Add `encoding.BinaryMarshaler` to `blocklist.Trie`, or walk it to a `[]netip.Prefix`
and `gob`-encode that.

**Why not:** it invents an on-disk format the project has to own, version
(coding-standards §14 — "interfaces are versioned"), and test for
forward/backward compatibility — all to avoid re-parsing an MMDB file that the
loader parses in well under the time a restart already costs. The trie's
unexported internals are a deliberate encapsulation; a marshalling surface
punches a hole in it for no benefit. The flat-prefix-list variant is less bad
but still a custom format with its own corruption modes. KISS (coding-standards
§15): reuse the loader.

### A separate integrity check (checksum / MMDB metadata validation) before load

Store a checksum alongside the cache, or validate the MMDB metadata section
before trusting the file.

**Why not:** "does `LoadCountryBlocklist` return without error" *is* the
integrity check — a truncated or corrupt MMDB fails the loader's `maxminddb.Open`
or its record decode, both already returning errors. A separate checksum guards
only against the file changing between checksum-write and load, which the
atomic-rename write already prevents. Redundant machinery.

### Write the cache in a fixed location with no temp file (write-in-place)

Open the final path and stream bytes into it.

**Why not:** a crash mid-write leaves a truncated MMDB at the canonical path —
the next startup loads garbage, the loader errors, and the cache is useless
*and* must be detected and removed. Temp-file + rename makes a partial write
unobservable. This is the same reasoning as ADR 0001's "no partial update ever
reaches lookup," applied to disk.

### No staleness bound — serve any loadable cache

**Why not:** a daemon that has been down for a week would serve week-old geo
data as if current. GeoLite2 country assignments do drift; an unbounded cache
silently degrades accuracy. The `max_age` bound (default 48h) keeps a routine
restart's cache (hours old) while rejecting a long-downtime cache, at the cost
of one `Stat` call. Cheap insurance.

## Open questions surfaced

- **OQ-CACHE-1 (new — proposed for the `docs/BitBlocker.md` Open Questions
  table, for PM to land).** The disk cache adds two config fields — `cache.path`
  (default `/var/cache/bitblocker/GeoLite2-Country.mmdb`) and `cache.max_age`
  (default 48h). Open sub-decision for the Developer at touch time: a dedicated
  `cache:` YAML block vs. fields under the existing `behavior:` block. Architect
  lean: a dedicated `cache:` block — it is its own concern and reads cleanly in
  `config.example.yaml`. Either way both fields need `Config.Validate()`
  coverage and a `config.example.yaml` entry. Routes to Developer; PM to log the
  config-surface addition.
- **OQ-CACHE-2 (new — proposed for the Open Questions table).** Should a corrupt
  or stale cache file be *removed* on detection, or left in place? Architect
  lean: remove it (a corrupt file serves no purpose and would re-trip the next
  start's load attempt + WARN). Low stakes; flagged so the Developer makes a
  deliberate choice rather than an incidental one.
- **OQ-CACHE-3 (routes to DevOps, Sprint 4).** The systemd unit needs
  `CacheDirectory=bitblocker` (creates `/var/cache/bitblocker` owned by the
  service user) and the Docker image needs the cache path on a writable volume
  or `tmpfs`. Out of Sprint 2 scope; recorded so Sprint 4 deploy work picks it
  up.

## Cross-references

- `docs/bitblocker-spec.md` § Blocklist update strategy, § Deployment.
- `docs/BitBlocker.md` decisions log 2026-04-22 (fail-closed posture and its
  guardrails — the disk cache is one of them).
- `docs/adr/0001-blocklist-swap-via-atomic-pointer.md` (the in-memory swap; the
  startup cache read calls `Source.Swap`).
- `docs/interfaces/blocklist-source.md` (the `Source` contract and the
  disk-cache read/write contract).
- `internal/mmdb/loader.go` (`LoadCountryBlocklist` — reused verbatim for the
  cache read).
- `internal/config/config.go` (the `cache.path` / `cache.max_age` fields and
  their `Validate()` coverage land here).
- Coding standards `coding-standards-go.md` §1 (`t.TempDir()` for the cache
  read/write tests), §9 (config), §6 (error wrapping); `coding-standards.md` §4
  (explicit I/O boundaries).

---

*End of ADR 0002.*
