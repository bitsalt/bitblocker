# Interface: fail-open wiring and readiness observability

> **Boundary owner:** `internal/server` (the readiness gate, `/check`, `/healthz`,
> and the readiness-state tracker).
> **Consumers:** `cmd/bitblocker` (wiring), Traefik (`/check` via `forwardAuth`),
> orchestrators and operators (`/healthz`, logs).
> **Governing standards:** `coding-standards.md` §14 (API / interface design),
> §4 (explicit boundaries); `coding-standards-go.md` §15 (concurrency),
> §10 (doc comments), §1 (tests).
> **Decisions:** ADR 0004 (this slice), ADR 0002 (disk cache — narrows the
> window), ADR 0001 (the swap seam this reads through).

This spec is precise enough for the Developer to implement the fail-open slice
with no further design decisions. Where a sub-decision is genuinely open it is
flagged as an OQ in ADR 0004 and called out here. Do not re-derive the reasoning
— it is in ADR 0004; this document is the contract.

---

## 1. Purpose of the boundary

`behavior.startup_mode` is declared, validated, and logged but consumed nowhere
(`internal/config/config.go`; `cmd/bitblocker/main.go:61`). This slice wires it,
and — because a silently-allow-all daemon is indistinguishable from a healthy one
— pairs it with a mandatory observability contract that makes a persistent
fail-open state loud on three independent channels: logs, `/healthz`, and the
orchestrator health check keyed on `/healthz`.

Three things meet here:

1. **The readiness predicate** — one boolean over the active `Lookup`, already
   present at `server.go:172` and `server.go:228`, now named and shared.
2. **The decision branch** — what `/check` returns when the predicate says
   "unusable," which is where `startup_mode` finally has an effect.
3. **The readiness-state tracker** — the small piece of state that turns an
   instantaneous predicate into the durable, reportable signals an operator needs.

---

## 2. The readiness predicate

### 2.1 Definition

```
blocklist is USABLE   ⇔  lookup != nil && lookup.Len() > 0
blocklist is UNUSABLE  ⇔  lookup == nil || lookup.Len() == 0
```

This is exactly today's condition at `server.go:172`. It does not change. Give
it a name (suggested: a small `func usable(l Lookup) bool` helper in
`internal/server`) so `/check`, `/healthz`, and the tracker cannot drift apart.

**`lookup` must be obtained by a single call to `s.lookup()` per request** and
reused for both the predicate and the subsequent `Contains`. Do not call
`s.lookup()` twice in one handler — a concurrent `Swap` between the calls would
let a request evaluate two different tries.

### 2.2 Fail-open engagement predicate — the exact condition

```
fail-open ENGAGES  ⇔  cfg.Behavior.StartupMode == config.StartupFailOpen
                      AND blocklist is UNUSABLE
```

Nothing else engages it. In particular:

- **Not** on an unparseable client IP (see §3.3). This is the hard boundary.
- **Not** on a refresh failure. Verified: every failure path in
  `fetcher.Refresh` returns before `f.source.Swap(trie)`
  (`internal/fetcher/fetcher.go:139-170`), so a failed refresh leaves a populated
  trie in place and the predicate stays USABLE. No change is needed in
  `internal/fetcher` or `internal/scheduler` for this slice — **do not touch
  them.**
- **Not** on a `/healthz` request. `/healthz` never consults `startup_mode`.

### 2.3 The two UNUSABLE sub-states

| Sub-state | Reached when | Operator's fix |
|---|---|---|
| `never-ready` | No `Swap` has ever published a usable trie — cold start with no usable disk cache and no successful fetch | Network / DB-IP reachability / cache permissions |
| `ready-then-empty` | A `Swap` published a trie with `Len() == 0` (ADR 0002 §C — a valid MMDB matching no configured country) | Correct `block.countries` |

Distinguished by the latched `ever_ready` flag (§4.1). Both engage fail-open
identically; only the reporting differs.

---

## 3. `/check` contract

### 3.1 Decision table

Evaluated top to bottom; first match wins.

| # | Condition | Status | Notes |
|---|---|---|---|
| 1 | Method != GET | `405` + `Allow: GET` | Unchanged |
| 2 | UNUSABLE && `startup_mode == fail-closed` | `cfg.Behavior.ResponseCode` | Unchanged behavior; **logging changes** (§4.4) |
| 3 | UNUSABLE && `startup_mode == fail-open` | `200` | **New.** Client IP is not extracted |
| 4 | USABLE && client IP unparseable | `cfg.Behavior.ResponseCode` | Unchanged — **always fail-closed** |
| 5 | USABLE && `lookup.Contains(addr)` | `cfg.Behavior.ResponseCode` | Unchanged |
| 6 | USABLE && !`Contains(addr)` | `200` | Unchanged |

Body is always empty on every row (unchanged).

### 3.2 Row 3 — skip IP extraction

Under row 3 the response does not depend on the client address, so
`extractClientIP` is not called. Rationale: calling it would emit a misleading
"fail-closed (unparseable client IP)" signal for a request that is about to be
allowed regardless, and it burns work on the hot path for no decision value.

### 3.3 Row 4 — the carve-out that must not be generalized

**`startup_mode` must not be consulted on row 4.** An unparseable
`X-Real-IP` / `X-Forwarded-For` is attacker-influenced input, not a symptom of
missing data. Honoring fail-open there would let any client bypass the blocklist
by sending a malformed header — a total bypass available even to a daemon with a
fully populated trie. Row 4 is fail-closed under both modes, permanently.

The Developer's tests must include an explicit regression guard for this: a
USABLE blocklist + `startup_mode: fail-open` + malformed `X-Real-IP` must still
return `cfg.Behavior.ResponseCode`.

### 3.4 Counter side-effect

Row 3 increments the fail-open allow counters (§4.1). No other row touches them.
Row 3 emits **no per-request log line** (§4.4).

---

## 4. Readiness-state tracker and log contract

### 4.1 State

State owned by `*Server` (it is the only component that observes per-request
readiness). All fields are accessed from many request goroutines plus the
heartbeat goroutine, so all access is atomic or mutex-guarded —
`coding-standards-go.md` §15; `go test -race` is mandatory.

| Field | Type | Meaning |
|---|---|---|
| `everReady` | latched bool | Set true the first time the predicate is observed USABLE. Never reset. Distinguishes §2.3's sub-states |
| `unusableSince` | timestamp, zero when USABLE | When the current UNUSABLE window began |
| `failOpenAllowed` | counter | Total requests allowed via row 3 since process start |
| `failOpenAllowedWindow` | counter | Requests allowed via row 3 since the last heartbeat; reset on each heartbeat emission |

**Transition detection** happens on each `/check` and `/healthz` evaluation of
the predicate: compare the observed usability against the tracker's current
state and emit §4.2 / §4.3 on a change. The transition emit must fire exactly
once per transition even under concurrent requests (compare-and-swap on the
state, or a mutex around the transition path — not around the whole handler).

Injected clock: the tracker takes a `now func() time.Time` defaulting to
`time.Now`, per `coding-standards-go.md` §4, so durations are testable without
wall time. Same for the heartbeat ticker — inject the ticker channel or a
`newTicker` func so tests do not sleep.

### 4.2 Signal — entering the UNUSABLE state

Emitted once per transition into UNUSABLE, **including the initial cold-start
entry at process start**.

- **Level:** `ERROR` — under **both** modes. A daemon that cannot make
  authorization decisions has failed at its only job.
- **Message (fail-closed):** `check: blocklist unusable; denying all requests`
- **Message (fail-open):** `check: blocklist unusable; ALLOWING ALL REQUESTS (startup_mode=fail-open)`
- **Fields:** `startup_mode`, `ever_ready` (bool), `serving`
  (`"deny-all"` | `"allow-all"`).

### 4.3 Signal — the recurring heartbeat (the load-bearing one)

Emitted every **60 seconds** for as long as the state remains UNUSABLE. Runs on
a goroutine owned by `Server.Run`'s lifecycle and stops on context cancellation.
It fires on wall-clock cadence, **not** on request activity — a daemon receiving
zero traffic while inert must still report.

- **Level:** `ERROR`, under both modes.
- **Message:** `check: blocklist still unusable`
- **Fields:**

| Field | Type | Notes |
|---|---|---|
| `startup_mode` | string | |
| `serving` | string | `"deny-all"` \| `"allow-all"` |
| `ever_ready` | bool | **`false` here means the daemon has never functioned** — the dead-daemon case |
| `unusable_for` | duration string | e.g. `"72h14m"` |
| `failopen_allowed_total` | int | Omit under fail-closed |
| `failopen_allowed_since_last` | int | Omit under fail-closed; reset after emit |
| `likely_cause` | string | `"no successful fetch since start"` when `ever_ready==false`; `"blocklist loaded but matched no configured countries — check block.countries"` when `ever_ready==true` |

**This signal is not suppressible** (ADR 0004 §E): it must not be gated behind
`behavior.log_blocked`, `behavior.log_allowed`, or any new flag. `logging.level`
already tops out at `error`, so a valid config cannot silence it.

The first heartbeat fires 60s after entering the state, not immediately — §4.2
already covers t=0.

### 4.4 Signal — leaving the UNUSABLE state

Emitted once per transition into USABLE.

- **Level:** `INFO` — recovery is good news, not a fault.
- **Message:** `check: blocklist now usable; normal enforcement resumed`
- **Fields:** `unusable_for` (total duration of the window just ended),
  `failopen_allowed_total_window` (requests allowed via row 3 during that
  window; omit under fail-closed), `prefixes` (`lookup.Len()`).

### 4.5 Removal of the existing per-request WARN

`server.go:173` currently emits a `WARN` **per request** while the blocklist is
empty:

```go
logger.Warn("check: fail-closed (blocklist not ready)", ...)
```

**Delete it.** Behind Traefik at real request rates it is a log flood during
exactly the incident an operator needs to read the logs through. Its information
is fully carried by §4.2 (the transition) and §4.3 (the heartbeat, with counts).
Removing it is in scope for this pass.

The row-4 unparseable-IP `WARN` at `server.go:183` is **unchanged** — it is
genuinely per-request and rate-bounded by how often malformed headers arrive.

### 4.6 Volume summary

| Situation | Log volume |
|---|---|
| Healthy daemon | Nothing from this contract |
| Normal restart with a warm cache | Nothing (predicate never goes UNUSABLE) |
| Cold start, fetch succeeds in 20s | 1 ERROR + 1 INFO |
| Cold start, fetch succeeds in 5min | 1 ERROR + 4 heartbeats + 1 INFO |
| Permanently inert daemon | 1 ERROR + 1440 ERROR/day, forever — intentional |

---

## 5. `/healthz` contract

### 5.1 Status codes — unchanged, and independent of `startup_mode`

| Predicate | Status |
|---|---|
| USABLE | `200` |
| UNUSABLE | `503` |

**`/healthz` never consults `startup_mode`.** A fail-open daemon with no
blocklist returns `503`. This is deliberate and is the single most important
line in this spec after §3.3 — see ADR 0004 §C. A future reader optimizing for
"stop my orchestrator restarting the container" will be tempted to change it;
they must read ADR 0004 §C first.

### 5.2 Body — additive fields only

Existing `status` values `"ok"` and `"empty"` are **preserved verbatim**; no new
`status` value is introduced, so existing consumers keep working (coding
standards §14 — extend, do not redefine).

USABLE (`200`):

```json
{"status":"ok","serving":"enforcing","ever_ready":true,"prefixes":184213}
```

UNUSABLE (`503`), fail-closed:

```json
{"status":"empty","serving":"deny-all","ever_ready":false,"empty_for_seconds":3721}
```

UNUSABLE (`503`), fail-open:

```json
{"status":"empty","serving":"allow-all","ever_ready":false,"empty_for_seconds":3721}
```

### 5.3 Field contract

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `status` | string | always | `"ok"` \| `"empty"` — unchanged domain |
| `serving` | string | always | `"enforcing"` \| `"deny-all"` \| `"allow-all"` — the posture, externally legible |
| `ever_ready` | bool | always | False ⇒ the daemon has never held a usable blocklist |
| `prefixes` | int | USABLE only | `lookup.Len()` |
| `empty_for_seconds` | int | UNUSABLE only | Integer seconds since entering the current UNUSABLE window |

`serving: "allow-all"` combined with a large `empty_for_seconds` and
`ever_ready: false` is the machine-readable form of "this daemon is dead code" —
scriptable in one `curl | jq` without touching logs.

Method handling (405 on non-GET) and `Content-Type: application/json` are
unchanged.

---

## 6. Wiring changes

### 6.1 `server.Options` — one new field

```go
// StartupMode selects the /check behavior while the blocklist is
// unusable: fail-closed denies, fail-open allows. It has no effect
// once a usable blocklist is loaded, and never affects /healthz or
// the unparseable-client-IP path. See ADR 0004.
StartupMode config.StartupMode
```

Validate in `New` with the existing `config` predicate shape: reject anything
that is neither `StartupFailClosed` nor `StartupFailOpen`, consistent with how
`BlockStatus` is validated at `server.go:86`. Do not default a zero value
silently — an unset `StartupMode` is a wiring bug and should error, matching the
"All fields are required — the constructor applies no defaults of its own"
contract already stated in `Options`' doc comment.

### 6.2 `cmd/bitblocker/main.go`

Pass it through in the existing `server.New(server.Options{...})` literal
(currently lines 71-78):

```go
StartupMode: cfg.Behavior.StartupMode,
```

That is the entire wiring change. **No other change to `main.go`**; in
particular `loadDiskCache`, `newFetcher`, `buildScheduler`, and the
`newLookupSource` closure (including its untyped-nil contract, interface spec
`blocklist-source.md` §2.5) are untouched.

### 6.3 Config schema

**No change.** `behavior.startup_mode` already exists with the right type,
default, and validation. This slice adds no config fields — a deliberate
outcome: the heartbeat interval is a constant, not a knob (ADR 0004
§Consequences/Neutral, OQ-FAILOPEN-2).

---

## 7. Versioning stance

- `server.Options` is an `internal/` type — no external consumers, ordinary code
  change.
- **`/check` is the Traefik `forwardAuth` contract.** Its status-code semantics
  are unchanged for every previously-reachable input; row 3 changes behavior only
  for a configuration that previously could not be selected meaningfully (it was
  declared but inert). No consumer breakage.
- **`/healthz` is a documented public contract**
  (`docs/bitblocker-spec.md` § `GET /healthz`). This slice is **additive only**:
  same status codes, same `status` value domain, new sibling fields. A consumer
  parsing only `status` or only the status code is unaffected. Future changes to
  this body must remain additive; changing the `status` domain would be a
  breaking change requiring its own ADR.
- The log message strings in §4 are an operator-facing contract in practice
  (people grep them). Treat changes to them as user-visible and note them in the
  release notes.

---

## 8. Test obligations (per `coding-standards-go.md` §1)

`httptest`-based, table-driven, alongside the existing handler tests.

**`/check` decision table** — all six rows of §3.1 under both `startup_mode`
values (twelve cases), against stubbed `Lookup` values covering `nil`,
`Len()==0`, and populated.

**Regression guards (each is a named test, not a table row):**

- **§3.3** — USABLE + `fail-open` + malformed `X-Real-IP` ⇒ blocked, not allowed.
  This is the bypass this spec exists to prevent.
- **§5.1** — UNUSABLE + `fail-open` ⇒ `/healthz` is `503`, not `200`.
- **`blocklist-source.md` §2.5** — the cold-start path through the real
  `newLookupSource` closure (untyped nil) must not panic under `fail-open`; row 3
  must not call `Len()` on a nil interface.

**State tracker:**

- `ever_ready` latches on first USABLE observation and never resets after a
  return to UNUSABLE.
- Transition signals fire exactly once per transition under concurrent requests
  (`go test -race`, many goroutines hammering `/check` across a `Swap`).
- Heartbeat fires on the injected clock with no request traffic at all — the
  inert-daemon case.
- Heartbeat is emitted at `ERROR` and carries `ever_ready:false` +
  `likely_cause` for never-ready, and `ever_ready:true` + the
  `block.countries` cause for ready-then-empty.
- `failopen_allowed_since_last` resets after each heartbeat;
  `failopen_allowed_total` does not.
- Fail-closed mode emits no `failopen_allowed_*` fields.

**Negative:** assert the removed per-request `WARN` (§4.5) is gone — N requests
under UNUSABLE produce zero per-request lines from this path.

Capture logs via an `slog` handler writing to a buffer, consistent with the
existing `internal/logging` test approach.

---

## 9. Cross-references

- `docs/adr/0004-fail-open-wiring-and-readiness-observability.md` — the decision
  and its reasoning. Read it before changing anything here.
- `docs/adr/0002-disk-cache-snapshot-format.md` §C — why the cache makes this
  window rare, and the `Len()==0` case behind §2.3.
- `docs/interfaces/blocklist-source.md` §2.5 — the untyped-nil contract this
  slice must not break.
- `internal/server/server.go` — `handleCheck` (163), `handleHealthz` (218), the
  predicate (172, 228), the WARN to remove (173).
- `internal/fetcher/fetcher.go:139-170` — evidence that a failed refresh never
  empties the blocklist.
- `internal/config/config.go:15-24, 107-112, 245-254` — the existing knob.
- `docs/bitblocker-spec.md` § `GET /healthz`, § HTTP interface.
- Coding standards §14, §4; `coding-standards-go.md` §15, §4, §1, §10.
</content>
