# Interface: blocklist source and disk cache

> **Boundary owner:** `internal/blocklist` (and a disk-cache component, see §4).
> **Consumers:** `cmd/bitblocker` (wiring), `internal/server` (via the existing
> `server.LookupSource` seam), the Sprint 3 fetcher/scheduler.
> **Governing standards:** `coding-standards.md` §14 (API / interface design),
> §4 (explicit boundaries); `coding-standards-go.md` §15 (concurrency),
> §10 (doc comments).
> **Decisions:** ADR 0001 (swap mechanism), ADR 0002 (disk cache).

This spec is precise enough for the Developer to implement the Sprint 2
swap+disk-cache slice with no further design decisions. Where a sub-decision is
genuinely open it is flagged as an OQ in the relevant ADR and called out here.

---

## 1. Purpose of the boundary

Three things meet at this boundary:

1. The **trie producer** — `mmdb.LoadCountryBlocklist`, which builds an
   immutable `*blocklist.Trie` from an MMDB file. Already exists (PR #2).
2. The **publish/consume seam** — a `blocklist.Source` holding the active trie,
   readable lock-free on the `/check` hot path, swappable atomically on refresh.
   New in this slice.
3. The **disk cache** — writes the source MMDB after a successful load, reads it
   on startup to give the daemon a head start ahead of the first fetch. New in
   this slice.

The server side (`server.Lookup`, `server.LookupSource`) is **unchanged** — it
already defines the consumer-side interface. This boundary provides the
producer.

---

## 2. `blocklist.Source` — the swap seam

### 2.1 Type

A new type in `internal/blocklist` (suggested file `source.go`):

```go
// Source holds the active blocklist trie and publishes replacements
// atomically. The read side is lock-free and safe for unbounded
// concurrent callers; the write side is a single atomic store.
type Source struct {
    current atomic.Pointer[Trie]
}
```

### 2.2 Constructor

```go
// NewSource returns a Source with no trie loaded. Current returns nil
// until the first Swap. The daemon is in its fail-closed cold-start
// posture while the Source is empty.
func NewSource() *Source
```

### 2.3 Methods

| Method | Signature | Contract |
|---|---|---|
| `Current` | `func (s *Source) Current() *Trie` | Returns the active trie, or `nil` if none has been published. One `atomic.Pointer.Load()`. Safe for unbounded concurrent callers. Hot-path: must not lock, must not allocate. |
| `Swap` | `func (s *Source) Swap(t *Trie)` | Publishes `t` as the active trie via one `atomic.Pointer.Store()`. `t` may be `nil` (not expected in practice, but defined: returns to the empty state). Callers must treat `t` as immutable after the call — never mutate a trie after handing it to `Swap`. |

### 2.4 Invariants

- **The trie is immutable after publication.** `Swap` callers (the disk-cache
  read, the Sprint 3 fetcher) hand over a fully-constructed `*Trie` and never
  touch it again. `mmdb.LoadCountryBlocklist` already returns such a trie. This
  invariant is what makes the lock-free read correct (ADR 0001).
- **`Current` never blocks and never errors.** It is a pointer load. A `nil`
  return is a valid, expected state (cold start), not a failure.
- **No write/write mutual exclusion is required.** `atomic.Pointer.Store` is
  itself safe under concurrent callers; last store wins; every reader sees a
  consistent trie. The Sprint 3 scheduler is single-goroutine anyway.

### 2.5 Wiring into the server seam (`cmd/bitblocker`)

The daemon adapts `*Source` to the existing `server.LookupSource`
(`func() server.Lookup`) with a closure:

```go
src := blocklist.NewSource()

lookupSource := func() server.Lookup {
    if t := src.Current(); t != nil {
        return t            // *blocklist.Trie satisfies server.Lookup
    }
    return nil              // untyped nil — server treats this as "not ready"
}
```

**Hard requirement (do not get this wrong).** The closure must return an
**untyped `nil`** when the trie is absent — *not* a `server.Lookup` holding a
`(*blocklist.Trie)(nil)`. A non-nil interface wrapping a nil pointer would pass
the server's `lookup == nil` check and then panic on `lookup.Len()`. The
`if t != nil` guard above is mandatory, not stylistic. The Developer's tests
must exercise the cold-start path *through this closure* (`Current() == nil` →
closure returns nil → server fails closed cleanly), not only through `Source`
directly.

`*blocklist.Trie` already satisfies `server.Lookup` — `Contains(netip.Addr) bool`
and `Len() int` are present on `Trie` today. No change to `Trie`, no change to
`server`.

---

## 3. Error shapes

`Source` has no error-returning methods — it is a pointer cell. Errors live in
the two surrounding operations:

- **Trie construction** — `mmdb.LoadCountryBlocklist` already returns
  `(*Trie, error)`; its error shapes are unchanged (open failure, decode
  failure, traversal failure, `ErrNoCountries`). See `internal/mmdb/loader.go`.
- **Disk cache** — see §4.4.

---

## 4. Disk cache

### 4.1 Purpose

Persist the MMDB the active blocklist was built from, so a daemon restart can
rebuild the trie locally and serve a recent blocklist before the first network
fetch completes. Per ADR 0002 the on-disk artifact is the **raw MMDB file** —
no derived format.

### 4.2 Configuration surface

Two new config fields (placement — dedicated `cache:` block vs. under
`behavior:` — is OQ-CACHE-1; Architect lean is a dedicated block):

| Field | Type | Default | Validation |
|---|---|---|---|
| `cache.path` | `string` | `/var/cache/bitblocker/GeoLite2-Country.mmdb` | non-empty |
| `cache.max_age` | `time.Duration` | `48h` | `> 0` |

Both land in `internal/config` with `Validate()` coverage and a
`config.example.yaml` entry.

### 4.3 Write contract

`func WriteCache(path string, mmdbBytes io.Reader) error` (or
`(path, srcMMDBPath string)` — Developer's choice of source shape).

Procedure (ADR 0002 §B):

1. `os.MkdirAll(filepath.Dir(path), 0o755)` — tolerate a missing parent dir.
2. `os.CreateTemp(filepath.Dir(path), "*.mmdb.tmp")` — temp file **in the same
   directory** as `path` (same filesystem → atomic rename).
3. Copy bytes into the temp file; `f.Sync()` before close.
4. `os.Rename(tmp, path)` — atomic on POSIX same-filesystem.
5. On any pre-rename failure: `os.Remove(tmp)`, return the wrapped error. The
   prior cache file is left untouched.

**Failure is non-fatal.** A caller that fails to write the cache logs `WARN` and
continues — the in-memory trie is already serving. A cache-write failure must
never fail an otherwise-successful load.

### 4.4 Read contract

`func LoadCache(path string, maxAge time.Duration, now time.Time, countries []config.CountryCode) (*blocklist.Trie, error)`

(Inject `now` per `coding-standards-go.md` §4 — do not call `time.Now()` inside
the staleness check.)

Procedure (ADR 0002 §C):

1. `os.Stat(path)`. If the file does not exist → return a sentinel
   (`ErrCacheAbsent`) the caller treats as "no cache, proceed cold." Not an
   error condition to log at `ERROR`.
2. **Staleness check.** If `now.Sub(modTime) > maxAge` → return
   `ErrCacheStale`. Caller logs `WARN`, skips the cache.
3. **Load = validate.** Call `mmdb.LoadCountryBlocklist(path, countries)`. If it
   errors, the cache is corrupt/truncated → wrap and return; caller logs `WARN`,
   skips, and (per OQ-CACHE-2, Architect lean) removes the file.
4. On success return the `*Trie`. A trie with `Len() == 0` is **not** an error —
   return it; the caller `Swap`s it and the server's existing `Len() == 0` check
   keeps the daemon fail-closed until the fetcher delivers a populated trie.

### 4.5 Sentinel errors

```go
var ErrCacheAbsent = errors.New("blocklist: disk cache not present")
var ErrCacheStale  = errors.New("blocklist: disk cache exceeds max age")
```

Both are *expected* control-flow signals, distinguished with `errors.Is` at the
caller. They are not `ERROR`-level events. A corrupt-cache error from the loader
is wrapped (`%w`) and is `WARN`-level.

### 4.6 Startup integration (`cmd/bitblocker`)

```
1. src := blocklist.NewSource()
2. trie, err := blocklist.LoadCache(cfg.Cache.Path, cfg.Cache.MaxAge, time.Now(), cfg.Block.Countries)
   - ErrCacheAbsent  → log INFO "no disk cache; cold start", continue
   - ErrCacheStale   → log WARN "disk cache stale; skipping", continue
   - other error     → log WARN "disk cache unreadable; skipping", (remove file), continue
   - success         → src.Swap(trie); log INFO "loaded blocklist from disk cache" with trie.Len()
3. wire lookupSource closure (see §2.5), construct server, Run.
4. [Sprint 3] hand src to the fetcher/scheduler; every successful fetch calls
   src.Swap(newTrie) AND WriteCache(...).
```

The cache is always a *head start*, never a *substitute* for a fresh fetch.

---

## 5. Versioning stance

- `blocklist.Source` is an `internal/` type — no external consumers, no API
  version. Changes are ordinary code changes.
- The **on-disk cache format is the MaxMind MMDB format**, versioned by MaxMind
  and pinned via `maxminddb-golang v1.13.1`. The project introduces no codec of
  its own, so there is no project-owned on-disk format to version (this is a
  deliberate consequence of ADR 0002 — see its "no bespoke format" rationale).
- A future MMDB major-format change is handled by the loader/library, not here.

---

## 6. Test obligations (per `coding-standards-go.md` §1)

- `Source`: concurrent `Current` during `Swap` under `go test -race`;
  `Current` before any `Swap` returns `nil`.
- The `lookupSource` closure: cold-start path returns untyped `nil` (regression
  guard for the nil-interface trap, §2.5).
- `WriteCache`: temp-file + rename leaves no `.tmp` on success; a forced failure
  leaves the prior cache intact; round-trips through `LoadCache`. Use
  `t.TempDir()`.
- `LoadCache`: absent / stale (via injected `now`) / corrupt (truncated fixture)
  / valid-but-empty (`Len() == 0`) / valid-and-populated. Use `t.TempDir()` and
  the existing MMDB test fixtures from `internal/mmdb`.

---

## 7. Cross-references

- `docs/adr/0001-blocklist-swap-via-atomic-pointer.md`
- `docs/adr/0002-disk-cache-snapshot-format.md`
- `internal/server/server.go` — `Lookup`, `LookupSource` (consumer seam).
- `internal/blocklist/trie.go` — the immutable `Trie`.
- `internal/mmdb/loader.go` — `LoadCountryBlocklist`.
- `coding-standards.md` §14, §4; `coding-standards-go.md` §15, §4, §1.
