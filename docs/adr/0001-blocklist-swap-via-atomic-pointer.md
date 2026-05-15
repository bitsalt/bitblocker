# ADR 0001: Blocklist swap is lock-free via `atomic.Pointer[blocklist.Trie]`, not pointer-swap under `RWMutex`

- **Status:** proposed
- **Date:** 2026-05-15
- **Deciders:** Architect; confirmable at Developer touch time
- **Supersedes:** none
- **Superseded by:** none

## Context

Sprint 2's swap+disk-cache slice replaces today's empty `LookupSource` (an
always-`nil` stub wired in `cmd/bitblocker/main.go`) with a real source backed
by a populated CIDR trie. The server already defines the seam — `server.Lookup`
(an interface, `Contains` + `Len`) and `server.LookupSource` (a
`func() Lookup`) — at the consumer side, per the Go addendum §15 ("define
interfaces at the consumer"). The swap mechanism is the *provider* of that
function. No server-side change is required regardless of which mechanism we
pick; the seam is the contract.

The project docs currently disagree on the mechanism, and the two readings are
materially different designs:

- `.agent-context.md` § Key invariants (line 54) and the decisions log
  (2026-05-08 row, and the Milestones table "atomic swap" wording) say
  **"pointer-swap under `RWMutex`"** — a lock-based design.
- The 2026-05-10 mid-sprint sprint note (`docs/BitBlocker.md` line 51), the
  `.agent-context.md` § Current sprint goal paragraph, and the
  `cmd/bitblocker/main.go` code comment (lines 56–57) say an
  **`atomic.Pointer[blocklist.Trie]`-backed source** — a lock-free design.

These cannot both be the mechanism. This ADR settles it and flags the stale
text for correction.

### What the two designs actually are

**Lock-based (`RWMutex`).** A struct holds `mu sync.RWMutex` and `trie *Trie`.
Every `/check` does `mu.RLock()` → read pointer → `mu.RUnlock()`. A refresh does
`mu.Lock()` → overwrite pointer → `mu.Unlock()`. Readers contend on the
`RWMutex`'s internal atomic counters even when no writer is present; under high
read concurrency the `RLock`/`RUnlock` pair is a measurable shared-cacheline
cost on every request.

**Lock-free (`atomic.Pointer[Trie]`).** A struct holds
`current atomic.Pointer[blocklist.Trie]`. Every `/check` does `current.Load()` —
a single atomic pointer read, no lock, no cacheline write. A refresh does
`current.Store(newTrie)` — a single atomic pointer write. The trie payload is
immutable after construction, so a reader holding an old `*Trie` keeps reading a
fully-consistent snapshot while the next one is published.

### The constraints that decide it

1. **Access pattern is read-heavy / write-rare in the extreme.** `/check` fires
   once per inbound HTTP request Traefik proxies — potentially thousands per
   second on a busy host. A refresh fires on the cron schedule, default
   `0 3 * * *` — once per day. The read:write ratio is on the order of
   10^7 : 1.
2. **The spec's hot-path budget is <1ms per check** (`bitblocker-spec.md`
   § Blocklist cache; reaffirmed in the trie doc comment). The trie itself
   benchmarks at ~39 ns (IPv4) / ~211 ns (IPv6) against a 10k-prefix set
   (Sprint 1 notes). The swap mechanism's per-read overhead is added on top of
   that and should be as close to zero as possible.
3. **The trie is already immutable-after-construction.** `blocklist.Trie`'s doc
   comment states it explicitly: "A `Trie` is not safe for concurrent writes …
   Readers and a single writer may interleave safely only through the
   atomic-swap mechanism." `mmdb.LoadCountryBlocklist` returns a fully-built
   `*Trie` that is never mutated after it is returned. This is exactly the
   precondition `atomic.Pointer[T]` requires — the payload is immutable once
   published.
4. **The Go addendum already names this case.** §15 Concurrency, verbatim:
   "Swapping a pointer under a mutex is cheaper than locking the whole structure
   for reads — use `atomic.Pointer[T]` when the payload is immutable after
   publication." The trie satisfies the immutability precondition, so the
   addendum's guidance points directly at `atomic.Pointer`.

## Decision

**The blocklist swap is implemented with `atomic.Pointer[blocklist.Trie]`.**

A new type in `internal/blocklist` — `Source` — owns a single
`atomic.Pointer[Trie]` field:

- `Source.Lookup()` (or the field accessed via the closure the daemon passes as
  `server.LookupSource`) calls `current.Load()` and returns the `*Trie` as a
  `server.Lookup`. One atomic pointer read on the hot path. No lock.
- `Source.Swap(newTrie *Trie)` calls `current.Store(newTrie)`. One atomic
  pointer write. Called by the Sprint 3 fetcher/scheduler and by the disk-cache
  load path (ADR 0002).
- A freshly-constructed `Source` holds a `nil` pointer. `Load()` on a `nil`
  pointer returns a `nil` `*Trie`; the daemon's `LookupSource` closure returns
  `nil`, which the server already treats as "not ready" (`handleCheck` and
  `handleHealthz` both null-check the result). This preserves the fail-closed
  cold-start posture with no special-casing.

The `RWMutex` reading is rejected. It buys nothing the immutable-trie design
needs and adds shared-state contention to the single most latency-sensitive code
path in the daemon.

### Why the seam needs no server change either way

`server.LookupSource` is `func() server.Lookup`. The daemon wires it as a
closure over the `*blocklist.Source`:

```go
src := blocklist.NewSource()                 // empty; nil pointer inside
lookupSource := func() server.Lookup {
    if t := src.Current(); t != nil {        // *blocklist.Trie or nil
        return t                             // *Trie satisfies server.Lookup
    }
    return nil                               // server treats nil as "not ready"
}
```

`*blocklist.Trie` already satisfies `server.Lookup` (`Contains(netip.Addr) bool`
and `Len() int` are both present on `Trie` today). The closure is the only new
wiring in `main.go`. The server package is untouched. This holds for the
`RWMutex` design too — which is *why* the mechanism choice is purely an
internal-to-`blocklist` decision and this ADR can settle it without a
server-side consequence.

### `nil`-pointer return and the `server.Lookup` interface

One subtlety the Developer must get right: a `nil` `*blocklist.Trie` assigned
into a `server.Lookup` interface value is **not** a `nil` interface — it is a
non-nil interface holding a nil concrete pointer, and calling `Len()` on it
would dereference nil. The closure above avoids this by returning an untyped
`nil` explicitly when the trie is absent (the `if t != nil` guard). The
interface contract in `docs/interfaces/blocklist-source.md` § Invariants states
this as a hard requirement on the closure, and the Developer's tests must cover
the cold-start (nil) path through the closure, not just through `Source`
directly.

## Consequences

### Positive

- **Zero-lock hot path.** `/check` adds one `atomic.Pointer.Load()` —
  single-digit nanoseconds, no cacheline write, no reader contention — on top of
  the trie's own ~39–211 ns. Comfortably inside the <1ms budget with room to
  spare.
- **Matches the Go addendum's named guidance** (§15) rather than diverging from
  it. A reviewer reading the addendum and the code sees the same decision.
- **The trie's existing immutability invariant is load-bearing and now
  documented as such.** No new constraint on `blocklist.Trie`; the design
  *relies on* the property the trie doc comment already asserts.
- **Cold start needs no special path.** `nil` pointer → `nil` lookup → server's
  existing fail-closed branches. The disk-cache load (ADR 0002) and the Sprint 3
  fetcher both just call `Swap`.
- **`go test -race` clean by construction.** `atomic.Pointer` is the race
  detector's blessed primitive for exactly this publish/consume pattern.

### Negative

- **The losing doc text is now stale and must be corrected.** Three locations
  say "RWMutex": `.agent-context.md` line 54, the `docs/BitBlocker.md` decisions
  log, and the `docs/BitBlocker.md` Milestones table ("atomic swap" is fine; any
  "under RWMutex" phrasing is not). The sprint-file corrections are PM's to
  land — see Open questions surfaced below. `.agent-context.md` is not a sprint
  file; the orchestrator/PM should correct invariant line 54 to read
  "pointer-swap via `atomic.Pointer[blocklist.Trie]`" when this ADR is accepted.
- **`atomic.Pointer[T]` is a Go 1.19+ generic API.** The project's toolchain pin
  is `go 1.22.2` (`.agent-context.md` § Stack), well above the floor. No
  toolchain-bump interaction; this is independent of the open Go 1.24 question.

### Neutral

- **No write-side mutual exclusion between concurrent refreshes.** The Sprint 3
  scheduler is single-goroutine (one cron tick at a time; a refresh that
  overruns the next tick is the scheduler's concern, not the swap's). If a
  future design ever has two goroutines calling `Swap` concurrently,
  `atomic.Pointer.Store` is itself safe — the last store wins, and every reader
  sees a fully-consistent trie either way. No `RWMutex` is needed to make
  concurrent writers safe here; that is a property of publishing immutable
  snapshots, not of the lock.

## Alternatives considered

### Pointer-swap under `sync.RWMutex`

The reading in `.agent-context.md` line 54 and the decisions log.

**Why not:** it adds `RLock`/`RUnlock` to every `/check` — a shared-cacheline
read-modify-write on the mutex's internal reader counter — to protect a pointer
read that `atomic.Pointer` makes safe with a single uncontended atomic load. The
`RWMutex` is the right tool when readers must hold the lock *across* a
multi-step read of mutable shared state; here the reader does exactly one thing
(grab a pointer) and the pointee is immutable. The lock guards nothing the
atomic does not already guard, and costs reader contention under the exact
high-concurrency conditions the daemon is built for. The Go addendum §15 draws
this line explicitly.

### `RWMutex` guarding the trie's *internals* (lock the structure, not the pointer)

`/check` takes `RLock` and walks the live trie; refresh takes `Lock` and mutates
the trie in place.

**Why not:** this contradicts the trie's documented design — `Trie` is
"immutable-after-construction" and "not safe for concurrent writes." It would
require rewriting the trie to support in-place mutation under a lock, hold the
read lock for the *entire* O(prefix-length) bit walk (up to 128 comparisons for
IPv6) rather than for a single pointer grab, and serialize a refresh against
every in-flight check. Strictly worse on every axis. Rejected without
reservation.

### `sync.Map` or a channel-published snapshot

Store the trie in a `sync.Map` under a fixed key, or have the fetcher publish
new tries on a channel that a reader goroutine consumes.

**Why not:** `sync.Map` is built for many-key, churny workloads — a single-key
use is a misfit and still costs more than `atomic.Pointer`. A channel adds a
relay goroutine with its own lifecycle and exit path (Go addendum §15:
"every goroutine has a documented exit path") for no benefit over a direct
atomic store. KISS (coding-standards §15): the boring primitive is the right
one.

## Open questions surfaced

- **OQ-SWAP-1 (new — proposed for the `docs/BitBlocker.md` Open Questions
  table, for PM to land).** The decisions log and Milestones table in
  `docs/BitBlocker.md`, and the § Key invariants line 54 of `.agent-context.md`,
  describe the swap mechanism as "pointer-swap under `RWMutex`." This ADR
  supersedes that with `atomic.Pointer[blocklist.Trie]`. PM action: on
  acceptance of this ADR, add a decisions-log row dated 2026-05-15 recording the
  mechanism choice (pointing here), and correct any "under RWMutex" phrasing in
  the sprint file to "via `atomic.Pointer`." The `.agent-context.md` invariant
  is an agent-context file, not a sprint file — the orchestrator should correct
  line 54 in the same change. Architect does not edit either file directly
  (PM is the sole sprint-file writer; the agent-context file is the
  orchestrator's).

## Cross-references

- `docs/bitblocker-spec.md` § Blocklist update strategy ("Do not apply a partial
  or corrupted update — atomic swap only").
- `docs/interfaces/blocklist-source.md` (the contract this ADR's `Source` type
  must satisfy — the precise interface spec for the Developer).
- `docs/adr/0002-disk-cache-snapshot-format.md` (the disk-cache slice; calls
  `Source.Swap` on a successful snapshot load).
- `internal/server/server.go` (`Lookup` / `LookupSource` — the consumer-side
  seam, unchanged by this ADR).
- `internal/blocklist/trie.go` (the immutable-after-construction `Trie` whose
  invariant this design relies on).
- `internal/mmdb/loader.go` (`LoadCountryBlocklist` — returns the `*Trie` that
  gets swapped in).
- Coding standards `coding-standards-go.md` §15 Concurrency (the
  `atomic.Pointer[T]`-for-immutable-payloads guidance), §4 (explicit
  boundaries).

---

*End of ADR 0001.*
