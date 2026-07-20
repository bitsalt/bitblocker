# BitBlocker — QA test plan

> First QA pass for this project. This document is durable — extend it for
> future slices rather than replacing it. Each feature slice gets its own
> `##` section; a dated verification record for a specific PR lives at the
> end of that section rather than in a separate file, unless a future pass
> decides a standalone `test-run-<date>-<slug>.md` is warranted.

---

## Feature: `behavior.startup_mode: fail-open` wiring + readiness observability

- **Governing docs:** `docs/interfaces/fail-open-and-readiness.md` (acceptance
  criteria), `docs/adr/0004-fail-open-wiring-and-readiness-observability.md`
  (reasoning, incl. §D observability and §E security posture).
- **Boundary:** `internal/server` (`handleCheck`, `handleHealthz`, the
  `readiness` tracker, `Server.Run`'s heartbeat goroutine).
- **Operator requirement driving this feature (ADR 0004 context):** "I'd
  like to somehow know if this is a consistent state. If it's failing open
  consistently, at best we need to determine why and, at worst, the entire
  thing is dead code because it's never functioning." The design answer is
  a non-suppressible, recurring ERROR heartbeat every 60s on wall-clock
  cadence, independent of request traffic.

### Scope

Covers: the `/check` decision table under both `startup_mode` values, the
`/healthz` additive fields and its independence from `startup_mode`, the
readiness-state tracker (transition signals, heartbeat, `ever_ready`
latch, fail-open counters), removal of the old per-request WARN, and the
concurrency/lifecycle properties of the new server state.

Does not cover: `internal/fetcher`, `internal/scheduler`, `internal/mmdb`,
`internal/diskcache` (interface spec §2.2 — explicitly untouched by this
slice; the `ready-then-empty` reachability proof lives in
`internal/mmdb/loader_test.go` and was verified, not re-derived, below).
Does not cover README / `config.example.yaml` / `traefik-integration.md`
(Technical Writer, `bitblocker:OQ-FAILOPEN-4`, sequenced after this pass).

### Acceptance criteria (derived from the interface spec)

| # | Criterion | Source |
|---|---|---|
| AC1 | Fail-open engages iff `startup_mode==fail-open` AND blocklist unusable; never on unparseable IP, never on refresh failure, never on `/healthz` | spec §2.2 |
| AC2 | `/check` decision table, all 6 rows × both modes | spec §3.1 |
| AC3 | Row 3 (unusable + fail-open) skips IP extraction entirely — no misleading unparseable-IP signal, no dependency on header validity | spec §3.2 |
| AC4 | Row 4 (unparseable IP) is fail-closed under **both** modes, permanently — the security carve-out | spec §3.3 |
| AC5 | Row 3 increments fail-open counters; no other row does; row 3 emits no per-request log line | spec §3.4 |
| AC6 | `ever_ready` latches on first USABLE observation, never resets | spec §4.1 |
| AC7 | Transition signals (entering/leaving UNUSABLE) fire exactly once per transition under concurrency | spec §4.1 |
| AC8 | Entering-UNUSABLE signal: ERROR, both modes, correct message/fields, fires on cold start too | spec §4.2 |
| AC9 | Heartbeat: ERROR every 60s while UNUSABLE, wall-clock cadence (not request-driven), correct fields, non-suppressible | spec §4.3 |
| AC10 | Leaving-UNUSABLE signal: INFO, correct fields incl. window fail-open count | spec §4.4 |
| AC11 | Old per-request WARN (`server.go:173` pre-change) is gone; row-4 WARN unchanged | spec §4.5 |
| AC12 | `/healthz` status codes independent of `startup_mode`; `status` domain unchanged; additive fields correct per state | spec §5 |
| AC13 | `server.Options.StartupMode` wired end-to-end from `cmd/bitblocker/main.go` through to the decision; unset/invalid mode rejected by `New` | spec §6 |
| AC14 | `-race` clean; fail-open counters accurate under concurrent load | spec §4.1, coding-standards-go.md §15 |
| AC15 | Heartbeat goroutine started by `Run` stops cleanly on shutdown; no leak | spec (implied by §D wall-clock cadence + Run lifecycle) |

### Verification class

**Statically-verifiable, with one execution-required sub-element treated as
low-risk and covered by a substitute.** The readiness-tracker logic is pure
state-machine logic over an injected clock and an injected tick channel —
fully exercisable by mocked-seam unit tests, which is what this slice's
test suite does throughout. The one execution-required edge is
`Server.Run`'s wiring of the **real** `time.NewTicker(heartbeatInterval)`
(a hardcoded 60s constant, not injectable) to the real `heartbeatLoop` — a
true end-to-end proof of that exact wiring would require either a ~65s
wall-clock sleep (rejected: this project's own convention, and coding
standards generally, prefer deterministic non-sleeping tests) or a new
test-only seam in production code (out of QA's writes-allowlist — see
Gap G1 below). This is treated as **low risk, not blocking**: the
extracted-logic tests give strong confidence in the heartbeat's behavior,
and the goroutine-lifecycle risk around the real ticker (leak / hang on
shutdown) is independently covered by `TestRun_LifecycleAgainstLoopback`
and the new `TestRun_HeartbeatGoroutineDoesNotLeakOnShutdown` (both would
fail/hang if `Run`'s wiring were broken). No `QA-provisional` designation
is warranted; see Gap G1 for the recommended follow-up.

### Test matrix

| Criterion | Unit | Integration (`httptest`/`Run()` over loopback) | Concurrency (`-race`) |
|---|---|---|---|
| AC1–AC5 (decision table + carve-out) | ✅ table-driven, `failopen_test.go` | ✅ | — |
| AC6 (ever_ready latch) | ✅ | — | — |
| AC7 (transitions once) | ✅ | — | ✅ |
| AC8–AC10 (signals) | ✅ | — | — |
| AC11 (no per-request log) | ✅ | — | — |
| AC12 (`/healthz`) | ✅ | ✅ | — |
| AC13 (wiring) | ✅ (`server.New` validation) | partial — see Gap G2 | — |
| AC14 (concurrency/counters) | — | — | ✅ |
| AC15 (lifecycle) | — | ✅ | ✅ (implicit hang-detection) |

Every criterion has coverage at some level; G1/G2 below name the two
narrow spots where the coverage is a substitute rather than a direct proof.

### Non-functional targets

- `go test -race ./...` clean (mandatory per interface spec §4.1 / coding
  standards-go.md §15).
- `golangci-lint run ./...` clean at the CI-pinned version (v2.11.4).
- `govulncheck ./...` clean under the **pinned toolchain** (`go1.25.12` —
  a higher local Go minor version does not carry the same stdlib CVE
  backports; see Environment note below).
- No new external dependencies introduced by this slice.

### Environments and data

- Toolchain: `GOTOOLCHAIN=go1.25.12` explicitly, not whatever `go` resolves
  to locally — this machine's Go 1.26.4 does not carry the
  `crypto/tls` backport for GO-2026-5856, so `govulncheck` under it
  reports a false failure.
- `golangci-lint` / `govulncheck` are not on `PATH` by default on this
  machine; both live in `~/go/bin`.
- No external services required — all tests are `httptest`/in-process;
  the one real-network dependency in the codebase (`fetcher`'s DB-IP
  fetch) is out of scope for this slice and untouched.

### Out of scope / deferred

- `/metrics` (v1.1, per ADR 0004's v1.0/v1.1 split) — not part of this
  criterion set.
- README / `config.example.yaml` / `traefik-integration.md` — Technical
  Writer pass, sequenced after this one.
- `cmd/bitblocker/main.go` end-to-end wiring proof via a live `run()` —
  see Gap G2.

---

## Verification record — PR #22 (`developer/bitblocker/adr-0004-fail-open-readiness`)

- **Commit under test:** `cf9f60b`, based on `origin/main` at `415613d`.
- **Plan version exercised:** this document, first revision.
- **Toolchain:** `GOTOOLCHAIN=go1.25.12` (pinned per `go.mod`), darwin/arm64.
- **Commands run:**
  - `GOTOOLCHAIN=go1.25.12 go build ./...` — clean.
  - `GOTOOLCHAIN=go1.25.12 go vet ./...` — clean.
  - `GOTOOLCHAIN=go1.25.12 go test -race -count=1 ./...` — all packages pass;
    `internal/server` at 96.2% statement coverage.
  - `GOTOOLCHAIN=go1.25.12 go test -race -count=5 ./internal/server/...` —
    repeated to rule out flakiness; stable.
  - `~/go/bin/golangci-lint run ./...` (v2.11.4, matching the CI pin) — `0 issues`.
  - `~/go/bin/govulncheck ./...` (under the pinned toolchain) — `0 vulnerabilities`.

### Results by acceptance criterion

| # | Criterion | Result | Notes |
|---|---|---|---|
| AC1 | Fail-open engagement predicate | **Pass** | Confirmed in code (`handleCheck`) and by the full decision-table + carve-out tests. `internal/fetcher/fetcher.go:139-170` re-verified: every failure path returns before `Swap`, so a failed refresh cannot engage fail-open — untouched by this PR, as required. |
| AC2 | `/check` decision table, 6 rows × 2 modes | **Pass** | `TestCheck_DecisionTable` covers 5 of 6 rows explicitly (405 is method-level, mode-independent, also covered) × both modes = matches spec table. |
| AC3 | Row 3 skips IP extraction | **Pass** | Confirmed by code inspection (`handleCheck` branches on `readiness.observe` before ever calling `extractClientIP`) **and** closed a test gap: added `TestCheck_UnusableBlocklistIgnoresClientIPValidity`, which the Developer's suite did not cover — malformed `X-Real-IP`/`X-Forwarded-For` against nil/empty blocklist state, both modes (12 sub-cases), asserting both the correct status code and the **absence** of the row-4 WARN (proof the extraction path is truly never reached, not just that the mode happens not to matter). |
| AC4 | Security carve-out (row 4, both modes, incl. fully-populated blocklist) | **Pass, and now more broadly proven.** | Developer's `TestCheck_FailOpenDoesNotApplyToUnparseableClientIP` covers fail-open + populated (length 5) + 3 malformed-header variants; `TestCheck_FailClosedOnUnparseableIP` covers fail-closed + populated. New `TestCheck_UnusableBlocklistIgnoresClientIPValidity` adds the nil/empty × malformed-IP × both-modes cells that were previously unverified (see AC3), closing the full mode × blocklist-state matrix the task asked to verify adversarially, not just the one case originally tested. |
| AC5 | Counters + no per-request log on row 3 | **Pass** | `TestCheck_NoPerRequestLoggingWhileUnusable` (50 requests → 1 log line, valid IP). Extended by AC3's new test for the malformed-IP variant. Counter *presence* was already tested (`TestHeartbeat_FailOpenCounters`); counter *accuracy under concurrency* was not — closed by new `TestReadiness_FailOpenCounterAccurateUnderConcurrentLoad` (20 goroutines × 50 requests = 1000, recovery signal's window counter asserted to equal exactly 1000 with `-race` clean). |
| AC6 | `ever_ready` latch | **Pass** | `TestReadiness_EverReadyLatches`, `TestHeartbeat_ReadyThenEmptyReportsConfigCause` — confirms the discriminator distinguishes never-ready vs ready-then-empty correctly (task item 3). The `ready-then-empty` reachability claim (a non-empty `block.countries` matching nothing yields `Len()==0` with a **nil** error, reaching `Source.Swap`) was independently re-verified by reading `internal/mmdb/loader.go` — confirmed correct; the Developer's `TestLoadCountryBlocklist_NoMatchingCountries_TrieIsEmpty` (with a clarifying comment added in this PR) proves it. |
| AC7 | Transitions fire once under concurrency | **Pass** | `TestReadiness_TransitionsEmitOnceUnderConcurrency` — 16 goroutines × 25 requests across a live `Swap`, `-race` clean. |
| AC8 | Entering-UNUSABLE ERROR signal | **Pass** | `TestCheck_FailClosedWhenBlocklistEmpty`, `TestHeartbeat_FiresWithNoTrafficAtAll` (cold-start entry). |
| AC9 | **Heartbeat with zero traffic (the single highest-value check)** | **Pass — adversarially confirmed.** | `TestHeartbeat_FiresWithNoTrafficAtAll` drives `heartbeatLoop` directly with an injected tick channel and a fake clock across 4 ticks, with **zero HTTP requests at any point** — asserts exactly 4 heartbeat lines, `ERROR` level, correct `ever_ready:false`, `serving:allow-all`, `startup_mode:fail-open`, `likely_cause`, and duration fields. This is a genuine test of the inert-daemon case the operator described, not a side effect of request activity. See "Non-suppressibility" below for the closing half of this check, and Gap G1 for the one part of the *wiring* (not the logic) this doesn't reach. |
| AC9b | Non-suppressibility (no `logging.level`, no `log_blocked`/`log_allowed` flag can silence it) | **Pass — closed two test gaps.** | Confirmed `internal/config/config.go` caps `logging.level` at `error` (no stricter value is valid config), so `slog.LevelError` is the strictest a real config can produce. Added `TestHeartbeat_NotSuppressibleAtStrictestConfiguredLogLevel`, which builds a handler at exactly that level and confirms both the entering-signal and heartbeat ERROR lines still pass, while explicitly confirming the (expected, non-mandatory) recovery INFO is filtered there — pinning the boundary precisely rather than leaving it implied. Added `TestHeartbeat_NotGatedByLogBlockedOrLogAllowedFlags`, setting both flags false (the opposite of every other test in the suite) and confirming the readiness signals still fire — neither flag is consulted by `readiness.go`, confirmed by code inspection and now by test. |
| AC10 | Leaving-UNUSABLE INFO signal | **Pass** | `TestReadiness_RecoverySignal` — duration, window fail-open count, prefix count. |
| AC11 | Old per-request WARN removed; row-4 WARN unchanged | **Pass** | Confirmed by diff inspection (`git diff 415613d cf9f60b -- internal/server/server.go`) — the old `server.go:173` WARN is fully replaced; the row-4 WARN at (now) line ~254 is untouched. `TestCheck_FailClosedOnUnparseableIP` still exercises it. |
| AC12 | `/healthz` contract | **Pass** | `TestHealthz_503UnderFailOpen`, `TestHealthz_DiscriminatorFields`, `TestHealthz_EmptyForSecondsTracksTheWindow` — status domain unchanged, `serving`/`ever_ready`/`prefixes`/`empty_for_seconds` all correct per state, independent of `startup_mode` as required. |
| AC13 | Wiring: config → `server.Options` → decision | **Pass at the `server` boundary; unverified at the `main.go` boundary — see Gap G2.** | `TestNew_RejectsMissingFields` confirms `New` rejects an unset/invalid `StartupMode` rather than defaulting it (spec §6.1). `cmd/bitblocker/main.go` diff confirms the one-line pass-through matches spec §6.2 exactly (code inspection). No automated test exercises `cmd/bitblocker`'s `run()` with `startup_mode: fail-open` end-to-end — see Gap G2 for why, and the recommended follow-up. |
| AC14 | Concurrency / counter accuracy | **Pass** | `-race` clean across the whole suite (`count=1` and `count=5`); new `TestReadiness_FailOpenCounterAccurateUnderConcurrentLoad` confirms no lost increments under 1000 concurrent fail-open allows. |
| AC15 | Lifecycle — heartbeat goroutine stops cleanly, no leak | **Pass.** | `TestRun_LifecycleAgainstLoopback` already implicitly proves this (a stuck heartbeat goroutine would hang `hbWG.Wait()` and fail the test's 2s shutdown deadline). Added an explicit check, `TestRun_HeartbeatGoroutineDoesNotLeakOnShutdown`, repeating 3 full bind/serve/shutdown cycles and asserting `runtime.NumGoroutine()` returns to baseline. **QA process note:** my first draft of this test used `require.Eventually` to poll the goroutine count and failed consistently — root-caused to testify's `Eventually` spawning a fresh goroutine per tick to run the condition function, so a `runtime.NumGoroutine()` read *inside* that condition always counts the checker goroutine itself and can never settle to a pre-`Eventually` baseline. This was a bug in the new test, not in the implementation. Fixed by polling synchronously from the test's own goroutine instead; stable across 5+ repeated runs after the fix. Documented here per the QA role's "flag non-determinism as a finding" standard, applied to my own artifact rather than the Developer's. |

### Gaps identified (non-blocking) and recommendation

- **G1 — `Run()`'s real 60s ticker wiring has no direct end-to-end test.**
  The heartbeat *logic* is thoroughly proven via the injected-channel test
  (AC9), and the goroutine-lifecycle risk around the real ticker is
  covered (AC15), but nothing proves the literal
  `time.NewTicker(heartbeatInterval)` line in `Run()` fires on the actual
  configured cadence, because `heartbeatInterval` is a hardcoded constant
  with no test seam and a real-time test would require a ~65s sleep
  (rejected as non-deterministic per this project's own testing
  convention). **Recommendation (non-blocking):** a future pass could add
  a small unexported seam (e.g., an optional ticker-factory field on
  `Server`, defaulting to `time.NewTicker` in `New`, overridable only by
  same-package tests) mirroring the existing clock-injection pattern
  (`coding-standards-go.md` §4) — this is a Developer-owned change, not a
  QA one, since it touches production code.
- **G2 — `cmd/bitblocker/main.go`'s `StartupMode` pass-through is
  unverified by any automated test.** This is the exact line ADR 0004
  exists because of (a config value declared, validated, and logged, but
  not consumed) — verified correct by code inspection and diff review, but
  not by a test, because `cmd/bitblocker`'s `run()` starts a real
  scheduler/fetcher pointed at the live DB-IP host (`defaultBaseURL` is
  hardcoded, not injectable via `fetcher.Options`), so exercising `run()`
  in a test would make a real network call. **Recommendation
  (non-blocking, low severity):** flag to Architect/Developer as a
  possible future seam (an injectable fetcher base URL, or extracting the
  `server.Options{...}` construction into an independently-testable
  function) if `cmd/bitblocker`-level wiring tests become a recurring
  need. Low severity because it is a single literal line, trivially
  reviewable in the diff — unlike the original defect, which sat unwired
  and undetected across multiple sprints inside otherwise-plausible code.

Neither gap is a defect in the shipped behavior; both are named because
the task asked for an adversarial, not a confirmatory, pass.

### Verdict

**Mergeable as-is.** No defects found against the interface spec or ADR
0004. Every acceptance criterion passed, including the two most
adversarial checks this pass was scoped around:

1. **The heartbeat fires on wall-clock cadence with genuinely zero request
   traffic** (`TestHeartbeat_FiresWithNoTrafficAtAll`), and is confirmed
   non-suppressible via `logging.level` and via `log_blocked`/`log_allowed`
   (two new tests).
2. **The security carve-out (row 4, unparseable client IP) holds
   fail-closed under both modes across the full blocklist-state matrix**,
   not just the one populated-blocklist case the Developer's suite
   originally covered (one new test closing the nil/empty × malformed-IP
   cells).

G1 and G2 are recorded as follow-ups, not blockers — both are gaps in
*proof of wiring* for code that is otherwise correct by inspection and
indirectly exercised, not suspected defects.
