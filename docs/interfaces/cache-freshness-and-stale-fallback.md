# Interface: disk-cache freshness and stale-cache fallback

> **Boundary owner:** `internal/diskcache` (the on-disk snapshot's freshness
> semantics) and `internal/fetcher` (the only writer of that freshness).
> **Consumers:** `cmd/bitblocker` (the startup read), the fail-closed cold-start
> posture, and any operator reasoning about `cache.max_age`.
> **Governing standards:** `coding-standards.md` §14 (interface design), §4
> (explicit boundaries); `coding-standards-go.md` §4 (injected clock), §1
> (tests), §6 (error wrapping), §10 (doc comments).
> **Decisions:** ADR 0005 §3.3/§3.5 (this slice), ADR 0002 (the cache contract
> being repaired), ADR 0004 §E.2 (the security bound that depends on it).

This spec is precise enough for a Developer to implement without re-deriving the
design. **Nothing here is implemented.** Fix A is a defect fix that should land
before enforcement is turned on for the app01 rollout (ADR 0005 §5.1, Phase 4);
Fix B is a Sprint 5 improvement. They are independent — Fix A can ship alone.

Do not re-derive the reasoning; it is in ADR 0005 §3.3–§3.5.

---

## 1. The defect this spec repairs

ADR 0002 §C gave the disk cache a staleness bound (`cache.max_age`, default
`48h`) whose stated purpose is to keep "a cache written by yesterday's refresh"
while rejecting "a cache from a daemon that has been down for days." ADR 0004
§E.2 then costed the `fail-open` security trade-off against that bound, on the
basis that a routine restart during an upstream outage serves cached data.

**The bound does not hold in the steady state.** Traced through the code:

| Step | Evidence |
|---|---|
| Staleness is measured against the cache file's **mtime** | `internal/diskcache/cache.go:108` — `now.Sub(info.ModTime()) > maxAge` |
| mtime advances **only** on `OutcomeUpdated` (a 200 that writes fresh bytes) | `internal/fetcher/fetcher.go:157` — `diskcache.Write` |
| A `304 Not Modified` returns **before** any cache write | `internal/fetcher/fetcher.go:147-150` |
| The fetcher sends conditional-GET validators from the last download | `internal/fetcher/fetcher.go:216-221` |
| DB-IP publishes **monthly**; the default refresh cadence is **daily** | ADR 0003; `internal/config/config.go:163` (`0 3 * * *`) |

So a healthy, long-running daemon downloads once, then receives `304` on every
subsequent daily fetch for the rest of the month. **The cache file's mtime is
pinned to the last actual download and exceeds `max_age` within two days**, then
stays stale for the remaining ~28 days.

And a stale cache is not merely skipped — it is **deleted**
(`cmd/bitblocker/main.go:195-199`, `removeUnusableCache`; OQ-CACHE-2 ratified
2026-07-19). The daemon then starts UNUSABLE with nothing on disk to fall back
to, and restarting again does not undo the deletion.

Net effect: the uncovered window is not "a restart after >48h of both downtime
and fetch failure" (ADR 0004 §E.2) but **"any restart that coincides with a
fetch failure"** — because the cache is stale-and-therefore-deleted for most of
any given month. If that fetch also fails through the cold-start budget (8
attempts, ~4m14s — `internal/scheduler/scheduler.go:19-23, 164-194`), the daemon
waits for the next cron tick, up to ~24h, denying every request the whole time.

### 1.1 Prerequisite verification (ADR 0005 OQ-D)

The mechanism depends on `download.db-ip.com` actually serving conditional-GET
validators. If it serves neither `ETag` nor `Last-Modified`, `f.etag` /
`f.lastModified` stay empty, every daily fetch is unconditional, every fetch
rewrites the cache, and the mtime is never more than 24h old — **the defect does
not exist.**

```
curl -sSI https://download.db-ip.com/free/dbip-country-lite-2026-08.mmdb.gz | grep -iE '^(etag|last-modified):'
```

Run this before scheduling the work. Fix A is correct and harmless either way;
the result decides whether it is urgent or merely tidy. Record the result in the
PR that lands Fix A.

---

## 2. Fix A — a 304 refreshes the cache's freshness stamp

### 2.1 The semantic claim

A `304 Not Modified` is **affirmative upstream confirmation that the cached
bytes are the currently-published artifact.** That is evidence of freshness at
least as strong as a `200` (which only says new bytes arrived). `cache.max_age`
asks "how old is this geo data?", and after a 304 the honest answer is "it is
the current published data, as of now."

Therefore: on `OutcomeUnchanged`, advance the cache file's modification time.

### 2.2 Contract

| Property | Value |
|---|---|
| Trigger | `fetcher.Refresh` takes the `notModified` branch (`fetcher.go:147-150`) |
| Effect | The file at `f.cachePath` has its mtime (and atime) set to `f.now()` |
| Failure | **Non-fatal.** Log `WARN` and return `OutcomeUnchanged, nil` as today. |
| File absent | **Not an error.** Log at `DEBUG` and continue. |
| Content | **Never modified.** This touches metadata only; no bytes are read or written. |
| Return value | Unchanged — still `(OutcomeUnchanged, nil)` |

`Refresh`'s contract to the scheduler is unchanged: a 304 is still a success,
still swaps nothing, still leaves the active trie alone.

### 2.3 Where it goes

A new exported function in `internal/diskcache`, so the freshness semantics stay
owned by the package that defines them and `internal/fetcher` does not grow a
direct `os` dependency for cache-file metadata:

```go
// Touch advances the cache file's modification time to now, marking the
// cached bytes as confirmed-current without rewriting them.
//
// Call this when the upstream reports the artifact unchanged (HTTP 304):
// a 304 is affirmative confirmation that the cached copy IS the currently
// published artifact, which is what Load's max-age bound is asking about.
// Without it the mtime advances only on an actual download — monthly for
// DB-IP — so a healthy daemon's cache goes stale within days and is
// deleted on the next restart. See ADR 0005 §3.3.
//
// A missing file returns ErrAbsent; the caller treats that as benign.
// All other failures are non-fatal to the caller: the cache is an
// optimization for the next start, never a correctness dependency.
func Touch(path string, now time.Time) error
```

Implementation is `os.Chtimes(path, now, now)`, mapping `os.ErrNotExist` to
`ErrAbsent` and wrapping anything else per `coding-standards-go.md` §6.

`now` is a parameter, not `time.Now()` internally, matching `Load`'s existing
injected-clock shape (`cache.go:99`) and `coding-standards-go.md` §4. The
`Fetcher` already holds an injectable `f.now` (`fetcher.go:65, 115-118`), so the
call site is `diskcache.Touch(f.cachePath, f.now())`.

Call site, inside the existing `notModified` branch:

```go
if notModified {
    f.logger.Debug("fetcher: source unchanged (304); retaining active blocklist")
    if terr := diskcache.Touch(f.cachePath, f.now()); terr != nil && !errors.Is(terr, diskcache.ErrAbsent) {
        f.logger.Warn("fetcher: could not refresh cache freshness stamp", "path", f.cachePath, "error", terr)
    }
    return OutcomeUnchanged, nil
}
```

**No other change to `fetcher.go`.** In particular the validators (`f.etag`,
`f.lastModified`) are untouched, `Swap` is not called, and the 200 path's
`diskcache.Write` already sets a fresh mtime and needs nothing.

### 2.4 What this changes about the system

- A healthy daemon's cache is never older than one refresh interval. Every
  routine restart — deploy, ansible apply, host reboot — finds a fresh cache and
  never enters the UNUSABLE state at all.
- The uncovered window collapses to exactly the bound ADR 0004 §E.2 claimed:
  **a restart after >48h of continuous fetch failure.**
- Month-rollover is unaffected. When the URL changes to the new month's
  filename, the stored validators do not match the new resource, the server
  returns 200, and the normal download path runs. If the new month is not yet
  published, the fetcher falls back to the prior month's URL
  (`fetcher.go:192-202`), gets a 304, and touches — correctly.
- Nothing about `Load`, `max_age`, or the fail-closed contract changes. This
  makes an existing bound work; it does not relax one.

### 2.5 Test obligations

- **Touch on 304.** Stub server returns 304; assert the cache file's mtime
  advanced to the injected clock's value and its **bytes are byte-identical**
  before and after.
- **Touch is skipped benignly when the file is absent.** 304 with no cache file
  ⇒ `Refresh` returns `(OutcomeUnchanged, nil)`, no error surfaced, no panic.
- **Touch failure is non-fatal.** Make `Chtimes` fail (read-only directory, or
  inject the error) ⇒ `Refresh` still returns `(OutcomeUnchanged, nil)` and logs
  `WARN`.
- **The 200 path is unchanged.** Assert `Write` still sets the mtime and that
  `Touch` is not additionally invoked in a way that changes behavior.
- **The regression this fixes, end to end** — the test that would have caught
  it: a fetcher whose first refresh downloads (200) and whose next N refreshes
  are 304s, driven by an injected clock advanced past `max_age`, followed by a
  `diskcache.Load` at the advanced time ⇒ **must not** return `ErrStale`. Name
  this test after the defect so its purpose survives.
- `t.TempDir()` for all cache files (`coding-standards-go.md` §1).

---

## 3. Fix B — a stale cache becomes a last-resort fallback instead of being deleted

**Recommended, Sprint 5. Independent of Fix A and lower priority.** Fix A removes
the *common* path to a total outage; Fix B removes the *tail*.

### 3.1 The claim

Serving month-old geo data is materially better than serving nothing. Its
failure mode is a handful of reassigned prefixes — a few IPs decided wrongly.
The alternative failure mode is every attached site returning 403 to every
visitor for up to 24 hours.

Crucially this **preserves the fail-closed contract exactly**: the daemon still
never allows traffic it cannot evaluate. It evaluates against older data, loudly,
rather than refusing to evaluate at all. This is not a step toward fail-open and
must not be described as one.

### 3.2 Contract

Startup ordering changes as follows. Steps 1–2 are today's behavior with one
deletion removed; steps 3–4 are new.

| Step | Condition | Behavior |
|---|---|---|
| 1 | Cache absent | Log `INFO`, cold start. **Unchanged.** |
| 2 | Cache present and fresh (`age <= max_age`) | Load, `Swap`, serve. **Unchanged.** |
| 3 | Cache present but **stale** | Log `WARN`, **do not load, and DO NOT DELETE** — retain it as the fallback candidate. *(Changed: today it is deleted.)* |
| 4 | Cache present but **corrupt / unreadable** | Log `WARN` and **delete**, exactly as today. A corrupt file is not a fallback candidate. **Unchanged.** |
| 5 | Cold-start retry budget exhausted **and** the blocklist is still UNUSABLE **and** a retained stale cache exists | Load it, `Swap` it, and emit the §3.4 signals. **New.** |
| 6 | The stale cache also fails to load at step 5 | Delete it, stay UNUSABLE, fail-closed. **New.** |

**Step 3 partially reverses OQ-CACHE-2** (ratified 2026-07-19: "remove a corrupt
or stale cache file on detection"). The reversal is narrow and deliberate:
OQ-CACHE-2's reasoning was that an unusable file re-trips the same failed load
and the same WARN on every start, producing a permanent noisy-but-harmless error
operators learn to ignore. That reasoning is entirely correct for a **corrupt**
file, which can never become useful, and step 4 keeps it. It does not hold for a
**stale** file, which is a perfectly valid MMDB that is merely old and is the
only recovery material available when the network is gone. Any PR implementing
Fix B must say this explicitly in its body and update the OQ-CACHE-2 decision
record; do not let it read as an accidental regression.

### 3.3 Where the trigger lives

Step 5 fires where the cold-start budget is exhausted —
`scheduler.runColdStart`'s `attempt+1 >= s.coldStart.MaxAttempts` branch
(`internal/scheduler/scheduler.go:181-185`) — but the scheduler must **not**
grow a `diskcache` dependency. Add an optional injected callback to
`scheduler.Options`:

```go
// OnColdStartExhausted is invoked once if the cold-start retry budget is
// exhausted while Ready() is still false. It is the seam for a
// last-resort recovery attempt (see docs/interfaces/
// cache-freshness-and-stale-fallback.md §3). Optional; nil is a no-op.
// It is called synchronously, before Run proceeds to the cron loop.
OnColdStartExhausted func(ctx context.Context)
```

`cmd/bitblocker` supplies the closure that attempts the stale load and calls
`src.Swap`. The scheduler stays a pure orchestrator with no knowledge of caches,
consistent with its current shape and `coding-standards.md` §4.

`internal/diskcache` gains a sibling to `Load` that performs the same work with
the age check bypassed and reports the age it loaded, so the caller can name it
in the log:

```go
// LoadStale reads and rebuilds the trie from path with NO max-age bound,
// returning the file's age alongside the trie. It is the last-resort
// recovery path only — call it exclusively after Load has returned
// ErrStale and the cold-start retry budget has been exhausted. Every
// other caller must use Load. See ADR 0005 §3.5 Fix B.
func LoadStale(path string, now time.Time, countries []config.CountryCode) (*blocklist.Trie, time.Duration, error)
```

### 3.4 Observability — non-negotiable

Serving stale data silently would be a worse defect than the one being fixed.
The state must be as loud as the UNUSABLE state it replaces.

**On entering stale-fallback — `ERROR`, once:**
`blocklist: serving STALE cached data after cold-start failure`
with fields `cache_age` (duration), `cache_path`, `prefixes`, `attempts`.

**Recurring — `ERROR`, every 60s while still stale-serving**, reusing the
existing heartbeat cadence and goroutine (`internal/server/server.go:184-200`):
`blocklist: still serving STALE cached data`
with `cache_age`, `stale_for`, `prefixes`.

**On recovery (a successful fetch `Swap`s fresh data) — `INFO`, once:**
`blocklist: fresh data restored; stale fallback ended`
with `stale_for`.

**`/healthz` must expose it.** Additively, per ADR 0004 §C and coding standards
§14 — the `status` value domain (`"ok"` | `"empty"`) does **not** change:

```json
{"status":"ok","serving":"enforcing-stale","ever_ready":true,"prefixes":184213,"cache_age_seconds":2851200}
```

`serving` gains one new value, **`"enforcing-stale"`**, alongside the existing
`"enforcing"` / `"deny-all"` / `"allow-all"`. `cache_age_seconds` is present only
in this state.

**Open sub-decision for the ratifying pass, flagged rather than decided:**
should `/healthz` return `200` or `503` while serving stale? Architect lean:
**`200`.** The daemon is making real authorization decisions, which is what
readiness means (ADR 0004 §C), and a 503 here would be read by an orchestrator
as "restart me" — the exact action §5.4 of ADR 0005 shows to be harmful. The
degradation is carried by `serving: "enforcing-stale"` and by the ERROR
heartbeat, which are the channels ADR 0004 §D established for precisely this
purpose. Confirm at the ADR that ratifies Fix B rather than deciding it in code.

### 3.5 Test obligations

- Stale cache present + cold-start budget exhausted ⇒ trie is populated from the
  stale file, `/check` enforces, `/healthz` reports `serving: "enforcing-stale"`
  with a plausible `cache_age_seconds`.
- Stale cache present + cold-start fetch **succeeds** ⇒ the fallback never
  fires, the stale file is overwritten by the fresh download, no ERROR emitted.
- Stale cache present + cold start exhausted + the stale file is **also corrupt**
  ⇒ it is deleted, the daemon stays UNUSABLE and fail-closed, and no partial
  trie is published.
- A **corrupt** (not stale) cache is still deleted on the startup read — step 4,
  a regression guard on the unchanged half of OQ-CACHE-2.
- Recovery: a later scheduled refresh succeeds ⇒ INFO emitted once,
  `serving` returns to `"enforcing"`, `cache_age_seconds` disappears from
  `/healthz`.
- `OnColdStartExhausted` is invoked exactly once, is not invoked when `Ready()`
  is true, and a nil callback is a no-op.
- `go test -race` throughout; the heartbeat's state is shared with request
  goroutines (`coding-standards-go.md` §15).

---

## 4. What is explicitly NOT changed by either fix

Stated so neither fix drifts into the posture question ADR 0004 settled:

- **`behavior.startup_mode` is untouched.** No new value, no new semantics, no
  new default. Fail-closed remains the default and the recommendation.
- **The fail-open readiness gate is untouched** (ADR 0004 §A). Fix B changes how
  often the UNUSABLE predicate is reached; it does not change what happens when
  it is.
- **The unparseable-client-IP branch stays fail-closed under both modes**
  (ADR 0004 §A.1, `server.go:245-260`). Nothing here goes near it.
- **`Source.Swap` remains the only mutator of the active trie** (ADR 0001).
  Fix B publishes through it like every other path.
- **The `status` value domain of `/healthz` is unchanged.** `serving` gains a
  value; `status` does not (coding standards §14 — extend, do not redefine).
- **No new config fields.** `cache.max_age` keeps its meaning and its `48h`
  default; Fix A makes that meaning true rather than changing it. Raising
  `max_age` to work around the defect is explicitly rejected in ADR 0005
  § Alternatives.

---

## 5. Versioning stance

- `diskcache.Touch` and `diskcache.LoadStale` are new exported functions in an
  `internal/` package — no external consumers, ordinary code change.
- `scheduler.Options.OnColdStartExhausted` is additive and optional; a nil value
  preserves today's behavior exactly.
- **`/healthz` is a documented public contract**
  (`docs/bitblocker-spec.md` § `GET /healthz`). Fix B is additive: same status
  code domain, same `status` value domain, one new `serving` value and one new
  optional field. A consumer parsing only `status` or only the status code is
  unaffected. A consumer matching `serving` against an exhaustive set would see
  a new value — note it in the v1.1 release notes.
- The log message strings in §3.4 are an operator-facing contract in practice
  (people grep and alert on them). Treat changes as user-visible.

---

## 6. Sequencing

1. **Run §1.1's `curl`.** Record the result. It decides urgency, not scope.
2. **Fix A** — one Developer pass, one QA pass. Should land before the app01
   rollout turns enforcement on (ADR 0005 §5.1, Phase 4). Small enough to ride
   with a patch
   release; per the project's own Sprint 4 lesson, rehearse the tag with an `rc`
   first.
3. **Fix B** — Sprint 5, after OQ-10's `fetcher.BaseURL` test seam, which is
   sequenced first in Sprint 5 precisely because it makes `cmd/bitblocker`
   wiring testable and Fix B's step-5 closure lives in exactly that wiring.
   Fix B wants its own short ADR to ratify §3.2's narrow reversal of OQ-CACHE-2
   and §3.4's `/healthz` sub-decision.

---

## 7. Cross-references

- `docs/adr/0005-shared-host-per-site-policy-attachment-and-fail-mode.md`
  §3.3–§3.5 — the finding and the decision. Read before changing anything here.
- `docs/adr/0004-fail-open-wiring-and-readiness-observability.md` §A.2, §C, §D,
  §E.2 — the posture this preserves and the security bound §2.4 restores. §E.2
  carries a correction block filed by the ADR 0005 pass.
- `docs/adr/0002-disk-cache-snapshot-format.md` §C, § Alternatives ("No
  staleness bound"), OQ-CACHE-2 — the contract being repaired and the decision
  Fix B narrowly reverses.
- `docs/interfaces/fail-open-and-readiness.md` §2, §4, §5 — the readiness
  predicate, the heartbeat contract Fix B reuses, and the `/healthz` field
  contract Fix B extends.
- `docs/interfaces/blocklist-source.md` §2.5 — the untyped-nil contract neither
  fix may break.
- `internal/fetcher/fetcher.go:139-170, 192-202, 216-221`
- `internal/diskcache/cache.go:99-120`
- `cmd/bitblocker/main.go:190-221`
- `internal/scheduler/scheduler.go:19-23, 164-194`
- `internal/server/server.go:184-200, 280-330`
- Coding standards §14, §4; `coding-standards-go.md` §1, §4, §6, §10, §15.
