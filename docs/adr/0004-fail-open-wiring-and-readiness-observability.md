# ADR 0004: `behavior.startup_mode: fail-open` is wired at the `/check` readiness gate only, paired with a mandatory recurring ERROR heartbeat; `/healthz` stays 503 while the blocklist is empty

- **Status:** accepted — ratified by the operator 2026-07-20. **OQ-FAILOPEN-1** resolved: hold the v1.0 tag for this work rather than tagging v1.0 now and shipping fail-open in v1.0.1 (Architect lean confirmed).
- **Date:** 2026-07-20
- **Deciders:** Jeff (operator) — the decision to wire `fail-open` rather than drop or defer it (2026-07-20); Architect — the engagement predicate, the `/healthz` semantics, and the observability contract
- **Amends:** `docs/BitBlocker.md` decisions log 2026-04-22 — "Cold-start fail mode: fail-closed with guardrails." The **default posture is unchanged** (fail-closed remains the default and the recommended setting). What changes: the `startup_mode` knob listed there as a guardrail becomes *functional* rather than declarative, and the `/healthz` 503 guardrail is explicitly re-scoped to be independent of `startup_mode`.
- **Interacts with:** ADR 0002 (disk-cache snapshot) — the cache narrows the fail-open window in exactly the way it narrows the fail-closed window; no change to ADR 0002's contract.
- **Supersedes:** none
- **Superseded by:** none

## Context

`behavior.startup_mode` (`fail-closed` | `fail-open`, default `fail-closed`) has
been in the config schema since Sprint 1. It is parsed, defaulted, validated
(`internal/config/config.go` lines 15–24, 107–112, 166–171, 245–254, 278–280),
and logged at startup (`cmd/bitblocker/main.go:61`).

**It is not wired.** A repository-wide search for `StartupMode` finds consumers
only in `internal/config` and that one `main.go` log line. Neither
`internal/server`, `internal/fetcher`, nor `internal/scheduler` branches on its
value. `server.Options` has no field for it. An operator who sets
`startup_mode: "fail-open"` gets a valid config, a startup log line echoing
`fail-open`, and **fail-closed behavior**.

This was surfaced by the Technical Writer on 2026-07-19 while documenting the
config surface for PR #20 (`bitblocker:OQ-9`), and confirmed independently
against the code by this ADR.

A related record defect: `docs/BitBlocker.md:147` marks the Sprint 3 task
"`behavior.startup_mode` config knob" ✅ with the note that "this Sprint 3
task's scope was the fetcher/scheduler's cold-start path actually consulting it,
which PR #14 wires in." PR #14 did not wire it. That row is inaccurate and is
surfaced to PM separately (this ADR does not edit the sprint file).

**The operator's decision (2026-07-20): wire it.** Not drop it, not defer it.

### The requirement that shapes the design

The operator's stated requirement is not merely "make the knob work." It is:

> "I'd like to somehow know if this is a consistent state. If it's failing open
> consistently, at best we need to determine why and, at worst, the entire thing
> is dead code because it's never functioning."

A daemon serving allow-all because it has no data is, functionally, *not
running* — it passes every health check a naive orchestrator applies while doing
nothing. `fail-open` without detection is strictly worse than no `fail-open` at
all, because it converts a loud outage (everything 403s, operator notices in
minutes) into a silent one (everything passes, operator never notices). The
observability contract is therefore not an accompaniment to this decision; it is
the larger half of it.

### What this ADR must settle

- **A.** The exact predicate under which fail-open engages.
- **B.** `/check` behavior in each state.
- **C.** `/healthz` semantics under fail-open — the trap where wiring fail-open
  deletes the only existing not-doing-my-job signal.
- **D.** The v1.0 observability contract: signals, levels, cadence, volume.
- **E.** The security posture change and whether it warrants a guardrail.

## Decision

### A. Fail-open engages at exactly one predicate: the readiness gate

**Fail-open engages if and only if `startup_mode == "fail-open"` AND the active
blocklist is unusable**, where "unusable" is the predicate the server already
applies at `internal/server/server.go:172`:

```
lookup == nil || lookup.Len() == 0
```

This is one branch in `handleCheck`. Nothing else changes.

**Consequences of pinning it there, stated explicitly:**

1. **The unparseable-client-IP branch (`server.go:181-189`) stays fail-closed
   unconditionally.** `startup_mode` governs *data availability*, not *input
   validity*. A request whose `X-Real-IP` / `X-Forwarded-For` cannot be parsed is
   attacker-influenced input, not a symptom of a missing dataset; allowing it
   would hand any client a trivial bypass by sending a malformed header. This is
   the single most important boundary in this ADR and the Developer must not
   generalize the flag across both branches.

2. **A failed refresh never engages fail-open.** Verified against
   `internal/fetcher/fetcher.go`: every failure path in `Refresh` returns before
   `f.source.Swap(trie)` (line 165). A network error, a both-months 404
   (`ErrNotPublished`), a corrupt gzip, a cache-write failure, or a failed
   MMDB load all leave the previously published trie in place. `Source.Swap` is
   the *only* mutator, and it is called only after a fully successful load.
   Therefore a populated daemon whose refreshes are failing keeps blocking with
   stale data — correctly — and never enters fail-open.

3. **Therefore fail-open is a cold-start-only state in practice.** Reaching it
   requires a process start where the disk cache is absent, stale, or corrupt
   (`main.go:189-208`) *and* no fetch has yet succeeded. This bound is
   load-bearing for the security analysis in §E.

4. **Two distinguishable sub-states share the predicate, and the daemon must
   distinguish them in its signals.** `Len() == 0` is reachable two ways:
   - **never-ready** — no `Swap` has ever published a usable trie. This is the
     inert-daemon case the operator fears.
   - **ready-then-empty** — a `Swap` published a trie with `Len() == 0` (ADR 0002
     §C: a valid cache or fetch that matched no prefixes for the configured
     countries, e.g. a misconfigured `block.countries`).

   These have different causes and different fixes, so the observability
   contract carries an `ever_ready` discriminator (§D). The daemon derives it by
   latching "the lookup has been observed non-empty at least once" — no new
   plumbing through the fetcher is required.

### B. `/check` behavior

| State | `startup_mode` | `/check` response |
|---|---|---|
| Blocklist usable, IP parses, IP in blocklist | either | `behavior.response_code` (default 403) |
| Blocklist usable, IP parses, IP not in blocklist | either | `200` |
| Blocklist usable, IP unparseable | either | `behavior.response_code` — **always fail-closed** |
| Blocklist unusable (`nil` or `Len()==0`) | `fail-closed` | `behavior.response_code` (unchanged from today) |
| Blocklist unusable (`nil` or `Len()==0`) | `fail-open` | **`200`** — new behavior |

In the fail-open row, client-IP extraction is skipped entirely: the decision does
not depend on the address, and skipping it avoids emitting a misleading
unparseable-IP signal for a request that is being allowed regardless.

### C. `/healthz` stays 503 while the blocklist is empty, regardless of `startup_mode`

This is the design trap and it is resolved deliberately.

The tempting reasoning is: under `fail-open` the daemon is serving traffic
successfully, so `/healthz` should return 200. **Rejected.** `/healthz` is the
readiness probe, and readiness means "ready to make authorization decisions,"
not "the HTTP server is answering." A 200 here would mean:

- The one pre-existing signal that the daemon is not doing its job is deleted by
  the very feature most likely to hide that fact. Fail-open would mask itself.
- Container orchestrators, Traefik health checks, and any monitoring keyed on
  `/healthz` would report a fully healthy daemon that is blocking nothing.
- The operator's requirement — "know if this is a consistent state" — becomes
  unanswerable from the outside, leaving only log archaeology.

**Decision: `/healthz` returns `503` whenever the blocklist is unusable,
identically under both startup modes.** `startup_mode` changes what `/check`
does; it does not change what `/healthz` reports. The two endpoints answer
different questions and are deliberately decoupled.

**Response body gains additive discriminator fields.** The existing
`{"status":"ok"}` / `{"status":"empty"}` shape is preserved verbatim — the
`status` values do not change, so any existing consumer keeps working. Three
fields are added so an operator or a script can tell the states apart without
parsing logs. Full contract in the interface spec; the shape is:

```json
{"status":"empty","serving":"allow-all","ever_ready":false,"empty_for_seconds":3721}
```

`serving` is `"deny-all"` under fail-closed and `"allow-all"` under fail-open,
and is the field that makes the posture externally legible. Adding fields rather
than new `status` values keeps this backward-compatible; see the interface spec
§5 for the versioning stance.

### D. Observability contract for v1.0

Four signals. All are log-based; no metrics dependency (see §"v1.0 / v1.1 split"
below).

1. **Entering the unusable-blocklist serving state — `ERROR`, once per
   transition.** Level is `ERROR`, not `WARN`, under **both** modes: a daemon
   that cannot make authorization decisions has failed at its only job. Under
   fail-open it is additionally allowing everything.

2. **A recurring heartbeat while the state persists — `ERROR`, every 60s.** This
   is the signal that actually answers the operator's question. A single line at
   startup scrolls away and cannot distinguish "briefly empty during a normal
   restart" from "empty for six weeks." The heartbeat carries the duration, the
   requests-allowed-under-fail-open counters, and `ever_ready`. A daemon that has
   been inert since deployment emits one ERROR per minute, forever, saying so —
   which is exactly loud enough to be caught by any log-based alerting and by a
   human running `journalctl -u bitblocker -p err`.

3. **Leaving the state — `INFO`, once per transition**, carrying the total time
   spent unusable and the total requests allowed under fail-open. This closes the
   incident in the log and makes "how long was the window and what got through"
   answerable after the fact.

4. **No per-request logging of this state.** This also **fixes an existing
   defect**: `server.go:173` today emits a `WARN` *per request* while the
   blocklist is empty. Behind Traefik at real request rates that is a log flood
   during precisely the incident an operator needs to read the logs. It is
   replaced by the transition + heartbeat pair, with the per-request detail
   folded into the heartbeat's counters. Fixing this is in scope for the same
   Developer pass.

Exact messages, levels, fields, and cadence are specified in
`docs/interfaces/fail-open-and-readiness.md` §4.

### E. Security posture: fail-open ships without a hard guardrail, with a documented risk

The threat is real and worth stating plainly. Per ADR 0003 the DB-IP fetch is a
**keyless, predictable, publicly documented** monthly HTTPS GET of
`https://download.db-ip.com/free/dbip-country-lite-YYYY-MM.mmdb.gz`. An actor who
can induce a fetch failure — block the download, interfere with DNS, or simply
wait for a DB-IP outage — can prevent the daemon from acquiring data. Under
`fail-open`, that becomes "the daemon stops blocking entirely."

**Costed against three bounds:**

1. **The attack requires a restart, not just a fetch failure.** Per §A.2, a
   failed refresh cannot empty a populated trie. To reach fail-open the attacker
   must induce a fetch failure *and* have the daemon restart *and* have the disk
   cache be absent/stale/corrupt. An attacker who can force a restart of the
   target's daemon has capabilities that dwarf this bypass.
2. **The disk cache covers the realistic overlap.** ADR 0002's cache (default
   `max_age` 48h) means a routine restart during a DB-IP outage serves cached
   data and never reaches the predicate at all. The uncovered window is a restart
   after >48h of both downtime and fetch failure.
3. **The blast radius is bounded by what BitBlocker is.** It is scanning-noise
   reduction in front of an application, not an authentication boundary. Nothing
   behind it should be relying on it for access control; the spec is explicit
   that it is a noise filter. A total bypass returns the operator to the security
   posture they had before installing BitBlocker — degraded, not breached.

**Decision: no hard guardrail in v1.0.** Specifically, the tempting
"honor fail-open only for a bounded window, then revert to fail-closed" is
**rejected**: a daemon that silently flips its security posture partway through
an incident is harder to reason about than one that does what it was configured
to do, and it converts a legible state into a timing-dependent one. The operator
opted into availability-over-security explicitly, on a non-default knob.

What ships instead of a guardrail:

- **`fail-closed` remains the default** and the documented recommendation.
- **The ERROR heartbeat (§D.2) is not optional and is not suppressible** — it is
  not gated behind `behavior.log_allowed` or the `logging.level` intent of
  `warn`. An operator choosing fail-open cannot silently choose to also not know
  about it. (`logging.level: error` still emits it; only setting a level above
  `error`, which the schema does not permit, would suppress it.)
- **`/healthz` stays 503 (§C)**, so orchestrator-level health checks still
  register the degradation.
- **README documents the induced-failure risk** where `startup_mode` is
  described — the reader choosing `fail-open` should see the keyless-fetch
  attack path stated, not have to derive it.

## Consequences

### Positive

- **A declared, validated, documented config knob stops lying.** Shipping v1.0
  with a knob that silently does nothing is a known-defect release; PR #20's
  current "treat fail-open as reserved/not-yet-wired" caveat becomes unnecessary.
- **Fail-open cannot hide.** The transition + heartbeat + `/healthz` 503 +
  `serving: allow-all` combination makes a persistent fail-open state detectable
  from logs, from an HTTP probe, and from an orchestrator health check
  independently. The operator's actual question — "is this a consistent state?" —
  is answerable from any one of the three.
- **The dead-daemon case is named, not inferred.** `ever_ready: false` plus a
  long `empty_for_seconds` in a recurring ERROR is an unambiguous statement that
  the daemon has never functioned — the worst case the operator called out,
  reported directly rather than reconstructed.
- **An existing log-flood defect is fixed** in the same pass (§D.4).
- **The change is small and well-contained**: one new `server.Options` field, one
  branch in `handleCheck`, additive `/healthz` fields, and a small state tracker
  with a ticker on the server's existing `Run` lifecycle. No change to
  `fetcher`, `scheduler`, `diskcache`, `blocklist`, or `mmdb`.

### Negative

- **v1.0 does not tag until this lands.** Sprint 4's remaining item after docs is
  the gated v1.0 tag; this adds one Developer pass plus one QA pass ahead of it.
  Costed explicitly in the v1.0/v1.1 split below — this is the real price of the
  decision and it is accepted knowingly.
- **A new, if narrow, security-bypass path exists for operators who opt in.**
  Bounded by §E but not eliminated. An operator who sets `fail-open` and ignores
  ERROR logs has a daemon that can be neutralized by a sustained upstream outage
  plus a restart.
- **The server grows state.** It has been a pure function of `LookupSource` to
  date; it now latches `ever_ready`, timestamps a transition, counts fail-open
  allows, and runs a heartbeat goroutine. Small, but it is the first stateful
  thing in the package and needs `-race` coverage.
- **`/healthz`'s body grows three fields.** Additive and backward-compatible, but
  it is now a contract with more surface to keep stable.

### Neutral

- **`ready-then-empty` (§A.4) is expected to be rare** — it requires a
  `block.countries` set that matches nothing in the dataset. It is distinguished
  anyway because the fix (correct the config) differs completely from the
  never-ready fix (fix the network/cache), and conflating them would send an
  operator down the wrong path.
- **The heartbeat interval (60s) is a constant, not a config field**, matching
  the precedent set by the scheduler's cold-start retry budget
  (`internal/scheduler/scheduler.go:19-23`): cadence knobs the operator does not
  need are implementation details.

## Alternatives considered

### Drop `startup_mode` entirely and hard-code fail-closed

Remove the knob, the validation, and the documentation; the daemon is always
fail-closed.

**Why not:** the operator explicitly decided to wire it (2026-07-20), which
closes this. Recorded because it was genuinely on the table and remains the
simplest option: fail-closed is correct for an authorization gate, and no user
has asked for fail-open. Had the decision gone the other way, this would have
been the cheapest path to a truthful v1.0 — a config-schema removal plus a
README line, no new state in the server.

### `/healthz` returns 200 under fail-open

Reasoning: the daemon is serving requests successfully, so it is healthy.

**Why not:** it deletes the only pre-existing signal that the daemon is not doing
its job, and does so specifically in the mode where that signal matters most.
See §C. This is the alternative most likely to be re-proposed by a future reader
optimizing for "stop the orchestrator from restarting my container," so the
reasoning is recorded at length rather than dismissed.

### A distinct third `status` value (e.g. `{"status":"fail_open"}`)

Rather than additive fields, return a new `status` string.

**Why not:** `status` is consumed by anything already scripting against
`/healthz`, and changing its value domain is a breaking change to a documented
contract (`docs/bitblocker-spec.md` § `GET /healthz`) for no gain — the same
information is conveyed by an additive `serving` field that old consumers ignore
harmlessly. Coding standards §14: extend, do not redefine.

### Per-request logging of fail-open allows

Log every request allowed under fail-open, so the record is complete.

**Why not:** at real request rates behind Traefik this is a log flood during an
incident, filling the disk and burying the transition lines that explain what
happened. Counters in a 60s heartbeat carry the same information at bounded
volume. This is also why the existing per-request `WARN` at `server.go:173` is
being removed rather than mirrored.

### A single startup log line, no heartbeat

Log once on entering fail-open and consider the operator informed.

**Why not:** this is very close to the status quo's failure mode. A line at
startup scrolls out of any bounded log buffer and cannot distinguish a 30-second
window during a deploy from a permanently inert daemon. The operator's
requirement is specifically about detecting a *consistent* state, and a
non-recurring signal cannot express duration.

### Time-bounded fail-open (honor it for N minutes, then revert to fail-closed)

Cap the exposure by treating fail-open as a startup grace period.

**Why not:** see §E. It makes the daemon's security posture a function of
elapsed time rather than of configuration, so an operator debugging a live
incident has to reason about when the process started to know what it is
currently doing. It also defeats the one legitimate use case for fail-open
(availability-over-security on a noise filter, where the operator would rather
serve traffic than 403 everyone), by reintroducing the 403s at the worst moment.
Rejected in favor of making the state loud.

### Pull the v1.1 `/metrics` endpoint forward and answer the question with metrics

Ship the Prometheus endpoint in v1.0 so `blocklist_loaded` and
`last_successful_refresh_timestamp` are queryable.

**Why not (for v1.0):** it is the *right* long-term answer and it is
recommended for v1.1 — see the split below — but it expands the v1.0 pass from
one contained change into a new admin listener, a metrics registry, a
dependency addition, and its own interface spec, delaying the tag substantially.
The log heartbeat answers the operator's question adequately today at a fraction
of the cost.

## v1.0 / v1.1 split

**Ships in v1.0, alongside the fail-open wiring (one Developer pass):**

- The `/check` branch (§B) and the unparseable-IP carve-out.
- `/healthz` staying 503 with the additive discriminator fields (§C).
- All four log signals, including the heartbeat (§D).
- Removal of the per-request `WARN` at `server.go:173` (§D.4).
- README/`config.example.yaml`/`traefik-integration.md` doc updates.

**Stays in v1.1 (Sprint 5), unchanged:**

- `/metrics` Prometheus endpoint on a separate admin listener.
- `bitblocker check <ip>` and `bitblocker list` CLI subcommands.
- Alert webhook on refresh failure.

**Nothing is pulled forward from Sprint 5.** The recommendation is deliberate:
"is this a consistent state?" is *properly* answered by metrics — a
`blocklist_loaded` gauge and a `last_successful_refresh_timestamp` are what an
operator should eventually alert on, and the ERROR heartbeat is the poor
relation of that. But the heartbeat is sufficient, is one small addition to a
change already being made, and does not require standing up a second listener.
Recommendation for Sprint 5: when `/metrics` lands, add `blocklist_loaded`
(gauge, 0/1), `blocklist_prefixes` (gauge), `last_successful_refresh_timestamp`
(gauge, unix seconds), and `checks_allowed_failopen_total` (counter) — the
metric forms of exactly the fields this ADR puts in the heartbeat — and keep the
heartbeat rather than replacing it, since a self-hosted operator with no
Prometheus is the project's median user.

**Sequencing consequence, stated plainly:** v1.0 is otherwise ready to tag once
PR #20 merges. This work adds one Developer pass and one QA pass before the tag.
The recommendation is to accept that delay: tagging v1.0 with a documented
config knob that silently does the opposite of what it says is a defect the
project would carry in a released artifact and have to explain in a v1.0.1.

## Open questions surfaced

- **OQ-FAILOPEN-1 (for the ratification gate).** ~~Confirm the recommendation to
  hold the v1.0 tag for this work rather than tagging v1.0 now and shipping
  fail-open in v1.0.1.~~ **Resolved 2026-07-20 — hold.** The operator confirmed
  the Architect lean: the v1.0 tag waits for the Developer + QA passes rather
  than shipping a released artifact whose documented `startup_mode` knob does
  the opposite of what it says. This ADR is therefore `accepted`.
- **OQ-FAILOPEN-2 (Developer, at touch time).** The heartbeat interval is
  specified as a 60s constant. If the Developer finds a reason it should be
  derived from something (e.g. backing off after the first hour to reduce log
  volume on a long-inert daemon), flag it rather than deciding silently.
  Architect lean: keep it a flat 60s — a daemon that has been inert for a week
  *should* still be shouting once a minute.
- **OQ-FAILOPEN-3 (PM).** `docs/BitBlocker.md:147` records the Sprint 3
  `startup_mode` task as complete with a note claiming PR #14 wired the
  cold-start path to consult it. It did not. The row needs correcting so the
  historical record does not assert a capability the code never had. PM-owned;
  this ADR does not edit the sprint file.
- **OQ-FAILOPEN-4 (Technical Writer, downstream of this decision).** Open PR #20
  documents `startup_mode` with an inline "treat fail-open as reserved /
  not-yet-wired pending Developer/Architect confirmation" flag. Once this ADR is
  ratified and the Developer pass lands, that paragraph needs replacing with the
  real behavior plus the §E security note. Sequencing: PR #20 can merge as-is
  (its caveat is accurate today); the doc update follows the implementation.

## Cross-references

- `docs/interfaces/fail-open-and-readiness.md` — the implementation-ready spec
  derived from this ADR.
- `docs/BitBlocker.md` decisions log 2026-04-22 ("Cold-start fail mode:
  fail-closed with guardrails") — amended per the header.
- `docs/adr/0002-disk-cache-snapshot-format.md` §C — the cache's interaction with
  the cold-start posture; §A.2/§E.2 here depend on it.
- `docs/adr/0003-geoip-source-db-ip-over-maxmind.md` — the keyless, predictable
  fetch URL underpinning the §E threat analysis.
- `docs/bitblocker-spec.md` § `GET /healthz`, § v1.1 Observability and ops.
- `internal/server/server.go` — `handleCheck` (line 163), `handleHealthz`
  (line 218), the readiness predicate (lines 172, 228).
- `internal/fetcher/fetcher.go:139-170` — the evidence that a failed refresh
  never empties the active blocklist.
- `internal/config/config.go:15-24, 245-254` — the declared-but-unwired knob.
- Coding standards §14 (interface design — additive extension over redefinition),
  §4 (explicit boundaries); `coding-standards-go.md` §15 (concurrency — the
  server's new state needs `-race` coverage).

---

*End of ADR 0004.*
</content>
</invoke>
